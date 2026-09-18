package engine

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// GitCommitPatch drives REAL GIT, for gitdiff_test.go's reason: it shells out,
// and a fake would prove nothing about what `diff-tree` reports.

// TestGitCommitPatchRendersOneCommitScoped: the body is the named commit's own
// patch against its parent — not the working tree, not the range from the
// run's base — pathspec'd to the scope, with the paths the commit touched
// outside the scope named and their hunks following under the marked heading.
func TestGitCommitPatchRendersOneCommitScoped(t *testing.T) {
	dir := gitRepo(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv()
		out, err := cmd.CombinedOutput()
		testsupport.Must(t, err, "git %s: %v\n%s", strings.Join(args, " "), err, out)
		return strings.TrimSpace(string(out))
	}

	// Commit A touches the in-scope file only; commit B touches it again AND a
	// file outside the scope; the working tree then changes a third time
	// without committing, so a body that read the tree would show it.
	writeFile(t, dir, "internal/tracked.txt", "first change\n")
	git("add", "-A")
	git("commit", "-qm", "A")
	writeFile(t, dir, "internal/tracked.txt", "second change\n")
	writeFile(t, dir, "docs/outside.md", "outside the scope\n")
	git("add", "-A")
	git("commit", "-qm", "B")
	b := git("rev-parse", "HEAD")
	writeFile(t, dir, "internal/tracked.txt", "uncommitted third change\n")

	body, err := GitCommitPatch(dir, b, []string{"internal/"})
	testsupport.Must(t, err, "GitCommitPatch: %v", err)

	if !strings.Contains(body, "+second change") || !strings.Contains(body, "-first change") {
		t.Errorf("body does not carry B's own patch against its parent:\n%s", body)
	}
	if strings.Contains(body, "uncommitted third change") {
		t.Errorf("body read the working tree, not the commit:\n%s", body)
	}
	if !strings.Contains(body, "1 changed file(s) fall outside") || !strings.Contains(body, "#   docs/outside.md") {
		t.Errorf("body does not disclose the out-of-scope path:\n%s", body)
	}
	if !strings.Contains(body, "+outside the scope") {
		t.Errorf("body does not carry the out-of-scope hunk after the heading:\n%s", body)
	}
	if idx := strings.Index(body, "outside declared scope"); idx < 0 ||
		strings.Index(body, "+outside the scope") < idx {
		t.Errorf("the out-of-scope hunk does not FOLLOW the in-scope diff:\n%s", body)
	}

	// An empty scope renders the whole commit with no disclosure to make.
	whole, err := GitCommitPatch(dir, b, nil)
	testsupport.Must(t, err, "GitCommitPatch (unscoped): %v", err)
	if !strings.Contains(whole, "+outside the scope") || strings.Contains(whole, "fall outside") {
		t.Errorf("unscoped body = %q, want the whole commit and no scope trailer", whole)
	}

	// A root commit renders too (--root), rather than failing on a missing parent.
	root := git("rev-list", "--max-parents=0", "HEAD")
	rootBody, err := GitCommitPatch(dir, root, nil)
	testsupport.Must(t, err, "GitCommitPatch (root): %v", err)
	if !strings.Contains(rootBody, "+original") {
		t.Errorf("root commit body = %q, want its own patch", rootBody)
	}
}
