package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `run budget --set` — B24's loop closed (docs/tdd/events-follow.md §7).

// TestBudgetRaiseUnparksABreachedRun is THE HEADLINE, and it is the whole
// argument for the verb existing.
//
// S6 left the loop open and said so in two places — a comment at the end of
// `enforceBudgetTx` and operations.md §4's runbook — both describing the same
// dead end: a breached run resumes to `active`, the cap has not moved, and the
// next claim breaches again. This test drives that dead end, applies the verb,
// and asserts the claim that could not commit now commits.
//
// It also asserts the TRAIL, in order, because the point of event-logging the
// change is that an auditor reading the run afterwards can see why a run that
// stopped at one number later proceeded past it.
func TestBudgetRaiseUnparksABreachedRun(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0.01) // below any costed step
	instance := "implement@0"
	stepID := stepIDByInstance(t, conn, instance)

	// 1. The breach. This is S6's behavior, unchanged.
	if _, err := ClaimStep(conn, stepID,
		ClaimOptions{Owner: "worker", NowMS: nowMS}); err == nil {
		t.Fatal("the claim past the cap was admitted")
	}
	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Fatalf("run is %s after the breach, want %s", run.Status, model.RunWaitingHuman)
	}

	// 2. THE DEAD END S6 DOCUMENTED: resume alone changes nothing, because the
	// condition has not changed. Asserting it here rather than trusting the
	// comment is what makes the next step a fix rather than a coincidence.
	err = db.SetRunStatus(conn, runID, model.RunActive, "", nowMS)
	testsupport.Must(t, err, "SetRunStatus: %v", err)
	if _, err := ClaimStep(conn, stepID,
		ClaimOptions{Owner: "worker", NowMS: nowMS}); err == nil {
		t.Fatal("a resume alone admitted the claim; the cap had not moved, so " +
			"the condition had not changed")
	}

	// 3. The verb. A cap comfortably above the run's floor.
	cost := expectedCostOf(t, conn, instance)
	result, err := SetRunBudget(conn, runID, cost*10, "estimate was low", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)
	if result.Budget != cost*10 {
		t.Errorf("the cap is %g, want %g", result.Budget, cost*10)
	}

	// B-10: THE STATUS DID NOT MOVE. Raising a cap is not restarting a run —
	// `waiting-human` means a person decides when work resumes, and a verb that
	// un-parked as a side effect would take that decision back.
	//
	// (The run was set `active` in step 2 to prove the dead end, so this
	// re-parks it to observe the property on the transition that matters.)
	err = db.SetRunStatus(conn, runID, model.RunWaitingHuman, "", nowMS)
	testsupport.Must(t, err, "SetRunStatus: %v", err)
	_, err = SetRunBudget(conn, runID, cost*10, "", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)
	run, err = db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Errorf("the run is %s after a budget change; --set must not move the "+
			"status (B-10)", run.Status)
	}

	// 4. Resume, and THE CLAIM NOW COMMITS. B24, closed.
	err = db.SetRunStatus(conn, runID, model.RunActive, "", nowMS)
	testsupport.Must(t, err, "SetRunStatus: %v", err)
	_, err = ClaimStep(conn, stepID,
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "the claim was still refused after the cap was raised: %v — "+
		"this is the loop B24 left open and DKT-29 closes", err)

	// 5. THE TRAIL. `run-paused(budget)` … `run-budget-set(from,to)`, and the
	// budget event carries both numbers so an auditor does not have to
	// reconstruct the old cap from an earlier event.
	page, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var sawPause, sawSet bool
	for _, e := range page.Events {
		switch e.Kind {
		case EventRunPaused:
			sawPause = true
		case EventRunBudgetSet:
			sawSet = true
			for _, want := range []string{`"from"`, `"to"`} {
				if !strings.Contains(string(e.Data), want) {
					t.Errorf("the run-budget-set data %s does not carry %s", e.Data, want)
				}
			}
			if !sawPause {
				t.Error("the budget change is recorded before the pause it answers")
			}
		}
	}
	if !sawPause || !sawSet {
		t.Errorf("the trail is incomplete: pause=%v set=%v — a cap that moved "+
			"with nothing in the log explaining why is the gap operations.md §4 "+
			"warned the manual edit would leave", sawPause, sawSet)
	}
}

