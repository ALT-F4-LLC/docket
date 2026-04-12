package render

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

func TestRenderUIHeaderBarIncludesContext(t *testing.T) {
	rendered := RenderUIHeaderBar("docket", "list", RefreshStatus{Enabled: true, Interval: 5 * time.Second, LastSuccess: time.Date(2026, 3, 28, 12, 34, 56, 0, time.UTC)}, 96, "SORT ID DESC")

	for _, fragment := range []string{"docket tui", "docket", "LIST", "READ-ONLY", "SORT ID DESC", "12:34:56"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("header missing %q: %q", fragment, rendered)
		}
	}
}

func TestRenderUIFooterBarWrapsInsteadOfTruncatingWhenDetailFocused(t *testing.T) {
	rendered := RenderUIFooterBar(false, true, false, RefreshStatus{Enabled: true, Interval: 5 * time.Second, LastSuccess: time.Date(2026, 3, 28, 12, 34, 56, 0, time.UTC)}, 90)
	if !strings.Contains(rendered, "\n") {
		t.Fatalf("expected wrapped footer when detail is focused at narrow widths, got %q", rendered)
	}
	for _, fragment := range []string{"h/l region", "enter open/zoom", "ctrl+u/d page", "p pause", "12:34:56"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected wrapped footer to include %q, got %q", fragment, rendered)
		}
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
	}}, nil, 40, 8, true, true, 0)

	for _, fragment := range []string{"TODO (1)", "DKT-12", "Ship UI..."} {
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
	}}, nil, 40, 4, true, true, 0)

	if strings.Count(rendered, "TODO (1)") != 1 {
		t.Fatalf("expected a single TODO column title, got %q", rendered)
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
	}}, nil, 28, 10, true, true, 0)

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

func TestRenderUIListRowKeepsMetadataSingleLineAtConstrainedWidth(t *testing.T) {
	rendered := RenderUIListRow(&model.Issue{
		ID:       14,
		Title:    "Render richer list metadata without wrapping rows",
		Status:   model.StatusInProgress,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindFeature,
	}, HierarchyDecoration{}, 34, true)

	if strings.Contains(rendered, "\n") {
		t.Fatalf("expected single-line list row, got %q", rendered)
	}
	for _, fragment := range []string{"DKT-14", "INPROG", "↑", "FEATURE"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected list row to include %q: %q", fragment, rendered)
		}
	}
	if got := lipgloss.Width(rendered); got != 34 {
		t.Fatalf("list row width = %d, want 34: %q", got, rendered)
	}
}

func TestRenderUIListRowColorizesBrowseMetadata(t *testing.T) {
	ConfigureUIOutput()
	rendered := RenderUIListRow(&model.Issue{
		ID:       18,
		Title:    "Colorize list metadata for faster scanning",
		Status:   model.StatusReview,
		Priority: model.PriorityCritical,
		Kind:     model.IssueKindBug,
	}, HierarchyDecoration{}, 72, false)

	if !strings.Contains(rendered, "\x1b[") {
		t.Fatalf("expected ANSI styling in unselected list row, got %q", rendered)
	}
	for _, fragment := range []string{"REVIEW", "⏫", "BUG"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected colored list row to include %q: %q", fragment, rendered)
		}
	}
}

func TestRenderUIBoardColumnIncludesStatusAndKindMetadata(t *testing.T) {
	rendered := RenderUIBoardColumn(model.StatusTodo, []*model.Issue{{
		ID:       7,
		Title:    "Board rows keep status and kind metadata visible",
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindEpic,
	}}, nil, 38, 8, true, true, 0)

	for _, fragment := range []string{"DKT-7", "TODO", "↑", "EPIC"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected board row to include %q: %q", fragment, rendered)
		}
	}
}

func TestRenderUIListRowIncludesHierarchyDecoration(t *testing.T) {
	rendered := RenderUIListRow(&model.Issue{
		ID:       21,
		Title:    "Epic row keeps hierarchy progress visible",
		Status:   model.StatusInProgress,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindEpic,
	}, HierarchyDecoration{IsEpic: true, ChildCount: 3, Done: 1, Total: 3}, 64, false)

	for _, fragment := range []string{"DKT-21", "EPIC", "[3 sub 1/3]"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected list row to include %q: %q", fragment, rendered)
		}
	}
}

