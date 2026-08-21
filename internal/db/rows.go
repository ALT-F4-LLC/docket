package db

import (
	"database/sql"
	"fmt"
)

// scanner abstracts *sql.Row and *sql.Rows for scanning a single row.
type scanner interface {
	Scan(dest ...any) error
}

// scanRows collects every row of an already-open *sql.Rows into a slice,
// using scan to decode each row, and closes rows before returning. It
// factors out the Query -> defer Close -> for rows.Next() -> rows.Err()
// shape shared by every list query in this package; each caller supplies
// only the column-specific Scan call, which is where the real behavior
// (and the genuinely different column lists) lives.
//
// It is for collect-into-a-slice queries only: loops that filter, skip, or
// accumulate into a map (rollups.go's MetadataRollup, usage.go's
// StepAttemptsFor, the Hydrate* family, and similar) stay hand-written loops.
//
// Errors are returned exactly as produced: a scan error carries whatever
// wrapping the scan func gave it, and a rows.Err() failure is wrapped as
// "iterating <what>: %w" so each caller keeps its own distinct message
// without passing a runtime format string that go vet cannot check.
func scanRows[T any](rows *sql.Rows, what string, scan func(*sql.Rows) (T, error)) ([]T, error) {
	defer rows.Close()

	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating %s: %w", what, err)
	}
	return out, nil
}
