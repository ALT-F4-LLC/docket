package engine

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// A pre-gate longer than the claim's budget is measured AHEAD of the claim
// and served to it (TDD docs/tdd/gates-trust.md §7.6.2 PG6).
//
// PG5's 60s budget keeps `docket step claim` inside the executor's tool
// timeout, and it cut the ac-commands pre-gate — the full test suite, about
// three minutes — short on every verify step, so the gate-results the verify
// packet carried never held the evidence the step was there to judge. The
// mechanism: the moment a writer's completion records the round record whose
// `head` is the verify step's target, the engine launches a detached run of
// that step's pre-gates against that sha, bounded by the entry's own timeout;
// the claim finds the complete result keyed to the same step and target and
// serves it instead of running the gate inside the budget. The budget itself
// is unchanged, and so is the claim's bound.

// withDetachedPreGateLockDir points the in-flight locks at a temp directory
// for one test, so no test touches the operator's store.
func withDetachedPreGateLockDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := detachedPreGateLockDir
	detachedPreGateLockDir = func() string { return dir }
	t.Cleanup(func() { detachedPreGateLockDir = prev })
	return dir
}

// installDetachedLauncher sets the launcher for one test and restores it.
func installDetachedLauncher(t *testing.T, launch func(stepID int, targetSHA string) error) {
	t.Helper()
	prev := LaunchDetachedPreGates
	LaunchDetachedPreGates = launch
	t.Cleanup(func() { LaunchDetachedPreGates = prev })
}

// advanceToVerifyWithCommit is advanceToVerify with implement@0 COMMITTING
// work in the run's checkout between its claim and its completion, so its
// round record names a head and the verify step's pre-gates resolve a target
// sha. A writer that commits nothing records no head (appendRoundDelta), and
// a verify step with no sha measures a tree rather than a key.
//
// It returns verify@0's id and the head implement@0 recorded.
func advanceToVerifyWithCommit(
	t *testing.T, conn *sql.DB, e *Engine, repoRoot string,
) (stepID int, head string) {
	t.Helper()

	pass := testEngine() // pass-through completion gates
	pass.DiffFn = e.DiffFn

	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement@0: %v", err)

	err = os.WriteFile(filepath.Join(repoRoot, "work.txt"), []byte("the change\n"), 0o644)
	testsupport.Must(t, err, "writing the work: %v", err)
	gitRun(t, repoRoot, "add", "work.txt")
	gitRun(t, repoRoot, "commit", "-q", "-m", "the work under review")
	head = gitRun(t, repoRoot, "rev-parse", "HEAD")

	err = pass.CompleteStep(conn, implementID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement@0: %v", err)

	for i := range 4 {
		claimAndComplete(t, conn, pass, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, pass, "synthesize@0", "synthesized", "")
	driveAction(t, conn, pass, "reconcile@0")

	return stepIDByInstance(t, conn, "verify@0"), head
}

// preGateRows returns a step's recorded pre-gate rows for one gate, in
// recording order.
func preGateRows(t *testing.T, conn *sql.DB, stepID int, gate string) []db.GateResultRow {
	t.Helper()
	rows, err := db.GateResultsForStep(conn, stepID)
	testsupport.Must(t, err, "GateResultsForStep: %v", err)
	var out []db.GateResultRow
	for _, r := range rows {
		if r.Pre && r.Gate == gate {
			out = append(out, r)
		}
	}
	return out
}

// TestDetachedPreGateResultServesTheClaim is the acceptance case, driven
// through the real seam: implement@0's completion records the head, the hook
// at the saga's close launches the detached run (synchronous here, through the
// injected launcher), and verify@0's claim — under a budget far shorter than
// the gate — receives the gate's COMPLETE result: exit 0, pass, no budget cut,
// and returns inside the bound TestClaimReturnsWithinThePreGateBudget allows.
//
// Two mutants go red here. Running the gate inside the claim's budget as
// before records the cut-off (fail, a reason naming the budget). Launching at
// every later completion rather than once records the measurement twice.
func TestDetachedPreGateResultServesTheClaim(t *testing.T) {
	shrinkClaimBudget(t, 500*time.Millisecond)
	withDetachedPreGateLockDir(t)

	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)

	// Past the budget, under the entry's default 5m timeout, and exits 0.
	argv := []string{"/bin/sleep", "1"}
	e := execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})

	// The launcher runs the detached phase in-process and synchronously, so
	// "the run finished before the claim" is a fact of the fixture rather than
	// a race; what it is handed is what the production launcher re-executes.
	var launched []string
	installDetachedLauncher(t, func(stepID int, targetSHA string) error {
		launched = append(launched, strconv.Itoa(stepID)+"@"+targetSHA)
		_, err := e.RunDetachedPreGates(conn, stepID, targetSHA, nowMS)
		return err
	})

	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)

	// LAUNCHED ONCE, for verify@0 at the head implement@0 recorded: the
	// writer's completion is the hook point, and the six completions after it
	// found the result complete and launched nothing.
	want := strconv.Itoa(stepID) + "@" + head
	if len(launched) != 1 || launched[0] != want {
		t.Fatalf("detached launches = %v, want exactly [%s]", launched, want)
	}

	start := time.Now()
	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	elapsed := time.Since(start)
	testsupport.Must(t, err, "claim: %v", err)

	// THE CLAIM'S OWN BOUND HOLDS: nothing ran inside it.
	if elapsed > 5*time.Second {
		t.Errorf("the claim took %s against a %s pre-gate budget", elapsed, claimPreGateBudget)
	}
	if claim.Token == "" {
		t.Fatal("the claim returned no token")
	}

	// THE COMPLETE RESULT RIDES IN THE BUNDLE: exit 0, pass, measured against
	// the target, with no reason naming the budget that could not hold it.
	if len(claim.Context.PreGates) != 1 {
		t.Fatalf("pre-gate results = %+v, want one", claim.Context.PreGates)
	}
	row := claim.Context.PreGates[0]
	if row.Exit == nil || *row.Exit != 0 {
		t.Errorf("exit = %v, want 0", row.Exit)
	}
	if row.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want %q (reason: %s)", row.Verdict, VerdictPass, row.Reason)
	}
	if strings.Contains(row.Reason, "budget") {
		t.Errorf("the served result names the claim budget, so it was cut by it: %q", row.Reason)
	}
	if !strings.Contains(row.Reason, head[:12]) {
		t.Errorf("the reason does not name the target it measured (%.12s): %q", head, row.Reason)
	}

	// THE LEDGER HOLDS ONE MEASUREMENT, keyed to the target: the claim served
	// it rather than recording a second, budget-cut row beside it.
	rows := preGateRows(t, conn, stepID, "ac-commands")
	if len(rows) != 1 {
		t.Fatalf("recorded %d ac-commands pre-gate rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].TargetSHA != head {
		t.Errorf("target_sha = %q, want %s", rows[0].TargetSHA, head)
	}
}

