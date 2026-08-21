package engine

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-294: the engine keeps an issue's status live as its steps progress, and
// drops a short activity-trail comment at the notable transitions — all
// without any `issue move` / `issue comment add`.

// unverifiablePayload routes `verify` straight to `waiting-human`:
// `any(status == unverifiable)` is true (the fixture's OTHER threshold arm,
// alongside `unmetPayload`'s fix-loop one).
const unverifiablePayload = `[{"status":"unverifiable"}]`

// issueComments reads an issue's comments, engine-authored or not.
func issueComments(t *testing.T, conn *sql.DB, issueID int) []*model.Comment {
	t.Helper()
	comments, err := db.ListComments(conn, issueID)
	testsupport.Must(t, err, "ListComments: %v", err)
	return comments
}

// engineCommentBodies filters an issue's comments down to the engine's own
// (author == db.EngineAuthor), so an assertion about the auto-generated trail
// is not tripped up by a human comment sharing the same issue.
func engineCommentBodies(t *testing.T, conn *sql.DB, issueID int) []string {
	t.Helper()
	var bodies []string
	for _, c := range issueComments(t, conn, issueID) {
		if c.Author == db.EngineAuthor {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

// containsBody reports whether any comment body contains want.
func containsBody(bodies []string, want string) bool {
	for _, b := range bodies {
		if strings.Contains(b, want) {
			return true
		}
	}
	return false
}

// TestClaimMovesTodoIssueToInProgress is the first AC, verified directly
// against `issues.status` — no `issue move` involved. It also covers the
// activity-trail comment: the claim narrates itself regardless of the flip.
func TestClaimMovesTodoIssueToInProgress(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	// Premise: activation's own promotion already ran (backlog -> todo,
	// promoteIssueTx, unmodified by this change) and nothing has claimed yet.
	if got := issueStatus(t, conn, issue); got != string(model.StatusTodo) {
		t.Fatalf("premise: issue = %q, want %q before any claim", got, model.StatusTodo)
	}

	stepID := stepIDByInstance(t, conn, "implement@0")
	_, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	if got := issueStatus(t, conn, issue); got != string(model.StatusInProgress) {
		t.Errorf("issue = %q after the first claim, want %q — the engine must "+
			"flip it without any `issue move`", got, model.StatusInProgress)
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if _, ok := findEvent(t, page, EventIssueInProgress); !ok {
		t.Error("no issue-in-progress event for the flip")
	}

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "implement@0 claimed") {
		t.Errorf("engine comments = %v, want one narrating the claim", bodies)
	}
}

// TestSecondClaimCommentsWithoutRewritingStatus: a claim against an issue
// already `in-progress` still drops its own trail line, but the status write
// is a correctly-guarded no-op (promoteIssueTx's own shape) — there is
// exactly one issue-in-progress event for the run, not one per claim.
func TestSecondClaimCommentsWithoutRewritingStatus(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	completeReviewFanout(t, conn, e, 0)

	if got := issueStatus(t, conn, issue); got != string(model.StatusInProgress) {
		t.Fatalf("issue = %q after two rounds of claims, want %q", got, model.StatusInProgress)
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var flips int
	for _, ev := range page.Events {
		if ev.Kind == EventIssueInProgress {
			flips++
		}
	}
	if flips != 1 {
		t.Errorf("issue-in-progress fired %d times, want exactly 1 (the guarded "+
			"UPDATE must no-op on an issue already in-progress)", flips)
	}

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "implement@0 claimed") {
		t.Error("missing the implement@0 claim comment")
	}
	for i := range 4 {
		want := "review@0#" + strconv.Itoa(i) + " claimed"
		if !containsBody(bodies, want) {
			t.Errorf("missing a claim comment for review@0#%d", i)
		}
	}
}

// TestVerifyUnverifiableEntersReview is the second AC: an open gate/vote step
// (here, `verify` parked directly `waiting-human` by its own threshold arm)
// puts the issue in `review`, with its own event and trail comment.
func TestVerifyUnverifiableEntersReview(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unverifiablePayload)

	if got := stepStatus(t, conn, "verify@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: verify@0 = %q, want %q", got, db.StepWaitingHuman)
	}
	if got := issueStatus(t, conn, issue); got != string(model.StatusReview) {
		t.Errorf("issue = %q with verify@0 parked, want %q", got, model.StatusReview)
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if _, ok := findEvent(t, page, EventIssueReview); !ok {
		t.Error("no issue-review event for the flip into review")
	}

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "verify@0 is awaiting review") {
		t.Errorf("engine comments = %v, want one narrating the park", bodies)
	}
}

// TestReviewReturnsToInProgressOnFixRound is the third AC — the operator's own
// words: "bidirectional — reflect live reality". Authorizing a fix round on the
// parked step supersedes it, and the issue is not left stuck at `review` once
// nothing on it is `waiting-human` any longer.
func TestReviewReturnsToInProgressOnFixRound(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unverifiablePayload)
	if got := issueStatus(t, conn, issue); got != string(model.StatusReview) {
		t.Fatalf("premise: issue = %q, want %q", got, model.StatusReview)
	}

	verifyID := stepIDByInstance(t, conn, "verify@0")
	err := e.ResolveStep(conn, verifyID, ResolveFixRound, "let's take another look", nowMS)
	testsupport.Must(t, err, "resolve --as fix-round: %v", err)

	if got := stepStatus(t, conn, "verify@0"); got != db.StepSuperseded {
		t.Fatalf("verify@0 = %q after the re-entry, want %q", got, db.StepSuperseded)
	}
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("fix-round minted no fix@1; nothing left `waiting-human` to leave review over")
	}
	if got := issueStatus(t, conn, issue); got != string(model.StatusInProgress) {
		t.Errorf("issue = %q once verify@0's park cleared, want %q — the mirror "+
			"must not stay stuck at review", got, model.StatusInProgress)
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	var reviewed, resumed int
	for _, ev := range page.Events {
		switch ev.Kind {
		case EventIssueReview:
			reviewed++
		case EventIssueInProgress:
			resumed++
		}
	}
	if reviewed != 1 {
		t.Errorf("issue-review fired %d times, want 1 (the entry)", reviewed)
	}
	// Once for the initial claim, once for leaving review.
	if resumed != 2 {
		t.Errorf("issue-in-progress fired %d times, want 2 (claim, then the "+
			"bidirectional return)", resumed)
	}

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "did not pass review; starting another round") {
		t.Errorf("engine comments = %v, want the fix-loop entry narrated", bodies)
	}
}

