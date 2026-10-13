package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1056 — the step VIEW answers the target question the context bundle
// already answers.
//
// A wave seats a vote panel from `step show`, and probes the (up to 1MiB)
// bundle only when that read names a target. The bundle carried
// `target_sha`/`target_worktree` from DKT-24 onward and the view carried
// neither, so every panel read its own HEAD instead of the judged tree and the
// wave logged "NO target ref on the bundle". These tests pin the view to the
// bundle: same pair, same resolution, no invention.

// TestStepViewCarriesTheResolvedTarget: a step consuming `issue.diff` reports
// the round record's head and worktree, and reports EXACTLY what the bundle
// reports — the two are resolved by one function and must not drift.
func TestStepViewCarriesTheResolvedTarget(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	e.HeadFn = func(string) string { return "cafe1234cafe1234" }

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

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	view, err := LoadStepView(conn, reviewID, nowMS)
	testsupport.Must(t, err, "LoadStepView(review@0#0): %v", err)

	if view.TargetSHA != "cafe1234cafe1234" {
		t.Errorf("view target_sha = %q, want the recorded head", view.TargetSHA)
	}
	if view.TargetWorktree != "/worktrees/issue-under-test" {
		t.Errorf("view target_worktree = %q, want the declared worktree",
			view.TargetWorktree)
	}

	bundle, err := ReadContext(conn, reviewID, nowMS)
	testsupport.Must(t, err, "ReadContext(review@0#0): %v", err)
	if view.TargetSHA != bundle.TargetSHA || view.TargetWorktree != bundle.TargetWorktree {
		t.Errorf("view target (%q, %q) != bundle target (%q, %q); the cheap read "+
			"exists to save the bundle probe, so a disagreement makes it useless",
			view.TargetSHA, view.TargetWorktree,
			bundle.TargetSHA, bundle.TargetWorktree)
	}
}

// TestStepViewReportsNoTargetForAStepThatConsumesNoDiff: `implement` declares
// no inputs at all, so there is nothing under review yet. Both halves stay
// empty — the shared checkout's HEAD is NOT a stand-in.
func TestStepViewReportsNoTargetForAStepThatConsumesNoDiff(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	view, err := LoadStepView(conn, stepIDByInstance(t, conn, "implement@0"), nowMS)
	testsupport.Must(t, err, "LoadStepView(implement@0): %v", err)
	if view.TargetSHA != "" || view.TargetWorktree != "" {
		t.Errorf("implement@0 reports target (%q, %q); a step consuming no "+
			"issue.diff has no tree under review to report",
			view.TargetSHA, view.TargetWorktree)
	}
}

// TestStepViewFabricatesNoTargetWithoutARoundRecord is the failure mode the
// issue names: a diff recorded with no resolvable head and no declared
// worktree must yield the EMPTY pair. One fabricated sha in 22 recorded probes
// is why the wave stopped trusting this read at all.
func TestStepViewFabricatesNoTargetWithoutARoundRecord(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	e.HeadFn = func(string) string { return "" }

	implementID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, implementID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement: %v", err)
	err = e.CompleteStep(conn, implementID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete implement: %v", err)

	reviewID := stepIDByInstance(t, conn, "review@0#0")
	view, err := LoadStepView(conn, reviewID, nowMS)
	testsupport.Must(t, err, "LoadStepView(review@0#0): %v", err)
	if view.TargetSHA != "" || view.TargetWorktree != "" {
		t.Errorf("view target = (%q, %q), want both empty: the producer recorded "+
			"no round record and a plausible-looking value here seats a panel "+
			"on a tree nobody judged", view.TargetSHA, view.TargetWorktree)
	}

	bundle, err := ReadContext(conn, reviewID, nowMS)
	testsupport.Must(t, err, "ReadContext(review@0#0): %v", err)
	if bundle.TargetSHA != "" || bundle.TargetWorktree != "" {
		t.Fatalf("premise: the bundle itself reports a target (%q, %q)",
			bundle.TargetSHA, bundle.TargetWorktree)
	}
}
