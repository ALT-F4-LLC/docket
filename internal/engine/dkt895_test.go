package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-895 — the `vote-tallied` event's detail rendered the WEIGHTED SCORE as a
// bare parenthesised number, which reads as a ballot count.
//
// RUN-62 is the measured case: a three-seat panel all cast
// approve-with-concerns, the tally scored 1.00 against a 67% threshold, and the
// feed said `DKT-V289 approved (1)`. A reader following the run saw what looked
// like a one-ballot approval on a three-seat panel — a panel failure — and
// could only disprove it by leaving the feed for `docket vote show`.

// dkt895VoteSrc is that shape reduced: one declared vote gate, three voters,
// and a rejection that skips rather than looping (the loop is DKT-168's test,
// not this one).
const dkt895VoteSrc = `
[pipeline]
name = "dkt895-vote"
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
voters = ["seat-a", "seat-b", "seat-c"]
vote_rule = "majority"
on_fail = "skip"
`

// TestTalliedVoteEventNamesScoreAndBallotsSeparately pins the remedy: the
// detail labels both numbers, so a 3-ballot unanimous approval scoring 1.00
// cannot be read as one ballot.
func TestTalliedVoteEventNamesScoreAndBallotsSeparately(t *testing.T) {
	conn := mustDB(t)
	// RUN-62's threshold, verbatim.
	registerVoteRule(t, conn, "majority", "0.67", "")
	registerSource(t, conn, []byte(dkt895VoteSrc), "dkt895-vote.toml")
	issue := createIssue(t, conn, "voted", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	claimAndComplete(t, conn, e, "seed@0", "the findings", "")
	err = e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after the record: %v", err)

	gate, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
	testsupport.Must(t, err, "reading gate@0: %v", err)
	proposalID, err := findVoteProposal(conn, gate)
	testsupport.Must(t, err, "finding gate@0's proposal: %v", err)
	if proposalID == 0 {
		t.Fatal("no proposal opened for gate@0")
	}

	// Three seats, all approve-with-concerns: db.CastVote's own arithmetic
	// puts every cast's weight in the numerator, so the score is exactly 1.
	for _, seat := range []string{"seat-a", "seat-b", "seat-c"} {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID: proposalID, VoterName: seat,
			Verdict:    model.VerdictApproveWithConcerns,
			Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the approved proposal: %v", err)

	var raw string
	err = conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		run.ID, EventVoteTallied).Scan(&raw)
	testsupport.Must(t, err, "reading the vote-tallied event: %v", err)

	// `data` is the recorded envelope; `detail` is the string an operator reads
	// in `docket events list`.
	var envelope struct {
		Detail string `json:"detail"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(raw), &envelope),
		"decoding the event data %q: %v", raw, err)

	want := model.FormatProposalID(proposalID) + " approved score=1.00 ballots=3/3"
	if envelope.Detail != want {
		t.Errorf("vote-tallied detail = %q, want %q", envelope.Detail, want)
	}

	// The defect stated as its own assertion: the count-like rendering is gone.
	if strings.Contains(envelope.Detail, "(1)") {
		t.Errorf("vote-tallied detail %q still renders the score as a bare "+
			"count-like number", envelope.Detail)
	}

	// And the detail agrees with `vote show`, digit for digit — the surface a
	// reader had to leave the feed for.
	proposal, err := db.GetProposal(conn, proposalID)
	testsupport.Must(t, err, "GetProposal: %v", err)
	votes, err := db.GetProposalVotes(conn, proposalID)
	testsupport.Must(t, err, "GetProposalVotes: %v", err)
	if proposal.WeightedScore == nil || *proposal.WeightedScore != 1 {
		t.Fatalf("weighted_score = %v, want 1", proposal.WeightedScore)
	}
	if len(votes) != 3 || proposal.RequiredVoters != 3 {
		t.Fatalf("ballots = %d/%d, want 3/3", len(votes), proposal.RequiredVoters)
	}
}

// TestTalliedVoteEventWithoutAScoreSaysSo: a proposal finalized with no
// weighted score (an operator's manual commit, §8.4) still labels both fields
// rather than printing a bare parenthesis.
func TestTalliedVoteEventWithoutAScoreSaysSo(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.6", "medium")
	step, spec := seedVoteStep(t, conn)

	id, err := OpenVoteProposal(conn, step, spec, nowMS)
	testsupport.Must(t, err, "OpenVoteProposal: %v", err)
	_, err = conn.Exec(`UPDATE proposals SET status = ? WHERE id = ?`,
		model.ProposalStatusCommitted, id)
	testsupport.Must(t, err, "committing the proposal: %v", err)

	outcome, err := ReadVoteOutcome(conn, step, spec)
	testsupport.Must(t, err, "ReadVoteOutcome: %v", err)

	detail, err := voteTallyDetail(conn, outcome)
	testsupport.Must(t, err, "voteTallyDetail: %v", err)

	want := model.FormatProposalID(id) + " committed score=none ballots=0/3"
	if detail != want {
		t.Errorf("vote-tallied detail = %q, want %q", detail, want)
	}
}
