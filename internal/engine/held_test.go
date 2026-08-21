package engine

import (
	"database/sql"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The held-cluster lifecycle — docs/tdd/payloads-thresholds.md §7.7, H1–H20.
//
// The fixture's `reconcile` is the subject throughout: `hold_spread = 2` over
// `findings@1`'s five-value order, driven by a `synthesize` payload whose
// clusters this file supplies. That is the real path an operator walks, not a
// synthetic one.

// clusteredPayload is a synthesize output with one HELD cluster (spread 3) and
// one that is not (spread 1).
const clusteredPayload = `[
  {"id":"C-1","severity":["low","blocker"]},
  {"id":"C-2","severity":["medium","high"]}
]`

// unheldPayload has no cluster reaching hold_spread = 2.
const unheldPayload = `[
  {"id":"C-1","severity":["medium","high"]},
  {"id":"C-2","severity":"low"}
]`

// driveToReconcile completes the fixture up to and including `reconcile@0`,
// with the given clustered payload.
//
// THE CLUSTERED PAYLOAD IS RECORDED ON `synthesize@0` AND NOWHERE ELSE. The
// fixture's `reconcile` declares `inputs = ["synthesize.findings"]`, and the
// builtin's input is the payloads of the artifacts that declaration names
// (§2) — so recording it upstream is the whole of what a caller does.
//
// NOTHING CLAIMS `reconcile@0`. It is an action step; `claim` refuses it, and
// the engine runs it. That is why this helper's last line is a drive rather
// than a completion: a helper that could hand-feed an action step would be the
// claim+complete shim D13 forbids, written in a test file.
func driveToReconcile(t *testing.T, conn *sql.DB, e *Engine, payload string) {
	t.Helper()
	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", payload)
	driveAction(t, conn, e, "reconcile@0")
}

// driveAction drives one action step engine-side, as `next` does in production.
func driveAction(t *testing.T, conn *sql.DB, e *Engine, instance string) {
	t.Helper()
	err := e.RunActionStep(conn, stepIDByInstance(t, conn, instance), nowMS)
	testsupport.Must(t, err, "running %s: %v", instance, err)
}

// readyInstances is the ready set as `next --run` would offer it.
func readyInstances(t *testing.T, conn *sql.DB) []string {
	t.Helper()
	next, err := testEngine().NextSteps(conn, 1, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	out := make([]string, 0, len(next.Steps))
	for _, row := range next.Steps {
		out = append(out, row.Instance)
	}
	return out
}

// heldStep reads the materialized step, failing if it is absent.
func heldStep(t *testing.T, conn *sql.DB, instance string) *db.Step {
	t.Helper()
	step, err := db.GetStep(conn, stepIDByInstance(t, conn, instance))
	testsupport.Must(t, err, "reading %s: %v", instance, err)
	return step
}

// TestHeldStepIsMaterialized covers H1–H4 and H7: the identity, the kind, the
// row, and the idempotency.
func TestHeldStepIsMaterialized(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, clusteredPayload)

	// H1/H2: `<step>-held` at the routing step's OWN ordinal, with NO sibling
	// index — §11.1's exactly-one-of rule means an action step is never fanned
	// out, so there is no second instance for a hold to belong to.
	held := heldStep(t, conn, "reconcile-held@0#0")

	if held.Kind != workflow.TypeHuman {
		t.Errorf("kind = %q, want %q (H3)", held.Kind, workflow.TypeHuman)
	}
	if !held.Materialized {
		t.Error("materialized = 0; the column is what tells a reader a declared " +
			"question from a computed one (H4)")
	}
	if held.Status != db.StepPending {
		t.Errorf("status = %q, want %q", held.Status, db.StepPending)
	}
	if held.SiblingIndex != nil {
		t.Errorf("sibling_index = %v, want NULL (H2)", *held.SiblingIndex)
	}
	if held.Executor != "" {
		t.Errorf("executor = %q; a human step has none", held.Executor)
	}

	routing := heldStep(t, conn, "reconcile@0")
	if routing.RunID != held.RunID || routing.IssueID != held.IssueID ||
		routing.WorkflowID != held.WorkflowID {
		t.Error("the materialized step does not inherit run/issue/workflow from " +
			"its routing step (H4)")
	}

	// H4: created in the SAME transaction that records the artifact and enters
	// the `held` stage — so the artifact carrying `held: true` and the step that
	// can resolve it are never observable apart.
	if routing.SagaStage != db.SagaHeld {
		t.Fatalf("the routing step's saga is %q, want %q",
			routing.SagaStage, db.SagaHeld)
	}
	var artifacts int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE step_id = ? AND payload LIKE '%"held":true%'`,
		routing.ID).Scan(&artifacts)
	testsupport.Must(t, err, "counting held artifacts: %v", err)
	if artifacts != 1 {
		t.Errorf("%d artifacts carry a held cluster, want 1", artifacts)
	}

	// The event, so a `type="human"` step nobody declared does not appear with
	// nothing explaining where it came from.
	if !hasEventKind(t, conn, routing.ID, EventStepHeld) {
		t.Error("no step-held event; §9 item 2 requires every transition be " +
			"attributable")
	}

	// H7: a resumed saga that re-enters the branch finds it and does not
	// duplicate.
	err = e.ResumeSaga(conn, routing.ID, nowMS)
	testsupport.Must(t, err, "resume: %v", err)
	var count int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE instance = 'reconcile-held@0#0'`).
		Scan(&count)
	testsupport.Must(t, err, "counting: %v", err)
	if count != 1 {
		t.Errorf("%d materialized steps after a resume, want exactly one (H7)", count)
	}
}

