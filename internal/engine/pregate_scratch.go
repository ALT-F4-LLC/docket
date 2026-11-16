package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ALT-F4-LLC/docket/internal/store"
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
//
// THE CHECKOUT WRITES NOTHING TO THE REPOSITORY'S GIT DIRECTORY. `git worktree
// add` registers the tree under the repository's worktrees/ directory, and a
// sandbox that keeps that directory read-only failed every reconstruction into
// `skipped`. The scratch tree is instead a repository of its own:
//
//	<tmp>/docket-pregate-N        the checkout; it holds a `.git` FILE, as a
//	                              linked worktree does
//	<tmp>/docket-pregate-N.git    its git directory, whose objects/info/alternates
//	                              names the repository's object store
//	<tmp>/docket-pregate-N.lock   the liveness flock, and the record of which
//	                              repository the tree was made for
//	<tmp>/docket-pregate-N-cache  the tree-lifetime cache root
//
// It borrows the repository's objects read-only and owns its HEAD and index,
// so `git rev-parse HEAD` and a diff against the recorded base answer there
// as they did in a linked worktree. What it does not share:
//
//   - refs. The tree has a detached HEAD and nothing else; a gate is handed
//     its base as a commit id, and a branch name does not resolve there.
//   - the repository's own config, attributes outside the commit, and
//     info/exclude. A filter the REPOSITORY config names does not run on this
//     checkout, which closes that half of the filter residual gitCommand
//     records; a repository that depends on such a filter gets the unfiltered
//     bytes. User and system config still apply, except a user `includeIf
//     "gitdir:..."` section keyed on the repository's path, which no longer
//     matches a git directory under the temp directory.
//   - anything else the repository's git directory holds beyond objects and
//     the shallow boundary. A filter that keeps its own store there, as LFS
//     does, has none in the scratch git directory. This is reasoned from the
//     layout and not tested: where the user's config marks such a filter
//     required, the checkout should fail and the gate record `skipped`.
//   - protection from `git gc`. A registered worktree's HEAD kept its commit
//     alive; this one does not, for the minutes the tree exists.
//
// WHO CAN WRITE THE TREE'S GIT CONFIG. A linked worktree was governed by the
// repository's config, hooks and alternates. This tree is governed by the
// ones in its own git directory, which sits in the temp directory, mode 0700.
// Any process running as this user that can write the temp directory can
// rewrite them while the tree stands. A filter named in that config, with the
// attribute that selects it planted in info/attributes beside it, runs on the
// engine's next `git diff` in the tree, and on a gate command's own git
// calls; the commit does not have to name a filter. gitCommand cannot turn
// filters off. It turns off hooks found by path; a hook named in config is
// disabled from a listing taken before this config exists (configHookFlags),
// so one planted afterwards is not on that list. Where a sandbox keeps the
// repository's config read-only and leaves the temp directory writable to
// every sandboxed process of this user, that is a write those processes did
// not have before. The tree's files were in the same directory before and
// after, and a gate runs the commit's own code from them.
//
// A repository whose own alternates are relative paths does not chain
// through, and one moved after a tree was made no longer matches that tree's
// record. The first fails the checkout into `skipped`; the second leaves the
// tree for a person to remove, as does a claim killed after the tree's name
// was reserved and before its sidecar carried a record.
//
// A `docket` a gate runs inside the tree resolves no project from it: the
// checkout is registered with no repository, which internal/config reads as
// unanchored. A verb addressed by id still answers; a project-scoped listing
// finds no rows.

// scratchTree is a throwaway checkout of one commit that exists for the
// duration of one step's pre-gate phase or one gate stage.
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
	// lock is the liveness flock this process holds on the tree's sidecar
	// lockfile for as long as the tree exists. It is what lets a later sweep
	// tell a tree whose claim is still measuring from one whose claim died:
	// the kernel drops the flock when this process exits by any means,
	// including SIGKILL, so a sweeper that can take the lock knows nobody is
	// left to release the tree. The same property the tree mutex chose flock
	// for, and for the same reason: no pid file, no stale-lock detection.
	lock *os.File
}

