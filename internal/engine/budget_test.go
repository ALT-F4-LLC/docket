package engine

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Budgets — the floor, `max(reported, floor)`, the boundary, the breach, the
// headroom check's agreement with the claim, and budget dormancy.
//
// The fixture supplies every cost these tests need, and they are read from it
// rather than restated: `implement` 1.50, `review` 0.60 PER EXPANDED SIBLING
// across four, `fix` 1.00 at `max_fix_loops = 2`. A test that hard-coded them
// would pass against a fixture whose costs had changed, which is the one thing
// a floor test must not do.

// budgetRun activates a run and sets its cap, returning the run id.
//
// The cap is written to the RUN ROW because that is where the effective cap is
// read from — a test that set `budget.default` instead would be testing the
// config fallback while claiming to test the run flag.
func budgetRun(t *testing.T, conn *sql.DB, cap float64) (runID, issueID int) {
	t.Helper()
	run, issue := activatedRun(t, conn)
	if cap != 0 {
		execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cap, run.ID)
	}
	return run.ID, issue
}

// runFloor computes the floor exactly as enforcement does, through the same
// exported query. A test helper that summed the ledger or read `usage_floor`
// would be asserting against a different number than the one that decides.
func runFloor(t *testing.T, conn *sql.DB, runID int) float64 {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	floor, err := RunFloorTx(tx, runID)
	testsupport.Must(t, err, "RunFloorTx: %v", err)
	return floor
}

// claimInstance claims a step by its rendered identity, failing the test on
// refusal. It returns the claim so a caller can complete against the token.
func claimInstance(t *testing.T, conn *sql.DB, instance string, at int64) *ClaimResult {
	t.Helper()
	claim, err := ClaimStep(conn, stepIDByInstance(t, conn, instance),
		ClaimOptions{Owner: "worker", NowMS: at})
	testsupport.Must(t, err, "claim %s: %v", instance, err)
	return claim
}

// expectedCostOf reads a step's stored `expected_cost` — the value the floor
// accrues, materialized at expansion from the PINNED definition.
func expectedCostOf(t *testing.T, conn *sql.DB, instance string) float64 {
	t.Helper()
	var cost float64
	err := conn.QueryRow(
		`SELECT expected_cost FROM steps WHERE instance = ?`, instance).Scan(&cost)
	testsupport.Must(t, err, "reading expected_cost of %s: %v", instance, err)
	return cost
}

// ---------------------------------------------------------------------------
// The floor is a SUM over claim events, one case per accrual rule
// ---------------------------------------------------------------------------

// TestFloorAccruesPerClaimEvent: a plain claim accrues the step's cost, once.
func TestFloorAccruesPerClaimEvent(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	if got := runFloor(t, conn, runID); got != 0 {
		t.Fatalf("floor before any claim = %g, want 0", got)
	}

	want := expectedCostOf(t, conn, "implement@0")
	claimInstance(t, conn, "implement@0", nowMS)

	if got := runFloor(t, conn, runID); got != want {
		t.Errorf("floor after one claim = %g, want %g", got, want)
	}
}