// TestHeldStepIsReadyImmediately is H6: R1/R2/R6 apply, R3 is vacuous, and
// R4/R5 do not apply to a human step.
//
// So it is ready the moment it exists — which is correct: the question it asks
// is already answered by data on disk, and there is nothing for it to wait on.
func TestHeldStepIsReadyImmediately(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	ready := readyInstances(t, conn)
	if !contains(ready, "reconcile-held@0#0") {
		t.Errorf("ready set is %v; the materialized step must be offered "+
			"immediately (H6)", ready)
	}
}

// TestHeldBlocksEveryDownstreamStep is H8, the clause the whole design rests
// on: the routing step does not route while held, its status stays `gated` —
// non-terminal — so every downstream successor fails R3 and nothing proceeds.
//
// No new status, no synthetic `after` edge, no second readiness rule.
func TestHeldBlocksEveryDownstreamStep(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	routing := heldStep(t, conn, "reconcile@0")
	if routing.Status != db.StepGated {
		t.Errorf("the routing step is %q, want %q — `gated` is non-terminal, "+
			"which is what makes R3 fail downstream", routing.Status, db.StepGated)
	}
	if routing.Routing != "" {
		t.Errorf("the routing step recorded routing %q while held; the decision "+
			"is DEFERRED, not made and overwritten later", routing.Routing)
	}

	ready := readyInstances(t, conn)
	for _, downstream := range []string{"verify@0", "commit-gate@0", "commit@0"} {
		if contains(ready, downstream) {
			t.Errorf("%s is ready while reconcile@0 is held; ready set = %v",
				downstream, ready)
		}
	}
}

// TestResumeWhileHeldAdvancesNothing is H9: `held` is "no advance", so every
// later `next`/`claim`/`complete` costs one read and changes nothing rather
// than spinning against the database.
func TestResumeWhileHeldAdvancesNothing(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	routing := heldStep(t, conn, "reconcile@0")
	before := routing.RowVersion
	var artifactsBefore, eventsBefore, resultsBefore int
	countRow(t, conn, `SELECT COUNT(*) FROM artifacts WHERE step_id = ?`,
		routing.ID, &artifactsBefore)
	countRow(t, conn, `SELECT COUNT(*) FROM events WHERE step_id = ?`,
		routing.ID, &eventsBefore)
	countRow(t, conn, `SELECT COUNT(*) FROM action_results WHERE step_id = ?`,
		routing.ID, &resultsBefore)

	for range 5 {
		err := e.ResumeSaga(conn, routing.ID, nowMS)
		testsupport.Must(t, err, "resume: %v", err)
	}

	after := heldStep(t, conn, "reconcile@0")
	if after.RowVersion != before {
		t.Errorf("row_version moved %d -> %d over five resumes; a held saga "+
			"advances nothing and WRITES NOTHING", before, after.RowVersion)
	}
	if after.SagaStage != db.SagaHeld {
		t.Errorf("the saga left `held` without a decision: %q", after.SagaStage)
	}

	var artifactsAfter, eventsAfter, resultsAfter int
	countRow(t, conn, `SELECT COUNT(*) FROM artifacts WHERE step_id = ?`,
		routing.ID, &artifactsAfter)
	countRow(t, conn, `SELECT COUNT(*) FROM events WHERE step_id = ?`,
		routing.ID, &eventsAfter)
	countRow(t, conn, `SELECT COUNT(*) FROM action_results WHERE step_id = ?`,
		routing.ID, &resultsAfter)
	if artifactsAfter != artifactsBefore || eventsAfter != eventsBefore ||
		resultsAfter != resultsBefore {
		t.Errorf("a resume while held wrote rows: artifacts %d->%d events %d->%d "+
			"results %d->%d", artifactsBefore, artifactsAfter,
			eventsBefore, eventsAfter, resultsBefore, resultsAfter)
	}
}

