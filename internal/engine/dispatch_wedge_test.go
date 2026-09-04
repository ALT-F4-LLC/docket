package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-315: `usage-rows-missing` could wedge a run permanently. Four separate
// defects made that possible, and each has its own case here.

// TestTerminatedUnrunStepIsNotADiscrepancy is the root cause.
//
// D5's "a step that was never claimed produced nothing to report" was written
// as a list of two statuses — `skipped` and `superseded` — and `failed-routed`
// was missing from it. That is the status the abandon cascade and `run abandon
// --issue` write onto steps expanded and then terminated UNRUN. Harness
// RUN-32's STEP-1070 was failed-routed at attempt 0, with zero usage rows and
// zero events mentioning it anywhere in the run, and the probe refused the
// whole run over it.
func TestTerminatedUnrunStepIsNotADiscrepancy(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)

	// The cascade's shape: terminal, past activation, no ledger row, and never
	// claimed — `attempt` counts claims and this step had none.
	id := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn,
		`UPDATE steps SET status = ?, updated_at_ms = ?, attempt = 0 WHERE id = ?`,
		db.StepFailedRouted, nowMS+1000, id)

	for _, d := range discrepanciesAt(t, conn, runID, nowMS) {
		if d.Kind == DiscrepancyMissingUsage {
			t.Errorf("a step terminated before it ever claimed is a %s "+
				"discrepancy: %+v — it never spawned and never spent, so "+
				"there is nothing it could report", d.Kind, d)
		}
	}
	_, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "`next` refused over an unrun step: %v", err)
}

// TestRanWithoutReportingIsStillADiscrepancy is the lower bound of the fix: a
// step a worker actually held and that reported nothing is exactly what D2 is
// for, and the never-claimed exemption must not swallow it.
func TestRanWithoutReportingIsStillADiscrepancy(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)

	id := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn,
		`UPDATE steps SET status = ?, updated_at_ms = ?, attempt = 1 WHERE id = ?`,
		db.StepDone, nowMS+1000, id)

	var found bool
	for _, d := range discrepanciesAt(t, conn, runID, nowMS) {
		if d.Kind == DiscrepancyMissingUsage {
			found = true
		}
	}
	if !found {
		t.Error("a step that was claimed, ran, and reported no usage is not a " +
			"discrepancy; that is precisely what D2 exists to catch")
	}
}

// TestDiscrepancyRefusalNamesStepIDs is the second, smaller defect.
//
// The refusal named step INSTANCES, and instances repeat across issues in one
// run — RUN-14's eight rows were `design-qa@1` three times over. The other
// documented way out, `dispatch backfill-usage --step STEP-N`, needs the id,
// and no read verb exposed which rows the probe was counting, so an operator
// handed the refusal had no way to enumerate what to back-fill.
func TestDiscrepancyRefusalNamesStepIDs(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)
	id := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn,
		`UPDATE steps SET status = ?, updated_at_ms = ?, attempt = 1 WHERE id = ?`,
		db.StepDone, nowMS+1000, id)

	_, err := NewEngine().NextSteps(conn, runID, 0, nowMS)
	if err == nil {
		t.Fatal("premise: `next` must refuse over the discrepancy")
	}
	if !strings.Contains(err.Error(), model.FormatStepID(id)) {
		t.Errorf("the refusal names no step id: %v — `backfill-usage --step` "+
			"needs one, and the instance alone does not identify a row", err)
	}
}