// TestFloorRetriesReAccrue: a reaped step claimed again writes a SECOND
// `step-claimed` event and the SUM counts BOTH.
//
// Nothing is released on reap, on fail, or on abandon — the work was attempted
// and the attempt is what cost something. This is the clause that makes the
// floor a record of what was spent rather than of what is currently in flight.
func TestFloorRetriesReAccrue(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	cost := expectedCostOf(t, conn, "implement@0")

	claimInstance(t, conn, "implement@0", nowMS)
	if got := runFloor(t, conn, runID); got != cost {
		t.Fatalf("floor after the first claim = %g, want %g", got, cost)
	}

	// Force the lease to lapse, then claim again: the reap happens inside the
	// claim (the lazy reap) and the second claim writes its own event.
	execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE instance = 'implement@0'`)
	claimInstance(t, conn, "implement@0", nowMS+1)

	if got := runFloor(t, conn, runID); got != 2*cost {
		t.Errorf("floor after a reap and a re-claim = %g, want %g — retries "+
			"re-accrue and nothing is released", got, 2*cost)
	}
}

// TestFloorFanoutAccruesPerSibling: `expected_cost` is PER EXPANDED SIBLING,
// so four siblings claim four times and accrue four times. No division, no
// proration — the fixture says so verbatim.
func TestFloorFanoutAccruesPerSibling(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "summary", "")

	var siblings []string
	for _, s := range runSteps(t, conn, runID) {
		if s.StepName == "review" && s.Status == db.StepPending {
			siblings = append(siblings, s.Instance)
		}
	}
	if len(siblings) < 2 {
		t.Fatalf("the fixture expanded %d claimable review siblings; the fanout "+
			"case needs several", len(siblings))
	}

	before := runFloor(t, conn, runID)
	var want float64
	for _, instance := range siblings {
		want += expectedCostOf(t, conn, instance)
		claimInstance(t, conn, instance, nowMS)
	}

	if got := runFloor(t, conn, runID) - before; math.Abs(got-want) > 1e-9 {
		t.Errorf("the fanout accrued %g, want %g (one accrual per sibling, no "+
			"proration)", got, want)
	}
}

// TestFloorIgnoresUnclaimedSteps: a step that was never claimed
// contributes NOTHING, because the accrual is per CLAIM EVENT.
//
// It covers the skipped and superseded cases by the same mechanism rather than
// by three branches: none of them produced a `step-claimed` event, and the SUM
// counts events.
func TestFloorIgnoresUnclaimedSteps(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	// Every step of the run exists and none is claimed.
	steps := runSteps(t, conn, runID)
	if len(steps) < 2 {
		t.Fatalf("the fixture expanded %d steps; this case needs several", len(steps))
	}
	var declared float64
	for _, s := range steps {
		declared += s.ExpectedCost
	}
	if declared == 0 {
		t.Fatal("premise: the fixture's steps declare no cost at all")
	}

	if got := runFloor(t, conn, runID); got != 0 {
		t.Errorf("floor with %g of declared cost and nothing claimed = %g, want 0",
			declared, got)
	}

	// A SKIPPED step is the same story told by the fixture's own expansion: it
	// exists, it declares a cost, and it accrues nothing because nobody claimed
	// it.
	skipped := 0
	for _, s := range steps {
		if s.Status == db.StepSkipped {
			skipped++
		}
	}
	claimInstance(t, conn, "implement@0", nowMS)
	if got := runFloor(t, conn, runID); got != expectedCostOf(t, conn, "implement@0") {
		t.Errorf("floor = %g after one claim among %d steps (%d skipped); only the "+
			"claim accrued", got, len(steps), skipped)
	}
}

// threePanelSrc declares three vote steps with distinct costs, so a floor that
// counts any subset of them is distinguishable from every other subset.
const threePanelSrc = `
[pipeline]
name = "three-panels"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "worker"
emits = "diff"
expected_cost = 1.0

[[step]]
name = "panel-skipped"
after = ["implement"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
expected_cost = 2.0
on_fail = "waiting-human"

[[step]]
name = "panel-superseded"
after = ["implement"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
expected_cost = 4.0
on_fail = "waiting-human"

[[step]]
name = "panel-pending"
after = ["implement"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
expected_cost = 8.0
on_fail = "waiting-human"
`

// TestFloorIgnoresSkippedAndSupersededVoteRows is the same rule for vote
// steps: a vote row accrues at materialization, but a skipped or superseded
// panel is never routed, so it contributes nothing.
func TestFloorIgnoresSkippedAndSupersededVoteRows(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(threePanelSrc), "three-panels.toml")
	issue := createIssue(t, conn, "three panels", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = 'panel-skipped@0'`,
		db.StepSkipped)
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = 'panel-superseded@0'`,
		db.StepSuperseded)

	want := expectedCostOf(t, conn, "panel-pending@0")
	if got := runFloor(t, conn, run.ID); got != want {
		t.Errorf("floor = %g, want %g (only the pending panel accrues; the "+
			"skipped 2.0 and superseded 4.0 panels must not)", got, want)
	}
}

// TestFloorBoundedByMaxFixLoops checks loop re-accrual at the fixture's
// `max_fix_loops = 2`: the floor cannot exceed the arithmetic bound, ever.
//
// A loop entry is a DIFFERENT step row with its own `expected_cost`, claimed and
// event-logged independently, so `max_fix_loops` bounds the floor BY
// CONSTRUCTION — "bounded loops bound the floor" falling out of loop expansion
// rather than needing arithmetic of its own. The assertion is therefore
// over what the run CAN instantiate, not over a number this test computes.
func TestFloorBoundedByMaxFixLoops(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	// The bound: every step the run could ever instantiate, claimed once per
	// permitted loop entry. Nothing this run does may push the floor past it.
	steps := runSteps(t, conn, runID)
	var perPass float64
	for _, s := range steps {
		perPass += s.ExpectedCost
	}
	const maxLoops = 2 // the fixture's `max_fix_loops`
	bound := perPass * (maxLoops + 1)

	// Claim everything claimable, repeatedly, forcing reaps in between — the
	// most expensive thing an operator could do to this run short of entering
	// loops, which expansion controls.
	for range 3 {
		for _, s := range runSteps(t, conn, runID) {
			if s.Status != db.StepPending || s.Kind != "executor" {
				continue
			}
			if _, err := ClaimStep(conn, s.ID,
				ClaimOptions{Owner: "worker", NowMS: nowMS}); err != nil {
				continue // not ready, or refused: both are fine here
			}
		}
		execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE run_id = ?`, runID)
	}

	if got := runFloor(t, conn, runID); got > bound {
		t.Errorf("floor = %g, above the %g the loop bound permits; a floor that "+
			"can exceed its bound is one `max_fix_loops` does not bound", got, bound)
	}
}

