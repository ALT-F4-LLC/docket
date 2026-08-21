package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// Gates run in the step's worktree (DKT-9).
//
// Before the fix, every gate spawned with the shared checkout as its cwd
// unconditionally, so a gate's recorded evidence could describe a HEAD the
// step under review never touched — a `verdict: pass` measured against the
// wrong tree. The observable throughout this file is `git rev-parse HEAD`:
// the same primitive ac-commands-shaped scripts record, and the one AC2 says
// must match the target, not the shared checkout.

// gitRun executes git in dir and returns its trimmed output, with the
// identity flags every commit needs in a bare test environment.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// commit.gpgsign is forced OFF: a machine whose global config signs via an
	// agent (1Password, gpg-agent) hangs every fixture commit in a sandboxed
	// test run — the agent's socket is unreachable and git waits it out.
	cmd := exec.Command("git",
		append([]string{
			"-c", "user.name=t", "-c", "user.email=t@t",
			"-c", "commit.gpgsign=false",
		}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	testsupport.Must(t, err, "git %v in %s: %v\n%s", args, dir, err, out)
	return strings.TrimSpace(string(out))
}

// gitFixture builds a repo with one commit and a linked worktree advanced one
// commit further, so the two checkouts' HEADs DIFFER — without that, a gate
// running in the wrong tree would still print the right sha and the tests
// here would prove nothing.
func gitFixture(t *testing.T) (execRoot, worktree, execHead, workHead string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	execRoot = t.TempDir()
	gitRun(t, execRoot, "init", "-q")
	gitRun(t, execRoot, "commit", "--allow-empty", "-q", "-m", "base")
	worktree = filepath.Join(t.TempDir(), "wt")
	gitRun(t, execRoot, "worktree", "add", "-q", worktree)
	gitRun(t, worktree, "commit", "--allow-empty", "-q", "-m", "step work")

	execHead = gitRun(t, execRoot, "rev-parse", "HEAD")
	workHead = gitRun(t, worktree, "rev-parse", "HEAD")
	if execHead == workHead {
		t.Fatal("fixture heads are equal; the worktree commit did not diverge")
	}
	return execRoot, worktree, execHead, workHead
}

// TestGateSpawnsInWorkRootNotExecRoot is the runner half of DKT-9 AC1/AC2: a
// StepContext carrying a WorkRoot spawns the gate THERE, and one without
// keeps the shared checkout — the saga.go diff stage's resolution, applied to
// gate dispatch.
func TestGateSpawnsInWorkRootNotExecRoot(t *testing.T) {
	execRoot, worktree, execHead, workHead := gitFixture(t)
	gitPath, err := exec.LookPath("git")
	testsupport.Must(t, err, "resolving git: %v", err)

	argv := []string{gitPath, "rev-parse", "HEAD"}
	runner := NewExecRunner(testRepoPaths(execRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "tests", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(execRoot),
	})

	ex, err := runner.Execute(context.Background(),
		GateSpec{Name: "tests"}, StepContext{WorkRoot: worktree})
	testsupport.Must(t, err, "running the gate in a worktree: %v", err)
	if got := strings.TrimSpace(ex.Results[0].Output); got != workHead {
		t.Errorf("the gate saw HEAD %s, want the worktree's %s — it ran in the "+
			"shared checkout instead of the step's tree", got, workHead)
	}

	ex, err = runner.Execute(context.Background(),
		GateSpec{Name: "tests"}, StepContext{})
	testsupport.Must(t, err, "running the gate without a worktree: %v", err)
	if got := strings.TrimSpace(ex.Results[0].Output); got != execHead {
		t.Errorf("with no WorkRoot the gate saw HEAD %s, want the shared "+
			"checkout's %s", got, execHead)
	}
}

// TestGateWorkRootGoneFailsClosed: a recorded worktree that no longer exists
// (they are swept at integration) records an honest failure — it neither runs
// the gate in the shared checkout in its place (the silent wrong-tree defect
// again) nor surfaces an engine error, which on the pre-claim path would
// block the claim PG2/PG3 forbid blocking.
func TestGateWorkRootGoneFailsClosed(t *testing.T) {
	repoRoot := t.TempDir()
	argv, sentinel := witnessCommand(t, repoRoot, "gone-ran")

	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "tests", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot),
	})

	gone := filepath.Join(repoRoot, "swept-away")
	ex, err := runner.Execute(context.Background(),
		GateSpec{Name: "tests"}, StepContext{WorkRoot: gone})
	testsupport.Must(t, err,
		"a missing worktree must record a failure, not return an error: %v", err)

	if sentinelExists(t, sentinel) {
		t.Error("the gate EXECUTED with a missing worktree; nothing may run " +
			"when the tree it would measure is gone")
	}
	if ex.Verdict != VerdictFail {
		t.Errorf("gate verdict = %q, want %q", ex.Verdict, VerdictFail)
	}
	r := ex.Results[0]
	// DKT-169: the row says `skipped`, not `fail` — nothing was measured, and
	// a reader (or a verify step told to read recorded output) must be able to
	// tell that from a gate that ran and failed. Routing stays fail-closed via
	// the execution verdict above.
	if r.Verdict != VerdictSkipped {
		t.Errorf("row verdict = %q, want %q", r.Verdict, VerdictSkipped)
	}
	if r.Exit != nil {
		t.Errorf("exit = %v on a gate that never spawned, want nil", r.Exit)
	}
	if !strings.Contains(r.Reason, gone) {
		t.Errorf("the reason does not name the missing worktree: %q", r.Reason)
	}
}

