package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// TestRoundDeltaGuardsTheForkPointClip: the unreviewed round is integrated onto
// the shared branch before the next round forks fresh. The fork point then
// descends from the head review@0 judged, but advancing to it would clip fix@1
// — which no review ever read — out of review@2's delta.
func TestRoundDeltaGuardsTheForkPointClip(t *testing.T) {
	conn, runID, issue, e, rounds := unreviewedRoundFixture(t)

	rounds.commitRound(t, "IMPLEMENT ROUND 0")
	completeStepAt(t, conn, e, issue, "implement@0", rounds.work)
	s0 := gitRun(t, rounds.work, "rev-parse", "HEAD")

	completeReviewRoundIn(t, conn, e, issue, 0)
	completeInIssue(t, conn, e, issue, "synthesize@0", "the synthesis", "")
	driveActionInIssue(t, conn, e, issue, "reconcile@0")
	completeInIssue(t, conn, e, issue, "verify@0", "the ac report", unmetPayload)

	w1 := commitInFreshWorktree(t, rounds.work, "fix-round-1",
		"feature.txt", "IMPLEMENT ROUND 0\nFIX ROUND 1 THE UNREVIEWED CHANGE\n")
	completeStepAt(t, conn, e, issue, "fix@1", w1)

	failToPark(t, conn, e, issue, "review@1#0")
	resolveErr := e.ResolveStep(conn, stepIDIn(t, conn, issue, "review@1#0"),
		ResolveFixRound, "one more round", nowMS)
	testsupport.Must(t, resolveErr, "resolving review@1#0: %v", resolveErr)
	if !stepExists(t, conn, "fix@2") {
		t.Fatal("premise: the authorized round must have minted fix@2")
	}

	// The conductor integrates fix@1, and fix@2 forks fresh from the result.
	gitRun(t, rounds.work, "cherry-pick", gitRun(t, w1, "rev-parse", "HEAD"))
	w2 := commitInFreshWorktree(t, rounds.work, "fix-round-2", "fix2.txt", "FIX ROUND 2\n")
	completeStepAt(t, conn, e, issue, "fix@2", w2)

	if got := issueRoundBase(t, conn, runID, issue); got != s0 {
		t.Errorf("fix@2 round_base = %.12s, want the head review@0 judged %.12s", got, s0)
	}
	delta := reviewRoundDelta(t, conn, issue, "review@2#0")
	if !strings.Contains(delta, "\n+FIX ROUND 1 THE UNREVIEWED CHANGE") {
		t.Errorf("the integrated but unreviewed round's change is missing from "+
			"the delta its judges read:\n%s", delta)
	}
}

// cherryPickScenario drives the conductor's cherry-pick integration of one
// issue's rounds against a real shared checkout: every round forks a fresh
// worktree at the shared head, and the conductor cherry-picks each recorded
// head onto the shared branch as a new commit.
type cherryPickScenario struct {
	t            *testing.T
	conn         *sql.DB
	e            *Engine
	runID, issue int
	shared       string
	// judged is the head review@0 judged; judgedCopy is its cherry-picked copy,
	// which landed after a sibling issue's commit.
	judged, judgedCopy string
	seated             bool
}

// newCherryPickScenario records implement@0's s0 in a worktree and has review@0
// judge it, then lands a sibling and cherry-picks s0 after it as s0'.
func newCherryPickScenario(t *testing.T) *cherryPickScenario {
	t.Helper()
	conn, runID, issue, e, rounds := unreviewedRoundFixture(t)
	s := &cherryPickScenario{t: t, conn: conn, e: e, runID: runID, issue: issue, shared: rounds.work}

	w0 := commitInFreshWorktree(t, s.shared, "implement-0", "feature.txt", "IMPLEMENT ROUND 0\n")
	completeStepAt(t, conn, e, issue, "implement@0", w0)
	s.judged = gitRun(t, w0, "rev-parse", "HEAD")

	completeReviewRoundIn(t, conn, e, issue, 0)
	completeInIssue(t, conn, e, issue, "synthesize@0", "the synthesis", "")
	driveActionInIssue(t, conn, e, issue, "reconcile@0")
	completeInIssue(t, conn, e, issue, "verify@0", "the ac report", unmetPayload)

	integrateSibling(t, s.shared)
	s.judgedCopy = s.integrate(s.judged)
	return s
}

