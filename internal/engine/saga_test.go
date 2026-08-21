package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// testEngine is the engine these saga tests drive: a DETERMINISTIC diff, and a
// gate runner that does not spawn.
//
// The diff is stubbed because the real GitDiff shells out to git, and a test
// that invoked it would depend on the working tree these tests are supposed to
// prove immunity from.
//
// THE GATE RUNNER IS STUBBED FOR A SHARPER REASON, and it is worth stating
// because the swap to the real runner (gates-trust §7.1) is what made it
// necessary. These tests are about the SAGA — its stage table, its transaction
// boundaries, its routing — and their fixture declares gates (`build`, `tests`,
// `secret-scan`, …) purely as saga stages to traverse. Under the real runner
// those gates are correctly `unmatched` in a sandbox with no trust entries, so
// every one of them fails the step and routes it to `waiting-human` before the
// saga property under test is ever reached. That is the ENGINE BEHAVING
// CORRECTLY — an unmatched gate is a failure (§6.2 N3) — and pinning it here
// would be testing the trust model in the wrong file.
//
// So the saga tests keep the pass-through, and the trust model is proven where
// it belongs: gate_exec_test.go, saga_resume_test.go, and pregate_test.go drive
// the REAL runner against a sandbox trust store.
// fixtureRound is the loop ordinal the shared drive helpers are currently
// driving, read by testEngine's stub DiffFn so each round's tree differs from
// the last — which is what a fix round that actually fixes something does.
//
// It exists because DKT-340's non-convergence guard parks a loop whose round
// changed nothing in scope, and a stub returning ONE constant forever models
// exactly that: a run in which no round ever moves the tree. Every loop test
// that reaches ordinal 2 was relying on the engine not noticing.
//
// It is keyed on the ORDINAL, not on a call counter, so a test that never
// drives a loop sees the same bytes it always did and the byte-identical
// assembly and golden-packet assertions are untouched.
var fixtureRound int

// driveFixtureRound sets the ordinal the stub tree reflects, and restores it
// afterward so a helper cannot leak a round into an unrelated test.
func driveFixtureRound(t *testing.T, ordinal int) {
	t.Helper()
	prev := fixtureRound
	fixtureRound = ordinal
	t.Cleanup(func() { fixtureRound = prev })
}

func testEngine() *Engine {
	e := NewEngine()
	e.Gates = PassThroughRunner{}
	e.DiffFn = func(_, _ string, scope []string) (string, error) {
		diff := "diff for scope " + jsonString(scope)
		if fixtureRound > 0 {
			diff += fmt.Sprintf("\n+ round %d", fixtureRound)
		}
		return diff, nil
	}
	return e
}

func jsonString(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}

// claimAndComplete drives a step through claim and the whole saga.
func claimAndComplete(
	t *testing.T, conn *sql.DB, e *Engine, instance string, artifact, payload string,
) {
	t.Helper()
	stepID := stepIDByInstance(t, conn, instance)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim %s: %v", instance, err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte(artifact),
		Payload: []byte(payload), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete %s: %v", instance, err)
}

// stepStatus reads a step's STORED status.
func stepStatus(t *testing.T, conn *sql.DB, instance string) string {
	t.Helper()
	var status string
	err := conn.QueryRow(
		`SELECT status FROM steps WHERE instance = ?`, instance).Scan(&status)
	testsupport.Must(t, err, "reading status of %s: %v", instance, err)
	return status
}

