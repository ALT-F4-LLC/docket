package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-870: fix-loop non-convergence was invisible to the engine. The 2026-08-26
// retro across 19 fix-loop runs found three shapes, all ending only by operator
// action; two are closed here and one was already closed:
//
//  1. FLAT VOLUME NEVER TRIPPED ANYTHING. RUN-51 held 8-12 clusters across TEN
//     rounds (~271k + ~251k output tokens on the last two alone, after the
//     run's own ruling that the defect was structural); RUN-50 held 7-10
//     across six, ended by a hand-broken deadlock. DKT-340 saw moving trees
//     and DKT-589 saw byte-distinct verdicts, so both stayed silent. The
//     author now declares `max_stalled_rounds` on the routing step (V38), and
//     a loop entry after that many consecutive measured rounds without a new
//     minimum routed volume parks in the non-convergence refusal's exact
//     shape — `--as fix-round` stays the way out.
//
//  2. EMPTY WORK LISTS RAN FULL ROUNDS. Already closed by DKT-588
//     (dkt588_test.go drives the verbatim RUN-34 shape): a loop body handing
//     back its previous round's commit parks at its source before the fanout.
//
//  3. LOOP EXIT WAS NOT GATED ON THE RECORDED PAYLOAD. RUN-58's reconcile@1
//     routed `pass` and the loop exited with all 16 clusters open, SIX at the
//     order's high position, none held and none operator-resolved —
//     "converged" in the ledger meaning "dispositioned". The author now
//     declares `pass_floor = { field, at }` (V37/V37a), and a `pass` whose
//     recorded payload still holds unexempt elements at or above the floor's
//     position parks `waiting-human` instead, naming `--as override-pass`
//     and `--as fix-round` as the ways out.
//
// Both knobs are OPT-IN (absent means the engine behaves exactly as before)
// and both are positional: field names and floor values are opaque tokens
// compared only by position in the pinned schema's order, so core learns
// nothing about severities (genericity.md).

// floorWorkflowSrc is the RUN-58 shape as a workflow: the routing step's
// threshold reads `status`, so a payload whose elements are open at `high`
// severity but carry no unmet status routes `pass` — the exact self-reported
// "converged" that contradicted the recorded evidence. `pass_floor` is the
// declared exit bar; `hold_spread` is kept so the held-exemption path is the
// fixture's real one.
const floorWorkflowSrc = `
[pipeline]
name = "floor-exit"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
class = "write"
emits = "change-summary"

[[step]]
name = "scan"
after = ["implement"]
executor = "scan"
emits = "findings"

[[step]]
name = "reconcile"
after = ["scan"]
action = "aggregate"
inputs = ["scan.findings"]
payload = "findings@1"
params = { field = "severity", method = "median", hold_spread = 2, output = "findings" }
threshold = { "fix-loop" = "any(status == unmet)" }
pass_floor = { field = "severity", at = "high" }
max_fix_loops = 4

[[step]]
name = "fix"
executor = "fix"
class = "write"
emits = "change-summary"
loop = true
inputs = ["reconcile.findings"]
after_loop = "scan"

[[step]]
name = "verify"
after = ["reconcile"]
executor = "verify"
emits = "ac-report"
`

// volumeWorkflowSrc is the RUN-51/RUN-50 shape: a loop whose routing step
// re-routes `fix-loop` every round over a standing set that never shrinks.
// `max_stalled_rounds = 2` is the declared tolerance; `max_fix_loops` is high
// enough that the plateau, not the budget, must be what stops the loop —
// which is the retro's own finding ("max_fix_loops never fired as the
// terminator in the runs where it existed").
const volumeWorkflowSrc = `
[pipeline]
name = "flat-volume"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
class = "write"
emits = "change-summary"

[[step]]
name = "scan"
after = ["implement"]
executor = "scan"
emits = "findings"

[[step]]
name = "reconcile"
after = ["scan"]
action = "aggregate"
inputs = ["scan.findings"]
payload = "findings@1"
params = { field = "severity", method = "median", output = "findings" }
threshold = { "fix-loop" = "any(severity >= high)" }
max_fix_loops = 8
max_stalled_rounds = 2

[[step]]
name = "fix"
executor = "fix"
class = "write"
emits = "change-summary"
loop = true
inputs = ["reconcile.findings"]
after_loop = "scan"
`

