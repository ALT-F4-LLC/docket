package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The vote facts on the wire: `voters` and `proposal` on a step row.
//
// A caller holding a `next` row could see a vote step sitting open and had no
// way to learn who had not cast, nor which proposal to cast on. The roster was
// readable only by opening the pinned definition off disk, and the proposal id
// only by re-deriving an idempotency key nothing on the wire disclosed — so
// the two facts a voter needs were the two the row did not carry.

// voteRow finds one instance among a `next --run` answer.
func voteRow(t *testing.T, rows []model.StepRow, instance string) model.StepRow {
	t.Helper()
	for _, row := range rows {
		if row.Instance == instance {
			return row
		}
	}
	t.Fatalf("%s is not among the offered rows", instance)
	return model.StepRow{}
}

// TestVoteStepRowCarriesVotersAndProposal is the wire contract, over the vote
// step the engine itself mints.
func TestVoteStepRowCarriesVotersAndProposal(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	driveToReconcile(t, conn, e, clusteredPayload)

	// The FIRST answer already carries the proposal: this call is the one that
	// opens it (§8.1 phase 2), and reporting it a poll later would show a vote
	// with no ballot to cast on.
	row := voteRow(t, nextRun(t, conn, e).Steps, "reconcile-held@0#0")
	if row.Kind != workflow.TypeVote {
		t.Fatalf("kind = %q, want %q", row.Kind, workflow.TypeVote)
	}
	if strings.Join(row.Voters, ",") != "alice,bob,carol" {
		t.Errorf("voters = %v, want the roster the vote is waiting on", row.Voters)
	}
	if row.Proposal == "" {
		t.Fatal("the row carries no proposal id after the proposal was opened")
	}

	// The id resolves to a real proposal, whose required voters is the roster's
	// size — so the field names the ballot rather than merely being non-empty.
	id, err := model.ParseProposalID(row.Proposal)
	testsupport.Must(t, err, "parsing %q: %v", row.Proposal, err)
	proposal, err := db.GetProposal(conn, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if proposal.RequiredVoters != 3 {
		t.Errorf("required_voters = %d, want the roster's 3", proposal.RequiredVoters)
	}

	// A second answer reports the SAME id, resolved from the snapshot rather
	// than from a re-open.
	again := voteRow(t, nextRun(t, conn, e).Steps, "reconcile-held@0#0")
	if again.Proposal != row.Proposal {
		t.Errorf("proposal = %q on the second answer, want the stable %q",
			again.Proposal, row.Proposal)
	}
}

// TestStepShowCarriesTheVoteFacts: the same two fields on the read verb, since
// an operator diagnosing a stalled run reaches for `step show` rather than for
// a `next` they may not be entitled to trigger.
func TestStepShowCarriesTheVoteFacts(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	driveToReconcile(t, conn, e, clusteredPayload)
	nextRun(t, conn, e)

	view, err := LoadStepView(conn,
		stepIDByInstance(t, conn, "reconcile-held@0#0"), nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if len(view.Row.Voters) != 3 {
		t.Errorf("voters = %v on `step show`", view.Row.Voters)
	}
	if view.Row.Proposal == "" {
		t.Error("`step show` carries no proposal id for an open vote")
	}
}

// TestNonVoteRowsCarryNeitherField is the omission half: the fields are absent
// on every other kind, so a caller cannot read an empty roster as a real one.
func TestNonVoteRowsCarryNeitherField(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()

	for _, row := range nextRun(t, conn, e).Steps {
		if row.Kind == workflow.TypeVote {
			continue
		}
		if len(row.Voters) != 0 {
			t.Errorf("%s (kind %q) carries voters %v", row.Instance, row.Kind, row.Voters)
		}
		if row.Proposal != "" {
			t.Errorf("%s (kind %q) carries proposal %q",
				row.Instance, row.Kind, row.Proposal)
		}
	}
}
