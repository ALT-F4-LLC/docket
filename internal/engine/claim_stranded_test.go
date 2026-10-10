package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// DKT-1564 — a claim that commits its lease must never end without handing the
// token over.
//
// RUN-90 stranded two verify steps: the claim transaction committed, the
// `step-claimed` event landed, and the process then exited 1 with no token
// anywhere. Every recovery verb needs that token, so the only exits were the
// full TTL and an operator-gated reap — one ack-reap panel per occurrence.

// failingGateRunner is the post-commit failure injected at the one seam the
// incident implicates: phase 2 runs after transaction A committed, and
// runPreGates propagates a runner error straight out of the claim.
type failingGateRunner struct{ err error }

func (f failingGateRunner) Run(context.Context, GateSpec, StepContext) (GateResult, error) {
	return GateResult{}, f.err
}

// TestPreGateFailureAfterCommitStillDeliversTheToken is AC1: a pre-gate phase
// that fails AFTER the lease committed returns the token with the failure
// rather than instead of it.
//
// The step is legitimately claimed — the CAS was won, the accrual event is
// written — so the honest outcome is the claim plus the bad news, not a bare
// refusal that leaves a live lease nobody can end.
func TestPreGateFailureAfterCommitStillDeliversTheToken(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	e := testEngine()
	stepID := advanceToVerify(t, conn, e)

	e.Gates = failingGateRunner{err: errors.New("database is locked (5) (SQLITE_BUSY)")}

	_, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS,
	})
	if err == nil {
		t.Fatal("a failing pre-gate phase reported success; the failure must still surface")
	}

	var incomplete *IncompleteClaimError
	if !errors.As(err, &incomplete) {
		t.Fatalf("err = %v (%T), want an *IncompleteClaimError carrying the token "+
			"— without it the caller holds a committed lease it cannot end", err, err)
	}
	if !strings.Contains(incomplete.Error(), "SQLITE_BUSY") {
		t.Errorf("err = %q, want the underlying pre-gate failure preserved",
			incomplete.Error())
	}
	if incomplete.Result == nil || incomplete.Result.Token == "" {
		t.Fatalf("the incomplete claim carries no token: %+v", incomplete.Result)
	}

	// The decisive assertion: the token is LIVE against the committed lease,
	// not merely a non-empty string. This is what `step reap`, `step fail` and
	// `step complete` all require.
	if err := db.AuthorizeStepRead(
		conn, stepID, incomplete.Result.Token, nowMS); err != nil {
		t.Errorf("the carried token does not authorize the lease it was minted "+
			"for: %v", err)
	}

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepClaimed {
		t.Errorf("status = %q, want %q — transaction A committed and the claim stands",
			step.Status, db.StepClaimed)
	}
	if step.Owner != "wave:STEP-1" {
		t.Errorf("owner = %q, want the claimant that committed", step.Owner)
	}
}

