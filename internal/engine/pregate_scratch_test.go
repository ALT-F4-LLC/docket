package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A scratch checkout is a repository of its own beside the temp directory's
// other entries: it borrows the run's objects and writes nothing to the
// repository's git directory. These tests pin the three things that design
// has to keep true — the git directory stays untouched, the tree still
// answers a diff, and the sweep finds and removes only what the engine made.

// gitDirListing is every path under repo's git directory, relative to it.
func gitDirListing(t *testing.T, repo string) []string {
	t.Helper()
	root := filepath.Join(repo, ".git")
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		paths = append(paths, rel)
		return err
	})
	testsupport.Must(t, err, "walking %s: %v", root, err)
	return paths
}

// isolateTempDir points os.TempDir() at a directory of this test's own, so a
// sweep reads only the sidecars the test put there.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	return tmp
}

// abandonScratch is what a killed claim leaves: every path the tree was made
// with, and nobody holding the flock. Closing the handle is the kernel's half
// of a SIGKILL; release() never runs.
func abandonScratch(t *testing.T, tree scratchTree) {
	t.Helper()
	testsupport.Must(t, tree.lock.Close(), "dropping the scratch lock")
}

// scratchPaths is everything one scratch tree is made of.
func scratchPaths(dir string) []string {
	return []string{dir, scratchGitPath(dir), scratchCachePath(dir), scratchLockPath(dir)}
}

