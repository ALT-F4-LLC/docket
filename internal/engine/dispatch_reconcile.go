package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// One-verb wave reconcile (DKT-580).
//
// A conductor closing a wave typed the same pipeline every time — back-fill
// what the wave measured, verify the manifest still matches, close it — once
// per wave, five times in harness RUN-44 alone. Nothing about that pipeline is
// a decision: the ORDER is forced (usage must land before the close's probe
// reads it, and a close over a drifted manifest is the thing verify exists to
// stop), and every retype is a chance to name the wrong journal, pipe a stale
// file, or skip the verify entirely — the three ways a ledger silently starts
// lying.
//
// THIS ADDS NO NEW SEMANTICS. It is the same three engine calls in the same
// order, and each stage is the EXACT function the standalone verb calls —
// BackfillUsage, VerifyDispatch, CloseDispatch — never a re-implementation.
// That is what makes the discrepancy probe see a reconciled back-fill exactly
// as it sees a hand-typed one: `usage_recorded` is set by the same
// MarkStepUsageRecordedTx inside the same BackfillUsage transaction, and
// `missingUsage` reads that column. There is no second codepath that could
// drift from the first.
//
// WHAT "ATOMIC" MEANS HERE, precisely, because it is not one transaction and
// cannot be:
//
//   - Each stage is individually all-or-nothing. BackfillUsage is one
//     transaction over the whole batch; VerifyDispatch writes nothing at all
//     and is rolled back by construction; CloseDispatch is one transaction.
//   - A stage that fails STOPS THE SEQUENCE. Verify never runs on a failed
//     back-fill, close never runs on a failed verify, and the run is left in
//     whatever state the last SUCCESSFUL stage produced — never half-closed.
//   - Every failure NAMES ITS STAGE (StageError), so an operator reading a
//     refusal knows which of the three verbs to reach for.
//
// The three could not share one transaction even if that were wanted:
// VerifyDispatch's stale-target pass shells out to git and must therefore run
// with no transaction open (§6, no subprocess inside a transaction), and its
// read-only-by-rollback property is the mechanism that makes it a verify at
// all. Nesting it inside a writing transaction would destroy the very property
// P11 is.
const (
	// StageBackfill is stage 1: the usage rows the wave measured.
	StageBackfill = "backfill"
	// StageVerify is stage 2: the manifest still renders as it was stored.
	StageVerify = "verify"
	// StageClose is stage 3: the manifest is reconciled and retired.
	StageClose = "close"
)

// StageError names WHICH stage of a reconcile refused.
//
// It wraps rather than replaces: Unwrap reaches the stage's own error, so
// CodeOf still finds the taxonomy code the standalone verb would have exited
// with and errors.Is still reaches any db sentinel underneath. A reconcile
// therefore exits with the SAME code for the same failure — the message gains
// a stage name and nothing else changes.
type StageError struct {
	// Stage is one of StageBackfill, StageVerify, StageClose.
	Stage string
	// Err is the stage's own refusal, unmodified.
	Err error
	// Verify and Mismatch are set ONLY on a verify-stage row mismatch, which
	// is the one refusal a caller cannot render from the error text alone:
	// P9's diagnostic is the differing BYTES of both renderings plus DKT-243's
	// per-row verdict block, and those live on the result, not in a message.
	// Carrying them here is what lets the reconcile path print the identical
	// refusal `dispatch verify` prints rather than a summary of it.
	Verify   *VerifyResult
	Mismatch *RowMismatch
}

func (e *StageError) Error() string {
	return fmt.Sprintf("%s stage: %s", e.Stage, e.Err)
}

func (e *StageError) Unwrap() error { return e.Err }

// ReconcileOutcome is what all three stages did, in the order they ran.
//
// A stage that never ran is absent (`omitempty`), so a reader can tell "verify
// passed with nothing to say" from "verify never happened" — the distinction
// the per-stage errors exist to preserve.
type ReconcileOutcome struct {
	Run      string           `json:"run"`
	Backfill *BackfillOutcome `json:"backfill,omitempty"`
	Verify   *VerifyResult    `json:"verify,omitempty"`
	Close    *CloseOutcome    `json:"close,omitempty"`
}

// ReconcileDispatch performs back-fill, then verify, then close, refusing at
// whichever stage fails (DKT-580).
//
// ON FAILURE IT RETURNS BOTH the partial outcome and a *StageError. The
// outcome is deliberately not discarded: after a verify-stage refusal the
// back-fill HAS landed and is a fact the operator needs — re-running the whole
// pipeline would hit the ledger's (step, attempt, unit) key on the rows that
// are already in — so a caller that reports the partial work reports the
// truth. The error is always non-nil when a stage failed; nothing about the
// partial outcome should be read as success.
//
// AN EMPTY BATCH IS REFUSED, at the back-fill stage, by BackfillUsage's own
// rule. That is the intended behaviour rather than a skip-to-verify: a wave
// whose usage journal produced no rows is a measurement that went missing, and
// closing quietly over it is exactly the silent lie this verb exists to make
// harder.
func (e *Engine) ReconcileDispatch(
	conn *sql.DB, runID int, rows []BackfillRow, source string,
	onDuplicate string, acceptMissingUsage bool, skipIntegrationReason string, nowMS int64,
) (*ReconcileOutcome, error) {
	out := &ReconcileOutcome{Run: model.FormatRunID(runID)}

	// ---- stage 1: back-fill -------------------------------------------------
	//
	// THE SAME CALL `dispatch backfill-usage` MAKES. Criterion 3 holds by
	// construction rather than by testing: there is one BackfillUsage, it
	// writes the ledger rows and sets `usage_recorded` in one transaction, and
	// the discrepancy probe stage 3 runs reads that column. Nothing here
	// reimplements any of it.
	backfill, err := e.BackfillUsage(conn, runID, rows, source, onDuplicate, nowMS)
	if err != nil {
		return out, &StageError{Stage: StageBackfill, Err: err}
	}
	out.Backfill = backfill

	// ---- stage 2: verify ----------------------------------------------------
	//
	// It runs AFTER the back-fill, which is the order the manual pipeline used
	// and the only order that makes sense: the back-fill writes usage rows and
	// marks steps, neither of which touches the ready set, so it cannot
	// invalidate a verify — but a verify placed first would be stale by the
	// time the close ran, which is precisely the window this verb closes.
	verify, mismatch, err := e.VerifyDispatch(conn, runID, nowMS)
	if err != nil {
		return out, &StageError{Stage: StageVerify, Err: err}
	}
	if mismatch != nil {
		return out, &StageError{
			Stage: StageVerify, Verify: verify, Mismatch: mismatch,
			Err: conflictErr(
				"%s does not match its current rendering (manifest row %d)",
				verify.Dispatch, mismatch.Position),
		}
	}
	out.Verify = verify

	// ---- stage 3: close -----------------------------------------------------
	closed, err := e.CloseDispatch(conn, runID, acceptMissingUsage, skipIntegrationReason, nowMS)
	if err != nil {
		return out, &StageError{Stage: StageClose, Err: err}
	}
	out.Close = closed

	return out, nil
}
