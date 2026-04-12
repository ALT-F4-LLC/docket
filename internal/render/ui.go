package render

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

type HierarchyDecoration struct {
	ChildCount int
	Done       int
	Total      int
	IsEpic     bool
	IsChild    bool
}

type RefreshStatus struct {
	Enabled     bool
	Pending     bool
	LastSuccess time.Time
	LastError   string
	Interval    time.Duration
}

func ConfigureUIOutput() {
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	if os.Getenv("GLAMOUR_STYLE") == "" {
		_ = os.Setenv("GLAMOUR_STYLE", "dark")
	}
}

func JoinUIVertical(parts ...string) string {
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func JoinUIHorizontal(parts ...string) string {
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

func PlaceUICentered(width, height int, content string) string {
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func WrapUIContent(content string, width int) string {
	return lipgloss.NewStyle().Width(uiMax(width, 1)).Render(content)
}

func RenderUIDimText(text string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(text)
}

func RenderUIErrorText(text string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render(text)
}

func RenderUIHeaderBar(projectName, viewName string, refreshStatus RefreshStatus, width int, contextParts ...string) string {
	left := lipgloss.NewStyle().Bold(true).Render("docket tui")
	project := lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render(projectName)
	mode := lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render(strings.ToUpper(viewName))
	badge := RenderUIDimText("READ-ONLY")
	refresh := RenderUIDimText(formatHeaderRefreshStatus(refreshStatus))

	parts := []string{left, project, mode, badge}
	if strings.TrimSpace(projectName) == "" {
		parts = []string{left, mode, badge}
	}
	for _, part := range contextParts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render(part))
	}
	parts = append(parts, refresh)

	line := truncateUIWidth(strings.Join(parts, "  •  "), uiMax(width-2, 1))
	return lipgloss.NewStyle().Width(width).Padding(0, 1).Render(line)
}

func RenderUIFooterBar(detailExpanded, detailFocused, browseFocused bool, refreshStatus RefreshStatus, width int) string {
	hints := []string{"1 list", "2 board", "j/k move", "tab pane", "enter detail", "r refresh", "p pause", "? help", "q quit"}
	if detailExpanded {
		hints = []string{"h/l region", "j/k move", "enter open/zoom", "u parent", "esc collapse/back", "ctrl+u/d page", refreshToggleHint(refreshStatus.Enabled), "q quit"}
	} else if detailFocused {
		hints = []string{"h/l region", "j/k move", "enter open/zoom", "u parent", "esc back", "ctrl+u/d page", refreshToggleHint(refreshStatus.Enabled), "q quit"}
	} else if browseFocused {
		hints = []string{"j/k move", "J/K detail", "o drill-down", "tab pane", "enter detail", "r refresh", refreshToggleHint(refreshStatus.Enabled), "q quit"}
	}

	return renderUIFooterContent(formatFooterRefreshStatus(refreshStatus), hints, width)
}

func RenderUIListFooterBar(detailExpanded, detailFocused, browseFocused bool, refreshStatus RefreshStatus, width int) string {
	hints := []string{"1 list", "2 board", "j/k move", "s sort-field", "S sort-dir", "tab pane", "enter detail", "r refresh", refreshToggleHint(refreshStatus.Enabled), "q quit"}
	if detailExpanded {
		hints = []string{"h/l region", "j/k move", "enter open/zoom", "u parent", "esc collapse/back", "ctrl+u/d page", refreshToggleHint(refreshStatus.Enabled), "q quit"}
	} else if detailFocused {
		hints = []string{"h/l region", "j/k move", "enter open/zoom", "u parent", "esc back", "ctrl+u/d page", refreshToggleHint(refreshStatus.Enabled), "q quit"}
	} else if browseFocused {
		hints = []string{"j/k move", "s sort-field", "S sort-dir", "J/K detail", "o drill-down", "tab pane", "enter detail", "r refresh", refreshToggleHint(refreshStatus.Enabled), "q quit"}
	}

	return renderUIFooterContent(formatFooterRefreshStatus(refreshStatus), hints, width)
}

func renderUIFooterContent(refresh string, hints []string, width int) string {
	innerWidth := uiMax(width-2, 1)
	content := wrapUIFooterContent(refresh, hints, innerWidth)
	return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Width(width).Padding(0, 1).Render(
		lipgloss.NewStyle().Width(innerWidth).Render(content),
	)
}

func wrapUIFooterContent(refresh string, hints []string, width int) string {
	if width <= 0 {
		return ""
	}

	current := truncateUIWidth(refresh, width)
	hasHintOnLine := false
	lines := make([]string, 0, 2)
	for _, hint := range hints {
		segment := hint
		separator := "  "
		if !hasHintOnLine {
			separator = "  •  "
		}
		candidate := current + separator + hint
		if lipgloss.Width(candidate) <= width {
			current = candidate
			hasHintOnLine = true
			continue
		}
		lines = append(lines, current)
		current = truncateUIWidth(segment, width)
		hasHintOnLine = true
	}
	lines = append(lines, current)
	return strings.Join(lines, "\n")
}

func formatHeaderRefreshStatus(status RefreshStatus) string {
	if status.Pending {
		return "refreshing"
	}
	if status.LastError != "" {
		return "refresh failed"
	}
	if !status.LastSuccess.IsZero() {
		return "refreshed " + status.LastSuccess.Format("15:04:05")
	}
	return "not loaded"
}

func formatFooterRefreshStatus(status RefreshStatus) string {
	state := "refresh"
	if status.Enabled {
		state += " auto"
	} else {
		state += " paused"
	}
	if status.Interval > 0 {
		state += " " + status.Interval.Round(time.Second).String()
	}
	if status.Pending {
		return state + " updating"
	}
	if status.LastError != "" {
		return state + " failed"
	}
	if !status.LastSuccess.IsZero() {
		return state + " " + status.LastSuccess.Format("15:04:05")
	}
	return state + " not loaded"
}

func refreshToggleHint(enabled bool) string {
	if enabled {
		return "p pause"
	}
	return "p resume"
}

func RenderUIPane(title, content string, width, height int, focused bool) string {
	borderColor := lipgloss.Color("8")
	if focused {
		borderColor = lipgloss.Color("12")
	}

	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	body := lipgloss.NewStyle().Width(uiMax(width-4, 1)).Height(uiMax(height-3, 1)).Render(content)
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1).
		Width(uiMax(width-2, 1)).
		Height(uiMax(height-2, 1))

	return style.Render(titleStyle.Render(title) + "\n" + body)
}

