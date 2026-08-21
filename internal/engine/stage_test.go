package engine

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DEPENDENCY-STAGED READY SETS — the acceptance criteria, one test each.

// stageOf returns the stage of a named instance in an answer, and whether it
// was offered at all.
func stageOf(answer *ReadySteps, instance string) (int, bool) {
	for _, row := range answer.Steps {
		if row.Instance == instance {
			return row.Stage, true
		}
	}
	return 0, false
}

// stepIDIn resolves an instance WITHIN a given issue.
//
// stepIDByInstance resolves ties by lowest id, which is fine for a single-issue
// run and wrong for a multi-issue one: two issues expanded from one workflow
// carry identical instance strings, so the bare lookup silently addresses
// whichever issue expanded first.
func stepIDIn(t *testing.T, conn *sql.DB, issueID int, instance string) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE issue_id = ? AND instance = ?`,
		issueID, instance,
	).Scan(&id)
	testsupport.Must(t, err, "finding %s in issue %d: %v", instance, issueID, err)
	return id
}

// issueIDOfStep reads the issue a rendered row's step belongs to.
func stepIssueID(t *testing.T, conn *sql.DB, stepID string) int {
	t.Helper()
	id, err := model.ParseStepID(stepID)
	testsupport.Must(t, err, "parsing %q: %v", stepID, err)
	var issueID int
	err = conn.QueryRow(
		`SELECT issue_id FROM steps WHERE id = ?`, id).Scan(&issueID)
	testsupport.Must(t, err, "reading issue of %s: %v", stepID, err)
	return issueID
}

// completeInIssue is claimAndComplete, scoped to one issue.
func completeInIssue(
	t *testing.T, conn *sql.DB, e *Engine, issueID int,
	instance, artifact, payload string,
) {
	t.Helper()
	stepID := stepIDIn(t, conn, issueID, instance)
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim %s: %v", instance, err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte(artifact),
		Payload: []byte(payload), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete %s: %v", instance, err)
}

// driveActionInIssue is driveAction, scoped to one issue.
func driveActionInIssue(
	t *testing.T, conn *sql.DB, e *Engine, issueID int, instance string,
) {
	t.Helper()
	err := e.RunActionStep(conn, stepIDIn(t, conn, issueID, instance), nowMS)
	testsupport.Must(t, err, "running %s: %v", instance, err)
}

// driveIssueToLoopReentry drives one issue of an activated fixture run to its
// loop re-entry: implement, the four judges, synthesize, reconcile, and a
// verify completed against the unmet threshold — leaving fix@1 minted and the
// review@1 judges restored, the shape every loop-staging test starts from.
func driveIssueToLoopReentry(t *testing.T, conn *sql.DB, e *Engine, issueID int) {
	t.Helper()
	completeInIssue(t, conn, e, issueID, "implement@0", "the change summary", "")
	for i := range 4 {
		completeInIssue(t, conn, e, issueID,
			fmt.Sprintf("review@0#%d", i), "findings", "")
	}
	completeInIssue(t, conn, e, issueID, "synthesize@0", "the synthesis", "")
	driveActionInIssue(t, conn, e, issueID, "reconcile@0")
	completeInIssue(t, conn, e, issueID, "verify@0", "the ac report", unmetPayload)
}

// containsKey reports whether a marshaled row carries a top-level key —
// decoded rather than substring-matched, so a value that happens to contain the
// word cannot produce a false positive.
func containsKey(t *testing.T, raw []byte, key string) bool {
	t.Helper()
	var fields map[string]any
	err := json.Unmarshal(raw, &fields)
	testsupport.Must(t, err, "unmarshal %s: %v", raw, err)
	_, ok := fields[key]
	return ok
}

// TestFixerIsStagedBeforeItsReReviewJudges is AC2: the engine POPULATES the
// ordering for the case that motivates the issue.
//
// Both halves are asserted, because either alone is insufficient. The FIELD
// must order them (a dispatcher honoring `stage` gets it right), and the WIRE
// ORDER must too (the dispatcher that had this bug spawned rows in the order it
// received them, and SortSteps emits the judges first — they are older, since
// their rows were created at the original expansion while the fixer was minted
// by the re-entry).
func TestFixerIsStagedBeforeItsReReviewJudges(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	fixStage, ok := stageOf(answer, "fix@1")
	if !ok {
		t.Fatalf("premise: `fix@1` must be offered, got %v", instancesIn(answer))
	}
	if fixStage != 0 {
		t.Errorf("fix@1 is at stage %d, want 0: the writer runs first", fixStage)
	}

	judges := []string{"review@1#0", "review@1#1", "review@1#2", "review@1#3"}
	for _, judge := range judges {
		stage, ok := stageOf(answer, judge)
		if !ok {
			t.Fatalf("premise: %s must be co-dispatched with the fixer, got %v",
				judge, instancesIn(answer))
		}
		if stage <= fixStage {
			t.Errorf("%s is at stage %d and fix@1 at %d — a judge that starts "+
				"before its fixer finishes reviews the PRE-FIX tree", judge, stage, fixStage)
		}
	}

	// The wire order agrees with the field, so reading top-to-bottom is safe.
	if got := answer.Steps[0].Instance; got != "fix@1" {
		t.Errorf("the first offered row is %s, want fix@1 — a dispatcher that "+
			"spawns rows in the order it received them must not start with a "+
			"judge, which is exactly the observed defect", got)
	}
}

// TestStageNeedsNoClassHeuristic is AC3: a dispatcher honoring `stage` does not
// need a class-keyed writers-first partition to get the fixer/judge case right.
//
// Asserted as a property of the DATA rather than by deleting a script this
// repository does not contain: the staging must be derivable with no reference
// to `class` at all. So this test partitions purely on `stage` and checks the
// result is the ordering the class heuristic used to produce — meaning the
// heuristic is now redundant, which is what AC3 asks for.
func TestStageNeedsNoClassHeuristic(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	// The dispatcher a staged wire format allows: no `class` is read.
	first, rest := []string{}, []string{}
	for _, row := range answer.Steps {
		if row.Stage == 0 {
			first = append(first, row.Instance)
		} else {
			rest = append(rest, row.Instance)
		}
	}

	if len(first) != 1 || first[0] != "fix@1" {
		t.Errorf("stage 0 = %v, want exactly [fix@1]; a stage-only dispatcher "+
			"must reach the same answer the class partition did", first)
	}
	// The judges are exactly stage 1 — behind the fixer, ahead of the staged
	// closure (synthesize and beyond sit at deeper stages, per their own
	// `after` edges).
	stageOne := []string{}
	for _, row := range answer.Steps {
		if row.Stage == 1 {
			stageOne = append(stageOne, row.Instance)
		}
	}
	if len(stageOne) != 4 {
		t.Errorf("stage 1 = %v, want the four judges", stageOne)
	}
	if len(rest) < 4 {
		t.Errorf("stages past 0 = %v, want at least the four judges", rest)
	}
}

// TestOnlyATreeHolderStagesOthers pins the clause that keeps this rule
// GENERIC rather than a class heuristic in disguise: only a step the instance
// DECLARED as holding the tree (`holds_tree`, workflow.Step.HoldsTree) can
// stage anything behind it.
//
// The distinction is the whole design. Core assigns no meaning to a class name
// (§6.5) — `write` is the reference instance's word, not core's — so a rule
// keyed on the name would be exactly the dispatcher heuristic staging exists to
// replace, moved one layer down. `holds_tree` is the author's own statement
// about the tree, which is the fact the ordering actually depends on: a step
// that only reads cannot stale a sibling's view of the tree, so it has no
// business delaying one.
func TestOnlyATreeHolderStagesOthers(t *testing.T) {
	// Same fixture shape as the real workflows, with the ONE difference that
	// matters: the loop step declares it does not hold the tree. Nothing about
	// its class changes, so a rule reading `class` would stage identically and
	// this test would fail.
	const src = `
