package db

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestScanRowsWrapsIterationError drives scanRows' rows.Err() tail through a
// real *sql.Rows whose iteration fails mid-scan (a canceled query context),
// rather than through the wrapping function in isolation, and pins two
// things every one of the package's ~54 callers relies on: the returned
// error is non-nil, and it unwraps to the underlying cause via errors.Is —
// exactly what a malformed %w escape in the "iterating %s: %w" format would
// break.
func TestScanRowsWrapsIterationError(t *testing.T) {
	db := mustOpen(t)
	testsupport.Must(t, execErr(db, `CREATE TABLE probe (id INTEGER)`), "create probe table")
	for i := 0; i < 200; i++ {
		testsupport.Must(t, execErr(db, `INSERT INTO probe (id) VALUES (?)`, i), "insert probe row")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rows, err := db.QueryContext(ctx, `SELECT id FROM probe`)
	testsupport.Must(t, err, "querying probe rows")

	canceled := false
	_, err = scanRows(rows, "probe rows", func(r *sql.Rows) (int, error) {
		var n int
		if scanErr := r.Scan(&n); scanErr != nil {
			return 0, scanErr
		}
		if !canceled {
			canceled = true
			// Cancel mid-iteration, then hand off until the *sql.Rows itself
			// reports the cancellation. database/sql records it from a
			// background goroutine, and Next() only consults that record on
			// entry; without this handshake the remaining rows can drain to
			// EOF first, after which Rows.Err() reports nil and the wrapping
			// tail under test never runs. Waiting on r.Err() makes the rest
			// of the iteration observe the cancellation deterministically.
			cancel()
			for r.Err() == nil {
				runtime.Gosched()
			}
		}
		return n, nil
	})

	if err == nil {
		t.Fatalf("scanRows returned nil error for a canceled query; want the wrapped context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scanRows error = %v, want it to unwrap (errors.Is) to context.Canceled", err)
	}
	wantSubstr := "iterating probe rows:"
	if got := err.Error(); len(got) < len(wantSubstr) || got[:len(wantSubstr)] != wantSubstr {
		t.Fatalf("scanRows error = %q, want it to start with %q", got, wantSubstr)
	}
}

func execErr(db *sql.DB, query string, args ...any) error {
	_, err := db.Exec(query, args...)
	return err
}
