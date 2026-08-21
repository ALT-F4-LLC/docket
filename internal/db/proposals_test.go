package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// mustInitAndMigrate initializes a fresh in-memory DB and runs migrations.
func mustInitAndMigrate(t *testing.T) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)
	return db
}

// --- Schema Migration ---

func TestMigrateV1ToV2CreatesProposalTables(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	// Before migration, schema is at v1.
	v, err := SchemaVersion(db)
	testsupport.Must(t, err, "SchemaVersion: %v", err)
	if v != 1 {
		t.Fatalf("schema_version = %d before migration, want 1", v)
	}

	// Run migration.
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate: %v", err)

	// Migrate advances all the way to head; this test verifies v1→v2 in
	// particular, but Migrate is contractually all-or-head.
	v, err = SchemaVersion(db)
	testsupport.Must(t, err, "SchemaVersion: %v", err)
	if v != currentSchemaVersion {
		t.Fatalf("schema_version = %d after migration, want %d", v, currentSchemaVersion)
	}

	// Verify new tables exist.
	for _, table := range []string{"proposals", "votes", "proposal_issues"} {
		var name string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found after migration: %v", table, err)
		}
	}

	// Verify indexes exist.
	for _, idx := range []string{"idx_proposals_status", "idx_proposals_created_at", "idx_votes_proposal_id"} {
		var name string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", idx,
		).Scan(&name)
		if err != nil {
			t.Errorf("index %q not found after migration: %v", idx, err)
		}
	}
}

// --- CreateProposal / GetProposal CRUD ---

func TestCreateAndGetProposal(t *testing.T) {
	db := mustInitAndMigrate(t)

	p := &model.Proposal{
		Description:    "Test proposal",
		Criticality:    model.CriticalityHigh,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
		CreatedBy:      "test-user",
	}

	id, err := CreateProposal(db, p)
	testsupport.Must(t, err, "CreateProposal: %v", err)
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	got, err := GetProposal(db, id)
	testsupport.Must(t, err, "GetProposal: %v", err)

	if got.ID != id {
		t.Errorf("ID = %d, want %d", got.ID, id)
	}
	if got.Description != "Test proposal" {
		t.Errorf("Description = %q, want %q", got.Description, "Test proposal")
	}
	if got.Criticality != model.CriticalityHigh {
		t.Errorf("Criticality = %q, want %q", got.Criticality, model.CriticalityHigh)
	}
	if got.Status != model.ProposalStatusOpen {
		t.Errorf("Status = %q, want %q", got.Status, model.ProposalStatusOpen)
	}
	if got.RequiredVoters != 3 {
		t.Errorf("RequiredVoters = %d, want 3", got.RequiredVoters)
	}
	if got.Threshold != 0.67 {
		t.Errorf("Threshold = %f, want 0.67", got.Threshold)
	}
	if got.WeightedScore != nil {
		t.Errorf("WeightedScore = %v, want nil", got.WeightedScore)
	}
	if got.CreatedBy != "test-user" {
		t.Errorf("CreatedBy = %q, want %q", got.CreatedBy, "test-user")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
}

func TestGetProposalNotFound(t *testing.T) {
	db := mustInitAndMigrate(t)

	_, err := GetProposal(db, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// --- ListProposals ---

func TestListProposals(t *testing.T) {
	db := mustInitAndMigrate(t)

	// Create proposals with different statuses and criticalities.
	proposals := []struct {
		desc        string
		criticality model.Criticality
		status      model.ProposalStatus
	}{
		{"Open high", model.CriticalityHigh, model.ProposalStatusOpen},
		{"Open low", model.CriticalityLow, model.ProposalStatusOpen},
		{"Approved medium", model.CriticalityMedium, model.ProposalStatusApproved},
	}

	for _, pp := range proposals {
		_, err := CreateProposal(db, &model.Proposal{
			Description:    pp.desc,
			Criticality:    pp.criticality,
			Status:         pp.status,
			RequiredVoters: 1,
			Threshold:      0.67,
		})
		testsupport.Must(t, err, "CreateProposal(%q): %v", pp.desc, err)
	}

	// List all (no filters).
	list, total, err := ListProposals(db, 0, "", "", "", 0)
	testsupport.Must(t, err, "ListProposals (all): %v", err)
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(list) != 3 {
		t.Errorf("len = %d, want 3", len(list))
	}

	// Filter by status.
	list, total, err = ListProposals(db, 0, "open", "", "", 0)
	testsupport.Must(t, err, "ListProposals (open): %v", err)
	if total != 2 {
		t.Errorf("total open = %d, want 2", total)
	}
	if len(list) != 2 {
		t.Errorf("len open = %d, want 2", len(list))
	}

	// Filter by criticality.
	list, total, err = ListProposals(db, 0, "", "high", "", 0)
	testsupport.Must(t, err, "ListProposals (high): %v", err)
	if total != 1 {
		t.Errorf("total high = %d, want 1", total)
	}
	if len(list) != 1 || list[0].Description != "Open high" {
		t.Errorf("list high = %+v, want [Open high]", list)
	}

	// Limit.
	list, _, err = ListProposals(db, 0, "", "", "", 1)
	testsupport.Must(t, err, "ListProposals (limit 1): %v", err)
	if len(list) != 1 {
		t.Errorf("len with limit = %d, want 1", len(list))
	}
}

// --- CastVote happy path ---

func TestCastVoteHappyPath(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Vote test",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	result, err := CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-1",
		VoterRole:       "security",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Findings:        "Looks good",
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	if result.Vote.ID <= 0 {
		t.Errorf("vote ID = %d, want > 0", result.Vote.ID)
	}
	if result.ProposalStatus != model.ProposalStatusOpen {
		t.Errorf("status = %q, want %q", result.ProposalStatus, model.ProposalStatusOpen)
	}
	if result.VotesCast != 1 {
		t.Errorf("votes_cast = %d, want 1", result.VotesCast)
	}
	if result.VotesRequired != 3 {
		t.Errorf("votes_required = %d, want 3", result.VotesRequired)
	}
	if result.QuorumReached {
		t.Error("quorum_reached = true, want false")
	}
	if result.WeightedScore != nil {
		t.Errorf("weighted_score = %v, want nil", result.WeightedScore)
	}
}

// --- CastVote auto-finalization ---

func TestCastVoteAutoFinalizationApproved(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Auto finalize test",
		Criticality:    model.CriticalityHigh,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 2,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	// Vote 1: approve with high confidence.
	_, err = CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-1",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
	})
	testsupport.Must(t, err, "CastVote 1: %v", err)

	// Vote 2 (quorum): approve.
	result, err := CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-2",
		Verdict:         model.VerdictApprove,
		Confidence:      0.8,
		DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote 2: %v", err)

	if !result.QuorumReached {
		t.Error("expected quorum_reached = true")
	}
	if result.ProposalStatus != model.ProposalStatusApproved {
		t.Errorf("status = %q, want %q", result.ProposalStatus, model.ProposalStatusApproved)
	}
	if result.WeightedScore == nil {
		t.Fatal("expected weighted_score, got nil")
	}

	// Verify weighted score computation:
	// voter-1: conf=0.9, rel=0.8, approve -> weight=0.72, weighted=0.72
	// voter-2: conf=0.8, rel=0.9, approve -> weight=0.72, weighted=0.72
	// score = (0.72 + 0.72) / (0.72 + 0.72) = 1.0
	if *result.WeightedScore != 1.0 {
		t.Errorf("weighted_score = %f, want 1.0", *result.WeightedScore)
	}

	// Verify proposal persisted as approved.
	p, err := GetProposal(db, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if p.Status != model.ProposalStatusApproved {
		t.Errorf("persisted status = %q, want %q", p.Status, model.ProposalStatusApproved)
	}
	if p.WeightedScore == nil || *p.WeightedScore != 1.0 {
		t.Errorf("persisted weighted_score = %v, want 1.0", p.WeightedScore)
	}
}

func TestCastVoteAutoFinalizationRejected(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Reject test",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 2,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	// Vote 1: reject.
	_, err = CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-1",
		Verdict:         model.VerdictReject,
		Confidence:      0.9,
		DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote 1: %v", err)

	// Vote 2: reject (quorum).
	result, err := CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-2",
		Verdict:         model.VerdictReject,
		Confidence:      0.8,
		DomainRelevance: 0.8,
	})
	testsupport.Must(t, err, "CastVote 2: %v", err)

	if result.ProposalStatus != model.ProposalStatusRejected {
		t.Errorf("status = %q, want %q", result.ProposalStatus, model.ProposalStatusRejected)
	}
	if result.WeightedScore == nil || *result.WeightedScore != 0.0 {
		t.Errorf("weighted_score = %v, want 0.0", result.WeightedScore)
	}
}

