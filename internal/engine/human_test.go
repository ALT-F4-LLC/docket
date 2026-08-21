package engine

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// §6.15's lifecycle, pinned.

// TestApproveMovesAHumanStepToDone is §6.15's human row, first half.
func TestApproveMovesAHumanStepToDone(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", "")
	driveAction(t, conn, e, "reconcile@0")
	claimAndComplete(t, conn, e, "verify@0", "report", `[{"status":"met"}]`)

	gateID := stepIDByInstance(t, conn, "commit-gate@0")

	err := e.DecideStep(conn, gateID, true, "looks right", nowMS)
	testsupport.Must(t, err, "approve: %v", err)
	if got := stepStatus(t, conn, "commit-gate@0"); got != db.StepDone {
		t.Errorf("status after approve = %q, want %q", got, db.StepDone)
	}
}

// TestRejectRoutesPerEffectiveOnFailAndNeverWaitingHuman is §6.15's second
// half, and the reason V13a exists.
//
// The fixture's `commit-gate` declares `on_fail = "fix-loop"` explicitly. A
// reject must route THERE — never `waiting-human`, which would park the issue
// on the resolution of the thing that just rejected.
func TestRejectRoutesPerEffectiveOnFailAndNeverWaitingHuman(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", "")
	driveAction(t, conn, e, "reconcile@0")
	claimAndComplete(t, conn, e, "verify@0", "report", `[{"status":"met"}]`)

	gateID := stepIDByInstance(t, conn, "commit-gate@0")
	err := e.DecideStep(conn, gateID, false, "not yet", nowMS)
	testsupport.Must(t, err, "reject: %v", err)

	step, err := db.GetStep(conn, gateID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if !strings.HasPrefix(step.Routing, workflow.OnFailFixLoop) {
		t.Errorf("reject routed %q, want the fixture's explicit on_fail %q",
			step.Routing, workflow.OnFailFixLoop)
	}
	if step.Status == db.StepWaitingHuman {
		t.Error("a rejected human gate PARKED in waiting-human — it would wait " +
			"on the resolution of the thing that just rejected (V13/DKT-17)")
	}
}

// TestApproveRejectRefuseANonHumanStep is R10.
func TestApproveRejectRefuseANonHumanStep(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	executorID := stepIDByInstance(t, conn, "implement@0")

	for _, approve := range []bool{true, false} {
		err := e.DecideStep(conn, executorID, approve, "", nowMS)
		if err == nil {
			t.Fatalf("approve=%v on an executor step was accepted", approve)
		}
		if code, _ := CodeOf(err); code != CodeValidation {
			t.Errorf("code = %v, want VALIDATION_ERROR (R10)", code)
		}
		// The refusal names the step's ACTUAL class: an operator reaching for
		// approve has usually mistaken which step is blocking.
		if !strings.Contains(err.Error(), "executor") {
			t.Errorf("the refusal does not name the step's class: %v", err)
		}
	}
}

// TestApproveRejectOnWaitingHumanExecutorNamesResolve is DKT-104: approve/
// reject on an executor step that is PARKED waiting-human (attempts
// exhausted) must name the verb that actually moves it — `step resolve --as`
// — not just the one that doesn't apply. The grammar fix ("a executor" ->
// "an executor") rides the same message.
func TestApproveRejectOnWaitingHumanExecutorNamesResolve(t *testing.T) {
	const src = `
[pipeline]
name = "parks"
version = 1

[match]
kind = ["task"]

[[step]]
name = "flaky"
after = []
executor = "w"
emits = "out"
max_attempts = 1
on_fail = "waiting-human"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "parks.toml")
	issue := createIssue(t, conn, "parked", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "flaky@0")

	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, id, claim.Token, "gave up", "", nowMS)
	testsupport.Must(t, err, "fail: %v", err)
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepWaitingHuman {
		t.Fatalf("status = %q, want %q (max_attempts = 1)", step.Status, db.StepWaitingHuman)
	}

	err = e.DecideStep(conn, id, true, "", nowMS)
	if err == nil {
		t.Fatal("approve on a waiting-human executor step was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("code = %v, want VALIDATION_ERROR (R10)", code)
	}
	msg := err.Error()
	if !strings.Contains(msg, "an executor step") {
		t.Errorf("refusal grammar = %q, want %q somewhere in it", msg, "an executor step")
	}
	if !strings.Contains(msg, "step resolve") || !strings.Contains(msg, "--as") {
		t.Errorf("refusal does not name `step resolve --as`: %v", err)
	}
	for _, v := range []string{ResolveRetry, ResolveSkip, ResolveAbandonIssue, ResolveOverridePass} {
		if !strings.Contains(msg, v) {
			t.Errorf("refusal omits --as value %q: %v", v, err)
		}
	}
}

// TestClaimRefusesHumanVoteAndActionSteps is §6.15's "none of the three is
// offered as executor work", asserted as a test rather than left to the reader.
//
// The `action` row carries the weight: an action step that
// could be claimed is a relay a dispatcher would write to copy a predecessor's
// payload onto it, which is the claim+complete shim D13 forbids. The refusal is
// what makes that unwritable rather than merely discouraged.
func TestClaimRefusesHumanVoteAndActionSteps(t *testing.T) {
	const src = `
[pipeline]
name = "gates-only"
version = 1

[match]
kind = ["task"]

[[step]]
name = "decide"
after = []
type = "human"
on_fail = "skip"

[[step]]
name = "poll"
after = []
type = "vote"
voters = ["a", "b"]
vote_rule = "majority"
on_fail = "skip"

[[step]]
name = "seed"
after = []
executor = "x"
emits = "findings"

[[step]]
name = "reduce"
after = ["seed"]
action = "aggregate"
inputs = ["seed.findings"]
params = { field = "severity", method = "median", output = "findings" }
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "gates-only.toml")
	issue := createIssue(t, conn, "gated", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// Each row names the class the refusal must carry, and what the refusal must
	// say ADVANCES the step instead. The `action` row's "the engine" is the
	// substantive half: a refusal naming an operator verb would be an invitation
	// to write the shim.
	cases := []struct{ instance, class, advances string }{
		{"decide@0", "human", "approve|reject"},
		{"poll@0", "vote", "resolve"},
		{"reduce@0", "action", "the engine"},
	}

	for _, c := range cases {
		t.Run(c.instance, func(t *testing.T) {
			id := stepIDByInstance(t, conn, c.instance)
			_, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
			if err == nil {
				t.Fatalf("%s was claimable; it is not executor work", c.instance)
			}
			if code, _ := CodeOf(err); code != CodeConflict {
				t.Errorf("code = %v, want CONFLICT", code)
			}
			// The refusal names the class, so a dispatcher knows why.
			msg := err.Error()
			if !strings.Contains(msg, c.class) {
				t.Errorf("the refusal does not name the step class %q: %s",
					c.class, msg)
			}
			if !strings.Contains(msg, c.advances) {
				t.Errorf("the refusal does not say what advances the step "+
					"(%q): %s", c.advances, msg)
			}
			if !strings.Contains(msg, "not by a worker") {
				t.Errorf("the refusal does not rule out a worker: %s", msg)
			}
		})
	}
}

// TestVoteStepParksAndOnlyResolveMovesIt is §6.15's vote row: at S3 a ready
// vote step is offered, claimable by nothing, and moved only by `step resolve`.
//
// Vote EXECUTION is deferred to S4 explicitly (§1's scope table), so this
// asserts the park is real rather than that voting works.
func TestVoteStepParksAndOnlyResolveMovesIt(t *testing.T) {
	const src = `
[pipeline]
name = "vote-only"
version = 1

[match]
kind = ["task"]

[[step]]
name = "poll"
after = []
type = "vote"
voters = ["a", "b"]
vote_rule = "majority"
on_fail = "skip"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "vote-only.toml")
	issue := createIssue(t, conn, "voted", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "poll@0")

	// It EXPANDS and becomes ready — a dispatcher can see the run is waiting.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ready, cond := sched.Ready(stepNamed(t, sched, "poll@0")); !ready {
			t.Errorf("a vote step is not ready: %s", cond)
		}
	})

	// No verb advances it: not claim, not approve, not reject.
	if _, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS}); err == nil {
		t.Error("a vote step was claimable")
	}
	if err := e.DecideStep(conn, id, true, "", nowMS); err == nil {
		t.Error("approve advanced a vote step; voting lands at stage 4")
	}

	if got := stepStatus(t, conn, "poll@0"); got != db.StepPending {
		t.Errorf("the vote step moved to %q without any verb advancing it", got)
	}

	// `step resolve` is the one thing that does (§6.15).
	err = e.ResolveStep(conn, id, ResolveSkip, "not needed", nowMS)
	testsupport.Must(t, err, "resolve --as skip on a vote step: %v — it is the operator's "+
		"only way past one at S3, and refusing would deadlock the run", err)

	if got := stepStatus(t, conn, "poll@0"); got != db.StepSkipped {
		t.Errorf("status after resolve --as skip = %q, want %q", got, db.StepSkipped)
	}
}

// TestResolveRefusesAnUnparkedStep is R11.
func TestResolveRefusesAnUnparkedStep(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	id := stepIDByInstance(t, conn, "implement@0")
	err := e.ResolveStep(conn, id, ResolveSkip, "", nowMS)
	if err == nil {
		t.Fatal("resolve on a pending executor step was accepted")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("code = %v, want VALIDATION_ERROR (R11)", code)
	}
}

// TestResolveRetryResetsAttemptsAndClearsTheSaga pins §6.10's retry semantics,
// including the saga clear that a naive implementation omits: a retry re-does
// the work, so a half-finished saga from the previous try must not resume.
//
// "Attempts reset" means the BUDGET resets (DKT-86/DKT-90): `attempt_base`
// moves to the current attempt, and `attempt` itself carries forward — it is
// the usage ledger's key half, and zeroing it made a retried step's next claim
// re-mint an attempt number whose ledger slot was already taken.
func TestResolveRetryResetsAttemptsAndClearsTheSaga(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	id := stepIDByInstance(t, conn, "implement@0")

	// Claim twice (via expiry) so `attempt` is 2, then park it.
	_, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "first claim: %v", err)
	later := nowMS + 10_000_000
	_, err = ClaimStep(conn, id, ClaimOptions{Owner: "w2", NowMS: later})
	testsupport.Must(t, err, "second claim: %v", err)
	execSQL(t, conn, `UPDATE steps SET status = ?, saga_stage = ? WHERE id = ?`,
		db.StepWaitingHuman, db.SagaRecorded, id)

	before, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if before.Attempt != 2 {
		t.Fatalf("attempt = %d before the retry, want 2", before.Attempt)
	}

	err = e.ResolveStep(conn, id, ResolveRetry, "try again", later)
	testsupport.Must(t, err, "resolve --as retry: %v", err)

	after, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if after.Attempt != 2 {
		t.Errorf("attempt = %d after retry, want 2 — the counter is monotonic; "+
			"zeroing it collided the next claim with the ledger's "+
			"(step, attempt, unit) key (DKT-86)", after.Attempt)
	}
	if after.AttemptBase != 2 {
		t.Errorf("attempt_base = %d after retry, want 2 — §2's \"retry resets "+
			"attempts\" is the budget counting fresh from here", after.AttemptBase)
	}
	if after.Status != db.StepPending {
		t.Errorf("status = %q after retry, want %q", after.Status, db.StepPending)
	}
	if after.InSaga() {
		t.Errorf("saga_stage = %q after retry — the previous try's half-finished "+
			"saga would resume over work that is being redone", after.SagaStage)
	}
	if after.StartedMS != nil {
		t.Error("started_ms survived a retry; the new try gets a fresh " +
			"schedule-to-close budget")
	}
}

// TestFailBelowMaxAttemptsReturnsToThePool pins §6.10's `step fail` below the
// limit: the step returns to the pool and is re-offered.
//
// `attempt` COUNTS CLAIMS — and ONLY claims. A step with `max_attempts = 3`
// has attempt 1 after its first claim, and its first failure leaves it at 1,
// still below the limit; the counter next moves when the step is RE-CLAIMED.
//
// Corrected for E-8. The comment here previously read "its first failure takes
// it to 2" and drew the conclusion that the fixture's `implement` (declaring 2)
// "exhausts on the FIRST failure" — the double-count stated as doctrine, in the
// comment that taught it to the next reader.
func TestFailBelowMaxAttemptsReturnsToThePool(t *testing.T) {
	const src = `
[pipeline]
name = "retries"
version = 1

[match]
kind = ["task"]

[[step]]
name = "flaky"
after = []
executor = "w"
emits = "out"
max_attempts = 3
on_fail = "waiting-human"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "retries.toml")
	issue := createIssue(t, conn, "retried", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "flaky@0")

	// Claim (attempt 1) then fail — attempt STAYS 1, below the limit of 3.
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, id, claim.Token, "flaky", "", nowMS)
	testsupport.Must(t, err, "fail: %v", err)

	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)

	// Asserted explicitly, which it was not before: a status-only check passes
	// just as well against the double-count this test was written under.
	if step.Attempt != 1 {
		t.Errorf("attempt = %d after one claim + one failure, want 1 — the "+
			"failure does not spend an attempt the re-claim will spend (E-8)",
			step.Attempt)
	}
	if step.Status != db.StepPending {
		t.Errorf("status = %q at attempt %d of 3, want %q — a step below its "+
			"limit returns to the pool", step.Status, step.Attempt, db.StepPending)
	}
	if step.Owner != "" {
		t.Errorf("the lease survived a failure: owner = %q", step.Owner)
	}
	if step.StartedMS != nil {
		t.Error("started_ms survived a failure; the retry gets a fresh " +
			"schedule-to-close budget")
	}
}

