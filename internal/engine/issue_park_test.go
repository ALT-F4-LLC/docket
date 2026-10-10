package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A park is the ISSUE's, not the run's (R2b, ready.go; reconcileRun).
//
// RUN-90 pinned the old behaviour's cost: eleven parks, each one issue's
// verify reporting an unverifiable AC, and each rolled up to the run — R1
// then refused every claim, so 1372 of the 1656 rows dispatched across the
// session never launched, and the operator was the critical path for all 45
// issues every time one of them asked a question.
//
// The shape here is two issues on one run. A parks; B must still schedule,
// the run must still read `active`, A's own later rows must refuse with the
// park named, and the run reads `waiting-human` only once B is finished — at
// which point resolving A's park returns it to `active` exactly as before.
func TestAStepParkHoldsItsIssueNotTheRun(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issueA := createIssue(t, conn, "issue A", "a body", "task", nil)
	issueB := createIssue(t, conn, "issue B", "a body", "task", nil)
	run := startRun(t, conn, issueA, issueB)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	if got := runStatusOf(t, conn, run.ID); got != string(model.RunActive) {
		t.Fatalf("premise: run is %q after activation, want %q", got, model.RunActive)
	}

	// A's implement parks on an operator decision.
	execSQL(t, conn,
		`UPDATE steps SET status = ? WHERE run_id = ? AND issue_id = ? AND instance = 'implement@0'`,
		string(db.StepWaitingHuman), run.ID, issueA)
	rollup(t, conn, run.ID)

	if got := runStatusOf(t, conn, run.ID); got != string(model.RunActive) {
		t.Fatalf("after A's park the run is %q, want %q: B still has unfinished work", got, model.RunActive)
	}

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepOf(t, sched, issueB, "implement@0")); !ok {
			t.Fatalf("B's implement@0 is not ready (%s): A's park must hold only A", cond)
		}
		for _, step := range sched.Steps() {
			if step.IssueID != issueA || step.Status != db.StepPending {
				continue
			}
			if ok, cond := sched.Ready(step); ok || cond != CondIssueParked {
				t.Fatalf("A's %s: ready=%v cond=%q, want the park named (%q)",
					step.Instance, ok, cond, CondIssueParked)
			}
		}
	})

	// B finishes. Nothing unparked is left, so NOW the run waits on a person.
	execSQL(t, conn,
		`UPDATE steps SET status = ? WHERE run_id = ? AND issue_id = ?`,
		string(db.StepDone), run.ID, issueB)
	rollup(t, conn, run.ID)
	if got := runStatusOf(t, conn, run.ID); got != string(model.RunWaitingHuman) {
		t.Fatalf("with only A's park left the run is %q, want %q", got, model.RunWaitingHuman)
	}

	// The operator resolves the park; the rollup resumes what it parked.
	execSQL(t, conn,
		`UPDATE steps SET status = ? WHERE run_id = ? AND issue_id = ? AND instance = 'implement@0'`,
		string(db.StepDone), run.ID, issueA)
	rollup(t, conn, run.ID)
	if got := runStatusOf(t, conn, run.ID); got != string(model.RunActive) {
		t.Fatalf("after the park is resolved the run is %q, want %q", got, model.RunActive)
	}
}

// A one-issue run keeps the old shape: its only issue's park is all the work
// there is, so the run rolls up to `waiting-human` at once.
func TestASoleIssuesParkStillParksTheRun(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	execSQL(t, conn,
		`UPDATE steps SET status = ? WHERE run_id = ? AND issue_id = ? AND instance = 'implement@0'`,
		string(db.StepWaitingHuman), run.ID, issue)
	rollup(t, conn, run.ID)
	if got := runStatusOf(t, conn, run.ID); got != string(model.RunWaitingHuman) {
		t.Fatalf("a sole issue's park leaves the run %q, want %q", got, model.RunWaitingHuman)
	}
}

// rollup runs reconcileRun in its own transaction, the way every routing
// transaction ends.
func rollup(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	testsupport.Must(t, reconcileRun(tx, runID, nowMS), "reconcileRun: %v", err)
	testsupport.Must(t, tx.Commit(), "commit: %v", err)
}

// stepOf finds one issue's step by instance in a loaded scheduler —
// stepNamed is ambiguous once two issues expand the same workflow.
func stepOf(t *testing.T, sched *Scheduler, issueID int, instance string) *db.Step {
	t.Helper()
	for _, step := range sched.Steps() {
		if step.IssueID == issueID && step.Instance == instance {
			return step
		}
	}
	t.Fatalf("no step %q for issue %d in the snapshot", instance, issueID)
	return nil
}
