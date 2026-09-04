package engine

import (
	"database/sql"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The loop table — TDD §7.7, one case per §7.2 row.
//
// The fixture's loop is driven at `verify`, not at `reconcile`, and §7.7's QA
// table says why: `reconcile`'s threshold is `any(severity >= high)` over an
// EMPTY stub payload, which T4 short-circuits to false before the ordered
// comparison is attempted. `verify`'s is `any(status == unmet)` — an equality
// operator, live at S3 per T1 — over a payload a caller supplies. These tests
// drive the same one, so a change to T4 breaks the threshold tests rather than
// making the loop tests mysteriously stop looping.

// unmetPayload routes `verify` to `fix-loop`: `any(status == unmet)` is true.
const unmetPayload = `[{"status":"unmet"}]`

// metPayload routes `verify` to `pass`: no threshold matches.
const metPayload = `[{"status":"met"}]`

// roundReport is ONE ROUND'S verdict body, naming the ordinal that produced it.
//
// It exists for DKT-589 the way driveFixtureRound exists for DKT-340: a routing
// step that records the BYTE-IDENTICAL artifact at two consecutive ordinals now
// parks the loop, so a fixture reusing one constant `"report"` across rounds
// parks at the first repeat and never reaches the bound, the sweep, or the
// input binding those tests are actually about. A real round's report differs
// from the last one's whenever the round found anything new; these do too. A
// test whose SUBJECT is the identical-verdict park uses one constant
// deliberately — see dkt589_test.go.
func roundReport(ordinal int) string {
	return fmt.Sprintf("the report of round %d", ordinal)
}

// ---------------------------------------------------------------------------
// §11.3 identity: instance rendering
// ---------------------------------------------------------------------------

// TestInstanceRenderingAcrossOrdinalsAndFanout covers all four combinations of
// (ordinal 0/n) × (fanned/not) — §7.2's first row.
//
// The `#i` suffix is ABSENT when the step is not fanned out, which is the half
// a formatter that always appended an index would get wrong: `implement@0#0`
// would be a different identity from the `implement@0` every other surface
// names, and the two would diverge silently.
func TestInstanceRenderingAcrossOrdinalsAndFanout(t *testing.T) {
	index := func(i int) *int { return &i }

	for _, tc := range []struct {
		name     string
		step     string
		ordinal  int
		sibling  *int
		expected string
	}{
		{"ordinal 0, not fanned", "implement", 0, nil, "implement@0"},
		{"ordinal n, not fanned", "fix", 1, nil, "fix@1"},
		{"ordinal 0, fanned", "review", 0, index(2), "review@0#2"},
		{"ordinal n, fanned", "review", 1, index(3), "review@1#3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := workflow.RenderInstance(tc.step, tc.ordinal, tc.sibling)
			if got != tc.expected {
				t.Errorf("RenderInstance = %q, want %q", got, tc.expected)
			}

			// And the rendering round-trips: ParseInstance is the inverse, so a
			// caller reading the stored column recovers exactly the parts that
			// produced it.
			name, ordinal, sibling, err := workflow.ParseInstance(got)
			testsupport.Must(t, err, "ParseInstance(%q): %v", got, err)
			if name != tc.step || ordinal != tc.ordinal {
				t.Errorf("ParseInstance(%q) = (%q, %d), want (%q, %d)",
					got, name, ordinal, tc.step, tc.ordinal)
			}
			switch {
			case tc.sibling == nil && sibling != nil:
				t.Errorf("ParseInstance(%q) yielded sibling %d, want none", got, *sibling)
			case tc.sibling != nil && sibling == nil:
				t.Errorf("ParseInstance(%q) yielded no sibling, want %d", got, *tc.sibling)
			case tc.sibling != nil && *sibling != *tc.sibling:
				t.Errorf("ParseInstance(%q) sibling = %d, want %d", got, *sibling, *tc.sibling)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// §11.3 (1): the counter, and the bound
// ---------------------------------------------------------------------------

// TestLoopCountIncrementsPerEntry is §11.3 (1)'s first half: a `fix-loop`
// routing raises the ISSUE's counter.
func TestLoopCountIncrementsPerEntry(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	if got := loopCount(t, conn, run.ID, issue); got != 0 {
		t.Fatalf("loop_count = %d on a fresh run, want 0", got)
	}

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Errorf("loop_count = %d after one fix-loop routing, want 1", got)
	}
	if got := stepRouting(t, conn, "verify@0"); got != workflow.OnFailFixLoop {
		t.Errorf("verify@0 routing = %q, want %q", got, workflow.OnFailFixLoop)
	}

	// The counter is the ISSUE's (§11.3 (1)), so it lives on run_issues and not
	// on the step that routed.
	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Errorf("run_issues.loop_count = %d, want 1", got)
	}
}

// TestLoopsAreBoundedByConstruction is §11.3 (1)'s second half and §7.2's
// "loops are bounded by construction" row: with `max_fix_loops = 2`, a THIRD
// entry is impossible however many times routing fires.
//
// This is the test that a bound implemented as "warn and continue" passes and a
// correct one does not: the third `fix-loop` must not produce a `fix@3`, and the
// step that routed it must park `waiting-human` for an operator rather than
// looping on.
func TestLoopsAreBoundedByConstruction(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	// Loop 1 and loop 2, both legal at max_fix_loops = 2.
	for k := range 2 {
		driveToVerify(t, conn, e, k)
		claimAndComplete(t, conn, e, fmt.Sprintf("verify@%d", k), roundReport(k), unmetPayload)

		if got := loopCount(t, conn, run.ID, issue); got != k+1 {
			t.Fatalf("loop_count = %d after loop %d, want %d", got, k+1, k+1)
		}
		if !stepExists(t, conn, fmt.Sprintf("fix@%d", k+1)) {
			t.Fatalf("fix@%d was not instantiated by loop %d", k+1, k+1)
		}
	}

	// The third attempt: routing fires, and the bound converts it.
	driveToVerify(t, conn, e, 2)
	claimAndComplete(t, conn, e, "verify@2", roundReport(2), unmetPayload)

	if stepExists(t, conn, "fix@3") {
		t.Error("fix@3 exists; max_fix_loops = 2 must make a third entry impossible")
	}
	if got := stepStatus(t, conn, "verify@2"); got != db.StepWaitingHuman {
		t.Errorf("verify@2 status = %q, want %q — an exhausted loop parks",
			got, db.StepWaitingHuman)
	}

	// The routing RECORDS the reason, so the operator resolving the park can
	// see what could not proceed rather than finding a step parked for no
	// stated cause.
	routing := stepRoutingRaw(t, conn, "verify@2")
	if !strings.Contains(routing, "max_fix_loops") {
		t.Errorf("verify@2 routing = %q, want it to name max_fix_loops", routing)
	}
}

// ---------------------------------------------------------------------------
// §11.3 (2) / §7.3: the supersede sweep
// ---------------------------------------------------------------------------

// TestSupersedeSweepStatusTable is §7.7's "supersede table": for each PERSISTED
// status, whether a downstream instance below the new ordinal is superseded or
// left alone.
//
// Nine persisted statuses (§6.2 — `ready` is computed, never stored), plus the
// computed-ready row §7.7 asks for separately. The table is the specification;
// stating it as data rather than as prose is what makes a future edit to the
// sweep fail on the row it changed.
func TestSupersedeSweepStatusTable(t *testing.T) {
	for _, tc := range []struct {
		status     string
		superseded bool
		why        string
	}{
		{db.StepPending, true, "unclaimed: the loop replaces it"},
		{db.StepClaimed, false, "a claimed instance finishes; its routing is inert"},
		{db.StepRunning, false, "a running instance finishes; its routing is inert"},
		{db.StepGated, false, "the saga owns it; killing it mid-saga would strand the artifact"},
		{db.StepWaitingHuman, false, "an operator's open question is not the loop's to close"},
		{db.StepDone, false, "terminal, immutable, and addressable (§11.3)"},
		{db.StepSkipped, false, "terminal"},
		{db.StepFailedRouted, false, "terminal"},
		{db.StepSuperseded, false, "terminal; an earlier entry already swept it"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)
			e := testEngine()

			// The chain runs FIRST, then `commit-gate@0` is forced to the status
			// under test. The order matters for the `waiting-human` row: a
			// parked step rolls the RUN up to `waiting-human` (§6.8), and R1
			// then refuses every claim — so forcing the status before driving
			// the chain would fail on the setup rather than on the sweep.
			driveToVerify(t, conn, e, 0)

			// `commit-gate` is downstream of `after_loop = "review"` and is
			// unclaimed at ordinal 0 — the sweep's natural subject.
			execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ?`,
				tc.status, "commit-gate@0")

			claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

			got := stepStatus(t, conn, "commit-gate@0")
			if tc.superseded {
				if got != db.StepSuperseded {
					t.Errorf("commit-gate@0 (%s) = %q after the sweep, want %q — %s",
						tc.status, got, db.StepSuperseded, tc.why)
				}
				return
			}
			if got != tc.status {
				t.Errorf("commit-gate@0 (%s) = %q after the sweep, want it left alone — %s",
					tc.status, got, tc.why)
			}
		})
	}
}

// TestSupersedeSweepTakesAComputedReadyInstance is §7.7's extra row: a step that
// is COMPUTED ready at sweep time must be superseded like any other unclaimed
// instance.
//
// `ready` is never stored (§6.2), so such a step is a `pending` row that the
// predicate happens to answer true for. A sweep keyed off a stored `ready`
// would find none and silently skip exactly the instances most likely to be
// picked up by the next `next` — the ones a loop entry most needs to stop.
func TestSupersedeSweepTakesAComputedReadyInstance(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)

	// `commit-gate@0` is not ready (verify@0 is not done), so make the case
	// honestly: verify@0 completing to `pass` first would make it ready. Assert
	// readiness through the predicate itself rather than assuming it.
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ?`,
		db.StepDone, "verify@0")
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		gate := stepNamed(t, sched, "commit-gate@0")
		if ready, cond := sched.Ready(gate); !ready {
			t.Fatalf("commit-gate@0 is not computed-ready (%s); the case is not set up", cond)
		}
		if gate.Status != db.StepPending {
			t.Fatalf("commit-gate@0 stored status = %q, want %q — `ready` is computed",
				gate.Status, db.StepPending)
		}
	})

	// Now enter the loop from a fresh verify instance.
	execSQL(t, conn, `UPDATE steps SET status = ?, saga_stage = NULL, routing = NULL
	                   WHERE instance = ?`, db.StepPending, "verify@0")
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	if got := stepStatus(t, conn, "commit-gate@0"); got != db.StepSuperseded {
		t.Errorf("a computed-ready commit-gate@0 = %q after the sweep, want %q",
			got, db.StepSuperseded)
	}
}

// TestSupersedeSweepIsScopedToAfterLoopDownstream proves the sweep's SET is
// §7.3 (1)'s and not "everything below the ordinal".
//
// `implement` is upstream of `after_loop = "review"`. It is `done`, so the
// status rule would leave it alone anyway — the sharper case is that a sweep
// scoped wrongly would also take steps that are neither downstream nor
// terminal. `verify@0` itself, and the ordinal-0 `review` siblings, ARE
// downstream and demonstrate the boundary from the other side.
func TestSupersedeSweepIsScopedToAfterLoopDownstream(t *testing.T) {
	conn := mustDB(t)
	_, _ = activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	// Upstream of after_loop: untouched, and still `done` — prior instances
	// remain immutable (§11.3).
	if got := stepStatus(t, conn, "implement@0"); got != db.StepDone {
		t.Errorf("implement@0 = %q; it is upstream of after_loop and must not be swept", got)
	}

	// Downstream and unclaimed: superseded.
	for _, instance := range []string{"commit-gate@0", "commit@0"} {
		if got := stepStatus(t, conn, instance); got != db.StepSuperseded {
			t.Errorf("%s = %q, want %q — it is downstream of after_loop and unclaimed",
				instance, got, db.StepSuperseded)
		}
	}
}

// TestPriorArtifactsRemainAddressableAfterSupersede is §7.2's "prior instances
// and artifacts remain immutable and addressable" row.
//
// `superseded` is A STATUS, NOT A DELETION. The distinction matters for the
// ledger: a run that looped three times must still be able to show what each
// ordinal produced, and an implementation that cleaned up superseded rows would
// destroy the evidence the retro pipeline reads.
func TestPriorArtifactsRemainAddressableAfterSupersede(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)

	before := artifactIDsFor(t, conn, run.ID)
	if len(before) == 0 {
		t.Fatal("no artifacts recorded before the loop; the case is not set up")
	}

	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	after := artifactIDsFor(t, conn, run.ID)
	for id, kind := range before {
		gotKind, ok := after[id]
		if !ok {
			t.Errorf("artifact %d (%s) disappeared across the loop entry", id, kind)
			continue
		}
		if gotKind != kind {
			t.Errorf("artifact %d kind = %q after the loop, want %q", id, gotKind, kind)
		}
	}

	// And the superseded STEP rows are still there, addressable by instance.
	if !stepExists(t, conn, "commit-gate@0") {
		t.Error("commit-gate@0 was deleted; supersede is a status, not a deletion")
	}
}