// TestBudgetLowerTakesEffectAtTheNextClaim is B-2 and B-12: the verb lowers as
// well as raises, and lowering below what a run has spent stops it.
//
// engine-spec §1 says "raise/lower a live cap". A verb that refused to lower
// would be a verb that half exists — and lowering is the case an operator
// reaches for when a run is spending faster than expected and they want it to
// stop at the next boundary rather than be abandoned.
func TestBudgetLowerTakesEffectAtTheNextClaim(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 1000) // generous
	first := "implement@0"

	_, err := ClaimStep(conn, stepIDByInstance(t, conn, first),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "the first claim was refused under a generous cap: %v", err)

	// Lower the cap below what has already been spent. THE FLOOR DOES NOT MOVE
	// (B-15): raising or lowering a cap changes what a run is ALLOWED to spend,
	// never what it has spent, because the floor is a sum over claim events.
	before, err := GetRunBudget(conn, runID)
	testsupport.Must(t, err, "GetRunBudget: %v", err)
	if before.Floor <= 0 {
		t.Fatalf("the fixture accrued no floor (%g); this test needs one", before.Floor)
	}

	after, err := SetRunBudget(conn, runID, 0.01, "slow down", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)
	if after.Floor != before.Floor {
		t.Errorf("the floor moved from %g to %g when the cap changed; a cap "+
			"change cannot un-spend what was spent (B-15)", before.Floor, after.Floor)
	}

	// And the next claim refuses. (`review` is fanned out in this fixture, so
	// the instance carries its sibling index.)
	if _, err := ClaimStep(conn, stepIDByInstance(t, conn, "review@0#0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS}); err == nil {
		t.Error("a claim was admitted under a cap below the run's spend")
	}
}

// TestBudgetSetIsCASGuarded is B-5 through B-8.
func TestBudgetSetIsCASGuarded(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 10)

	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	stale := run.RowVersion

	// B-7: THE VERSION IS BUMPED WITHOUT `--if-version`.
	//
	// operations.md §4's manual runbook made incrementing `row_version` a step
	// an operator had to remember, warning that otherwise "a concurrent
	// --if-version check will pass against a row that changed underneath it".
	// Making it structural is most of why the verb is better than the edit, so
	// it is asserted on the path where nobody asked for it.
	updated, err := SetRunBudget(conn, runID, 20, "", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)
	if updated.RowVersion <= stale {
		t.Errorf("row_version stayed at %d after a write; a CAS column that does "+
			"not move makes every other verb's --if-version check unsound",
			updated.RowVersion)
	}

	// B-6: the stale precondition now conflicts.
	if _, err := SetRunBudget(conn, runID, 30, "", &stale, nowMS); err == nil {
		t.Error("a stale --if-version was accepted")
	} else if !strings.Contains(err.Error(), "version conflict") {
		t.Errorf("the refusal is %v; want a version conflict", err)
	}

	// And the current one succeeds.
	current := updated.RowVersion
	if _, err := SetRunBudget(conn, runID, 30, "", &current, nowMS); err != nil {
		t.Errorf("the current --if-version was refused: %v", err)
	}
}

