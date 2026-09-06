package engine

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1374: a write-class executor whose `git commit` was refused hands back a
// worktree still standing at its BASE HEAD. The round record wrote that sha as
// `head` regardless, so every downstream judge's `target_sha` named a tree
// PREDATING the work — indistinguishable from a correct target, and reviewed as
// if the change had never been made (RUN-82/DOT-1269/STEP-3846: both ACs read
// as violated against a change that satisfied them). A worktree still at its
// own base carries no commit to name, so the record names none and the judge
// falls back to the rendered `issue.diff`.

// TestBlockedCommitOmitsTheTargetSHA is the defect's shape: real git, a
// worktree forked from the shared checkout with UNCOMMITTED work, recorded as
// the write step's hand-back. The downstream judge's target must carry the
// declared worktree and NO sha.
func TestBlockedCommitOmitsTheTargetSHA(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	shared := gitRepo(t)
	pinned := gitRun(t, shared, "rev-parse", "HEAD")

	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "blocked commit", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "blocked-commit run", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue: %v", err)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.DiffFn = GitDiff
	e.HeadFn = sharedCheckoutHead

	// The executor's worktree: the edit lands, the commit does not.
	work := filepath.Join(t.TempDir(), "wf-implement")
	gitRun(t, shared, "worktree", "add", "-q", work)
	writeFile(t, work, "internal/tracked.txt", "THE BLOCKED CHANGE\n")

	completeStepAt(t, conn, e, issue, "implement@0", work)

	review, err := ClaimStep(conn, stepIDIn(t, conn, issue, "review@0#0"),
		ClaimOptions{Owner: "judge", NowMS: nowMS})
	testsupport.Must(t, err, "claim review: %v", err)

	if review.Context.TargetSHA != "" {
		t.Errorf("target_sha = %q, want none — the worktree stands at its base "+
			"commit, so this sha names the PRE-fix tree", review.Context.TargetSHA)
	}
	if review.Context.TargetWorktree != work {
		t.Errorf("target_worktree = %q, want the declared worktree %q — the "+
			"blocked hand-back is still reachable there",
			review.Context.TargetWorktree, work)
	}

	// The packet a judge reads states no sha either, and still carries the
	// uncommitted change as `issue.diff` — the evidence obligation 2r sends a
	// judge to when the target is empty.
	rendered, err := RenderStep(conn, stepIDIn(t, conn, issue, "review@0#1"), "", nowMS)
	testsupport.Must(t, err, "RenderStep: %v", err)
	if strings.Contains(rendered.Packet, "target_sha: "+pinned) {
		t.Errorf("the packet names the base commit as the target:\n%s", rendered.Packet)
	}
	if !strings.Contains(rendered.Packet, "THE BLOCKED CHANGE") {
		t.Errorf("the packet carries no diff of the blocked change:\n%s", rendered.Packet)
	}
}

// TestCommittedHandBackKeepsTheTargetSHA is the control the rule must not
// disturb: the same fixture with the commit made names that commit.
func TestCommittedHandBackKeepsTheTargetSHA(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	shared := gitRepo(t)
	pinned := gitRun(t, shared, "rev-parse", "HEAD")

	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "committed hand-back", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "committed run", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue: %v", err)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.DiffFn = GitDiff
	e.HeadFn = sharedCheckoutHead

	work := filepath.Join(t.TempDir(), "wf-implement")
	gitRun(t, shared, "worktree", "add", "-q", work)
	writeFile(t, work, "internal/tracked.txt", "THE COMMITTED CHANGE\n")
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-qm", "the candidate")
	candidate := gitRun(t, work, "rev-parse", "HEAD")

	completeStepAt(t, conn, e, issue, "implement@0", work)

	review, err := ClaimStep(conn, stepIDIn(t, conn, issue, "review@0#0"),
		ClaimOptions{Owner: "judge", NowMS: nowMS})
	testsupport.Must(t, err, "claim review: %v", err)
	if review.Context.TargetSHA != candidate {
		t.Errorf("target_sha = %q, want the candidate commit %q",
			review.Context.TargetSHA, candidate)
	}
}