// TestSagaHappyPath is the baseline: claim, complete, and the step lands `done`
// with its artifact recorded, its gates trailed, and its saga closed.
func TestSagaHappyPath(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	if got := stepStatus(t, conn, "implement@0"); got != db.StepDone {
		t.Errorf("status = %q, want %q", got, db.StepDone)
	}

	step, err := db.GetStep(conn, stepIDByInstance(t, conn, "implement@0"))
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.InSaga() {
		t.Errorf("saga_stage = %q after completion, want empty", step.SagaStage)
	}
	if step.Routing != RoutingPass {
		t.Errorf("routing = %q, want %q (no threshold declared ⇒ pass)",
			step.Routing, RoutingPass)
	}

	// The fixture's `implement` declares five gates; every one has a recorded
	// result. At v8 those rows live in `gate_results`, not in the trail JSON
	// (gates-trust §4.3 G3, M-a) — this test moved its reader with them.
	//
	// They are STUBBED because this test drives the pass-through runner
	// (see testEngine): the stub marker is what lets an operator tell a gate
	// that ran from one that did not, and it is exactly what the real runner
	// never sets (T11, N4).
	results, err := db.GateResultsForStep(conn, step.ID)
	testsupport.Must(t, err, "reading gate results: %v", err)
	if len(results) != 5 {
		t.Errorf("recorded %d gate results, want the fixture's 5", len(results))
	}
	for _, r := range results {
		if !r.Stub {
			t.Errorf("gate %q result is not marked stub:true — an operator must "+
				"be able to tell a stubbed gate from a real one", r.Gate)
		}
	}

	// The artifact is recorded under the step's declared `emits` kind.
	artifacts, err := db.ListRunArtifacts(conn, run.ID)
	testsupport.Must(t, err, "ListRunArtifacts: %v", err)
	var sawSummary, sawDiff bool
	for _, a := range artifacts {
		switch a.Kind {
		case "change-summary":
			sawSummary = true
			if a.Body != "the change summary" {
				t.Errorf("artifact body = %q", a.Body)
			}
		case ArtifactKindIssueDiff:
			sawDiff = true
		}
	}
	if !sawSummary {
		t.Error("no change-summary artifact recorded")
	}
	// D1: the diff is recomputed at every EXECUTOR step's completion.
	if !sawDiff {
		t.Error("no issue.diff artifact recorded at an executor step's completion (D1)")
	}
}

// TestTokenRetiresExactlyAtStageOne is §6.8's HINGE, asserted in both
// directions: before stage 1 commits the token is required, and after it the
// step has no lease at all.
func TestTokenRetiresExactlyAtStageOne(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	// Before: the lease is held and the token authorizes.
	lease, err := db.GetStepLease(conn, stepID)
	testsupport.Must(t, err, "GetStepLease: %v", err)
	if !lease.Held() {
		t.Fatal("the step holds no lease after a successful claim")
	}

	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	// After: no lease, and the attempt trail survives.
	lease, err = db.GetStepLease(conn, stepID)
	testsupport.Must(t, err, "GetStepLease: %v", err)
	if lease.Held() {
		t.Errorf("the token did not retire: owner = %q", lease.Owner)
	}
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d after retirement, want 1 — the trail must survive",
			lease.Attempt)
	}
}

// TestCompleteTwiceIsAuthError is R9, the token-retirement proof at the verb
// level: a worker that completes twice — a plausible retry after a dropped
// response — gets AUTH_ERROR on the second call rather than recording a
// duplicate artifact.
func TestCompleteTwiceIsAuthError(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	opts := CompleteOptions{Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS}

	err = e.CompleteStep(conn, stepID, opts)
	testsupport.Must(t, err, "first complete: %v", err)

	before, err := db.ListRunArtifacts(conn, run.ID)
	testsupport.Must(t, err, "ListRunArtifacts: %v", err)

	// The saga is CLOSED, so the second call re-enters stage 0 and is refused.
	err = e.CompleteStep(conn, stepID, opts)
	if !errors.Is(err, db.ErrNotHolder) {
		t.Errorf("second complete = %v, want ErrNotHolder (AUTH_ERROR, R9)", err)
	}

	after, err := db.ListRunArtifacts(conn, run.ID)
	testsupport.Must(t, err, "ListRunArtifacts: %v", err)
	if len(after) != len(before) {
		t.Errorf("the refused second complete recorded %d extra artifacts",
			len(after)-len(before))
	}
}

