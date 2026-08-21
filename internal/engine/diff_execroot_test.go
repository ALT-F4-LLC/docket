package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The diff base one resolution level deeper than a9eaebd (DKT-11).
//
// a9eaebd fixed GitDiff's base to be the SHARED checkout's HEAD rather than
// dir's own — but sourced that shared HEAD from `resolvePaths().ExecRoot`,
// the INVOKING PROCESS's cwd. Under the global store an executor runs
// `step record --worktree <checkout>` FROM INSIDE that checkout (the
// executor brief forbids cd-ing out), so ExecRoot there resolves to the
// worktree itself — the same tree `dir` names — reproducing the identical
// empty-diff bug one level deeper. The fix reads the base from the RUN's own
// exec root, captured once at `run start` (db.RunContext, G8) and immune to
// where `step record` happens to run from.

// TestRunExecRootPrefersTheRunsRecordedExecRoot pins runExecRoot's own
// contract in isolation: the run's stored exec root wins over whatever the
// current process happens to resolve, and an unrecorded one (run predates
// the field, or started outside a checkout) falls back to the process's own
// resolution — the pre-fix behavior, preserved for that case.
func TestRunExecRootPrefersTheRunsRecordedExecRoot(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "pin exec root", "body", "task", nil)

	recorded, err := db.InsertRunWithContext(conn, 1, "recorded", 0, nowMS,
		db.RunContext{ExecRoot: "/recorded/exec/root"})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, recorded.ID, issue),
		"AddRunIssue: %v", err)

	if got := runExecRoot(conn, recorded.ID); got != "/recorded/exec/root" {
		t.Errorf("runExecRoot = %q, want the run's own recorded exec root", got)
	}

	unrecorded, err := db.InsertRun(conn, 1, "unrecorded", 0, nowMS)
	testsupport.Must(t, err, "InsertRun: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, unrecorded.ID, issue),
		"AddRunIssue: %v", err)

	want := resolvePaths().ExecRoot
	if got := runExecRoot(conn, unrecorded.ID); got != want {
		t.Errorf("runExecRoot for a run with no recorded exec root = %q, "+
			"want the process's own resolution %q (the pre-fix fallback)", got, want)
	}
}

// TestRunDiffBasePrefersThePinnedCommitSHA is DKT-20's regression: the diff
// base must be the run's commit_sha, PINNED once at `run start`, never a live
// read of the shared checkout's current HEAD — a live read drifts with every
// commit that lands there after the run forked, either erasing this step's
// own diff (the checkout fast-forwards onto it) or attributing a sibling
// issue's commits to this step as deletions (the checkout advances past the
// fork on other work in the meantime).
func TestRunDiffBasePrefersThePinnedCommitSHA(t *testing.T) {
	shared := gitRepo(t)
	pinned := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))

	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "pin diff base", "body", "task", nil)

	pinnedRun, err := db.InsertRunWithContext(conn, 1, "pinned", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, pinnedRun.ID, issue),
		"AddRunIssue: %v", err)

	// The shared checkout advances PAST the pinned commit after the run
	// forked — the exact live-drift trigger this issue exists to close.
	writeFile(t, shared, "internal/sibling.txt", "a sibling issue's own commit\n")
	runGit(t, shared, "add", "-A")
	runGit(t, shared, "commit", "-qm", "sibling issue advances the shared checkout")

	if got := runDiffBase(conn, pinnedRun.ID, shared, shared); got != pinned {
		t.Errorf("runDiffBase = %q, want the run's pinned commit_sha %q — "+
			"a live read would have followed the checkout past its fork point",
			got, pinned)
	}

	unpinnedRun, err := db.InsertRunWithContext(conn, 1, "unpinned", 0, nowMS,
		db.RunContext{ExecRoot: shared})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, unpinnedRun.ID, issue),
		"AddRunIssue: %v", err)

	want := sharedCheckoutHead(shared)
	if got := runDiffBase(conn, unpinnedRun.ID, shared, shared); got != want {
		t.Errorf("runDiffBase for a run with no recorded commit_sha = %q, "+
			"want the live fallback %q (the pre-fix behavior, preserved)", got, want)
	}
}

