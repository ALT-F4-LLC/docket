package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// GateResultRow is one recorded gate execution — §11.4's `gate result` shape as
// it lives in the v8 table (TDD docs/tdd/gates-trust.md §4.2, §6.1).
//
// Argv and Exit are POINTERS because NULL is meaningful here and zero is not:
// an `unmatched` gate never ran, so it has no argv and no exit code. Recording
// `exit = 0` for a process that does not exist is exactly the confusion the
// gate-forgery threat (T11) exists to prevent — a zero exit reads as success.
// NULL is the honest encoding of "no process existed".
type GateResultRow struct {
	ID         int
	RunID      int
	StepID     int
	Gate       string
	Ordinal    int
	Argv       []string
	Exit       *int
	DurationMS int64
	Output     string
	Truncated  bool
	Verdict    string
	Pre        bool
	// Stub is 1 ONLY on rows migrated from an S3 `gate_trail`. Nothing this
	// stage records sets it, which is what makes the S3->S4 window auditable
	// after the fact.
	Stub bool
	// StubEntry is 1 when the trust entry that authorized this gate declared
	// itself a PLACEHOLDER (DKT-265) — an echo, a `/usr/bin/true`, a script
	// that exits 0 without looking at anything.
	//
	// IT IS A DIFFERENT FACT FROM `Stub` AND THE NAMES ARE NOT AN ACCIDENT.
	// `Stub` is about WHICH ERA of this codebase produced the row; `StubEntry`
	// is about WHAT RAN. A row can be either, both, or neither, and a reader
	// asking "did a secret scan actually happen" is asking the second question
	// only.
	StubEntry bool
	// Reason explains an `unmatched` verdict or a timeout (§6.3, amendment A6).
	// An unmatched verdict has four distinct causes needing four different
	// remedies, and without this field they render identically to an operator.
	Reason      string
	CreatedAtMS int64
}

// Gate verdicts as stored. `unmatched` is a FIRST-CLASS outcome, not an error
// and not a pass: the command was not trusted, so it did not run (§6.2 N1-N4).
const (
	GateVerdictPass      = "pass"
	GateVerdictFail      = "fail"
	GateVerdictUnmatched = "unmatched"
	// GateVerdictSkipped: the gate's tree was gone at spawn time, so nothing
	// ran and nothing was measured (DKT-169). Not a pass for routing.
	GateVerdictSkipped = "skipped"
)