// integrate cherry-picks a recorded head onto the shared branch and returns
// the new commit.
func (s *cherryPickScenario) integrate(head string) string {
	s.t.Helper()
	gitRun(s.t, s.shared, "cherry-pick", head)
	return gitRun(s.t, s.shared, "rev-parse", "HEAD")
}

// annotate records `step annotate --integrated-sha` on one of the issue's steps.
func (s *cherryPickScenario) annotate(instance, sha string) {
	s.t.Helper()
	if !s.seated {
		testsupport.Must(s.t, seatTestConductor(s.conn, s.runID), "seating the conductor")
		s.e.IsAncestorFn = gitAncestorOfHead
		s.seated = true
	}
	_, err := s.e.AnnotateIntegration(s.conn, stepIDIn(s.t, s.conn, s.issue, instance),
		sha, "", testBy, testConductorToken, nowMS)
	testsupport.Must(s.t, err, "annotate %s --integrated-sha: %v", instance, err)
}

// recordRound forks a fresh worktree at the shared head, commits one file in
// it, and records the step there. It returns the recorded head.
func (s *cherryPickScenario) recordRound(instance, worktree, file, content string) string {
	s.t.Helper()
	w := commitInFreshWorktree(s.t, s.shared, worktree, file, content)
	completeStepAt(s.t, s.conn, s.e, s.issue, instance, w)
	return gitRun(s.t, w, "rev-parse", "HEAD")
}

// parkIntoFixRound parks a review panel without a verdict and authorizes the
// next fix round off the park, so that round follows with no review between.
func (s *cherryPickScenario) parkIntoFixRound(review, fix string) {
	s.t.Helper()
	failToPark(s.t, s.conn, s.e, s.issue, review)
	err := s.e.ResolveStep(s.conn, stepIDIn(s.t, s.conn, s.issue, review),
		ResolveFixRound, "one more round", nowMS)
	testsupport.Must(s.t, err, "resolving %s: %v", review, err)
	if !stepExists(s.t, s.conn, fix) {
		s.t.Fatalf("premise: the authorized round must have minted %s", fix)
	}
}

// unreviewedFixRound records fix@1 at a fresh fork of the shared head, parks
// review@1 into fix@2 so no review reads fix@1, and cherry-picks fix@1's head
// onto the shared branch. It returns that copy, f1', where fix@2 will fork.
func (s *cherryPickScenario) unreviewedFixRound() string {
	s.t.Helper()
	f1 := s.recordRound("fix@1", "fix-round-1", "fix1.txt", "FIX ROUND 1 THE UNREVIEWED CHANGE\n")
	s.parkIntoFixRound("review@1#0", "fix@2")
	f1Copy := s.integrate(f1)
	if isAncestor(s.shared, s.judged, f1Copy) || isAncestor(s.shared, f1Copy, s.judged) {
		s.t.Fatal("premise: the judged head and fix@2's fork point must have diverged")
	}
	return f1Copy
}

// TestRoundDeltaKeepsUnreviewedRoundAfterCherryPick: the judged head and the
// unreviewed round were both cherry-picked, so the judged head is off the
// branch beside fix@2's fork point. The base reaches back to where fix@1's
// integrated commit landed, keeping fix@1 in review@2's delta and the sibling
// integrated before it out.
func TestRoundDeltaKeepsUnreviewedRoundAfterCherryPick(t *testing.T) {
	s := newCherryPickScenario(t)
	fork := s.unreviewedFixRound()
	s.annotate("fix@1", fork)
	s.recordRound("fix@2", "fix-round-2", "fix2.txt", "FIX ROUND 2\n")

	if got := issueRoundBase(t, s.conn, s.runID, s.issue); got != s.judgedCopy {
		t.Errorf("fix@2 round_base = %.12s, want %.12s, the commit fix@1's "+
			"integrated copy landed on (fork point %.12s)", got, s.judgedCopy, fork)
	}
	delta := reviewRoundDelta(t, s.conn, s.issue, "review@2#0")
	if !strings.Contains(delta, "\n+FIX ROUND 1 THE UNREVIEWED CHANGE") {
		t.Errorf("the unreviewed round's change is missing from the delta its "+
			"judges read:\n%s", delta)
	}
	if strings.Contains(delta, "sibling.txt") {
		t.Errorf("the sibling integrated before the judged head's copy leaked "+
			"into the delta:\n%s", delta)
	}
}

