package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestFixLoopExtensionHardCap: a triage panel's `fix-round` approval is the
// extension vote, and each one mints exactly one round. A declared
// `max_fix_loops_hard` stops the panel minting past it — the step parks for an
// operator instead — while an undeclared one leaves extension unbounded, and an
// operator's own `fix-round` at the cap is still admitted.
func TestFixLoopExtensionHardCap(t *testing.T) {
	t.Run("approvals mint one round each until the hard cap, then park", func(t *testing.T) {
		conn, run, issue, e := hardCapLane(t, 1, 3)

		// Ordinals 2 and 3 are the two rounds the cap leaves the panel.
		for ordinal := 1; ordinal <= 2; ordinal++ {
			approveFailedRound(t, conn, e, ordinal)
			next := fmt.Sprintf("fix@%d", ordinal+1)
			if !stepExists(t, conn, next) {
				t.Fatalf("approval at ordinal %d minted no %s; the hard cap "+
					"of 3 still had room", ordinal, next)
			}
			if beyond := fmt.Sprintf("fix@%d", ordinal+2); stepExists(t, conn, beyond) {
				t.Fatalf("approval at ordinal %d minted %s as well; one approval "+
					"mints exactly one round", ordinal, beyond)
			}
			if got := stepStatus(t, conn, fmt.Sprintf("fix@%d", ordinal)); got != db.StepSuperseded {
				t.Errorf("fix@%d = %q after its round was approved, want %q",
					ordinal, got, db.StepSuperseded)
			}
		}

		grantsBefore := loopGrants(t, conn, run.ID, issue)
		approveFailedRound(t, conn, e, 3)
		if stepExists(t, conn, "fix@4") {
			t.Fatal("fix@4 exists: the panel minted a round past max_fix_loops_hard = 3")
		}
		// A refused extension records nothing: a grant here would raise the
		// automatic loop's allowance for a round no one minted.
		if got := loopGrants(t, conn, run.ID, issue); got != grantsBefore {
			t.Errorf("loop_grants = %d after the refused approval, want it unchanged at %d",
				got, grantsBefore)
		}
		if got := loopCount(t, conn, run.ID, issue); got != 3 {
			t.Errorf("loop_count = %d after the refused approval, want it unchanged at 3", got)
		}
		assertHardCapPark(t, conn, "fix@3")
		if got := runStatusOf(t, conn, run.ID); got != string(model.RunWaitingHuman) {
			t.Errorf("run status = %q with fix@3 parked at the hard cap, want %q",
				got, model.RunWaitingHuman)
		}
	})

	t.Run("a hard cap equal to max_fix_loops admits no extension", func(t *testing.T) {
		conn, _, _, e := hardCapLane(t, 1, 1)

		approveFailedRound(t, conn, e, 1)
		if stepExists(t, conn, "fix@2") {
			t.Fatal("fix@2 exists: max_fix_loops_hard = max_fix_loops = 1 " +
				"declares no extensions, yet the panel minted one")
		}
		assertHardCapPark(t, conn, "fix@1")
	})

	t.Run("a panel round inside the soft cap does not let the automatic loop pass the hard cap", func(t *testing.T) {
		conn, run, issue, e := hardCapLane(t, 2, 2)

		// Round 2 is within max_fix_loops = 2, so this approval extends nothing.
		approveFailedRound(t, conn, e, 1)
		if !stepExists(t, conn, "fix@2") {
			t.Fatal("premise: the approval at ordinal 1 minted no fix@2")
		}

		// fix@2 passes, and review@2's failure asks the automatic fix-loop for
		// round 3: past both caps, with no vote or operator approving it.
		driveFixtureRound(t, 2)
		e.Gates = PassThroughRunner{}
		claimAndComplete(t, conn, e, "fix@2", "a candidate that passes its gates", "")
		testsupport.Must(t, e.DriveRunLifecycles(conn, run.ID, nowMS),
			"driving after fix@2 passed")
		e.Gates = failingGates{}
		claimAndComplete(t, conn, e, "review@2", "the verdict at ordinal 2", "")
		testsupport.Must(t, e.DriveRunLifecycles(conn, run.ID, nowMS),
			"driving after review@2's failure")

		if stepExists(t, conn, "fix@3") {
			t.Fatal("fix@3 exists: the automatic fix-loop entered round 3 " +
				"past max_fix_loops_hard = 2")
		}
		if got := loopCount(t, conn, run.ID, issue); got != 2 {
			t.Errorf("loop_count = %d after the refused entry, want 2", got)
		}
		step := mustStep(t, conn, "review@2")
		if step.Status != db.StepWaitingHuman {
			t.Errorf("review@2 = %q past the hard cap, want %q",
				step.Status, db.StepWaitingHuman)
		}
		if !strings.Contains(step.ParkReason, "max_fix_loops_hard = 2") {
			t.Errorf("review@2's park reason does not name the hard cap: %q", step.ParkReason)
		}
	})

	t.Run("with no hard cap declared, approvals keep minting rounds", func(t *testing.T) {
		conn, _, _, e := hardCapLane(t, 1, 0)

		// Past ordinal 3, where the capped case parks.
		for ordinal := 1; ordinal <= 4; ordinal++ {
			approveFailedRound(t, conn, e, ordinal)
			if next := fmt.Sprintf("fix@%d", ordinal+1); !stepExists(t, conn, next) {
				t.Fatalf("approval at ordinal %d minted no %s; without "+
					"max_fix_loops_hard extension is unbounded", ordinal, next)
			}
		}
		if got := stepStatus(t, conn, "fix@4"); got == db.StepWaitingHuman {
			t.Errorf("fix@4 parked %q with no hard cap declared", got)
		}
	})

	t.Run("an operator's fix-round at the hard cap is admitted", func(t *testing.T) {
		conn, _, _, e := hardCapLane(t, 1, 2)

		approveFailedRound(t, conn, e, 1)
		approveFailedRound(t, conn, e, 2)
		if got := stepStatus(t, conn, "fix@2"); got != db.StepWaitingHuman {
			t.Fatalf("premise: fix@2 = %q, want the hard-cap park %q",
				got, db.StepWaitingHuman)
		}

		err := e.ResolveStep(conn, stepIDByInstance(t, conn, "fix@2"),
			ResolveFixRound, "one more, by hand", nowMS)
		testsupport.Must(t, err, "resolving fix@2 --as fix-round: %v", err)
		if !stepExists(t, conn, "fix@3") {
			t.Fatal("the operator's fix-round minted no fix@3: the hard cap " +
				"bounds vote-minted rounds only")
		}
		if got := stepStatus(t, conn, "fix@2"); got != db.StepSuperseded {
			t.Errorf("fix@2 = %q after the operator's round, want %q",
				got, db.StepSuperseded)
		}
	})
}

