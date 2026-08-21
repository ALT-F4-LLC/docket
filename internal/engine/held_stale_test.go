package engine

import (
	"database/sql"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-414: resolving a held reconcile while the shared branch HEAD has moved
// off the recorded target let verify and both panels consume packets rendered
// from a tree the branch no longer carried (RUN-26/FLX-141). The ancestry
// check existed — `dispatch open` ran it — but one phase too late: under the
// staged closure the downstream rows were already in an open dispatch, so no
// open ran between the resolution and their execution. The resolution verbs
// now ask the SAME question at resolve time, through the same primitives
// (staleTargetCandidates + staleTargets), which this file pins by comparing
// the resolve-time answer against `dispatch open`'s own, field for field.
//
// REAL git throughout: the repo, the recorded target commit, and the
// divergence are all made with git itself, so the test exercises
// sharedCheckoutHead and gitAncestorOfHead rather than stubs.

// heldDivergenceFixture drives the fixture workflow to a resolvable hold over
// a real checkout: a repo whose HEAD is the recorded target, implement@0
// completed IN that checkout (so the round record names its HEAD), the review
// fanout and synthesize done with a payload whose first cluster trips
// hold_spread, and reconcile@0 held on it.
//
// Returned: the repo path and the recorded target sha. The shared branch
// still carries the target when this returns — divergence is each test's own
// move.
func heldDivergenceFixture(t *testing.T, conn *sql.DB, e *Engine, runID int) (repo, target string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	// THE COMMITS CARRY REAL CONTENT (DKT-451). They were empty commits while
	// ancestry was the whole test; now that a disproved ancestry opens the tree
	// question, empty commits all share the empty tree — every "divergence"
	// built from them is content-identical, and the advisory correctly says
	// nothing about it. Divergence has to be a difference in a file to be the
	// thing RUN-26 actually hit.
	repo = t.TempDir()
	gitRun(t, repo, "init", "-q")
	writeFile(t, repo, "internal/work.txt", "original\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	writeFile(t, repo, "internal/work.txt", "the recorded target's content\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "the recorded target")
	target = gitRun(t, repo, "rev-parse", "HEAD")

	// The run's exec root is the checkout runExecRoot resolves — captured at
	// `run start` in production, planted directly here as pregate_test.go and
	// activate_test.go already do.
	execSQL(t, conn, `UPDATE runs SET exec_root = ? WHERE id = ?`, repo, runID)

	// implement@0 completes IN the checkout, so its round record's head is the
	// repo's real HEAD — the staleFixture shape, with the real HeadFn instead
	// of a canned sha.
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

	for _, i := range []string{"0", "1", "2", "3"} {
		claimAndComplete(t, conn, e, "review@0#"+i, "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", clusteredPayload)
	driveAction(t, conn, e, "reconcile@0")

	return repo, target
}

// TestHeldResolveFlagsDivergedTarget is the acceptance case: the shared
// branch moves off the recorded target (the cherry-pick / re-integration
// shape), the operator approves the held cluster, and the resolve-time
// advisory names the divergence — the same rows, same shas, same reason the
// next `dispatch open` emits, proven by deep-comparing the two.
func TestHeldResolveFlagsDivergedTarget(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	repo, target := heldDivergenceFixture(t, conn, e, run.ID)

	// Integration diverges IN CONTENT: the branch is rebuilt from base with a
	// commit the recorded target is not part of, writing something else to the
	// very file the work touched. So the target is provably not an ancestor of
	// HEAD *and* the branch no longer carries its tree — the RUN-26 shape, and
	// the only shape that is still a warning after DKT-451.
	gitRun(t, repo, "checkout", "-q", "-b", "integration", "HEAD~1")
	writeFile(t, repo, "internal/work.txt", "integration rewrote this\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "re-integrated differently")
	sharedHead := gitRun(t, repo, "rev-parse", "HEAD")

	heldID := stepIDByInstance(t, conn, "reconcile-held@0#0")

	// BEFORE the hold resolves, nothing downstream is ready and the advisory
	// says nothing — it fires when packets become consumable, not earlier.
	if got := e.HeldResolutionStaleTargets(conn, heldID, nowMS); got != nil {
		t.Fatalf("advisory before the resolution = %+v, want nothing: the hold "+
			"still blocks every downstream packet", got)
	}

	err := e.DecideStep(conn, heldID, true, "accepted as computed", nowMS)
	testsupport.Must(t, err, "approving the held cluster: %v", err)

	stale := e.HeldResolutionStaleTargets(conn, heldID, nowMS)
	if len(stale) != 1 {
		t.Fatalf("stale targets = %+v, want exactly the un-blocked verify@0", stale)
	}
	s := stale[0]
	if s.Instance != "verify@0" {
		t.Errorf("instance = %q, want verify@0 — the step whose packet renders "+
			"from the recorded target", s.Instance)
	}
	if s.TargetSHA != target {
		t.Errorf("target_sha = %q, want the recorded head %q", s.TargetSHA, target)
	}
	if s.SharedHead != sharedHead {
		t.Errorf("shared_head = %q, want the checkout's real HEAD %q",
			s.SharedHead, sharedHead)
	}
	if !strings.Contains(s.Reason, "not an ancestor") {
		t.Errorf("reason %q does not name the ancestry fact", s.Reason)
	}
	// DKT-415: sharing staleTargets means sharing its claim-time sentence —
	// the resolve-time reader faces the same "is it safe to let these run"
	// decision the manifest reader does.
	if !strings.Contains(s.Reason, "does not re-derive the target from HEAD") {
		t.Errorf("reason %q does not name the claim-time semantics", s.Reason)
	}

	// THE SAME FACT `dispatch open` WOULD NAME — not a lookalike. The next
	// open's stale_targets must be field-for-field what the resolution already
	// warned, or the two surfaces have forked the primitive.
	m, err := e.OpenDispatch(conn, run.ID, 0, nil, nowMS)
	testsupport.Must(t, err, "dispatch open: %v", err)
	if !reflect.DeepEqual(m.StaleTargets, stale) {
		t.Errorf("dispatch open names %+v,\nthe resolution named %+v;\n"+
			"the two surfaces must share one answer", m.StaleTargets, stale)
	}

	// A declared (non-materialized) step stays quiet even with the divergence
	// standing: the advisory belongs to held resolutions alone.
	implementID := stepIDByInstance(t, conn, "implement@0")
	if got := e.HeldResolutionStaleTargets(conn, implementID, nowMS); got != nil {
		t.Errorf("a declared step produced the held-resolution advisory: %+v", got)
	}
}

// TestHeldResolveStaysQuietWhenTargetCarried is the complement: the branch
// never moved, the recorded target IS the shared HEAD, and the resolution
// warns about nothing.
func TestHeldResolveStaysQuietWhenTargetCarried(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	heldDivergenceFixture(t, conn, e, run.ID)

	heldID := stepIDByInstance(t, conn, "reconcile-held@0#0")
	err := e.DecideStep(conn, heldID, true, "accepted", nowMS)
	testsupport.Must(t, err, "approving the held cluster: %v", err)

	if got := e.HeldResolutionStaleTargets(conn, heldID, nowMS); len(got) != 0 {
		t.Errorf("an integrated (ancestor) target was flagged at resolve time: %+v", got)
	}
}
