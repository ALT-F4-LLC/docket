package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// leaseTestDB returns a migrated database with one issue, ready to claim.
func leaseTestDB(t *testing.T) (*sql.DB, int) {
	t.Helper()
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)
	id, err := CreateIssue(db, &model.Issue{
		Title:    "lease subject",
		Status:   model.StatusTodo,
		Priority: model.PriorityNone,
		Kind:     model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)
	return db, id
}

const testTTLMS = 60_000

// TestClaimStoresOnlyTheHash is the hash-at-rest proof: a stolen database
// file must yield no live capability.
func TestClaimStoresOnlyTheHash(t *testing.T) {
	db, id := leaseTestDB(t)

	token, _, err := ClaimIssue(db, id, "worker-1", testTTLMS, model.NowMS())
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	// Dump every text column of the row and assert the plaintext token appears
	// in none of them.
	rows, err := db.Query(`SELECT * FROM issues WHERE id = ?`, id)
	testsupport.Must(t, err, "querying row: %v", err)
	defer rows.Close()

	cols, err := rows.Columns()
	testsupport.Must(t, err, "reading columns: %v", err)
	if !rows.Next() {
		t.Fatal("no row returned")
	}
	cells := make([]any, len(cols))
	for i := range cells {
		var s sql.NullString
		cells[i] = &s
	}
	err = rows.Scan(cells...)
	testsupport.Must(t, err, "scanning row: %v", err)
	for i, cell := range cells {
		got := cell.(*sql.NullString).String
		if got != "" && strings.Contains(got, token) {
			t.Errorf("column %q contains the plaintext token", cols[i])
		}
	}
}

// TestClaimPredicateIsLoadBearing is the mutation test: it re-runs the
// concurrency scenario against a deliberately BROKEN claim — the same UPDATE
// with the CAS predicate removed — and asserts that version fails.
//
// A guard that cannot fail is not a guard. This proves the exclusion in
// TestStoreContract's ConcurrentClaims case comes from
// `(owner IS NULL OR expires_ms <= ?)` and not from incidental serialization
// elsewhere in the stack.
func TestClaimPredicateIsLoadBearing(t *testing.T) {
	db, id := leaseTestDB(t)
	now := int64(1_000_000)

	// brokenClaim is ClaimIssue with the predicate stripped: every caller
	// matches the row, so every caller believes it won.
	brokenClaim := func(owner string) error {
		_, hash, err := model.MintToken()
		if err != nil {
			return err
		}
		_, err = db.Exec(
			`UPDATE issues
			    SET owner = ?, token_hash = ?, expires_ms = ?,
			        attempt = attempt + 1, version = version + 1
			  WHERE id = ?`,
			owner, hash, now+testTTLMS, id,
		)
		return err
	}

	const claimants = 4
	for i := range claimants {
		err := brokenClaim("worker")
		testsupport.Must(t, err, "broken claim %d: %v", i, err)
	}

	lease, err := GetIssueLease(db, id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)

	// Without the predicate every claim applies: the attempt counter records
	// all of them, and the last writer silently stole the lease from the
	// first. If this assertion ever fails, the predicate is no longer what
	// produces mutual exclusion and the ConcurrentClaims contract case is
	// passing for the wrong reason.
	if lease.Attempt != claimants {
		t.Fatalf("unguarded claim applied %d times, want %d — "+
			"the mutation test is no longer exercising the predicate it targets",
			lease.Attempt, claimants)
	}

	// And the guarded path, on the same row state, refuses.
	if _, _, err := ClaimIssue(db, id, "worker-guarded", testTTLMS, now); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("guarded claim error = %v, want ErrLeaseHeld", err)
	}
}
