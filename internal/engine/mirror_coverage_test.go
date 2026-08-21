package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-380: five places where DKT-294's shipped behavior had no test that could
// fail. Each of these was written against the mutation the finding used.

// TestFailedStepNarratesTheRetry is finding 1.
//
// FailStep's non-exhausted branch reaps the step back to the pool, records
// `step-failed`, and commits — it never reaches reconcileIssueAndRun, so
// nothing was going to write the trail. The string "step fail" appeared
// nowhere in DKT-294's test file: no test drove this path at all, and the
// probe showed an issue whose step had just failed still narrating its
// original claim as the most recent line.
func TestFailedStepNarratesTheRetry(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	testsupport.Must(t, e.FailStep(conn, stepID, claim.Token,
		"the build did not link", "", nowMS), "fail: %v", err)

	// The non-exhausted branch: back to the pool for another attempt. The
	// fixture declares no `max_attempts` on `implement`, so it retries
	// indefinitely and this is the branch under test.
	if got := stepStatus(t, conn, "implement@0"); got != db.StepPending {
		t.Fatalf("premise: implement@0 = %q after a fail, want %q — this test "+
			"is about the RETRY branch", got, db.StepPending)
	}

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "implement@0 failed") {
		t.Errorf("engine comments = %v, want one naming the failure — a "+
			"failure that silently re-offers looks exactly like a step that "+
			"is merely slow", bodies)
	}
	if !containsBody(bodies, "the build did not link") {
		t.Errorf("engine comments = %v, want the worker's reason carried into "+
			"the trail", bodies)
	}
	if !containsBody(bodies, "attempt 1") {
		t.Errorf("engine comments = %v, want the attempt count — it is the "+
			"difference between one failure and three", bodies)
	}
}

// TestAbandonRoutingNarratesTheAbandonment is finding 2.
//
// abandonIssue's trail comment was executed by the existing abandon tests and
// asserted by none of them: dropping the commentEngineEvent call left
// `go test ./internal/engine/...` green. They proved the call did not return
// an error, which is not the same as proving it did anything.
func TestAbandonRoutingNarratesTheAbandonment(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unverifiablePayload)

	verifyID := stepIDByInstance(t, conn, "verify@0")
	testsupport.Must(t, e.ResolveStep(conn, verifyID, ResolveAbandonIssue,
		"out of scope", nowMS), "resolve --as abandon-issue")

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "verify@0 abandoned the issue") {
		t.Errorf("engine comments = %v, want one naming the step that "+
			"abandoned the issue", bodies)
	}
}

// TestOpenHoldEntersReview is finding 3 — AC2's actual subject.
//
// The AC reads "an issue with an open gate/vote step reads `review`", and the
// test standing in for it drove `verify@0`, an executor step parked by its own
// threshold: a broader case that proves a different thing. The fixture's only
// DECLARED `type=human` step (`commit-gate`) is forbidden from routing
// `waiting-human` at register time, so the AC's literal subject looked
// unreachable by that fixture's construction.
//
// It is reachable — through the hold. A tripped `hold_spread` materializes a
// `type=human` step and stops the routing step at `gated`, which is an issue
// blocked on a human decision by construction. And the mirror did not see it:
// a materialized held step is minted `pending`, never `waiting-human`, and the
// hold path never routes so nothing recomputed the status. The whole hold path
// read `in-progress` — the status for work in flight, of which a hold has none.
func TestOpenHoldEntersReview(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	driveToReconcile(t, conn, e, clusteredPayload)

	held := heldStep(t, conn, "reconcile-held@0#0")
	if held.Kind != workflow.TypeHuman || held.Status != db.StepPending {
		t.Fatalf("premise: the held step is %s/%s, want a pending human gate",
			held.Kind, held.Status)
	}
	if got := issueStatus(t, conn, issue); got != string(model.StatusReview) {
		t.Errorf("issue = %q with an open hold, want %q — the routing step is "+
			"`gated` and nothing downstream of it can advance until a human "+
			"decides", got, model.StatusReview)
	}
	if !containsBody(engineCommentBodies(t, conn, issue), "reconcile@0 is awaiting review") {
		t.Errorf("engine comments = %v, want the hold narrated like every "+
			"other transition into review", engineCommentBodies(t, conn, issue))
	}
}