// TestDetachedResultForAnotherTargetIsNotServed is the key's other half: a
// complete result recorded against a DIFFERENT sha is not this step's
// evidence, and the claim measures for itself exactly as before.
func TestDetachedResultForAnotherTargetIsNotServed(t *testing.T) {
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)
	argv, sentinel := witnessCommand(t, repoRoot, "claim-measured")
	e := execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})

	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	// A complete, passing result — for a sha this step never resolved.
	other := strings.Repeat("0", 39) + "1"
	zero := 0
	err = recordPreGateRows(conn, step, "ac-commands", []GateResultRow{{
		Gate: "ac-commands", Argv: argv, Exit: &zero, Verdict: VerdictPass,
		Pre: true, TargetSHA: other,
	}}, nowMS)
	testsupport.Must(t, err, "recording the foreign result: %v", err)

	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	if !sentinelExists(t, sentinel) {
		t.Error("the claim served a result measured against another target instead of measuring")
	}
	if len(claim.Context.PreGates) != 1 {
		t.Fatalf("pre-gate results = %+v, want one", claim.Context.PreGates)
	}
	if got := claim.Context.PreGates[0].Reason; !strings.Contains(got, head[:12]) ||
		strings.Contains(got, "detached") {
		t.Errorf("the bundle does not carry the claim's own measurement of %.12s: %q", head, got)
	}
	if rows := preGateRows(t, conn, stepID, "ac-commands"); len(rows) != 2 {
		t.Errorf("recorded %d rows, want the foreign result and the claim's own: %+v", len(rows), rows)
	}
}

// TestDetachedRunDeclinesWhileAnotherHoldsTheLock: one run per (step, target)
// at a time. The lock is held by a process this test can kill, so what the
// second run observes is a lock another process holds, not an fd of its own.
func TestDetachedRunDeclinesWhileAnotherHoldsTheLock(t *testing.T) {
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)
	argv, sentinel := witnessCommand(t, repoRoot, "second-run-measured")
	e := execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)

	lockPath := detachedPreGateLockPath(stepID, head)
	lock, held, err := holdDetachedPreGateLock(lockPath)
	testsupport.Must(t, err, "taking the lock: %v", err)
	if held {
		t.Fatal("the lock was held before anything took it")
	}
	holder := exec.Command("/bin/sleep", "60")
	holder.ExtraFiles = []*os.File{lock}
	err = holder.Start()
	testsupport.Must(t, err, "starting the lock holder: %v", err)
	lock.Close()
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })

	if !detachedPreGateInFlight(lockPath) {
		t.Error("the launcher's probe does not see the run in flight")
	}
	out, err := e.RunDetachedPreGates(conn, stepID, head, nowMS)
	testsupport.Must(t, err, "RunDetachedPreGates: %v", err)
	if out.Outcome != DetachedPreGateRunning {
		t.Errorf("outcome = %q, want %q", out.Outcome, DetachedPreGateRunning)
	}
	if sentinelExists(t, sentinel) {
		t.Error("a second run measured while the first held the lock")
	}
	if rows := preGateRows(t, conn, stepID, "ac-commands"); len(rows) != 0 {
		t.Errorf("a declined run recorded rows: %+v", rows)
	}
}

// TestDetachedRunMeasuresOnlyTheStepsCurrentTarget: a run asked for a sha the
// step no longer resolves records nothing — its result could serve no claim.
func TestDetachedRunMeasuresOnlyTheStepsCurrentTarget(t *testing.T) {
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)
	argv, sentinel := witnessCommand(t, repoRoot, "stale-run-measured")
	e := execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	stepID, _ := advanceToVerifyWithCommit(t, conn, e, repoRoot)

	other := strings.Repeat("0", 39) + "1"
	out, err := e.RunDetachedPreGates(conn, stepID, other, nowMS)
	testsupport.Must(t, err, "RunDetachedPreGates: %v", err)
	if out.Outcome != DetachedPreGateStale {
		t.Errorf("outcome = %q, want %q", out.Outcome, DetachedPreGateStale)
	}
	if sentinelExists(t, sentinel) {
		t.Error("the run measured a target the step does not resolve")
	}
	if rows := preGateRows(t, conn, stepID, "ac-commands"); len(rows) != 0 {
		t.Errorf("a stale run recorded rows: %+v", rows)
	}
}

