package engine

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Scratch-tree reconstruction for pre-gates (DKT-254).
//
// A pre-gate's subject is the tree its step is about to judge. Two situations
// leave that tree unreachable at the moment the gate would spawn:
//
//	MODE 1  A pre-claim gate resolves a target sha whose worktree does not
//	        exist — the producing step declared a sha and no tree, or the
//	        tree is on another machine. RUN-2's ac-commands recorded PASS at
//	        76f5d0c, the SHARED CHECKOUT's HEAD, while the sha under review
//	        was 2b9d9c8.
//	MODE 2  A verify step's pre-gate resolves the IMPLEMENT wave's worktree,
//	        which integration sweeps before verify runs in a later wave.
//	        Deterministic 2/2 across RUN-22 STEP-380 and RUN-27 STEP-467.
//
// Both used to end at the shared checkout, or at a park. Neither is necessary:
// THE COMMIT IS STILL IN THE OBJECT DATABASE. A worktree is a checkout of an
// object, and sweeping the checkout does not delete the object — so the tree
// can be reconstructed exactly, measured, and thrown away.
//
// WHAT THIS IS NOT. It is not a fallback that "tries its best": if the sha
// cannot be checked out, the pre-gates record `skipped` naming the sha, exactly
// as they would have without this file. Measuring a DIFFERENT tree is the
// defect; measuring no tree is merely a gap, and the two must not be traded for
// each other.

// scratchTree is a detached worktree that exists for the duration of one step's
// pre-gate phase.
type scratchTree struct {
	// Dir is the reconstructed checkout, or "" when reconstruction was not
	// attempted or did not succeed.
	Dir string
	// Cache is a scratch cache root that lives exactly as long as Dir, handed
	// to the gate's children as their linter result caches (DKT-1166).
	//
	// It is a SIBLING of the reconstruction, never a directory inside it: the
	// tree is the subject under measurement, and a cache written into it would
	// show up in `git status`, in a linter's own file walk, and in any gate
	// that hashes the tree.
	//
	// WHY IT EXISTS AT ALL. A result cache keyed by package content but
	// carrying absolute source paths outlives the tree it was written from.
	// Reconstructions are deleted within the minute, so entries written from
	// one poison every later run over the same content — the linter re-opens a
	// path that is gone, cannot find the `//nolint` comment there, and
	// re-emits an issue the source suppressed (harness RUN-64/STEP-2939).
	// Scoping the cache to the tree's own lifetime removes the carrier.
	Cache string
	// parent is the checkout whose object database holds the sha, and the one
	// that must be told to forget the worktree on removal. `git worktree
	// remove` run from anywhere else does not know about it.
	parent string
	// lock is the liveness flock this process holds on the tree's sidecar
	// lockfile for as long as the tree exists. It is what lets a later sweep
	// tell a tree whose claim is still measuring from one whose claim died:
	// the kernel drops the flock when this process exits by any means,
	// including SIGKILL, so a sweeper that can take the lock knows nobody is
	// left to release the tree. The same property the tree mutex chose flock
	// for, and for the same reason: no pid file, no stale-lock detection.
	lock *os.File
}

// scratchLockPath is the sidecar lockfile beside a scratch tree, and
// scratchCachePath its cache root: both are DERIVED from the tree's own path,
// so a sweeper that finds the tree in `git worktree list` can find everything
// that was created with it without any registry.
func scratchLockPath(dir string) string  { return dir + ".lock" }
func scratchCachePath(dir string) string { return dir + "-cache" }

// scratchPrefix is the name every scratch tree starts with, and the only
// thing the sweep matches on: a detached worktree named otherwise is
// somebody else's, whatever directory it sits in.
const scratchPrefix = "docket-pregate-"