// TestRoundDeltaKeepsUnreviewedRoundWithoutIntegratedSHA: the same shape with
// no integrated_sha recorded. Nothing names where fix@1 landed, so the base
// falls back to the judged head itself: the delta over-reports the
// integration's reshaping rather than hiding fix@1.
func TestRoundDeltaKeepsUnreviewedRoundWithoutIntegratedSHA(t *testing.T) {
	s := newCherryPickScenario(t)
	fork := s.unreviewedFixRound()
	s.recordRound("fix@2", "fix-round-2", "fix2.txt", "FIX ROUND 2\n")

	if got := issueRoundBase(t, s.conn, s.runID, s.issue); got != s.judged {
		t.Errorf("fix@2 round_base = %.12s, want the head review@0 judged %.12s "+
			"(fork point %.12s)", got, s.judged, fork)
	}
	delta := reviewRoundDelta(t, s.conn, s.issue, "review@2#0")
	if !strings.Contains(delta, "\n+FIX ROUND 1 THE UNREVIEWED CHANGE") {
		t.Errorf("the unreviewed round's change is missing from the delta its "+
			"judges read:\n%s", delta)
	}
}

// TestRoundDeltaIgnoresAnnotatedJudgedHead: the conductor annotates the judged
// implement@0 with its cherry-picked copy, which re-records implement@0's
// issue.diff after the one review@0 judged. That re-record is the judged round
// re-pointed, not a round no review read, so fix@1's delta still starts at its
// fork point and the sibling integrated before s0' stays out.
func TestRoundDeltaIgnoresAnnotatedJudgedHead(t *testing.T) {
	s := newCherryPickScenario(t)
	s.annotate("implement@0", s.judgedCopy)
	s.recordRound("fix@1", "fix-round-1", "fix1.txt", "FIX ROUND 1\n")
	if isAncestor(s.shared, s.judged, s.judgedCopy) || isAncestor(s.shared, s.judgedCopy, s.judged) {
		t.Fatal("premise: the judged head and fix@1's fork point must have diverged")
	}

	if got := issueRoundBase(t, s.conn, s.runID, s.issue); got != s.judgedCopy {
		t.Errorf("fix@1 round_base = %.12s, want its fork point %.12s (judged head %.12s)",
			got, s.judgedCopy, s.judged)
	}
	delta := reviewRoundDelta(t, s.conn, s.issue, "review@1#0")
	if !strings.Contains(delta, "\n+FIX ROUND 1") {
		t.Errorf("fix@1's own change is missing from its delta:\n%s", delta)
	}
	if strings.Contains(delta, "sibling.txt") {
		t.Errorf("the sibling integrated before the judged head's copy leaked "+
			"into the delta:\n%s", delta)
	}
}