[pipeline]
name = "readonly-loop"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 1 }

[[step]]
name = "implement"
executor = "w"
class = "write"
emits = "change-summary"
after = []

[[step]]
name = "review"
after = ["implement"]
fanout = ["judge-a", "judge-b"]
emits = "findings"
inputs = ["implement.change-summary"]

[[step]]
name = "verify"
after = ["review"]
executor = "verify-ac"
emits = "ac-report"
threshold = { "fix-loop" = "any(status == unmet)" }
on_fail = "waiting-human"

[[step]]
name = "fix"
executor = "fix"
class = "write"
holds_tree = false
emits = "change-summary"
loop = true
after_loop = "review"
inputs = ["implement.change-summary"]
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "readonly-loop.toml")

	issue := createIssue(t, conn, "readonly-loop", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
	}
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	if _, ok := stageOf(answer, "fix@1"); !ok {
		t.Fatalf("premise: the loop must co-dispatch fix@1, got %v", instancesIn(answer))
	}

	// Every READY row sits at stage 0: the non-holding fixer stages nothing
	// behind it. STAGED rows are the separate, legitimate case — verify@1
	// rides the closure behind the judges because of its own `after`, which
	// is dependency staging, not the loop rule this test falsifies.
	for _, row := range answer.Steps {
		if row.Status == db.StepReady && row.Stage != 0 {
			t.Errorf("%s carries stage %d, but the loop step declares "+
				"`holds_tree = false` — a step that only reads the tree cannot "+
				"stale another step's view of it, so it must stage nothing. A "+
				"rule keyed on `class` (still \"write\" here) would get this "+
				"wrong, which is the heuristic DKT-18 replaces", row.Instance, row.Stage)
		}
	}
}

// TestUnstagedSetsAreUnchanged is AC5: rows with no ordering constraint carry
// no stage, so existing dispatchers and recorded manifests are unaffected.
//
// The `omitempty` half matters as much as the value: a row that gained
// `"stage":0` would change every manifest hash in the repo for a field that
// says nothing.
func TestUnstagedSetsAreUnchanged(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	// The ordinary opening offer: one READY implement step (the closure rows
	// behind it are `staged` and legitimately carry stages — their ordering
	// constraint is their own `after`).
	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	if len(answer.Steps) == 0 {
		t.Fatal("premise: the run must offer work")
	}

	for _, row := range answer.Steps {
		if row.Status != db.StepReady {
			continue
		}
		if row.Stage != 0 {
			t.Errorf("%s carries stage %d with no ordering constraint on the "+
				"ready cohort", row.Instance, row.Stage)
		}
		raw, err := json.Marshal(row)
		testsupport.Must(t, err, "marshal: %v", err)
		if containsKey(t, raw, "stage") {
			t.Errorf("an unstaged row serialized a `stage` key (%s); the field "+
				"is omitempty so existing manifest hashes do not move", raw)
		}
	}
}