// TestFailAtMaxAttemptsRoutesPerOnFail is the exhausted half, over the
// committed fixture: `implement` declares max_attempts = 2, so the budget is
// spent by the SECOND claim and the second failure routes.
//
// Corrected for E-8. This test previously claimed once, failed once, and
// asserted `attempt == 2` with status `waiting-human` — which pinned the
// double-count as intended behavior and made the fixture's `implement` a step
// that never got the second of its two declared attempts.
func TestFailAtMaxAttemptsRoutesPerOnFail(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	id := stepIDByInstance(t, conn, "implement@0")

	// First attempt: claim (attempt 1 of 2), fail, and RETURN TO THE POOL —
	// one of two attempts is spent, so the limit is not yet reached.
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.FailStep(conn, id, claim.Token, "gave up", "", nowMS)
	testsupport.Must(t, err, "fail: %v", err)
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Attempt != 1 || step.Status != db.StepPending {
		t.Fatalf("attempt = %d status = %q after one claim + one failure, want "+
			"1 and %q — a step with two declared attempts gets its second",
			step.Attempt, step.Status, db.StepPending)
	}

	// Second attempt: the re-claim spends the budget, and this failure routes.
	claim2, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "re-claim: %v", err)
	err = e.FailStep(conn, id, claim2.Token, "gave up again", "", nowMS)
	testsupport.Must(t, err, "second fail: %v", err)

	step, err = db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2 (two claims; failures never count)",
			step.Attempt)
	}
	if step.Status != db.StepWaitingHuman {
		t.Errorf("status = %q at the limit, want %q — the fixture's `implement` "+
			"declares on_fail = waiting-human", step.Status, db.StepWaitingHuman)
	}
	// And the token retired, so the exhausted step needs no lease to resolve.
	if step.Owner != "" {
		t.Errorf("the lease survived exhaustion: owner = %q", step.Owner)
	}
}