// ---------------------------------------------------------------------------
// §7.3 (3): the inert half
// ---------------------------------------------------------------------------

// TestStaleLineageRoutingIsInert is §7.3's named test, and the subtle half of
// §11.3 (2).
//
// A slow `verify@0` that was CLAIMED before the sweep — so the sweep left it
// alone — completes after `fix@1` has started. Its routing is recorded for the
// ledger, and applies NO downstream effect: no supersede, no re-expansion, no
// issue status change, and above all NO SECOND LOOP INCREMENT.
//
// Without the ordinal guard this is a real race, not a theoretical one: the
// stale routing would enter a loop for findings ordinal 1 has already moved
// past, and at `max_fix_loops = 2` it would burn a loop nobody asked for.
func TestStaleLineageRoutingIsInert(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	// Enter loop 1 through `verify@0`, then reset that instance to a claimed,
	// mid-flight state: the shape a slow sibling has when the sweep passes it
	// by. Its ordinal (0) is now below the issue's loop_count (1).
	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Fatalf("loop_count = %d after the first entry, want 1", got)
	}
	supersededBefore := countStatus(t, conn, db.StepSuperseded)

	execSQL(t, conn, `UPDATE steps SET status = ?, saga_stage = NULL, routing = NULL
	                   WHERE instance = ?`, db.StepPending, "verify@0")

	// The stale completion, routing `fix-loop` on ordinal-0 findings.
	claimAndComplete(t, conn, e, "verify@0", "stale report", unmetPayload)

	// NO loop increment: the issue is still at loop 1.
	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Errorf("loop_count = %d after a stale routing, want 1 — a superseded "+
			"lineage must not enter a loop", got)
	}
	// NO re-expansion.
	if stepExists(t, conn, "fix@2") {
		t.Error("fix@2 was instantiated by a stale routing")
	}
	// NO further supersede.
	if got := countStatus(t, conn, db.StepSuperseded); got != supersededBefore {
		t.Errorf("superseded count = %d after a stale routing, want %d",
			got, supersededBefore)
	}

	// But the routing IS recorded — "records the routing on the step for the
	// ledger" (§7.3 (3)). Inert means no downstream effect, not unrecorded.
	if got := stepRouting(t, conn, "verify@0"); got != workflow.OnFailFixLoop {
		t.Errorf("verify@0 routing = %q, want it recorded as %q",
			got, workflow.OnFailFixLoop)
	}
	// And the saga CLOSED: an inert routing that skipped its own completion
	// would leave the step resumed forever by every later engine invocation.
	step, err := db.GetStep(conn, stepIDByInstance(t, conn, "verify@0"))
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.InSaga() {
		t.Errorf("saga_stage = %q after an inert routing, want closed", step.SagaStage)
	}
}

