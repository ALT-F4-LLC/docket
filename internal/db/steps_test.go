package db

import (
	"database/sql"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// stepTestDB returns a migrated database with a run, an issue, a workflow, and
// one expanded step, ready to claim.
func stepTestDB(t *testing.T) (*sql.DB, int) {
	t.Helper()
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)

	issueID, err := CreateIssue(db, &model.Issue{
		Title:    "step subject",
		Status:   model.StatusTodo,
		Priority: model.PriorityNone,
		Kind:     model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)

	res, err := db.Exec(
		`INSERT INTO workflows (name, version, source_sha256, body, parsed, created_at_ms, row_version)
		 VALUES ('wf', 1, 'sha', 'body', '{}', 1000, 1)`)
	testsupport.Must(t, err, "seeding workflow: %v", err)
	wfID, _ := res.LastInsertId()

	run, err := InsertRun(db, 1, "req", 0, 1000)
	testsupport.Must(t, err, "InsertRun: %v", err)

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = InsertStepTx(tx, StepRow{
		RunID: run.ID, IssueID: issueID, WorkflowID: int(wfID),
		StepName: "implement", Instance: "implement@0", Kind: "executor",
		Executor: "worker", Class: "worker", Status: StepPending,
	}, 1000)
	testsupport.Must(t, err, "InsertStepTx: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	var stepID int
	err = db.QueryRow(`SELECT id FROM steps LIMIT 1`).Scan(&stepID)
	testsupport.Must(t, err, "reading step id: %v", err)
	return db, stepID
}

// TestClaimStepMintsTokenAndTakesLease is the step-level analog of
// TestClaimMintsTokenAndTakesLease, proving the generalized helper behaves
// identically against `steps`.
func TestClaimStepMintsTokenAndTakesLease(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	token, lease, err := ClaimStep(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "ClaimStep: %v", err)
	if token == "" {
		t.Fatal("ClaimStep returned an empty token")
	}
	if lease.Owner != "worker-1" {
		t.Errorf("owner = %q, want worker-1", lease.Owner)
	}
	if lease.ExpiresMS != now+testTTLMS {
		t.Errorf("expires = %d, want %d", lease.ExpiresMS, now+testTTLMS)
	}
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", lease.Attempt)
	}

	// Only the hash is stored — the token is returned exactly once.
	var stored string
	err = db.QueryRow(`SELECT token_hash FROM steps WHERE id = ?`, id).Scan(&stored)
	testsupport.Must(t, err, "reading token_hash: %v", err)
	if stored == token {
		t.Fatal("the plaintext token was stored")
	}
	if !model.TokenMatches(token, stored) {
		t.Error("stored hash does not match the returned token")
	}
}

// TestClaimStepAgainstLiveLeaseConflicts is R5 of §6.9 at the db layer.
func TestClaimStepAgainstLiveLeaseConflicts(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	_, _, err := ClaimStep(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "first ClaimStep: %v", err)
	if _, _, err := ClaimStep(db, id, "worker-2", testTTLMS, now+1); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("second ClaimStep error = %v, want ErrLeaseHeld", err)
	}
}

// TestClaimStepAgainstExpiredLeaseSucceeds is R7 of §6.9: expiry alone
// re-readies a step, with no operator action and no reaper — the liveness
// mechanism §9 item 4 rests on.
func TestClaimStepAgainstExpiredLeaseSucceeds(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	_, _, err := ClaimStep(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "first ClaimStep: %v", err)

	after := now + testTTLMS + 1
	_, lease, err := ClaimStep(db, id, "worker-2", testTTLMS, after)
	testsupport.Must(t, err, "claim after expiry: %v", err)
	if lease.Owner != "worker-2" {
		t.Errorf("owner = %q, want worker-2", lease.Owner)
	}
	// The attempt trail counts both claims, for all time.
	if lease.Attempt != 2 {
		t.Errorf("attempt = %d, want 2 — the attempt trail must record both claims", lease.Attempt)
	}
}

// TestConcurrentStepClaims is the N-goroutine race: exactly one winner.
func TestConcurrentStepClaims(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	const claimants = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		losers  int
		other   []error
	)

	wg.Add(claimants)
	for i := range claimants {
		go func(i int) {
			defer wg.Done()
			_, _, err := ClaimStep(db, id, "worker", testTTLMS, now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, ErrLeaseHeld):
				losers++
			default:
				other = append(other, err)
			}
		}(i)
	}
	wg.Wait()

	for _, err := range other {
		t.Errorf("unexpected claim error: %v", err)
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1", winners)
	}
	if losers != claimants-1 {
		t.Errorf("losers = %d, want %d", losers, claimants-1)
	}

	// The attempt counter records exactly the winning claim: a loser that
	// incremented would mean the CAS did not actually exclude it.
	lease, err := GetStepLease(db, id)
	testsupport.Must(t, err, "GetStepLease: %v", err)
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d after %d concurrent claims, want 1", lease.Attempt, claimants)
	}
}

