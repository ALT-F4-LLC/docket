package app

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

func mustOpenDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := db.Initialize(conn); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return conn
}

func createIssue(t *testing.T, conn *sql.DB, issue model.Issue, labels, files []string) int {
	t.Helper()
	id, err := db.CreateIssue(conn, &issue, labels, files)
	if err != nil {
		t.Fatalf("CreateIssue(%q): %v", issue.Title, err)
	}
	return id
}

func TestListIssuesBuildsParentContext(t *testing.T) {
	conn := mustOpenDB(t)

	parentID := createIssue(t, conn, model.Issue{
		Title:    "Parent epic",
		Status:   model.StatusBacklog,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindEpic,
	}, nil, nil)

	createIssue(t, conn, model.Issue{
		ParentID: &parentID,
		Title:    "Child task",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, nil, nil)

	data, err := ListIssues(conn, ListIssuesParams{Statuses: []string{string(model.StatusTodo)}})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}

	if data.Total != 1 || len(data.Issues) != 1 {
		t.Fatalf("got total=%d len=%d, want 1/1", data.Total, len(data.Issues))
	}
	if _, ok := data.ParentMap[parentID]; !ok {
		t.Fatalf("expected parent %d in parent map", parentID)
	}
	if progress, ok := data.Progress[parentID]; !ok || progress.Total != 1 {
		t.Fatalf("expected progress for parent %d, got %#v", parentID, data.Progress[parentID])
	}
}

func TestLoadBoardRollsUpChildrenByDefault(t *testing.T) {
	conn := mustOpenDB(t)

	parentID := createIssue(t, conn, model.Issue{
		Title:    "Parent task",
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindTask,
	}, nil, nil)
	createIssue(t, conn, model.Issue{
		ParentID: &parentID,
		Title:    "Nested task",
		Status:   model.StatusTodo,
		Priority: model.PriorityLow,
		Kind:     model.IssueKindTask,
	}, nil, nil)
	createIssue(t, conn, model.Issue{
		Title:    "Standalone task",
		Status:   model.StatusReview,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, nil, nil)

	rolledUp, err := LoadBoard(conn, BoardParams{})
	if err != nil {
		t.Fatalf("LoadBoard: %v", err)
	}
	if len(rolledUp.Issues) != 2 {
		t.Fatalf("len(rolledUp.Issues) = %d, want 2", len(rolledUp.Issues))
	}
	if progress, ok := rolledUp.Progress[parentID]; !ok || progress.Total != 1 {
		t.Fatalf("expected rolled-up progress for parent %d, got %#v", parentID, rolledUp.Progress[parentID])
	}

	expanded, err := LoadBoard(conn, BoardParams{Expand: true})
	if err != nil {
		t.Fatalf("LoadBoard expand: %v", err)
	}
	if len(expanded.Issues) != 3 {
		t.Fatalf("len(expanded.Issues) = %d, want 3", len(expanded.Issues))
	}
}

func TestGetIssueDetailHydratesAssociatedData(t *testing.T) {
	conn := mustOpenDB(t)

	issueID := createIssue(t, conn, model.Issue{
		Title:    "Feature work",
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindFeature,
	}, []string{"ui"}, []string{"internal/tui/browser.go"})

	childID := createIssue(t, conn, model.Issue{
		ParentID: &issueID,
		Title:    "Child work",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, nil, nil)

	otherID := createIssue(t, conn, model.Issue{
		Title:    "Dependency",
		Status:   model.StatusBacklog,
		Priority: model.PriorityLow,
		Kind:     model.IssueKindTask,
	}, nil, nil)

	if _, err := db.CreateRelation(conn, &model.Relation{
		SourceIssueID: issueID,
		TargetIssueID: otherID,
		RelationType:  model.RelationBlocks,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}

	if _, err := db.CreateComment(conn, &model.Comment{IssueID: issueID, Body: "Looks good", Author: "cam"}); err != nil {
		t.Fatalf("CreateComment: %v", err)
	}

	data, err := GetIssueDetail(conn, issueID)
	if err != nil {
		t.Fatalf("GetIssueDetail: %v", err)
	}

	if data.Issue == nil || data.Issue.ID != issueID {
		t.Fatalf("expected issue %d, got %#v", issueID, data.Issue)
	}
	if len(data.Issue.Labels) != 1 || data.Issue.Labels[0] != "ui" {
		t.Fatalf("expected hydrated labels, got %#v", data.Issue.Labels)
	}
	if len(data.Issue.Files) != 1 || data.Issue.Files[0] != "internal/tui/browser.go" {
		t.Fatalf("expected hydrated files, got %#v", data.Issue.Files)
	}
	if len(data.SubIssues) != 1 || data.SubIssues[0].ID != childID {
		t.Fatalf("expected child issue %d, got %#v", childID, data.SubIssues)
	}
	if len(data.Relations) != 1 {
		t.Fatalf("expected one relation, got %#v", data.Relations)
	}
	if len(data.Comments) != 1 || data.Comments[0].Body != "Looks good" {
		t.Fatalf("expected one comment, got %#v", data.Comments)
	}
	if len(data.Activity) == 0 {
		t.Fatalf("expected activity entries")
	}
}
