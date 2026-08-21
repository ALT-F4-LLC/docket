package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `next` must offer a CLAIMABLE-NOW cohort that can be claimed AS A SET.
//
// R4 asks whether a step conflicts with a `claimed` or `running` holder, and
// says nothing about two steps that are both merely READY — neither holds
// anything yet. So an offer could contain rows that exclude each other: a
// dispatcher spawned the whole set, the first claim won, and every other agent
// died on CONFLICT having done nothing. RUN-3 lost 21 of 53 spawns this way.
//
// The READY rows are narrowed to a prefix-greedy disjoint subset. Since the
// staged closure (lookahead.go) the rest of the cluster is not merely held
// back for a later round: it rides in the SAME offer as `staged` rows at
// later stages, which never run concurrently with the cohort that excluded
// them — so these tests assert on the ready cohort (readyOffered) and on the
// per-stage disjointness the staged rows must keep.

// readyOffered filters an offer to its claimable-now rows — the set the
// pre-closure tests asserted on, and still the set a dispatcher may spawn
// immediately.
func readyOffered(rows []model.StepRow) []model.StepRow {
	var out []model.StepRow
	for _, r := range rows {
		if r.Status == db.StepReady {
			out = append(out, r)
		}
	}
	return out
}

// TestNextOffersOnlyClaimableSet is the regression proper: two issues sharing
// a scope, both ready, must not both be claimable at once. The loser is not
// dropped from the offer — it rides `staged` at a later stage — but exactly
// one row may say `ready`, and no two rows of one stage may share the scope.
func TestNextOffersOnlyClaimableSet(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	for _, id := range []int{a, b} {
		err := db.SetIssueScopeGlobs(conn, id, `["internal/engine/**"]`)
		testsupport.Must(t, err, "setting scope: %v", err)
	}
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	rows := offeredRows(t, conn, run.ID)
	ready := readyOffered(rows)
	if len(ready) != 1 {
		var got []string
		for _, r := range ready {
			got = append(got, r.Step+"("+r.Issue+")")
		}
		t.Fatalf("offered %d ready rows %v; the two issues share "+
			"internal/engine/** so only ONE can be claimed now", len(ready), got)
	}
	// The staged closure must keep the same exclusion PER STAGE: both issues'
	// tree-holding rows share one scope, so no stage may carry both.
	implementByStage := make(map[int][]string)
	for _, r := range rows {
		if r.Instance == "implement@0" {
			implementByStage[r.Stage] = append(implementByStage[r.Stage], r.Issue)
		}
	}
	for stage, issues := range implementByStage {
		if len(issues) > 1 {
			t.Errorf("stage %d carries %v together; the shared scope must "+
				"serialize the two issues across stages", stage, issues)
		}
	}
}