// TestFailWithoutMaxAttemptsRetriesIndefinitely pins that core ships NO default
// attempt limit — the same reasoning as R5's unbounded class. An operator who
// wants a bound declares one.
func TestFailWithoutMaxAttemptsRetriesIndefinitely(t *testing.T) {
	const src = `
[pipeline]
name = "unbounded-retries"
version = 1

[match]
kind = ["task"]

[[step]]
name = "forever"
after = []
executor = "w"
emits = "out"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "unbounded-retries.toml")
	issue := createIssue(t, conn, "unbounded", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "forever@0")

	for i := range 5 {
		claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
		testsupport.Must(t, err, "claim %d: %v", i, err)
		err = e.FailStep(conn, id, claim.Token, "again", "", nowMS)
		testsupport.Must(t, err, "fail %d: %v", i, err)
		step, err := db.GetStep(conn, id)
		testsupport.Must(t, err, "GetStep: %v", err)
		if step.Status != db.StepPending {
			t.Fatalf("status = %q after failure %d with NO declared max_attempts, "+
				"want %q — core ships no default limit",
				step.Status, i+1, db.StepPending)
		}
	}
}

// TestFailRequiresTheToken pins that `fail` passes through the same authorize
// as `complete`, so R1-R4 hold identically.
func TestFailRequiresTheToken(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	id := stepIDByInstance(t, conn, "implement@0")
	_, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	err = e.FailStep(conn, id, "not-the-token", "", "", nowMS)
	if !errors.Is(err, db.ErrNotHolder) {
		t.Errorf("fail with a wrong token = %v, want ErrNotHolder", err)
	}
}

// TestAttemptCountsClaimsNotClaimsPlusFailures is E-8's regression spine — the
// RUN-3 judges' probe, made permanent.
//
// `attempt` COUNTS CLAIMS. claims-leases §5 states the invariant in the table
// itself ("claims made against this issue, ever") and again in prose ("`attempt`
// increments on that claim … every claim ever made is counted"), and
// engine-core's status machine draws the retry edge as `attempt++ < max` taken
// at the RE-CLAIM. `ReapStepTx`'s own comment says the same thing from the other
// side: attempt is "LEFT ALONE" on reap because "incrementing again here would
// double-count a single death".
//
// `FailStep` bumped it a second time, so the counter measured claims PLUS
// failures. The user-visible consequence is what this test pins: with
// max_attempts = 2, the FIRST failure exhausted the budget, so the retry branch
// was unreachable and no step declaring 2 ever got its second try.
func TestAttemptCountsClaimsNotClaimsPlusFailures(t *testing.T) {
	const src = `
[pipeline]
name = "attempt-arithmetic"
version = 1

[match]
kind = ["task"]

[[step]]
name = "twice"
after = []
executor = "w"
emits = "out"
max_attempts = 2
on_fail = "waiting-human"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "attempt-arithmetic.toml")
	issue := createIssue(t, conn, "two tries", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()
	id := stepIDByInstance(t, conn, "twice@0")

	// FIRST attempt: claim (attempt 1 of 2), then fail.
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "first claim: %v", err)
	step, err := db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after first claim: %v", err)
	if step.Attempt != 1 {
		t.Fatalf("attempt = %d after ONE claim, want 1 — the claim is what "+
			"counts an attempt (claims-leases §5)", step.Attempt)
	}

	err = e.FailStep(conn, id, claim.Token, "first try failed", "", nowMS)
	testsupport.Must(t, err, "first fail: %v", err)
	step, err = db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after first fail: %v", err)

	// THE DEFECT, pinned twice over. `fail` must not advance the counter: one
	// claim has happened, so attempt is 1 — not 2.
	if step.Attempt != 1 {
		t.Errorf("attempt = %d after one claim + one failure, want 1 — `fail` "+
			"must not bump a counter the claim already advanced (E-8)",
			step.Attempt)
	}
	// And the consequence that ended RUN-3: with a budget of 2 and one attempt
	// spent, the step is BELOW its limit and must return to the pool.
	if step.Status != db.StepPending {
		t.Fatalf("status = %q at attempt %d of max_attempts = 2, want %q — the "+
			"retry branch is unreachable when `fail` double-counts (E-8)",
			step.Status, step.Attempt, db.StepPending)
	}

	// SECOND attempt: the retry actually happens — this is the branch E-8 made
	// unreachable, and re-claiming is what spends the budget.
	claim2, err := ClaimStep(conn, id, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "re-claim (the retry E-8 made unreachable): %v", err)
	step, err = db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after re-claim: %v", err)
	if step.Attempt != 2 {
		t.Fatalf("attempt = %d after two claims, want 2", step.Attempt)
	}

	// NOW the budget is spent. The second failure exhausts and routes.
	err = e.FailStep(conn, id, claim2.Token, "second try failed", "", nowMS)
	testsupport.Must(t, err, "second fail: %v", err)
	step, err = db.GetStep(conn, id)
	testsupport.Must(t, err, "GetStep after second fail: %v", err)
	if step.Attempt != 2 {
		t.Errorf("attempt = %d after two claims + two failures, want 2 — "+
			"failures never count (E-8)", step.Attempt)
	}
	if step.Status != db.StepWaitingHuman {
		t.Errorf("status = %q after exhausting max_attempts = 2, want %q — "+
			"N attempts means N claims, so exhaustion follows the Nth failure",
			step.Status, db.StepWaitingHuman)
	}
	if step.Owner != "" {
		t.Errorf("the lease survived exhaustion: owner = %q", step.Owner)
	}
	_ = run
}