// TestStagedRowSurvivesCanonicalRoundTrip is AC4: the staged field rides the
// SAME marshaling the wire uses, so a manifest's stored bytes and its rows
// cannot disagree.
func TestStagedRowSurvivesCanonicalRoundTrip(t *testing.T) {
	row := model.StepRow{
		Step: "STEP-1", Instance: "review@1#0", Issue: "DKT-1", Run: "RUN-1",
		Kind: "executor", Class: "judge", Attempt: 1, LeaseTTLS: 900,
		Status: "ready", Stage: 1,
	}

	raw, sum, err := canonicalRowBytes(row)
	testsupport.Must(t, err, "canonicalRowBytes: %v", err)

	var back model.StepRow
	err = json.Unmarshal([]byte(raw), &back)
	testsupport.Must(t, err, "unmarshal: %v", err)
	if back.Stage != row.Stage {
		t.Errorf("stage survived the canonical bytes as %d, want %d — a manifest "+
			"row that drops the field records an ordering it did not open with",
			back.Stage, row.Stage)
	}

	// And the hash covers it: two rows differing ONLY in stage must not hash
	// alike, or a manifest could not detect a reordering.
	other := row
	other.Stage = 0
	_, otherSum, err := canonicalRowBytes(other)
	testsupport.Must(t, err, "canonicalRowBytes: %v", err)
	if sum == otherSum {
		t.Error("rows differing only in `stage` hash identically; the manifest " +
			"hash must cover the ordering it recorded")
	}
}

// TestStagingIsScopedToOneIssue pins the clause that keeps staging from
// serializing unrelated work: a tree-holding loop step in ONE issue must not
// stage another issue's steps behind it.
//
// ClaimablePrefix has already guaranteed two issues admitted together do not
// share a scope, so ordering across them would be pure latency for no
// correctness gain.
// TestEvictedFixerTakesItsDependentsFromTheOffer is DKT-26, the measured
// failure exactly: with `write = { max = 1 }`, another issue's `implement@0`
// (older row, same class) wins the greedy write slot, the loop's `fix@1` —
// the set's youngest row — was rationed out, and before the fix `next` still
// offered `review@1#0..#3`, UNSTAGED, because staging only compared rows the
// offer kept. RUN-2 hit this live (2026-08-11): the conductor was handed
// DKT-20's re-review with no fixer and no stage labels, one dispatch away
// from re-reviewing the commit the fix was about to rewrite.
//
// The staged closure resolves the same hazard the other way around: instead
// of evicting the judges for the next round, the rationed fixer itself rides
// STAGED at a later stage — the write cap holds per stage, so it never shares
// one with issue B's implement — and its judges stage strictly behind it. The
// invariant this test pins is unchanged from DKT-26's: NEVER a judge the wire
// lets a dispatcher start at or before its fixer.
func TestEvictedFixerTakesItsDependentsFromTheOffer(t *testing.T) {
	conn := mustDB(t)
	registerFixtureSchema(t, conn)
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)
	src = append(src, []byte("\n[limits]\nwrite = { max = 1 }\n")...)
	registerSource(t, conn, src, "example-write-limited.toml")

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	issueB := createIssue(t, conn, "issue b", "body", "task", nil)
	run := startRun(t, conn, issueA, issueB)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	// ISSUE A into its fix loop; ISSUE B left at `implement@0` — ready, class
	// write, and OLDER than A's fixer (created at activation, where the fixer
	// is minted by the loop entry), so B wins the one write slot exactly as
	// DKT-19's implement did live.
	driveIssueToLoopReentry(t, conn, e, issueA)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	var sawBImplement bool
	implementStage, fixStage := -1, -1
	var judgeStages []int
	for _, row := range answer.Steps {
		issue := stepIssueID(t, conn, row.Step)
		switch {
		case issue == issueB && row.Instance == "implement@0":
			sawBImplement = true
			implementStage = row.Stage
			if row.Status != db.StepReady {
				t.Errorf("issue B's implement@0 is %q, want ready — it holds "+
					"the one write slot", row.Status)
			}
		case issue == issueA && row.Instance == "fix@1":
			fixStage = row.Stage
			if row.Status != db.StepStaged {
				t.Errorf("fix@1 is %q while issue B's implement holds the one "+
					"write slot — the rationed fixer must ride staged, never "+
					"claimable-now", row.Status)
			}
		case issue == issueA && strings.HasPrefix(row.Instance, "review@1"):
			judgeStages = append(judgeStages, row.Stage)
			if row.Status != db.StepStaged {
				t.Errorf("%s is %q while its fixer is rationed to a later "+
					"stage — an unstaged re-review of the tree fix@1 is about "+
					"to rewrite", row.Instance, row.Status)
			}
		}
	}
	if !sawBImplement {
		t.Fatalf("premise: issue B's implement@0 must hold the write slot, got %v",
			instancesIn(answer))
	}
	if fixStage < 0 {
		t.Fatalf("fix@1 must ride the offer staged, got %v", instancesIn(answer))
	}
	// The per-stage write cap: the staged fixer never shares a stage with the
	// implement that beat it, and the judges never start at or before it.
	if fixStage <= implementStage {
		t.Errorf("fix@1 at stage %d alongside issue B's implement at stage %d "+
			"— write = { max = 1 } must hold within every stage", fixStage,
			implementStage)
	}
	if len(judgeStages) != 4 {
		t.Errorf("offered %d review@1 judges, want all 4 staged behind the fixer",
			len(judgeStages))
	}
	for _, s := range judgeStages {
		if s <= fixStage {
			t.Errorf("a judge sits at stage %d, at or before fix@1's stage %d — "+
				"the DKT-26 hazard verbatim", s, fixStage)
		}
	}

	// Once the slot frees, the fixer and its judges co-dispatch, staged — the
	// design TestFixerIsStagedBeforeItsReReviewJudges pins, restored.
	completeInIssue(t, conn, e, issueB, "implement@0", "the change summary", "")

	answer, err = e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next after the slot freed: %v", err)

	var sawFixer, sawStagedJudge bool
	for _, row := range answer.Steps {
		if stepIssueID(t, conn, row.Step) != issueA {
			continue
		}
		if row.Instance == "fix@1" {
			sawFixer = true
		}
		if strings.HasPrefix(row.Instance, "review@1") && row.Stage > 0 {
			sawStagedJudge = true
		}
	}
	if !sawFixer {
		t.Fatalf("fix@1 is still not offered after the write slot freed: %v",
			instancesIn(answer))
	}
	if !sawStagedJudge {
		t.Errorf("the re-review is not staged behind fix@1 in the restored "+
			"co-dispatch: %v", instancesIn(answer))
	}
}