// TestStepClaimPredicateIsLoadBearing is §6.16's "mutation test repeated for
// the step table": rewrite the predicate away and the concurrency guarantee
// must collapse.
//
// A guard that cannot fail is not a guard. If this test ever passes with the
// predicate intact, TestConcurrentStepClaims is passing for some other reason
// and the CAS is no longer what produces mutual exclusion.
func TestStepClaimPredicateIsLoadBearing(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	// brokenClaim is the step claim with the predicate stripped: every caller
	// matches the row, so every caller believes it won.
	brokenClaim := func(owner string) error {
		_, hash, err := model.MintToken()
		if err != nil {
			return err
		}
		_, err = db.Exec(
			`UPDATE steps
			    SET owner = ?, token_hash = ?, expires_ms = ?,
			        attempt = attempt + 1, row_version = row_version + 1
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

	lease, err := GetStepLease(db, id)
	testsupport.Must(t, err, "GetStepLease: %v", err)
	if lease.Attempt != claimants {
		t.Fatalf("unguarded claim applied %d times, want %d — "+
			"the mutation test is no longer exercising the predicate it targets",
			lease.Attempt, claimants)
	}

	// And the guarded path, on the same row state, refuses.
	if _, _, err := ClaimStep(db, id, "worker-guarded", testTTLMS, now); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("guarded claim error = %v, want ErrLeaseHeld", err)
	}
}

// TestLeaseHelpersAreShared is §6.16's "lease helpers shared with issues proven
// by asserting both call the same function (no duplicated SQL)".
//
// The proof is by function identity rather than by behavior: two independent
// implementations could pass every behavioral test today and drift on the first
// fix applied to only one of them. Comparing the compiled function pointers
// reachable from each entity's entry point asserts there is only ONE
// implementation to fix.
func TestLeaseHelpersAreShared(t *testing.T) {
	// Each pair is (issue-side method value, step-side method value) taken
	// from the two leaseTarget values. Identical function pointers mean both
	// entities execute the same code with a different receiver — the
	// generalization §6.6 requires, rather than a copy.
	pairs := []struct {
		name         string
		issue, steps any
	}{
		{"claimTx", leaseIssues.claimTx, leaseSteps.claimTx},
		{"authorize", leaseIssues.authorize, leaseSteps.authorize},
		{"heartbeat", leaseIssues.heartbeat, leaseSteps.heartbeat},
		{"getLeaseTx", leaseIssues.getLeaseTx, leaseSteps.getLeaseTx},
		{"clearLeaseTx", leaseIssues.clearLeaseTx, leaseSteps.clearLeaseTx},
	}

	for _, p := range pairs {
		issueFn := runtime.FuncForPC(reflect.ValueOf(p.issue).Pointer())
		stepFn := runtime.FuncForPC(reflect.ValueOf(p.steps).Pointer())
		if issueFn.Entry() != stepFn.Entry() {
			t.Errorf(
				"%s: issues reach %s but steps reach %s — the lease implementation "+
					"has been duplicated rather than generalized (TDD §6.6)",
				p.name, issueFn.Name(), stepFn.Name())
		}
	}

	// And the two targets genuinely differ where they must: same SQL, two
	// tables, two CAS column names.
	if leaseIssues.table == leaseSteps.table {
		t.Error("both lease targets name the same table")
	}
	if leaseIssues.versionColumn != "version" || leaseSteps.versionColumn != "row_version" {
		t.Errorf("CAS columns = %q/%q, want version/row_version",
			leaseIssues.versionColumn, leaseSteps.versionColumn)
	}
}

// TestStepRefusalMatrix is §6.9's R1-R4 at the db layer: the same three
// sentinels the issue matrix produces, from the same authorize function.
func TestStepRefusalMatrix(t *testing.T) {
	now := int64(1_000_000)

	cases := []struct {
		name  string
		setup func(t *testing.T, db *sql.DB, id int) (token string)
		at    int64
		want  error
	}{
		{
			name:  "R2 token supplied, step unclaimed",
			setup: func(*testing.T, *sql.DB, int) string { return "deadbeef" },
			at:    now,
			want:  ErrNotHolder,
		},
		{
			name: "R3 token supplied, wrong value",
			setup: func(t *testing.T, db *sql.DB, id int) string {
				_, _, err := ClaimStep(db, id, "worker-1", testTTLMS, now)
				testsupport.Must(t, err, "ClaimStep: %v", err)
				return "not-the-token"
			},
			at:   now,
			want: ErrNotHolder,
		},
		{
			name: "R4 correct token, lease expired",
			setup: func(t *testing.T, db *sql.DB, id int) string {
				token, _, err := ClaimStep(db, id, "worker-1", testTTLMS, now)
				testsupport.Must(t, err, "ClaimStep: %v", err)
				return token
			},
			at:   now + testTTLMS + 1,
			want: ErrLeaseExpired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, id := stepTestDB(t)
			token := tc.setup(t, db, id)

			tx, err := db.Begin()
			testsupport.Must(t, err, "Begin: %v", err)
			defer tx.Rollback()

			if _, err := AuthorizeStepTx(tx, id, token, tc.at); !errors.Is(err, tc.want) {
				t.Errorf("AuthorizeStepTx error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestGetStepLeaseNeverWrites pins engine-spec §6 "reads never write" for the
// step entity: the read path must not reap, even when the lease has expired.
func TestGetStepLeaseNeverWrites(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	_, _, err := ClaimStep(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "ClaimStep: %v", err)

	var before int
	err = db.QueryRow(`SELECT row_version FROM steps WHERE id = ?`, id).Scan(&before)
	testsupport.Must(t, err, "reading row_version: %v", err)

	// Read well past expiry, several times.
	for range 3 {
		_, err := GetStepLease(db, id)
		testsupport.Must(t, err, "GetStepLease: %v", err)
		_, err = GetStep(db, id)
		testsupport.Must(t, err, "GetStep: %v", err)
	}

	var after int
	var owner sql.NullString
	if err := db.QueryRow(
		`SELECT row_version, owner FROM steps WHERE id = ?`, id,
	).Scan(&after, &owner); err != nil {
		t.Fatalf("reading row_version: %v", err)
	}
	if after != before {
		t.Errorf("row_version moved %d -> %d: a read wrote", before, after)
	}
	// The stale owner survives the read — status is computed, not reaped.
	if owner.String != "worker-1" {
		t.Errorf("owner = %q, want the stale worker-1 to survive an expired read", owner.String)
	}
}

// TestSagaStageCASAdvancesExactlyOnce is §6.8's "two concurrent engine
// invocations resuming the same saga produce exactly one advance". The loser
// matches zero rows and gets ErrSagaStageMoved — not an error condition, but it
// must be distinguishable, or a resumer cannot tell "I lost" from "it broke".
func TestSagaStageCASAdvancesExactlyOnce(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	advance := func(from, to string) error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := AdvanceSagaTx(tx, id, from, to, now); err != nil {
			return err
		}
		return tx.Commit()
	}

	// Entering the saga: guarded on the NULL stage.
	err := advance("", SagaRecorded)
	testsupport.Must(t, err, "entering the saga: %v", err)
	// A second entry loses: the stage is no longer NULL.
	if err := advance("", SagaRecorded); !errors.Is(err, ErrSagaStageMoved) {
		t.Errorf("re-entry error = %v, want ErrSagaStageMoved", err)
	}
	// Advancing from the right stage wins.
	err = advance(SagaRecorded, SagaRouting)
	testsupport.Must(t, err, "advancing to routing: %v", err)
	// Advancing from a stale stage loses.
	if err := advance(SagaRecorded, SagaRouting); !errors.Is(err, ErrSagaStageMoved) {
		t.Errorf("stale advance error = %v, want ErrSagaStageMoved", err)
	}
	// Leaving the saga.
	err = advance(SagaRouting, "")
	testsupport.Must(t, err, "leaving the saga: %v", err)

	step, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.InSaga() {
		t.Errorf("saga_stage = %q after completion, want empty", step.SagaStage)
	}
}

// TestSagaStageRefreshesActivityClock is §6.8's "every stage commit refreshing
// the step's activity clock" — the property that lets a long saga survive a
// heartbeat-based liveness check it is not driving.
func TestSagaStageRefreshesActivityClock(t *testing.T) {
	db, id := stepTestDB(t)

	stages := []struct {
		from, to string
		at       int64
	}{
		{"", SagaRecorded, 2_000_000},
		{SagaRecorded, SagaRouting, 3_000_000},
		{SagaRouting, "", 4_000_000},
	}

	for _, s := range stages {
		tx, err := db.Begin()
		testsupport.Must(t, err, "Begin: %v", err)
		if err := AdvanceSagaTx(tx, id, s.from, s.to, s.at); err != nil {
			tx.Rollback()
			t.Fatalf("advance to %q: %v", s.to, err)
		}
		err = tx.Commit()
		testsupport.Must(t, err, "Commit: %v", err)

		step, err := GetStep(db, id)
		testsupport.Must(t, err, "GetStep: %v", err)
		if step.ActivityMS == nil || *step.ActivityMS != s.at {
			t.Errorf("after advancing to %q: activity_ms = %v, want %d",
				s.to, step.ActivityMS, s.at)
		}
	}
}

// TestRetireStepTokenIsTheSameClearAsRelease pins §6.8's hinge: retirement and
// release are one state change reached by two authorities, so they are one
// function. A step whose token retired is indistinguishable from an unclaimed
// one AS FAR AS THE LEASE GOES — which is exactly what makes the saga
// resumable by any later engine invocation with no owner to wait for.
func TestRetireStepTokenIsTheSameClearAsRelease(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	token, _, err := ClaimStep(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "ClaimStep: %v", err)

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	if err := RetireStepTokenTx(tx, id); err != nil {
		tx.Rollback()
		t.Fatalf("RetireStepTokenTx: %v", err)
	}
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	lease, err := GetStepLease(db, id)
	testsupport.Must(t, err, "GetStepLease: %v", err)
	if lease.Held() {
		t.Errorf("lease still held after retirement: owner = %q", lease.Owner)
	}
	// attempt survives retirement, as it survives release.
	if lease.Attempt != 1 {
		t.Errorf("attempt = %d after retirement, want 1 — the trail must survive", lease.Attempt)
	}

	// R9: the retired token no longer authorizes anything.
	tx2, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx2.Rollback()
	if _, err := AuthorizeStepTx(tx2, id, token, now); !errors.Is(err, ErrNotHolder) {
		t.Errorf("authorize with a retired token = %v, want ErrNotHolder (R9)", err)
	}
}

// TestReapStepDoesNotDoubleCountAttempts pins the reaping rule: the claim's own
// increment is the attempt trail, so reaping must not increment again.
//
// Getting this wrong is invisible in a passing run and shows up as an attempt
// count that is twice the number of tries — which would make `max_attempts`
// exhaust at half its declared budget.
func TestReapStepDoesNotDoubleCountAttempts(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	_, _, err := ClaimStep(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "ClaimStep: %v", err)

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	if err := ReapStepTx(tx, id, now+testTTLMS+1); err != nil {
		tx.Rollback()
		t.Fatalf("ReapStepTx: %v", err)
	}
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	step, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Attempt != 1 {
		t.Errorf("attempt = %d after one claim and one reap, want 1", step.Attempt)
	}
	if step.Status != StepPending {
		t.Errorf("status = %q after reaping, want %q", step.Status, StepPending)
	}
	if step.Owner != "" {
		t.Errorf("owner = %q after reaping, want empty", step.Owner)
	}
	if step.StartedMS != nil {
		t.Errorf("started_ms = %v after reaping, want NULL — "+
			"a re-claim gets a fresh schedule-to-close budget", step.StartedMS)
	}

	// And the re-claim increments, so the trail records both tries.
	_, lease, err := ClaimStep(db, id, "worker-2", testTTLMS, now+testTTLMS+2)
	testsupport.Must(t, err, "re-claim: %v", err)
	if lease.Attempt != 2 {
		t.Errorf("attempt = %d after the second claim, want 2", lease.Attempt)
	}
}

// TestReadyIsNeverPersisted pins §6.2: `ready` is computed at read time and is
// not a member of the persisted enum. The nine stored statuses are the machine's
// ten minus this one.
func TestReadyIsNeverPersisted(t *testing.T) {
	persisted := []string{
		StepPending, StepClaimed, StepRunning, StepGated, StepDone,
		StepWaitingHuman, StepSkipped, StepSuperseded, StepFailedRouted,
	}
	if len(persisted) != 9 {
		t.Fatalf("persisted statuses = %d, want 9 (TDD §6.2)", len(persisted))
	}
	for _, s := range persisted {
		if s == StepReady {
			t.Fatalf("%q is in the persisted set, but §6.2 makes it computed-only", StepReady)
		}
	}
}