// TestRoundDeltaSkipsAnnotatedJudgedHeadBeforeUnreviewedRound: both the judged
// implement@0 and the unreviewed fix@1 are annotated with their cherry-picked
// copies. The base is where fix@1 landed, not where the judged round's
// re-record landed, so neither the sibling nor the already-judged round is in
// review@2's delta.
func TestRoundDeltaSkipsAnnotatedJudgedHeadBeforeUnreviewedRound(t *testing.T) {
	s := newCherryPickScenario(t)
	s.annotate("implement@0", s.judgedCopy)
	fork := s.unreviewedFixRound()
	s.annotate("fix@1", fork)
	s.recordRound("fix@2", "fix-round-2", "fix2.txt", "FIX ROUND 2\n")

	if got := issueRoundBase(t, s.conn, s.runID, s.issue); got != s.judgedCopy {
		t.Errorf("fix@2 round_base = %.12s, want %.12s, the commit fix@1's "+
			"integrated copy landed on (fork point %.12s)", got, s.judgedCopy, fork)
	}
	delta := reviewRoundDelta(t, s.conn, s.issue, "review@2#0")
	if !strings.Contains(delta, "\n+FIX ROUND 1 THE UNREVIEWED CHANGE") {
		t.Errorf("the unreviewed round's change is missing from the delta its "+
			"judges read:\n%s", delta)
	}
	if strings.Contains(delta, "sibling.txt") {
		t.Errorf("the sibling integrated before the judged head's copy leaked "+
			"into the delta:\n%s", delta)
	}
	if strings.Contains(delta, "\n+IMPLEMENT ROUND 0") {
		t.Errorf("the round review@0 already judged is back in the delta:\n%s", delta)
	}
}

// TestRoundDeltaIgnoresLateAnnotationOfAnEarlierRound: review@1 judges fix@1,
// and only afterwards does the conductor annotate implement@0 with s0'. That
// re-records implement@0's issue.diff after the one review@1 judged, but
// implement@0 is a round review@0 already read, not a round no review read. So
// fix@2's delta starts at its fork point, f1', and carries neither judged round
// nor either sibling.
func TestRoundDeltaIgnoresLateAnnotationOfAnEarlierRound(t *testing.T) {
	s := newCherryPickScenario(t)
	f1 := s.recordRound("fix@1", "fix-round-1", "fix1.txt", "FIX ROUND 1 REVIEWED\n")
	completeReviewRoundIn(t, s.conn, s.e, s.issue, 1)
	completeInIssue(t, s.conn, s.e, s.issue, "synthesize@1", "the synthesis", "")
	driveActionInIssue(t, s.conn, s.e, s.issue, "reconcile@1")
	// A verdict that differs from verify@0's, so the loop's repeated-verdict
	// guard lets the round through.
	completeInIssue(t, s.conn, s.e, s.issue, "verify@1", "the round 1 ac report",
		`[{"id":"AC2","status":"unmet"}]`)
	if !stepExists(t, s.conn, "fix@2") {
		t.Fatal("premise: review round 1 must have entered the loop again")
	}

	s.annotate("implement@0", s.judgedCopy)
	// A second sibling lands before f1 is cherry-picked, so f1' is a new
	// commit beside f1 rather than f1 itself.
	second := commitInFreshWorktree(t, s.shared, "sibling-2", "sibling2.txt", "SECOND SIBLING\n")
	gitRun(t, s.shared, "merge", "-q", "--ff-only", gitRun(t, second, "rev-parse", "HEAD"))
	fork := s.integrate(f1)
	if isAncestor(s.shared, f1, fork) || isAncestor(s.shared, fork, f1) {
		t.Fatal("premise: the head review@1 judged and fix@2's fork point must have diverged")
	}
	s.recordRound("fix@2", "fix-round-2", "fix2.txt", "FIX ROUND 2\n")

	if got := issueRoundBase(t, s.conn, s.runID, s.issue); got != fork {
		t.Errorf("fix@2 round_base = %.12s, want its fork point %.12s (judged head %.12s)",
			got, fork, f1)
	}
	delta := reviewRoundDelta(t, s.conn, s.issue, "review@2#0")
	if !strings.Contains(delta, "\n+FIX ROUND 2") {
		t.Errorf("fix@2's own change is missing from its delta:\n%s", delta)
	}
	for _, judged := range []string{
		"sibling.txt", "sibling2.txt", "\n+IMPLEMENT ROUND 0", "\n+FIX ROUND 1 REVIEWED",
	} {
		if strings.Contains(delta, judged) {
			t.Errorf("work already judged or integrated before the fork (%q) is in "+
				"the delta:\n%s", strings.TrimSpace(judged), delta)
		}
	}
}

