package engine

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A retry whose recomputed issue.diff records no change leaves downstream
// consumers on the RETAINED record's target — by decision, not by accident.
//
// Attempt 1 committed the change in worktree A and parked on a failing gate.
// The conductor integrated that commit into the shared branch by hand (a
// cherry-pick, which mints a new sha), then resolved `--as retry`. Attempt 2
// forked worktree B from the integrated head, found the work already in place,
// committed nothing, and recorded done. Its diff against B's fork point is
// empty, so the empty-re-record guard drops the re-record and the issue's newest
// issue.diff is still attempt 1's.
//
// Recording attempt 1's body under attempt 2's payload instead would pair a
// diff measured in A with a round record naming B — a row saying that diff was
// observed in a tree where nothing was; and B's payload carries no `head` at
// all, since B stands at its own base, so the packets would lose
// `target_sha` and gain a worktree path that integration sweeps. The retained
// target is a commit whose patch the shared branch carries, which is exactly
// what the stale-target advisory acquits; moving the target
// on purpose is the annotate-integration and `--worktree` re-pin verbs' job.

// TestRetryThatDiffsEmptyKeepsTheRetainedTarget is the reproduction the
// issue asked for: real git, a real retry, and the packet a downstream judge
// is handed afterwards.
func TestRetryThatDiffsEmptyKeepsTheRetainedTarget(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	shared := gitRepo(t)
	pinned := gitRun(t, shared, "rev-parse", "HEAD")

	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "empty retry", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "empty-retry run", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "AddRunIssue: %v", err)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	gates := &scriptedGates{fail: true}
	e := testEngine()
	e.Gates = gates
	e.DiffFn = GitDiff
	e.HeadFn = sharedCheckoutHead

	// Attempt 1: the change lands as a commit in worktree A; the gate fails
	// and the step parks.
	treeA := filepath.Join(t.TempDir(), "wf-implement-a")
	gitRun(t, shared, "worktree", "add", "-q", treeA)
	writeFile(t, treeA, "internal/tracked.txt", "THE CHANGE\n")
	gitRun(t, treeA, "add", "-A")
	gitRun(t, treeA, "commit", "-qm", "the change")
	headA := gitRun(t, treeA, "rev-parse", "HEAD")

	id := stepIDIn(t, conn, issue, "implement@0")
	claim, err := ClaimStep(conn, id, ClaimOptions{Owner: "w1", NowMS: nowMS})
	testsupport.Must(t, err, "claim (attempt 1): %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: treeA, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete (attempt 1): %v", err)
	if got := stepStatusByID(t, conn, id); got != db.StepWaitingHuman {
		t.Fatalf("status = %q after a failing gate, want %q", got, db.StepWaitingHuman)
	}
	head, worktree, records := newestIssueDiffTarget(t, conn, run.ID, issue)
	if records != 1 || head != headA || worktree != treeA {
		t.Fatalf("attempt 1 recorded %d issue.diff(s) at %s in %s, want 1 at %s in %s",
			records, head, worktree, headA, treeA)
	}

	// A sibling's work lands on the shared branch, then the conductor
	// integrates attempt 1's commit by hand on top of it and retries.
	writeFile(t, shared, "internal/sibling.txt", "a sibling issue's work\n")
	gitRun(t, shared, "add", "-A")
	gitRun(t, shared, "commit", "-qm", "sibling work")
	gitRun(t, shared, "cherry-pick", headA)
	integrated := gitRun(t, shared, "rev-parse", "HEAD")
	if integrated == headA {
		t.Fatalf("the cherry-pick kept sha %s; the scenario needs a rewritten one", headA)
	}
	gates.fail = false
	err = e.ResolveStep(conn, id, ResolveRetry, "redo it on the integrated base", nowMS+1)
	testsupport.Must(t, err, "resolve --as retry: %v", err)

	// Attempt 2 forks worktree B from the integrated head, finds the work in
	// place, and commits nothing.
	treeB := filepath.Join(t.TempDir(), "wf-implement-b")
	gitRun(t, shared, "worktree", "add", "-q", treeB)
	claim, err = ClaimStep(conn, id, ClaimOptions{Owner: "w2", NowMS: nowMS + 2})
	testsupport.Must(t, err, "claim (attempt 2): %v", err)
	err = e.CompleteStep(conn, id, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change was already there"),
		WorkDir: treeB, NowMS: nowMS + 2,
	})
	testsupport.Must(t, err, "complete (attempt 2): %v", err)
	if got := stepStatusByID(t, conn, id); got != db.StepDone {
		t.Fatalf("status = %q after the retry passed its gates, want %q", got, db.StepDone)
	}

	// THE LEDGER: the empty re-record was dropped, so the newest issue.diff
	// is still attempt 1's, while the step ROW's worktree moved to B — the
	// retried step's own gates measure B, its downstream readers target A.
	head, worktree, records = newestIssueDiffTarget(t, conn, run.ID, issue)
	if records != 1 {
		t.Errorf("the issue holds %d issue.diff record(s), want 1 — an empty "+
			"re-record must not replace a recorded change", records)
	}
	if head != headA || worktree != treeA {
		t.Errorf("newest issue.diff names %s in %s, want attempt 1's %s in %s",
			head, worktree, headA, treeA)
	}
	var workRoot string
	err = conn.QueryRow(`SELECT work_root FROM steps WHERE id = ?`, id).Scan(&workRoot)
	testsupport.Must(t, err, "reading work_root: %v", err)
	if workRoot != treeB {
		t.Errorf("step work_root = %q, want the retry's worktree %q", workRoot, treeB)
	}

	// THE PACKET: the downstream judge is handed attempt 1's commit and
	// worktree as the target, and the change as issue.diff.
	review, err := ClaimStep(conn, stepIDIn(t, conn, issue, "review@0#0"),
		ClaimOptions{Owner: "judge", NowMS: nowMS + 3})
	testsupport.Must(t, err, "claim review: %v", err)
	if review.Context.TargetSHA != headA {
		t.Errorf("target_sha = %q, want the retained record's %s", review.Context.TargetSHA, headA)
	}
	if review.Context.TargetWorktree != treeA {
		t.Errorf("target_worktree = %q, want the retained record's %s",
			review.Context.TargetWorktree, treeA)
	}
	rendered, err := RenderStep(conn, stepIDIn(t, conn, issue, "review@0#1"), "", nowMS+3)
	testsupport.Must(t, err, "RenderStep: %v", err)
	if !strings.Contains(rendered.Packet, "target_sha: "+headA) {
		t.Errorf("the packet does not name %s as the target:\n%s", headA, rendered.Packet)
	}
	if !strings.Contains(rendered.Packet, "THE CHANGE") {
		t.Errorf("the packet carries no diff of the change:\n%s", rendered.Packet)
	}

	// THE ADVISORY: the retained target is off the shared branch's history
	// (the cherry-pick rewrote its sha) but its patch is on it, so the
	// stale-target advisory acquits rather than warns — the same probes the
	// engine wires (NewEngine) and staleTargets asks.
	if ancestor, known := e.IsAncestorFn(shared, headA); !known || ancestor {
		t.Errorf("IsAncestorFn(%s) = (%v, %v), want a known non-ancestor after the cherry-pick",
			headA, ancestor, known)
	}
	if contained, known := e.PatchContainedFn(shared, headA); !known || !contained {
		t.Errorf("PatchContainedFn(%s) = (%v, %v), want the patch found on the shared branch",
			headA, contained, known)
	}
}

