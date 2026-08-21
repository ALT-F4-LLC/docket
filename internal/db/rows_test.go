package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

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

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	rows, err := db.QueryContext(ctx, `SELECT id FROM probe`)
	testsupport.Must(t, err, "querying probe rows")

	// Give the deadline time to elapse before the loop below drains rows,
	// so Next() observes the cancellation rather than racing it.
	time.Sleep(5 * time.Millisecond)

	_, err = scanRows(rows, "probe rows", func(r *sql.Rows) (int, error) {
		var n int
		scanErr := r.Scan(&n)
		return n, scanErr
	})

	if err == nil {
		t.Fatalf("scanRows returned nil error for a canceled query; want the wrapped context.DeadlineExceeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("scanRows error = %v, want it to unwrap (errors.Is) to context.DeadlineExceeded", err)
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
