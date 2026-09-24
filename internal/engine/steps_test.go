package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestParkReasonIsAlwaysEngineComposed is DKT-2529.
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
		if !strings.Contains(view.ParkReason, string(db.ParkClassAttemptsExhausted)) {
			t.Errorf("park_reason = %q, want it to name the park class %q",
				view.ParkReason, db.ParkClassAttemptsExhausted)
		}
	})

	// (ii) A worker's note is the worker's; it explains the failure on the
	// step-failed event and never becomes the engine's account of the park.
	t.Run("attempts-exhausted with a worker note", func(t *testing.T) {
		conn := mustDB(t)
		_, stepID := failToExhaustion(t, conn, workerNote)

		view, err := LoadStepView(conn, stepID, nowMS)
		testsupport.Must(t, err, "LoadStepView: %v", err)
		if view.ParkReason == "" {
			t.Fatal("park_reason is empty on a parked step")
		}
		if !strings.Contains(view.ParkReason, string(db.ParkClassAttemptsExhausted)) {
			t.Errorf("park_reason = %q, want it to name the park class %q",
				view.ParkReason, db.ParkClassAttemptsExhausted)
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
		testsupport.Must(t,
			e.DecideStep(conn, stepIDByInstance(t, conn, "gate@0"), false,
				"first pass needs work", nowMS),
			"first rejection: %v", nil)
		claimAndComplete(t, conn, e, "fix@1", "the fix", "")

		// Round 2 would exceed the bound, so the entry is refused and the
		// gate parks.
		gateID := stepIDByInstance(t, conn, "gate@1")
		testsupport.Must(t, e.DecideStep(conn, gateID, false, rejectNote, nowMS),
			"second rejection: %v", nil)

		step, err := db.GetStep(conn, gateID)
		testsupport.Must(t, err, "GetStep: %v", err)
		if step.Status != db.StepWaitingHuman {
			t.Fatalf("gate@1 is %q, want %q — the fixture must park on the bound",
				step.Status, db.StepWaitingHuman)
		}

		view, err := LoadStepView(conn, gateID, nowMS)
		testsupport.Must(t, err, "LoadStepView: %v", err)
		if !strings.Contains(view.ParkReason, "fix-loop-exhausted") {
			t.Errorf("park_reason = %q, want it to name the refused loop as "+
				"fix-loop-exhausted", view.ParkReason)
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
	testsupport.Must(t,
		e.FailStep(conn, stepID, claim.Token, note, "", nowMS), "fail: %v", err)

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