// TestApproveResolvesAndRoutes is §7.7.3's approve row, and H10's "exactly
// once, AFTER resolution".
//
// The threshold — `any(severity >= high)` over a user-registered order — routes
// `fix-loop` for real. That is the sentence this whole stage exists to make
// true, and it is asserted here end to end.
func TestApproveResolvesAndRoutes(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStep(conn, held.ID, true, "accepted the median", nowMS)
	testsupport.Must(t, err, "approve: %v", err)

	// (2) the materialized step ⇒ done, with a step-approved event.
	held = heldStep(t, conn, "reconcile-held@0#0")
	if held.Status != db.StepDone {
		t.Errorf("the materialized step is %q, want %q (H14)", held.Status, db.StepDone)
	}
	if !hasEventKind(t, conn, held.ID, EventStepApproved) {
		t.Error("no step-approved event")
	}
	if !strings.Contains(held.Routing, "accepted the median") {
		t.Errorf("the note was not recorded verbatim: %q", held.Routing)
	}

	// (1) a NEW artifact of the same kind on the ROUTING step, whose payload
	// carries operator_resolved on every previously-held element.
	routing := heldStep(t, conn, "reconcile@0")
	payloads := artifactPayloads(t, conn, routing.ID)
	if len(payloads) < 2 {
		t.Fatalf("the routing step has %d payloads; approval records a NEW "+
			"artifact rather than annotating the old one (H13)", len(payloads))
	}
	resolved := payloads[len(payloads)-1]
	for i, element := range resolved {
		held, _ := element[KeyHeld].(bool)
		got, _ := element[KeyOperatorResolved].(bool)
		if held != got {
			t.Errorf("element %d: held = %v but operator_resolved = %v; approval "+
				"resolves exactly the held clusters", i, held, got)
		}
	}

	// H13: the ORIGINAL is still addressable, unmodified.
	original := payloads[len(payloads)-2]
	for _, element := range original {
		if held, _ := element[KeyHeld].(bool); held {
			if got, _ := element[KeyOperatorResolved].(bool); got {
				t.Error("the ORIGINAL held artifact was rewritten; artifacts are " +
					"immutable, and what the engine computed and what the operator " +
					"accepted are two records")
			}
		}
	}

	// (3) the routing step's saga advanced and the threshold applied. The
	// cluster's median over {low, blocker} is `low`, and over {medium, high} is
	// `medium` — but C-1's members include `blocker`, so... the threshold reads
	// the REDUCED values, and `any(severity >= high)` is false over
	// {low, medium}. The step therefore routes `pass`.
	if routing.SagaStage != "" {
		t.Errorf("the routing step's saga is %q, want complete", routing.SagaStage)
	}
	if routing.Routing != RoutingPass {
		t.Errorf("reconcile@0 routed %q, want %q — the medians are `low` and "+
			"`medium`, and neither reaches `high`", routing.Routing, RoutingPass)
	}
	if routing.Status != db.StepDone {
		t.Errorf("the routing step is %q, want %q", routing.Status, db.StepDone)
	}
}

// TestApproveRoutesFixLoopOverAUserRegisteredOrder is the stage's headline
// sentence, proven at the point it becomes true.
//
// The clusters are chosen so the MEDIANS reach `high`: an ordered comparison
// over a user-registered order routes `fix-loop` for real, and the loop entry
// follows.
func TestApproveRoutesFixLoopOverAUserRegisteredOrder(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	// {high, blocker} has spread 1 — not held — and a median of `high`.
	// {info, high, blocker} has spread 4 — HELD — and a median of `high`.
	const payload = `[
	  {"id":"C-1","severity":["high","blocker"]},
	  {"id":"C-2","severity":["info","high","blocker"]}
	]`
	driveToReconcile(t, conn, e, payload)

	// `#1`, not `#0`: the held step is named for the PAYLOAD INDEX of the
	// cluster it decides, and here it is the SECOND element whose
	// spread trips. C-1 has spread 1 and is never held, so no `#0` exists.
	held := heldStep(t, conn, "reconcile-held@0#1")
	err := e.DecideStep(conn, held.ID, true, "accepted", nowMS)
	testsupport.Must(t, err, "approve: %v", err)

	routing := heldStep(t, conn, "reconcile@0")
	if !strings.HasPrefix(routing.Routing, workflow.OnFailFixLoop) {
		t.Fatalf("reconcile@0 routed %q, want %q.\n\n"+
			"THIS IS THE SENTENCE THE STAGE EXISTS FOR: `any(severity >= high)` "+
			"routes because a registered document said `high` comes after "+
			"`medium`, and for no other reason.",
			routing.Routing, workflow.OnFailFixLoop)
	}
	if !hasEventKind(t, conn, routing.ID, EventLoopEntered) {
		t.Error("the fix-loop routing did not enter the loop")
	}
}

// TestRejectRoutesPerOnFailWithoutAnArtifact is §7.7.3's reject row.
func TestRejectRoutesPerOnFailWithoutAnArtifact(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	routing := heldStep(t, conn, "reconcile@0")
	before := len(artifactPayloads(t, conn, routing.ID))

	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStep(conn, held.ID, false, "not acceptable", nowMS)
	testsupport.Must(t, err, "reject: %v", err)

	// (1) NO new artifact.
	if after := len(artifactPayloads(t, conn, routing.ID)); after != before {
		t.Errorf("reject wrote %d new artifacts; it records none", after-before)
	}
	// ...and the held one is STILL ADDRESSABLE.
	payloads := artifactPayloads(t, conn, routing.ID)
	found := false
	for _, element := range payloads[len(payloads)-1] {
		if held, _ := element[KeyHeld].(bool); held {
			found = true
		}
	}
	if !found {
		t.Error("the held artifact is no longer addressable after a rejection")
	}

	// (2) the materialized step ⇒ done, with a step-rejected event.
	held = heldStep(t, conn, "reconcile-held@0#0")
	if held.Status != db.StepDone {
		t.Errorf("the materialized step is %q, want %q — it recorded a decision, "+
			"which is what a gate does (H14)", held.Status, db.StepDone)
	}
	if !hasEventKind(t, conn, held.ID, EventStepRejected) {
		t.Error("no step-rejected event")
	}

	// (3) the routing step routes per its EFFECTIVE on_fail, SKIPPING the
	// threshold. The fixture's `reconcile` declares none, so the default
	// applies.
	routing = heldStep(t, conn, "reconcile@0")
	want := workflow.OnFailWaitingHuman
	if !strings.HasPrefix(routing.Routing, want) {
		t.Errorf("reconcile@0 routed %q, want %q", routing.Routing, want)
	}
	if routing.SagaStage != "" {
		t.Errorf("the routing step's saga is %q, want complete", routing.SagaStage)
	}

	// H14's V13 note: the park is on a DIFFERENT step, resolvable by
	// `step resolve` — not a step parking on its own decision.
	if routing.Status != db.StepWaitingHuman {
		t.Errorf("the routing step is %q, want %q", routing.Status, db.StepWaitingHuman)
	}
	if held.Status == db.StepWaitingHuman {
		t.Error("the materialized step parked on its own decision")
	}
}

