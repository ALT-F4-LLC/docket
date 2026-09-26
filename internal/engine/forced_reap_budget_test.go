package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-585: a reap carrying `data.forced` does not consume the step's attempt
// budget. RUN-30 STEP-755: implement@0's attempt 1 was force-reaped after an
// accidental hotkey interrupt killed the wave — a relay declaring a dead spawn,
// not an executor failure — yet the step showed attempts 2 of max_attempts 2
// after the successor succeeded, one interrupt from `waiting-human` on a
// healthy charter. The fix is a classification at the forced call site: a +1
// nudge of `attempt_base` (never a touch of `attempt`, the usage ledger's key),
// so the exhaustion math `attempt - attempt_base >= max_attempts` skips exactly
// the dead attempt. An ordinary TTL expiry still charges as before.

const forcedBudgetSrc = `
[pipeline]
name = "forced-budget"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "w"
emits = "out"
max_attempts = 2
on_fail = "waiting-human"
`

// TestForcedReapDoesNotConsumeAttemptBudget is the acceptance criterion
// verbatim, in RUN-30's own shape: attempt 1 force-reaped, attempt 2 is a real
// try — and with `max_attempts = 2` the step must NOT be exhausted by it. The
// budget still bounds: the next genuine failure after that does exhaust.
func TestForcedReapDoesNotConsumeAttemptBudget(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(forcedBudgetSrc), "forced-budget.toml")
	issue := createIssue(t, conn, "forced budget", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "work@0")

	// Attempt 1: the claim spends the count, then the relay establishes the
	// spawn is dead and force-reaps it.
	_, err = ClaimStep(conn, id, ClaimOptions{Owner: "doomed", NowMS: nowMS})
	testsupport.Must(t, err, "claim 1: %v", err)
	err = ForceReapStep(conn, id, "wave killed by an accidental interrupt", nowMS+1)
	testsupport.Must(t, err, "ForceReapStep: %v", err)

	// The exemption is a base nudge, nothing else: attempt stands at 1 (the
	// ledger's key), the base moved to 1, so the budget reads zero spent.
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after the forced reap: %v", err)
	if step.Attempt != 1 {
		t.Fatalf("attempt = %d after a forced reap, want 1 — the counter is the "+
			"usage ledger's key and is never reset or decremented", step.Attempt)
	}
	if step.AttemptBase != 1 {
		t.Fatalf("attempt_base = %d after a forced reap, want 1 — the dead "+
			"attempt is exempted from the budget by the nudge", step.AttemptBase)
	}

	// Attempt 2 is the step's FIRST legitimate try. If it fails, the step must
	// return to the pool, not park: 2 claims spent, but only 1 counts.
	claim2, err := ClaimStep(conn, id, ClaimOptions{Owner: "successor", NowMS: nowMS + 2})
	testsupport.Must(t, err, "claim 2: %v", err)
	err = e.FailStep(conn, id, claim2.Token, "genuine failure", "", nowMS+2)
	testsupport.Must(t, err, "fail 2: %v", err)
	if got := stepStatus(t, conn, "work@0"); got != db.StepPending {
		t.Fatalf("status = %q after the first legitimate failure, want %q — "+
			"the force-reaped attempt must not count against max_attempts",
			got, db.StepPending)
	}

	// The budget is exempted, not abolished: the SECOND legitimate failure is
	// 2 of 2 and exhausts as declared.
	claim3, err := ClaimStep(conn, id, ClaimOptions{Owner: "w3", NowMS: nowMS + 3})
	testsupport.Must(t, err, "claim 3: %v", err)
	err = e.FailStep(conn, id, claim3.Token, "failed again", "", nowMS+3)
	testsupport.Must(t, err, "fail 3: %v", err)
	if got := stepStatus(t, conn, "work@0"); got != db.StepWaitingHuman {
		t.Errorf("status = %q after two legitimate failures, want %q — the "+
			"exemption covers only the forced-reaped attempt", got, db.StepWaitingHuman)
	}
}

