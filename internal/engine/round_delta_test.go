package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-171: a loop re-entry's round delta diffed prev..HEAD, and a fresh
// worktree forked after integration inherits every commit the shared branch
// gained between rounds — sibling issues' cherry-picks rendered as this
// issue's own round work (RUN-14/HRN-25: five sibling files across 31 diff
// headers in one issue's delta). roundDeltaBase advances the base to the
// worktree's fork point when the fork point descends from prev — and, since
// DKT-409, also when the two have diverged outright (a cherry-pick
// integration minted new shas for prev's work). prev survives only in a
// persisted worktree, whose fork point sits behind prev.

// TestRoundDeltaBaseSkipsInheritedIntegration is the defect's shape: round 1's
// head is integrated onto the shared branch, a sibling lands after it, and
// round 2's worktree forks from the result. The delta base must be the fork
// point — prev..HEAD would carry the sibling's commit.
func TestRoundDeltaBaseSkipsInheritedIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	execRoot := t.TempDir()
	gitRun(t, execRoot, "init", "-q")
	gitRun(t, execRoot, "commit", "--allow-empty", "-q", "-m", "base")

	// Round 1 in its own worktree; its head is `prev`.
	w1 := filepath.Join(t.TempDir(), "w1")
	gitRun(t, execRoot, "worktree", "add", "-q", w1)
	gitRun(t, w1, "commit", "--allow-empty", "-q", "-m", "round 1")
	prev := gitRun(t, w1, "rev-parse", "HEAD")

	// The conductor integrates round 1 as-is, then a SIBLING issue lands.
	gitRun(t, execRoot, "merge", "-q", "--ff-only", prev)
	gitRun(t, execRoot, "commit", "--allow-empty", "-q", "-m", "sibling work")
	sharedHead := gitRun(t, execRoot, "rev-parse", "HEAD")

	// Round 2 forks from the shared head and does its own work.
	w2 := filepath.Join(t.TempDir(), "w2")
	gitRun(t, execRoot, "worktree", "add", "-q", w2)
	gitRun(t, w2, "commit", "--allow-empty", "-q", "-m", "round 2")

	if got := roundDeltaBase(w2, execRoot, prev); got != sharedHead {
		t.Errorf("base = %.12s, want the fork point %.12s — prev..HEAD "+
			"attributes the sibling's commit to this round", got, sharedHead)
	}
}

// TestRoundDeltaBaseKeepsPrevInAPersistedWorktree: a worktree that survives
// across rounds has its fork point BEHIND prev; advancing to it would
// re-attribute the previous round's own work to this round.
func TestRoundDeltaBaseKeepsPrevInAPersistedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	execRoot := t.TempDir()
	gitRun(t, execRoot, "init", "-q")
	gitRun(t, execRoot, "commit", "--allow-empty", "-q", "-m", "base")

	w1 := filepath.Join(t.TempDir(), "w1")
	gitRun(t, execRoot, "worktree", "add", "-q", w1)
	gitRun(t, w1, "commit", "--allow-empty", "-q", "-m", "round 1")
	prev := gitRun(t, w1, "rev-parse", "HEAD")
	gitRun(t, w1, "commit", "--allow-empty", "-q", "-m", "round 2 in place")

	if got := roundDeltaBase(w1, execRoot, prev); got != prev {
		t.Errorf("base = %.12s, want prev %.12s — the fork point predates "+
			"round 1 and would fold its work into this round's delta", got, prev)
	}
}

// TestRoundDeltaBaseAdvancesWhenIntegrationDiverged is DKT-409's shape: the
// conductor integrated round 1 by CHERRY-PICK — new shas on the shared branch,
// so prev is on a line the branch no longer carries — and round 2's worktree
// forks from the result. DKT-171 kept prev here, and prev..HEAD then rendered
// sibling-issue commits and the integration's reshaping as this round's own
// work (RUN-35, three rounds of it). The base must advance to the fork point:
// the base actually shared with the current branch state, an ancestor of HEAD
// by construction.
func TestRoundDeltaBaseAdvancesWhenIntegrationDiverged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	execRoot := t.TempDir()
	gitRun(t, execRoot, "init", "-q")
	gitRun(t, execRoot, "commit", "--allow-empty", "-q", "-m", "base")

	w1 := filepath.Join(t.TempDir(), "w1")
	gitRun(t, execRoot, "worktree", "add", "-q", w1)
	gitRun(t, w1, "commit", "--allow-empty", "-q", "-m", "round 1")
	prev := gitRun(t, w1, "rev-parse", "HEAD")

	// The shared branch advances on commits that are NOT prev: round 1's work
	// re-minted by a cherry-pick, then a sibling issue's integration.
	gitRun(t, execRoot, "commit", "--allow-empty", "-q", "-m", "round 1, cherry-picked")
	gitRun(t, execRoot, "commit", "--allow-empty", "-q", "-m", "sibling work")
	sharedHead := gitRun(t, execRoot, "rev-parse", "HEAD")

	w2 := filepath.Join(t.TempDir(), "w2")
	gitRun(t, execRoot, "worktree", "add", "-q", w2)
	gitRun(t, w2, "commit", "--allow-empty", "-q", "-m", "round 2")

	if got := roundDeltaBase(w2, execRoot, prev); got != sharedHead {
		t.Errorf("base = %.12s, want the fork point %.12s — prev %.12s was "+
			"superseded by the cherry-pick, and prev..HEAD attributes the "+
			"sibling's commit to this round", got, sharedHead, prev)
	}
}