// TestIssueDiffUsesRunsRecordedExecRootUnderGlobalStore is the live repro:
// `step record --worktree` invoked FROM INSIDE the worktree, under the
// global store (no DOCKET_PATH, no local .docket above the worktree) — the
// exact combination production hit. Before the fix this renders an empty
// diff, because resolvePaths().ExecRoot resolves to the worktree the diff is
// already comparing.
func TestIssueDiffUsesRunsRecordedExecRootUnderGlobalStore(t *testing.T) {
	shared := gitRepo(t)

	worktree := t.TempDir() + "/wt"
	runGit(t, shared, "worktree", "add", "-q", "-b", "wt-branch-execroot", worktree)

	// The executor's committed change, exactly as `step record --worktree`
	// leaves it: committed IN the worktree.
	writeFile(t, worktree, "internal/tracked.txt", "original\nCOMMITTED IN WORKTREE\n")
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-qm", "worktree change")

	// Fixture/run setup reads fixture files by RELATIVE path, so it happens
	// before the chdir below — only the claim/complete calls need to run
	// with the cwd inside the worktree.
	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "diff execroot", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "test run", 0, nowMS,
		db.RunContext{ExecRoot: shared})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue: %v", err)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.DiffFn = GitDiff // the real thing — this is what production wires.

	// The global-store resolution path: no env override, no local store, cwd
	// inside the worktree — exactly what an executor's `step record
	// --worktree <checkout>` runs under.
	t.Setenv("DOCKET_PATH", "")
	t.Chdir(worktree)

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), WorkDir: worktree, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	var body string
	err = conn.QueryRow(
		`SELECT body FROM artifacts WHERE kind = ? AND run_id = ?`,
		ArtifactKindIssueDiff, run.ID).Scan(&body)
	testsupport.Must(t, err, "reading issue.diff: %v", err)
	if !strings.Contains(body, "COMMITTED IN WORKTREE") {
		t.Errorf("issue.diff = %q, want the committed change: the base must "+
			"be the run's recorded exec root %s, never this process's own "+
			"cwd %s", body, shared, worktree)
	}
}

// TestWorktreeDiffBaseIsTheForkPoint is DKT-42's regression: a worktree is
// created at the shared checkout's CURRENT head, not at the run's pinned
// commit, so a pinned base attributes every commit the worktree merely
// INHERITED — sibling issues' work landed between run start and worktree
// creation — to this issue's diff (RUN-2 measured 32 files of it). The base
// for a worktree-recorded step is its fork point: the step's own commits
// render, the inherited ones do not.
func TestWorktreeDiffBaseIsTheForkPoint(t *testing.T) {
	shared := gitRepo(t)
	pinned := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))

	conn := mustDB(t)
	run, err := db.InsertRunWithContext(conn, 1, "worktree run", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)

	// A sibling issue lands on the shared checkout AFTER run start...
	writeFile(t, shared, "sibling/left.txt", "A SIBLING'S INHERITED WORK\n")
	runGit(t, shared, "add", "-A")
	runGit(t, shared, "commit", "-qm", "sibling issue's commit")
	fork := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))

	// ...and THEN the worktree is created, inheriting that commit.
	worktree := filepath.Join(t.TempDir(), "wt")
	runGit(t, shared, "worktree", "add", "-q", worktree)
	writeFile(t, worktree, "mine/change.txt", "THE STEP'S OWN WORK\n")
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-qm", "the step's commit")

	base := runDiffBase(conn, run.ID, worktree, shared)
	if base != fork {
		t.Errorf("runDiffBase(worktree) = %q, want the fork point %q, not "+
			"the pinned %q", base, fork, pinned)
	}

	diff, err := GitDiff(worktree, base, nil)
	testsupport.Must(t, err, "GitDiff: %v", err)
	if !strings.Contains(diff, "THE STEP'S OWN WORK") {
		t.Errorf("the step's own commit is missing from its diff\n%s", diff)
	}
	if strings.Contains(diff, "A SIBLING'S INHERITED WORK") {
		t.Errorf("an inherited sibling commit rendered in issue.diff — the "+
			"over-inclusion DKT-42 measured\n%s", diff)
	}

	// The shared checkout itself still diffs from the pinned commit: there
	// is no fork point when dir IS the exec root.
	if got := runDiffBase(conn, run.ID, shared, shared); got != pinned {
		t.Errorf("runDiffBase(exec root) = %q, want the pinned %q", got, pinned)
	}
}