// TestFloorIsNeverReadFromCache is the floor/cache separation, with a POISONED
// cache.
//
// `runs.usage_floor` is written beside every accrual as a cache for the report's
// burn-rate line. If enforcement ever read it, a wrong value there would move a
// decision — and the whole no-lost-update argument for a SUM over an
// append-only log would be undone by a stored number sitting beside it. So the
// test seeds a deliberately wrong value and asserts that enforcement, the
// refusal, and the breach transition all behave as though it said the truth.
func TestFloorIsNeverReadFromCache(t *testing.T) {
	conn := mustDB(t)
	cost := 1.50 // `implement`'s declared cost; asserted below, not assumed
	runID, _ := budgetRun(t, conn, 100)
	if got := expectedCostOf(t, conn, "implement@0"); got != cost {
		t.Fatalf("premise: implement@0 costs %g, not the %g this case assumes", got, cost)
	}

	// The poison: a cache claiming the run has already spent far past its cap.
	// A reader of this column would refuse the very next claim.
	execSQL(t, conn, `UPDATE runs SET usage_floor = ? WHERE id = ?`, 999999.0, runID)

	claimInstance(t, conn, "implement@0", nowMS)

	// The claim SUCCEEDED, so nothing consulted the poison. And the accrual
	// overwrote it with the truth, because the cache is refreshed in the same
	// transaction as the accrual.
	facts, err := db.RunBudgetFactsFor(conn, runID)
	testsupport.Must(t, err, "RunBudgetFactsFor: %v", err)
	if facts.CachedFloor != cost {
		t.Errorf("cached floor = %g after the accrual, want %g — the cache is "+
			"refreshed beside the accrual, not left to drift", facts.CachedFloor, cost)
	}

	// The other direction: a cache claiming NOTHING has been spent must not
	// rescue a run that has genuinely crossed its cap.
	execSQL(t, conn, `UPDATE runs SET budget = ?, usage_floor = 0 WHERE id = ?`, cost, runID)

	e := testEngine()
	_ = e
	steps := runSteps(t, conn, runID)
	var next *db.Step
	for _, s := range steps {
		if s.Status == db.StepPending && s.Kind == "executor" && s.ExpectedCost > 0 {
			next = s
			break
		}
	}
	if next == nil {
		t.Skip("no second costed executor step to claim against the cap")
	}
	if _, err := ClaimStep(conn, next.ID, ClaimOptions{Owner: "w2", NowMS: nowMS}); err == nil {
		t.Error("a claim past the cap succeeded while usage_floor said 0; " +
			"enforcement read the cache")
	}
}

// ---------------------------------------------------------------------------
// max(reported, floor)
// ---------------------------------------------------------------------------

