package engine

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// declaredPayloadWorkflow is the §4.8 subject: one step that declares a
// `payload`, one that does not, so C1 and C2 are exercised against the same run.
const declaredPayloadWorkflow = `
[pipeline]
name = "declares-a-payload"
version = 1
[match]
kind = ["task"]
[[step]]
name = "assess"
after = []
executor = "someone"
emits = "report"
payload = "findings@1"
[[step]]
name = "note"
after = ["assess"]
executor = "someone"
emits = "note"
`

// activatedPayloadRun registers the schema and the workflow, activates a run,
// and returns it — the state every C-clause test starts from.
func activatedPayloadRun(t *testing.T, conn *sql.DB) *model.Run {
	t.Helper()
	registerSchemaFixture(t, conn, "findings", 1, pinFindingsSchema)
	registerSource(t, conn, []byte(declaredPayloadWorkflow), "declares-a-payload.toml")
	issue := createIssue(t, conn, "validates its payload", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run
}

// completeWithPayload claims a step and completes it, returning the refusal (or
// nil) plus the step's row_version BEFORE the attempt, so a caller can assert
// that a refusal wrote nothing.
func completeWithPayload(
	t *testing.T, conn *sql.DB, e *Engine, instance, payload string,
) (err error, versionBefore int) {
	t.Helper()
	stepID := stepIDByInstance(t, conn, instance)

	claim, cerr := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	if cerr != nil {
		t.Fatalf("claim %s: %v", instance, cerr)
	}

	step, gerr := db.GetStep(conn, stepID)
	if gerr != nil {
		t.Fatalf("GetStep: %v", gerr)
	}

	return e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("a body"),
		Payload: []byte(payload), NowMS: nowMS,
	}), step.RowVersion
}