// activatedCustomRun is activatedRun over one of this file's workflows: the
// fixture's schema (the aggregate needs `findings@1`'s order), the custom
// TOML through the real parse-validate-lint path, one task issue, activation.
func activatedCustomRun(t *testing.T, conn *sql.DB, src string) (*model.Run, int) {
	t.Helper()
	registerFixtureSchema(t, conn)
	registerSource(t, conn, []byte(src), "dkt870.toml")
	issue := createIssue(t, conn, "converge the loop", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run, issue
}

// driveFloorRound completes floor-exit's round 0 up to and including
// `reconcile@0`, with the scan payload under the test's control.
func driveFloorRound(t *testing.T, conn *sql.DB, e *Engine, payload string) {
	t.Helper()
	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	claimAndComplete(t, conn, e, "scan@0", "the scan", payload)
	driveAction(t, conn, e, "reconcile@0")
}

// TestPassWithStandingFloorPayloadParks is the verbatim RUN-58 shape: the
// threshold reads a field that says nothing is unmet, the payload's own
// severity evidence says otherwise, and the exit is refused.
func TestPassWithStandingFloorPayloadParks(t *testing.T) {
	conn := mustDB(t)
	activatedCustomRun(t, conn, floorWorkflowSrc)
	e := testEngine()

	driveFloorRound(t, conn, e,
		`[{"id":"C-1","severity":"high"},{"id":"C-2","severity":"medium"}]`)

	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepWaitingHuman {
		t.Errorf("reconcile@0 = %q, want the pass_floor park %q",
			got, db.StepWaitingHuman)
	}
	// The park says WHY and NAMES THE WAYS OUT, like every refusal in the
	// loop family.
	routing := stepRoutingRaw(t, conn, "reconcile@0")
	for _, want := range []string{"pass_floor", "severity >= high",
		"override-pass", "fix-round"} {
		if !strings.Contains(routing, want) {
			t.Errorf("the park does not mention %q: %q", want, routing)
		}
	}
	// And the chain did NOT advance: a parked pass hands nothing downstream.
	if ready := readyInstances(t, conn); contains(ready, "verify@0") {
		t.Error("verify@0 became ready past a parked pass; the floor park " +
			"must stop the lineage at its source")
	}
}

// TestPassBelowTheFloorExitsClean is the regression half that matters most: a
// payload with nothing at the floor exits exactly as it always has.
func TestPassBelowTheFloorExitsClean(t *testing.T) {
	conn := mustDB(t)
	activatedCustomRun(t, conn, floorWorkflowSrc)
	e := testEngine()

	driveFloorRound(t, conn, e, `[{"id":"C-1","severity":"medium"}]`)

	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepDone {
		t.Errorf("reconcile@0 = %q, want %q — a pass below the floor is a "+
			"genuine exit", got, db.StepDone)
	}
	if got := stepRouting(t, conn, "reconcile@0"); got != RoutingPass {
		t.Errorf("routing = %q, want %q", got, RoutingPass)
	}
}

// TestFloorLeavesFixLoopRoutingAlone: the floor gates only the `pass` exit. A
// threshold that already decided to loop is not second-guessed — the loop IS
// the remedy the floor would otherwise ask an operator for.
func TestFloorLeavesFixLoopRoutingAlone(t *testing.T) {
	conn := mustDB(t)
	activatedCustomRun(t, conn, floorWorkflowSrc)
	e := testEngine()

	driveFloorRound(t, conn, e,
		`[{"id":"C-1","severity":"high","status":"unmet"}]`)

	if !stepExists(t, conn, "fix@1") {
		t.Error("the threshold's own fix-loop routing did not enter the loop; " +
			"the floor must never override a routing that already decided")
	}
	if got := stepStatus(t, conn, "reconcile@0"); got == db.StepWaitingHuman {
		t.Error("reconcile@0 parked on a fix-loop routing; the floor gates " +
			"only the pass exit")
	}
}

// TestOperatorResolvedElementsDoNotBlockThePass drives the real held path: a
// cluster the spread held, an operator approving it AT a value above the
// floor, and the resumed pass standing — the decision channel already ran, and
// re-parking on it would ask the answered question again.
func TestOperatorResolvedElementsDoNotBlockThePass(t *testing.T) {
	conn := mustDB(t)
	activatedCustomRun(t, conn, floorWorkflowSrc)
	e := testEngine()

	// Spread 4 across the five-value order: held.
	driveFloorRound(t, conn, e, `[{"id":"C-1","severity":["low","blocker"]}]`)
	held := heldInstances(t, conn)
	if len(held) != 1 {
		t.Fatalf("premise: expected 1 held step, got %v", held)
	}

	// The operator accepts the cluster at `blocker` — ABOVE the floor. The
	// resumed routing must still pass: `operator_resolved` is the exemption.
	err := e.DecideStepValue(conn, stepIDByInstance(t, conn, held[0]),
		true, "ship it, tracked separately", "blocker", nowMS)
	testsupport.Must(t, err, "approving %s: %v", held[0], err)

	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepDone {
		t.Errorf("reconcile@0 = %q after an operator resolved the only "+
			"cluster, want %q — the floor must not re-ask an answered question",
			got, db.StepDone)
	}
}

// TestOverridePassExitsTheFloorPark is the recorded operator exit the park
// names: the pass the floor refused, taken anyway, on an operator's authority.
func TestOverridePassExitsTheFloorPark(t *testing.T) {
	conn := mustDB(t)
	activatedCustomRun(t, conn, floorWorkflowSrc)
	e := testEngine()

	driveFloorRound(t, conn, e, `[{"id":"C-1","severity":"high"}]`)
	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: reconcile@0 = %q, want the park", got)
	}

	err := e.ResolveStep(conn, stepIDByInstance(t, conn, "reconcile@0"),
		ResolveOverridePass, "accepted; the opens are tracked in follow-ups", nowMS)
	testsupport.Must(t, err, "resolve --as override-pass: %v", err)

	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepDone {
		t.Errorf("reconcile@0 = %q after override-pass, want %q", got, db.StepDone)
	}
	if ready := readyInstances(t, conn); !contains(ready, "verify@0") {
		t.Errorf("verify@0 is not ready after the override; the resolved pass "+
			"must hand the chain onward (ready: %v)", ready)
	}
}