// TestSameOwnerReclaimReMintsTheStrandedToken is AC2: the executor whose own
// claim stranded re-claims and is handed a working token, instead of waiting
// out the lease or costing a reap panel.
//
// The owner string is per-step (`wave:STEP-N`), so a live lease held under the
// caller's own owner is the caller's own stranded claim and nobody else's.
func TestSameOwnerReclaimReMintsTheStrandedToken(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	first, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS,
	})
	testsupport.Must(t, err, "the first claim: %v", err)

	// The same owner, well inside the lease window — the retry a stranded
	// executor makes.
	second, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS + 1000,
	})
	testsupport.Must(t, err,
		"a same-owner re-claim inside the lease window was refused: %v", err)
	if second.Token == "" {
		t.Fatal("the re-claim returned no token")
	}
	if err := db.AuthorizeStepRead(conn, stepID, second.Token, nowMS+1000); err != nil {
		t.Errorf("the re-minted token does not authorize the lease: %v", err)
	}

	// The re-mint REPLACES the previous token: one lease, one live capability.
	if db.AuthorizeStepRead(conn, stepID, first.Token, nowMS+1000) == nil {
		t.Error("the stranded claim's token still authorizes; a re-mint that leaves " +
			"two live tokens on one lease is two claimants by another name")
	}

	// It is NOT a new claim: no attempt spent, no second accrual event.
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Attempt != first.Attempt {
		t.Errorf("attempt = %d after the re-mint, want %d — a re-mint of the "+
			"caller's own lease is not a retry", step.Attempt, first.Attempt)
	}
	if second.Attempt != first.Attempt {
		t.Errorf("the re-mint reported attempt %d, want %d",
			second.Attempt, first.Attempt)
	}
	var claimed int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind = ? AND step_id = ?`,
		EventStepClaimed, stepID).Scan(&claimed)
	testsupport.Must(t, err, "counting step-claimed events: %v", err)
	if claimed != 1 {
		t.Errorf("%d step-claimed events for one claim, want 1 — the budget floor "+
			"sums these, so a second one bills the run twice", claimed)
	}
}

// TestSameOwnerClaimantsShareOneLease pins what mutual exclusion means once a
// same-owner re-claim re-mints (DKT-1564).
//
// "Exactly one success response" no longer holds for callers presenting the
// SAME owner string: they are, by the re-mint's premise, one claimant asking
// twice. What still holds — and is what mutual exclusion is actually protecting
// — is that the step carries ONE live capability, ONE attempt, and ONE accrual
// however many of them ask.
func TestSameOwnerClaimantsShareOneLease(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	const claimants = 6
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		tokens []string
	)
	for range claimants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := ClaimStep(conn, stepID, ClaimOptions{
				Owner: "wave:STEP-1", NowMS: nowMS,
			})
			if err != nil {
				return
			}
			mu.Lock()
			tokens = append(tokens, result.Token)
			mu.Unlock()
		}()
	}
	wg.Wait()

	live := 0
	for _, token := range tokens {
		if db.AuthorizeStepRead(conn, stepID, token, nowMS) == nil {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d of %d issued tokens authorize the lease, want exactly 1 — "+
			"one lease carries one live capability", live, len(tokens))
	}

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Attempt != 1 {
		t.Errorf("attempt = %d, want 1 — only the CAS winner took a claim", step.Attempt)
	}

	var claimed int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind = ? AND step_id = ?`,
		EventStepClaimed, stepID).Scan(&claimed)
	testsupport.Must(t, err, "counting step-claimed events: %v", err)
	if claimed != 1 {
		t.Errorf("%d step-claimed events, want 1 — the budget floor sums these", claimed)
	}
}

// TestReclaimByAnotherOwnerIsStillRefused keeps the mutual-exclusion guarantee
// the re-mint sits next to: a DIFFERENT owner is refused exactly as before.
func TestReclaimByAnotherOwnerIsStillRefused(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	stepID := stepIDByInstance(t, conn, "implement@0")

	_, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "wave:STEP-1", NowMS: nowMS})
	testsupport.Must(t, err, "the first claim: %v", err)

	_, err = ClaimStep(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-2", NowMS: nowMS + 1000,
	})
	if err == nil {
		t.Fatal("a second owner claimed a live lease; exactly one claimant wins")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("code = %q, want %q", code, CodeConflict)
	}
}

