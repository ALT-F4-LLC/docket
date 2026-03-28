package render

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

func TestRenderUIHeaderBarIncludesContext(t *testing.T) {
	rendered := RenderUIHeaderBar("docket", "board", time.Date(2026, 3, 28, 12, 34, 56, 0, time.UTC), 80)

	for _, fragment := range []string{"docket ui", "docket", "BOARD", "READ-ONLY", "12:34:56"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("header missing %q: %q", fragment, rendered)
		}
	}
}

func TestRenderUIFooterBarDoesNotWrapWhenDetailFocused(t *testing.T) {
	rendered := RenderUIFooterBar(false, true, false, 90)
	if strings.Contains(rendered, "\n") {
		t.Fatalf("expected single-line footer when detail is focused, got %q", rendered)
	}
	if !strings.Contains(rendered, "h/l region") {
		t.Fatalf("expected footer hints to remain present, got %q", rendered)
	}
}

func TestRenderUIPaneUsesRequestedDimensions(t *testing.T) {
	rendered := RenderUIPane("Title", "body", 28, 10, false)
	lines := strings.Split(rendered, "\n")
	if got := len(lines); got != 10 {
		t.Fatalf("pane lines = %d, want 10: %q", got, rendered)
	}
	for _, line := range lines {
		if got := lipgloss.Width(line); got != 28 {
			t.Fatalf("pane width = %d, want 28: %q", got, rendered)
		}
	}
}

func TestRenderUIBoardColumnIncludesIssues(t *testing.T) {
	rendered := RenderUIBoardColumn(model.StatusTodo, []*model.Issue{{
		ID:       12,
		Title:    "Ship UI helper refactor",
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
	}}, 40, 8, true, true, 0)

	for _, fragment := range []string{"TODO", "DKT-12", "Ship UI helper refactor"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("board column missing %q: %q", fragment, rendered)
		}
	}
}

func TestRenderUIBoardColumnUsesSingleHeaderLineAtSmallHeights(t *testing.T) {
	rendered := RenderUIBoardColumn(model.StatusTodo, []*model.Issue{{
		ID:       12,
		Title:    "Ship UI helper refactor",
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
	}}, 40, 4, true, true, 0)

	if strings.Count(rendered, "TODO") != 1 {
		t.Fatalf("expected a single TODO header, got %q", rendered)
	}
	if !strings.Contains(rendered, "TODO (1)") {
		t.Fatalf("expected pane title to include issue count: %q", rendered)
	}
	if !strings.Contains(rendered, "DKT-12") {
		t.Fatalf("expected first issue row to remain visible: %q", rendered)
	}
}

func TestRenderUIBoardColumnSelectedRowStaysSingleLine(t *testing.T) {
	rendered := RenderUIBoardColumn(model.StatusTodo, []*model.Issue{{
		ID:       7,
		Title:    "Epic: full read-only docket ui roadmap",
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
	}}, 28, 10, true, true, 0)

	lines := strings.Split(rendered, "\n")
	if len(lines) < 4 {
		t.Fatalf("expected board column output, got %q", rendered)
	}
	if !strings.Contains(lines[2], "DKT-7") {
		t.Fatalf("expected selected issue row on a single line, got %q", rendered)
	}
	if strings.Contains(lines[3], "...") || strings.Contains(lines[3], "roadmap") || strings.Contains(lines[3], "read-only") {
		t.Fatalf("expected title truncation instead of wrapped continuation line, got %q", rendered)
	}
}

func TestRenderUIDetailSubIssueRowIncludesIssueFields(t *testing.T) {
	rendered := RenderUIDetailSubIssueRow(&model.Issue{
		ID:       7,
		Title:    "Nested issue title",
		Status:   model.StatusReview,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, 50, true, true)

	for _, fragment := range []string{"DKT-7", "Nested issue title"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("sub-issue row missing %q: %q", fragment, rendered)
		}
	}
}
