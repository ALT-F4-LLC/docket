package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-86: the operator lifecycle verbs move the run row THROUGH THE LEDGER.
// Before MoveRun existed the CLI wrote the status directly, and abandonment —
// the one lifecycle transition with no rollup twin — left no event at all:
// RUN-5's 574-event feed ended on a pause while the row said `abandoned`.

var abandonFrom = []model.RunStatus{
	model.RunPlanning, model.RunActive, model.RunWaitingHuman,
}

// lifecycleData is the event payload MoveRun writes.
type lifecycleData struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

func findEvent(t *testing.T, page *EventPage, kind string) (Event, bool) {
	t.Helper()
	for _, e := range page.Events {
		if e.Kind == kind {
			return e, true
		}
	}
	return Event{}, false
}

// TestAbandonEmitsRunAbandoned is the headline: the terminal transition is
// visible to the ledger, carrying when, why, and (via the kind's attribution)
// by whom.
func TestAbandonEmitsRunAbandoned(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	updated, _, err := MoveRun(conn, run.ID, "abandon", model.RunAbandoned,
		abandonFrom, "scope changed under it", nowMS)
	testsupport.Must(t, err, "MoveRun: %v", err)
	if updated.Status != model.RunAbandoned {
		t.Fatalf("run is %s, want %s", updated.Status, model.RunAbandoned)
	}
	if updated.Reason != "scope changed under it" {
		t.Errorf("run reason = %q, want the operator's", updated.Reason)
	}

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	event, ok := findEvent(t, page, EventRunAbandoned)
	if !ok {
		t.Fatalf("no %s event in the feed; the terminal transition is invisible "+
			"to the ledger (DKT-86)", EventRunAbandoned)
	}

	var data lifecycleData
	testsupport.Must(t, json.Unmarshal(event.Data, &data), "decoding data: %v", err)
	if data.From != string(model.RunActive) || data.To != string(model.RunAbandoned) {
		t.Errorf("event data = %s -> %s, want active -> abandoned", data.From, data.To)
	}
	if data.Reason != "scope changed under it" {
		t.Errorf("event reason = %q, want the operator's", data.Reason)
	}
}

// TestPauseAndResumeEmitEvents: the same discipline on the non-terminal edges,
// so an operator pause is distinguishable in the feed from a budget breach
// (which writes its own `run-paused` with data.reason = "budget").
func TestPauseAndResumeEmitEvents(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	_, _, err := MoveRun(conn, run.ID, "pause", model.RunWaitingHuman,
		[]model.RunStatus{model.RunActive}, "operator break", nowMS)
	testsupport.Must(t, err, "pause: %v", err)
	_, _, err = MoveRun(conn, run.ID, "resume", model.RunActive,
		[]model.RunStatus{model.RunWaitingHuman}, "", nowMS)
	testsupport.Must(t, err, "resume: %v", err)

	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)

	paused, ok := findEvent(t, page, EventRunPaused)
	if !ok {
		t.Fatal("no run-paused event for an operator pause")
	}
	var data lifecycleData
	testsupport.Must(t, json.Unmarshal(paused.Data, &data), "decoding data")
	if data.Reason != "operator break" {
		t.Errorf("pause reason = %q, want the operator's (this is what "+
			"distinguishes it from a budget breach)", data.Reason)
	}
	if _, ok := findEvent(t, page, EventRunResumed); !ok {
		t.Fatal("no run-resumed event for an operator resume")
	}
}