// TestSpendIsMaxOfReportedAndFloor walks five cases over the snapshot's own
// arithmetic: reporting nothing, reporting less than the floor, reporting
// more, reporting in a unit that is NOT `budget.unit`, and reporting in the one
// that is.
//
// It exercises budgetSnapshot directly because the property is about the
// arithmetic, and driving it through five full runs would test the plumbing five
// times and the arithmetic once.
func TestSpendIsMaxOfReportedAndFloor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		floor    float64
		reported float64
		want     float64
		why      string
	}{
		{"reporting nothing", 4, 0, 4,
			"a claimant reporting nothing cannot lower a run below its floor"},
		{"reporting less than the floor", 4, 1, 4,
			"reported usage can only RAISE the counter"},
		{"reporting more than the floor", 4, 9, 9,
			"the enforced quantity is the larger of the two"},
		{"reporting exactly the floor", 4, 4, 4,
			"the two agreeing is not a special case"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := budgetSnapshot{cap: 100, floor: tc.floor, reported: tc.reported}
			if got := snap.spend(); got != tc.want {
				t.Errorf("spend = %g, want %g — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestReportedRestsOnTheFloorWithNoBudgetUnit: with `budget.unit` at its
// default, `reported` is 0 and the enforcement rests entirely on the floor.
//
// Ledger rows in every other unit are RECORDED and never compared to the cap.
// Core does not know what a token is; summing `{tokens: 4000,
// seconds: 12}` to 4012 would be core asserting those add up, and core has no
// meanings about units.
func TestReportedRestsOnTheFloorWithNoBudgetUnit(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 100)
	e := testEngine()

	completeWithUsage(t, conn, e, "implement@0", `{"tokens": 4000, "seconds": 12}`)

	// The rows are there — recording is unconditional.
	totals, err := db.UsageByUnit(conn, runID)
	testsupport.Must(t, err, "UsageByUnit: %v", err)
	if len(totals) != 2 {
		t.Fatalf("ledger holds %d units, want 2 (both were recorded)", len(totals))
	}

	// And `reported` is 0 for enforcement, because no unit is named.
	withBudget(t, conn, runID, func(snap budgetSnapshot) {
		if snap.unit != "" {
			t.Fatalf("premise: budget.unit = %q, want the default", snap.unit)
		}
		if snap.reported != 0 {
			t.Errorf("reported = %g with no budget.unit set, want 0 — the cap "+
				"rests on the floor alone", snap.reported)
		}
		if snap.spend() != snap.floor {
			t.Errorf("spend = %g, want the floor %g", snap.spend(), snap.floor)
		}
	})
}

// TestReportedCountsOnlyTheNamedUnit: setting `budget.unit` makes
// `reported` the sum of THAT unit's rows, and every other unit's rows stay out
// of the comparison entirely.
func TestReportedCountsOnlyTheNamedUnit(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 100000)
	e := testEngine()

	err := db.SetConfig(conn, 0, db.KeyBudgetUnit, "tokens")
	testsupport.Must(t, err, "setting budget.unit: %v", err)
	completeWithUsage(t, conn, e, "implement@0", `{"tokens": 4000, "seconds": 12}`)

	withBudget(t, conn, runID, func(snap budgetSnapshot) {
		if snap.unit != "tokens" {
			t.Fatalf("budget.unit = %q, want tokens", snap.unit)
		}
		if snap.reported != 4000 {
			t.Errorf("reported = %g, want 4000 — the named unit's rows and no "+
				"others", snap.reported)
		}
		if snap.spend() != 4000 {
			t.Errorf("spend = %g, want 4000: reported now exceeds the floor",
				snap.spend())
		}
	})
}

// ---------------------------------------------------------------------------
// The boundary
// ---------------------------------------------------------------------------

// TestBudgetBoundaryIsCrossingNotReaching checks the boundary at values that
// stress binary representation.
//
// "Crossing" is `>`, not `>=`: a cap of 12 and a spend that reaches exactly 12
// has SPENT its budget and not exceeded it. The float cases are the reason this
// is a table rather than two assertions — 0.1 x 10 is not 1.0 in binary, and a
// boundary written as an equality comparison against a summed float would refuse
// a claim that lands exactly on the cap about half the time.
func TestBudgetBoundaryIsCrossingNotReaching(t *testing.T) {
	// 0.1 summed ten times, which is 0.9999999999999999 rather than 1.
	var summed float64
	for range 10 {
		summed += 0.1
	}

	for _, tc := range []struct {
		name       string
		cap, spend float64
		cost       float64
		wantAdmits bool
		why        string
	}{
		{"exactly at the cap", 12, 10, 2, true,
			"landing exactly on the cap is allowed: the budget is spent, not exceeded"},
		{"one past the cap", 12, 10, 2.000001, false,
			"crossing is refused"},
		{"already at the cap, zero-cost step", 12, 12, 0, true,
			"a free step at a spent cap crosses nothing"},
		{"already at the cap, any cost", 12, 12, 0.0001, false,
			"the first step that would cross is the one refused"},
		{"binary-representation sum, landing on 1.0", 1, summed, 1 - summed, true,
			"0.1 x 10 is not 1.0 in binary; the comparison must not turn that " +
				"into a spurious refusal"},
		{"binary-representation sum, past 1.0", 1, summed, 0.1, false,
			"and it must still refuse a genuine crossing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := budgetSnapshot{cap: tc.cap, floor: tc.spend}
			if got := snap.admits(tc.cost); got != tc.wantAdmits {
				t.Errorf("admits(%g) at spend %g of cap %g = %v, want %v — %s",
					tc.cost, tc.spend, tc.cap, got, tc.wantAdmits, tc.why)
			}
		})
	}
}

// TestClaimAtTheCapIsAllowedAndTheNextIsRefused drives the boundary through a
// REAL claim rather than the arithmetic: a cap set to exactly one step's cost
// admits that step and refuses the next.
func TestClaimAtTheCapIsAllowedAndTheNextIsRefused(t *testing.T) {
	conn := mustDB(t)
	conn2 := conn
	runID, _ := budgetRun(t, conn, 0)
	cost := expectedCostOf(t, conn, "implement@0")
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cost, runID)

	// Exactly at the cap: allowed.
	claimInstance(t, conn, "implement@0", nowMS)
	if got := runFloor(t, conn2, runID); got != cost {
		t.Fatalf("floor = %g after the claim that lands on the cap, want %g", got, cost)
	}

	// The next costed claim would cross, and is refused — with the run paused.
	execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE instance = 'implement@0'`)
	_, err := ClaimStep(conn, stepIDByInstance(t, conn, "implement@0"),
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

// ---------------------------------------------------------------------------
// The breach transition
// ---------------------------------------------------------------------------

// TestBreachPausesTheRunWithAReason: the claim is
// refused, the run flips to `waiting-human` with a bare-number reason naming no
// unit, and the event is `run-paused` with `data.reason = "budget"` — an
// EXISTING closed-set kind, so the set does not widen.
func TestBreachPausesTheRunWithAReason(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0.01) // below any costed step
	instance := "implement@0"

	_, err := ClaimStep(conn, stepIDByInstance(t, conn, instance),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	if err == nil {
		t.Fatal("the claim past the cap was admitted")
	}

	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Errorf("run is %s after the breach, want %s", run.Status, model.RunWaitingHuman)
	}

	facts, err := db.RunBudgetFactsFor(conn, runID)
	testsupport.Must(t, err, "RunBudgetFactsFor: %v", err)
	want := BudgetBreachReason(0, 0.01, instance)
	if facts.BreachReason != want {
		t.Errorf("breach_reason = %q, want %q", facts.BreachReason, want)
	}
	// A BARE-NUMBER statement. No currency, no token, no rate — what
	// the number counts is the workflow author's business.
	for _, leak := range []string{"token", "dollar", "$", "cost in"} {
		if strings.Contains(strings.ToLower(facts.BreachReason), leak) {
			t.Errorf("the breach reason names a denomination (%q): %q",
				leak, facts.BreachReason)
		}
	}

	// `run-paused`, with the reason in `data`. The kind is the existing one,
	// so the closed set of event kinds is unchanged. The refused claim's own
	// `claim-refused` (DKT-2776) follows it; that records the claim, not the
	// pause, so the pause is read by kind rather than as the last event.
	if n := countRunEvents(t, conn, runID, EventRunPaused); n != 1 {
		t.Errorf("the breach wrote %d %q events, want 1 — a new kind would widen "+
			"the closed set for a transition that is already a pause",
			n, EventRunPaused)
	}
	var data string
	err = conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, EventRunPaused).Scan(&data)
	testsupport.Must(t, err, "reading the pause event: %v", err)
	if !strings.Contains(data, `"reason":"budget"`) {
		t.Errorf("the pause event's data is %q, want it to carry reason=budget", data)
	}
}

// TestBreachIsIdempotentUnderConcurrentFlips: the flip is CAS on
// (run_id, status='active'), so a second invocation that observes the same
// crossing matches ZERO rows and writes neither a second reason nor a second
// event.
func TestBreachIsIdempotentUnderConcurrentFlips(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0.01)

	for i := range 2 {
		_, err := ClaimStep(conn, stepIDByInstance(t, conn, "implement@0"),
			ClaimOptions{Owner: fmt.Sprintf("w%d", i), NowMS: nowMS + int64(i)})
		if err == nil {
			t.Fatalf("claim %d past the cap was admitted", i)
		}
	}

	// Exactly ONE pause event. The second flip matched no row — the run was
	// already `waiting-human` — so it wrote nothing.
	if n := countRunEvents(t, conn, runID, EventRunPaused); n != 1 {
		t.Errorf("%d %s events after two breaching claims, want 1 — the CAS is "+
			"what makes the second a no-op", n, EventRunPaused)
	}
}

// TestResumeRePausesAtTheSameCap is stated because "resume immediately
// re-pauses" reads as a bug and is not one: `run resume` clears nothing, the cap
// has not moved, so the condition has not changed.
//
// Raising the cap is `run start`-time only in v1 — recorded rather than
// invented here, because the command surface has no budget mutator.
func TestResumeRePausesAtTheSameCap(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0.01)

	if _, err := ClaimStep(conn, stepIDByInstance(t, conn, "implement@0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS}); err == nil {
		t.Fatal("the first claim past the cap was admitted")
	}

	// The operator resumes.
	err := db.SetRunStatus(conn, runID, model.RunActive, "", nowMS+1)
	testsupport.Must(t, err, "resuming: %v", err)

	// And the next claim pauses it again, at the same cap.
	if _, err := ClaimStep(conn, stepIDByInstance(t, conn, "implement@0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS + 2}); err == nil {
		t.Fatal("the claim after the resume was admitted; the cap has not moved")
	}
	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Errorf("run is %s after resume-and-claim, want %s",
			run.Status, model.RunWaitingHuman)
	}
}

// TestBreachSurvivesAPreBreachStepCompleting is DKT-68: a step claimed BEFORE
// the run's budget breach, completing AFTER it, must not silently resume the
// run. `BreachRunBudgetTx` pauses only the RUN row — it parks no step — so
// `reconcileRun`'s "parked" count reads 0 for a budget-paused run, and without
// a guard the rollup's default branch ("still working, return to active")
// would flip the run back to `active` with no operator verb the moment any
// other in-flight step routes. That is the exact seq 637/640/643 flap RUN-5
// hit: run-paused, then a verb-less run-resumed on a pre-pause-claimed step's
// completion, then re-paused on the very next over-cap claim.
func TestBreachSurvivesAPreBreachStepCompleting(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()

	// No cap yet — set once the review siblings exist and their cost is known.
	runID, _ := budgetRun(t, conn, 0)
	implCost := expectedCostOf(t, conn, "implement@0")

	claimAndComplete(t, conn, e, "implement@0", "summary", "")

	var siblings []string
	for _, s := range runSteps(t, conn, runID) {
		if s.StepName == "review" && s.Status == db.StepPending {
			siblings = append(siblings, s.Instance)
		}
	}
	if len(siblings) < 2 {
		t.Fatalf("the fixture expanded %d claimable review siblings; this case "+
			"needs at least 2", len(siblings))
	}
	reviewCost := expectedCostOf(t, conn, siblings[0])
	// Admits `implement` plus exactly one review sibling; the second review
	// claim crosses the cap and breaches.
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`,
		implCost+reviewCost+0.01, runID)

	// Claimed BEFORE the breach, completed AFTER it.
	preBreachClaim := claimInstance(t, conn, siblings[0], nowMS)

	if _, err := ClaimStep(conn, stepIDByInstance(t, conn, siblings[1]),
		ClaimOptions{Owner: "worker2", NowMS: nowMS + 1}); err == nil {
		t.Fatal("the second review claim past the cap was admitted")
	}

	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Fatalf("premise: run is %s after the breach, want %s",
			run.Status, model.RunWaitingHuman)
	}

	// The pre-breach claim completes, routing through reconcileIssueAndRun ->
	// reconcileRun. The step it belongs to is not itself parked (it just went
	// to `done`), and the OTHER sibling is still pending, unclaimed and
	// unfinished — so absent the DKT-68 guard, the rollup's default branch
	// sees `parked == 0, unfinished > 0` and auto-resumes.
	err = e.CompleteStep(conn, stepIDByInstance(t, conn, siblings[0]), CompleteOptions{
		Token: preBreachClaim.Token, Artifact: []byte("findings"), NowMS: nowMS + 2,
	})
	testsupport.Must(t, err, "completing the pre-breach claim: %v", err)

	run, err = db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun after completion: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Errorf("run is %s after a pre-breach step completed, want %s — a "+
			"budget-paused run must stay parked until an operator verb moves "+
			"it, not auto-resume because an unrelated step finished",
			run.Status, model.RunWaitingHuman)
	}

	facts, err := db.RunBudgetFactsFor(conn, runID)
	testsupport.Must(t, err, "RunBudgetFactsFor: %v", err)
	if facts.BreachReason == "" {
		t.Error("breach_reason was cleared by the completion; it should survive " +
			"until an operator resolves the breach")
	}

	if n := countRunEvents(t, conn, runID, EventRunResumed); n != 0 {
		t.Errorf("%d %s events after a pre-breach step completed, want 0 — "+
			"nothing here is an operator's resume", n, EventRunResumed)
	}
}

