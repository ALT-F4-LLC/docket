package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-490: `attempt` is a claims-so-far spent-count and says nothing about how
// each claim ended. A consumer that read it as "attempts that failed" walked
// one escalation hop too many when a claim had merely been REAPED — a lease
// expiry spends an attempt with nothing failing — and no field on the row
// carried the distinction. `failed_attempts` and `reaped_claims` now do:
// `step fail` bumps the first, every reap path bumps the second, a recorded
// completion bumps neither, and `step resolve --as retry` touches neither —
// exactly as it leaves `attempt` alone.

// TestAttemptBreakdownAcrossReapFailAndRetry walks one step through every way
// a claim can end short of recording, asserting the breakdown at each stop:
// fresh (0/0/0) -> claim expires and is reaped by `next` (1 reaped, 0 failed)
// -> re-claimed and failed by its holder (+1 failed) -> re-claimed again ->
// expired again and lazily reaped by `claim` itself (+1 reaped) -> parked by a
// gap-only completion and retried by an operator (counters untouched).
func TestAttemptBreakdownAcrossReapFailAndRetry(t *testing.T) {
	const src = `
[pipeline]
name = "breakdown"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "w"
emits = "out"
on_fail = "waiting-human"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "breakdown.toml")
	issue := createIssue(t, conn, "breakdown", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "work@0")

	assertBreakdown := func(when string, attempt, failed, reaped int) {
		t.Helper()
		step, err := db.GetStep(conn, id)
		testsupport.Must(t, err, "GetStep %s: %v", when, err)
		if step.Attempt != attempt || step.FailedAttempts != failed ||
			step.ReapedClaims != reaped {
			t.Fatalf("%s: attempt/failed/reaped = %d/%d/%d, want %d/%d/%d",
				when, step.Attempt, step.FailedAttempts, step.ReapedClaims,
				attempt, failed, reaped)
		}
	}

	// Fresh: never claimed, nothing to break down — and the wire says so by
	// OMISSION (`omitempty`), so a pre-v23 consumer's rows are byte-identical.
	assertBreakdown("fresh", 0, 0, 0)
	next, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps fresh: %v", err)
	if len(next.Steps) != 1 || next.Steps[0].Attempt != 0 ||
		next.Steps[0].FailedAttempts != 0 || next.Steps[0].ReapedClaims != 0 {
		t.Fatalf("fresh offer rows = %+v, want one row at 0/0/0", next.Steps)
	}
	raw, err := json.Marshal(next.Steps[0])
	testsupport.Must(t, err, "marshal fresh row: %v", err)
	if strings.Contains(string(raw), "failed_attempts") ||
		strings.Contains(string(raw), "reaped_claims") {
		t.Errorf("fresh row JSON %s carries a zero breakdown; omitempty must "+
			"keep an outcome-less row serializing exactly as before", raw)
	}

	// Claim 1 dies in silence: the lease lapses and `next`'s lazy reap frees
	// the step. One attempt spent, ZERO failed — the DKT-490 distinction.
	claim1, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "claim 1: %v", err)
	assertBreakdown("after claim 1", 1, 0, 0)
	late := claim1.LeaseExpiresMS + 1
	next, err = e.NextSteps(conn, run.ID, 0, late)
	testsupport.Must(t, err, "NextSteps reaping: %v", err)
	if len(next.Reaped) != 1 {
		t.Fatalf("reaped %v, want the expired claim reaped", next.Reaped)
	}
	assertBreakdown("after the expiry reap", 1, 0, 1)
	// The SAME call's offer already carries the reap it performed — snapshot
	// reflection, the discipline reapOneTx applies to status and lease.
	if len(next.Steps) != 1 || next.Steps[0].ReapedClaims != 1 ||
		next.Steps[0].FailedAttempts != 0 {
		t.Fatalf("post-reap offer rows = %+v, want the row carrying 1 reaped, "+
			"0 failed", next.Steps)
	}

	// Claim 2 fails FOR REAL: the holder measures its own work and records
	// the failure. Below the (undeclared, so unbounded) budget the step
	// returns to the pool — mechanically the same row reset as a reap, and
	// the breakdown is what keeps the two endings distinguishable.
	claim2, err := ClaimStep(conn, id, ClaimOptions{Owner: "w2", NowMS: late})
	testsupport.Must(t, err, "claim 2: %v", err)
	err = e.FailStep(conn, id, claim2.Token, "went wrong", "", late)
	testsupport.Must(t, err, "fail: %v", err)
	if got := stepStatus(t, conn, "work@0"); got != db.StepPending {
		t.Fatalf("status = %q after a below-budget failure, want %q", got, db.StepPending)
	}
	assertBreakdown("after the failure", 2, 1, 1)

	// Claim 3 expires and the NEXT CLAIM reaps it lazily — the other reap
	// path (§6.3 confines reaping to next and claim), same counter.
	claim3, err := ClaimStep(conn, id, ClaimOptions{Owner: "w3", NowMS: late + 1})
	testsupport.Must(t, err, "claim 3: %v", err)
	assertBreakdown("after claim 3", 3, 1, 1)
	late2 := claim3.LeaseExpiresMS + 1
	claim4, err := ClaimStep(conn, id, ClaimOptions{Owner: "w4", NowMS: late2})
	testsupport.Must(t, err, "claim 4 over the expired lease: %v", err)
	assertBreakdown("after the lazy claim reap", 4, 1, 2)

	// A gap-only completion RECORDS and parks (DKT-25): the claim ended in an
	// artifact, so it counts as neither a failure nor a reap. The operator's
	// `resolve --as retry` then touches nothing here either — the budget base
	// moves, `attempt` and the breakdown stand.
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token:    claim4.Token,
		Artifact: []byte("  \n"),
		Gaps:     [][]byte{[]byte("# Cannot be done here\n\nResidue.")},
		NowMS:    late2 + 1,
	})
	testsupport.Must(t, err, "gap-only complete: %v", err)
	if got := stepStatus(t, conn, "work@0"); got != db.StepWaitingHuman {
		t.Fatalf("status = %q after a gap-only completion, want %q",
			got, db.StepWaitingHuman)
	}
	assertBreakdown("after the recorded park", 4, 1, 2)
	err = e.ResolveStep(conn, id, ResolveRetry, "re-run it", late2+2)
	testsupport.Must(t, err, "resolve --as retry: %v", err)
	assertBreakdown("after resolve --as retry", 4, 1, 2)

	// Both read surfaces carry the breakdown beside the count it explains,
	// under the documented wire names.
	view, err := LoadStepView(conn, id, late2+3)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.Row.Attempt != 4 || view.Row.FailedAttempts != 1 || view.Row.ReapedClaims != 2 {
		t.Errorf("step show renders attempt/failed/reaped = %d/%d/%d, want 4/1/2",
			view.Row.Attempt, view.Row.FailedAttempts, view.Row.ReapedClaims)
	}
	raw, err = json.Marshal(view.Row)
	testsupport.Must(t, err, "marshal: %v", err)
	if !strings.Contains(string(raw), `"failed_attempts":1`) ||
		!strings.Contains(string(raw), `"reaped_claims":2`) {
		t.Errorf("step show JSON %s does not carry the breakdown fields", raw)
	}
	rows, err := RunStepList(conn, run.ID, late2+3)
	testsupport.Must(t, err, "RunStepList: %v", err)
	if row := stepListRowOf(t, rows, "work@0"); row.FailedAttempts != 1 ||
		row.ReapedClaims != 2 || row.Attempt != 4 {
		t.Errorf("step list renders attempt/failed/reaped = %d/%d/%d, want 4/1/2",
			row.Attempt, row.FailedAttempts, row.ReapedClaims)
	}
}

// TestExhaustedFailuresCountIntoTheBreakdown pins the other `step fail`
// branch: a failure that EXHAUSTS the declared budget routes instead of
// re-pooling, and it must count exactly like the ones that re-pooled — the
// parked step's row is precisely where an operator asks "how many attempts
// failed", which is the DKT-490 human-surface half.
func TestExhaustedFailuresCountIntoTheBreakdown(t *testing.T) {
	const src = `
[pipeline]
name = "bounded-breakdown"
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
	registerSource(t, conn, []byte(src), "bounded-breakdown.toml")
	issue := createIssue(t, conn, "bounded", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "flaky@0")

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

	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Attempt != 2 || step.FailedAttempts != 2 || step.ReapedClaims != 0 {
		t.Errorf("attempt/failed/reaped = %d/%d/%d after two failures (the "+
			"second exhausting), want 2/2/0 — the routing branch must count "+
			"its failure like the re-pooling branch does",
			step.Attempt, step.FailedAttempts, step.ReapedClaims)
	}
}

// TestForcedReapCountsAsReapedNotFailed pins `step reap` (DKT-83) to the reap
// side of the ledger: an operator asserting a holder dead records a silence,
// not a measurement, and an escalation policy must not climb a tier over it.
func TestForcedReapCountsAsReapedNotFailed(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	id := stepIDByInstance(t, conn, "implement@0")

	_, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = ForceReapStep(conn, id, "relay watched the process die", nowMS+1)
	testsupport.Must(t, err, "forced reap: %v", err)

	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Attempt != 1 || step.FailedAttempts != 0 || step.ReapedClaims != 1 {
		t.Errorf("attempt/failed/reaped = %d/%d/%d after a forced reap, want "+
			"1/0/1", step.Attempt, step.FailedAttempts, step.ReapedClaims)
	}
}