// TestRoundDeltaReachesTheEarliestUnreviewedRound: fix@1 and fix@2 both follow
// the judged head with no review between them, and both are cherry-picked and
// annotated. fix@3's base is where the EARLIER of the two landed, so review@3
// reads both rounds.
func TestRoundDeltaReachesTheEarliestUnreviewedRound(t *testing.T) {
	s := newCherryPickScenario(t)
	f1 := s.recordRound("fix@1", "fix-round-1", "fix1.txt", "FIX ROUND 1 THE UNREVIEWED CHANGE\n")
	s.parkIntoFixRound("review@1#0", "fix@2")
	s.annotate("fix@1", s.integrate(f1))

	f2 := s.recordRound("fix@2", "fix-round-2", "fix2.txt", "FIX ROUND 2 ALSO UNREVIEWED\n")
	s.parkIntoFixRound("review@2#0", "fix@3")
	s.annotate("fix@2", s.integrate(f2))

	s.recordRound("fix@3", "fix-round-3", "fix3.txt", "FIX ROUND 3\n")

	if got := issueRoundBase(t, s.conn, s.runID, s.issue); got != s.judgedCopy {
		t.Errorf("fix@3 round_base = %.12s, want %.12s, the commit the earliest "+
			"unreviewed round's integrated copy landed on", got, s.judgedCopy)
	}
	delta := reviewRoundDelta(t, s.conn, s.issue, "review@3#0")
	for _, marker := range []string{"\n+FIX ROUND 1 THE UNREVIEWED CHANGE", "\n+FIX ROUND 2 ALSO UNREVIEWED"} {
		if !strings.Contains(delta, marker) {
			t.Errorf("an unreviewed round's change (%q) is missing from the delta "+
				"its judges read:\n%s", strings.TrimSpace(marker), delta)
		}
	}
}

// TestRoundDeltaFallsBackOnUnusableIntegratedSHA: `--metadata` sets the step
// row free-form. A value that is not a full commit id, one git cannot resolve,
// or metadata that is not a JSON object names no landing, so the base falls
// back to the judged head and fix@1 stays in the delta. The abbreviated id
// resolves in git, so only the full-id rule keeps it out; the array still
// reports fix@1 as a round no review read rather than hiding it.
func TestRoundDeltaFallsBackOnUnusableIntegratedSHA(t *testing.T) {
	integratedSHA := func(sha string) string {
		return fmt.Sprintf(`{%q: %q}`, MetadataKeyIntegratedSHA, sha)
	}
	for name, metadata := range map[string]func(f1Copy string) string{
		"abbreviated id": func(f1Copy string) string { return integratedSHA(f1Copy[:12]) },
		"flag-like":      func(string) string { return integratedSHA("--all") },
		"absent commit":  func(string) string { return integratedSHA(strings.Repeat("0123456789", 4)) },
		"not an object":  func(f1Copy string) string { return fmt.Sprintf("[%q]", f1Copy) },
	} {
		t.Run(name, func(t *testing.T) {
			s := newCherryPickScenario(t)
			f1Copy := s.unreviewedFixRound()
			_, err := s.conn.Exec(`UPDATE steps SET metadata = ? WHERE id = ?`,
				metadata(f1Copy), stepIDIn(t, s.conn, s.issue, "fix@1"))
			testsupport.Must(t, err, "setting fix@1's metadata: %v", err)

			s.recordRound("fix@2", "fix-round-2", "fix2.txt", "FIX ROUND 2\n")

			if got := issueRoundBase(t, s.conn, s.runID, s.issue); got != s.judged {
				t.Errorf("fix@2 round_base = %.12s, want the head review@0 judged %.12s "+
					"(fix@1 metadata %s)", got, s.judged, metadata(f1Copy))
			}
			delta := reviewRoundDelta(t, s.conn, s.issue, "review@2#0")
			if !strings.Contains(delta, "\n+FIX ROUND 1 THE UNREVIEWED CHANGE") {
				t.Errorf("the unreviewed round's change is missing from the delta its "+
					"judges read:\n%s", delta)
			}
		})
	}
}

