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
