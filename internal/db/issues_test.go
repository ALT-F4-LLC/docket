package db

import (
	"database/sql"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
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
	testsupport.Must(t, err, "CreateIssue(%q): %v", title, err)
	// Small sleep to ensure distinct created_at timestamps for tiebreaking.
	time.Sleep(time.Millisecond)
	return id
}

func TestListIssuesDefaultSortOrder(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

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
	testsupport.Must(t, err, "ListIssues: %v", err)
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

// TestListIssuesEmptyIsNotNil pins ListIssues' zero-result contract: an
// empty database returns a non-nil empty slice, not nil.
func TestListIssuesEmptyIsNotNil(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	issues, total, err := ListIssues(db, ListOptions{})
	testsupport.Must(t, err, "ListIssues: %v", err)
	if total != 0 {
		t.Fatalf("total = %d, want 0", total)
	}
	if issues == nil {
		t.Fatalf("ListIssues returned nil, want a non-nil empty slice")
	}
	if len(issues) != 0 {
		t.Fatalf("len(issues) = %d, want 0", len(issues))
	}
}

func TestListIssuesDefaultSortCreatedAtTiebreaker(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

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
	testsupport.Must(t, err, "CreateIssue(older): %v", err)

	// Manually update the created_at of the "older" issue to be clearly in the past.
	pastTime := now.Add(-10 * time.Second).Format(time.RFC3339)
	_, err = db.Exec("UPDATE issues SET created_at = ? WHERE id = ?", pastTime, olderID)
	testsupport.Must(t, err, "updating created_at: %v", err)

	newer := &model.Issue{
		Title:    "newer",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}
	newerID, err := CreateIssue(db, newer, nil, nil)
	testsupport.Must(t, err, "CreateIssue(newer): %v", err)

	issues, _, err := ListIssues(db, ListOptions{})
	testsupport.Must(t, err, "ListIssues: %v", err)
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
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	createTestIssue(t, db, "ip-low", model.StatusInProgress, model.PriorityLow)
	createTestIssue(t, db, "todo-crit", model.StatusTodo, model.PriorityCritical)

	// Explicit sort by priority ascending should put "low" before "critical",
	// regardless of the default status-first ordering.
	issues, _, err := ListIssues(db, ListOptions{
		Sort:    "priority",
		SortDir: "asc",
	})
	testsupport.Must(t, err, "ListIssues: %v", err)
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
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	createTestIssue(t, db, "done-high", model.StatusDone, model.PriorityHigh)
	todoMed := createTestIssue(t, db, "todo-med", model.StatusTodo, model.PriorityMedium)
	ipHigh := createTestIssue(t, db, "ip-high", model.StatusInProgress, model.PriorityHigh)

	// With IncludeDone, done issues should sort last.
	issues, _, err := ListIssues(db, ListOptions{IncludeDone: true})
	testsupport.Must(t, err, "ListIssues: %v", err)
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

// createTestIssueWithParent creates a child issue with the specified parent ID.
func createTestIssueWithParent(t *testing.T, conn *sql.DB, title string, status model.Status, priority model.Priority, parentID int) int {
	t.Helper()
	issue := &model.Issue{
		Title:    title,
		Status:   status,
		Priority: priority,
		Kind:     model.IssueKindTask,
		ParentID: &parentID,
	}
	id, err := CreateIssue(conn, issue, nil, nil)
	testsupport.Must(t, err, "CreateIssue(%q): %v", title, err)
	time.Sleep(time.Millisecond)
	return id
}

func TestListIssues_ParentFetchingPattern(t *testing.T) {
	db := mustOpen(t)
	err := Initialize(db)
	testsupport.Must(t, err, "Initialize: %v", err)

	// Create a parent issue that is in-progress (will be excluded by a "todo" filter).
	parentID := createTestIssue(t, db, "parent-epic", model.StatusInProgress, model.PriorityHigh)

	// Create child issues that are "todo" (will match the filter).
	child1ID := createTestIssueWithParent(t, db, "child-1", model.StatusTodo, model.PriorityHigh, parentID)
	child2ID := createTestIssueWithParent(t, db, "child-2", model.StatusTodo, model.PriorityMedium, parentID)
	child3ID := createTestIssueWithParent(t, db, "child-3", model.StatusTodo, model.PriorityLow, parentID)

	// Filter for "todo" status only -- parent (in-progress) should be excluded.
	issues, total, err := ListIssues(db, ListOptions{
		Statuses: []string{string(model.StatusTodo)},
	})
	testsupport.Must(t, err, "ListIssues: %v", err)
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(issues) != 3 {
		t.Fatalf("len(issues) = %d, want 3", len(issues))
	}

	// Verify only children are returned (parent should not be in the result).
	returnedIDs := make(map[int]bool)
	for _, iss := range issues {
		returnedIDs[iss.ID] = true
	}
	if returnedIDs[parentID] {
		t.Errorf("parent (ID=%d) should NOT be in filtered results, but it is", parentID)
	}
	for _, childID := range []int{child1ID, child2ID, child3ID} {
		if !returnedIDs[childID] {
			t.Errorf("child (ID=%d) should be in filtered results, but it is not", childID)
		}
	}

	// Verify all children reference the parent.
	for _, iss := range issues {
		if iss.ParentID == nil {
			t.Errorf("issue %d should have ParentID set, got nil", iss.ID)
		} else if *iss.ParentID != parentID {
			t.Errorf("issue %d ParentID = %d, want %d", iss.ID, *iss.ParentID, parentID)
		}
	}

	// Now fetch the parent via GetIssuesByIDs -- this is the pattern used by
	// the CLI to get parent headers for children whose parents were excluded by filters.
	parentMap, err := GetIssuesByIDs(db, []int{parentID})
	testsupport.Must(t, err, "GetIssuesByIDs: %v", err)
	if len(parentMap) != 1 {
		t.Fatalf("parentMap length = %d, want 1", len(parentMap))
	}

	parent, ok := parentMap[parentID]
	if !ok {
		t.Fatalf("parentMap missing parent ID %d", parentID)
	}
	if parent.Title != "parent-epic" {
		t.Errorf("parent.Title = %q, want %q", parent.Title, "parent-epic")
	}
	if parent.Status != model.StatusInProgress {
		t.Errorf("parent.Status = %q, want %q", parent.Status, model.StatusInProgress)
	}
}

// seedResolution sets an issue's resolution the way the engine does, outside
// the shared update path, so the update under test sees it as the old value.
func seedResolution(t *testing.T, conn *sql.DB, id int, resolution string) {
	t.Helper()
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	testsupport.Must(t, SetIssueResolutionTx(tx, id, resolution), "SetIssueResolutionTx")
	testsupport.Must(t, tx.Commit(), "Commit")
}

// activityFor returns the issue's activity rows for one field.
func activityFor(t *testing.T, conn *sql.DB, id int, field string) []model.Activity {
	t.Helper()
	rows, err := GetActivity(conn, id, 0)
	testsupport.Must(t, err, "GetActivity: %v", err)
	var out []model.Activity
	for _, a := range rows {
		if a.FieldChanged == field {
			out = append(out, a)
		}
	}
	return out
}

func assertResolutionRow(t *testing.T, rows []model.Activity, oldVal, newVal string) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("resolution activity rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].OldValue != oldVal || rows[0].NewValue != newVal {
		t.Errorf("resolution row = %q -> %q, want %q -> %q", rows[0].OldValue, rows[0].NewValue, oldVal, newVal)
	}
}

func TestUpdateIssue_ImplicitDoneClearRecordsResolutionActivity(t *testing.T) {
	conn := mustOpen(t)
	testsupport.Must(t, Initialize(conn), "Initialize")
	id := createTestIssue(t, conn, "abandoned-todo", model.StatusTodo, model.PriorityMedium)
	seedResolution(t, conn, id, IssueResolutionAbandoned)

	err := UpdateIssue(conn, id, map[string]interface{}{"status": string(model.StatusDone)}, "tester")
	testsupport.Must(t, err, "UpdateIssue: %v", err)

	assertResolutionRow(t, activityFor(t, conn, id, "resolution"), IssueResolutionAbandoned, "")
}

func TestUpdateIssue_ReopenClearRecordsResolutionActivity(t *testing.T) {
	conn := mustOpen(t)
	testsupport.Must(t, Initialize(conn), "Initialize")
	id := createTestIssue(t, conn, "abandoned-done", model.StatusDone, model.PriorityMedium)
	seedResolution(t, conn, id, IssueResolutionAbandoned)

	err := UpdateIssue(conn, id, map[string]interface{}{
		"status":     string(model.StatusBacklog),
		"resolution": "",
	}, "tester")
	testsupport.Must(t, err, "UpdateIssue: %v", err)

	assertResolutionRow(t, activityFor(t, conn, id, "resolution"), IssueResolutionAbandoned, "")
}

func TestUpdateIssue_DoneWithEmptyResolutionRecordsNoResolutionActivity(t *testing.T) {
	conn := mustOpen(t)
	testsupport.Must(t, Initialize(conn), "Initialize")
	id := createTestIssue(t, conn, "plain-todo", model.StatusTodo, model.PriorityMedium)

	err := UpdateIssue(conn, id, map[string]interface{}{"status": string(model.StatusDone)}, "tester")
	testsupport.Must(t, err, "UpdateIssue: %v", err)

	if rows := activityFor(t, conn, id, "status"); len(rows) != 1 {
		t.Fatalf("status activity rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows := activityFor(t, conn, id, "resolution"); len(rows) != 0 {
		t.Errorf("resolution activity rows = %d, want 0: %+v", len(rows), rows)
	}
}

func TestDoneTransitionClearsResolution(t *testing.T) {
	// Every row starts from an issue resolved IssueResolutionAbandoned; the
	// rule decides whether the status write clears it or leaves it standing.
	cases := []struct {
		name               string
		prior, target      model.Status
		explicitResolution bool
		wantClear          bool
	}{
		{"todo moving to done clears the abandoned resolution", model.StatusTodo, model.StatusDone, false, true},
		{"done re-asserting done keeps the abandoned resolution", model.StatusDone, model.StatusDone, false, false},
		{"todo moving to done with an explicit resolution keeps the explicit value", model.StatusTodo, model.StatusDone, true, false},
		{"todo moving to in-progress keeps the abandoned resolution", model.StatusTodo, model.StatusInProgress, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DoneTransitionClearsResolution(c.prior, c.target, c.explicitResolution); got != c.wantClear {
				t.Errorf("DoneTransitionClearsResolution(%q, %q, %v) = %v, want %v",
					c.prior, c.target, c.explicitResolution, got, c.wantClear)
			}
		})
	}
}
