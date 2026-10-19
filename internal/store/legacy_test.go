package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func openMigratedDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(filepath.Join(t.TempDir(), "issues.db"))
	testsupport.Must(t, err, "opening the database: %v", err)
	t.Cleanup(func() { conn.Close() })
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)
	return conn
}

func TestTxOnSharesTheCallersTransaction(t *testing.T) {
	const key = db.KeyVoteHoldVoters
	conn := openMigratedDB(t)

	sqlTx, err := conn.Begin()
	testsupport.Must(t, err, "opening the caller's transaction: %v", err)
	t.Cleanup(func() { sqlTx.Rollback() })

	tx := TxOn(sqlTx)
	_, err = tx.SetConfigTx(0, key, "bob")
	testsupport.Must(t, err, "writing through TxOn: %v", err)

	entry, err := db.GetConfigTx(sqlTx, 0, key)
	testsupport.Must(t, err, "reading through the caller's transaction: %v", err)
	if entry.Value != "bob" {
		t.Fatalf("caller's transaction read %s = %q before commit, want %q written through TxOn", key, entry.Value, "bob")
	}

	err = tx.Commit()
	testsupport.Must(t, err, "committing through TxOn: %v", err)
	if err := sqlTx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("caller's transaction Rollback after TxOn Commit = %v, want sql.ErrTxDone", err)
	}
	committed, err := db.GetConfig(conn, 0, key)
	testsupport.Must(t, err, "reading committed state: %v", err)
	if committed.Value != "bob" {
		t.Fatalf("committed %s = %q, want %q", key, committed.Value, "bob")
	}
}

func TestStoreOnCloseLeavesTheCallersDBOpen(t *testing.T) {
	const key = db.KeyVoteHoldVoters
	conn := openMigratedDB(t)

	st := StoreOn(conn)
	err := st.SetConfig(0, key, "alice")
	testsupport.Must(t, err, "writing through StoreOn: %v", err)
	err = st.Close()
	testsupport.Must(t, err, "closing the StoreOn store: %v", err)

	err = conn.Ping()
	testsupport.Must(t, err, "pinging the caller's database after Close: %v", err)
	entry, err := db.GetConfig(conn, 0, key)
	testsupport.Must(t, err, "reading through the caller's database after Close: %v", err)
	if entry.Value != "alice" {
		t.Fatalf("caller's database read %s = %q, want %q", key, entry.Value, "alice")
	}
}