func TestCastVoteMixedVerdictWeightedScore(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Mixed vote test",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	// Vote 1: approve, conf=0.9, rel=1.0 -> weight=0.9, weighted=0.9
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 1.0,
	})
	testsupport.Must(t, err, "CastVote 1: %v", err)

	// Vote 2: reject, conf=0.8, rel=0.5 -> weight=0.4, weighted=0.0
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-2",
		Verdict: model.VerdictReject, Confidence: 0.8, DomainRelevance: 0.5,
	})
	testsupport.Must(t, err, "CastVote 2: %v", err)

	// Vote 3: approve, conf=0.7, rel=0.6 -> weight=0.42, weighted=0.42
	result, err := CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-3",
		Verdict: model.VerdictApprove, Confidence: 0.7, DomainRelevance: 0.6,
	})
	testsupport.Must(t, err, "CastVote 3: %v", err)

	// Expected score = (0.9 + 0 + 0.42) / (0.9 + 0.4 + 0.42) = 1.32 / 1.72 ≈ 0.7674
	if result.WeightedScore == nil {
		t.Fatal("expected weighted_score, got nil")
	}
	score := *result.WeightedScore
	if score < 0.76 || score > 0.77 {
		t.Errorf("weighted_score = %f, want ~0.7674", score)
	}

	// Score > 0.67 threshold -> approved.
	if result.ProposalStatus != model.ProposalStatusApproved {
		t.Errorf("status = %q, want %q", result.ProposalStatus, model.ProposalStatusApproved)
	}
}

// --- CastVote edge case: all-zero weights ---

func TestCastVoteAllZeroWeightsRejected(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Zero weights",
		Criticality:    model.CriticalityLow,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 2,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	// Both voters have 0 confidence or 0 domain_relevance.
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.0, DomainRelevance: 0.5,
	})
	testsupport.Must(t, err, "CastVote 1: %v", err)

	result, err := CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-2",
		Verdict: model.VerdictApprove, Confidence: 0.5, DomainRelevance: 0.0,
	})
	testsupport.Must(t, err, "CastVote 2: %v", err)

	if result.ProposalStatus != model.ProposalStatusRejected {
		t.Errorf("status = %q, want %q (all-zero weights)", result.ProposalStatus, model.ProposalStatusRejected)
	}
	if result.WeightedScore == nil || *result.WeightedScore != 0.0 {
		t.Errorf("weighted_score = %v, want 0.0", result.WeightedScore)
	}
}

// --- CastVote duplicate voter ---

func TestCastVoteDuplicateVoterRejected(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Dup voter",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	// Same voter again.
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictReject, Confidence: 0.5, DomainRelevance: 0.5,
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict for duplicate voter, got %v", err)
	}
}

// --- CastVote on finalized proposal ---

func TestCastVoteOnFinalizedProposalRejected(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Finalized test",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	// Single vote finalizes.
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	// Try voting on finalized proposal.
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-2",
		Verdict: model.VerdictReject, Confidence: 0.5, DomainRelevance: 0.5,
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict for finalized proposal, got %v", err)
	}
}

// --- CastVote on nonexistent proposal ---

