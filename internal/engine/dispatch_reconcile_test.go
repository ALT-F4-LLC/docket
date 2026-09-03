package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-580's one-verb wave reconcile.
//
// RUN-44 retyped back-fill / verify / close five times, once per wave. The
// tests below are the three acceptance criteria, plus the two orderings the
// word "atomically" actually forbids: a failed back-fill must not verify or
// close, and a failed verify must not close.

// TestReconcileRunsAllThreeStages is criterion 1's happy path: one call
// back-fills, verifies, and closes, and every stage reports what it did.
func TestReconcileRunsAllThreeStages(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)

	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	// The premise: without the back-fill this close REFUSES. If it did not,
	// the test below would prove nothing about the back-fill stage running.
	if _, err := e.CloseDispatch(conn, runID, false, "", nowMS); err == nil {
		t.Fatal("premise: `close` must refuse over the missing usage that " +
			"the back-fill stage exists to record")
	}

	out, err := e.ReconcileDispatch(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 48211},
	}, "wave-journal:wf-7", "", false, "", nowMS)
	testsupport.Must(t, err, "reconcile: %v", err)

	if out.Backfill == nil || out.Backfill.Written != 1 || out.Backfill.Steps != 1 {
		t.Fatalf("backfill stage reported %+v, want 1 row across 1 step", out.Backfill)
	}
	if out.Backfill.Source != "wave-journal:wf-7" {
		t.Errorf("source = %q, want the one passed — a reconcile must not "+
			"rewrite who measured the work", out.Backfill.Source)
	}
	if out.Verify == nil || out.Verify.Dispatch == "" {
		t.Fatalf("verify stage reported %+v, want a named dispatch", out.Verify)
	}
	if out.Close == nil || out.Close.Status != db.DispatchClosed {
		t.Fatalf("close stage reported %+v, want status %q", out.Close, db.DispatchClosed)
	}

	// And the manifest really is closed, not merely reported as such.
	status, reason := dispatchStatus(t, conn, runID)
	if status != db.DispatchClosed || reason != db.CloseReasonReconciled {
		t.Fatalf("stored dispatch is (%s, %s), want (%s, %s)",
			status, reason, db.DispatchClosed, db.CloseReasonReconciled)
	}
}

