package engine

import (
	"database/sql"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-193: a conductor may integrate an executor's work SELECTIVELY — cherry-
// picking part of the diff into a new commit — so the sha the step recorded is
// no longer an ancestor of the shared branch's HEAD, and every review packet
// rendered from it (git archive <target_sha>) silently reviews a tree the
// branch does not carry. The dispatch verbs are where the manifest meets the
// conductor, so they carry the structural check: rows whose packets render
// from a recorded target sha are flagged when that sha has diverged from the
// shared checkout's history.

// staleFixture completes implement@0 with a recorded head and worktree, so
// the review fanout's packets will render from that sha.
func staleFixture(t *testing.T, conn *sql.DB, e *Engine) {
	t.Helper()
	e.HeadFn = func(string) string { return "cafe1234cafe1234" }
	// The canned sha exists in no repository, so the DKT-424 tree probe could
	// answer nothing about it anyway. Faked to say exactly that, rather than
	// left to shell out to whatever checkout the test process happens to sit
	// in: these cases are about the ancestry verdict alone.
	e.TreeMatchFn = func(string, string) (match, known bool) { return false, false }
	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, implementID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("the change summary"),
		WorkDir:  "/worktrees/issue-under-test",
		NowMS:    nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)
}

func TestDispatchOpenAndVerifyFlagStaleTargets(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)

	e.IsAncestorFn = func(_, sha string) (ancestor, known bool) {
		if sha != "cafe1234cafe1234" {
			t.Errorf("ancestry asked about %q, want the recorded head", sha)
		}
		return false, true
	}

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 4 {
		t.Fatalf("stale targets = %d, want the four review siblings: %+v",
			len(m.StaleTargets), m.StaleTargets)
	}
	seen := map[string]bool{}
	for _, s := range m.StaleTargets {
		seen[s.Instance] = true
		if s.TargetSHA != "cafe1234cafe1234" {
			t.Errorf("%s target_sha = %q, want the recorded head", s.Instance, s.TargetSHA)
		}
		if s.SharedHead == "" || s.Reason == "" {
			t.Errorf("%s carries no shared head or reason: %+v", s.Instance, s)
		}
		// DKT-415: the reason must SAY which claim-time behavior applies.
		// RUN-26's conductor dispatched through this warning on a guess that
		// the packet would reconstruct its target from current HEAD at claim;
		// it does not (claim re-resolves the inputs and takes the recorded
		// round record's head), and a reason that leaves that to be guessed
		// cannot inform the decision it exists for.
		if !strings.Contains(s.Reason, "does not re-derive the target from HEAD") ||
			!strings.Contains(s.Reason, "resolves at claim time") {
			t.Errorf("%s reason %q does not name the claim-time semantics",
				s.Instance, s.Reason)
		}
	}
	if !seen["review@0#0"] || !seen["review@0#1"] || !seen["review@0#2"] || !seen["review@0#3"] {
		t.Errorf("stale targets name %v, want every review sibling", seen)
	}

	// verify recomputes the same advisory against the checkout as it stands.
	result, mismatch, err := e.VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "dispatch verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify mismatch: %+v", mismatch)
	}
	if !result.Verified || len(result.StaleTargets) != 4 {
		t.Errorf("verify: verified=%v stale=%d, want verified with every review row flagged",
			result.Verified, len(result.StaleTargets))
	}
}

func TestDispatchStaysQuietWhenTargetIsAncestor(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)

	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return true, true }
	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 0 {
		t.Errorf("an integrated (ancestor) target was flagged: %+v", m.StaleTargets)
	}
}

// The unanswerable case must not warn: git absent, a GC'd object, a tree that
// is not a repository. Absence of evidence is not staleness.
func TestDispatchStaysQuietWhenAncestryUnknown(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)

	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, false }
	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 0 {
		t.Errorf("an unanswerable ancestry question was reported as stale: %+v",
			m.StaleTargets)
	}
}

