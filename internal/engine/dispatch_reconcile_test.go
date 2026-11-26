package engine

import (
	"database/sql"
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

	// The premise: without the back-fill the step is OWING. A plain close no
	// longer refuses over a freshly recorded step (the grace gives it
	// `dispatch.grace`), so the ungraced view is where the debt shows; past
	// the grace the close would refuse as it always did.
	if _, err := e.CloseDispatch(conn, runID, false, IntegrationSkip{}, nowMS+graceMS(t, conn)+1); err == nil {
		t.Fatal("premise: `close` must refuse past the grace over the missing " +
			"usage that the back-fill stage exists to record")
	}
	if ds := discrepanciesAt(t, conn, runID, nowMS+graceMS(t, conn)+1); !containsKind(ds, DiscrepancyMissingUsage) {
		t.Fatalf("premise: the completed step must be owing usage, got %v", ds)
	}

	out, err := e.ReconcileDispatch(conn, runID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 48211},
	}, "wave-journal:wf-7", "", false, IntegrationSkip{}, nowMS)
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
	}, "", "", false, IntegrationSkip{}, nowMS)
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
	}, "", "", false, IntegrationSkip{}, nowMS)
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

// TestReconcileClosesOverParkedSuccessors is DKT-3764's loop, broken: a lane
// whose implement parked `waiting-human` leaves its staged successors pending
// and unofferable, and the reconcile must still close the dispatch so that
// `run refresh-scope`, the park's remedy, is accepted afterwards.
func TestReconcileClosesOverParkedSuccessors(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	run, issue := activatedRun(t, conn)

	manifest := openDispatch(t, conn, run.ID, 0, nowMS)
	if len(stagedRowsOf(t, conn, manifest.Rows, issue)) == 0 {
		t.Fatalf("premise: the manifest stages no successor of implement@0: %+v",
			manifest.Rows)
	}
	implID := stepIDByInstance(t, conn, "implement@0")
	parkGapOnly(t, conn, implID)

	out, err := e.ReconcileDispatch(conn, run.ID, []BackfillRow{
		{Step: implID, Unit: "tokens", Quantity: 48211},
	}, "", "", false, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "reconcile over a parked lane: %v", err)
	if out.Close == nil || out.Close.Status != db.DispatchClosed {
		t.Fatalf("close stage reported %+v, want status %q", out.Close, db.DispatchClosed)
	}
	if status, _ := dispatchStatus(t, conn, run.ID); status != db.DispatchClosed {
		t.Fatalf("stored dispatch is %q, want %q", status, db.DispatchClosed)
	}

	widen(t, conn, issue, `["internal/widened/**"]`)
	_, err = refresh(conn, run.ID, issue)
	testsupport.Must(t, err, "refresh-scope after the close: %v", err)
}

// retriedAfterOpen parks implement@0 on a gap-only record after the manifest
// opened and resolves it --as retry. Claim and record both pass --metadata, as
// a real wave's do, so the retried step renders the claim history its spent
// claim left behind: attempt and the merged metadata bag.
func retriedAfterOpen(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()
	openDispatch(t, conn, runID, 0, nowMS)
	id := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, id, ClaimOptions{
		Owner: "worker", Metadata: `{"dispatched_tier":"standard"}`, NowMS: nowMS,
	})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, testEngine().CompleteStep(conn, id, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("   \n"),
		Gaps:     [][]byte{[]byte("# The fix is out of scope\n\nNothing here to change.")},
		Metadata: `{"model_resolved":"unknown","effort_resolved":"unknown"}`,
		NowMS:    nowMS,
	}), "gap-only record")
	if got := statusOf(t, conn, id); got != db.StepWaitingHuman {
		t.Fatalf("premise: implement@0 is %q after a gap-only record, want %q",
			got, db.StepWaitingHuman)
	}
	testsupport.Must(t, testEngine().ResolveStep(conn, id, ResolveRetry, "run it again", nowMS),
		"resolve --as retry")
	if got := statusOf(t, conn, id); got != db.StepPending {
		t.Fatalf("premise: implement@0 is %q after the retry, want %q", got, db.StepPending)
	}
}

// TestVerifyRetriedRowDoesNotFail pins a retry resolved on a manifest row while
// its dispatch is open: the operator's ruling on that step is what moved its
// rendering, so verify reports the row as retried and still verifies.
func TestVerifyRetriedRowDoesNotFail(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	retriedAfterOpen(t, conn, runID)

	result, mismatch, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
	testsupport.Must(t, err, "verify: %v", err)
	if got := verdictOf(result, "implement@0"); got != RowRetried {
		t.Fatalf("the retried row reads %q, want %q: %+v", got, RowRetried, result.Rows)
	}
	if mismatch != nil || !result.Verified {
		t.Fatalf("verify refused a manifest whose only shift is a recorded retry: "+
			"mismatch %+v, rows %+v", mismatch, result.Rows)
	}
}

// TestVerifyUnrelatedShiftStillFails keeps the retry exemption narrow: a shift
// no recorded retry resolution explains still refuses.
func TestVerifyUnrelatedShiftStillFails(t *testing.T) {
	t.Run("a failed claim returned to the pool is not a retry", func(t *testing.T) {
		conn := mustDB(t)
		runID := dispatchRun(t, conn)
		openDispatch(t, conn, runID, 0, nowMS)
		id := stepIDByInstance(t, conn, "implement@0")
		claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "worker", NowMS: nowMS})
		testsupport.Must(t, err, "claim: %v", err)
		testsupport.Must(t, testEngine().FailStep(conn, id, claim.Token, "went wrong", "", nowMS),
			"fail")
		if got := statusOf(t, conn, id); got != db.StepPending {
			t.Fatalf("premise: implement@0 is %q after a below-budget fail, want %q",
				got, db.StepPending)
		}

		result, mismatch, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
		testsupport.Must(t, err, "verify: %v", err)
		if got := verdictOf(result, "implement@0"); got != RowShifted {
			t.Errorf("a failed-and-reoffered row reads %q, want %q", got, RowShifted)
		}
		if mismatch == nil || result.Verified {
			t.Errorf("verify passed a shift no retry resolution explains: %+v", result.Rows)
		}
	})

	t.Run("a retried row that also shifted elsewhere still refuses", func(t *testing.T) {
		conn := mustDB(t)
		runID := dispatchRun(t, conn)
		retriedAfterOpen(t, conn, runID)
		testsupport.Must(t, db.SetConfig(conn, 0, db.KeyLeaseTTLDefault, "45m"),
			"setting the default lease TTL")

		result, mismatch, err := NewEngine().VerifyDispatch(conn, runID, nowMS)
		testsupport.Must(t, err, "verify: %v", err)
		if got := verdictOf(result, "implement@0"); got != RowShifted {
			t.Errorf("a retried row whose lease TTL also changed reads %q, want %q",
				got, RowShifted)
		}
		if mismatch == nil || result.Verified {
			t.Errorf("verify passed a lease-TTL shift on a retried row: %+v", result.Rows)
		}
	})
}