// TestRoundDeltaAdvancesPastReviewedSiblingIntegration: every earlier round
// was reviewed, so sibling work integrated before this round's fresh fork
// stays clipped out of the delta.
func TestRoundDeltaAdvancesPastReviewedSiblingIntegration(t *testing.T) {
	conn, runID, issue, e, rounds := unreviewedRoundFixture(t)

	rounds.commitRound(t, "IMPLEMENT ROUND 0")
	completeStepAt(t, conn, e, issue, "implement@0", rounds.work)

	completeReviewRoundIn(t, conn, e, issue, 0)
	completeInIssue(t, conn, e, issue, "synthesize@0", "the synthesis", "")
	driveActionInIssue(t, conn, e, issue, "reconcile@0")
	completeInIssue(t, conn, e, issue, "verify@0", "the ac report", unmetPayload)

	fork := integrateSibling(t, rounds.work)
	w1 := commitInFreshWorktree(t, rounds.work, "fix-round-1",
		"feature.txt", "IMPLEMENT ROUND 0\nFIX ROUND 1\n")
	completeStepAt(t, conn, e, issue, "fix@1", w1)

	if got := issueRoundBase(t, conn, runID, issue); got != fork {
		t.Errorf("fix@1 round_base = %.12s, want the fork point %.12s", got, fork)
	}
	if delta := reviewRoundDelta(t, conn, issue, "review@1#0"); strings.Contains(delta, siblingMarker) {
		t.Errorf("the sibling's integrated commit leaked into the delta:\n%s", delta)
	}
}

// TestRoundDeltaNoReviewedHeadAdvancesToFork: with no review-judged head the
// base falls back to the newest recorded one, and a fresh fork after a verbatim
// integration still advances to the fork point.
func TestRoundDeltaNoReviewedHeadAdvancesToFork(t *testing.T) {
	conn, runID, issue, e, rounds := unreviewedRoundFixture(t)

	rounds.commitRound(t, "IMPLEMENT ROUND 0")
	completeStepAt(t, conn, e, issue, "implement@0", rounds.work)

	failToPark(t, conn, e, issue, "review@0#0")
	resolveErr := e.ResolveStep(conn, stepIDIn(t, conn, issue, "review@0#0"),
		ResolveFixRound, "skip the panel", nowMS)
	testsupport.Must(t, resolveErr, "resolving review@0#0: %v", resolveErr)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: the authorized round must have minted fix@1")
	}

	fork := integrateSibling(t, rounds.work)
	w1 := commitInFreshWorktree(t, rounds.work, "fix-round-1",
		"feature.txt", "IMPLEMENT ROUND 0\nFIX ROUND 1\n")
	completeStepAt(t, conn, e, issue, "fix@1", w1)

	if got := issueRoundBase(t, conn, runID, issue); got != fork {
		t.Errorf("fix@1 round_base = %.12s, want the fork point %.12s", got, fork)
	}
}

