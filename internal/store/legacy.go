package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// LegacyDB, LegacyTx and LegacyReadTx name the raw database/sql handles that
// code not yet moved onto Store, Tx and ReadTx still passes around. They are
// aliases, so a *sql.DB or *sql.Tx is assignable to them in either direction
// and keeps its methods; LegacyReadTx marks a *sql.Tx used only for reads.
type (
	LegacyDB     = *sql.DB
	LegacyTx     = *sql.Tx
	LegacyReadTx = *sql.Tx
)

// borrowedStore is a sqliteStore over a *sql.DB the caller owns, so Close
// leaves that handle open.
type borrowedStore struct {
	*sqliteStore
}

func (borrowedStore) Close() error { return nil }

// borrowedSnapshotWait bounds how long a StoreOn snapshot waits for a free
// connection on the caller's pool.
const borrowedSnapshotWait = 2 * time.Second

// BeginReadSnapshot opens the snapshot on the borrowed pool, but fails once no
// connection frees up within borrowedSnapshotWait instead of waiting for a Tx
// that may hold the pool's only connection until after this call returns.
//
// The context is cancelled only when the wait expires, never after a snapshot
// is handed out, because database/sql rolls a transaction back when its
// context is cancelled.
func (s borrowedStore) BeginReadSnapshot() (ReadTx, error) {
	ctx, cancel := context.WithCancel(context.Background())
	expiry := time.AfterFunc(borrowedSnapshotWait, cancel)
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if !expiry.Stop() {
		if err == nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = rbErr
			}
		}
		return nil, errors.Join(fmt.Errorf("store: no connection free for a read snapshot within %v; "+
			"a Tx open on the same *sql.DB may hold it, so open the snapshot on OpenSQLiteReaderDB", borrowedSnapshotWait), err)
	}
	if err != nil {
		return nil, err
	}
	return &sqliteTx{tx: tx}, nil
}

// StoreOn wraps a *sql.DB the caller already owns as a Store, for callers not
// yet holding one of their own. Every method runs on db, with no second
// connection, and Close does not close it. BeginReadSnapshot also runs on db;
// on a single-connection pool such as db.Open's, while a Tx is open it returns
// an error after borrowedSnapshotWait rather than waiting for that Tx.
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
// on conn, so on a db.Open pool it fails while a write transaction stays open,
// as StoreOn documents; open it on an OpenSQLiteReaderDB connection to avoid
// that. The caller must Rollback it.
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

// SQLDB returns the *sql.DB under a SQLite Store, and an error for any other
// implementation. It is how an internal/engine Store form reaches the SQLite
// handle; nothing outside internal/engine and internal/store calls it.
//
// For a Store from OpenSQLite it returns the writer pool, db.Open's
// single-connection pool whose Begin is BEGIN IMMEDIATE; for one from StoreOn
// it returns the wrapped *sql.DB. Either way the engine body sees the same
// handle its LegacyDB form passed before.
//
// Naming rule: the Store form of an exported engine function or method that
// takes a store.LegacyDB is its name with the suffix Store, on the same
// receiver, with store.Store in the LegacyDB parameter's place and every
// other parameter and result unchanged (CompleteStep and CompleteStepStore).
// The Store form holds the original body behind one prologue that calls
// SQLDB; the LegacyDB form keeps its name and signature, and its body is one
// statement calling the Store form with StoreOn.
//
// On a Store that is not SQLite, such as internal/db/pgstore's, a Store form
// that returns an error returns SQLDB's error before reading or writing
// anything. Doctor, ScopeEditFrozenForActiveRuns,
// DescriptionEditFrozenForActiveRuns and OverridePassSkipsInterposedTargets
// return no error, so their Store forms panic with it instead: an empty
// result there would read as "nothing frozen" or "nothing skipped", and a
// freeze guard must fail closed.
//
// The retarget in docs/design/sql-handle-migration.md points LegacyDB at
// Store and removes the LegacyDB forms; SQLDB goes once no engine body passes
// a *sql.DB to an internal/db helper.
func SQLDB(s Store) (*sql.DB, error) {
	switch s := s.(type) {
	case *sqliteStore:
		return s.writer, nil
	case borrowedStore:
		return s.writer, nil
	}
	return nil, fmt.Errorf("store: %T is not a SQLite store, so it has no *sql.DB", s)
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

// CreateCommentIdempotentTx and StatusReachedAt forward to the internal/db
// helpers of the same name, which take a raw handle; their contracts are
// documented there. They are functions rather than func-typed variables so no
// importer can reassign them.

func CreateCommentIdempotentTx(tx LegacyTx, comment *model.Comment, scope, idempotencyKey string) (int, bool, error) {
	return db.CreateCommentIdempotentTx(tx, comment, scope, idempotencyKey)
}

func StatusReachedAt(conn LegacyDB, ids []int, status string) (map[int]time.Time, error) {
	return db.StatusReachedAt(conn, ids, status)
}
