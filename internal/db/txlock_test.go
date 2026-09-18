package db

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestBeginTakesWriteLockUpFront proves a read-then-write transaction on one
// Open handle survives a commit from a second Open handle on the same file —
// the shape every dispatcher process hits when it claims: LoadScheduler reads,
// ClaimStepTx writes, one transaction.
//
// Handle A begins, reads, and then writes while handle B commits in between.
// With a deferred BEGIN, A holds only a read snapshot after its SELECT, B's
// commit makes that snapshot stale, and A's UPDATE fails at once with
// SQLITE_BUSY_SNAPSHOT — busy_timeout never runs, because waiting cannot
// refresh a snapshot. With BEGIN IMMEDIATE, A owns the write lock from Begin,
// so B waits inside busy_timeout and both writes land.
//
// B runs in a goroutine and A waits on it with a deadline rather than
// unconditionally: under immediate mode B cannot finish until A commits, so a
// blocking wait would deadlock the very case this test asserts works.
func TestBeginTakesWriteLockUpFront(t *testing.T) {
	path := filepath.Join(t.TempDir(), "txlock.db")

	a, err := Open(path)
	testsupport.Must(t, err, "Open(a): %v", err)
	defer a.Close()
	b, err := Open(path)
	testsupport.Must(t, err, "Open(b): %v", err)
	defer b.Close()

	_, err = a.Exec("CREATE TABLE counter (id INTEGER PRIMARY KEY, n INTEGER NOT NULL)")
	testsupport.Must(t, err, "creating table: %v", err)
	_, err = a.Exec("INSERT INTO counter (id, n) VALUES (1, 0)")
	testsupport.Must(t, err, "seeding row: %v", err)

	tx, err := a.Begin()
	testsupport.Must(t, err, "a.Begin: %v", err)
	defer tx.Rollback()

	var n int
	err = tx.QueryRow("SELECT n FROM counter WHERE id = 1").Scan(&n)
	testsupport.Must(t, err, "a reads: %v", err)

	bDone := make(chan error, 1)
	go func() {
		_, err := b.Exec("UPDATE counter SET n = n + 1 WHERE id = 1")
		bDone <- err
	}()

	// Give B its chance to commit between A's read and A's write. Under a
	// deferred BEGIN it does; under an immediate BEGIN it blocks on A's lock
	// and this deadline expires instead.
	select {
	case err := <-bDone:
		testsupport.Must(t, err, "b writes while a holds a snapshot: %v", err)
		bDone <- nil
	case <-time.After(time.Second):
	}

	_, err = tx.Exec("UPDATE counter SET n = n + 1 WHERE id = 1")
	testsupport.Must(t, err, "a writes after b committed: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "a commits: %v", err)

	err = <-bDone
	testsupport.Must(t, err, "b writes after a committed: %v", err)

	err = a.QueryRow("SELECT n FROM counter WHERE id = 1").Scan(&n)
	testsupport.Must(t, err, "final read: %v", err)
	if n != 2 {
		t.Fatalf("counter = %d after two committed increments, want 2", n)
	}
}