// TestLoopBodyExcludedFromReadinessEvictsItsDependents is DKT-48: the same
// invariant TestEvictedFixerTakesItsDependentsFromTheOffer pins, with the
// fixer excluded a DIFFERENT way. There, `fix@1` reached the ready set and
// lost ClaimablePrefix's greedy scan on a class slot — visible to
// precedesInSet, which scans `sorted`, the ready set itself. Here the budget
// cap is set so `fix@1`'s OWN cost fails R7 (ready.go's budgetHeadroom)
// before it ever reaches `sorted` — invisible to precedesInSet, which never
// gets a chance to compare it against anything. Only a definition-keyed
// check (blockingLoopBodyAbsent) can evict `review@1`'s judges here, because
// the set-scanning half has nothing in the set to find.
func TestLoopBodyExcludedFromReadinessEvictsItsDependents(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	run := startRun(t, conn, issueA)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	// Drive issue A into its fix loop.
	driveIssueToLoopReentry(t, conn, e, issueA)

	// A cap set so `fix@1`'s OWN cost fails R7 alone (budgetHeadroom compares
	// spend()+cost against cap for the one row under test, with nothing else
	// admitted yet) while `review@1`'s judges individually still fit — the
	// margin sits strictly between the fixture's review cost (0.60) and fix
	// cost (1.00), so `fix@1` never enters `sorted` and the judges do.
	//
	// R7's check is uniform per candidate against the SAME snapshot floor
	// (budget.floor, computed once), so this cap admits only ONE judge into
	// the OFFER: claimablePass' cumulative offerBudget check then rejects the
	// second at floor+admittedCost+cost, and a cap wide enough to admit all
	// four cumulatively (>= floor + 4*reviewCost) is also wide enough to pass
	// fix@1's own R7 check (floor + fixCost < floor + 4*reviewCost here),
	// which would defeat the premise below. The four-judge shape of "the
	// dependents leave the offer with it" is pinned separately, by
	// TestEvictedFixerTakesItsDependentsFromTheOffer (class-headroom
	// exclusion, not budget), where offerBudget never rations the set.
	floor := runFloor(t, conn, run.ID)
	reviewCost := expectedCostOf(t, conn, "review@1#0")
	fixCost := expectedCostOf(t, conn, "fix@1")
	margin := (reviewCost + fixCost) / 2
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, floor+margin, run.ID)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	var sawFixer bool
	var sawJudge []string
	for _, row := range answer.Steps {
		switch {
		case row.Instance == "fix@1":
			sawFixer = true
		case strings.HasPrefix(row.Instance, "review@1"):
			sawJudge = append(sawJudge, row.Instance)
		}
	}
	if sawFixer {
		t.Fatalf("premise: fix@1 must fail R7 budget headroom and never "+
			"reach the ready set, got it offered: %v", instancesIn(answer))
	}
	if len(sawJudge) > 0 {
		t.Errorf("%v offered with fix@1 excluded from readiness by budget — "+
			"an unstaged re-review of the tree fix@1 is about to rewrite "+
			"(DKT-48)", sawJudge)
	}

	// Once the fixer fits, it and its judges co-dispatch together again — the
	// same liveness promise every other narrowing here makes.
	_, err = SetRunBudget(conn, run.ID, floor+4*reviewCost+fixCost, "raised", nil, nowMS)
	testsupport.Must(t, err, "SetRunBudget: %v", err)

	answer, err = e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next after the budget raised: %v", err)

	sawFixer = false
	var sawStagedJudge bool
	for _, row := range answer.Steps {
		if row.Instance == "fix@1" {
			sawFixer = true
		}
		if strings.HasPrefix(row.Instance, "review@1") && row.Stage > 0 {
			sawStagedJudge = true
		}
	}
	if !sawFixer {
		t.Fatalf("fix@1 is still not offered after the budget raised: %v",
			instancesIn(answer))
	}
	if !sawStagedJudge {
		t.Errorf("the re-review is not staged behind fix@1 in the restored "+
			"co-dispatch: %v", instancesIn(answer))
	}
}

