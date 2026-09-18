package engine

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A round delta that starts above an unreviewed fix round hides that round's
// whole change from the judges of the next one. RUN-14/HRN-27: fix@1 landed
// +1258/-550, fix@2 followed with no review round between them, and review@2's
// packet diffed from fix@1's own commit — so the 74 lines fix@2 moved were the
// whole reviewed object and fix@1's 1258 were invisible to every judge.
//
// The base therefore derives from the head the last REVIEW judged, which the
// ledger already records: step_inputs holds the issue.diff artifact each claim
// actually handed over, and a done step that consumed one while recording none
// of its own read the tree rather than wrote it.

// completeReviewRoundIn drives one issue's four review judges at an ordinal.
func completeReviewRoundIn(t *testing.T, conn *sql.DB, e *Engine, issue, ordinal int) {
	t.Helper()
	for i := range 4 {
		completeInIssue(t, conn, e, issue,
			fmt.Sprintf("review@%d#%d", ordinal, i), "findings", "")
	}
}

// unreviewedRoundFixture activates one issue over the fixture workflow against
// a real checkout. ONE worktree throughout, with no integration and no fork, so
// roundDeltaBase leaves the resolved head unclipped and the assertions read the
// head the helper chose rather than a fork point.
func unreviewedRoundFixture(t *testing.T) (*sql.DB, int, int, *Engine, *roundLog) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	work := gitRepo(t)
	pinned := gitRun(t, work, "rev-parse", "HEAD")

	conn := mustDB(t)
	parkableReviewFixture(t, conn)
	issue := createIssue(t, conn, "the issue", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "unreviewed round run", 0, nowMS,
		db.RunContext{ExecRoot: work, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue: %v", err)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.DiffFn = GitDiff
	e.HeadFn = sharedCheckoutHead
	return conn, run.ID, issue, e, &roundLog{work: work}
}

// roundLog accumulates one checkout's committed markers, so commitRound APPENDS
// rather than overwrites: each round's marker has to survive the next one, since
// the question a delta answers is whether it still carries the earlier round.
type roundLog struct {
	work    string
	markers []string
}

func (r *roundLog) commitRound(t *testing.T, marker string) {
	t.Helper()
	r.markers = append(r.markers, marker)
	writeFile(t, r.work, "feature.txt", strings.Join(r.markers, "\n")+"\n")
	gitRun(t, r.work, "add", "-A")
	gitRun(t, r.work, "commit", "-qm", marker)
}

// TestRoundBaseSpansUnreviewedFixRound is the incident's shape: implement@0, a
// full review round, then fix@1 and fix@2 with no review between them. fix@2's
// delta must reach back to the head review@0 judged, so fix@1's commit is
// inside the object review@2 is handed.
func TestRoundBaseSpansUnreviewedFixRound(t *testing.T) {
	conn, runID, issue, e, rounds := unreviewedRoundFixture(t)

	// implement@0 hands back S0 — the head review@0 then judges.
	rounds.commitRound(t, "IMPLEMENT ROUND 0")
	completeStepAt(t, conn, e, issue, "implement@0", rounds.work)
	s0 := gitRun(t, rounds.work, "rev-parse", "HEAD")

	completeReviewRoundIn(t, conn, e, issue, 0)
	completeInIssue(t, conn, e, issue, "synthesize@0", "the synthesis", "")
	driveActionInIssue(t, conn, e, issue, "reconcile@0")
	completeInIssue(t, conn, e, issue, "verify@0", "the ac report", unmetPayload)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: round 0 must have entered the loop")
	}

	// fix@1 commits the round no review ever reads.
	rounds.commitRound(t, "FIX ROUND 1 THE UNREVIEWED CHANGE")
	completeStepAt(t, conn, e, issue, "fix@1", rounds.work)

	// Round 1's judges never reach a verdict: the panel parks, and the operator
	// authorizes another round off the park. fix@2 therefore follows fix@1 with
	// no review between them — the incident's sequence exactly.
	failToPark(t, conn, e, issue, "review@1#0")
	resolveErr := e.ResolveStep(conn, stepIDIn(t, conn, issue, "review@1#0"),
		ResolveFixRound, "one more round", nowMS)
	testsupport.Must(t, resolveErr, "resolving review@1#0: %v", resolveErr)
	if !stepExists(t, conn, "fix@2") {
		t.Fatal("premise: the authorized round must have minted fix@2")
	}

	rounds.commitRound(t, "FIX ROUND 2")
	completeStepAt(t, conn, e, issue, "fix@2", rounds.work)

	if got := issueRoundBase(t, conn, runID, issue); got != s0 {
		t.Errorf("fix@2 round_base = %.12s, want the head review@0 judged %.12s — "+
			"an unreviewed fix round fell outside the next round's delta", got, s0)
	}

	rendered, err := RenderStep(conn, stepIDIn(t, conn, issue, "review@2#0"), "", nowMS)
	testsupport.Must(t, err, "RenderStep(review@2#0): %v", err)
	_, delta, found := strings.Cut(rendered.Packet, "round delta: changes since")
	if !found {
		t.Fatalf("review@2#0's packet carries no round-delta section:\n%s", rendered.Packet)
	}
	if !strings.Contains(delta, "FIX ROUND 1 THE UNREVIEWED CHANGE") {
		t.Errorf("the unreviewed round's own change is missing from the delta "+
			"its judges read:\n%s", delta)
	}
}

