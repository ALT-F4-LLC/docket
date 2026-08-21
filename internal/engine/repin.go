package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// REPIN — the recovery half of the pin story (DKT-408).
//
// `verify-pins` (DKT-297) made drift visible; nothing made it survivable. When
// a corpus install replaces files under an instance-config root while a run is
// active or parked, every pin naming a replaced file mismatches disk forever:
// re-activation deliberately INHERITS the original pin set (RA2), so the only
// disposition was abandon + full re-plan. Four runs across three projects died
// exactly that way, one of them forfeiting 76 completed steps, and one of them
// existing only to reconcile the previous casualty of the same mechanism.
//
// RepinRun is the explicit, operator-gated opposite of RA2's inheritance: it
// rewrites the run's CURRENT agreement — the `pins` rows — to what the refs
// resolve to on disk now, so the steps not yet claimed proceed under the new
// bytes. It is a separate verb rather than a flag on re-activation on purpose:
// re-activation inheriting pins is a guarantee in-flight work relies on, and a
// repin is a person deciding, with a recorded reason, that the agreement moves.
//
// COMPLETED WORK'S PROVENANCE IS NEVER REWRITTEN. Two mechanisms enforce it at
// the data level rather than by convention:
//
//   - The transaction refuses unless the run is quiesced: no step `claimed`
//     (an executor mid-flight claimed under the old agreement, and its packet
//     re-renders would silently flip contract mid-execution) and no dispatch
//     open (a manifest is a frozen offer; its relay must not straddle the
//     transition). It also refuses when every step is terminal — a run whose
//     pins are referenced only by completed work has nothing left to recover,
//     so a repin could only falsify history. Under those guards every step is
//     either terminal (it consumed the old bytes wholly, before the repin
//     event's seq) or pre-claim (it will consume the new bytes wholly) — no
//     step ever straddles the old and new agreement.
//
//   - The write touches ONLY the `pins` table. The tables that record what
//     completed steps did — `steps`, `artifacts`, `step_inputs`, `events` —
//     are never updated; the old sha is preserved in the same transaction as
//     a `run-repinned` event per changed ref (old sha -> new sha), so the
//     agreement a completed step worked under stays recoverable from the trail
//     even after the current agreement moves. The event log is already the
//     package's history mechanism (§9 item 2); a parallel pin-history table
//     would be a second source of the same fact.
type RepinChange struct {
	Kind      string `json:"kind"`
	Ref       string `json:"ref"`
	OldSHA256 string `json:"old_sha256"`
	NewSHA256 string `json:"new_sha256"`
	// Path is where the file pin resolved on disk, "" for registered-object
	// pins — the same field PinVerdict carries, for the same operator.
	Path string `json:"path,omitempty"`
}

// RepinOutcome reports what RepinRun did.
type RepinOutcome struct {
	Run string `json:"run"`
	// Repinned is one entry per pin whose recorded hash moved, in the
	// (kind, ref) order the report walks — empty (never nil) on a no-op.
	Repinned []RepinChange `json:"repinned"`
	// Unchanged counts the pins that already matched disk.
	Unchanged int `json:"unchanged"`
}

// RepinRun re-pins a run's drifted pins to what their refs resolve to now.
func RepinRun(conn *sql.DB, runID int, reason string, nowMS int64) (*RepinOutcome, error) {
	return repinRunIn(conn, runID, reason, nowMS, instanceConfigRoots())
}

