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

	// Schema should now be at v3.
	v, err = SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 3 {
		t.Fatalf("schema_version = %d after migration, want 3", v)
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
	list, total, err := ListProposals(db, "", "", "", 0)
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
	list, total, err = ListProposals(db, "open", "", "", 0)
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
	list, total, err = ListProposals(db, "", "high", "", 0)
	if err != nil {
		t.Fatalf("ListProposals (high): %v", err)
	}
	if total != 1 {
		t.Errorf("total high = %d, want 1", total)
	}

	// Limit.
	list, _, err = ListProposals(db, "", "", "", 1)
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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	got, err := GetProposal(db, id)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	// Single approve vote to finalize as approved.
	_, err = CastVote(db, &model.Vote{
		ProposalID: id, VoterName: "voter-1",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9,
	})
	if err != nil {
		t.Fatalf("CastVote: %v", err)
	}

	// Commit the approved proposal.
	outcome := "Changes applied to main branch."
	if err := CommitProposal(db, id, outcome, ""); err != nil {
		t.Fatalf("CommitProposal: %v", err)
	}

	// Verify persisted state.
	p, err := GetProposal(db, id)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
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
	if err != nil {
		t.Fatalf("CastVote: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CastVote: %v", err)
	}

	if err := CommitProposal(db, id, "first commit", ""); err != nil {
		t.Fatalf("CommitProposal: %v", err)
	}

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

	reason := "Quorum not reached after 3 rounds"
	if err := CommitProposal(db, id, "Committed with escalation", reason); err != nil {
		t.Fatalf("CommitProposal: %v", err)
	}

	p, err := GetProposal(db, id)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
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

	// Commit with empty escalation reason — should preserve the original.
	if err := CommitProposal(db, id, "Done", ""); err != nil {
		t.Fatalf("CommitProposal: %v", err)
	}

	p, err := GetProposal(db, id)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CastVote 1: %v", err)
	}

	// Vote 2: approve, conf=0.9, rel=1.0 -> weight=0.9, verdict_weight=1.0
	result, err := CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-2",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 1.0,
	})
	if err != nil {
		t.Fatalf("CastVote 2: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

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
	if err != nil {
		t.Fatalf("CastVote with findings: %v", err)
	}

	// Vote without structured findings.
	_, err = CastVote(db, &model.Vote{
		ProposalID:      id,
		VoterName:       "voter-no-findings",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Findings:        "Just text",
	})
	if err != nil {
		t.Fatalf("CastVote without findings: %v", err)
	}

	votes, err := GetProposalVotes(db, id)
	if err != nil {
		t.Fatalf("GetProposalVotes: %v", err)
	}
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
	if err != nil {
		t.Fatalf("CreateProposal 1: %v", err)
	}

	_, err = CreateProposal(db, &model.Proposal{
		Description:    "Security-only proposal",
		DomainTags:     []string{"security"},
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	if err != nil {
		t.Fatalf("CreateProposal 2: %v", err)
	}

	_, err = CreateProposal(db, &model.Proposal{
		Description:    "No tags proposal",
		DomainTags:     []string{},
		Criticality:    model.CriticalityLow,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 1,
		Threshold:      0.67,
	})
	if err != nil {
		t.Fatalf("CreateProposal 3: %v", err)
	}

	// Filter by "security" - should match 2 proposals.
	list, total, err := ListProposals(db, "", "", "security", 0)
	if err != nil {
		t.Fatalf("ListProposals(security): %v", err)
	}
	if total != 2 {
		t.Errorf("total for 'security' = %d, want 2", total)
	}
	if len(list) != 2 {
		t.Errorf("len for 'security' = %d, want 2", len(list))
	}

	// Filter by "architecture" - should match 1 proposal.
	list, total, err = ListProposals(db, "", "", "architecture", 0)
	if err != nil {
		t.Fatalf("ListProposals(architecture): %v", err)
	}
	if total != 1 {
		t.Errorf("total for 'architecture' = %d, want 1", total)
	}

	// Filter by nonexistent tag - should match 0.
	list, total, err = ListProposals(db, "", "", "nonexistent", 0)
	if err != nil {
		t.Fatalf("ListProposals(nonexistent): %v", err)
	}
	if total != 0 {
		t.Errorf("total for 'nonexistent' = %d, want 0", total)
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
	if err != nil {
		t.Fatalf("CreateProposal 4: %v", err)
	}

	list, total, err = ListProposals(db, "", "", "security", 0)
	if err != nil {
		t.Fatalf("ListProposals(security) after api-security: %v", err)
	}
	// Should still be 2 -- json_each() does exact match, not substring.
	if total != 2 {
		t.Errorf("total for 'security' after api-security = %d, want 2 (exact match)", total)
	}
}

// --- Gap 10: v2->v3 migration specifically ---

func TestMigrateV2ToV3Columns(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Apply only v1->v2 migration first.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := migrateV1ToV2(tx); err != nil {
		t.Fatalf("migrateV1ToV2: %v", err)
	}
	if _, err := tx.Exec(`UPDATE meta SET value = '2' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("updating version to 2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify we're at v2.
	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Insert v2 proposal: %v", err)
	}

	// Insert a v2-style vote.
	_, err = db.Exec(
		`INSERT INTO votes (proposal_id, voter_name, voter_role, verdict, confidence, domain_relevance, findings, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		1, "voter-1", "security", "approve", 0.9, 0.8, "Looks good", now,
	)
	if err != nil {
		t.Fatalf("Insert v2 vote: %v", err)
	}

	// Now run Migrate() which should apply v2->v3.
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v2->v3: %v", err)
	}

	// Verify version is now 3.
	v, err = SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion after migration: %v", err)
	}
	if v != 3 {
		t.Fatalf("schema_version = %d after migration, want 3", v)
	}

	// Verify existing proposal has correct defaults for new columns.
	p, err := GetProposal(db, 1)
	if err != nil {
		t.Fatalf("GetProposal after migration: %v", err)
	}
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
	if err != nil {
		t.Fatalf("GetProposalVotes after migration: %v", err)
	}
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
