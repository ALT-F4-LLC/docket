package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// ActionResultRow is one recorded action execution — the `action result` shape
// of docs/tdd/payloads-thresholds.md §6.3, recorded as an amendment.
//
// IT IS `GateResultRow` FOR ACTIONS, deliberately and field for field. The
// alternative — recording nothing and leaving the routing reason as the only
// trace — makes an `unmatched` action invisible in exactly the way gates-trust
// T11's audit argument says it must not be, and makes a failed computation
// indistinguishable from a failed threshold in a run report. Same shape, same
// ordinal semantics, same reason discipline, so `run report` (S6) reads one
// pattern twice rather than two patterns once.
//
// Argv and Exit are POINTERS for the reason they are on a gate result: a
// builtin spawns nothing (§6.1 B2) and an unmatched action never ran (§6.2 A3),
// so NULL is the honest encoding of "no process existed". A zero exit on
// something that did not execute reads as success.
type ActionResultRow struct {
	ID     int
	RunID  int
	StepID int
	// Action is the `action` value — the builtin's name, or the trust entry
	// name a non-builtin looked up. Core carries it and never interprets it.
	Action     string
	Ordinal    int
	Argv       []string
	Exit       *int
	DurationMS int64
	Output     string
	Truncated  bool
	Verdict    string
	// Builtin marks a result core computed itself. It is the field that lets an
	// operator tell `aggregate` — which consults no trust store, by B1 — from a
	// user-trusted command that happened to succeed.
	Builtin bool
	// Reason explains an `unmatched` verdict, a refusal, or a failure, for the
	// same four-causes-one-verdict reason the gate row carries one.
	Reason      string
	CreatedAtMS int64
}

// Action verdicts as stored, mirroring the gate vocabulary exactly. `unmatched`
// is a FIRST-CLASS outcome: the action name matched no trust entry, so nothing
// ran — which is neither a pass nor an error (§6.2 A3).
const (
	ActionVerdictPass      = "pass"
	ActionVerdictFail      = "fail"
	ActionVerdictUnmatched = "unmatched"
)

// Trust-cache kinds (§6.3). `trust_cache` gains a `kind` column rather than a
// second table: the table answers "what did this run consider trusted, and
// when", and splitting that answer by the SHAPE OF THE CALLER would make the
// one question two queries.
const (
	TrustKindGate   = "gate"
	TrustKindAction = "action"
)

// InsertActionResultTx records one action result.
//
// It takes a transaction because the result commits with the routing stage's
// own writes — the subprocess ran OUTSIDE any transaction (engine-spec §6) and
// only its recorded fact lands in one.
func InsertActionResultTx(tx *sql.Tx, r ActionResultRow) error {
	var argv any
	if r.Argv != nil {
		encoded, err := json.Marshal(r.Argv)
		if err != nil {
			return fmt.Errorf("encoding the action argv: %w", err)
		}
		argv = string(encoded)
	}

	var exit any
	if r.Exit != nil {
		exit = *r.Exit
	}

	var reason any
	if r.Reason != "" {
		reason = r.Reason
	}

	_, err := tx.Exec(
		`INSERT INTO action_results
		   (run_id, step_id, action, ordinal, argv, exit, duration_ms, output,
		    truncated, verdict, builtin, reason, created_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.StepID, r.Action, r.Ordinal, argv, exit, r.DurationMS,
		r.Output, boolToInt(r.Truncated), r.Verdict, boolToInt(r.Builtin),
		reason, r.CreatedAtMS)
	if err != nil {
		return fmt.Errorf("recording the action result: %w", err)
	}
	return nil
}

// ActionResultsForStep returns every recorded result for a step, in insertion
// order.
func ActionResultsForStep(conn *sql.DB, stepID int) ([]ActionResultRow, error) {
	return scanActionResults(conn.Query(actionResultSelect+` WHERE step_id = ? ORDER BY id`, stepID))
}

// NextActionOrdinalTx returns the ordinal a new result for this step+action
// takes.
//
// Ordinals are per (step, action) and ascend, carrying §4's "flaky-declared
// re-runs recorded individually" (A8) and, with it, the resume case: a routing
// stage re-entered after a crash must not collide with the interrupted
// attempt's row, which the UNIQUE(step_id, action, ordinal) index would refuse.
func NextActionOrdinalTx(tx *sql.Tx, stepID int, action string) (int, error) {
	var next sql.NullInt64
	err := tx.QueryRow(
		`SELECT MAX(ordinal) FROM action_results WHERE step_id = ? AND action = ?`,
		stepID, action).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("reading the action ordinal: %w", err)
	}
	if !next.Valid {
		return 0, nil
	}
	return int(next.Int64) + 1, nil
}

const actionResultSelect = `
SELECT id, run_id, step_id, action, ordinal, argv, exit, duration_ms, output,
       truncated, verdict, builtin, reason, created_at_ms
  FROM action_results`

func scanActionResults(rows *sql.Rows, err error) ([]ActionResultRow, error) {
	if err != nil {
		return nil, fmt.Errorf("reading action results: %w", err)
	}
	return scanRows(rows, "action results", func(r *sql.Rows) (ActionResultRow, error) {
		var (
			row       ActionResultRow
			argv      sql.NullString
			exit      sql.NullInt64
			reason    sql.NullString
			truncated int
			builtin   int
		)
		if err := r.Scan(
			&row.ID, &row.RunID, &row.StepID, &row.Action, &row.Ordinal, &argv, &exit,
			&row.DurationMS, &row.Output, &truncated, &row.Verdict, &builtin,
			&reason, &row.CreatedAtMS,
		); err != nil {
			return ActionResultRow{}, fmt.Errorf("reading an action result: %w", err)
		}
		if argv.Valid && argv.String != "" {
			if err := json.Unmarshal([]byte(argv.String), &row.Argv); err != nil {
				return ActionResultRow{}, fmt.Errorf("reading a recorded argv: %w", err)
			}
		}
		if exit.Valid {
			code := int(exit.Int64)
			row.Exit = &code
		}
		row.Reason = reason.String
		row.Truncated, row.Builtin = truncated != 0, builtin != 0
		return row, nil
	})
}