// TestAbandonIssueInRunStopsOneIssueOnly is DKT-28: the per-issue
// disposition. Before it, every path out of a mis-routed issue was blocked or
// terminal — `step resolve` wants a parked step, an on_fail-less fix step
// re-offers forever, `run pause` parks the run around still-pending steps,
// and `run abandon` takes the whole run down with cleanly delivered issues
// in it.
func TestAbandonIssueInRunStopsOneIssueOnly(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	doomed := createIssue(t, conn, "mis-routed work", "body", "task", nil)
	kept := createIssue(t, conn, "healthy work", "body", "task", nil)
	run := startRun(t, conn, doomed, kept)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	outcome, err := AbandonIssueInRun(conn, run.ID, doomed, "wrong repository", nowMS)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)
	if len(outcome.Steps) == 0 {
		t.Fatal("outcome names no abandoned steps")
	}
	if outcome.RunStatus != string(model.RunActive) {
		t.Errorf("run status = %q, want %q — the other issue's work continues",
			outcome.RunStatus, model.RunActive)
	}

	// Every step of the doomed issue is terminal; every step of the kept issue
	// is untouched.
	var doomedOpen, keptOpen int
	err = conn.QueryRow(
		`SELECT
		   SUM(CASE WHEN issue_id = ? AND status != 'failed-routed' THEN 1 ELSE 0 END),
		   SUM(CASE WHEN issue_id = ? AND status = 'failed-routed' THEN 1 ELSE 0 END)
		 FROM steps WHERE run_id = ?`, doomed, kept, run.ID).Scan(&doomedOpen, &keptOpen)
	testsupport.Must(t, err, "counting steps: %v", err)
	if doomedOpen != 0 {
		t.Errorf("%d step(s) of the abandoned issue escaped the disposition", doomedOpen)
	}
	if keptOpen != 0 {
		t.Errorf("%d step(s) of the OTHER issue were abandoned too", keptOpen)
	}

	// The issue's own status is not forced terminal — triage stays the
	// operator's, exactly as the abandon-issue routing behaves.
	issue, err := db.GetIssue(conn, doomed)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Status == model.StatusDone {
		t.Error("the abandoned issue was forced done; triage belongs to the operator")
	}

	// The ledger carries the disposition with its reason.
	page, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	event, ok := findEvent(t, page, EventIssueAbandoned)
	if !ok {
		t.Fatalf("no %s event in the feed", EventIssueAbandoned)
	}
	var data struct {
		Issue  string   `json:"issue"`
		Reason string   `json:"reason"`
		Steps  []string `json:"steps"`
	}
	testsupport.Must(t, json.Unmarshal(event.Data, &data), "decoding data: %v", err)
	if data.Reason != "wrong repository" || len(data.Steps) != len(outcome.Steps) {
		t.Errorf("event data = %+v, want the reason and the abandoned steps", data)
	}

	// A second disposition of the same issue finds nothing left: CONFLICT.
	if _, err := AbandonIssueInRun(conn, run.ID, doomed, "again", nowMS); err == nil {
		t.Fatal("abandoning an already-terminal issue succeeded")
	} else if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("error code = %q, want %q", code, CodeConflict)
	}

	// An issue outside the run is a miss, not a silent no-op.
	stranger := createIssue(t, conn, "elsewhere", "body", "task", nil)
	if _, err := AbandonIssueInRun(conn, run.ID, stranger, "r", nowMS); err == nil {
		t.Fatal("abandoning an issue the run does not hold succeeded")
	} else if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("error code = %q, want %q", code, CodeNotFound)
	}
}

// TestAbandonLastIssueCompletesTheRun: the rollup runs in the disposition's
// transaction, so abandoning the run's only unfinished issue ends the run the
// same way its last routed step would have.
func TestAbandonLastIssueCompletesTheRun(t *testing.T) {
	conn := mustDB(t)
	run, issueID := activatedRun(t, conn)

	outcome, err := AbandonIssueInRun(conn, run.ID, issueID, "nothing else to do", nowMS)
	testsupport.Must(t, err, "AbandonIssueInRun: %v", err)
	if outcome.RunStatus != string(model.RunDone) {
		t.Errorf("run status = %q, want %q — the disposition retired the run's "+
			"last unfinished work", outcome.RunStatus, model.RunDone)
	}
}

