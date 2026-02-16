package db

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// --- DB layer tests ---

func TestListAllIssues(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

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
		if err != nil {
			t.Fatalf("CreateIssue %d: %v", i, err)
		}
		if id <= 0 {
			t.Fatalf("expected positive id, got %d", id)
		}
	}

	issues, err := ListAllIssues(db)
	if err != nil {
		t.Fatalf("ListAllIssues: %v", err)
	}
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
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Create two issues.
	id1, err := CreateIssue(db, &model.Issue{
		Title: "issue 1", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue 1: %v", err)
	}
	id2, err := CreateIssue(db, &model.Issue{
		Title: "issue 2", Status: model.StatusTodo, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue 2: %v", err)
	}

	// Create comments on both issues.
	for _, c := range []*model.Comment{
		{IssueID: id1, Body: "comment A", Author: "alice"},
		{IssueID: id2, Body: "comment B", Author: "bob"},
		{IssueID: id1, Body: "comment C", Author: "alice"},
	} {
		if _, err := CreateComment(db, c); err != nil {
			t.Fatalf("CreateComment: %v", err)
		}
	}

	comments, err := ListAllComments(db)
	if err != nil {
		t.Fatalf("ListAllComments: %v", err)
	}
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
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Create 3 issues.
	ids := make([]int, 3)
	for i := 0; i < 3; i++ {
		id, err := CreateIssue(db, &model.Issue{
			Title: "issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, nil, nil)
		if err != nil {
			t.Fatalf("CreateIssue %d: %v", i, err)
		}
		ids[i] = id
	}

	// Create relations of various types.
	rels := []*model.Relation{
		{SourceIssueID: ids[0], TargetIssueID: ids[1], RelationType: model.RelationBlocks},
		{SourceIssueID: ids[1], TargetIssueID: ids[2], RelationType: model.RelationRelatesTo},
	}
	for _, r := range rels {
		if _, err := CreateRelation(db, r); err != nil {
			t.Fatalf("CreateRelation: %v", err)
		}
	}

	allRels, err := GetAllRelations(db)
	if err != nil {
		t.Fatalf("GetAllRelations: %v", err)
	}
	if len(allRels) != 2 {
		t.Errorf("expected 2 relations, got %d", len(allRels))
	}
}

func TestListAllLabelsRaw(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Create labels via issues.
	if _, err := CreateIssue(db, &model.Issue{
		Title: "issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"bug", "urgent", "frontend"}, nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	labels, err := ListAllLabelsRaw(db)
	if err != nil {
		t.Fatalf("ListAllLabelsRaw: %v", err)
	}
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
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Create two issues with labels.
	id1, err := CreateIssue(db, &model.Issue{
		Title: "issue 1", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"bug", "urgent"}, nil)
	if err != nil {
		t.Fatalf("CreateIssue 1: %v", err)
	}

	id2, err := CreateIssue(db, &model.Issue{
		Title: "issue 2", Status: model.StatusTodo, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"bug"}, nil)
	if err != nil {
		t.Fatalf("CreateIssue 2: %v", err)
	}

	mappings, err := ListAllIssueLabelMappings(db)
	if err != nil {
		t.Fatalf("ListAllIssueLabelMappings: %v", err)
	}

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
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Empty DB should have 0 issues.
	count, err := CountIssues(db)
	if err != nil {
		t.Fatalf("CountIssues: %v", err)
	}
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

	count, err = CountIssues(db)
	if err != nil {
		t.Fatalf("CountIssues: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5 issues, got %d", count)
	}
}

func TestClearAllData(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Populate all tables.
	id1, err := CreateIssue(db, &model.Issue{
		Title: "issue 1", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"bug"}, nil)
	if err != nil {
		t.Fatalf("CreateIssue 1: %v", err)
	}
	id2, err := CreateIssue(db, &model.Issue{
		Title: "issue 2", Status: model.StatusTodo, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue 2: %v", err)
	}

	if _, err := CreateComment(db, &model.Comment{IssueID: id1, Body: "test"}); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}
	if _, err := CreateRelation(db, &model.Relation{
		SourceIssueID: id1, TargetIssueID: id2, RelationType: model.RelationBlocks,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}

	// Clear everything.
	if err := ClearAllData(db); err != nil {
		t.Fatalf("ClearAllData: %v", err)
	}

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
		if err := db.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("counting %s: %v", name, err)
		}
		if count != 0 {
			t.Errorf("expected 0 rows in %s after ClearAllData, got %d", name, count)
		}
	}
}

func TestInsertIssueWithID(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

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
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := InsertIssueWithID(tx,issue); err != nil {
		t.Fatalf("InsertIssueWithID: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify retrievable.
	got, err := GetIssue(db, 42)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
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
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Create an issue first (FK dependency).
	issueID, err := CreateIssue(db, &model.Issue{
		Title: "issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	comment := &model.Comment{
		ID:        99,
		IssueID:   issueID,
		Body:      "specific ID comment",
		Author:    "bob",
		CreatedAt: now,
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := InsertCommentWithID(tx,comment); err != nil {
		t.Fatalf("InsertCommentWithID: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify retrievable.
	got, err := GetComment(db, 99)
	if err != nil {
		t.Fatalf("GetComment: %v", err)
	}
	if got.Body != "specific ID comment" {
		t.Errorf("expected body %q, got %q", "specific ID comment", got.Body)
	}
	if got.Author != "bob" {
		t.Errorf("expected author %q, got %q", "bob", got.Author)
	}
}

func TestInsertRelationWithID(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Create two issues.
	id1, err := CreateIssue(db, &model.Issue{
		Title: "issue 1", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue 1: %v", err)
	}
	id2, err := CreateIssue(db, &model.Issue{
		Title: "issue 2", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue 2: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	rel := &model.Relation{
		ID:            77,
		SourceIssueID: id1,
		TargetIssueID: id2,
		RelationType:  model.RelationDependsOn,
		CreatedAt:     now,
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := InsertRelationWithID(tx,rel); err != nil {
		t.Fatalf("InsertRelationWithID: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify retrievable via GetAllRelations.
	allRels, err := GetAllRelations(db)
	if err != nil {
		t.Fatalf("GetAllRelations: %v", err)
	}
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
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	label := &model.Label{
		ID:    55,
		Name:  "specific-label",
		Color: "#ff0000",
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := InsertLabelWithID(tx,label); err != nil {
		t.Fatalf("InsertLabelWithID: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify retrievable.
	labels, err := ListAllLabelsRaw(db)
	if err != nil {
		t.Fatalf("ListAllLabelsRaw: %v", err)
	}
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
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Create an issue and a label with specific IDs.
	now := time.Now().UTC().Truncate(time.Second)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := InsertIssueWithID(tx,&model.Issue{
		ID: 10, Title: "issue", Status: model.StatusBacklog, Priority: model.PriorityNone,
		Kind: model.IssueKindTask, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertIssueWithID: %v", err)
	}
	if _, err := InsertLabelWithID(tx,&model.Label{ID: 20, Name: "test-label"}); err != nil {
		t.Fatalf("InsertLabelWithID: %v", err)
	}
	if _, err := InsertIssueLabelMapping(tx,10, 20); err != nil {
		t.Fatalf("InsertIssueLabelMapping: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify.
	mappings, err := ListAllIssueLabelMappings(db)
	if err != nil {
		t.Fatalf("ListAllIssueLabelMappings: %v", err)
	}
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
	if err := Initialize(srcDB); err != nil {
		t.Fatalf("Initialize src: %v", err)
	}

	// Create a parent issue.
	parentID, err := CreateIssue(srcDB, &model.Issue{
		Title:       "parent issue",
		Description: "top-level",
		Status:      model.StatusInProgress,
		Priority:    model.PriorityHigh,
		Kind:        model.IssueKindEpic,
		Assignee:    "alice",
	}, []string{"epic", "v1"}, nil)
	if err != nil {
		t.Fatalf("CreateIssue parent: %v", err)
	}

	// Create child issues under the parent.
	child1ID, err := CreateIssue(srcDB, &model.Issue{
		Title:    "child task 1",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, []string{"backend"}, nil)
	if err != nil {
		t.Fatalf("CreateIssue child1: %v", err)
	}
	// Set parent.
	if err := UpdateIssue(srcDB, child1ID, map[string]interface{}{"parent_id": parentID}, "test"); err != nil {
		t.Fatalf("UpdateIssue child1 parent: %v", err)
	}

	child2ID, err := CreateIssue(srcDB, &model.Issue{
		Title:    "child task 2",
		Status:   model.StatusDone,
		Priority: model.PriorityLow,
		Kind:     model.IssueKindBug,
	}, []string{"frontend"}, nil)
	if err != nil {
		t.Fatalf("CreateIssue child2: %v", err)
	}
	if err := UpdateIssue(srcDB, child2ID, map[string]interface{}{"parent_id": parentID}, "test"); err != nil {
		t.Fatalf("UpdateIssue child2 parent: %v", err)
	}

	// Standalone issue.
	standaloneID, err := CreateIssue(srcDB, &model.Issue{
		Title:    "standalone issue",
		Status:   model.StatusBacklog,
		Priority: model.PriorityNone,
		Kind:     model.IssueKindFeature,
	}, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue standalone: %v", err)
	}

	// Create comments.
	for _, c := range []*model.Comment{
		{IssueID: parentID, Body: "started work on this", Author: "alice"},
		{IssueID: child1ID, Body: "need to investigate", Author: "bob"},
		{IssueID: child2ID, Body: "fixed the bug", Author: "alice"},
	} {
		if _, err := CreateComment(srcDB, c); err != nil {
			t.Fatalf("CreateComment: %v", err)
		}
	}

	// Create relations.
	if _, err := CreateRelation(srcDB, &model.Relation{
		SourceIssueID: child1ID, TargetIssueID: standaloneID, RelationType: model.RelationRelatesTo,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}

	// Phase 2: Export from source DB.
	srcExport := exportDB(t, srcDB)

	// Phase 3: Marshal to JSON.
	jsonBytes, err := json.Marshal(srcExport)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// Phase 4: Create fresh DB, unmarshal, and import.
	dstDB := mustOpen(t)
	if err := Initialize(dstDB); err != nil {
		t.Fatalf("Initialize dst: %v", err)
	}

	var importData model.ExportData
	if err := json.Unmarshal(jsonBytes, &importData); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	importAll(t, dstDB, &importData)

	// Phase 5: Export from destination DB.
	dstExport := exportDB(t, dstDB)

	// Phase 6: Compare. Normalize ExportedAt to match.
	srcExport.ExportedAt = "normalized"
	dstExport.ExportedAt = "normalized"

	srcJSON, err := json.Marshal(srcExport)
	if err != nil {
		t.Fatalf("marshal src: %v", err)
	}
	dstJSON, err := json.Marshal(dstExport)
	if err != nil {
		t.Fatalf("marshal dst: %v", err)
	}

	if string(srcJSON) != string(dstJSON) {
		t.Errorf("round-trip mismatch:\n  src: %s\n  dst: %s", string(srcJSON), string(dstJSON))
	}
}

// --- Import behavior tests ---

func TestImportToEmptyDB(t *testing.T) {
	srcDB := mustOpen(t)
	if err := Initialize(srcDB); err != nil {
		t.Fatalf("Initialize src: %v", err)
	}

	// Populate source.
	if _, err := CreateIssue(srcDB, &model.Issue{
		Title: "test issue", Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, []string{"tag1"}, nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	srcExport := exportDB(t, srcDB)
	jsonBytes, err := json.Marshal(srcExport)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// Import into empty DB.
	dstDB := mustOpen(t)
	if err := Initialize(dstDB); err != nil {
		t.Fatalf("Initialize dst: %v", err)
	}

	var importData model.ExportData
	if err := json.Unmarshal(jsonBytes, &importData); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	importAll(t, dstDB, &importData)

	// Verify data was imported.
	count, err := CountIssues(dstDB)
	if err != nil {
		t.Fatalf("CountIssues: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 issue after import, got %d", count)
	}

	labels, err := ListAllLabelsRaw(dstDB)
	if err != nil {
		t.Fatalf("ListAllLabelsRaw: %v", err)
	}
	if len(labels) != 1 {
		t.Errorf("expected 1 label after import, got %d", len(labels))
	}
}

func TestImportMergeSkipsDuplicates(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	// Pre-populate with some data.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := InsertLabelWithID(tx,&model.Label{ID: 1, Name: "existing-label"}); err != nil {
		t.Fatalf("InsertLabelWithID: %v", err)
	}
	if _, err := InsertIssueWithID(tx,&model.Issue{
		ID: 1, Title: "existing issue", Status: model.StatusBacklog,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("InsertIssueWithID: %v", err)
	}
	if _, err := InsertIssueLabelMapping(tx,1, 1); err != nil {
		t.Fatalf("InsertIssueLabelMapping: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

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
	count, err := CountIssues(db)
	if err != nil {
		t.Fatalf("CountIssues: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 issues after merge, got %d", count)
	}

	// Should have 2 labels.
	labels, err := ListAllLabelsRaw(db)
	if err != nil {
		t.Fatalf("ListAllLabelsRaw: %v", err)
	}
	if len(labels) != 2 {
		t.Errorf("expected 2 labels after merge, got %d", len(labels))
	}

	// Should have 2 mappings.
	mappings, err := ListAllIssueLabelMappings(db)
	if err != nil {
		t.Fatalf("ListAllIssueLabelMappings: %v", err)
	}
	if len(mappings) != 2 {
		t.Errorf("expected 2 mappings after merge, got %d", len(mappings))
	}
}

func TestImportReplaceClears(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)

	// Pre-populate with data that should be replaced.
	if _, err := CreateIssue(db, &model.Issue{
		Title: "old issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"old-label"}, nil); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	// Clear all data (simulating --replace).
	if err := ClearAllData(db); err != nil {
		t.Fatalf("ClearAllData: %v", err)
	}

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
	issues, err := ListAllIssues(db)
	if err != nil {
		t.Fatalf("ListAllIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue after replace, got %d", len(issues))
	}
	if issues[0].ID != 100 {
		t.Errorf("expected issue ID 100, got %d", issues[0].ID)
	}
	if issues[0].Title != "replacement issue" {
		t.Errorf("expected title %q, got %q", "replacement issue", issues[0].Title)
	}

	labels, err := ListAllLabelsRaw(db)
	if err != nil {
		t.Fatalf("ListAllLabelsRaw: %v", err)
	}
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

	issues, err := ListAllIssues(db)
	if err != nil {
		t.Fatalf("ListAllIssues: %v", err)
	}
	comments, err := ListAllComments(db)
	if err != nil {
		t.Fatalf("ListAllComments: %v", err)
	}
	relations, err := GetAllRelations(db)
	if err != nil {
		t.Fatalf("GetAllRelations: %v", err)
	}
	labels, err := ListAllLabelsRaw(db)
	if err != nil {
		t.Fatalf("ListAllLabelsRaw: %v", err)
	}
	mappings, err := ListAllIssueLabelMappings(db)
	if err != nil {
		t.Fatalf("ListAllIssueLabelMappings: %v", err)
	}
	fileMappings, err := ListAllIssueFileMappings(db)
	if err != nil {
		t.Fatalf("ListAllIssueFileMappings: %v", err)
	}

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

	return &model.ExportData{
		Version:            1,
		ExportedAt:         time.Now().UTC().Format(time.RFC3339),
		Issues:             issues,
		Comments:           comments,
		Relations:          relations,
		Labels:             labels,
		IssueLabelMappings: mappings,
		IssueFileMappings:  fileMappings,
	}
}

// importAll imports all data from an ExportData into the database (no merge, empty DB).
func importAll(t *testing.T, db *sql.DB, data *model.ExportData) {
	t.Helper()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	// 1. Labels first (no FK deps).
	for _, label := range data.Labels {
		if _, err := InsertLabelWithID(tx,label); err != nil {
			t.Fatalf("InsertLabelWithID %q: %v", label.Name, err)
		}
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
		if _, err := InsertIssueWithID(tx,issue); err != nil {
			issue.ParentID = origParentID
			t.Fatalf("InsertIssueWithID %d: %v", issue.ID, err)
		}
		issue.ParentID = origParentID
	}
	for issueID, pid := range parentIDs {
		if _, err := tx.Exec(`UPDATE issues SET parent_id = ? WHERE id = ?`, *pid, issueID); err != nil {
			t.Fatalf("setting parent_id for %d: %v", issueID, err)
		}
	}

	// 3. Issue-label mappings.
	for _, m := range data.IssueLabelMappings {
		if _, err := InsertIssueLabelMapping(tx,m.IssueID, m.LabelID); err != nil {
			t.Fatalf("InsertIssueLabelMapping (%d, %d): %v", m.IssueID, m.LabelID, err)
		}
	}

	// 4. Issue-file mappings.
	for _, m := range data.IssueFileMappings {
		if _, err := InsertIssueFileMapping(tx, m.IssueID, m.FilePath); err != nil {
			t.Fatalf("InsertIssueFileMapping (issue=%d, file=%q): %v", m.IssueID, m.FilePath, err)
		}
	}

	// 5. Comments.
	for _, comment := range data.Comments {
		if _, err := InsertCommentWithID(tx,comment); err != nil {
			t.Fatalf("InsertCommentWithID %d: %v", comment.ID, err)
		}
	}

	// 6. Relations.
	for i := range data.Relations {
		if _, err := InsertRelationWithID(tx,&data.Relations[i]); err != nil {
			t.Fatalf("InsertRelationWithID %d: %v", data.Relations[i].ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