// TestNextStillOffersDisjointScopesTogether is the other half, and the reason
// this is a filter rather than a serialization: work that genuinely cannot
// collide must still run in parallel. A fix that offered one row at a time
// would pass the test above and destroy the system's throughput.
func TestNextStillOffersDisjointScopesTogether(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	err := db.SetIssueScopeGlobs(conn, a, `["internal/a/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	err = db.SetIssueScopeGlobs(conn, b, `["internal/b/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, a, b)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if ready := readyOffered(offeredRows(t, conn, run.ID)); len(ready) != 2 {
		t.Fatalf("offered %d ready rows, want both: internal/a/** and "+
			"internal/b/** cannot collide", len(ready))
	}
}

// TestNextOffersScopelessIssuesTogether keeps S1 intact through the filter: an
// issue that declares no scope never excludes and is never excluded, which is
// the overwhelmingly common case and the one every pre-existing issue is in.
func TestNextOffersScopelessIssuesTogether(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if ready := readyOffered(offeredRows(t, conn, run.ID)); len(ready) != 2 {
		t.Fatalf("offered %d ready rows, want both: a scope-less issue never "+
			"excludes (S1)", len(ready))
	}
}

// TestClaimablePrefixIsFair pins the property that keeps a loser from starving.
//
// The filter is greedy over the caller's order, and SortSteps has already made
// that order total (priority, then age, then id). So the winner of a
// conflicting cluster is deterministic and is always the oldest — which means a
// step that lost stays at the front of the queue and takes the floor as soon as
// the winner releases it. RUN-3's review fanout went 0-for-5 against an
// unordered race; the ordering is what forecloses that.
func TestClaimablePrefixIsFair(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	for _, id := range []int{a, b} {
		err := db.SetIssueScopeGlobs(conn, id, `["internal/engine/**"]`)
		testsupport.Must(t, err, "setting scope: %v", err)
	}
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// Asked repeatedly with the state unchanged, the same step wins every
	// time. An arbitrary choice would schedule just as correctly and starve
	// unpredictably.
	first := readyOffered(offeredRows(t, conn, run.ID))
	if len(first) != 1 {
		t.Fatalf("offered %d ready rows, want 1", len(first))
	}
	for i := 0; i < 5; i++ {
		again := readyOffered(offeredRows(t, conn, run.ID))
		if len(again) != 1 || again[0].Step != first[0].Step {
			t.Fatalf("offer %d chose %v, want the same winner %s every time",
				i, again, first[0].Step)
		}
	}
}

// ---------------------------------------------------------------------------
// DKT-23 — the offer's CLASS half. The scope tests above prove the set is
// claimable against R4; these prove it against R5. RUN-2's dispatch relay
// spawned one executor per offered row, five write-class rows rode out over
// one free slot, and four spawns paid a full worktree bootstrap to die on "no
// concurrency headroom in its class".
// ---------------------------------------------------------------------------

// TestNextCapsSameClassOfferAtHeadroom is the regression proper: with
// `[limits]` bounding a class at 1 and TWO ready steps of that class (one per
// issue), the offer carries exactly ONE — while the unbounded class's rows all
// still ride, because a cap on one class must not serialize the others.
func TestNextCapsSameClassOfferAtHeadroom(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	rows := offeredRows(t, conn, run.ID)
	byClass := make(map[string]int)
	writesByStage := make(map[int]int)
	for _, r := range readyOffered(rows) {
		byClass[r.Class]++
	}
	for _, r := range rows {
		if r.Class == "write" {
			writesByStage[r.Stage]++
		}
	}

	// Both issues' `one@0` are ready and both are class "write" (max = 1); the
	// claimable cohort must ration them to the one slot.
	if byClass["write"] != 1 {
		t.Errorf("offered %d ready write-class rows against max 1; a "+
			"dispatcher spawns one executor per row and every extra spawn "+
			"bounces at claim (DKT-23): %v", byClass["write"], rows)
	}
	// The unbounded read fan-out is untouched: two issues x read-a/read-b.
	if byClass["read"] != 4 {
		t.Errorf("offered %d ready read-class rows, want all 4; rationing a "+
			"bounded class must not withhold an unbounded one", byClass["read"])
	}
	// The rationed write rows ride STAGED, and the cap holds per stage:
	// stages run sequentially, so one slot means one write row per stage.
	for stage, n := range writesByStage {
		if n > 1 {
			t.Errorf("stage %d carries %d write-class rows against max 1; the "+
				"cap must hold within every stage", stage, n)
		}
	}
}

// TestHeldBackClassRowIsOfferedOnceTheSlotFrees is the liveness half: a row
// held back by the cap is STILL READY — nothing was written — and flows the
// moment the class has room again.
func TestHeldBackClassRowIsOfferedOnceTheSlotFrees(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	rows := offeredRows(t, conn, run.ID)
	var winner string
	for _, r := range readyOffered(rows) {
		if r.Class == "write" {
			winner = r.Instance
		}
	}
	if winner == "" {
		t.Fatal("premise: no ready write-class row offered")
	}
	claimInstance(t, conn, winner, nowMS)

	// While the slot is held, the offer carries NO write-class row at all —
	// ready or staged. A step blocked on a LIVE claim is blocked on the world
	// outside the offer, which the closure deliberately never stages against.
	for _, r := range offeredRows(t, conn, run.ID) {
		if r.Class == "write" {
			t.Errorf("%s offered while the class's one slot is claimed", r.Instance)
		}
	}

	// The claimant finishes (usage recorded — the ordinary complete path), and
	// the very next offer carries a claimable write-class row again.
	execSQL(t, conn,
		`UPDATE steps SET status = ?, usage_recorded = 1
		  WHERE run_id = ? AND instance = ? AND status = ?`,
		db.StepDone, run.ID, winner, db.StepClaimed)
	found := 0
	for _, r := range readyOffered(offeredRows(t, conn, run.ID)) {
		if r.Class == "write" {
			found++
		}
	}
	if found != 1 {
		t.Errorf("offered %d ready write-class rows after the slot freed, "+
			"want 1 — a held-back row must flow as soon as headroom exists", found)
	}
}

// parallelWritersSrc bounds a class at 1 with TWO independent steps in it, so a
// single issue produces two ready same-class steps — DKT-23's minimal repro,
// and the shape the claim-refusal test needs (no `after` edge to refuse on
// first).
const parallelWritersSrc = `
[pipeline]
name = "parallel-writers"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 1 }

[[step]]
name = "one"
executor = "w"
class = "write"
emits = "out"
after = []

[[step]]
name = "two"
executor = "w"
class = "write"
emits = "out"
after = []
`

// TestHeadroomRefusalNamesTheNumbers is DKT-23's second ask. The bounced
// claimant heard "no concurrency headroom in its class" and could answer none
// of the questions that follow — what is the cap, how full is the class, who
// set the bound — without grepping the corpus. The refusal now carries all
// three.
func TestHeadroomRefusalNamesTheNumbers(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(parallelWritersSrc), "parallel-writers.toml")

	issue := createIssue(t, conn, "issue", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	claimInstance(t, conn, "one@0", nowMS)

	_, err = ClaimStep(conn, stepIDByInstance(t, conn, "two@0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	if err == nil {
		t.Fatal("claiming a second write-class step against max 1 succeeded")
	}
	for _, want := range []string{
		"no concurrency headroom",
		`class "write" has 1 claimed/running`,
		"against max 1",
		"set by [limits] in parallel-writers@1",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the headroom refusal %q does not name %q; the claimant "+
				"needs the cap, the occupancy, and the source without a corpus "+
				"grep (DKT-23)", err.Error(), want)
		}
	}
}

