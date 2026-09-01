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

// exhaustTheLoop drives the fixture's issue past `max_fix_loops = 2`, leaving
// `verify@2` parked with nothing able to schedule another round.
func exhaustTheLoop(t *testing.T, conn *sql.DB, e *Engine) {
	t.Helper()
	for k := range 3 {
		driveToVerify(t, conn, e, k)
		claimAndComplete(t, conn, e, fmt.Sprintf("verify@%d", k), roundReport(k), unmetPayload)
	}
}

// TestFixRoundReentersAnExhaustedLoop is DKT-237.
//
// Exhausting `max_fix_loops` parks the issue, correctly — but nothing could
// then mint another round. After HRN-26's third round verify-ac read 7/14
// acceptance criteria unmet and design-qa held 2 blockers, the workflow
// scheduled no further fix round, and no verb could ask for one. The fix was
// built OUTSIDE the engine instead: an agent, a 1,128-insertion commit
// cherry-picked with no judge review as a step, ~100,923 output tokens in no
// ledger. A park that names no next move is what makes going around the
// engine the reasonable choice.
func TestFixRoundReentersAnExhaustedLoop(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	exhaustTheLoop(t, conn, e)

	// The state DKT-237 describes: parked, bound exhausted, no fix@3.
	if got := stepStatus(t, conn, "verify@2"); got != db.StepWaitingHuman {
		t.Fatalf("verify@2 = %q, want the exhausted-loop park", got)
	}
	if stepExists(t, conn, "fix@3") {
		t.Fatal("premise: fix@3 must not exist before the re-entry")
	}

	// The park now NAMES the way out, because a refusal that names no next
	// move is half the defect.
	if routing := stepRoutingRaw(t, conn, "verify@2"); !strings.Contains(routing, "fix-round") {
		t.Errorf("the park does not name the re-entry verb: %q", routing)
	}

	// The operator authorizes one more round.
	verifyID := stepIDByInstance(t, conn, "verify@2")
	err := e.ResolveStep(conn, verifyID, ResolveFixRound,
		"7/14 ACs unmet and 2 design blockers stand", nowMS)
	testsupport.Must(t, err, "resolve --as fix-round: %v", err)

	// A FRESH ROUND exists — the whole point. Not a re-run of the check that
	// reported the problem, but new work on it, judged like every other round.
	if !stepExists(t, conn, "fix@3") {
		t.Error("fix@3 was not instantiated; the re-entry minted no work")
	}
	if got := loopCount(t, conn, run.ID, issue); got != 3 {
		t.Errorf("loop_count = %d after the re-entry, want 3", got)
	}

	// The grant is RECORDED, so an auditor can see that the bound moved and
	// who moved it — rather than finding a workflow whose declared bound was
	// quietly edited.
	grants := loopGrants(t, conn, run.ID, issue)
	if grants != 1 {
		t.Errorf("loop_grants = %d after one authorization, want 1", grants)
	}

	// The parked step is SUPERSEDED, not passed: the question it asked is
	// answered by the new round's work, and calling it done would assert a
	// verdict nobody reached.
	if got := stepStatus(t, conn, "verify@2"); got != db.StepSuperseded {
		t.Errorf("verify@2 = %q after the re-entry, want %q — passing it would "+
			"record a verdict the re-entry explicitly did not make",
			got, db.StepSuperseded)
	}

	// The note reaches the row, so the ruling is on the record (and, per
	// DKT-247, in the next packet).
	if routing := stepRoutingRaw(t, conn, "verify@2"); !strings.Contains(routing, "7/14") {
		t.Errorf("the ruling did not reach the row: %q", routing)
	}
}