func TestCastVoteProposalNotFound(t *testing.T) {
	db := mustInitAndMigrate(t)

	_, err := CastVote(db, &model.Vote{
		ProposalID: 999, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// --- GetProposalVotes ---

func TestGetProposalVotes(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Votes retrieval",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	for _, name := range []string{"alice", "bob"} {
		_, err := CastVote(db, &model.Vote{
			ProposalID: id, VoterName: name,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
			Findings: "ok from " + name,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", name, err)
	}

	votes, err := GetProposalVotes(db, id)
	testsupport.Must(t, err, "GetProposalVotes: %v", err)
	if len(votes) != 2 {
		t.Fatalf("len(votes) = %d, want 2", len(votes))
	}
	if votes[0].VoterName != "alice" {
		t.Errorf("votes[0].VoterName = %q, want %q", votes[0].VoterName, "alice")
	}
	if votes[1].VoterName != "bob" {
		t.Errorf("votes[1].VoterName = %q, want %q", votes[1].VoterName, "bob")
	}
	if votes[0].Findings != "ok from alice" {
		t.Errorf("votes[0].Findings = %q, want %q", votes[0].Findings, "ok from alice")
	}
}

// --- LinkProposalIssue / UnlinkProposalIssue / GetProposalIssues ---

func createTestIssueForProposal(t *testing.T, conn *sql.DB, title string) int {
	t.Helper()
	issue := &model.Issue{
		Title:    title,
		Status:   model.StatusBacklog,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}
	id, err := CreateIssue(conn, issue, nil, nil)
	testsupport.Must(t, err, "CreateIssue(%q): %v", title, err)
	return id
}

func TestLinkAndGetProposalIssues(t *testing.T) {
	db := mustInitAndMigrate(t)

	pid, err := CreateProposal(db, &model.Proposal{
		Description: "Link test", Criticality: model.CriticalityMedium,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	iid1 := createTestIssueForProposal(t, db, "issue-1")
	iid2 := createTestIssueForProposal(t, db, "issue-2")

	err = LinkProposalIssue(db, pid, iid1)
	testsupport.Must(t, err, "LinkProposalIssue 1: %v", err)
	err = LinkProposalIssue(db, pid, iid2)
	testsupport.Must(t, err, "LinkProposalIssue 2: %v", err)

	ids, err := GetProposalIssues(db, pid)
	testsupport.Must(t, err, "GetProposalIssues: %v", err)
	if len(ids) != 2 {
		t.Fatalf("len(ids) = %d, want 2", len(ids))
	}
	// Sorted by issue_id ASC.
	if ids[0] != iid1 || ids[1] != iid2 {
		t.Errorf("ids = %v, want [%d, %d]", ids, iid1, iid2)
	}
}

func TestLinkProposalIssueDuplicate(t *testing.T) {
	db := mustInitAndMigrate(t)

	pid, _ := CreateProposal(db, &model.Proposal{
		Description: "Dup link", Criticality: model.CriticalityMedium,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.67,
	})
	iid := createTestIssueForProposal(t, db, "issue-dup")

	err := LinkProposalIssue(db, pid, iid)
	testsupport.Must(t, err, "LinkProposalIssue: %v", err)

	err = LinkProposalIssue(db, pid, iid)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict for duplicate link, got %v", err)
	}
}

func TestLinkProposalIssueMissingProposal(t *testing.T) {
	db := mustInitAndMigrate(t)

	iid := createTestIssueForProposal(t, db, "issue-no-proposal")

	err := LinkProposalIssue(db, 999, iid)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing proposal, got %v", err)
	}
}

func TestLinkProposalIssueMissingIssue(t *testing.T) {
	db := mustInitAndMigrate(t)

	pid, _ := CreateProposal(db, &model.Proposal{
		Description: "Missing issue", Criticality: model.CriticalityMedium,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.67,
	})

	err := LinkProposalIssue(db, pid, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing issue, got %v", err)
	}
}

func TestUnlinkProposalIssue(t *testing.T) {
	db := mustInitAndMigrate(t)

	pid, _ := CreateProposal(db, &model.Proposal{
		Description: "Unlink test", Criticality: model.CriticalityMedium,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.67,
	})
	iid := createTestIssueForProposal(t, db, "issue-unlink")

	err := LinkProposalIssue(db, pid, iid)
	testsupport.Must(t, err, "LinkProposalIssue: %v", err)

	err = UnlinkProposalIssue(db, pid, iid)
	testsupport.Must(t, err, "UnlinkProposalIssue: %v", err)

	ids, err := GetProposalIssues(db, pid)
	testsupport.Must(t, err, "GetProposalIssues: %v", err)
	if len(ids) != 0 {
		t.Errorf("expected 0 linked issues after unlink, got %d", len(ids))
	}
}

func TestUnlinkProposalIssueNotFound(t *testing.T) {
	db := mustInitAndMigrate(t)

	err := UnlinkProposalIssue(db, 999, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for unlink non-existent, got %v", err)
	}
}

// --- Gap 4: Proposal new fields roundtrip through DB ---

func TestCreateAndGetProposalWithV3Fields(t *testing.T) {
	db := mustInitAndMigrate(t)

	escalation := "Security review required"
	p := &model.Proposal{
		Description:      "V3 proposal test",
		Rationale:        "Schema gaps identified in v2",
		DomainTags:       []string{"architecture", "security"},
		FilesChanged:     []string{"internal/db/schema.go", "internal/model/proposal.go"},
		Criticality:      model.CriticalityHigh,
		Status:           model.ProposalStatusOpen,
		FinalOutcome:     "Pending",
		EscalationReason: &escalation,
		RequiredVoters:   3,
		Threshold:        0.67,
		CreatedBy:        "test-user",
	}

	id, err := CreateProposal(db, p)
	testsupport.Must(t, err, "CreateProposal: %v", err)

	got, err := GetProposal(db, id)
	testsupport.Must(t, err, "GetProposal: %v", err)

	if got.Rationale != "Schema gaps identified in v2" {
		t.Errorf("Rationale = %q, want %q", got.Rationale, "Schema gaps identified in v2")
	}
	if len(got.DomainTags) != 2 || got.DomainTags[0] != "architecture" || got.DomainTags[1] != "security" {
		t.Errorf("DomainTags = %v, want [architecture security]", got.DomainTags)
	}
	if len(got.FilesChanged) != 2 || got.FilesChanged[0] != "internal/db/schema.go" {
		t.Errorf("FilesChanged = %v", got.FilesChanged)
	}
	if got.FinalOutcome != "Pending" {
		t.Errorf("FinalOutcome = %q, want %q", got.FinalOutcome, "Pending")
	}
	if got.EscalationReason == nil || *got.EscalationReason != "Security review required" {
		t.Errorf("EscalationReason = %v", got.EscalationReason)
	}
}

// --- Gap 6: CommitProposal happy path and error cases ---

func TestCommitProposalHappyPath(t *testing.T) {
	db := mustInitAndMigrate(t)

	// Create and approve a proposal via votes.
	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Commit test",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	// Single approve vote to finalize as approved.
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	// Commit the approved proposal.
	outcome := "Changes applied to main branch."
	err = CommitProposal(db, id, outcome, "")
	testsupport.Must(t, err, "CommitProposal: %v", err)

	// Verify persisted state.
	p, err := GetProposal(db, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if p.Status != model.ProposalStatusCommitted {
		t.Errorf("Status = %q, want 'committed'", p.Status)
	}
	if p.FinalOutcome != outcome {
		t.Errorf("FinalOutcome = %q, want %q", p.FinalOutcome, outcome)
	}
}

func TestCommitProposalNotFound(t *testing.T) {
	db := mustInitAndMigrate(t)

	err := CommitProposal(db, 999, "outcome", "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestCommitProposalOpenRejected(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, _ := CreateProposal(db, &model.Proposal{
		Description:    "Open proposal",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
	})

	err := CommitProposal(db, id, "outcome", "")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict for open proposal, got %v", err)
	}
}

func TestCommitProposalRejectedRejected(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, _ := CreateProposal(db, &model.Proposal{
		Description:    "Rejected proposal",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})

	// Cast a reject vote to finalize as rejected.
	_, err := CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictReject, Confidence: 0.9, DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	err = CommitProposal(db, id, "outcome", "")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict for rejected proposal, got %v", err)
	}
}

func TestCommitProposalAlreadyCommitted(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, _ := CreateProposal(db, &model.Proposal{
		Description:    "Double commit",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})

	_, err := CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	err = CommitProposal(db, id, "first commit", "")
	testsupport.Must(t, err, "CommitProposal: %v", err)

	// Second commit should fail.
	err = CommitProposal(db, id, "second commit", "")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict for already committed, got %v", err)
	}
}

func TestCommitProposalWithEscalationReason(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Escalation commit test",
		Criticality:    model.CriticalityHigh,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	reason := "Quorum not reached after 3 rounds"
	err = CommitProposal(db, id, "Committed with escalation", reason)
	testsupport.Must(t, err, "CommitProposal: %v", err)

	p, err := GetProposal(db, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if p.EscalationReason == nil || *p.EscalationReason != reason {
		t.Errorf("EscalationReason = %v, want %q", p.EscalationReason, reason)
	}
	if p.FinalOutcome != "Committed with escalation" {
		t.Errorf("FinalOutcome = %q, want %q", p.FinalOutcome, "Committed with escalation")
	}
}

func TestCommitProposalPreservesExistingEscalationReason(t *testing.T) {
	db := mustInitAndMigrate(t)

	original := "Set at creation time"
	id, err := CreateProposal(db, &model.Proposal{
		Description:      "Preserve escalation test",
		Criticality:      model.CriticalityHigh,
		Status:           model.ProposalStatusOpen,
		RequiredVoters:   1,
		Threshold:        0.67,
		EscalationReason: &original,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	// Commit with empty escalation reason — should preserve the original.
	err = CommitProposal(db, id, "Done", "")
	testsupport.Must(t, err, "CommitProposal: %v", err)

	p, err := GetProposal(db, id)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if p.EscalationReason == nil || *p.EscalationReason != original {
		t.Errorf("EscalationReason = %v, want %q (preserved from create)", p.EscalationReason, original)
	}
}

// --- Gap 7: approve-with-concerns quorum math (weight = 1.0) ---

func TestCastVoteApproveWithConcernsQuorumMath(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Approve with concerns quorum",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 2,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	// Vote 1: approve-with-concerns, conf=0.8, rel=0.9 -> weight=0.72, verdict_weight=1.0
	_, err = CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-1",
		VoterRole:       "architecture",
		Verdict:         model.VerdictApproveWithConcerns,
		Confidence:      0.8,
		DomainRelevance: 0.9,
		FindingsJSON: &model.Findings{
			Blockers:    []string{},
			Concerns:    []string{"hardcoded paths"},
			Suggestions: []string{},
		},
		Summary: "Sound with concerns",
	})
	testsupport.Must(t, err, "CastVote 1: %v", err)

	// Vote 2: approve, conf=0.9, rel=1.0 -> weight=0.9, verdict_weight=1.0
	result, err := CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-2",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 1.0,
	})
	testsupport.Must(t, err, "CastVote 2: %v", err)

	if !result.QuorumReached {
		t.Error("expected quorum_reached = true")
	}
	// Both votes are approvals (approve-with-concerns treated as 1.0).
	// score = (0.72 + 0.9) / (0.72 + 0.9) = 1.0
	if result.WeightedScore == nil || *result.WeightedScore != 1.0 {
		t.Errorf("weighted_score = %v, want 1.0", result.WeightedScore)
	}
	if result.ProposalStatus != model.ProposalStatusApproved {
		t.Errorf("status = %q, want 'approved'", result.ProposalStatus)
	}
}

