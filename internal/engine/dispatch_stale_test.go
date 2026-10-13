package engine

import (
	"database/sql"
	"fmt"
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
	// The DKT-1033 patch probe, faked unanswerable for the same reason.
	e.PatchContainedFn = func(string, string) (contained, known bool) { return false, false }
	// Existence unanswerable by default, for the tree probe's exact reason:
	// these cases are about the ancestry verdict alone, and the DKT-742
	// absence probe accuses only on a DEFINITIVE "no such object".
	e.ObjectExistsFn = func(string, string) (exists, known bool) { return false, false }
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

// The unanswerable case must not warn: git absent, a tree that is not a
// repository. Absence of evidence is not staleness — but only while it stays
// ABSENCE of evidence: a definitive "no such object" is evidence, and the
// DKT-742 cases below prove it warns.
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

// TestDispatchWarnsWhenConflictResolutionEditedTheHunk is the other acceptance
// criterion, and the falsification of the fix: a target whose cherry-pick
// conflicted and whose hunk was EDITED in the resolution — so the patch HEAD
// carries is not the patch the packet renders — still warns, and the reason
// says the patch was measured missing rather than inferring it from a sha.
//
// The pick is `-x`, deliberately: the resolution commit carries a `(cherry
// picked from commit <target>)` trailer, which is why a trailer scan cannot be
// the containment test — it would acquit exactly this case.
func TestDispatchWarnsWhenConflictResolutionEditedTheHunk(t *testing.T) {
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
	shared := gitRun(t, repo, "rev-parse", "--abbrev-ref", "HEAD")

	gitRun(t, repo, "checkout", "-q", "-b", "executor")
	writeFile(t, repo, "internal/work.txt", "the executor's change\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "implement the issue")
	target := gitRun(t, repo, "rev-parse", "HEAD")

	execSQL(t, conn, `UPDATE runs SET exec_root = ? WHERE id = ?`, repo, run.ID)
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

	// The shared branch has edited the same line by the time the pick lands,
	// so the pick conflicts, and the conductor resolves it by hand to
	// something that is neither side.
	gitRun(t, repo, "checkout", "-q", shared)
	writeFile(t, repo, "internal/work.txt", "a sibling's change\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "a sibling's integration")
	gitConflict(t, repo, "cherry-pick", "-x", target)
	writeFile(t, repo, "internal/work.txt", "the executor's change, edited in resolution\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "--no-edit")
	sharedHead := gitRun(t, repo, "rev-parse", "HEAD")

	// THE PREMISES: ancestry is a definitive no, and the trailer really is
	// there — so a containment test that trusted it would acquit this row.
	if ancestor, known := gitAncestorOfHead(repo, target); !known || ancestor {
		t.Fatalf("premise broken: ancestry = (%v, %v), want a definitive NO",
			ancestor, known)
	}
	if msg := gitRun(t, repo, "log", "-1", "--format=%B"); !strings.Contains(msg,
		"(cherry picked from commit "+target+")") {
		t.Fatalf("premise broken: the resolution commit carries no cherry-pick "+
			"trailer, so this case no longer shows why a trailer scan is not "+
			"the test:\n%s", msg)
	}

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
			!strings.Contains(s.Reason, "no commit on the shared branch since their merge base carries its patch") ||
			!strings.Contains(s.Reason, "diverged") {
			t.Errorf("%s reason %q does not name BOTH measurements — the "+
				"ancestry fact alone is not the finding, and only a patch "+
				"measured missing may be called a divergence",
				s.Instance, s.Reason)
		}
	}
}

// gitConflict runs a git command that is EXPECTED to stop on a conflict, and
// fails the test if it did not — a fixture that means to resolve a conflict by
// hand must have had one.
func gitConflict(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false",
	}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("premise broken: git %v applied cleanly, but this fixture "+
			"needs a conflict to resolve:\n%s", args, out)
	}
}

// TestStaleTargetReasonSeparatesTheThreeShapes pins the one thing the advisory
// shapes must never do: read alike. A patch measured missing is a divergence to
// act on; a tree the patch probe could not check is content to hand-check
// against whatever landed on those paths since; an unanswerable question is a
// probe to repair. The string has to say which it is, and only the first may
// say "diverged" (DKT-1033).
func TestStaleTargetReasonSeparatesTheThreeShapes(t *testing.T) {
	missing := staleTargetReason("abc123abc123abc", "def456def456def", stalePatchMissing)
	unmatched := staleTargetReason("abc123abc123abc", "def456def456def", staleTreeUnmatched)
	unanswered := staleTargetReason("abc123abc123abc", "def456def456def", staleUndetermined)

	if !strings.Contains(missing, "diverged") ||
		!strings.Contains(missing, "no commit on the shared branch") {
		t.Errorf("the missing-patch reason does not name the patch measurement: %q", missing)
	}
	if strings.Contains(missing, "could not be determined") ||
		strings.Contains(missing, "could not be matched") {
		t.Errorf("the missing-patch reason hedges as if nothing was measured: %q", missing)
	}
	if !strings.Contains(unmatched, "could not be matched") ||
		!strings.Contains(unmatched, "could not be tested") {
		t.Errorf("the unmatched-tree reason does not say the tree was compared "+
			"because the patch could not be: %q", unmatched)
	}
	if !strings.Contains(unanswered, "could not be determined") ||
		!strings.Contains(unanswered, "cherry-picked integration mints a new sha") {
		t.Errorf("the unanswered reason does not say the question went "+
			"unanswered, nor name DKT-424's cause: %q", unanswered)
	}
	// The word belongs to the patch measurement alone. RUN-67 read it on nine
	// rows whose patches were intact.
	for _, r := range []string{unmatched, unanswered} {
		if strings.Contains(strings.ToLower(r), "diverge") {
			t.Errorf("a reason that measured no patch claims a divergence: %q", r)
		}
	}
	// All three keep DKT-415's claim-time semantics: the decision they inform
	// is still "is dispatching through this safe".
	for _, r := range []string{missing, unmatched, unanswered} {
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
	if !strings.Contains(m.StaleTargets[0].Reason, "could not be determined") ||
		strings.Contains(strings.ToLower(m.StaleTargets[0].Reason), "diverge") {
		t.Errorf("reason %q claims a difference nothing measured",
			m.StaleTargets[0].Reason)
	}
}

// TestStaleTargetsNameTheProbeThatAccused drives the two probes through the
// seam in every combination that changes the outcome (DKT-1033): the patch
// probe acquits alone and is asked first; a tree acquittal still stands after
// a patch miss (a squash of several issues carries this work's content without
// its patch-id); and the reason's wording follows the probe that measured —
// "diverged" only from a patch measured missing, "could not be matched" from
// a tree the patch probe could not check, "could not be determined" from
// neither.
func TestStaleTargetsNameTheProbeThatAccused(t *testing.T) {
	known := func(v bool) func(string, string) (bool, bool) {
		return func(string, string) (bool, bool) { return v, true }
	}
	unknown := func(string, string) (bool, bool) { return false, false }

	cases := []struct {
		name        string
		patch, tree func(string, string) (bool, bool)
		stale       bool
		treeAsked   bool
		want        []string
		forbid      []string
	}{
		{
			name:  "the patch probe acquits, and the tree probe is never asked",
			patch: known(true), tree: known(false),
			stale: false, treeAsked: false,
		},
		{
			name:  "the patch is missing and the tree differs: a divergence",
			patch: known(false), tree: known(false),
			stale: true, treeAsked: true,
			want:   []string{"diverged", "no commit on the shared branch"},
			forbid: []string{"could not be matched", "could not be determined"},
		},
		{
			name:  "the patch is missing but the tree matches: the squash shape acquits",
			patch: known(false), tree: known(true),
			stale: false, treeAsked: true,
		},
		{
			name:  "the patch could not be checked and the tree differs: not a divergence",
			patch: unknown, tree: known(false),
			stale: true, treeAsked: true,
			want:   []string{"could not be matched"},
			forbid: []string{"diverge", "could not be determined"},
		},
		{
			name:  "no patch probe wired and the tree differs: not a divergence",
			patch: nil, tree: known(false),
			stale: true, treeAsked: true,
			want:   []string{"could not be matched"},
			forbid: []string{"diverge"},
		},
		{
			name:  "neither probe can answer",
			patch: unknown, tree: unknown,
			stale: true, treeAsked: true,
			want:   []string{"could not be determined"},
			forbid: []string{"diverge", "could not be matched"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn := mustDB(t)
			run, _ := activatedRun(t, conn)
			e := testEngine()
			staleFixture(t, conn, e)
			e.IsAncestorFn = func(_, _ string) (ancestor, known bool) { return false, true }
			e.PatchContainedFn = c.patch
			treeAsked := false
			e.TreeMatchFn = func(execRoot, sha string) (match, known bool) {
				treeAsked = true
				return c.tree(execRoot, sha)
			}

			m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
			testsupport.Must(t, err, "dispatch open: %v", err)
			if treeAsked != c.treeAsked {
				t.Errorf("tree probe asked = %v, want %v", treeAsked, c.treeAsked)
			}
			if !c.stale {
				if len(m.StaleTargets) != 0 {
					t.Fatalf("an acquitted target was flagged: %+v", m.StaleTargets)
				}
				return
			}
			if len(m.StaleTargets) != 4 {
				t.Fatalf("stale targets = %d, want the four review siblings: %+v",
					len(m.StaleTargets), m.StaleTargets)
			}
			reason := m.StaleTargets[0].Reason
			for _, w := range c.want {
				if !strings.Contains(reason, w) {
					t.Errorf("reason lacks %q: %q", w, reason)
				}
			}
			for _, f := range c.forbid {
				if strings.Contains(strings.ToLower(reason), f) {
					t.Errorf("reason claims %q, which nothing measured: %q", f, reason)
				}
			}
		})
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

// DKT-1033: THE TREE COMPARISON FALSE-POSITIVES ON A SIBLING'S INTEGRATION.
//
// RUN-67's `dispatch open` warned "integration diverged" on all nine review
// rows of a run whose three integrations were plain `git cherry-pick -x`: two
// into non-overlapping regions of one Makefile, one followed by a conductor
// patch on the same test file, every hunk byte-identical on HEAD. DKT-424's
// probe compared the target's TREE with HEAD's on the paths the work touched,
// and a sibling's commit on the same path is a tree difference whatever it did
// to this work's hunks. The tests below run REAL git, for DKT-424's reason.

// numberedLines renders a ten-line file, with the given lines replaced — the
// Makefile two issues edit in different places.
func numberedLines(edits map[int]string) string {
	var b strings.Builder
	for i := 1; i <= 10; i++ {
		if body, ok := edits[i]; ok {
			b.WriteString(body)
		} else {
			fmt.Fprintf(&b, "line %d", i)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// siblingFixture builds RUN-67's shape on one file: the executor's commit edits
// line 3, a SIBLING issue's commit edits `siblingLine`, and the shared branch
// integrates the sibling first and then the target, both by `git cherry-pick
// -x`. The distance between the two edits is what separates the two probes:
// far apart, `git cherry`'s three-line-context patch-id already matches; close
// together, only the zero-context comparison does.
func siblingFixture(
	t *testing.T, conn *sql.DB, e *Engine, runID, siblingLine int,
) (repo, target string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	repo = t.TempDir()
	gitRun(t, repo, "init", "-q")
	writeFile(t, repo, "Makefile", numberedLines(nil))
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	shared := gitRun(t, repo, "rev-parse", "--abbrev-ref", "HEAD")

	gitRun(t, repo, "checkout", "-q", "-b", "executor")
	writeFile(t, repo, "Makefile", numberedLines(map[int]string{3: "line 3, by the target"}))
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "implement the issue")
	target = gitRun(t, repo, "rev-parse", "HEAD")

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

	gitRun(t, repo, "checkout", "-q", "-b", "sibling", shared)
	writeFile(t, repo, "Makefile", numberedLines(map[int]string{
		siblingLine: fmt.Sprintf("line %d, by the sibling", siblingLine),
	}))
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "implement the sibling issue")
	sibling := gitRun(t, repo, "rev-parse", "HEAD")

	gitRun(t, repo, "checkout", "-q", shared)
	gitRun(t, repo, "cherry-pick", "-x", sibling)
	gitRun(t, repo, "cherry-pick", "-x", target)
	return repo, target
}

// TestDispatchAcquitsSiblingIntegrationOnTheSamePath is DKT-1033's acceptance
// case: two issues cherry-picked into different regions of one file produce NO
// stale_targets entry after both picks — in both distances, and again after a
// conductor's follow-up patch lands on the same file.
func TestDispatchAcquitsSiblingIntegrationOnTheSamePath(t *testing.T) {
	cases := []struct {
		name        string
		siblingLine int
		// What `git cherry HEAD <target>` marks the target with: `-` where its
		// three-line-context patch-id already matches, `+` where the sibling's
		// edit sits inside that context and only the zero-context comparison
		// can see the patch is intact.
		cherryMark string
	}{
		{"a distant region of the same file", 9, "-"},
		{"a region inside the patch's own context window", 5, "+"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn := mustDB(t)
			run, _ := activatedRun(t, conn)
			e := testEngine()
			repo, target := siblingFixture(t, conn, e, run.ID, c.siblingLine)

			// THE PREMISES, ASSERTED RATHER THAN ASSUMED. Ancestry really
			// fails (the sha was rewritten), the target's own hunk really is
			// intact on HEAD, DKT-424's tree probe really does call this a
			// difference (without that, the case would pass under the old
			// code and prove nothing), and `git cherry` reads the two
			// distances the way the fixture says it does.
			if ancestor, known := gitAncestorOfHead(repo, target); !known || ancestor {
				t.Fatalf("premise broken: ancestry = (%v, %v), want a definitive NO",
					ancestor, known)
			}
			if body := gitOutput(t, repo, "show", "HEAD:Makefile"); !strings.Contains(body, "line 3, by the target\n") {
				t.Fatalf("premise broken: HEAD's Makefile lost the target's hunk:\n%s", body)
			}
			if match, known := gitTouchedPathsMatchHead(repo, target); match || !known {
				t.Fatalf("premise broken: the tree probe = (%v, %v), want a measured "+
					"NO — the sibling's edit must make HEAD's tree differ on the "+
					"shared path, or this case does not reproduce DKT-1033",
					match, known)
			}
			if marks := gitOutput(t, repo, "cherry", "HEAD", target); !strings.HasPrefix(marks, c.cherryMark+" ") {
				t.Fatalf("premise broken: `git cherry HEAD <target>` = %q, want a %q "+
					"mark for %s", marks, c.cherryMark, c.name)
			}

			m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
			testsupport.Must(t, err, "dispatch open: %v", err)
			if len(m.StaleTargets) != 0 {
				t.Fatalf("a sibling's integration on the same path was read as "+
					"this target's divergence — DKT-1033: %+v", m.StaleTargets)
			}

			// RUN-67's third shape: a conductor's follow-up patch lands on
			// the same file AFTER integration. The target's hunk is still
			// intact, and verify must stay quiet.
			body := gitOutput(t, repo, "show", "HEAD:Makefile")
			writeFile(t, repo, "Makefile",
				strings.Replace(body, "line 7\n", "line 7, by the conductor\n", 1))
			gitRun(t, repo, "add", "-A")
			gitRun(t, repo, "commit", "-q", "-m", "a follow-up patch on the same file")

			result, mismatch, err := e.VerifyDispatch(conn, run.ID, nowMS)
			testsupport.Must(t, err, "dispatch verify: %v", err)
			if mismatch != nil {
				t.Fatalf("verify mismatch: %+v", mismatch)
			}
			if len(result.StaleTargets) != 0 {
				t.Errorf("a follow-up patch on the same file re-armed the "+
					"warning: %+v", result.StaleTargets)
			}
		})
	}
}

// TestGitPatchContainedInHead drives the real implementation across every
// answer it owes: a clean pick at both distances, a squashed line, an edited
// resolution, a line that never landed, a later rewrite of the work's own
// lines, and the unanswerable shapes.
func TestGitPatchContainedInHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q")
	writeFile(t, repo, "Makefile", numberedLines(nil))
	writeFile(t, repo, "notes.txt", "notes\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	base := gitRun(t, repo, "rev-parse", "HEAD")
	shared := gitRun(t, repo, "rev-parse", "--abbrev-ref", "HEAD")

	line := func(name string, edits map[int]string, msg string) string {
		gitRun(t, repo, "checkout", "-q", "-b", name, base)
		writeFile(t, repo, "Makefile", numberedLines(edits))
		gitRun(t, repo, "add", "-A")
		gitRun(t, repo, "commit", "-q", "-m", msg)
		return gitRun(t, repo, "rev-parse", "HEAD")
	}
	// The target: one commit editing line 3.
	target := line("executor", map[int]string{3: "line 3, by the target"}, "the work")
	// A two-commit line the conductor will squash into one commit.
	line("two", map[int]string{10: "line 10, by two"}, "two, first half")
	writeFile(t, repo, "extra.txt", "two's new file\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "two, second half")
	squashed := gitRun(t, repo, "rev-parse", "HEAD")
	// A line whose pick will conflict and be resolved to something else.
	conflicting := line("conflicting", map[int]string{8: "line 8, by the conflicting line"}, "conflicting work")
	// A line that never lands.
	gitRun(t, repo, "checkout", "-q", "-b", "never", base)
	writeFile(t, repo, "notes.txt", "notes, never integrated\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "never integrated")
	never := gitRun(t, repo, "rev-parse", "HEAD")
	// An empty commit: no patch content to match (DKT-451's shape).
	gitRun(t, repo, "checkout", "-q", "-b", "empty-line", base)
	gitRun(t, repo, "commit", "--allow-empty", "-q", "-m", "touches nothing")
	empty := gitRun(t, repo, "rev-parse", "HEAD")

	// Integration on the shared branch: a sibling inside the target's context
	// window first, then the target's pick, then the squash, then the
	// conflicted pick resolved by hand.
	gitRun(t, repo, "checkout", "-q", shared)
	writeFile(t, repo, "Makefile", numberedLines(map[int]string{5: "line 5, by the sibling"}))
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "the sibling's integration")
	gitRun(t, repo, "cherry-pick", "-x", target)
	gitRun(t, repo, "cherry-pick", "-n", squashed+"~1", squashed)
	gitRun(t, repo, "commit", "-q", "-m", "two, squashed")
	body := gitOutput(t, repo, "show", "HEAD:Makefile")
	writeFile(t, repo, "Makefile", strings.Replace(body, "line 8\n", "line 8, by the shared branch\n", 1))
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "the shared branch's own edit")
	gitConflict(t, repo, "cherry-pick", "-x", conflicting)
	body = gitOutput(t, repo, "show", "HEAD:Makefile")
	writeFile(t, repo, "Makefile", strings.Replace(body, "line 8, by the shared branch\n", "line 8, resolved by hand\n", 1))
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "--no-edit")

	// The premise that separates the two probes: the sibling's edit sits
	// inside the target's three-line context, so `git cherry` alone does not
	// see the pick.
	if marks := gitOutput(t, repo, "cherry", "HEAD", target); !strings.HasPrefix(marks, "+ ") {
		t.Fatalf("premise broken: `git cherry HEAD <target>` = %q, want a `+` — "+
			"the sibling's edit must sit inside the target's context window", marks)
	}

	cases := []struct {
		name             string
		execRoot, sha    string
		contained, known bool
	}{
		{"a clean pick beside a sibling's edit", repo, target, true, true},
		{"a squashed two-commit line", repo, squashed, true, true},
		{"a pick whose hunk was edited in resolution", repo, conflicting, false, true},
		{"a line that never landed", repo, never, false, true},
		{"an empty commit", repo, empty, false, false},
		{"an unknown object", repo, "0123456789abcdef0123456789abcdef01234567", false, false},
		{"no repository", t.TempDir(), target, false, false},
		{"empty inputs", "", "", false, false},
	}
	for _, c := range cases {
		if contained, known := gitPatchContainedInHead(c.execRoot, c.sha); contained != c.contained || known != c.known {
			t.Errorf("%s: = (%v, %v), want (%v, %v)",
				c.name, contained, known, c.contained, c.known)
		}
	}

	// A LATER commit rewriting the work's own lines is not a divergence of the
	// integration: the branch carried the patch and then moved on, which is
	// what a branch does. The tree probe reads this as a difference — that is
	// why it may no longer word one — and the patch probe does not.
	body = gitOutput(t, repo, "show", "HEAD:Makefile")
	writeFile(t, repo, "Makefile", strings.Replace(body, "line 3, by the target\n", "line 3, rewritten later\n", 1))
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "a later rewrite of the target's line")
	if contained, known := gitPatchContainedInHead(repo, target); !contained || !known {
		t.Errorf("after a later rewrite: = (%v, %v), want (true, true) — the "+
			"integration carried the patch; what landed after is not its divergence",
			contained, known)
	}
	if match, known := gitTreeMatchesHead(repo, target); match || !known {
		t.Errorf("the tree probe after a later rewrite: = (%v, %v), want (false, "+
			"true) — the shape it must no longer call a divergence", match, known)
	}
}
