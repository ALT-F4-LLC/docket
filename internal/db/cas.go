package db

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
)

// ErrVersionConflict is returned when a CAS-guarded mutation is attempted
// against a row whose version differs from the caller's expectation — someone
// else wrote to it in between. Callers surface this as CONFLICT (exit 4),
// distinct from ErrNotFound (exit 2).
var ErrVersionConflict = errors.New("version conflict")

// GetVersion returns the current CAS version of a row.
// The table must be one of versionedTables.
func GetVersion(db *sql.DB, table string, id int) (int, error) {
	if !isVersionedTable(table) {
		return 0, fmt.Errorf("table %q does not carry a version column", table)
	}
	var version int
	// Safe: table is validated against the versionedTables allowlist above.
	query := fmt.Sprintf(`SELECT version FROM %s WHERE id = ?`, table)
	err := db.QueryRow(query, id).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("reading %s version: %w", table, err)
	}
	return version, nil
}

// CheckAndBumpVersion enforces an --if-version precondition inside tx and
// increments the row's version.
//
// ifVersion nil means "no precondition": the version is still bumped, so every
// mutation advances it and concurrent CAS writers are detected.
//
// The zero-rows case is deliberately re-probed rather than assumed: a missing
// row is ErrNotFound (exit 2) while a present row at a different version is
// ErrVersionConflict (exit 4). Collapsing the two would report a live
// conflict as a missing entity.
func CheckAndBumpVersion(tx *sql.Tx, table string, id int, ifVersion *int) error {
	if !isVersionedTable(table) {
		return fmt.Errorf("table %q does not carry a version column", table)
	}

	// Safe: table is validated against the versionedTables allowlist above.
	if ifVersion == nil {
		stmt := fmt.Sprintf(`UPDATE %s SET version = version + 1 WHERE id = ?`, table)
		res, err := tx.Exec(stmt, id)
		if err != nil {
			return fmt.Errorf("bumping %s version: %w", table, err)
		}
		return requireAffected(res)
	}

	stmt := fmt.Sprintf(
		`UPDATE %s SET version = version + 1 WHERE id = ? AND version = ?`, table,
	)
	res, err := tx.Exec(stmt, id, *ifVersion)
	if err != nil {
		return fmt.Errorf("CAS on %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 1 {
		return nil
	}

	// Zero rows: distinguish "gone" from "changed underneath us".
	var exists bool
	probe := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id = ?)`, table)
	if err := tx.QueryRow(probe, id).Scan(&exists); err != nil {
		return fmt.Errorf("probing %s existence: %w", table, err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrVersionConflict
}

// isVersionedTable reports whether the table carries a CAS version column.
func isVersionedTable(table string) bool {
	return slices.Contains(versionedTables, table)
}
