package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A pre-gate scratch tree must not outlive its claim.
//
// release() is a defer inside the claim. A claim the harness backgrounded at
// its tool timeout and terminated at the executor's turn end never reaches
// it, and the detached worktree, its cache, and its administrative record in
// the parent's .git/worktrees stayed registered across sessions with doctor
// able only to report them. The sweep at dispatch open and close reclaims
// exactly the trees whose claim is dead, and liveness is the sidecar flock —
// released by the kernel on SIGKILL — not a pid or an age.

// listedScratchTrees returns the scratch-prefixed worktrees the repo has
// registered.
func listedScratchTrees(t *testing.T, repo string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	testsupport.Must(t, err, "git worktree list: %v", err)
	var trees []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok &&
			strings.HasPrefix(filepath.Base(path), scratchPrefix) {
			trees = append(trees, path)
		}
	}
	return trees
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestKilledClaimScratchIsSweptAtDispatchOpen is the acceptance case: the
// claim holding a scratch tree is SIGKILLed mid-pre-gate, and the next
// dispatch open leaves no scratch entry in `git worktree list` and no
// directory under the scratch root. While the claim is alive, the same sweep
// leaves the tree alone.
func TestKilledClaimScratchIsSweptAtDispatchOpen(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	sha := seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)

	scratch := reconstructTarget(conn, run.ID, sha)
	if scratch.Dir == "" || scratch.lock == nil {
		t.Fatalf("reconstruction did not produce a locked scratch tree: %+v", scratch)
	}
	lockPath, cachePath := scratchLockPath(scratch.Dir), scratchCachePath(scratch.Dir)

	// The lock must be held by a process this test can KILL, so that "the
	// kernel released it" is what the sweep below actually observes. The
	// locked fd is inherited by a child and the local handle closed, so no
	// cleanup code of ours can ever release it — only the child's death.
	holder := exec.Command("/bin/sleep", "60")
	holder.ExtraFiles = []*os.File{scratch.lock}
	err := holder.Start()
	testsupport.Must(t, err, "starting the lock holder: %v", err)
	scratch.lock.Close()
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })

	// ALIVE: the sweep must leave a tree whose claim still holds its lock.
	if swept := sweepStalePreGateScratch(repoRoot); len(swept) != 0 {
		t.Fatalf("the sweep removed %v while its claim was alive", swept)
	}
	if got := listedScratchTrees(t, repoRoot); len(got) != 1 || got[0] != canonical(t, scratch.Dir) {
		t.Fatalf("scratch trees after a sweep under a live claim = %v, want [%s]", got, scratch.Dir)
	}

	// KILL -9. No defer runs, no handler fires.
	err = holder.Process.Signal(syscall.SIGKILL)
	testsupport.Must(t, err, "killing the lock holder: %v", err)
	holder.Wait()

	// DEAD: the next dispatch open reclaims everything the claim created.
	_, err = NewEngine().OpenDispatch(conn, run.ID, 10, nil, nowMS)
	testsupport.Must(t, err, "OpenDispatch: %v", err)

	if got := listedScratchTrees(t, repoRoot); len(got) != 0 {
		t.Errorf("scratch trees still registered after dispatch open: %v", got)
	}
	for _, p := range []string{scratch.Dir, cachePath, lockPath} {
		if exists(p) {
			t.Errorf("%s survived the sweep", p)
		}
	}
}

// TestScratchWithoutASidecarLockIsSwept covers trees left by a binary that
// predates the liveness lock: nothing can be holding a lock that does not
// exist, so they are stale by the same rule and go on the first sweep.
func TestScratchWithoutASidecarLockIsSwept(t *testing.T) {
	repoRoot := t.TempDir()
	sha := seedGitRepo(t, repoRoot, "measured.txt", "under review")

	legacy := filepath.Join(t.TempDir(), scratchPrefix+"legacy")
	err := exec.Command("git", "-C", repoRoot, "worktree", "add", "--detach", legacy, sha).Run()
	testsupport.Must(t, err, "git worktree add: %v", err)

	// Resolved BEFORE the sweep: git reports the resolved path, and a path
	// that is gone cannot be resolved afterwards.
	want := canonical(t, legacy)
	swept := sweepStalePreGateScratch(repoRoot)
	if len(swept) != 1 || swept[0] != want {
		t.Errorf("swept = %v, want [%s]", swept, want)
	}
	if got := listedScratchTrees(t, repoRoot); len(got) != 0 {
		t.Errorf("the lockless scratch tree is still registered: %v", got)
	}
	if exists(legacy) {
		t.Errorf("%s survived the sweep", legacy)
	}
}

// TestReleaseRemovesTheSidecarLock: the normal path leaves nothing behind
// either — tree, cache, and the lock that vouched for them.
func TestReleaseRemovesTheSidecarLock(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repoRoot := t.TempDir()
	sha := seedGitRepo(t, repoRoot, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repoRoot)

	scratch := reconstructTarget(conn, run.ID, sha)
	if scratch.Dir == "" {
		t.Fatal("reconstruction did not produce a scratch tree")
	}
	if !exists(scratchLockPath(scratch.Dir)) || !exists(scratch.Cache) {
		t.Fatalf("a live scratch tree must have its lock and cache beside it")
	}
	scratch.release()
	for _, p := range []string{scratch.Dir, scratch.Cache, scratchLockPath(scratch.Dir)} {
		if exists(p) {
			t.Errorf("%s survived release()", p)
		}
	}
	if got := listedScratchTrees(t, repoRoot); len(got) != 0 {
		t.Errorf("release() left the tree registered: %v", got)
	}
}
