package store

import "database/sql"

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
