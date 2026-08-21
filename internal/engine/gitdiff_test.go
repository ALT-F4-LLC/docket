package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// GitDiff and the untracked-file blind spot (E-9).
//
// `git diff HEAD --` cannot see untracked files by construction, so a NEW file
// was invisible to every review that read `issue.diff` — and new files are
// exactly the class of change a review most needs to see. RUN-3 recorded a
// review whose entire subject (a new test file and the adaptation it covered)
// was absent from the diff it was handed.
//
// These tests drive REAL GIT in a throwaway repository. GitDiff shells out, and
// a fake would prove nothing about the one behavior in question: what the git
// command actually reports.

// gitEnv is the one hermetic environment every git subprocess in this file
// runs with, so no test call reads the operator's own identity or config.
func gitEnv() []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME=qa", "GIT_AUTHOR_EMAIL=qa@example.invalid",
		"GIT_COMMITTER_NAME=qa", "GIT_COMMITTER_EMAIL=qa@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
}

// gitRepo builds a throwaway repository with one committed file and returns its
// path. Every command runs with the sandbox's own identity so nothing reads the
// operator's git config.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv()
		out, err := cmd.CombinedOutput()
		testsupport.Must(t, err, "git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	run("init", "-q", ".")
	writeFile(t, dir, "internal/tracked.txt", "original\n")
	run("add", "-A")
	run("commit", "-qm", "base")
	return dir
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	testsupport.Must(t, err, "mkdir: %v", err)
	err = os.WriteFile(path, []byte(body), 0o644)
	testsupport.Must(t, err, "writing %s: %v", rel, err)
}

// inRepo runs GitDiff against the repo BY DIRECTORY — the G7 fix's shape: the
// engine passes the recorded worktree (or the invoking checkout's root), so
// the diff never depends on the process cwd. The cwd stays wherever the test
// harness put it, which is itself part of what this asserts.
func inRepo(t *testing.T, dir string, scope []string) string {
	t.Helper()
	diff, err := GitDiff(dir, "", scope)
	testsupport.Must(t, err, "GitDiff: %v", err)
	return diff
}

// TestGitDiffSeesUntrackedFiles is E-9's regression, and it fails on HEAD
// before the fix: a brand-new file is part of the change under review.
func TestGitDiffSeesUntrackedFiles(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "internal/brand_new_test.go", "package x // the new test\n")

	diff := inRepo(t, dir, nil)

	if !strings.Contains(diff, "brand_new_test.go") {
		t.Errorf("the diff omits an untracked file — a reviewer cannot see the "+
			"new file at all (E-9)\n%s", diff)
	}
	if !strings.Contains(diff, "the new test") {
		t.Errorf("the diff names the untracked file but omits its CONTENT\n%s", diff)
	}
	// It reads as an addition, which is what it is.
	if !strings.Contains(diff, "new file") {
		t.Errorf("the untracked file is not marked as a new file\n%s", diff)
	}
}

// TestGitDiffStillSeesTrackedChanges is the baseline every falsification is
// measured against: the fix must ADD the untracked half, never replace the
// tracked one.
func TestGitDiffStillSeesTrackedChanges(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "internal/tracked.txt", "original\nEDITED\n")
	writeFile(t, dir, "internal/brand_new.txt", "new\n")

	diff := inRepo(t, dir, nil)

	if !strings.Contains(diff, "EDITED") {
		t.Errorf("the diff lost the tracked modification\n%s", diff)
	}
	if !strings.Contains(diff, "brand_new.txt") {
		t.Errorf("the diff lost the untracked addition\n%s", diff)
	}
}