// TestGitAncestorOfHead drives the real implementation: three-valued git
// preserved as two booleans, with "could not answer" distinct from "not an
// ancestor".
func TestGitAncestorOfHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "commit", "--allow-empty", "-q", "-m", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "commit", "--allow-empty", "-q", "-m", "tip")
	tip := gitRun(t, repo, "rev-parse", "HEAD")

	// A commit NOT reachable from HEAD: the cherry-pick shape.
	gitRun(t, repo, "checkout", "-q", "-b", "side", base)
	gitRun(t, repo, "commit", "--allow-empty", "-q", "-m", "side work")
	side := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "-q", "-")

	cases := []struct {
		name            string
		execRoot, sha   string
		ancestor, known bool
	}{
		{"an ancestor", repo, base, true, true},
		{"HEAD itself", repo, tip, true, true},
		{"a diverged commit", repo, side, false, true},
		{"an unknown object", repo, "0123456789abcdef0123456789abcdef01234567", false, false},
		{"no repository", t.TempDir(), base, false, false},
		{"empty inputs", "", "", false, false},
	}
	for _, c := range cases {
		ancestor, known := gitAncestorOfHead(c.execRoot, c.sha)
		if ancestor != c.ancestor || known != c.known {
			t.Errorf("%s: = (%v, %v), want (%v, %v)",
				c.name, ancestor, known, c.ancestor, c.known)
		}
	}
}

// DKT-424: THE ANCESTRY TEST FALSE-POSITIVES ON THE SANCTIONED FLOW.
//
// The conduct protocol integrates an executor's isolated worktree commit with
// `git cherry-pick` onto the shared branch — a real commit, a NEW sha, byte-
// identical content. So the recorded target can never again be an ancestor of
// the shared HEAD, and RUN-33 drew a stale_targets entry on all five judge
// rows of a run whose `git diff <target> <head>` was empty. The tests below
// run REAL git, because a fake proves nothing about what a cherry-pick does
// to an object graph.