// scratchLockPath is the sidecar lockfile beside a scratch tree,
// scratchGitPath the tree's git directory and scratchCachePath its cache
// root: all three are DERIVED from the tree's own path, so a sweeper that
// finds the sidecar can find everything that was created with it.
//
// The git directory is a sibling for the cache root's reason: the tree is the
// subject under measurement, and an object store inside it would be walked by
// a linter and hashed by a gate that hashes the tree.
func scratchLockPath(dir string) string  { return dir + scratchLockSuffix }
func scratchGitPath(dir string) string   { return dir + ".git" }
func scratchCachePath(dir string) string { return dir + "-cache" }

// scratchPrefix is the name every scratch tree starts with. A tree this
// mechanism makes is named scratchPrefix plus decimal digits (os.MkdirTemp),
// and its sidecar that name plus scratchLockSuffix.
const (
	scratchPrefix     = "docket-pregate-"
	scratchLockSuffix = ".lock"
)

// scratchRecordHeader is the first line of a sidecar lockfile. The second
// line is the canonical path of the object store the tree borrows from, which
// is what binds the tree to one repository: the sweep for a repository acts
// only on sidecars that name its object store.
const scratchRecordHeader = "docket-scratch 1"

// scratchRecordMax bounds what the sweep reads from a sidecar it has not yet
// recognized as its own.
const scratchRecordMax = 8 << 10