// TestReconcileMatchesTheManualOrdering is criterion 3, asserted the only way
// that means anything: run the SAME wave twice, once through the three verbs by
// hand and once through the reconcile, and compare what the discrepancy probe
// sees afterwards.
//
// The ledger row, its source, its attempt, and the `usage_recorded` fast path
// the probe actually reads must be identical, and `next` — which runs the probe
// — must answer in both.
func TestReconcileMatchesTheManualOrdering(t *testing.T) {
	// The manual ordering, in its own database.
	manualConn := mustDB(t)
	manualEngine := testEngine()
	manualRun := dispatchRun(t, manualConn)
	openDispatch(t, manualConn, manualRun, 0, nowMS)
	manualStep := stepIDByInstance(t, manualConn, "implement@0")
	completeWithoutUsage(t, manualConn, manualEngine, manualStep)

	_, err := manualEngine.BackfillUsage(manualConn, manualRun, []BackfillRow{
		{Step: manualStep, Unit: "tokens", Quantity: 48211},
	}, "wave-journal:wf-7", "", nowMS)
	testsupport.Must(t, err, "manual backfill-usage: %v", err)
	_, mismatch, err := manualEngine.VerifyDispatch(manualConn, manualRun, nowMS)
	testsupport.Must(t, err, "manual verify: %v", err)
	if mismatch != nil {
		t.Fatalf("manual verify found a mismatch at row %d", mismatch.Position)
	}
	_, err = manualEngine.CloseDispatch(manualConn, manualRun, false, "", nowMS)
	testsupport.Must(t, err, "manual close: %v", err)

	// The reconcile, in another.
	oneConn := mustDB(t)
	oneEngine := testEngine()
	oneRun := dispatchRun(t, oneConn)
	openDispatch(t, oneConn, oneRun, 0, nowMS)
	oneStep := stepIDByInstance(t, oneConn, "implement@0")
	completeWithoutUsage(t, oneConn, oneEngine, oneStep)

	_, err = oneEngine.ReconcileDispatch(oneConn, oneRun, []BackfillRow{
		{Step: oneStep, Unit: "tokens", Quantity: 48211},
	}, "wave-journal:wf-7", "", false, "", nowMS)
	testsupport.Must(t, err, "reconcile: %v", err)

	// What the ledger holds, row for row.
	manualRows := usageRowsFor(t, manualConn, manualStep)
	oneRows := usageRowsFor(t, oneConn, oneStep)
	if len(manualRows) != 1 || len(oneRows) != 1 {
		t.Fatalf("ledger rows: manual %d, reconciled %d, want 1 each",
			len(manualRows), len(oneRows))
	}
	m, o := manualRows[0], oneRows[0]
	if m.Unit != o.Unit || m.Quantity != o.Quantity ||
		m.Attempt != o.Attempt || m.Source != o.Source {
		t.Fatalf("the reconciled row differs from the hand-typed one:\n"+
			"  manual:     %+v\n  reconciled: %+v", m, o)
	}

	// And what the PROBE reads — the fast-path column, not the rows.
	manualStepRow, err := db.GetStep(manualConn, manualStep)
	testsupport.Must(t, err, "GetStep: %v", err)
	oneStepRow, err := db.GetStep(oneConn, oneStep)
	testsupport.Must(t, err, "GetStep: %v", err)
	if manualStepRow.UsageRecorded != oneStepRow.UsageRecorded {
		t.Fatalf("usage_recorded: manual %v, reconciled %v — D2's probe reads "+
			"this column and the two orderings must leave it identical",
			manualStepRow.UsageRecorded, oneStepRow.UsageRecorded)
	}
	if !oneStepRow.UsageRecorded {
		t.Fatal("usage_recorded is false after a reconcile; the probe would " +
			"keep refusing")
	}

	// The probe itself, run end to end, on both.
	if _, err := manualEngine.NextSteps(manualConn, manualRun, 0, nowMS); err != nil {
		t.Fatalf("`next` refuses after the manual ordering: %v", err)
	}
	if _, err := oneEngine.NextSteps(oneConn, oneRun, 0, nowMS); err != nil {
		t.Fatalf("`next` refuses after the reconcile but not after the manual "+
			"ordering: %v", err)
	}
}

// TestReconcileStopsAtAFailedBackfill is the first half of "atomically": a
// back-fill that refuses runs no verify and no close, and leaves the run in the
// state it found it — dispatch still open, ledger still empty.
func TestReconcileStopsAtAFailedBackfill(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)

	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	// A batch naming a step that does not exist. BackfillUsage resolves every
	// step before writing anything, so this refuses with nothing written.
	out, err := e.ReconcileDispatch(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 48211},
		{Step: 999999, Unit: "tokens", Quantity: 1},
	}, "", "", false, "", nowMS)
	if err == nil {
		t.Fatal("the reconcile succeeded over a step that does not exist")
	}

	var stage *StageError
	if !errors.As(err, &stage) {
		t.Fatalf("error %v is not a *StageError; a reconcile failure must name "+
			"its stage", err)
	}
	if stage.Stage != StageBackfill {
		t.Fatalf("stage = %q, want %q", stage.Stage, StageBackfill)
	}
	if !strings.Contains(err.Error(), StageBackfill+" stage") {
		t.Errorf("the message %q does not name the stage that failed", err)
	}
	if code, ok := CodeOf(err); !ok || code != CodeNotFound {
		t.Errorf("code = %q (found=%v), want %q — StageError must unwrap to "+
			"the stage's own taxonomy code", code, ok, CodeNotFound)
	}

	// NO LATER STAGE RAN.
	if out.Backfill != nil || out.Verify != nil || out.Close != nil {
		t.Fatalf("a failed back-fill reported later stages: %+v", out)
	}
	// THE RUN IS AS IT WAS. Nothing written, nothing closed.
	if rows := usageRowsFor(t, conn, implID); len(rows) != 0 {
		t.Fatalf("the refused batch still wrote %d ledger row(s): %+v",
			len(rows), rows)
	}
	if status, _ := dispatchStatus(t, conn, runID); status != db.DispatchOpen {
		t.Fatalf("the dispatch is %q after a failed back-fill, want %q — a "+
			"failed stage must never leave a partial close", status, db.DispatchOpen)
	}
}

