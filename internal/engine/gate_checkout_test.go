package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	docketexec "github.com/ALT-F4-LLC/docket/internal/exec"
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

	// The diff stage read the same commit: the artifact the close integrates
	// names the sha the gates measured, though no worktree was left to read.
	if got := issueDiffHead(t, conn, stepID); got != workHead {
		t.Errorf("issue.diff head = %q, want the recorded sha %q", got, workHead)
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

	// The record only pins the head: it is Live, so no checkout replaces the
	// worktree that alone holds the uncommitted work.
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if rec, ok := recordedHeadOf(conn, step); !ok || !rec.Live {
		t.Errorf("recorded head = %+v (ok %v) for a writer that committed nothing; want a Live pin", rec, ok)
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

// Edits made after the record, or left uncommitted at it, are work a checkout
// of the recorded sha does not hold: the gates measure the live worktree.
func TestGatesMeasureTheLiveWorktreeWhenItHoldsMoreThanTheRecordedSha(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, worktree string)
		after func(t *testing.T, worktree string)
	}{
		{"uncommitted at record", func(t *testing.T, wt string) {
			writeFile(t, wt, "mine/dirty.txt", "not committed\n")
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shared := gitRepo(t)
			pinned := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))
			conn := mustDB(t)
			registerFixture(t, conn)
			issue := createIssue(t, conn, "live", "body", "task", nil)
			run, err := db.InsertRunWithContext(conn, 1, "live", 0, nowMS,
				db.RunContext{ExecRoot: shared, CommitSHA: pinned})
			testsupport.Must(t, err, "InsertRunWithContext: %v", err)
			testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue")
			_, err = activate(conn, run.ID)
			testsupport.Must(t, err, "activate: %v", err)

			worktree := filepath.Join(t.TempDir(), "wt")
			runGit(t, shared, "worktree", "add", "-q", worktree)
			writeFile(t, worktree, "mine/first.txt", "first\n")
			runGit(t, worktree, "add", "-A")
			runGit(t, worktree, "commit", "-q", "-m", "first")
			if tc.setup != nil {
				tc.setup(t, worktree)
			}

			e := testEngine()
			captured := &treeCapture{baseCapture: baseCapture{seen: map[string]StepContext{}}, heads: map[string]string{}}
			e.Gates = captured
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
			if pending {
				if tc.after != nil {
					tc.after(t, worktree)
				}
				_, err := e.RunDetachedSaga(conn, stepID, nowMS)
				testsupport.Must(t, err, "detached saga: %v", err)
			}
			if len(captured.seen) == 0 {
				t.Fatal("no completion gate was dispatched")
			}
			for gate, sc := range captured.seen {
				if sc.WorkRoot != worktree {
					t.Errorf("gate %s measured %q, want the live worktree %q", gate, sc.WorkRoot, worktree)
				}
			}
		})
	}
}

// Taking the run's root as the containment boundary must not stop a binary
// inside the tree the gate measures from being refused.
func TestAnExecutorsOwnFileIsRefusedEvenOutsideTheRunsRoot(t *testing.T) {
	runRoot, worktree := t.TempDir(), t.TempDir()
	tool := filepath.Join(worktree, "bin", "tool")
	writeFile(t, worktree, "bin/tool", "#!/bin/sh\n")
	testsupport.Must(t, os.Chmod(tool, 0o755), "chmod")

	runner := NewExecRunner(RepoPaths{ExecRoot: runRoot, Identity: runRoot})
	_, err := runner.resolveContained(tool, "tool", StepContext{PinRoot: runRoot, WorkRoot: worktree})
	if !errors.Is(err, docketexec.ErrRefused) || errors.Is(err, docketexec.ErrEnvironment) {
		t.Fatalf("resolving the executor's own file = %v; want a plain refusal", err)
	}
}