// TestVoteParkEntersReview is finding 3's other half: a VOTE step, which the
// new test file contained no case for at all.
//
// Unlike a declared human gate, a vote step CAN route `waiting-human` — its
// `on_fail` is unrestricted — so this is the AC's literal subject with no
// stand-in.
func TestVoteParkEntersReview(t *testing.T) {
	const src = `
[pipeline]
name = "vote-parks"
version = 1

[match]
kind = ["task"]

[[step]]
name = "poll"
after = []
type = "vote"
voters = ["a", "b"]
vote_rule = "majority"
on_fail = "waiting-human"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "vote-parks.toml")
	issue := createIssue(t, conn, "voted", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The issue must be inside the mirror's live range for the flip to have
	// anywhere to come FROM; a vote step is never claimed, so nothing else
	// would have moved it off `todo`.
	execSQL(t, conn, `UPDATE issues SET status = ? WHERE id = ?`,
		string(model.StatusInProgress), issue)

	id := stepIDByInstance(t, conn, "poll@0")
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE id = ?`,
		db.StepWaitingHuman, id)
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	testsupport.Must(t, reconcileIssueAndRun(tx, step, nil, workflow.OnFailWaitingHuman, nowMS),
		"reconcile: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit")

	if got := issueStatus(t, conn, issue); got != string(model.StatusReview) {
		t.Errorf("issue = %q with a parked vote step, want %q", got, model.StatusReview)
	}
}

// TestMirrorTransitionsRecordActivityLog is finding 4.
//
// moveIssueStatusTx writes four things: the status column, an event, a
// comment, and an `activity_log` row. Three were asserted. Deleting the
// INSERT outright left the suite green — and `activity_log` is what an
// operator actually reads out of `docket issue show` history, so the one
// unasserted effect was the one with a human audience.
func TestMirrorTransitionsRecordActivityLog(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unverifiablePayload)

	// todo -> in-progress, written by the first claim (reflectIssueOnClaim).
	if n := statusMoves(t, conn, issue, string(model.StatusInProgress)); n == 0 {
		t.Error("no activity_log row for the todo -> in-progress flip; the " +
			"status column moved and the issue's own history does not say so")
	}
	// in-progress -> review, written by the park (reflectIssueStatus).
	if n := statusMoves(t, conn, issue, string(model.StatusReview)); n == 0 {
		t.Error("no activity_log row for the in-progress -> review flip")
	}

	// The rows say where the issue came FROM, not just where it landed —
	// which is the half a bare "status is now review" cannot reconstruct.
	var from string
	err := conn.QueryRow(
		`SELECT old_value FROM activity_log
		  WHERE issue_id = ? AND field_changed = 'status' AND new_value = ?`,
		issue, string(model.StatusReview)).Scan(&from)
	testsupport.Must(t, err, "reading the review move: %v", err)
	if from != string(model.StatusInProgress) {
		t.Errorf("the review move records old_value = %q, want %q",
			from, model.StatusInProgress)
	}
}

// TestFailureNoteStatesTheBound is the unbounded/bounded split in
// failureNote's own output — core ships no default attempt limit, so a step
// with no declared `max_attempts` must not be narrated as "attempt 3 of 0".
func TestFailureNoteStatesTheBound(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"unbounded", failureNote("implement@0", "boom", 2, 0), "attempt 2: boom"},
		{"bounded", failureNote("implement@0", "boom", 2, 3), "attempt 2 of 3: boom"},
		{"no reason", failureNote("implement@0", "", 1, 0), "attempt 1."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.HasSuffix(tc.got, tc.want) {
				t.Errorf("failureNote = %q, want it to end %q", tc.got, tc.want)
			}
		})
	}
}
