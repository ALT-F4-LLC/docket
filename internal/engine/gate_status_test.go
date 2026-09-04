package engine

import (
	"database/sql"
	"slices"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `docket gate status` — DKT-1286.

// voteGateStatusSrc is a plain `type="vote"` gate with no threshold routing
// (the default, RoutingPass) — GateStatus's own facts are under test here, not
// a workflow's routing table.
const voteGateStatusSrc = `
[pipeline]
name = "vote-gate-status"
version = 1

[match]
kind = ["task"]

[[step]]
name = "seed"
after = []
executor = "x"
emits = "findings"

[[step]]
name = "gate"
after = ["seed"]
type = "vote"
voters = ["alice", "bob", "carol"]
vote_rule = "majority"
on_fail = "waiting-human"
`

// activatedVoteGateRun registers voteGateStatusSrc, activates one run over it,
// and returns the run id.
func activatedVoteGateRun(t *testing.T, conn *sql.DB) int {
	t.Helper()
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(voteGateStatusSrc), "vote-gate-status.toml")
	issue := createIssue(t, conn, "subject", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run.ID
}

// TestGateStatusRefusesANonGateStep is the kind guard: `gate status` answers
// for `type="human"` or `type="vote"` steps only, the same restriction `guard
// gate` applies.
func TestGateStatusRefusesANonGateStep(t *testing.T) {
	conn := mustDB(t)
	activatedVoteGateRun(t, conn)
	seedID := stepIDByInstance(t, conn, "seed@0")

	_, err := GateStatus(conn, seedID, nowMS)
	if err == nil {
		t.Fatal("GateStatus answered for an executor step, which has no gate to report")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("error code = %q, want %q", code, CodeValidation)
	}
}

// TestGateStatusUndecidedVoteReportsEveryVoterMissing is AC1's undecided
// shape: before a proposal even opens, every declared seat reads as missing —
// nobody can have cast on a ballot that does not exist yet.
func TestGateStatusUndecidedVoteReportsEveryVoterMissing(t *testing.T) {
	conn := mustDB(t)
	activatedVoteGateRun(t, conn)
	gateID := stepIDByInstance(t, conn, "gate@0")

	status, err := GateStatus(conn, gateID, nowMS)
	testsupport.Must(t, err, "GateStatus: %v", err)

	if status.Outcome != GateOutcomeOpen {
		t.Errorf("outcome = %q, want open — no proposal has opened yet", status.Outcome)
	}
	if status.Proposal != "" {
		t.Errorf("proposal = %q, want empty before one opens", status.Proposal)
	}
	if status.Tally != nil {
		t.Errorf("tally = %+v, want nil before a proposal exists to tally", status.Tally)
	}
	wantVoters := []string{"alice", "bob", "carol"}
	if len(status.Seats) != len(wantVoters) {
		t.Fatalf("seats = %v, want the %d declared voters", status.Seats, len(wantVoters))
	}
	for i, seat := range status.Seats {
		if seat.Voter != wantVoters[i] || seat.Cast {
			t.Errorf("seat[%d] = %+v, want {%s false}", i, seat, wantVoters[i])
		}
	}
	if len(status.MissingSeats) != len(wantVoters) {
		t.Errorf("missing_seats = %v, want all %v", status.MissingSeats, wantVoters)
	}
}

// TestGateStatusReportsPartialCastsAndMissingSeats is AC1's live case: one
// seat cast, two have not — one call answers both without a second read.
func TestGateStatusReportsPartialCastsAndMissingSeats(t *testing.T) {
	conn := mustDB(t)
	runID := activatedVoteGateRun(t, conn)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, runID)
	castSeat(t, conn, proposalID, "alice", model.VerdictApprove, "")

	gateID := stepIDByInstance(t, conn, "gate@0")
	status, err := GateStatus(conn, gateID, nowMS)
	testsupport.Must(t, err, "GateStatus: %v", err)

	if status.Outcome != GateOutcomeOpen {
		t.Errorf("outcome = %q, want open — quorum not reached at 1/3", status.Outcome)
	}
	if status.Proposal != model.FormatProposalID(proposalID) {
		t.Errorf("proposal = %q, want %q", status.Proposal, model.FormatProposalID(proposalID))
	}
	if status.Tally == nil || status.Tally.Threshold != 0.5 {
		t.Errorf("tally = %+v, want threshold 0.5 once the proposal exists", status.Tally)
	}

	cast := map[string]bool{}
	for _, seat := range status.Seats {
		cast[seat.Voter] = seat.Cast
	}
	if !cast["alice"] || cast["bob"] || cast["carol"] {
		t.Errorf("seats cast = %v, want only alice", cast)
	}
	if len(status.MissingSeats) != 2 {
		t.Fatalf("missing_seats = %v, want [bob carol]", status.MissingSeats)
	}
	for _, want := range []string{"bob", "carol"} {
		if !slices.Contains(status.MissingSeats, want) {
			t.Errorf("missing_seats %v does not name %q", status.MissingSeats, want)
		}
	}
}