// holdScratchLock creates the sidecar lockfile and takes its flock. It is
// taken BEFORE `git worktree add`, so there is no moment at which the tree
// exists and its holder cannot be told from a corpse.
func holdScratchLock(dir string) (*os.File, error) {
	file, err := os.OpenFile(scratchLockPath(dir),
		os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// reconstructTarget checks `sha` out into a throwaway detached worktree.
//
// It returns a zero scratchTree and NO ERROR when reconstruction is not
// possible. That is deliberate: every caller's fallback is to record `skipped`,
// which is a better outcome than failing the claim, and an error return would
// make a missing commit look like an engine fault. The reason a caller reports
// comes from the sha it asked for, not from git's stderr — an operator needs to
// know WHICH tree could not be built, and git's message about a detached HEAD
// would bury it.
func reconstructTarget(conn *sql.DB, runID int, sha string) scratchTree {
	if sha == "" {
		return scratchTree{}
	}
	parent := runExecRoot(conn, runID)
	if parent == "" {
		return scratchTree{}
	}

	// The object must be present BEFORE a worktree is attempted, because a
	// failed `worktree add` can still leave administrative files behind in
	// .git/worktrees. `cat-file -e` is the cheap, side-effect-free question.
	if err := exec.Command("git", gitDirArgs(parent,
		"cat-file", "-e", sha+"^{commit}")...).Run(); err != nil {
		return scratchTree{}
	}

	dir, err := os.MkdirTemp("", scratchPrefix)
	if err != nil {
		return scratchTree{}
	}
	// MkdirTemp creates the directory; `git worktree add` requires it not to
	// exist. Removing it and handing git the path keeps the name reserved for
	// the length of one syscall, which is as close to atomic as this gets.
	if err := os.Remove(dir); err != nil {
		return scratchTree{}
	}

	// The liveness lock comes FIRST. A tree that exists before its lock is
	// held is a tree a concurrent sweep would read as abandoned.
	lock, err := holdScratchLock(dir)
	if err != nil {
		return scratchTree{}
	}

	// --detach: no branch is created, so nothing about the repository's branch
	// namespace changes and two concurrent reconstructions of the same sha do
	// not collide on a name.
	if err := exec.Command("git", gitDirArgs(parent,
		"worktree", "add", "--detach", dir, sha)...).Run(); err != nil {
		os.RemoveAll(dir)
		lock.Close()
		os.Remove(scratchLockPath(dir))
		return scratchTree{}
	}

	// The tree-lifetime cache root (DKT-1166). Its failure is treated exactly
	// like the worktree's: no reconstruction, and the caller records `skipped`.
	// That is the same trade this file already makes everywhere else — a
	// measurement taken without it can report a suppressed issue as live, and
	// this file's whole doctrine is that measuring the WRONG thing is the
	// defect while measuring nothing is merely a gap. It is a sibling named
	// after the tree, so the sweep can find it from the tree alone.
	cache := scratchCachePath(dir)
	if err := os.Mkdir(cache, 0o700); err != nil {
		(scratchTree{Dir: dir, parent: parent, lock: lock}).release()
		return scratchTree{}
	}
	return scratchTree{Dir: dir, Cache: cache, parent: parent, lock: lock}
}

// release removes the scratch tree and its administrative record.
//
// BOTH HALVES, and in this order. `os.RemoveAll` alone leaves a stale entry in
// the parent's .git/worktrees that `git worktree list` reports forever; `git
// worktree remove` alone can decline when the tree has stray files in it, which
// a gate that wrote a log or a coverage file will have produced. `--force`
// covers the second, and the RemoveAll after it covers a `remove` that failed
// for any reason at all.
//
// Errors are ignored, and the reason is that there is nothing useful to do with
// one: the measurement already happened and its verdict is recorded, so failing
// the step over a directory that would not delete would discard real evidence
// to report a housekeeping problem. Whatever is left behind — by a failure
// here, or by a claim that was killed before reaching this defer — is
// reclaimed by sweepStalePreGateScratch at the next dispatch open or close.
func (s scratchTree) release() {
	// The cache root goes whatever else happens, and BEFORE the early return:
	// it is a sibling of the tree, so a scratchTree that never got a Dir can
	// still be holding one, and a cache left behind is the exact carrier this
	// mechanism exists to destroy (DKT-1166).
	if s.Cache != "" {
		_ = os.RemoveAll(s.Cache)
	}
	if s.Dir == "" {
		return
	}
	removeScratchTree(s.parent, s.Dir)
	// The lock goes LAST, after the tree it vouches for: a sweeper that takes
	// it must find nothing left to remove.
	if s.lock != nil {
		s.lock.Close()
	}
	_ = os.Remove(scratchLockPath(s.Dir))
}

// removeScratchTree removes one scratch worktree and its administrative
// record, in both directions, with the cache root beside it.
func removeScratchTree(parent, dir string) {
	if parent != "" {
		_ = exec.Command("git", gitDirArgs(parent,
			"worktree", "remove", "--force", dir)...).Run()
	}
	_ = os.RemoveAll(dir)
	_ = os.RemoveAll(scratchCachePath(dir))
}

// sweepStalePreGateScratch removes every scratch tree registered against
// execRoot whose creating claim is no longer alive, and reports what it
// removed.
//
// WHY IT EXISTS. release() is a defer inside the claim, and a claim the
// harness backgrounded at its tool timeout and terminated at the executor's
// turn end never reaches it. Five detached worktrees and their caches stayed
// registered across sessions; doctor reported them, and reported them only,
// because doctor is read-only by doctrine. The sweep runs at the two points
// a conductor already touches the run — dispatch open and dispatch close —
// which are the "later safe point" that makes a leaked tree bounded rather
// than permanent.
//
// LIVENESS IS THE FLOCK, NOTHING ELSE. A tree whose sidecar lock a sweeper
// can take has no holder: the kernel released it when the claim died. A tree
// whose lock is held is a claim mid-measurement, and the sweep leaves it
// alone however old it looks. A tree with NO sidecar lock predates this
// mechanism — nothing can be holding it — and is stale by the same rule. No
// pid is inspected and no age is consulted, for the reason the tree mutex
// gives: deciding whether a pid is alive is the doctrine this design retires.
//
// Best-effort throughout. A removal that fails leaves the entry for the next
// sweep and for doctor to report; nothing here can fail the dispatch, because
// housekeeping must not stand between an operator and a manifest.
func sweepStalePreGateScratch(execRoot string) []string {
	if execRoot == "" {
		return nil
	}
	out, err := exec.Command("git", gitDirArgs(execRoot,
		"worktree", "list", "--porcelain")...).Output()
	if err != nil {
		return nil
	}

	var swept []string
	for line := range strings.SplitSeq(string(out), "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok || !strings.HasPrefix(filepath.Base(path), scratchPrefix) {
			continue
		}
		lock, live := probeScratchLock(path)
		if live {
			continue
		}
		removeScratchTree(execRoot, path)
		if lock != nil {
			lock.Close()
		}
		_ = os.Remove(scratchLockPath(path))
		swept = append(swept, path)
	}
	if len(swept) > 0 {
		// An entry whose directory was already gone declines `worktree
		// remove`; prune reclaims the administrative record git kept for it.
		_ = exec.Command("git", gitDirArgs(execRoot, "worktree", "prune")...).Run()
	}
	return swept
}

// probeScratchLock reports whether a scratch tree's claim is still alive, and
// hands back the lock when the sweeper now holds it so removal can proceed
// under it.
//
// A missing lockfile is "not live": nothing can hold a lock that does not
// exist. Any other failure to open it is "live", the fail-closed direction
// for a sweeper — leaving a stale tree for the next pass costs a WARN, while
// removing a tree a claim is measuring costs a recorded verdict.
func probeScratchLock(dir string) (lock *os.File, live bool) {
	file, err := os.OpenFile(scratchLockPath(dir), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, !os.IsNotExist(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, true
	}
	return file, false
}

// bindablePreGateRoot reports the directory a step's pre-gates should measure,
// reconstructing the target when the resolved worktree cannot serve.
//
// The three answers, in the order they are decided:
//
//  1. The resolved worktree exists — use it, reconstruct nothing. This is the
//     overwhelmingly common path and it costs one stat.
//  2. It does not exist (or was never resolved) and the target sha can be
//     checked out — reconstruct, and hand back the scratch tree so the caller
//     can release it.
//  3. Neither — return "" with a scratch tree that releases to a no-op. The
//     caller records `skipped`.
//
// Case 1 is checked BEFORE case 2 even though case 2 is exact, because a live
// worktree may hold uncommitted work that the sha does not: the step's own tree
// is the subject, and a reconstruction of its last commit would silently
// measure a tree the worker has moved past.
func bindablePreGateRoot(
	conn *sql.DB, runID int, sha, worktree string,
) (dir string, scratch scratchTree) {
	if worktree != "" {
		if info, err := os.Stat(worktree); err == nil && info.IsDir() {
			return worktree, scratchTree{}
		}
	}
	scratch = reconstructTarget(conn, runID, sha)
	return scratch.Dir, scratch
}

// scratchNote is the sentence a recorded pre-gate result carries when it
// measured a reconstruction rather than a live worktree.
//
// A verdict measured in a throwaway tree is EXACTLY as good as one measured in
// the original — same objects, same content — but a reader of the row should
// not have to infer that. It also explains an absence they would otherwise find
// alarming: the tree the row names no longer exists on disk.
func scratchNote(sha, dir string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf(
		"measured a reconstruction of %s in %s; the tree under review was not "+
			"checked out anywhere reachable, so it was rebuilt from the object "+
			"database rather than substituting a different tree",
		short, filepath.Base(dir))
}

// withScratchNote appends the reconstruction note to a row's reason, keeping
// whatever the runner already recorded there.
//
// Appending rather than replacing: a failing gate's own reason is the one the
// operator needs first, and the provenance of the tree is context for it.
func withScratchNote(reason, note string) string {
	if reason == "" {
		return note
	}
	return strings.TrimRight(reason, " ") + " — " + note
}