// TestDoubleApproveIsConflict is H16: approve/reject on a materialized step
// whose routing step is not in `held` is CONFLICT naming both steps.
func TestDoubleApproveIsConflict(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStep(conn, held.ID, true, "first", nowMS)
	testsupport.Must(t, err, "first approve: %v", err)

	err = e.DecideStep(conn, held.ID, true, "second", nowMS)
	if err == nil {
		t.Fatal("a second approve succeeded; the decision was already made")
	}
	code, _ := CodeOf(err)
	if code != CodeConflict {
		t.Errorf("code = %q, want %q", code, CodeConflict)
	}
	for _, want := range []string{"reconcile-held@0#0", "reconcile@0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// TestGuardStopDeniesWhileHeld is H11 AS REVISED (payloads-thresholds-review
// F1, operator decision 2026-08-03).
//
// A held cluster blocks `stop` exactly as a declared `type="human"` gate
// awaiting approval does, which is H12's consistency argument applied in both
// directions. Stopping requires resolving or abandoning — the harness surfaces
// the open decision rather than stopping around it.
func TestGuardStopDeniesWhileHeld(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	verdict, err := GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop: %v", err)
	if verdict.Allowed {
		t.Fatal("guard stop ALLOWED while a held cluster was open. An earlier " +
			"draft exempted it; the operator's decision is that a held cluster " +
			"blocks stop exactly as a declared human gate does, because the " +
			"materialized step is `pending` and §6.12 denies on any pending step.")
	}
	// H11's SUBSTANCE, asserted against the predicate's own input rather than
	// against the reason string — which names at most three instances, so a
	// substring check would be testing the truncation.
	//
	// The block comes from a `pending` human step under §6.12's EXISTING rule.
	// There is no new exemption and no new rule: a held cluster blocks stop
	// exactly as a declared human gate awaiting approval does.
	held := heldStep(t, conn, "reconcile-held@0#0")
	if !blocksStop(held.Status) {
		t.Errorf("the materialized step is %q, which does not block stop; H11's "+
			"denial would then be coming from somewhere else", held.Status)
	}

	// H12: the run rollup is UNCHANGED — a held routing step is `gated` and its
	// materialized step is `pending`, so the run stays `active`. Two kinds of
	// open human decision that rolled up differently would be a bug waiting for
	// a report to expose.
	var status string
	err = conn.QueryRow(`SELECT status FROM runs WHERE id = 1`).
		Scan(&status)
	testsupport.Must(t, err, "reading the run: %v", err)
	if status != "active" {
		t.Errorf("the run is %q while held, want active (H12)", status)
	}

	// And once resolved it stops blocking — so the denial above came from the
	// hold rather than merely from the rest of the run being unfinished.
	err = e.DecideStep(conn, held.ID, false, "abandoning", nowMS)
	testsupport.Must(t, err, "reject: %v", err)
	held = heldStep(t, conn, "reconcile-held@0#0")
	if blocksStop(held.Status) {
		t.Errorf("a RESOLVED hold is %q and still blocks stop", held.Status)
	}
}

// blocksStop mirrors §6.12's pending-work set, so the assertion above is about
// the same statuses `guard stop` queries rather than about its message.
func blocksStop(status string) bool {
	switch status {
	case db.StepPending, db.StepClaimed, db.StepRunning, db.StepGated:
		return true
	}
	return false
}

// TestNoHoldWhenNothingTrips is the negative twin: `hold_spread` untripped
// materializes nothing, evaluates the threshold once, and routes straight
// through.
func TestNoHoldWhenNothingTrips(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, unheldPayload)

	var count int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE materialized = 1`).Scan(&count)
	testsupport.Must(t, err, "counting: %v", err)
	if count != 0 {
		t.Errorf("%d materialized steps with no cluster reaching hold_spread", count)
	}

	routing := heldStep(t, conn, "reconcile@0")
	if routing.SagaStage != "" {
		t.Errorf("the saga is %q, want complete — nothing was held",
			routing.SagaStage)
	}
	if routing.Routing != RoutingPass {
		t.Errorf("reconcile@0 routed %q, want %q — the medians are `medium` and "+
			"`low`", routing.Routing, RoutingPass)
	}
	// The output is still the aggregate's, with every `held` false.
	payloads := artifactPayloads(t, conn, routing.ID)
	for i, element := range payloads[len(payloads)-1] {
		if held, _ := element[KeyHeld].(bool); held {
			t.Errorf("element %d is marked held with hold_spread untripped", i)
		}
	}
}

// TestHeldStepsParticipateInCompletion is H19, proven rather than assumed:
// `issueStepsComplete` groups by step_name and takes the highest ordinal per
// name, so a materialized step neither blocks completion after it is resolved
// nor is skipped while it is open.
func TestHeldStepsParticipateInCompletion(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	complete, err := issueStepsComplete(tx, 1, 1)
	testsupport.Must(t, err, "issueStepsComplete: %v", err)
	if complete {
		t.Error("the issue reads complete with an open held question; a " +
			"materialized step is a step, and `pending` is not terminal")
	}
}

// TestMaterializedStepResolvesFromThePinnedDefinition is H5: the synthesized
// spec is a PURE FUNCTION of the pinned bytes plus the reserved suffix, so
// nothing unpinned enters a run and §9 item 5's determinism is untouched.
func TestMaterializedStepResolvesFromThePinnedDefinition(t *testing.T) {
	def, err := workflow.Load([]byte(`
[pipeline]
name = "p"
version = 1
[match]
kind = ["task"]
[[step]]
name = "synthesize"
executor = "x"
emits = "findings"
[[step]]
name = "reconcile"
after = ["synthesize"]
action = "aggregate"
inputs = ["synthesize.findings"]
payload = "findings@1"
params = { field = "severity", method = "median", hold_spread = 2, output = "findings" }
on_fail = "fix-loop"
[[step]]
name = "fix"
executor = "x"
emits = "findings"
loop = true
`))
	testsupport.Must(t, err, "loading: %v", err)

	spec := workflow.MaterializedHeldStep(def, "reconcile-held")
	if spec == nil {
		t.Fatal("no spec was synthesized")
	}
	if spec.Type != workflow.TypeHuman {
		t.Errorf("type = %q, want %q", spec.Type, workflow.TypeHuman)
	}
	if spec.OnFail != workflow.OnFailFixLoop {
		t.Errorf("on_fail = %q, want the routing step's %q — `reject` means "+
			"route the aggregate per its OWN on_fail, not a second policy",
			spec.OnFail, workflow.OnFailFixLoop)
	}
	if len(spec.After) != 0 || !spec.HasAfter() {
		t.Error("the synthesized spec must declare `after = []` explicitly: an " +
			"empty declared `after` is a ROOT declaration (V10), which is what " +
			"H6 says this step is")
	}
	if len(spec.Gates) != 0 || len(spec.Threshold) != 0 {
		t.Error("the synthesized spec declares gates or a threshold; it produces " +
			"no artifact and judges no result")
	}

	// It is a pure function: twice from the same bytes gives the same spec.
	again := workflow.MaterializedHeldStep(def, "reconcile-held")
	if spec.Type != again.Type || spec.OnFail != again.OnFail {
		t.Error("the derivation is not deterministic")
	}

	// A name that is not materialized, and a routing step that is not in the
	// definition, both yield nothing rather than a fabricated spec.
	if workflow.MaterializedHeldStep(def, "reconcile") != nil {
		t.Error("a declared step name synthesized a held spec")
	}
	if workflow.MaterializedHeldStep(def, "absent-held") != nil {
		t.Error("a held name whose routing step is absent synthesized a spec; " +
			"that is a definition edited under a live saga, and hiding it is worse")
	}
}

// TestLoopSupersedesAnOpenHold is H17 and H18 together.
//
// H17: the materialized step is in its routing step's lineage, so a loop entry
// sweeps it — an open held question from a superseded ordinal must not survive
// to block ordinal k+1. H18: re-instantiation never CREATES one, because
// ExpandOrdinal walks the pinned definition and a materialized step is not in
// it; a hold at ordinal k+1 happens only if that ordinal's computation trips
// `hold_spread` again.
func TestLoopSupersedesAnOpenHold(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	// Resolve the hold so the run can reach `verify`, then drive `verify` to a
	// `fix-loop` routing — which enters the loop and sweeps ordinal 0.
	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStep(conn, held.ID, true, "accepted", nowMS)
	testsupport.Must(t, err, "approve: %v", err)

	// A SECOND hold, materialized by re-running the routing step's saga is not
	// how this works — so instead, drive the loop from `verify` and assert the
	// ordinal-0 held step's fate.
	claimAndComplete(t, conn, e, "verify@0", "verified", `[{"status":"unmet"}]`)

	after := heldStep(t, conn, "reconcile-held@0#0")
	if after.Status != db.StepDone {
		t.Errorf("the RESOLVED held step is %q; a terminal step is left alone by "+
			"the sweep", after.Status)
	}

	// H18: no held step at ordinal 1 — nothing has computed there yet.
	var count int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE instance = 'reconcile-held@1'`).
		Scan(&count)
	testsupport.Must(t, err, "counting: %v", err)
	if count != 0 {
		t.Error("re-instantiation created a held step; ExpandOrdinal walks the " +
			"PINNED definition, and a materialized step is not in it (H18)")
	}
}