// InsertGateResultTx records one gate result.
//
// It takes a transaction because the result commits with the saga's stage
// advance — the subprocess ran OUTSIDE any transaction (engine-spec §6) and
// only its recorded fact lands in one.
func InsertGateResultTx(tx *sql.Tx, r GateResultRow) error {
	var argv any
	if r.Argv != nil {
		encoded, err := json.Marshal(r.Argv)
		if err != nil {
			return fmt.Errorf("encoding the gate argv: %w", err)
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

	// `stub` is written from the row rather than hardcoded to 0, because the S3
	// pass-through runner still exists and is still what the saga tests drive.
	// The REAL runner never sets it (T11, N4) — the guarantee is a property of
	// what ExecRunner produces, not of what this writer will accept, and
	// TestRealResultsCarryNoStubField asserts it where it belongs.
	_, err := tx.Exec(
		`INSERT INTO gate_results
		   (run_id, step_id, gate, ordinal, argv, exit, duration_ms, output,
		    truncated, verdict, pre, stub, stub_entry, reason, created_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.StepID, r.Gate, r.Ordinal, argv, exit, r.DurationMS, r.Output,
		boolToInt(r.Truncated), r.Verdict, boolToInt(r.Pre), boolToInt(r.Stub),
		boolToInt(r.StubEntry), reason, r.CreatedAtMS)
	if err != nil {
		return fmt.Errorf("recording the gate result: %w", err)
	}
	return nil
}

const gateResultSelect = `
SELECT id, run_id, step_id, gate, ordinal, argv, exit, duration_ms,
       output, truncated, verdict, pre, stub, stub_entry, reason, created_at_ms
  FROM gate_results`

// GateResultsForStep returns every recorded result for a step, in insertion
// order.
func GateResultsForStep(conn *sql.DB, stepID int) ([]GateResultRow, error) {
	return scanGateResults(conn.Query(gateResultSelect+` WHERE step_id = ? ORDER BY id`, stepID))
}

// GateResultsForStepTx is GateResultsForStep inside a transaction, for the
// readers that run within one.
func GateResultsForStepTx(tx *sql.Tx, stepID int) ([]GateResultRow, error) {
	return scanGateResults(tx.Query(gateResultSelect+` WHERE step_id = ? ORDER BY id`, stepID))
}

// NextGateOrdinal returns the ordinal a new result for this step+gate takes.
//
// Ordinals are per (step, gate) and ascend, which is what carries §4's
// "flaky-declared re-runs recorded individually": each attempt is its own row,
// never an overwrite and never an aggregate.
func NextGateOrdinal(conn *sql.DB, stepID int, gate string) (int, error) {
	var next sql.NullInt64
	err := conn.QueryRow(
		`SELECT MAX(ordinal) FROM gate_results WHERE step_id = ? AND gate = ?`,
		stepID, gate).Scan(&next)
	if err != nil {
		return 0, fmt.Errorf("reading the gate ordinal: %w", err)
	}
	if !next.Valid {
		return 0, nil
	}
	return int(next.Int64) + 1, nil
}

// HasGateResult reports whether a step+gate has any recorded result.
//
// This is the at-least-once detector (§7.5 A1): a `saga_stage` of `gate:<name>`
// with no result row is a started-but-unrecorded gate — a crash between the
// `gate-started` commit and the result commit.
func HasGateResult(conn *sql.DB, stepID int, gate string) (bool, error) {
	var exists bool
	err := conn.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM gate_results WHERE step_id = ? AND gate = ?)`,
		stepID, gate).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("probing for a gate result: %w", err)
	}
	return exists, nil
}

// InsertTrustCacheTx records what a run considered trusted, and when (§4.5).
//
// IT IS AN AUDIT RECORD, NEVER AN AUTHORIZATION SHORTCUT. Every gate consults
// the LIVE trust store on every execution (§7.2 M1); nothing is ever executed
// because a row here says a previous run matched. The opposite implementation is
// the tempting one and it is a revocation failure: a cache hit that authorized a
// spawn would make `trust rm` take effect only after the cache cleared.
//
// `kind` is TrustKindGate or TrustKindAction (§6.3). It exists so the one
// question this table answers stays one query when actions start consulting the
// same store: a second table keyed by the shape of the caller would split it.
// Every pre-v9 row reads `gate` from the column default, which is what it was.
func InsertTrustCacheTx(
	tx *sql.Tx, runID int, kind, gate, argvSHA256, entryName string,
	matched, prefix bool, atMS int64,
) error {
	_, err := tx.Exec(
		`INSERT OR IGNORE INTO trust_cache
		   (run_id, kind, gate, argv_sha256, entry_name, matched, prefix, at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, kind, gate, argvSHA256, entryName,
		boolToInt(matched), boolToInt(prefix), atMS)
	if err != nil {
		return fmt.Errorf("recording the trust decision: %w", err)
	}
	return nil
}

func scanGateResults(rows *sql.Rows, err error) ([]GateResultRow, error) {
	if err != nil {
		return nil, fmt.Errorf("reading gate results: %w", err)
	}
	return scanRows(rows, "gate results", func(r *sql.Rows) (GateResultRow, error) {
		var (
			row       GateResultRow
			argv      sql.NullString
			exit      sql.NullInt64
			reason    sql.NullString
			truncated int
			pre       int
			stub      int
			stubEntry int
		)
		if err := r.Scan(
			&row.ID, &row.RunID, &row.StepID, &row.Gate, &row.Ordinal, &argv, &exit,
			&row.DurationMS, &row.Output, &truncated, &row.Verdict, &pre, &stub,
			&stubEntry,
			&reason, &row.CreatedAtMS,
		); err != nil {
			return GateResultRow{}, fmt.Errorf("reading a gate result: %w", err)
		}
		if argv.Valid && argv.String != "" {
			if err := json.Unmarshal([]byte(argv.String), &row.Argv); err != nil {
				return GateResultRow{}, fmt.Errorf("reading a recorded argv: %w", err)
			}
		}
		if exit.Valid {
			code := int(exit.Int64)
			row.Exit = &code
		}
		row.Reason = reason.String
		row.Truncated, row.Pre, row.Stub = truncated != 0, pre != 0, stub != 0
		row.StubEntry = stubEntry != 0
		return row, nil
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
