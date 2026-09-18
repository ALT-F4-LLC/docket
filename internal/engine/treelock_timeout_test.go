package engine

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-91's remaining case: a `tree = true` gate whose working-tree lock does
// not come free within its bound.
//
// The serialization the gate requires could not be provided, so no process was
// spawned and nothing about the tree was read. That is the same fact DKT-254's
// vanished worktree carries, and it takes the same verdict: `skipped`, routed
// as a gate that measured nothing. Recording `fail` here spent the token a
// genuinely failing build spends, and a fix loop entered on it would ask a
// worker to fix a tree the engine never opened.

// treeLockGateSrc is one executor step with one gate whose failure routes into
// a fix loop. The `on_fail` is what makes the routing assertion discriminating:
// a timeout recorded as `fail` spawns `fix@1`, and one recorded as unmeasured
// parks instead.
const treeLockGateSrc = `
[pipeline]
name = "tree-lock-gate"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "implement"
emits = "change-summary"
gates = ["build"]
on_fail = "fix-loop"

[[step]]
name = "fix"
executor = "implement"
emits = "change-summary"
loop = true
after_loop = "implement"
`

// treeLockedBuildEntry is a `build` entry declaring `tree` with a bound short
// enough that a held lock times out without the suite waiting on the default.
func treeLockedBuildEntry(repoRoot string) trust.Entry {
	argv := []string{"/usr/bin/true"}
	return trust.Entry{
		Name: "build", Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
		Repo: mustResolve(repoRoot), Tree: true, Timeout: "50ms", ReRunnable: true,
	}
}

// TestTreeLockTimeoutRecordsSkipped is the row half: the runner's own output.
func TestTreeLockTimeoutRecordsSkipped(t *testing.T) {
	repoRoot, docketDir := treeLockRepo(t)
	lockPath := filepath.Join(docketDir, "tree.lock")

	// Held for the whole test, so the acquisition below cannot succeed.
	held, err := acquireTreeLock(lockPath, time.Second)
	testsupport.Must(t, err, "acquireTreeLock: %v", err)
	defer held.release()

	runner := NewExecRunner(RepoPaths{
		ExecRoot: repoRoot, Identity: repoRoot, LockPath: lockPath,
	})
	runner.LoadStore = sandboxTrust(t, treeLockedBuildEntry(repoRoot))

	ex, err := runner.Execute(t.Context(), GateSpec{Name: "build"}, StepContext{})
	testsupport.Must(t, err, "Execute: %v", err)

	if len(ex.Results) != 1 {
		t.Fatalf("recorded %d rows, want 1: %+v", len(ex.Results), ex.Results)
	}
	row := ex.Results[0]
	if row.Verdict != VerdictSkipped {
		t.Errorf("row verdict = %q, want %q — the lock never came free, so "+
			"nothing was measured and %q would claim a judgment about the tree",
			row.Verdict, VerdictSkipped, VerdictFail)
	}
	// The reason is what tells an operator WHICH unmeasured condition this was,
	// and the wait is the actionable part of it.
	if !strings.Contains(row.Reason, "working-tree") {
		t.Errorf("the reason does not name the lock wait: %q", row.Reason)
	}
	// Nothing ran, so the fields that describe a process must stay empty: a
	// zero exit on a gate that never spawned is the confusion T11 exists to
	// prevent.
	if row.Exit != nil {
		t.Errorf("exit = %d on a gate that never spawned, want NULL", *row.Exit)
	}
	if row.DurationMS != 0 || row.Output != "" {
		t.Errorf("duration = %d, output = %q on a gate that never spawned",
			row.DurationMS, row.Output)
	}
	// The EXECUTION verdict stays fail so routing remains fail-closed, exactly
	// as the vanished-worktree case above it does.
	if ex.Verdict != VerdictFail {
		t.Errorf("execution verdict = %q, want %q — an unmeasured gate is not "+
			"a pass", ex.Verdict, VerdictFail)
	}
}

// TestTreeLockTimeoutParksAsUnmeasured is the routing half, end to end through
// the saga: the recording step parks for an operator rather than entering the
// fix loop its `on_fail` names.
func TestTreeLockTimeoutParksAsUnmeasured(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(treeLockGateSrc), "tree-lock-gate.toml")
	issue := createIssue(t, conn, "locked", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	repoRoot, docketDir := treeLockRepo(t)
	lockPath := filepath.Join(docketDir, "tree.lock")
	held, err := acquireTreeLock(lockPath, time.Second)
	testsupport.Must(t, err, "acquireTreeLock: %v", err)
	defer held.release()

	e := testEngine()
	runner := NewExecRunner(RepoPaths{
		ExecRoot: repoRoot, Identity: repoRoot, LockPath: lockPath,
	})
	runner.LoadStore = sandboxTrust(t, treeLockedBuildEntry(repoRoot))
	e.Gates = runner

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	step, err := db.GetStep(conn, stepIDByInstance(t, conn, "implement@0"))
	testsupport.Must(t, err, "GetStep: %v", err)
	if !strings.HasPrefix(step.Routing, workflow.OnFailWaitingHuman) {
		t.Errorf("routing = %q, want %q — a gate that measured nothing routes "+
			"to a person, not into a fix loop over an unread tree",
			step.Routing, workflow.OnFailWaitingHuman)
	}
	if !strings.Contains(step.Routing, "measured nothing") {
		t.Errorf("the park reason does not say the gate measured nothing: %q",
			step.Routing)
	}
}
