package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-304: RUN-30's operator paused mid-wave and the run resumed itself one
// millisecond after a step routed, with an event that named nobody.

// TestOperatorPauseSurvivesAnActionStepRouting is the path DKT-304 named and
// TestOperatorPauseSurvivesAnInFlightStepCompleting does not exercise: an
// ACTION step, driven by the engine rather than completed by a claimant, and
// routing through the same rollup.
//
// The pause_origin guard covers it — this test is what says so, rather than
// leaving the claim resting on the executor path alone.
func TestOperatorPauseSurvivesAnActionStepRouting(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID, _ := budgetRun(t, conn, 0)

	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	completeReviewFanout(t, conn, e, 0)
	// `unheldPayload` trips no hold, so `reconcile` ROUTES rather than parking
	// at `gated` — which is what puts this drive through reconcileRun.
	claimAndComplete(t, conn, e, "synthesize@0", "the synthesis", unheldPayload)

	_, _, err := MoveRun(conn, runID, "pause", model.RunWaitingHuman,
		[]model.RunStatus{model.RunActive}, "operator asked to stop", nowMS+1)
	testsupport.Must(t, err, "pause: %v", err)

	driveAction(t, conn, e, "reconcile@0")

	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepDone {
		t.Fatalf("premise: reconcile@0 = %q, want it to have ROUTED", got)
	}
	if got := runStatusOf(t, conn, runID); got != string(model.RunWaitingHuman) {
		t.Errorf("run = %q after an action step routed under a pause, want %q "+
			"— the pause the operator asked for must stand until an operator "+
			"verb moves it", got, model.RunWaitingHuman)
	}
}

// TestRollupLifecycleEventsNameTheRollup is DKT-304's other half, and it stands
// whether or not any pause is involved.
//
// Every kind reconcileRun writes has an operator verb that writes the same
// kind, and `MoveRun` carries `from`, `to`, and the operator's reason. The
// rollup wrote `{}`, so an engine-decided resume was byte-identical in the feed
// to a person typing `docket run resume` — which is exactly what RUN-30's
// operator could not tell apart.
func TestRollupLifecycleEventsNameTheRollup(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID, issue := budgetRun(t, conn, 0)

	// A step-level park pauses the run through the rollup...
	driveToVerify(t, conn, e, 0)
	claimAndComplete(t, conn, e, "verify@0", "report", unverifiablePayload)
	if got := runStatusOf(t, conn, runID); got != string(model.RunWaitingHuman) {
		t.Fatalf("premise: run = %q after the park, want %q", got, model.RunWaitingHuman)
	}
	assertRollupEvent(t, conn, runID, EventRunPaused)

	// ...and resolving it resumes through the same rollup.
	verifyID := stepIDByInstance(t, conn, "verify@0")
	testsupport.Must(t, e.ResolveStep(conn, verifyID, ResolveOverridePass, "accepted", nowMS),
		"resolve --as override-pass: %v", nil)
	if got := runStatusOf(t, conn, runID); got != string(model.RunActive) {
		t.Fatalf("premise: run = %q after the resolution, want %q", got, model.RunActive)
	}
	assertRollupEvent(t, conn, runID, EventRunResumed)
	_ = issue
}

// TestOperatorResumeIsNotMarkedAsARollup is the lower bound: the marker must
// distinguish, which means an operator's own verb must not carry it.
func TestOperatorResumeIsNotMarkedAsARollup(t *testing.T) {
	conn := mustDB(t)
	runID, _ := budgetRun(t, conn, 0)

	_, _, err := MoveRun(conn, runID, "pause", model.RunWaitingHuman,
		[]model.RunStatus{model.RunActive}, "stepping away", nowMS+1)
	testsupport.Must(t, err, "pause: %v", err)
	_, _, err = MoveRun(conn, runID, "resume", model.RunActive,
		[]model.RunStatus{model.RunWaitingHuman}, "back at the desk", nowMS+2)
	testsupport.Must(t, err, "resume: %v", err)

	data := lastEventData(t, conn, runID, EventRunResumed)
	if strings.Contains(data, runRollupReason) {
		t.Errorf("an operator's `run resume` is marked as a rollup: %s", data)
	}
	if !strings.Contains(data, "back at the desk") {
		t.Errorf("the operator's reason is not in the event: %s", data)
	}
}

// assertRollupEvent requires the newest event of a kind to name the rollup.
func assertRollupEvent(t *testing.T, conn *sql.DB, runID int, kind string) {
	t.Helper()
	data := lastEventData(t, conn, runID, kind)
	if !strings.Contains(data, runRollupReason) {
		t.Errorf("the %s the rollup wrote carries %s — nothing in the feed "+
			"says no verb was typed", kind, data)
	}
}

// lastEventData reads the newest event of a kind for a run.
func lastEventData(t *testing.T, conn *sql.DB, runID int, kind string) string {
	t.Helper()
	var data string
	err := conn.QueryRow(
		`SELECT COALESCE(data, '') FROM events
		  WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, kind).Scan(&data)
	testsupport.Must(t, err, "reading the newest %s: %v", kind, err)
	return data
}
