package db

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// --- DB layer tests ---

func TestListAllIssues(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Create issues with various statuses including done.
	statuses := []model.Status{model.StatusBacklog, model.StatusTodo, model.StatusInProgress, model.StatusDone}
	for i, s := range statuses {
		issue := &model.Issue{
			Title:    "issue " + string(s),
			Status:   s,
			Priority: model.PriorityMedium,
			Kind:     model.IssueKindTask,
		}
		id, err := CreateIssue(db, issue, nil, nil)
		testsupport.Must(t, err, "CreateIssue %d: %v", i, err)
		if id <= 0 {
			t.Fatalf("expected positive id, got %d", id)
		}
	}

	issues, err := ListAllIssues(db, 0)
	testsupport.Must(t, err, "ListAllIssues: %v", err)
	if len(issues) != 4 {
		t.Errorf("expected 4 issues, got %d", len(issues))
	}

	// Verify done issue is included (ListAllIssues returns everything).
	var foundDone bool
	for _, iss := range issues {
		if iss.Status == model.StatusDone {
			foundDone = true
			break
		}
	}
	if !foundDone {
		t.Error("expected done issue to be included in ListAllIssues")
	}
}

func TestListAllComments(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Create two issues.
	id1, err := CreateIssue(db, &model.Issue{
		Title: "issue 1", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue 1: %v", err)
	id2, err := CreateIssue(db, &model.Issue{
		Title: "issue 2", Status: model.StatusTodo, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue 2: %v", err)

	// Create comments on both issues.
	for _, c := range []*model.Comment{
		{IssueID: id1, Body: "comment A", Author: "alice"},
		{IssueID: id2, Body: "comment B", Author: "bob"},
		{IssueID: id1, Body: "comment C", Author: "alice"},
	} {
		_, err := CreateComment(db, c)
		testsupport.Must(t, err, "CreateComment: %v", err)
	}

	comments, err := ListAllComments(db, 0)
	testsupport.Must(t, err, "ListAllComments: %v", err)
	if len(comments) != 3 {
		t.Errorf("expected 3 comments, got %d", len(comments))
	}

	// Verify ordered by created_at ascending.
	for i := 1; i < len(comments); i++ {
		if comments[i].CreatedAt.Before(comments[i-1].CreatedAt) {
			t.Errorf("comments not sorted by created_at: [%d]=%v > [%d]=%v",
				i-1, comments[i-1].CreatedAt, i, comments[i].CreatedAt)
		}
	}
}

func TestGetAllRelations(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Create 3 issues.
	ids := make([]int, 3)
	for i := 0; i < 3; i++ {
		id, err := CreateIssue(db, &model.Issue{
			Title: "issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, nil, nil)
		testsupport.Must(t, err, "CreateIssue %d: %v", i, err)
		ids[i] = id
	}

	// Create relations of various types.
	rels := []*model.Relation{
		{SourceIssueID: ids[0], TargetIssueID: ids[1], RelationType: model.RelationBlocks},
		{SourceIssueID: ids[1], TargetIssueID: ids[2], RelationType: model.RelationRelatesTo},
	}
	for _, r := range rels {
		_, err := CreateRelation(db, r)
		testsupport.Must(t, err, "CreateRelation: %v", err)
	}

	allRels, err := GetAllRelations(db, 0)
	testsupport.Must(t, err, "GetAllRelations: %v", err)
	if len(allRels) != 2 {
		t.Errorf("expected 2 relations, got %d", len(allRels))
	}
}

func TestListAllLabelsRaw(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Create labels via issues.
	if _, err := CreateIssue(db, &model.Issue{
		Title: "issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"bug", "urgent", "frontend"}, nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	labels, err := ListAllLabelsRaw(db, 0)
	testsupport.Must(t, err, "ListAllLabelsRaw: %v", err)
	if len(labels) != 3 {
		t.Errorf("expected 3 labels, got %d", len(labels))
	}

	// Verify they are model.Label pointers with Name set.
	for _, l := range labels {
		if l.Name == "" {
			t.Error("expected non-empty label name")
		}
	}
}

func TestListAllIssueLabelMappings(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Create two issues with labels.
	id1, err := CreateIssue(db, &model.Issue{
		Title: "issue 1", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"bug", "urgent"}, nil)
	testsupport.Must(t, err, "CreateIssue 1: %v", err)

	id2, err := CreateIssue(db, &model.Issue{
		Title: "issue 2", Status: model.StatusTodo, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"bug"}, nil)
	testsupport.Must(t, err, "CreateIssue 2: %v", err)

	mappings, err := ListAllIssueLabelMappings(db, 0)
	testsupport.Must(t, err, "ListAllIssueLabelMappings: %v", err)

	// Issue 1 has 2 labels, issue 2 has 1 label = 3 mappings total.
	if len(mappings) != 3 {
		t.Errorf("expected 3 mappings, got %d", len(mappings))
	}

	// Verify mappings reference the correct issue IDs.
	issueIDs := make(map[int]bool)
	for _, m := range mappings {
		issueIDs[m.IssueID] = true
	}
	if !issueIDs[id1] || !issueIDs[id2] {
		t.Errorf("expected mappings for issues %d and %d, got %v", id1, id2, issueIDs)
	}
}

func TestCountIssues(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Empty DB should have 0 issues.
	count, err := CountIssues(db, 0)
	testsupport.Must(t, err, "CountIssues: %v", err)
	if count != 0 {
		t.Errorf("expected 0 issues, got %d", count)
	}

	// Create 5 issues.
	for i := 0; i < 5; i++ {
		if _, err := CreateIssue(db, &model.Issue{
			Title: "issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, nil, nil); err != nil {
			t.Fatalf("CreateIssue %d: %v", i, err)
		}
	}

	count, err = CountIssues(db, 0)
	testsupport.Must(t, err, "CountIssues: %v", err)
	if count != 5 {
		t.Errorf("expected 5 issues, got %d", count)
	}
}

func TestClearAllData(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Populate all tables.
	id1, err := CreateIssue(db, &model.Issue{
		Title: "issue 1", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"bug"}, nil)
	testsupport.Must(t, err, "CreateIssue 1: %v", err)
	id2, err := CreateIssue(db, &model.Issue{
		Title: "issue 2", Status: model.StatusTodo, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue 2: %v", err)

	_, err = CreateComment(db, &model.Comment{IssueID: id1, Body: "test"})
	testsupport.Must(t, err, "CreateComment: %v", err)
	if _, err := CreateRelation(db, &model.Relation{
		SourceIssueID: id1, TargetIssueID: id2, RelationType: model.RelationBlocks,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}

	// Clear everything.
	err = ClearAllData(db)
	testsupport.Must(t, err, "ClearAllData: %v", err)

	// Verify all tables are empty.
	tables := map[string]string{
		"issues":          "SELECT COUNT(*) FROM issues",
		"comments":        "SELECT COUNT(*) FROM comments",
		"labels":          "SELECT COUNT(*) FROM labels",
		"issue_labels":    "SELECT COUNT(*) FROM issue_labels",
		"issue_files":     "SELECT COUNT(*) FROM issue_files",
		"issue_relations": "SELECT COUNT(*) FROM issue_relations",
		"activity_log":    "SELECT COUNT(*) FROM activity_log",
	}
	for name, query := range tables {
		var count int
		err := db.QueryRow(query).Scan(&count)
		testsupport.Must(t, err, "counting %s: %v", name, err)
		if count != 0 {
			t.Errorf("expected 0 rows in %s after ClearAllData, got %d", name, count)
		}
	}
}

func TestInsertIssueWithID(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	now := time.Now().UTC().Truncate(time.Second)
	issue := &model.Issue{
		ID:          42,
		Title:       "specific ID issue",
		Description: "test description",
		Status:      model.StatusTodo,
		Priority:    model.PriorityHigh,
		Kind:        model.IssueKindBug,
		Assignee:    "alice",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	_, err = InsertIssueWithID(tx, issue)
	testsupport.Must(t, err, "InsertIssueWithID: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	// Verify retrievable.
	got, err := GetIssue(db, 42)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if got.Title != "specific ID issue" {
		t.Errorf("expected title %q, got %q", "specific ID issue", got.Title)
	}
	if got.Status != model.StatusTodo {
		t.Errorf("expected status %q, got %q", model.StatusTodo, got.Status)
	}
	if got.Priority != model.PriorityHigh {
		t.Errorf("expected priority %q, got %q", model.PriorityHigh, got.Priority)
	}
}

func TestInsertCommentWithID(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Create an issue first (FK dependency).
	issueID, err := CreateIssue(db, &model.Issue{
		Title: "issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)

	now := time.Now().UTC().Truncate(time.Second)
	comment := &model.Comment{
		ID:        99,
		IssueID:   issueID,
		Body:      "specific ID comment",
		Author:    "bob",
		CreatedAt: now,
	}

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	_, err = InsertCommentWithID(tx, comment)
	testsupport.Must(t, err, "InsertCommentWithID: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	// Verify retrievable.
	got, err := GetComment(db, 99)
	testsupport.Must(t, err, "GetComment: %v", err)
	if got.Body != "specific ID comment" {
		t.Errorf("expected body %q, got %q", "specific ID comment", got.Body)
	}
	if got.Author != "bob" {
		t.Errorf("expected author %q, got %q", "bob", got.Author)
	}
}

func TestInsertRelationWithID(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Create two issues.
	id1, err := CreateIssue(db, &model.Issue{
		Title: "issue 1", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue 1: %v", err)
	id2, err := CreateIssue(db, &model.Issue{
		Title: "issue 2", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue 2: %v", err)

	now := time.Now().UTC().Truncate(time.Second)
	rel := &model.Relation{
		ID:            77,
		SourceIssueID: id1,
		TargetIssueID: id2,
		RelationType:  model.RelationDependsOn,
		CreatedAt:     now,
	}

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	_, err = InsertRelationWithID(tx, rel)
	testsupport.Must(t, err, "InsertRelationWithID: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	// Verify retrievable via GetAllRelations.
	allRels, err := GetAllRelations(db, 0)
	testsupport.Must(t, err, "GetAllRelations: %v", err)
	if len(allRels) != 1 {
		t.Fatalf("expected 1 relation, got %d", len(allRels))
	}
	if allRels[0].ID != 77 {
		t.Errorf("expected relation ID 77, got %d", allRels[0].ID)
	}
	if allRels[0].RelationType != model.RelationDependsOn {
		t.Errorf("expected relation type %q, got %q", model.RelationDependsOn, allRels[0].RelationType)
	}
}

func TestInsertLabelWithID(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	label := &model.Label{
		ID:    55,
		Name:  "specific-label",
		Color: "#ff0000",
	}

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	_, err = InsertLabelWithID(tx, label)
	testsupport.Must(t, err, "InsertLabelWithID: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	// Verify retrievable.
	labels, err := ListAllLabelsRaw(db, 0)
	testsupport.Must(t, err, "ListAllLabelsRaw: %v", err)
	if len(labels) != 1 {
		t.Fatalf("expected 1 label, got %d", len(labels))
	}
	if labels[0].ID != 55 {
		t.Errorf("expected label ID 55, got %d", labels[0].ID)
	}
	if labels[0].Name != "specific-label" {
		t.Errorf("expected label name %q, got %q", "specific-label", labels[0].Name)
	}
	if labels[0].Color != "#ff0000" {
		t.Errorf("expected label color %q, got %q", "#ff0000", labels[0].Color)
	}
}

func TestInsertIssueLabelMapping(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize failed: %v", err)

	// Create an issue and a label with specific IDs.
	now := time.Now().UTC().Truncate(time.Second)
	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	if _, err := InsertIssueWithID(tx, &model.Issue{
		ID: 10, Title: "issue", Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertIssueWithID: %v", err)
	}
	_, err = InsertLabelWithID(tx, &model.Label{ID: 20, Name: "test-label"})
	testsupport.Must(t, err, "InsertLabelWithID: %v", err)
	_, err = InsertIssueLabelMapping(tx, 10, 20)
	testsupport.Must(t, err, "InsertIssueLabelMapping: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	// Verify.
	mappings, err := ListAllIssueLabelMappings(db, 0)
	testsupport.Must(t, err, "ListAllIssueLabelMappings: %v", err)
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping, got %d", len(mappings))
	}
	if mappings[0].IssueID != 10 || mappings[0].LabelID != 20 {
		t.Errorf("expected mapping (10, 20), got (%d, %d)", mappings[0].IssueID, mappings[0].LabelID)
	}
}

// --- Round-trip test ---

func TestExportImportRoundTrip(t *testing.T) {
	// Phase 1: Create a populated database.
	srcDB := mustOpen(t)
	err := Initialize(srcDB)
	testsupport.Must(t, err, "Initialize src: %v", err)
	err = Migrate(srcDB)
	testsupport.Must(t, err, "Migrate src: %v", err)

	// Create a parent issue.
	parentID, err := CreateIssue(srcDB, &model.Issue{
		Title:       "parent issue",
		Description: "top-level",
		Status:      model.StatusInProgress,
		Priority:    model.PriorityHigh,
		Kind:        model.IssueKindEpic,
		Assignee:    "alice",
	}, []string{"epic", "v1"}, nil)
	testsupport.Must(t, err, "CreateIssue parent: %v", err)

	// Create child issues under the parent.
	child1ID, err := CreateIssue(srcDB, &model.Issue{
		Title:    "child task 1",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, []string{"backend"}, nil)
	testsupport.Must(t, err, "CreateIssue child1: %v", err)
	// Set parent.
	err = UpdateIssue(srcDB, child1ID, map[string]interface{}{"parent_id": parentID}, "test")
	testsupport.Must(t, err, "UpdateIssue child1 parent: %v", err)

	child2ID, err := CreateIssue(srcDB, &model.Issue{
		Title:    "child task 2",
		Status:   model.StatusDone,
		Priority: model.PriorityLow,
		Kind:     model.IssueKindBug,
	}, []string{"frontend"}, nil)
	testsupport.Must(t, err, "CreateIssue child2: %v", err)
	err = UpdateIssue(srcDB, child2ID, map[string]interface{}{"parent_id": parentID}, "test")
	testsupport.Must(t, err, "UpdateIssue child2 parent: %v", err)

	// Standalone issue.
	standaloneID, err := CreateIssue(srcDB, &model.Issue{
		Title:    "standalone issue",
		Status:   model.StatusBacklog,
		Priority: model.PriorityNone,
		Kind:     model.IssueKindFeature,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue standalone: %v", err)

	// Create comments.
	for _, c := range []*model.Comment{
		{IssueID: parentID, Body: "started work on this", Author: "alice"},
		{IssueID: child1ID, Body: "need to investigate", Author: "bob"},
		{IssueID: child2ID, Body: "fixed the bug", Author: "alice"},
	} {
		_, err := CreateComment(srcDB, c)
		testsupport.Must(t, err, "CreateComment: %v", err)
	}

	// Create relations.
	if _, err := CreateRelation(srcDB, &model.Relation{
		SourceIssueID: child1ID, TargetIssueID: standaloneID, RelationType: model.RelationRelatesTo,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}

	// Create docs with revisions, comments, and an issue link.
	doc1ID, err := CreateDoc(srcDB, &model.Doc{
		Type: "tdd", Status: "draft", Title: "design doc", Body: "initial body", Author: "alice",
	})
	testsupport.Must(t, err, "CreateDoc 1: %v", err)
	// Append a revision by editing the body.
	newBody := "revised body"
	_, err = UpdateDoc(srcDB, doc1ID, DocUpdate{Body: &newBody, Author: "bob"})
	testsupport.Must(t, err, "UpdateDoc 1: %v", err)

	doc2ID, err := CreateDoc(srcDB, &model.Doc{
		Type: "adr", Status: "accepted", Title: "decision record", Body: "the decision", Author: "carol",
	})
	testsupport.Must(t, err, "CreateDoc 2: %v", err)

	// Comments on docs.
	for _, c := range []*model.DocComment{
		{DocID: doc1ID, Body: "looks good", Author: "bob"},
		{DocID: doc1ID, Body: "one nit", Author: "carol"},
		{DocID: doc2ID, Body: "approved", Author: "alice"},
	} {
		_, err := CreateDocComment(srcDB, c)
		testsupport.Must(t, err, "CreateDocComment: %v", err)
	}

	// Link a doc to an issue.
	err = LinkDocIssue(srcDB, doc1ID, parentID)
	testsupport.Must(t, err, "LinkDocIssue: %v", err)

	// Create a proposal with a vote and issue/doc links.
	proposalID, err := CreateProposal(srcDB, &model.Proposal{
		Description:    "ship the feature",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: 2,
		Threshold:      0.67,
		CreatedBy:      "@team-lead",
		Rationale:      "ready",
		DomainTags:     []string{"backend"},
		FilesChanged:   []string{"main.go"},
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	if _, err := CastVote(srcDB, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "@senior-engineer",
		VoterRole:       "senior-engineer",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Summary:         "approved",
		FindingsJSON:    &model.Findings{Concerns: []string{"nit"}},
	}); err != nil {
		t.Fatalf("CastVote: %v", err)
	}
	err = LinkProposalIssue(srcDB, proposalID, standaloneID)
	testsupport.Must(t, err, "LinkProposalIssue: %v", err)
	err = LinkProposalDoc(srcDB, proposalID, doc2ID)
	testsupport.Must(t, err, "LinkProposalDoc: %v", err)

	// Phase 2: Export from source DB.
	srcExport := exportDB(t, srcDB)

	// Phase 3: Marshal to JSON.
	jsonBytes, err := json.Marshal(srcExport)
	testsupport.Must(t, err, "json.Marshal: %v", err)

	// Phase 4: Create fresh DB, unmarshal, and import.
	dstDB := mustOpen(t)
	err = Initialize(dstDB)
	testsupport.Must(t, err, "Initialize dst: %v", err)
	err = Migrate(dstDB)
	testsupport.Must(t, err, "Migrate dst: %v", err)

	var importData model.ExportData
	err = json.Unmarshal(jsonBytes, &importData)
	testsupport.Must(t, err, "json.Unmarshal: %v", err)

	importAll(t, dstDB, &importData)

	// Phase 5: Export from destination DB.
	dstExport := exportDB(t, dstDB)

	// Phase 6: Compare. Normalize ExportedAt to match.
	srcExport.ExportedAt = "normalized"
	dstExport.ExportedAt = "normalized"

	srcJSON, err := json.Marshal(srcExport)
	testsupport.Must(t, err, "marshal src: %v", err)
	dstJSON, err := json.Marshal(dstExport)
	testsupport.Must(t, err, "marshal dst: %v", err)

	if string(srcJSON) != string(dstJSON) {
		t.Errorf("round-trip mismatch:\n  src: %s\n  dst: %s", string(srcJSON), string(dstJSON))
	}
}

// --- Import behavior tests ---

func TestImportToEmptyDB(t *testing.T) {
	srcDB := mustOpen(t)
	err := Initialize(srcDB)
	testsupport.Must(t, err, "Initialize src: %v", err)
	err = Migrate(srcDB)
	testsupport.Must(t, err, "Migrate src: %v", err)

	// Populate source.
	if _, err := CreateIssue(srcDB, &model.Issue{
		Title: "test issue", Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, []string{"tag1"}, nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	srcExport := exportDB(t, srcDB)
	jsonBytes, err := json.Marshal(srcExport)
	testsupport.Must(t, err, "json.Marshal: %v", err)

	// Import into empty DB.
	dstDB := mustOpen(t)
	err = Initialize(dstDB)
	testsupport.Must(t, err, "Initialize dst: %v", err)
	err = Migrate(dstDB)
	testsupport.Must(t, err, "Migrate dst: %v", err)

	var importData model.ExportData
	err = json.Unmarshal(jsonBytes, &importData)
	testsupport.Must(t, err, "json.Unmarshal: %v", err)

	importAll(t, dstDB, &importData)

	// Verify data was imported.
	count, err := CountIssues(dstDB, 0)
	testsupport.Must(t, err, "CountIssues: %v", err)
	if count != 1 {
		t.Errorf("expected 1 issue after import, got %d", count)
	}

	labels, err := ListAllLabelsRaw(dstDB, 0)
	testsupport.Must(t, err, "ListAllLabelsRaw: %v", err)
	if len(labels) != 1 {
		t.Errorf("expected 1 label after import, got %d", len(labels))
	}
}

func TestImportMergeSkipsDuplicates(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	now := time.Now().UTC().Truncate(time.Second)

	// Pre-populate with some data.
	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	_, err = InsertLabelWithID(tx, &model.Label{ID: 1, Name: "existing-label"})
	testsupport.Must(t, err, "InsertLabelWithID: %v", err)
	if _, err := InsertIssueWithID(tx, &model.Issue{
		ID: 1, Title: "existing issue", Status: model.StatusBacklog,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertIssueWithID: %v", err)
	}
	_, err = InsertIssueLabelMapping(tx, 1, 1)
	testsupport.Must(t, err, "InsertIssueLabelMapping: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	// Build import data with a duplicate issue (ID 1) and a new one (ID 2).
	importData := &model.ExportData{
		Version:    1,
		ExportedAt: now.Format(time.RFC3339),
		Labels: []*model.Label{
			{ID: 1, Name: "existing-label"},
			{ID: 2, Name: "new-label"},
		},
		Issues: []*model.Issue{
			{ID: 1, Title: "existing issue", Status: model.StatusBacklog,
				Priority: model.PriorityNone, Kind: model.IssueKindTask,
				CreatedAt: now, UpdatedAt: now},
			{ID: 2, Title: "new issue", Status: model.StatusTodo,
				Priority: model.PriorityHigh, Kind: model.IssueKindFeature,
				CreatedAt: now, UpdatedAt: now},
		},
		IssueLabelMappings: []model.IssueLabelMapping{
			{IssueID: 1, LabelID: 1},
			{IssueID: 2, LabelID: 2},
		},
		Comments:  []*model.Comment{},
		Relations: []model.Relation{},
	}

	importAll(t, db, importData)

	// Should have 2 issues now (existing + new).
	count, err := CountIssues(db, 0)
	testsupport.Must(t, err, "CountIssues: %v", err)
	if count != 2 {
		t.Errorf("expected 2 issues after merge, got %d", count)
	}

	// Should have 2 labels.
	labels, err := ListAllLabelsRaw(db, 0)
	testsupport.Must(t, err, "ListAllLabelsRaw: %v", err)
	if len(labels) != 2 {
		t.Errorf("expected 2 labels after merge, got %d", len(labels))
	}

	// Should have 2 mappings.
	mappings, err := ListAllIssueLabelMappings(db, 0)
	testsupport.Must(t, err, "ListAllIssueLabelMappings: %v", err)
	if len(mappings) != 2 {
		t.Errorf("expected 2 mappings after merge, got %d", len(mappings))
	}
}

func TestImportReplaceClears(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	now := time.Now().UTC().Truncate(time.Second)

	// Pre-populate with data that should be replaced.
	if _, err := CreateIssue(db, &model.Issue{
		Title: "old issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"old-label"}, nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Clear all data (simulating --replace).
	err = ClearAllData(db)
	testsupport.Must(t, err, "ClearAllData: %v", err)

	// Import new data.
	importData := &model.ExportData{
		Version:    1,
		ExportedAt: now.Format(time.RFC3339),
		Labels: []*model.Label{
			{ID: 100, Name: "new-label"},
		},
		Issues: []*model.Issue{
			{ID: 100, Title: "replacement issue", Status: model.StatusTodo,
				Priority: model.PriorityMedium, Kind: model.IssueKindFeature,
				CreatedAt: now, UpdatedAt: now},
		},
		IssueLabelMappings: []model.IssueLabelMapping{
			{IssueID: 100, LabelID: 100},
		},
		Comments:  []*model.Comment{},
		Relations: []model.Relation{},
	}

	importAll(t, db, importData)

	// Verify only the imported data exists.
	issues, err := ListAllIssues(db, 0)
	testsupport.Must(t, err, "ListAllIssues: %v", err)
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue after replace, got %d", len(issues))
	}
	if issues[0].ID != 100 {
		t.Errorf("expected issue ID 100, got %d", issues[0].ID)
	}
	if issues[0].Title != "replacement issue" {
		t.Errorf("expected title %q, got %q", "replacement issue", issues[0].Title)
	}

	labels, err := ListAllLabelsRaw(db, 0)
	testsupport.Must(t, err, "ListAllLabelsRaw: %v", err)
	if len(labels) != 1 {
		t.Fatalf("expected 1 label after replace, got %d", len(labels))
	}
	if labels[0].Name != "new-label" {
		t.Errorf("expected label name %q, got %q", "new-label", labels[0].Name)
	}
}

// --- Test helpers ---

// exportDB builds an ExportData from the given database.
func exportDB(t *testing.T, db *sql.DB) *model.ExportData {
	t.Helper()

	issues, err := ListAllIssues(db, 0)
	testsupport.Must(t, err, "ListAllIssues: %v", err)
	comments, err := ListAllComments(db, 0)
	testsupport.Must(t, err, "ListAllComments: %v", err)
	relations, err := GetAllRelations(db, 0)
	testsupport.Must(t, err, "GetAllRelations: %v", err)
	labels, err := ListAllLabelsRaw(db, 0)
	testsupport.Must(t, err, "ListAllLabelsRaw: %v", err)
	mappings, err := ListAllIssueLabelMappings(db, 0)
	testsupport.Must(t, err, "ListAllIssueLabelMappings: %v", err)
	fileMappings, err := ListAllIssueFileMappings(db, 0)
	testsupport.Must(t, err, "ListAllIssueFileMappings: %v", err)
	docs, err := ListAllDocs(db, 0)
	testsupport.Must(t, err, "ListAllDocs: %v", err)
	docRevisions, err := ListAllDocRevisions(db, 0)
	testsupport.Must(t, err, "ListAllDocRevisions: %v", err)
	docComments, err := ListAllDocComments(db, 0)
	testsupport.Must(t, err, "ListAllDocComments: %v", err)
	docIssueLinks, err := ListAllDocIssueLinks(db, 0)
	testsupport.Must(t, err, "ListAllDocIssueLinks: %v", err)
	proposalDocs, err := ListAllProposalDocs(db, 0)
	testsupport.Must(t, err, "ListAllProposalDocs: %v", err)
	proposals, err := ListAllProposals(db, 0)
	testsupport.Must(t, err, "ListAllProposals: %v", err)
	votes, err := ListAllVotes(db, 0)
	testsupport.Must(t, err, "ListAllVotes: %v", err)
	proposalIssues, err := ListAllProposalIssues(db, 0)
	testsupport.Must(t, err, "ListAllProposalIssues: %v", err)
	activityLog, err := ListAllActivity(db, 0)
	testsupport.Must(t, err, "ListAllActivity: %v", err)

	// Ensure nil slices become empty for JSON consistency.
	if issues == nil {
		issues = []*model.Issue{}
	}
	if comments == nil {
		comments = []*model.Comment{}
	}
	if relations == nil {
		relations = []model.Relation{}
	}
	if labels == nil {
		labels = []*model.Label{}
	}
	if mappings == nil {
		mappings = []model.IssueLabelMapping{}
	}
	if fileMappings == nil {
		fileMappings = []model.IssueFileMapping{}
	}
	if activityLog == nil {
		activityLog = []*model.Activity{}
	}
	if docs == nil {
		docs = []*model.Doc{}
	}
	if docRevisions == nil {
		docRevisions = []*model.DocRevision{}
	}
	if docComments == nil {
		docComments = []*model.DocComment{}
	}
	if proposals == nil {
		proposals = []*model.Proposal{}
	}
	if votes == nil {
		votes = []*model.Vote{}
	}

	return &model.ExportData{
		Version:            1,
		ExportedAt:         time.Now().UTC().Format(time.RFC3339),
		Issues:             issues,
		Comments:           comments,
		Relations:          relations,
		Labels:             labels,
		IssueLabelMappings: mappings,
		IssueFileMappings:  fileMappings,
		ActivityLog:        activityLog,
		Docs:               docs,
		DocRevisions:       docRevisions,
		DocComments:        docComments,
		DocIssueLinks:      docIssueLinks,
		Proposals:          proposals,
		Votes:              votes,
		ProposalIssues:     proposalIssues,
		ProposalDocs:       proposalDocs,
	}
}

// importAll imports all data from an ExportData into the database (no merge, empty DB).
func importAll(t *testing.T, db *sql.DB, data *model.ExportData) {
	t.Helper()

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	// 1. Labels first (no FK deps).
	for _, label := range data.Labels {
		_, err := InsertLabelWithID(tx, label)
		testsupport.Must(t, err, "InsertLabelWithID %q: %v", label.Name, err)
	}

	// 2. Issues without parent_id, then update parent_id.
	parentIDs := make(map[int]*int)
	for _, issue := range data.Issues {
		origParentID := issue.ParentID
		if issue.ParentID != nil {
			pid := *issue.ParentID
			parentIDs[issue.ID] = &pid
			issue.ParentID = nil
		}
		if _, err := InsertIssueWithID(tx, issue); err != nil {
			issue.ParentID = origParentID
			t.Fatalf("InsertIssueWithID %d: %v", issue.ID, err)
		}
		issue.ParentID = origParentID
	}
	for issueID, pid := range parentIDs {
		_, err := tx.Exec(`UPDATE issues SET parent_id = ? WHERE id = ?`, *pid, issueID)
		testsupport.Must(t, err, "setting parent_id for %d: %v", issueID, err)
	}

	// 3. Issue-label mappings.
	for _, m := range data.IssueLabelMappings {
		_, err := InsertIssueLabelMapping(tx, m.IssueID, m.LabelID)
		testsupport.Must(t, err, "InsertIssueLabelMapping (%d, %d): %v", m.IssueID, m.LabelID, err)
	}

	// 4. Issue-file mappings.
	for _, m := range data.IssueFileMappings {
		_, err := InsertIssueFileMapping(tx, m.IssueID, m.FilePath)
		testsupport.Must(t, err, "InsertIssueFileMapping (issue=%d, file=%q): %v", m.IssueID, m.FilePath, err)
	}

	// 5. Comments.
	for _, comment := range data.Comments {
		_, err := InsertCommentWithID(tx, comment)
		testsupport.Must(t, err, "InsertCommentWithID %d: %v", comment.ID, err)
	}

	// 6. Relations.
	for i := range data.Relations {
		_, err := InsertRelationWithID(tx, &data.Relations[i])
		testsupport.Must(t, err, "InsertRelationWithID %d: %v", data.Relations[i].ID, err)
	}

	// 7. Activity log.
	for _, a := range data.ActivityLog {
		_, err := InsertActivityWithID(tx, a)
		testsupport.Must(t, err, "InsertActivityWithID %d: %v", a.ID, err)
	}

	// 8. Proposals.
	for _, p := range data.Proposals {
		_, err := InsertProposalWithID(tx, p)
		testsupport.Must(t, err, "InsertProposalWithID %d: %v", p.ID, err)
	}

	// 9. Votes.
	for _, v := range data.Votes {
		_, err := InsertVoteWithID(tx, v)
		testsupport.Must(t, err, "InsertVoteWithID %d: %v", v.ID, err)
	}

	// 10. Proposal-issue links.
	for _, l := range data.ProposalIssues {
		_, err := InsertProposalIssueLink(tx, l.ProposalID, l.IssueID)
		testsupport.Must(t, err, "InsertProposalIssueLink (%d,%d): %v", l.ProposalID, l.IssueID, err)
	}

	// 11. Docs.
	for _, doc := range data.Docs {
		_, err := InsertDocWithID(tx, doc)
		testsupport.Must(t, err, "InsertDocWithID %d: %v", doc.ID, err)
	}

	// 12. Doc revisions.
	for _, rev := range data.DocRevisions {
		_, err := InsertDocRevisionWithID(tx, rev)
		testsupport.Must(t, err, "InsertDocRevisionWithID %d: %v", rev.ID, err)
	}

	// 13. Doc comments.
	for _, c := range data.DocComments {
		_, err := InsertDocCommentWithID(tx, c)
		testsupport.Must(t, err, "InsertDocCommentWithID %d: %v", c.ID, err)
	}

	// 14. Doc-issue links.
	for _, l := range data.DocIssueLinks {
		_, err := InsertDocIssueLink(tx, l.DocID, l.IssueID, l.CreatedAt)
		testsupport.Must(t, err, "InsertDocIssueLink (%d,%d): %v", l.DocID, l.IssueID, err)
	}

	// 15. Proposal-doc links.
	for _, l := range data.ProposalDocs {
		_, err := InsertProposalDocLink(tx, l.ProposalID, l.DocID, l.CreatedAt)
		testsupport.Must(t, err, "InsertProposalDocLink (%d,%d): %v", l.ProposalID, l.DocID, err)
	}

	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)
}