func TestRenderUIBoardColumnIncludesHierarchyDecoration(t *testing.T) {
	rendered := RenderUIBoardColumn(model.StatusTodo, []*model.Issue{{
		ID:       8,
		Title:    "Epic card keeps hierarchy summary visible",
		Status:   model.StatusTodo,
		Priority: model.PriorityHigh,
		Kind:     model.IssueKindEpic,
	}}, map[int]HierarchyDecoration{8: {IsEpic: true, ChildCount: 2, Done: 1, Total: 2}}, 48, 8, true, true, 0)

	for _, fragment := range []string{"DKT-8", "EPIC", "[2 sub 1/2]"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected board row to include %q: %q", fragment, rendered)
		}
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

func TestConfigureUIOutputSetsDefaultGlamourStyle(t *testing.T) {
	t.Setenv("GLAMOUR_STYLE", "")
	ConfigureUIOutput()
	if got := os.Getenv("GLAMOUR_STYLE"); got != "dark" {
		t.Fatalf("GLAMOUR_STYLE = %q, want dark", got)
	}
}

func TestJoinAndWrapHelpersPreserveContent(t *testing.T) {
	vertical := JoinUIVertical("one", "two")
	if !strings.Contains(vertical, "one\ntwo") {
		t.Fatalf("expected vertical join to stack content, got %q", vertical)
	}

	horizontal := JoinUIHorizontal("one", "two")
	if !strings.Contains(horizontal, "onetwo") {
		t.Fatalf("expected horizontal join to keep both fragments, got %q", horizontal)
	}

	wrapped := WrapUIContent("body", 8)
	if got := lipgloss.Width(wrapped); got != 8 {
		t.Fatalf("wrapped width = %d, want 8: %q", got, wrapped)
	}
}

func TestRenderUITextHelpersPreserveText(t *testing.T) {
	if !strings.Contains(RenderUIDimText("dim text"), "dim text") {
		t.Fatalf("expected dim text helper to keep content")
	}
	if !strings.Contains(RenderUIErrorText("error text"), "error text") {
		t.Fatalf("expected error text helper to keep content")
	}
}

func TestRenderUIFooterBarHandlesRefreshStates(t *testing.T) {
	notLoaded := RenderUIFooterBar(false, false, true, RefreshStatus{Enabled: true, Interval: 5 * time.Second}, 90)
	if !strings.Contains(notLoaded, "refresh auto 5s not loaded") {
		t.Fatalf("expected not-loaded refresh state, got %q", notLoaded)
	}

	expanded := RenderUIFooterBar(true, false, false, RefreshStatus{Enabled: false, Interval: 5 * time.Second, LastSuccess: time.Date(2026, 3, 28, 12, 34, 56, 0, time.UTC)}, 180)
	for _, fragment := range []string{"refresh paused 5s 12:34:56", "p resume"} {
		if !strings.Contains(expanded, fragment) {
			t.Fatalf("footer missing %q: %q", fragment, expanded)
		}
	}
}

func TestRenderUIListFooterBarIncludesSortHints(t *testing.T) {
	rendered := RenderUIListFooterBar(false, false, true, RefreshStatus{Enabled: true, Interval: 5 * time.Second}, 160)
	for _, fragment := range []string{"s sort-field", "S sort-dir", "refresh auto 5s not loaded"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected list footer to include %q: %q", fragment, rendered)
		}
	}
}

func TestRenderUIListFooterBarWrapsInsteadOfTruncatingAtNarrowWidths(t *testing.T) {
	rendered := RenderUIListFooterBar(false, false, true, RefreshStatus{Enabled: true, Interval: 5 * time.Second}, 78)
	if !strings.Contains(rendered, "\n") {
		t.Fatalf("expected wrapped footer at narrow widths, got %q", rendered)
	}
	for _, fragment := range []string{"refresh auto 5s not loaded", "s sort-field", "S sort-dir", "J/K detail", "o drill-down", "q quit"} {
		if !strings.Contains(rendered, fragment) {
			t.Fatalf("expected wrapped list footer to include %q: %q", fragment, rendered)
		}
	}
}

