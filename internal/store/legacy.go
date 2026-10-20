package store

import (
	"database/sql"
	"errors"

	"github.com/ALT-F4-LLC/docket/internal/db"
)

// borrowedStore is a sqliteStore over a *sql.DB the caller owns, so Close
// leaves that handle open.
type borrowedStore struct {
	*sqliteStore
}

func (borrowedStore) Close() error { return nil }

// StoreOn wraps a *sql.DB the caller already owns as a Store, for callers not
// yet holding one of their own. Every method runs on db, with no second
// connection, and Close does not close it. BeginReadSnapshot also runs on db,
// so on a single-connection pool such as db.Open's it waits for an open Tx.
func StoreOn(db *sql.DB) Store {
	return borrowedStore{&sqliteStore{writer: db, reader: db}}
}

// TxOn wraps a transaction the caller already owns as a Tx. Every method runs
// inside tx, and Commit and Rollback act on tx itself.
func TxOn(tx *sql.Tx) Tx {
	return &sqliteTx{tx: tx}
}

// OpenSQLiteDB opens the SQLite database at path as the single-writer pool
// db.Open documents, for callers that still hold a raw *sql.DB.
func OpenSQLiteDB(path string) (*sql.DB, error) {
	return db.Open(path)
}

// OpenSQLiteReaderDB opens a second connection to the database at path, as
// db.OpenReader documents, whose transactions take no write lock.
func OpenSQLiteReaderDB(path string) (*sql.DB, error) {
	return db.OpenReader(path)
}

// BeginSQLiteTx opens a write transaction on conn through the Store interface
// and returns it as a *sql.Tx, for code that still takes one. The caller must
// Commit or Rollback it.
func BeginSQLiteTx(conn *sql.DB) (*sql.Tx, error) {
	tx, err := StoreOn(conn).Begin()
	if err != nil {
		return nil, err
	}
	return unwrapSQLiteTx(tx)
}

// BeginSQLiteReadSnapshot opens a read snapshot on conn through the Store
// interface and returns it as a *sql.Tx, for code that still takes one. It runs
// on conn, so on a db.Open pool it waits for an open write transaction; open
// it on an OpenSQLiteReaderDB connection to avoid that. The caller must
// Rollback it.
func BeginSQLiteReadSnapshot(conn *sql.DB) (*sql.Tx, error) {
	tx, err := StoreOn(conn).BeginReadSnapshot()
	if err != nil {
		return nil, err
	}
	return unwrapSQLiteTx(tx)
}

// SQLTx returns the *sql.Tx under a transaction the SQLite store opened, and
// false for any other implementation.
func SQLTx(tx ReadTx) (*sql.Tx, bool) {
	sqlite, ok := tx.(*sqliteTx)
	if !ok {
		return nil, false
	}
	return sqlite.tx, true
}

// unwrapSQLiteTx is SQLTx for a transaction StoreOn opened, which is always
// the SQLite one; it rolls tx back and fails rather than leak it otherwise.
func unwrapSQLiteTx(tx ReadTx) (*sql.Tx, error) {
	sqlTx, ok := SQLTx(tx)
	if !ok {
		return nil, errors.Join(errors.New("store: StoreOn opened a transaction that is not SQLite"), tx.Rollback())
	}
	return sqlTx, nil
}