// TestDispatchOpenCapsSameClassOffer mirrors the cap through `dispatch open`,
// exactly as TestDispatchOpenOffersOnlyClaimableSet mirrors the scope half:
// readyRows is the manifest's shared tail and must narrow identically, or the
// manifest offers a row `next` would have withheld.
func TestDispatchOpenCapsSameClassOffer(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(writeLimitedSrc), "serialized.toml")

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	m := openDispatch(t, conn, run.ID, 0, nowMS)
	writes := 0
	writesByStage := make(map[int]int)
	for _, r := range m.Rows {
		if r.Class != "write" {
			continue
		}
		writesByStage[r.Stage]++
		if r.Status == db.StepReady {
			writes++
		}
	}
	if writes != 1 {
		t.Errorf("manifest carries %d ready write-class rows against max 1 "+
			"(DKT-23); the wave spawns every ready row it is handed", writes)
	}
	for stage, n := range writesByStage {
		if n > 1 {
			t.Errorf("manifest stage %d carries %d write-class rows against "+
				"max 1; the cap must hold within every stage", stage, n)
		}
	}
}

// ---------------------------------------------------------------------------
// DKT-47 — the offer's BUDGET half. The scope and class tests above prove the
// set is claimable against R4 and R5; these prove it against R7/B14. RUN-2's
// DISPATCH-20 offered nine executor rows that each fit the remaining headroom
// alone but summed past it together: the first claim advanced the floor, the
// claim-side projection paused the run, and every other spawned executor died
// on "run is paused".
// ---------------------------------------------------------------------------

// TestNextCapsOfferAtBudgetHeadroom is the regression proper: two scope-less,
// same-class ready rows whose costs each fit the remaining headroom alone but
// not together must not both be offered. The winning row is pinned too:
// SortSteps' own ordering (priority, age, id) says issue A's row, minted
// first, wins — an accumulator that admitted by any other order would still
// pass a count-only assertion.
func TestNextCapsOfferAtBudgetHeadroom(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// Each ready row is "implement" at the fixture's own expected_cost. A cap
	// of 1.5x that cost admits one alone (cost <= cap) but not both
	// (2*cost > cap) — derived from the fixture rather than restated, so a
	// changed fixture cost fails loudly instead of silently passing.
	cost := expectedCostOf(t, conn, "implement@0")
	cap := cost * 1.5
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cap, run.ID)

	rows := offeredRows(t, conn, run.ID)
	ready := readyOffered(rows)
	if len(ready) != 1 {
		var got []string
		for _, r := range ready {
			got = append(got, r.Step+"("+r.Issue+")")
		}
		t.Fatalf("offered %d ready rows %v against cap %g with two %g-cost "+
			"rows; the offer's summed cost must fit remaining headroom, not "+
			"just each row alone", len(ready), got, cap, cost)
	}
	if want := model.FormatID(a); ready[0].Issue != want {
		t.Errorf("the surviving row is %s, want %s's — the oldest row in a "+
			"conflicting cluster always wins (fairness), and budget is now a "+
			"third way to lose it", ready[0].Issue, want)
	}
	// The budget cap is WHOLE-OFFER: staged rows spend too if the wave
	// succeeds, so everything offered — winner and closure alike — must sum
	// inside the cap.
	var sum float64
	for _, r := range rows {
		sum += r.ExpectedCost
	}
	if sum > cap {
		t.Errorf("the offer's summed expected cost %g exceeds cap %g; the "+
			"staged closure must ration against the same budget the ready "+
			"cohort does", sum, cap)
	}
}