// TestSingleRowOfferStillEvictsAnExcludedLoopBody is DKT-48-C2:
// ClaimablePrefix used to return `sorted` unchecked whenever it held fewer
// than two rows, on the assumption that a single row cannot conflict with
// anything else IN THE SET — true for precedesInSet's old set-scanning
// eviction, but blockingLoopBodyAbsent needs no second member of `sorted` at
// all, since it asks the DEFINITION. A workflow whose `after_loop` root has
// no fanout offers exactly one row once its fixer fails R7, and that single
// row is exactly the one this issue's dependents-leave-with-it rule must
// still reach.
func TestSingleRowOfferStillEvictsAnExcludedLoopBody(t *testing.T) {
	const src = `
[pipeline]
name = "single-dependent-loop"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "w"
class = "write"
emits = "change-summary"
expected_cost = 1.0
after = []

[[step]]
name = "review"
executor = "judge"
after = ["implement"]
emits = "findings"
expected_cost = 1.0
inputs = ["implement.change-summary"]

[[step]]
name = "verify"
after = ["review"]
executor = "verify-ac"
emits = "ac-report"
expected_cost = 0.1
threshold = { "fix-loop" = "any(status == unmet)" }
on_fail = "waiting-human"

[[step]]
name = "fix"
executor = "fix"
class = "write"
emits = "change-summary"
expected_cost = 2.0
loop = true
after_loop = "review"
inputs = ["implement.change-summary"]
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "single-dependent-loop.toml")

	issue := createIssue(t, conn, "single-dependent-loop", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	claimAndComplete(t, conn, e, "review@0", "findings", "")
	claimAndComplete(t, conn, e, "verify@0", "the ac report", unmetPayload)

	// A cap between the review and fix costs, exactly as
	// TestLoopBodyExcludedFromReadinessEvictsItsDependents sizes it: fix@1
	// fails R7 alone and never reaches `sorted`, review@1 passes it and is
	// the ONLY row `sorted` can hold — no other step in this workflow, or
	// this run, is ready at this point.
	floor := runFloor(t, conn, run.ID)
	reviewCost := expectedCostOf(t, conn, "review@1")
	fixCost := expectedCostOf(t, conn, "fix@1")
	margin := (reviewCost + fixCost) / 2
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, floor+margin, run.ID)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	if _, ok := stageOf(answer, "fix@1"); ok {
		t.Fatalf("premise: fix@1 must fail R7 budget headroom and never "+
			"reach the ready set, got it offered: %v", instancesIn(answer))
	}
	if _, ok := stageOf(answer, "review@1"); ok {
		t.Errorf("review@1 offered alone with fix@1 excluded from readiness "+
			"by budget — a single-row offer must still evict a dependent "+
			"whose loop body is absent: %v", instancesIn(answer))
	}
}

func TestStagingIsScopedToOneIssue(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	// TWO issues in ONE run, so their steps land in the same ready set and the
	// cross-issue question is actually asked. A single-issue run cannot
	// distinguish "scoped to one issue" from "no staging happened".
	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	issueB := createIssue(t, conn, "issue b", "body", "task", nil)
	run := startRun(t, conn, issueA, issueB)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	// Drive ISSUE A ONLY into its fix loop, so the set holds A's staged fixer
	// and judges alongside B's ordinary, unrelated steps.
	//
	// Every step is addressed by (issue, instance): two issues in one run carry
	// the SAME instance strings, and stepIDByInstance resolves ties by lowest
	// id, so driving issue A by bare instance would silently drive issue B's
	// step for any instance B happened to create first.
	driveIssueToLoopReentry(t, conn, e, issueA)

	// Put ISSUE B at its own review fanout, which is IN afterLoopDownstream
	// (the set roots at `review`). Leaving B at `implement@0` would make this
	// test vacuous: `implement` is upstream of `review`, so the staging rule
	// could not order it behind A's fixer even with the same-issue guard
	// removed, and the test would pass for the wrong reason.
	completeInIssue(t, conn, e, issueB, "implement@0", "the change summary", "")

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	// Issue A's loop really did co-dispatch, or this test proves nothing.
	var sawStagedA bool
	for _, row := range answer.Steps {
		if stepIssueID(t, conn, row.Step) == issueA && row.Stage > 0 {
			sawStagedA = true
		}
	}
	if !sawStagedA {
		t.Fatalf("premise: issue A's re-review must be staged behind its fixer, got %v",
			instancesIn(answer))
	}

	// And ISSUE B's READY rows are untouched. B shares no scope with A
	// (ClaimablePrefix guarantees it for anything admitted together), so
	// ordering B behind A's writer would be pure latency for no correctness.
	// B's own STAGED rows legitimately carry stages — that is B's own `after`
	// closure, not A's fixer reaching across issues.
	for _, row := range answer.Steps {
		if stepIssueID(t, conn, row.Step) != issueB {
			continue
		}
		if row.Status == db.StepReady && row.Stage != 0 {
			t.Errorf("%s belongs to issue B and carries stage %d — another "+
				"issue's fixer must not stage unrelated ready work behind it",
				row.Instance, row.Stage)
		}
	}
}

// TestLoopBodyEvictionIsScopedToOneIssue pins blockingLoopBodyAbsent's own
// same-issue guard (DKT48-T1): TestStagingIsScopedToOneIssue only exercises
// precedesInSet, the set-scanning half, and the definition-keyed eviction
// path carries its own copy of the same-issue rule with nothing that fails
// if it is dropped.
//
// Two issues, both driven into their fix loop; issue B's `fix@1` is claimed
// (non-terminal, off the offer) while issue A's is left ready. Without the
// guard, B's claimed fixer — same workflow, same ordinal, a loop body by its
// own `after_loop` — would wrongly withhold issue A's re-review too.
func TestLoopBodyEvictionIsScopedToOneIssue(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	issueB := createIssue(t, conn, "issue b", "body", "task", nil)
	run := startRun(t, conn, issueA, issueB)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	for _, issue := range []int{issueA, issueB} {
		driveIssueToLoopReentry(t, conn, e, issue)
	}

	// Claim issue B's fix@1 so it leaves PENDING (and the offer) without
	// recording — non-terminal and unoffered, exactly the shape
	// blockingLoopBodyAbsent looks for, but on a DIFFERENT issue than the
	// review it must not reach.
	bFixID := stepIDIn(t, conn, issueB, "fix@1")
	_, err = ClaimStep(conn, bFixID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim issue B's fix@1: %v", err)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	var sawAFixer, sawAJudge, sawBJudge bool
	for _, row := range answer.Steps {
		issue := stepIssueID(t, conn, row.Step)
		switch {
		case issue == issueA && row.Instance == "fix@1":
			sawAFixer = true
		case issue == issueA && strings.HasPrefix(row.Instance, "review@1"):
			sawAJudge = true
		case issue == issueB && strings.HasPrefix(row.Instance, "review@1"):
			sawBJudge = true
		}
	}
	if !sawAFixer {
		t.Fatalf("premise: issue A's fix@1 must still be offered, got %v",
			instancesIn(answer))
	}
	if !sawAJudge {
		t.Errorf("issue A's review@1 is withheld by issue B's claimed fix@1 — "+
			"blockingLoopBodyAbsent's same-issue guard did not scope the "+
			"match: %v", instancesIn(answer))
	}
	if sawBJudge {
		t.Errorf("issue B's review@1 is offered while its own fixer sits "+
			"claimed and unstaged: %v", instancesIn(answer))
	}
}

// TestLoopBodyEvictionIsScopedToOneOrdinal pins blockingLoopBodyAbsent's own
// same-ordinal guard (DKT48-T2), the same way TestLoopBodyEvictionIsScopedToOneIssue
// pins the issue guard: same-ordinal is written into the function's doc, the
// fix direction and the AC, and nothing else in the tree distinguishes it —
// dropping it leaves the whole package green.
//
// fix@1 is claimed (non-terminal, off the offer) and its own `ordinal`
// column is relabeled to one review@1 does not share. A loop body at any
// OTHER ordinal must not withhold this ordinal's re-review.
func TestLoopBodyEvictionIsScopedToOneOrdinal(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	run := startRun(t, conn, issueA)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	driveIssueToLoopReentry(t, conn, e, issueA)

	fixID := stepIDIn(t, conn, issueA, "fix@1")
	_, err = ClaimStep(conn, fixID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim fix@1: %v", err)
	execSQL(t, conn, `UPDATE steps SET ordinal = 99 WHERE id = ?`, fixID)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	var sawJudge []string
	for _, row := range answer.Steps {
		if strings.HasPrefix(row.Instance, "review@1") {
			sawJudge = append(sawJudge, row.Instance)
		}
	}
	if len(sawJudge) != 4 {
		t.Errorf("review@1's judges = %v, want all four offered — a "+
			"different-ordinal loop body must not withhold a re-review the "+
			"ordinal guard says it has nothing to do with: %v",
			sawJudge, instancesIn(answer))
	}
}

// TestLimitTruncationKeepsTheFixerBeforeItsJudges pins DKT-38: a limit cuts
// a STAGE-ORDERED set, so a truncated offer keeps the tree-holding fixer
// (stage 0) and drops judges — never the reverse. SortSteps emits the
// judges first (they are older rows), so a raw cut kept exactly the wrong
// end: two unstaged judges and no fixer, which the DKT-58 eviction then
// removed as well, publishing an empty offer while a whole loop stood ready.
// readyRows is the shared tail of `next` and `dispatch open`, so the pin
// covers `dispatch open --limit` — the reported repro — through either verb.
func TestLimitTruncationKeepsTheFixerBeforeItsJudges(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	run := startRun(t, conn, issueA)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	driveIssueToLoopReentry(t, conn, e, issueA)

	// The ready set is fix@1 plus review@1#0..3; the limit admits two.
	answer, err := e.NextSteps(conn, run.ID, 2, nowMS)
	testsupport.Must(t, err, "next with limit 2: %v", err)

	// The true pre-slice count spans the whole offer, staged closure included:
	// fix@1, four judges, then synthesize@1, reconcile@1, and verify@1 riding
	// staged behind them.
	if answer.Total != 8 {
		t.Errorf("total = %d, want the true pre-slice count 8", answer.Total)
	}
	if len(answer.Steps) != 2 {
		t.Fatalf("offered %d rows under limit 2, want 2: %v",
			len(answer.Steps), instancesIn(answer))
	}
	if answer.Steps[0].Instance != "fix@1" || answer.Steps[0].Stage != 0 {
		t.Errorf("row 0 = %s (stage %d), want fix@1 at stage 0 — the cut "+
			"must keep the predecessor end of the stage order",
			answer.Steps[0].Instance, answer.Steps[0].Stage)
	}
	if !strings.HasPrefix(answer.Steps[1].Instance, "review@1") ||
		answer.Steps[1].Stage != 1 {
		t.Errorf("row 1 = %s (stage %d), want a review@1 judge at stage 1",
			answer.Steps[1].Instance, answer.Steps[1].Stage)
	}
}

// TestLoopBodyEvictionSkipsANonTreeHolder pins blockingLoopBodyAbsent's
// holds_tree clause (DKT-76), completing the guard-per-test set the issue and
// ordinal guards started (DKT48-T1/T2): a loop body that declares
// `holds_tree = false` cannot stale a sibling's view of the tree, so leaving
// it non-terminal and off the offer must not withhold its `after_loop`
// dependents — withholding would be pure latency, and before this test,
// neutralising the clause left the whole tree green.
func TestLoopBodyEvictionSkipsANonTreeHolder(t *testing.T) {
	conn := mustDB(t)

	// The committed fixture with ONE edit — the fix step declares it does not
	// hold the tree — registered through the same parse-validate-lint path.
	registerFixtureSchema(t, conn)
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)
	patched := bytes.Replace(src,
		[]byte("name = \"fix\"\nexecutor = \"fix\"\nclass = \"write\""),
		[]byte("name = \"fix\"\nexecutor = \"fix\"\nclass = \"write\"\nholds_tree = false"),
		1)
	if bytes.Equal(patched, src) {
		t.Fatal("the fixture's fix step no longer matches this test's patch anchor")
	}
	registerSource(t, conn, patched, fixturePath)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	run := startRun(t, conn, issueA)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	driveIssueToLoopReentry(t, conn, e, issueA)

	// Claimed: non-terminal and off the offer, the exact shape the eviction
	// scans for — but a declared non-holder of the tree.
	fixID := stepIDIn(t, conn, issueA, "fix@1")
	_, err = ClaimStep(conn, fixID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim fix@1: %v", err)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	var sawJudge []string
	for _, row := range answer.Steps {
		if strings.HasPrefix(row.Instance, "review@1") {
			sawJudge = append(sawJudge, row.Instance)
		}
	}
	if len(sawJudge) != 4 {
		t.Errorf("review@1's judges = %v, want all four offered — a loop body "+
			"that does not hold the tree cannot stale their view of it: %v",
			sawJudge, instancesIn(answer))
	}
}

// TestNonterminalLoopBodyWithholdsThenReleasesItsReReview is DKT48-T3 /
// DKT-48-C4: blockingLoopBodyAbsent's `db.StepTerminal` guard widens the
// eviction rule beyond "ready-but-rationed" (precedesInSet's old reach) to
// ANY non-terminal status — claimed, running, gated, and parked at
// waiting-human — and nothing pinned either half of that widening: that it
// blocks while the body sits open, or that it releases once the body
// actually records (rather than only when a budget or class cap lifts, the
// only liveness path the sibling test exercises).
func TestNonterminalLoopBodyWithholdsThenReleasesItsReReview(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	run := startRun(t, conn, issueA)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	driveIssueToLoopReentry(t, conn, e, issueA)

	fixID := stepIDIn(t, conn, issueA, "fix@1")
	claim, err := ClaimStep(conn, fixID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim fix@1: %v", err)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	for _, row := range answer.Steps {
		if strings.HasPrefix(row.Instance, "review@1") {
			t.Errorf("%s offered while fix@1 sits claimed, non-terminal and "+
				"off the offer: %v", row.Instance, instancesIn(answer))
		}
	}

	// The body RECORDS — the promise ready.go's eviction doc makes ("offered
	// later, once their predecessor records") — and the judges co-dispatch.
	err = e.CompleteStep(conn, fixID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the fix"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete fix@1: %v", err)

	answer, err = e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next after fix@1 recorded: %v", err)
	var sawJudge int
	for _, row := range answer.Steps {
		if strings.HasPrefix(row.Instance, "review@1") {
			sawJudge++
		}
	}
	if sawJudge != 4 {
		t.Errorf("review@1's judges = %d offered after fix@1 recorded, want "+
			"4: %v", sawJudge, instancesIn(answer))
	}
}

// TestWaitingHumanLoopBodyWithholdsItsReReviewIndefinitely is the other half
// of DKT-48-C4/DKT-48-C5: `waiting-human` is a PERSISTED non-terminal status
// (db.StepTerminal excludes it), so a loop body an operator must act on
// withholds its re-review exactly as a claimed or running one does — with no
// liveness path but an operator's own resolution, which this test does not
// claim to pin (that promise lives outside this package); only the
// withholding itself is asserted here.
func TestWaitingHumanLoopBodyWithholdsItsReReviewIndefinitely(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	run := startRun(t, conn, issueA)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	driveIssueToLoopReentry(t, conn, e, issueA)

	// Park fix@1 the way an exhausted gate or attempt cap would, without
	// exercising that whole machinery here — a step this package elsewhere
	// (TestLoopsAreBoundedByConstruction) confirms `waiting-human` is a real,
	// persisted, non-terminal status a step can carry.
	fixID := stepIDIn(t, conn, issueA, "fix@1")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`, db.StepWaitingHuman, fixID)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	for _, row := range answer.Steps {
		if strings.HasPrefix(row.Instance, "review@1") {
			t.Errorf("%s offered while fix@1 sits parked at waiting-human — "+
				"an unstaged re-review of the tree the parked fixer owns: %v",
				row.Instance, instancesIn(answer))
		}
	}
}