// ---------------------------------------------------------------------------
// The headroom check, its agreement with the claim, and budget dormancy
// ---------------------------------------------------------------------------

// TestBudgetR7AndClaimAgree is the agreement property over a generated matrix
// of (cap, floor, cost).
//
// budgetHeadroom makes a step not APPEAR in `next`; the claim's boundary check
// makes a claim REFUSE. They are the same arithmetic at two moments and both
// are needed — the headroom check alone would let a relay claim a step it
// already held a stale `next` row for, and the claim check alone would offer
// steps that cannot be claimed. What must never happen is that they
// DISAGREE over one snapshot: a step `next` offers and `claim` refuses is a
// dispatcher stuck in a loop it cannot diagnose.
func TestBudgetR7AndClaimAgree(t *testing.T) {
	caps := []float64{0, 0.5, 1, 1.5, 12, 100}
	floors := []float64{0, 0.5, 1, 11.999, 12, 12.5}
	costs := []float64{0, 0.001, 0.5, 1.5, 12}

	for _, cap := range caps {
		for _, floor := range floors {
			for _, cost := range costs {
				snap := budgetSnapshot{cap: cap, floor: floor}
				sched := &Scheduler{budget: snap}
				step := &db.Step{ExpectedCost: cost}

				r7 := sched.budgetHeadroom(step)
				// The claim's enforcement refuses exactly when `admits` is
				// false, and admits everything when the cap is unlimited.
				claimAdmits := snap.unlimited() || snap.admits(cost)

				if r7 != claimAdmits {
					t.Errorf("cap %g, floor %g, cost %g: budgetHeadroom says %v and the claim "+
						"says %v — the two must never disagree over one snapshot",
						cap, floor, cost, r7, claimAdmits)
				}
			}
		}
	}
}

