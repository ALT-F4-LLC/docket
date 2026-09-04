package engine

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// DKT-992 — gate processes learn the step's base commit, so a gate can scan
// exactly the step's committed range.
//
// Executors commit before `step record`, so at gate time a worktree-recorded
// step's tree is CLEAN: RUN-66's secret-scan passed 8/8 write steps having
// scanned zero lines, and range-shaped gates fell back to guesses
// (`git diff HEAD~1`, wrong for multi-commit steps). The engine knows the
// step's base at record time — the same fork point the diff stage resolves —
// and DOCKET_GATE_BASE is how a gate child finally learns it. The contract:
// set to the worktree's creation commit for a worktree-recorded step, UNSET
// for a non-worktree step (never an invented value), so a gate that finds it
// absent over a clean tree can fail closed.

// baseCapture is a GateRunner that records the full StepContext each gate was
// dispatched with, keyed by gate name.
type baseCapture struct {
	mu   sync.Mutex
	seen map[string]StepContext
}

func (c *baseCapture) Run(_ context.Context, g GateSpec, sc StepContext) (GateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen[g.Name] = sc
	return GateResult{Gate: g.Name, Verdict: VerdictPass}, nil
}

// TestGateChildSeesDocketGateBase is the runner half, with a REAL spawn: the
// witness is printenv itself, so the assertion is about the environment the
// child actually received, not about a struct field. A context carrying a
// base exports it; one carrying none leaves the variable UNSET — printenv
// exits 1 having printed nothing, which is the child's own proof of absence.
func TestGateChildSeesDocketGateBase(t *testing.T) {
	execRoot, worktree, execHead, _ := gitFixture(t)
	printenvPath, err := exec.LookPath("printenv")
	if err != nil {
		t.Skip("printenv is not installed")
	}

	argv := []string{printenvPath, "DOCKET_GATE_BASE"}
	runner := NewExecRunner(testRepoPaths(execRoot))
	runner.LoadStore = sandboxTrust(t, trust.Entry{
		Name: "secret-scan", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(execRoot),
	})

	// The worktree-recorded shape: the fixture's worktree was created at the
	// shared checkout's HEAD, so that commit is the base the saga resolves.
	ex, err := runner.Execute(context.Background(),
		GateSpec{Name: "secret-scan"},
		StepContext{WorkRoot: worktree, Base: execHead})
	testsupport.Must(t, err, "running the gate with a base: %v", err)
	if ex.Verdict != VerdictPass {
		t.Fatalf("gate verdict = %q (reason %q), want %q — printenv found no "+
			"DOCKET_GATE_BASE in the child environment",
			ex.Verdict, ex.Results[0].Reason, VerdictPass)
	}
	if got := strings.TrimSpace(ex.Results[0].Output); got != execHead {
		t.Errorf("the child saw DOCKET_GATE_BASE=%q, want the worktree's "+
			"creation commit %s", got, execHead)
	}

	// The non-worktree shape: no base rides in the context, and the variable
	// must be ABSENT — not empty — in the child. printenv exiting 1 with no
	// output is the observable.
	ex, err = runner.Execute(context.Background(),
		GateSpec{Name: "secret-scan"}, StepContext{})
	testsupport.Must(t, err, "running the gate without a base: %v", err)
	if got := strings.TrimSpace(ex.Results[0].Output); got != "" {
		t.Errorf("the child saw DOCKET_GATE_BASE=%q for a non-worktree step, "+
			"want the variable unset", got)
	}
	if ex.Verdict != VerdictFail {
		t.Errorf("gate verdict = %q, want %q — printenv must exit 1 when the "+
			"variable is genuinely unset", ex.Verdict, VerdictFail)
	}
}

