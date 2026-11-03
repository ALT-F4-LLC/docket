package db

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

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

// TestClaimStepMintsTokenAndTakesLease is the step-level analog of the
// TestStoreContract ClaimMintsTokenAndTakesLease case, proving the generalized
// helper behaves identically against `steps`.
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

// TestClaimStepAgainstLiveLeaseConflicts is the live-lease refusal at the db
// layer.
func TestClaimStepAgainstLiveLeaseConflicts(t *testing.T) {
	db, id := stepTestDB(t)
	now := int64(1_000_000)

	_, _, err := ClaimStep(db, id, "worker-1", testTTLMS, now)
	testsupport.Must(t, err, "first ClaimStep: %v", err)
	if _, _, err := ClaimStep(db, id, "worker-2", testTTLMS, now+1); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("second ClaimStep error = %v, want ErrLeaseHeld", err)
	}
}

// TestClaimStepAgainstExpiredLeaseSucceeds: expiry alone re-readies a step,
// with no operator action and no reaper — the liveness mechanism.
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

// TestStepClaimPredicateIsLoadBearing is the "mutation test repeated for
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

// TestLeaseHelpersAreShared is "lease helpers shared with issues proven
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
	// generalization the lease contract requires, rather than a copy.
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
					"has been duplicated rather than generalized",
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

// TestStepRefusalMatrix is the step refusal matrix at the db layer: the same
// three sentinels the issue matrix produces, from the same authorize function.
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

// TestGetStepLeaseNeverWrites pins "reads never write" for the
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

// TestSagaStageCASAdvancesExactlyOnce is "two concurrent engine
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

// TestSagaStageRefreshesActivityClock is "every stage commit refreshing
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

