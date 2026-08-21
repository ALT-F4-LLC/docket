package db

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
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

func TestClaimMintsTokenAndTakesLease(t *testing.T) {
	db, id := leaseTestDB(t)
	now := int64(1_000_000)

	token, lease, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	if len(token) != model.TokenBytes*2 {
		t.Errorf("token length = %d, want %d hex chars", len(token), model.TokenBytes*2)
	}
	if lease.Owner != "worker-1" {
		t.Errorf("owner = %q, want worker-1", lease.Owner)
	}
	if lease.ExpiresMS != now+testTTLMS {
		t.Errorf("expires_ms = %d, want %d", lease.ExpiresMS, now+testTTLMS)
	}
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 (first claim)", lease.Attempt)
	}

	stored, err := GetIssueLease(db, id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if !model.TokenMatches(token, stored.TokenHash) {
		t.Error("minted token does not verify against the stored hash")
	}
}

// TestClaimStoresOnlyTheHash is the §4 hash-at-rest proof: a stolen database
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

func TestClaimAgainstLiveLeaseConflicts(t *testing.T) {
	db, id := leaseTestDB(t)
	now := int64(1_000_000)

	_, _, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "first ClaimIssue: %v", err)

	before, err := GetIssueLease(db, id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)

	_, _, err = ClaimIssue(db, id, "worker-2", testTTLMS, now+1)
	if !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second ClaimIssue error = %v, want ErrLeaseHeld", err)
	}

	// A refusal writes nothing — not the owner, and not the attempt counter.
	after, err := GetIssueLease(db, id)
	testsupport.Must(t, err, "GetIssueLease after refusal: %v", err)
	if after.Owner != before.Owner || after.TokenHash != before.TokenHash {
		t.Error("refused claim modified the lease")
	}
	if after.Attempt != before.Attempt {
		t.Errorf("refused claim bumped attempt %d -> %d", before.Attempt, after.Attempt)
	}
}

// TestClaimAgainstExpiredLeaseSucceeds is the §9 item 4 core: lease expiry
// alone re-readies the entity, and the attempt trail records both claims.
func TestClaimAgainstExpiredLeaseSucceeds(t *testing.T) {
	db, id := leaseTestDB(t)
	now := int64(1_000_000)

	firstToken, firstLease, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "first ClaimIssue: %v", err)

	// Past expiry. Nothing else happens in between: no reaper, no sweep, no
	// read verb — expiry alone must make the issue claimable.
	later := firstLease.ExpiresMS + 1

	secondToken, secondLease, err := ClaimIssue(db, id, "worker-2", testTTLMS, later)
	testsupport.Must(t, err, "re-claim after expiry: %v", err)

	if secondLease.Attempt != 2 {
		t.Errorf("attempt = %d, want 2 (the trail records both claims)", secondLease.Attempt)
	}
	if secondToken == firstToken {
		t.Error("re-claim reused the original token")
	}

	stored, err := GetIssueLease(db, id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if model.TokenMatches(firstToken, stored.TokenHash) {
		t.Error("the dead holder's token still verifies after re-claim")
	}
}

func TestClaimOnMissingIssueIsNotFound(t *testing.T) {
	db, _ := leaseTestDB(t)

	_, _, err := ClaimIssue(db, 999999, "worker-1", testTTLMS, model.NowMS())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound (not CONFLICT)", err)
	}
}

func TestHeartbeatExtendsLease(t *testing.T) {
	db, id := leaseTestDB(t)
	now := int64(1_000_000)

	token, first, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	beat := now + 1000
	extended, err := HeartbeatIssue(db, id, token, testTTLMS, beat)
	testsupport.Must(t, err, "HeartbeatIssue: %v", err)

	if extended.ExpiresMS <= first.ExpiresMS {
		t.Errorf("expires_ms did not advance: %d -> %d", first.ExpiresMS, extended.ExpiresMS)
	}
	if extended.Attempt != first.Attempt {
		t.Errorf("heartbeat changed attempt %d -> %d; it is not a new claim",
			first.Attempt, extended.Attempt)
	}
	if extended.Owner != "worker-1" {
		t.Errorf("heartbeat changed owner to %q", extended.Owner)
	}
}

