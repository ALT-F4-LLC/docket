package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Open opens or creates the SQLite database at the given path.
// It sets pragmas for WAL mode, foreign key enforcement, and busy timeout,
// and makes every transaction on the handle BEGIN IMMEDIATE.
func Open(dbPath string) (*sql.DB, error) {
	// _txlock=immediate makes Begin() take the write lock up front instead
	// of at the first write. Docket's transactions read before they write —
	// a claim loads the scheduler and then claims the step in one
	// transaction — and under a deferred BEGIN in WAL mode such a transaction
	// holds only a read snapshot until its first write. When another process
	// commits in between, the write upgrade fails immediately with
	// SQLITE_BUSY_SNAPSHOT: busy_timeout cannot help, because no amount of
	// waiting refreshes a stale snapshot. With the lock taken at Begin, a
	// competing writer waits inside busy_timeout instead, and the transaction
	// that wins reads state no one else can change under it.
	//
	// Read-only transactions on this handle take the lock too. In WAL mode
	// that blocks other writers, never readers, and every such transaction
	// here is a short SELECT — a millisecond wait for a writer, not a stall.
	db, err := sql.Open("sqlite", dbPath+"?_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// SQLite is single-writer; limit the pool to one connection to avoid
	// lock contention and make the single-connection intent explicit.
	db.SetMaxOpenConns(1)

	// busy_timeout MUST come first. Setting journal_mode=WAL briefly takes a
	// write lock, so with the timeout still at its default of 0 a process
	// opening the database while another holds that lock fails immediately
	// with SQLITE_BUSY instead of waiting. That turns concurrent opens — N
	// dispatchers claiming at once — into spurious open failures, which is
	// exactly the case leases exist to serve.
	//
	// busy_timeout is a per-connection setting and takes no lock itself, so
	// applying it first is always safe.
	pragmas := []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	}

	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("setting pragma %q: %w", p, err)
		}
	}

	return db, nil
}

// OpenReader opens a SEPARATE connection to the same database file, for
// best-effort point reads that must never contend with Open's single writer
// connection.
//
// Open caps its pool at one connection because SQLite is single-writer — but
// that same cap means a query issued on THAT connection while it holds an open
// transaction (a caller mid-`BeginTx`) blocks forever: database/sql serializes
// all use of one *sql.DB through its pool, transaction included, and there is
// no second connection for the query to check out. That is a guaranteed
// self-deadlock, not a race — it fires every time a caller formats or looks up
// something from inside its own open transaction.
//
// WAL mode (set by Open, and already on disk by the time this connects) is
// what makes a SECOND, independent connection the fix rather than a new
// hazard: a reader on its own connection sees a consistent snapshot without
// blocking, or being blocked by, an in-flight writer transaction on Open's
// connection.
//
// This is for best-effort lookups only — id rendering, not decision logic —
// exactly the callers that already tolerate a failed read as "unknown" rather
// than an error.
func OpenReader(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("setting pragma busy_timeout: %w", err)
	}
	return db, nil
}
