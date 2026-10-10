package engine

import (
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

// The tree mutex guards the SHARED CHECKOUT, not every tree of the project
// (TDD docs/tdd/gates-trust.md §7.4 L2, as amended).
//
// Keyed per project, every `tree = true` gate of a wave — records in distinct
// worktrees, claim-time pre-gates in scratch reconstructions — queued on one
// lockfile. Under sixteen executors the queue outran the 5m bound: build
// gates recorded `skipped` with "waited longer than" and parked correct work
// as unmeasured, and claims blocked behind sibling gates past the executor's
// 120s tool timeout. A gate on an isolated tree has that tree to itself and
// takes no lock; a gate on the shared checkout still does.

// isolatedTreeEntry is a `build` entry declaring `tree` whose command holds
// the tree long enough that serialized runs would overrun the bound, and
// whose bound is short enough that the overrun records within the test.
func isolatedTreeEntry(repoRoot string) trust.Entry {
	argv := []string{"/bin/sleep", "0.5"}
	return trust.Entry{
		Name: "build", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot), Tree: true, Timeout: "1s", ReRunnable: true,
	}
}

// TestIsolatedWorktreeGatesDoNotSerialize is the acceptance case: N tree
// gates on N distinct worktrees against one project all run, none records a
// lock wait, and the whole set finishes in about one gate's time rather
// than N of them.
//
// The failing variant is the per-project lock: eight 500ms holders queue on
// one file with a 1s bound, so the third and later record "waited longer
// than" and the set takes several seconds.
func TestIsolatedWorktreeGatesDoNotSerialize(t *testing.T) {
	repoRoot, docketDir := treeLockRepo(t)
	lockPath := filepath.Join(docketDir, "tree.lock")

	runner := NewExecRunner(RepoPaths{
		ExecRoot: repoRoot, Identity: repoRoot, LockPath: lockPath,
	})
	runner.LoadStore = sandboxTrust(t, isolatedTreeEntry(repoRoot))

	const gates = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []GateExecution
	)
	start := time.Now()
	for range gates {
		worktree := t.TempDir() // distinct, and it exists: the runner stats it
		wg.Add(1)
		go func() {
			defer wg.Done()
			ex, err := runner.Execute(t.Context(), GateSpec{Name: "build"},
				StepContext{WorkRoot: worktree})
			if err != nil {
				t.Errorf("Execute: %v", err)
				return
			}
			mu.Lock()
			results = append(results, ex)
			mu.Unlock()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if len(results) != gates {
		t.Fatalf("%d gates completed, want %d", len(results), gates)
	}
	for _, ex := range results {
		if ex.Verdict != VerdictPass {
			t.Errorf("a gate on its own worktree recorded %q: %+v", ex.Verdict, ex.Results)
		}
		for _, row := range ex.Results {
			if strings.Contains(row.Reason, "waited longer than") {
				t.Errorf("a gate on its own worktree waited on the tree mutex: %q", row.Reason)
			}
		}
	}
	// Eight 500ms gates serialized take 4s; concurrent, about 0.5s. The
	// bound leaves room for a loaded machine and none for serialization.
	if elapsed > 2500*time.Millisecond {
		t.Errorf("%d isolated-worktree gates took %s; they serialized on the "+
			"per-project tree mutex", gates, elapsed)
	}
}

// treeLockScopeWorkflow pairs a tree-holding `write` step, whose completion
// runs a `build` tree gate in its declared worktree, with a `check` step whose
// claim-time `ac-commands` pre-gate binds that worktree through `issue.diff`.
const treeLockScopeWorkflow = `
[pipeline]
name = "treelockscope"
version = 1
[[step]]
name = "write"
executor = "implement"
class = "write"
emits = "change-summary"
gates = ["build"]
[[step]]
name = "check"
after = ["write"]
executor = "verify-ac"
emits = "ac-report"
gates = [{ name = "ac-commands", pre = true }]
inputs = ["issue.diff"]
`

// TestConcurrentClaimsAndRecordsOnWorktreesDoNotSerialize drives acceptance
// (1) through the database: eight claims of tree-pre-gated steps and eight
// records into distinct worktrees run at once against one project. No gate
// waits on the tree mutex, and every claim returns a token inside the budget.
//
// The pre-gate wants 30s, so the budget bounds every claim. The budget is 1s
// rather than the reference test's 500ms so that eight claims serialized on
// one lock (about 8s) overrun the bound, while the margin above the budget
// stays the 4.5s TestClaimReturnsWithinThePreGateBudget allows.
func TestConcurrentClaimsAndRecordsOnWorktreesDoNotSerialize(t *testing.T) {
	shrinkClaimBudget(t, time.Second)
	const claimBoundMargin = 4500 * time.Millisecond

	repoRoot, docketDir := treeLockRepo(t)
	preGateArgv := []string{"/bin/sleep", "30"}
	runner := NewExecRunner(RepoPaths{
		ExecRoot: repoRoot, Identity: repoRoot,
		LockPath: filepath.Join(docketDir, "tree.lock"),
	})
	runner.LoadStore = sandboxTrust(t, isolatedTreeEntry(repoRoot), trust.Entry{
		Name: "ac-commands", Argv: preGateArgv, ArgvSHA256: trust.ArgvSHA256(preGateArgv),
		Repo: mustResolve(repoRoot), Tree: true,
	})
	e := testEngine()
	e.Gates = runner

	const n = 8
	conn := mustDB(t)
	registerSource(t, conn, []byte(treeLockScopeWorkflow), "treelockscope.toml")
	issues := make([]int, 2*n)
	for i := range issues {
		issues[i] = createIssue(t, conn, "tree lock scope "+strconv.Itoa(i), "body", "task", nil)
	}
	run := startRun(t, conn, issues...)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The first eight issues' writes record through pass-through gates, so
	// their checks are claimable with a worktree to bind. The last eight
	// writes are claimed now and recorded concurrently below.
	setup := testEngine()
	claimIDs := make([]int, n)
	for i := range n {
		write := stepIDIn(t, conn, issues[i], "write@0")
		claim, err := ClaimStep(conn, write, ClaimOptions{Owner: "setup", NowMS: nowMS})
		testsupport.Must(t, err, "claim write for issue %d: %v", issues[i], err)
		err = setup.CompleteStep(conn, write, CompleteOptions{
			Token: claim.Token, Artifact: []byte("summary"), WorkDir: t.TempDir(), NowMS: nowMS,
		})
		testsupport.Must(t, err, "complete write for issue %d: %v", issues[i], err)
		claimIDs[i] = stepIDIn(t, conn, issues[i], "check@0")
	}
	type pendingRecord struct {
		stepID   int
		token    string
		worktree string
	}
	records := make([]pendingRecord, n)
	for i := range n {
		write := stepIDIn(t, conn, issues[n+i], "write@0")
		claim, err := ClaimStep(conn, write, ClaimOptions{Owner: "writer", NowMS: nowMS})
		testsupport.Must(t, err, "claim write for issue %d: %v", issues[n+i], err)
		records[i] = pendingRecord{stepID: write, token: claim.Token, worktree: t.TempDir()}
	}

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		claimed int
	)
	for i, stepID := range claimIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			began := time.Now()
			claim, err := e.ClaimStepWithGates(conn, stepID, ClaimOptions{
				Owner: "claimant" + strconv.Itoa(i), NowMS: nowMS,
			})
			elapsed := time.Since(began)
			if err != nil {
				t.Errorf("claim of step %d: %v", stepID, err)
				return
			}
			if claim.Token == "" {
				t.Errorf("claim of step %d returned no token", stepID)
				return
			}
			if bound := claimPreGateBudget + claimBoundMargin; elapsed > bound {
				t.Errorf("claim of step %d took %s, past the %s budget plus %s",
					stepID, elapsed, claimPreGateBudget, claimBoundMargin)
			}
			mu.Lock()
			claimed++
			mu.Unlock()
		}()
	}
	for _, r := range records {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := e.CompleteStep(conn, r.stepID, CompleteOptions{
				Token: r.token, Artifact: []byte("summary"), WorkDir: r.worktree, NowMS: nowMS,
			})
			if err != nil {
				t.Errorf("record of step %d in %s: %v", r.stepID, r.worktree, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if claimed != n {
		t.Fatalf("%d claims returned a token, want %d", claimed, n)
	}

	stepIDs := append([]int(nil), claimIDs...)
	for _, r := range records {
		stepIDs = append(stepIDs, r.stepID)
	}
	var preRows, buildRows int
	for _, stepID := range stepIDs {
		rows, err := db.GateResultsForStep(conn, stepID)
		testsupport.Must(t, err, "GateResultsForStep(%d): %v", stepID, err)
		for _, row := range rows {
			if strings.Contains(row.Reason, "waited longer than") {
				t.Errorf("step %d gate %s waited on the tree mutex: %q", stepID, row.Gate, row.Reason)
			}
			switch {
			case row.Pre && row.Gate == "ac-commands":
				preRows++
			case !row.Pre && row.Gate == "build":
				buildRows++
				if row.Verdict != VerdictPass {
					t.Errorf("step %d build gate on its own worktree recorded %q: %q",
						stepID, row.Verdict, row.Reason)
				}
			}
		}
	}
	if preRows != n || buildRows != n {
		t.Errorf("recorded %d pre-gate rows and %d build rows, want %d of each",
			preRows, buildRows, n)
	}
}

// TestSharedCheckoutGateStillTakesTheLock is acceptance (2)'s runner half: a
// tree gate whose work root IS the shared checkout serializes exactly as
// before, so a held lock still makes it record the wait.
func TestSharedCheckoutGateStillTakesTheLock(t *testing.T) {
	repoRoot, docketDir := treeLockRepo(t)
	lockPath := filepath.Join(docketDir, "tree.lock")

	held, err := acquireTreeLock(lockPath, time.Second)
	testsupport.Must(t, err, "acquireTreeLock: %v", err)
	defer held.release()

	runner := NewExecRunner(RepoPaths{
		ExecRoot: repoRoot, Identity: repoRoot, LockPath: lockPath,
	})
	runner.LoadStore = sandboxTrust(t, treeLockedBuildEntry(repoRoot))

	for _, workRoot := range []string{"", repoRoot, repoRoot + "/."} {
		ex, err := runner.Execute(t.Context(), GateSpec{Name: "build"},
			StepContext{WorkRoot: workRoot})
		testsupport.Must(t, err, "Execute (work root %q): %v", workRoot, err)
		if len(ex.Results) != 1 || ex.Results[0].Verdict != VerdictSkipped ||
			!strings.Contains(ex.Results[0].Reason, "working-tree") {
			t.Errorf("work root %q: a shared-checkout tree gate did not wait on "+
				"the held mutex: %+v", workRoot, ex.Results)
		}
	}
}
