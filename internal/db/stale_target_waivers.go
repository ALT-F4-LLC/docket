package db

import (
	"database/sql"
	"fmt"
)

// StaleTargetWaiver is one operator ruling that a specific stale-target
// warning — one (step instance, target sha) pair — has been adjudicated for
// the remainder of ONE run (DKT-742): dispatch open/verify stop re-firing the
// identical warning, and the standing precedent is engine-visible instead of
// session-memory-only.
//
// The waiver is RUN-SCOPED BY CONSTRUCTION — the run_id foreign key is the
// whole scope rule, exactly gate_override_grants' shape (DKT-546). A new run
// re-warns, and there is deliberately no way to widen a waiver past its run.
//
// THE SIGNATURE IS THE PAIR AND NOTHING ELSE. The warning's rendered reason
// names the shared HEAD, which moves at every integration — RUN-52's four
// firings of one adjudicated warning each carried a different HEAD — so the
// reason is not part of the match. A DIFFERENT target sha on the same row, or
// the same sha on a row this waiver does not name, is a different question
// and still warns.
//
// TargetSHA may be a PREFIX of the recorded sha (>= 7 hex characters,
// enforced at the verb): the warning an operator copies it from renders the
// sha at 12 characters, and demanding the full 40 would make the verb
// unusable from its own advisory. Matching is case-insensitive prefix
// (engine: waiverCovers).
type StaleTargetWaiver struct {
	ID           int
	RunID        int
	StepInstance string
	TargetSHA    string
	Note         string
	CreatedAtMS  int64
}

// InsertStaleTargetWaiverTx records one waiver and returns its id.
//
// It takes a transaction because the verb records one waiver per named step
// plus one event per waiver, and a batch that half-applied would leave the
// operator's ruling covering rows they never listed — or missing rows they
// did.
func InsertStaleTargetWaiverTx(tx *sql.Tx, w StaleTargetWaiver) (int, error) {
	res, err := tx.Exec(
		`INSERT INTO stale_target_waivers
		   (run_id, step_instance, target_sha, note, created_at_ms)
		 VALUES (?, ?, ?, ?, ?)`,
		w.RunID, w.StepInstance, w.TargetSHA, w.Note, w.CreatedAtMS)
	if err != nil {
		return 0, fmt.Errorf("recording the stale-target waiver: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading the stale-target waiver id: %w", err)
	}
	return int(id), nil
}

// StaleTargetWaiversForRun returns every waiver recorded for one run, in
// insertion order.
//
// It reads on the pooled connection because its one consumer — staleTargets —
// runs OUTSIDE every transaction by design (§6: no subprocess inside one),
// exactly as batchOverrideCover reads grants.
func StaleTargetWaiversForRun(conn *sql.DB, runID int) ([]StaleTargetWaiver, error) {
	rows, err := conn.Query(
		`SELECT id, run_id, step_instance, target_sha, note, created_at_ms
		   FROM stale_target_waivers WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading stale-target waivers: %w", err)
	}
	return scanRows(rows, "stale-target waivers",
		func(r *sql.Rows) (StaleTargetWaiver, error) {
			var w StaleTargetWaiver
			if err := r.Scan(
				&w.ID, &w.RunID, &w.StepInstance, &w.TargetSHA, &w.Note,
				&w.CreatedAtMS,
			); err != nil {
				return StaleTargetWaiver{}, fmt.Errorf(
					"reading a stale-target waiver: %w", err)
			}
			return w, nil
		})
}