// TestRoundDeltaBaseKeepsPrevInAPersistedWorktreeAfterDivergedIntegration:
// the shape DKT-409's amendment must NOT disturb. The worktree persisted
// across rounds — its fork point predates round 1 — while the shared branch
// integrated round 1 as new commits. The fork point is BEHIND prev, not
// diverged from it, and advancing would re-attribute round 1's work to this
// round.
func TestRoundDeltaBaseKeepsPrevInAPersistedWorktreeAfterDivergedIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	execRoot := t.TempDir()
	gitRun(t, execRoot, "init", "-q")
	gitRun(t, execRoot, "commit", "--allow-empty", "-q", "-m", "base")

	w1 := filepath.Join(t.TempDir(), "w1")
	gitRun(t, execRoot, "worktree", "add", "-q", w1)
	gitRun(t, w1, "commit", "--allow-empty", "-q", "-m", "round 1")
	prev := gitRun(t, w1, "rev-parse", "HEAD")

	// The shared branch takes round 1 as a NEW commit, and round 2 continues
	// in the SAME worktree.
	gitRun(t, execRoot, "commit", "--allow-empty", "-q", "-m", "round 1, cherry-picked")
	gitRun(t, w1, "commit", "--allow-empty", "-q", "-m", "round 2 in place")

	if got := roundDeltaBase(w1, execRoot, prev); got != prev {
		t.Errorf("base = %.12s, want prev %.12s — the fork point predates "+
			"round 1 and would fold its work into this round's delta", got, prev)
	}
}

// TestRoundDeltaBaseSharedCheckoutKeepsPrev: with no distinct worktree there
// is no fork point, and the previous round's head remains the base.
func TestRoundDeltaBaseSharedCheckoutKeepsPrev(t *testing.T) {
	if got := roundDeltaBase("/same/root", "/same/root", "abc123"); got != "abc123" {
		t.Errorf("base = %q, want prev in the shared checkout", got)
	}
	if got := roundDeltaBase("", "/root", "abc123"); got != "abc123" {
		t.Errorf("base = %q, want prev with no dir", got)
	}
}

// ---------------------------------------------------------------------------
// DKT-409: the round-delta packet section after cherry-pick integration
// ---------------------------------------------------------------------------