// TestLimitSplittingTheOfferCutsFromTheJudgesEnd is DKT-58/DKT-75 as
// amended by DKT-38: fix@1 and review@1's judges are ALL admitted together —
// the ordinary co-dispatch case — and `--limit` cuts the offer after
// ClaimablePrefix returns.
//
// The history matters because the order used to be the bug: SortSteps
// (age-then-id) puts the judges BEFORE the fixer (the fixer, `loop = true`,
// is minted by the loop re-entry and sorts last), and the `[:limit]` slice
// once ran over that pre-reorder order — a limit of 4 kept the four judges
// and cut the fixer, and DKT-58's eviction then removed the orphans too,
// publishing an EMPTY offer while a whole loop stood ready. DKT-38 moved
// the stage sort in front of the cut (sortStepsByStage, dispatch.go), so
// truncation now keeps a prefix of the manifest an unlimited open would
// publish: the fixer first, then as many judges as fit, staged.
func TestLimitSplittingTheOfferCutsFromTheJudgesEnd(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	run := startRun(t, conn, issueA)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	// Drive issue A into its fix loop with NOTHING rationing fix@1 out of the
	// ready set — unlike TestEvictedFixerTakesItsDependentsFromTheOffer
	// (class headroom) and TestLoopBodyExcludedFromReadinessEvictsItsDependents
	// (budget), the premise here is the ordinary case: fix@1 and all four
	// review@1 judges are simultaneously ready and simultaneously admitted.
	driveIssueToLoopReentry(t, conn, e, issueA)

	// Premise: unlimited, fix@1 and all four review@1 judges co-dispatch.
	unlimited, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next (unlimited): %v", err)
	var sawFixer bool
	judgeCount := 0
	for _, row := range unlimited.Steps {
		switch {
		case row.Instance == "fix@1":
			sawFixer = true
		case strings.HasPrefix(row.Instance, "review@1"):
			judgeCount++
		}
	}
	if !sawFixer || judgeCount != 4 {
		t.Fatalf("premise: fix@1 and all four review@1 judges must be offered "+
			"unlimited, got %v", instancesIn(unlimited))
	}
	if unlimited.Total != 8 {
		t.Fatalf("premise: unlimited Total = %d, want 8 (fix@1 + 4 judges + "+
			"the staged synthesize/reconcile/verify closure)", unlimited.Total)
	}

	// The old reproduction, under the new contract: a limit of 4 over the
	// stage-ordered set keeps the fixer (stage 0) plus three judges (stage
	// 1) — the fourth judge is what the cut drops, never the predecessor.
	limited, err := e.NextSteps(conn, run.ID, 4, nowMS)
	testsupport.Must(t, err, "next (limit=4): %v", err)

	if limited.Total != 8 {
		t.Errorf("Total = %d with limit=4, want the TRUE pre-limit count 8 "+
			"(narrowing must not change what Total reports)", limited.Total)
	}
	if len(limited.Steps) != 4 || limited.Steps[0].Instance != "fix@1" ||
		limited.Steps[0].Stage != 0 {
		t.Fatalf("limit=4 must keep the stage-order prefix — fix@1 first at "+
			"stage 0, then three judges — got %v", instancesIn(limited))
	}
	for _, row := range limited.Steps[1:] {
		if !strings.HasPrefix(row.Instance, "review@1") || row.Stage != 1 {
			t.Errorf("row %s (stage %d) under limit=4, want a review@1 judge "+
				"at stage 1 behind the fixer", row.Instance, row.Stage)
		}
	}

	// Once the fixer fits, it and its judges co-dispatch together again.
	full, err := e.NextSteps(conn, run.ID, 5, nowMS)
	testsupport.Must(t, err, "next (limit=5): %v", err)
	sawFixer = false
	var sawStagedJudge bool
	for _, row := range full.Steps {
		if row.Instance == "fix@1" {
			sawFixer = true
		}
		if strings.HasPrefix(row.Instance, "review@1") && row.Stage > 0 {
			sawStagedJudge = true
		}
	}
	if !sawFixer || !sawStagedJudge {
		t.Errorf("limit=5 (the exact offer size) should restore the staged "+
			"co-dispatch, got %v", instancesIn(full))
	}
}