// TestCrashAtEveryStageBoundary is §9 item 10 and the reason the saga exists in
// this shape: for EACH resume point, a crash after that stage's commit must be
// recoverable by any later engine invocation, with exactly-once advance.
//
// The crash is simulated by driving the saga one stage at a time and abandoning
// it — which is what a `kill -9` between two transactions leaves behind — then
// resuming from a fresh call that supplies NO TOKEN. That the resume needs no
// token is the property being proved: after stage 1 there is no lease to lose
// and no owner to wait for.
func TestCrashAtEveryStageBoundary(t *testing.T) {
	// Each entry is a resume point reachable in the fixture's `implement` step:
	// `recorded`, then one per gate, then `routing`.
	boundaries := []string{
		db.SagaRecorded,
		db.SagaGatePrefix + "build",
		db.SagaGatePrefix + "tests",
		db.SagaGatePrefix + "scope",
		db.SagaGatePrefix + "self-hygiene",
		db.SagaGatePrefix + "secret-scan",
		db.SagaRouting,
	}

	for _, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)
			e := testEngine()

			stepID := stepIDByInstance(t, conn, "implement@0")
			claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
			testsupport.Must(t, err, "claim: %v", err)

			// Stage 0/1: record the artifact and retire the token.
			step, err := db.GetStep(conn, stepID)
			testsupport.Must(t, err, "GetStep: %v", err)
			err = e.stageZero(conn, step, CompleteOptions{
				Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
			})
			testsupport.Must(t, err, "stage 0: %v", err)

			// Advance one stage at a time until the crash point, then stop —
			// which is the crash.
			for {
				step, err = db.GetStep(conn, stepID)
				testsupport.Must(t, err, "GetStep: %v", err)
				if step.SagaStage == boundary || !step.InSaga() {
					break
				}
				_, err := e.advanceOne(conn, step, nowMS)
				testsupport.Must(t, err, "advancing to %s: %v", boundary, err)
			}

			if step.SagaStage != boundary {
				t.Fatalf("could not reach the %q boundary; stopped at %q",
					boundary, step.SagaStage)
			}

			// THE CRASH: nothing else runs. A fresh invocation — with NO TOKEN —
			// must carry the saga to completion.
			err = e.ResumeSaga(conn, stepID, nowMS)
			testsupport.Must(t, err, "resuming from %q: %v", boundary, err)

			final, err := db.GetStep(conn, stepID)
			testsupport.Must(t, err, "GetStep: %v", err)
			if final.InSaga() {
				t.Errorf("saga_stage = %q after resume, want complete", final.SagaStage)
			}
			if final.Status != db.StepDone {
				t.Errorf("status = %q after resume from %q, want %q",
					final.Status, boundary, db.StepDone)
			}

			// EXACTLY-ONCE: the artifact recorded once, and every gate trailed
			// once, however many times the saga was interrupted.
			var artifacts int
			err = conn.QueryRow(
				`SELECT COUNT(*) FROM artifacts WHERE step_id = ? AND kind = 'change-summary'`,
				stepID).Scan(&artifacts)
			testsupport.Must(t, err, "counting artifacts: %v", err)
			if artifacts != 1 {
				t.Errorf("recorded %d change-summary artifacts after a crash at %q, "+
					"want exactly 1", artifacts, boundary)
			}

			results, err := db.GateResultsForStep(conn, final.ID)
			testsupport.Must(t, err, "reading gate results: %v", err)
			if len(results) != 5 {
				t.Errorf("recorded %d gate results after a crash at %q, want 5 — "+
					"a resume must neither skip nor repeat a gate", len(results), boundary)
			}
			seen := map[string]int{}
			for _, r := range results {
				seen[r.Gate]++
			}
			for gate, n := range seen {
				if n != 1 {
					t.Errorf("gate %q recorded %d times after a crash at %q",
						gate, n, boundary)
				}
			}
		})
	}
}