// ---------------------------------------------------------------------------
// §11.3 (3) / §7.4: ordinal-scoped input binding
// ---------------------------------------------------------------------------

// TestOrdinalScopedInputBindingFallsBackPerInput is §7.4's central case, and
// the fixture's `fix` step exactly.
//
// `fix@1` declares `["reconcile.findings", "implement.change-summary"]`.
// `reconcile` re-ran at ordinal 1, so that input binds FRESH at ordinal 1;
// `implement` is upstream of `after_loop` and never re-runs, so its input falls
// back to ordinal 0. TWO INPUTS OF ONE STEP, BOUND AT DIFFERENT ORDINALS —
// which is why §11.3 (3) says "per input" and why a per-step rule cannot be
// right about both.
func TestOrdinalScopedInputBindingFallsBackPerInput(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	// Enter loop 1, then run ordinal 1 as far as `reconcile@1` so both inputs
	// have candidates and they sit at different ordinals.
	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", roundReport(0), unmetPayload)
	// Ordinal 1 is driven by hand here rather than through driveToVerify, so
	// the stub tree is moved by hand too — otherwise round 1 changes nothing
	// and DKT-340's guard correctly refuses to mint `fix@2`. Each verify
	// records its own report for DKT-589's sibling guard, for the same reason.
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix summary", "")
	completeReviewFanout(t, conn, e, 1)
	claimAndComplete(t, conn, e, "synthesize@1", "the synthesis", "")
	driveAction(t, conn, e, "reconcile@1")

	// A second loop entry, so there is a `fix@2` whose inputs we can inspect
	// with ordinal-1 and ordinal-0 candidates both present.
	claimAndComplete(t, conn, e, "verify@1", roundReport(1), unmetPayload)

	inputs := contextInputs(t, conn, run.ID, "fix@2")

	// `reconcile.findings` binds at the HIGHEST available ordinal (2 has not
	// run; 1 has), and `implement.change-summary` at 0 — the only one there is.
	assertInputOrdinal(t, inputs, "reconcile", 1)
	assertInputOrdinal(t, inputs, "implement", 0)
}