// TestRenderedReclaimReMintsForItsOwnOwner runs AC2 through `claim --render`,
// which is the form the wave actually issues and the form both RUN-90 steps
// died on.
//
// It is a distinct case from the plain claim: ClaimStepRendered validates the
// packet BEFORE taking the lease (DKT-804), so on a retry that preflight runs
// against a step that is already `claimed`. A refusal there would put the
// re-mint out of reach on the one invocation that needs it.
func TestRenderedReclaimReMintsForItsOwnOwner(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml",
		autoWorkflowSrc+"packet = [\"contracts/w.md\"]\n")
	writeConfigFile(t, configDir, "contracts/w.md", "the declared contract\n")

	issue := createIssue(t, conn, "the stranded retry", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")
	opts := ClaimOptions{Owner: "wave:STEP-1", NowMS: nowMS}

	first, _, err := NewEngine().ClaimStepRendered(conn, stepID, opts, "", "")
	testsupport.Must(t, err, "the first rendered claim: %v", err)

	// The retry, with the token from the first call never captured.
	opts.NowMS = nowMS + 1000
	second, packet, err := NewEngine().ClaimStepRendered(conn, stepID, opts, "", "")
	testsupport.Must(t, err,
		"the stranded executor's rendered re-claim was refused: %v", err)
	if !second.ReMinted {
		t.Error("the rendered re-claim reported a fresh claim, not a re-mint")
	}
	if err := db.AuthorizeStepRead(conn, stepID, second.Token, nowMS+1000); err != nil {
		t.Errorf("the re-minted token does not authorize the lease: %v", err)
	}
	if db.AuthorizeStepRead(conn, stepID, first.Token, nowMS+1000) == nil {
		t.Error("the stranded token still authorizes after the re-mint")
	}
	// The packet is the point of --render: a re-mint that returned no packet
	// would hand the executor a token and no contract to execute against.
	if packet == nil || !strings.Contains(packet.Packet, "the declared contract") {
		t.Errorf("the re-mint returned no usable packet: %+v", packet)
	}
}

// TestReclaimAfterTheTokenRetiresIsRefused is the re-mint's boundary: the
// branch keys on a LIVE lease, and a token that retired at `step complete` is
// not one.
//
// Retirement clears owner, hash and expiry together (clearLeaseTx), so the
// guard holds through the owner check alone; this pins the BEHAVIOR rather
// than that mechanism, so a future change to either cannot quietly re-key a
// dead lease.
func TestReclaimAfterTheTokenRetiresIsRefused(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	e := testEngine()
	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	stepID := stepIDByInstance(t, conn, "implement@0")

	_, err := ClaimStep(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS,
	})
	if err == nil {
		t.Fatal("a recorded step was re-claimed; its token retired at completion")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("code = %q, want %q — err = %q", code, CodeConflict, err.Error())
	}
}

// parkedGateRunner is a trusted runner whose every execution parks until the
// test releases it, so a claim can be held inside its pre-gate phase: the
// lease is committed and no pre-gate row is recorded yet.
type parkedGateRunner struct {
	*ExecRunner
	entered chan struct{}
	release chan struct{}
}

func (p parkedGateRunner) Execute(
	ctx context.Context, g GateSpec, sc StepContext,
) (GateExecution, error) {
	p.entered <- struct{}{}
	<-p.release
	return p.ExecRunner.Execute(ctx, g, sc)
}