// TestBudgetSetRefusals is B-1 and B-3.
func TestBudgetSetRefusals(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 10)

	// B-1: negative. Zero is legal and means unlimited; below zero means
	// nothing at all.
	if _, err := SetRunBudget(conn, runID, -1, "", nil, nowMS); err == nil {
		t.Error("a negative cap was accepted")
	} else if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("a negative cap is %q, want %q", code, CodeValidation)
	}

	// Zero IS accepted, and means unlimited — the meaning the flag has carried
	// since S3. A verb that refused it would make "remove this run's cap"
	// inexpressible.
	if _, err := SetRunBudget(conn, runID, 0, "", nil, nowMS); err != nil {
		t.Errorf("a cap of 0 was refused: %v — 0 means unlimited", err)
	}

	// B-3: a terminal run's cap is history.
	execSQL(t, conn, `UPDATE runs SET status = 'done' WHERE id = ?`, runID)
	if _, err := SetRunBudget(conn, runID, 50, "", nil, nowMS); err == nil {
		t.Error("a finished run's budget was changed")
	} else if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("changing a terminal run's cap is %q, want %q", code, CodeConflict)
	}

	// A run that does not exist.
	if _, err := SetRunBudget(conn, 9999, 50, "", nil, nowMS); err == nil {
		t.Error("a missing run's budget was set")
	} else if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("a missing run is %q, want %q", code, CodeNotFound)
	}
}

// TestBudgetReadShowsWhatEnforcementCompares is B-4, and the property that
// makes the read form worth having: it must show the numbers that DECIDE, not a
// cached approximation of them.
//
// The floor is a SUM over claim events, and `runs.usage_floor` is a cache the
// report reads and enforcement never does (§4.3). A read verb that consulted the
// cache would answer correctly right up until the moment it mattered — when the
// cache and the events disagree — so this test poisons the cache and asserts the
// answer is unchanged, the same technique TestFloorIsNeverReadFromCache uses.
func TestBudgetReadShowsWhatEnforcementCompares(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 100)
	claimInstance(t, conn, "implement@0", nowMS)

	truth := runFloor(t, conn, runID)
	if truth <= 0 {
		t.Fatal("the fixture accrued no floor; this test needs one")
	}

	execSQL(t, conn, `UPDATE runs SET usage_floor = ? WHERE id = ?`, truth+999, runID)

	got, err := GetRunBudget(conn, runID)
	testsupport.Must(t, err, "GetRunBudget: %v", err)
	if got.Floor != truth {
		t.Errorf("the read reported a floor of %g against a true floor of %g: it "+
			"read the cache rather than the events enforcement sums", got.Floor, truth)
	}
	if got.Spend != truth {
		t.Errorf("spend = %g, want %g — max(reported, floor) with nothing reported",
			got.Spend, truth)
	}
	if got.Budget != 100 {
		t.Errorf("the cap reads %g, want 100", got.Budget)
	}
}

// TestBudgetVerbWritesOnlyWhatItSays is the blast radius: `--set` changes the
// cap and writes an event, and touches nothing else.
//
// It matters because this verb is reached for when a run is already in trouble.
// An operator raising a cap on a breached run must be able to predict what the
// command does — and "it also cleared the breach reason" or "it also reaped
// something" would be discovered at the worst possible moment.
func TestBudgetVerbWritesOnlyWhatItSays(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0.01)
	instance := "implement@0"

	if _, err := ClaimStep(conn, stepIDByInstance(t, conn, instance),
		ClaimOptions{Owner: "worker", NowMS: nowMS}); err == nil {
		t.Fatal("the claim past the cap was admitted")
	}

	facts, err := db.RunBudgetFactsFor(conn, runID)
	testsupport.Must(t, err, "RunBudgetFactsFor: %v", err)
	breachBefore := facts.BreachReason

	before := allTableCounts(t, conn)

	_, err = SetRunBudget(conn, runID, 500, "raised", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)

	after := allTableCounts(t, conn)
	for table, n := range before {
		if table == "events" {
			if after[table] != n+1 {
				t.Errorf("events went from %d to %d; --set writes exactly one event",
					n, after[table])
			}
			continue
		}
		if after[table] != n {
			t.Errorf("%s went from %d rows to %d: --set reached beyond the run row "+
				"and its event", table, n, after[table])
		}
	}

	// DKT-80 (overturning B-13): a cap change that RESOLVES the breach clears
	// the breach record. The history of the breach lives in the `run-paused`
	// and `run-budget-set` events; the row states what is true now, and after
	// this raise "spend reached the cap" is no longer it.
	if breachBefore == "" {
		t.Fatal("no breach was recorded; the fixture changed under this test")
	}
	facts, err = db.RunBudgetFactsFor(conn, runID)
	testsupport.Must(t, err, "RunBudgetFactsFor: %v", err)
	if facts.BreachReason != "" {
		t.Errorf("the breach reason survived a cap raise that resolved it: %q "+
			"(DKT-80)", facts.BreachReason)
	}
}