// TestReconcileStopsAtAFailedVerify is the second half: a verify that finds a
// drifted manifest runs no close. The back-fill it already committed STAYS
// committed — it is a measurement, not a lock — but the dispatch is untouched.
func TestReconcileStopsAtAFailedVerify(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)

	manifest := openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	// Drift the manifest under the reconcile: claim a still-ready row, so it is
	// non-terminal and no longer offerable — verify's `genuinely-missing`.
	var claimed int
	for _, row := range manifest.Rows {
		id, err := model.ParseStepID(row.Step)
		testsupport.Must(t, err, "parsing %q: %v", row.Step, err)
		if id == implID {
			continue
		}
		if _, err := ClaimStep(conn, id, ClaimOptions{Owner: "w2", NowMS: nowMS}); err == nil {
			claimed = id
			break
		}
	}
	if claimed == 0 {
		t.Skip("the fixture manifest has no second claimable row to drift")
	}

	out, err := e.ReconcileDispatch(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 48211},
	}, "", "", false, "", nowMS)
	if err == nil {
		t.Fatal("the reconcile closed over a drifted manifest")
	}

	var stage *StageError
	if !errors.As(err, &stage) {
		t.Fatalf("error %v is not a *StageError", err)
	}
	if stage.Stage != StageVerify {
		t.Fatalf("stage = %q, want %q", stage.Stage, StageVerify)
	}
	if stage.Mismatch == nil || stage.Verify == nil {
		t.Fatalf("a verify-stage refusal carries no diagnostic: %+v", stage)
	}
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Errorf("code = %q (found=%v), want %q", code, ok, CodeConflict)
	}

	// THE CLOSE DID NOT RUN.
	if out.Close != nil {
		t.Fatalf("the close stage reported %+v after a failed verify", out.Close)
	}
	if status, _ := dispatchStatus(t, conn, runID); status != db.DispatchOpen {
		t.Fatalf("the dispatch is %q after a failed verify, want %q",
			status, db.DispatchOpen)
	}
	// The back-fill that already landed is reported rather than hidden: an
	// operator who re-runs the pipeline would otherwise hit the ledger's
	// (step, attempt, unit) key with no idea why.
	if out.Backfill == nil || out.Backfill.Written != 1 {
		t.Fatalf("the committed back-fill is not reported: %+v", out.Backfill)
	}
	if rows := usageRowsFor(t, conn, implID); len(rows) != 1 {
		t.Fatalf("the back-fill wrote %d ledger row(s), want 1 — the stage "+
			"committed before verify refused", len(rows))
	}
}

// TestReconcileLeavesTheStandaloneVerbsAlone is criterion 2 at the engine
// level: the three functions the reconcile calls are the SAME ones the verbs
// call, so a plain CloseDispatch on a plain run behaves exactly as before.
func TestReconcileLeavesTheStandaloneVerbsAlone(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	runID := dispatchRun(t, conn)

	openDispatch(t, conn, runID, 0, nowMS)
	implID := stepIDByInstance(t, conn, "implement@0")
	completeWithoutUsage(t, conn, e, implID)

	_, err := e.BackfillUsage(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 7},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill-usage: %v", err)

	outcome, err := e.CloseDispatch(conn, runID, false, "", nowMS)
	testsupport.Must(t, err, "close: %v", err)
	if outcome.Status != db.DispatchClosed || outcome.Reason != db.CloseReasonReconciled {
		t.Fatalf("close outcome = %+v, want (%s, %s)",
			outcome, db.DispatchClosed, db.CloseReasonReconciled)
	}
}
