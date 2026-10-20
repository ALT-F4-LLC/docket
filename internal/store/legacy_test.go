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

// openLegacyPair opens a migrated database file through OpenSQLiteDB and a
// second connection to it through OpenSQLiteReaderDB.
func openLegacyPair(t *testing.T) (writer, reader *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "issues.db")
	writer, err := OpenSQLiteDB(path)
	testsupport.Must(t, err, "OpenSQLiteDB: %v", err)
	t.Cleanup(func() { writer.Close() })
	err = db.Initialize(writer)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(writer)
	testsupport.Must(t, err, "Migrate: %v", err)
	reader, err = OpenSQLiteReaderDB(path)
	testsupport.Must(t, err, "OpenSQLiteReaderDB: %v", err)
	t.Cleanup(func() { reader.Close() })
	return writer, reader
}

func TestLegacyOpenSQLiteDBEnforcesForeignKeys(t *testing.T) {
	writer, _ := openLegacyPair(t)

	var enabled int
	err := writer.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled)
	testsupport.Must(t, err, "reading PRAGMA foreign_keys: %v", err)
	if enabled != 1 {
		t.Fatalf("OpenSQLiteDB connection has foreign_keys = %d, want 1", enabled)
	}
}

func TestLegacyOpenSQLiteReaderDBBeginsWithoutTheWriteLock(t *testing.T) {
	const key = db.KeyVoteHoldVoters
	writer, reader := openLegacyPair(t)

	writeTx, err := writer.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { writeTx.Rollback() })
	_, err = db.SetConfigTx(writeTx, 0, key, "bob")
	testsupport.Must(t, err, "writing inside the write transaction: %v", err)

	readTx, err := reader.Begin()
	testsupport.Must(t, err, "plain Begin on the reader while a write is open: %v", err)
	t.Cleanup(func() { readTx.Rollback() })
	entry, err := db.GetConfigTx(readTx, 0, key)
	testsupport.Must(t, err, "reading through the reader: %v", err)
	if entry.Source != "default" {
		t.Fatalf("reader saw %s = %q (source %s) from an uncommitted write, want the default", key, entry.Value, entry.Source)
	}
}

func TestBeginSQLiteTxCommitMakesTheWriteVisible(t *testing.T) {
	const key = db.KeyVoteHoldVoters
	conn := openMigratedDB(t)

	tx, err := BeginSQLiteTx(conn)
	testsupport.Must(t, err, "BeginSQLiteTx: %v", err)
	if tx == nil {
		t.Fatal("BeginSQLiteTx returned a nil *sql.Tx")
	}
	_, err = db.SetConfigTx(tx, 0, key, "bob")
	testsupport.Must(t, err, "writing inside the opened transaction: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "committing: %v", err)

	entry, err := db.GetConfig(conn, 0, key)
	testsupport.Must(t, err, "reading after commit: %v", err)
	if entry.Value != "bob" || entry.Source != "set" {
		t.Fatalf("after commit %s = %q (source %s), want %q set", key, entry.Value, entry.Source, "bob")
	}
}

func TestBeginSQLiteTxRollbackDiscardsTheWrite(t *testing.T) {
	const key = db.KeyVoteHoldVoters
	conn := openMigratedDB(t)

	tx, err := BeginSQLiteTx(conn)
	testsupport.Must(t, err, "BeginSQLiteTx: %v", err)
	if tx == nil {
		t.Fatal("BeginSQLiteTx returned a nil *sql.Tx")
	}
	_, err = db.SetConfigTx(tx, 0, key, "bob")
	testsupport.Must(t, err, "writing inside the opened transaction: %v", err)
	err = tx.Rollback()
	testsupport.Must(t, err, "rolling back: %v", err)

	entry, err := db.GetConfig(conn, 0, key)
	testsupport.Must(t, err, "reading after rollback: %v", err)
	if entry.Source != "default" {
		t.Fatalf("after rollback %s = %q (source %s), want the default", key, entry.Value, entry.Source)
	}
}

func TestBeginSQLiteReadSnapshotDoesNotSeeAnUncommittedWrite(t *testing.T) {
	const key = db.KeyVoteHoldVoters
	writer, reader := openLegacyPair(t)

	writeTx, err := BeginSQLiteTx(writer)
	testsupport.Must(t, err, "BeginSQLiteTx: %v", err)
	t.Cleanup(func() { writeTx.Rollback() })
	_, err = db.SetConfigTx(writeTx, 0, key, "bob")
	testsupport.Must(t, err, "writing inside the write transaction: %v", err)

	snapshot, err := BeginSQLiteReadSnapshot(reader)
	testsupport.Must(t, err, "BeginSQLiteReadSnapshot: %v", err)
	if snapshot == nil {
		t.Fatal("BeginSQLiteReadSnapshot returned a nil *sql.Tx")
	}
	t.Cleanup(func() { snapshot.Rollback() })
	entry, err := db.GetConfigTx(snapshot, 0, key)
	testsupport.Must(t, err, "reading through the snapshot: %v", err)
	if entry.Source != "default" {
		t.Fatalf("snapshot saw %s = %q (source %s) from an uncommitted write, want the default", key, entry.Value, entry.Source)
	}
}

func TestSQLTxRejectsATxThatIsNotSQLite(t *testing.T) {
	type otherTx struct{ ReadTx }

	sqlTx, ok := SQLTx(otherTx{})
	if ok || sqlTx != nil {
		t.Fatalf("SQLTx(non-SQLite Tx) = (%v, %t), want (nil, false)", sqlTx, ok)
	}
}
