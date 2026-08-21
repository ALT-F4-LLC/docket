package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-86 / DKT-90: `step resolve --as retry` used to zero `attempt`, so a step
// that recorded, parked, and genuinely re-executed re-claimed under an attempt
// number the usage ledger had already recorded — and the second execution's
// real, separately-measured usage was permanently unrecordable through
// `dispatch backfill-usage` (`(step, attempt, unit)` refused as already
// recorded). The counter is now monotonic and retry moves `attempt_base`
// instead, so the sanctioned back-fill path keeps its own help text's promise:
// "a retried step's second attempt records beside its first".

// TestRetriedStepsSecondExecutionBackfillsItsOwnUsage is RUN-13/RUN-14's exact
// sequence: claim → record (parks) → back-fill → resolve --as retry → re-claim
// → record → back-fill the SECOND execution. The final back-fill must land,
// and the ledger must hold both executions' rows under distinct attempts.
func TestRetriedStepsSecondExecutionBackfillsItsOwnUsage(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	id := stepIDByInstance(t, conn, "implement@0")

	// First execution: claim (attempt 1), then a gap-only completion, which
	// parks the step `waiting-human` (DKT-25) — the record-then-park shape.
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "first claim: %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("  \n"),
		Gaps:     [][]byte{[]byte("# Cannot be done here\n\nResidue.")},
		NowMS:    nowMS,
	})
	testsupport.Must(t, err, "first complete: %v", err)
	if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Fatalf("status = %q after a gap-only completion, want %q", got, db.StepWaitingHuman)
	}

	// The relay back-fills the first execution's measured usage.
	_, err = e.BackfillUsage(conn, run.ID, []BackfillRow{
		{Step: id, Unit: "input_tokens", Quantity: 38},
	}, "wave-journal:first", "", nowMS)
	testsupport.Must(t, err, "first back-fill: %v", err)

	// The operator retries the park. The budget resets; the counter must not.
	err = e.ResolveStep(conn, id, ResolveRetry, "re-run it", nowMS)
	testsupport.Must(t, err, "resolve --as retry: %v", err)
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after retry: %v", err)
	if step.Attempt != 1 {
		t.Fatalf("attempt = %d after retry, want 1 — zeroing it is what made the "+
			"second execution's ledger slot collide with the first's", step.Attempt)
	}

	// Second execution: a fresh claim mints attempt 2, and this time the work
	// records real content.
	claim2, err := ClaimStep(conn, id, ClaimOptions{Owner: "w2", NowMS: nowMS + 1})
	testsupport.Must(t, err, "second claim: %v", err)
	step, err = db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after re-claim: %v", err)
	if step.Attempt != 2 {
		t.Fatalf("attempt = %d after the re-claim, want 2 — the re-execution "+
			"needs its own ledger attempt", step.Attempt)
	}
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token:    claim2.Token,
		Artifact: []byte("the change summary"),
		NowMS:    nowMS + 2,
	})
	testsupport.Must(t, err, "second complete: %v", err)

	// DKT-90 AC-1: the second execution's genuinely distinct usage back-fills
	// without an "already has usage" refusal.
	_, err = e.BackfillUsage(conn, run.ID, []BackfillRow{
		{Step: id, Unit: "input_tokens", Quantity: 44},
	}, "wave-journal:second", "", nowMS+3)
	testsupport.Must(t, err, "second back-fill refused: %v", err)

	// Both executions' rows are in the ledger, under distinct attempts.
	rows, err := conn.Query(
		`SELECT attempt, quantity FROM usage_ledger
		  WHERE step_id = ? AND unit = 'input_tokens' ORDER BY attempt`, id)
	testsupport.Must(t, err, "reading the ledger: %v", err)
	defer rows.Close()
	var got [][2]float64
	for rows.Next() {
		var attempt, quantity float64
		testsupport.Must(t, rows.Scan(&attempt, &quantity), "scanning")
		got = append(got, [2]float64{attempt, quantity})
	}
	testsupport.Must(t, rows.Err(), "reading the ledger")
	want := [][2]float64{{1, 38}, {2, 44}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ledger rows = %v, want %v — one row per real execution, "+
			"neither overwriting the other", got, want)
	}

	// DKT-90 AC-2: the slot an execution already filled stays refused — the
	// fix opens a slot for a NEW execution, never a rewrite of an old one.
	_, err = e.BackfillUsage(conn, run.ID, []BackfillRow{
		{Step: id, Unit: "input_tokens", Quantity: 99},
	}, "wave-journal:rewrite", "", nowMS+4)
	if err == nil {
		t.Fatal("a repeat back-fill of a recorded (step, attempt, unit) was accepted")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("repeat back-fill code = %v, want CONFLICT", code)
	}
}

// TestRetryRefreshesTheMaxAttemptsBudget pins the budget half: after a retry,
// a step's prior claims must not count against `max_attempts` — the budget
// counts from the base, so the retried step gets the full declared allowance.
func TestRetryRefreshesTheMaxAttemptsBudget(t *testing.T) {
	const src = `
[pipeline]
name = "budgeted"
version = 1

[match]
kind = ["task"]

[[step]]
name = "flaky"
after = []
executor = "w"
emits = "out"
max_attempts = 2
on_fail = "waiting-human"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "budgeted.toml")
	issue := createIssue(t, conn, "budgeted", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "flaky@0")

	// Exhaust the declared budget: two claims, two failures, parked.
	for i := range 2 {
		claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS + int64(i)})
		testsupport.Must(t, err, "claim %d: %v", i+1, err)
		err = e.FailStep(conn, id, claim.Token, "gave up", "", nowMS+int64(i))
		testsupport.Must(t, err, "fail %d: %v", i+1, err)
	}
	if got := stepStatus(t, conn, "flaky@0"); got != db.StepWaitingHuman {
		t.Fatalf("status = %q after exhausting max_attempts = 2, want %q",
			got, db.StepWaitingHuman)
	}

	err = e.ResolveStep(conn, id, ResolveRetry, "fresh budget", nowMS+10)
	testsupport.Must(t, err, "resolve --as retry: %v", err)

	// The next claim is attempt 3, and its failure must NOT exhaust: the
	// budget counts from the base (2), so this is failure 1 of 2.
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS + 11})
	testsupport.Must(t, err, "claim after retry: %v", err)
	err = e.FailStep(conn, id, claim.Token, "flaky again", "", nowMS+11)
	testsupport.Must(t, err, "fail after retry: %v", err)
	if got := stepStatus(t, conn, "flaky@0"); got != db.StepPending {
		t.Errorf("status = %q after the first post-retry failure, want %q — "+
			"§2's \"retry resets attempts\" is exactly this budget refresh",
			got, db.StepPending)
	}
}
