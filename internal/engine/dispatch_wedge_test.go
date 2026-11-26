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
	finishWithoutUsage(t, conn, "implement@0")

	past := nowMS + 1000 + graceMS(t, conn) + 1
	_, err := NewEngine().NextSteps(conn, runID, 0, past)
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
	finishWithoutUsage(t, conn, "implement@0")

	past := nowMS + 1000 + graceMS(t, conn) + 1
	_, nextErr := NewEngine().NextSteps(conn, runID, 0, past)
	_, openErr := NewEngine().OpenDispatch(conn, runID, 0, nil, past)

	if nextErr == nil {
		t.Fatal("premise: `next` must refuse")
	}
	if openErr == nil {
		t.Error("`dispatch open` succeeded on a run `next` refuses; the two " +
			"verbs must agree on whether a run is blocked")
	}
}

// TestCloseWithNoDispatchStillRefusesWithoutTheFlag is the lower bound of that
// escape hatch: plain `dispatch close` on a run with no manifest is still a
// conflict, because there is nothing to close.
func TestCloseWithNoDispatchStillRefusesWithoutTheFlag(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)

	_, err := testEngine().CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS)
	if err == nil {
		t.Fatal("`dispatch close` succeeded with no dispatch open")
	}
	if !strings.Contains(err.Error(), "no dispatch is open") {
		t.Errorf("the refusal is %q, want one naming the missing dispatch", err)
	}
}