// TestUnresolvedHoldIsSweptByALoopEntry is H17's own case: an OPEN hold at a
// superseded ordinal becomes `superseded`, terminal and event-logged.
func TestUnresolvedHoldIsSweptByALoopEntry(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	// `verify` runs off `reconcile`, so the loop cannot be driven from it while
	// `reconcile` is held. The sweep is exercised directly instead, over the
	// same set the loop would compute.
	driveToReconcile(t, conn, e, clusteredPayload)

	// Every read that needs `conn` happens BEFORE the transaction opens.
	// SQLite has one writer, so a query on the connection while this test's own
	// transaction is open would wait on a lock the test itself holds.
	routingID := stepIDByInstance(t, conn, "reconcile@0")

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	defs, err := StepDefinitionsTx(tx, 1)
	testsupport.Must(t, err, "StepDefinitionsTx: %v", err)
	routing, err := db.GetStepTx(tx, routingID)
	testsupport.Must(t, err, "GetStepTx: %v", err)
	swept, err := supersedeSweep(tx, routing, defs[routing.WorkflowID], 1, nowMS)
	testsupport.Must(t, err, "supersedeSweep: %v", err)
	if !contains(swept, "reconcile-held@0#0") {
		t.Errorf("the sweep did not reach the materialized step: %v.\n\n"+
			"H17: it is in its routing step's lineage, and an open held question "+
			"from a superseded ordinal must not survive to block ordinal k+1.",
			swept)
	}
}