// workRootCapture is a GateRunner that records the WorkRoot each gate was
// dispatched with, keyed by gate name.
type workRootCapture struct {
	mu   sync.Mutex
	seen map[string]string
}

func (c *workRootCapture) Run(_ context.Context, g GateSpec, sc StepContext) (GateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[g.Name] = sc.WorkRoot
	return GateResult{Gate: g.Name, Verdict: VerdictPass}, nil
}

// TestCompletionGatesCarryTheStepsWorkRoot is AC1's completion half: every
// gate that runs inside `complete` for a step that declared `--worktree` is
// dispatched with that worktree, not the shared checkout.
func TestCompletionGatesCarryTheStepsWorkRoot(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	e := testEngine()
	captured := &workRootCapture{seen: map[string]string{}}
	e.Gates = captured

	workDir := t.TempDir()
	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"),
		WorkDir: workDir, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	// The fixture's `implement` declares five completion gates; each must have
	// been dispatched with the declared worktree.
	if len(captured.seen) == 0 {
		t.Fatal("no completion gate was dispatched")
	}
	for gate, workRoot := range captured.seen {
		if workRoot != workDir {
			t.Errorf("gate %s dispatched with WorkRoot %q, want the step's "+
				"declared worktree %q", gate, workRoot, workDir)
		}
	}
}

// TestPreGatesRunInTheResolvedTargetWorktree is AC2's mechanism, engine-level:
// `implement` completes declaring a private worktree, and `verify`'s
// ac-commands pre-gate — a `git rev-parse HEAD` stand-in for the same
// primitive ac-commands.sh records — must report the TARGET worktree's HEAD,
// not the shared checkout's.
//
// This is the hoisted resolution's proof (DKT-9 change 3): at the moment the
// pre-gate spawns, no bundle exists yet, so the claim resolves the target ref
// itself, inside transaction A, from the same `issue.diff` round record the
// bundle's target_sha/target_worktree are later lifted from.
func TestPreGatesRunInTheResolvedTargetWorktree(t *testing.T) {
	conn := mustDB(t)

	// The committed fixture leaves holds_tree defaulted — true — on every
	// step, so review and synthesize would each re-record a payload-less
	// `issue.diff` OVER implement's round record, and the resolved diff would
	// carry no target ref at all. The reference workflows declare
	// holds_tree = false on their read-shaped steps (DKT-75) precisely so the
	// last HOLDING record — the reviewed object — is the one consumers
	// resolve; this test needs that same shape.
	registerFixtureSchema(t, conn)
	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)
	patched := strings.Replace(string(src),
		"name = \"review\"", "name = \"review\"\nholds_tree = false", 1)
	patched = strings.Replace(patched,
		"name = \"synthesize\"", "name = \"synthesize\"\nholds_tree = false", 1)
	registerSource(t, conn, []byte(patched), fixturePath)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	execRoot, worktree, execHead, workHead := gitFixture(t)
	gitPath, err := exec.LookPath("git")
	testsupport.Must(t, err, "resolving git: %v", err)

	// The driver completes `implement` DECLARING the worktree, exactly as an
	// executor in a private checkout reports it. Its HeadFn resolves real
	// HEADs so the recorded round record carries the fixture's actual sha —
	// and, like the real one, answers "" where a tree has no commit: the
	// fixture's OTHER holding steps diff the run's exec root, which is no git
	// repository at all.
	driver := testEngine()
	driver.HeadFn = func(dir string) string {
		cmd := exec.Command("git", "rev-parse", "HEAD")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = driver.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"),
		WorkDir: worktree, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	for i := range 4 {
		claimAndComplete(t, conn, driver, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, driver, "synthesize@0", "synthesized", "")
	driveAction(t, conn, driver, "reconcile@0")

	// The claiming engine runs the REAL runner: verify's pre-gate is the
	// git-HEAD stand-in, trusted for the fixture's exec root.
	argv := []string{gitPath, "rev-parse", "HEAD"}
	e := testEngine()
	runner := NewExecRunner(testRepoPaths(execRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "ac-commands", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(execRoot),
	})
	e.Gates = runner

	verifyID := stepIDByInstance(t, conn, "verify@0")
	got, err := e.ClaimStepWithGates(conn, verifyID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claiming verify: %v", err)

	if len(got.Context.PreGates) != 1 {
		t.Fatalf("bundle carries %d pre-gate results, want 1", len(got.Context.PreGates))
	}
	pre := got.Context.PreGates[0]
	if pre.Verdict != VerdictPass {
		t.Fatalf("pre-gate verdict = %q (reason %q), want %q",
			pre.Verdict, pre.Reason, VerdictPass)
	}
	if head := strings.TrimSpace(pre.Output); head != workHead {
		t.Errorf("the pre-gate recorded HEAD %s, want the target worktree's %s "+
			"(the shared checkout's is %s) — its evidence describes a tree the "+
			"step under review never touched", head, workHead, execHead)
	}

	// The bundle's own target ref agrees with where the gate ran.
	if got.Context.TargetWorktree != worktree {
		t.Errorf("target_worktree = %q, want %q", got.Context.TargetWorktree, worktree)
	}
}