// TestOrdinalScopedInputBindingPrefersItsOwnOrdinal is §7.4 (1): among `done`
// instances, the step's OWN ordinal wins over any earlier one.
//
// The fallback exists for inputs with nothing at ordinal k; it must not fire
// when ordinal k has produced something, or a loop would feed itself the very
// findings it was entered to address.
func TestOrdinalScopedInputBindingPrefersItsOwnOrdinal(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)
	claimAndComplete(t, conn, e, "fix@1", "the fix summary", "")
	completeReviewFanout(t, conn, e, 1)

	// `synthesize@1` declares `inputs = ["review.*"]`. Both ordinals have four
	// `done` review siblings now, and the ordinal-1 set is the right answer.
	inputs := contextInputs(t, conn, run.ID, "synthesize@1")
	if len(inputs) == 0 {
		t.Fatal("synthesize@1 bound no inputs")
	}
	for _, in := range inputs {
		_, ordinal, _, err := workflow.ParseInstance(in.ProducerStep)
		testsupport.Must(t, err, "ParseInstance(%q): %v", in.ProducerStep, err)
		if ordinal != 1 {
			t.Errorf("synthesize@1 bound %s at ordinal %d, want 1 — its own "+
				"ordinal has `done` instances and the fallback must not fire",
				in.ProducerStep, ordinal)
		}
	}
}

// TestLoopReentryRebindsInputToLoopProducer is DKT-12: a step downstream of
// `after_loop` whose declared input names the loop's ORIGINAL producer
// (`implement.change-summary`) must, on re-entry, resolve to the loop BODY's
// latest emit (`fix@N`) — not `implement`'s stale ordinal-0 one.
//
// RUN-1 graph-engine measured the cost of the bug this pins: four re-review
// judges reconstructed and judged the superseded `implement` commit instead
// of `fix`'s, because their packets' only commit reference was `implement`'s
// change-summary (DKT-12's problem statement).
func TestLoopReentryRebindsInputToLoopProducer(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)
	// The loop body's fresh change-summary — the artifact `review@1` must see
	// in place of `implement@0`'s.
	claimAndComplete(t, conn, e, "fix@1", "the fix summary", "")

	inputs := contextInputs(t, conn, run.ID, "review@1#0")

	var found bool
	for _, in := range inputs {
		if in.Kind != "change-summary" {
			continue
		}
		found = true
		name, ordinal, _, err := workflow.ParseInstance(in.ProducerStep)
		testsupport.Must(t, err, "ParseInstance(%q): %v", in.ProducerStep, err)
		if name != "fix" || ordinal != 1 {
			t.Errorf("review@1#0's change-summary input came from %s, want fix@1 "+
				"(the loop's latest emit, not implement@0's stale one)", in.ProducerStep)
		}
		if in.Body != "the fix summary" {
			t.Errorf("review@1#0's change-summary body = %q, want the fix's own body", in.Body)
		}
	}
	if !found {
		t.Fatal("review@1#0 bound no change-summary input")
	}
}

// ---------------------------------------------------------------------------
// §11.3 (4): re-instantiation
// ---------------------------------------------------------------------------

// TestAfterLoopChainReinstantiatesAtOrdinalK is §11.3 (4): when the loop's gates
// pass, `after_loop` AND ITS TRANSITIVE DOWNSTREAM re-instantiate at ordinal k.
//
// The transitive half is the one a naive implementation drops. Re-instantiating
// only `review` would leave `synthesize`/`reconcile`/`verify`/`commit-gate`/
// `commit` with no ordinal-1 instance, so the chain would run one step and
// stop — and the run would sit at "every step terminal" with work undone.
func TestAfterLoopChainReinstantiatesAtOrdinalK(t *testing.T) {
	conn := mustDB(t)
	_, _ = activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	// The loop body itself (clause 3).
	if !stepExists(t, conn, "fix@1") {
		t.Error("fix@1 (loop = true) was not instantiated at ordinal 1")
	}

	// `after_loop = "review"` and everything transitively after it (clause 4),
	// with the fanout re-expanded per hint.
	for _, instance := range []string{
		"review@1#0", "review@1#1", "review@1#2", "review@1#3",
		"synthesize@1", "reconcile@1", "verify@1", "commit-gate@1", "commit@1",
	} {
		if !stepExists(t, conn, instance) {
			t.Errorf("%s was not re-instantiated at ordinal 1", instance)
		}
	}

	// UPSTREAM of after_loop is NOT re-instantiated: `implement` does not re-run,
	// which is exactly what makes §7.4's per-input fallback necessary.
	if stepExists(t, conn, "implement@1") {
		t.Error("implement@1 exists; it is upstream of after_loop and must not re-instantiate")
	}
}

