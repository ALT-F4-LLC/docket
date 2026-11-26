package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
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

	t.Run("the read-only call's transaction refuses writes", func(t *testing.T) {
		readCtx, cancelRead := context.WithTimeout(ctx, 5*time.Second)
		defer cancelRead()
		err := WithReadTx(readCtx, db, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(readCtx, `CREATE TEMP TABLE pgstore_read_only_probe (n int)`)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
			t.Fatalf("write inside WithReadTx: got %v, want SQLSTATE 25006 (read_only_sql_transaction)", err)
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

	t.Run("a failing writer returns its error and releases the lock", func(t *testing.T) {
		errWriter := errors.New("writer failed")
		// ctx outlives this subtest, so only the rollback can end the failed
		// transaction and release its lock before the next writer's deadline.
		if err := WithWriteTx(ctx, db, func(*sql.Tx) error { return errWriter }); !errors.Is(err, errWriter) {
			t.Fatalf("failing writer: got %v, want %v", err, errWriter)
		}
		nextCtx, cancelNext := context.WithTimeout(ctx, 5*time.Second)
		defer cancelNext()
		if err := WithWriteTx(nextCtx, db, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatalf("writer after a failed writer: %v", err)
		}
	})

	t.Run("a writer runs at read committed whatever the server default", func(t *testing.T) {
		cfg := testConnConfig(t)
		cfg.RuntimeParams["default_transaction_isolation"] = "repeatable read"
		strictDB := stdlib.OpenDB(*cfg)
		defer strictDB.Close()

		var isolation string
		err := WithWriteTx(ctx, strictDB, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation)
		})
		if err != nil {
			t.Fatalf("writer on a repeatable-read default: %v", err)
		}
		if isolation != "read committed" {
			t.Fatalf("writer isolation = %q, want %q", isolation, "read committed")
		}
	})
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

// lockTestDB returns a pool on the test server. The advisory lock is
// database-wide, so no schema isolation applies.
func lockTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := stdlib.OpenDB(*testConnConfig(t))
	t.Cleanup(func() { db.Close() })
	return db
}

// testConnConfig returns the connection config for DOCKET_TEST_POSTGRES_DSN
// under the store's DSN policy, with the password from
// DOCKET_TEST_POSTGRES_PASSWORD_FILE. Every live-server test builds its pool
// from it, so none connects without the password file. It skips or fails as
// testsupport.PostgresDSN does when the DSN is empty.
func testConnConfig(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	cfg, err := resolveDSN(testsupport.PostgresDSN(t), testsupport.PostgresPasswordFile(), (*os.File).Stat)
	if err != nil {
		t.Fatalf("resolving DOCKET_TEST_POSTGRES_DSN: %v", err)
	}
	return cfg
}