// TestGitDiffUntrackedRespectsScope pins that the untracked half is filtered by
// the issue's snapshotted scope exactly as the tracked half is. An issue that
// narrowed what it may touch must not have its diff widened by new files
// outside that scope — their HUNKS stay out; DKT-43 amended the contract so
// their NAMES appear in the disclosure trailer instead of vanishing.
func TestGitDiffUntrackedRespectsScope(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "internal/in_scope.txt", "inside\n")
	writeFile(t, dir, "elsewhere/out_of_scope.txt", "outside\n")

	diff := inRepo(t, dir, []string{"internal/**"})

	if !strings.Contains(diff, "in_scope.txt") {
		t.Errorf("an in-scope untracked file is missing\n%s", diff)
	}
	// DKT-86: out-of-scope content renders only AFTER the marked heading —
	// the untracked half honors scope in the leading diff exactly as before.
	marker := strings.Index(diff, "=== outside declared scope")
	if marker < 0 {
		t.Fatalf("the out-of-scope heading is missing (DKT-86)\n%s", diff)
	}
	if lead := diff[:marker]; strings.Contains(lead, "+outside") {
		t.Errorf("an out-of-scope untracked file's CONTENT leaked into the "+
			"leading diff — the untracked half must honor scope as the "+
			"tracked half does\n%s", diff)
	}
	if !strings.Contains(diff, "#   elsewhere/out_of_scope.txt") {
		t.Errorf("the trailer does not NAME the scope-omitted file (DKT-43)\n%s", diff)
	}
	if !strings.Contains(diff[marker:], "+outside") {
		t.Errorf("the scope-omitted file's hunk is missing from the marked "+
			"section (DKT-86)\n%s", diff)
	}
}

// TestGitDiffUntrackedHonoursGitignore keeps build output and scratch files out
// of review. `--exclude-standard` is what makes the untracked half usable at
// all: without it, every ignored artifact in the tree would land in the diff.
func TestGitDiffUntrackedHonoursGitignore(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, ".gitignore", "*.log\n")
	writeFile(t, dir, "internal/noisy.log", "build output nobody reviews\n")
	writeFile(t, dir, "internal/real_change.txt", "reviewable\n")

	diff := inRepo(t, dir, nil)

	if strings.Contains(diff, "noisy.log") {
		t.Errorf("an ignored file reached the diff\n%s", diff)
	}
	if !strings.Contains(diff, "real_change.txt") {
		t.Errorf("the reviewable addition is missing\n%s", diff)
	}
}

// TestGitDiffOutsideARepoIsEmpty preserves the existing contract: a directory
// that is not a checkout yields an empty diff rather than wedging a run. The
// engine records what the tree says, and "nothing" is a truthful answer.
func TestGitDiffOutsideARepoIsEmpty(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "loose.txt", "not in any repository\n")

	if diff := inRepo(t, dir, nil); diff != "" {
		t.Errorf("GitDiff outside a checkout returned %q, want empty", diff)
	}
}

// TestGitDiffNoChangesIsEmpty pins the other quiet case: a clean tree produces
// no diff, and in particular the untracked pass adds nothing when there is
// nothing untracked.
func TestGitDiffNoChangesIsEmpty(t *testing.T) {
	dir := gitRepo(t)

	if diff := inRepo(t, dir, nil); diff != "" {
		t.Errorf("a clean tree produced a diff:\n%s", diff)
	}
}

// TestGitDiffDoesNotMutateTheIndex is why `--intent-to-add` was rejected.
//
// That flag is the obvious way to make untracked files visible to `git diff`,
// and it WRITES TO THE INDEX — staging paths the operator never staged. The
// engine computes a diff as a read; a read verb that leaves the operator's
// staging area different from how it found it is a defect, not a side effect.
func TestGitDiffDoesNotMutateTheIndex(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "internal/brand_new.txt", "new\n")

	before := gitOutput(t, dir, "status", "--porcelain")
	_ = inRepo(t, dir, nil)
	after := gitOutput(t, dir, "status", "--porcelain")

	if before != after {
		t.Errorf("computing the diff changed the working tree state\n"+
			"before:\n%s\nafter:\n%s", before, after)
	}
}

