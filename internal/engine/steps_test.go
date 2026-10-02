package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestParkReasonIsAlwaysEngineComposed pins park_reason as the engine's text.
//
// Five read surfaces document `park_reason` as the ENGINE's text for why a
// step parked, but two park sites wrote whatever the caller passed: the
// attempts-exhausted park stored the failing worker's `--note` verbatim (an
// empty note left a parked row with no reason at all, which `step show` then
// omitted as if the step had never parked), and a rejection whose fix loop was
// refused stored `<reject note>; <bound reason>`. Each case below drives one of
// those parks and asserts that the reason is the engine's — it names the park
// class and carries none of the caller's words — while the caller's words are
// still readable from the event that already carried them, so the change
// removes nothing an operator could read before.
func TestParkReasonIsAlwaysEngineComposed(t *testing.T) {
	const workerNote = "worker-note-sentinel"
	const rejectNote = "reject-note-sentinel"
	// The fixtures spend a budget of one, so the count is fixed.
	exhaustedReason := string(db.ParkClassAttemptsExhausted) + ": 1 of 1"

	// (i) An exhausted budget with nothing said about the last failure still
	// parks with a reason: the budget is the reason, and the engine knows it.
	t.Run("attempts-exhausted with an empty note", func(t *testing.T) {
		conn := mustDB(t)
		_, stepID := failToExhaustion(t, conn, "")

		view, err := LoadStepView(conn, stepID, nowMS)
		testsupport.Must(t, err, "LoadStepView: %v", err)
		if view.ParkReason == "" {
			t.Fatal("park_reason is empty on a parked step; the engine must " +
				"compose the reason rather than store the worker's (empty) note")
		}
		if view.ParkReason != exhaustedReason {
			t.Errorf("park_reason = %q, want %q", view.ParkReason, exhaustedReason)
		}
	})

	// (ii) A worker's note is the worker's; it explains the failure on the
	// step-failed event and never becomes the engine's account of the park.
	t.Run("attempts-exhausted with a worker note", func(t *testing.T) {
		conn := mustDB(t)
		_, stepID := failToExhaustion(t, conn, workerNote)

		view, err := LoadStepView(conn, stepID, nowMS)
		testsupport.Must(t, err, "LoadStepView: %v", err)
		if view.ParkReason != exhaustedReason {
			t.Errorf("park_reason = %q, want %q", view.ParkReason, exhaustedReason)
		}
		if strings.Contains(view.ParkReason, workerNote) {
			t.Errorf("park_reason = %q carries the worker's note; the field is "+
				"the engine's text and the note has its own event", view.ParkReason)
		}

		// AC2: the note is still readable where it always was.
		failed := latestEventData(t, conn, stepID, EventStepFailed)
		if !strings.Contains(failed, workerNote) {
			t.Errorf("step-failed data = %q, want it to carry the worker's note %q",
				failed, workerNote)
		}
	})

	// (iii) A rejection whose fix loop is refused parks on the bound, and the
	// bound is the reason. The operator's reject note explains the rejection
	// on the step-rejected event, not the park.
	t.Run("human reject whose fix loop is refused", func(t *testing.T) {
		conn := mustDB(t)
		activateSrc(t, conn, loopBoundSrc, "loop-bound.toml")
		e := testEngine()

		claimAndComplete(t, conn, e, "work@0", "the work", "")

		// Round 1 is within `max_fix_loops = 1`: this rejection enters the
		// loop, mints `fix@1`, and re-instantiates the gate.
		err := e.DecideStep(conn, stepIDByInstance(t, conn, "gate@0"), false,
			"first pass needs work", nowMS)
		testsupport.Must(t, err, "first rejection: %v", err)
		claimAndComplete(t, conn, e, "fix@1", "the fix", "")

		// Round 2 would exceed the bound, so the entry is refused and the
		// gate parks.
		gateID := stepIDByInstance(t, conn, "gate@1")
		err = e.DecideStep(conn, gateID, false, rejectNote, nowMS)
		testsupport.Must(t, err, "second rejection: %v", err)

		step, err := db.GetStep(conn, gateID)
		testsupport.Must(t, err, "GetStep: %v", err)
		if step.Status != db.StepWaitingHuman {
			t.Fatalf("gate@1 is %q, want %q — the fixture must park on the bound",
				step.Status, db.StepWaitingHuman)
		}

		view, err := LoadStepView(conn, gateID, nowMS)
		testsupport.Must(t, err, "LoadStepView: %v", err)
		if !strings.HasPrefix(view.ParkReason, "fix-loop-exhausted: ") ||
			!strings.Contains(view.ParkReason, "max_fix_loops") {
			t.Errorf("park_reason = %q, want `fix-loop-exhausted: ` followed by "+
				"the bound's own sentence", view.ParkReason)
		}
		if strings.Contains(view.ParkReason, rejectNote) {
			t.Errorf("park_reason = %q carries the operator's reject note; the "+
				"field is the engine's text and the note has its own event",
				view.ParkReason)
		}

		// AC2: the reject note is still readable where it always was.
		rejected := latestEventData(t, conn, gateID, EventStepRejected)
		if !strings.Contains(rejected, rejectNote) {
			t.Errorf("step-rejected data = %q, want it to carry the reject note %q",
				rejected, rejectNote)
		}
	})

	// (iv) An exhausted budget whose `on_fail` names a triage panel that has
	// already ruled on this ordinal parks for an operator. The panel's
	// sentence is the engine's and names the cause; the park keeps the
	// budget's class, and the reason names both.
	t.Run("attempts-exhausted after a spent triage panel", func(t *testing.T) {
		conn := mustDB(t)
		registerVoteRule(t, conn, "majority", "0.5", "")
		src := strings.Replace(triageSrc, "APPROVED", "retry", 1)
		src = strings.Replace(src, "REJECTED", "abandon-issue", 1)
		src = strings.Replace(src, "gates = [\"build\"]\non_fail = \"triage\"",
			"max_attempts = 1\non_fail = \"triage\"", 1)
		if !strings.Contains(src, "max_attempts = 1") {
			t.Fatal("fixture edit missed: implement must carry max_attempts")
		}
		activateSrc(t, conn, src, "triage-exhausted.toml")
		e := testEngine()
		stepID := stepIDByInstance(t, conn, "implement@0")

		// First exhaustion: the step suspends for the panel, which rules
		// `retry` and refreshes the budget.
		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "x", NowMS: nowMS})
		testsupport.Must(t, err, "first claim: %v", err)
		err = e.FailStep(conn, stepID, claim.Token, "first failure", "", nowMS)
		testsupport.Must(t, err, "first fail: %v", err)
		if got := stepStatus(t, conn, "implement@0"); got != db.StepGated {
			t.Fatalf("implement@0 is %q after its first exhaustion, want %q — "+
				"the fixture must suspend for the panel", got, db.StepGated)
		}
		runID := mustStep(t, conn, "implement@0").RunID
		err = e.DriveRunLifecycles(conn, runID, nowMS)
		testsupport.Must(t, err, "opening the panel: %v", err)
		castTriage(t, conn, e, model.VerdictApprove)

		// Second exhaustion at the same ordinal: the panel is spent, so the
		// step parks.
		claim, err = ClaimStep(conn, stepID, ClaimOptions{Owner: "x", NowMS: nowMS})
		testsupport.Must(t, err, "second claim: %v", err)
		err = e.FailStep(conn, stepID, claim.Token, workerNote, "", nowMS)
		testsupport.Must(t, err, "second fail: %v", err)
		assertParkClass(t, conn, park{runID: runID, stepID: stepID},
			db.ParkClassAttemptsExhausted)

		view, err := LoadStepView(conn, stepID, nowMS)
		testsupport.Must(t, err, "LoadStepView: %v", err)
		if !strings.HasPrefix(view.ParkReason, exhaustedReason+"; ") {
			t.Errorf("park_reason = %q, want it to lead with %q",
				view.ParkReason, exhaustedReason+"; ")
		}
		if !strings.Contains(view.ParkReason, "already ruled on this ordinal") {
			t.Errorf("park_reason = %q, want the engine's spent-panel sentence",
				view.ParkReason)
		}
		if strings.Contains(view.ParkReason, workerNote) {
			t.Errorf("park_reason = %q carries the worker's note", view.ParkReason)
		}
		// The worker's note survives beside the panel's sentence.
		failed := latestEventData(t, conn, stepID, EventStepFailed)
		if !strings.Contains(failed, workerNote) ||
			!strings.Contains(failed, "already ruled on this ordinal") {
			t.Errorf("step-failed data = %q, want both the worker's note %q and "+
				"the spent-panel sentence", failed, workerNote)
		}
		step, err := db.GetStep(conn, stepID)
		testsupport.Must(t, err, "GetStep: %v", err)
		if !strings.Contains(step.Routing, workerNote) {
			t.Errorf("routing = %q lost the worker's note", step.Routing)
		}
	})
}