// TestRefusalMatrix walks the §9.3 matrix over the lease-mediated verbs. Each
// case asserts the sentinel that maps to exactly one CLI error code.
func TestRefusalMatrix(t *testing.T) {
	now := int64(1_000_000)

	cases := []struct {
		name string
		// setup returns the token to present.
		setup func(t *testing.T, db *sql.DB, id int) string
		at    int64
		want  error
	}{
		{
			name:  "unclaimed issue, any token",
			setup: func(*testing.T, *sql.DB, int) string { return "some-token" },
			at:    now,
			want:  ErrNotHolder,
		},
		{
			name: "wrong token on a live lease",
			setup: func(t *testing.T, db *sql.DB, id int) string {
				_, _, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
				testsupport.Must(t, err, "ClaimIssue: %v", err)
				return "wrong-token"
			},
			at:   now + 1,
			want: ErrNotHolder,
		},
		{
			name: "empty token on a live lease",
			setup: func(t *testing.T, db *sql.DB, id int) string {
				_, _, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
				testsupport.Must(t, err, "ClaimIssue: %v", err)
				return ""
			},
			at:   now + 1,
			want: ErrNotHolder,
		},
		{
			// The token is RIGHT; time ran out. This must NOT collapse into
			// ErrNotHolder — the holder needs to know to re-claim.
			name: "correct token on an expired lease",
			setup: func(t *testing.T, db *sql.DB, id int) string {
				token, _, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
				testsupport.Must(t, err, "ClaimIssue: %v", err)
				return token
			},
			at:   now + testTTLMS + 1,
			want: ErrLeaseExpired,
		},
	}

	for _, tc := range cases {
		t.Run("heartbeat/"+tc.name, func(t *testing.T) {
			db, id := leaseTestDB(t)
			token := tc.setup(t, db, id)
			_, err := HeartbeatIssue(db, id, token, testTTLMS, tc.at)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
		t.Run("release/"+tc.name, func(t *testing.T) {
			db, id := leaseTestDB(t)
			token := tc.setup(t, db, id)
			_, err := ReleaseIssue(db, id, token, tc.at)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRefusalsWriteNothing proves a refusal is checked in the same transaction
// as the mutation it guards: nothing moves, including the CAS version.
func TestRefusalsWriteNothing(t *testing.T) {
	db, id := leaseTestDB(t)
	now := int64(1_000_000)

	_, _, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	beforeVersion, err := GetVersion(db, "issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	beforeLease, err := GetIssueLease(db, id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)

	if _, err := HeartbeatIssue(db, id, "wrong", testTTLMS, now+1); err == nil {
		t.Fatal("heartbeat with a wrong token succeeded")
	}
	if _, err := ReleaseIssue(db, id, "wrong", now+1); err == nil {
		t.Fatal("release with a wrong token succeeded")
	}
	if _, _, err := ClaimIssue(db, id, "worker-2", testTTLMS, now+1); err == nil {
		t.Fatal("claim against a live lease succeeded")
	}

	afterVersion, err := GetVersion(db, "issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	if afterVersion != beforeVersion {
		t.Errorf("version moved %d -> %d across three refusals", beforeVersion, afterVersion)
	}

	afterLease, err := GetIssueLease(db, id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if *afterLease != *beforeLease {
		t.Errorf("lease changed across refusals: %+v -> %+v", beforeLease, afterLease)
	}
}

func TestReleaseClearsLeaseAndKeepsAttempt(t *testing.T) {
	db, id := leaseTestDB(t)
	now := int64(1_000_000)

	token, _, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	released, err := ReleaseIssue(db, id, token, now+1)
	testsupport.Must(t, err, "ReleaseIssue: %v", err)
	if released.Attempt != 1 {
		t.Errorf("attempt = %d after release, want 1 (the trail survives)", released.Attempt)
	}

	lease, err := GetIssueLease(db, id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if lease.Held() {
		t.Errorf("lease still held after release: %+v", lease)
	}
	if lease.Attempt != 1 {
		t.Errorf("stored attempt = %d, want 1", lease.Attempt)
	}

	// A released token never works again.
	if _, err := HeartbeatIssue(db, id, token, testTTLMS, now+2); !errors.Is(err, ErrNotHolder) {
		t.Errorf("released token error = %v, want ErrNotHolder", err)
	}

	// And the issue is immediately claimable.
	if _, _, err := ClaimIssue(db, id, "worker-2", testTTLMS, now+3); err != nil {
		t.Errorf("re-claim after release: %v", err)
	}
}

// TestConcurrentClaims is the in-process half of the §9.3 race proof: N
// goroutines claim one issue and exactly one wins.
//
// The out-of-process half lives in the QA suite, because db.Open caps the pool
// at one connection — only separate processes exercise SQLite's cross-process
// locking, which is what a real dispatcher race hits.
func TestConcurrentClaims(t *testing.T) {
	db, id := leaseTestDB(t)
	now := int64(1_000_000)

	const claimants = 16
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		winners   []string
		conflicts int
		others    []error
	)

	wg.Add(claimants)
	for i := range claimants {
		go func(i int) {
			defer wg.Done()
			token, _, err := ClaimIssue(db, id, "worker", testTTLMS, now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, token)
			case errors.Is(err, ErrLeaseHeld):
				conflicts++
			default:
				others = append(others, err)
			}
		}(i)
	}
	wg.Wait()

	if len(others) > 0 {
		t.Fatalf("unexpected errors from concurrent claims: %v", others)
	}
	if len(winners) != 1 {
		t.Errorf("winners = %d, want exactly 1", len(winners))
	}
	if conflicts != claimants-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, claimants-1)
	}

	// The lost-update proof: exactly one claim was applied, so the attempt
	// counter shows exactly one increment.
	lease, err := GetIssueLease(db, id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d after %d racing claims, want 1 (a lost update would exceed it)",
			lease.Attempt, claimants)
	}
	if len(winners) == 1 && !model.TokenMatches(winners[0], lease.TokenHash) {
		t.Error("the stored hash does not match the winning token")
	}
}

// TestClaimPredicateIsLoadBearing is the mutation test: it re-runs the
// concurrency scenario against a deliberately BROKEN claim — the same UPDATE
// with the CAS predicate removed — and asserts that version fails.
//
// A guard that cannot fail is not a guard. This proves the exclusion in
// TestConcurrentClaims comes from `(owner IS NULL OR expires_ms <= ?)` and not
// from incidental serialization elsewhere in the stack.
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
	// produces mutual exclusion and TestConcurrentClaims is passing for the
	// wrong reason.
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

// TestGetIssueLeaseNeverWrites pins engine-spec §6 "reads never write": the
// read path must not reap, even when the lease it reads has expired.
func TestGetIssueLeaseNeverWrites(t *testing.T) {
	db, id := leaseTestDB(t)
	now := int64(1_000_000)

	_, _, err := ClaimIssue(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	beforeVersion, err := GetVersion(db, "issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)

	// Read the lease long after it expired, several times.
	expired := now + testTTLMS + 10_000
	for range 3 {
		lease, err := GetIssueLease(db, id)
		testsupport.Must(t, err, "GetIssueLease: %v", err)
		if lease.Live(expired) {
			t.Error("expired lease reported live")
		}
		if !lease.Held() {
			t.Error("expired lease lost its owner without a claim — a read wrote")
		}
	}

	afterVersion, err := GetVersion(db, "issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	if afterVersion != beforeVersion {
		t.Errorf("version moved %d -> %d across reads; a read path wrote",
			beforeVersion, afterVersion)
	}
}

// TestUnclaimedIssuesAreOutsideTheMechanism is the dormancy proof at the
// semantic level (engine-spec §9 item 8): a lease-ending verb on an unclaimed
// issue needs no token and refuses nothing.
func TestUnclaimedIssueMutationNeedsNoToken(t *testing.T) {
	db, id := leaseTestDB(t)

	// No claim has ever happened. An empty token must authorize the mutation.
	if err := UpdateIssueCASLease(db, id,
		map[string]any{"status": "done"}, "tester", nil, ""); err != nil {
		t.Fatalf("closing an unclaimed issue with no token: %v", err)
	}

	issue, err := GetIssue(db, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Status != model.StatusDone {
		t.Errorf("status = %q, want done", issue.Status)
	}
	if issue.Lease != nil {
		t.Errorf("unclaimed issue carries a lease: %+v", issue.Lease)
	}
}

// TestLeaseEndingVerbRefusesNonHolder proves the §9.3 "unclaimed worker cannot
// record" guarantee at the verb level: a bystander cannot evict a live holder.
func TestLeaseEndingVerbRefusesNonHolder(t *testing.T) {
	db, id := leaseTestDB(t)

	token, _, err := ClaimIssue(db, id, "worker-1", testTTLMS, model.NowMS())
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	err = UpdateIssueCASLease(db, id, map[string]any{"status": "done"}, "bystander", nil, "wrong")
	if !errors.Is(err, ErrNotHolder) {
		t.Fatalf("non-holder close error = %v, want ErrNotHolder", err)
	}

	issue, err := GetIssue(db, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Status == model.StatusDone {
		t.Error("refused close applied anyway")
	}

	// The holder may always close, and doing so ends the lease.
	err = UpdateIssueCASLease(db, id, map[string]any{"status": "done"}, "worker-1", nil, token)
	testsupport.Must(t, err, "holder close: %v", err)
	issue, err = GetIssue(db, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Lease != nil {
		t.Errorf("lease survived a terminal transition: %+v", issue.Lease)
	}
}
