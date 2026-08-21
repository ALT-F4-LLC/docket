package engine

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The starvation cases of §11.3 — the ones about a downstream step that never
// gets to run, rather than about the rows a loop entry writes.
//
// The fixture below is `example-workflow.toml`'s shape reduced to the part
// these cases are about: a step that DRIVES the loop (`review`, carrying the
// `fix-loop` threshold and the bound) with a checking step strictly downstream
// of it (`verify-ac`) and a terminal step after that (`commit`). Reduced rather
// than reused because the shipped fixture drives its loop at `verify` itself,
// which is precisely the step whose starvation is in question — a loop driven
// by the victim cannot show the victim being skipped over.
//
// Two failures live here and they are NOT the same failure:
//
//	(1) a loop ENTRY supersedes the pending `verify-ac` before it ran, which is
//	    correct only because the entry instantiates its successor in the same
//	    transaction (§11.3 (4)); and
//	(2) a loop entry REFUSED BY THE BOUND (§11.3 (1)) instantiates nothing —
//	    so the final ordinal's `verify-ac` is the last one there will ever be,
//	    and everything about the run's ending depends on that ordinal still
//	    being live.

// starvationWorkflow is the reduced shape described above.
const starvationWorkflow = `
[pipeline]
name = "downstream-check"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"

[[step]]
name = "assess"
after = ["implement"]
executor = "assess"
emits = "findings"
threshold = { "fix-loop" = "any(status == unmet)" }
max_fix_loops = 2

[[step]]
name = "fix"
executor = "fix"
emits = "change-summary"
loop = true
after_loop = "assess"

[[step]]
name = "verify-ac"
after = ["assess"]
executor = "verify-ac"
emits = "ac-report"
threshold = { "fix-loop" = "any(status == unmet)" }

[[step]]
name = "commit"
after = ["verify-ac"]
executor = "commit"
emits = "commit-record"
`

// startedStarvationRun registers the reduced workflow and activates a run on
// one issue bound to it.
func startedStarvationRun(t *testing.T, conn *sql.DB) (*model.Run, int) {
	t.Helper()
	registerSource(t, conn, []byte(starvationWorkflow), "downstream-check.toml")
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run, issue
}

// enterLoopAt drives one ordinal's chain to the `assess` step and routes it
// `fix-loop`, so `verify-ac` at that ordinal is reached by the sweep having
// never been claimed — attempt 0, exactly as DKT-78 observed.
func enterLoopAt(t *testing.T, conn *sql.DB, e *Engine, ordinal int) {
	t.Helper()
	// Each round moves the stub tree, as a real fix round does — see
	// driveFixtureRound. Without it DKT-340's non-convergence guard parks the
	// loop at ordinal 2 and the bound this file is about is never reached.
	driveFixtureRound(t, ordinal)
	if ordinal == 0 {
		claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	} else {
		claimAndComplete(t, conn, e, fmt.Sprintf("fix@%d", ordinal), "the fix summary", "")
	}
	claimAndComplete(t, conn, e,
		fmt.Sprintf("assess@%d", ordinal), "the assessment", unmetPayload)
}

// ---------------------------------------------------------------------------
// §11.3 (2) + (4): supersede implies a successor
// ---------------------------------------------------------------------------

// TestSupersedeInstantiatesTheSuccessorOfEveryPendingItTakes is the invariant
// DKT-78 asks for, stated over the sweep's whole output rather than over one
// step name: no instance is superseded at ordinal k-1 unless an instance of the
// SAME STEP exists at ordinal k when the transaction closes.
//
// It is the invariant and not a spot check because the two sets are computed by
// different code from different inputs — the sweep reads step ROWS and filters
// by `after_loop` downstream membership, while the instantiation walks the
// DEFINITION and takes `loop` steps as well. They agree today because
// `downstream` is passed to both, and this test is what fails if a later edit
// widens one without the other, leaving a step swept with nothing to succeed it
// and a run that completes with that step's work never done.
func TestSupersedeInstantiatesTheSuccessorOfEveryPendingItTakes(t *testing.T) {
	conn := mustDB(t)
	_, issue := startedStarvationRun(t, conn)
	e := testEngine()

	enterLoopAt(t, conn, e, 0)

	// `verify-ac@0` is the DKT-78 case exactly: downstream of `after_loop`,
	// never claimed, taken by the sweep at attempt 0.
	if got := stepStatus(t, conn, "verify-ac@0"); got != db.StepSuperseded {
		t.Fatalf("verify-ac@0 = %q after the entry, want %q — the case is not set up",
			got, db.StepSuperseded)
	}

	assertSupersededHaveSuccessors(t, conn, issue, 1)
}