// TestDetachedRunRecordsNothingItCouldNotMeasure: a target that cannot be
// reconstructed is an error and no row — the claim's own phase says what
// there is to say about an unbindable target, in the words it already has.
// It is also the shape a run that dies leaves: rows land only after a gate
// finished, so nothing short of a measurement ever reads as one.
func TestDetachedRunRecordsNothingItCouldNotMeasure(t *testing.T) {
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)
	argv := []string{"/bin/sleep", "0"}
	e := execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)

	// The run's checkout moves to a repository whose object database does not
	// hold the head: the step still resolves it, and nothing can rebuild it.
	elsewhere := t.TempDir()
	gitRun(t, elsewhere, "init", "-q")
	setRunExecRoot(t, conn, run.ID, elsewhere)

	if _, err := e.RunDetachedPreGates(conn, stepID, head, nowMS); err == nil {
		t.Error("a run that could not reconstruct its target reported success")
	}
	if rows := preGateRows(t, conn, stepID, "ac-commands"); len(rows) != 0 {
		t.Errorf("a run that measured nothing recorded rows: %+v", rows)
	}
}

// movingTargetGates is a GateRunner whose FIRST spawn runs `move` before it
// reports a pass: the seam through which a test makes the world change while
// a detached run's gate is "running" — the step's target moves, or the step
// is claimed — so that what the run does with a result nobody asked for any
// more is observable.
type movingTargetGates struct {
	move func()
	once sync.Once
}

func (g *movingTargetGates) Run(_ context.Context, spec GateSpec, _ StepContext) (GateResult, error) {
	g.once.Do(g.move)
	return GateResult{Gate: spec.Name, Exit: 0, Verdict: VerdictPass}, nil
}

// TestLateDetachedResultForAMovedTargetIsNeitherRecordedNorReplayed: a child
// that started measuring target A finishes after the target moved to B and
// after the claim took the step at B. Two things must hold. The child
// RECORDS NOTHING — the check that it is still wanted runs inside the
// recording transaction, not only before the gate — and a re-minted claim's
// replay NEVER RETURNS a row keyed to a target other than the one the claim
// resolved, even when such a row sits at the highest ordinal.
//
// Two mutants go red here. Dropping the record-time guard records A's pass
// after the claim. Dropping the target filter from recordedPreGates replays
// that pass to the re-minted claim for B.
func TestLateDetachedResultForAMovedTargetIsNeitherRecordedNorReplayed(t *testing.T) {
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)

	e := testEngine()
	stepID, headA := advanceToVerifyWithCommit(t, conn, e, repoRoot)
	implementID := stepIDByInstance(t, conn, "implement@0")
	// B is in no object database: the claim at B records `skipped`, which is
	// exactly the row a replay must prefer over A's late pass.
	headB := strings.Repeat("1eb345b3", 5)

	// While the child's gate runs: the target moves to B, and the claim takes
	// the step there.
	owner := ClaimOptions{Owner: "w", NowMS: nowMS}
	var claimed *ClaimResult
	child := testEngine()
	child.Gates = &movingTargetGates{move: func() {
		supersedeIssueDiff(t, conn, run.ID, implementID, headB, "")
		c, err := testEngine().ClaimStepWithGates(conn, stepID, owner)
		testsupport.Must(t, err, "claim at B: %v", err)
		claimed = c
	}}

	out, err := child.RunDetachedPreGates(conn, stepID, headA, nowMS)
	testsupport.Must(t, err, "RunDetachedPreGates: %v", err)
	if out.Outcome == DetachedPreGateMeasured || len(out.Gates) != 0 {
		t.Errorf("the late child reported %q with %d gates; it measured a target "+
			"nobody asks about any more", out.Outcome, len(out.Gates))
	}
	if claimed == nil {
		t.Fatal("the fixture's gate never ran, so nothing moved")
	}

	// THE CHILD RECORDED NOTHING: the ledger holds the claim's own row and no
	// row keyed to A.
	rows := preGateRows(t, conn, stepID, "ac-commands")
	for _, r := range rows {
		if r.TargetSHA == headA {
			t.Errorf("the late child recorded a row for the target that moved: %+v", r)
		}
	}
	if len(rows) != 1 || rows[0].TargetSHA != "" {
		t.Fatalf("recorded rows = %+v, want the claim's own row alone", rows)
	}

	// A late A pass AT THE HIGHEST ORDINAL, as a child without the record
	// guard would have written it.
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	zero := 0
	const marker = "a late pass for the target that moved"
	err = recordPreGateRows(conn, step, "ac-commands", []GateResultRow{{
		Gate: "ac-commands", Exit: &zero, Verdict: VerdictPass, Pre: true,
		TargetSHA: headA, Reason: marker,
	}}, nowMS+1)
	testsupport.Must(t, err, "recording the late pass: %v", err)

	// THE RE-MINT REPLAYS THE CLAIM'S OWN ROW, not A's pass.
	owner.NowMS = nowMS + 1000
	again, err := testEngine().ClaimStepWithGates(conn, stepID, owner)
	testsupport.Must(t, err, "the owner's re-claim: %v", err)
	if !again.ReMinted {
		t.Fatal("the owner's re-claim was not a re-mint")
	}
	if len(again.Context.PreGates) != 1 {
		t.Fatalf("re-minted pre-gates = %+v, want one", again.Context.PreGates)
	}
	got, want := again.Context.PreGates[0], claimed.Context.PreGates[0]
	if strings.Contains(got.Reason, marker) || got.Verdict == VerdictPass {
		t.Errorf("the re-mint replayed A's late pass for a claim at B: %+v", got)
	}
	if got.Verdict != want.Verdict || got.Reason != want.Reason {
		t.Errorf("the re-mint replayed %+v, want the claim's own %+v", got, want)
	}
}

