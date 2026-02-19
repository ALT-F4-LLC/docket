package render

import (
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

func intPtr(i int) *int { return &i }

func makeTestIssue(id int, title string, status model.Status, priority model.Priority, kind model.IssueKind, parentID *int) *model.Issue {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &model.Issue{
		ID:        id,
		Title:     title,
		Status:    status,
		Priority:  priority,
		Kind:      kind,
		ParentID:  parentID,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestRenderGroupedTable_AllStandalone(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issues := []*model.Issue{
		makeTestIssue(1, "Task A", model.StatusTodo, model.PriorityHigh, model.IssueKindTask, nil),
		makeTestIssue(2, "Task B", model.StatusInProgress, model.PriorityMedium, model.IssueKindFeature, nil),
		makeTestIssue(3, "Task C", model.StatusTodo, model.PriorityLow, model.IssueKindBug, nil),
	}

	got := RenderGroupedTable(issues, nil, nil)

	// When there are no parent-child relationships and no groups, RenderGroupedTable
	// falls back to RenderTable (flat table). Verify all issue IDs appear.
	for _, id := range []string{"DKT-1", "DKT-2", "DKT-3"} {
		if !strings.Contains(got, id) {
			t.Errorf("expected %s in output, got:\n%s", id, got)
		}
	}

	// Should NOT have grouped section markers since there are no groups.
	if strings.Contains(got, "===") {
		t.Errorf("expected no grouped sections for all-standalone issues, got:\n%s", got)
	}
	if strings.Contains(got, "Standalone Issues") {
		t.Errorf("expected no 'Standalone Issues' header for all-standalone issues, got:\n%s", got)
	}
}

func TestRenderGroupedTable_SingleParentWithChildren(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	parent := makeTestIssue(1, "Epic: Build Feature", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)
	child1 := makeTestIssue(2, "Subtask A", model.StatusTodo, model.PriorityHigh, model.IssueKindTask, intPtr(1))
	child2 := makeTestIssue(3, "Subtask B", model.StatusDone, model.PriorityMedium, model.IssueKindTask, intPtr(1))
	child3 := makeTestIssue(4, "Subtask C", model.StatusTodo, model.PriorityLow, model.IssueKindTask, intPtr(1))

	issues := []*model.Issue{parent, child1, child2, child3}

	progress := map[int]SubIssueProgress{
		1: {Done: 1, Total: 3},
	}

	got := RenderGroupedTable(issues, nil, progress)

	// Parent should appear as section header.
	if !strings.Contains(got, "DKT-1") {
		t.Errorf("expected parent DKT-1 in output, got:\n%s", got)
	}
	if !strings.Contains(got, "Epic: Build Feature") {
		t.Errorf("expected parent title in output, got:\n%s", got)
	}

	// All children should appear.
	for _, id := range []string{"DKT-2", "DKT-3", "DKT-4"} {
		if !strings.Contains(got, id) {
			t.Errorf("expected child %s in output, got:\n%s", id, got)
		}
	}

	// Progress should be displayed.
	if !strings.Contains(got, "(1/3 done)") {
		t.Errorf("expected progress '(1/3 done)' in output, got:\n%s", got)
	}

	// Children should be inside a bordered table section.
	if !strings.Contains(got, "┌") || !strings.Contains(got, "└") {
		t.Errorf("expected bordered table section, got:\n%s", got)
	}
	// Child rows should be inside the box (prefixed with │).
	lines := strings.Split(got, "\n")
	foundChildInBox := false
	for _, line := range lines {
		if strings.Contains(line, "DKT-2") && strings.Contains(line, "│") {
			foundChildInBox = true
			break
		}
	}
	if !foundChildInBox {
		t.Errorf("expected children inside bordered table, got:\n%s", got)
	}
}

func TestRenderGroupedTable_MultipleGroups(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	parent1 := makeTestIssue(1, "Epic A", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)
	child1a := makeTestIssue(2, "Child A1", model.StatusTodo, model.PriorityHigh, model.IssueKindTask, intPtr(1))
	child1b := makeTestIssue(3, "Child A2", model.StatusTodo, model.PriorityMedium, model.IssueKindTask, intPtr(1))

	parent2 := makeTestIssue(4, "Epic B", model.StatusTodo, model.PriorityMedium, model.IssueKindEpic, nil)
	child2a := makeTestIssue(5, "Child B1", model.StatusTodo, model.PriorityHigh, model.IssueKindTask, intPtr(4))

	standalone := makeTestIssue(6, "Standalone Task", model.StatusTodo, model.PriorityLow, model.IssueKindTask, nil)

	issues := []*model.Issue{parent1, child1a, child1b, parent2, child2a, standalone}

	progress := map[int]SubIssueProgress{
		1: {Done: 0, Total: 2},
		4: {Done: 0, Total: 1},
	}

	got := RenderGroupedTable(issues, nil, progress)

	// Both parent groups should appear.
	if !strings.Contains(got, "DKT-1") {
		t.Errorf("expected parent DKT-1 in output, got:\n%s", got)
	}
	if !strings.Contains(got, "DKT-4") {
		t.Errorf("expected parent DKT-4 in output, got:\n%s", got)
	}

	// Standalone section should appear.
	if !strings.Contains(got, "Standalone Issues") {
		t.Errorf("expected 'Standalone Issues' section in output, got:\n%s", got)
	}

	// Standalone issue should be present.
	if !strings.Contains(got, "DKT-6") {
		t.Errorf("expected standalone DKT-6 in output, got:\n%s", got)
	}

	// Parent1 (in-progress/high) should appear before parent2 (todo/medium) based on rank.
	idx1 := strings.Index(got, "Epic A")
	idx2 := strings.Index(got, "Epic B")
	if idx1 < 0 || idx2 < 0 {
		t.Fatalf("missing parent titles in output:\n%s", got)
	}
	if idx1 >= idx2 {
		t.Errorf("expected Epic A (in-progress) before Epic B (todo), got Epic A at %d, Epic B at %d", idx1, idx2)
	}
}

func TestRenderGroupedTable_FilteredParentInMap(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	// Parent is NOT in the issues result set (simulates a filter excluding the parent).
	child1 := makeTestIssue(2, "Child 1", model.StatusTodo, model.PriorityHigh, model.IssueKindTask, intPtr(1))
	child2 := makeTestIssue(3, "Child 2", model.StatusTodo, model.PriorityMedium, model.IssueKindTask, intPtr(1))

	issues := []*model.Issue{child1, child2}

	// Parent provided via parentMap (fetched separately).
	parentIssue := makeTestIssue(1, "Filtered Parent Epic", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)
	parentMap := map[int]*model.Issue{
		1: parentIssue,
	}

	progress := map[int]SubIssueProgress{
		1: {Done: 0, Total: 2},
	}

	got := RenderGroupedTable(issues, parentMap, progress)

	// The parent header should appear even though it's not in the issues slice.
	if !strings.Contains(got, "DKT-1") {
		t.Errorf("expected parent DKT-1 from parentMap in output, got:\n%s", got)
	}
	if !strings.Contains(got, "Filtered Parent Epic") {
		t.Errorf("expected parent title from parentMap in output, got:\n%s", got)
	}

	// Children should appear.
	if !strings.Contains(got, "DKT-2") {
		t.Errorf("expected child DKT-2 in output, got:\n%s", got)
	}
	if !strings.Contains(got, "DKT-3") {
		t.Errorf("expected child DKT-3 in output, got:\n%s", got)
	}

	// Progress should be displayed.
	if !strings.Contains(got, "(0/2 done)") {
		t.Errorf("expected progress '(0/2 done)' in output, got:\n%s", got)
	}
}

func TestRenderGroupedTable_EmptyList(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	got := RenderGroupedTable(nil, nil, nil)
	if !strings.Contains(got, "No issues found.") {
		t.Errorf("expected empty state message, got:\n%s", got)
	}

	got = RenderGroupedTable([]*model.Issue{}, nil, nil)
	if !strings.Contains(got, "No issues found.") {
		t.Errorf("expected empty state message for empty slice, got:\n%s", got)
	}
}

func TestRenderGroupedTable_ParentNoChildrenInResult(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	// A parent issue is in the result set, but none of its children are (e.g., all
	// children are filtered out). The parent should appear as standalone, not as an
	// empty group.
	parent := makeTestIssue(1, "Parent With No Children Here", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)
	standalone := makeTestIssue(2, "Some Other Task", model.StatusTodo, model.PriorityMedium, model.IssueKindTask, nil)

	issues := []*model.Issue{parent, standalone}

	got := RenderGroupedTable(issues, nil, nil)

	// Since there are no parent-child groups, it should fall back to flat table.
	if !strings.Contains(got, "DKT-1") {
		t.Errorf("expected DKT-1 in output, got:\n%s", got)
	}
	if !strings.Contains(got, "DKT-2") {
		t.Errorf("expected DKT-2 in output, got:\n%s", got)
	}

	// Should NOT have grouped section markers (no groups formed).
	if strings.Contains(got, "===") && strings.Contains(got, "Standalone Issues") {
		t.Errorf("expected flat table when parent has no children in result, got:\n%s", got)
	}
}

func TestRenderGroupedTable_ProgressDisplay(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	parent := makeTestIssue(1, "Parent Task", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)
	child1 := makeTestIssue(2, "Done Child", model.StatusDone, model.PriorityHigh, model.IssueKindTask, intPtr(1))
	child2 := makeTestIssue(3, "Done Child 2", model.StatusDone, model.PriorityMedium, model.IssueKindTask, intPtr(1))
	child3 := makeTestIssue(4, "Todo Child", model.StatusTodo, model.PriorityLow, model.IssueKindTask, intPtr(1))

	issues := []*model.Issue{parent, child1, child2, child3}

	progress := map[int]SubIssueProgress{
		1: {Done: 2, Total: 3},
	}

	got := RenderGroupedTable(issues, nil, progress)

	// Verify progress indicator format.
	if !strings.Contains(got, "(2/3 done)") {
		t.Errorf("expected '(2/3 done)' progress on parent header, got:\n%s", got)
	}
}

func TestRenderGroupedTable_ProgressNotShownWhenNil(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	parent := makeTestIssue(1, "Parent Task", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)
	child := makeTestIssue(2, "Child Task", model.StatusTodo, model.PriorityMedium, model.IssueKindTask, intPtr(1))

	issues := []*model.Issue{parent, child}

	// nil progress map.
	got := RenderGroupedTable(issues, nil, nil)

	// Should still render the group, just without progress.
	if !strings.Contains(got, "DKT-1") {
		t.Errorf("expected DKT-1 in output, got:\n%s", got)
	}
	// No progress text should appear.
	if strings.Contains(got, "done)") {
		t.Errorf("expected no progress indicator when progress map is nil, got:\n%s", got)
	}
}

func TestRenderGroupedTable_ProgressNotShownWhenEmpty(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	parent := makeTestIssue(1, "Parent Task", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)
	child := makeTestIssue(2, "Child Task", model.StatusTodo, model.PriorityMedium, model.IssueKindTask, intPtr(1))

	issues := []*model.Issue{parent, child}

	// Empty progress map (no entry for this parent).
	got := RenderGroupedTable(issues, nil, map[int]SubIssueProgress{})

	// Should still render the group, just without progress.
	if !strings.Contains(got, "DKT-1") {
		t.Errorf("expected DKT-1 in output, got:\n%s", got)
	}
	if strings.Contains(got, "done)") {
		t.Errorf("expected no progress indicator when progress map has no entry, got:\n%s", got)
	}
}

func TestRenderGroupedTable_ColorPathExecutes(t *testing.T) {
	// Call the color rendering functions directly to verify they don't panic.
	parent := makeTestIssue(1, "Color Parent", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)
	child := makeTestIssue(2, "Color Child", model.StatusTodo, model.PriorityMedium, model.IssueKindTask, intPtr(1))

	issues := []*model.Issue{parent, child}
	standalone := []*model.Issue{}

	progress := map[int]SubIssueProgress{
		1: {Done: 0, Total: 1},
	}

	groups := []parentGroup{
		{parent: parent, children: []*model.Issue{child}},
	}

	got := renderGroupedColorTable(groups, standalone, progress)
	if got == "" {
		t.Error("expected non-empty output from renderGroupedColorTable")
	}

	// Also test renderColorChildTable directly.
	childTable := renderColorChildTable(issues, false)
	if childTable == "" {
		t.Error("expected non-empty output from renderColorChildTable")
	}
}

func TestRenderGroupedTable_ChildOrderingWithinGroup(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	parent := makeTestIssue(1, "Parent", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)

	// Children have different statuses and priorities. After sorting by rank:
	// - child2 (in-progress/high) should come first
	// - child1 (todo/high) should come second
	// - child3 (todo/low) should come third
	child1 := makeTestIssue(2, "Child Todo High", model.StatusTodo, model.PriorityHigh, model.IssueKindTask, intPtr(1))
	child2 := makeTestIssue(3, "Child InProgress High", model.StatusInProgress, model.PriorityHigh, model.IssueKindTask, intPtr(1))
	child3 := makeTestIssue(4, "Child Todo Low", model.StatusTodo, model.PriorityLow, model.IssueKindTask, intPtr(1))

	issues := []*model.Issue{parent, child1, child2, child3}

	got := RenderGroupedTable(issues, nil, nil)

	// Verify child ordering: in-progress before todo, high before low.
	idxIP := strings.Index(got, "Child InProgress High")
	idxTodoHigh := strings.Index(got, "Child Todo High")
	idxTodoLow := strings.Index(got, "Child Todo Low")

	if idxIP < 0 || idxTodoHigh < 0 || idxTodoLow < 0 {
		t.Fatalf("missing child titles in output:\n%s", got)
	}

	if idxIP >= idxTodoHigh {
		t.Errorf("expected in-progress child before todo/high child, got positions: ip=%d, todo-high=%d", idxIP, idxTodoHigh)
	}
	if idxTodoHigh >= idxTodoLow {
		t.Errorf("expected todo/high child before todo/low child, got positions: todo-high=%d, todo-low=%d", idxTodoHigh, idxTodoLow)
	}
}

func TestRenderGroupedTable_PlainHeaderFormat(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	parent := makeTestIssue(1, "My Epic", model.StatusInProgress, model.PriorityHigh, model.IssueKindEpic, nil)
	child := makeTestIssue(2, "A Task", model.StatusTodo, model.PriorityMedium, model.IssueKindTask, intPtr(1))

	issues := []*model.Issue{parent, child}

	progress := map[int]SubIssueProgress{
		1: {Done: 1, Total: 2},
	}

	got := RenderGroupedTable(issues, nil, progress)

	// Plain text header should use a bordered title box.
	if !strings.Contains(got, "┌") || !strings.Contains(got, "┐") {
		t.Errorf("expected bordered title box in header, got:\n%s", got)
	}

	// Header should contain parent kind icon, ID, title, status, and priority.
	if !strings.Contains(got, model.IssueKindEpic.Icon()) {
		t.Errorf("expected epic icon in header, got:\n%s", got)
	}
	if !strings.Contains(got, "DKT-1") {
		t.Errorf("expected DKT-1 in header, got:\n%s", got)
	}
	if !strings.Contains(got, "My Epic") {
		t.Errorf("expected parent title in header, got:\n%s", got)
	}
	if !strings.Contains(got, string(model.StatusInProgress)) {
		t.Errorf("expected status in header, got:\n%s", got)
	}
	if !strings.Contains(got, string(model.PriorityHigh)) {
		t.Errorf("expected priority in header, got:\n%s", got)
	}
	if !strings.Contains(got, "(1/2 done)") {
		t.Errorf("expected progress in header, got:\n%s", got)
	}
}

func TestRenderGroupedTable_MixedParentNotInIssuesOrMap(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	// Children reference a parent that is neither in the issues slice nor in parentMap.
	// These children should be treated as standalone.
	child1 := makeTestIssue(2, "Orphan Child 1", model.StatusTodo, model.PriorityHigh, model.IssueKindTask, intPtr(99))
	child2 := makeTestIssue(3, "Orphan Child 2", model.StatusTodo, model.PriorityMedium, model.IssueKindTask, intPtr(99))
	standalone := makeTestIssue(4, "Normal Task", model.StatusTodo, model.PriorityLow, model.IssueKindTask, nil)

	issues := []*model.Issue{child1, child2, standalone}

	got := RenderGroupedTable(issues, nil, nil)

	// Since the parent (99) is not available, children fall to standalone.
	// With no valid groups, it should fall back to a flat table.
	if !strings.Contains(got, "DKT-2") {
		t.Errorf("expected DKT-2 in output, got:\n%s", got)
	}
	if !strings.Contains(got, "DKT-3") {
		t.Errorf("expected DKT-3 in output, got:\n%s", got)
	}
	if !strings.Contains(got, "DKT-4") {
		t.Errorf("expected DKT-4 in output, got:\n%s", got)
	}
}