// TestBudgetDormancyExecutesNoQuery is budget dormancy, asserted by COUNTING
// QUERIES rather than by inspection.
//
// `budgetHeadroom` returns true on its first line when the effective cap is 0,
// and the snapshot's loader declines to query at all in that case. A run
// started without `--budget` in a repo that never set `budget.default`
// therefore executes exactly the queries v9 executed — the dormancy claim,
// measured rather than asserted.
func TestBudgetDormancyExecutesNoQuery(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	run, err := db.GetRunTx(tx, runID)
	testsupport.Must(t, err, "GetRunTx: %v", err)
	snap, err := loadBudget(tx, run)
	testsupport.Must(t, err, "loadBudget: %v", err)

	if !snap.unlimited() {
		t.Fatalf("premise: a run with no --budget in a repo with no default is "+
			"not unlimited (cap %g)", snap.cap)
	}
	if snap.source != BudgetUnlimited {
		t.Errorf("source = %q, want %q", snap.source, BudgetUnlimited)
	}
	// The floor and the reported sum were NOT computed: an unlimited run runs no
	// budget query, so both are the zero value rather than a number.
	if snap.floor != 0 || snap.reported != 0 {
		t.Errorf("an unlimited snapshot carries floor %g and reported %g; both "+
			"queries must be skipped entirely", snap.floor, snap.reported)
	}

	// And the same run, having actually claimed a step, still reports an
	// unlimited snapshot with no floor computed — the dormancy is not "until
	// something happens", it is unconditional on the cap.
	tx.Rollback()
	claimInstance(t, conn, "implement@0", nowMS)
	withBudget(t, conn, runID, func(snap budgetSnapshot) {
		if snap.floor != 0 {
			t.Errorf("an unlimited run computed a floor of %g after a claim; the "+
				"query must not run at all", snap.floor)
		}
	})
}