// hardCapLane activates loopTriageSrc with `max_fix_loops = soft` and, when
// hard > 0, `max_fix_loops_hard = hard` declared on `review`, passes
// implement@0, and fails review@0 into ordinal 1, returning with fix@1
// instantiated. Every later round is one the triage panel behind `fix` mints
// or the automatic loop enters. The engine it returns fails every gate.
func hardCapLane(t *testing.T, soft, hard int) (*sql.DB, *model.Run, int, *Engine) {
	t.Helper()
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	bounds := fmt.Sprintf("max_fix_loops = %d\n", soft)
	if hard > 0 {
		bounds += fmt.Sprintf("max_fix_loops_hard = %d\n", hard)
	}
	const reviewRouting = "on_fail = \"fix-loop\"\n"
	src := strings.Replace(loopTriageSrc, reviewRouting, reviewRouting+bounds, 1)
	registerSource(t, conn, []byte(src), "loop-triage-lane.toml")

	issue := createIssue(t, conn, "hard-capped", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	claimAndComplete(t, conn, e, "implement@0", "the candidate", "")
	e.Gates = failingGates{}
	claimAndComplete(t, conn, e, "review@0", "the verdict", "")
	testsupport.Must(t, e.DriveRunLifecycles(conn, run.ID, nowMS),
		"driving after review@0's failure: %v", err)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: review@0's fix-loop did not enter ordinal 1")
	}
	return conn, run, issue, e
}

// assertHardCapPark checks that a triaged step the panel approved at the hard
// cap parked for an operator with the loop-bound class and a reason naming
// the cap.
func assertHardCapPark(t *testing.T, conn *sql.DB, instance string) {
	t.Helper()
	step := mustStep(t, conn, instance)
	if step.Status != db.StepWaitingHuman {
		t.Errorf("%s = %q at the hard cap, want %q", instance, step.Status, db.StepWaitingHuman)
	}
	if step.ParkClass != db.ParkClassLoopBound {
		t.Errorf("%s park class = %q, want %q", instance, step.ParkClass, db.ParkClassLoopBound)
	}
	if !strings.Contains(step.ParkReason, "max_fix_loops_hard") {
		t.Errorf("%s's park reason does not name the hard cap: %q", instance, step.ParkReason)
	}
}

// approveFailedRound fails fix@ordinal's gates, which suspends it for
// triage@ordinal, and has both seats approve the panel's `fix-round`.
func approveFailedRound(t *testing.T, conn *sql.DB, e *Engine, ordinal int) {
	t.Helper()
	fix := fmt.Sprintf("fix@%d", ordinal)
	claimAndComplete(t, conn, e, fix, "a candidate that fails its gates", "")
	step := mustStep(t, conn, fix)
	testsupport.Must(t, e.DriveRunLifecycles(conn, step.RunID, nowMS),
		"driving after %s's failure", fix)
	if step = mustStep(t, conn, fix); step.Status != db.StepGated ||
		!routingIs(step.Routing, "triage") {
		t.Fatalf("premise: %s = %q routing %q, want %q suspended for the panel",
			fix, step.Status, step.Routing, db.StepGated)
	}

	panel := mustStep(t, conn, fmt.Sprintf("triage@%d", ordinal))
	proposalID, err := findVoteProposal(conn, panel)
	testsupport.Must(t, err, "finding %s's proposal: %v", panel.Instance, err)
	if proposalID == 0 {
		t.Fatalf("no proposal opened for %s", panel.Instance)
	}
	for _, seat := range []string{"seat-a", "seat-b"} {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID: proposalID, VoterName: seat,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}
	testsupport.Must(t, e.DriveVoteProposal(conn, proposalID, nowMS),
		"driving %s's tally", panel.Instance)
	if got := stepStatus(t, conn, panel.Instance); got != db.StepDone {
		t.Fatalf("%s = %q after its tally, want %q: the panel ruled",
			panel.Instance, got, db.StepDone)
	}
}
