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

func TestHydrateDocs_BatchNoNPlus1(t *testing.T) {
	db := mustInitAndMigrate(t)

	docA := mustCreateDoc(t, db, "Title A", "tdd", "approved", "body")
	docB := mustCreateDoc(t, db, "Title B", "adr", "draft", "body")
	docC := mustCreateDoc(t, db, "Title C", "ux", "draft", "body")

	id1 := createTestIssue(t, db, "i1", model.StatusTodo, model.PriorityMedium)
	id2 := createTestIssue(t, db, "i2", model.StatusTodo, model.PriorityMedium)
	id3 := createTestIssue(t, db, "i3", model.StatusTodo, model.PriorityMedium)

	if err := LinkDocIssue(db, docB, id1); err != nil {
		t.Fatalf("LinkDocIssue docB→id1: %v", err)
	}
	if err := LinkDocIssue(db, docA, id1); err != nil {
		t.Fatalf("LinkDocIssue docA→id1: %v", err)
	}
	if err := LinkDocIssue(db, docC, id1); err != nil {
		t.Fatalf("LinkDocIssue docC→id1: %v", err)
	}
	if err := LinkDocIssue(db, docB, id2); err != nil {
		t.Fatalf("LinkDocIssue docB→id2: %v", err)
	}

	issues := []*model.Issue{{ID: id1}, {ID: id2}, {ID: id3}}
	if err := HydrateDocs(db, issues); err != nil {
		t.Fatalf("HydrateDocs: %v", err)
	}

	if len(issues[0].Docs) != 3 {
		t.Fatalf("issue 1: expected 3 docs, got %d", len(issues[0].Docs))
	}
	if issues[0].Docs[0].ID != docA || issues[0].Docs[1].ID != docB || issues[0].Docs[2].ID != docC {
		t.Errorf("issue 1 docs not ordered by doc_id ASC: %+v", issues[0].Docs)
	}
	if issues[0].Docs[0].Type != "tdd" || issues[0].Docs[0].Status != "approved" || issues[0].Docs[0].Title != "Title A" {
		t.Errorf("issue 1 doc A projection wrong: %+v", issues[0].Docs[0])
	}

	if len(issues[1].Docs) != 1 {
		t.Fatalf("issue 2: expected 1 doc, got %d", len(issues[1].Docs))
	}
	if issues[1].Docs[0].ID != docB {
		t.Errorf("issue 2: expected doc %d, got %d", docB, issues[1].Docs[0].ID)
	}

	if len(issues[2].Docs) != 0 {
		t.Errorf("issue 3: expected 0 docs, got %d", len(issues[2].Docs))
	}
}

func TestHydrateDocs_Empty(t *testing.T) {
	db := mustInitAndMigrate(t)

	if err := HydrateDocs(db, nil); err != nil {
		t.Fatalf("HydrateDocs nil: %v", err)
	}
	if err := HydrateDocs(db, []*model.Issue{}); err != nil {
		t.Fatalf("HydrateDocs empty: %v", err)
	}
}

func TestHydrateLinkedIssues_BatchNoNPlus1(t *testing.T) {
	db := mustInitAndMigrate(t)

	docA := mustCreateDoc(t, db, "Doc A", "tdd", "draft", "body")
	docB := mustCreateDoc(t, db, "Doc B", "adr", "draft", "body")

	id1 := createTestIssue(t, db, "Alpha", model.StatusInProgress, model.PriorityMedium)
	id2 := createTestIssue(t, db, "Beta", model.StatusTodo, model.PriorityMedium)
	id3 := createTestIssue(t, db, "Gamma", model.StatusDone, model.PriorityMedium)

	if err := LinkDocIssue(db, docA, id2); err != nil {
		t.Fatalf("LinkDocIssue docA→id2: %v", err)
	}
	if err := LinkDocIssue(db, docA, id1); err != nil {
		t.Fatalf("LinkDocIssue docA→id1: %v", err)
	}
	if err := LinkDocIssue(db, docB, id3); err != nil {
		t.Fatalf("LinkDocIssue docB→id3: %v", err)
	}

	refs, err := HydrateLinkedIssues(db, []int{docA, docB})
	if err != nil {
		t.Fatalf("HydrateLinkedIssues: %v", err)
	}

	if len(refs[docA]) != 2 {
		t.Fatalf("docA: expected 2 linked issues, got %d", len(refs[docA]))
	}
	if refs[docA][0].ID != id1 || refs[docA][1].ID != id2 {
		t.Errorf("docA refs not ordered by issue_id ASC: %+v", refs[docA])
	}
	if refs[docA][0].Kind != "task" || refs[docA][0].Status != string(model.StatusInProgress) || refs[docA][0].Title != "Alpha" {
		t.Errorf("docA ref[0] projection wrong: %+v", refs[docA][0])
	}

	if len(refs[docB]) != 1 || refs[docB][0].ID != id3 {
		t.Errorf("docB: expected [%d], got %+v", id3, refs[docB])
	}
	if refs[docB][0].Status != string(model.StatusDone) || refs[docB][0].Title != "Gamma" {
		t.Errorf("docB ref projection wrong: %+v", refs[docB][0])
	}
}

func TestHydrateLinkedIssues_Empty(t *testing.T) {
	db := mustInitAndMigrate(t)

	refs, err := HydrateLinkedIssues(db, nil)
	if err != nil {
		t.Fatalf("HydrateLinkedIssues nil: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty map, got %+v", refs)
	}

	docID := mustCreateDoc(t, db, "Unlinked", "tdd", "draft", "b")
	refs, err = HydrateLinkedIssues(db, []int{docID})
	if err != nil {
		t.Fatalf("HydrateLinkedIssues unlinked: %v", err)
	}
	if len(refs[docID]) != 0 {
		t.Errorf("expected no refs for unlinked doc, got %+v", refs[docID])
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