// TestReinstantiatedStepsRerunGatesAndThresholds is §11.3 (4)'s tail: "gates
// re-run; thresholds re-apply".
//
// A re-instantiated step is a FRESH instance — no gate trail, no routing, no
// saga — so its gates run again and its threshold is evaluated again against
// the new ordinal's payload. Carrying either forward would make ordinal 1's
// verdict a copy of ordinal 0's, and the loop would decide nothing.
func TestReinstantiatedStepsRerunGatesAndThresholds(t *testing.T) {
	conn := mustDB(t)
	_, _ = activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	fresh, err := db.GetStep(conn, stepIDByInstance(t, conn, "verify@1"))
	testsupport.Must(t, err, "GetStep(verify@1): %v", err)
	if fresh.GateTrail != "" {
		t.Errorf("verify@1 gate_trail = %q on a fresh instance, want empty — gates re-run",
			fresh.GateTrail)
	}
	if fresh.Routing != "" {
		t.Errorf("verify@1 routing = %q on a fresh instance, want empty — thresholds re-apply",
			fresh.Routing)
	}
	if fresh.Status != db.StepPending {
		t.Errorf("verify@1 status = %q, want %q", fresh.Status, db.StepPending)
	}

	// And the threshold genuinely re-applies: the same step routes differently
	// at ordinal 1 given a different payload, which is the whole point of
	// re-running it.
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	completeReviewFanout(t, conn, e, 1)
	claimAndComplete(t, conn, e, "synthesize@1", "synthesis", "")
	driveAction(t, conn, e, "reconcile@1")
	claimAndComplete(t, conn, e, "verify@1", "report", metPayload)

	if got := stepRouting(t, conn, "verify@1"); got != RoutingPass {
		t.Errorf("verify@1 routing = %q on a `met` payload, want %q", got, RoutingPass)
	}
}

// ---------------------------------------------------------------------------
// §11.3: completion over highest-ordinal instances only
// ---------------------------------------------------------------------------

// TestCompletionIgnoresLowerOrdinals is §7.7's completion row, stated as the TDD
// states it: an issue with a `done` `verify@0` and a `pending` `verify@1` is NOT
// complete.
//
// The rule cuts both ways, and both halves are asserted here: a lower ordinal's
// `done` does not COUNT TOWARD completion, and a lower ordinal's `superseded`
// does not BLOCK it.
func TestCompletionIgnoresLowerOrdinals(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	// verify@0 is `done`; verify@1 is `pending`. The issue is NOT complete.
	if got := stepStatus(t, conn, "verify@0"); got != db.StepDone {
		t.Fatalf("verify@0 = %q, want %q", got, db.StepDone)
	}
	if got := issueStatus(t, conn, issue); got == "done" {
		t.Error("the issue is `done` with a pending verify@1; completion must be " +
			"evaluated over highest-ordinal instances only")
	}

	// The ordinal-0 `commit-gate`/`commit` are superseded and must not block
	// completion once ordinal 1 finishes.
	if got := stepStatus(t, conn, "commit-gate@0"); got != db.StepSuperseded {
		t.Fatalf("commit-gate@0 = %q, want %q", got, db.StepSuperseded)
	}
}

// TestSupersededLineageDoesNotBlockCompletion closes the loop the QA run walks:
// ordinal 1 finishes, the superseded ordinal-0 instances stay superseded, and
// the issue completes anyway.
func TestSupersededLineageDoesNotBlockCompletion(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	// Run ordinal 1 to the end.
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	completeReviewFanout(t, conn, e, 1)
	claimAndComplete(t, conn, e, "synthesize@1", "synthesis", "")
	driveAction(t, conn, e, "reconcile@1")
	claimAndComplete(t, conn, e, "verify@1", "report", metPayload)
	// `commit-gate` is `type="human"`: it is approved, not claimed (§6.15).
	err := e.DecideStep(conn, stepIDByInstance(t, conn, "commit-gate@1"),
		true, "", nowMS)
	testsupport.Must(t, err, "approve commit-gate@1: %v", err)
	claimAndComplete(t, conn, e, "commit@1", "the commit record", "")

	// The superseded ordinal-0 rows are untouched...
	for _, instance := range []string{"commit-gate@0", "commit@0"} {
		if got := stepStatus(t, conn, instance); got != db.StepSuperseded {
			t.Errorf("%s = %q, want %q", instance, got, db.StepSuperseded)
		}
	}
	// ...and the issue is complete regardless.
	if got := issueStatus(t, conn, issue); got != "done" {
		t.Errorf("issue status = %q after ordinal 1 completed, want done", got)
	}
}

// ---------------------------------------------------------------------------
// §11.3: "There is no other loop construct"
// ---------------------------------------------------------------------------

