package pgstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestPgstoreWriteLock(t *testing.T) {
	db := lockTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithWriteTx(ctx, db, func(*sql.Tx) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstEntered:
	case err := <-firstDone:
		t.Fatalf("first writer returned before entering its transaction: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			close(releaseFirst)
		}
	}
	defer release()

	t.Run("a read-only call returns while a writer holds the lock", func(t *testing.T) {
		readCtx, cancelRead := context.WithTimeout(ctx, 5*time.Second)
		defer cancelRead()
		var one int
		err := WithReadTx(readCtx, db, func(tx *sql.Tx) error {
			return tx.QueryRowContext(readCtx, `SELECT 1`).Scan(&one)
		})
		if err != nil {
			t.Fatalf("read-only call while a writer holds the lock: %v", err)
		}
	})

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- WithWriteTx(ctx, db, func(*sql.Tx) error {
			close(secondEntered)
			return nil
		})
	}()

	// The second writer must queue on the advisory lock rather than enter.
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case <-secondEntered:
			t.Fatal("second writer entered its transaction while the first held the write lock")
		case err := <-secondDone:
			t.Fatalf("second writer returned while the first held the write lock: %v", err)
		default:
		}
		if writeLockWaiters(t, db) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second writer neither entered nor queued on the write lock within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}

	release()
	if err := <-firstDone; err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second writer: %v", err)
	}
	select {
	case <-secondEntered:
	default:
		t.Fatal("second writer returned without running its function")
	}
}

// writeLockWaiters counts sessions queued on writeLockKey. PostgreSQL shows a
// bigint advisory key as its high half in classid and low half in objid, with
// objsubid 1.
func writeLockWaiters(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
		  AND classid::bigint = $1 AND objid::bigint = $2`,
		int64(writeLockKey)>>32, int64(writeLockKey)&0xffffffff,
	).Scan(&n)
	if err != nil {
		t.Fatalf("reading pg_locks: %v", err)
	}
	return n
}

// lockTestDB returns a pool on DOCKET_TEST_POSTGRES_DSN. The advisory lock is
// database-wide, so no schema isolation applies. In CI an unset DSN fails the
// test instead of skipping it.
func lockTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DOCKET_TEST_POSTGRES_DSN")
	if dsn == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("DOCKET_TEST_POSTGRES_DSN is unset in CI; the write-lock check needs a PostgreSQL server")
		}
		t.Skip("DOCKET_TEST_POSTGRES_DSN is unset; skipping the write-lock check")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing DOCKET_TEST_POSTGRES_DSN: %v", err)
	}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { db.Close() })
	return db
}