// cherryPickFixture builds that flow: base commit, the executor's own commit
// on an isolated line (the same object graph a `git worktree` produces, with
// one fewer moving part), implement@0 recording THAT sha, and then integration
// on the shared branch — an unrelated issue's commit first, because a real
// branch tip carries other work, then the cherry-pick.
//
// Returned: the repo, the recorded target sha, and the shared branch's name.
func cherryPickFixture(
	t *testing.T, conn *sql.DB, e *Engine, runID int,
) (repo, target, shared string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	repo = t.TempDir()
	gitRun(t, repo, "init", "-q")
	writeFile(t, repo, "internal/work.txt", "original\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	shared = gitRun(t, repo, "rev-parse", "--abbrev-ref", "HEAD")

	gitRun(t, repo, "checkout", "-q", "-b", "executor")
	writeFile(t, repo, "internal/work.txt", "the executor's change\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "implement the issue")
	target = gitRun(t, repo, "rev-parse", "HEAD")

	// The run's exec root is the checkout runExecRoot resolves, planted
	// directly as pregate_test.go and held_stale_test.go already do.
	execSQL(t, conn, `UPDATE runs SET exec_root = ? WHERE id = ?`, repo, runID)

	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, implementID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("the change summary"),
		WorkDir:  repo,
		NowMS:    nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	gitRun(t, repo, "checkout", "-q", shared)
	writeFile(t, repo, "internal/unrelated.txt", "another issue's work\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "an unrelated integration")
	gitRun(t, repo, "cherry-pick", target)
	return repo, target, shared
}

// TestDispatchAcquitsCherryPickIntegratedTarget is DKT-424's acceptance case:
// a worktree-recorded step, cherry-picked onto the shared branch and therefore
// tree-identical under a different sha, produces NO stale_targets entry.
//
// Its second half is why the comparison is path-scoped rather than a whole-
// tree diff: more unrelated work lands on the branch afterward, and the row
// must stay quiet. An unscoped diff would go non-empty on that commit and
// re-arm exactly the false positive this issue is about, one integration late.
func TestDispatchAcquitsCherryPickIntegratedTarget(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	repo, target, _ := cherryPickFixture(t, conn, e, run.ID)

	// THE PREMISE, ASSERTED RATHER THAN ASSUMED: ancestry really does fail
	// here (a definitive no, not an unanswerable question), and the tree
	// really is identical. Without both, this test could pass for the wrong
	// reason — the DKT-193 path staying silent because git said nothing.
	if ancestor, known := gitAncestorOfHead(repo, target); !known || ancestor {
		t.Fatalf("premise broken: ancestry = (%v, %v), want a definitive NO — "+
			"the cherry-pick must have put the target off HEAD's history",
			ancestor, known)
	}
	if out := gitOutput(t, repo, "diff", target, "HEAD", "--", "internal/work.txt"); out != "" {
		t.Fatalf("premise broken: the integrated content is not identical on the "+
			"work's own path:\n%s", out)
	}
	// And the UNSCOPED diff is deliberately NOT empty here — the branch's
	// unrelated commit shows up in it. That is the whole reason the check is
	// path-scoped: an unscoped comparison would call this row stale.
	if out := gitOutput(t, repo, "diff", target, "HEAD"); out == "" {
		t.Fatal("premise broken: the fixture's unrelated integration is missing, " +
			"so this case cannot distinguish a scoped comparison from a whole-tree one")
	}

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 0 {
		t.Fatalf("a cherry-pick-integrated target was flagged stale — DKT-424's "+
			"false positive on the sanctioned flow: %+v", m.StaleTargets)
	}

	// Unrelated work keeps landing on the shared branch. The packet's own
	// paths are untouched, so the row stays quiet at verify.
	writeFile(t, repo, "internal/unrelated.txt", "a third issue's work\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "more unrelated integration")

	result, mismatch, err := e.VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "dispatch verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify mismatch: %+v", mismatch)
	}
	if len(result.StaleTargets) != 0 {
		t.Errorf("unrelated work elsewhere on the branch re-armed the warning "+
			"— the comparison is not scoped to the packet's paths: %+v",
			result.StaleTargets)
	}
}

// TestDispatchWarnsWhenTheWorksOwnPathsDiverged is the other acceptance
// criterion, and the falsification of the fix: a target whose tree differs
// from the shared HEAD ON THE PACKET'S PATHS still warns, and the reason says
// the difference was measured rather than merely inferred from a sha.
func TestDispatchWarnsWhenTheWorksOwnPathsDiverged(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	repo, target, _ := cherryPickFixture(t, conn, e, run.ID)

	// Integration then goes its own way on the very file the work touched:
	// the branch no longer carries the reviewed content.
	writeFile(t, repo, "internal/work.txt", "integration rewrote this\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "re-integrated differently")
	sharedHead := gitRun(t, repo, "rev-parse", "HEAD")

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 4 {
		t.Fatalf("stale targets = %d, want the four review siblings — a genuine "+
			"divergence must still warn: %+v", len(m.StaleTargets), m.StaleTargets)
	}
	for _, s := range m.StaleTargets {
		if s.TargetSHA != target {
			t.Errorf("%s target_sha = %q, want the recorded head %q",
				s.Instance, s.TargetSHA, target)
		}
		if s.SharedHead != sharedHead {
			t.Errorf("%s shared_head = %q, want the checkout's real HEAD %q",
				s.Instance, s.SharedHead, sharedHead)
		}
		if !strings.Contains(s.Reason, "not an ancestor") ||
			!strings.Contains(s.Reason, "still differs from that HEAD on the paths") {
			t.Errorf("%s reason %q does not name BOTH measurements — after "+
				"DKT-424 the ancestry fact alone is not the finding",
				s.Instance, s.Reason)
		}
	}
}

// TestStaleTargetReasonSeparatesMeasuredFromUnanswered pins the one thing the
// two advisory shapes must never do: read alike. A measured tree difference is
// a divergence to act on; an unanswerable tree question is a warning the
// conductor still has to check by hand, and the string has to say which it is.
func TestStaleTargetReasonSeparatesMeasuredFromUnanswered(t *testing.T) {
	measured := staleTargetReason("abc123abc123abc", "def456def456def", true)
	unanswered := staleTargetReason("abc123abc123abc", "def456def456def", false)

	if !strings.Contains(measured, "still differs from that HEAD on the paths") {
		t.Errorf("the measured reason does not name the tree comparison: %q", measured)
	}
	if strings.Contains(measured, "could not be determined") {
		t.Errorf("the measured reason hedges as if nothing was measured: %q", measured)
	}
	if !strings.Contains(unanswered, "could not be determined") ||
		!strings.Contains(unanswered, "cherry-picked integration mints a new sha") {
		t.Errorf("the unanswered reason does not say the tree question went "+
			"unanswered, nor name DKT-424's cause: %q", unanswered)
	}
	// Both keep DKT-415's claim-time semantics: the decision they inform is
	// still "is dispatching through this safe".
	for _, r := range []string{measured, unanswered} {
		if !strings.Contains(r, "does not re-derive the target from HEAD") ||
			!strings.Contains(r, "resolves at claim time") {
			t.Errorf("reason %q dropped the claim-time semantics", r)
		}
	}
}

// TestDispatchStaysQuietWhenTheTreeMatches drives the judge through the seam
// rather than through git: a disproved ancestry plus a matching tree acquits,
// whatever produced the answer.
func TestDispatchStaysQuietWhenTheTreeMatches(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)

	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, true }
	asked := 0
	e.TreeMatchFn = func(_, sha string) (match, known bool) {
		asked++
		if sha != "cafe1234cafe1234" {
			t.Errorf("the tree probe asked about %q, want the recorded head", sha)
		}
		return true, true
	}

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 0 {
		t.Errorf("a tree-identical target was flagged: %+v", m.StaleTargets)
	}
	// One question per SHA, not per row: four review siblings share one
	// recorded target, and the git call is not free.
	if asked != 1 {
		t.Errorf("the tree probe ran %d times for one shared sha, want 1", asked)
	}
}

// A target the ancestry test ACQUITS never reaches the tree probe: the second
// question exists only to overturn the first, so asking it otherwise would be
// pure subprocess cost.
func TestTreeProbeIsSkippedWhenAncestryAlreadyAcquits(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)

	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return true, true }
	e.TreeMatchFn = func(_, _ string) (match, known bool) {
		t.Error("the tree probe ran on a target ancestry had already acquitted")
		return false, false
	}
	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 0 {
		t.Errorf("an integrated (ancestor) target was flagged: %+v", m.StaleTargets)
	}
}