// repinRunIn is RepinRun over an explicit root list — the same seam
// verifyPinsIn takes, for the same reason: which file a ref resolves to is the
// thing under test.
func repinRunIn(
	conn *sql.DB, runID int, reason string, nowMS int64, roots []string,
) (*RepinOutcome, error) {
	// The status gate runs BEFORE the disk walk so a repin of a finished run
	// refuses cleanly rather than reporting drift it would then refuse to fix.
	// It is re-checked inside the transaction below; this read exists for the
	// error, not for the guarantee.
	run, err := db.GetRun(conn, runID)
	if errors.Is(err, db.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}
	if err := repinStatusGuard(run); err != nil {
		return nil, err
	}

	// The SAME comparison `verify-pins` reports — one definition of drift, so
	// the detection verb and the recovery verb can never disagree about what
	// is drifted.
	report, err := verifyPinsIn(conn, runID, roots)
	if err != nil {
		return nil, err
	}

	// MISSING REFUSES THE WHOLE SET. Repin's contract is "adopt current disk
	// bytes", and a missing ref has no bytes to adopt; repinning the changed
	// pins around it would report recovery while the run stays wedged on the
	// rest. All-or-nothing is the same rule activation's own pinning follows
	// ("pinning is never partial").
	var missing []string
	// A changed verdict that repinning cannot fix: no resolved hash, or the
	// same hash (a schema that matches its pin byte-for-byte but no longer
	// compiles reports `changed` with Found == Pinned — new bytes are not the
	// remedy for that).
	var unfixable []string
	var changes []RepinChange
	for _, v := range report.Pins {
		switch v.Status {
		case PinMissing:
			missing = append(missing, fmt.Sprintf("%s %s", v.Kind, v.Ref))
		case PinChanged:
			if v.Found == "" || v.Found == v.Pinned {
				unfixable = append(unfixable, fmt.Sprintf("%s %s", v.Kind, v.Ref))
				continue
			}
			changes = append(changes, RepinChange{
				Kind: v.Kind, Ref: v.Ref,
				OldSHA256: v.Pinned, NewSHA256: v.Found, Path: v.Path,
			})
		}
	}
	if len(missing) > 0 {
		return nil, notFoundErr(nil,
			"%s cannot be repinned: %s no longer resolve(s) at all — restore the "+
				"file(s) or abandon the run; repin adopts current disk bytes and "+
				"a missing ref has none", run.Ref(), strings.Join(missing, ", "))
	}
	if len(unfixable) > 0 {
		return nil, conflictErr(
			"%s cannot be repinned: %s report(s) drift that new bytes do not "+
				"explain (run `docket run verify-pins %s` for the verdicts)",
			run.Ref(), strings.Join(unfixable, ", "), run.Ref())
	}

	outcome := &RepinOutcome{
		Run: run.Ref(), Repinned: []RepinChange{},
		Unchanged: len(report.Pins) - len(changes),
	}
	if len(changes) == 0 {
		// Nothing drifted: a clean no-op, so running repin twice is safe and
		// the second run says so instead of inventing an event.
		return outcome, nil
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("repinning %s: %w", run.Ref(), err)
	}
	defer tx.Rollback()

	// The guards re-run against the transaction's snapshot: everything below
	// holds at the moment the agreement moves, not merely when the verb began.
	fresh, err := db.GetRunTx(tx, runID)
	if err != nil {
		return nil, err
	}
	if err := repinStatusGuard(fresh); err != nil {
		return nil, err
	}
	if err := repinQuiescenceGuard(tx, runID, fresh.Ref()); err != nil {
		return nil, err
	}

	for _, c := range changes {
		// The UPDATE is keyed on the OLD hash — the compare-and-swap that makes
		// "never rewrite completed provenance" a property of the statement
		// rather than of the caller's timing. A pin that moved between the
		// disk walk and this write hits zero rows and the whole repin rolls
		// back, instead of overwriting an agreement this verb never inspected.
		res, err := tx.Exec(
			`UPDATE pins SET sha256 = ? WHERE run_id = ? AND kind = ? AND ref = ? AND sha256 = ?`,
			c.NewSHA256, runID, c.Kind, c.Ref, c.OldSHA256,
		)
		if err != nil {
			return nil, fmt.Errorf("repinning %s %s: %w", c.Kind, c.Ref, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return nil, conflictErr(
				"%s %s changed while repinning %s; re-run `docket run verify-pins %s` and retry",
				c.Kind, c.Ref, fresh.Ref(), fresh.Ref())
		}

		// One event PER CHANGED REF, in the same transaction as its UPDATE:
		// the old sha's survival must not depend on a second commit landing.
		data, err := json.Marshal(map[string]any{
			"kind":       c.Kind,
			"ref":        c.Ref,
			"old_sha256": c.OldSHA256,
			"new_sha256": c.NewSHA256,
			"path":       c.Path,
			"reason":     reason,
		})
		if err != nil {
			return nil, fmt.Errorf("recording the repin of %s %s: %w", c.Kind, c.Ref, err)
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventRunRepinned, RunID: runID, Data: string(data), AtMS: nowMS,
		}); err != nil {
			return nil, err
		}
		outcome.Repinned = append(outcome.Repinned, c)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("repinning %s: %w", run.Ref(), err)
	}
	return outcome, nil
}

// repinStatusGuard refuses every run status with no pending-side work.
//
// `done` and `abandoned` are the acceptance criterion's hard case stated as a
// status: every pin of a terminal run is referenced only by completed steps'
// history, so a repin could not help any future claim and could only make the
// recorded agreement disagree with the bytes that work actually consumed.
// `planning` has no frozen agreement yet — activation is where pinning happens.
func repinStatusGuard(run *model.Run) error {
	if run.Status == model.RunActive || run.Status == model.RunWaitingHuman {
		return nil
	}
	return conflictErr(
		"run %s is %s; repin applies to a run that is %s — a run with no "+
			"remaining steps has only completed steps' pins, and those are "+
			"history, not an agreement to move",
		run.Ref(), run.Status,
		orStatusList([]model.RunStatus{model.RunActive, model.RunWaitingHuman}))
}