// isScratchName reports whether name is one os.MkdirTemp produced from
// scratchPrefix: the prefix and decimal digits, nothing else. The cache root
// (`-cache`), the git directory (`.git`) and the legacy cache roots
// (`docket-pregate-cache-*`) all fail it.
func isScratchName(name string) bool {
	digits, ok := strings.CutPrefix(name, scratchPrefix)
	if !ok || digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// holdScratchLock creates the sidecar lockfile, takes its flock, and writes
// the record naming the object store. It runs BEFORE anything else is made
// for the tree, so everything a claim killed mid-checkout leaves behind has a
// sidecar the sweep can read.
//
// The record is written only once the flock is held. A sidecar that carries a
// record therefore has a holder or had one, and the sweep never takes the
// flock of a sidecar without a record: it would otherwise win the lock from a
// claim that had created the file and not yet locked it.
//
// O_EXCL: the name was minted a moment ago, so a file already under it is
// never this claim's. Adopting one would let whoever planted it hold the
// flock, or keep a handle on a record the sweep trusts.
func holdScratchLock(dir, objectStore string) (*os.File, error) {
	file, err := os.OpenFile(scratchLockPath(dir),
		os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_, err = file.WriteAt([]byte(scratchRecordHeader+"\n"+objectStore+"\n"), 0)
	}
	if err != nil {
		file.Close()
		_ = os.Remove(scratchLockPath(dir))
		return nil, err
	}
	return file, nil
}

// scratchSource is what a scratch tree takes from the repository it is made
// for. Every checkout of one repository answers with the same values.
type scratchSource struct {
	// objects is the repository's object store as a canonical path: what the
	// tree's alternates file names and what its sidecar records.
	objects string
	// shallow is where the repository keeps its shallow boundary, whether or
	// not the file exists.
	shallow string
	// format is the hash the object store uses.
	format string
}

// scratchSourceOf asks execRoot's repository for its scratchSource.
func scratchSourceOf(execRoot string) (scratchSource, bool) {
	out, err := gitCommand(gitDirArgs(execRoot, "rev-parse", "--path-format=absolute",
		"--git-path", "objects", "--git-path", "shallow", "--show-object-format")...).Output()
	if err != nil {
		return scratchSource{}, false
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 3 || lines[0] == "" || lines[1] == "" || lines[2] == "" {
		// A path holding a newline lands here too: an alternates file and a
		// sidecar record are both line-oriented and cannot name it.
		return scratchSource{}, false
	}
	objects, err := filepath.EvalSymlinks(lines[0])
	if err != nil {
		return scratchSource{}, false
	}
	return scratchSource{objects: objects, shallow: lines[1], format: lines[2]}, true
}

// reconstructTarget checks `sha` out into a throwaway repository of its own
// that borrows the run's objects (see the file comment for the layout).
//
// It returns a zero scratchTree and NO ERROR when reconstruction is not
// possible. That is deliberate: every caller's fallback is to record `skipped`,
// which is a better outcome than failing the claim, and an error return would
// make a missing commit look like an engine fault. The reason a caller reports
// comes from the sha it asked for, not from git's stderr — an operator needs to
// know WHICH tree could not be built, and git's message about a detached HEAD
// would bury it.
func reconstructTarget(conn store.LegacyDB, runID int, sha string) scratchTree {
	if sha == "" {
		return scratchTree{}
	}
	parent := runExecRoot(conn, runID)
	if parent == "" {
		return scratchTree{}
	}

	// The commit must be present BEFORE anything is created, and it is
	// resolved to its full id HERE: the scratch repository has no refs, so an
	// abbreviation or a name the caller holds means nothing inside it.
	// Peeling to ^{commit} reads the object, so a missing one fails the
	// command rather than echoing the id back.
	out, err := gitCommand(gitDirArgs(parent,
		"rev-parse", "--verify", "--quiet", "--end-of-options", sha+"^{commit}")...).Output()
	commit := strings.TrimSpace(string(out))
	if err != nil || commit == "" {
		return scratchTree{}
	}
	source, ok := scratchSourceOf(parent)
	if !ok {
		return scratchTree{}
	}

	// MkdirTemp reserves the name and leaves the directory 0700. The path is
	// made absolute because git is handed it twice, once as the directory to
	// run in and once as an argument read from inside that directory.
	dir, err := os.MkdirTemp("", scratchPrefix)
	if err != nil {
		return scratchTree{}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}

	// The liveness lock comes FIRST, with its record. The sweep finds trees
	// through their sidecars, so a directory that has none yet is invisible to
	// it rather than abandoned-looking.
	lock, err := holdScratchLock(dir, source.objects)
	if err != nil {
		_ = os.Remove(dir)
		return scratchTree{}
	}

	// The git directory and the tree-lifetime cache root (DKT-1166) are made
	// before anything is put in them, so that release removes only what this
	// claim made. A failure here is treated exactly like the checkout's: no
	// reconstruction, and the caller records `skipped`. That is the same trade
	// this file already makes everywhere else — a measurement taken without
	// the cache can report a suppressed issue as live, and this file's whole
	// doctrine is that measuring the WRONG thing is the defect while measuring
	// nothing is merely a gap.
	if !reserveScratchSiblings(dir) {
		_ = os.Remove(dir)
		_ = os.Remove(scratchLockPath(dir))
		lock.Close()
		return scratchTree{}
	}
	tree := scratchTree{Dir: dir, Cache: scratchCachePath(dir), lock: lock}

	if !checkoutScratch(dir, source, commit) {
		tree.release()
		return scratchTree{}
	}
	return tree
}

// reserveScratchSiblings makes the git directory and the cache root beside the
// tree, each 0700, and reports whether both are now this claim's.
//
// MkdirTemp reserved the tree's name and nobody reserved these two. Mkdir
// fails on anything already under either: a symbolic link planted at the git
// directory's name would otherwise send `git init`, and every step after it,
// into whatever repository the link names. What was already there is left
// there; it is not this claim's to remove.
func reserveScratchSiblings(dir string) bool {
	if err := os.Mkdir(scratchGitPath(dir), 0o700); err != nil {
		return false
	}
	if err := os.Mkdir(scratchCachePath(dir), 0o700); err != nil {
		_ = os.Remove(scratchGitPath(dir))
		return false
	}
	return true
}

// checkoutScratch turns the reserved directory into a checkout of commit, in
// steps that each write only beside or inside it:
//
//  1. The git directory must already be a real directory this user owns,
//     the one reserveScratchSiblings made. Anything else under that name
//     stops the checkout before git is told the name.
//  2. `git init` into that directory as the tree's separate git dir.
//     `--template=` keeps a template directory the user's or the system's
//     config names from copying hooks in, and the hash is the repository's,
//     since the default need not match.
//  3. objects/info/alternates names the repository's object store. Git reads
//     it; nothing is written there. A shallow repository's boundary file is
//     copied beside it, or history walks in the tree would run off the end
//     of the borrowed objects.
//  4. `update-ref --no-deref HEAD` detaches HEAD at the commit: no branch is
//     created, and the id is verified against the borrowed objects.
//  5. `read-tree -m -u` fills the index and the tree. It runs no
//     post-checkout hook and writes no reflog; the post-index-change hook it
//     would fire is stopped by gitCommand's hooks-off, as the
//     reference-transaction hook of step 4 is. `-m` rather than `--reset`,
//     so a file that is somehow already in the tree fails the checkout
//     instead of being overwritten.
//
// Every git call is gitCommand's, unchanged. From step 4 on the git directory
// and the work tree are named outright rather than discovered: a tree whose
// `.git` file was lost would otherwise hand the command to a repository that
// encloses the temp directory.
//
// A step that fails leaves what it made for the caller's release to remove.
func checkoutScratch(dir string, source scratchSource, commit string) bool {
	gitDir := scratchGitPath(dir)
	if owned, _ := ownedScratchDir(gitDir); !owned {
		return false
	}
	if err := gitCommand(gitDirArgs(dir, "init", "--quiet", "--template=",
		"--object-format="+source.format, "--separate-git-dir="+gitDir)...).Run(); err != nil {
		return false
	}
	if err := os.WriteFile(filepath.Join(gitDir, "objects", "info", "alternates"),
		[]byte(source.objects+"\n"), 0o600); err != nil {
		return false
	}
	// Only a regular file is read: a pipe or a device under that name would
	// hold the claim for as long as it kept producing bytes.
	switch info, err := os.Stat(source.shallow); {
	case err == nil:
		if !info.Mode().IsRegular() {
			return false
		}
		boundary, err := os.ReadFile(source.shallow)
		if err != nil {
			return false
		}
		if err := os.WriteFile(filepath.Join(gitDir, "shallow"), boundary, 0o600); err != nil {
			return false
		}
	case !os.IsNotExist(err):
		return false
	}
	pinned := func(args ...string) []string {
		return append([]string{"-C", dir, "--git-dir=" + gitDir, "--work-tree=" + dir}, args...)
	}
	if err := gitCommand(pinned(
		"update-ref", "--no-deref", "HEAD", commit)...).Run(); err != nil {
		return false
	}
	return gitCommand(pinned("read-tree", "-m", "-u", commit)...).Run() == nil
}

// gateTreeCache is the one checkout the completion-gate stages of a single
// saga resume share: each stage asks for the step's recorded sha, and a
// five-gate step otherwise materializes the same commit five times. It is
// per-process state for the lifetime of one resumeSaga call, never persisted;
// the tree it holds carries the scratch flock like any other, so a process
// that dies with it leaves it to sweepStalePreGateScratch. The diff stage
// does not share it: a gate may have written build output into the tree, and
// GitDiff reads untracked files.
type gateTreeCache struct {
	runID int
	sha   string
	tree  scratchTree
}

// checkout hands back a checkout of sha, reusing the held one when it is of
// the same (runID, sha) and replacing it otherwise, with the release the
// caller owes: a no-op for a cached tree (the cache releases it), the tree's
// own release when there is no cache.
func (c *gateTreeCache) checkout(conn store.LegacyDB, runID int, sha string) (scratchTree, func()) {
	if c == nil {
		tree := reconstructTarget(conn, runID, sha)
		return tree, tree.release
	}
	if c.tree.Dir == "" || c.runID != runID || c.sha != sha {
		c.release()
		c.runID, c.sha, c.tree = runID, sha, reconstructTarget(conn, runID, sha)
	}
	return c.tree, func() {}
}

// release removes the held checkout, if any.
func (c *gateTreeCache) release() {
	if c == nil {
		return
	}
	c.tree.release()
	c.tree = scratchTree{}
}

// release removes the scratch tree and everything made with it.
//
// Nothing outside the temp directory knows the tree exists, so removal is
// deleting its directories: no git command runs and the repository is not
// touched.
//
// Errors are ignored, and the reason is that there is nothing useful to do with
// one: the measurement already happened and its verdict is recorded, so failing
// the step over a directory that would not delete would discard real evidence
// to report a housekeeping problem. Whatever is left behind — by a failure
// here, or by a claim that was killed before reaching this defer — keeps its
// sidecar, and sweepStalePreGateScratch tries it again at the next dispatch
// open or close. The sidecar is the only index of these trees, so it is
// unlinked only once nothing it vouches for is left.
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
	// The lock goes LAST, after the tree it vouches for, and only when all of
	// that tree is gone: a sweeper that takes a sidecar must find either
	// nothing left to remove or the remains this call could not. It is
	// unlinked before the flock is dropped, so no sweeper can take a sidecar
	// this call is about to unlink.
	if removeScratchTree(s.Dir) {
		_ = os.Remove(scratchLockPath(s.Dir))
	}
	if s.lock != nil {
		s.lock.Close()
	}
}

// removeScratchTree removes one scratch tree with the git directory and the
// cache root beside it, and reports whether all three are now gone.
// os.RemoveAll opens each directory O_NOFOLLOW and unlinks by name, so a
// symbolic link at any of the three paths, or inside them, is removed as a
// link and never followed. It does not change permissions: a directory in the
// tree that is not writable keeps its contents, and the answer is false.
func removeScratchTree(dir string) bool {
	gone := true
	for _, path := range []string{dir, scratchGitPath(dir), scratchCachePath(dir)} {
		_ = os.RemoveAll(path)
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			gone = false
		}
	}
	return gone
}