// TestGitDiffWorktreeCommitAgainstSharedHead is DKT-11's regression — see
// GitDiff's own doc for the rationale.
func TestGitDiffWorktreeCommitAgainstSharedHead(t *testing.T) {
	shared := gitRepo(t)
	sharedHead := gitOutput(t, shared, "rev-parse", "HEAD")
	sharedHead = strings.TrimSpace(sharedHead)

	worktree := t.TempDir()
	worktree = filepath.Join(worktree, "wt")
	runGit(t, shared, "worktree", "add", "-q", "-b", "wt-branch", worktree)

	writeFile(t, worktree, "internal/tracked.txt", "original\nCOMMITTED IN WORKTREE\n")
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-qm", "worktree change")

	// The fixture's own state, not a GitDiff assertion: the change is already
	// committed IN the worktree, so its working tree exactly matches its own
	// HEAD — the fact that makes a diff against dir's own HEAD structurally
	// unable to show the change, independent of whatever GitDiff's own
	// defaulted-base policy happens to do.
	status := gitOutput(t, worktree, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		t.Fatalf("expected the worktree's tree to already equal its own HEAD "+
			"(the change is committed), got a dirty status:\n%s", status)
	}

	// The fix: diffing the worktree against the SHARED checkout's HEAD sees
	// the commit.
	diff, err := GitDiff(worktree, sharedHead, nil)
	testsupport.Must(t, err, "GitDiff: %v", err)
	if !strings.Contains(diff, "COMMITTED IN WORKTREE") {
		t.Errorf("issue.diff for a worktree-recorded step is empty against "+
			"the shared checkout's HEAD, want the committed change\n%s", diff)
	}
}

// TestGitDiffDisclosesOutOfScopeChanges is DKT-43 / DKT-44's regression, as
// amended by DKT-86. A fix commit touched four files; the issue's declared
// scope named one; the rendered diff showed one file with nothing to say the
// other three existed. DKT-43/44 made the omission NAMED (a names-only
// trailer); DKT-86 found that still hid a file the issue's own request
// mandated editing from every judge, so the excluded hunks now FOLLOW the
// in-scope diff under a clearly marked heading. The scope filter still
// decides what leads: the in-scope hunk renders first, the excluded ones
// after the marker, never interleaved.
func TestGitDiffDisclosesOutOfScopeChanges(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "elsewhere/committed.txt", "committed\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "second base file")

	writeFile(t, dir, "internal/tracked.txt", "original\nIN SCOPE EDIT\n")
	writeFile(t, dir, "elsewhere/committed.txt", "committed\nOUT OF SCOPE EDIT\n")
	writeFile(t, dir, "elsewhere/new_test.go", "package x // out-of-scope new file\n")

	diff := inRepo(t, dir, []string{"internal/**"})

	if !strings.Contains(diff, "IN SCOPE EDIT") {
		t.Errorf("the in-scope hunk is missing\n%s", diff)
	}
	if !strings.Contains(diff, "# issue.diff: 2 changed file(s) fall outside "+
		"this issue's declared scope and are excluded from the diff above:") {
		t.Errorf("the disclosure trailer is missing or miscounted\n%s", diff)
	}
	if !strings.Contains(diff, "#   elsewhere/committed.txt") {
		t.Errorf("the trailer omits the tracked out-of-scope edit\n%s", diff)
	}
	if !strings.Contains(diff, "#   elsewhere/new_test.go") {
		t.Errorf("the trailer omits the untracked out-of-scope file\n%s", diff)
	}

	marker := strings.Index(diff, "=== outside declared scope")
	if marker < 0 {
		t.Fatalf("the out-of-scope heading is missing (DKT-86)\n%s", diff)
	}
	outHunk := strings.Index(diff, "OUT OF SCOPE EDIT")
	if outHunk < 0 {
		t.Fatalf("the out-of-scope HUNK is absent — a file the change touched "+
			"is hidden from the review again (DKT-86)\n%s", diff)
	}
	if outHunk < marker {
		t.Errorf("an out-of-scope hunk renders BEFORE the marked heading — "+
			"the in-scope diff must lead\n%s", diff)
	}
	if !strings.Contains(diff[marker:], "out-of-scope new file") {
		t.Errorf("the untracked out-of-scope file's hunk is missing from the "+
			"marked section\n%s", diff)
	}
	if inHunk := strings.Index(diff, "IN SCOPE EDIT"); inHunk > marker {
		t.Errorf("the in-scope hunk renders after the out-of-scope marker\n%s", diff)
	}
}

// TestGitDiffNoTrailerWhenAllInScope pins the quiet side of the disclosure: a
// change fully inside its scope gets exactly the diff it always got, with no
// trailer to make a clean record look qualified.
func TestGitDiffNoTrailerWhenAllInScope(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "internal/tracked.txt", "original\nEDIT\n")

	diff := inRepo(t, dir, []string{"internal/**"})

	if !strings.Contains(diff, "EDIT") {
		t.Errorf("the in-scope change is missing\n%s", diff)
	}
	if strings.Contains(diff, "fall outside this issue's declared scope") {
		t.Errorf("a fully in-scope change drew a disclosure trailer\n%s", diff)
	}
}