// TestSupersededSuccessorBecomesReachable is the other half of the invariant,
// and the half a row-count check would miss: the successor must be REACHABLE,
// not merely present.
//
// A `verify-ac@1` that exists but can never satisfy R3 is starvation with a row
// to point at. Its predecessor `assess` re-instantiates at ordinal 1 (§11.3
// (4)), so the successor becomes ready the moment ordinal 1's chain reaches it
// — which is what makes superseding the ordinal-0 instance a deferral rather
// than a discard.
func TestSupersededSuccessorBecomesReachable(t *testing.T) {
	conn := mustDB(t)
	run, _ := startedStarvationRun(t, conn)
	e := testEngine()

	enterLoopAt(t, conn, e, 0)

	// Not ready yet: ordinal 1's `assess` has not run.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		step := stepNamed(t, sched, "verify-ac@1")
		if ready, _ := sched.Ready(step); ready {
			t.Fatal("verify-ac@1 is ready before assess@1 completed; the case is not set up")
		}
	})

	claimAndComplete(t, conn, e, "fix@1", "the fix summary", "")
	claimAndComplete(t, conn, e, "assess@1", "the assessment", metPayload)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		step := stepNamed(t, sched, "verify-ac@1")
		if ready, cond := sched.Ready(step); !ready {
			t.Errorf("verify-ac@1 is not ready (%s) with assess@1 done — a "+
				"superseded instance's successor must be reachable, or the "+
				"supersede discarded the step rather than deferring it", cond)
		}
	})
}