// twoPreGatesWorkflow is the fixture's writer-then-verifier pair with TWO
// pre-gates on the verifier, so a detached run has a gate to die between.
const twoPreGatesWorkflow = `
[pipeline]
name = "twopregates"
version = 1
[[step]]
name = "implement"
executor = "implement"
class = "write"
emits = "change-summary"
[[step]]
name = "verify"
after = ["implement"]
executor = "verify-ac"
emits = "ac-report"
gates = [{ name = "ac-commands", pre = true }, { name = "ac-lint", pre = true }]
inputs = ["issue.diff"]
`

// TestRelaunchedDetachedRunServesOneRowPerGate: a child recorded gate X and
// died before gate Y; the relaunch measures Y ONLY, and the claim's bundle
// carries exactly one row per gate — X's latest complete row, not every row
// X ever recorded and not the stale `unmatched` beside its pass.
//
// Two mutants go red here. A relaunch that re-measures X touches X's
// sentinel. A serve that returns every matched row puts three results in a
// two-gate bundle.
func TestRelaunchedDetachedRunServesOneRowPerGate(t *testing.T) {
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn := mustDB(t)
	registerSource(t, conn, []byte(twoPreGatesWorkflow), "twopregates.toml")
	issue := createIssue(t, conn, "two pre-gates", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)
	argvX, sentinelX := witnessCommand(t, repoRoot, "ac-commands-remeasured")
	argvY, sentinelY := witnessCommand(t, repoRoot, "ac-lint-measured")
	e := execEngineWithTrust(t, repoRoot,
		trust.Entry{Name: "ac-commands", Argv: argvX, ArgvSHA256: trust.ArgvSHA256(argvX),
			Repo: mustResolve(repoRoot)},
		trust.Entry{Name: "ac-lint", Argv: argvY, ArgvSHA256: trust.ArgvSHA256(argvY),
			Repo: mustResolve(repoRoot)},
	)

	// The writer commits, so the verifier resolves a target sha.
	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement@0: %v", err)
	err = os.WriteFile(filepath.Join(repoRoot, "work.txt"), []byte("the change\n"), 0o644)
	testsupport.Must(t, err, "writing the work: %v", err)
	gitRun(t, repoRoot, "add", "work.txt")
	gitRun(t, repoRoot, "commit", "-q", "-m", "the work under review")
	head := gitRun(t, repoRoot, "rev-parse", "HEAD")
	pass := testEngine()
	pass.DiffFn = e.DiffFn
	err = pass.CompleteStep(conn, implementID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement@0: %v", err)

	stepID := stepIDByInstance(t, conn, "verify@0")
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	// THE DEAD CHILD'S LEDGER: for X, a stale `unmatched` row and then a
	// complete pass, both keyed to the target; nothing for Y.
	zero := 0
	err = recordPreGateRows(conn, step, "ac-commands", []GateResultRow{{
		Gate: "ac-commands", Verdict: VerdictUnmatched, Pre: true, TargetSHA: head,
		Reason: "the stale unmatched attempt",
	}}, nowMS)
	testsupport.Must(t, err, "recording the stale row: %v", err)
	err = recordPreGateRows(conn, step, "ac-commands", []GateResultRow{{
		Gate: "ac-commands", Argv: argvX, Exit: &zero, Verdict: VerdictPass, Pre: true,
		TargetSHA: head, Reason: detachedNote(head),
	}}, nowMS)
	testsupport.Must(t, err, "recording the dead child's pass: %v", err)

	// THE RELAUNCH measures Y and only Y.
	out, err := e.RunDetachedPreGates(conn, stepID, head, nowMS)
	testsupport.Must(t, err, "RunDetachedPreGates: %v", err)
	if out.Outcome != DetachedPreGateMeasured {
		t.Errorf("outcome = %q, want %q", out.Outcome, DetachedPreGateMeasured)
	}
	if sentinelExists(t, sentinelX) {
		t.Error("the relaunch re-measured a gate that already had a complete row")
	}
	if !sentinelExists(t, sentinelY) {
		t.Error("the relaunch did not measure the gate the dead child never reached")
	}
	if rows := preGateRows(t, conn, stepID, "ac-commands"); len(rows) != 2 {
		t.Errorf("ac-commands rows = %d, want the dead child's two and no more: %+v", len(rows), rows)
	}
	if rows := preGateRows(t, conn, stepID, "ac-lint"); len(rows) != 1 || rows[0].TargetSHA != head {
		t.Errorf("ac-lint rows = %+v, want one keyed to %.12s", rows, head)
	}

	// THE CLAIM'S BUNDLE: exactly one row per gate, X's being the pass.
	got, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	perGate := make(map[string]int)
	for _, r := range got.Context.PreGates {
		perGate[r.Gate]++
		if r.Gate == "ac-commands" && r.Verdict != VerdictPass {
			t.Errorf("ac-commands served %q, want the latest complete row's pass: %+v", r.Verdict, r)
		}
	}
	if len(got.Context.PreGates) != 2 || perGate["ac-commands"] != 1 || perGate["ac-lint"] != 1 {
		t.Errorf("bundle pre-gates = %+v, want exactly one row per gate", got.Context.PreGates)
	}
	if sentinelExists(t, sentinelX) {
		t.Error("the claim re-measured a gate it was served")
	}
}

// holdLockInAnotherProcess hands a (step, target) in-flight lock to a child
// process that outlives the test body, so what the scheduler probes is a lock
// another process holds — a detached child mid-measurement — never an fd of
// this process's own. Killing the returned process is the kernel's release of
// a child that died: the flock goes, the lockfile stays.
func holdLockInAnotherProcess(t *testing.T, lockPath string) *exec.Cmd {
	t.Helper()
	lock, held, err := holdDetachedPreGateLock(lockPath)
	testsupport.Must(t, err, "taking the lock: %v", err)
	if held {
		t.Fatal("the lock was held before anything took it")
	}
	holder := exec.Command("/bin/sleep", "60")
	holder.ExtraFiles = []*os.File{lock}
	err = holder.Start()
	testsupport.Must(t, err, "starting the lock holder: %v", err)
	lock.Close()
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	return holder
}

// offeredAs reports the status `next` rendered a step instance at, or "" when
// the offer does not carry it at all — ready or staged.
func offeredAs(next *ReadySteps, instance string) string {
	for _, row := range next.Steps {
		if row.Instance == instance {
			return row.Status
		}
	}
	return ""
}

// TestDetachedPreGateInFlightHoldsTheStepUntilItsResultLands is the hold's
// acceptance case (§7.6.2 PG6, the scheduler's half). While another process
// holds verify@0's (step, target) in-flight lock, the step that every other
// clause admits is not ready: Ready names CondPreGatePending, `next` offers
// it neither ready nor staged, and the `step show` surface carries the
// condition as its blocked reason. The hold lifts the way the lock does —
// here the holder is killed, the kernel's release for a child that died with
// its lockfile in place — and once the detached run records its complete row
// the claim, under a budget far shorter than the gate, serves that row with
// no reason naming the budget and returns inside the claim's own bound.
//
// Three mutants go red here. Dropping the hold clause from Ready reports the
// step ready while the lock is held. Reading the lockfile's presence instead
// of its flock holds the step after the holder died. Holding whenever a
// pre-gate is declared never lets the step become ready.
func TestDetachedPreGateInFlightHoldsTheStepUntilItsResultLands(t *testing.T) {
	shrinkClaimBudget(t, 500*time.Millisecond)
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)

	// Past the budget, under the entry's default 5m timeout, and exits 0.
	argv := []string{"/bin/sleep", "1"}
	e := execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)
	holder := holdLockInAnotherProcess(t, detachedPreGateLockPath(stepID, head))

	// HELD: not ready, by name, and absent from the offer altogether.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		step := stepNamed(t, sched, "verify@0")
		if ok, cond := sched.Ready(step); ok || cond != CondPreGatePending {
			t.Errorf("Ready(verify@0) = (%v, %q) while the detached run's lock is held, want (false, %q)",
				ok, cond, CondPreGatePending)
		}
		if got := BlockedReason(sched, step); got != string(CondPreGatePending) {
			t.Errorf("blocked reason = %q while held, want %q", got, CondPreGatePending)
		}
	})
	next, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next while held: %v", err)
	if status := offeredAs(next, "verify@0"); status != "" {
		t.Errorf("next offered verify@0 as %q while the detached run's lock is held", status)
	}

	// THE HOLDER DIES: the kernel drops the flock, the lockfile stays, and the
	// hold lifts with nothing in flight to wait on.
	holder.Process.Kill()
	holder.Wait()
	if _, err := os.Stat(detachedPreGateLockPath(stepID, head)); err != nil {
		t.Fatalf("the dead holder's lockfile is gone, so the free-lock case is not what is tested: %v", err)
	}
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "verify@0")); !ok {
			t.Errorf("verify@0 is held (%q) after the lock's holder died with the lockfile in place", cond)
		}
	})

	// THE RUN RECORDS ITS COMPLETE ROW and releases the lock; the step is
	// ready and offered as such.
	out, err := e.RunDetachedPreGates(conn, stepID, head, nowMS)
	testsupport.Must(t, err, "RunDetachedPreGates: %v", err)
	if out.Outcome != DetachedPreGateMeasured {
		t.Fatalf("outcome = %q, want %q", out.Outcome, DetachedPreGateMeasured)
	}
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "verify@0")); !ok {
			t.Errorf("verify@0 is held (%q) after the detached run recorded and released", cond)
		}
	})
	next, err = e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next after the run: %v", err)
	if status := offeredAs(next, "verify@0"); status != db.StepReady {
		t.Errorf("next offers verify@0 as %q after the detached run recorded, want %q", status, db.StepReady)
	}

	// THE CLAIM SERVES THE ROW, inside its own bound: exit 0, pass, measured
	// against the target, with no reason naming the budget.
	start := time.Now()
	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	elapsed := time.Since(start)
	testsupport.Must(t, err, "claim: %v", err)
	if elapsed > 5*time.Second {
		t.Errorf("the claim took %s against a %s pre-gate budget", elapsed, claimPreGateBudget)
	}
	if claim.Token == "" {
		t.Fatal("the claim returned no token")
	}
	if len(claim.Context.PreGates) != 1 {
		t.Fatalf("pre-gate results = %+v, want one", claim.Context.PreGates)
	}
	row := claim.Context.PreGates[0]
	if row.Exit == nil || *row.Exit != 0 {
		t.Errorf("exit = %v, want 0", row.Exit)
	}
	if row.Verdict != VerdictPass {
		t.Errorf("verdict = %q, want %q (reason: %s)", row.Verdict, VerdictPass, row.Reason)
	}
	if strings.Contains(row.Reason, "budget") {
		t.Errorf("the served result names the claim budget, so it was cut by it: %q", row.Reason)
	}
	if !strings.Contains(row.Reason, head[:12]) {
		t.Errorf("the reason does not name the target it measured (%.12s): %q", head, row.Reason)
	}
	rows := preGateRows(t, conn, stepID, "ac-commands")
	if len(rows) != 1 || rows[0].TargetSHA != head {
		t.Fatalf("recorded rows = %+v, want the detached run's one row keyed to %.12s", rows, head)
	}
}

