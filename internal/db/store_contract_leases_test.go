package db_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

const contractTTLMS = 60_000

// leaseContract pins the issue lease: claim, heartbeat, release, and the
// refusals that guard them.
var leaseContract = []contractCase{
	{"ClaimMintsTokenAndTakesLease", contractClaimMintsTokenAndTakesLease},
	{"ClaimAgainstLiveLeaseConflicts", contractClaimAgainstLiveLeaseConflicts},
	{"ClaimAgainstExpiredLeaseSucceeds", contractClaimAgainstExpiredLeaseSucceeds},
	{"ClaimOnMissingIssueIsNotFound", contractClaimOnMissingIssueIsNotFound},
	{"HeartbeatExtendsLease", contractHeartbeatExtendsLease},
	{"RefusalMatrix", contractRefusalMatrix},
	{"RefusalsWriteNothing", contractRefusalsWriteNothing},
	{"ReleaseClearsLeaseAndKeepsAttempt", contractReleaseClearsLeaseAndKeepsAttempt},
	{"ConcurrentClaims", contractConcurrentClaims},
	{"GetIssueLeaseNeverWrites", contractGetIssueLeaseNeverWrites},
	{"UnclaimedIssueMutationNeedsNoToken", contractUnclaimedIssueMutationNeedsNoToken},
	{"LeaseEndingVerbRefusesNonHolder", contractLeaseEndingVerbRefusesNonHolder},
}

func contractClaimMintsTokenAndTakesLease(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")
	now := int64(1_000_000)

	token, lease, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	if len(token) != model.TokenBytes*2 {
		t.Errorf("token length = %d, want %d hex chars", len(token), model.TokenBytes*2)
	}
	if lease.Owner != "worker-1" {
		t.Errorf("owner = %q, want worker-1", lease.Owner)
	}
	if lease.ExpiresMS != now+contractTTLMS {
		t.Errorf("expires_ms = %d, want %d", lease.ExpiresMS, now+contractTTLMS)
	}
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 (first claim)", lease.Attempt)
	}

	stored, err := st.GetIssueLease(id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if !model.TokenMatches(token, stored.TokenHash) {
		t.Error("minted token does not verify against the stored hash")
	}
}

func contractClaimAgainstLiveLeaseConflicts(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")
	now := int64(1_000_000)

	_, _, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
	testsupport.Must(t, err, "first ClaimIssue: %v", err)

	before, err := st.GetIssueLease(id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)

	_, _, err = st.ClaimIssue(id, "worker-2", contractTTLMS, now+1)
	if !errors.Is(err, db.ErrLeaseHeld) {
		t.Fatalf("second ClaimIssue error = %v, want ErrLeaseHeld", err)
	}

	// A refusal writes nothing — not the owner, and not the attempt counter.
	after, err := st.GetIssueLease(id)
	testsupport.Must(t, err, "GetIssueLease after refusal: %v", err)
	if after.Owner != before.Owner || after.TokenHash != before.TokenHash {
		t.Error("refused claim modified the lease")
	}
	if after.Attempt != before.Attempt {
		t.Errorf("refused claim bumped attempt %d -> %d", before.Attempt, after.Attempt)
	}
}

// contractClaimAgainstExpiredLeaseSucceeds is the liveness core: lease expiry
// alone re-readies the entity, and the attempt trail records both claims.
func contractClaimAgainstExpiredLeaseSucceeds(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")
	now := int64(1_000_000)

	firstToken, firstLease, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
	testsupport.Must(t, err, "first ClaimIssue: %v", err)

	// Past expiry. Nothing else happens in between: no reaper, no sweep, no
	// read verb — expiry alone must make the issue claimable.
	later := firstLease.ExpiresMS + 1

	secondToken, secondLease, err := st.ClaimIssue(id, "worker-2", contractTTLMS, later)
	testsupport.Must(t, err, "re-claim after expiry: %v", err)

	if secondLease.Attempt != 2 {
		t.Errorf("attempt = %d, want 2 (the trail records both claims)", secondLease.Attempt)
	}
	if secondToken == firstToken {
		t.Error("re-claim reused the original token")
	}

	stored, err := st.GetIssueLease(id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if model.TokenMatches(firstToken, stored.TokenHash) {
		t.Error("the dead holder's token still verifies after re-claim")
	}
}

func contractClaimOnMissingIssueIsNotFound(t *testing.T, st store.Store) {
	createContractIssue(t, st, "lease subject")

	_, _, err := st.ClaimIssue(999999, "worker-1", contractTTLMS, model.NowMS())
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound (not CONFLICT)", err)
	}
}