func RenderUIListRow(issue *model.Issue, decoration HierarchyDecoration, width int, selected bool) string {
	content := renderUIIssueLine(issue, decoration, width)

	style := lipgloss.NewStyle().Width(width)
	if selected {
		style = style.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("12")).Bold(true)
	}
	return style.Render(content)
}

func RenderUIBoardColumn(status model.Status, issues []*model.Issue, decorations map[int]HierarchyDecoration, width, height int, selected, browseFocused bool, selectedRow int) string {
	rowsHeight := uiMax(height-3, 1)
	innerWidth := uiMax(width-4, 1)
	start, end := uiWindowBounds(selectedRow, len(issues), rowsHeight)
	rows := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		row := renderUIIssueLine(issues[i], decorations[issues[i].ID], innerWidth)
		style := lipgloss.NewStyle().Width(innerWidth)
		if selected && i == selectedRow {
			style = style.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11")).Bold(true)
		}
		rows = append(rows, style.Render(row))
	}

	return RenderUIPane(
		fmt.Sprintf("%s (%d)", strings.ToUpper(string(status)), len(issues)),
		strings.Join(rows, "\n"),
		width,
		height,
		selected && browseFocused,
	)
}

func renderUIIssueLine(issue *model.Issue, decoration HierarchyDecoration, width int) string {
	idLabel := fmt.Sprintf("%-7s", model.FormatID(issue.ID))
	statusLabel := fmt.Sprintf("%-7s", uiStatusLabel(issue.Status))
	priorityLabel := issue.Priority.Icon()
	kindLabel := fmt.Sprintf("%-7s", uiKindLabel(issue.Kind))
	prefixWidth := lipgloss.Width(idLabel) + 1 + lipgloss.Width(statusLabel) + 1 + lipgloss.Width(priorityLabel) + 1 + lipgloss.Width(kindLabel) + 1
	title := issue.Title
	if decoration.IsChild {
		title = "> " + title
	}
	suffix := uiHierarchyLabel(decoration)
	suffixWidth := 0
	if suffix != "" {
		suffixWidth = lipgloss.Width(" " + suffix)
	}
	titleWidth := uiMax(width-prefixWidth-suffixWidth, 0)
	if titleWidth == 0 {
		plain := strings.TrimSpace(strings.Join([]string{idLabel, statusLabel, priorityLabel, kindLabel, suffix}, " "))
		return truncateUIWidth(plain, width)
	}
	content := strings.Join([]string{
		idLabel,
		lipgloss.NewStyle().Foreground(ColorFromName(issue.Status.Color())).Render(statusLabel),
		lipgloss.NewStyle().Foreground(ColorFromName(issue.Priority.Color())).Render(priorityLabel),
		lipgloss.NewStyle().Foreground(ColorFromName(issue.Kind.Color())).Render(kindLabel),
		truncateUIWidth(title, titleWidth),
	}, " ")
	if suffix != "" {
		content += " " + RenderUIDimText(suffix)
	}
	return content
}