// TestNoDetachedLockLeavesReadinessAndTheClaimUnchanged is the hold's other
// half: a step whose pre-gates resolve a target but whose detached run never
// launched — no lockfile exists — is ready exactly as before, and its claim
// runs the gate on PG5's path and records the budget cut, the row
// TestClaimReturnsWithinThePreGateBudget expects of a claim with no detached
// result to serve.
//
// The mutant that holds whenever a pre-gate is declared, regardless of the
// lock, reports the step blocked here.
func TestNoDetachedLockLeavesReadinessAndTheClaimUnchanged(t *testing.T) {
	shrinkClaimBudget(t, 500*time.Millisecond)
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)

	// Far longer than the budget, under the entry's default 5m timeout.
	argv := []string{"/bin/sleep", "30"}
	e := execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)
	if _, err := os.Stat(detachedPreGateLockPath(stepID, head)); !os.IsNotExist(err) {
		t.Fatalf("a lockfile exists before any run was launched: %v", err)
	}

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		step := stepNamed(t, sched, "verify@0")
		if ok, cond := sched.Ready(step); !ok {
			t.Errorf("verify@0 is held (%q) with no detached run in flight", cond)
		}
		if got := BlockedReason(sched, step); got != "" {
			t.Errorf("blocked reason = %q with no detached run in flight, want none", got)
		}
	})

	start := time.Now()
	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	elapsed := time.Since(start)
	testsupport.Must(t, err, "claim: %v", err)
	if elapsed > 5*time.Second {
		t.Errorf("the claim took %s against a %s pre-gate budget", elapsed, claimPreGateBudget)
	}
	if claim.Token == "" {
		t.Fatal("the claim returned no token")
	}
	if len(claim.Context.PreGates) != 1 {
		t.Fatalf("pre-gate results = %+v, want one", claim.Context.PreGates)
	}
	row := claim.Context.PreGates[0]
	if row.Verdict != VerdictFail || !strings.Contains(row.Reason, "pre-gate budget") {
		t.Errorf("row = %+v, want the budgeted path's cut-off fail naming the budget", row)
	}
	if rows := preGateRows(t, conn, stepID, "ac-commands"); len(rows) != 1 || rows[0].TargetSHA != "" {
		t.Errorf("recorded rows = %+v, want the claim's own single row, keyed to no target", rows)
	}
}