func contractHeartbeatExtendsLease(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")
	now := int64(1_000_000)

	token, first, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	beat := now + 1000
	extended, err := st.HeartbeatIssue(id, token, contractTTLMS, beat)
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

// contractRefusalMatrix walks the refusal matrix over the lease-mediated
// verbs. Each case asserts the sentinel that maps to exactly one CLI error
// code. Every subtest claims an issue of its own, so the subtests share the
// store but no row.
func contractRefusalMatrix(t *testing.T, st store.Store) {
	now := int64(1_000_000)

	cases := []struct {
		name string
		// setup returns the token to present.
		setup func(t *testing.T, id int) string
		at    int64
		want  error
	}{
		{
			name:  "unclaimed issue, any token",
			setup: func(*testing.T, int) string { return "some-token" },
			at:    now,
			want:  db.ErrNotHolder,
		},
		{
			name: "wrong token on a live lease",
			setup: func(t *testing.T, id int) string {
				_, _, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
				testsupport.Must(t, err, "ClaimIssue: %v", err)
				return "wrong-token"
			},
			at:   now + 1,
			want: db.ErrNotHolder,
		},
		{
			name: "empty token on a live lease",
			setup: func(t *testing.T, id int) string {
				_, _, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
				testsupport.Must(t, err, "ClaimIssue: %v", err)
				return ""
			},
			at:   now + 1,
			want: db.ErrNotHolder,
		},
		{
			// The token is RIGHT; time ran out. This must NOT collapse into
			// ErrNotHolder — the holder needs to know to re-claim.
			name: "correct token on an expired lease",
			setup: func(t *testing.T, id int) string {
				token, _, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
				testsupport.Must(t, err, "ClaimIssue: %v", err)
				return token
			},
			at:   now + contractTTLMS + 1,
			want: db.ErrLeaseExpired,
		},
	}

	for _, tc := range cases {
		t.Run("heartbeat/"+tc.name, func(t *testing.T) {
			id := createContractIssue(t, st, "lease subject")
			token := tc.setup(t, id)
			_, err := st.HeartbeatIssue(id, token, contractTTLMS, tc.at)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
		t.Run("release/"+tc.name, func(t *testing.T) {
			id := createContractIssue(t, st, "lease subject")
			token := tc.setup(t, id)
			_, err := st.ReleaseIssue(id, token, tc.at)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// contractRefusalsWriteNothing proves a refusal is checked in the same
// transaction as the mutation it guards: nothing moves, including the CAS
// version.
func contractRefusalsWriteNothing(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")
	now := int64(1_000_000)

	_, _, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	beforeVersion, err := st.GetVersion("issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	beforeLease, err := st.GetIssueLease(id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)

	if _, err := st.HeartbeatIssue(id, "wrong", contractTTLMS, now+1); err == nil {
		t.Fatal("heartbeat with a wrong token succeeded")
	}
	if _, err := st.ReleaseIssue(id, "wrong", now+1); err == nil {
		t.Fatal("release with a wrong token succeeded")
	}
	if _, _, err := st.ClaimIssue(id, "worker-2", contractTTLMS, now+1); err == nil {
		t.Fatal("claim against a live lease succeeded")
	}

	afterVersion, err := st.GetVersion("issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	if afterVersion != beforeVersion {
		t.Errorf("version moved %d -> %d across three refusals", beforeVersion, afterVersion)
	}

	afterLease, err := st.GetIssueLease(id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if *afterLease != *beforeLease {
		t.Errorf("lease changed across refusals: %+v -> %+v", beforeLease, afterLease)
	}
}

func contractReleaseClearsLeaseAndKeepsAttempt(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")
	now := int64(1_000_000)

	token, _, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	released, err := st.ReleaseIssue(id, token, now+1)
	testsupport.Must(t, err, "ReleaseIssue: %v", err)
	if released.Attempt != 1 {
		t.Errorf("attempt = %d after release, want 1 (the trail survives)", released.Attempt)
	}

	lease, err := st.GetIssueLease(id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if lease.Held() {
		t.Errorf("lease still held after release: %+v", lease)
	}
	if lease.Attempt != 1 {
		t.Errorf("stored attempt = %d, want 1", lease.Attempt)
	}

	// A released token never works again.
	if _, err := st.HeartbeatIssue(id, token, contractTTLMS, now+2); !errors.Is(err, db.ErrNotHolder) {
		t.Errorf("released token error = %v, want ErrNotHolder", err)
	}

	// And the issue is immediately claimable.
	if _, _, err := st.ClaimIssue(id, "worker-2", contractTTLMS, now+3); err != nil {
		t.Errorf("re-claim after release: %v", err)
	}
}

// contractConcurrentClaims is the in-process half of the race proof: N
// goroutines claim one issue and exactly one wins.
//
// The out-of-process half lives in the QA suite: only separate processes
// exercise SQLite's cross-process locking, which is what a real dispatcher
// race hits.
func contractConcurrentClaims(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")
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
	for range claimants {
		go func() {
			defer wg.Done()
			token, _, err := st.ClaimIssue(id, "worker", contractTTLMS, now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, token)
			case errors.Is(err, db.ErrLeaseHeld):
				conflicts++
			default:
				others = append(others, err)
			}
		}()
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
	lease, err := st.GetIssueLease(id)
	testsupport.Must(t, err, "GetIssueLease: %v", err)
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d after %d racing claims, want 1 (a lost update would exceed it)",
			lease.Attempt, claimants)
	}
	if len(winners) == 1 && !model.TokenMatches(winners[0], lease.TokenHash) {
		t.Error("the stored hash does not match the winning token")
	}
}

// contractGetIssueLeaseNeverWrites pins "reads never write": the read path
// must not reap, even when the lease it reads has expired.
func contractGetIssueLeaseNeverWrites(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")
	now := int64(1_000_000)

	_, _, err := st.ClaimIssue(id, "worker-1", contractTTLMS, now)
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	beforeVersion, err := st.GetVersion("issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)

	// Read the lease long after it expired, several times.
	expired := now + contractTTLMS + 10_000
	for range 3 {
		lease, err := st.GetIssueLease(id)
		testsupport.Must(t, err, "GetIssueLease: %v", err)
		if lease.Live(expired) {
			t.Error("expired lease reported live")
		}
		if !lease.Held() {
			t.Error("expired lease lost its owner without a claim — a read wrote")
		}
	}

	afterVersion, err := st.GetVersion("issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	if afterVersion != beforeVersion {
		t.Errorf("version moved %d -> %d across reads; a read path wrote",
			beforeVersion, afterVersion)
	}
}

// contractUnclaimedIssueMutationNeedsNoToken is the dormancy proof at the
// semantic level: a lease-ending verb on an unclaimed issue needs no token
// and refuses nothing.
func contractUnclaimedIssueMutationNeedsNoToken(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")

	// No claim has ever happened. An empty token must authorize the mutation.
	if err := st.UpdateIssueCASLease(id,
		map[string]any{"status": "done"}, "tester", nil, ""); err != nil {
		t.Fatalf("closing an unclaimed issue with no token: %v", err)
	}

	issue, err := st.GetIssue(id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Status != model.StatusDone {
		t.Errorf("status = %q, want done", issue.Status)
	}
	if issue.Lease != nil {
		t.Errorf("unclaimed issue carries a lease: %+v", issue.Lease)
	}
}

// contractLeaseEndingVerbRefusesNonHolder proves the "unclaimed worker cannot
// record" guarantee at the verb level: a bystander cannot evict a live holder.
func contractLeaseEndingVerbRefusesNonHolder(t *testing.T, st store.Store) {
	id := createContractIssue(t, st, "lease subject")

	token, _, err := st.ClaimIssue(id, "worker-1", contractTTLMS, model.NowMS())
	testsupport.Must(t, err, "ClaimIssue: %v", err)

	err = st.UpdateIssueCASLease(id, map[string]any{"status": "done"}, "bystander", nil, "wrong")
	if !errors.Is(err, db.ErrNotHolder) {
		t.Fatalf("non-holder close error = %v, want ErrNotHolder", err)
	}

	issue, err := st.GetIssue(id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Status == model.StatusDone {
		t.Error("refused close applied anyway")
	}

	// The holder may always close, and doing so ends the lease.
	err = st.UpdateIssueCASLease(id, map[string]any{"status": "done"}, "worker-1", nil, token)
	testsupport.Must(t, err, "holder close: %v", err)
	issue, err = st.GetIssue(id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Lease != nil {
		t.Errorf("lease survived a terminal transition: %+v", issue.Lease)
	}
}