// repinQuiescenceGuard is the transaction-side half of the provenance rule: the
// agreement may move only when no step could consume both sides of it.
func repinQuiescenceGuard(tx *sql.Tx, runID int, runRef string) error {
	// In-flight executors first, because theirs is the sharper refusal: a
	// claimed step's packet was rendered under the old agreement, and its later
	// renders and its completion would land under the new one — the exact
	// straddle that would falsify its provenance the moment it recorded.
	rows, err := tx.Query(
		`SELECT instance FROM steps WHERE run_id = ? AND status = ? ORDER BY id`,
		runID, db.StepClaimed)
	if err != nil {
		return fmt.Errorf("collecting %s's claimed steps: %w", runRef, err)
	}
	claimed, err := scanTxRows(rows, func(r *sql.Rows) (string, error) {
		var s string
		return s, r.Scan(&s)
	})
	if err != nil {
		return err
	}
	if len(claimed) > 0 {
		return conflictErr(
			"%d step(s) of %s are claimed and mid-flight (%s); a repin under a "+
				"live claim would change what the executor's packet means "+
				"mid-execution — wait for them to record or for their leases "+
				"to be reaped, then retry",
			len(claimed), runRef, strings.Join(claimed, ", "))
	}

	// A manifest is a frozen copy of one `next` answer. Repinning under an open
	// one would hand its relay rows whose packets no longer mean what the
	// manifest's reader saw at open time.
	if open, err := db.OpenDispatchTx(tx, runID); err == nil {
		return conflictErr(
			"a dispatch is open for %s (%s, expiring at %d); close or abandon it "+
				"before repinning — its manifest was offered under the current pins",
			runRef, FormatDispatchID(open.ID), open.ExpiresMS)
	} else if !errors.Is(err, db.ErrNoOpenDispatch) {
		return err
	}

	// And there must be something left for the new agreement to govern. The
	// run-status guard usually implies this, but the rollup that retires a
	// finished run runs lazily — this is the direct form of "completed steps'
	// pins are never rewritten", asked of the steps themselves.
	var remaining int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM steps
		  WHERE run_id = ? AND status NOT IN (?, ?, ?, ?)`,
		runID, db.StepDone, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
	).Scan(&remaining); err != nil {
		return fmt.Errorf("counting %s's remaining steps: %w", runRef, err)
	}
	if remaining == 0 {
		return conflictErr(
			"every step of %s is terminal; its pins are referenced only by "+
				"completed steps' history now, and repinning them would rewrite "+
				"that record without helping any future claim", runRef)
	}
	return nil
}

// PinDrift returns the unsound subset of a run's pin report — the rows a read
// surface states when it warns (DKT-408's remedy 2). nil when every pin is
// sound, so callers can gate rendering on emptiness alone.
//
// It is VerifyPins minus the sound rows rather than its own walk, so the
// warning a surface prints and the report `verify-pins` exits 4 on can never
// name different pins.
func PinDrift(conn *sql.DB, runID int) ([]PinVerdict, error) {
	report, err := VerifyPins(conn, runID)
	if err != nil {
		return nil, err
	}
	var out []PinVerdict
	for _, v := range report.Pins {
		if v.Status != PinOK {
			out = append(out, v)
		}
	}
	return out, nil
}

// pinDriftAdvisory is PinDrift as a best-effort advisory, the same posture
// staleTargets takes: the caller has already done its real work, and a failed
// advisory computation must not turn a committed answer into an error. The
// authoritative surfaces — `verify-pins`, and the CONFLICT every pinned read
// raises — do not run through this.
func pinDriftAdvisory(conn *sql.DB, runID int) []PinVerdict {
	drift, err := PinDrift(conn, runID)
	if err != nil {
		return nil
	}
	return drift
}

// PinDriftNotice renders drifted pins for a human surface, one line per pin,
// naming both hashes and the recovery verb. Empty for no drift.
func PinDriftNotice(drift []PinVerdict, runRef string) string {
	if len(drift) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b,
		"Pin drift: %d pin(s) no longer match what this run froze at activation:\n",
		len(drift))
	for _, v := range drift {
		switch v.Status {
		case PinChanged:
			fmt.Fprintf(&b, "  %s %s changed: pinned %s, on disk %s\n",
				v.Kind, v.Ref, v.Pinned, v.Found)
		case PinMissing:
			fmt.Fprintf(&b, "  %s %s is pinned at %s but no longer resolves\n",
				v.Kind, v.Ref, v.Pinned)
		}
	}
	fmt.Fprintf(&b,
		"Steps reading these refs will refuse (CONFLICT). Restore the files, or "+
			"run `docket run repin %s --reason ...` to adopt current disk bytes "+
			"for the remaining steps.", runRef)
	return b.String()
}
