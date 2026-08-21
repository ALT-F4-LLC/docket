package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-262 — terminal transitions close what they orphan.
//
// Four proposals of the current epoch stood open forever because the thing they
// were attached to ended without closing them. An open proposal is NOT inert:
// `vote list` shows it as outstanding work and, since DKT-236, it is what a
// spawn-guard carve-out points at — so a stale entry makes two surfaces lie,
// one of them a guard. DKT-114 added the close verb; nothing called it
// automatically, which is what left them standing.

// openBallot registers an OPEN proposal under `key`, the way both an engine
// vote step and a conductor's ballot are registered.
func openBallot(t *testing.T, conn *sql.DB, key, description string) int {
	t.Helper()
	id, err := db.CreateProposalIdempotent(conn, &model.Proposal{
		Description: description,
		// Status is set EXPLICITLY. CreateProposal inserts what it is given
		// rather than leaning on the column default, so a zero-valued Status
		// stores the empty string — which is not `open` and therefore not
		// closeable, and the resulting failure looks like the close is broken.
		Status:         model.ProposalStatusOpen,
		Criticality:    model.CriticalityMedium,
		RequiredVoters: 3,
		Threshold:      0.67,
	}, key)
	testsupport.Must(t, err, "creating the ballot: %v", err)
	return id
}

func proposalStatus(t *testing.T, conn *sql.DB, id int) model.ProposalStatus {
	t.Helper()
	var status string
	err := conn.QueryRow(`SELECT status FROM proposals WHERE id = ?`, id).Scan(&status)
	testsupport.Must(t, err, "reading proposal %d: %v", id, err)
	return model.ProposalStatus(status)
}

func proposalOutcome(t *testing.T, conn *sql.DB, id int) string {
	t.Helper()
	var outcome string
	err := conn.QueryRow(
		`SELECT COALESCE(final_outcome, '') FROM proposals WHERE id = ?`, id).Scan(&outcome)
	testsupport.Must(t, err, "reading proposal %d: %v", id, err)
	return outcome
}

// seedReapFor inserts an UNACKNOWLEDGED reap-ack row for a run and returns the
// seq an acknowledgment names.
//
// The row is written directly rather than driven through a real reap, because
// what these cases are about is the ACK's consequence for a ballot, not the
// reap's own machinery — which reap_ack.go's own tests already cover. Driving a
// real reap would couple every case here to write-class configuration that has
// nothing to do with the question.
func seedReapFor(t *testing.T, conn *sql.DB, runID int) int64 {
	t.Helper()
	stepID := stepIDByInstance(t, conn, "implement@0")
	const seq = int64(9001)
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = db.InsertReapAckTx(tx, db.ReapAck{
		RunID: runID, StepID: stepID, Class: "write", ReapedSeq: seq,
	}, nowMS)
	testsupport.Must(t, err, "InsertReapAckTx: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)
	return seq
}

// TestAbandonClosesTheRunsOpenBallots is DKT-V11's shape: orphaned when RUN-4
// was abandoned.
func TestAbandonClosesTheRunsOpenBallots(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	ballot := openBallot(t, conn,
		voteIdempotencyKey(run.ID, issue, "commit-gate@0"), "approve the commit")
	// An operator's OWN proposal, bound to no step. It must survive: it is
	// outside the run's machinery and its author decides when it ends.
	adhoc := openBallot(t, conn, "adhoc:should-we-refactor", "an operator's question")

	_, _, err := MoveRun(conn, run.ID, "run abandon", model.RunAbandoned,
		[]model.RunStatus{model.RunActive}, "not worth finishing", nowMS)
	testsupport.Must(t, err, "run abandon: %v", err)

	if got := proposalStatus(t, conn, ballot); got != model.ProposalStatusClosed {
		t.Errorf("the run's ballot is %s after abandonment, want %s — an open "+
			"proposal is what `vote list` shows as outstanding work and what a "+
			"spawn-guard carve-out points at", got, model.ProposalStatusClosed)
	}
	if got := proposalStatus(t, conn, adhoc); got != model.ProposalStatusOpen {
		t.Errorf("an ad-hoc proposal was closed by a run's abandonment (%s); it "+
			"is outside the run's machinery and its author decides when it ends",
			got)
	}

	// The reason must say THE RUN ENDED, never that the vote was decided. A
	// final_outcome that read like a verdict would replace an honest stale-open
	// row with a dishonest decided one — and a reader can spot the first.
	outcome := proposalOutcome(t, conn, ballot)
	if !strings.Contains(outcome, "abandoned") {
		t.Errorf("the closure reason %q does not say the run was abandoned", outcome)
	}
	if !strings.Contains(outcome, "not because a verdict was reached") {
		t.Errorf("the closure reason %q does not disclaim a verdict; these "+
			"ballots reached none", outcome)
	}
	if !strings.Contains(outcome, "not worth finishing") {
		t.Errorf("the closure reason %q drops the operator's own reason", outcome)
	}
}

// TestAbandonLeavesADecidedBallotAlone: only `open` rows move.
//
// Every other status is the record of a decision, and a bulk close that
// rewrote one would be the overwrite the immutable-record rule forbids. It
// matters more here than for the single-id verb, because this caller passes a
// SET it has not inspected one by one.
func TestAbandonLeavesADecidedBallotAlone(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	decided := openBallot(t, conn,
		voteIdempotencyKey(run.ID, issue, "commit-gate@0"), "approve the commit")
	testsupport.Must(t, db.CloseProposal(conn, decided, "decided earlier"),
		"closing: %v", nil)

	_, _, err := MoveRun(conn, run.ID, "run abandon", model.RunAbandoned,
		[]model.RunStatus{model.RunActive}, "stop", nowMS)
	testsupport.Must(t, err, "run abandon: %v", err)

	if got := proposalOutcome(t, conn, decided); got != "decided earlier" {
		t.Errorf("the abandonment rewrote a decided ballot's outcome to %q; "+
			"only an open proposal may close", got)
	}
}

// TestReapAckClosesTheBallotItSatisfies is DKT-V21 and DKT-V46's shape: ack-reap
// proposals the operator resolved out of band while the ballot stayed open.
//
// The acknowledgment IS the answer. A panel was asked whether to accept a reap;
// an operator acknowledged it directly, so the question is settled.
func TestReapAckClosesTheBallotItSatisfies(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	seq := seedReapFor(t, conn, run.ID)
	ballot := openBallot(t, conn, ReapAckProposalKey(run.ID, seq), "accept the reap?")

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = ackReapsTx(tx, run.ID, []int64{seq}, "operator", nowMS)
	testsupport.Must(t, err, "ackReapsTx: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)

	if got := proposalStatus(t, conn, ballot); got != model.ProposalStatusClosed {
		t.Errorf("the ack-reap ballot is %s after the reap was acknowledged, "+
			"want %s — the acknowledgment is the answer", got, model.ProposalStatusClosed)
	}
	if got := proposalOutcome(t, conn, ballot); !strings.Contains(got, "out of band") {
		t.Errorf("the closure reason %q does not say the question was settled "+
			"out of band", got)
	}
}

// TestReapAckWithNoBallotIsNotAnError: most reaps convene no panel at all.
//
// A missing ballot is the ORDINARY case, so it must not be a failure and must
// not be a warning — an ack that reported a problem every time it ran would
// train an operator to ignore it.
func TestReapAckWithNoBallotIsNotAnError(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	seq := seedReapFor(t, conn, run.ID)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = ackReapsTx(tx, run.ID, []int64{seq}, "operator", nowMS)
	testsupport.Must(t, err, "ackReapsTx with no ballot must succeed: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit: %v", err)
}