// TestBudgetChangeClearsResolvedBreach is DKT-80 in full: after a breach, a
// cap change that resolves it clears `breach_reason`, rewrites the row's
// parked `reason` to a currently-true statement, and records the clearing in
// the same `run-budget-set` event as the change that caused it.
func TestBudgetChangeClearsResolvedBreach(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0.01)
	instance := "implement@0"

	if _, err := ClaimStep(conn, stepIDByInstance(t, conn, instance),
		ClaimOptions{Owner: "worker", NowMS: nowMS}); err == nil {
		t.Fatal("the claim past the cap was admitted")
	}
	facts, err := db.RunBudgetFactsFor(conn, runID)
	testsupport.Must(t, err, "RunBudgetFactsFor: %v", err)
	breach := facts.BreachReason
	if breach == "" {
		t.Fatal("no breach recorded; the fixture changed under this test")
	}

	cost := expectedCostOf(t, conn, instance)
	_, err = SetRunBudget(conn, runID, cost*10, "estimate was low", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)

	// The breach record is gone.
	facts, err = db.RunBudgetFactsFor(conn, runID)
	testsupport.Must(t, err, "RunBudgetFactsFor: %v", err)
	if facts.BreachReason != "" {
		t.Errorf("breach_reason survived the raise: %q", facts.BreachReason)
	}

	// The row's reason no longer asserts the stale breach; it records the
	// change and the way forward instead.
	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Reason == breach {
		t.Errorf("run.reason still reads the stale breach: %q", run.Reason)
	}
	if !strings.Contains(run.Reason, "cap changed") ||
		!strings.Contains(run.Reason, "run resume") {
		t.Errorf("run.reason = %q, want the cap change and the resume hint", run.Reason)
	}
	if !strings.Contains(run.Reason, breach) {
		t.Errorf("run.reason = %q dropped the original breach text; why the run "+
			"stopped should remain readable from the row", run.Reason)
	}

	// The clearing rides in the run-budget-set event's data.
	page, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var cleared string
	for _, e := range page.Events {
		if e.Kind != EventRunBudgetSet {
			continue
		}
		var data struct {
			BreachCleared string `json:"breach_cleared"`
		}
		testsupport.Must(t, json.Unmarshal(e.Data, &data), "decoding data")
		cleared = data.BreachCleared
	}
	if cleared != breach {
		t.Errorf("run-budget-set data.breach_cleared = %q, want the retired "+
			"breach reason %q", cleared, breach)
	}
}