// TestGateStatusApprovedTallyCarriesEveryVerdict is AC1's decided case: once
// every seat casts and the tally approves, the outcome, tally and each seat's
// verdict all read from the one call, with no seat left missing.
func TestGateStatusApprovedTallyCarriesEveryVerdict(t *testing.T) {
	conn := mustDB(t)
	runID := activatedVoteGateRun(t, conn)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, runID)
	castSeat(t, conn, proposalID, "alice", model.VerdictApprove, "")
	castSeat(t, conn, proposalID, "bob", model.VerdictApproveWithConcerns, "minor nit")
	castSeat(t, conn, proposalID, "carol", model.VerdictApprove, "")
	testsupport.Must(t, e.DriveVoteProposal(conn, proposalID, nowMS),
		"DriveVoteProposal: %v", nil)

	gateID := stepIDByInstance(t, conn, "gate@0")
	status, err := GateStatus(conn, gateID, nowMS)
	testsupport.Must(t, err, "GateStatus: %v", err)

	if status.Outcome != GateOutcomeApproved {
		t.Errorf("outcome = %q, want approved", status.Outcome)
	}
	if status.Tally == nil || status.Tally.WeightedScore == nil {
		t.Fatalf("tally = %+v, want a weighted score on a decided proposal", status.Tally)
	}
	if len(status.MissingSeats) != 0 {
		t.Errorf("missing_seats = %v, want none — every seat cast", status.MissingSeats)
	}
	verdicts := map[string]string{}
	for _, seat := range status.Seats {
		if !seat.Cast {
			t.Errorf("seat %q did not cast, want every seat cast", seat.Voter)
		}
		verdicts[seat.Voter] = seat.Verdict
	}
	if verdicts["bob"] != string(model.VerdictApproveWithConcerns) {
		t.Errorf("bob's verdict = %q, want %q", verdicts["bob"], model.VerdictApproveWithConcerns)
	}
}

// TestGateStatusRejectedTally is the outcome's other decided branch.
func TestGateStatusRejectedTally(t *testing.T) {
	conn := mustDB(t)
	runID := activatedVoteGateRun(t, conn)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, runID)
	castSeat(t, conn, proposalID, "alice", model.VerdictReject, "no")
	castSeat(t, conn, proposalID, "bob", model.VerdictReject, "no")
	castSeat(t, conn, proposalID, "carol", model.VerdictReject, "no")
	testsupport.Must(t, e.DriveVoteProposal(conn, proposalID, nowMS),
		"DriveVoteProposal: %v", nil)

	gateID := stepIDByInstance(t, conn, "gate@0")
	status, err := GateStatus(conn, gateID, nowMS)
	testsupport.Must(t, err, "GateStatus: %v", err)

	if status.Outcome != GateOutcomeRejected {
		t.Errorf("outcome = %q, want rejected", status.Outcome)
	}
}

// TestGateStatusClosedProposalReadsOpen is the collapse GateOutcome documents:
// a proposal retired without a tally (DKT-114) answered no vote, so it reads
// as undecided rather than adding a fourth outcome nothing above this asks for.
func TestGateStatusClosedProposalReadsOpen(t *testing.T) {
	conn := mustDB(t)
	runID := activatedVoteGateRun(t, conn)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, runID)
	_, err := conn.Exec(`UPDATE proposals SET status = ? WHERE id = ?`,
		model.ProposalStatusClosed, proposalID)
	testsupport.Must(t, err, "closing the proposal: %v", err)

	gateID := stepIDByInstance(t, conn, "gate@0")
	status, err := GateStatus(conn, gateID, nowMS)
	testsupport.Must(t, err, "GateStatus: %v", err)

	if status.Outcome != GateOutcomeOpen {
		t.Errorf("outcome = %q, want open for a closed, untallied proposal", status.Outcome)
	}
}