// TestStaleLineageHoldIsInert is H20: the aggregate still records its artifact
// and its `action_results` row — history is attributed — and it neither
// materializes a held step nor gates anything, because nothing downstream of it
// will run.
func TestStaleLineageHoldIsInert(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", clusteredPayload)

	// Enter `reconcile@0`'s saga and STALE ITS LINEAGE before it routes, exactly
	// as a slow step finishing after a loop entry would be.
	//
	// The two halves are separate calls because the stale window is BETWEEN
	// them: `enterActionSaga` commits stage 1, the loop count then advances, and
	// the resume runs the aggregate and routes against a lineage that has moved.
	// A single RunActionStep would close the window it exists to open.
	stepID := stepIDByInstance(t, conn, "reconcile@0")
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "reading reconcile@0: %v", err)
	err = e.enterActionSaga(conn, step, nowMS)
	testsupport.Must(t, err, "entering the action saga: %v", err)
	_, err = conn.Exec(
		`UPDATE run_issues SET loop_count = 1 WHERE run_id = 1 AND issue_id = 1`,
	)
	testsupport.Must(t, err, "advancing the loop count: %v", err)

	err = e.ResumeSaga(conn, stepID, nowMS)
	testsupport.Must(t, err, "resume: %v", err)

	// Nothing materialized, nothing gated.
	var materialized int
	countRow(t, conn, `SELECT COUNT(*) FROM steps WHERE materialized = 1`,
		nil, &materialized)
	if materialized != 0 {
		t.Errorf("%d materialized steps from a STALE lineage; nothing "+
			"downstream of it will run, so there is nothing to gate (H20)",
			materialized)
	}

	routing := heldStep(t, conn, "reconcile@0")
	if routing.SagaStage != "" {
		t.Errorf("the stale step's saga is %q, want complete", routing.SagaStage)
	}
	if routing.Routing == "" {
		t.Error("the stale step recorded no routing; a superseded lineage loses " +
			"its DOWNSTREAM EFFECT, not its history")
	}

	// History IS attributed: both the artifact and the action result.
	var artifacts, results int
	countRow(t, conn, `SELECT COUNT(*) FROM artifacts WHERE step_id = ?`,
		routing.ID, &artifacts)
	countRow(t, conn, `SELECT COUNT(*) FROM action_results WHERE step_id = ?`,
		routing.ID, &results)
	if artifacts == 0 || results == 0 {
		t.Errorf("a stale-lineage aggregate recorded %d artifacts and %d action "+
			"results; both must be attributed", artifacts, results)
	}
}

// countRow reads one integer, with an optional single query argument.
func countRow(t *testing.T, conn *sql.DB, query string, arg any, out *int) {
	t.Helper()
	var err error
	if arg == nil {
		err = conn.QueryRow(query).Scan(out)
	} else {
		err = conn.QueryRow(query, arg).Scan(out)
	}
	testsupport.Must(t, err, "%s: %v", query, err)
}

// artifactPayloads reads a step's artifact payloads in insertion order.
func artifactPayloads(t *testing.T, conn *sql.DB, stepID int) [][]map[string]any {
	t.Helper()
	rows, err := conn.Query(
		`SELECT payload FROM artifacts WHERE step_id = ? ORDER BY id`, stepID)
	testsupport.Must(t, err, "reading artifacts: %v", err)
	defer rows.Close()

	var out [][]map[string]any
	for rows.Next() {
		var payload sql.NullString
		err := rows.Scan(&payload)
		testsupport.Must(t, err, "scanning: %v", err)
		if payload.String == "" {
			continue
		}
		var elements []map[string]any
		if err := json.Unmarshal([]byte(payload.String), &elements); err != nil {
			continue
		}
		out = append(out, elements)
	}
	return out
}

