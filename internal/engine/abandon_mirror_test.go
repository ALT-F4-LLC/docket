package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-377: the two abandonment paths agree with each other and with DKT-294's
// live mirror. Both narrate, both release the live status, and the abandon
// cascade — the one park-clearing write no routing transaction follows — ends
// `review` itself.

// parkTheIssueUnderReview drives the fixture to `verify@0` parked by its own
// `unverifiable` threshold arm, which puts the issue in `review`.
func parkTheIssueUnderReview(t *testing.T, conn *sql.DB, e *Engine, issue int) {
	t.Helper()
	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unverifiablePayload)
	if got := stepStatus(t, conn, "verify@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: verify@0 = %q, want %q", got, db.StepWaitingHuman)
	}
	if got := issueStatus(t, conn, issue); got != string(model.StatusReview) {
		t.Fatalf("premise: issue = %q, want %q", got, model.StatusReview)
	}
}

// TestAbandonIssueInRunNarratesTheAbandonment closes the asymmetry between the
// two abandon verbs: `abandonIssue` (the routing) drops a trail comment and
// `AbandonIssueInRun` (`docket run abandon --issue`) did not.
//
// That function's own doc comment already argued the two paths "are one fact
// about the issue, so they must not leave the tracker saying different things
// about it" — and the comment trail was exactly where they differed. An
// operator reading `docket issue show` could tell which verb stopped the work
// by whether the trail mentioned it at all.
func TestAbandonIssueInRunNarratesTheAbandonment(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	_, err := AbandonIssueInRun(conn, run.ID, issue, "the approach was wrong", nowMS)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)

	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "abandoned the issue") {
		t.Errorf("engine comments = %v, want one narrating the abandonment — "+
			"the routing path drops one and this path dropped none", bodies)
	}
	if !containsBody(bodies, model.FormatRunID(run.ID)) {
		t.Errorf("engine comments = %v, want the run named — no step decided "+
			"this one", bodies)
	}
	if !containsBody(bodies, "the approach was wrong") {
		t.Errorf("engine comments = %v, want the operator's reason carried "+
			"into the trail", bodies)
	}
}

// TestAbandonRoutingReleasesTheLiveStatus is the strand DKT-294's mirror
// opened.
//
// `abandon-issue` leaves the issue's status untouched on purpose — triage
// stays the operator's. Before the mirror nothing could write `in-progress` or
// `review`, so "untouched" meant `todo` and both ready-set filters
// (`[backlog, todo]`) still offered the issue. Now the issue can be abandoned
// FROM a live status, and nothing anywhere writes one back — so the issue the
// operator is invited to triage is one no listing shows them.
//
// It is the same write that ENDS `review`: the cascade terminalizes the parked
// `verify@0` without routing, so no `reflectIssueStatus` call is left to
// notice the park count reach zero.
func TestAbandonRoutingReleasesTheLiveStatus(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	e := testEngine()

	parkTheIssueUnderReview(t, conn, e, issue)

	verifyID := stepIDByInstance(t, conn, "verify@0")
	testsupport.Must(t, e.ResolveStep(conn, verifyID, ResolveAbandonIssue,
		"not worth another round", nowMS), "resolve --as abandon-issue")

	if got := issueStatus(t, conn, issue); got != string(model.StatusTodo) {
		t.Errorf("issue = %q after the abandonment, want %q — a live status "+
			"here is invisible to every ready-set filter and nothing else "+
			"writes an issue back", got, model.StatusTodo)
	}

	// The resolution still carries the fact; the status move is not a verdict.
	after, err := db.GetIssue(conn, issue)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if after.Resolution != db.IssueResolutionAbandoned {
		t.Errorf("resolution = %q, want %q — releasing the status must not "+
			"erase what happened", after.Resolution, db.IssueResolutionAbandoned)
	}
}

// TestAbandonInRunReleasesTheLiveStatus is the same release on the operator
// verb, which reaches the cascade without going through the routing at all.
func TestAbandonInRunReleasesTheLiveStatus(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	parkTheIssueUnderReview(t, conn, e, issue)

	_, err := AbandonIssueInRun(conn, run.ID, issue, "shipping without it", nowMS)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)

	if got := issueStatus(t, conn, issue); got != string(model.StatusTodo) {
		t.Errorf("issue = %q after `run abandon --issue`, want %q", got, model.StatusTodo)
	}
	if got := stepStatus(t, conn, "verify@0"); got != db.StepFailedRouted {
		t.Errorf("verify@0 = %q, want the cascade's %q", got, db.StepFailedRouted)
	}
}

