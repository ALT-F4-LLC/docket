package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestBudgetWithholdingIsReported is DKT-242.
//
// R7 withholds a step whose cost would cross the cap. That is correct — but
// before this, it withheld SILENTLY: with 0.9 headroom the engine offered 1 of
// 5 ready judges and named no reason, then `next` answered {"steps":[],
// "total":0} against a run reporting 9 pending. A dispatcher cannot tell that
// from a graph that has run dry, and "run dry" is the reading that makes it
// stop asking. The round serialized around an invisible wall for ~10 minutes
// and an extra wave cycle.
func TestBudgetWithholdingIsReported(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	cost := expectedCostOf(t, conn, "implement@0")

	// A cap BELOW the one ready step's cost: the step is ready by every other
	// condition and admitted by none of the budget.
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cost/2, runID)

	ready, err := NewEngine().NextSteps(conn, runID, 10, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)

	if len(ready.Steps) != 0 {
		t.Fatalf("a cap below the only step's cost still offered %d row(s); "+
			"this test's premise is that R7 withholds here", len(ready.Steps))
	}
	if ready.BudgetHeldReason == "" {
		t.Fatal("steps were withheld for budget and the offer said nothing — " +
			"an empty answer that cannot be told from a finished graph is the " +
			"whole of DKT-242")
	}
	for _, want := range []string{"withheld", "budget headroom", "implement@0"} {
		if !strings.Contains(ready.BudgetHeldReason, want) {
			t.Errorf("the hold does not mention %q: %s", want, ready.BudgetHeldReason)
		}
	}

	// The manifest carries the same fact: a conductor that only ever calls
	// `dispatch open` never sees `next`'s answer.
	manifest, err := NewEngine().OpenDispatch(conn, runID, 10, nil, nowMS)
	testsupport.Must(t, err, "OpenDispatch: %v", err)
	if manifest.BudgetHeld == "" {
		t.Error("the manifest withheld the same rows and named no reason; a " +
			"dispatcher that never calls next would never learn why")
	}
}

// TestNoBudgetHoldWhenNothingIsWithheld keeps the field dormant.
//
// The contract HeldReason and LoopHeldReason set is "empty whenever nothing
// was withheld", so a run with no cap — B29's dormancy — must carry no new
// field on the wire. A hold string that appeared on every offer would be
// noise, and noise is what makes a real hold get skimmed past.
func TestNoBudgetHoldWhenNothingIsWithheld(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	ready, err := NewEngine().NextSteps(conn, runID, 10, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	if len(ready.Steps) == 0 {
		t.Fatal("an uncapped activated run offered nothing; the premise fails")
	}
	if ready.BudgetHeldReason != "" {
		t.Errorf("an uncapped run reports a budget hold: %s", ready.BudgetHeldReason)
	}

	manifest, err := NewEngine().OpenDispatch(conn, runID, 10, nil, nowMS)
	testsupport.Must(t, err, "OpenDispatch: %v", err)
	if manifest.BudgetHeld != "" {
		t.Errorf("an uncapped manifest reports a budget hold: %s", manifest.BudgetHeld)
	}
}

// TestBudgetHoldReasonSummarizesManyRows keeps the message bounded: a run that
// withholds twenty rows must not print twenty of them, the same cap
// LoopHoldReason applies.
func TestBudgetHoldReasonSummarizesManyRows(t *testing.T) {
	s := &Scheduler{budget: budgetSnapshot{cap: 10, floor: 9.5}}
	s.budgetHolds = map[string]float64{
		"a@0": 1, "b@0": 2, "c@0": 3, "d@0": 4, "e@0": 5,
	}
	got := s.BudgetHoldReason()
	if !strings.Contains(got, "5 step(s)") {
		t.Errorf("the count is missing: %s", got)
	}
	if !strings.Contains(got, "and 2 more") {
		t.Errorf("the message is not summarized: %s", got)
	}
	if strings.Contains(got, "e@0") {
		t.Errorf("the message lists past the cap: %s", got)
	}
	if !strings.Contains(got, "0.5") {
		t.Errorf("the headroom that decided it is missing: %s", got)
	}
}