// TestThresholdStepNameRoutingIsNotALoop is §7.2's last row: a `threshold`
// routing to a STEP NAME is an interposed gate (§11.2), not a loop, and does
// not touch `loop_count`.
//
// The two are easy to conflate — both are thresholds routing somewhere other
// than `pass` — and conflating them would make an interposed gate consume a
// `max_fix_loops` budget it has nothing to do with.
func TestThresholdStepNameRoutingIsNotALoop(t *testing.T) {
	const src = `
[pipeline]
name = "interposed"
version = 1

[match]
kind = ["task"]

[[step]]
name = "assess"
executor = "assess"
emits = "findings"
threshold = { "escalate" = "any(status == blocked)" }

[[step]]
name = "escalate"
after = []
type = "human"
on_fail = "skip"

[[step]]
name = "finish"
after = ["assess"]
executor = "finish"
emits = "record"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "interposed.toml")
	issue := createIssue(t, conn, "interposed", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	claimAndComplete(t, conn, e, "assess@0", "the assessment", `[{"status":"blocked"}]`)

	if got := stepRouting(t, conn, "assess@0"); got != "escalate" {
		t.Fatalf("assess@0 routing = %q, want the interposed step name", got)
	}
	// THE COUNTER IS UNTOUCHED. This is the assertion the test exists for.
	if got := loopCount(t, conn, run.ID, issue); got != 0 {
		t.Errorf("loop_count = %d after a step-name routing, want 0 — an "+
			"interposed gate is not a loop construct", got)
	}
	// And nothing re-instantiated at ordinal 1.
	if stepExists(t, conn, "assess@1") || stepExists(t, conn, "finish@1") {
		t.Error("a step-name routing re-instantiated at ordinal 1; it is not a loop")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// driveToVerify runs one ordinal's chain up to (not including) `verify@k`, so a
// test can complete `verify` with the payload that drives its case.
//
// At ordinal 0 it starts from `implement`; at ordinal k > 0 the loop entry
// already created the chain, so it starts from `fix@k`.
func driveToVerify(t *testing.T, conn *sql.DB, e *Engine, ordinal int) {
	t.Helper()
	// Each round moves the stub tree, as a fix round that fixes something
	// does — without which DKT-340's guard correctly parks the loop at
	// ordinal 2 and no caller of this helper ever reaches a later one.
	driveFixtureRound(t, ordinal)
	if ordinal == 0 {
		claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	} else {
		claimAndComplete(t, conn, e, fmt.Sprintf("fix@%d", ordinal), "the fix summary", "")
	}
	completeReviewFanout(t, conn, e, ordinal)
	claimAndComplete(t, conn, e, fmt.Sprintf("synthesize@%d", ordinal), "the synthesis", "")
	// `reconcile` is an ACTION step: the ENGINE runs it, no claim (§6.15 as
	// amended). `synthesize` recorded no payload here, so the
	// aggregate reduces an empty input set and T4 short-circuits its threshold
	// to `pass` (§6.14) — which is what every caller of this helper wants.
	driveAction(t, conn, e, fmt.Sprintf("reconcile@%d", ordinal))
}

// completeReviewFanout completes all four `review` siblings at one ordinal, so
// the join (J1) releases.
func completeReviewFanout(t *testing.T, conn *sql.DB, e *Engine, ordinal int) {
	t.Helper()
	for i := range 4 {
		claimAndComplete(t, conn, e,
			fmt.Sprintf("review@%d#%d", ordinal, i), "findings", "")
	}
}

// loopCount reads the issue's loop counter.
func loopCount(t *testing.T, conn *sql.DB, runID, issueID int) int {
	t.Helper()
	var count int
	err := conn.QueryRow(
		`SELECT loop_count FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID,
	).Scan(&count)
	testsupport.Must(t, err, "reading loop_count: %v", err)
	return count
}

// stepRouting reads a step's routing, stripped of any appended reason.
func stepRouting(t *testing.T, conn *sql.DB, instance string) string {
	t.Helper()
	raw := stepRoutingRaw(t, conn, instance)
	routing, _, _ := strings.Cut(raw, ":")
	return routing
}

// stepRoutingRaw reads a step's routing column verbatim, reason included.
func stepRoutingRaw(t *testing.T, conn *sql.DB, instance string) string {
	t.Helper()
	var routing sql.NullString
	err := conn.QueryRow(
		`SELECT routing FROM steps WHERE instance = ?`, instance).Scan(&routing)
	testsupport.Must(t, err, "reading routing of %s: %v", instance, err)
	return routing.String
}

// stepExists reports whether an instance has a row.
func stepExists(t *testing.T, conn *sql.DB, instance string) bool {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE instance = ?`, instance).Scan(&n)
	testsupport.Must(t, err, "counting %s: %v", instance, err)
	return n > 0
}

// countStatus counts steps in a status, across the database.
func countStatus(t *testing.T, conn *sql.DB, status string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE status = ?`, status).Scan(&n)
	testsupport.Must(t, err, "counting %s steps: %v", status, err)
	return n
}

// issueStatus reads an issue's status.
func issueStatus(t *testing.T, conn *sql.DB, issueID int) string {
	t.Helper()
	var status string
	err := conn.QueryRow(
		`SELECT status FROM issues WHERE id = ?`, issueID).Scan(&status)
	testsupport.Must(t, err, "reading issue status: %v", err)
	return status
}

// artifactIDsFor maps every artifact id of a run to its kind.
func artifactIDsFor(t *testing.T, conn *sql.DB, runID int) map[int]string {
	t.Helper()
	rows, err := conn.Query(`SELECT id, kind FROM artifacts WHERE run_id = ?`, runID)
	testsupport.Must(t, err, "listing artifacts: %v", err)
	defer rows.Close()

	out := make(map[int]string)
	for rows.Next() {
		var (
			id   int
			kind string
		)
		err := rows.Scan(&id, &kind)
		testsupport.Must(t, err, "reading artifact: %v", err)
		out[id] = kind
	}
	return out
}

// contextInputs assembles a step's context bundle and returns its inputs.
func contextInputs(t *testing.T, conn *sql.DB, runID int, instance string) []ContextInput {
	t.Helper()

	defs, err := StepDefinitions(conn, runID)
	testsupport.Must(t, err, "loading definitions: %v", err)
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	testsupport.Must(t, err, "LoadScheduler: %v", err)
	step := stepNamed(t, sched, instance)
	spec := workflow.StepByName(defs[step.WorkflowID], step.StepName)
	if spec == nil {
		t.Fatalf("no spec for %s", instance)
	}

	ri, err := db.GetRunIssueTx(tx, runID, step.IssueID)
	testsupport.Must(t, err, "GetRunIssueTx: %v", err)

	artifacts, err := db.ListRunArtifactsTx(tx, step.RunID)
	testsupport.Must(t, err, "ListRunArtifactsTx: %v", err)
	inputs, err := resolveInputs(tx, sched, step, spec, ri.BodySnapshot, artifacts, nil)
	testsupport.Must(t, err, "resolveInputs(%s): %v", instance, err)
	return inputs
}

