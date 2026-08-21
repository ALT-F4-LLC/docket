package db

import (
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// CloseProposal (DKT-114): an operator-bypassed proposal is retired rather
// than sitting open forever, and a decided proposal's record never moves.

func TestCloseProposal(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "ack-reap 488",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	err = CloseProposal(db, id, "operator authorized the ack directly")
	testsupport.Must(t, err, "CloseProposal: %v", err)

	got, err := GetProposal(db, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if got.Status != model.ProposalStatusClosed {
		t.Errorf("status = %q, want %q", got.Status, model.ProposalStatusClosed)
	}
	if got.FinalOutcome != "operator authorized the ack directly" {
		t.Errorf("final_outcome = %q, want the reason", got.FinalOutcome)
	}

	// Closed is terminal: a second close is a conflict, not an idempotent
	// success — the caller's picture of the proposal is stale and should say so.
	if err := CloseProposal(db, id, "again"); !errors.Is(err, ErrConflict) {
		t.Errorf("closing a closed proposal: err = %v, want ErrConflict", err)
	}

	if err := CloseProposal(db, 99999, "r"); !errors.Is(err, ErrNotFound) {
		t.Errorf("closing a missing proposal: err = %v, want ErrNotFound", err)
	}
}

// TestCloseProposalRefusesDecided: every decided status is a record, and a
// close that rewrote one would be the overwrite the immutable-record rule
// forbids.
func TestCloseProposalRefusesDecided(t *testing.T) {
	db := mustInitAndMigrate(t)

	for _, status := range []model.ProposalStatus{
		model.ProposalStatusApproved,
		model.ProposalStatusRejected,
		model.ProposalStatusCommitted,
	} {
		id, err := CreateProposal(db, &model.Proposal{
			Description:    "decided " + string(status),
			Criticality:    model.CriticalityLow,
			Status:         model.ProposalStatusOpen,
			RequiredVoters: 1,
			Threshold:      0.5,
		})
		testsupport.Must(t, err, "CreateProposal: %v", err)
		_, err = db.Exec(`UPDATE proposals SET status = ? WHERE id = ?`, string(status), id)
		testsupport.Must(t, err, "setting status: %v", err)

		if err := CloseProposal(db, id, "r"); !errors.Is(err, ErrConflict) {
			t.Errorf("closing a %s proposal: err = %v, want ErrConflict", status, err)
		}
	}
}

// TestCastVoteRefusedOnClosedProposal: closed is not open, so the existing
// finalized guard refuses further casts — a seat cannot vote a settled
// question back to life.
func TestCastVoteRefusedOnClosedProposal(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "to close",
		Criticality:    model.CriticalityLow,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.5,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	testsupport.Must(t, CloseProposal(db, id, "settled elsewhere"), "CloseProposal")

	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "late-seat",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("casting on a closed proposal: err = %v, want ErrConflict", err)
	}
}