// TestRunDiffBaseResolvesInsideACheckout pins why the issue's title case — a
// comment-only issue.diff body, written when computeIssueDiff gets NO base for
// a worktree — is not a shape a real retry produces.
//
// runDiffBase tries the worktree's fork point, then the run's pinned
// commit_sha, then the exec root's own HEAD. `run start` pins commit_sha from
// the exec root's HEAD (internal/cli/run_start.go) whenever that root is a
// checkout, and the last fallback reads the same HEAD, so inside a checkout a
// base always resolves — even for a worktree with no fork point and a run that
// pinned nothing. Only an exec root that is not a checkout yields no base, and
// there GitDiff has nothing to diff either way.
func TestRunDiffBaseResolvesInsideACheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	shared := gitRepo(t)
	sharedHead := gitRun(t, shared, "rev-parse", "HEAD")
	// A tree with no fork point at all: an unrelated repository's checkout.
	// Built by hand rather than with gitRepo, whose fixed identity and content
	// would mint the SAME root commit as `shared` and so a merge-base.
	unrelated := t.TempDir()
	gitRun(t, unrelated, "init", "-q", ".")
	writeFile(t, unrelated, "elsewhere.txt", "another history\n")
	gitRun(t, unrelated, "add", "-A")
	gitRun(t, unrelated, "commit", "-qm", "unrelated root")

	conn := mustDB(t)
	registerFixture(t, conn)
	issue := createIssue(t, conn, "diff base", "body", "task", nil)

	unpinned, err := db.InsertRunWithContext(conn, 1, "unpinned", 0, nowMS,
		db.RunContext{ExecRoot: shared})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, unpinned.ID, issue), "AddRunIssue: %v", err)

	base, live := runDiffBase(conn, unpinned.ID, unrelated, shared)
	if base != sharedHead || !live {
		t.Errorf("runDiffBase(no fork point, no pinned commit) = (%q, %v), "+
			"want the exec root's own HEAD %s as a live base", base, live, sharedHead)
	}

	noCheckout := t.TempDir()
	outside, err := db.InsertRunWithContext(conn, 1, "outside", 0, nowMS,
		db.RunContext{ExecRoot: noCheckout})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, outside.ID, issue), "AddRunIssue: %v", err)

	if base, _ := runDiffBase(conn, outside.ID, unrelated, noCheckout); base != "" {
		t.Errorf("runDiffBase(exec root is not a checkout) = %q, want no base", base)
	}
}