// TestFixRoundGrantsOneRoundOnly keeps the authorization narrow: one grant
// buys one round, and the bound reasserts itself immediately after.
func TestFixRoundGrantsOneRoundOnly(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	exhaustTheLoop(t, conn, e)
	verifyID := stepIDByInstance(t, conn, "verify@2")
	testsupport.Must(t,
		e.ResolveStep(conn, verifyID, ResolveFixRound, "once", nowMS),
		"first re-entry")

	// Round 3 runs and fails again. The bound — now 2 declared + 1 granted —
	// is exhausted again, so it parks again rather than looping on.
	driveToVerify(t, conn, e, 3)
	claimAndComplete(t, conn, e, "verify@3", roundReport(3), unmetPayload)

	if stepExists(t, conn, "fix@4") {
		t.Error("fix@4 exists; one grant must buy exactly one round")
	}
	if got := stepStatus(t, conn, "verify@3"); got != db.StepWaitingHuman {
		t.Errorf("verify@3 = %q, want the park again — a grant is not a "+
			"standing exemption", got)
	}
	if got := loopGrants(t, conn, run.ID, issue); got != 1 {
		t.Errorf("loop_grants = %d, want it unchanged at 1", got)
	}

	// And a second authorization buys a second round, so the operator can keep
	// deciding rather than being told no.
	testsupport.Must(t,
		e.ResolveStep(conn, stepIDByInstance(t, conn, "verify@3"),
			ResolveFixRound, "twice", nowMS),
		"second re-entry")
	if !stepExists(t, conn, "fix@4") {
		t.Error("a second authorization minted no round")
	}
	if got := loopGrants(t, conn, run.ID, issue); got != 2 {
		t.Errorf("loop_grants = %d after two authorizations, want 2", got)
	}
}

// TestFixRoundLeavesTheWorkflowBoundAlone is why this is a grant and not an
// edit: the workflow's declared policy is unchanged, so a SECOND issue on the
// same workflow still gets the bound its author wrote.
func TestFixRoundLeavesTheWorkflowBoundAlone(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	first := createIssue(t, conn, "first", "body", "task", nil)
	second := createIssue(t, conn, "second", "body", "task", nil)
	run := startRun(t, conn, first, second)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if got := loopGrants(t, conn, run.ID, second); got != 0 {
		t.Errorf("an untouched issue carries %d grants, want 0", got)
	}

	// A grant on the FIRST issue must not appear on the second: the decision
	// was about one issue, and the row it lives on says so.
	tx, err := conn.Begin()
	testsupport.Must(t, err, "begin: %v", err)
	_, err = db.GrantLoopTx(tx, run.ID, first)
	testsupport.Must(t, err, "GrantLoopTx: %v", err)
	testsupport.Must(t, tx.Commit(), "commit: %v", err)

	if got := loopGrants(t, conn, run.ID, first); got != 1 {
		t.Errorf("the granted issue reports %d, want 1", got)
	}
	if got := loopGrants(t, conn, run.ID, second); got != 0 {
		t.Errorf("the OTHER issue reports %d grants; a grant is one decision "+
			"about one issue", got)
	}
}

// TestFixRoundIsNotRetry pins the distinction the vocabulary exists for.
func TestFixRoundIsNotRetry(t *testing.T) {
	var found bool
	for _, v := range resolveValues {
		if v == ResolveFixRound {
			found = true
		}
	}
	if !found {
		t.Error("fix-round is missing from the closed --as vocabulary, so a " +
			"bad --as will not name it as an option")
	}
	if ResolveFixRound == ResolveRetry {
		t.Error("fix-round and retry collapsed into one value; retry re-runs " +
			"the check, fix-round schedules work on what the check found")
	}
	if workflow.OnFailFixLoop == "" {
		t.Error("the fix-loop routing constant is empty")
	}
}

// loopGrants reads the operator-authorized extra loops for an issue.
func loopGrants(t *testing.T, conn *sql.DB, runID, issueID int) int {
	t.Helper()
	var grants int
	err := conn.QueryRow(
		`SELECT loop_grants FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID).Scan(&grants)
	testsupport.Must(t, err, "reading loop_grants: %v", err)
	return grants
}