// TestClaimWhileDetachedRunInFlightIsRefusedWithoutWaiting: the claim
// re-checks readiness itself (R8), so a claim that arrives while the lock is
// held is refused — naming the condition, inside the claim's bound, with
// nothing spawned, nothing recorded, and the step still pending — rather than
// waiting on the lock or running the gate on the budgeted path.
//
// The mutant that waits on the lock inside the claim instead of holding the
// step keeps this claim for the holder's whole lifetime.
func TestClaimWhileDetachedRunInFlightIsRefusedWithoutWaiting(t *testing.T) {
	shrinkClaimBudget(t, 500*time.Millisecond)
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)
	argv, sentinel := witnessCommand(t, repoRoot, "claimed-while-held")
	e := execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)
	holdLockInAnotherProcess(t, detachedPreGateLockPath(stepID, head))

	start := time.Now()
	_, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("the claim succeeded while the detached run's lock was held")
	}
	if !strings.Contains(err.Error(), string(CondPreGatePending)) {
		t.Errorf("the refusal does not name the hold: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the refused claim took %s; it waited on the lock", elapsed)
	}
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepPending {
		t.Errorf("status = %q after the refusal, want %q", step.Status, db.StepPending)
	}
	if sentinelExists(t, sentinel) {
		t.Error("the refused claim ran the gate")
	}
	if rows := preGateRows(t, conn, stepID, "ac-commands"); len(rows) != 0 {
		t.Errorf("the refused claim recorded rows: %+v", rows)
	}
}

// preGatedRun is the hold tests' shared setup: an activated fixture run whose
// checkout is a seeded git repo, with one trusted `ac-commands` entry running
// argv. The caller then advances to verify@0 and decides what holds its lock.
func preGatedRun(t *testing.T, argv []string) (conn *sql.DB, e *Engine, run *model.Run, repoRoot string) {
	t.Helper()
	withDetachedPreGateLockDir(t)
	installDetachedLauncher(t, func(int, string) error { return nil })

	conn = mustDB(t)
	run, _ = activatedRun(t, conn)
	repoRoot = t.TempDir()
	seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)
	e = execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	return conn, e, run, repoRoot
}