// TestOpenDispatchRefusesWhatNextRefuses closes the disagreement between the
// two scheduling verbs.
//
// `next` refused and `dispatch open` did not, so the same run answered "you get
// no work until these are resolved" and "here are your rows" to two verbs one
// second apart. A conductor following the documented loop — ask `next` first —
// stalled; one that reached for `dispatch open` proceeded, and the
// accountability gap went unreconciled either way.
func TestOpenDispatchRefusesWhatNextRefuses(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)
	id := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn,
		`UPDATE steps SET status = ?, updated_at_ms = ?, attempt = 1 WHERE id = ?`,
		db.StepDone, nowMS+1000, id)

	_, nextErr := NewEngine().NextSteps(conn, runID, 0, nowMS)
	_, openErr := NewEngine().OpenDispatch(conn, runID, 0, nil, nowMS)

	if nextErr == nil {
		t.Fatal("premise: `next` must refuse")
	}
	if openErr == nil {
		t.Error("`dispatch open` succeeded on a run `next` refuses; the two " +
			"verbs must agree on whether a run is blocked")
	}
}

// TestAcceptedMissingUsageUnblocksNext is the sharpest of the four.
//
// `--accept-missing-usage` recorded the acceptance in the `dispatch-closed`
// event and nowhere else. The probe `next` runs recomputes from step rows and
// never read that event, so following the documented remedy exactly left the
// run refusing in precisely the same words: RUN-32 accepted its one row, the
// close reported success, and the very next `next` reported the same
// discrepancy.
func TestAcceptedMissingUsageUnblocksNext(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	id := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn,
		`UPDATE steps SET status = ?, updated_at_ms = ?, attempt = 1 WHERE id = ?`,
		db.StepDone, nowMS+1000, id)

	outcome, err := e.CloseDispatch(conn, runID, true, "", nowMS)
	testsupport.Must(t, err, "close --accept-missing-usage: %v", err)
	if len(outcome.Accepted) == 0 {
		t.Fatal("premise: the close must have accepted something")
	}

	if _, err := e.NextSteps(conn, runID, 0, nowMS); err != nil {
		t.Errorf("`next` still refuses after the acceptance was recorded: %v — "+
			"the flag's own help says it records the acceptance, and the "+
			"acceptance must clear the verb that is actually blocked", err)
	}
}

// TestAcceptMissingUsageNeedsNoOpenDispatch breaks the cycle itself.
//
// `dispatch close --accept-missing-usage` was the only resolution the refusal
// offered, and `close` required an open dispatch. RUN-14 sat active with 190
// steps done and no way forward from 2026-08-19: `next` would not offer work
// until the discrepancy cleared, and the named way to clear it needed a
// manifest that was not there.
func TestAcceptMissingUsageNeedsNoOpenDispatch(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)
	id := stepIDByInstance(t, conn, "implement@0")
	execSQL(t, conn,
		`UPDATE steps SET status = ?, updated_at_ms = ?, attempt = 1 WHERE id = ?`,
		db.StepDone, nowMS+1000, id)

	if _, err := e.NextSteps(conn, runID, 0, nowMS); err == nil {
		t.Fatal("premise: the run must be refusing")
	}

	outcome, err := e.CloseDispatch(conn, runID, true, "", nowMS)
	testsupport.Must(t, err, "close --accept-missing-usage with no dispatch "+
		"open: %v — this is the documented way out of the refusal, and "+
		"requiring a manifest to reach it is the cycle", err)
	if len(outcome.Accepted) == 0 {
		t.Error("nothing was accepted; the acceptance is the whole substance " +
			"of the verb in this state")
	}

	if _, err := e.NextSteps(conn, runID, 0, nowMS); err != nil {
		t.Errorf("`next` still refuses: %v", err)
	}
}

// TestCloseWithNoDispatchStillRefusesWithoutTheFlag is the lower bound of that
// escape hatch: plain `dispatch close` on a run with no manifest is still a
// conflict, because there is nothing to close.
func TestCloseWithNoDispatchStillRefusesWithoutTheFlag(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	_, err := testEngine().CloseDispatch(conn, runID, false, "", nowMS)
	if err == nil {
		t.Fatal("`dispatch close` succeeded with no dispatch open")
	}
	if !strings.Contains(err.Error(), "no dispatch is open") {
		t.Errorf("the refusal is %q, want one naming the missing dispatch", err)
	}
}
