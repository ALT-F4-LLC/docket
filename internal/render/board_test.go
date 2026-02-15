package render

import (
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// makeIssue creates a minimal issue for testing.
func makeIssue(id int, title string, status model.Status, priority model.Priority) *model.Issue {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &model.Issue{
		ID:        id,
		Title:     title,
		Status:    status,
		Priority:  priority,
		Kind:      model.IssueKindTask,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestRenderBoardEmpty(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	got := RenderBoard(nil, BoardOptions{})
	if got != "" {
		t.Errorf("RenderBoard(nil) = %q, want empty string", got)
	}

	got = RenderBoard([]*model.Issue{}, BoardOptions{})
	if got != "" {
		t.Errorf("RenderBoard([]) = %q, want empty string", got)
	}
}

func TestRenderPlainBoardGroupsByStatus(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issues := []*model.Issue{
		makeIssue(1, "Task A", model.StatusTodo, model.PriorityHigh),
		makeIssue(2, "Task B", model.StatusDone, model.PriorityLow),
		makeIssue(3, "Task C", model.StatusTodo, model.PriorityMedium),
	}

	got := RenderBoard(issues, BoardOptions{})

	// Should have TODO column with 2 issues
	if !strings.Contains(got, "=== TODO (2) ===") {
		t.Errorf("expected TODO column with 2 issues, got:\n%s", got)
	}
	// Should have DONE column with 1 issue
	if !strings.Contains(got, "=== DONE (1) ===") {
		t.Errorf("expected DONE column with 1 issue, got:\n%s", got)
	}
	// Should NOT have BACKLOG, IN-PROGRESS, or REVIEW columns (no issues in those)
	for _, status := range []string{"BACKLOG", "IN-PROGRESS", "REVIEW"} {
		if strings.Contains(got, "=== "+status) {
			t.Errorf("should not have %s column when no issues have that status, got:\n%s", status, got)
		}
	}
}

func TestRenderPlainBoardColumnOrder(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issues := []*model.Issue{
		makeIssue(1, "Done task", model.StatusDone, model.PriorityNone),
		makeIssue(2, "Backlog task", model.StatusBacklog, model.PriorityNone),
		makeIssue(3, "Review task", model.StatusReview, model.PriorityNone),
		makeIssue(4, "Todo task", model.StatusTodo, model.PriorityNone),
		makeIssue(5, "InProgress task", model.StatusInProgress, model.PriorityNone),
	}

	got := RenderBoard(issues, BoardOptions{})

	// Verify column order: backlog < todo < in-progress < review < done
	backlogIdx := strings.Index(got, "=== BACKLOG")
	todoIdx := strings.Index(got, "=== TODO")
	inProgressIdx := strings.Index(got, "=== IN-PROGRESS")
	reviewIdx := strings.Index(got, "=== REVIEW")
	doneIdx := strings.Index(got, "=== DONE")

	if backlogIdx < 0 || todoIdx < 0 || inProgressIdx < 0 || reviewIdx < 0 || doneIdx < 0 {
		t.Fatalf("missing column headers in output:\n%s", got)
	}
	if !(backlogIdx < todoIdx && todoIdx < inProgressIdx && inProgressIdx < reviewIdx && reviewIdx < doneIdx) {
		t.Errorf("columns not in expected order (backlog=%d, todo=%d, in-progress=%d, review=%d, done=%d)",
			backlogIdx, todoIdx, inProgressIdx, reviewIdx, doneIdx)
	}
}

func TestRenderPlainBoardTitleTruncation(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	longTitle := strings.Repeat("A", 60)
	issues := []*model.Issue{
		makeIssue(1, longTitle, model.StatusTodo, model.PriorityMedium),
	}

	got := RenderBoard(issues, BoardOptions{})

	// The plain-text card uses truncate(title, maxTitleWidth=40), so titles >40 chars
	// should be truncated with "..."
	if strings.Contains(got, longTitle) {
		t.Error("expected long title to be truncated, but found full title in output")
	}
	if !strings.Contains(got, "...") {
		t.Error("expected truncated title to contain ellipsis (...)")
	}
}

func TestRenderPlainBoardPriorityIndicators(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issues := []*model.Issue{
		makeIssue(1, "Critical task", model.StatusTodo, model.PriorityCritical),
		makeIssue(2, "High task", model.StatusTodo, model.PriorityHigh),
		makeIssue(3, "Medium task", model.StatusTodo, model.PriorityMedium),
		makeIssue(4, "Low task", model.StatusTodo, model.PriorityLow),
		makeIssue(5, "No-pri task", model.StatusTodo, model.PriorityNone),
	}

	got := RenderBoard(issues, BoardOptions{})

	// Plain-text cards render priority as "[priority]"
	for _, pri := range []string{"[critical]", "[high]", "[medium]", "[low]", "[none]"} {
		if !strings.Contains(got, pri) {
			t.Errorf("expected priority indicator %q in output, got:\n%s", pri, got)
		}
	}
}

func TestRenderPlainBoardSubIssueProgress(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issues := []*model.Issue{
		makeIssue(1, "Parent task", model.StatusTodo, model.PriorityMedium),
	}

	progress := map[int]SubIssueProgress{
		1: {Done: 3, Total: 5},
	}

	got := RenderBoard(issues, BoardOptions{Progress: progress})

	if !strings.Contains(got, "Sub: 3/5 done") {
		t.Errorf("expected sub-issue progress 'Sub: 3/5 done' in output, got:\n%s", got)
	}
}

func TestRenderPlainBoardNoProgressWhenNil(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issues := []*model.Issue{
		makeIssue(1, "Regular task", model.StatusTodo, model.PriorityMedium),
	}

	got := RenderBoard(issues, BoardOptions{Progress: nil})

	if strings.Contains(got, "Sub:") {
		t.Errorf("expected no sub-issue progress line when Progress is nil, got:\n%s", got)
	}
}

func TestRenderPlainBoardOverflow(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	// Create 13 issues in the same status column
	var issues []*model.Issue
	for i := 1; i <= 13; i++ {
		issues = append(issues, makeIssue(i, "Task", model.StatusTodo, model.PriorityMedium))
	}

	got := RenderBoard(issues, BoardOptions{})

	// Should show count of 13
	if !strings.Contains(got, "=== TODO (13) ===") {
		t.Errorf("expected TODO (13) header, got:\n%s", got)
	}
	// Should show "+3 more" (13 - maxCardsPerColumn=10 = 3 overflow)
	if !strings.Contains(got, "+3 more") {
		t.Errorf("expected '+3 more' overflow indicator, got:\n%s", got)
	}
}

func TestRenderPlainBoardExactlyMaxCards(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	// Create exactly maxCardsPerColumn (10) issues
	var issues []*model.Issue
	for i := 1; i <= 10; i++ {
		issues = append(issues, makeIssue(i, "Task", model.StatusTodo, model.PriorityMedium))
	}

	got := RenderBoard(issues, BoardOptions{})

	// Should NOT show overflow indicator
	if strings.Contains(got, "more") {
		t.Errorf("expected no overflow indicator for exactly 10 issues, got:\n%s", got)
	}
}

func TestRenderPlainBoardAllIssuesOneStatus(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issues := []*model.Issue{
		makeIssue(1, "Task A", model.StatusInProgress, model.PriorityHigh),
		makeIssue(2, "Task B", model.StatusInProgress, model.PriorityLow),
	}

	got := RenderBoard(issues, BoardOptions{})

	// Should only have IN-PROGRESS column
	if !strings.Contains(got, "=== IN-PROGRESS (2) ===") {
		t.Errorf("expected IN-PROGRESS column with 2 issues, got:\n%s", got)
	}
	// Should not have other columns
	for _, status := range []string{"BACKLOG", "TODO", "REVIEW", "DONE"} {
		if strings.Contains(got, "=== "+status) {
			t.Errorf("should not have %s column, got:\n%s", status, got)
		}
	}
}

func TestRenderPlainBoardIssueWithNoLabels(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issue := makeIssue(1, "No labels", model.StatusTodo, model.PriorityMedium)
	// Labels is nil by default from makeIssue
	got := RenderBoard([]*model.Issue{issue}, BoardOptions{})

	// Should have the issue ID and title
	if !strings.Contains(got, "DKT-1") {
		t.Errorf("expected DKT-1 in output, got:\n%s", got)
	}
	if !strings.Contains(got, "No labels") {
		t.Errorf("expected title in output, got:\n%s", got)
	}
}

func TestRenderPlainBoardIssueWithLabels(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issue := makeIssue(1, "With labels", model.StatusTodo, model.PriorityMedium)
	issue.Labels = []string{"bug", "frontend"}
	got := RenderBoard([]*model.Issue{issue}, BoardOptions{})

	if !strings.Contains(got, "bug, frontend") {
		t.Errorf("expected labels 'bug, frontend' in output, got:\n%s", got)
	}
}

func TestRenderBoardColorPathExecutes(t *testing.T) {
	// The color path uses lipgloss which respects the TERM env var.
	// We cannot truly unset NO_COLOR with t.Setenv (it only sets, never unsets).
	// Instead, we test the color rendering functions directly.
	issues := []*model.Issue{
		makeIssue(1, "Task A", model.StatusTodo, model.PriorityHigh),
		makeIssue(2, "Task B", model.StatusDone, model.PriorityLow),
	}

	progress := map[int]SubIssueProgress{
		1: {Done: 1, Total: 3},
	}

	// Call renderColorBoard directly to exercise the color path.
	got := renderColorBoard(issues, BoardOptions{Progress: progress})
	if got == "" {
		t.Error("expected non-empty output from color board render")
	}
}

func TestRenderPlainBoardExpandFlag(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	// The Expand flag is stored in BoardOptions but doesn't change
	// renderPlainBoard output directly -- it controls sub-issue filtering
	// at the command level (cmd/docket/board.go). The render layer just
	// receives the final filtered issue list. Verify the flag is accepted.
	parentID := 1
	issues := []*model.Issue{
		makeIssue(1, "Parent", model.StatusTodo, model.PriorityMedium),
		{
			ID:        2,
			ParentID:  &parentID,
			Title:     "Child",
			Status:    model.StatusTodo,
			Priority:  model.PriorityLow,
			Kind:      model.IssueKindTask,
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	got := RenderBoard(issues, BoardOptions{Expand: true})
	// With expand, both parent and child appear
	if !strings.Contains(got, "DKT-1") || !strings.Contains(got, "DKT-2") {
		t.Errorf("expected both DKT-1 and DKT-2 in expanded output, got:\n%s", got)
	}
}

func TestGroupByStatus(t *testing.T) {
	issues := []*model.Issue{
		makeIssue(1, "A", model.StatusTodo, model.PriorityMedium),
		makeIssue(2, "B", model.StatusDone, model.PriorityMedium),
		makeIssue(3, "C", model.StatusTodo, model.PriorityMedium),
	}

	groups := groupByStatus(issues)

	if len(groups[model.StatusTodo]) != 2 {
		t.Errorf("expected 2 todo issues, got %d", len(groups[model.StatusTodo]))
	}
	if len(groups[model.StatusDone]) != 1 {
		t.Errorf("expected 1 done issue, got %d", len(groups[model.StatusDone]))
	}
	if len(groups[model.StatusBacklog]) != 0 {
		t.Errorf("expected 0 backlog issues, got %d", len(groups[model.StatusBacklog]))
	}
}

func TestFormatProgressBar(t *testing.T) {
	tests := []struct {
		name     string
		done     int
		total    int
		maxWidth int
		wantSub  string // substring that must appear
	}{
		{
			name:     "partial progress",
			done:     3,
			total:    5,
			maxWidth: 30,
			wantSub:  "3/5",
		},
		{
			name:     "all done",
			done:     5,
			total:    5,
			maxWidth: 30,
			wantSub:  "5/5",
		},
		{
			name:     "none done",
			done:     0,
			total:    5,
			maxWidth: 30,
			wantSub:  "0/5",
		},
		{
			name:     "narrow width falls back",
			done:     1,
			total:    2,
			maxWidth: 5,
			wantSub:  "Sub: 1/2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatProgressBar(tt.done, tt.total, tt.maxWidth)
			if !strings.Contains(got, tt.wantSub) {
				t.Errorf("formatProgressBar(%d, %d, %d) = %q, want substring %q",
					tt.done, tt.total, tt.maxWidth, got, tt.wantSub)
			}
			if !strings.HasPrefix(got, "Sub: ") {
				t.Errorf("formatProgressBar output should start with 'Sub: ', got %q", got)
			}
		})
	}
}

func TestRenderPlainBoardCardFormat(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	issue := makeIssue(1, "My Test Task", model.StatusBacklog, model.PriorityCritical)
	issue.Labels = []string{"urgent"}
	got := RenderBoard([]*model.Issue{issue}, BoardOptions{})

	// Verify card contains expected elements
	lines := strings.Split(got, "\n")

	var foundID, foundTitle, foundLabel bool
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "DKT-1") && strings.Contains(trimmed, "[critical]") {
			foundID = true
		}
		if strings.Contains(trimmed, "My Test Task") {
			foundTitle = true
		}
		if strings.Contains(trimmed, "urgent") {
			foundLabel = true
		}
	}

	if !foundID {
		t.Errorf("expected 'DKT-1 [critical]' line in card, got:\n%s", got)
	}
	if !foundTitle {
		t.Errorf("expected title 'My Test Task' in card, got:\n%s", got)
	}
	if !foundLabel {
		t.Errorf("expected label 'urgent' in card, got:\n%s", got)
	}
}