// verdictOf returns the verify verdict a dispatch row drew, "" when the row
// is not in the result.
func verdictOf(result *VerifyResult, instance string) string {
	for _, row := range result.Rows {
		if row.Instance == instance {
			return row.Verdict
		}
	}
	return ""
}

// TestHeldStagedRowVerifiesAsMatched is the dispatch verb's half of the hold.
// A manifest opened at the run's start stages verify@0 behind its
// predecessors; the wave completes them, the saga launches the detached run,
// and the child takes the lock. `dispatch verify` recomputes the offer with
// the step held — absent from the ready set, no longer stageable — and must
// read the stored staged row as matched, not genuinely-missing: nothing about
// what the manifest promised has changed, the claim is only deferred. The
// reconcile runs the same verify, so it must not refuse at that stage either.
//
// The mutant that classifies the held row as missing (dropping the hold
// clause from verifyDispatchTx) refuses the verify and the reconcile on
// nearly every implement → verify-ac lane the moment the child locks.
func TestHeldStagedRowVerifiesAsMatched(t *testing.T) {
	conn, e, run, repoRoot := preGatedRun(t, []string{"/bin/sleep", "1"})

	manifest := openDispatch(t, conn, run.ID, 0, nowMS)
	var staged bool
	for _, row := range manifest.Rows {
		if row.Instance == "verify@0" {
			staged = row.Status == db.StepStaged
		}
	}
	if !staged {
		t.Fatalf("premise: verify@0 is not staged in the opened manifest: %+v", manifest.Rows)
	}

	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)
	implementID := stepIDByInstance(t, conn, "implement@0")

	// Premise: with its predecessors done and no lock held, the staged row
	// verifies (as a ready one) with no discrepancy.
	result, mismatch, err := e.VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "verify before the lock: %v", err)
	if mismatch != nil {
		t.Fatalf("premise: verify reports a mismatch before any lock is held: %+v", mismatch)
	}
	if got := verdictOf(result, "verify@0"); got != RowMatched {
		t.Fatalf("premise: verify@0 verdict = %q before the lock, want %q", got, RowMatched)
	}

	holdLockInAnotherProcess(t, detachedPreGateLockPath(stepID, head))
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "verify@0")); ok || cond != CondPreGatePending {
			t.Fatalf("premise: Ready(verify@0) = (%v, %q) while the lock is held", ok, cond)
		}
	})

	result, mismatch, err = e.VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "verify while held: %v", err)
	if mismatch != nil {
		t.Errorf("verify refuses over the held staged row: position %d, stored %s",
			mismatch.Position, mismatch.Stored)
	}
	if got := verdictOf(result, "verify@0"); got != RowMatched {
		t.Errorf("verify@0 verdict = %q while its detached run holds the lock, want %q", got, RowMatched)
	}

	// The reconcile's verify stage is this same verify, so the whole pipeline
	// — back-fill, verify, close — runs to a closed dispatch over the held row.
	out, err := e.ReconcileDispatch(conn, run.ID, []BackfillRow{
		{Step: implementID, Unit: "tokens", Quantity: 1000},
	}, "wave-journal:held", "", true, IntegrationSkip{}, nowMS)
	testsupport.Must(t, err, "reconcile over the held staged row: %v", err)
	if got := verdictOf(out.Verify, "verify@0"); got != RowMatched {
		t.Errorf("reconcile's verify@0 verdict = %q while held, want %q", got, RowMatched)
	}
	if out.Close == nil || out.Close.Status != db.DispatchClosed {
		t.Errorf("reconcile close stage reported %+v, want status %q", out.Close, db.DispatchClosed)
	}
}

// TestUnprobeableLockIsNoHold is the probe's error reading. A lockfile the
// probe cannot open, or cannot flock for any reason but a holder, is NOT a
// hold: on a filesystem where flock fails, the child creates the file, fails
// its own flock, and exits without removing it, so a probe that read every
// error as "held" would hold the step and suppress every relaunch for the
// rest of the run. Only EWOULDBLOCK — another holder — means held, for the
// scheduler and for the launcher alike.
//
// The mutant that restores the sweeper's fail-closed reading (probeLiveLock:
// any open error but ENOENT, any flock error, is "live") holds the step here
// and reports the run in flight.
func TestUnprobeableLockIsNoHold(t *testing.T) {
	conn, e, run, repoRoot := preGatedRun(t, []string{"/bin/sleep", "1"})
	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)
	path := detachedPreGateLockPath(stepID, head)

	// A directory at the lock path: the open fails with something other than
	// ENOENT.
	err := os.Mkdir(path, 0o755)
	testsupport.Must(t, err, "making a directory at the lock path: %v", err)
	if detachedPreGateHeld(path) {
		t.Error("a lock path the probe cannot open reads as held")
	}
	if detachedPreGateInFlight(path) {
		t.Error("a lock path the launcher cannot open reads as in flight, suppressing every relaunch")
	}
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		step := stepNamed(t, sched, "verify@0")
		if ok, cond := sched.Ready(step); !ok {
			t.Errorf("verify@0 is held (%q) behind a lock path that cannot be opened", cond)
		}
		if got := BlockedReason(sched, step); got != "" {
			t.Errorf("blocked reason = %q behind a lock path that cannot be opened, want none", got)
		}
	})

	// Positive control: the same path, as a lockfile another process holds,
	// IS a hold — the probe distinguishes a holder from an error.
	err = os.Remove(path)
	testsupport.Must(t, err, "removing the directory: %v", err)
	holdLockInAnotherProcess(t, path)
	if !detachedPreGateHeld(path) {
		t.Error("a lock another process holds does not read as held")
	}
	if !detachedPreGateInFlight(path) {
		t.Error("a lock another process holds does not read as in flight")
	}
}