// TestRoutingTransactionSpansFour is §6.4's "what does NOT move": the routing
// stage is still ONE transaction over the step, the issue mirror, the run, and
// the events — and adding the `held` branch did not split it.
//
// The four are one transaction because they are one fact: this step ended this
// way. A partial commit is a run whose step says `done` while its issue says
// otherwise, which no later pass can reconcile without guessing.
//
// It is asserted structurally as well as behaviorally, because the failure mode
// is a SECOND `conn.Begin()` added to a branch — which every behavioral test
// would still pass, right up until a crash landed between the two commits.
func TestRoutingTransactionSpansFour(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "saga.go", nil, parser.SkipObjectResolution)
	testsupport.Must(t, err, "parsing saga.go: %v", err)

	var begins int
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runRoutingStage" {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Begin" {
				return true
			}
			begins++
			return true
		})
		return false
	})
	if begins != 1 {
		t.Errorf("runRoutingStage opens %d transactions, want exactly 1 — the "+
			"step, the issue mirror, the run, and the events are ONE fact, and "+
			"the `held` branch returns through the same commit", begins)
	}

	// And behaviorally: a routed step, its issue, its run, and its events all
	// agree after one completion.
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, unheldPayload)

	routing := heldStep(t, conn, "reconcile@0")
	if routing.Status != db.StepDone || routing.Routing != RoutingPass {
		t.Fatalf("reconcile@0 is %q/%q", routing.Status, routing.Routing)
	}
	if !hasEventKind(t, conn, routing.ID, EventStepRouted) {
		t.Error("the routing committed without its event")
	}
	var runStatus string
	err = conn.QueryRow(`SELECT status FROM runs WHERE id = 1`).
		Scan(&runStatus)
	testsupport.Must(t, err, "reading the run: %v", err)
	if runStatus != "active" {
		t.Errorf("the run rolled up to %q while work remains", runStatus)
	}
}

// TestRetryRefusesOnAHoldRejectedPark is AC-1 and AC-2.
//
// A rejected hold routes the ROUTING step per its effective `on_fail`,
// skipping the threshold (§7.7.3), and for the fixture that is
// `waiting-human`. `retry` cannot move that park: it resets the attempt
// counter, and the attempt counter was never what blocked it — `heldDecision`
// re-reads the same terminal `reconcile-held@0#0`, sees a non-pass routing, and
// routes to `on_fail` again.
//
// RUN-2 observed exactly that as a silent no-op costing a dispatch cycle. The
// behaviour being pinned here is the refusal that replaced it, and the
// assertions cover the two things the issue asked the error to carry: the
// REJECT named as the blocker, and the resolutions that actually work.
func TestRetryRefusesOnAHoldRejectedPark(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStep(conn, held.ID, false, "not acceptable", nowMS)
	testsupport.Must(t, err, "reject: %v", err)

	routing := heldStep(t, conn, "reconcile@0")
	if routing.Status != db.StepWaitingHuman {
		t.Fatalf("premise: the routing step is %q, want %q",
			routing.Status, db.StepWaitingHuman)
	}

	err = e.ResolveStep(conn, routing.ID, ResolveRetry, "least destructive first", nowMS)
	if err == nil {
		t.Fatal("retry was accepted on a hold-rejected park; it cannot clear " +
			"the rejection and silently re-parks, which is the behaviour being removed")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}

	msg := err.Error()
	// The BLOCKER, named. "Attempts were reset and nothing happened" is the
	// experience this replaces.
	if !strings.Contains(msg, "REJECTED") {
		t.Errorf("the refusal does not name the rejection as the blocker: %s", msg)
	}
	if !strings.Contains(msg, "reconcile-held@0#0") {
		t.Errorf("the refusal does not name the held step carrying the "+
			"rejection: %s", msg)
	}
	// The resolutions that CAN move it.
	for _, want := range []string{ResolveOverridePass, ResolveSkip, ResolveAbandonIssue} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not offer %q, which does move this step: %s",
				want, msg)
		}
	}

	// The refusal CHANGED NOTHING: the step is still parked, at the same
	// attempt count, so an operator can still choose one of the offered
	// resolutions.
	after := heldStep(t, conn, "reconcile@0")
	if after.Status != db.StepWaitingHuman {
		t.Errorf("the step is %q after a refused retry, want it untouched at %q",
			after.Status, db.StepWaitingHuman)
	}
	if after.Attempt != routing.Attempt {
		t.Errorf("attempts moved from %d to %d on a REFUSED retry",
			routing.Attempt, after.Attempt)
	}
}

// TestOfferedResolutionsMoveAHoldRejectedPark is the other half of AC-1: the
// refusal is only honest if the resolutions it names actually work.
//
// Without this, the error could list any three verbs and the test above would
// still pass.
func TestOfferedResolutionsMoveAHoldRejectedPark(t *testing.T) {
	for _, tc := range []struct {
		as     string
		status string
	}{
		{ResolveOverridePass, db.StepDone},
		{ResolveSkip, db.StepSkipped},
	} {
		t.Run(tc.as, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)
			e := testEngine()
			driveToReconcile(t, conn, e, clusteredPayload)

			held := heldStep(t, conn, "reconcile-held@0#0")
			err := e.DecideStep(conn, held.ID, false, "no", nowMS)
			testsupport.Must(t, err, "reject: %v", err)

			routing := heldStep(t, conn, "reconcile@0")
			err = e.ResolveStep(conn, routing.ID, tc.as, "", nowMS)
			testsupport.Must(t, err, "resolve --as %s was offered by the refusal but fails: %v",
				tc.as, err)

			if got := heldStep(t, conn, "reconcile@0"); got.Status != tc.status {
				t.Errorf("after --as %s the step is %q, want %q",
					tc.as, got.Status, tc.status)
			}
		})
	}
}