// TestFixRoundBuysARoundFromTheFloorPark is the other way out the park names:
// instead of exiting over standing work, the operator mints the round that
// addresses it.
func TestFixRoundBuysARoundFromTheFloorPark(t *testing.T) {
	conn := mustDB(t)
	activatedCustomRun(t, conn, floorWorkflowSrc)
	e := testEngine()

	driveFloorRound(t, conn, e, `[{"id":"C-1","severity":"high"}]`)
	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: reconcile@0 = %q, want the park", got)
	}

	err := e.ResolveStep(conn, stepIDByInstance(t, conn, "reconcile@0"),
		ResolveFixRound, "fix the standing highs", nowMS)
	testsupport.Must(t, err, "resolve --as fix-round: %v", err)

	if !stepExists(t, conn, "fix@1") {
		t.Error("the operator authorized a round from the floor park and none " +
			"was minted")
	}
}

// ---------------------------------------------------------------------------
// Flat volume (`max_stalled_rounds`)
// ---------------------------------------------------------------------------

// volumePayload is one round's scan output: `volume` one-member clusters, ids
// unique to the round so no verdict is ever byte-identical across rounds —
// DKT-589's guard must stay silent for the plateau to be what parks.
func volumePayload(round, volume int) string {
	elements := make([]string, 0, volume)
	for i := range volume {
		elements = append(elements,
			fmt.Sprintf(`{"id":"C-%d-%d","severity":"high"}`, round, i))
	}
	return "[" + strings.Join(elements, ",") + "]"
}

// driveVolumeRound completes one flat-volume ordinal — the tree MOVES each
// round (driveFixtureRound), so DKT-340's guard stays silent too and the
// plateau is the only signal left standing, exactly as in RUN-51.
func driveVolumeRound(t *testing.T, conn *sql.DB, e *Engine, ordinal, volume int) {
	t.Helper()
	driveFixtureRound(t, ordinal)
	if ordinal == 0 {
		claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	} else {
		claimAndComplete(t, conn, e, fmt.Sprintf("fix@%d", ordinal), "the fix summary", "")
	}
	claimAndComplete(t, conn, e, fmt.Sprintf("scan@%d", ordinal),
		"the scan", volumePayload(ordinal, volume))
	driveAction(t, conn, e, fmt.Sprintf("reconcile@%d", ordinal))
}

