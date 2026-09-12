package engine

import (
	"math"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-867 — `expected_cost` was variant-blind: the definition declares one
// number per step template, so a claim the dispatcher escalated to a far
// pricier executor variant accrued exactly what the cheap variant did
// (RUN-60 STEP-2752 vs STEP-2769: a 22.51M-token escalated fix attempt and a
// 1.83M ordinary one priced identically at the cap), and every measured run
// showed budget.spend == budget.floor exactly.
//
// The remedy under test: the DISPATCHER — the party that resolves a step to a
// variant, at claim time — declares the scaling (`step claim
// --cost-multiplier`), and the claim checks the SCALED cost at the cap and
// records it in its own `step-claimed` event, which RunFloorTx sums in place
// of the step row's declaration. Core learns no variant vocabulary
// (genericity.md): it learns a number, from the one party positioned to know
// it. The ledger-reading remedy DKT-867 also named is NOT taken here — its
// safe form already exists as DKT-238's measured usage cap, and folding token
// counts into the declared cap's `max()` is the incomparability trap
// budgetSnapshot.usageCap documents.
//
// The fixture's costs are read, never restated: `implement` is 1.50 in the
// activatedRun workflow, and every expectation below is derived from
// expectedCostOf so a fixture change cannot silently hollow these tests.

// TestEscalatedClaimAccruesScaledCost is the core defect, fixed: the SAME step
// kind claimed as an escalated variant contributes materially more to the cap
// sum than a claim that stayed on the cheap variant.
func TestEscalatedClaimAccruesScaledCost(t *testing.T) {
	// The cheap run: no multiplier, the declared cost verbatim.
	connCheap := mustDB(t)
	runCheap, _ := budgetRun(t, connCheap, 0)
	cost := expectedCostOf(t, connCheap, "implement@0")
	claimInstance(t, connCheap, "implement@0", nowMS)
	cheap := runFloor(t, connCheap, runCheap)

	// The escalated run: the dispatcher resolved the same step kind to a
	// variant it prices at 4x.
	connEsc := mustDB(t)
	runEsc, _ := budgetRun(t, connEsc, 0)
	_, err := ClaimStep(connEsc, stepIDByInstance(t, connEsc, "implement@0"),
		ClaimOptions{Owner: "worker", CostMultiplier: 4, NowMS: nowMS})
	testsupport.Must(t, err, "escalated claim: %v", err)
	escalated := runFloor(t, connEsc, runEsc)

	if cheap != cost {
		t.Fatalf("cheap-variant floor = %g, want the declared %g", cheap, cost)
	}
	if escalated != 4*cost {
		t.Errorf("escalated floor = %g, want %g — the scaled cost, not the "+
			"declared one", escalated, 4*cost)
	}
	if escalated <= cheap {
		t.Errorf("escalated floor %g is not above the cheap floor %g — the "+
			"DKT-867 defect (variants pricing identically) is not fixed",
			escalated, cheap)
	}
}

// TestUnescalatedClaimIsByteForByteUnchanged is the regression half: a run
// that never escalates — no `--cost-multiplier` anywhere — accrues exactly the
// declared costs, writes the claim event with an EMPTY data payload (no new
// key for consumers of the feed to trip on), and hits the cap at exactly the
// boundary it always did.
func TestUnescalatedClaimIsByteForByteUnchanged(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	cost := expectedCostOf(t, conn, "implement@0")
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cost, runID)

	// Exactly at the cap: admitted, and the floor is the declared cost.
	claimInstance(t, conn, "implement@0", nowMS)
	if got := runFloor(t, conn, runID); got != cost {
		t.Fatalf("floor = %g after an unescalated claim, want the declared %g",
			got, cost)
	}

	// The claim event carries no cost override — only the instance key every
	// step-claimed event has always carried — so RunFloorTx's fallback to the
	// step row is what priced it, and a feed consumer sees the exact
	// pre-DKT-867 bytes.
	var data string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, EventStepClaimed).Scan(&data)
	testsupport.Must(t, err, "reading the claim event: %v", err)
	if strings.Contains(data, "expected_cost") {
		t.Errorf("unescalated claim event data = %q, want no expected_cost "+
			"override — an unscaled claim must serialize exactly as before", data)
	}
	if data != `{"instance":"implement@0"}` {
		t.Errorf("unescalated claim event data = %q, want the pre-DKT-867 "+
			`{"instance":"implement@0"}`, data)
	}

	// The NEXT costed claim crosses and is refused — the pre-DKT-867 boundary,
	// unchanged.
	execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE instance = 'implement@0'`)
	_, err = ClaimStep(conn, stepIDByInstance(t, conn, "implement@0"),
		ClaimOptions{Owner: "w2", NowMS: nowMS + 1})
	if err == nil {
		t.Fatal("the claim that crosses the cap was admitted")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("the refusal is %v, want %v", code, CodeConflict)
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("the refusal does not name the budget: %v", err)
	}
}

// TestEscalatedClaimIsRefusedAtTheCap is consequence (1) of DKT-867, closed: a
// cap with headroom for the DECLARED cost refuses a claim whose dispatcher
// declared an escalation the cap cannot absorb — the hop is visible at the
// cap, before the accrual commits, and the run pauses exactly as any other
// breach does.
func TestEscalatedClaimIsRefusedAtTheCap(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	cost := expectedCostOf(t, conn, "implement@0")
	// Room for the declared cost with headroom to spare, but not for 4x it.
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, 2*cost, runID)

	_, err := ClaimStep(conn, stepIDByInstance(t, conn, "implement@0"),
		ClaimOptions{Owner: "worker", CostMultiplier: 4, NowMS: nowMS})
	if err == nil {
		t.Fatal("the escalated claim that crosses the cap was admitted — " +
			"the escalation hop is still invisible to the budget")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("the refusal is %v, want %v", code, CodeConflict)
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("the refusal does not name the budget: %v", err)
	}

	// The refusal and the pause are one fact (B20), same as every breach.
	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Errorf("run is %s after the escalated breach, want %s",
			run.Status, model.RunWaitingHuman)
	}

	// Nothing accrued: the refused claim wrote no claim event, so the floor
	// still reads zero — a refusal must not cost anything.
	if got := runFloor(t, conn, runID); got != 0 {
		t.Errorf("floor = %g after the refused escalated claim, want 0", got)
	}
}

// TestEscalatedRetryReAccruesItsOwnCost is B9's re-accrual made variant-aware
// — the DOT-846 shape: a cheap first attempt, then an escalated retry of the
// SAME step. Each claim accrues what ITS dispatcher declared, so the retry's
// hop lands in the floor at its real price.
func TestEscalatedRetryReAccruesItsOwnCost(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	cost := expectedCostOf(t, conn, "implement@0")

	claimInstance(t, conn, "implement@0", nowMS)
	if got := runFloor(t, conn, runID); got != cost {
		t.Fatalf("floor after the cheap first attempt = %g, want %g", got, cost)
	}

	// The lease lapses; the dispatcher escalates the retry to a 4x variant.
	execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE instance = 'implement@0'`)
	_, err := ClaimStep(conn, stepIDByInstance(t, conn, "implement@0"),
		ClaimOptions{Owner: "w2", CostMultiplier: 4, NowMS: nowMS + 1})
	testsupport.Must(t, err, "escalated retry: %v", err)

	if got, want := runFloor(t, conn, runID), cost+4*cost; got != want {
		t.Errorf("floor after the escalated retry = %g, want %g — each claim "+
			"accrues what its own dispatcher declared", got, want)
	}
}