// assertInputOrdinal requires that the input produced by `stepName` was bound
// at `want`, naming what it found when it was not.
func assertInputOrdinal(t *testing.T, inputs []ContextInput, stepName string, want int) {
	t.Helper()
	for _, in := range inputs {
		name, ordinal, _, err := workflow.ParseInstance(in.ProducerStep)
		if err != nil {
			continue // An engine-form input (issue.body / issue.diff) has no producer.
		}
		if name != stepName {
			continue
		}
		if ordinal != want {
			t.Errorf("input from %s bound at ordinal %d, want %d (per-input fallback, §7.4)",
				in.ProducerStep, ordinal, want)
		}
		return
	}
	t.Errorf("no input produced by %q among %d inputs", stepName, len(inputs))
}

// ---------------------------------------------------------------------------
// DKT-106: the round record on issue.diff
// ---------------------------------------------------------------------------

// TestRoundDeltaRidesTheLoopDiff pins the two halves of the round record.
//
// Every tree-holding completion records the checkout's HEAD in the diff
// artifact's payload; a loop-round completion whose previous round recorded a
// different head ALSO appends that round's own delta — computed unscoped, so
// the judge-testing lens sees test files the issue's scope excludes — under a
// marked trailer, beside the unchanged cumulative diff. RUN-8's re-review
// judges had neither: three of five wrote target-reconstruction preambles to
// recover the round's commit from the cumulative issue-range diff.
func TestRoundDeltaRidesTheLoopDiff(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	head := "sha-round-0"
	e.HeadFn = func(string) string { return head }
	e.DiffFn = func(_, base string, _ []string) (string, error) {
		if base == "sha-round-0" {
			return "the round-1 hunks\n", nil
		}
		return "the cumulative hunks\n", nil
	}

	driveToVerify(t, conn, e, 0)

	// Ordinal 0 records the head and NO delta: there is no earlier round.
	body, payload := issueDiffArtifact(t, conn, run.ID, "implement@0")
	if !strings.Contains(payload, `"head":"sha-round-0"`) {
		t.Errorf("implement@0 diff payload = %q, want the recorded head", payload)
	}
	if strings.Contains(body, "round delta") {
		t.Errorf("implement@0 diff carries a round delta at ordinal 0:\n%s", body)
	}

	// Enter loop 1; the fix commits a new head.
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)
	head = "sha-round-1"
	claimAndComplete(t, conn, e, "fix@1", "the fix summary", "")

	body, payload = issueDiffArtifact(t, conn, run.ID, "fix@1")
	if !strings.Contains(body, "the cumulative hunks") {
		t.Errorf("the cumulative diff no longer leads the body:\n%s", body)
	}
	if !strings.Contains(body, "round delta: changes since sha-round-0") ||
		!strings.Contains(body, "the round-1 hunks") {
		t.Errorf("fix@1's diff carries no round delta:\n%s", body)
	}
	if !strings.Contains(payload, `"head":"sha-round-1"`) ||
		!strings.Contains(payload, `"round_base":"sha-round-0"`) {
		t.Errorf("fix@1 diff payload = %q, want head and round_base", payload)
	}
}

// issueDiffArtifact reads the issue.diff artifact one instance recorded.
func issueDiffArtifact(t *testing.T, conn *sql.DB, runID int, instance string) (body, payload string) {
	t.Helper()
	var b, p sql.NullString
	err := conn.QueryRow(
		`SELECT a.body, a.payload FROM artifacts a
		  WHERE a.run_id = ? AND a.step_id = ? AND a.kind = 'issue.diff'
		  ORDER BY a.id DESC LIMIT 1`,
		runID, stepIDByInstance(t, conn, instance)).Scan(&b, &p)
	testsupport.Must(t, err, "reading %s's issue.diff: %v", instance, err)
	return b.String, p.String
}

// ---------------------------------------------------------------------------
// DKT-63: a re-review round reads the critique it answers
// ---------------------------------------------------------------------------

// TestReReviewRoundCarriesThePreviousRoundsFindings pins DKT-63: a loop
// re-entry step's bundle binds the PREVIOUS round's artifacts of its own
// emitted kind — the review@0 fanout's findings and the reconciled set —
// beside its declared inputs, so a judge answers the critique itself rather
// than the fix step's summary of it. RUN-5's re-review judges had exactly
// two inputs (the fix's change-summary and issue.diff) and could not tell an
// answered ask from an ask answered as the fix characterised it.
func TestReReviewRoundCarriesThePreviousRoundsFindings(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)
	claimAndComplete(t, conn, e, "fix@1", "the fix summary", "")

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "review@1#0"), nowMS)
	testsupport.Must(t, err, "ReadContext(review@1#0): %v", err)

	byProducer := map[string]ContextInput{}
	for _, in := range bundle.Inputs {
		byProducer[in.ProducerStep] = in
	}
	for i := range 4 {
		judge := fmt.Sprintf("review@0#%d", i)
		in, ok := byProducer[judge]
		if !ok {
			t.Errorf("review@1#0 does not carry %s's findings — the judge "+
				"reads the fix's summary of the critique, not the critique", judge)
			continue
		}
		if in.Kind != "findings" {
			t.Errorf("%s's prior-round input has kind %q, want findings", judge, in.Kind)
		}
	}
	if _, ok := byProducer["reconcile@0"]; !ok {
		t.Errorf("review@1#0 does not carry reconcile@0's reconciled set: %v",
			producerList(bundle.Inputs))
	}
	// And nothing of the kind from OUTSIDE review's lineage (DKT-1055): the
	// synthesis between the fanout and the reconciled set is neither review's
	// own prior round nor the standing set the fix acted on.
	if _, ok := byProducer["synthesize@0"]; ok {
		t.Errorf("review@1#0 carries synthesize@0's findings — the previous-round "+
			"pass matched on kind rather than lineage: %v", producerList(bundle.Inputs))
	}
	// The declared inputs are untouched: the fix's own account still binds.
	if _, ok := byProducer["fix@1"]; !ok {
		t.Errorf("review@1#0 lost its declared change-summary from fix@1: %v",
			producerList(bundle.Inputs))
	}

	// Ordinal 0 stays byte-identical: no previous round exists.
	first, err := ReadContext(conn, stepIDByInstance(t, conn, "review@0#0"), nowMS)
	testsupport.Must(t, err, "ReadContext(review@0#0): %v", err)
	for _, in := range first.Inputs {
		if strings.HasPrefix(in.ProducerStep, "review@") {
			t.Errorf("review@0#0 binds a review artifact (%s) at ordinal 0 — "+
				"there is no previous round to carry", in.ProducerStep)
		}
	}
}

