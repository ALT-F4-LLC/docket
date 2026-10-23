package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4273: an operator's hold on an issue lives in the engine. `HoldIssue`
// parks every pending step of the issue, ready or staged, and a manifest lists
// them as held with the reason instead of the conductor filtering them out.

// holdIssueRun activates a run over the claim-refused fixture, whose single
// issue holds two ready steps (one, two), one staged step (later, after one),
// and a human step, and returns the run, the issue, and the conductor token.
func holdIssueRun(t *testing.T, conn *sql.DB) (runID, issueID int, token string) {
	t.Helper()
	registerSource(t, conn, []byte(claimRefusedFixtureSrc), "claim-refused.toml")
	issueID = createIssue(t, conn, "hold issue", "body", "task", nil)
	run := startRun(t, conn, issueID)
	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS})
	testsupport.Must(t, err, "activate: %v", err)
	if result.ConductorToken == "" {
		t.Fatal("premise: activation minted no conductor capability")
	}
	return run.ID, issueID, result.ConductorToken
}

func manifestInstances(m *Manifest) map[string]bool {
	got := map[string]bool{}
	for _, r := range m.Rows {
		got[r.Instance] = true
	}
	return got
}

// TestHoldIssueWithholdsReadyAndStagedRowsUntilReleased is the acceptance
// check: hold an issue with a ready and a staged step; dispatch open returns no
// row for either and lists both as held with the reason; after release both
// rows return.
func TestHoldIssueWithholdsReadyAndStagedRowsUntilReleased(t *testing.T) {
	conn := mustDB(t)
	runID, issueID, token := holdIssueRun(t, conn)

	const reason = "operator note 146: keep holding"
	held, err := HoldIssue(conn, runID, issueID, HoldOptions{
		Reason: reason, By: factBy, Token: token, NowMS: nowMS})
	testsupport.Must(t, err, "HoldIssue: %v", err)
	if len(held) < 2 {
		t.Fatalf("held %d steps; want the ready and the staged one at least", len(held))
	}

	m := openDispatch(t, conn, runID, 0, nowMS)
	if got := manifestInstances(m); got["one@0"] || got["later@0"] {
		t.Errorf("a held issue's rows were offered: %v", got)
	}
	listed := map[string]string{}
	for _, h := range m.Held {
		listed[h.Instance] = h.Reason
	}
	for _, instance := range []string{"one@0", "later@0"} {
		if listed[instance] != reason {
			t.Errorf("held list for %s = %q; want the reason %q", instance, listed[instance], reason)
		}
	}
	if n := len(heldEvents(t, conn, runID)); n != len(held) {
		t.Errorf("%d step-held events; want one per held step (%d)", n, len(held))
	}

	closeOpenDispatch(t, conn, runID)
	for _, id := range held {
		_, err := testEngine().ResolveStepWith(conn, id, ResolveOptions{
			As: "retry", Note: "released", By: testBy, Under: testUnder,
			Token: token, NowMS: nowMS + 1,
		})
		testsupport.Must(t, err, "release %d: %v", id, err)
	}
	m = openDispatch(t, conn, runID, 0, nowMS+2)
	if got := manifestInstances(m); !got["one@0"] || !got["two@0"] || !got["later@0"] {
		t.Errorf("rows after release = %v; want one@0, two@0 and later@0 back", got)
	}
	if len(m.Held) != 0 {
		t.Errorf("held list after release = %+v; want empty", m.Held)
	}
}

// closeOpenDispatch closes the manifest so the next open is legal.
func closeOpenDispatch(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()
	_, err := NewEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS+1)
	testsupport.Must(t, err, "closing the open dispatch: %v", err)
}

// TestHoldStepAcceptsAStagedStep: `step hold` parks a pending step that is not
// yet ready, the case DKT-3287 left to a hand-typed filter.
func TestHoldStepAcceptsAStagedStep(t *testing.T) {
	conn := mustDB(t)
	_, _, token := holdIssueRun(t, conn)
	staged := stepIDByInstance(t, conn, "later@0")

	err := HoldStep(conn, staged, HoldOptions{Reason: "r", By: factBy, Token: token, NowMS: nowMS})
	testsupport.Must(t, err, "HoldStep on a staged step: %v", err)
	if status, class, _ := heldStepRow(t, conn, staged); status != db.StepWaitingHuman || class != string(db.ParkClassHeld) {
		t.Errorf("staged step is %s/%s; want waiting-human/held", status, class)
	}
}

// TestHoldIssueRefusesInFlightStepsAndChangesNothing: a claimed step of the
// issue stops the hold before any step moves.
func TestHoldIssueRefusesInFlightStepsAndChangesNothing(t *testing.T) {
	conn := mustDB(t)
	runID, issueID, token := holdIssueRun(t, conn)
	claimInstance(t, conn, "one@0", nowMS)
	before := otherStatuses(t, conn, runID, 0)

	_, err := HoldIssue(conn, runID, issueID, HoldOptions{
		Reason: "r", By: factBy, Token: token, NowMS: nowMS})
	if err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("HoldIssue past a claimed step = %v; want an in-flight refusal", err)
	}
	after := otherStatuses(t, conn, runID, 0)
	for id, status := range before {
		if after[id] != status {
			t.Errorf("step %d moved from %s to %s by a refused hold", id, status, after[id])
		}
	}
	if n := len(heldEvents(t, conn, runID)); n != 0 {
		t.Errorf("%d step-held events after a refused hold", n)
	}
}

// TestHoldIssueNeedsAReasonAndTheCapability: the two guards `step hold` has.
func TestHoldIssueNeedsAReasonAndTheCapability(t *testing.T) {
	conn := mustDB(t)
	runID, issueID, token := holdIssueRun(t, conn)

	if _, err := HoldIssue(conn, runID, issueID, HoldOptions{
		By: factBy, Token: token, NowMS: nowMS}); err == nil {
		t.Error("an issue was held with no reason")
	}
	if _, err := HoldIssue(conn, runID, issueID, HoldOptions{
		Reason: "r", By: factBy, Token: "wrong", NowMS: nowMS}); err == nil {
		t.Error("an issue was held with the wrong capability")
	}
	if n := len(heldEvents(t, conn, runID)); n != 0 {
		t.Errorf("%d step-held events after refused holds", n)
	}
}