// assertRefusedCleanly is the assertion every §6.9 refusal carries: a
// VALIDATION_ERROR wrote nothing, so `row_version` has not moved and no artifact
// exists.
func assertRefusedCleanly(t *testing.T, conn *sql.DB, instance string, versionBefore int) {
	t.Helper()

	step, err := db.GetStep(conn, stepIDByInstance(t, conn, instance))
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.RowVersion != versionBefore {
		t.Errorf("row_version moved from %d to %d during a refusal",
			versionBefore, step.RowVersion)
	}
	if step.InSaga() {
		t.Errorf("saga_stage = %q after a refusal; stage 0 must not have committed",
			step.SagaStage)
	}

	var artifacts int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE step_id = ?`, step.ID).Scan(&artifacts)
	testsupport.Must(t, err, "counting artifacts: %v", err)
	if artifacts != 0 {
		t.Errorf("%d artifacts recorded during a refusal, want 0", artifacts)
	}
}

// TestPayloadC1SchemalessStepIsUnchanged is C1: a step that declares no
// `payload` keeps S3/S4 behavior exactly — shape-only validation, and no new
// refusal exists for it.
func TestPayloadC1SchemalessStepIsUnchanged(t *testing.T) {
	// A payload NO schema would accept records fine, because the step declares
	// no schema to accept it against.
	t.Run("anything the shape allows records", func(t *testing.T) {
		conn := mustDB(t)
		activatedPayloadRun(t, conn)
		e := testEngine()

		claimAndComplete(t, conn, e, "assess@0", "a body", `[{"severity":"low"}]`)
		claimAndComplete(t, conn, e, "note@0", "a body",
			`[{"anything":"at all","nested":{"x":1}}]`)

		if got := stepStatus(t, conn, "note@0"); got != db.StepDone {
			t.Errorf("status = %q, want %q — a schema-less step is unchanged",
				got, db.StepDone)
		}
	})

	// The shape check that DID exist still does, and is still the ONLY one.
	t.Run("the S3 shape check survives", func(t *testing.T) {
		conn := mustDB(t)
		activatedPayloadRun(t, conn)
		e := testEngine()

		claimAndComplete(t, conn, e, "assess@0", "a body", `[{"severity":"low"}]`)

		err, before := completeWithPayload(t, conn, e, "note@0", `{"not":"an array"}`)
		if err == nil {
			t.Fatal("a non-array payload was accepted on a schema-less step")
		}
		if !strings.Contains(err.Error(), "JSON array of objects") {
			t.Errorf("refusal = %v, want the S3 shape message", err)
		}
		assertRefusedCleanly(t, conn, "note@0", before)
	})
}

// TestPayloadC2C3RefusalIsPathPrecise is C2 and C3: a declared payload is
// validated against the PINNED schema, and the refusal names the element and the
// property.
func TestPayloadC2C3RefusalIsPathPrecise(t *testing.T) {
	conn := mustDB(t)
	activatedPayloadRun(t, conn)
	e := testEngine()

	err, before := completeWithPayload(t, conn, e, "assess@0",
		`[{"severity":"low"},{"severity":"urgent"}]`)
	if err == nil {
		t.Fatal("an invalid payload was accepted")
	}
	assertCode(t, err, CodeValidation)
	const want = `payload[1].severity: value "urgent" is not one of ["low","medium","high"]`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal is not path-precise\n  want: %s\n  got:  %s", want, err.Error())
	}
	if !strings.Contains(err.Error(), "findings@1") {
		t.Errorf("refusal does not name the schema it consulted: %v", err)
	}
	assertRefusedCleanly(t, conn, "assess@0", before)
}

// TestPayloadC3CapsAtFiveLines is C3's cap: a worker's log is not improved by a
// hundred lines, and the count of what was dropped is reported so the operator
// knows the list is partial.
func TestPayloadC3CapsAtFiveLines(t *testing.T) {
	conn := mustDB(t)
	activatedPayloadRun(t, conn)
	e := testEngine()

	var elements []string
	for range 8 {
		elements = append(elements, `{"severity":"nope"}`)
	}
	err, before := completeWithPayload(t, conn, e, "assess@0", "["+strings.Join(elements, ",")+"]")
	if err == nil {
		t.Fatal("an invalid payload was accepted")
	}

	lines := strings.Count(err.Error(), "payload[")
	if lines != 5 {
		t.Errorf("rendered %d payload lines, want the 5-line cap:\n%s", lines, err.Error())
	}
	if !strings.Contains(err.Error(), "(+3 more)") {
		t.Errorf("the capped refusal does not say how many were dropped:\n%s", err.Error())
	}
	assertRefusedCleanly(t, conn, "assess@0", before)
}

// TestPayloadC4AbsentPayloadIsRefused is C4: a declared payload is a contract.
// Silently recording none would make every threshold over it evaluate against
// the empty set and route `pass` — a silent misroute produced by an omission.
func TestPayloadC4AbsentPayloadIsRefused(t *testing.T) {
	conn := mustDB(t)
	activatedPayloadRun(t, conn)
	e := testEngine()

	err, before := completeWithPayload(t, conn, e, "assess@0", "")
	if err == nil {
		t.Fatal("a step declaring `payload` completed with none")
	}
	assertCode(t, err, CodeValidation)
	if !strings.Contains(err.Error(), "findings@1") {
		t.Errorf("refusal does not name the schema: %v", err)
	}
	assertRefusedCleanly(t, conn, "assess@0", before)
}

// TestPayloadC5SizeCapPrecedesTheSchema is C5: the order of the two
// pre-transaction content checks is PINNED, so the error a huge invalid payload
// produces is stable rather than a race between two checks.
func TestPayloadC5SizeCapPrecedesTheSchema(t *testing.T) {
	conn := mustDB(t)
	activatedPayloadRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "assess@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	// Both wrong: an oversize artifact AND a payload the schema refuses.
	oversize := make([]byte, db.ArtifactMaxBytes+1)
	for i := range oversize {
		oversize[i] = 'x'
	}
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: oversize,
		Payload: []byte(`[{"severity":"urgent"}]`), NowMS: nowMS,
	})
	if err == nil {
		t.Fatal("an oversize artifact was accepted")
	}
	if !strings.Contains(err.Error(), "over the") {
		t.Errorf("the size cap did not decide first: %v", err)
	}
	if strings.Contains(err.Error(), "urgent") {
		t.Errorf("the schema refusal won a race the size cap should have decided: %v", err)
	}
}

// TestPayloadC6AuthorizationPrecedesContent is C6, and it is a SECURITY
// property, not a tidiness one: an unauthenticated caller must not be able to
// learn a run's schema by probing it with payloads and reading the refusals.
func TestPayloadC6AuthorizationPrecedesContent(t *testing.T) {
	conn := mustDB(t)
	activatedPayloadRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "assess@0")
	_, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	for _, tc := range []struct{ name, payload string }{
		{"an invalid payload", `[{"severity":"urgent"}]`},
		{"an absent payload", ""},
		{"a valid payload", `[{"severity":"low"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := e.CompleteStep(conn, stepID, CompleteOptions{
				Token: "not-the-holders-token", Artifact: []byte("a body"),
				Payload: []byte(tc.payload), NowMS: nowMS,
			})
			if err == nil {
				t.Fatal("a non-holder completed the step")
			}
			// The sentinel, not the message: AUTH_ERROR is what the CLI maps
			// db.ErrNotHolder to, and it is what a harness branches on.
			if !errors.Is(err, db.ErrNotHolder) {
				t.Fatalf("a non-holder got %v; authorization precedes content, always", err)
			}
			// The refusal must give away NOTHING about the schema.
			for _, leak := range []string{"findings", "severity", "urgent", "not one of"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the AUTH refusal leaks %q: %v", leak, err)
				}
			}
		})
	}

	assertRefusedCleanly(t, conn, "assess@0", step.RowVersion)
}

