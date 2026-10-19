package pgstore

import (
	"context"
	"database/sql"
	"fmt"
)

// writeLockKey is the advisory-lock key every read-write transaction holds:
// the ASCII bytes of "docket" read as a big-endian integer. Advisory locks are
// scoped to the database, so the key serializes writers from every process
// connected to it.
const writeLockKey int64 = 0x646f636b6574

// WithWriteTx runs fn in a read-write transaction that holds the global write
// lock, so no two writers overlap. The lock is transaction-scoped: commit or
// rollback releases it, and a pooled connection never carries it past the
// transaction. fn's error rolls the transaction back and is returned.
func WithWriteTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning postgres write transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, writeLockKey); err != nil {
		return fmt.Errorf("taking the postgres write lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing postgres write transaction: %w", err)
	}
	return nil
}

// WithReadTx runs fn in a READ ONLY transaction that takes no write lock, so a
// read proceeds while a writer holds it. The server refuses any write fn
// attempts. The transaction is always rolled back.
func WithReadTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("beginning postgres read transaction: %w", err)
	}
	defer tx.Rollback()
	return fn(tx)
}