// sweepStalePreGateScratch removes every scratch tree made for execRoot's
// repository whose creating claim is no longer alive, and reports what it
// removed.
//
// WHY IT EXISTS. release() is a defer inside the claim, and a claim the
// harness backgrounded at its tool timeout and terminated at the executor's
// turn end never reaches it. Five scratch trees and their caches outlived
// their sessions; doctor reported them, and reported them only, because
// doctor is read-only by doctrine. The sweep runs at the two points a
// conductor already touches the run — dispatch open and dispatch close —
// which are the "later safe point" that makes a leaked tree bounded rather
// than permanent.
//
// LIVENESS IS THE FLOCK, NOTHING ELSE. A tree whose sidecar lock a sweeper
// can take has no holder: the kernel released it when the claim died. A tree
// whose lock is held is a claim mid-measurement, and the sweep leaves it
// alone however old it looks. No pid is inspected and no age is consulted,
// for the reason the tree mutex gives: deciding whether a pid is alive is the
// doctrine this design retires.
//
// TWO KINDS OF TREE, found two ways. A tree this binary makes is in no git
// registry; its sidecar names the repository, and sweepRecordedScratch reads
// the temp directory for those. A tree an earlier binary made with `git
// worktree add` is registered with the repository and may have no sidecar at
// all; sweepRegisteredScratch still finds those where git lists them.
//
// Best-effort throughout. A removal that fails leaves the tree, with its
// sidecar, for the next sweep; nothing here can fail the dispatch, because
// housekeeping must not stand between an operator and a manifest. Doctor's
// straggler check reports both kinds: the registered kind from git's
// worktree list, and the recorded kind, including one that will not delete,
// through recordedScratchTrees.
//
// Legacy caches are swept first and independently of execRoot; see
// sweepLegacyPreGateCaches. They are not part of the returned list, which
// names scratch trees only.
func sweepStalePreGateScratch(execRoot string) []string {
	sweepLegacyPreGateCaches()
	if execRoot == "" {
		return nil
	}
	return append(sweepRegisteredScratch(execRoot), sweepRecordedScratch(execRoot)...)
}

