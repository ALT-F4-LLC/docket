package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// exhaustionSrc is dkt587's threshold fixture with `on_exhausted = "TARGET"`
// declared beside the bound. `panel` is a vote step and `drain` an executor,
// both ordered behind `check`, so the four cases below differ only in the
// value the exhaustion routes to.
const exhaustionSrc = `
[pipeline]
name = "exhaustion"
version = 1

[match]
kind = ["task"]

[[step]]
name = "check"
executor = "check"
emits = "findings"
threshold = { "fix-loop" = "any(status == unmet)" }
max_fix_loops = 1
on_exhausted = "TARGET"

[[step]]
name = "fix"
executor = "fix"
emits = "findings"
loop = true
after_loop = "check"

[[step]]
name = "panel"
type = "vote"
voters = ["seat-a"]
vote_rule = "majority"
on_fail = "abandon-issue"
after = ["check"]

[[step]]
name = "drain"
executor = "drain"
emits = "notes"
after = ["check"]
`

// TestFixLoopExhaustionRouting is criterion 2: the routing a fix-loop
// exhaustion takes is the one `on_exhausted` declares, and the same
// transaction records the loop history — rounds run against the cap, the
// instance that triggered the loop, and the verdict the last round ended on.
//
// `max_fix_loops = 1` so each case spends the budget in one round: check@0
// enters ordinal 1, and check@1 exhausts.
func TestFixLoopExhaustionRouting(t *testing.T) {
	t.Run("the default parks waiting-human", func(t *testing.T) {
		conn, _ := exhaust(t, workflow.OnFailWaitingHuman)

		step := mustStep(t, conn, "check@1")
		if step.Status != db.StepWaitingHuman {
			t.Errorf("check@1 = %q, want %q", step.Status, db.StepWaitingHuman)
		}
		if step.ParkClass != db.ParkClassLoopBound {
			t.Errorf("park class = %q, want %q", step.ParkClass, db.ParkClassLoopBound)
		}
		assertLoopHistory(t, step)
	})

	t.Run("abandon-issue abandons", func(t *testing.T) {
		conn, _ := exhaust(t, workflow.OnFailAbandonIssue)

		step := mustStep(t, conn, "check@1")
		if step.Status != db.StepFailedRouted {
			t.Errorf("check@1 = %q, want %q", step.Status, db.StepFailedRouted)
		}
		if !routingIs(step.Routing, workflow.OnFailAbandonIssue) {
			t.Errorf("check@1 routing = %q, want %q",
				step.Routing, workflow.OnFailAbandonIssue)
		}
		issue, err := db.GetIssue(conn, step.IssueID)
		testsupport.Must(t, err, "reading the issue: %v", err)
		if issue.Resolution != db.IssueResolutionAbandoned {
			t.Errorf("issue resolution = %q, want %q — the exhaustion routing "+
				"must abandon, not merely record a routing",
				issue.Resolution, db.IssueResolutionAbandoned)
		}
		assertLoopHistory(t, step)
	})

	t.Run("a named vote step opens its proposal", func(t *testing.T) {
		conn, runID := exhaust(t, "panel")

		step := mustStep(t, conn, "check@1")
		if step.Status == db.StepWaitingHuman {
			t.Error("check@1 parked; the vote step was declared precisely so " +
				"exhaustion does not wait on an operator")
		}
		if !routingIs(step.Routing, "panel") {
			t.Errorf("check@1 routing = %q, want the panel %q", step.Routing, "panel")
		}
		if step.ParkClass != "" {
			t.Errorf("park class = %q on a routed step, want none", step.ParkClass)
		}
		assertLoopHistory(t, step)

		assertReady(t, conn, runID, "panel@1")

		panel := mustStep(t, conn, "panel@1")
		spec := workflow.StepByName(defOf(t, conn, panel), "panel")
		id, err := OpenVoteProposal(conn, panel, spec, nowMS)
		testsupport.Must(t, err, "opening panel@1's proposal: %v", err)
		if id == 0 {
			t.Error("no proposal opened for panel@1 — the panel was never asked")
		}
	})

	t.Run("a named executor step is instantiated and ready", func(t *testing.T) {
		conn, runID := exhaust(t, "drain")

		step := mustStep(t, conn, "check@1")
		if step.Status == db.StepWaitingHuman {
			t.Error("check@1 parked; the executor step was declared precisely " +
				"so the file-and-stop default runs machine-side")
		}
		if !routingIs(step.Routing, "drain") {
			t.Errorf("check@1 routing = %q, want %q", step.Routing, "drain")
		}
		assertLoopHistory(t, step)

		if !stepExists(t, conn, "drain@1") {
			t.Fatal("drain@1 was never instantiated")
		}
		assertReady(t, conn, runID, "drain@1")
	})
}

// exhaust runs one issue's loop until `max_fix_loops = 1` refuses the second
// entry, under a workflow whose exhaustion routes to target.
func exhaust(t *testing.T, target string) (*sql.DB, int) {
	t.Helper()
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	runID, _ := activateInterposed(
		t, conn, strings.Replace(exhaustionSrc, "TARGET", target, 1))
	e := testEngine()

	claimAndComplete(t, conn, e, "check@0", roundReport(0), unmetPayload)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("fix@1 was not instantiated by the first entry")
	}
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", roundReport(1), unmetPayload)

	if stepExists(t, conn, "fix@2") {
		t.Fatal("fix@2 exists; max_fix_loops = 1 must refuse the second entry")
	}
	return conn, runID
}

// assertReady pins the readiness latch's answer, which is what a routing to a
// step name buys: the target's row stays `pending` until an invocation
// dispatches it, and `Ready` is the scheduler's own verdict about whether it
// may be.
func assertReady(t *testing.T, conn *sql.DB, runID int, instance string) {
	t.Helper()
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, instance)); !ok {
			t.Errorf("%s not ready after the exhaustion routed to it: %q",
				instance, cond)
		}
	})
}

// assertLoopHistory pins the three facts the routing transaction records: one
// round ran against the cap, `check@1` is the instance whose verdict opened
// the loop, and the round ended on a `fix-loop` verdict.
func assertLoopHistory(t *testing.T, step *db.Step) {
	t.Helper()
	if step.LoopRoundsRun != 1 {
		t.Errorf("loop_rounds_run = %d, want 1", step.LoopRoundsRun)
	}
	if step.LoopTriggerStep != "check@1" {
		t.Errorf("loop_trigger_step = %q, want %q", step.LoopTriggerStep, "check@1")
	}
	if step.LoopLatestVerdict != workflow.OnFailFixLoop {
		t.Errorf("loop_latest_verdict = %q, want %q",
			step.LoopLatestVerdict, workflow.OnFailFixLoop)
	}
}

func defOf(t *testing.T, conn *sql.DB, step *db.Step) *workflow.Definition {
	t.Helper()
	defs, err := StepDefinitions(conn, step.RunID)
	testsupport.Must(t, err, "reading the run's definitions: %v", err)
	def := defs[step.WorkflowID]
	if def == nil {
		t.Fatalf("no pinned definition for workflow %d", step.WorkflowID)
	}
	return def
}