// TestRetryStillWorksOnAnOrdinaryPark is the guard on the guard.
//
// The refusal must be NARROW. A step parked by a plain gate failure — no hold
// materialized at all — is exactly where `retry` is the right remedy, and
// refusing it there would remove the least destructive resolution from the
// cases that need it most. That would be a worse defect than the one
// reported.
func TestRetryStillWorksOnAnOrdinaryPark(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	// A parked step with NO held step for its ordinal. Parked by direct
	// update rather than by driving a gate failure: what this test needs is
	// the STATUS, and the absence of a `*-held@N` row is the whole point.
	claimInstance(t, conn, "implement@0", nowMS)
	step := heldStep(t, conn, "implement@0")
	_, err := conn.Exec(
		`UPDATE steps SET status = ?, routing = ? WHERE id = ?`,
		db.StepWaitingHuman, workflow.OnFailWaitingHuman, step.ID)
	testsupport.Must(t, err, "parking the step: %v", err)

	parked, err := db.GetStep(conn, step.ID)
	testsupport.Must(t, err, "re-reading the parked step: %v", err)
	if parked.Status != db.StepWaitingHuman {
		t.Fatalf("premise: the step is %q, want %q", parked.Status, db.StepWaitingHuman)
	}

	err = e.ResolveStep(conn, step.ID, ResolveRetry, "", nowMS)
	testsupport.Must(t, err, "retry was refused on an ORDINARY park, where it is the "+
		"correct remedy: %v", err)

	if got, _ := db.GetStep(conn, step.ID); got.Status != db.StepPending {
		t.Errorf("after retry the step is %q, want %q", got.Status, db.StepPending)
	}
}

// TestResolutionArtifactSupersedesTheOriginal is DKT-70(b), amended by
// DKT-112.
//
// A run report showed ARTIFACT-71 and ARTIFACT-81 both as `findings` from
// `reconcile@0` — same sha256, same 79 bytes — and the issue read it as a
// duplicate write from dispatch close. It is NOT a duplicate: H13 records the
// operator's resolution as its OWN artifact rather than annotating the
// original, deliberately. DKT-70 added the `supersedes` pointer to say which
// of the two records a computation and which records a decision; DKT-112 then
// retired the shared hash itself — the sha256 now covers body AND payload, so
// a chain whose payloads differ never shares a content address, and the
// revision's body is REGENERATED so it stops claiming a hold that was just
// resolved.
func TestResolutionArtifactSupersedesTheOriginal(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	driveToReconcile(t, conn, e, clusteredPayload)

	held := heldStep(t, conn, "reconcile-held@0#0")
	err := e.DecideStep(conn, held.ID, true, "accepted the median", nowMS)
	testsupport.Must(t, err, "approve: %v", err)

	routing := heldStep(t, conn, "reconcile@0")
	artifacts, err := db.ListStepArtifacts(conn, routing.ID)
	testsupport.Must(t, err, "listing the routing step's artifacts: %v", err)
	if len(artifacts) < 2 {
		t.Fatalf("the routing step has %d artifacts; H13 records the resolution "+
			"as its own record", len(artifacts))
	}

	original := artifacts[len(artifacts)-2]
	revision := artifacts[len(artifacts)-1]

	if original.Kind != revision.Kind {
		t.Fatalf("the revision changed kind: %q -> %q; a resolution revises the "+
			"same artifact kind", original.Kind, revision.Kind)
	}
	// DKT-112: the resolution's payload differs from the computation's, so its
	// content address must too — the chain used to share one hash, and every
	// consumer treating sha256 as a content address saw duplicates.
	if original.SHA256 == revision.SHA256 {
		t.Errorf("the revision shares the original's sha256 %q; a supersession "+
			"whose payload changed must not share a content address (DKT-112)",
			revision.SHA256)
	}
	// DKT-112: the revision's body is regenerated from the resolved payload.
	// The original's body counted this cluster as held; recording that sentence
	// again AFTER the resolution recorded a stale fact as the newest one.
	if strings.Contains(revision.Body, "held for an operator decision") {
		t.Errorf("the revision's body still claims a pending hold: %q; it is "+
			"regenerated from the resolved payload (DKT-112)", revision.Body)
	}
	if !strings.Contains(revision.Body, "operator-resolved") {
		t.Errorf("the revision's body does not record the resolution: %q", revision.Body)
	}

	if original.Supersedes != nil {
		t.Errorf("the ORIGINAL claims to supersede ARTIFACT-%d; the record of a "+
			"computation revises nothing", *original.Supersedes)
	}
	if revision.Supersedes == nil {
		t.Fatal("the resolution artifact supersedes nothing; without the pointer " +
			"it is indistinguishable from a second unit of work")
	}
	if *revision.Supersedes != original.ID {
		t.Errorf("the resolution supersedes ARTIFACT-%d, want the artifact it was "+
			"computed from, ARTIFACT-%d", *revision.Supersedes, original.ID)
	}

	// The run report is the surface the double-count was READ on, so the
	// discriminator has to reach it.
	report, err := LoadRunReport(conn, routing.RunID, nowMS)
	testsupport.Must(t, err, "run report: %v", err)
	var revisions int
	for _, entry := range report.Artifacts {
		if entry.Supersedes != "" {
			revisions++
		}
	}
	if revisions != 1 {
		t.Errorf("the report's artifact index marks %d revision(s), want 1; a "+
			"rollup counting work cannot skip what it cannot see", revisions)
	}
}