// sweepRegisteredScratch removes the dead scratch trees an earlier binary
// registered as worktrees of execRoot's repository. Ownership is git's
// registry plus the prefix: a detached worktree named otherwise is somebody
// else's. That is the proof the earlier binary's sweep had, and it is weaker
// than sweepRecordedScratch's: whoever can write the repository's worktree
// registry can name a path. `git worktree remove` acts on the registry's
// word; what this function removes itself, it removes only as a real
// directory this user owns. A tree with NO sidecar lock predates the liveness
// lock — nothing can be holding it — and is stale by the flock rule.
func sweepRegisteredScratch(execRoot string) []string {
	out, err := gitCommand(gitDirArgs(execRoot,
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
		_ = gitCommand(gitDirArgs(execRoot,
			"worktree", "remove", "--force", path)...).Run()
		// The tree and its cache, which is all such a tree was made with. Not
		// removeScratchTree: a registered worktree keeps its git directory in
		// the repository, and a `<path>.git` beside it is not this tree's.
		for _, left := range []string{path, scratchCachePath(path)} {
			if owned, _ := ownedScratchDir(left); owned {
				_ = os.RemoveAll(left)
			}
		}
		if lock != nil {
			lock.Close()
		}
		_ = os.Remove(scratchLockPath(path))
		swept = append(swept, path)
	}
	if len(swept) > 0 {
		// An entry whose directory was already gone declines `worktree
		// remove`; prune reclaims the administrative record git kept for it.
		_ = gitCommand(gitDirArgs(execRoot, "worktree", "prune")...).Run()
	}
	return swept
}

// scratchSidecars lists the paths in os.TempDir() that carry a scratch
// sidecar's name. The name is all it checks.
func scratchSidecars() []string {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), scratchLockSuffix)
		if ok && isScratchName(name) {
			paths = append(paths, filepath.Join(tmp, entry.Name()))
		}
	}
	return paths
}