func TestRenderUIHeaderBarShowsRefreshFailure(t *testing.T) {
	rendered := RenderUIHeaderBar("docket", "board", RefreshStatus{Enabled: true, Interval: 5 * time.Second, LastError: "boom"}, 80)
	if !strings.Contains(rendered, "refresh failed") {
		t.Fatalf("expected refresh failure in header, got %q", rendered)
	}
}

func TestRenderUIDetailSubIssuesHeaderHighlightsFocus(t *testing.T) {
	rendered := RenderUIDetailSubIssuesHeader(2, 3, true)
	if !strings.Contains(rendered, "Sub-issues (2/3 done)") {
		t.Fatalf("expected sub-issue summary, got %q", rendered)
	}
}

func TestRenderUIModalUsesRequestedDimensions(t *testing.T) {
	rendered := RenderUIModal("Help", "body", 28, 8)
	lines := strings.Split(rendered, "\n")
	if got := len(lines); got != 8 {
		t.Fatalf("modal lines = %d, want 8: %q", got, rendered)
	}
	for _, line := range lines {
		if got := lipgloss.Width(line); got != 28 {
			t.Fatalf("modal width = %d, want 28: %q", got, rendered)
		}
	}
}

func TestPlaceUICenteredReturnsSizedCanvas(t *testing.T) {
	rendered := PlaceUICentered(20, 4, "x")
	lines := strings.Split(rendered, "\n")
	if got := len(lines); got != 4 {
		t.Fatalf("centered lines = %d, want 4: %q", got, rendered)
	}
	for _, line := range lines {
		if got := lipgloss.Width(line); got != 20 {
			t.Fatalf("centered width = %d, want 20: %q", got, rendered)
		}
	}
}

func TestUIWindowBoundsAndMaxHelpers(t *testing.T) {
	if start, end := uiWindowBounds(0, 0, 5); start != 0 || end != 0 {
		t.Fatalf("empty bounds = %d,%d, want 0,0", start, end)
	}
	if start, end := uiWindowBounds(1, 3, 5); start != 0 || end != 3 {
		t.Fatalf("full bounds = %d,%d, want 0,3", start, end)
	}
	if start, end := uiWindowBounds(4, 10, 4); start != 2 || end != 6 {
		t.Fatalf("middle bounds = %d,%d, want 2,6", start, end)
	}
	if got := uiMax(2, 5); got != 5 {
		t.Fatalf("uiMax = %d, want 5", got)
	}
}

func TestTruncateUIWidthHandlesEdgeCases(t *testing.T) {
	if got := truncateUIWidth("hello", 0); got != "" {
		t.Fatalf("truncate width 0 = %q, want empty", got)
	}
	if got := truncateUIWidth("hello", 5); got != "hello" {
		t.Fatalf("truncate exact width = %q, want hello", got)
	}
	if got := truncateUIWidth("hello", 3); got != "hel" {
		t.Fatalf("truncate narrow width = %q, want hel", got)
	}
	if got := truncateUIWidth("hello world", 8); got != "hello..." {
		t.Fatalf("truncate ellipsis width = %q, want hello...", got)
	}
}

func TestIssueMetadataLabelsUseExpectedShortForms(t *testing.T) {
	if got := uiStatusLabel(model.StatusInProgress); got != "INPROG" {
		t.Fatalf("in-progress label = %q, want INPROG", got)
	}
	if got := uiKindLabel(model.IssueKindFeature); got != "FEATURE" {
		t.Fatalf("feature label = %q, want FEATURE", got)
	}
	line := renderUIIssueLine(&model.Issue{
		ID:       4,
		Title:    "Short",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, HierarchyDecoration{}, 12)
	if strings.Contains(line, "\n") {
		t.Fatalf("expected single-line issue metadata, got %q", line)
	}
}
