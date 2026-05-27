package db

import (
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// --- Doc ↔ Issue links ---

func TestLinkDocIssue(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	issueID := createTestIssue(t, db, "i", model.StatusTodo, model.PriorityMedium)

	if err := LinkDocIssue(db, docID, issueID); err != nil {
		t.Fatalf("LinkDocIssue: %v", err)
	}

	ids, err := GetDocIssues(db, docID)
	if err != nil {
		t.Fatalf("GetDocIssues: %v", err)
	}
	if len(ids) != 1 || ids[0] != issueID {
		t.Errorf("GetDocIssues = %v, want [%d]", ids, issueID)
	}
}

func TestGetIssueDocs(t *testing.T) {
	db := mustInitAndMigrate(t)
	docA := mustCreateDoc(t, db, "a", "tdd", "draft", "b")
	docB := mustCreateDoc(t, db, "b", "tdd", "draft", "b")
	issueID := createTestIssue(t, db, "i", model.StatusTodo, model.PriorityMedium)

	if err := LinkDocIssue(db, docA, issueID); err != nil {
		t.Fatalf("LinkDocIssue A: %v", err)
	}
	if err := LinkDocIssue(db, docB, issueID); err != nil {
		t.Fatalf("LinkDocIssue B: %v", err)
	}

	ids, err := GetIssueDocs(db, issueID)
	if err != nil {
		t.Fatalf("GetIssueDocs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("len(ids) = %d, want 2", len(ids))
	}
	if ids[0] != docA || ids[1] != docB {
		t.Errorf("GetIssueDocs = %v, want [%d, %d]", ids, docA, docB)
	}
}

func TestLinkDocIssue_DuplicateReturnsErrConflict(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	issueID := createTestIssue(t, db, "i", model.StatusTodo, model.PriorityMedium)

	if err := LinkDocIssue(db, docID, issueID); err != nil {
		t.Fatalf("first LinkDocIssue: %v", err)
	}
	err := LinkDocIssue(db, docID, issueID)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("second LinkDocIssue err = %v, want ErrConflict", err)
	}
}

func TestLinkDocIssue_MissingDoc(t *testing.T) {
	db := mustInitAndMigrate(t)
	issueID := createTestIssue(t, db, "i", model.StatusTodo, model.PriorityMedium)
	err := LinkDocIssue(db, 999, issueID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLinkDocIssue_MissingIssue(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	err := LinkDocIssue(db, docID, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUnlinkDocIssue(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	issueID := createTestIssue(t, db, "i", model.StatusTodo, model.PriorityMedium)

	if err := LinkDocIssue(db, docID, issueID); err != nil {
		t.Fatalf("LinkDocIssue: %v", err)
	}
	if err := UnlinkDocIssue(db, docID, issueID); err != nil {
		t.Fatalf("UnlinkDocIssue: %v", err)
	}

	ids, _ := GetDocIssues(db, docID)
	if len(ids) != 0 {
		t.Errorf("after unlink, GetDocIssues = %v, want empty", ids)
	}

	err := UnlinkDocIssue(db, docID, issueID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("second UnlinkDocIssue err = %v, want ErrNotFound", err)
	}
}

// --- Doc ↔ Proposal links ---

func TestLinkDocProposal(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	pID, err := CreateProposal(db, &model.Proposal{
		Description: "p", Criticality: model.CriticalityHigh,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	if err := LinkProposalDoc(db, pID, docID); err != nil {
		t.Fatalf("LinkProposalDoc: %v", err)
	}

	ids, err := GetProposalDocs(db, pID)
	if err != nil {
		t.Fatalf("GetProposalDocs: %v", err)
	}
	if len(ids) != 1 || ids[0] != docID {
		t.Errorf("GetProposalDocs = %v, want [%d]", ids, docID)
	}

	docProps, err := GetDocProposals(db, docID)
	if err != nil {
		t.Fatalf("GetDocProposals: %v", err)
	}
	if len(docProps) != 1 || docProps[0] != pID {
		t.Errorf("GetDocProposals = %v, want [%d]", docProps, pID)
	}
}

func TestGetProposalDocs(t *testing.T) {
	db := mustInitAndMigrate(t)
	docA := mustCreateDoc(t, db, "a", "tdd", "draft", "b")
	docB := mustCreateDoc(t, db, "b", "tdd", "draft", "b")
	pID, _ := CreateProposal(db, &model.Proposal{
		Description: "p", Criticality: model.CriticalityHigh,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5,
	})

	if err := LinkProposalDoc(db, pID, docA); err != nil {
		t.Fatalf("LinkProposalDoc A: %v", err)
	}
	if err := LinkProposalDoc(db, pID, docB); err != nil {
		t.Fatalf("LinkProposalDoc B: %v", err)
	}

	ids, err := GetProposalDocs(db, pID)
	if err != nil {
		t.Fatalf("GetProposalDocs: %v", err)
	}
	if len(ids) != 2 || ids[0] != docA || ids[1] != docB {
		t.Errorf("GetProposalDocs = %v, want [%d, %d]", ids, docA, docB)
	}
}

func TestLinkProposalDoc_DuplicateReturnsErrConflict(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	pID, _ := CreateProposal(db, &model.Proposal{
		Description: "p", Criticality: model.CriticalityHigh,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5,
	})

	if err := LinkProposalDoc(db, pID, docID); err != nil {
		t.Fatalf("first LinkProposalDoc: %v", err)
	}
	err := LinkProposalDoc(db, pID, docID)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("second LinkProposalDoc err = %v, want ErrConflict", err)
	}
}

func TestLinkProposalDoc_MissingProposal(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	err := LinkProposalDoc(db, 999, docID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLinkProposalDoc_MissingDoc(t *testing.T) {
	db := mustInitAndMigrate(t)
	pID, _ := CreateProposal(db, &model.Proposal{
		Description: "p", Criticality: model.CriticalityHigh,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5,
	})
	err := LinkProposalDoc(db, pID, 999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUnlinkProposalDoc(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	pID, _ := CreateProposal(db, &model.Proposal{
		Description: "p", Criticality: model.CriticalityHigh,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5,
	})

	if err := LinkProposalDoc(db, pID, docID); err != nil {
		t.Fatalf("LinkProposalDoc: %v", err)
	}
	if err := UnlinkProposalDoc(db, pID, docID); err != nil {
		t.Fatalf("UnlinkProposalDoc: %v", err)
	}
	ids, _ := GetProposalDocs(db, pID)
	if len(ids) != 0 {
		t.Errorf("after unlink, GetProposalDocs = %v, want empty", ids)
	}

	err := UnlinkProposalDoc(db, pID, docID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("second UnlinkProposalDoc err = %v, want ErrNotFound", err)
	}
}

// --- Round-trip helpers ---

func TestInsertDocIssueLink_RoundTrip(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	issueID := createTestIssue(t, db, "i", model.StatusTodo, model.PriorityMedium)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	inserted, err := InsertDocIssueLink(tx, docID, issueID, "2026-05-26T16:00:00Z")
	if err != nil {
		t.Fatalf("InsertDocIssueLink: %v", err)
	}
	if !inserted {
		t.Error("inserted = false, want true")
	}

	inserted2, err := InsertDocIssueLink(tx, docID, issueID, "2026-05-26T16:00:00Z")
	if err != nil {
		t.Fatalf("InsertDocIssueLink 2: %v", err)
	}
	if inserted2 {
		t.Error("inserted2 = true, want false")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	ids, _ := GetDocIssues(db, docID)
	if len(ids) != 1 {
		t.Errorf("len(ids) = %d, want 1", len(ids))
	}
}

func TestInsertProposalDocLink_RoundTrip(t *testing.T) {
	db := mustInitAndMigrate(t)
	docID := mustCreateDoc(t, db, "d", "tdd", "draft", "b")
	pID, _ := CreateProposal(db, &model.Proposal{
		Description: "p", Criticality: model.CriticalityHigh,
		Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5,
	})

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	inserted, err := InsertProposalDocLink(tx, pID, docID, "2026-05-26T16:00:00Z")
	if err != nil {
		t.Fatalf("InsertProposalDocLink: %v", err)
	}
	if !inserted {
		t.Error("inserted = false, want true")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	ids, _ := GetProposalDocs(db, pID)
	if len(ids) != 1 || ids[0] != docID {
		t.Errorf("GetProposalDocs = %v, want [%d]", ids, docID)
	}
}