// TestAbandonBeforeAnyClaimLeavesTheStatusAlone pins the release's LOWER
// bound: it undoes the mirror's own writes and nothing more. An issue
// abandoned before any step was claimed is still at `todo`, and the release
// must not rewrite it (or the `activity_log` would carry a move that never
// happened).
func TestAbandonBeforeAnyClaimLeavesTheStatusAlone(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	before := issueVersion(t, conn, issue)
	// `activatedRun` promoted the issue `backlog -> todo`, so the baseline is
	// not zero; what must not grow is the count AFTER this point.
	movesBefore := statusMoves(t, conn, issue, string(model.StatusTodo))
	_, err := AbandonIssueInRun(conn, run.ID, issue, "never started", nowMS)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)

	if got := issueStatus(t, conn, issue); got != string(model.StatusTodo) {
		t.Errorf("issue = %q, want it left at %q", got, model.StatusTodo)
	}
	if got := issueVersion(t, conn, issue); got != before {
		t.Errorf("issue version moved %d -> %d with no status change to make",
			before, got)
	}
	if n := statusMoves(t, conn, issue, string(model.StatusTodo)); n != movesBefore {
		t.Errorf("activity_log moves TO todo went %d -> %d; the issue was "+
			"already there and no move happened", movesBefore, n)
	}
}

// TestAbandonmentDoesNotCompleteTheIssue pins the short-circuit in
// `reconcileIssueAndRun`.
//
// `abandonIssue`'s cascade terminalizes every remaining step of the issue, so
// `issueStepsComplete` is true on the very next line — and `completeIssue`'s
// UPDATE guards on nothing but `status != done`. An abandoned issue therefore
// rendered "✔ done": the exact contradiction DKT-245 was filed about, which
// the resolution column recorded rather than removed.
func TestAbandonmentDoesNotCompleteTheIssue(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	parkTheIssueUnderReview(t, conn, e, issue)

	verifyID := stepIDByInstance(t, conn, "verify@0")
	testsupport.Must(t, e.ResolveStep(conn, verifyID, ResolveAbandonIssue,
		"not worth another round", nowMS), "resolve --as abandon-issue")

	if got := issueStatus(t, conn, issue); got == string(model.StatusDone) {
		t.Errorf("issue = %q after being abandoned — abandonment is the "+
			"OPPOSITE of completion, and the cascade is what makes the "+
			"completion check true", got)
	}
	if n := statusMoves(t, conn, issue, string(model.StatusDone)); n != 0 {
		t.Errorf("%d activity_log rows record a move to done on an abandoned "+
			"issue", n)
	}

	// The run rollup still ran: every step is terminal, so the run is finished.
	updated, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if updated.Status != model.RunDone {
		t.Errorf("run = %q after its only issue was abandoned, want %q — "+
			"short-circuiting the ISSUE completion must not skip the rollup",
			updated.Status, model.RunDone)
	}
}

// The abandon cascades stamp `recorded_at_ms` on every step they terminalize,
// because the usage grace's wave-end clock (latestExecutorRecordMS) reads that
// column for every terminal executor step with attempt > 0. A cascaded step
// left at 0 drops out of the clock and shortens the wave's usage grace.

// TestAbandonIssueInRunStampsTheCascadedStepsRecordTime: the operator verb's
// cascade stamps the abandon time on a claimed attempt-1 step and leaves an
// already-terminal sibling's record time alone.
func TestAbandonIssueInRunStampsTheCascadedStepsRecordTime(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	claimInstance(t, conn, "review@0#0", nowMS)

	held := stepByInstance(t, conn, "review@0#0")
	if held.Attempt != 1 || db.StepTerminal(held.Status) {
		t.Fatalf("premise: review@0#0 attempt %d status %q, want a live attempt 1",
			held.Attempt, held.Status)
	}
	finishedBefore := stepByInstance(t, conn, "implement@0").RecordedAtMS
	if finishedBefore == 0 {
		t.Fatal("premise: implement@0 completed with recorded_at_ms 0")
	}

	abandonAt := nowMS + 1_000
	_, err := AbandonIssueInRun(conn, run.ID, issue, "stopping the issue", abandonAt)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)

	requireCascadeStamp(t, conn, "review@0#0", abandonAt)
	if got := stepByInstance(t, conn, "implement@0").RecordedAtMS; got != finishedBefore {
		t.Errorf("implement@0 recorded_at_ms = %d after the abandon, want its "+
			"original %d — the cascade must skip terminal steps", got, finishedBefore)
	}
}