// TestCheaperVariantAdmitsWhereDeclaredWouldRefuse is the multiplier's other
// direction: a cap the DECLARED cost would cross admits a claim the
// dispatcher routed to a cheaper variant, and the floor records the honest
// smaller number. Budget is §6.3's last clause, so nothing else can hide
// behind the fall-through.
func TestCheaperVariantAdmitsWhereDeclaredWouldRefuse(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	cost := expectedCostOf(t, conn, "implement@0")
	// Below the declared cost: an unscaled claim would breach here.
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cost/2, runID)

	_, err := ClaimStep(conn, stepIDByInstance(t, conn, "implement@0"),
		ClaimOptions{Owner: "worker", CostMultiplier: 0.25, NowMS: nowMS})
	testsupport.Must(t, err, "cheap-variant claim under a tight cap: %v", err)

	if got, want := runFloor(t, conn, runID), cost*0.25; got != want {
		t.Errorf("floor = %g, want %g — the accrual is the scaled cost", got, want)
	}

	// The run is still active: no breach was recorded on the admitted claim.
	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunActive {
		t.Errorf("run is %s after an admitted cheap-variant claim, want %s",
			run.Status, model.RunActive)
	}
}

// TestCostMultiplierValidation: a multiplier that is not a positive finite
// number is refused BEFORE the transaction opens — nothing mutates, no
// attempt is consumed, and the floor is untouched.
func TestCostMultiplierValidation(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	stepID := stepIDByInstance(t, conn, "implement@0")

	for _, bad := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := ClaimStep(conn, stepID,
			ClaimOptions{Owner: "worker", CostMultiplier: bad, NowMS: nowMS})
		if err == nil {
			t.Fatalf("cost multiplier %g was accepted", bad)
		}
		if code, _ := CodeOf(err); code != CodeValidation {
			t.Errorf("multiplier %g refused with %v, want %v", bad, code, CodeValidation)
		}
	}

	// Nothing mutated across all the refusals: the step is still unclaimed at
	// attempt zero and the floor never moved.
	var status string
	var attempt int
	err := conn.QueryRow(
		`SELECT status, attempt FROM steps WHERE id = ?`, stepID).Scan(&status, &attempt)
	testsupport.Must(t, err, "reading the step after refusals: %v", err)
	if status != string(db.StepPending) || attempt != 0 {
		t.Errorf("step is %s at attempt %d after refused claims, want %s at 0",
			status, attempt, db.StepPending)
	}
	if got := runFloor(t, conn, runID); got != 0 {
		t.Errorf("floor = %g after refused claims, want 0", got)
	}
}
