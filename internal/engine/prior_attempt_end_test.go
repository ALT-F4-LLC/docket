package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1279: a reap is a liveness event, not a quality verdict. RUN-80
// DISPATCH-400 was killed mid-wave by a session usage limit; the engine
// reaped ten leases and the steps re-dispatched at `attempt` incremented, and
// wave.js/policy's on_failure escalation read that as "failed once" and
// routed all ten a tier up. FailedAttempts/ReapedClaims (DKT-490) answer "how
// many of each has this step EVER had" and go ambiguous the moment a step's
// history mixes both; `prior_attempt_end` answers the question an escalation
// policy actually asks — how did the LAST one end — directly on the `next`
// row.

// TestNextRowNamesAReapedPriorAttempt is DKT-1279's repro, verbatim: claim a
// step, let its lease lapse, let `next` reap it lazily, and assert the
// re-offered row says the prior attempt was reaped rather than failed.
func TestNextRowNamesAReapedPriorAttempt(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	id := stepIDByInstance(t, conn, "implement@0")
	e := testEngine()

	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	// Nothing measured the failure — the lease simply lapses, and `next`'s
	// lazy reap frees the step.
	late := claim.LeaseExpiresMS + 1
	next, err := e.NextSteps(conn, run.ID, 0, late)
	testsupport.Must(t, err, "NextSteps reaping: %v", err)
	if len(next.Reaped) != 1 {
		t.Fatalf("reaped %v, want the expired claim reaped", next.Reaped)
	}

	row := stepRowOf(t, next.Steps, "implement@0")
	if row.PriorAttemptEnd != db.ClaimEndReaped {
		t.Fatalf("next row's prior_attempt_end = %q, want %q — a re-offer "+
			"following a reap must say so, not leave a router to infer it "+
			"from attempt alone", row.PriorAttemptEnd, db.ClaimEndReaped)
	}

	raw, err := json.Marshal(row)
	testsupport.Must(t, err, "marshal reaped row: %v", err)
	if !strings.Contains(string(raw), `"prior_attempt_end":"reaped"`) {
		t.Errorf("next row JSON %s does not carry prior_attempt_end", raw)
	}

	// The stored row agrees with the offer that rendered it.
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.LastClaimEnd != db.ClaimEndReaped {
		t.Errorf("stored last_claim_end = %q, want %q", step.LastClaimEnd, db.ClaimEndReaped)
	}
}

// TestNextRowNamesAFailedPriorAttempt is the reaped test's other half: a
// claim that ended in an explicit `step fail` must say "failed", never
// "reaped" — the whole point is that the two are distinguishable.
func TestNextRowNamesAFailedPriorAttempt(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	id := stepIDByInstance(t, conn, "implement@0")
	e := testEngine()

	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, id, claim.Token, "gave up", "", nowMS+1)
	testsupport.Must(t, err, "fail: %v", err)

	next, err := e.NextSteps(conn, run.ID, 0, nowMS+2)
	testsupport.Must(t, err, "NextSteps: %v", err)
	row := stepRowOf(t, next.Steps, "implement@0")
	if row.PriorAttemptEnd != db.ClaimEndFailed {
		t.Errorf("next row's prior_attempt_end = %q, want %q",
			row.PriorAttemptEnd, db.ClaimEndFailed)
	}
}

// TestFreshStepOmitsPriorAttemptEnd pins `omitempty`: a step never claimed
// has no prior attempt to name, and the field must be ABSENT from the wire —
// not present and empty — so a pre-DKT-1279 consumer's rows stay
// byte-identical.
func TestFreshStepOmitsPriorAttemptEnd(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	next, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	row := stepRowOf(t, next.Steps, "implement@0")
	if row.PriorAttemptEnd != "" {
		t.Fatalf("fresh row's prior_attempt_end = %q, want empty", row.PriorAttemptEnd)
	}
	raw, err := json.Marshal(row)
	testsupport.Must(t, err, "marshal fresh row: %v", err)
	if strings.Contains(string(raw), "prior_attempt_end") {
		t.Errorf("fresh row JSON %s carries prior_attempt_end; omitempty must "+
			"keep an attempt-less row serializing exactly as before", raw)
	}
}

// TestForcedReapNamesThePriorAttemptReaped pins `step reap` (DKT-83) to the
// same field: an operator asserting a holder dead records a silence, and the
// re-offered row must say "reaped", exactly as a lazy expiry reap does.
func TestForcedReapNamesThePriorAttemptReaped(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	id := stepIDByInstance(t, conn, "implement@0")
	e := testEngine()

	_, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = ForceReapStep(conn, id, "relay watched the process die", nowMS+1)
	testsupport.Must(t, err, "forced reap: %v", err)

	next, err := e.NextSteps(conn, run.ID, 0, nowMS+2)
	testsupport.Must(t, err, "NextSteps: %v", err)
	row := stepRowOf(t, next.Steps, "implement@0")
	if row.PriorAttemptEnd != db.ClaimEndReaped {
		t.Errorf("next row's prior_attempt_end = %q after a forced reap, want %q",
			row.PriorAttemptEnd, db.ClaimEndReaped)
	}
}

// stepRowOf finds the row for one instance in a `next` offer, failing the
// test if it is absent.
func stepRowOf(t *testing.T, rows []model.StepRow, instance string) model.StepRow {
	t.Helper()
	for _, row := range rows {
		if row.Instance == instance {
			return row
		}
	}
	t.Fatalf("no row for instance %q among %d rows", instance, len(rows))
	return model.StepRow{}
}