// TestRetireStepTokenIsTheSameClearAsRelease pins the saga's hinge:
// retirement and release are one state change reached by two authorities, so
// they are one function. A step whose token retired is indistinguishable from
// an unclaimed one AS FAR AS THE LEASE GOES — which is exactly what makes the
// saga resumable by any later engine invocation with no owner to wait for.
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

	// The retired token no longer authorizes anything.
	tx2, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx2.Rollback()
	if _, err := AuthorizeStepTx(tx2, id, token, now); !errors.Is(err, ErrNotHolder) {
		t.Errorf("authorize with a retired token = %v, want ErrNotHolder", err)
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

// TestReadyIsNeverPersisted pins that `ready` is computed at read time and is
// not a member of the persisted enum. The nine stored statuses are the machine's
// ten minus this one.
func TestReadyIsNeverPersisted(t *testing.T) {
	persisted := []string{
		StepPending, StepClaimed, StepRunning, StepGated, StepDone,
		StepWaitingHuman, StepSkipped, StepSuperseded, StepFailedRouted,
	}
	if len(persisted) != 9 {
		t.Fatalf("persisted statuses = %d, want 9", len(persisted))
	}
	for _, s := range persisted {
		if s == StepReady {
			t.Fatalf("%q is in the persisted set, but it is computed-only", StepReady)
		}
	}
}

// TestLoopHistoryFactsRoundTrip proves the three loop-history facts a
// fix-loop-exhausted step parks with — rounds run against the cap, the step
// that triggered the loop, and the latest fix-round verdict — survive one
// SetStepLoopHistoryTx write and come back on the row's model.StepRow.
//
// It writes through the setter directly rather than through the exhaustion
// routing transaction: the facts are storage here, and the call site inside
// that transaction is a separate criterion.
//
// It also pins the row move the setter promises CAS-guarded readers: one
// row_version bump and updated_at_ms set to the caller's nowMS.
func TestLoopHistoryFactsRoundTrip(t *testing.T) {
	db, id := stepTestDB(t)

	before, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep before write: %v", err)

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = SetStepLoopHistoryTx(tx, id, 3, "judge-correctness@0", "concerns", 2000)
	testsupport.Must(t, err, "SetStepLoopHistoryTx: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	step, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep: %v", err)

	row := model.StepRow{
		LoopRoundsRun:     step.LoopRoundsRun,
		LoopTriggerStep:   step.LoopTriggerStep,
		LoopLatestVerdict: step.LoopLatestVerdict,
	}
	if row.LoopRoundsRun != 3 {
		t.Errorf("LoopRoundsRun = %d, want 3", row.LoopRoundsRun)
	}
	if row.LoopTriggerStep != "judge-correctness@0" {
		t.Errorf("LoopTriggerStep = %q, want judge-correctness@0", row.LoopTriggerStep)
	}
	if row.LoopLatestVerdict != "concerns" {
		t.Errorf("LoopLatestVerdict = %q, want concerns", row.LoopLatestVerdict)
	}

	if step.RowVersion != before.RowVersion+1 {
		t.Errorf("RowVersion = %d, want %d — CAS-guarded readers must see the row move",
			step.RowVersion, before.RowVersion+1)
	}
	if step.UpdatedAtMS != 2000 {
		t.Errorf("UpdatedAtMS = %d, want 2000 (the setter's nowMS)", step.UpdatedAtMS)
	}
}

// TestLoopHistoryStoresAppliedBound proves the bound-taking setter stores the
// applied fix-loop bound in steps.loop_cap, and the bound-less setter stores 0
// over it. The bound differs from roundsRun so a write of the wrong value
// cannot pass.
func TestLoopHistoryStoresAppliedBound(t *testing.T) {
	db, id := stepTestDB(t)
	readLoopCap := func() int {
		t.Helper()
		var loopCap int
		err := db.QueryRow(`SELECT loop_cap FROM steps WHERE id = ?`, id).Scan(&loopCap)
		testsupport.Must(t, err, "reading loop_cap: %v", err)
		return loopCap
	}
	write := func(name string, fn func(tx *sql.Tx) error) {
		t.Helper()
		tx, err := db.Begin()
		testsupport.Must(t, err, "Begin: %v", err)
		defer tx.Rollback()
		err = fn(tx)
		testsupport.Must(t, err, "%s: %v", name, err)
		err = tx.Commit()
		testsupport.Must(t, err, "Commit: %v", err)
	}

	write("SetStepLoopHistoryWithCapTx", func(tx *sql.Tx) error {
		return SetStepLoopHistoryWithCapTx(tx, id, 3, 5, "judge-correctness@0", "concerns", 2000)
	})
	if got := readLoopCap(); got != 5 {
		t.Errorf("loop_cap = %d after the bound-taking write, want the applied bound 5", got)
	}
	step, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.LoopRoundsRun != 3 {
		t.Errorf("LoopRoundsRun = %d, want 3", step.LoopRoundsRun)
	}
	if step.LoopCap != 5 {
		t.Errorf("GetStep LoopCap = %d, want the applied bound 5", step.LoopCap)
	}

	write("SetStepLoopHistoryTx", func(tx *sql.Tx) error {
		return SetStepLoopHistoryTx(tx, id, 3, "judge-correctness@0", "concerns", 3000)
	})
	if got := readLoopCap(); got != 0 {
		t.Errorf("loop_cap = %d after the bound-less write, want 0 (no recorded cap)", got)
	}
	step, err = GetStep(db, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.LoopCap != 0 {
		t.Errorf("GetStep LoopCap = %d after the bound-less write, want 0", step.LoopCap)
	}
}

// TestStepsAuthorityColumnRoundTrips proves the authority a resolution was
// made under, and the standing grant it names, survive one SetStepAuthorityTx
// write and come back on the row.
func TestStepsAuthorityColumnRoundTrips(t *testing.T) {
	db, id := stepTestDB(t)

	before, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep before write: %v", err)
	if before.Authority != "" || before.AuthorityRef != "" {
		t.Fatalf("an unresolved step carries authority %q/%q, want none",
			before.Authority, before.AuthorityRef)
	}

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = SetStepAuthorityTx(tx, id, "standing-grant", "RUN NOTE 54", 2000)
	testsupport.Must(t, err, "SetStepAuthorityTx: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	step, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Authority != "standing-grant" {
		t.Errorf("Authority = %q, want standing-grant", step.Authority)
	}
	if step.AuthorityRef != "RUN NOTE 54" {
		t.Errorf("AuthorityRef = %q, want %q — a standing grant must name the "+
			"authorization it applies", step.AuthorityRef, "RUN NOTE 54")
	}
	if step.RowVersion != before.RowVersion+1 {
		t.Errorf("RowVersion = %d, want %d — CAS-guarded readers must see the row move",
			step.RowVersion, before.RowVersion+1)
	}
}

// TestStepsAuthorityOverwriteClearsRef proves a later ruling replaces the
// whole authority record: an operator ruling after a standing grant leaves no
// stale grant reference on the row.
func TestStepsAuthorityOverwriteClearsRef(t *testing.T) {
	db, id := stepTestDB(t)

	before, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep before write: %v", err)

	writes := []struct{ authority, ref string }{
		{"standing-grant", "RUN NOTE 54"},
		{"operator", ""},
	}
	for i, w := range writes {
		tx, err := db.Begin()
		testsupport.Must(t, err, "Begin: %v", err)
		err = SetStepAuthorityTx(tx, id, w.authority, w.ref, int64(2000+i))
		testsupport.Must(t, err, "SetStepAuthorityTx(%q, %q): %v", w.authority, w.ref, err)
		err = tx.Commit()
		testsupport.Must(t, err, "Commit: %v", err)
	}

	step, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Authority != "operator" {
		t.Errorf("Authority = %q, want operator", step.Authority)
	}
	if step.AuthorityRef != "" {
		t.Errorf("AuthorityRef = %q, want empty — a stale standing-grant "+
			"reference survived the operator ruling", step.AuthorityRef)
	}
	if step.RowVersion != before.RowVersion+2 {
		t.Errorf("RowVersion = %d, want %d — each authority write must move the row",
			step.RowVersion, before.RowVersion+2)
	}
}

// TestLoopHistoryFactsAbsentBeforeWrite pins the unwritten reading: a step that
// never exhausted a loop carries no loop history, so the fields stay at their
// zero values and serialize away under omitempty.
func TestLoopHistoryFactsAbsentBeforeWrite(t *testing.T) {
	db, id := stepTestDB(t)

	step, err := GetStep(db, id)
	testsupport.Must(t, err, "GetStep: %v", err)

	row := model.StepRow{
		LoopRoundsRun:     step.LoopRoundsRun,
		LoopTriggerStep:   step.LoopTriggerStep,
		LoopLatestVerdict: step.LoopLatestVerdict,
	}
	if row.LoopRoundsRun != 0 || row.LoopTriggerStep != "" || row.LoopLatestVerdict != "" {
		t.Errorf("unwritten loop history = %d/%q/%q, want 0//",
			row.LoopRoundsRun, row.LoopTriggerStep, row.LoopLatestVerdict)
	}
}

// TestRecordedAtStampsOnlyTheMoveIntoTerminal pins `recorded_at_ms`'s writers:
// the move into a terminal status stamps it, and neither a non-terminal move,
// a metadata write, nor a rewrite of an already-terminal row moves it.
func TestRecordedAtStampsOnlyTheMoveIntoTerminal(t *testing.T) {
	db, id := stepTestDB(t)
	write := func(name string, fn func(tx *sql.Tx) error) *Step {
		t.Helper()
		tx, err := db.Begin()
		testsupport.Must(t, err, "Begin: %v", err)
		defer tx.Rollback()
		err = fn(tx)
		testsupport.Must(t, err, "%s: %v", name, err)
		err = tx.Commit()
		testsupport.Must(t, err, "Commit: %v", err)
		step, err := GetStep(db, id)
		testsupport.Must(t, err, "GetStep: %v", err)
		return step
	}

	step := write("running", func(tx *sql.Tx) error {
		return SetStepStatusTx(tx, id, StepRunning, 1500, 0)
	})
	if step.RecordedAtMS != 0 {
		t.Errorf("after a move to %s, RecordedAtMS = %d, want 0", StepRunning, step.RecordedAtMS)
	}

	step = write("routing to done", func(tx *sql.Tx) error {
		return SetStepRoutingTx(tx, id, "next", "", StepDone, "", 2000)
	})
	if step.RecordedAtMS != 2000 {
		t.Errorf("after the move to %s, RecordedAtMS = %d, want 2000", StepDone, step.RecordedAtMS)
	}

	step = write("metadata", func(tx *sql.Tx) error {
		return SetStepMetadataTx(tx, id, `{"k":"v"}`, 3000)
	})
	if step.RecordedAtMS != 2000 || step.UpdatedAtMS != 3000 {
		t.Errorf("after a metadata write, RecordedAtMS = %d and UpdatedAtMS = %d, "+
			"want 2000 and 3000", step.RecordedAtMS, step.UpdatedAtMS)
	}

	step = write("terminal rewrite", func(tx *sql.Tx) error {
		return SetStepStatusTx(tx, id, StepSuperseded, 4000, 0)
	})
	if step.RecordedAtMS != 2000 {
		t.Errorf("after a terminal-to-terminal rewrite, RecordedAtMS = %d, want 2000",
			step.RecordedAtMS)
	}
}

// routingFixture is a migrated database with two runs and two issues, and an
// open transaction the routing-query helpers read inside. `run` holds both
// issues; `otherRun` holds `issue` only. Every row a test seeds goes through
// the transaction, because the pool's one connection belongs to it.
type routingFixture struct {
	t                 *testing.T
	db                *sql.DB
	tx                *sql.Tx
	run, otherRun     int
	issue, otherIssue int
	workflow          int
}

func newRoutingFixture(t *testing.T) *routingFixture {
	t.Helper()
	db := mustOpen(t)
	testsupport.Must(t, Initialize(db), "Initialize")
	testsupport.Must(t, Migrate(db), "Migrate")
	f := &routingFixture{t: t, db: db}
	for _, id := range []*int{&f.issue, &f.otherIssue} {
		issueID, err := CreateIssue(db, &model.Issue{
			Title: "routing subject", Status: model.StatusTodo,
			Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, nil, nil)
		testsupport.Must(t, err, "CreateIssue: %v", err)
		*id = issueID
	}
	res, err := db.Exec(
		`INSERT INTO workflows (name, version, source_sha256, body, parsed, created_at_ms, row_version)
		 VALUES ('wf', 1, 'sha', 'body', '{}', 1000, 1)`)
	testsupport.Must(t, err, "seeding workflow: %v", err)
	wfID, _ := res.LastInsertId()
	f.workflow = int(wfID)
	for _, id := range []*int{&f.run, &f.otherRun} {
		run, err := InsertRun(db, 1, "req", 0, 1000)
		testsupport.Must(t, err, "InsertRun: %v", err)
		*id = run.ID
	}
	testsupport.Must(t, AddRunIssue(db, f.run, f.issue), "AddRunIssue")
	testsupport.Must(t, AddRunIssue(db, f.run, f.otherIssue), "AddRunIssue")
	testsupport.Must(t, AddRunIssue(db, f.otherRun, f.issue), "AddRunIssue")
	f.tx, err = db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	t.Cleanup(func() { _ = f.tx.Rollback() })
	return f
}

// add inserts one step and returns its id. A zero RunID or IssueID takes the
// fixture's `run` and `issue`; an empty Instance is `<name>@<ordinal>`.
func (f *routingFixture) add(row StepRow) int {
	f.t.Helper()
	if row.RunID == 0 {
		row.RunID = f.run
	}
	if row.IssueID == 0 {
		row.IssueID = f.issue
	}
	if row.Instance == "" {
		row.Instance = fmt.Sprintf("%s@%d", row.StepName, row.Ordinal)
	}
	if row.Kind == "" {
		row.Kind = "executor"
	}
	row.WorkflowID = f.workflow
	testsupport.Must(f.t, InsertStepTx(f.tx, row, 1000), "InsertStepTx %s", row.Instance)
	var id int
	testsupport.Must(f.t, f.tx.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND issue_id = ? AND instance = ?`,
		row.RunID, row.IssueID, row.Instance).Scan(&id), "reading %s's id", row.Instance)
	return id
}

func (f *routingFixture) exec(query string, args ...any) {
	f.t.Helper()
	_, err := f.tx.Exec(query, args...)
	testsupport.Must(f.t, err, "%s: %v", query, err)
}

func (f *routingFixture) artifact(stepID int, kind, sha, body, payload string) {
	f.t.Helper()
	var runID int
	testsupport.Must(f.t, f.tx.QueryRow(`SELECT run_id FROM steps WHERE id = ?`, stepID).Scan(&runID),
		"reading step %d's run", stepID)
	_, err := InsertArtifactTx(f.tx, Artifact{
		RunID: runID, StepID: stepID, Kind: kind, Body: body, Payload: payload, SHA256: sha,
	}, 1000)
	testsupport.Must(f.t, err, "InsertArtifactTx: %v", err)
}

func (f *routingFixture) event(kind string, stepID int, data string) int64 {
	f.t.Helper()
	testsupport.Must(f.t, InsertEventTx(f.tx, NewEvent{
		AtMS: 1, Kind: kind, RunID: f.run, StepID: stepID, Data: data,
	}), "seeding a %s event", kind)
	var seq int64
	testsupport.Must(f.t, f.tx.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&seq), "reading the seq")
	return seq
}

func TestRoutingQueriesStepInstancesAtOrdinalTx(t *testing.T) {
	f := newRoutingFixture(t)
	zero, one := 0, 1
	passed := f.add(StepRow{StepName: "review", Ordinal: 1, SiblingIndex: &zero,
		Instance: "review@1#0", Status: StepDone})
	f.exec(`UPDATE steps SET routing = 'pass' WHERE id = ?`, passed)
	f.add(StepRow{StepName: "review", Ordinal: 1, SiblingIndex: &one,
		Instance: "review@1#1", Status: StepFailedRouted})
	f.add(StepRow{StepName: "review", Ordinal: 0, Status: StepDone})
	f.add(StepRow{StepName: "verify", Ordinal: 1, Status: StepDone})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "review", Ordinal: 1, Status: StepPending})
	f.add(StepRow{RunID: f.otherRun, StepName: "review", Ordinal: 1, Status: StepPending})

	got, err := StepInstancesAtOrdinalTx(f.tx, f.run, f.issue, "review", 1)
	testsupport.Must(t, err, "StepInstancesAtOrdinalTx: %v", err)
	want := []StepStatusRouting{{Status: StepDone, Routing: "pass"}, {Status: StepFailedRouted}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("review@1's instances = %+v, want %+v (a NULL routing reads \"\")", got, want)
	}

	got, err = StepInstancesAtOrdinalTx(f.tx, f.run, f.issue, "absent", 1)
	if err != nil || len(got) != 0 {
		t.Errorf("a step with no instance = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestRoutingQueriesPendingInstancesUpToTx(t *testing.T) {
	f := newRoutingFixture(t)
	g0 := f.add(StepRow{StepName: "gate", Ordinal: 0, Status: StepPending})
	g1 := f.add(StepRow{StepName: "gate", Ordinal: 1, Status: StepPending})
	f.add(StepRow{StepName: "gate", Ordinal: 2, Status: StepPending})
	f.add(StepRow{StepName: "gate", Ordinal: 1, Instance: "gate@1#1", Status: StepDone})
	f.add(StepRow{StepName: "other", Ordinal: 0, Status: StepPending})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "gate", Ordinal: 0, Status: StepPending})

	got, err := PendingInstancesUpToTx(f.tx, f.run, f.issue, "gate", 1)
	testsupport.Must(t, err, "PendingInstancesUpToTx: %v", err)
	want := []StepInstanceRef{
		{ID: g0, Instance: "gate@0", Status: StepPending},
		{ID: g1, Instance: "gate@1", Status: StepPending},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pending gates up to ordinal 1 = %+v, want %+v", got, want)
	}

	got, err = PendingInstancesUpToTx(f.tx, f.run, f.issue, "absent", 5)
	if err != nil || len(got) != 0 {
		t.Errorf("a target with no instance = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestRoutingQueriesIssueStatusTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.exec(`UPDATE issues SET status = ? WHERE id = ?`, model.StatusReview, f.otherIssue)

	got, err := IssueStatusTx(f.tx, f.otherIssue)
	if err != nil || got != model.StatusReview {
		t.Errorf("IssueStatusTx = (%q, %v), want (%q, nil)", got, err, model.StatusReview)
	}
	got, err = IssueStatusTx(f.tx, f.issue)
	if err != nil || got != model.StatusTodo {
		t.Errorf("IssueStatusTx = (%q, %v), want (%q, nil)", got, err, model.StatusTodo)
	}
	if _, err := IssueStatusTx(f.tx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing issue's error = %v, want ErrNotFound", err)
	}
}

func TestRoutingQueriesIssueHasParkedOrHeldStepTx(t *testing.T) {
	f := newRoutingFixture(t)
	ask := func(name string, issueID int, want bool) {
		t.Helper()
		got, err := IssueHasParkedOrHeldStepTx(f.tx, f.run, issueID)
		testsupport.Must(t, err, "%s: %v", name, err)
		if got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	ask("an issue with no steps", f.issue, false)

	f.add(StepRow{StepName: "implement", Status: StepPending})
	f.add(StepRow{StepName: "review-held", Status: StepDone, Materialized: true})
	f.add(StepRow{RunID: f.otherRun, StepName: "verify", Status: StepWaitingHuman})
	ask("only unparked and terminal held steps", f.issue, false)

	f.add(StepRow{IssueID: f.otherIssue, StepName: "verify", Status: StepWaitingHuman})
	ask("a waiting-human step", f.otherIssue, true)
	ask("another issue's park", f.issue, false)

	f.add(StepRow{StepName: "aggregate-held", Status: StepPending, Materialized: true})
	ask("an open materialized hold", f.issue, true)
}

func TestRoutingQueriesPendingDeclaredStepCountTx(t *testing.T) {
	f := newRoutingFixture(t)
	kinds := []string{"human", "vote"}
	f.add(StepRow{StepName: "approve", Kind: "human", Status: StepPending})
	f.add(StepRow{StepName: "ballot", Kind: "vote", Status: StepPending})
	f.add(StepRow{StepName: "approve", Ordinal: 1, Kind: "human", Status: StepDone})
	f.add(StepRow{StepName: "approve-held", Kind: "human", Status: StepPending, Materialized: true})
	f.add(StepRow{StepName: "implement", Kind: "executor", Status: StepPending})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "approve", Kind: "human", Status: StepPending})

	got, err := PendingDeclaredStepCountTx(f.tx, f.run, f.issue, kinds)
	if err != nil || got != 2 {
		t.Errorf("pending declared gates = (%d, %v), want (2, nil)", got, err)
	}
	got, err = PendingDeclaredStepCountTx(f.tx, f.run, f.issue, nil)
	if err != nil || got != 0 {
		t.Errorf("no kinds = (%d, %v), want (0, nil)", got, err)
	}
	got, err = PendingDeclaredStepCountTx(f.tx, f.otherRun, f.issue, kinds)
	if err != nil || got != 0 {
		t.Errorf("a run with no steps = (%d, %v), want (0, nil)", got, err)
	}
}

func TestRoutingQueriesIssueLatestInstancesTerminalTx(t *testing.T) {
	f := newRoutingFixture(t)
	ask := func(name string, issueID int, want bool) {
		t.Helper()
		got, err := IssueLatestInstancesTerminalTx(f.tx, f.run, issueID)
		testsupport.Must(t, err, "%s: %v", name, err)
		if got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	f.add(StepRow{StepName: "implement", Status: StepDone})
	f.add(StepRow{StepName: "review", Ordinal: 0, Status: StepPending})
	f.add(StepRow{StepName: "review", Ordinal: 1, Status: StepSuperseded})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Status: StepPending})
	f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepPending})
	ask("every latest instance terminal, a lower ordinal pending", f.issue, true)

	f.add(StepRow{StepName: "verify", Ordinal: 1, Status: StepWaitingHuman})
	ask("a waiting-human latest instance", f.issue, false)
	ask("another issue's pending step", f.otherIssue, false)
	ask("an issue with no steps", 999, true)
}

func TestRoutingQueriesCompleteIssueTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.exec(`UPDATE issues SET status = ?, resolution = ? WHERE id = ?`,
		model.StatusInProgress, model.ResolutionAbandoned, f.issue)
	var version int
	testsupport.Must(t, f.tx.QueryRow(`SELECT version FROM issues WHERE id = ?`, f.issue).Scan(&version),
		"reading the version")
	nowMS := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC).UnixMilli()

	moved, err := CompleteIssueTx(f.tx, f.issue, model.StatusInProgress, "", nowMS)
	if err != nil || !moved {
		t.Fatalf("CompleteIssueTx = (%v, %v), want (true, nil)", moved, err)
	}
	var status, resolution, updated string
	var gotVersion int
	testsupport.Must(t, f.tx.QueryRow(
		`SELECT status, resolution, updated_at, version FROM issues WHERE id = ?`, f.issue).
		Scan(&status, &resolution, &updated, &gotVersion), "reading the issue")
	if status != string(model.StatusDone) || resolution != "" ||
		updated != "2026-03-04T05:06:07Z" || gotVersion != version+1 {
		t.Errorf("issue = (%q, %q, %q, v%d), want (done, \"\", 2026-03-04T05:06:07Z, v%d)",
			status, resolution, updated, gotVersion, version+1)
	}
	activity := func() []string {
		t.Helper()
		rows, err := f.tx.Query(`SELECT issue_id || ' ' || field_changed || ' ' || old_value || ' ' ||
			new_value || ' [' || changed_by || '] ' || created_at FROM activity_log
			WHERE field_changed = 'status' ORDER BY id`)
		testsupport.Must(t, err, "reading activity: %v", err)
		out, err := scanRows(rows, "activity", func(r *sql.Rows) (string, error) {
			var s string
			return s, r.Scan(&s)
		})
		testsupport.Must(t, err, "reading activity: %v", err)
		return out
	}
	want := []string{fmt.Sprintf("%d status in-progress done [] 2026-03-04T05:06:07Z", f.issue)}
	if got := activity(); !reflect.DeepEqual(got, want) {
		t.Errorf("activity = %q, want %q", got, want)
	}

	moved, err = CompleteIssueTx(f.tx, f.issue, model.StatusDone, "", nowMS+1000)
	if err != nil || moved {
		t.Errorf("an issue already done = (%v, %v), want (false, nil)", moved, err)
	}
	moved, err = CompleteIssueTx(f.tx, 999, model.StatusTodo, "", nowMS)
	if err != nil || moved {
		t.Errorf("a missing issue = (%v, %v), want (false, nil)", moved, err)
	}
	if got := activity(); !reflect.DeepEqual(got, want) {
		t.Errorf("activity after two no-op completions = %q, want %q", got, want)
	}
}

func TestRoutingQueriesCascadeFailIssueStepsTx(t *testing.T) {
	f := newRoutingFixture(t)
	router := f.add(StepRow{StepName: "verify", Status: StepRunning})
	open := []int{
		f.add(StepRow{StepName: "commit", Status: StepPending}),
		f.add(StepRow{StepName: "review", Status: StepGated}),
		f.add(StepRow{StepName: "approve", Status: StepWaitingHuman}),
	}
	finished := f.add(StepRow{StepName: "implement", Status: StepDone})
	skipped := f.add(StepRow{StepName: "lint", Status: StepSkipped})
	other := f.add(StepRow{IssueID: f.otherIssue, StepName: "commit", Status: StepPending})
	elsewhere := f.add(StepRow{RunID: f.otherRun, StepName: "commit", Status: StepPending})

	n, err := CascadeFailIssueStepsTx(f.tx, f.run, f.issue, router, "abandon-issue: cascade", 5000)
	if err != nil || n != len(open) {
		t.Fatalf("CascadeFailIssueStepsTx = (%d, %v), want (%d, nil)", n, err, len(open))
	}
	read := func(id int) (status, routing string, recorded int64) {
		t.Helper()
		var r sql.NullString
		testsupport.Must(t, f.tx.QueryRow(
			`SELECT status, routing, recorded_at_ms FROM steps WHERE id = ?`, id).
			Scan(&status, &r, &recorded), "reading step %d", id)
		return status, r.String, recorded
	}
	for _, id := range open {
		if s, r, at := read(id); s != StepFailedRouted || r != "abandon-issue: cascade" || at != 5000 {
			t.Errorf("open step %d = (%q, %q, %d), want (failed-routed, the cascade, 5000)", id, s, r, at)
		}
	}
	for id, want := range map[int]string{
		router: StepRunning, finished: StepDone, skipped: StepSkipped,
		other: StepPending, elsewhere: StepPending,
	} {
		if s, r, _ := read(id); s != want || r != "" {
			t.Errorf("step %d = (%q, %q), want (%q, \"\"): not the cascade's", id, s, r, want)
		}
	}

	n, err = CascadeFailIssueStepsTx(f.tx, f.run, 999, 0, "x", 6000)
	if err != nil || n != 0 {
		t.Errorf("an issue with no steps = (%d, %v), want (0, nil)", n, err)
	}
}

func TestRoutingQueriesRunStepRollupTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.add(StepRow{StepName: "verify", Status: StepWaitingHuman})
	f.add(StepRow{StepName: "commit", Status: StepPending})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Status: StepRunning})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "lint", Status: StepDone})
	f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepPending})

	got, err := RunStepRollupTx(f.tx, f.run)
	testsupport.Must(t, err, "RunStepRollupTx: %v", err)
	want := RunStepRollup{Parked: 1, Unfinished: 3, UnparkedWork: 1}
	if got != want {
		t.Errorf("rollup = %+v, want %+v", got, want)
	}

	got, err = RunStepRollupTx(f.tx, 999)
	if err != nil || got != (RunStepRollup{}) {
		t.Errorf("a run with no steps = (%+v, %v), want (zero, nil)", got, err)
	}
}

func TestRoutingQueriesUnexpandedRunIssueCountTx(t *testing.T) {
	f := newRoutingFixture(t)
	ask := func(name string, runID, want int) {
		t.Helper()
		got, err := UnexpandedRunIssueCountTx(f.tx, runID)
		if err != nil || got != want {
			t.Errorf("%s = (%d, %v), want (%d, nil)", name, got, err, want)
		}
	}
	ask("two unexpanded issues", f.run, 2)
	testsupport.Must(t, MarkExpandedTx(f.tx, f.run, f.issue, 2000), "MarkExpandedTx")
	ask("one issue expanded", f.run, 1)
	ask("the other run", f.otherRun, 1)
	ask("a missing run", 999, 0)
}

func TestRoutingQueriesRunStatusTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.exec(`UPDATE runs SET status = ? WHERE id = ?`, model.RunActive, f.run)
	f.exec(`UPDATE runs SET status = ? WHERE id = ?`, model.RunWaitingHuman, f.otherRun)

	got, err := RunStatusTx(f.tx, f.run)
	if err != nil || got != model.RunActive {
		t.Errorf("RunStatusTx = (%q, %v), want (%q, nil)", got, err, model.RunActive)
	}
	if _, err := RunStatusTx(f.tx, 999); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("a missing run's error = %v, want ErrRunNotFound", err)
	}
}

func TestRoutingQueriesTransitionRunStatusTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.exec(`UPDATE runs SET status = ? WHERE id = ?`, model.RunActive, f.run)
	read := func(id int) (status model.RunStatus, updated int64, version int) {
		t.Helper()
		testsupport.Must(t, f.tx.QueryRow(
			`SELECT status, updated_at_ms, row_version FROM runs WHERE id = ?`, id).
			Scan(&status, &updated, &version), "reading run %d", id)
		return status, updated, version
	}
	_, _, version := read(f.run)
	_, otherUpdated, otherVersion := read(f.otherRun)

	from, moved, err := TransitionRunStatusTx(f.tx, f.run, model.RunWaitingHuman, 7000)
	if err != nil || !moved || from != model.RunActive {
		t.Fatalf("active -> waiting-human = (%q, %v, %v), want (active, true, nil)", from, moved, err)
	}
	if s, at, v := read(f.run); s != model.RunWaitingHuman || at != 7000 || v != version+1 {
		t.Errorf("run = (%q, %d, v%d), want (waiting-human, 7000, v%d)", s, at, v, version+1)
	}
	if _, at, v := read(f.otherRun); at != otherUpdated || v != otherVersion {
		t.Errorf("the other run moved: (%d, v%d), want (%d, v%d)", at, v, otherUpdated, otherVersion)
	}

	from, moved, err = TransitionRunStatusTx(f.tx, f.run, model.RunWaitingHuman, 8000)
	if err != nil || moved || from != model.RunWaitingHuman {
		t.Errorf("an unchanged status = (%q, %v, %v), want (waiting-human, false, nil)", from, moved, err)
	}
	if _, at, _ := read(f.run); at != 7000 {
		t.Errorf("an unchanged status wrote updated_at_ms %d, want 7000", at)
	}

	f.exec(`UPDATE runs SET status = ? WHERE id = ?`, model.RunDone, f.run)
	from, moved, err = TransitionRunStatusTx(f.tx, f.run, model.RunActive, 9000)
	if err != nil || moved || from != model.RunDone {
		t.Errorf("a terminal run = (%q, %v, %v), want (done, false, nil)", from, moved, err)
	}
	if s, _, _ := read(f.run); s != model.RunDone {
		t.Errorf("a terminal run was revived to %q", s)
	}

	if _, _, err := TransitionRunStatusTx(f.tx, 999, model.RunActive, 1); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("a missing run's error = %v, want ErrRunNotFound", err)
	}
}

func TestRoutingQueriesNewestStepPayloadTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "verify", Status: StepDone})
	other := f.add(StepRow{StepName: "review", Status: StepDone})
	bare := f.add(StepRow{StepName: "lint", Status: StepDone})
	f.artifact(step, "ac-report", "s1", "b", `[{"verdict":"old"}]`)
	f.artifact(step, "ac-report", "s2", "b", `[{"verdict":"new"}]`)
	f.artifact(step, "notes", "s3", "b", `[{"verdict":"other kind"}]`)
	f.artifact(other, "ac-report", "s4", "b", `[{"verdict":"other step"}]`)
	f.artifact(bare, "ac-report", "s5", "b", "")

	ask := func(name string, stepID int, kind, want string, wantFound bool) {
		t.Helper()
		got, found, err := NewestStepPayloadTx(f.tx, stepID, kind)
		testsupport.Must(t, err, "%s: %v", name, err)
		if got != want || found != wantFound {
			t.Errorf("%s = (%q, %v), want (%q, %v)", name, got, found, want, wantFound)
		}
	}
	ask("the newest ac-report", step, "ac-report", `[{"verdict":"new"}]`, true)
	ask("a NULL payload", bare, "ac-report", "", true)
	ask("a kind never recorded", step, "findings", "", false)
}

func TestRoutingQueriesNewestIssueArtifactUpToTx(t *testing.T) {
	f := newRoutingFixture(t)
	const diff = "issue.diff"
	f.artifact(f.add(StepRow{StepName: "implement", Ordinal: 0, Status: StepDone}), diff, "a", "body-a", "")
	fix1 := f.add(StepRow{StepName: "fix", Ordinal: 1, Status: StepDone})
	f.artifact(fix1, diff, "b", "body-b", "")
	f.artifact(f.add(StepRow{StepName: "fix", Ordinal: 2, Status: StepDone}), diff, "c", "body-c", "")
	f.artifact(fix1, "change-summary", "not-a-diff", "x", "")
	f.artifact(f.add(StepRow{IssueID: f.otherIssue, StepName: "fix", Ordinal: 1, Status: StepDone}),
		diff, "other-issue", "x", "")

	ask := func(name string, issueID, ordinal int, wantHash, wantBody string, wantFound bool) {
		t.Helper()
		hash, body, found, err := NewestIssueArtifactUpToTx(f.tx, f.run, issueID, diff, ordinal)
		testsupport.Must(t, err, "%s: %v", name, err)
		if hash != wantHash || body != wantBody || found != wantFound {
			t.Errorf("%s = (%q, %q, %v), want (%q, %q, %v)",
				name, hash, body, found, wantHash, wantBody, wantFound)
		}
	}
	ask("up to ordinal 0", f.issue, 0, "a", "body-a", true)
	ask("up to ordinal 1", f.issue, 1, "b", "body-b", true)
	ask("past the last round", f.issue, 9, "c", "body-c", true)
	ask("an issue with no diff that far back", f.otherIssue, 0, "", "", false)
}

func TestRoutingQueriesNewestStepNameArtifactTx(t *testing.T) {
	f := newRoutingFixture(t)
	v0 := f.add(StepRow{StepName: "verify", Ordinal: 0, Status: StepDone})
	v1 := f.add(StepRow{StepName: "verify", Ordinal: 1, Status: StepDone})
	f.artifact(v0, "ac-report", "v0", "body-0", `[0]`)
	f.artifact(v1, "ac-report", "v1", "body-1", "")
	f.artifact(v1, "ac-report", "v1b", "body-1b", `[1]`)
	f.artifact(f.add(StepRow{StepName: "fix", Ordinal: 1, Status: StepDone}), "ac-report", "fix", "x", "")
	f.artifact(v1, "findings", "kind", "x", "")
	f.artifact(f.add(StepRow{IssueID: f.otherIssue, StepName: "verify", Ordinal: 1, Status: StepDone}),
		"ac-report", "issue", "x", "")

	q := StepArtifactRange{RunID: f.run, IssueID: f.issue, StepName: "verify", Kind: "ac-report"}
	ask := func(name string, lo, hi int, want StepArtifact, wantFound bool) {
		t.Helper()
		r := q
		r.MinOrdinal, r.MaxOrdinal = lo, hi
		got, found, err := NewestStepNameArtifactTx(f.tx, r)
		testsupport.Must(t, err, "%s: %v", name, err)
		if got != want || found != wantFound {
			t.Errorf("%s = (%+v, %v), want (%+v, %v)", name, got, found, want, wantFound)
		}
	}
	ask("at ordinal 1", 1, 1, StepArtifact{SHA256: "v1b", Body: "body-1b", Payload: `[1]`, Ordinal: 1}, true)
	ask("at ordinal 0", 0, 0, StepArtifact{SHA256: "v0", Body: "body-0", Payload: `[0]`, Ordinal: 0}, true)
	ask("across every round", 0, 5, StepArtifact{SHA256: "v1b", Body: "body-1b", Payload: `[1]`, Ordinal: 1}, true)
	ask("a range with no record", 2, 3, StepArtifact{}, false)
}

func TestRoutingQueriesStepArtifactPayloadsTx(t *testing.T) {
	f := newRoutingFixture(t)
	v0 := f.add(StepRow{StepName: "verify", Ordinal: 0, Status: StepDone})
	v1 := f.add(StepRow{StepName: "verify", Ordinal: 1, Status: StepDone})
	f.artifact(v0, "findings", "a", "b", `[0]`)
	f.artifact(v1, "findings", "b", "b", `[1, 1]`)
	f.artifact(v1, "findings", "c", "b", "")
	f.artifact(f.add(StepRow{StepName: "verify", Ordinal: 2, Status: StepDone}), "findings", "d", "b", `[2]`)
	f.artifact(f.add(StepRow{StepName: "fix", Ordinal: 1, Status: StepDone}), "findings", "e", "b", `[9]`)
	f.artifact(v1, "ac-report", "f", "b", `[9]`)

	got, err := StepArtifactPayloadsTx(f.tx, StepArtifactRange{
		RunID: f.run, IssueID: f.issue, StepName: "verify", Kind: "findings", MaxOrdinal: 1,
	})
	testsupport.Must(t, err, "StepArtifactPayloadsTx: %v", err)
	want := []OrdinalPayload{{Ordinal: 0, Payload: `[0]`}, {Ordinal: 1, Payload: `[1, 1]`}, {Ordinal: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payloads = %+v, want %+v in artifact order", got, want)
	}

	got, err = StepArtifactPayloadsTx(f.tx, StepArtifactRange{
		RunID: f.run, IssueID: f.otherIssue, StepName: "verify", Kind: "findings", MaxOrdinal: 9,
	})
	if err != nil || len(got) != 0 {
		t.Errorf("an issue with no payloads = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestRoutingQueriesStepRoundCountTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.add(StepRow{StepName: "fix", Ordinal: 1, Status: StepDone})
	f.add(StepRow{StepName: "fix", Ordinal: 2, Status: StepDone})
	f.add(StepRow{StepName: "review", Ordinal: 2, Status: StepDone})
	f.add(StepRow{StepName: "review", Ordinal: 3, Status: StepPending})
	f.add(StepRow{StepName: "lint", Ordinal: 4, Status: StepDone})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "fix", Ordinal: 5, Status: StepDone})

	ask := func(name string, names []string, want int) {
		t.Helper()
		got, err := StepRoundCountTx(f.tx, f.run, f.issue, names)
		if err != nil || got != want {
			t.Errorf("%s = (%d, %v), want (%d, nil)", name, got, err, want)
		}
	}
	ask("fix and review", []string{"fix", "review"}, 3)
	ask("no bodies", nil, 0)
	ask("a body that never ran", []string{"absent"}, 0)
}

func TestRoutingQueriesIssueStepsBelowOrdinalTx(t *testing.T) {
	f := newRoutingFixture(t)
	a := f.add(StepRow{StepName: "implement", Ordinal: 0, Status: StepDone})
	b := f.add(StepRow{StepName: "review", Ordinal: 1, Status: StepPending})
	f.add(StepRow{StepName: "review", Ordinal: 2, Status: StepPending})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "review", Ordinal: 0, Status: StepPending})

	got, err := IssueStepsBelowOrdinalTx(f.tx, f.run, f.issue, 2)
	testsupport.Must(t, err, "IssueStepsBelowOrdinalTx: %v", err)
	want := []StepInstanceRef{
		{ID: a, Instance: "implement@0", Status: StepDone},
		{ID: b, Instance: "review@1", Status: StepPending},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("steps below ordinal 2 = %+v, want %+v", got, want)
	}
	got, err = IssueStepsBelowOrdinalTx(f.tx, f.run, f.issue, 0)
	if err != nil || len(got) != 0 {
		t.Errorf("below ordinal 0 = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestRoutingQueriesStepByInstanceTx(t *testing.T) {
	f := newRoutingFixture(t)
	mine := f.add(StepRow{StepName: "review", Ordinal: 1, Status: StepSkipped})
	theirs := f.add(StepRow{IssueID: f.otherIssue, StepName: "review", Ordinal: 1, Status: StepPending})

	got, err := StepByInstanceTx(f.tx, f.run, f.issue, "review@1")
	if err != nil || got != (StepInstanceRef{ID: mine, Instance: "review@1", Status: StepSkipped}) {
		t.Errorf("this issue's review@1 = (%+v, %v), want step %d skipped", got, err, mine)
	}
	got, err = StepByInstanceTx(f.tx, f.run, f.otherIssue, "review@1")
	if err != nil || got.ID != theirs {
		t.Errorf("the other issue's review@1 = (%+v, %v), want step %d", got, err, theirs)
	}
	if _, err := StepByInstanceTx(f.tx, f.run, f.issue, "review@2"); !errors.Is(err, ErrStepNotFound) {
		t.Errorf("a missing instance's error = %v, want ErrStepNotFound", err)
	}
}

func TestRoutingQueriesStepNamesAtOrdinalTx(t *testing.T) {
	f := newRoutingFixture(t)
	zero, one := 0, 1
	f.add(StepRow{StepName: "review", Ordinal: 1, SiblingIndex: &zero, Instance: "review@1#0", Status: StepPending})
	f.add(StepRow{StepName: "review", Ordinal: 1, SiblingIndex: &one, Instance: "review@1#1", Status: StepPending})
	f.add(StepRow{StepName: "verify", Ordinal: 1, Status: StepPending})
	f.add(StepRow{StepName: "implement", Ordinal: 0, Status: StepDone})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "commit", Ordinal: 1, Status: StepPending})

	got, err := StepNamesAtOrdinalTx(f.tx, f.run, f.issue, 1)
	testsupport.Must(t, err, "StepNamesAtOrdinalTx: %v", err)
	if want := map[string]bool{"review": true, "verify": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("names at ordinal 1 = %v, want %v", got, want)
	}
	got, err = StepNamesAtOrdinalTx(f.tx, f.run, f.issue, 7)
	if err != nil || len(got) != 0 {
		t.Errorf("an empty ordinal = (%v, %v), want (none, nil)", got, err)
	}
}

func TestRoutingQueriesSetRunIssueLoopCountTx(t *testing.T) {
	f := newRoutingFixture(t)
	loopCount := func(runID, issueID int) int {
		t.Helper()
		var n int
		testsupport.Must(t, f.tx.QueryRow(
			`SELECT loop_count FROM run_issues WHERE run_id = ? AND issue_id = ?`, runID, issueID).
			Scan(&n), "reading the loop count")
		return n
	}
	testsupport.Must(t, SetRunIssueLoopCountTx(f.tx, f.run, f.issue, 3), "SetRunIssueLoopCountTx")
	if got := loopCount(f.run, f.issue); got != 3 {
		t.Errorf("loop_count = %d, want 3", got)
	}
	if a, b := loopCount(f.run, f.otherIssue), loopCount(f.otherRun, f.issue); a != 0 || b != 0 {
		t.Errorf("other bindings moved to (%d, %d), want (0, 0)", a, b)
	}
	if err := SetRunIssueLoopCountTx(f.tx, f.run, 999, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("an issue not in the run: error = %v, want ErrNotFound", err)
	}
}

func TestRoutingQueriesStepHasLaterInstanceTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.add(StepRow{StepName: "review", Ordinal: 0, Status: StepDone})
	f.add(StepRow{StepName: "review", Ordinal: 1, Status: StepPending})
	f.add(StepRow{StepName: "verify", Ordinal: 3, Status: StepPending})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "review", Ordinal: 4, Status: StepPending})

	ask := func(name, step string, ordinal int, want bool) {
		t.Helper()
		got, err := StepHasLaterInstanceTx(f.tx, f.run, f.issue, step, ordinal)
		if err != nil || got != want {
			t.Errorf("%s = (%v, %v), want (%v, nil)", name, got, err, want)
		}
	}
	ask("review@0 has review@1", "review", 0, true)
	ask("review@1 is the latest", "review", 1, false)
	ask("a step with no instance", "absent", 0, false)
}

func TestRoutingQueriesHighestOrdinalsTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.add(StepRow{StepName: "implement", Ordinal: 0, Status: StepDone})
	f.add(StepRow{StepName: "review", Ordinal: 0, Status: StepDone})
	f.add(StepRow{StepName: "review", Ordinal: 2, Status: StepPending})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Ordinal: 5, Status: StepPending})

	got, err := HighestOrdinalsTx(f.tx, f.run, f.issue)
	testsupport.Must(t, err, "HighestOrdinalsTx: %v", err)
	if want := map[string]int{"implement": 0, "review": 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("highest ordinals = %v, want %v", got, want)
	}
	got, err = HighestOrdinalsTx(f.tx, f.otherRun, f.issue)
	if err != nil || len(got) != 0 {
		t.Errorf("an issue with no steps in the run = (%v, %v), want (none, nil)", got, err)
	}
}

func TestRoutingQueriesGatedStepIDAtOrdinal(t *testing.T) {
	f := newRoutingFixture(t)
	f.add(StepRow{StepName: "verify", Ordinal: 0, Status: StepGated})
	gated := f.add(StepRow{StepName: "verify", Ordinal: 1, Status: StepGated})
	f.add(StepRow{StepName: "review", Ordinal: 1, Status: StepPending})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "review", Ordinal: 1, Status: StepGated})
	testsupport.Must(t, f.tx.Commit(), "Commit")

	id, found, err := GatedStepIDAtOrdinal(f.db, f.run, f.issue, "verify", 1)
	if err != nil || !found || id != gated {
		t.Errorf("gated verify@1 = (%d, %v, %v), want (%d, true, nil)", id, found, err, gated)
	}
	id, found, err = GatedStepIDAtOrdinal(f.db, f.run, f.issue, "review", 1)
	if err != nil || found || id != 0 {
		t.Errorf("a pending review@1 = (%d, %v, %v), want (0, false, nil)", id, found, err)
	}
}

func TestRoutingQueriesDeferralRecordedForDispatchTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "review", Status: StepPending})
	other := f.add(StepRow{StepName: "verify", Status: StepPending})
	q := DispatchDeferralQuery{
		RunID: f.run, StepID: step, Kind: "step-deferred", Cause: "chain", Dispatch: "DISPATCH-7",
	}
	ask := func(name string, want bool) {
		t.Helper()
		got, err := DeferralRecordedForDispatchTx(f.tx, q)
		if err != nil || got != want {
			t.Errorf("%s = (%v, %v), want (%v, nil)", name, got, err, want)
		}
	}
	f.event("step-deferred", step, `{"cause":"budget","dispatch":"DISPATCH-7"}`)
	f.event("step-deferred", step, `{"cause":"chain","dispatch":"DISPATCH-6"}`)
	f.event("step-claimed", step, `{"cause":"chain","dispatch":"DISPATCH-7"}`)
	f.event("step-deferred", other, `{"cause":"chain","dispatch":"DISPATCH-7"}`)
	ask("only near misses recorded", false)

	f.event("step-deferred", step, `{"cause":"chain","dispatch":"DISPATCH-7"}`)
	ask("the deferral recorded", true)
}

func TestRoutingQueriesStepsWithEventKindsSinceTx(t *testing.T) {
	f := newRoutingFixture(t)
	early := f.add(StepRow{StepName: "implement", Status: StepPending})
	claimed := f.add(StepRow{StepName: "review", Status: StepPending})
	failed := f.add(StepRow{StepName: "verify", Status: StepPending})
	other := f.add(StepRow{StepName: "commit", Status: StepPending})
	opened := f.event("step-claimed", early, `{}`)
	f.event("step-claimed", claimed, `{}`)
	f.event("step-claimed", claimed, `{}`)
	f.event("step-failed", failed, `{}`)
	f.event("step-recorded", other, `{}`)
	f.event("step-claimed", 0, `{}`)
	kinds := []string{"step-claimed", "step-failed"}

	got, err := StepsWithEventKindsSinceTx(f.tx, f.run, opened, kinds)
	testsupport.Must(t, err, "StepsWithEventKindsSinceTx: %v", err)
	if want := map[int]bool{claimed: true, failed: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("attempted steps = %v, want %v", got, want)
	}
	got, err = StepsWithEventKindsSinceTx(f.tx, f.otherRun, 0, kinds)
	if err != nil || len(got) != 0 {
		t.Errorf("a run with no events = (%v, %v), want (none, nil)", got, err)
	}
	got, err = StepsWithEventKindsSinceTx(f.tx, f.run, 0, nil)
	if err != nil || len(got) != 0 {
		t.Errorf("no kinds = (%v, %v), want (none, nil)", got, err)
	}
}

func TestRoutingQueriesLastDispatchDeferralTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "review", Status: StepPending})
	q := SourcedDeferralQuery{
		RunID: f.run, StepID: step, Kind: "step-deferred", Cause: "chain", Source: "engine",
	}
	ask := func(name string, wantSeq int64, wantData string, wantFound bool) {
		t.Helper()
		seq, data, found, err := LastDispatchDeferralTx(f.tx, q)
		testsupport.Must(t, err, "%s: %v", name, err)
		if seq != wantSeq || data != wantData || found != wantFound {
			t.Errorf("%s = (%d, %q, %v), want (%d, %q, %v)",
				name, seq, data, found, wantSeq, wantData, wantFound)
		}
	}
	dispatch := func(status string, closedAt int64) string {
		t.Helper()
		res, err := f.tx.Exec(`INSERT INTO dispatches (run_id, status, opened_seq, expires_ms, closed_at_ms, created_at_ms)
			VALUES (?, ?, 0, 1, ?, 1)`, f.run, status, closedAt)
		testsupport.Must(t, err, "seeding a dispatch: %v", err)
		id, _ := res.LastInsertId()
		return fmt.Sprintf("DISPATCH-%d", id)
	}
	first := dispatch(DispatchClosed, 10)
	f.event("step-deferred", step, fmt.Sprintf(`{"cause":"chain","source":"engine","dispatch":%q}`, first))
	latest := dispatch(DispatchClosed, 20)
	ask("only a deferral of an earlier dispatch", 0, "", false)

	open := dispatch("open", 30)
	f.event("step-deferred", step, fmt.Sprintf(`{"cause":"chain","dispatch":%q}`, latest))
	f.event("step-deferred", step, fmt.Sprintf(`{"cause":"budget","source":"engine","dispatch":%q}`, latest))
	f.event("step-deferred", step, fmt.Sprintf(`{"cause":"chain","source":"engine","dispatch":%q}`, open))
	ask("only conductor, budget, and open-dispatch deferrals", 0, "", false)

	older := fmt.Sprintf(`{"cause":"chain","source":"engine","dispatch":%q,"n":1}`, latest)
	newer := fmt.Sprintf(`{"cause":"chain","source":"engine","dispatch":%q,"n":2}`, latest)
	f.event("step-deferred", step, older)
	newerSeq := f.event("step-deferred", step, newer)
	ask("the newest deferral of the latest closed dispatch", newerSeq, newer, true)
}

// holdFixture seeds one hold's rows out of instance order, plus rows the hold
// queries must not match: another hold at the next ordinal, the same hold on
// another issue, and the same hold in another run.
func holdFixture(t *testing.T) *routingFixture {
	t.Helper()
	f := newRoutingFixture(t)
	zero, one := 0, 1
	approved := f.add(StepRow{StepName: "review-held", Ordinal: 1, SiblingIndex: &one,
		Instance: "review-held@1#1", Status: StepDone, Materialized: true})
	f.exec(`UPDATE steps SET routing = 'pass' WHERE id = ?`, approved)
	f.add(StepRow{StepName: "review-held", Ordinal: 1, Status: StepWaitingHuman, Materialized: true})
	f.add(StepRow{StepName: "review-held", Ordinal: 1, SiblingIndex: &zero,
		Instance: "review-held@1#0", Status: StepPending, Materialized: true})
	f.add(StepRow{StepName: "review-held", Ordinal: 2, SiblingIndex: &zero,
		Instance: "review-held@2#0", Status: StepPending, Materialized: true})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "review-held", Ordinal: 1, SiblingIndex: &zero,
		Instance: "review-held@1#0", Status: StepPending, Materialized: true})
	f.add(StepRow{RunID: f.otherRun, StepName: "review-held", Ordinal: 1, SiblingIndex: &zero,
		Instance: "review-held@1#0", Status: StepPending, Materialized: true})
	return f
}

func TestActivationQueriesHoldInstances(t *testing.T) {
	f := holdFixture(t)
	testsupport.Must(t, f.tx.Commit(), "Commit")

	got, err := HoldInstances(f.db, f.run, f.issue, "review-held@1")
	testsupport.Must(t, err, "HoldInstances: %v", err)
	want := []InstanceStatusRouting{
		{Instance: "review-held@1", Status: StepWaitingHuman},
		{Instance: "review-held@1#0", Status: StepPending},
		{Instance: "review-held@1#1", Status: StepDone, Routing: "pass"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("review-held@1's rows = %+v, want %+v (instance order; a NULL routing reads \"\")", got, want)
	}

	got, err = HoldInstances(f.db, f.run, f.issue, "review-held@3")
	if err != nil || len(got) != 0 {
		t.Errorf("a hold never materialized = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestActivationQueriesHoldClusterInstancesTx(t *testing.T) {
	f := holdFixture(t)

	got, err := HoldClusterInstancesTx(f.tx, f.run, f.issue, "review-held@1")
	testsupport.Must(t, err, "HoldClusterInstancesTx: %v", err)
	want := []InstanceStatusRouting{
		{Instance: "review-held@1#0", Status: StepPending},
		{Instance: "review-held@1#1", Status: StepDone, Routing: "pass"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("review-held@1's clusters = %+v, want %+v (no whole-hold row)", got, want)
	}

	got, err = HoldClusterInstancesTx(f.tx, f.run, f.issue, "review-held@3")
	if err != nil || len(got) != 0 {
		t.Errorf("a hold with no clusters = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestActivationQueriesNewestStepArtifactExceptKindsTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "triage", Status: StepDone})
	other := f.add(StepRow{StepName: "review", Status: StepDone})
	onlyOwned := f.add(StepRow{StepName: "lint", Status: StepDone})
	f.artifact(step, "findings", "s1", "b", `[{"n":"old"}]`)
	f.artifact(step, "findings", "s2", "b", `[{"n":"new"}]`)
	f.artifact(step, "gap", "s3", "b", `[{"n":"gap"}]`)
	f.artifact(step, "recorded-head", "s4", "b", "")
	f.artifact(other, "findings", "s5", "b", `[{"n":"other step"}]`)
	f.artifact(onlyOwned, "gap", "s6", "b", `[]`)

	var newest int
	testsupport.Must(t, f.tx.QueryRow(
		`SELECT id FROM artifacts WHERE step_id = ? AND sha256 = 's2'`, step).Scan(&newest), "reading s2's id")
	owned := []string{"gap", "recorded-head"}

	got, found, err := NewestStepArtifactExceptKindsTx(f.tx, step, owned)
	testsupport.Must(t, err, "NewestStepArtifactExceptKindsTx: %v", err)
	want := StepArtifactRef{ID: newest, Kind: "findings", Payload: `[{"n":"new"}]`}
	if !found || got != want {
		t.Errorf("the newest unowned artifact = (%+v, %v), want (%+v, true)", got, found, want)
	}

	got, found, err = NewestStepArtifactExceptKindsTx(f.tx, step, nil)
	testsupport.Must(t, err, "with no kinds excluded: %v", err)
	if !found || got.Kind != "recorded-head" || got.Payload != "" {
		t.Errorf("with no kinds excluded = (%+v, %v), want the NULL-payload recorded-head", got, found)
	}

	got, found, err = NewestStepArtifactExceptKindsTx(f.tx, onlyOwned, owned)
	if err != nil || found || got != (StepArtifactRef{}) {
		t.Errorf("a step with only excluded kinds = (%+v, %v, %v), want (zero, false, nil)", got, found, err)
	}
}

func TestActivationQueriesStepIDAtOrdinal(t *testing.T) {
	f := newRoutingFixture(t)
	f.add(StepRow{StepName: "review", Ordinal: 0, Status: StepDone})
	review1 := f.add(StepRow{StepName: "review", Ordinal: 1, Status: StepWaitingHuman})
	f.add(StepRow{StepName: "verify", Ordinal: 1, Status: StepPending})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "review", Ordinal: 2, Status: StepPending})
	testsupport.Must(t, f.tx.Commit(), "Commit")

	id, found, err := StepIDAtOrdinal(f.db, f.run, f.issue, "review", 1)
	if err != nil || !found || id != review1 {
		t.Errorf("review@1 = (%d, %v, %v), want (%d, true, nil)", id, found, err, review1)
	}
	id, found, err = StepIDAtOrdinal(f.db, f.run, f.issue, "review", 2)
	if err != nil || found || id != 0 {
		t.Errorf("review@2 of an issue without one = (%d, %v, %v), want (0, false, nil)", id, found, err)
	}
}

func TestActivationQueriesProjectIdentityTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.exec(`INSERT INTO projects (id, identity, name, prefix) VALUES (7, 'github.com/acme/widgets', 'widgets', 'WID')`)

	got, err := ProjectIdentityTx(f.tx, 7)
	if err != nil || got != "github.com/acme/widgets" {
		t.Errorf("project 7's identity = (%q, %v), want (github.com/acme/widgets, nil)", got, err)
	}
	got, err = ProjectIdentityTx(f.tx, DefaultProjectID)
	if err != nil || got != "" {
		t.Errorf("the default project's identity = (%q, %v), want (\"\", nil)", got, err)
	}
	got, err = ProjectIdentityTx(f.tx, 0)
	if err != nil || got != "" {
		t.Errorf("project 0's identity = (%q, %v), want the default project's (\"\", nil)", got, err)
	}
	if _, err := ProjectIdentityTx(f.tx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing project's error = %v, want ErrNotFound", err)
	}
}

func TestActivationQueriesRepinStepWorkRootTx(t *testing.T) {
	f := newRoutingFixture(t)
	step := f.add(StepRow{StepName: "implement", Status: StepDone})
	sibling := f.add(StepRow{StepName: "review", Status: StepPending})
	f.exec(`UPDATE steps SET work_root = '/old' WHERE id IN (?, ?)`, step, sibling)

	testsupport.Must(t, RepinStepWorkRootTx(f.tx, step, "/repinned"), "RepinStepWorkRootTx")
	roots := map[int]string{}
	for _, id := range []int{step, sibling} {
		var root string
		testsupport.Must(t, f.tx.QueryRow(`SELECT work_root FROM steps WHERE id = ?`, id).Scan(&root),
			"reading step %d's work_root", id)
		roots[id] = root
	}
	if want := map[int]string{step: "/repinned", sibling: "/old"}; !reflect.DeepEqual(roots, want) {
		t.Errorf("work roots after the re-pin = %v, want %v (only the re-pinned step moves)", roots, want)
	}

	if err := RepinStepWorkRootTx(f.tx, 999, "/repinned"); !errors.Is(err, ErrStepNotFound) {
		t.Errorf("re-pinning a missing step = %v, want ErrStepNotFound", err)
	}
}

func TestActivationQueriesRunExpectedCostTx(t *testing.T) {
	f := newRoutingFixture(t)
	got, err := RunExpectedCostTx(f.tx, f.run)
	if err != nil || got != 0 {
		t.Errorf("a run with no steps = (%v, %v), want (0, nil)", got, err)
	}

	f.add(StepRow{StepName: "implement", Status: StepDone, ExpectedCost: 2.5})
	f.add(StepRow{IssueID: f.otherIssue, StepName: "implement", Status: StepPending, ExpectedCost: 4})
	f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepPending, ExpectedCost: 100})

	got, err = RunExpectedCostTx(f.tx, f.run)
	if err != nil || got != 6.5 {
		t.Errorf("run's expected cost = (%v, %v), want (6.5, nil)", got, err)
	}
}

// workflowRow seeds one registered workflow in a project and returns its id.
func (f *routingFixture) workflowRow(projectID int, name string, version int, parsed string) int {
	f.t.Helper()
	res, err := f.tx.Exec(
		`INSERT INTO workflows (project_id, name, version, source_sha256, body, parsed, created_at_ms, row_version)
		 VALUES (?, ?, ?, 'sha', 'body', ?, 1000, 1)`, projectID, name, version, parsed)
	testsupport.Must(f.t, err, "seeding workflow %s@%d: %v", name, version, err)
	id, _ := res.LastInsertId()
	return int(id)
}

func TestActivationQueriesProjectWorkflowsTx(t *testing.T) {
	f := newRoutingFixture(t)
	f.exec(`INSERT INTO projects (id, identity, name, prefix) VALUES (7, 'github.com/acme/widgets', 'widgets', 'WID')`)
	alpha := f.workflowRow(DefaultProjectID, "alpha", 1, `{"a":1}`)
	wf2 := f.workflowRow(DefaultProjectID, "wf", 2, `{"w":2}`)
	f.exec(`UPDATE workflows SET description = 'second', deprecated_at_ms = 5 WHERE id = ?`, wf2)
	elsewhere := f.workflowRow(7, "alpha", 1, `{"elsewhere":true}`)

	got, err := ProjectWorkflowsTx(f.tx, 0)
	testsupport.Must(t, err, "ProjectWorkflowsTx: %v", err)
	type row struct {
		ID           int
		Name         string
		Version      int
		Parsed       string
		Description  string
		DeprecatedAt int64
	}
	var rows []row
	for _, wf := range got {
		rows = append(rows, row{wf.ID, wf.Name, wf.Version, wf.Parsed, wf.Description, wf.DeprecatedAtMS})
	}
	want := []row{
		{alpha, "alpha", 1, `{"a":1}`, "", 0},
		{wf2, "wf", 2, `{"w":2}`, "second", 5},
		{f.workflow, "wf", 1, `{}`, "", 0},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("the default project's workflows = %+v, want %+v (name, then newest version)", rows, want)
	}

	got, err = ProjectWorkflowsTx(f.tx, 7)
	if err != nil || len(got) != 1 || got[0].ID != elsewhere {
		t.Errorf("project 7's workflows = (%+v, %v), want only workflow %d", got, err, elsewhere)
	}
	got, err = ProjectWorkflowsTx(f.tx, 999)
	if err != nil || len(got) != 0 {
		t.Errorf("a project with no workflows = (%+v, %v), want (none, nil)", got, err)
	}
}

// definitionsFixture seeds a run whose steps were expanded from two workflows
// and whose issues are bound to two (one shared), plus another run's step and
// binding to a workflow neither query of the first run may return.
func definitionsFixture(t *testing.T) (f *routingFixture, second, bound int) {
	t.Helper()
	f = newRoutingFixture(t)
	second = f.workflowRow(DefaultProjectID, "second", 1, `{"second":true}`)
	bound = f.workflowRow(DefaultProjectID, "bound", 1, `{"bound":true}`)
	unrelated := f.workflowRow(DefaultProjectID, "unrelated", 1, `{"unrelated":true}`)

	f.add(StepRow{StepName: "implement", Ordinal: 0, Status: StepDone})
	f.add(StepRow{StepName: "implement", Ordinal: 1, Status: StepPending})
	review := f.add(StepRow{IssueID: f.otherIssue, StepName: "review", Status: StepPending})
	f.exec(`UPDATE steps SET workflow_id = ? WHERE id = ?`, second, review)
	foreign := f.add(StepRow{RunID: f.otherRun, StepName: "implement", Status: StepPending})
	f.exec(`UPDATE steps SET workflow_id = ? WHERE id = ?`, unrelated, foreign)

	f.exec(`UPDATE run_issues SET workflow_id = ? WHERE run_id = ? AND issue_id = ?`, bound, f.run, f.issue)
	f.exec(`UPDATE run_issues SET workflow_id = ? WHERE run_id = ? AND issue_id = ?`, f.workflow, f.run, f.otherIssue)
	f.exec(`UPDATE run_issues SET workflow_id = ? WHERE run_id = ? AND issue_id = ?`, unrelated, f.otherRun, f.issue)
	return f, second, bound
}

func TestActivationQueriesRunStepWorkflows(t *testing.T) {
	f, second, _ := definitionsFixture(t)
	testsupport.Must(t, f.tx.Commit(), "Commit")

	got, err := RunStepWorkflows(f.db, f.run)
	testsupport.Must(t, err, "RunStepWorkflows: %v", err)
	want := []ParsedWorkflow{{ID: f.workflow, Parsed: `{}`}, {ID: second, Parsed: `{"second":true}`}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the run's step workflows = %+v, want %+v (each once, by id)", got, want)
	}

	empty, err := InsertRun(f.db, 1, "req", 0, 1000)
	testsupport.Must(t, err, "InsertRun: %v", err)
	got, err = RunStepWorkflows(f.db, empty.ID)
	if err != nil || len(got) != 0 {
		t.Errorf("a run with no steps = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestActivationQueriesRunStepWorkflowsTx(t *testing.T) {
	f, second, _ := definitionsFixture(t)

	got, err := RunStepWorkflowsTx(f.tx, f.run)
	testsupport.Must(t, err, "RunStepWorkflowsTx: %v", err)
	want := []ParsedWorkflow{{ID: f.workflow, Parsed: `{}`}, {ID: second, Parsed: `{"second":true}`}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the run's step workflows = %+v, want %+v (each once, by id)", got, want)
	}
	got, err = RunStepWorkflowsTx(f.tx, 999)
	if err != nil || len(got) != 0 {
		t.Errorf("a run with no steps = (%+v, %v), want (none, nil)", got, err)
	}
}

func TestActivationQueriesRunBoundWorkflows(t *testing.T) {
	f, _, bound := definitionsFixture(t)
	testsupport.Must(t, f.tx.Commit(), "Commit")

	got, err := RunBoundWorkflows(f.db, f.run)
	testsupport.Must(t, err, "RunBoundWorkflows: %v", err)
	want := []ParsedWorkflow{{ID: f.workflow, Parsed: `{}`}, {ID: bound, Parsed: `{"bound":true}`}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the run's bound workflows = %+v, want %+v (each once, by id)", got, want)
	}

	unbound, err := InsertRun(f.db, 1, "req", 0, 1000)
	testsupport.Must(t, err, "InsertRun: %v", err)
	testsupport.Must(t, AddRunIssue(f.db, unbound.ID, f.issue), "AddRunIssue")
	got, err = RunBoundWorkflows(f.db, unbound.ID)
	if err != nil || len(got) != 0 {
		t.Errorf("a run whose issues are unbound = (%+v, %v), want (none, nil)", got, err)
	}
}
