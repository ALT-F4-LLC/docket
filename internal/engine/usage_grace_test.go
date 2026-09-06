package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// D7 — usage pending.
//
// The relay measures a wave's spend from agent transcripts AFTER the wave
// returns, and D2 used to refuse `next` and `close` the instant a step
// recorded without usage, which put that join on the critical path of every
// close. A step recorded less than `dispatch.grace` ago is now usage PENDING:
// the close proceeds, the join lands beside it, and only a step still unbilled
// past the grace is unreconciled.

// TestFreshlyRecordedStepIsUsagePending pins the window at both edges, and
// that a plain close and the next offer proceed inside it.
func TestFreshlyRecordedStepIsUsagePending(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	instance := manifest.Rows[0].Instance
	finishWithoutUsage(t, conn, instance) // recorded at nowMS+1000

	grace := graceMS(t, conn)
	fresh := nowMS + 1000
	lastInside := fresh + grace - 1
	past := fresh + grace

	if ds := discrepanciesAt(t, conn, runID, fresh); containsKind(ds, DiscrepancyMissingUsage) {
		t.Errorf("a step recorded this instant is already a %s discrepancy (%v); "+
			"D7 gives the relay `dispatch.grace` to back-fill it",
			DiscrepancyMissingUsage, ds)
	}
	if ds := discrepanciesAt(t, conn, runID, lastInside); containsKind(ds, DiscrepancyMissingUsage) {
		t.Errorf("one ms inside the grace the step is a discrepancy (%v)", ds)
	}
	if ds := discrepanciesAt(t, conn, runID, past); !containsKind(ds, DiscrepancyMissingUsage) {
		t.Errorf("at the grace the unbilled step is still not a discrepancy (%v); "+
			"D7 defers D2, it does not retire it", ds)
	}

	// The close reconciles inside the window, and `next` answers after it:
	// the join is off the critical path.
	outcome, err := e.CloseDispatch(conn, runID, false, "", fresh)
	testsupport.Must(t, err, "a plain close inside the grace refused: %v", err)
	if outcome.Reason != db.CloseReasonReconciled {
		t.Errorf("close_reason = %q, want %q", outcome.Reason, db.CloseReasonReconciled)
	}
	if _, err := e.NextSteps(conn, runID, 0, fresh+1); err != nil {
		t.Errorf("`next` refused inside the grace: %v", err)
	}

	// The back-fill lands after the close, and the step never becomes one.
	_, err = e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: stepIDByInstance(t, conn, instance), Unit: "tokens", Quantity: 48211},
	}, "", "", fresh+2)
	testsupport.Must(t, err, "backfill-usage after the close: %v", err)
	if ds := discrepanciesAt(t, conn, runID, past); containsKind(ds, DiscrepancyMissingUsage) {
		t.Errorf("a step billed inside the grace is a discrepancy past it (%v)", ds)
	}
}

// TestAcceptMissingUsageSettlesPendingStepsToo: the acceptance flag asks
// WITHOUT the grace. An acceptance that skipped the young steps would
// resurface them as a refusal minutes after the operator settled the run.
func TestAcceptMissingUsageSettlesPendingStepsToo(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	finishWithoutUsage(t, conn, manifest.Rows[0].Instance)

	outcome, err := e.CloseDispatch(conn, runID, true, "", nowMS+1000)
	testsupport.Must(t, err, "close --accept-missing-usage inside the grace: %v", err)
	if len(outcome.Accepted) == 0 {
		t.Fatal("the acceptance skipped a step inside the grace; it would " +
			"resurface as a refusal once the grace lapsed")
	}
	if outcome.Reason != db.CloseReasonAcceptedMissingUsage {
		t.Errorf("close_reason = %q, want %q",
			outcome.Reason, db.CloseReasonAcceptedMissingUsage)
	}

	past := nowMS + 1000 + graceMS(t, conn) + 1
	if _, err := e.NextSteps(conn, runID, 0, past); err != nil {
		t.Errorf("the accepted step resurfaced once its grace lapsed: %v", err)
	}
}

// TestRunReportListsUsagePendingSteps: the report asks WITHOUT the grace, so a
// run's last wave — which no later close probes — still shows what it owes.
func TestRunReportListsUsagePendingSteps(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	abandon(t, conn, runID, nowMS)
	instance := completeAStepWithoutUsage(t, conn, runID) // recorded at nowMS+1000

	report, err := LoadRunReport(conn, runID, nowMS+1000)
	testsupport.Must(t, err, "LoadRunReport: %v", err)
	var listed bool
	for _, d := range report.MissingUsage {
		if d.Instance == instance && d.Kind == DiscrepancyMissingUsage {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("missing_usage = %v; the freshly recorded, unbilled %s is not in it, "+
			"so a run's last wave could be reported done with its spend unrecorded",
			report.MissingUsage, instance)
	}

	_, err = e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: stepIDByInstance(t, conn, instance), Unit: "tokens", Quantity: 100},
	}, "", "", nowMS+2000)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	report, err = LoadRunReport(conn, runID, nowMS+2000)
	testsupport.Must(t, err, "LoadRunReport after the back-fill: %v", err)
	if len(report.MissingUsage) != 0 {
		t.Errorf("missing_usage = %v after the back-fill, want none", report.MissingUsage)
	}
}