// TestGitDiffNoScopeNoTrailer: an issue that declared no scope has not
// narrowed its diff, so there is nothing to disclose — the whole tree is
// already shown.
func TestGitDiffNoScopeNoTrailer(t *testing.T) {
	dir := gitRepo(t)
	writeFile(t, dir, "elsewhere/anything.txt", "content\n")

	diff := inRepo(t, dir, nil)

	if !strings.Contains(diff, "anything.txt") {
		t.Errorf("an unscoped diff is missing a changed file\n%s", diff)
	}
	if strings.Contains(diff, "fall outside this issue's declared scope") {
		t.Errorf("an unscoped diff drew a disclosure trailer\n%s", diff)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	testsupport.Must(t, err, "git %s: %v\n%s", strings.Join(args, " "), err, out)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.Output()
	testsupport.Must(t, err, "git %s: %v", strings.Join(args, " "), err)
	return string(out)
}

// TestSharedCheckoutHead pins the helper's three degrade inputs directly: no
// production test named it before this, only its call site and its
// definition — a fixture passing against today's behavior for each was
// missing.
func TestSharedCheckoutHead(t *testing.T) {
	if got := sharedCheckoutHead(""); got != "" {
		t.Errorf("sharedCheckoutHead(\"\") = %q, want empty (no execRoot given)", got)
	}

	notARepo := t.TempDir()
	if got := sharedCheckoutHead(notARepo); got != "" {
		t.Errorf("sharedCheckoutHead(%s) = %q, want empty (not a git checkout)",
			notARepo, got)
	}

	repo := gitRepo(t)
	want := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))
	if got := sharedCheckoutHead(repo); got != want {
		t.Errorf("sharedCheckoutHead(%s) = %q, want the repo's own HEAD %q",
			repo, got, want)
	}
}

// TestGitDiffBaseAndScopeCombined is the case the production call site
// always exercises but no fixture combined before this: a base other than
// dir's own HEAD, together with a declared scope, over a change that
// straddles the scope boundary.
func TestGitDiffBaseAndScopeCombined(t *testing.T) {
	shared := gitRepo(t)
	sharedHead := strings.TrimSpace(gitOutput(t, shared, "rev-parse", "HEAD"))

	worktree := filepath.Join(t.TempDir(), "wt")
	runGit(t, shared, "worktree", "add", "-q", "-b", "wt-branch-scope", worktree)

	// One committed change touching an in-scope file and an out-of-scope
	// file, exactly what a worktree-recorded step commits before recording.
	writeFile(t, worktree, "internal/in_scope.txt", "inside\n")
	writeFile(t, worktree, "elsewhere/out_of_scope.txt", "outside\n")
	runGit(t, worktree, "add", "-A")
	runGit(t, worktree, "commit", "-qm", "scoped worktree change")

	diff, err := GitDiff(worktree, sharedHead, []string{"internal/**"})
	testsupport.Must(t, err, "GitDiff: %v", err)

	if !strings.Contains(diff, "in_scope.txt") {
		t.Errorf("the in-scope addition against a non-default base is missing\n%s", diff)
	}
	if !strings.Contains(diff, "inside") {
		t.Errorf("the in-scope file's content is missing from the diff\n%s", diff)
	}
	// DKT-86: the based+scoped case layers exactly like the default-base one —
	// scope governs the leading diff, and the omitted hunks follow the marker.
	marker := strings.Index(diff, "=== outside declared scope")
	if marker < 0 {
		t.Fatalf("the out-of-scope heading is missing (DKT-86)\n%s", diff)
	}
	if lead := diff[:marker]; strings.Contains(lead, "+outside") {
		t.Errorf("out-of-scope CONTENT leaked into the based diff's leading "+
			"half\n%s", diff)
	}
	if !strings.Contains(diff, "#   elsewhere/out_of_scope.txt") {
		t.Errorf("the trailer does not NAME the scope-omitted file in the "+
			"based+scoped case the production call site always exercises (DKT-43)\n%s", diff)
	}
	if !strings.Contains(diff[marker:], "+outside") {
		t.Errorf("the scope-omitted hunk is missing from the marked section "+
			"in the based+scoped case (DKT-86)\n%s", diff)
	}
}