// TestSameOwnerReclaimDuringPreGatesIsRefusedUntilRowsRecord pins the boundary
// between a stranded claim and one whose pre-gate phase is still running.
//
// While the phase runs, the same owner's re-claim must not re-mint: the new
// token would replace the hash the original claim's lease refresh is keyed
// on, and the re-minted bundle would carry no gate results because none are
// recorded yet. Once the rows are recorded, the re-claim re-mints and the
// bundle replays them.
func TestSameOwnerReclaimDuringPreGatesIsRefusedUntilRowsRecord(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(selfGateResultsWorkflow), "selfgateresults.toml")
	issue := createIssue(t, conn, "a re-claim during pre-gates", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	repoRoot := t.TempDir()
	argv := []string{"/usr/bin/true"}
	inner := NewExecRunner(testRepoPaths(repoRoot))
	inner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	runner := parkedGateRunner{
		ExecRunner: inner,
		entered:    make(chan struct{}, 1),
		release:    make(chan struct{}),
	}
	released := false
	releaseGate := func() {
		if !released {
			released = true
			close(runner.release)
		}
	}
	t.Cleanup(releaseGate)

	e := testEngine()
	e.Gates = runner
	stepID := stepIDByInstance(t, conn, "verify@0")
	const owner = "wave:STEP-1"

	type claimOutcome struct {
		result *ClaimResult
		err    error
	}
	original := make(chan claimOutcome, 1)
	go func() {
		result, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
			Owner: owner, NowMS: nowMS,
		})
		original <- claimOutcome{result, err}
	}()
	<-runner.entered

	before, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	_, err = e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: owner, NowMS: nowMS + 1000,
	})
	if err == nil {
		t.Fatal("a same-owner re-claim re-minted while the pre-gate phase was running")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("code = %q, want %q — err = %q", code, CodeConflict, err.Error())
	}
	if !strings.Contains(err.Error(), "pre-gate phase") {
		t.Errorf("err = %q, want it to name the in-flight pre-gate phase", err.Error())
	}

	after, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if after.TokenHash != before.TokenHash {
		t.Error("the refused re-claim re-keyed the lease the running claim holds")
	}

	releaseGate()
	first := <-original
	testsupport.Must(t, first.err,
		"the original claim failed after its pre-gate phase finished: %v", first.err)
	if err := db.AuthorizeStepRead(conn, stepID, first.result.Token, nowMS+1000); err != nil {
		t.Errorf("the original claim's token does not authorize its lease: %v", err)
	}
	assertOwnPreGateInput(t, "original claim bundle", first.result.Context)

	rows, err := db.GateResultsForStep(conn, stepID)
	testsupport.Must(t, err, "GateResultsForStep: %v", err)
	if len(rows) != 1 || !rows[0].Pre || rows[0].Gate != "ac-commands" {
		t.Errorf("recorded gate rows = %+v, want the one pre-gate row", rows)
	}

	reMinted, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: owner, NowMS: nowMS + 2000,
	})
	testsupport.Must(t, err,
		"a same-owner re-claim after the pre-gate rows recorded was refused: %v", err)
	if !reMinted.ReMinted {
		t.Error("the re-claim after the pre-gate phase was not a re-mint")
	}
	if reMinted.Attempt != first.result.Attempt {
		t.Errorf("the re-mint reported attempt %d, want %d",
			reMinted.Attempt, first.result.Attempt)
	}
	if len(reMinted.Context.PreGates) == 0 {
		t.Error("the re-minted bundle carries no pre-gate results")
	}
	assertOwnPreGateInput(t, "re-minted bundle", reMinted.Context)
}

