package engine

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// parent is the checkout whose object database holds the sha, and the one
	// that must be told to forget the worktree on removal. `git worktree
	// remove` run from anywhere else does not know about it.
	parent string
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

	dir, err := os.MkdirTemp("", "docket-pregate-")
	if err != nil {
		return scratchTree{}
	}
	// MkdirTemp creates the directory; `git worktree add` requires it not to
	// exist. Removing it and handing git the path keeps the name reserved for
	// the length of one syscall, which is as close to atomic as this gets.
	if err := os.Remove(dir); err != nil {
		return scratchTree{}
	}

	// --detach: no branch is created, so nothing about the repository's branch
	// namespace changes and two concurrent reconstructions of the same sha do
	// not collide on a name.
	if err := exec.Command("git", gitDirArgs(parent,
		"worktree", "add", "--detach", dir, sha)...).Run(); err != nil {
		os.RemoveAll(dir)
		return scratchTree{}
	}
	return scratchTree{Dir: dir, parent: parent}
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
// to report a housekeeping problem. `git worktree prune` reclaims anything left
// behind.
func (s scratchTree) release() {
	if s.Dir == "" {
		return
	}
	if s.parent != "" {
		_ = exec.Command("git", gitDirArgs(s.parent,
			"worktree", "remove", "--force", s.Dir)...).Run()
	}
	_ = os.RemoveAll(s.Dir)
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