// assertCode checks an error's taxonomy code, which is what determines the
// process exit code a harness branches on.
func assertCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	got, ok := CodeOf(err)
	if !ok {
		t.Errorf("error %v carries no taxonomy code, want %q", err, want)
		return
	}
	if got != want {
		t.Errorf("code = %q, want %q (%v)", got, want, err)
	}
}

// TestPayloadValidationReadsThePinnedBytes is P4 proven at the verb: the schema
// a `complete` validates against is the one the RUN pinned, not the one the
// registry holds now.
//
// The registry cannot legally change under a run — `name@version` is frozen — so
// the divergence is forced here the only way it can arise, by writing the row
// directly. That is the shape a database restored from elsewhere has.
func TestPayloadValidationReadsThePinnedBytes(t *testing.T) {
	conn := mustDB(t)
	activatedPayloadRun(t, conn)
	e := testEngine()

	// Swap the registry row's bytes underneath the run.
	_, err := conn.Exec(
		`UPDATE schemas SET body = ?, source_sha256 = 'swapped' WHERE name = 'findings'`,
		`{"type":"array"}`)
	testsupport.Must(t, err, "swapping the registry row: %v", err)

	err, before := completeWithPayload(t, conn, e, "assess@0", `[{"severity":"low"}]`)
	if err == nil {
		t.Fatal("validation used the swapped bytes; the pin decided nothing")
	}
	if !strings.Contains(err.Error(), "this run pinned") {
		t.Errorf("refusal = %v, want it to name the pin the run holds", err)
	}
	assertRefusedCleanly(t, conn, "assess@0", before)
}

// TestAValidPayloadRecords closes the loop: the refusals above are not the
// whole story, and a payload the schema accepts completes the step normally.
func TestAValidPayloadRecords(t *testing.T) {
	conn := mustDB(t)
	run := activatedPayloadRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "assess@0", "a body", `[{"severity":"high"}]`)

	if got := stepStatus(t, conn, "assess@0"); got != db.StepDone {
		t.Errorf("status = %q, want %q", got, db.StepDone)
	}
	artifacts, err := db.ListRunArtifacts(conn, run.ID)
	testsupport.Must(t, err, "listing artifacts: %v", err)

	// The step's own `emits` kind. A completed step also records the snapshotted
	// `issue.diff` (§6.7.1 D1), which is not what this assertion is about.
	var recorded []string
	for _, a := range artifacts {
		if a.Kind != "report" {
			continue
		}
		recorded = append(recorded, a.Payload)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorded %d `report` artifacts, want 1", len(recorded))
	}
	if recorded[0] != `[{"severity":"high"}]` {
		t.Errorf("payload = %q, want the bytes as given", recorded[0])
	}
}