// TestScratchCheckoutWritesNothingToTheGitDir: with the repository's
// worktrees/ directory not writable, the pre-gate checkout and the gate
// resume checkout each produce a tree at the requested commit, and the git
// directory holds exactly what it held before.
//
// The listing is asserted as well as the checkout's success, so the test
// does not pass vacuously for a user the mode bits do not stop: a checkout
// that registered a worktree would succeed as root and still add an entry.
func TestScratchCheckoutWritesNothingToTheGitDir(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	sha := seedGitRepo(t, repo, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repo)

	worktrees := filepath.Join(repo, ".git", "worktrees")
	testsupport.Must(t, os.Mkdir(worktrees, 0o555), "making worktrees/ read-only")
	t.Cleanup(func() { _ = os.Chmod(worktrees, 0o755) })
	before := gitDirListing(t, repo)

	checkouts := []struct {
		name     string
		checkout func() (scratchTree, func())
	}{
		{"pre-gate", func() (scratchTree, func()) {
			gone := filepath.Join(t.TempDir(), "swept-worktree")
			dir, tree := bindablePreGateRoot(conn, run.ID, sha, gone)
			if dir != tree.Dir {
				t.Errorf("the pre-gate bound %q, not its scratch tree %q", dir, tree.Dir)
			}
			return tree, tree.release
		}},
		{"gate resume", func() (scratchTree, func()) {
			trees := &gateTreeCache{}
			tree, _ := trees.checkout(conn, run.ID, sha)
			return tree, trees.release
		}},
	}
	for _, c := range checkouts {
		t.Run(c.name, func(t *testing.T) {
			tree, release := c.checkout()
			defer release()
			if tree.Dir == "" {
				t.Fatal("the checkout failed with worktrees/ not writable")
			}
			if got := gitRun(t, tree.Dir, "rev-parse", "HEAD"); got != sha {
				t.Errorf("git rev-parse HEAD in the scratch tree = %s, want the requested commit %s", got, sha)
			}
			entries, err := os.ReadDir(worktrees)
			testsupport.Must(t, err, "reading worktrees/: %v", err)
			if len(entries) != 0 {
				t.Errorf("the checkout added %d entries under the git directory's worktrees/", len(entries))
			}
			if after := gitDirListing(t, repo); !slices.Equal(before, after) {
				t.Errorf("the checkout changed the repository's git directory:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// TestScratchCheckoutKeepsTheDiff: the diff stage runs GitDiff inside the
// scratch tree, and GitDiff answers a directory that is not a checkout with
// an empty diff and no error. A tree exported without git metadata would
// record "nothing changed" for every step.
func TestScratchCheckoutKeepsTheDiff(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	base := seedGitRepo(t, repo, "measured.txt", "one")
	writeFile(t, repo, "measured.txt", "two\n")
	gitRun(t, repo, "commit", "-q", "-am", "the change under review")
	head := gitRun(t, repo, "rev-parse", "HEAD")
	setRunExecRoot(t, conn, run.ID, repo)

	tree := reconstructTarget(conn, run.ID, head)
	if tree.Dir == "" {
		t.Fatal("reconstruction did not produce a scratch tree")
	}
	defer tree.release()

	body, err := GitDiff(tree.Dir, base, nil)
	testsupport.Must(t, err, "GitDiff: %v", err)
	if !strings.Contains(body, "-one") || !strings.Contains(body, "+two") {
		t.Errorf("the diff against the recorded base does not carry the change:\n%q", body)
	}
	// Against its own commit the tree is clean: the index matches HEAD and
	// the checkout left nothing untracked for the diff to report.
	own, err := GitDiff(tree.Dir, "", nil)
	testsupport.Must(t, err, "GitDiff against HEAD: %v", err)
	if own != "" {
		t.Errorf("a fresh scratch tree differs from its own commit:\n%s", own)
	}
}

// TestScratchCheckoutResolvesAnAbbreviatedCommit: the scratch repository has
// no refs and no objects of its own to complete a short id against, so the
// id is resolved in the run's repository first.
func TestScratchCheckoutResolvesAnAbbreviatedCommit(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	sha := seedGitRepo(t, repo, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repo)

	tree := reconstructTarget(conn, run.ID, sha[:12])
	if tree.Dir == "" {
		t.Fatal("an abbreviated commit id was not checked out")
	}
	defer tree.release()
	if got := gitRun(t, tree.Dir, "rev-parse", "HEAD"); got != sha {
		t.Errorf("HEAD = %s, want %s", got, sha)
	}
}

// TestScratchCheckoutRunsNoFilterTheRepositoryConfigNames: the repository's
// config is the executor's to write, and a smudge filter it names would run
// in the engine's context on every reconstruction. The scratch repository
// reads none of that config.
func TestScratchCheckoutRunsNoFilterTheRepositoryConfigNames(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	seedGitRepo(t, repo, "measured.txt", "under review")
	writeFile(t, repo, ".gitattributes", "*.txt filter=planted\n")
	gitRun(t, repo, "add", ".gitattributes")
	gitRun(t, repo, "commit", "-q", "-m", "attributes that name a filter")
	sha := gitRun(t, repo, "rev-parse", "HEAD")
	setRunExecRoot(t, conn, run.ID, repo)

	marker := filepath.Join(t.TempDir(), "filter-ran")
	script := filepath.Join(t.TempDir(), "filter.sh")
	testsupport.Must(t, os.WriteFile(script,
		[]byte("#!/bin/sh\necho ran >> "+marker+"\ncat\n"), 0o755), "writing the filter")
	gitRun(t, repo, "config", "filter.planted.smudge", script)
	gitRun(t, repo, "config", "filter.planted.process", script)

	tree := reconstructTarget(conn, run.ID, sha)
	if tree.Dir == "" {
		t.Fatal("reconstruction did not produce a scratch tree")
	}
	defer tree.release()
	if exists(marker) {
		t.Error("the checkout ran a filter named by the repository's config")
	}
	got, err := os.ReadFile(filepath.Join(tree.Dir, "measured.txt"))
	testsupport.Must(t, err, "reading the scratch tree: %v", err)
	if strings.TrimSpace(string(got)) != "under review" {
		t.Errorf("the scratch tree holds %q, not the committed bytes", got)
	}
}

// TestScratchWithoutWorktreeMetadataIsSwept: git has no record of a scratch
// tree, so the sweep finds it through its sidecar. The abandoned tree goes,
// with its git directory, cache and sidecar; the one whose claim still holds
// its lock stays whole.
func TestScratchWithoutWorktreeMetadataIsSwept(t *testing.T) {
	isolateTempDir(t)
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	sha := seedGitRepo(t, repo, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repo)

	abandoned := reconstructTarget(conn, run.ID, sha)
	held := reconstructTarget(conn, run.ID, sha)
	if abandoned.Dir == "" || held.Dir == "" {
		t.Fatalf("reconstruction did not produce two scratch trees: %+v, %+v", abandoned, held)
	}
	t.Cleanup(held.release)

	if got := registeredScratchTrees(t, repo); len(got) != 0 {
		t.Fatalf("premise: git lists scratch worktrees %v; the sweep must find these trees without it", got)
	}
	if got := recordedScratchTrees(repo); !sameSet(got, []string{abandoned.Dir, held.Dir}) {
		t.Fatalf("recorded scratch trees = %v, want both %s and %s", got, abandoned.Dir, held.Dir)
	}

	abandonScratch(t, abandoned)
	swept := sweepStalePreGateScratch(repo)
	if !slices.Equal(swept, []string{abandoned.Dir}) {
		t.Errorf("swept = %v, want [%s]", swept, abandoned.Dir)
	}
	for _, p := range scratchPaths(abandoned.Dir) {
		if exists(p) {
			t.Errorf("%s survived the sweep", p)
		}
	}
	for _, p := range scratchPaths(held.Dir) {
		if !exists(p) {
			t.Errorf("%s was removed while its claim held the lock", p)
		}
	}
	if got := recordedScratchTrees(repo); !slices.Equal(got, []string{held.Dir}) {
		t.Errorf("recorded scratch trees after the sweep = %v, want [%s]", got, held.Dir)
	}
}

// sameSet reports whether a and b hold the same strings in any order.
func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// forgeSidecar writes an unheld sidecar for dir whose record names
// objectStore, as a program other than the engine could.
func forgeSidecar(t *testing.T, dir, objectStore string) {
	t.Helper()
	err := os.WriteFile(scratchLockPath(dir),
		[]byte(scratchRecordHeader+"\n"+objectStore+"\n"), 0o600)
	testsupport.Must(t, err, "forging a sidecar for %s: %v", dir, err)
}

// TestSweepRemovesOnlyScratchItCanProveItMade: the temp directory is shared,
// so a matching name is not ownership. Each case plants something a sweep
// that trusted the name alone would delete or follow.
func TestSweepRemovesOnlyScratchItCanProveItMade(t *testing.T) {
	tmp := isolateTempDir(t)
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	seedGitRepo(t, repo, "measured.txt", "under review")
	other := t.TempDir()
	otherSHA := seedGitRepo(t, other, "measured.txt", "another repository")
	source, ok := scratchSourceOf(repo)
	if !ok {
		t.Fatal("the repository's object store did not resolve")
	}

	// What a followed link would destroy.
	victim := t.TempDir()
	writeFile(t, victim, "keep.txt", "not the engine's\n")
	kept := filepath.Join(victim, "keep.txt")

	// A directory under the engine's name with no sidecar: nothing says the
	// engine made it, or for which repository.
	nameOnly := filepath.Join(tmp, scratchPrefix+"1001")
	testsupport.Must(t, os.Mkdir(nameOnly, 0o700), "mkdir %s", nameOnly)

	// A dead tree made for ANOTHER repository: that repository's sweep
	// removes it, this one's does not.
	setRunExecRoot(t, conn, run.ID, other)
	foreign := reconstructTarget(conn, run.ID, otherSHA)
	setRunExecRoot(t, conn, run.ID, repo)
	if foreign.Dir == "" {
		t.Fatal("reconstruction for the other repository failed")
	}
	abandonScratch(t, foreign)

	// A symbolic link under a tree's name, with a sidecar that names this
	// repository.
	linkTree := filepath.Join(tmp, scratchPrefix+"1002")
	testsupport.Must(t, os.Symlink(victim, linkTree), "symlink %s", linkTree)
	forgeSidecar(t, linkTree, source.objects)

	// A real directory whose sidecar is a symbolic link to a valid record.
	linkSidecar := filepath.Join(tmp, scratchPrefix+"1003")
	testsupport.Must(t, os.Mkdir(linkSidecar, 0o700), "mkdir %s", linkSidecar)
	record := filepath.Join(victim, "record")
	testsupport.Must(t, os.WriteFile(record,
		[]byte(scratchRecordHeader+"\n"+source.objects+"\n"), 0o600), "writing the record")
	testsupport.Must(t, os.Symlink(record, scratchLockPath(linkSidecar)), "symlink the sidecar")

	// A sidecar with the right name and no record: the engine writes the
	// record before it makes anything else, so this vouches for nothing.
	unrecorded := filepath.Join(tmp, scratchPrefix+"1004")
	testsupport.Must(t, os.Mkdir(unrecorded, 0o700), "mkdir %s", unrecorded)
	testsupport.Must(t, os.WriteFile(scratchLockPath(unrecorded), nil, 0o600), "writing an empty sidecar")

	// Everything about this one says the engine made it for this repository,
	// and it is dead. It goes; the link inside it is removed as a link.
	forged := filepath.Join(tmp, scratchPrefix+"1005")
	for _, p := range []string{forged, scratchGitPath(forged), scratchCachePath(forged)} {
		testsupport.Must(t, os.Mkdir(p, 0o700), "mkdir %s", p)
	}
	testsupport.Must(t, os.Symlink(victim, filepath.Join(forged, "inside")), "symlink inside the tree")
	forgeSidecar(t, forged, source.objects)

	// A dead tree with a valid sidecar, and a symbolic link where its git
	// directory or its cache root should be: one unprovable name stops the
	// removal of all of them.
	var linkSiblings []string
	for i, sibling := range []func(string) string{scratchGitPath, scratchCachePath} {
		tree := filepath.Join(tmp, scratchPrefix+[]string{"1007", "1008"}[i])
		testsupport.Must(t, os.Mkdir(tree, 0o700), "mkdir %s", tree)
		testsupport.Must(t, os.Symlink(victim, sibling(tree)), "symlink %s", sibling(tree))
		forgeSidecar(t, tree, source.objects)
		linkSiblings = append(linkSiblings, tree, sibling(tree), scratchLockPath(tree))
	}

	// A sidecar whose tree is already gone: debris, removed and not reported.
	orphan := filepath.Join(tmp, scratchPrefix+"1006")
	forgeSidecar(t, orphan, source.objects)

	swept := sweepStalePreGateScratch(repo)
	if !slices.Equal(swept, []string{forged}) {
		t.Errorf("swept = %v, want only the tree that proves itself, [%s]", swept, forged)
	}
	for _, p := range append(scratchPaths(forged), scratchLockPath(orphan)) {
		if exists(p) {
			t.Errorf("%s survived the sweep", p)
		}
	}
	survivors := []string{
		kept, record,
		nameOnly,
		linkTree, scratchLockPath(linkTree),
		linkSidecar, scratchLockPath(linkSidecar),
		unrecorded, scratchLockPath(unrecorded),
	}
	survivors = append(survivors, linkSiblings...)
	survivors = append(survivors, scratchPaths(foreign.Dir)...)
	for _, p := range survivors {
		if !exists(p) {
			t.Errorf("%s was removed by a sweep that could not prove the engine made it for this repository", p)
		}
	}

	// The other repository's own sweep does reclaim its tree.
	if got := sweepStalePreGateScratch(other); !slices.Equal(got, []string{foreign.Dir}) {
		t.Errorf("the other repository's sweep removed %v, want [%s]", got, foreign.Dir)
	}
}

// TestScratchCheckoutRefusesAPlantedGitDir: the tree's name is reserved by
// MkdirTemp and the git directory's name beside it is not. A symbolic link
// planted there would send `git init`, the alternates file and the HEAD
// update into the repository the link names.
func TestScratchCheckoutRefusesAPlantedGitDir(t *testing.T) {
	repo := t.TempDir()
	sha := seedGitRepo(t, repo, "measured.txt", "under review")
	source, ok := scratchSourceOf(repo)
	if !ok {
		t.Fatal("the repository's object store did not resolve")
	}

	victim := t.TempDir()
	seedGitRepo(t, victim, "theirs.txt", "another repository")
	victimHead := gitRun(t, victim, "rev-parse", "HEAD")
	before := gitDirListing(t, victim)

	dir := filepath.Join(t.TempDir(), scratchPrefix+"2001")
	testsupport.Must(t, os.Mkdir(dir, 0o700), "mkdir %s", dir)
	testsupport.Must(t, os.Symlink(filepath.Join(victim, ".git"), scratchGitPath(dir)),
		"planting a link at the git directory's name")

	if reserveScratchSiblings(dir) {
		t.Error("the planted git directory was taken for this claim's own")
	}
	if exists(scratchCachePath(dir)) {
		t.Error("a cache root was left behind by a reservation that failed")
	}
	if checkoutScratch(dir, source, sha) {
		t.Error("the checkout succeeded through a planted git directory")
	}
	if info, err := os.Lstat(scratchGitPath(dir)); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the planted link is not where it was put: %v, %v", info, err)
	}
	if got := gitRun(t, victim, "rev-parse", "HEAD"); got != victimHead {
		t.Errorf("the planted link's repository now has HEAD %s, was %s", got, victimHead)
	}
	if after := gitDirListing(t, victim); !slices.Equal(before, after) {
		t.Errorf("the checkout wrote into the planted link's repository:\nbefore %v\nafter  %v", before, after)
	}
}

// TestScratchLockRefusesAnExistingSidecar: a file already under a name
// MkdirTemp just minted is not this claim's, and its record is not adopted.
func TestScratchLockRefusesAnExistingSidecar(t *testing.T) {
	dir := filepath.Join(t.TempDir(), scratchPrefix+"2002")
	planted := []byte("planted\n")
	testsupport.Must(t, os.WriteFile(scratchLockPath(dir), planted, 0o600), "planting a sidecar")

	if lock, err := holdScratchLock(dir, "/some/objects"); err == nil {
		lock.Close()
		t.Error("the lock adopted a sidecar that already existed")
	}
	got, err := os.ReadFile(scratchLockPath(dir))
	testsupport.Must(t, err, "reading the planted sidecar: %v", err)
	if string(got) != string(planted) {
		t.Errorf("the planted sidecar was rewritten to %q", got)
	}
}

// TestScratchCheckoutCarriesAShallowBoundary: a shallow repository's objects
// stop at its boundary, and only the `shallow` file says so. Without it a
// history walk in the scratch tree runs into a parent that is not there.
func TestScratchCheckoutCarriesAShallowBoundary(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	origin := t.TempDir()
	seedGitRepo(t, origin, "measured.txt", "one")
	writeFile(t, origin, "measured.txt", "two\n")
	gitRun(t, origin, "commit", "-q", "-am", "the change under review")

	shallow := filepath.Join(t.TempDir(), "shallow")
	gitRun(t, origin, "clone", "-q", "--depth", "1", "file://"+origin, shallow)
	sha := gitRun(t, shallow, "rev-parse", "HEAD")
	setRunExecRoot(t, conn, run.ID, shallow)

	tree := reconstructTarget(conn, run.ID, sha)
	if tree.Dir == "" {
		t.Fatal("reconstruction from a shallow repository failed")
	}
	defer tree.release()
	if got := gitRun(t, tree.Dir, "rev-list", "HEAD"); got != sha {
		t.Errorf("history in the scratch tree = %q, want the one commit the shallow repository holds, %s", got, sha)
	}
}

// TestScratchThatWillNotDeleteKeepsItsSidecar: the sidecar is the only index
// of these trees. A tree that cannot be removed, here one holding a directory
// that is not writable, keeps it through release() and through a sweep, is
// not reported as swept, and goes on the sweep after the obstacle does.
func TestScratchThatWillNotDeleteKeepsItsSidecar(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("mode bits do not stop root from deleting")
	}
	isolateTempDir(t)
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	sha := seedGitRepo(t, repo, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repo)

	tree := reconstructTarget(conn, run.ID, sha)
	if tree.Dir == "" {
		t.Fatal("reconstruction did not produce a scratch tree")
	}
	stuck := filepath.Join(tree.Dir, "build")
	testsupport.Must(t, os.Mkdir(stuck, 0o700), "mkdir %s", stuck)
	testsupport.Must(t, os.WriteFile(filepath.Join(stuck, "output"), nil, 0o600), "writing build output")
	testsupport.Must(t, os.Chmod(stuck, 0o500), "chmod %s", stuck)
	t.Cleanup(func() { _ = os.Chmod(stuck, 0o700) })

	standing := func(when string) {
		t.Helper()
		for _, p := range []string{tree.Dir, scratchLockPath(tree.Dir)} {
			if !exists(p) {
				t.Fatalf("%s: %s is gone while part of the tree is still there", when, p)
			}
		}
		if got := recordedScratchTrees(repo); !sameSet(got, []string{tree.Dir}) {
			t.Fatalf("%s: recorded scratch trees = %v, want [%s]", when, got, tree.Dir)
		}
	}

	tree.release()
	standing("after release()")

	if swept := sweepStalePreGateScratch(repo); len(swept) != 0 {
		t.Errorf("the sweep reported %v, which it did not remove", swept)
	}
	standing("after a sweep that could not remove it")

	testsupport.Must(t, os.Chmod(stuck, 0o700), "chmod %s", stuck)
	if swept := sweepStalePreGateScratch(repo); !sameSet(swept, []string{tree.Dir}) {
		t.Errorf("swept = %v, want [%s]", swept, tree.Dir)
	}
	for _, p := range scratchPaths(tree.Dir) {
		if exists(p) {
			t.Errorf("%s survived the sweep after the obstacle was gone", p)
		}
	}
}

// TestSweepLeavesWhatAnotherUserOwns: in a temp directory shared between
// users, a dead tree with a valid record is still not this user's to remove
// unless this user owns it. The process is made to answer as another user,
// which makes every path the claim created somebody else's.
func TestSweepLeavesWhatAnotherUserOwns(t *testing.T) {
	isolateTempDir(t)
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	sha := seedGitRepo(t, repo, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repo)

	tree := reconstructTarget(conn, run.ID, sha)
	if tree.Dir == "" {
		t.Fatal("reconstruction did not produce a scratch tree")
	}
	abandonScratch(t, tree)

	real := scratchEuid
	t.Cleanup(func() { scratchEuid = real })
	untouched := func(when string) {
		t.Helper()
		for _, p := range scratchPaths(tree.Dir) {
			if !exists(p) {
				t.Errorf("%s: %s was removed though another user owns it", when, p)
			}
		}
	}

	// Everything is another user's: the sidecar fails its own owner check
	// and the directories are never looked at.
	scratchEuid = func() int { return real() + 1 }
	if swept := sweepStalePreGateScratch(repo); len(swept) != 0 {
		t.Errorf("the sweep removed %v, whose sidecar another user owns", swept)
	}
	untouched("with a sidecar another user owns")

	// The sidecar is this user's and the directories are not: the record,
	// the flock and the name all pass, and the directory check alone
	// refuses. The owner is asked for once per path, sidecar first, and the
	// temp directory holds this one tree.
	asked := 0
	scratchEuid = func() int {
		asked++
		if asked == 1 {
			return real()
		}
		return real() + 1
	}
	if swept := sweepStalePreGateScratch(repo); len(swept) != 0 {
		t.Errorf("the sweep removed %v, whose directories another user owns", swept)
	}
	if asked < 2 {
		t.Fatalf("the owner was asked for %d time(s); the directory check was not reached", asked)
	}
	untouched("with directories another user owns")

	scratchEuid = real
	if swept := sweepStalePreGateScratch(repo); !sameSet(swept, []string{tree.Dir}) {
		t.Errorf("swept as the owner = %v, want [%s]", swept, tree.Dir)
	}
}

// TestSidecarStillAt: a sweeper that took the flock on a sidecar another
// sweeper has since unlinked holds a file that vouches for nothing, whatever
// now sits under its name.
func TestSidecarStillAt(t *testing.T) {
	path := scratchLockPath(filepath.Join(t.TempDir(), scratchPrefix+"3001"))
	testsupport.Must(t, os.WriteFile(path, nil, 0o600), "writing a sidecar")
	held, err := os.Open(path)
	testsupport.Must(t, err, "opening the sidecar: %v", err)
	defer held.Close()

	if !sidecarStillAt(held, path) {
		t.Error("an untouched sidecar reads as replaced")
	}
	testsupport.Must(t, os.Remove(path), "unlinking the sidecar")
	if sidecarStillAt(held, path) {
		t.Error("an unlinked sidecar reads as still in place")
	}
	testsupport.Must(t, os.WriteFile(path, nil, 0o600), "writing a new sidecar under the name")
	if sidecarStillAt(held, path) {
		t.Error("a new file under the sidecar's name reads as the one held")
	}
}

// TestScratchCheckoutRunsNoHookTheUserConfigNames: the scratch repository has
// no hooks of its own, so the checkout's hook hardening is tested where a
// hook can still come from: the user's config. It names a hooks path whose
// hooks record that they ran, and a template directory that would copy the
// same hooks into every new repository.
func TestScratchCheckoutRunsNoHookTheUserConfigNames(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	repo := t.TempDir()
	sha := seedGitRepo(t, repo, "measured.txt", "under review")
	setRunExecRoot(t, conn, run.ID, repo)

	home := t.TempDir()
	marker := filepath.Join(home, "ran")
	hooks := filepath.Join(home, "hooks")
	template := filepath.Join(home, "template")
	for _, dir := range []string{hooks, filepath.Join(template, "hooks")} {
		testsupport.Must(t, os.MkdirAll(dir, 0o700), "mkdir %s", dir)
		for _, name := range []string{"post-index-change", "post-checkout", "reference-transaction"} {
			err := os.WriteFile(filepath.Join(dir, name),
				[]byte("#!/bin/sh\necho \"$0\" >> '"+marker+"'\n"), 0o700)
			testsupport.Must(t, err, "writing the %s hook: %v", name, err)
		}
	}
	err := os.WriteFile(filepath.Join(home, ".gitconfig"),
		[]byte("[init]\n\ttemplateDir = "+template+"\n[core]\n\thooksPath = "+hooks+"\n"), 0o600)
	testsupport.Must(t, err, "writing the user config: %v", err)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))

	// Premise: git run without the engine's hardening does both things.
	plain := t.TempDir()
	gitRun(t, plain, "init", "-q")
	if !exists(filepath.Join(plain, ".git", "hooks", "post-checkout")) {
		t.Fatal("premise: the user's template directory is not applied to a new repository")
	}
	gitRun(t, repo, "update-ref", "refs/heads/premise", sha)
	if !exists(marker) {
		t.Fatal("premise: the user's hooks path does not run on a ref update")
	}
	testsupport.Must(t, os.Remove(marker), "clearing the premise's marker")

	tree := reconstructTarget(conn, run.ID, sha)
	if tree.Dir == "" {
		t.Fatal("reconstruction failed under a user config that names hooks")
	}
	defer tree.release()
	if ran, err := os.ReadFile(marker); err == nil {
		t.Errorf("the checkout ran hooks the user config names:\n%s", ran)
	}
	if exists(filepath.Join(scratchGitPath(tree.Dir), "hooks")) {
		t.Error("the scratch git directory was given the user's template hooks")
	}
}