// TestRoundBaseUnchangedWhenEveryRoundWasReviewed is the regression bound: the
// ordinary implement -> review -> fix sequence resolves the same base it always
// did, because the last reviewed head IS the previous round's.
func TestRoundBaseUnchangedWhenEveryRoundWasReviewed(t *testing.T) {
	conn, runID, issue, e, rounds := unreviewedRoundFixture(t)

	rounds.commitRound(t, "IMPLEMENT ROUND 0")
	completeStepAt(t, conn, e, issue, "implement@0", rounds.work)
	s0 := gitRun(t, rounds.work, "rev-parse", "HEAD")

	completeReviewRoundIn(t, conn, e, issue, 0)
	completeInIssue(t, conn, e, issue, "synthesize@0", "the synthesis", "")
	driveActionInIssue(t, conn, e, issue, "reconcile@0")
	completeInIssue(t, conn, e, issue, "verify@0", "the ac report", unmetPayload)

	rounds.commitRound(t, "FIX ROUND 1")
	completeStepAt(t, conn, e, issue, "fix@1", rounds.work)

	if got := issueRoundBase(t, conn, runID, issue); got != s0 {
		t.Errorf("fix@1 round_base = %.12s, want %.12s — the ordinary sequence moved",
			got, s0)
	}
}

// TestRoundBaseFallsBackWhenNoReviewHasRun is the empty case: a fix round
// minted before any review recorded has no judged head to reach back to, and
// the base stays the newest recorded one so the delta still renders.
func TestRoundBaseFallsBackWhenNoReviewHasRun(t *testing.T) {
	conn, runID, issue, e, rounds := unreviewedRoundFixture(t)

	rounds.commitRound(t, "IMPLEMENT ROUND 0")
	completeStepAt(t, conn, e, issue, "implement@0", rounds.work)
	s0 := gitRun(t, rounds.work, "rev-parse", "HEAD")

	// Round 0's panel parks before any judge records, so no done step has ever
	// consumed an issue.diff when fix@1's own completion resolves its base.
	failToPark(t, conn, e, issue, "review@0#0")
	resolveErr := e.ResolveStep(conn, stepIDIn(t, conn, issue, "review@0#0"),
		ResolveFixRound, "skip the panel", nowMS)
	testsupport.Must(t, resolveErr, "resolving review@0#0: %v", resolveErr)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: the authorized round must have minted fix@1")
	}

	rounds.commitRound(t, "FIX ROUND 1")
	completeStepAt(t, conn, e, issue, "fix@1", rounds.work)

	if got := issueRoundBase(t, conn, runID, issue); got != s0 {
		t.Errorf("fix@1 round_base = %.12s, want the newest recorded head %.12s — "+
			"the fallback left the round with no delta", got, s0)
	}
}

// failToPark exhausts a step's attempts so it parks at waiting-human — the
// state `step resolve` acts on, and the only way a round ends without its
// judges recording a verdict.
func failToPark(t *testing.T, conn *sql.DB, e *Engine, issue int, instance string) {
	t.Helper()
	stepID := stepIDIn(t, conn, issue, instance)
	for i := 0; i < 8; i++ {
		if stepStatusByID(t, conn, stepID) == db.StepWaitingHuman {
			return
		}
		at := nowMS + int64(i)
		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: at})
		testsupport.Must(t, err, "claim %s: %v", instance, err)
		err = e.FailStep(conn, stepID, claim.Token, "the judge could not run", "", at)
		testsupport.Must(t, err, "fail %s: %v", instance, err)
	}
	t.Fatalf("%s never parked; status %q", instance, stepStatusByID(t, conn, stepID))
}

// stepStatusByID reads one step's status by id.
func stepStatusByID(t *testing.T, conn *sql.DB, stepID int) string {
	t.Helper()
	var status string
	err := conn.QueryRow(`SELECT status FROM steps WHERE id = ?`, stepID).Scan(&status)
	testsupport.Must(t, err, "reading status of step %d: %v", stepID, err)
	return status
}

// parkableReviewFixture is the canonical fixture with `review` bounded to one
// attempt and parking on failure. The stock fixture leaves `review` unbounded,
// so its judges re-offer forever and a round can never end without a verdict —
// the very state an unreviewed fix round is reached through.
func parkableReviewFixture(t *testing.T, conn *sql.DB) {
	t.Helper()
	registerFixtureSchema(t, conn)
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)
	bounded := strings.Replace(string(src),
		"inputs = [\"implement.change-summary\", \"issue.diff\"]",
		"inputs = [\"implement.change-summary\", \"issue.diff\"]\nmax_attempts = 1\non_fail = \"waiting-human\"",
		1)
	if bounded == string(src) {
		t.Fatal("the fixture's review inputs line moved; the bound was not applied")
	}
	registerSource(t, conn, []byte(bounded), fixturePath)
}