// TestParkReasonIsNeverEmpty pins the write's fallback: a park whose routing
// reason is blank still records a reason, the class word, so `step show` never
// omits the field on a step that parked. A failed gate that printed nothing is
// the real case — its output is the routing reason.
func TestParkReasonIsNeverEmpty(t *testing.T) {
	conn := mustDB(t)
	activateSrc(t, conn, gatedSrc, "gated-silent.toml")
	e := testEngine()
	e.Gates = verdictGates{verdict: VerdictFail}

	stepID := stepIDByInstance(t, conn, "work@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the work"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	view, err := LoadStepView(conn, stepID, nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.Step.Status != db.StepWaitingHuman {
		t.Fatalf("work@0 is %q, want %q — the fixture must park on its gate",
			view.Step.Status, db.StepWaitingHuman)
	}
	if view.ParkReason == "" {
		t.Fatal("park_reason is empty on a step parked by a silent gate")
	}
	if !strings.Contains(view.ParkReason, string(view.ParkClass)) {
		t.Errorf("park_reason = %q, want it to name the park class %q",
			view.ParkReason, view.ParkClass)
	}
}

// failToExhaustion activates the single-attempt fixture, claims its only step,
// and fails it with `note`, which spends the budget and parks the step. It
// returns the run and step ids.
func failToExhaustion(t *testing.T, conn *sql.DB, note string) (int, int) {
	t.Helper()
	runID := activateSrc(t, conn, exhaustedSrc, "exhausted.toml")
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "work@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, stepID, claim.Token, note, "", nowMS)
	testsupport.Must(t, err, "fail: %v", err)

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepWaitingHuman {
		t.Fatalf("work@0 is %q, want %q — the fixture must park on its budget",
			step.Status, db.StepWaitingHuman)
	}
	return runID, stepID
}

// latestEventData reads the most recent event of `kind` recorded against a
// step, as the feed would show it.
func latestEventData(t *testing.T, conn *sql.DB, stepID int, kind string) string {
	t.Helper()
	var data string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE step_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		stepID, kind).Scan(&data)
	testsupport.Must(t, err, "reading the %s event: %v", kind, err)
	return data
}