// captureRecordedHead takes a head only when a checkout of it is the writer's
// whole work, and never off the shared checkout.
func TestCaptureRecordedHeadRefusesWorkItCannotReproduce(t *testing.T) {
	shared := gitRepo(t)
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "capture", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "capture", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue")

	worktree := filepath.Join(t.TempDir(), "wt")
	runGit(t, shared, "worktree", "add", "-q", worktree)
	writeFile(t, worktree, "mine/a.txt", "a\n")
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-q", "-m", "a")
	e := testEngine()
	e.HeadFn = sharedCheckoutHead

	if rec, ok := e.captureRecordedHead(conn, run.ID, worktree); !ok || rec.Live {
		t.Fatalf("a clean worktree with a commit of its own recorded %+v (ok %v), want a replaceable head", rec, ok)
	}
	writeFile(t, worktree, "mine/untracked.txt", "left over\n")
	if rec, ok := e.captureRecordedHead(conn, run.ID, worktree); !ok || !rec.Live {
		t.Errorf("an untracked file recorded %+v (ok %v), want a Live head that pins the commit without replacing the tree", rec, ok)
	}
	if _, ok := e.captureRecordedHead(conn, run.ID, shared); ok {
		t.Error("a head was recorded for the shared checkout")
	}
}

// issueDiffHead is the head the step's recorded issue.diff artifact names.
func issueDiffHead(t *testing.T, conn *sql.DB, stepID int) string {
	t.Helper()
	var payload string
	testsupport.Must(t, conn.QueryRow(
		`SELECT payload FROM artifacts WHERE step_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`,
		stepID, ArtifactKindIssueDiff).Scan(&payload), "reading the issue.diff artifact")
	var rec struct {
		Head string `json:"head"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(payload), &rec), "decoding the issue.diff payload")
	return rec.Head
}

// The record is authoritative for the whole saga. A worktree that commits again
// after the record changes neither what the gates measure nor what the diff
// names, so gates that passed one commit never stand for another.
func TestACommitMadeAfterTheRecordIsNeitherMeasuredNorIntegrated(t *testing.T) {
	shared := gitRepo(t)
	pinned := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "moved", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "moved", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue")
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	worktree := filepath.Join(t.TempDir(), "wt")
	runGit(t, shared, "worktree", "add", "-q", worktree)
	writeFile(t, worktree, "mine/first.txt", "first\n")
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-q", "-m", "first")
	recorded := strings.TrimSpace(gitOutput(t, worktree, "rev-parse", "HEAD"))

	e := testEngine()
	e.DiffFn = GitDiff
	captured := &treeCapture{baseCapture: baseCapture{seen: map[string]StepContext{}}, heads: map[string]string{}}
	e.Gates = captured
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

	writeFile(t, worktree, "mine/second.txt", "after the record\n")
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-q", "-m", "second")
	if moved := strings.TrimSpace(gitOutput(t, worktree, "rev-parse", "HEAD")); moved == recorded {
		t.Fatal("premise: the worktree did not move")
	}

	_, err = e.RunDetachedSaga(conn, stepID, nowMS)
	testsupport.Must(t, err, "detached saga: %v", err)
	if len(captured.heads) == 0 {
		t.Fatal("no completion gate was dispatched")
	}
	for gate, head := range captured.heads {
		if head != recorded {
			t.Errorf("gate %s measured %q, want the recorded commit %q", gate, head, recorded)
		}
	}
	if got := issueDiffHead(t, conn, stepID); got != recorded {
		t.Errorf("issue.diff head = %q, want the recorded commit %q", got, recorded)
	}
}

// The writer's recorded worktree stays a containment root when the gate
// measures an engine-owned checkout instead.
func TestTheWritersWorktreeStaysAContainmentRootUnderAScratchCheckout(t *testing.T) {
	runRoot, scratch, worktree := t.TempDir(), t.TempDir(), t.TempDir()
	tool := filepath.Join(worktree, "bin", "tool")
	writeFile(t, worktree, "bin/tool", "#!/bin/sh\n")
	testsupport.Must(t, os.Chmod(tool, 0o755), "chmod")

	runner := NewExecRunner(RepoPaths{ExecRoot: runRoot, Identity: runRoot})
	_, err := runner.resolveContained(tool, "tool",
		StepContext{PinRoot: runRoot, WorkRoot: scratch, Worktree: worktree})
	if !errors.Is(err, docketexec.ErrRefused) {
		t.Fatalf("resolving a file inside the writer's worktree = %v; want a refusal", err)
	}
}

// A writer that committed nothing AND left its worktree clean has no work to
// lose: a checkout of the base is that worktree, so the gates measure a checkout
// of the recorded sha (the engine's), not the writer's worktree, and the record
// is not Live.
func TestAWriterThatCommittedNothingAndLeftACleanWorktreeIsBoundToABaseCheckout(t *testing.T) {
	shared := gitRepo(t)
	pinned := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "clean", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "clean", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue")
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	worktree := filepath.Join(t.TempDir(), "wt")
	runGit(t, shared, "worktree", "add", "-q", worktree)

	e := testEngine()
	captured := &treeCapture{baseCapture: baseCapture{seen: map[string]StepContext{}}, heads: map[string]string{}}
	e.Gates = captured
	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), WorkDir: worktree, NowMS: nowMS,
	}), "complete")

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	rec, ok := recordedHeadOf(conn, step)
	if !ok || rec.Live || rec.Head != pinned {
		t.Errorf("recorded head = %+v (ok %v); want the base %s, not Live", rec, ok, pinned)
	}
	if len(captured.seen) == 0 {
		t.Fatal("no completion gate was dispatched")
	}
	for gate, sc := range captured.seen {
		if sc.WorkRoot == worktree {
			t.Errorf("gate %s measured the writer's worktree; want an engine-owned checkout", gate)
		}
	}
}

// A containment root that cannot be resolved is not skipped on faith: a binary
// lexically inside it is refused, with the root reached through a symlinked
// parent too, and a binary elsewhere is not.
func TestAnUnresolvableContainmentRootStillContainsItsBinary(t *testing.T) {
	parent := t.TempDir()
	real, err := filepath.EvalSymlinks(parent)
	testsupport.Must(t, err, "EvalSymlinks: %v", err)
	alias := filepath.Join(t.TempDir(), "alias")
	testsupport.Must(t, os.Symlink(real, alias), "symlink")

	gone := filepath.Join(real, "gone")
	inside := filepath.Join(gone, "bin", "tool")
	for name, tc := range map[string]struct {
		root, path string
		want       bool
	}{
		"plain":              {gone, inside, true},
		"root through alias": {filepath.Join(alias, "gone"), inside, true},
		"the root itself":    {gone, gone, true},
		"a sibling":          {gone, filepath.Join(real, "gone-evil", "tool"), false},
		"elsewhere":          {gone, filepath.Join(t.TempDir(), "tool"), false},
	} {
		if got := rootContainsLexically(tc.root, tc.path); got != tc.want {
			t.Errorf("%s: rootContainsLexically(%q, %q) = %v, want %v", name, tc.root, tc.path, got, tc.want)
		}
	}
}

// A root that is a symlink to nowhere may point at the binary, so it contains
// it for this check.
func TestADanglingSymlinkRootStillContainsItsBinary(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "EvalSymlinks: %v", err)
	link := filepath.Join(real, "wt")
	testsupport.Must(t, os.Symlink(filepath.Join(real, "nowhere"), link), "symlink")
	if !rootContainsLexically(link, filepath.Join(real, "other", "bin", "tool")) {
		t.Error("a dangling symlink root was treated as provably elsewhere")
	}
}

// Each extra root is compared with the path that will spawn, never with a
// fresh lookup of the command name.
func TestExtraRootRefusalChecksTheResolvedPath(t *testing.T) {
	real, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "EvalSymlinks: %v", err)
	root := filepath.Join(real, "wt")
	testsupport.Must(t, os.MkdirAll(filepath.Join(root, "bin"), 0o755), "mkdir")
	inside := filepath.Join(root, "bin", "tool")
	testsupport.Must(t, os.WriteFile(inside, []byte("#!/bin/sh\n"), 0o755), "write")
	elsewhere := filepath.Join(real, "usr-tool")
	testsupport.Must(t, os.WriteFile(elsewhere, []byte("#!/bin/sh\n"), 0o755), "write")
	gone := filepath.Join(real, "gone")

	for name, tc := range map[string]struct {
		entry, resolved, extra string
		refused                bool
	}{
		"inside a resolvable root":    {"tool", inside, root, true},
		"outside a resolvable root":   {"tool", elsewhere, root, false},
		"absolute entry is trusted":   {inside, inside, root, false},
		"inside an unresolvable root": {"tool", filepath.Join(gone, "bin", "tool"), gone, true},
		"outside an unresolvable one": {"tool", elsewhere, gone, false},
	} {
		err := extraRootRefusal("tool", tc.entry, tc.resolved, tc.extra)
		if refused := errors.Is(err, docketexec.ErrRefused); refused != tc.refused {
			t.Errorf("%s: err = %v, refused = %v, want %v", name, err, refused, tc.refused)
		}
	}
}