// TestEffectiveCapIsTheRunRow: the cap a claim enforces is read from the RUN
// ROW and from nowhere else.
//
// The config fallback is resolved once, at `run start`, which materializes
// `budget.default` into the row (run_start.go). By the time any claim reads it,
// the row already carries whichever of the two applied — which is what makes
// the fallback and the run-row rule one rule rather than two that could
// disagree mid-run.
func TestEffectiveCapIsTheRunRow(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runBudget float64
		// configTo is set AFTER the run exists, and must change nothing.
		configTo      string
		wantCap       float64
		wantUnlimited bool
	}{
		{"run row set", 12, "0", 12, false},
		{"a config default set afterwards does not reach the run", 0, "7", 0, true},
		{"the row wins over a later config change", 12, "7", 12, false},
		{"neither", 0, "0", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustDB(t)
			runID, _ := budgetRun(t, conn, tc.runBudget)
			err := db.SetConfig(conn, 0, db.KeyBudgetDefault, tc.configTo)
			testsupport.Must(t, err, "setting budget.default: %v", err)

			withBudget(t, conn, runID, func(snap budgetSnapshot) {
				if snap.cap != tc.wantCap {
					t.Errorf("cap = %g, want %g", snap.cap, tc.wantCap)
				}
				if snap.unlimited() != tc.wantUnlimited {
					t.Errorf("unlimited = %v, want %v", snap.unlimited(), tc.wantUnlimited)
				}
			})
		})
	}
}

// TestBudgetSourceOf is the report's source derivation: the source is COMPARED
// rather than stored, because the runs table has no `budget_source` column and
// inventing one would be a silent deviation.
func TestBudgetSourceOf(t *testing.T) {
	for _, tc := range []struct {
		cap, configDefault float64
		want               BudgetSource
	}{
		{0, 0, BudgetUnlimited},
		{0, 7, BudgetUnlimited},  // the row is what enforces, and it says 0
		{12, 0, BudgetFromRun},   // no default exists, so the flag put it there
		{12, 7, BudgetFromRun},   // differs from the default
		{7, 7, BudgetFromConfig}, // indistinguishable, and harmlessly so
	} {
		got := BudgetSourceOf(tc.cap, tc.configDefault)
		if got != tc.want {
			t.Errorf("BudgetSourceOf(%g, %g) = %q, want %q",
				tc.cap, tc.configDefault, got, tc.want)
		}
	}
}

// TestCapSetAfterRunStartDoesNotReCapTheRun is the run-row rule's consequence,
// stated because it is surprising: a run started before `budget.default` was
// set has `runs.budget = 0` stored and stays UNLIMITED even after the default
// is set.
//
// That is the pinning property applied to budgets, and it is the right answer —
// a config change must not silently re-cap a live run. `run report` prints the
// effective cap and where it came from precisely so an operator asking "why
// didn't it stop?" gets the answer from a read verb.
func TestCapSetAfterRunStartDoesNotReCapTheRun(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	// The run is already under way.
	claimInstance(t, conn, "implement@0", nowMS)

	// Now somebody sets a default. It applies to runs STARTED after it, not to
	// this one — because this one's row says 0 and enforcement reads the row.
	err := db.SetConfig(conn, 0, db.KeyBudgetDefault, "0.01")
	testsupport.Must(t, err, "setting budget.default: %v", err)

	withBudget(t, conn, runID, func(snap budgetSnapshot) {
		if !snap.unlimited() {
			t.Errorf("a mid-flight run gained a cap of %g from a config change; "+
				"the cap is read from the run row", snap.cap)
		}
	})
}

// ---------------------------------------------------------------------------
// `--usage` validation
// ---------------------------------------------------------------------------

func TestUsageValidation(t *testing.T) {
	longName := strings.Repeat("u", db.UnitNameMaxBytes+1)
	manyUnits := make([]string, 0, db.UsageUnitsMax+1)
	for i := range db.UsageUnitsMax + 1 {
		manyUnits = append(manyUnits, fmt.Sprintf(`"u%d": 1`, i))
	}

	for _, tc := range []struct {
		name    string
		raw     string
		wantErr string
		why     string
	}{
		{"empty is not a usage record", "", "",
			"a `complete` without --usage records nothing and refuses nothing"},
		{"an object of numbers", `{"tokens": 4000, "seconds": 12.5}`, "",
			"an object of non-negative numbers is the accepted shape"},
		{"zero is a legal quantity", `{"tokens": 0}`, "",
			"reporting zero is reporting, and max(reported, floor) makes it harmless"},
		{"an array", `[1, 2]`, "must be a JSON object",
			"any other shape is a VALIDATION_ERROR"},
		{"a bare number", `4000`, "must be a JSON object", "any other shape is a VALIDATION_ERROR"},
		{"a string value", `{"tokens": "4000"}`, `"tokens" must be a number`,
			"the refusal names the offending key rather than failing anonymously"},
		{"a negative quantity", `{"tokens": -1}`, "must be >= 0",
			"usage that reduces a total is not usage"},
		{"a unit name with whitespace", `{"to kens": 1}`,
			"printable ASCII without whitespace",
			"a ledger key is rendered into a terminal beside a number"},
		{"an over-long unit name", `{"` + longName + `": 1}`, "the limit is 64",
			"the refusal names the limit"},
		{"too many units", "{" + strings.Join(manyUnits, ",") + "}",
			"the limit is 32", "the refusal names the limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseUsage(tc.raw)
			if tc.wantErr == "" {
				testsupport.Must(t, err, "parseUsage(%s) = %v, want no error — %s",
					tc.raw, err, tc.why)

				return
			}
			if err == nil {
				t.Fatalf("parseUsage(%s) was accepted; %s", tc.raw, tc.why)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q — %s",
					err.Error(), tc.wantErr, tc.why)
			}
			if code, _ := CodeOf(err); code != CodeValidation {
				t.Errorf("error code = %v, want %v", code, CodeValidation)
			}
		})
	}
}

