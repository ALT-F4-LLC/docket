package db

import (
	"database/sql"
	"fmt"
)

// GateOverrideGrant is one operator ruling that a gate's failure signature is
// environmental for the remainder of ONE run (DKT-546): later steps of the
// same run whose SAME gate fails with the SAME exit and reason auto-pass at
// routing instead of re-asking the operator.
//
// Exit is a POINTER for gate_results' own reason: an `unmatched` gate never
// ran, so it has no exit code, and NULL must match only NULL — "no process
// existed" is not exit 0. Reason is compared verbatim; it is the engine's
// classification field (unmatched cause, timeout, network annotation), not
// free prose.
//
// The grant is RUN-SCOPED BY CONSTRUCTION — the run_id foreign key is the
// whole scope rule. A new run re-asks, exactly as DKT-546 requires; nothing
// here consults project config, and there is deliberately no way to widen a
// grant past its run.
//
// RUN-SCOPED INCLUDES STEPS THAT DO NOT EXIST YET (DKT-734). A fix round mints
// new steps INSIDE the same run, so a grant taken on `fix@7`'s park covers
// `fix@8` and `fix@9` when a later round's same gate fails the same way. That
// is the design and not a leak: RUN-51 read three authorizations there where
// one standing authorization was spent three times. Nothing narrower is
// available — the row has an `origin_step_id`, but it is provenance, never
// consulted by grantMatches — and the safety is elsewhere: the gates still run
// every round, a round failing DIFFERENTLY still parks, and each application
// records its own `step-batch-overridden` event naming this row's id, so the
// ledger distinguishes one ruling spent N times from N rulings.
type GateOverrideGrant struct {
	ID           int
	RunID        int
	OriginStepID int
	Gate         string
	Exit         *int
	Reason       string
	Note         string
	CoveredSteps int
	CreatedAtMS  int64
}

// InsertGateOverrideGrantTx records one grant and returns its id.
//
// It takes a transaction because the grant commits with the resolution that
// raised it (the DKT-237 loop-grant discipline): a grant recorded without the
// override-pass would cover failures the operator never ruled on, and an
// override-pass without the grant would silently drop the ruling's reach.
func InsertGateOverrideGrantTx(tx *sql.Tx, g GateOverrideGrant) (int, error) {
	var exit any
	if g.Exit != nil {
		exit = *g.Exit
	}
	res, err := tx.Exec(
		`INSERT INTO gate_override_grants
		   (run_id, origin_step_id, gate, exit, reason, note, covered_steps,
		    created_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?)`,
		g.RunID, g.OriginStepID, g.Gate, exit, g.Reason, g.Note, g.CreatedAtMS)
	if err != nil {
		return 0, fmt.Errorf("recording the gate override grant: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading the gate override grant id: %w", err)
	}
	return int(id), nil
}

// GateOverrideGrantsForRun returns every grant recorded for one run, in
// insertion order.
func GateOverrideGrantsForRun(conn *sql.DB, runID int) ([]GateOverrideGrant, error) {
	rows, err := conn.Query(
		`SELECT id, run_id, origin_step_id, gate, exit, reason, note,
		        covered_steps, created_at_ms
		   FROM gate_override_grants WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading gate override grants: %w", err)
	}
	return scanRows(rows, "gate override grants",
		func(r *sql.Rows) (GateOverrideGrant, error) {
			var (
				g    GateOverrideGrant
				exit sql.NullInt64
			)
			if err := r.Scan(
				&g.ID, &g.RunID, &g.OriginStepID, &g.Gate, &exit, &g.Reason,
				&g.Note, &g.CoveredSteps, &g.CreatedAtMS,
			); err != nil {
				return GateOverrideGrant{}, fmt.Errorf(
					"reading a gate override grant: %w", err)
			}
			if exit.Valid {
				code := int(exit.Int64)
				g.Exit = &code
			}
			return g, nil
		})
}

// CoverGateOverrideGrantsTx bumps `covered_steps` on each grant that just
// auto-passed one step, in the same transaction as the routing it decided —
// the counter and the pass must not be separable by a crash.
//
// A miss is a caller error rather than a no-op: the ids came from a read of
// this same table moments ago, and a grant that vanished between the read and
// the cover means the routing was decided on evidence that no longer stands.
func CoverGateOverrideGrantsTx(tx *sql.Tx, ids []int) error {
	for _, id := range ids {
		res, err := tx.Exec(
			`UPDATE gate_override_grants SET covered_steps = covered_steps + 1
			  WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("covering gate override grant %d: %w", id, err)
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("gate override grant %d no longer exists", id)
		}
	}
	return nil
}