// TestLoopHoldReasonNamesTheOpenBody pins DKT-61: when the offer withholds
// re-review rows behind an open loop body, `next`'s answer SAYS SO —
// LoopHeldReason names the withheld rows and the body with its status — and
// says nothing once the body records. Before the field existed, the eviction
// reached no surface at all: a dispatcher staring at an empty-for-the-judges
// offer could not tell a waiting-human-parked fixer's indefinite hold from
// any other narrowing.
func TestLoopHoldReasonNamesTheOpenBody(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issueA := createIssue(t, conn, "issue a", "body", "task", nil)
	run := startRun(t, conn, issueA)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()

	driveIssueToLoopReentry(t, conn, e, issueA)

	fixID := stepIDIn(t, conn, issueA, "fix@1")
	claim, err := ClaimStep(conn, fixID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim fix@1: %v", err)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	if !strings.Contains(answer.LoopHeldReason, "review@1") ||
		!strings.Contains(answer.LoopHeldReason, "fix@1 (claimed)") {
		t.Errorf("LoopHeldReason = %q, want it to name the withheld review@1 "+
			"rows and the open body fix@1 with its status", answer.LoopHeldReason)
	}

	// The body records; the hold — and its reason — are gone together.
	err = e.CompleteStep(conn, fixID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the fix summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete fix@1: %v", err)
	answer, err = e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next after the fix recorded: %v", err)
	if answer.LoopHeldReason != "" {
		t.Errorf("LoopHeldReason = %q after the body recorded, want empty — "+
			"a reason that outlives its hold is noise a dispatcher learns "+
			"to ignore", answer.LoopHeldReason)
	}
}
