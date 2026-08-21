package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// armUsageCap sets the measured dimension on a run: a cap on the row and a
// unit in config. BOTH are needed — a cap with no unit counts nothing.
func armUsageCap(t *testing.T, conn *sql.DB, runID int, cap float64, unit string) {
	t.Helper()
	execSQL(t, conn, `UPDATE runs SET usage_budget = ? WHERE id = ?`, cap, runID)
	testsupport.Must(t, db.SetConfig(conn, 0, db.KeyUsageBudgetUnit, unit),
		"setting %s", db.KeyUsageBudgetUnit)
}

// TestMeasuredUsageCapStopsARun is DKT-238.
//
// Budget enforcement was disconnected from real spend. A raise tribunal
// deliberated 140 -> 280 declared "units" while measured usage across the same
// run was 4,838,739 output / 689,187,075 cache-read tokens; its own security
// seat wrote that "the budget cap tracks declared step costs, not actual token
// spend ... so 280 is a floor-only proxy". The engine already held the
// numbers, back-filled per step. Nothing could enforce against them.
func TestMeasuredUsageCapStopsARun(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	implID := stepIDByInstance(t, conn, "implement@0")

	// The declared dimension is UNLIMITED throughout: this test is about the
	// measured one carrying the stop on its own.
	armUsageCap(t, conn, runID, 1000, "output_tokens")

	// Under the cap: work proceeds.
	ready, err := e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	if len(ready.Steps) == 0 {
		t.Fatal("a run under its measured cap was offered nothing")
	}
	if ready.BudgetHeldReason != "" {
		t.Errorf("a run under its measured cap reports a hold: %s",
			ready.BudgetHeldReason)
	}

	// The relay measures what the step actually consumed and back-fills it —
	// past the cap.
	completeWithoutUsage(t, conn, e, implID)
	_, err = e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "output_tokens", Quantity: 4_838_739},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill: %v", err)

	// Now the measured cap stops the run, and says which cap did it.
	ready, err = e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps after the back-fill: %v", err)
	if len(ready.Steps) != 0 {
		t.Errorf("a run past its measured cap was offered %d step(s); the "+
			"whole point of the dimension is that recorded spend can stop it",
			len(ready.Steps))
	}
	if !strings.Contains(ready.BudgetHeldReason, "measured") ||
		!strings.Contains(ready.BudgetHeldReason, "output_tokens") {
		t.Errorf("the hold does not name the measured cap or its unit: %q",
			ready.BudgetHeldReason)
	}
	// Naming WHICH cap matters: raising a declared-cost cap does nothing for a
	// run stopped on tokens.
	if strings.Contains(ready.BudgetHeldReason, "budget headroom") {
		t.Errorf("a measured-cap stop is reported as a declared-cost one: %q",
			ready.BudgetHeldReason)
	}
}

// TestMeasuredCapIsDormantUntilBothHalvesAreSet keeps the dimension off by
// default. A cap with no unit has nothing to count, and a unit with no cap has
// nothing to exceed; either alone must enforce nothing.
func TestMeasuredCapIsDormantUntilBothHalvesAreSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  float64
		unit string
	}{
		{"neither", 0, ""},
		{"a cap with no unit", 10, ""},
		{"a unit with no cap", 0, "output_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			e := testEngine()
			runID := dispatchRun(t, conn)
			implID := stepIDByInstance(t, conn, "implement@0")
			armUsageCap(t, conn, runID, tc.cap, tc.unit)

			// Spend far past any plausible cap.
			completeWithoutUsage(t, conn, e, implID)
			_, err := e.BackfillUsage(conn, runID, []BackfillRow{
				{Step: implID, Unit: "output_tokens", Quantity: 999_999},
			}, "", "", nowMS)
			testsupport.Must(t, err, "backfill: %v", err)

			ready, err := e.NextSteps(conn, runID, 0, nowMS)
			testsupport.Must(t, err, "NextSteps: %v", err)
			if ready.BudgetHeldReason != "" {
				t.Errorf("a dormant measured cap withheld work: %s",
					ready.BudgetHeldReason)
			}
		})
	}
}