// openScratchRecord opens a sidecar and returns the object store its record
// names, or "" for a file that is not a record this engine wrote. The caller
// closes the file.
//
// O_NOFOLLOW refuses a symbolic link under the sidecar's name. The file must
// be a regular one this user owns: a temp directory is shared, and another
// user's file under the same name is theirs to explain.
func openScratchRecord(lockPath string) (*os.File, string) {
	file, err := os.OpenFile(lockPath, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ""
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByThisUser(info) {
		file.Close()
		return nil, ""
	}
	buf := make([]byte, scratchRecordMax)
	n, _ := file.ReadAt(buf, 0)
	header, rest, ok := strings.Cut(string(buf[:n]), "\n")
	if !ok || header != scratchRecordHeader {
		return file, ""
	}
	objectStore, _, ok := strings.Cut(rest, "\n")
	if !ok {
		return file, ""
	}
	return file, objectStore
}

// scratchEuid is the user this process runs as. A variable so a test can
// stand in another user and see the ownership checks refuse.
var scratchEuid = os.Geteuid

// ownedByThisUser reports whether the file belongs to the user this process
// runs as.
func ownedByThisUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(scratchEuid())
}

// ownedScratchDir reports whether path is a real directory this user owns.
// Lstat, so a symbolic link is not a directory whatever it points at.
func ownedScratchDir(path string) (owned, absent bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, os.IsNotExist(err)
	}
	return info.IsDir() && ownedByThisUser(info), false
}

// recordedScratchTrees lists the scratch trees whose sidecar names execRoot's
// repository, whether or not their claim is alive. It takes no lock and
// removes nothing. Doctor's straggler check (checkDoctorStragglers) uses it
// to report standing trees.
func recordedScratchTrees(execRoot string) []string {
	source, ok := scratchSourceOf(execRoot)
	if !ok {
		return nil
	}
	var trees []string
	for _, lockPath := range scratchSidecars() {
		file, recorded := openScratchRecord(lockPath)
		if file != nil {
			file.Close()
		}
		dir := strings.TrimSuffix(lockPath, scratchLockSuffix)
		if owned, _ := ownedScratchDir(dir); recorded == source.objects && owned {
			trees = append(trees, dir)
		}
	}
	return trees
}