// TestSupersedeRepairsAMissingSuccessor exercises the guard the two tests above
// only observe holding.
//
// The guard is DEFENSIVE: the sweep and the instantiation are handed the same
// `after_loop` downstream set, so no workflow an author can write makes them
// disagree, and there is no way to reach the repair through the ordinary path.
// The gap is introduced directly instead — the successor row is deleted and the
// guard is called with the sweep's output — because a repair that has never run
// is a repair nobody knows is correct, and the day it is needed is the day the
// two sets stopped agreeing.
func TestSupersedeRepairsAMissingSuccessor(t *testing.T) {
	conn := mustDB(t)
	run, issue := startedStarvationRun(t, conn)
	e := testEngine()

	enterLoopAt(t, conn, e, 0)

	// The gap: ordinal 1 exists, minus the successor of a step the sweep took.
	execSQL(t, conn, `DELETE FROM steps WHERE instance = ?`, "verify-ac@1")
	if stepExists(t, conn, "verify-ac@1") {
		t.Fatal("verify-ac@1 survived the delete; the case is not set up")
	}

	defs, err := StepDefinitions(conn, run.ID)
	testsupport.Must(t, err, "loading definitions: %v", err)

	step, err := db.GetStep(conn, stepIDByInstance(t, conn, "assess@0"))
	testsupport.Must(t, err, "GetStep(assess@0): %v", err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	repaired, err := ensureSupersededHaveSuccessors(
		tx, step, defs[step.WorkflowID], []string{"verify-ac@0"}, 1, nowMS)
	testsupport.Must(t, err, "ensureSupersededHaveSuccessors: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	if len(repaired) != 1 || repaired[0] != "verify-ac@1" {
		t.Fatalf("repaired = %v, want [verify-ac@1]", repaired)
	}
	if got := stepStatus(t, conn, "verify-ac@1"); got != db.StepPending {
		t.Errorf("the repaired verify-ac@1 = %q, want %q — a successor written "+
			"in any status but `pending` is a row that documents the work "+
			"rather than scheduling it", got, db.StepPending)
	}

	// And the repair is not a second writer racing the first: run again over an
	// ordinal that now has its successor, and nothing is written.
	assertRepairIsIdempotent(t, conn, run.ID, issue)
}

// assertRepairIsIdempotent requires the guard to write nothing when every
// superseded instance already has its successor — the ordinary case, on every
// loop entry a run makes.
func assertRepairIsIdempotent(t *testing.T, conn *sql.DB, runID, issueID int) {
	t.Helper()

	before := countIssueSteps(t, conn, issueID)

	defs, err := StepDefinitions(conn, runID)
	testsupport.Must(t, err, "loading definitions: %v", err)
	step, err := db.GetStep(conn, stepIDByInstance(t, conn, "assess@0"))
	testsupport.Must(t, err, "GetStep(assess@0): %v", err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	repaired, err := ensureSupersededHaveSuccessors(
		tx, step, defs[step.WorkflowID], []string{"verify-ac@0", "commit@0"}, 1, nowMS)
	testsupport.Must(t, err, "ensureSupersededHaveSuccessors: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	if len(repaired) != 0 {
		t.Errorf("repaired = %v with every successor present, want none", repaired)
	}
	if got := countIssueSteps(t, conn, issueID); got != before {
		t.Errorf("step count = %d after a no-op repair, want %d", got, before)
	}
}

// countIssueSteps counts an issue's step rows.
func countIssueSteps(t *testing.T, conn *sql.DB, issueID int) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE issue_id = ?`, issueID).Scan(&n)
	testsupport.Must(t, err, "counting steps: %v", err)
	return n
}

// TestSupersedeRepairSkipsMaterializedNames is the guard's H17 case: a
// materialized `<step>-held` name is mapped back to its routing step before the
// lookup, so a held question superseded from an earlier ordinal does not cause
// a held row to be minted at the new one.
//
// The distinction matters because a held step is the one row expansion never
// writes. Its successor IS its routing step's new instance — the routing step
// re-materializes its own held row if the new ordinal's computation holds again
// — and a guard that took the name at face value would create a `type=human`
// step asking about a computation that has not happened yet.
func TestSupersedeRepairSkipsMaterializedNames(t *testing.T) {
	conn := mustDB(t)
	run, issue := startedStarvationRun(t, conn)
	e := testEngine()

	enterLoopAt(t, conn, e, 0)

	before := countIssueSteps(t, conn, issue)

	defs, err := StepDefinitions(conn, run.ID)
	testsupport.Must(t, err, "loading definitions: %v", err)
	step, err := db.GetStep(conn, stepIDByInstance(t, conn, "assess@0"))
	testsupport.Must(t, err, "GetStep(assess@0): %v", err)

	held := workflow.RenderInstance(workflow.HeldStepName("assess"), 0, nil)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	repaired, err := ensureSupersededHaveSuccessors(
		tx, step, defs[step.WorkflowID], []string{held}, 1, nowMS)
	testsupport.Must(t, err, "ensureSupersededHaveSuccessors: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	if len(repaired) != 0 {
		t.Errorf("repaired = %v for a materialized name whose routing step is "+
			"already at ordinal 1, want none", repaired)
	}
	if stepExists(t, conn, workflow.RenderInstance(workflow.HeldStepName("assess"), 1, nil)) {
		t.Error("a held row was minted at ordinal 1; expansion never writes one, " +
			"and the routing step materializes its own if it holds again")
	}
	if got := countIssueSteps(t, conn, issue); got != before {
		t.Errorf("step count = %d, want %d — a materialized name adds no rows", got, before)
	}
}

// ---------------------------------------------------------------------------
// §11.3 (1): the bound, and what it leaves behind
// ---------------------------------------------------------------------------

// TestBoundedEntryLeavesTheFinalOrdinalLive is DKT-78's starvation.
//
// A bounded entry instantiates NOTHING (§11.3 (1): it "is not a loop entry, it
// is a park"), so the ordinal it was refused at is the last one that will ever
// exist — and every step of that ordinal, `verify-ac` included, is the only
// instance of itself the run will ever have. The counter must therefore not
// move past it: `StaleLineage` calls any step below the issue's `loop_count` a
// superseded lineage whose routing applies NO downstream effect, so a counter
// left one above the highest instantiated ordinal makes the whole final ordinal
// inert.
//
// The consequence is exactly the DKT-78 report. `verify-ac` at the final
// ordinal still runs — a human unparks the driver, R3 clears, a worker claims
// it — and its verdict decides nothing: the threshold cannot route, the issue
// cannot reconcile, and an unmet acceptance criterion passes silently into the
// commit. A run in that state looks finished and is not.
func TestBoundedEntryLeavesTheFinalOrdinalLive(t *testing.T) {
	conn := mustDB(t)
	run, issue := startedStarvationRun(t, conn)
	e := testEngine()

	// Two legal entries at max_fix_loops = 2, then the third that is refused.
	enterLoopAt(t, conn, e, 0)
	enterLoopAt(t, conn, e, 1)
	enterLoopAt(t, conn, e, 2)

	if got := stepStatus(t, conn, "assess@2"); got != db.StepWaitingHuman {
		t.Fatalf("assess@2 = %q after the bound, want %q", got, db.StepWaitingHuman)
	}
	if stepExists(t, conn, "verify-ac@3") {
		t.Fatal("verify-ac@3 exists; a bounded entry instantiates nothing")
	}

	// THE COUNTER MUST NAME AN ORDINAL THAT EXISTS. Ordinal 2 is the highest
	// instantiated one, so a `loop_count` of 3 declares every ordinal-2
	// instance stale — including the only `verify-ac` the run has left.
	if got := loopCount(t, conn, run.ID, issue); got != 2 {
		t.Errorf("loop_count = %d after a bounded entry, want 2 — the bound "+
			"instantiated no ordinal 3, so a counter above 2 makes the final "+
			"ordinal a superseded lineage with nothing to succeed it", got)
	}

	if staleLineage(t, conn, "verify-ac@2") {
		t.Error("verify-ac@2 reads as a stale lineage after the bound; its " +
			"routing would apply no downstream effect, so the check it " +
			"performs could not stop the commit")
	}
}

// TestFinalOrdinalVerifyRunsBeforeTheIssueCompletes closes DKT-78's direction —
// "guarantee a verify execution on final loop exit before commit-gate" — as the
// property the operator cares about rather than as a mechanism.
//
// The run is taken past the bound, the park is resolved as an operator would
// resolve it, and the tail is driven to the end. Two things must hold: the
// issue must NOT be complete while the final ordinal's `verify-ac` is pending,
// and once it and its successor finish, the issue must actually complete.
//
// The second half is not a formality. Completion is written by the reconcile a
// step's routing triggers, and a stale lineage skips reconciling by design — so
// a final ordinal wrongly marked stale finishes every step and completes
// nothing, which reads to an operator as a run that stopped for no reason.
func TestFinalOrdinalVerifyRunsBeforeTheIssueCompletes(t *testing.T) {
	conn := mustDB(t)
	_, issue := startedStarvationRun(t, conn)
	e := testEngine()

	enterLoopAt(t, conn, e, 0)
	enterLoopAt(t, conn, e, 1)
	enterLoopAt(t, conn, e, 2)

	// The operator accepts the parked driver. `verify-ac@2` is now the only
	// unfinished work between here and the commit.
	err := e.ResolveStep(conn, stepIDByInstance(t, conn, "assess@2"),
		ResolveOverridePass, "accepted", nowMS)
	testsupport.Must(t, err, "resolve assess@2: %v", err)

	if got := stepStatus(t, conn, "verify-ac@2"); got != db.StepPending {
		t.Fatalf("verify-ac@2 = %q after the park was resolved, want %q",
			got, db.StepPending)
	}
	if got := issueStatus(t, conn, issue); got == "done" {
		t.Error("the issue completed with a pending final-ordinal verify-ac; " +
			"completion over highest-ordinal instances must hold the issue " +
			"open until the check it depends on has run")
	}

	claimAndComplete(t, conn, e, "verify-ac@2", "the ac report", metPayload)
	claimAndComplete(t, conn, e, "commit@2", "the commit record", "")

	if got := issueStatus(t, conn, issue); got != "done" {
		t.Errorf("issue status = %q after the final ordinal finished, want done — "+
			"a final ordinal that cannot reconcile leaves the run stopped with "+
			"every step terminal", got)
	}
}

// TestBoundedEntryRoutesTheFinalOrdinalVerify is the sharpest statement of what
// DKT-78 lost: the check at the final ordinal must be able to ROUTE.
//
// `verify-ac` earns its cost only by what it does with an unmet criterion, and
// what it does is route. A final-ordinal instance whose lineage reads stale
// records its routing for the ledger and applies nothing — the failure DKT-46
// showed, where the discrepancy was caught by other means entirely and the
// step built to catch it had already been reduced to a bystander.
func TestBoundedEntryRoutesTheFinalOrdinalVerify(t *testing.T) {
	conn := mustDB(t)
	run, issue := startedStarvationRun(t, conn)
	e := testEngine()

	enterLoopAt(t, conn, e, 0)
	enterLoopAt(t, conn, e, 1)
	enterLoopAt(t, conn, e, 2)

	err := e.ResolveStep(conn, stepIDByInstance(t, conn, "assess@2"),
		ResolveOverridePass, "accepted", nowMS)
	testsupport.Must(t, err, "resolve assess@2: %v", err)

	// The final ordinal's check finds an unmet criterion. Its `fix-loop` is
	// refused by the same bound — the loop is exhausted — so the step PARKS for
	// an operator, which is the outcome that stops the commit.
	claimAndComplete(t, conn, e, "verify-ac@2", "the ac report", unmetPayload)

	if got := stepStatus(t, conn, "verify-ac@2"); got != db.StepWaitingHuman {
		t.Errorf("verify-ac@2 = %q on an unmet criterion at the exhausted "+
			"bound, want %q — a check whose routing applies nothing cannot "+
			"stop a commit", got, db.StepWaitingHuman)
	}
	if got := issueStatus(t, conn, issue); got == "done" {
		t.Error("the issue completed with an unmet criterion parked on verify-ac@2")
	}
	if got := stepStatus(t, conn, "commit@2"); got == db.StepDone {
		t.Error("commit@2 is done past a parked verify-ac@2")
	}
	if got := loopCount(t, conn, run.ID, issue); got != 2 {
		t.Errorf("loop_count = %d after a second refused entry, want 2 — a "+
			"refusal creates no ordinal, so it must not advance the counter", got)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// staleLineage answers §11.3 (2)'s inert half for one instance, in a
// transaction of its own — the predicate reads the issue's `loop_count`, so
// asking it is a database question and not a scheduler one.
func staleLineage(t *testing.T, conn *sql.DB, instance string) bool {
	t.Helper()

	step, err := db.GetStep(conn, stepIDByInstance(t, conn, instance))
	testsupport.Must(t, err, "GetStep(%s): %v", instance, err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	stale, err := StaleLineage(tx, step)
	testsupport.Must(t, err, "StaleLineage(%s): %v", instance, err)
	return stale
}

// assertSupersededHaveSuccessors requires that every instance this issue has in
// `superseded` names a step with an instance at `ordinal`.
//
// A MATERIALIZED held step is mapped back to the routing step it belongs to
// before the lookup, the same mapping the sweep uses to take it in the first
// place (H17): its name is not in the definition, so expansion never writes one
// at the new ordinal, and its successor is its routing step's new instance.
func assertSupersededHaveSuccessors(t *testing.T, conn *sql.DB, issueID, ordinal int) {
	t.Helper()

	atOrdinal := make(map[string]bool)
	rows, err := conn.Query(
		`SELECT step_name FROM steps WHERE issue_id = ? AND ordinal = ?`,
		issueID, ordinal)
	testsupport.Must(t, err, "reading ordinal %d: %v", ordinal, err)
	for rows.Next() {
		var name string
		testsupport.Must(t, rows.Scan(&name), "reading a step name: %v", err)
		atOrdinal[name] = true
	}
	rows.Close()

	swept, err := conn.Query(
		`SELECT instance FROM steps WHERE issue_id = ? AND status = ?`,
		issueID, db.StepSuperseded)
	testsupport.Must(t, err, "reading the superseded set: %v", err)
	defer swept.Close()

	found := 0
	for swept.Next() {
		var instance string
		testsupport.Must(t, swept.Scan(&instance), "reading an instance: %v", err)
		found++

		name, _, _, err := workflow.ParseInstance(instance)
		testsupport.Must(t, err, "ParseInstance(%q): %v", instance, err)
		if routing, ok := workflow.RoutingStepNameOf(name); ok {
			name = routing
		}
		if !atOrdinal[name] {
			t.Errorf("%s was superseded with no instance of %q at ordinal %d — "+
				"a swept step with no successor is work the run will never do",
				instance, name, ordinal)
		}
	}
	if found == 0 {
		t.Fatal("nothing was superseded; the invariant was not exercised")
	}
}
