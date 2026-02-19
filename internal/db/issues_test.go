package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// createTestIssue is a helper that creates an issue with the given status and
// priority, sleeping 1ms between calls to guarantee distinct created_at values.
func createTestIssue(t *testing.T, conn *sql.DB, title string, status model.Status, priority model.Priority) int {
	t.Helper()
	issue := &model.Issue{
		Title:    title,
		Status:   status,
		Priority: priority,
		Kind:     model.IssueKindTask,
	}
	id, err := CreateIssue(conn, issue, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue(%q): %v", title, err)
	}
	// Small sleep to ensure distinct created_at timestamps for tiebreaking.
	time.Sleep(time.Millisecond)
	return id
}

func TestListIssuesDefaultSortOrder(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Create issues in an order that is intentionally NOT the expected output
	// order, so the test can verify the sort is actually applied.
	//
	// Expected output order (status rank, then priority rank, then created_at DESC):
	//   1. in-progress / critical  (id=ipCrit)
	//   2. in-progress / medium    (id=ipMed)
	//   3. todo / high             (id=todoHigh)
	//   4. todo / low              (id=todoLow)
	//   5. backlog / high          (id=blogHigh)
	//   6. backlog / medium        (id=blogMed)

	// Create them out-of-order.
	blogMed := createTestIssue(t, db, "backlog-med", model.StatusBacklog, model.PriorityMedium)
	todoLow := createTestIssue(t, db, "todo-low", model.StatusTodo, model.PriorityLow)
	ipMed := createTestIssue(t, db, "ip-med", model.StatusInProgress, model.PriorityMedium)
	blogHigh := createTestIssue(t, db, "backlog-high", model.StatusBacklog, model.PriorityHigh)
	todoHigh := createTestIssue(t, db, "todo-high", model.StatusTodo, model.PriorityHigh)
	ipCrit := createTestIssue(t, db, "ip-crit", model.StatusInProgress, model.PriorityCritical)

	// Default sort: no Sort field set.
	issues, total, err := ListIssues(db, ListOptions{IncludeDone: false})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if total != 6 {
		t.Fatalf("total = %d, want 6", total)
	}
	if len(issues) != 6 {
		t.Fatalf("len(issues) = %d, want 6", len(issues))
	}

	wantOrder := []int{ipCrit, ipMed, todoHigh, todoLow, blogHigh, blogMed}
	for i, want := range wantOrder {
		if issues[i].ID != want {
			// Build a readable description of what we got.
			var gotIDs []int
			for _, iss := range issues {
				gotIDs = append(gotIDs, iss.ID)
			}
			t.Fatalf("issues[%d].ID = %d, want %d\n  got order:  %v\n  want order: %v",
				i, issues[i].ID, want, gotIDs, wantOrder)
		}
	}
}

func TestListIssuesDefaultSortCreatedAtTiebreaker(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Insert two issues with the same status and priority but explicit timestamps
	// so we can verify the created_at DESC tiebreaker. CreateIssue uses
	// time.RFC3339 (second precision), so we need timestamps at least 1 second apart.
	now := time.Now().UTC()
	older := &model.Issue{
		Title:    "older",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}
	olderID, err := CreateIssue(db, older, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue(older): %v", err)
	}

	// Manually update the created_at of the "older" issue to be clearly in the past.
	pastTime := now.Add(-10 * time.Second).Format(time.RFC3339)
	if _, err := db.Exec("UPDATE issues SET created_at = ? WHERE id = ?", pastTime, olderID); err != nil {
		t.Fatalf("updating created_at: %v", err)
	}

	newer := &model.Issue{
		Title:    "newer",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}
	newerID, err := CreateIssue(db, newer, nil, nil)
	if err != nil {
		t.Fatalf("CreateIssue(newer): %v", err)
	}

	issues, _, err := ListIssues(db, ListOptions{})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2", len(issues))
	}
	if issues[0].ID != newerID || issues[1].ID != olderID {
		t.Errorf("expected newer (%d) before older (%d), got [%d, %d]",
			newerID, olderID, issues[0].ID, issues[1].ID)
	}
}

func TestListIssuesExplicitSortOverridesDefault(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	createTestIssue(t, db, "ip-low", model.StatusInProgress, model.PriorityLow)
	createTestIssue(t, db, "todo-crit", model.StatusTodo, model.PriorityCritical)

	// Explicit sort by priority ascending should put "low" before "critical",
	// regardless of the default status-first ordering.
	issues, _, err := ListIssues(db, ListOptions{
		Sort:    "priority",
		SortDir: "asc",
	})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2", len(issues))
	}
	// SQLite sorts "critical" < "low" alphabetically in ASC, so priority:asc
	// yields critical first, low second.
	if issues[0].Priority != model.PriorityCritical {
		t.Errorf("explicit sort priority:asc — issues[0].Priority = %q, want critical", issues[0].Priority)
	}
	if issues[1].Priority != model.PriorityLow {
		t.Errorf("explicit sort priority:asc — issues[1].Priority = %q, want low", issues[1].Priority)
	}
}

func TestListIssuesDefaultSortWithDoneStatus(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	createTestIssue(t, db, "done-high", model.StatusDone, model.PriorityHigh)
	todoMed := createTestIssue(t, db, "todo-med", model.StatusTodo, model.PriorityMedium)
	ipHigh := createTestIssue(t, db, "ip-high", model.StatusInProgress, model.PriorityHigh)

	// With IncludeDone, done issues should sort last.
	issues, _, err := ListIssues(db, ListOptions{IncludeDone: true})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 3 {
		t.Fatalf("len = %d, want 3", len(issues))
	}
	// in-progress first, then todo, then done.
	wantOrder := []int{ipHigh, todoMed}
	for i, want := range wantOrder {
		if issues[i].ID != want {
			t.Errorf("issues[%d].ID = %d, want %d", i, issues[i].ID, want)
		}
	}
	if issues[2].Status != model.StatusDone {
		t.Errorf("issues[2].Status = %q, want done", issues[2].Status)
	}
}