// TestHeldBackBudgetRowIsOfferedOnceHeadroomFrees is the liveness half: a row
// rationed out by the budget accumulator is STILL READY — nothing was
// written — and flows once the floor moves and headroom reopens for it. The
// raise goes through SetRunBudget, the actual raise path (row_version bump,
// run-budget-set event), rather than a raw UPDATE that never exercises it.
func TestHeldBackBudgetRowIsOfferedOnceHeadroomFrees(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	cost := expectedCostOf(t, conn, "implement@0")
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cost*1.5, run.ID)

	if got := len(readyOffered(offeredRows(t, conn, run.ID))); got != 1 {
		t.Fatalf("premise: offered %d ready rows, want 1", got)
	}

	// Raise the cap through the real raise path: both rows now fit inside
	// remaining headroom together.
	_, err = SetRunBudget(conn, run.ID, cost*2.5, "raised", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)
	if got := len(readyOffered(offeredRows(t, conn, run.ID))); got != 2 {
		t.Errorf("offered %d ready rows once the cap covers both %g-cost rows "+
			"together, want 2 — the held-back row was still ready", got, cost)
	}
}

// budgetOrderTaskSrc and budgetOrderQuickSrc are DKT-49-C1's minimal repro
// fixture: two workflows, matched by different issue kinds, giving each
// issue a DIFFERENT expected_cost while still letting scopes be set
// independently of the workflow through SetIssueScopeGlobs.
const budgetOrderTaskSrc = `
[pipeline]
name = "budget-order-task"
version = 1

[match]
kind = ["task"]

[[step]]
name = "one"
executor = "w"
emits = "out"
after = []
expected_cost = 1.2
`

const budgetOrderQuickSrc = `
[pipeline]
name = "budget-order-quick"
version = 1

[match]
kind = ["quick"]

[[step]]
name = "one"
executor = "w"
emits = "out"
after = []
expected_cost = 0.5
`