// TestRepinRoundBaseExcludesOwnHead: a re-pin recomputes the step's issue.diff
// after the step already recorded one, so the issue's newest head is the
// step's OWN earlier record. That head is this round, not a round recorded
// after the judged one, so the re-pinned delta still advances past the
// verbatim sibling integration to the worktree's fork point.
func TestRepinRoundBaseExcludesOwnHead(t *testing.T) {
	conn, runID, issue, e, rounds := unreviewedRoundFixture(t)

	rounds.commitRound(t, "IMPLEMENT ROUND 0")
	completeStepAt(t, conn, e, issue, "implement@0", rounds.work)

	completeReviewRoundIn(t, conn, e, issue, 0)
	completeInIssue(t, conn, e, issue, "synthesize@0", "the synthesis", "")
	driveActionInIssue(t, conn, e, issue, "reconcile@0")
	completeInIssue(t, conn, e, issue, "verify@0", "the ac report", unmetPayload)

	fork := integrateSibling(t, rounds.work)
	w1 := commitInFreshWorktree(t, rounds.work, "fix-round-1",
		"feature.txt", "IMPLEMENT ROUND 0\nFIX ROUND 1\n")
	completeStepAt(t, conn, e, issue, "fix@1", w1)
	if got := issueRoundBase(t, conn, runID, issue); got != fork {
		t.Fatalf("premise: fix@1 round_base = %.12s, want the fork point %.12s", got, fork)
	}

	// The re-pin's own computation: the step as recorded from the named
	// checkout (prepareIssueDiffRepin).
	step, err := db.GetStep(conn, stepIDIn(t, conn, issue, "fix@1"))
	testsupport.Must(t, err, "GetStep(fix@1): %v", err)
	pinned := *step
	pinned.WorkRoot = w1
	body, payload, err := e.computeIssueDiff(conn, &pinned, true)
	testsupport.Must(t, err, "computeIssueDiff(fix@1): %v", err)

	var record struct {
		RoundBase string `json:"round_base"`
	}
	err = json.Unmarshal([]byte(payload), &record)
	testsupport.Must(t, err, "unmarshal %q: %v", payload, err)
	if record.RoundBase != fork {
		t.Errorf("re-pinned fix@1 round_base = %.12s, want the fork point %.12s — "+
			"the step's own earlier head read as an unreviewed round", record.RoundBase, fork)
	}
	_, delta, found := strings.Cut(body, roundDeltaMarker)
	if !found {
		t.Fatalf("the re-pinned diff carries no round-delta section:\n%s", body)
	}
	if strings.Contains(delta, "sibling.txt") {
		t.Errorf("the sibling's integrated commit leaked into the re-pinned delta:\n%s", delta)
	}
}

const siblingMarker = "SIBLING ISSUE WORK"

// commitInFreshWorktree adds a worktree at the shared checkout's head, as a
// conductor does for each new round, and commits one file in it.
func commitInFreshWorktree(t *testing.T, shared, name, file, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitRun(t, shared, "worktree", "add", "-q", dir)
	writeFile(t, dir, file, content)
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-qm", name)
	return dir
}

// integrateSibling fast-forwards the shared checkout onto a sibling issue's
// commit and returns the new shared head.
func integrateSibling(t *testing.T, shared string) string {
	t.Helper()
	sibling := commitInFreshWorktree(t, shared, "sibling", "sibling.txt", siblingMarker+"\n")
	gitRun(t, shared, "merge", "-q", "--ff-only", gitRun(t, sibling, "rev-parse", "HEAD"))
	return gitRun(t, shared, "rev-parse", "HEAD")
}

// reviewRoundDelta renders a review step's packet and returns its round-delta
// section.
func reviewRoundDelta(t *testing.T, conn *sql.DB, issue int, instance string) string {
	t.Helper()
	rendered, err := RenderStep(conn, stepIDIn(t, conn, issue, instance), "", nowMS)
	testsupport.Must(t, err, "RenderStep(%s): %v", instance, err)
	_, delta, found := strings.Cut(rendered.Packet, "round delta: changes since")
	if !found {
		t.Fatalf("%s's packet carries no round-delta section:\n%s", instance, rendered.Packet)
	}
	return delta
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
//
// review, synthesize and verify also declare `holds_tree = false`, as the
// registered standard-change workflow does. A holding reader that completes in
// the shared checkout after an integration records its own issue.diff, which
// would make it a round of its own rather than a reader of the judged head.
func parkableReviewFixture(t *testing.T, conn *sql.DB) {
	t.Helper()
	registerFixtureSchema(t, conn)
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)
	edited := string(src)
	for _, edit := range [][2]string{
		{"inputs = [\"implement.change-summary\", \"issue.diff\"]",
			"inputs = [\"implement.change-summary\", \"issue.diff\"]\nmax_attempts = 1\non_fail = \"waiting-human\""},
		{"name = \"review\"\n", "name = \"review\"\nholds_tree = false\n"},
		{"name = \"synthesize\"\n", "name = \"synthesize\"\nholds_tree = false\n"},
		{"name = \"verify\"\n", "name = \"verify\"\nholds_tree = false\n"},
	} {
		next := strings.Replace(edited, edit[0], edit[1], 1)
		if next == edited {
			t.Fatalf("the fixture's %q line moved; the edit was not applied", edit[0])
		}
		edited = next
	}
	registerSource(t, conn, []byte(edited), fixturePath)
}