// An engine with no tree probe wired keeps DKT-193's behavior exactly: the
// probe may only ACQUIT, so its absence cannot silence a warning it did not
// disprove.
func TestMissingTreeProbeLeavesTheAncestryVerdictStanding(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	staleFixture(t, conn, e)

	e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, true }
	e.TreeMatchFn = nil

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 4 {
		t.Fatalf("stale targets = %d, want the four review siblings: %+v",
			len(m.StaleTargets), m.StaleTargets)
	}
	if !strings.Contains(m.StaleTargets[0].Reason, "could not be determined") {
		t.Errorf("reason %q claims a tree difference nothing measured",
			m.StaleTargets[0].Reason)
	}
}

// TestGitTreeMatchesHead drives the real implementation across the four
// answers it owes: a cherry-picked target (identical on the work's paths, with
// unrelated commits on both sides), a genuine divergence on those paths, and
// the two unanswerable shapes DKT-193's fail-open convention already draws.
func TestGitTreeMatchesHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	writeFile(t, repo, "a.txt", "original\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	shared := gitRun(t, repo, "rev-parse", "--abbrev-ref", "HEAD")

	// The executor's line, and an empty commit beside it — a target that
	// touched nothing has no paths to compare, and here its root tree differs
	// from HEAD's too (DKT-451's fallback finds nothing to acquit on either),
	// so the answer is unanswerable rather than a match.
	gitRun(t, repo, "checkout", "-q", "-b", "executor")
	writeFile(t, repo, "a.txt", "changed\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "the work")
	target := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "-q", "-b", "empty-line", base)
	gitRun(t, repo, "commit", "--allow-empty", "-q", "-m", "touches nothing")
	emptyTarget := gitRun(t, repo, "rev-parse", "HEAD")

	// Integration: unrelated work, then the cherry-pick, then more unrelated
	// work — the shape a shared branch actually has.
	gitRun(t, repo, "checkout", "-q", shared)
	writeFile(t, repo, "b.txt", "someone else's file\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "unrelated")
	gitRun(t, repo, "cherry-pick", target)
	writeFile(t, repo, "b.txt", "someone else's file, edited\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "more unrelated")

	if match, known := gitTreeMatchesHead(repo, target); !match || !known {
		t.Errorf("cherry-picked target: = (%v, %v), want (true, true) — the "+
			"branch carries this tree on the work's own paths", match, known)
	}
	if match, known := gitTreeMatchesHead(repo, emptyTarget); match || known {
		t.Errorf("a target that touched no path: = (%v, %v), want (false, "+
			"false) — there is no evidence there to acquit on", match, known)
	}
	for _, c := range []struct {
		name          string
		execRoot, sha string
	}{
		{"an unknown object", repo, "0123456789abcdef0123456789abcdef01234567"},
		{"no repository", t.TempDir(), target},
		{"empty inputs", "", ""},
	} {
		if match, known := gitTreeMatchesHead(c.execRoot, c.sha); match || known {
			t.Errorf("%s: = (%v, %v), want (false, false)", c.name, match, known)
		}
	}

	// The divergence: integration rewrites the file the work touched.
	writeFile(t, repo, "a.txt", "integration rewrote this\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "re-integrated differently")
	if match, known := gitTreeMatchesHead(repo, target); match || !known {
		t.Errorf("a diverged target: = (%v, %v), want (false, true) — the "+
			"work's own path differs and that is a measured no", match, known)
	}
}

// DKT-451: THE PATH-SCOPED PROBE HAS TWO SHAPES IT CANNOT ANSWER, AND THE ROOT
// TREE ANSWERS BOTH.
//
// RUN-37's reconcile warned on a review row whose recorded target and shared
// HEAD carried the SAME tree object (f36e7b157a5d), and the conductor spent
// ~50s establishing that by hand. Where the scoped comparison finds no merge
// base, or a target that touched no path relative to the base it does have, it
// reports "unanswered" and the ancestry warning stands — even though two
// commits sharing one root tree carry identical content everywhere.

// TestGitTreeMatchesHeadAcquitsOnRootTreeEquality drives the real
// implementation across both shapes, asserting the pre-fix verdict of the
// scoped half beside the fixed verdict of the whole, so a pass cannot come from
// the scoped comparison quietly answering after all.
func TestGitTreeMatchesHeadAcquitsOnRootTreeEquality(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	writeFile(t, repo, "internal/work.txt", "the integrated content\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	shared := gitRun(t, repo, "rev-parse", "--abbrev-ref", "HEAD")

	// SHAPE ONE: a target off HEAD's history that touched no path relative to
	// the merge base — no path set to compare on.
	gitRun(t, repo, "checkout", "-q", "-b", "empty-line", base)
	gitRun(t, repo, "commit", "--allow-empty", "-q", "-m", "records nothing new")
	emptyTarget := gitRun(t, repo, "rev-parse", "HEAD")

	// SHAPE TWO: a history with no seam to HEAD's at all, carrying the same
	// content — no merge base to take a path set from.
	gitRun(t, repo, "checkout", "-q", "--orphan", "unrelated")
	gitRun(t, repo, "commit", "-q", "-m", "same content, no shared history")
	orphanTarget := gitRun(t, repo, "rev-parse", "HEAD")
	gitRun(t, repo, "checkout", "-q", shared)

	// THE PREMISES, ASSERTED RATHER THAN ASSUMED. Both targets must be a
	// definitive ancestry NO (otherwise DKT-193 never warns and this test
	// proves nothing), the scoped half must be unable to answer about either
	// (that is the gap), and the root trees must really be one object.
	if _, ok := gitMergeBaseWithHead(repo, orphanTarget); ok {
		t.Fatal("premise broken: the orphan target shares history with HEAD")
	}
	if paths, ok := gitPathsTouchedSince(repo, base, emptyTarget); !ok || len(paths) != 0 {
		t.Fatalf("premise broken: the empty target touched %v", paths)
	}
	head, ok := gitTreeHash(repo, "HEAD")
	if !ok {
		t.Fatal("premise broken: HEAD has no resolvable tree")
	}
	for _, c := range []struct {
		name, sha string
	}{{"the empty target", emptyTarget}, {"the orphan target", orphanTarget}} {
		if ancestor, known := gitAncestorOfHead(repo, c.sha); !known || ancestor {
			t.Fatalf("premise broken: %s ancestry = (%v, %v), want a definitive NO",
				c.name, ancestor, known)
		}
		if tree, ok := gitTreeHash(repo, c.sha); !ok || tree != head {
			t.Fatalf("premise broken: %s tree = %q, want HEAD's %q",
				c.name, tree, head)
		}
		if match, known := gitTouchedPathsMatchHead(repo, c.sha); match || known {
			t.Fatalf("premise broken: the scoped half answered about %s = (%v, %v), "+
				"so this case no longer covers the gap DKT-451 is about",
				c.name, match, known)
		}
		if match, known := gitTreeMatchesHead(repo, c.sha); !match || !known {
			t.Errorf("%s: = (%v, %v), want (true, true) — one root tree is "+
				"identical content everywhere, which acquits", c.name, match, known)
		}
	}

	// THE FALSIFICATION: integration goes its own way, the root trees part, and
	// both shapes fall back to the unanswerable verdict — never to a claimed
	// difference the scoped comparison did not measure.
	writeFile(t, repo, "internal/work.txt", "integration rewrote this\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "re-integrated differently")
	for _, c := range []struct {
		name, sha string
	}{{"the empty target", emptyTarget}, {"the orphan target", orphanTarget}} {
		if match, known := gitTreeMatchesHead(repo, c.sha); match || known {
			t.Errorf("%s after divergence: = (%v, %v), want (false, false) — "+
				"differing root trees are not evidence about THIS work's paths",
				c.name, match, known)
		}
	}
}

// TestDispatchVerifyAcquitsWorktreeCherryPick walks DKT-451's stated repro with
// no substitutions: record a step commit in a REAL linked worktree, cherry-pick
// it onto the shared branch, run `dispatch verify`. The sha is rewritten and
// the content is byte-identical, so the reconcile must be silent.
//
// The linked worktree matters — it is the object graph the conduct protocol
// actually produces, and every other case here uses a branch in one checkout.
func TestDispatchVerifyAcquitsWorktreeCherryPick(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	writeFile(t, repo, "internal/work.txt", "original\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")

	work := filepath.Join(t.TempDir(), "executor")
	gitRun(t, repo, "worktree", "add", "-q", "-b", "executor", work)
	writeFile(t, work, "internal/work.txt", "the executor's change\n")
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "implement the issue")
	target := gitRun(t, work, "rev-parse", "HEAD")

	execSQL(t, conn, `UPDATE runs SET exec_root = ? WHERE id = ?`, repo, run.ID)
	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, implementID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("the change summary"),
		WorkDir:  work,
		NowMS:    nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	// The shared branch moves and is backed out again before integration. That
	// is not decoration: a cherry-pick onto the target's OWN parent, by the
	// same identity in the same second, reproduces the commit object bit for
	// bit and the sha never changes. Landing it on a different parent mints the
	// new sha the real flow always mints, while the branch's content comes back
	// to exactly what the executor built on — RUN-37's shape, one tree object
	// under two shas.
	writeFile(t, repo, "internal/unrelated.txt", "another issue's work\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "an unrelated integration")
	gitRun(t, repo, "rm", "-q", "internal/unrelated.txt")
	gitRun(t, repo, "commit", "-q", "-m", "backed out again")

	// Integration, the sanctioned way.
	gitRun(t, repo, "cherry-pick", target)
	if head := gitRun(t, repo, "rev-parse", "HEAD"); head == target {
		t.Fatalf("premise broken: the cherry-pick reproduced the sha %s, so "+
			"nothing here is the integration path DKT-451 is about", head)
	}

	// THE PREMISE: the sha really was rewritten (a definitive ancestry NO) and
	// the content really is identical, tree object for tree object — RUN-37's
	// exact shape.
	if ancestor, known := gitAncestorOfHead(repo, target); !known || ancestor {
		t.Fatalf("premise broken: ancestry = (%v, %v), want a definitive NO",
			ancestor, known)
	}
	targetTree, ok := gitTreeHash(repo, target)
	headTree, headOK := gitTreeHash(repo, "HEAD")
	if !ok || !headOK || targetTree != headTree {
		t.Fatalf("premise broken: trees %q vs %q — the cherry-pick did not "+
			"reproduce the content", targetTree, headTree)
	}

	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(m.StaleTargets) != 0 {
		t.Fatalf("dispatch open flagged a tree-identical integration: %+v",
			m.StaleTargets)
	}
	result, mismatch, err := e.VerifyDispatch(conn, run.ID, nowMS)
	testsupport.Must(t, err, "dispatch verify: %v", err)
	if mismatch != nil {
		t.Fatalf("verify mismatch: %+v", mismatch)
	}
	if len(result.StaleTargets) != 0 {
		t.Errorf("dispatch verify warned on the designed integration path — "+
			"DKT-451's false positive: %+v", result.StaleTargets)
	}
}