// TestConcurrentResumesAdvanceOnce is §6.8's CAS guarantee at the saga level:
// two engine invocations resuming the same saga produce exactly one advance per
// stage, so the gate trail has no duplicates.
func TestConcurrentResumesAdvanceOnce(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	err = e.stageZero(conn, step, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "stage 0: %v", err)

	const resumers = 4
	var wg sync.WaitGroup
	errs := make([]error, resumers)
	wg.Add(resumers)
	for i := range resumers {
		go func(i int) {
			defer wg.Done()
			errs[i] = e.ResumeSaga(conn, stepID, nowMS)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("resumer %d: %v", i, err)
		}
	}

	final, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if final.Status != db.StepDone {
		t.Errorf("status = %q, want %q", final.Status, db.StepDone)
	}

	results, err := db.GateResultsForStep(conn, final.ID)
	testsupport.Must(t, err, "reading gate results: %v", err)
	if len(results) != 5 {
		t.Errorf("recorded %d gate results after %d concurrent resumers, want 5 — "+
			"the saga_stage CAS must admit exactly one advance per stage",
			len(results), resumers)
	}
}

// TestEveryStageRefreshesActivity is §6.8's "every stage commit refreshing the
// step's activity clock", at the saga level rather than the db one.
func TestEveryStageRefreshesActivity(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	err = e.stageZero(conn, step, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "stage 0: %v", err)

	at := nowMS
	for {
		step, err = db.GetStep(conn, stepID)
		testsupport.Must(t, err, "GetStep: %v", err)
		if !step.InSaga() {
			break
		}
		at += 1000
		before := step.SagaStage
		_, err := e.advanceOne(conn, step, at)
		testsupport.Must(t, err, "advancing from %q: %v", before, err)

		after, err := db.GetStep(conn, stepID)
		testsupport.Must(t, err, "GetStep: %v", err)
		if after.ActivityMS == nil || *after.ActivityMS != at {
			t.Errorf("after the stage following %q: activity_ms = %v, want %d",
				before, after.ActivityMS, at)
		}
	}
}

// TestArtifactOverCapIsRefused is R12, and the refusal WRITES NOTHING —
// asserted by a version-unchanged check, as §6.9 requires of every refusal.
func TestArtifactOverCapIsRefused(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	before, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	oversized := make([]byte, db.ArtifactMaxBytes+1)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: oversized, NowMS: nowMS,
	})
	if err == nil {
		t.Fatal("an oversized artifact was accepted")
	}
	code, ok := CodeOf(err)
	if !ok || code != CodeValidation {
		t.Errorf("code = %v, want VALIDATION_ERROR", code)
	}
	// The message names BOTH numbers, so an operator knows by how much.
	msg := err.Error()
	if !strings.Contains(msg, "1048577") || !strings.Contains(msg, "1048576") {
		t.Errorf("the refusal does not name the size and the cap: %s", msg)
	}

	after, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if after.RowVersion != before.RowVersion {
		t.Errorf("row_version moved %d -> %d: a REFUSAL wrote",
			before.RowVersion, after.RowVersion)
	}
	if after.InSaga() {
		t.Errorf("the refused complete entered the saga at %q", after.SagaStage)
	}
}

// TestMalformedPayloadIsRefused is §6.8 stage 0's shape validation: a payload
// must be a JSON array of objects, which is what a threshold aggregates over.
//
// The SCHEMA is S5's. This asserts the shape check exists and the field check
// does NOT — an unknown field must pass at S3.
func TestMalformedPayloadIsRefused(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	// Not an array of objects.
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"),
		Payload: []byte(`{"not": "an array"}`), NowMS: nowMS,
	})
	if err == nil {
		t.Fatal("a non-array payload was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("code = %v, want VALIDATION_ERROR", code)
	}

	// An unknown FIELD passes: field validation against a registered schema is
	// S5's by §1's scope table, and checking it here would mean inventing the
	// schema language S5 is specified to define.
	if err := e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("body"),
		Payload: []byte(`[{"anything": "at all", "nested": {"ok": true}}]`), NowMS: nowMS,
	}); err != nil {
		t.Errorf("a payload with unregistered fields was refused at S3: %v — "+
			"field validation is S5's", err)
	}
}

// TestRunReachesDoneWhenEveryStepIs is the routing transaction's run half: the
// rollup moves the run to `done` only when the last step lands.
func TestRunReachesDoneWhenEveryStepIs(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "summary", "")

	// The run is still active: nine steps remain.
	current, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if current.Status != model.RunActive {
		t.Errorf("run status = %q after one step, want %q", current.Status, model.RunActive)
	}
}
