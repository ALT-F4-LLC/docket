package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// mustInitAndMigrate initializes a fresh in-memory DB and runs migrations.
func mustInitAndMigrate(t *testing.T) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

// --- Schema Migration ---

func TestMigrateV1ToV2CreatesProposalTables(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Before migration, schema is at v1.
	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 1 {
		t.Fatalf("schema_version = %d before migration, want 1", v)
	}

	// Run migration.
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Schema should now be at v2.
	v, err = SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 2 {
		t.Fatalf("schema_version = %d after migration, want 2", v)
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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	got, err := GetProposal(db, id)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}

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
		if err != nil {
			t.Fatalf("CreateProposal(%q): %v", pp.desc, err)
		}
	}

	// List all (no filters).
	list, total, err := ListProposals(db, "", "", 0)
	if err != nil {
		t.Fatalf("ListProposals (all): %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if len(list) != 3 {
		t.Errorf("len = %d, want 3", len(list))
	}

	// Filter by status.
	list, total, err = ListProposals(db, "open", "", 0)
	if err != nil {
		t.Fatalf("ListProposals (open): %v", err)
	}
	if total != 2 {
		t.Errorf("total open = %d, want 2", total)
	}
	if len(list) != 2 {
		t.Errorf("len open = %d, want 2", len(list))
	}

	// Filter by criticality.
	list, total, err = ListProposals(db, "", "high", 0)
	if err != nil {
		t.Fatalf("ListProposals (high): %v", err)
	}
	if total != 1 {
		t.Errorf("total high = %d, want 1", total)
	}

	// Limit.
	list, _, err = ListProposals(db, "", "", 1)
	if err != nil {
		t.Fatalf("ListProposals (limit 1): %v", err)
	}
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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	result, err := CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-1",
		VoterRole:       "security",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Findings:        "Looks good",
	})
	if err != nil {
		t.Fatalf("CastVote: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	// Vote 1: approve with high confidence.
	_, err = CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-1",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
	})
	if err != nil {
		t.Fatalf("CastVote 1: %v", err)
	}

	// Vote 2 (quorum): approve.
	result, err := CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-2",
		Verdict:         model.VerdictApprove,
		Confidence:      0.8,
		DomainRelevance: 0.9,
	})
	if err != nil {
		t.Fatalf("CastVote 2: %v", err)
	}

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
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	// Vote 1: reject.
	_, err = CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-1",
		Verdict:         model.VerdictReject,
		Confidence:      0.9,
		DomainRelevance: 0.9,
	})
	if err != nil {
		t.Fatalf("CastVote 1: %v", err)
	}

	// Vote 2: reject (quorum).
	result, err := CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-2",
		Verdict:         model.VerdictReject,
		Confidence:      0.8,
		DomainRelevance: 0.8,
	})
	if err != nil {
		t.Fatalf("CastVote 2: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	// Vote 1: approve, conf=0.9, rel=1.0 -> weight=0.9, weighted=0.9
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 1.0,
	})
	if err != nil {
		t.Fatalf("CastVote 1: %v", err)
	}

	// Vote 2: reject, conf=0.8, rel=0.5 -> weight=0.4, weighted=0.0
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-2",
		Verdict: model.VerdictReject, Confidence: 0.8, DomainRelevance: 0.5,
	})
	if err != nil {
		t.Fatalf("CastVote 2: %v", err)
	}

	// Vote 3: approve, conf=0.7, rel=0.6 -> weight=0.42, weighted=0.42
	result, err := CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-3",
		Verdict: model.VerdictApprove, Confidence: 0.7, DomainRelevance: 0.6,
	})
	if err != nil {
		t.Fatalf("CastVote 3: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	// Both voters have 0 confidence or 0 domain_relevance.
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.0, DomainRelevance: 0.5,
	})
	if err != nil {
		t.Fatalf("CastVote 1: %v", err)
	}

	result, err := CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-2",
		Verdict: model.VerdictApprove, Confidence: 0.5, DomainRelevance: 0.0,
	})
	if err != nil {
		t.Fatalf("CastVote 2: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	if err != nil {
		t.Fatalf("CastVote: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	// Single vote finalizes.
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	if err != nil {
		t.Fatalf("CastVote: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	for _, name := range []string{"alice", "bob"} {
		_, err := CastVote(db, &model.Vote{
			ProposalID: id, VoterName: name,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
			Findings: "ok from " + name,
		})
		if err != nil {
			t.Fatalf("CastVote(%s): %v", name, err)
		}
	}

	votes, err := GetProposalVotes(db, id)
	if err != nil {
		t.Fatalf("GetProposalVotes: %v", err)
	}
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
	if err != nil {
		t.Fatalf("CreateIssue(%q): %v", title, err)
	}
	return id
}

func TestLinkAndGetProposalIssues(t *testing.T) {
	db := mustInitAndMigrate(t)

	pid, err := CreateProposal(db, &model.Proposal{
		Description: "Link test", Criticality: model.CriticalityMedium,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.67,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	iid1 := createTestIssueForProposal(t, db, "issue-1")
	iid2 := createTestIssueForProposal(t, db, "issue-2")

	if err := LinkProposalIssue(db, pid, iid1); err != nil {
		t.Fatalf("LinkProposalIssue 1: %v", err)
	}
	if err := LinkProposalIssue(db, pid, iid2); err != nil {
		t.Fatalf("LinkProposalIssue 2: %v", err)
	}

	ids, err := GetProposalIssues(db, pid)
	if err != nil {
		t.Fatalf("GetProposalIssues: %v", err)
	}
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

	if err := LinkProposalIssue(db, pid, iid); err != nil {
		t.Fatalf("LinkProposalIssue: %v", err)
	}

	err := LinkProposalIssue(db, pid, iid)
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

	if err := LinkProposalIssue(db, pid, iid); err != nil {
		t.Fatalf("LinkProposalIssue: %v", err)
	}

	if err := UnlinkProposalIssue(db, pid, iid); err != nil {
		t.Fatalf("UnlinkProposalIssue: %v", err)
	}

	ids, err := GetProposalIssues(db, pid)
	if err != nil {
		t.Fatalf("GetProposalIssues: %v", err)
	}
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
