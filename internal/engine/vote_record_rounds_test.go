package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The `<step>.vote-record` form was not checked for the stale-round gap
// DKT-3131 closed for `<step>.gate-results`. That gap needs a named producer
// the loop does not re-run plus a loop body that stands in for it; neither
// exists for a vote record. Vote steps re-instantiate at every ordinal, and a
// vote step emits no artifact kind, so no loop body can stand in for one.
// Every corpus consumer of `<vote>.vote-record` is itself a loop body, whose
// inputs bind by the ordinary ordinal rule. This test pins that rule across
// rounds: the body at round 2 reads round 1's panel, never round 0's.

// fixVoteRecord claims one fix instance and returns the producer of the
// vote-record input its context carries, or "" when it carries none.
func fixVoteRecord(t *testing.T, conn *sql.DB, e *Engine, instance string) string {
	t.Helper()
	claim, err := ClaimStep(conn, stepIDByInstance(t, conn, instance),
		ClaimOptions{Owner: "fixer", NowMS: nowMS})
	testsupport.Must(t, err, "claim %s: %v", instance, err)

	producer := ""
	for _, input := range claim.Context.Inputs {
		if input.Kind == workflow.VoteRecordKind {
			if producer != "" {
				t.Fatalf("%s carries more than one vote-record input", instance)
			}
			producer = input.ProducerStep
		}
	}

	err = e.CompleteStep(conn, stepIDByInstance(t, conn, instance), CompleteOptions{
		Token: claim.Token, Artifact: []byte("the fix"), NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete %s: %v", instance, err)
	return producer
}

// concernRound casts the two concerned approvals that route concernLoopSrc's
// gate into another fix round, and drives the tally.
func concernRound(t *testing.T, conn *sql.DB, e *Engine, instance string) {
	t.Helper()
	gate, err := db.GetStep(conn, stepIDByInstance(t, conn, instance))
	testsupport.Must(t, err, "reading %s: %v", instance, err)
	proposalID, err := findVoteProposal(conn, gate)
	testsupport.Must(t, err, "finding %s's proposal: %v", instance, err)
	if proposalID == 0 {
		t.Fatalf("no proposal opened for %s", instance)
	}
	castSeat(t, conn, proposalID, "seat-a", model.VerdictApproveWithConcerns, instance+": auth check is thin")
	castSeat(t, conn, proposalID, "seat-b", model.VerdictApproveWithConcerns, instance+": no rollback path")
	castSeat(t, conn, proposalID, "seat-c", model.VerdictApprove, "")
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving %s: %v", instance, err)
}

func TestLoopBodyReadsTheLatestRoundsVoteRecord(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(concernLoopSrc), "concern-loop.toml")
	issue := createIssue(t, conn, "two rounds", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	// Each round records an in-scope change, so the engine's non-convergence
	// refusal lets the loop enter round 2.
	driveFixtureRound(t, 0)
	openGateProposal(t, conn, e, run.ID)
	concernRound(t, conn, e, "gate@0")
	driveFixtureRound(t, 1)
	if got := fixVoteRecord(t, conn, e, "fix@1"); got != "gate@0" {
		t.Fatalf("fix@1 read vote-record from %q, want gate@0", got)
	}

	err = e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after fix@1: %v", err)
	concernRound(t, conn, e, "gate@1")
	driveFixtureRound(t, 2)
	if got := fixVoteRecord(t, conn, e, "fix@2"); got != "gate@1" {
		t.Errorf("fix@2 read vote-record from %q, want gate@1: a loop body "+
			"at round 2 must read the panel that routed round 2, not round 0's", got)
	}
}

// A skipped vote step at the latest ordinal leaves the consumer with no
// vote-record input. Falling back would hand `fix@2` round 0's panel
// objections, which round 1 already settled. `gate@1` opens and tallies its
// proposal before it moves to `skipped`, as `resolve --as skip` leaves a
// parked vote step, so the proposal it carries is not read either.
func TestSkippedLatestVoteStepYieldsNoVoteRecord(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(concernLoopSrc), "concern-loop.toml")
	issue := createIssue(t, conn, "two rounds", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	driveFixtureRound(t, 0)
	openGateProposal(t, conn, e, run.ID)
	concernRound(t, conn, e, "gate@0")
	driveFixtureRound(t, 1)
	fixVoteRecord(t, conn, e, "fix@1")
	err = e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after fix@1: %v", err)
	concernRound(t, conn, e, "gate@1")
	driveFixtureRound(t, 2)
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = 'gate@1'`,
		db.StepSkipped)

	if got := fixVoteRecord(t, conn, e, "fix@2"); got != "" {
		t.Errorf("fix@2 read vote-record from %q, want none: a skipped gate@1 "+
			"pins ordinal 1 and must not fall back to gate@0's panel", got)
	}
}