// TestSameOwnerReclaimBeforeTransactionBCommitsIsRefused pins the window
// between the last pre-gate row committing and transaction B committing.
//
// Every declared pre-gate has a row there, so a row count cannot tell the
// window from a settled claim. A re-mint inside it would replace the token
// hash transaction B's lease refresh is keyed on, and the original claim
// would fail as lost. The re-claim must be a retryable CONFLICT that leaves
// the lease alone; once transaction B commits, the same re-claim re-mints.
func TestSameOwnerReclaimBeforeTransactionBCommitsIsRefused(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(selfGateResultsWorkflow), "selfgateresults.toml")
	issue := createIssue(t, conn, "a re-claim before transaction B", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	repoRoot := t.TempDir()
	argv := []string{"/usr/bin/true"}
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	e := testEngine()
	e.Gates = runner
	stepID := stepIDByInstance(t, conn, "verify@0")
	const owner = "wave:STEP-1"

	var (
		inWindowErr error
		hookRan     bool
	)
	claimHookAfterPreGates = func(id int) {
		claimHookAfterPreGates = nil
		hookRan = true

		before, err := db.GetStep(conn, id)
		testsupport.Must(t, err, "GetStep: %v", err)
		rows, err := db.GateResultsForStep(conn, id)
		testsupport.Must(t, err, "GateResultsForStep: %v", err)
		if len(rows) != 1 || !rows[0].Pre {
			t.Errorf("rows inside the window = %+v, want the one recorded pre-gate row", rows)
		}

		_, inWindowErr = e.ClaimStepWithGates(conn, id, ClaimOptions{
			Owner: owner, NowMS: nowMS + 1000,
		})

		after, err := db.GetStep(conn, id)
		testsupport.Must(t, err, "GetStep: %v", err)
		if after.TokenHash != before.TokenHash {
			t.Error("the in-window re-claim re-keyed the lease transaction B refreshes")
		}
	}
	t.Cleanup(func() { claimHookAfterPreGates = nil })

	first, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: owner, NowMS: nowMS,
	})
	if !hookRan {
		t.Fatal("the claim never reached the window after its pre-gates")
	}
	if inWindowErr == nil {
		t.Error("a same-owner re-claim re-minted before transaction B committed")
	} else if code, _ := CodeOf(inWindowErr); code != CodeConflict {
		t.Errorf("in-window code = %q, want %q — err = %q",
			code, CodeConflict, inWindowErr.Error())
	}
	testsupport.Must(t, err, "the original claim failed after the in-window re-claim: %v", err)
	if err := db.AuthorizeStepRead(conn, stepID, first.Token, nowMS+1000); err != nil {
		t.Errorf("the original claim's token does not authorize its lease: %v", err)
	}
	if len(first.Context.PreGates) == 0 {
		t.Error("the original claim's bundle carries no pre-gate results")
	}
	assertOwnPreGateInput(t, "original claim bundle", first.Context)

	reMinted, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: owner, NowMS: nowMS + 2000,
	})
	testsupport.Must(t, err,
		"a same-owner re-claim after transaction B committed was refused: %v", err)
	if !reMinted.ReMinted {
		t.Error("the re-claim after transaction B was not a re-mint")
	}
	if reMinted.Attempt != first.Attempt {
		t.Errorf("the re-mint reported attempt %d, want %d", reMinted.Attempt, first.Attempt)
	}
	if len(reMinted.Context.PreGates) == 0 {
		t.Error("the re-minted bundle carries no pre-gate results")
	}
	assertOwnPreGateInput(t, "re-minted bundle", reMinted.Context)
}

// TestUnsettledClaimDoesNotBlockAClaimAfterItsLeaseExpires pins that the
// pre-gate phase marker belongs to one lease. A claim that fails before
// transaction B commits leaves the marker behind; once its lease expires, a
// fresh claim reaps it and succeeds, and that claim settles normally.
func TestUnsettledClaimDoesNotBlockAClaimAfterItsLeaseExpires(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	e := testEngine()
	stepID := advanceToVerify(t, conn, e)
	const owner = "wave:STEP-1"
	const ttlMS = 1000

	e.Gates = failingGateRunner{err: errors.New("the pre-gate runner failed")}
	_, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: owner, TTLOverride: ttlMS, NowMS: nowMS,
	})
	var incomplete *IncompleteClaimError
	if !errors.As(err, &incomplete) {
		t.Fatalf("err = %v (%T), want the claim to fail before transaction B", err, err)
	}

	e.Gates = PassThroughRunner{}
	later := nowMS + ttlMS + 1
	fresh, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: owner, NowMS: later,
	})
	testsupport.Must(t, err,
		"a claim after the unsettled lease expired was refused: %v", err)
	if fresh.ReMinted {
		t.Error("the claim after expiry re-minted the expired lease instead of taking a new one")
	}
	if fresh.Attempt != incomplete.Result.Attempt+1 {
		t.Errorf("attempt = %d, want %d", fresh.Attempt, incomplete.Result.Attempt+1)
	}

	again, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
		Owner: owner, NowMS: later + 1,
	})
	testsupport.Must(t, err,
		"a same-owner re-claim of the settled fresh claim was refused: %v", err)
	if !again.ReMinted {
		t.Error("the re-claim of the settled fresh claim was not a re-mint")
	}
}