// TestRoundDeltaPacketAfterCherryPickIntegration is DKT-409's acceptance
// shape, driven end to end through the real engine and real git: two issues
// in one run, both with fix loops; the conductor integrates round 0 of each
// via CHERRY-PICK, so the recorded executor heads are superseded by new shas
// on the shared branch; then round 1's re-review packet for issue A must
// carry a round-delta section with NO issue-B commits, computed from a base
// that is an ancestor of the shared branch's HEAD. Before the fix, the delta
// diffed from issue A's superseded round-0 sha, which rendered issue B's
// integrated work as issue A's own round change (RUN-35, VPL-389).
func TestRoundDeltaPacketAfterCherryPickIntegration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	shared := gitRepo(t)
	pinned := gitRun(t, shared, "rev-parse", "HEAD")

	conn := mustDB(t)
	registerFixture(t, conn)
	issueA := createIssue(t, conn, "issue A", "body", "task", nil)
	issueB := createIssue(t, conn, "issue B", "body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "cherry-pick run", 0, nowMS,
		db.RunContext{ExecRoot: shared, CommitSHA: pinned})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueA), "AddRunIssue(A): %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueB), "AddRunIssue(B): %v", err)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.DiffFn = GitDiff            // the real thing — production's wiring
	e.HeadFn = sharedCheckoutHead // ditto: the recorded head is the worktree's

	// ROUND 0: each issue works in its own worktree, forked at the run's pin.
	wA := filepath.Join(t.TempDir(), "issue-a-round-0")
	gitRun(t, shared, "worktree", "add", "-q", wA)
	writeFile(t, wA, "a/feature.txt", "ISSUE A ROUND 0\n")
	gitRun(t, wA, "add", "-A")
	gitRun(t, wA, "commit", "-qm", "issue A round 0")

	wB := filepath.Join(t.TempDir(), "issue-b-round-0")
	gitRun(t, shared, "worktree", "add", "-q", wB)
	writeFile(t, wB, "b/feature.txt", "ISSUE B ROUND 0\n")
	gitRun(t, wB, "add", "-A")
	gitRun(t, wB, "commit", "-qm", "issue B round 0")

	driveIssueToReentryAt(t, conn, e, issueA, wA)
	driveIssueToReentryAt(t, conn, e, issueB, wB)

	// The conductor integrates round 0 of EACH issue via cherry-pick: new
	// shas on the shared branch, and both recorded heads become non-ancestors
	// of its HEAD.
	gitRun(t, shared, "cherry-pick", gitRun(t, wA, "rev-parse", "HEAD"))
	gitRun(t, shared, "cherry-pick", gitRun(t, wB, "rev-parse", "HEAD"))

	// ROUND 1 for issue A: a FRESH worktree forked from the integrated head,
	// exactly as a conductor creates one after sweeping round 0's.
	wA1 := filepath.Join(t.TempDir(), "issue-a-round-1")
	gitRun(t, shared, "worktree", "add", "-q", wA1)
	writeFile(t, wA1, "a/feature.txt", "ISSUE A ROUND 1 FIX\n")
	gitRun(t, wA1, "add", "-A")
	gitRun(t, wA1, "commit", "-qm", "issue A round 1 fix")
	completeStepAt(t, conn, e, issueA, "fix@1", wA1)

	// Round 1's review packet for issue A, rendered as `step render` renders
	// it — the surface RUN-35's judges actually read.
	rendered, err := RenderStep(conn, stepIDIn(t, conn, issueA, "review@1#0"), "", nowMS)
	testsupport.Must(t, err, "RenderStep(review@1#0): %v", err)

	_, delta, found := strings.Cut(rendered.Packet, "round delta: changes since")
	if !found {
		t.Fatalf("review@1#0's packet carries no round-delta section:\n%s",
			rendered.Packet)
	}
	if strings.Contains(delta, "ISSUE B ROUND 0") {
		t.Errorf("issue A's round delta carries issue B's commit — judges "+
			"invited to review out-of-scope work:\n%s", delta)
	}
	if !strings.Contains(delta, "ISSUE A ROUND 1 FIX") {
		t.Errorf("issue A's own round-1 fix is missing from its round delta:\n%s",
			delta)
	}

	// And the recorded base is an ancestor of the shared branch's HEAD — not
	// the superseded executor sha the cherry-pick left behind.
	base := issueRoundBase(t, conn, run.ID, issueA)
	if !isAncestor(shared, base, "HEAD") {
		t.Errorf("round_base %.12s is not an ancestor of the shared branch's "+
			"HEAD — the delta was computed against a superseded base", base)
	}
}

// driveIssueToReentryAt drives one issue of an activated fixture run to its
// loop re-entry — implement recorded from `workDir`, the four judges,
// synthesize, reconcile, and a verify completed against the unmet threshold —
// leaving fix@1 minted and the review@1 judges restored.
func driveIssueToReentryAt(
	t *testing.T, conn *sql.DB, e *Engine, issueID int, workDir string,
) {
	t.Helper()
	completeStepAt(t, conn, e, issueID, "implement@0", workDir)
	for i := range 4 {
		completeInIssue(t, conn, e, issueID,
			fmt.Sprintf("review@0#%d", i), "findings", "")
	}
	completeInIssue(t, conn, e, issueID, "synthesize@0", "the synthesis", "")
	driveActionInIssue(t, conn, e, issueID, "reconcile@0")
	completeInIssue(t, conn, e, issueID, "verify@0", "the ac report", unmetPayload)
}

// completeStepAt claims and completes one issue's step with a recorded
// worktree, as `step record --worktree` does.
func completeStepAt(
	t *testing.T, conn *sql.DB, e *Engine, issueID int, instance, workDir string,
) {
	t.Helper()
	stepID := stepIDIn(t, conn, issueID, instance)
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim %s: %v", instance, err)
	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("the change summary"),
		WorkDir: workDir, NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete %s: %v", instance, err)
}

// issueRoundBase reads the `round_base` the issue's newest issue.diff payload
// recorded.
func issueRoundBase(t *testing.T, conn *sql.DB, runID, issueID int) string {
	t.Helper()
	var payload string
	err := conn.QueryRow(
		`SELECT a.payload FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ?
		  ORDER BY a.id DESC LIMIT 1`,
		runID, issueID, ArtifactKindIssueDiff).Scan(&payload)
	testsupport.Must(t, err, "reading the newest issue.diff payload: %v", err)
	var record struct {
		RoundBase string `json:"round_base"`
	}
	err = json.Unmarshal([]byte(payload), &record)
	testsupport.Must(t, err, "unmarshal %q: %v", payload, err)
	if record.RoundBase == "" {
		t.Fatalf("no round_base recorded in %q", payload)
	}
	return record.RoundBase
}