// TestFixLoopEntryCommentsWithoutTouchingReview: a straight `verify -> fix-loop`
// that was never parked at all still drops the "another round" comment, and
// never touches `review` — the comment is unconditional on the routing, not
// gated on a status flip that, here, never happens.
func TestFixLoopEntryCommentsWithoutTouchingReview(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unmetPayload)

	if got := stepStatus(t, conn, "verify@0"); got != db.StepDone {
		t.Fatalf("premise: verify@0 = %q, want %q (fix-loop is a `pass` "+
			"disposition on the routing step itself)", got, db.StepDone)
	}
	if got := issueStatus(t, conn, issue); got != string(model.StatusInProgress) {
		t.Errorf("issue = %q after a straight fix-loop entry, want %q unchanged",
			got, model.StatusInProgress)
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if _, ok := findEvent(t, page, EventIssueReview); ok {
		t.Error("issue-review fired for a step that never parked")
	}

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "verify@0 did not pass review; starting another round") {
		t.Errorf("engine comments = %v, want the fix-loop entry narrated", bodies)
	}
}

// TestIssueCompletionComments drives the fixture's issue all the way to
// `done` (the pre-existing, "continue to work unmodified" transition) and
// asserts the new completion comment lands alongside it, without disturbing
// `completeIssue`'s own status/activity-log write.
func TestIssueCompletionComments(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", metPayload)
	err := e.DecideStep(conn, stepIDByInstance(t, conn, "commit-gate@0"), true, "", nowMS)
	testsupport.Must(t, err, "approve commit-gate@0: %v", err)
	claimAndComplete(t, conn, e, "commit@0", "the commit record", "")

	if got := issueStatus(t, conn, issue); got != "done" {
		t.Fatalf("issue = %q, want done — completeIssue's own transition must "+
			"keep working unmodified", got)
	}

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "commit@0 completed the issue") {
		t.Errorf("engine comments = %v, want the completion narrated", bodies)
	}
}

// TestTrailCommentsCarryTheTransactionsClock is DKT-378's second half.
//
// `commentEngineEvent` is the ONLY non-test caller of `InsertEngineComment` in
// the tree, so a mutation reverting it to read `time.Now()` directly re-opens
// the caller-supplied-stamp fix across all five trail call sites at once — and
// left both internal/engine and internal/db green. The db-layer test pins the
// WRITER's handling of a parameter; this pins the CALLER's choice of what to
// pass, which is where the fix actually lives.
//
// The fixture's `nowMS` is years away from any wall clock, so a second clock
// read here is not a near miss.
func TestTrailCommentsCarryTheTransactionsClock(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	want := time.UnixMilli(nowMS).UTC()
	var checked int
	for _, c := range issueComments(t, conn, issue) {
		if c.Author != db.EngineAuthor {
			continue
		}
		checked++
		if !c.CreatedAt.Equal(want) {
			t.Errorf("engine comment %q stamped %s, want the transaction's own "+
				"%s — the transition and its narration must carry ONE time",
				c.Body, c.CreatedAt.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	}
	if checked == 0 {
		t.Fatal("premise: no engine-authored comment to check")
	}
}