// TestFlatVolumeAcrossDeclaredRoundsParksTheLoop is the RUN-51 shape at test
// scale: every round moves the tree and reaches a byte-distinct verdict, and
// the standing volume never improves. After `max_stalled_rounds = 2`
// consecutive non-improving rounds the next entry is refused.
func TestFlatVolumeAcrossDeclaredRoundsParksTheLoop(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedCustomRun(t, conn, volumeWorkflowSrc)
	e := testEngine()

	driveVolumeRound(t, conn, e, 0, 2)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: round 0 must have entered the loop")
	}
	driveVolumeRound(t, conn, e, 1, 2)
	if !stepExists(t, conn, "fix@2") {
		t.Fatal("premise: one non-improving round is within tolerance")
	}
	driveVolumeRound(t, conn, e, 2, 2)

	if stepExists(t, conn, "fix@3") {
		t.Error("a third round was minted after two consecutive rounds of " +
			"flat volume; the declared tolerance must park the loop")
	}
	if got := stepStatus(t, conn, "reconcile@2"); got != db.StepWaitingHuman {
		t.Errorf("reconcile@2 = %q, want the flat-volume park %q",
			got, db.StepWaitingHuman)
	}
	routing := stepRoutingRaw(t, conn, "reconcile@2")
	for _, want := range []string{"max_stalled_rounds", "fix-round"} {
		if !strings.Contains(routing, want) {
			t.Errorf("the park does not mention %q: %q", want, routing)
		}
	}
	// The counter is put back, the bound refusal's own discipline (DKT-78): a
	// refusal minted no ordinal.
	if got := loopCount(t, conn, run.ID, issue); got != 2 {
		t.Errorf("loop_count = %d after the refused entry, want 2", got)
	}
}

// TestShrinkingVolumeKeepsTheLoopRunning is the lower bound: a loop that IS
// converging sets a new minimum every round and must never park on this
// signal — a guard that fired on convergence would break every workflow that
// declares the tolerance.
func TestShrinkingVolumeKeepsTheLoopRunning(t *testing.T) {
	conn := mustDB(t)
	activatedCustomRun(t, conn, volumeWorkflowSrc)
	e := testEngine()

	driveVolumeRound(t, conn, e, 0, 3)
	driveVolumeRound(t, conn, e, 1, 2)
	driveVolumeRound(t, conn, e, 2, 1)

	if !stepExists(t, conn, "fix@3") {
		t.Error("a converging loop was refused a round; shrinking volume is " +
			"progress and must keep the loop running")
	}
}

// TestNoiseAroundABestVolumeStillParks pins the measurement's semantics: "no
// improvement" means no NEW MINIMUM, not endpoint-to-endpoint decrease.
// RUN-51's volumes (8, 12, 10, 10, 7, 10, 7, 11, 10, ~8) fell between plenty
// of adjacent rounds while never trending anywhere; a consecutive-pairs
// reading would have stayed silent for exactly the run this rule exists for,
// and a tie with the best round is two rounds at the same wall, not progress.
func TestNoiseAroundABestVolumeStillParks(t *testing.T) {
	conn := mustDB(t)
	activatedCustomRun(t, conn, volumeWorkflowSrc)
	e := testEngine()

	driveVolumeRound(t, conn, e, 0, 3)
	driveVolumeRound(t, conn, e, 1, 2) // A new minimum: the streak resets.
	driveVolumeRound(t, conn, e, 2, 3) // Worse than best: one stalled round.
	if !stepExists(t, conn, "fix@3") {
		t.Fatal("premise: one stalled round is within tolerance")
	}
	driveVolumeRound(t, conn, e, 3, 2) // TIES the best: still not progress.

	if stepExists(t, conn, "fix@4") {
		t.Error("a round was minted after two rounds that never beat the best " +
			"volume; oscillating around a floor is the RUN-51 shape and must park")
	}
	if got := stepStatus(t, conn, "reconcile@3"); got != db.StepWaitingHuman {
		t.Errorf("reconcile@3 = %q, want the flat-volume park %q",
			got, db.StepWaitingHuman)
	}
}

// TestFixRoundOverridesTheFlatVolumePark: the plateau park is the
// non-convergence refusal's shape, so its escape hatch is the same one — the
// operator who has read the park keeps the decision (DKT-237).
func TestFixRoundOverridesTheFlatVolumePark(t *testing.T) {
	conn := mustDB(t)
	activatedCustomRun(t, conn, volumeWorkflowSrc)
	e := testEngine()

	driveVolumeRound(t, conn, e, 0, 2)
	driveVolumeRound(t, conn, e, 1, 2)
	driveVolumeRound(t, conn, e, 2, 2)
	if got := stepStatus(t, conn, "reconcile@2"); got != db.StepWaitingHuman {
		t.Fatalf("premise: reconcile@2 = %q, want the park", got)
	}

	err := e.ResolveStep(conn, stepIDByInstance(t, conn, "reconcile@2"),
		ResolveFixRound, "the plateau is expected here; one more round", nowMS)
	testsupport.Must(t, err, "resolve --as fix-round: %v", err)

	if !stepExists(t, conn, "fix@3") {
		t.Error("the operator authorized a round past the plateau and none " +
			"was minted")
	}
}
