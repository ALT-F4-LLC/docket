package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-334: the three acceptance criteria DKT-294 could not reach without
// editing files outside its declared scope.

// TestDeclaredHumanGateHoldsTheIssueInReview is AC1.
//
// The window a `type=human` gate is open runs from the moment its turn comes
// to the moment the operator decides — and DecideStep, the only human.go verb
// that touches the step, runs at the END of that window. Waiting for a status
// meant the issue read `review` only once the gate had already been answered,
// which is the opposite of what the mirror is for.
//
// The mirror now asks the Scheduler whether the gate's turn has come (R3 and
// its interposition clauses, and nothing else — nobody claims a gate, so scope,
// headroom and budget are not questions about it).
func TestDeclaredHumanGateHoldsTheIssueInReview(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	if got := issueStatus(t, conn, issue); got != string(model.StatusInProgress) {
		t.Fatalf("premise: issue = %q before the gate's turn, want %q",
			got, model.StatusInProgress)
	}

	// `verify` passes, which is what makes `commit-gate@0`'s turn come.
	claimAndComplete(t, conn, e, "verify@0", "report", metPayload)

	if got := stepStatus(t, conn, "commit-gate@0"); got != db.StepPending {
		t.Fatalf("premise: commit-gate@0 = %q — this test is about the gate "+
			"BEFORE anyone parks or decides it", got)
	}
	if got := issueStatus(t, conn, issue); got != string(model.StatusReview) {
		t.Errorf("issue = %q with an open, undecided human gate, want %q — the "+
			"whole window the gate is open, not only once it is parked",
			got, model.StatusReview)
	}

	// THE RETURN TRIP, from the same predicate: deciding the gate closes the
	// window, and the issue leaves `review` without anything special-casing it.
	gateID := stepIDByInstance(t, conn, "commit-gate@0")
	testsupport.Must(t, e.DecideStep(conn, gateID, true, "looks good", nowMS),
		"approve: %v", nil)
	if got := issueStatus(t, conn, issue); got != string(model.StatusInProgress) {
		t.Errorf("issue = %q after the gate was approved, want %q", got, model.StatusInProgress)
	}
}

// TestOpenVoteProposalHoldsTheIssueInReview is AC2.
//
// A vote step is never claimed and OpenVoteProposal writes no step status, so
// the step sits `pending` for the entire time its proposal is open. That is
// the same shape AC1 covers and it needs no separate hook: what makes the
// issue read `review` is the step's turn having come, which is true from the
// moment the predecessor routes until routeVoteStep records a verdict.
func TestOpenVoteProposalHoldsTheIssueInReview(t *testing.T) {
	const src = `
[pipeline]
name = "vote-window"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
after = []
executor = "implement"
emits = "change-summary"

[[step]]
name = "poll"
after = ["implement"]
type = "vote"
voters = ["a", "b"]
vote_rule = "majority"
on_fail = "skip"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "vote-window.toml")
	issue := createIssue(t, conn, "voted", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	if got := stepStatus(t, conn, "poll@0"); got != db.StepPending {
		t.Fatalf("premise: poll@0 = %q, want an undecided vote step", got)
	}
	if got := issueStatus(t, conn, issue); got != string(model.StatusReview) {
		t.Errorf("issue = %q with an open vote proposal, want %q", got, model.StatusReview)
	}

	// Deciding it closes the window the same way.
	id := stepIDByInstance(t, conn, "poll@0")
	testsupport.Must(t, e.ResolveStep(conn, id, ResolveSkip, "not needed", nowMS),
		"resolve --as skip: %v", nil)
	if got := issueStatus(t, conn, issue); got == string(model.StatusReview) {
		t.Errorf("issue still %q after the vote step was resolved", got)
	}
}

// TestGateNotYetItsTurnDoesNotEnterReview is the predicate's lower bound, and
// the reason it is R3 rather than "any pending human step".
//
// The fixture declares `commit-gate` from activation. If the mirror counted it
// the moment it existed, every issue in this workflow would read `review` from
// its first claim onward — a status that would then mean nothing at all.
func TestGateNotYetItsTurnDoesNotEnterReview(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	if got := stepStatus(t, conn, "commit-gate@0"); got != db.StepPending {
		t.Fatalf("premise: commit-gate@0 = %q", got)
	}
	if got := issueStatus(t, conn, issue); got != string(model.StatusInProgress) {
		t.Errorf("issue = %q with a human gate four steps downstream, want %q "+
			"— it is pending because its predecessors are still to run, not "+
			"because anyone is waiting on a decision", got, model.StatusInProgress)
	}
}

// TestExhaustedFailureNarratesTheRouting is AC3's second half: an exhausted
// failure routed somewhere terminal also drops a trail comment. The
// non-exhausted half is TestFailedStepNarratesTheRetry.
func TestExhaustedFailureNarratesTheRouting(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	// The fixture's `implement` declares max_attempts = 2, so the SECOND
	// failure is the exhausting one.
	stepID := stepIDByInstance(t, conn, "implement@0")
	for i := range 2 {
		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
		testsupport.Must(t, err, "claim %d: %v", i, err)
		testsupport.Must(t, e.FailStep(conn, stepID, claim.Token,
			"the build did not link", "", nowMS), "fail %d: %v", i, err)
	}

	if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: implement@0 = %q after exhausting its attempts, "+
			"want the fixture's `on_fail = waiting-human`", got)
	}

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "attempt 2 of 2") {
		t.Errorf("engine comments = %v, want the exhausting failure narrated "+
			"with the bound it hit", bodies)
	}
	// And the routing it produced is narrated too, by the mirror.
	if !containsBody(bodies, "implement@0 is awaiting review") {
		t.Errorf("engine comments = %v, want the park narrated", bodies)
	}
}

// TestReturnTripDoesNotComment is AC4's subject, asserted rather than left to
// the docs.
//
// The `review -> in-progress` arm records an event and NO comment: the trail
// narrates what a human is being asked to decide, and "you are no longer being
// asked" is the absence of a question, not a new one. The docs claimed a
// comment on every mirror write; this is the arm that makes that false.
func TestReturnTripDoesNotComment(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unverifiablePayload)
	before := len(engineCommentBodies(t, conn, issue))

	verifyID := stepIDByInstance(t, conn, "verify@0")
	testsupport.Must(t, e.ResolveStep(conn, verifyID, ResolveOverridePass, "accepted", nowMS),
		"resolve --as override-pass: %v", nil)

	if got := issueStatus(t, conn, issue); got != string(model.StatusReview) {
		// The override-pass opens `commit-gate`, so the issue stays in review
		// on the NEXT gate. What matters is the comment count, below.
		t.Logf("issue = %q after the override-pass", got)
	}
	after := engineCommentBodies(t, conn, issue)
	for _, b := range after[before:] {
		if containsBody([]string{b}, "no longer") || containsBody([]string{b}, "back in progress") {
			t.Errorf("the return trip narrated itself: %q", b)
		}
	}

	// The EVENT is still recorded, which is where the return trip belongs.
	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if _, ok := findEvent(t, page, EventIssueInProgress); !ok {
		t.Error("no issue-in-progress event; the return trip must be on the record")
	}
}