// TestMeasuredCapCountsOnlyItsOwnUnit pins the same rule the declared cap's
// `reported` follows: core never sums across units, because that would be core
// asserting two opaque units are commensurable.
func TestMeasuredCapCountsOnlyItsOwnUnit(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	implID := stepIDByInstance(t, conn, "implement@0")
	armUsageCap(t, conn, runID, 100, "output_tokens")

	completeWithoutUsage(t, conn, e, implID)
	_, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "cache_read", Quantity: 689_187_075},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill: %v", err)

	ready, err := e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	if ready.BudgetHeldReason != "" {
		t.Errorf("a cache_read total stopped an output_tokens cap; summing "+
			"across units is core asserting they add up: %s",
			ready.BudgetHeldReason)
	}
}

// TestBothDimensionsAreIndependent is the property the two caps exist for: a
// run under one and past the other stops, and it stops for the right reason.
func TestBothDimensionsAreIndependent(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	implID := stepIDByInstance(t, conn, "implement@0")

	// A generous DECLARED cap, and a measured cap the run is about to pass.
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, 1000.0, runID)
	armUsageCap(t, conn, runID, 10, "output_tokens")

	completeWithoutUsage(t, conn, e, implID)
	_, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "output_tokens", Quantity: 500},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill: %v", err)

	budget, err := GetRunBudget(conn, runID)
	testsupport.Must(t, err, "GetRunBudget: %v", err)

	// The declared numbers are untouched by the measured ones — folding them
	// together would make the token count swamp the declared discipline.
	if budget.Spend > budget.Budget {
		t.Errorf("declared spend %g exceeds its own cap %g; the measured "+
			"total leaked into it", budget.Spend, budget.Budget)
	}
	if budget.UsageSpend != 500 {
		t.Errorf("usage_spend = %g, want 500", budget.UsageSpend)
	}
	if budget.UsageUnit != "output_tokens" {
		t.Errorf("usage_unit = %q, want output_tokens", budget.UsageUnit)
	}

	ready, err := e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	if len(ready.Steps) != 0 {
		t.Errorf("a run past its measured cap but under its declared one was "+
			"offered %d step(s); the two caps are independent and either "+
			"stops the run", len(ready.Steps))
	}
}

// TestUsageBudgetBreachNamesTheUnit keeps the breach message actionable: which
// cap stopped the run, and over what.
func TestUsageBudgetBreachNamesTheUnit(t *testing.T) {
	got := UsageBudgetBreachReason(4_838_739, 1_000_000, "output_tokens", "fix@1")
	for _, want := range []string{"usage budget", "output_tokens", "fix@1"} {
		if !strings.Contains(got, want) {
			t.Errorf("the breach reason does not carry %q: %s", want, got)
		}
	}
	// The DECLARED cap's message names no unit, deliberately — what its number
	// counts is the workflow author's business. The two must stay tellable
	// apart.
	if strings.Contains(BudgetBreachReason(280, 140, "fix@1"), "usage budget") {
		t.Error("the declared breach reason reads as a measured one")
	}
}

// TestMeasuredCapBreachPausesTheRun pins the claim-side half. R7 makes a step
// not APPEAR; this makes a claim REFUSE, and the two must agree — a relay
// holding a stale `next` row must not get past a cap the offer already
// enforced.
func TestMeasuredCapBreachPausesTheRun(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	implID := stepIDByInstance(t, conn, "implement@0")
	armUsageCap(t, conn, runID, 10, "output_tokens")

	completeWithoutUsage(t, conn, e, implID)
	_, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "output_tokens", Quantity: 5000},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill: %v", err)

	// Any still-claimable step now refuses.
	reviewID := stepIDByInstance(t, conn, "review@0#0")
	_, err = ClaimStep(conn, reviewID, ClaimOptions{Owner: "w", NowMS: nowMS})
	if err == nil {
		t.Fatal("a claim succeeded past the measured cap; R7 and the claim " +
			"must agree or a stale next row gets past the wall")
	}

	// And the run is PAUSED with a reason naming the measured dimension, so
	// an operator raising a cap raises the right one.
	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if !strings.Contains(run.Reason, "usage budget") ||
		!strings.Contains(run.Reason, "output_tokens") {
		t.Errorf("breach reason = %q, want one naming the measured cap and "+
			"its unit", run.Reason)
	}
}
