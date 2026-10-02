package engine

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
