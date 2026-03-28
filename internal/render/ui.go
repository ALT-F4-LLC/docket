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

func RenderUIHeaderBar(projectName, viewName string, lastRefreshed time.Time, width int) string {
	left := lipgloss.NewStyle().Bold(true).Render("docket ui")
	project := lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Render(projectName)
	mode := lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Render(strings.ToUpper(viewName))
	badge := RenderUIDimText("READ-ONLY")
	refresh := RenderUIDimText("not loaded")
	if !lastRefreshed.IsZero() {
		refresh = RenderUIDimText("refreshed " + lastRefreshed.Format("15:04:05"))
	}

	line := strings.Join([]string{left, project, mode, badge, refresh}, "  •  ")
	return lipgloss.NewStyle().Width(width).Padding(0, 1).Render(line)
}

func RenderUIFooterBar(detailExpanded, detailFocused, browseFocused bool, width int) string {
	hints := []string{"1 list", "2 board", "j/k move", "tab pane", "enter detail", "r refresh", "? help", "q quit"}
	if detailExpanded {
		hints = []string{"h/l region", "j/k move", "enter open/zoom", "u parent", "esc collapse/back", "ctrl+u/d page", "q quit"}
	} else if detailFocused {
		hints = []string{"h/l region", "j/k move", "enter open/zoom", "u parent", "esc back", "ctrl+u/d page", "tab browse", "q quit"}
	} else if browseFocused {
		hints = []string{"j/k move", "J/K detail", "tab pane", "enter detail", "r refresh", "? help", "q quit"}
	}

	line := truncateUIWidth(strings.Join(hints, "  "), uiMax(width-4, 1))
	return lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Width(width).Padding(0, 1).Render(line)
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

func RenderUIListRow(issue *model.Issue, width int, selected bool) string {
	prefix := fmt.Sprintf("%-7s %s %s ", model.FormatID(issue.ID), issue.Status.Icon(), issue.Priority.Icon())
	content := prefix + truncateUIWidth(issue.Title, uiMax(width-lipgloss.Width(prefix), 1))

	style := lipgloss.NewStyle().Width(width)
	if selected {
		style = style.Foreground(lipgloss.Color("0")).Background(lipgloss.Color("12")).Bold(true)
	}
	return style.Render(content)
}

func RenderUIBoardColumn(status model.Status, issues []*model.Issue, width, height int, selected, browseFocused bool, selectedRow int) string {
	rowsHeight := uiMax(height-3, 1)
	innerWidth := uiMax(width-4, 1)
	start, end := uiWindowBounds(selectedRow, len(issues), rowsHeight)
	rows := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		prefix := fmt.Sprintf("%-7s %s ", model.FormatID(issues[i].ID), issues[i].Priority.Icon())
		row := prefix + truncateUIWidth(issues[i].Title, uiMax(innerWidth-lipgloss.Width(prefix), 1))
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