func uiHierarchyLabel(decoration HierarchyDecoration) string {
	if !decoration.IsEpic || decoration.Total == 0 {
		return ""
	}
	return fmt.Sprintf("[%d sub %d/%d]", decoration.ChildCount, decoration.Done, decoration.Total)
}

func uiStatusLabel(status model.Status) string {
	switch status {
	case model.StatusInProgress:
		return "INPROG"
	default:
		return strings.ToUpper(string(status))
	}
}

func uiKindLabel(kind model.IssueKind) string {
	return strings.ToUpper(string(kind))
}

func RenderUIDetailSubIssuesHeader(doneCount, totalCount int, focused bool) string {
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	if focused {
		headerStyle = headerStyle.Foreground(lipgloss.Color("11"))
	}
	return headerStyle.Render(fmt.Sprintf("Sub-issues (%d/%d done)", doneCount, totalCount))
}

func RenderUIDetailSubIssueRow(issue *model.Issue, width int, selected, focused bool) string {
	prefix := fmt.Sprintf("%-7s %s %s %s ", model.FormatID(issue.ID), issue.Status.Icon(), issue.Priority.Icon(), issue.Kind.Icon())
	content := prefix + truncateUIWidth(issue.Title, uiMax(width-lipgloss.Width(prefix), 1))

	style := lipgloss.NewStyle().Width(width)
	if selected {
		style = style.Bold(true)
		if focused {
			style = style.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("11"))
		} else {
			style = style.Foreground(lipgloss.Color("11"))
		}
	}

	return style.Render(content)
}

func RenderUIModal(title, content string, boxWidth, boxHeight int) string {
	return RenderUIPane(title, content, boxWidth, boxHeight, true)
}

func uiWindowBounds(selected, total, height int) (int, int) {
	if total == 0 {
		return 0, 0
	}
	if height >= total {
		return 0, total
	}

	start := selected - height/2
	if start < 0 {
		start = 0
	}
	end := start + height
	if end > total {
		end = total
		start = end - height
	}
	return start, end
}

func uiMax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func truncateUIWidth(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= maxWidth {
		return s
	}

	if maxWidth <= 3 {
		var b strings.Builder
		for _, r := range s {
			next := b.String() + string(r)
			if lipgloss.Width(next) > maxWidth {
				break
			}
			b.WriteRune(r)
		}
		return b.String()
	}

	const ellipsis = "..."
	budget := maxWidth - lipgloss.Width(ellipsis)
	var b strings.Builder
	for _, r := range s {
		next := b.String() + string(r)
		if lipgloss.Width(next) > budget {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + ellipsis
}