// --- Gap 8: findings_json round-trip through CastVote / GetProposalVotes ---

func TestFindingsJSONRoundTripThroughDB(t *testing.T) {
	db := mustInitAndMigrate(t)

	id, err := CreateProposal(db, &model.Proposal{
		Description:    "Findings JSON roundtrip",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 3,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	// Vote with structured findings.
	findings := &model.Findings{
		Blockers:    []string{"critical issue"},
		Concerns:    []string{"concern A", "concern B"},
		Suggestions: []string{"suggestion 1"},
	}
	_, err = CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-with-findings",
		VoterRole:       "security",
		Verdict:         model.VerdictApproveWithConcerns,
		Confidence:      0.85,
		DomainRelevance: 0.9,
		Findings:        "Plain text findings",
		FindingsJSON:    findings,
		Summary:         "Has concerns but approves",
	})
	testsupport.Must(t, err, "CastVote with findings: %v", err)

	// Vote without structured findings.
	_, err = CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-no-findings",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Findings:        "Just text",
	})
	testsupport.Must(t, err, "CastVote without findings: %v", err)

	votes, err := GetProposalVotes(db, id)
	testsupport.Must(t, err, "GetProposalVotes: %v", err)
	if len(votes) != 2 {
		t.Fatalf("expected 2 votes, got %d", len(votes))
	}

	// First vote should have structured findings.
	v1 := votes[0]
	if v1.FindingsJSON == nil {
		t.Fatal("vote 1 FindingsJSON is nil")
	}
	if len(v1.FindingsJSON.Blockers) != 1 || v1.FindingsJSON.Blockers[0] != "critical issue" {
		t.Errorf("vote 1 Blockers = %v", v1.FindingsJSON.Blockers)
	}
	if len(v1.FindingsJSON.Concerns) != 2 {
		t.Errorf("vote 1 Concerns = %v, want 2 items", v1.FindingsJSON.Concerns)
	}
	if len(v1.FindingsJSON.Suggestions) != 1 {
		t.Errorf("vote 1 Suggestions = %v", v1.FindingsJSON.Suggestions)
	}
	if v1.Summary != "Has concerns but approves" {
		t.Errorf("vote 1 Summary = %q", v1.Summary)
	}
	if v1.Findings != "Plain text findings" {
		t.Errorf("vote 1 Findings = %q", v1.Findings)
	}

	// Second vote should have nil findings_json.
	v2 := votes[1]
	if v2.FindingsJSON != nil {
		t.Errorf("vote 2 FindingsJSON should be nil, got %v", v2.FindingsJSON)
	}
	if v2.Findings != "Just text" {
		t.Errorf("vote 2 Findings = %q", v2.Findings)
	}
}

