package engine

import (
	"database/sql"
	"fmt"
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
payload = "round-verdict@1"
verdict_field = "verdict"
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

// roundVerdictSchema is `check`'s pinned payload: the `status` its threshold
// reads, and the optional `verdict` property its `verdict_field` declares.
const roundVerdictSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "status":  {"type": "string", "enum": ["met", "unmet"]},
      "verdict": {"type": "string"}
    }
  }
}`

// verdictPayload routes `check` to `fix-loop` like unmetPayload, and records
// a round verdict for the loop history to copy.
const verdictPayload = `[{"status":"unmet","verdict":"request-changes"}]`

// TestFixLoopExhaustionRouting is criterion 2: the routing a fix-loop
// exhaustion takes is the one `on_exhausted` declares, and the same
// transaction records the loop history — rounds run against the cap, the
// instance that triggered the loop, and the verdict the last round recorded.
//
// `max_fix_loops = 1` so each case spends the budget in one round: check@0
// enters ordinal 1, and check@1 exhausts.
func TestFixLoopExhaustionRouting(t *testing.T) {
	t.Run("the default parks waiting-human", func(t *testing.T) {
		conn, _ := exhaustRecording(t, workflow.OnFailWaitingHuman, verdictPayload)

		step := mustStep(t, conn, "check@1")
		if step.Status != db.StepWaitingHuman {
			t.Errorf("check@1 = %q, want %q", step.Status, db.StepWaitingHuman)
		}
		if step.ParkClass != db.ParkClassLoopBound {
			t.Errorf("park class = %q, want %q", step.ParkClass, db.ParkClassLoopBound)
		}
		assertLoopHistory(t, step, "request-changes")
	})

	t.Run("a round that recorded no verdict stores none", func(t *testing.T) {
		conn, _ := exhaustRecording(t, workflow.OnFailWaitingHuman, unmetPayload)

		step := mustStep(t, conn, "check@1")
		if step.Status != db.StepWaitingHuman {
			t.Errorf("check@1 = %q, want %q", step.Status, db.StepWaitingHuman)
		}
		assertLoopHistory(t, step, "")
	})

	t.Run("abandon-issue abandons", func(t *testing.T) {
		conn, _ := exhaustRecording(t, workflow.OnFailAbandonIssue, verdictPayload)

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
		assertLoopHistory(t, step, "request-changes")
	})

	t.Run("a named vote step opens its proposal", func(t *testing.T) {
		conn, runID := exhaustRecording(t, "panel", verdictPayload)

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
		assertLoopHistory(t, step, "request-changes")

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
		conn, runID := exhaustRecording(t, "drain", verdictPayload)

		step := mustStep(t, conn, "check@1")
		if step.Status == db.StepWaitingHuman {
			t.Error("check@1 parked; the executor step was declared precisely " +
				"so the file-and-stop default runs machine-side")
		}
		if !routingIs(step.Routing, "drain") {
			t.Errorf("check@1 routing = %q, want %q", step.Routing, "drain")
		}
		assertLoopHistory(t, step, "request-changes")

		if !stepExists(t, conn, "drain@1") {
			t.Fatal("drain@1 was never instantiated")
		}
		assertReady(t, conn, runID, "drain@1")
	})
}

// TestFixLoopExhaustionRecordsTheAppliedBound: a refused entry stores the
// bound that refused it beside the rest of the loop history, on each of the
// three refusal paths, and an admitted entry stores none. Every fixture's
// applied bound differs from the trigger's declared `max_fix_loops`, so a
// write of the declared value cannot pass.
func TestFixLoopExhaustionRecordsTheAppliedBound(t *testing.T) {
	t.Run("max_fix_loops plus a grant", func(t *testing.T) {
		conn := exhaustAfterRounds(t, "max_fix_loops = 2\n", 1, 3)

		if got := loopCapOf(t, conn, "check@3"); got != 3 {
			t.Errorf("check@3 loop_cap = %d, want the applied bound 3 "+
				"(max_fix_loops 2 + 1 grant)", got)
		}
		// check@0's entry was admitted at round one under max_fix_loops = 2.
		if got := loopCapOf(t, conn, "check@0"); got != 0 {
			t.Errorf("check@0 loop_cap = %d after an admitted entry, want 0", got)
		}
	})

	t.Run("the max_fix_loops_hard clamp", func(t *testing.T) {
		conn := exhaustAfterRounds(t, "max_fix_loops = 2\nmax_fix_loops_hard = 4\n", 3, 4)

		if got := loopCapOf(t, conn, "check@4"); got != 4 {
			t.Errorf("check@4 loop_cap = %d, want the hard clamp 4 "+
				"(max_fix_loops 2 + 3 grants clamped to max_fix_loops_hard 4)", got)
		}
	})

	t.Run("a cluster's max_fix_loops", func(t *testing.T) {
		conn := mustDB(t)
		activateInterposed(t, conn,
			strings.Replace(clusterSrc, "max_fix_loops = 2\n", "max_fix_loops = 3\n", 1))
		e := testEngine()

		claimAndComplete(t, conn, e, "draft@0", "the draft", "")
		claimAndComplete(t, conn, e, "prd-gate@0", roundReport(0), blockedPayload)
		driveFixtureRound(t, 1)
		claimAndComplete(t, conn, e, "prd-fix@1", "the fixed draft", "")
		claimAndComplete(t, conn, e, "prd-gate@1", roundReport(1), blockedPayload)

		if got := stepStatus(t, conn, "prd-gate@1"); got != db.StepWaitingHuman {
			t.Fatalf("premise: prd-gate@1 = %q, want the cluster-bound park %q",
				got, db.StepWaitingHuman)
		}
		if got := loopCapOf(t, conn, "prd-gate@1"); got != 1 {
			t.Errorf("prd-gate@1 loop_cap = %d, want the cluster's bound 1 "+
				"(under the step's max_fix_loops 3)", got)
		}
	})
}

// exhaustAfterRounds activates exhaustionSrc with bounds in place of its
// `max_fix_loops = 1`, records grants loop grants for the issue, and runs
// rounds admitted rounds; check@rounds then routes the refused entry.
func exhaustAfterRounds(t *testing.T, bounds string, grants, rounds int) *sql.DB {
	t.Helper()
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSchemaFixture(t, conn, "round-verdict", 1, roundVerdictSchema)
	src := strings.Replace(exhaustionSrc, "TARGET", workflow.OnFailWaitingHuman, 1)
	src = strings.Replace(src, "max_fix_loops = 1\n", bounds, 1)
	runID, issueID := activateInterposed(t, conn, src)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "opening the grant transaction: %v", err)
	for range grants {
		_, err := db.GrantLoopTx(tx, runID, issueID)
		testsupport.Must(t, err, "granting a loop: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "committing the grants")

	e := testEngine()
	for k := range rounds {
		claimAndComplete(t, conn, e, fmt.Sprintf("check@%d", k), roundReport(k), unmetPayload)
		next := fmt.Sprintf("fix@%d", k+1)
		if !stepExists(t, conn, next) {
			t.Fatalf("premise: %s was not instantiated; round %d is inside the bound", next, k+1)
		}
		driveFixtureRound(t, k+1)
		claimAndComplete(t, conn, e, next, "the fix", "")
	}
	last := fmt.Sprintf("check@%d", rounds)
	claimAndComplete(t, conn, e, last, roundReport(rounds), unmetPayload)
	if stepExists(t, conn, fmt.Sprintf("fix@%d", rounds+1)) {
		t.Fatalf("premise: fix@%d exists; %s's entry must be refused", rounds+1, last)
	}
	if got := stepStatus(t, conn, last); got != db.StepWaitingHuman {
		t.Fatalf("premise: %s = %q, want the loop-bound park %q",
			last, got, db.StepWaitingHuman)
	}
	return conn
}

// loopCapOf reads the bound stored on instance's row. db.Step does not carry
// the column, so the test reads it where the loop history writes it.
func loopCapOf(t *testing.T, conn *sql.DB, instance string) int {
	t.Helper()
	var loopCap int
	err := conn.QueryRow(`SELECT loop_cap FROM steps WHERE id = ?`,
		mustStep(t, conn, instance).ID).Scan(&loopCap)
	testsupport.Must(t, err, "reading %s's loop_cap: %v", instance, err)
	return loopCap
}

// exhaust runs one issue's loop until `max_fix_loops = 1` refuses the second
// entry, under a workflow whose exhaustion routes to target. check@1, the
// round that exhausts, records verdictPayload.
func exhaust(t *testing.T, target string) (*sql.DB, int) {
	t.Helper()
	return exhaustRecording(t, target, verdictPayload)
}

// exhaustRecording is exhaust with check@1 recording lastPayload.
func exhaustRecording(t *testing.T, target, lastPayload string) (*sql.DB, int) {
	t.Helper()
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSchemaFixture(t, conn, "round-verdict", 1, roundVerdictSchema)
	runID, _ := activateInterposed(
		t, conn, strings.Replace(exhaustionSrc, "TARGET", target, 1))
	e := testEngine()

	claimAndComplete(t, conn, e, "check@0", roundReport(0), unmetPayload)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("fix@1 was not instantiated by the first entry")
	}
	driveFixtureRound(t, 1)
	claimAndComplete(t, conn, e, "fix@1", "the fix", "")
	claimAndComplete(t, conn, e, "check@1", roundReport(1), lastPayload)

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
// the loop, and loop_latest_verdict holds the verdict check@1's payload
// recorded, verbatim — "" when it recorded none, never the routing token.
func assertLoopHistory(t *testing.T, step *db.Step, verdict string) {
	t.Helper()
	if step.LoopRoundsRun != 1 {
		t.Errorf("loop_rounds_run = %d, want 1", step.LoopRoundsRun)
	}
	if step.LoopTriggerStep != "check@1" {
		t.Errorf("loop_trigger_step = %q, want %q", step.LoopTriggerStep, "check@1")
	}
	if step.LoopLatestVerdict != verdict {
		t.Errorf("loop_latest_verdict = %q, want %q", step.LoopLatestVerdict, verdict)
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