// ---------------------------------------------------------------------------
// DKT-1055: a re-entry round reads its own lineage, not every producer of
// its kind
// ---------------------------------------------------------------------------

// TestReSynthesisRoundCarriesItsOwnLineageOnly pins DKT-1055: the previous-
// round pass binds the prior round of the step's OWN lineage — its own name
// at ordinal k-1 plus the standing set the loop body acted on — never every
// same-issue producer of its emitted kind. `review`, `synthesize`, and
// `reconcile` all emit `findings`, so the kind-only match handed synthesize@1
// review@0's whole raw fanout beside synthesize@0 and reconcile@0, on top of
// its declared `review.*` (already scoped to round 1): a security-change
// round-2 synthesis carried a 263KB packet holding four raw judge payloads
// whose digest it was also carrying as reconcile's aggregate.
func TestReSynthesisRoundCarriesItsOwnLineageOnly(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)
	claimAndComplete(t, conn, e, "fix@1", "the fix summary", "")
	completeReviewFanout(t, conn, e, 1)

	bundle, err := ReadContext(conn, stepIDByInstance(t, conn, "synthesize@1"), nowMS)
	testsupport.Must(t, err, "ReadContext(synthesize@1): %v", err)

	byProducer := map[string]ContextInput{}
	for _, in := range bundle.Inputs {
		byProducer[in.ProducerStep] = in
	}

	// The declared `review.*` still binds the CURRENT round's fanout.
	for i := range 4 {
		judge := fmt.Sprintf("review@1#%d", i)
		if _, ok := byProducer[judge]; !ok {
			t.Errorf("synthesize@1 lost its declared %s input: %v",
				judge, producerList(bundle.Inputs))
		}
	}
	// Its own lineage from the round before: the prior synthesis, and the
	// reconciled set it was digested into — what fix@1 read and acted on.
	for _, prior := range []string{"synthesize@0", "reconcile@0"} {
		in, ok := byProducer[prior]
		if !ok {
			t.Errorf("synthesize@1 does not carry %s's findings — the previous "+
				"round of its own lineage: %v", prior, producerList(bundle.Inputs))
			continue
		}
		if in.Kind != "findings" {
			t.Errorf("%s's prior-round input has kind %q, want findings", prior, in.Kind)
		}
	}
	// And NOT the previous round's raw judge fanout: reconcile@0's aggregate
	// is the digest of it, and a match on kind alone is what carried it.
	for i := range 4 {
		judge := fmt.Sprintf("review@0#%d", i)
		if _, ok := byProducer[judge]; ok {
			t.Errorf("synthesize@1 carries %s's raw findings from the round before "+
				"— the previous-round pass matched on kind, not lineage: %v",
				judge, producerList(bundle.Inputs))
		}
	}
	if len(bundle.Inputs) != 6 {
		t.Errorf("synthesize@1 carries %d inputs, want 6 (review@1's four judges, "+
			"synthesize@0, reconcile@0): %v", len(bundle.Inputs), producerList(bundle.Inputs))
	}
}

// TestPreviousRoundLineageIsOwnNamePlusTheStandingSet pins the definitional
// half of DKT-1055 against the fixtures directly: a re-entry step's lineage
// is its own name plus the in-chain producers the serving loop body reads of
// the step's kind — and a serves-scoped body whose chain does not contain the
// step contributes nothing to it (DKT-544).
func TestPreviousRoundLineageIsOwnNamePlusTheStandingSet(t *testing.T) {
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)
	standard, err := workflow.Parse(src)
	testsupport.Must(t, err, "parsing fixture: %v", err)
	clusters, err := workflow.Parse([]byte(clusterSrc))
	testsupport.Must(t, err, "parsing the cluster fixture: %v", err)

	for _, tc := range []struct {
		def  *workflow.Definition
		step string
		kind string
		want []string
	}{
		// Own name plus the reconciled set `fix` reads: never the raw fanout
		// for `synthesize`, never the synthesis for `review`, and never
		// `implement`, which is outside the chain and of another kind.
		{standard, "review", "findings", []string{"reconcile", "review"}},
		{standard, "synthesize", "findings", []string{"reconcile", "synthesize"}},
		{standard, "verify", "ac-report", []string{"verify"}},
		{standard, "commit", "commit-record", []string{"commit"}},
		// Cluster A's gate: `prd-fix`'s `draft.doc` is outside its chain, and
		// cluster B's body never contains the gate at all.
		{clusters, "prd-gate", "findings", []string{"prd-gate"}},
		{clusters, "design-gate", "report", []string{"design-gate"}},
		// A step outside every chain has only itself; loopReentryEmitter is
		// what keeps the pass from firing there in the first place.
		{standard, "implement", "change-summary", []string{"implement"}},
	} {
		got := previousRoundLineage(tc.def, tc.step, tc.kind)
		names := make([]string, 0, len(got))
		for name := range got {
			names = append(names, name)
		}
		sort.Strings(names)
		if !slices.Equal(names, tc.want) {
			t.Errorf("previousRoundLineage(%s, %s, %s) = %v, want %v",
				tc.def.Pipeline.Name, tc.step, tc.kind, names, tc.want)
		}
	}
}

// producerList names each input's producer, for failure messages.
func producerList(inputs []ContextInput) []string {
	out := make([]string, 0, len(inputs))
	for _, in := range inputs {
		out = append(out, in.ProducerStep+":"+in.Kind)
	}
	return out
}