// TestBudgetOverrideDoesNotBypassTheHold: the claim admits a CondBudget
// refusal on the dispatcher's scaled cost (`--cost-multiplier`, DKT-867),
// trusting that CondBudget means every other condition held. The hold must
// therefore be reported BEFORE the budget, or a held step whose declared cost
// crosses the cap is claimed through the override into a budget-cut row.
//
// The mutant that evaluates the hold after R7 admits the held claim here.
func TestBudgetOverrideDoesNotBypassTheHold(t *testing.T) {
	shrinkClaimBudget(t, 500*time.Millisecond)
	conn, e, run, repoRoot := preGatedRun(t, nil)
	argv, sentinel := witnessCommand(t, repoRoot, "claimed-through-override")
	e = execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)

	// A cap the DECLARED cost of verify@0 crosses and a quarter of it does not.
	floor := runFloor(t, conn, run.ID)
	cost := expectedCostOf(t, conn, "verify@0")
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, floor+cost/2, run.ID)
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "verify@0")); ok || cond != CondBudget {
			t.Fatalf("premise: Ready(verify@0) = (%v, %q) under the cap with no lock, want (false, %q)",
				ok, cond, CondBudget)
		}
	})

	holder := holdLockInAnotherProcess(t, detachedPreGateLockPath(stepID, head))
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "verify@0")); ok || cond != CondPreGatePending {
			t.Errorf("Ready(verify@0) = (%v, %q) held under the cap, want (false, %q): the hold must precede the budget",
				ok, cond, CondPreGatePending)
		}
	})

	_, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w", CostMultiplier: 0.25, NowMS: nowMS})
	if err == nil {
		t.Fatal("the cheaper-variant claim was admitted while the detached run's lock was held")
	}
	if !strings.Contains(err.Error(), string(CondPreGatePending)) {
		t.Errorf("the refusal does not name the hold: %v", err)
	}
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepPending {
		t.Errorf("status = %q after the refusal, want %q", step.Status, db.StepPending)
	}
	got, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if got.Status != model.RunActive {
		t.Errorf("run is %s after a hold refusal, want %s: a hold is not a budget breach", got.Status, model.RunActive)
	}
	if sentinelExists(t, sentinel) {
		t.Error("the refused claim ran the gate")
	}

	// Control: the holder dies, and the SAME claim is admitted through the
	// override — the path under test is the override, not the cap.
	holder.Process.Kill()
	holder.Wait()
	claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w", CostMultiplier: 0.25, NowMS: nowMS})
	testsupport.Must(t, err, "the cheaper-variant claim once the lock lifted: %v", err)
	if claim.Token == "" {
		t.Fatal("the admitted claim returned no token")
	}
}

// TestLazyReapReprobesTheHold: the holds are loaded with the snapshot, when
// a step whose lease lapsed is still `claimed` and so no candidate. The lazy
// reap — the claim's own and the shared one `next` and the dispatch verbs
// run — flips it back to pending inside the same snapshot, and the readiness
// pass that follows must see its lock: a claim is refused naming the hold,
// and `next` does not offer it.
//
// The mutant that drops the re-probe after the reap admits the claim and
// offers the step as ready.
func TestLazyReapReprobesTheHold(t *testing.T) {
	shrinkClaimBudget(t, 500*time.Millisecond)
	conn, e, run, repoRoot := preGatedRun(t, nil)
	argv, _ := witnessCommand(t, repoRoot, "first-claim")
	e = execEngineWithTrust(t, repoRoot, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})
	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)

	// The first claim, with nothing in flight, runs the gate on the budgeted
	// path and holds a lease that then lapses.
	first, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "first claim: %v", err)
	if first.Token == "" {
		t.Fatal("the first claim returned no token")
	}
	execSQL(t, conn, `UPDATE steps SET expires_ms = 1 WHERE id = ?`, stepID)

	// A detached run for the same target takes the lock before anyone reaps.
	holdLockInAnotherProcess(t, detachedPreGateLockPath(stepID, head))

	_, err = e.ClaimStepWithGates(conn, stepID, ClaimOptions{Owner: "w2", NowMS: nowMS + 1})
	if err == nil {
		t.Fatal("the claim over the lapsed lease was admitted while the detached run's lock was held")
	}
	if !strings.Contains(err.Error(), string(CondPreGatePending)) {
		t.Errorf("the refusal does not name the hold: %v", err)
	}

	next, err := e.NextSteps(conn, run.ID, 0, nowMS+1)
	testsupport.Must(t, err, "next over the lapsed lease: %v", err)
	if status := offeredAs(next, "verify@0"); status != "" {
		t.Errorf("next offered verify@0 as %q after reaping it while the detached run's lock is held", status)
	}
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepPending {
		t.Errorf("status = %q after next's reap, want %q", step.Status, db.StepPending)
	}
}
