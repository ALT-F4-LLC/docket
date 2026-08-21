package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// BackfillVoteUsage (DKT-115): governance spend reaches the vote-usage ledger
// after the casts have landed, distinguishable from the seats' own cast-time
// reports by source.

func backfillProposal(t *testing.T, conn *sql.DB, voters ...string) int {
	t.Helper()
	id, err := db.CreateProposal(conn, &model.Proposal{
		Description:    "panel",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: len(voters) + 1, // stay open: the tally is not under test
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	for _, voter := range voters {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID: id, VoterName: voter,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", voter, err)
	}
	return id
}

func voteUsageRows(t *testing.T, conn *sql.DB, proposalID int) map[string]map[string]struct {
	quantity float64
	source   string
} {
	t.Helper()
	rows, err := conn.Query(
		`SELECT v.voter_name, vu.unit, vu.quantity, vu.source
		   FROM vote_usage vu JOIN votes v ON v.id = vu.vote_id
		  WHERE v.proposal_id = ?`, proposalID)
	testsupport.Must(t, err, "reading vote_usage: %v", err)
	defer rows.Close()
	out := map[string]map[string]struct {
		quantity float64
		source   string
	}{}
	for rows.Next() {
		var voter, unit, source string
		var quantity float64
		testsupport.Must(t, rows.Scan(&voter, &unit, &quantity, &source), "scan")
		if out[voter] == nil {
			out[voter] = map[string]struct {
				quantity float64
				source   string
			}{}
		}
		out[voter][unit] = struct {
			quantity float64
			source   string
		}{quantity, source}
	}
	testsupport.Must(t, rows.Err(), "rows: %v", rows.Err())
	return out
}

func TestBackfillVoteUsage(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	id := backfillProposal(t, conn, "tribunal-security", "tribunal-correctness")

	err := e.BackfillVoteUsage(conn, id, []VoteBackfillRow{
		{Voter: "tribunal-security", Unit: "output_tokens", Quantity: 48211},
		{Voter: "tribunal-security", Unit: "input_tokens", Quantity: 146},
		{Voter: "tribunal-correctness", Unit: "output_tokens", Quantity: 30275},
	}, "", nowMS)
	testsupport.Must(t, err, "BackfillVoteUsage: %v", err)

	got := voteUsageRows(t, conn, id)
	if got["tribunal-security"]["output_tokens"].quantity != 48211 {
		t.Errorf("security output_tokens = %+v, want 48211",
			got["tribunal-security"]["output_tokens"])
	}
	if len(got["tribunal-security"]) != 2 || len(got["tribunal-correctness"]) != 1 {
		t.Errorf("row spread = %+v, want 2 units on security and 1 on correctness", got)
	}
	// The default source marks the relay's reconstruction, never the seat's
	// own report.
	for voter, units := range got {
		for unit, row := range units {
			if row.source != UsageSourceBackfilled {
				t.Errorf("%s/%s source = %q, want %q",
					voter, unit, row.source, UsageSourceBackfilled)
			}
		}
	}
}

func TestBackfillVoteUsageRefusals(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	id := backfillProposal(t, conn, "seat-1")

	// A seat that never cast has nothing to attach to.
	err := e.BackfillVoteUsage(conn, id, []VoteBackfillRow{
		{Voter: "never-cast", Unit: "tokens", Quantity: 1},
	}, "", nowMS)
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("unknown seat: code = %q (err %v), want %q", code, err, CodeValidation)
	}

	// A missing proposal is a miss, not a vacuous success.
	err = e.BackfillVoteUsage(conn, 99999, []VoteBackfillRow{
		{Voter: "seat-1", Unit: "tokens", Quantity: 1},
	}, "", nowMS)
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("missing proposal: code = %q (err %v), want %q", code, err, CodeNotFound)
	}

	// An empty batch is a refusal, not a no-op "success".
	if code, _ := CodeOf(e.BackfillVoteUsage(conn, id, nil, "", nowMS)); code != CodeValidation {
		t.Errorf("empty batch: code = %q, want %q", code, CodeValidation)
	}

	// The (vote, unit) key never merges: a repeat is a double-count refused.
	testsupport.Must(t, e.BackfillVoteUsage(conn, id, []VoteBackfillRow{
		{Voter: "seat-1", Unit: "tokens", Quantity: 10},
	}, "", nowMS), "first back-fill")
	err = e.BackfillVoteUsage(conn, id, []VoteBackfillRow{
		{Voter: "seat-1", Unit: "tokens", Quantity: 10},
	}, "", nowMS)
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("repeat back-fill: code = %q (err %v), want %q", code, err, CodeConflict)
	}

	// A cast-time report holds the same key: the back-fill cannot overwrite it.
	id2 := backfillProposal(t, conn)
	_, err = db.CastVote(conn, &model.Vote{
		ProposalID: id2, VoterName: "self-reporting",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
		Usage: map[string]float64{"tokens": 5},
	})
	testsupport.Must(t, err, "CastVote with usage: %v", err)
	got := voteUsageRows(t, conn, id2)
	if row := got["self-reporting"]["tokens"]; row.source != db.UsageSourceReported {
		t.Errorf("cast-time source = %q, want %q", row.source, db.UsageSourceReported)
	}
	err = e.BackfillVoteUsage(conn, id2, []VoteBackfillRow{
		{Voter: "self-reporting", Unit: "tokens", Quantity: 6},
	}, "", nowMS)
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("back-fill over a seat's own report: code = %q (err %v), want %q",
			code, err, CodeConflict)
	}
}

// TestBackfillVoteUsageIsOneTransaction: a batch whose later row refuses
// writes nothing — a half-applied batch strands its remainder behind the
// ledger's unique key on the re-run.
func TestBackfillVoteUsageIsOneTransaction(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	id := backfillProposal(t, conn, "seat-1")

	err := e.BackfillVoteUsage(conn, id, []VoteBackfillRow{
		{Voter: "seat-1", Unit: "tokens", Quantity: 10},
		{Voter: "ghost", Unit: "tokens", Quantity: 1},
	}, "", nowMS)
	if err == nil {
		t.Fatal("a batch naming a seat that never cast succeeded")
	}
	if rows := voteUsageRows(t, conn, id); len(rows) != 0 {
		t.Errorf("the refused batch left rows behind: %+v", rows)
	}
}