// TestBudgetRaiseWithNoStandingBreachClearsStaleReason is DKT-47's second
// symptom: a raise that resolves nothing currently breached must not leave a
// PREVIOUS raise's "cap changed from X to Y" sentence sitting on the row,
// naming numbers that are no longer the ones in force.
//
// `breachCleared` (run_budget.go) only fires when `facts.BreachReason != ""`,
// which is exactly right for `breach_reason` itself — there is no standing
// breach to clear a SECOND time. But the decorative `reason` text the FIRST
// raise wrote survives untouched, because nothing after it ever revisits a
// row with no standing breach.
func TestBudgetRaiseWithNoStandingBreachClearsStaleReason(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0.01)
	instance := "implement@0"

	if _, err := ClaimStep(conn, stepIDByInstance(t, conn, instance),
		ClaimOptions{Owner: "worker", NowMS: nowMS}); err == nil {
		t.Fatal("the claim past the cap was admitted")
	}
	facts, err := db.RunBudgetFactsFor(conn, runID)
	testsupport.Must(t, err, "RunBudgetFactsFor: %v", err)
	if facts.BreachReason == "" {
		t.Fatal("no breach recorded; the fixture changed under this test")
	}

	cost := expectedCostOf(t, conn, instance)

	// First raise: resolves the standing breach, writes the "cap changed
	// from X to Y" sentence DKT-80 added — this is the CORRECT behavior
	// this test's premise depends on.
	_, err = SetRunBudget(conn, runID, cost*10, "estimate was low", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)
	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	firstReason := run.Reason
	if !strings.Contains(firstReason, "cap changed") {
		t.Fatalf("premise: run.reason = %q after the resolving raise, want it "+
			"to carry the cap-changed sentence", firstReason)
	}

	// The claim refusal parked the run: it must still be waiting-human here,
	// or the premise for what follows (a still-parked run) does not hold.
	run, err = db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Fatalf("premise: run.status = %q after the resolving raise, want %q",
			run.Status, model.RunWaitingHuman)
	}

	// Second raise: nothing is CURRENTLY breached (breach_reason is already
	// NULL), so the guard that fires on a resolving raise never runs. The
	// stale sentence from the FIRST raise must not survive naming numbers
	// that are no longer in force — and the run REMAINS PARKED throughout
	// (SetRunBudget never touches status, B-10), so the property to prove
	// is not "the reason changed" (a mutant can satisfy that by writing a
	// DIFFERENT stale sentence) but "the reason names the cap now in force
	// and still tells a waiting-human operator how to resume".
	secondCost := cost * 20
	_, err = SetRunBudget(conn, runID, secondCost, "raised again", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)
	run, err = db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)

	if run.Status != model.RunWaitingHuman {
		t.Errorf("run.status = %q after the second raise, want %q — SetRunBudget "+
			"never resumes a parked run (B-10)", run.Status, model.RunWaitingHuman)
	}
	if run.Reason == firstReason {
		t.Errorf("run.reason is still %q after an unrelated later raise; the "+
			"cap it names is stale (DKT-47)", run.Reason)
	}
	want := fmt.Sprintf("%g", secondCost)
	if !strings.Contains(run.Reason, want) {
		t.Errorf("run.reason = %q, want it to name the cap now in force (%s)",
			run.Reason, want)
	}
	if !strings.Contains(run.Reason, "cap changed") ||
		!strings.Contains(run.Reason, "run resume") {
		t.Errorf("run.reason = %q, want a still-parked run to keep both the "+
			"cap-changed sentence and the resume hint, not go blank", run.Reason)
	}
}

// TestBudgetRaiseNeverClearsAnOperatorsOwnPauseReason is DKT-47's regression:
// the guard at run_budget.go:122 that retires a PREVIOUS raise's stale "cap
// changed from X to Y" sentence must never fire on a reason it did not write
// itself. An operator's own pause reason (e.g. from `run pause`) carries no
// budgetChangeReasonPrefix, so it must survive a later raise untouched — even
// one that resolves nothing currently breached.
func TestBudgetRaiseNeverClearsAnOperatorsOwnPauseReason(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 1000) // generous: no breach in play

	operatorReason := "paused by operator for a scheduled maintenance window"
	execSQL(t, conn, `UPDATE runs SET status = 'waiting-human', reason = ? WHERE id = ?`,
		operatorReason, runID)

	facts, err := db.RunBudgetFactsFor(conn, runID)
	testsupport.Must(t, err, "RunBudgetFactsFor: %v", err)
	if facts.BreachReason != "" {
		t.Fatalf("premise: a breach is standing (%q); this test needs none", facts.BreachReason)
	}

	_, err = SetRunBudget(conn, runID, 2000, "unrelated raise", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)

	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Fatalf("run.status = %q after the raise, want %q — SetRunBudget never "+
			"resumes a parked run (B-10)", run.Status, model.RunWaitingHuman)
	}
	if run.Reason != operatorReason {
		t.Errorf("run.reason = %q after an unrelated raise, want the operator's "+
			"own pause reason %q untouched — only this package's own stale "+
			"cap-changed sentence may be retired", run.Reason, operatorReason)
	}
}
