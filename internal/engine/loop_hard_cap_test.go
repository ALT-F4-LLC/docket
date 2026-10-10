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

// hardCapSrc is loopTriageSrc with the issue-level bound declared on `review`:
// `max_fix_loops = 1` spends the soft cap on review@0's own fix-loop, so every
// later round is one the triage panel behind `fix` mints. BOUNDS is replaced
// with the case's optional `max_fix_loops_hard` line.
const hardCapSrc = `
[pipeline]
name = "hard-cap-lane"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
after = []
executor = "x"
emits = "findings"
gates = ["build"]
on_fail = "triage"

[[step]]
name = "triage"
after = ["implement", "fix"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
on_fail = "waiting-human"

[step.on_fail_routes]
approved = "fix-round"
rejected = "abandon-issue"

[[step]]
name = "review"
after = ["implement"]
executor = "y"
emits = "verdict"
gates = ["verdict"]
on_fail = "fix-loop"
max_fix_loops = 1
BOUNDS
[[step]]
name = "fix"
executor = "x"
emits = "findings"
gates = ["build"]
on_fail = "triage"
loop = true
after_loop = "review"
`

// TestFixLoopExtensionHardCap: a triage panel's `fix-round` approval is the
// extension vote, and each one mints exactly one round. A declared
// `max_fix_loops_hard` stops the panel minting past it — the step parks for an
// operator instead — while an undeclared one leaves extension unbounded, and an
// operator's own `fix-round` at the cap is still admitted.
func TestFixLoopExtensionHardCap(t *testing.T) {
	t.Run("approvals mint one round each until the hard cap, then park", func(t *testing.T) {
		conn, _, e := hardCapLane(t, "max_fix_loops_hard = 3")

		// Ordinals 2 and 3 are the two rounds the cap leaves the panel.
		for ordinal := 1; ordinal <= 2; ordinal++ {
			approveFailedRound(t, conn, e, ordinal)
			next := fmt.Sprintf("fix@%d", ordinal+1)
			if !stepExists(t, conn, next) {
				t.Fatalf("approval at ordinal %d minted no %s; the hard cap "+
					"of 3 still had room", ordinal, next)
			}
			if got := stepStatus(t, conn, fmt.Sprintf("fix@%d", ordinal)); got != db.StepSuperseded {
				t.Errorf("fix@%d = %q after its round was approved, want %q",
					ordinal, got, db.StepSuperseded)
			}
		}

		approveFailedRound(t, conn, e, 3)
		if stepExists(t, conn, "fix@4") {
			t.Fatal("fix@4 exists: the panel minted a round past max_fix_loops_hard = 3")
		}
		step := mustStep(t, conn, "fix@3")
		if step.Status != db.StepWaitingHuman {
			t.Errorf("fix@3 = %q at the hard cap, want %q", step.Status, db.StepWaitingHuman)
		}
		if step.ParkClass != db.ParkClassLoopBound {
			t.Errorf("fix@3 park class = %q, want %q", step.ParkClass, db.ParkClassLoopBound)
		}
		if !strings.Contains(step.ParkReason, "max_fix_loops_hard") {
			t.Errorf("fix@3's park reason does not name the hard cap: %q", step.ParkReason)
		}
	})

	t.Run("with no hard cap declared, approvals keep minting rounds", func(t *testing.T) {
		conn, _, e := hardCapLane(t, "")

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
		conn, _, e := hardCapLane(t, "max_fix_loops_hard = 2")

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

// hardCapLane activates hardCapSrc with bounds declared on `review`, passes
// implement@0, and fails review@0 into the soft cap's one round, returning with
// fix@1 instantiated. The engine it returns fails every gate.
func hardCapLane(t *testing.T, bounds string) (*sql.DB, *model.Run, *Engine) {
	t.Helper()
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	src := strings.Replace(hardCapSrc, "BOUNDS", bounds, 1)
	registerSource(t, conn, []byte(src), "hard-cap-lane.toml")

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
	return conn, run, e
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