// TestMoveRunRefusalWritesNothing: an illegal transition is CONFLICT and the
// feed gains no event — a refused transition that logged itself would be a
// ledger describing something that did not happen.
func TestMoveRunRefusalWritesNothing(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	_, _, err := MoveRun(conn, run.ID, "abandon", model.RunAbandoned,
		abandonFrom, "first", nowMS)
	testsupport.Must(t, err, "abandon: %v", err)

	before, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)

	_, _, err = MoveRun(conn, run.ID, "abandon", model.RunAbandoned,
		abandonFrom, "second", nowMS)
	if err == nil {
		t.Fatal("abandoning an abandoned run succeeded")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("error code = %q, want %q", code, CodeConflict)
	}
	if !strings.Contains(err.Error(), "abandon applies to a run that is") {
		t.Errorf("refusal does not name the legal source statuses: %s", err)
	}

	after, err := ListEvents(conn, EventQuery{RunID: run.ID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if len(after.Events) != len(before.Events) {
		t.Errorf("a refused transition wrote %d event(s)",
			len(after.Events)-len(before.Events))
	}
}

// ---------------------------------------------------------------------------
// DKT-305 — an operator's pause is not undone by the rollup
// ---------------------------------------------------------------------------

// pauseOrigin reads where the run row says its park was decided.
func pauseOrigin(t *testing.T, conn *sql.DB, runID int) model.RunPauseOrigin {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	origin, err := db.RunPauseOriginTx(tx, runID)
	testsupport.Must(t, err, "RunPauseOriginTx: %v", err)
	return origin
}

// TestOperatorPauseSurvivesAnInFlightStepCompleting is DKT-305, and it is
// TestBreachSurvivesAPreBreachStepCompleting's twin with the other origin: a
// step claimed BEFORE `docket run pause`, completing AFTER it, must not resume
// the run.
//
// `MoveRun` pauses only the RUN row — it parks no step — so `reconcileRun`'s
// "parked" count reads 0 for an operator-paused run, exactly as it does for a
// budget-paused one, and its default branch flipped the run back to `active`
// the moment any in-flight step routed. RUN-31 hit this for real: paused at seq
// 3054, a verb-less `run-resumed` at seq 3077, and then a four-judge review, a
// synthesize, a reconcile, and two fresh votes ran unattended past the pause.
func TestOperatorPauseSurvivesAnInFlightStepCompleting(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID, _ := budgetRun(t, conn, 0)

	claimAndComplete(t, conn, e, "implement@0", "summary", "")

	var siblings []string
	for _, s := range runSteps(t, conn, runID) {
		if s.StepName == "review" && s.Status == db.StepPending {
			siblings = append(siblings, s.Instance)
		}
	}
	if len(siblings) < 2 {
		t.Fatalf("the fixture expanded %d claimable review siblings; this case "+
			"needs at least 2 so one is still unfinished after the other routes",
			len(siblings))
	}

	// Claimed BEFORE the pause, completed AFTER it — the in-flight step the
	// pause skill's own contract says may finish.
	inFlight := claimInstance(t, conn, siblings[0], nowMS)

	paused, _, err := MoveRun(conn, runID, "pause", model.RunWaitingHuman,
		[]model.RunStatus{model.RunActive}, "operator asked to stop", nowMS+1)
	testsupport.Must(t, err, "MoveRun pause: %v", err)
	if paused.Status != model.RunWaitingHuman {
		t.Fatalf("premise: run is %s after the pause, want %s",
			paused.Status, model.RunWaitingHuman)
	}
	if got := pauseOrigin(t, conn, runID); got != model.RunPauseOriginOperator {
		t.Fatalf("pause_origin = %q after `run pause`, want %q",
			got, model.RunPauseOriginOperator)
	}

	// Routes through reconcileIssueAndRun -> reconcileRun with `parked == 0`
	// (nothing is waiting-human; this step just went `done`) and
	// `unfinished > 0` (the other sibling is pending) — the default branch.
	err = e.CompleteStep(conn, stepIDByInstance(t, conn, siblings[0]), CompleteOptions{
		Token: inFlight.Token, Artifact: []byte("findings"), NowMS: nowMS + 2,
	})
	testsupport.Must(t, err, "completing the in-flight claim: %v", err)

	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunWaitingHuman {
		t.Errorf("run is %s after an in-flight step completed, want %s — an "+
			"operator's pause must stand until an operator verb moves it, not "+
			"be undone because a step finished", run.Status, model.RunWaitingHuman)
	}
	if run.Reason != "operator asked to stop" {
		t.Errorf("run reason = %q, want the operator's", run.Reason)
	}
	if got := pauseOrigin(t, conn, runID); got != model.RunPauseOriginOperator {
		t.Errorf("pause_origin = %q after the completion, want %q", got,
			model.RunPauseOriginOperator)
	}
	if n := countRunEvents(t, conn, runID, EventRunResumed); n != 0 {
		t.Errorf("%d %s events after an in-flight step completed, want 0 — "+
			"nothing here is an operator's resume", n, EventRunResumed)
	}

	// R1 still refuses the work the pause was typed to stop.
	if _, err := ClaimStep(conn, stepIDByInstance(t, conn, siblings[1]),
		ClaimOptions{Owner: "worker2", NowMS: nowMS + 3}); err == nil {
		t.Error("a claim was admitted on a paused run; the pause bought nothing")
	}
}

// TestResumeReleasesTheOperatorHold is the other half of the guard: the origin
// is a record of a STANDING park, not a permanent mark. `run resume` clears it,
// and the rollup goes back to its ordinary business — otherwise the fix for
// DKT-305 would wedge every run it protected.
func TestResumeReleasesTheOperatorHold(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID, _ := budgetRun(t, conn, 0)

	_, _, err := MoveRun(conn, runID, "pause", model.RunWaitingHuman,
		[]model.RunStatus{model.RunActive}, "stopping for now", nowMS)
	testsupport.Must(t, err, "MoveRun pause: %v", err)

	resumed, _, err := MoveRun(conn, runID, "resume", model.RunActive,
		[]model.RunStatus{model.RunWaitingHuman}, "carrying on", nowMS+1)
	testsupport.Must(t, err, "MoveRun resume: %v", err)
	if resumed.Status != model.RunActive {
		t.Fatalf("run is %s after resume, want %s", resumed.Status, model.RunActive)
	}
	if got := pauseOrigin(t, conn, runID); got != model.RunPauseOriginNone {
		t.Fatalf("pause_origin = %q after `run resume`, want it cleared", got)
	}

	claimAndComplete(t, conn, e, "implement@0", "summary", "")

	run, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if run.Status != model.RunActive {
		t.Errorf("run is %s after a resumed run's step completed, want %s",
			run.Status, model.RunActive)
	}
}

// TestStepParkStillAutoResumes is the control, and it is the reason the guard
// reads an ORIGIN rather than the bare fact of `waiting-human`.
//
// A park the rollup created because a STEP is parked is one the rollup must
// also resolve: when the step moves, the run returns to `active` on its own. A
// fix that made every `waiting-human` run wait for `run resume` would trade
// DKT-305 for a run that needs an operator verb after every retry.
func TestStepParkStillAutoResumes(t *testing.T) {
	const src = `
[pipeline]
name = "parks"
version = 1

[match]
kind = ["task"]

[[step]]
name = "flaky"
after = []
executor = "w"
emits = "out"
max_attempts = 1
on_fail = "waiting-human"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "parks.toml")
	issue := createIssue(t, conn, "parked", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "flaky@0")

	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, e.FailStep(conn, id, claim.Token, "gave up", "", nowMS),
		"fail: %v", err)

	parked, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if parked.Status != model.RunWaitingHuman {
		t.Fatalf("premise: run is %s with a parked step, want %s",
			parked.Status, model.RunWaitingHuman)
	}
	if got := pauseOrigin(t, conn, run.ID); got != model.RunPauseOriginNone {
		t.Fatalf("pause_origin = %q for a STEP-level park, want it empty — the "+
			"rollup parked this run and the rollup un-parks it", got)
	}

	testsupport.Must(t, e.ResolveStep(conn, id, ResolveSkip, "not worth it", nowMS+1),
		"resolve: %v", err)

	after, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun after resolve: %v", err)
	if after.Status == model.RunWaitingHuman {
		t.Errorf("run is still %s after its only parked step resolved — a "+
			"step-level park must un-park itself without an operator verb",
			after.Status)
	}
}