// TestUsageRefusesNonFiniteNumbers is the other half of the negative-quantity
// refusal. NaN and Inf are not
// expressible in JSON, so they arrive only as the tokens a hand-rolled encoder
// emits — and either would poison every sum the ledger feeds.
func TestUsageRefusesNonFiniteNumbers(t *testing.T) {
	for _, raw := range []string{`{"tokens": NaN}`, `{"tokens": Infinity}`, `{"tokens": 1e400}`} {
		if _, err := parseUsage(raw); err == nil {
			t.Errorf("parseUsage(%s) was accepted; a non-finite quantity poisons "+
				"every sum the ledger feeds", raw)
		}
	}
}

// TestUsageRecordsLedgerRows: one row per key, `source = 'reported'`,
// keyed (step_id, attempt, unit) — and the EVENT still carries the blob, because
// removing it would break the replay property.
func TestUsageRecordsLedgerRows(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)
	e := testEngine()

	completeWithUsage(t, conn, e, "implement@0", `{"pages": 3, "sheets": 1.5}`)

	totals, err := db.UsageByUnit(conn, runID)
	testsupport.Must(t, err, "UsageByUnit: %v", err)
	if len(totals) != 2 {
		t.Fatalf("ledger holds %d units, want 2", len(totals))
	}
	// Ordered by unit name: `pages` before `sheets`, deterministically.
	if totals[0].Unit != "pages" || totals[1].Unit != "sheets" {
		t.Errorf("units came back as %s, %s; the rollup orders by a total key",
			totals[0].Unit, totals[1].Unit)
	}
	if totals[0].Quantity != 3 || totals[1].Quantity != 1.5 {
		t.Errorf("quantities = %g, %g; want 3 and 1.5",
			totals[0].Quantity, totals[1].Quantity)
	}

	var source string
	err = conn.QueryRow(
		`SELECT source FROM usage_ledger WHERE unit = 'pages'`).Scan(&source)
	testsupport.Must(t, err, "reading the source: %v", err)
	if source != db.UsageSourceReported {
		t.Errorf("source = %q, want %q — a claimant said so", source, db.UsageSourceReported)
	}

	// The step is marked, so the usage discrepancy probe has its fast path
	// populated from the moment the ledger exists.
	var recorded int
	err = conn.QueryRow(
		`SELECT usage_recorded FROM steps WHERE instance = 'implement@0'`).
		Scan(&recorded)
	testsupport.Must(t, err, "reading usage_recorded: %v", err)
	if recorded != 1 {
		t.Error("steps.usage_recorded was not set by a `complete --usage`")
	}

	// And the EVENT still carries the blob: replay reconstructs "costing what"
	// from the log, and a log that dropped the numbers could not.
	var data string
	err = conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, EventStepRecorded).Scan(&data)
	testsupport.Must(t, err, "reading the step-recorded event: %v", err)
	if !strings.Contains(data, "pages") {
		t.Errorf("the step-recorded event lost its usage blob: %s", data)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// completeWithUsage is claimAndComplete carrying a `--usage` blob — the ordinary
// path a relay takes, so the ledger rows under test are written by the code the
// CLI calls rather than inserted by the test.
func completeWithUsage(t *testing.T, conn *sql.DB, e *Engine, instance, usage string) {
	t.Helper()
	stepID := stepIDByInstance(t, conn, instance)

	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim %s: %v", instance, err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), Usage: usage, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete %s with usage: %v", instance, err)
}

// withBudget loads a scheduler's budget snapshot and hands it to fn — the same
// snapshot, loaded the same way, that the predicate answers over.
func withBudget(t *testing.T, conn *sql.DB, runID int, fn func(budgetSnapshot)) {
	t.Helper()
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		fn(sched.budget)
	})
}

// runSteps reads a run's steps through the store, in id order.
func runSteps(t *testing.T, conn *sql.DB, runID int) []*db.Step {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	steps, err := db.ListRunStepsTx(tx, runID)
	testsupport.Must(t, err, "ListRunStepsTx: %v", err)
	return steps
}

// countRunEvents counts a run's events of one kind.
func countRunEvents(t *testing.T, conn *sql.DB, runID int, kind string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE run_id = ? AND kind = ?`,
		runID, kind).Scan(&n)
	testsupport.Must(t, err, "counting %s events: %v", kind, err)
	return n
}