// abandonRoutingFixtureSrc has three independent steps, so one can finish, one
// can be held mid-attempt, and one can exhaust into `abandon-issue` at once.
const abandonRoutingFixtureSrc = `
[pipeline]
name = "abandon-routing-fixture"
version = 1

[match]
kind = ["task"]

[[step]]
name = "finished"
executor = "w"
emits = "change-summary"
after = []

[[step]]
name = "held"
executor = "w"
emits = "change-summary"
after = []

[[step]]
name = "quits"
executor = "w"
emits = "change-summary"
after = []
max_attempts = 1
on_fail = "abandon-issue"
`

// TestAbandonRoutingStampsTheCascadedSiblingsRecordTime: a step exhausting
// into `on_fail = "abandon-issue"` reaches abandonIssue through the
// reconciliation, and that cascade stamps the routing time on a claimed
// attempt-1 sibling and leaves an already-terminal sibling's record time alone.
func TestAbandonRoutingStampsTheCascadedSiblingsRecordTime(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(abandonRoutingFixtureSrc), "abandon-routing.toml")
	issue := createIssue(t, conn, "abandon routing", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	claimAndComplete(t, conn, e, "finished@0", "the change summary", "")
	claimInstance(t, conn, "held@0", nowMS)

	held := stepByInstance(t, conn, "held@0")
	if held.Attempt != 1 || db.StepTerminal(held.Status) {
		t.Fatalf("premise: held@0 attempt %d status %q, want a live attempt 1",
			held.Attempt, held.Status)
	}
	finishedBefore := stepByInstance(t, conn, "finished@0").RecordedAtMS
	if finishedBefore == 0 {
		t.Fatal("premise: finished@0 completed with recorded_at_ms 0")
	}

	failAt := nowMS + 1_000
	quits := claimInstance(t, conn, "quits@0", failAt)
	testsupport.Must(t, e.FailStep(conn, stepIDByInstance(t, conn, "quits@0"),
		quits.Token, "giving up", "", failAt), "FailStep quits@0")

	after, err := db.GetIssue(conn, issue)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if after.Resolution != db.IssueResolutionAbandoned {
		t.Fatalf("premise: resolution = %q, want %q — the failure did not "+
			"route abandon-issue", after.Resolution, db.IssueResolutionAbandoned)
	}

	requireCascadeStamp(t, conn, "held@0", failAt)
	if got := stepByInstance(t, conn, "finished@0").RecordedAtMS; got != finishedBefore {
		t.Errorf("finished@0 recorded_at_ms = %d after the abandon, want its "+
			"original %d — the cascade must skip terminal steps", got, finishedBefore)
	}
}

// requireCascadeStamp fails unless the cascade terminalized the step and
// stamped its record time with the abandon's clock.
func requireCascadeStamp(t *testing.T, conn *sql.DB, instance string, at int64) {
	t.Helper()
	step := stepByInstance(t, conn, instance)
	if step.Status != db.StepFailedRouted {
		t.Fatalf("premise: %s = %q after the abandon, want the cascade's %q",
			instance, step.Status, db.StepFailedRouted)
	}
	if step.RecordedAtMS != at {
		t.Errorf("%s recorded_at_ms = %d, want the abandon time %d — a "+
			"cascaded attempt-1 step must still advance the wave-end clock",
			instance, step.RecordedAtMS, at)
	}
}

// stepByInstance reads a step's stored row by its rendered identity.
func stepByInstance(t *testing.T, conn *sql.DB, instance string) *db.Step {
	t.Helper()
	step, err := db.GetStep(conn, stepIDByInstance(t, conn, instance))
	testsupport.Must(t, err, "GetStep %s: %v", instance, err)
	return step
}

// issueVersion reads the issue row's optimistic-concurrency version.
func issueVersion(t *testing.T, conn *sql.DB, issueID int) int {
	t.Helper()
	var v int
	err := conn.QueryRow(`SELECT version FROM issues WHERE id = ?`, issueID).Scan(&v)
	testsupport.Must(t, err, "reading issue version: %v", err)
	return v
}

// statusMoves counts activity_log status rows landing on a given value.
func statusMoves(t *testing.T, conn *sql.DB, issueID int, to string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM activity_log
		  WHERE issue_id = ? AND field_changed = 'status' AND new_value = ?`,
		issueID, to).Scan(&n)
	testsupport.Must(t, err, "counting status moves: %v", err)
	return n
}