// TestOfferBudgetRejectionDoesNotLeaveAScopeGrantBehind is DKT-49-C1: the
// mixed-constraint regression. Scope's grant used to run BEFORE the budget
// check inside one admission pass, so a row rejected on budget still left
// its scope GRANTED — excluding a later, same-scope row that fit budget on
// its own, even though nothing had actually claimed the scope it was
// excluded against.
//
// Three issues, sorted oldest first: A (scope-less, cost 1.2), B (scope X,
// cost 1.2), C (scope X too, cost 0.5) — the exact shape one of DKT-49's
// three private-copy fixtures used. Cap 2: A is admitted (1.2 <= 2). B is
// then REJECTED on budget (1.2+1.2 = 2.4 > 2) — under the bug, admitScope
// had already granted B's issue scope X as a side effect of B merely
// PASSING the scope check, before budget ever rejected it, so C's own scope
// check saw X already held and excluded C too, though C's cost (0.5) fits
// the remaining headroom (1.2+0.5 = 1.7 <= 2) on its own. Fixed: B's
// rejection grants nothing, so C is offered.
func TestOfferBudgetRejectionDoesNotLeaveAScopeGrantBehind(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(budgetOrderTaskSrc), "budget-order-task.toml")
	registerSource(t, conn, []byte(budgetOrderQuickSrc), "budget-order-quick.toml")

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	c := createIssue(t, conn, "issue C", "body", "quick", nil)
	err := db.SetIssueScopeGlobs(conn, b, `["internal/shared/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	err = db.SetIssueScopeGlobs(conn, c, `["internal/shared/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, a, b, c)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	execSQL(t, conn, `UPDATE runs SET budget = 2 WHERE id = ?`, run.ID)

	rows := offeredRows(t, conn, run.ID)
	byIssue := make(map[string]bool)
	for _, r := range rows {
		byIssue[r.Issue] = true
	}
	refA, refB, refC := model.FormatID(a), model.FormatID(b), model.FormatID(c)
	if !byIssue[refA] || !byIssue[refC] || byIssue[refB] || len(rows) != 2 {
		var got []string
		for _, r := range rows {
			got = append(got, r.Step+"("+r.Issue+")")
		}
		t.Fatalf("offered %v against cap 2; want %s (1.2) and %s (0.5) "+
			"together and %s (1.2, budget-rejected) excluded — a budget "+
			"rejection must not leave a scope grant behind that excludes a "+
			"later row that fits (DKT-49-C1)", got, refA, refC, refB)
	}
}

// TestNextCapsOfferAtBudgetHeadroomWithNonZeroFloor is DKT-49-C2: the
// accumulator's own spend() term — the snapshot's floor, from a claim this
// offer never made — exercised at a NON-ZERO value. Every other budget test
// activates a fresh run and never claims, so spend() is 0 throughout and an
// offerBudget that compared only admittedCost+cost against cap (dropping the
// floor entirely) would still pass them.
//
// Three same-cost ready rows; one is claimed first (floor becomes non-zero),
// leaving two ready. The cap fits the floor plus ONE more row's cost, not
// the floor plus two — an accumulator ignoring the floor would offer both.
func TestNextCapsOfferAtBudgetHeadroomWithNonZeroFloor(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	c := createIssue(t, conn, "issue C", "body", "task", nil)
	run := startRun(t, conn, a, b, c)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	cost := expectedCostOf(t, conn, "implement@0")
	// Claim one issue's implement@0 (whichever the fixture assigns the
	// lowest step id, i.e. the first created) to give the floor a real,
	// non-zero value before the cap is ever set.
	claimed := claimInstance(t, conn, "implement@0", nowMS)
	claimedIssue := stepIssueID(t, conn, claimed.Step)

	if got := runFloor(t, conn, run.ID); got != cost {
		t.Fatalf("premise: floor = %g after one claim, want %g", got, cost)
	}

	// floor (cost) + one more row's cost fits; floor + two more does not.
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cost*2.5, run.ID)

	rows := offeredRows(t, conn, run.ID)
	for _, r := range rows {
		// The claimed issue can contribute NOTHING to this offer, staged rows
		// included: its implement@0 is claimed — non-terminal and outside the
		// offer — so nothing downstream of it is stageable either.
		if stepIssueID(t, conn, r.Step) == claimedIssue {
			t.Errorf("the already-claimed issue's %s is offered again", r.Instance)
		}
	}
	ready := readyOffered(rows)
	if len(ready) != 1 {
		var got []string
		for _, r := range ready {
			got = append(got, r.Step+"("+r.Issue+")")
		}
		t.Fatalf("offered %d ready rows %v against cap %g with floor %g "+
			"already spent and two more %g-cost rows ready; the accumulator "+
			"must add to the FLOOR, not just to itself, or it never catches "+
			"the RUN-2 shape (floor 24.2 of cap 25) where the floor — not the "+
			"offer's own sum — is most of what is already spent",
			len(ready), got, cost*2.5, cost, cost)
	}
	// And the closure obeys the same floor: whatever rode along staged must
	// still sum, with the floor, inside the cap.
	var sum float64
	for _, r := range rows {
		sum += r.ExpectedCost
	}
	if sum+cost > cost*2.5 {
		t.Errorf("offer cost %g plus floor %g exceeds cap %g; the staged "+
			"closure must count the floor exactly as the ready cohort does",
			sum, cost, cost*2.5)
	}
}

