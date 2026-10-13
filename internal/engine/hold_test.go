package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// holdRun activates a run bound to a conductor capability, returning the run,
// the capability, and the ready step's id.
func holdRun(t *testing.T, conn *sql.DB) (runID int, token string, stepID int) {
	t.Helper()
	runID, _, token = runFactRun(t, conn)
	return runID, token, stepIDByInstance(t, conn, "implement@0")
}

func heldStepRow(t *testing.T, conn *sql.DB, stepID int) (status, class, reason string) {
	t.Helper()
	err := conn.QueryRow(
		`SELECT status, COALESCE(park_class, ''), COALESCE(park_reason, '') FROM steps WHERE id = ?`,
		stepID).Scan(&status, &class, &reason)
	testsupport.Must(t, err, "reading step %d: %v", stepID, err)
	return status, class, reason
}

func otherStatuses(t *testing.T, conn *sql.DB, runID, except int) map[int]string {
	t.Helper()
	rows, err := conn.Query(`SELECT id, status FROM steps WHERE run_id = ? AND id != ?`, runID, except)
	testsupport.Must(t, err, "listing steps: %v", err)
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var id int
		var status string
		testsupport.Must(t, rows.Scan(&id, &status), "scanning: %v", nil)
		out[id] = status
	}
	return out
}

func heldEvents(t *testing.T, conn *sql.DB, runID int) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range factEvents(t, conn, runID, EventStepHeld) {
		var data map[string]any
		testsupport.Must(t, json.Unmarshal(e.Data, &data), "decoding: %v", nil)
		out = append(out, data)
	}
	return out
}

// TestHoldStepParksOneReadyStep is DKT-3287 criterion 1.
func TestHoldStepParksOneReadyStep(t *testing.T) {
	conn := mustDB(t)
	runID, token, stepID := holdRun(t, conn)
	others := otherStatuses(t, conn, runID, stepID)

	err := HoldStep(conn, stepID, HoldOptions{
		Reason: "classifier-stopped twice", By: factBy, Token: token, NowMS: nowMS,
	})
	testsupport.Must(t, err, "HoldStep: %v", err)

	status, class, reason := heldStepRow(t, conn, stepID)
	if status != db.StepWaitingHuman || class != string(db.ParkClassHeld) ||
		!strings.Contains(reason, "classifier-stopped twice") {
		t.Errorf("held step = (%s, %s, %q), want waiting-human, held, the reason", status, class, reason)
	}
	events := heldEvents(t, conn, runID)
	if len(events) != 1 || events[0]["reason"] != "classifier-stopped twice" ||
		events[0]["actor"] != factBy.Actor {
		t.Errorf("step-held events = %v, want one carrying the reason and actor", events)
	}
	if after := otherStatuses(t, conn, runID, stepID); len(after) != len(others) {
		t.Errorf("other steps changed: %v -> %v", others, after)
	} else {
		for id, s := range others {
			if after[id] != s {
				t.Errorf("step %d moved %s -> %s; a hold touches one step", id, s, after[id])
			}
		}
	}
	for _, row := range openDispatch(t, conn, runID, 0, nowMS+1).Rows {
		if row.Instance == "implement@0" {
			t.Error("dispatch open still offers the held step")
		}
	}
}

// TestHoldStepRequiresTheConductorCapability is DKT-3287 criterion 2.
func TestHoldStepRequiresTheConductorCapability(t *testing.T) {
	conn := mustDB(t)
	runID, _, stepID := holdRun(t, conn)
	for _, tc := range []struct {
		token string
		want  ErrorCode
	}{{"", CodeValidation}, {"an-executor-lease", CodeAuth}} {
		err := HoldStep(conn, stepID, HoldOptions{Reason: "r", By: factBy, Token: tc.token, NowMS: nowMS})
		if !hasCode(err, tc.want) {
			t.Errorf("token %q: err = %v, want %s", tc.token, err, tc.want)
		}
	}
	if status, _, _ := heldStepRow(t, conn, stepID); status != db.StepPending {
		t.Errorf("a refused hold left the step %s, want pending", status)
	}
	if n := len(heldEvents(t, conn, runID)); n != 0 {
		t.Errorf("%d step-held events after refusals, want 0", n)
	}
}

// TestHoldStepOnAnUnboundRunNeedsNoToken is DKT-3287 criterion 3.
func TestHoldStepOnAnUnboundRunNeedsNoToken(t *testing.T) {
	conn := mustDB(t)
	runID, _, stepID := holdRun(t, conn)
	execSQL(t, conn, `UPDATE runs SET conductor_token_hash = NULL WHERE id = ?`, runID)

	err := HoldStep(conn, stepID, HoldOptions{Reason: "r", By: factBy, NowMS: nowMS})
	testsupport.Must(t, err, "HoldStep on an unbound run: %v", err)
	if status, _, _ := heldStepRow(t, conn, stepID); status != db.StepWaitingHuman {
		t.Errorf("step is %s, want waiting-human", status)
	}
}

// TestHoldStepRefusesAStepThatIsNotReady is DKT-3287 criterion 4.
func TestHoldStepRefusesAStepThatIsNotReady(t *testing.T) {
	conn := mustDB(t)
	runID, token, stepID := holdRun(t, conn)
	hold := func() error {
		return HoldStep(conn, stepID, HoldOptions{Reason: "r", By: factBy, Token: token, NowMS: nowMS})
	}
	testsupport.Must(t, hold(), "first hold: %v", nil)
	if err := hold(); err == nil {
		t.Error("a waiting-human step was held again")
	}
	execSQL(t, conn, `UPDATE steps SET status = 'skipped' WHERE id = ?`, stepID)
	if err := hold(); err == nil {
		t.Error("a terminal step was held")
	}
	if status, _, _ := heldStepRow(t, conn, stepID); status != db.StepSkipped {
		t.Errorf("a refused hold moved the terminal step to %s", status)
	}
	if n := len(heldEvents(t, conn, runID)); n != 1 {
		t.Errorf("%d step-held events, want only the first hold's", n)
	}
}

// TestHoldStepResolvesLikeAnyPark is DKT-3287 criterion 5.
func TestHoldStepResolvesLikeAnyPark(t *testing.T) {
	for as, want := range map[string]string{"retry": db.StepPending, "skip": db.StepSkipped} {
		t.Run(as, func(t *testing.T) {
			conn := mustDB(t)
			_, token, stepID := holdRun(t, conn)
			testsupport.Must(t, HoldStep(conn, stepID, HoldOptions{
				Reason: "r", By: factBy, Token: token, NowMS: nowMS}), "hold: %v", nil)
			_, err := testEngine().ResolveStepWith(conn, stepID, ResolveOptions{
				As: as, Note: "operator decided", By: testBy, Under: testUnder,
				Token: token, NowMS: nowMS + 1,
			})
			testsupport.Must(t, err, "resolve --as %s: %v", as, err)
			if status, _, _ := heldStepRow(t, conn, stepID); status != want {
				t.Errorf("after resolve --as %s the step is %s, want %s", as, status, want)
			}
		})
	}
}