// TestCompletionGateBaseIsTheWorktreeCreationCommit is the saga half of the
// acceptance: every completion gate of a `--worktree`-recorded step is
// dispatched with Base naming the commit the worktree was created from — the
// fork point, the SAME base the recorded issue.diff compares against — never
// the run's pinned commit (which predates inherited sibling work, DKT-42's
// over-attribution) and never the worktree's own HEAD (an empty range).
func TestCompletionGateBaseIsTheWorktreeCreationCommit(t *testing.T) {
	shared := gitRepo(t)
	pinned := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))

	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "gate base", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "wt gate base", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue: %v", err)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// A sibling issue lands on the shared checkout AFTER run start...
	writeFile(t, shared, "sibling/left.txt", "a sibling's inherited work\n")
	runGit(t, shared, "add", "-A")
	runGit(t, shared, "commit", "-qm", "sibling issue's commit")
	fork := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))

	// ...and THEN the worktree is created from it, and the step commits its
	// own work — the exact shape `step record --worktree` leaves behind.
	worktree := filepath.Join(t.TempDir(), "wt")
	runGit(t, shared, "worktree", "add", "-q", worktree)
	writeFile(t, worktree, "mine/change.txt", "the step's own work\n")
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-qm", "the step's commit")
	workHead := strings.TrimSpace(gitOutput(t, worktree, "rev-parse", "HEAD"))

	e := testEngine()
	captured := &baseCapture{seen: map[string]StepContext{}}
	e.Gates = captured

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"),
		WorkDir: worktree, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	if len(captured.seen) == 0 {
		t.Fatal("no completion gate was dispatched")
	}
	for gate, sc := range captured.seen {
		if sc.Base != fork {
			t.Errorf("gate %s dispatched with Base %q, want the worktree's "+
				"creation commit %q (pinned run commit %q, worktree HEAD %q — "+
				"neither is the step's base)", gate, sc.Base, fork, pinned, workHead)
		}
		if sc.WorkRoot != worktree {
			t.Errorf("gate %s dispatched with WorkRoot %q, want %q",
				gate, sc.WorkRoot, worktree)
		}
	}
}

// TestCompletionGateBaseUnsetForNonWorktreeStep pins the other acceptance
// direction, and the CHOICE it encodes: a step recorded without `--worktree`
// dispatches every gate with Base == "" — the variable unset — rather than
// some live HEAD read docket cannot vouch for as a range endpoint. Unset is
// the documented pick; absence, not an invented value, is what lets a
// range-shaped gate fail closed instead of scanning the wrong range.
func TestCompletionGateBaseUnsetForNonWorktreeStep(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	e := testEngine()
	captured := &baseCapture{seen: map[string]StepContext{}}
	e.Gates = captured

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	if len(captured.seen) == 0 {
		t.Fatal("no completion gate was dispatched")
	}
	for gate, sc := range captured.seen {
		if sc.Base != "" {
			t.Errorf("gate %s dispatched with Base %q for a non-worktree step, "+
				"want \"\" (the variable unset)", gate, sc.Base)
		}
	}
}

// TestGateBaseSHADegenerateTreesAnswerUnset pins the resolver's fail-closed
// edges directly: no worktree, a "worktree" that IS the run's exec root, and
// a tree whose fork point cannot be resolved all answer "" — the variable
// stays unset — never a guessed sha and never runDiffBase's pinned fallback,
// which is not the commit any worktree was created from.
func TestGateBaseSHADegenerateTreesAnswerUnset(t *testing.T) {
	shared := gitRepo(t)
	conn := mustDB(t)
	run, err := db.InsertRunWithContext(conn, 1, "degenerate", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: "feedfacefeedface"})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)

	if got := gateBaseSHA(conn, run.ID, ""); got != "" {
		t.Errorf("gateBaseSHA(no worktree) = %q, want \"\"", got)
	}
	if got := gateBaseSHA(conn, run.ID, shared); got != "" {
		t.Errorf("gateBaseSHA(worktree == exec root) = %q, want \"\" — the "+
			"shared checkout has no fork point and no step-owned range", got)
	}
	notARepo := t.TempDir()
	if got := gateBaseSHA(conn, run.ID, notARepo); got != "" {
		t.Errorf("gateBaseSHA(unresolvable fork) = %q, want \"\" — an "+
			"unresolvable base exports nothing, never the pinned run commit", got)
	}
}