// TestDispatchOpenCapsBudgetOffer mirrors the budget cap through `dispatch
// open`, exactly as TestDispatchOpenCapsSameClassOffer mirrors the class
// half (DKT-49-C11): ClaimablePrefix has two independent call sites,
// next.go and dispatch.go, and the reported incident (DISPATCH-20) was a
// dispatch MANIFEST carrying rows next would have withheld — so the budget
// half needs the same mirror the class half already has.
func TestDispatchOpenCapsBudgetOffer(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	cost := expectedCostOf(t, conn, "implement@0")
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, cost*1.5, run.ID)

	m := openDispatch(t, conn, run.ID, 0, nowMS)
	ready := readyOffered(m.Rows)
	if len(ready) != 1 {
		var got []string
		for _, r := range ready {
			got = append(got, r.Step+"("+r.Issue+")")
		}
		t.Errorf("manifest carries %d ready rows %v against cap %g with two "+
			"%g-cost rows (DKT-47); the wave spawns every ready row it is "+
			"handed", len(ready), got, cost*1.5, cost)
	}
	var sum float64
	for _, r := range m.Rows {
		sum += r.ExpectedCost
	}
	if sum > cost*1.5 {
		t.Errorf("the manifest's summed expected cost %g exceeds cap %g; the "+
			"staged closure must ration against the same budget the ready "+
			"cohort does", sum, cost*1.5)
	}
}

// TestBudgetRationedJudgeIsNeverOfferedWithoutItsFixer is DKT-49-C14: the
// eviction interaction, budget's version of DKT-26/
// TestEvictedFixerTakesItsDependentsFromTheOffer. Budget is now a third
// reason a row can be rationed out of the offer, and the invariant to pin
// is the same one the class half already proves: a re-review judge is never
// offered unless its fixer (the tree it re-reviews) is offered alongside it
// — never an UNSTAGED judge free to review a tree its excluded fixer never
// touched.
//
// Unlike a class slot (a fixed COUNT), budget is a fungible SUM: evicting
// four rationed-out judges frees enough of it that the fixer, cheaper than
// their combined cost in this fixture, is re-admitted on the very next
// fixed-point pass — a legitimate, different-shaped resolution from the
// class half's "stays excluded, evicts its judges" outcome, and exactly why
// the invariant (never judges without their fixer), not a specific offered
// set, is what this test pins.
func TestBudgetRationedJudgeIsNeverOfferedWithoutItsFixer(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	issueB := createIssue(t, conn, "issue b", "body", "task", nil)
	run := startRun(t, conn, issueA, issueB)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	// ISSUE A into its fix loop; ISSUE B left at implement@0 — ready, and
	// OLDER than A's loop-1 chain (created at activation, where the loop's
	// steps are minted by the loop entry), so B is admitted first in every
	// pass exactly as DKT-19's implement did live for the class half.
	driveIssueToLoopReentry(t, conn, e, issueA)

	// A cap that fits the floor A's own loop already accrued, plus B's
	// implement@0, plus ALL FOUR of A's review@1 judges (individually cheap
	// enough to fit on their own budget check, since review@1 sorts BEFORE
	// fix@1 by id — the judges are minted before the fixer at loop entry)
	// — but not that plus A's fix@1 too. A single pass with no eviction
	// admits the judges (each fits) while rejecting the fixer, offering an
	// UNSTAGED re-review; this is the exact shape TestDispatchOpenCapsBudget
	// Offer and friends never exercise, since they carry no loop.
	floor := runFloor(t, conn, run.ID)
	implCost := expectedCostOf(t, conn, "implement@0")
	reviewCost := expectedCostOf(t, conn, "review@1#0")
	fixCost := expectedCostOf(t, conn, "fix@1")
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`,
		floor+implCost+4*reviewCost+fixCost/2, run.ID)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	var sawBImplement, sawFixer bool
	var sawJudge []string
	for _, row := range answer.Steps {
		issue := stepIssueID(t, conn, row.Step)
		switch {
		case issue == issueB && row.Instance == "implement@0":
			sawBImplement = true
		case issue == issueA && row.Instance == "fix@1":
			sawFixer = true
		case issue == issueA && strings.HasPrefix(row.Instance, "review@1"):
			sawJudge = append(sawJudge, row.Instance)
		}
	}
	if !sawBImplement {
		t.Fatalf("premise: issue B's implement@0 must hold the budget, got %v",
			instancesIn(answer))
	}
	if len(sawJudge) > 0 && !sawFixer {
		t.Errorf("%v offered with fix@1 rationed out on budget — an unstaged "+
			"re-review of the tree fix@1 is about to rewrite (DKT-49-C14)",
			sawJudge)
	}
}
