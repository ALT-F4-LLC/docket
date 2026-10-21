package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// DKT-4250: a completion gate measures a checkout the engine owns.
//
// A writer records from inside its isolated worktree, and the workflow removes
// that worktree once the writer returns. A detached saga that reached its gates
// afterwards ran them against a missing tree, and the runner's own repo root
// (the invoking cwd, the same worktree) no longer resolved either.

// gateNames are the implement step's completion gates in the fixture workflow.
var gateNames = []string{"build", "tests", "scope", "self-hygiene", "secret-scan"}

func TestCompletionGatesMeasureTheRecordedShaAfterTheWorktreeIsRemoved(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	testsupport.Must(t, err, "resolving git: %v", err)

	shared := gitRepo(t)
	pinned := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "gate checkout", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "gate checkout", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue")
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	worktree := filepath.Join(t.TempDir(), "wt")
	runGit(t, shared, "worktree", "add", "-q", worktree)
	writeFile(t, worktree, "mine/change.txt", "the step's own work\n")
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-qm", "the step's commit")
	workHead := strings.TrimSpace(gitOutput(t, worktree, "rev-parse", "HEAD"))

	// Every gate is `git rev-parse HEAD`, so a row's output names the commit the
	// gate actually measured.
	argv := []string{gitPath, "rev-parse", "HEAD"}
	var entries []trust.Entry
	for _, name := range gateNames {
		entries = append(entries, trust.Entry{
			Name: name, Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv),
			Repo: mustResolve(shared),
		})
	}
	// The runner is built the way a detached saga launched from the writer's
	// worktree builds it: its exec root is that worktree, its identity the
	// stable project path.
	runner := NewExecRunner(RepoPaths{
		ExecRoot: worktree, Identity: shared,
		LockPath: filepath.Join(shared, ".docket", "tree.lock"),
	})
	runner.LoadStore = sandboxTrust(t, entries...)

	e := testEngine()
	e.Gates = runner

	// The production flow: the record hands the gates to a detached run and
	// returns, the workflow removes the writer's worktree, and only then does
	// the detached run reach the gates.
	withDetachedPreGateLockDir(t)
	installDetachedSagaLauncher(t, func(int) error { return nil })
	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	var pending bool
	testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), WorkDir: worktree,
		Pending: &pending, NowMS: nowMS,
	}), "complete")
	if !pending {
		t.Fatal("premise: the gates were not handed to a detached run")
	}
	testsupport.Must(t, os.RemoveAll(worktree), "removing the worktree")
	run2, err := e.RunDetachedSaga(conn, stepID, nowMS)
	testsupport.Must(t, err, "detached saga: %v", err)
	if run2.Outcome != DetachedSagaRan {
		t.Fatalf("detached saga outcome = %q, want %q", run2.Outcome, DetachedSagaRan)
	}

	if _, statErr := os.Stat(worktree); !os.IsNotExist(statErr) {
		t.Fatalf("premise: the writer's worktree still exists (%v)", statErr)
	}
	rows, err := db.GateResultsForStep(conn, stepID)
	testsupport.Must(t, err, "reading gate rows: %v", err)
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Pre {
			continue
		}
		seen[row.Gate] = true
		if row.Verdict != db.GateVerdictPass || row.Exit == nil || *row.Exit != 0 {
			t.Errorf("gate %s: verdict %q exit %v reason %q; want a real pass",
				row.Gate, row.Verdict, row.Exit, row.Reason)
			continue
		}
		if got := strings.TrimSpace(row.Output); got != workHead {
			t.Errorf("gate %s measured %q, want the recorded sha %q", row.Gate, got, workHead)
		}
	}
	for _, name := range gateNames {
		if !seen[name] {
			t.Errorf("gate %s recorded no row", name)
		}
	}

	// The engine's checkout is gone once the stage settles.
	out := gitOutput(t, shared, "worktree", "list", "--porcelain")
	if strings.Count(out, "worktree ") > 2 {
		t.Errorf("scratch checkouts outlived the gate stage:\n%s", out)
	}
}

// treeCapture is a GateRunner that records, for each gate, the context it was
// dispatched with and the commit checked out at that context's WorkRoot while
// the gate runs: the checkout is released when the stage settles.
type treeCapture struct {
	baseCapture
	heads map[string]string
}

func (c *treeCapture) Run(ctx context.Context, g GateSpec, sc StepContext) (GateResult, error) {
	out, _ := exec.Command("git", "-C", sc.WorkRoot, "rev-parse", "HEAD").Output()
	c.mu.Lock()
	c.heads[g.Name] = strings.TrimSpace(string(out))
	c.mu.Unlock()
	return c.baseCapture.Run(ctx, g, sc)
}

// A writer that committed nothing has its work only in the worktree's
// uncommitted state. Checking out the head would measure a tree WITHOUT that
// work, so the recorded worktree stays the tree and nothing is captured.
func TestAWriterThatCommittedNothingStillMeasuresItsWorktree(t *testing.T) {
	shared := gitRepo(t)
	pinned := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "uncommitted", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "uncommitted", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue")
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	worktree := filepath.Join(t.TempDir(), "wt")
	runGit(t, shared, "worktree", "add", "-q", worktree)
	writeFile(t, worktree, "mine/uncommitted.txt", "work nobody committed\n")

	e := testEngine()
	captured := &treeCapture{baseCapture: baseCapture{seen: map[string]StepContext{}}, heads: map[string]string{}}
	e.Gates = captured
	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), WorkDir: worktree, NowMS: nowMS,
	}), "complete")

	var recorded int
	testsupport.Must(t, conn.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE step_id = ? AND kind = ?`,
		stepID, RecordedHeadArtifactKind).Scan(&recorded), "counting")
	if recorded != 0 {
		t.Errorf("%d recorded-head artifacts for a writer that committed nothing", recorded)
	}
	if len(captured.seen) == 0 {
		t.Fatal("no completion gate was dispatched")
	}
	for gate, sc := range captured.seen {
		if sc.WorkRoot != worktree {
			t.Errorf("gate %s measured %q, want the writer's worktree %q", gate, sc.WorkRoot, worktree)
		}
	}
}