// TestExpiryReapStillConsumesAttemptBudget pins the other path: an ordinary
// TTL expiry reap — no `data.forced`, no relay assertion — charges the budget
// exactly as before. An expiry may still be an executor wedged under its own
// load, and that judgment is unchanged.
func TestExpiryReapStillConsumesAttemptBudget(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(forcedBudgetSrc), "forced-budget.toml")
	issue := createIssue(t, conn, "expiry budget", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "work@0")

	// Attempt 1's lease lapses in silence and `next`'s lazy reap frees it.
	claim1, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "claim 1: %v", err)
	late := claim1.LeaseExpiresMS + 1
	next, err := e.NextSteps(conn, run.ID, 0, late)
	testsupport.Must(t, err, "NextSteps reaping: %v", err)
	if len(next.Reaped) != 1 {
		t.Fatalf("reaped %v, want the expired claim reaped", next.Reaped)
	}

	// No exemption: the base stands at 0, the expired attempt counts.
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after the expiry reap: %v", err)
	if step.Attempt != 1 || step.AttemptBase != 0 {
		t.Fatalf("attempt/attempt_base = %d/%d after an expiry reap, want 1/0 — "+
			"an ordinary expiry still charges the budget",
			step.Attempt, step.AttemptBase)
	}

	// Attempt 2's failure is therefore 2 of 2: exhausted, parked.
	claim2, err := ClaimStep(conn, id, ClaimOptions{Owner: "w2", NowMS: late})
	testsupport.Must(t, err, "claim 2: %v", err)
	err = e.FailStep(conn, id, claim2.Token, "gave up", "", late)
	testsupport.Must(t, err, "fail 2: %v", err)
	if got := stepStatus(t, conn, "work@0"); got != db.StepWaitingHuman {
		t.Errorf("status = %q after an expiry reap plus one failure with "+
			"max_attempts = 2, want %q — expiry behavior must be unchanged",
			got, db.StepWaitingHuman)
	}
}

// TestForcedReapKeepsTheDeadAttemptsLedgerSlot is the usage-ledger half of
// RUN-30: the dead attempt's measured usage was back-filled AGAINST ITS OWN
// ATTEMPT NUMBER after the forced reap, and the successor's usage recorded
// beside it. The exemption must leave `attempt` alone so both slots exist.
func TestForcedReapKeepsTheDeadAttemptsLedgerSlot(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	id := stepIDByInstance(t, conn, "implement@0")

	// Attempt 1 dies; the relay force-reaps and back-fills what the dead
	// spawn's journal recorded — against attempt 1, the step's recorded
	// attempt, exactly as `dispatch backfill-usage` promises.
	_, err := ClaimStep(conn, id, ClaimOptions{Owner: "doomed", NowMS: nowMS})
	testsupport.Must(t, err, "claim 1: %v", err)
	err = ForceReapStep(conn, id, "spawn died; journal has usage", nowMS+1)
	testsupport.Must(t, err, "ForceReapStep: %v", err)
	_, err = e.BackfillUsage(conn, run.ID, []BackfillRow{
		{Step: id, Unit: "output_tokens", Quantity: 48344},
	}, "wave-journal:dead", "", nowMS+2)
	testsupport.Must(t, err, "back-filling the dead attempt: %v", err)

	// Attempt 2 runs for real, records, and back-fills its own usage.
	claim2, err := ClaimStep(conn, id, ClaimOptions{Owner: "successor", NowMS: nowMS + 3})
	testsupport.Must(t, err, "claim 2: %v", err)
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after re-claim: %v", err)
	if step.Attempt != 2 {
		t.Fatalf("attempt = %d after the successor's claim, want 2 — the "+
			"successor needs its own ledger attempt", step.Attempt)
	}
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token:    claim2.Token,
		Artifact: []byte("the change summary"),
		NowMS:    nowMS + 4,
	})
	testsupport.Must(t, err, "complete: %v", err)
	_, err = e.BackfillUsage(conn, run.ID, []BackfillRow{
		{Step: id, Unit: "output_tokens", Quantity: 512},
	}, "wave-journal:successor", "", nowMS+5)
	testsupport.Must(t, err, "back-filling the successor: %v", err)

	// Both executions hold distinct slots: the dead attempt's row under its
	// own number, the successor's beside it.
	rows, err := conn.Query(
		`SELECT attempt, quantity FROM usage_ledger
		  WHERE step_id = ? AND unit = 'output_tokens' ORDER BY attempt`, id)
	testsupport.Must(t, err, "reading the ledger: %v", err)
	defer rows.Close()
	var got [][2]float64
	for rows.Next() {
		var attempt, quantity float64
		testsupport.Must(t, rows.Scan(&attempt, &quantity), "scanning")
		got = append(got, [2]float64{attempt, quantity})
	}
	testsupport.Must(t, rows.Err(), "reading the ledger")
	want := [][2]float64{{1, 48344}, {2, 512}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ledger rows = %v, want %v — the exemption must never touch "+
			"`attempt`, the ledger's key half", got, want)
	}
}
