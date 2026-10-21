package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// An executor in a linked worktree can write refs/replace for the whole shared
// repository. A gate that measures a checkout built through that ref measures
// a different tree than the commit `step integrate` later picks.
func TestAReplaceRefDoesNotChangeWhatTheEngineReconstructs(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	base := seedGitRepo(t, repo, "measured.txt", "base")
	setRunExecRoot(t, conn, run.ID, repo)

	// H is the commit the writer records; R is a sibling with passing content.
	writeFile(t, repo, "measured.txt", "failing\n")
	gitRun(t, repo, "commit", "-q", "-am", "the recorded work")
	recorded := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "-q", "--detach", base)
	writeFile(t, repo, "measured.txt", "passing\n")
	gitRun(t, repo, "commit", "-q", "-am", "a lookalike")
	lookalike := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "replace", recorded, lookalike)

	scratch := reconstructTarget(conn, run.ID, recorded)
	if scratch.Dir == "" {
		t.Fatal("reconstruction did not produce a scratch tree")
	}
	defer scratch.release()
	got, err := os.ReadFile(filepath.Join(scratch.Dir, "measured.txt"))
	testsupport.Must(t, err, "reading the scratch tree: %v", err)
	if strings.TrimSpace(string(got)) != "failing" {
		t.Errorf("the scratch tree holds %q; a replace ref made the gate measure "+
			"the lookalike instead of the recorded commit", got)
	}
}

// The engine's git reads must not run a program the repository's config or the
// executor's environment names: a linked worktree's `git config` writes the
// shared config, and the detached child inherits the executor's environment.
func TestEngineGitRunsNoProgramTheRepositoryOrEnvironmentNames(t *testing.T) {
	repo := t.TempDir()
	seedGitRepo(t, repo, "f.txt", "one")
	writeFile(t, repo, "f.txt", "two\n")

	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "hook.sh")
	testsupport.Must(t, os.WriteFile(script,
		[]byte("#!/bin/sh\necho ran >> "+marker+"\nexit 0\n"), 0o755), "writing the script")

	gitRun(t, repo, "config", "core.fsmonitor", script)
	gitRun(t, repo, "config", "diff.external", script)
	gitRun(t, repo, "config", "diff.textconv-test.textconv", script)
	writeFile(t, repo, ".gitattributes", "*.txt diff=textconv-test\n")
	t.Setenv("GIT_EXTERNAL_DIFF", script)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", script)

	if _, err := rawDiff(repo, "HEAD", nil); err != nil {
		t.Fatalf("rawDiff: %v", err)
	}
	if _, err := gitCommand("-C", repo, "status", "--porcelain").Output(); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("an engine git read ran a program named by the repository config or the " +
			"environment (fsmonitor, external diff or textconv)")
	}
}

// A tree-holding step whose worktree has no resolvable HEAD at record time is
// refused: storing no head would send the gates and the diff to the live tree
// unpinned, so an executor could show one commit to the gates and another to
// the diff.
func TestRecordingFromAWorktreeWithNoHeadIsRefused(t *testing.T) {
	conn := mustDB(t)
	stepID, _, token := claimedImplement(t, conn)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)

	shared := gitRepo(t)
	setRunExecRoot(t, conn, step.RunID, shared)
	worktree := filepath.Join(t.TempDir(), "wt")
	gitRun(t, shared, "worktree", "add", "-q", "--detach", worktree)
	gitRun(t, worktree, "checkout", "-q", "--orphan", "unborn")

	e := testEngine()
	e.HeadFn = sharedCheckoutHead
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: token, Artifact: []byte("summary"), NowMS: nowMS, WorkDir: worktree,
	})
	if err == nil || !strings.Contains(err.Error(), "no commit resolves") {
		t.Fatalf("recording from an unborn worktree = %v; want a refusal naming the missing HEAD", err)
	}

	// The refusal spent nothing: the same lease records once the work is committed.
	writeFile(t, worktree, "w.txt", "w\n")
	gitRun(t, worktree, "add", "w.txt")
	gitRun(t, worktree, "commit", "-q", "-m", "the work")
	testsupport.Must(t, e.CompleteStep(conn, stepID, CompleteOptions{
		Token: token, Artifact: []byte("summary"), NowMS: nowMS, WorkDir: worktree,
	}), "recording after committing")
}