// sweepRecordedScratch removes the dead scratch trees this mechanism made for
// execRoot's repository, and returns the trees it removed.
//
// IT DELETES ONLY WHAT IT CAN PROVE THE ENGINE MADE FOR THIS REPOSITORY. The
// temp directory is shared with every other program and, on some machines,
// every other user, so a matching name proves nothing. For each name the
// chain is, in order:
//
//  1. The sidecar's name is exactly the one os.MkdirTemp and
//     scratchLockPath produce, directly in os.TempDir().
//  2. The sidecar is a regular file this user owns, opened without
//     following a link, and its record names this repository's object
//     store. A sidecar for another repository is that repository's sweep's.
//  3. Its flock can be taken: no claim holds the tree. The flock is asked
//     for only after the record is read (see holdScratchLock).
//  4. The sidecar still sits at its path. One another sweeper finished and
//     unlinked can no longer vouch for whatever holds that name now.
//  5. The tree, its git directory and its cache root are each a real
//     directory this user owns, or already gone. A symbolic link, a file or
//     another user's directory under any of the three names stops the
//     removal of all of them: everything is left where it is, with the
//     sidecar, for a person to look at.
//
// Removal then never leaves the temp directory: three derived names
// directly in it, each removed without following a link (removeScratchTree).
// The sidecar goes only when all three are gone, and the tree is reported
// only then; what would not delete keeps its sidecar for the next sweep.
//
// WHAT THE PROOF DOES NOT COVER. A program running as this user can forge
// every step, and can unlink a live tree's sidecar and put an unheld one in
// its place, which has the sweep remove a live tree. It gains nothing by
// that: the sweep removes only real directories this user owns, under these
// three names directly in the temp directory, and that program can remove
// those itself. Between step 5 and the removal another user can still create
// a directory under a name that was absent; its contents go as far as that
// user's own permissions on it allow.
//
// A sidecar whose tree is gone is debris from a claim killed between the lock
// and the checkout, or mid-release; it is removed with whatever siblings it
// has and not reported.
//
// REACH. This reads the sweeping process's own os.TempDir(). A tree made by
// a claim that ran under a different TMPDIR is not found from here, where
// git's registry found a registered worktree from anywhere.
func sweepRecordedScratch(execRoot string) []string {
	source, ok := scratchSourceOf(execRoot)
	if !ok {
		return nil
	}
	var swept []string
	for _, lockPath := range scratchSidecars() {
		file, recorded := openScratchRecord(lockPath)
		if file == nil {
			continue
		}
		if recorded != source.objects ||
			syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil ||
			!sidecarStillAt(file, lockPath) {
			file.Close()
			continue
		}
		dir := strings.TrimSuffix(lockPath, scratchLockSuffix)
		owned, absent := ownedScratchDir(dir)
		provable := owned || absent
		for _, sibling := range []string{scratchGitPath(dir), scratchCachePath(dir)} {
			siblingOwned, siblingAbsent := ownedScratchDir(sibling)
			provable = provable && (siblingOwned || siblingAbsent)
		}
		if !provable {
			file.Close()
			continue
		}
		if !removeScratchTree(dir) {
			file.Close()
			continue
		}
		// Unlinked while the flock is still held, so nobody can take the
		// sidecar between the two.
		_ = os.Remove(lockPath)
		file.Close()
		if owned {
			swept = append(swept, dir)
		}
	}
	return swept
}

// sidecarStillAt reports whether lockPath still names the file the sweeper
// holds open.
func sidecarStillAt(file *os.File, lockPath string) bool {
	held, err := file.Stat()
	if err != nil {
		return false
	}
	now, err := os.Lstat(lockPath)
	return err == nil && os.SameFile(held, now)
}

// legacyCachePrefix names the cache roots an earlier binary created with
// os.MkdirTemp("", "docket-pregate-cache-*"), before caches were named after
// their tree.
const legacyCachePrefix = "docket-pregate-cache-"

// sweepLegacyPreGateCaches removes every legacy cache directory directly in
// os.TempDir().
//
// No tree derives these names, so the flock rule cannot reach them, and none
// is current: the binary that wrote them had no liveness lock and this one
// never creates the name. A current tree is docket-pregate-<digits> and its
// cache docket-pregate-<digits>-cache, so the prefix cannot match either.
// Best-effort, like the sweep that calls it.
func sweepLegacyPreGateCaches() {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), legacyCachePrefix) {
			_ = os.RemoveAll(filepath.Join(tmp, entry.Name()))
		}
	}
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
	return probeLiveLock(scratchLockPath(dir))
}

// probeLiveLock is probeScratchLock's mechanism: a liveness lockfile read the
// SWEEPER's way, where a lockfile that cannot be opened or locked is "live"
// because the cost of a wrong "dead" is a tree removed under a measurement.
// The detached pre-gate run's in-flight lock reads the other way
// (detachedPreGateHeld, pregate_detached.go): there a wrong "live" is a step
// held forever, so only a flock another holder refuses counts.
func probeLiveLock(path string) (lock *os.File, live bool) {
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
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
	conn store.LegacyDB, runID int, sha, worktree string,
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