// --- Gap 9: domainTag filtering via json_each() ---

func TestListProposalsDomainTagFilter(t *testing.T) {
	db := mustInitAndMigrate(t)

	// Create proposals with different domain tags.
	_, err := CreateProposal(db, &model.Proposal{
		Description:    "Architecture proposal",
		DomainTags:     []string{"architecture", "security"},
		Criticality:    model.CriticalityHigh,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal 1: %v", err)

	_, err = CreateProposal(db, &model.Proposal{
		Description:    "Security-only proposal",
		DomainTags:     []string{"security"},
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal 2: %v", err)

	_, err = CreateProposal(db, &model.Proposal{
		Description:    "No tags proposal",
		DomainTags:     []string{},
		Criticality:    model.CriticalityLow,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal 3: %v", err)

	// Filter by "security" - should match 2 proposals.
	list, total, err := ListProposals(db, 0, "", "", "security", 0)
	testsupport.Must(t, err, "ListProposals(security): %v", err)
	if total != 2 {
		t.Errorf("total for 'security' = %d, want 2", total)
	}
	if len(list) != 2 {
		t.Errorf("len for 'security' = %d, want 2", len(list))
	}

	// Filter by "architecture" - should match 1 proposal.
	list, total, err = ListProposals(db, 0, "", "", "architecture", 0)
	testsupport.Must(t, err, "ListProposals(architecture): %v", err)
	if total != 1 {
		t.Errorf("total for 'architecture' = %d, want 1", total)
	}
	if len(list) != 1 || list[0].Description != "Architecture proposal" {
		t.Errorf("list for 'architecture' = %+v, want [Architecture proposal]", list)
	}

	// Filter by nonexistent tag - should match 0.
	list, total, err = ListProposals(db, 0, "", "", "nonexistent", 0)
	testsupport.Must(t, err, "ListProposals(nonexistent): %v", err)
	if total != 0 {
		t.Errorf("total for 'nonexistent' = %d, want 0", total)
	}
	if len(list) != 0 {
		t.Errorf("list for 'nonexistent' = %+v, want empty", list)
	}

	// Verify exact match (not substring): "api-security" should NOT match "security".
	_, err = CreateProposal(db, &model.Proposal{
		Description:    "API security proposal",
		DomainTags:     []string{"api-security"},
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	testsupport.Must(t, err, "CreateProposal 4: %v", err)

	list, total, err = ListProposals(db, 0, "", "", "security", 0)
	testsupport.Must(t, err, "ListProposals(security) after api-security: %v", err)
	// Should still be 2 -- json_each() does exact match, not substring.
	if total != 2 {
		t.Errorf("total for 'security' after api-security = %d, want 2 (exact match)", total)
	}
	// Pair the exclusion with a cardinality check: an exclusion-only loop
	// passes vacuously if the query starts returning zero rows, which is
	// exactly the list/total divergence this test exists to catch.
	if len(list) != 2 {
		t.Errorf("len for 'security' after api-security = %d, want 2", len(list))
	}
	for _, p := range list {
		if p.Description == "API security proposal" {
			t.Errorf("list for 'security' after api-security includes %q, want exact-match only", p.Description)
		}
	}
}

// --- Gap 10: v2->v3 migration specifically ---

func TestMigrateV2ToV3Columns(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	// Apply only v1->v2 migration first.
	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	err = migrateV1ToV2(tx)
	testsupport.Must(t, err, "migrateV1ToV2: %v", err)
	_, err = tx.Exec(`UPDATE meta SET value = '2' WHERE key = 'schema_version'`)
	testsupport.Must(t, err, "updating version to 2: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	// Verify we're at v2.
	v, err := SchemaVersion(db)
	testsupport.Must(t, err, "SchemaVersion: %v", err)
	if v != 2 {
		t.Fatalf("schema_version = %d, want 2", v)
	}

	// Insert a v2-style proposal (no v3 columns).
	now := "2026-03-20T10:00:00Z"
	_, err = db.Exec(
		`INSERT INTO proposals (description, criticality, status, required_voters, threshold, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"Pre-migration proposal", "medium", "open", 3, 0.67, "test", now, now,
	)
	testsupport.Must(t, err, "Insert v2 proposal: %v", err)

	// Insert a v2-style vote.
	_, err = db.Exec(
		`INSERT INTO votes (proposal_id, voter_name, voter_role, verdict, confidence, domain_relevance, findings, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		1, "voter-1", "security", "approve", 0.9, 0.8, "Looks good", now,
	)
	testsupport.Must(t, err, "Insert v2 vote: %v", err)

	// Now run Migrate() which should apply v2->v3.
	err = Migrate(db)
	testsupport.Must(t, err, "Migrate v2->v3: %v", err)

	// Verify version is now at head (Migrate advances v2 to the current
	// schema version, applying v3 and any later migrations in sequence).
	v, err = SchemaVersion(db)
	testsupport.Must(t, err, "SchemaVersion after migration: %v", err)
	if v != currentSchemaVersion {
		t.Fatalf("schema_version = %d after migration, want %d", v, currentSchemaVersion)
	}

	// Verify existing proposal has correct defaults for new columns.
	p, err := GetProposal(db, 1)
	testsupport.Must(t, err, "GetProposal after migration: %v", err)
	if p.Rationale != "" {
		t.Errorf("Rationale = %q, want empty default", p.Rationale)
	}
	if len(p.DomainTags) != 0 {
		t.Errorf("DomainTags = %v, want empty array default", p.DomainTags)
	}
	if len(p.FilesChanged) != 0 {
		t.Errorf("FilesChanged = %v, want empty array default", p.FilesChanged)
	}
	if p.FinalOutcome != "" {
		t.Errorf("FinalOutcome = %q, want empty default", p.FinalOutcome)
	}
	if p.EscalationReason != nil {
		t.Errorf("EscalationReason = %v, want nil default", p.EscalationReason)
	}

	// Verify existing vote has correct defaults for new columns.
	votes, err := GetProposalVotes(db, 1)
	testsupport.Must(t, err, "GetProposalVotes after migration: %v", err)
	if len(votes) != 1 {
		t.Fatalf("expected 1 vote, got %d", len(votes))
	}
	if votes[0].FindingsJSON != nil {
		t.Errorf("FindingsJSON = %v, want nil default", votes[0].FindingsJSON)
	}
	if votes[0].Summary != "" {
		t.Errorf("Summary = %q, want empty default", votes[0].Summary)
	}
	// Verify existing data survived.
	if votes[0].Findings != "Looks good" {
		t.Errorf("Findings = %q, want 'Looks good'", votes[0].Findings)
	}
	if p.Description != "Pre-migration proposal" {
		t.Errorf("Description = %q, want 'Pre-migration proposal'", p.Description)
	}
}

// --- GetIssueProposals (reverse edge of GetProposalIssues) ---

func TestGetIssueProposalsZero(t *testing.T) {
	db := mustInitAndMigrate(t)
	iid := createTestIssueForProposal(t, db, "no-proposals")

	proposals, err := GetIssueProposals(db, iid)
	testsupport.Must(t, err, "GetIssueProposals: %v", err)
	if len(proposals) != 0 {
		t.Errorf("len(proposals) = %d, want 0", len(proposals))
	}
}

func TestGetIssueProposalsOne(t *testing.T) {
	db := mustInitAndMigrate(t)
	iid := createTestIssueForProposal(t, db, "one-proposal")

	pid, err := CreateProposal(db, &model.Proposal{
		Description: "Solo proposal", Criticality: model.CriticalityMedium,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.67,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	err = LinkProposalIssue(db, pid, iid)
	testsupport.Must(t, err, "LinkProposalIssue: %v", err)

	proposals, err := GetIssueProposals(db, iid)
	testsupport.Must(t, err, "GetIssueProposals: %v", err)
	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1", len(proposals))
	}
	if proposals[0].ID != pid {
		t.Errorf("proposals[0].ID = %d, want %d", proposals[0].ID, pid)
	}
	if proposals[0].Description != "Solo proposal" {
		t.Errorf("proposals[0].Description = %q, want 'Solo proposal'", proposals[0].Description)
	}
	if proposals[0].Status != model.ProposalStatusOpen {
		t.Errorf("proposals[0].Status = %q, want %q", proposals[0].Status, model.ProposalStatusOpen)
	}
}

func TestGetIssueProposalsManyMixedStatusDeterministicOrder(t *testing.T) {
	db := mustInitAndMigrate(t)
	iid := createTestIssueForProposal(t, db, "many-proposals")
	other := createTestIssueForProposal(t, db, "unrelated-issue")

	statuses := []model.ProposalStatus{
		model.ProposalStatusOpen,
		model.ProposalStatusApproved,
		model.ProposalStatusRejected,
		model.ProposalStatusCommitted,
	}
	var want []int
	for i, st := range statuses {
		pid, err := CreateProposal(db, &model.Proposal{
			Description: "Proposal " + string(rune('A'+i)), Criticality: model.CriticalityMedium,
			Status: st, RequiredVoters: 1, Threshold: 0.67,
		})
		testsupport.Must(t, err, "CreateProposal %d: %v", i, err)
		want = append(want, pid)
	}

	for i := len(want) - 1; i >= 0; i-- {
		err := LinkProposalIssue(db, want[i], iid)
		testsupport.Must(t, err, "LinkProposalIssue %d: %v", want[i], err)
	}

	otherIssuePID, err := CreateProposal(db, &model.Proposal{
		Description: "Other", Criticality: model.CriticalityMedium,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.67,
	})
	testsupport.Must(t, err, "CreateProposal other: %v", err)
	err = LinkProposalIssue(db, otherIssuePID, other)
	testsupport.Must(t, err, "LinkProposalIssue other: %v", err)

	proposals, err := GetIssueProposals(db, iid)
	testsupport.Must(t, err, "GetIssueProposals: %v", err)
	for _, p := range proposals {
		if p.ID == otherIssuePID {
			t.Errorf("proposal linked only to another issue must not appear in results, got %v", p.ID)
		}
	}
	if len(proposals) != len(want) {
		t.Fatalf("len(proposals) = %d, want %d", len(proposals), len(want))
	}
	for i, p := range proposals {
		if p.ID != want[i] {
			t.Errorf("proposals[%d].ID = %d, want %d (results must be sorted by query, not insertion order)", i, p.ID, want[i])
		}
		if p.Status != statuses[i] {
			t.Errorf("proposals[%d].Status = %q, want %q", i, p.Status, statuses[i])
		}
	}
}

// --- Vote metadata (the casting seat's own opaque claim) ---

// TestCastVoteRecordsMetadata pins the round trip DKT-71 asks for: the bag a
// seat asserts about itself survives CastVote and reads back WHOLE through
// GetProposalVotes — nested values and numbers included, since map to JSON to
// map is where fidelity is actually at risk — while a seat that asserted
// nothing reads back as an absent bag rather than an empty object or a copy
// of its neighbour's.
func TestCastVoteRecordsMetadata(t *testing.T) {
	conn := mustInitAndMigrate(t)

	proposalID, err := CreateProposal(conn, &model.Proposal{
		Description:    "cast metadata regression",
		RequiredVoters: 2,
		Threshold:      0.5,
		Status:         model.ProposalStatusOpen,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	claimed := map[string]any{
		"resolved":  map[string]any{"engine": "sonnet-5", "effort": "high"},
		"attempt":   float64(2),
		"delegated": true,
	}
	_, err = CastVote(conn, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "judge-security",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Metadata:        claimed,
	})
	testsupport.Must(t, err, "casting vote with metadata: %v", err)

	_, err = CastVote(conn, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "judge-correctness",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
	})
	testsupport.Must(t, err, "casting vote without metadata: %v", err)

	byVoter := votesByVoter(t, conn, proposalID)

	got := mustVote(t, byVoter, "judge-security").Metadata
	if !reflect.DeepEqual(got, claimed) {
		t.Errorf("judge-security metadata = %#v, want %#v", got, claimed)
	}
	if unclaimed := mustVote(t, byVoter, "judge-correctness").Metadata; unclaimed != nil {
		t.Errorf("judge-correctness metadata = %#v, want nil (asserted nothing)", unclaimed)
	}
}

// TestCastVoteMetadataDoesNotAffectTally is the control that the bag is inert:
// nothing in the tally may read it. Two otherwise-identical two-seat proposals
// — one where every vote carries a claim, one where none does — reach the same
// outcome AND the outcome each is expected to reach, so a tally that silently
// stopped running cannot pass as invariance.
func TestCastVoteMetadataDoesNotAffectTally(t *testing.T) {
	conn := mustInitAndMigrate(t)

	run := func(withMetadata bool) (model.ProposalStatus, float64) {
		t.Helper()
		proposalID, err := CreateProposal(conn, &model.Proposal{
			Description:    fmt.Sprintf("tally regression, metadata=%v", withMetadata),
			RequiredVoters: 2,
			Threshold:      0.5,
			Status:         model.ProposalStatusOpen,
		})
		testsupport.Must(t, err, "CreateProposal: %v", err)

		for i, voter := range []string{"seat-a", "seat-b"} {
			v := &model.Vote{
				ProposalID:      proposalID,
				VoterName:       voter,
				Verdict:         model.VerdictApprove,
				Confidence:      0.9,
				DomainRelevance: 0.8,
			}
			if withMetadata {
				v.Metadata = map[string]any{"seat": fmt.Sprintf("s-%d", i)}
			}
			_, err := CastVote(conn, v)
			testsupport.Must(t, err, "CastVote: %v", err)
		}

		p, err := GetProposal(conn, proposalID)
		testsupport.Must(t, err, "GetProposal: %v", err)
		if p.WeightedScore == nil {
			t.Fatalf("proposal has no weighted score after quorum (metadata=%v) — "+
				"the tally never ran, so this test would prove nothing", withMetadata)
		}
		return p.Status, *p.WeightedScore
	}

	statusPlain, scorePlain := run(false)
	statusMeta, scoreMeta := run(true)

	// The value both branches must agree ON, not merely agree about: two
	// unanimous approvals over a 0.5 threshold.
	if statusPlain != model.ProposalStatusApproved || scorePlain != 1.0 {
		t.Fatalf("plain proposal = (%s, %v), want (approved, 1) — the fixture no "+
			"longer reaches the outcome this test compares against",
			statusPlain, scorePlain)
	}
	if statusPlain != statusMeta || scorePlain != scoreMeta {
		t.Errorf("tally diverged by metadata presence: plain=(%s,%v) metadata=(%s,%v)",
			statusPlain, scorePlain, statusMeta, scoreMeta)
	}
}

// TestVoteMetadataReadMarksAnUndecodableCell pins BOTH halves of the read-side
// rule the column's opacity requires. A cell that does not decode never fails
// the read — one odd cell must not break `vote show`, `vote list` and
// `vote result` for the whole proposal — but it does not read as a seat that
// claimed nothing either: unreadable is its own state, so a corrupted or
// rewritten claim is visible to whoever reads the vote back.
func TestVoteMetadataReadMarksAnUndecodableCell(t *testing.T) {
	conn := mustInitAndMigrate(t)

	proposalID, err := CreateProposal(conn, &model.Proposal{
		Description:    "tolerant read",
		RequiredVoters: 4,
		Threshold:      0.5,
		Status:         model.ProposalStatusOpen,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	for _, voter := range []string{"seat-empty", "seat-garbage", "seat-silent"} {
		_, err := CastVote(conn, &model.Vote{
			ProposalID:      proposalID,
			VoterName:       voter,
			Verdict:         model.VerdictApprove,
			Confidence:      0.9,
			DomainRelevance: 0.8,
			Findings:        "kept",
		})
		testsupport.Must(t, err, "CastVote: %v", err)
	}

	// Cells no writer of ours would produce: a store predating the write-side
	// gate, or bytes some other tool put there. seat-silent's cell is left
	// exactly as CastVote wrote it — NULL — and is the control that separates
	// "claimed nothing" from "claim unreadable".
	mustExec(t, conn, `UPDATE votes SET metadata = '' WHERE voter_name = 'seat-empty'`)
	mustExec(t, conn, `UPDATE votes SET metadata = 'not json' WHERE voter_name = 'seat-garbage'`)

	byVoter := votesByVoter(t, conn, proposalID)
	for _, voter := range []string{"seat-empty", "seat-garbage"} {
		v := mustVote(t, byVoter, voter)
		if v.Metadata != nil {
			t.Errorf("%s metadata = %#v, want nil (undecodable)", voter, v.Metadata)
		}
		if !v.MetadataUnreadable {
			t.Errorf("%s reads as a seat that claimed nothing; an undecodable cell "+
				"must be distinguishable from an absent one", voter)
		}
		if v.Findings != "kept" {
			t.Errorf("%s findings = %q, want %q — the rest of the row must still read",
				voter, v.Findings, "kept")
		}
	}

	silent := mustVote(t, byVoter, "seat-silent")
	if silent.Metadata != nil || silent.MetadataUnreadable {
		t.Errorf("seat-silent = (%#v, unreadable=%v), want (nil, false) — a seat that "+
			"claimed nothing must not be reported as damaged",
			silent.Metadata, silent.MetadataUnreadable)
	}
}

// TestVoteMetadataStoresNullForAnEmptyBag asserts the persisted form directly,
// in SQL, rather than through the decoder that maps SQL NULL and a stored
// `null` onto the same Go nil. The invariant is that a vote nobody enriched is
// byte-identical in the column to a vote written before the column existed;
// only typeof() can see the difference the read path erases.
func TestVoteMetadataStoresNullForAnEmptyBag(t *testing.T) {
	conn := mustInitAndMigrate(t)

	proposalID, err := CreateProposal(conn, &model.Proposal{
		Description:    "persisted form",
		RequiredVoters: 4,
		Threshold:      0.5,
		Status:         model.ProposalStatusOpen,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	bags := map[string]map[string]any{
		"seat-silent": nil,
		"seat-empty":  {},
		"seat-claim":  {"resolved": "yes"},
	}
	for _, voter := range []string{"seat-silent", "seat-empty", "seat-claim"} {
		_, err := CastVote(conn, &model.Vote{
			ProposalID:      proposalID,
			VoterName:       voter,
			Verdict:         model.VerdictApprove,
			Confidence:      0.9,
			DomainRelevance: 0.8,
			Metadata:        bags[voter],
		})
		testsupport.Must(t, err, "CastVote: %v", err)
	}

	wantType := map[string]string{
		"seat-silent": "null",
		"seat-empty":  "null",
		"seat-claim":  "text",
	}
	for voter, want := range wantType {
		var got string
		err := conn.QueryRow(
			`SELECT typeof(metadata) FROM votes WHERE proposal_id = ? AND voter_name = ?`,
			proposalID, voter).Scan(&got)
		testsupport.Must(t, err, "reading typeof(metadata) for %s: %v", voter, err)
		if got != want {
			t.Errorf("%s stored typeof(metadata) = %q, want %q", voter, got, want)
		}
	}
}

// TestVoteMetadataCapAppliesToEveryWriter puts the size limit at the column
// rather than at one command's flag: the import path reaches votes through
// InsertVoteWithID and is held to the same refusal `vote cast` gets.
func TestVoteMetadataCapAppliesToEveryWriter(t *testing.T) {
	conn := mustInitAndMigrate(t)

	proposalID, err := CreateProposal(conn, &model.Proposal{
		Description:    "cap at the seam",
		RequiredVoters: 2,
		Threshold:      0.5,
		Status:         model.ProposalStatusOpen,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	oversized := blobBagOfEncodedSize(t, VoteMetadataMaxBytes+1)

	if _, err := CastVote(conn, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "seat-a",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Metadata:        oversized,
	}); err == nil {
		t.Error("CastVote accepted an over-cap metadata bag")
	}

	// The refusal must leave NOTHING behind. The marshal runs before the INSERT
	// today, so this holds — but it holds positionally, and a marshal moved
	// below the INSERT would leave a half-written vote with every other
	// assertion in this file still green.
	if got := votesByVoter(t, conn, proposalID); len(got) != 0 {
		t.Errorf("a refused cast left %d vote(s) in the store: %v", len(got), got)
	}

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer func() { _ = tx.Rollback() }()

	if _, err := InsertVoteWithID(tx, &model.Vote{
		ID:              1,
		ProposalID:      proposalID,
		VoterName:       "seat-imported",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Metadata:        oversized,
		CreatedAt:       time.Now(),
	}); err == nil {
		t.Error("InsertVoteWithID accepted an over-cap metadata bag — the import " +
			"path writes an unbounded column")
	}

	var inserted int
	err = tx.QueryRow(
		`SELECT COUNT(*) FROM votes WHERE voter_name = 'seat-imported'`).Scan(&inserted)
	testsupport.Must(t, err, "counting imported votes: %v", err)
	if inserted != 0 {
		t.Errorf("a refused import wrote %d vote row(s)", inserted)
	}
}

// TestVoteMetadataCapIsExactAtTheBoundary pins the constant's magnitude from
// the ACCEPTING side as well as the refusing one. Without an at-cap accept
// nothing distinguishes `>` from `>=`, and a cap silently one byte tighter
// than it says would pass every other test in this file.
func TestVoteMetadataCapIsExactAtTheBoundary(t *testing.T) {
	conn := mustInitAndMigrate(t)

	proposalID, err := CreateProposal(conn, &model.Proposal{
		Description:    "cap boundary",
		RequiredVoters: 3,
		Threshold:      0.5,
		Status:         model.ProposalStatusOpen,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)

	atCap := blobBagOfEncodedSize(t, VoteMetadataMaxBytes)
	if _, err := CastVote(conn, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "seat-at-cap",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Metadata:        atCap,
	}); err != nil {
		t.Errorf("CastVote refused a bag of exactly %d bytes: %v", VoteMetadataMaxBytes, err)
	}

	overCap := blobBagOfEncodedSize(t, VoteMetadataMaxBytes+1)
	if _, err := CastVote(conn, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "seat-over-cap",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Metadata:        overCap,
	}); err == nil {
		t.Errorf("CastVote accepted a bag of %d bytes, one over the cap", VoteMetadataMaxBytes+1)
	}
}

// blobBagOfEncodedSize builds a one-key bag whose ENCODED size is exactly n
// bytes — the quantity the column measures. Sizing the filler by hand is how
// the previous fixture came to be eleven bytes over a cap it described as one
// over; the encoder is asked instead, and the result is verified.
func blobBagOfEncodedSize(t *testing.T, n int) map[string]any {
	t.Helper()
	const key = "blob"
	overhead := len(`{"":""}`) + len(key)
	if n < overhead {
		t.Fatalf("cannot build a %d-byte bag: the empty shape is already %d", n, overhead)
	}
	bag := map[string]any{key: strings.Repeat("x", n-overhead)}
	encoded, err := json.Marshal(bag)
	testsupport.Must(t, err, "encoding the fixture bag: %v", err)
	if len(encoded) != n {
		t.Fatalf("fixture encodes to %d bytes, want %d", len(encoded), n)
	}
	return bag
}

// TestVoteMetadataPathReadsNoKey is the mechanical form of the promise two
// accepted risks rest on: core stores the bag whole and reads no key out of
// it, so a seat's self-asserted claim can never reach a decision. Stated in
// prose it is a precondition nothing enforces; parsed out of the source it
// fails the build on the day someone special-cases a key name.
//
// It parses rather than greps, so a key hidden in a switch, a map literal, or
// a comparison is caught the same as one in an if. Its limit is the same as
// TestMetadataRollupReadsNoKey's, whose shape it borrows: it covers the
// functions named below, not every line that could ever touch a vote.
func TestVoteMetadataPathReadsNoKey(t *testing.T) {
	src, err := os.ReadFile("proposals.go")
	testsupport.Must(t, err, "reading the vote source: %v", err)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "proposals.go", src, 0)
	testsupport.Must(t, err, "parsing the vote source: %v", err)

	guarded := map[string]bool{
		"marshalVoteMetadata": true,
		"scanVoteFrom":        true,
	}

	var offenders []string
	seen := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || !guarded[fn.Name.Name] {
			return true
		}
		seen[fn.Name.Name] = true
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			lit, ok := inner.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			// SQL and error text are the two legitimate literal classes here:
			// one names columns this schema defines, the other is a sentence.
			// Neither can be a metadata key.
			value := lit.Value
			if strings.Contains(value, "SELECT") || strings.Contains(value, " ") {
				return true
			}
			offenders = append(offenders, fn.Name.Name+": "+value)
			return true
		})
		return false
	})

	for name := range guarded {
		if !seen[name] {
			t.Fatalf("%s is no longer in proposals.go; this check is silently "+
				"guarding nothing", name)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("the vote metadata path contains bare string literals %v; a key-name "+
			"special case here would make a seat's own claim load-bearing", offenders)
	}
}

// votesByVoter reads a proposal's votes keyed by voter name.
func votesByVoter(t *testing.T, conn *sql.DB, proposalID int) map[string]*model.Vote {
	t.Helper()
	votes, err := GetProposalVotes(conn, proposalID)
	testsupport.Must(t, err, "GetProposalVotes: %v", err)
	byVoter := make(map[string]*model.Vote, len(votes))
	for _, v := range votes {
		byVoter[v.VoterName] = v
	}
	return byVoter
}

// mustVote fails BY NAME when a voter is absent, rather than panicking on a
// nil dereference in the assertion that follows.
func mustVote(t *testing.T, byVoter map[string]*model.Vote, voter string) *model.Vote {
	t.Helper()
	v, ok := byVoter[voter]
	if !ok {
		t.Fatalf("no vote read back for %q (read %d votes)", voter, len(byVoter))
	}
	return v
}
