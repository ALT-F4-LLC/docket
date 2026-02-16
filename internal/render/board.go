package render

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

const (
	maxCardsPerColumn = 10
	minColumnWidth    = 20
	defaultTermWidth  = 100
	cardPadding       = 2 // left+right padding inside cards
)

// StatusOrder defines the left-to-right column order for the board.
var StatusOrder = []model.Status{
	model.StatusBacklog,
	model.StatusTodo,
	model.StatusInProgress,
	model.StatusReview,
	model.StatusDone,
}

// PriorityOrder defines the display order for priorities (highest first).
var PriorityOrder = []model.Priority{
	model.PriorityCritical,
	model.PriorityHigh,
	model.PriorityMedium,
	model.PriorityLow,
	model.PriorityNone,
}

// SubIssueProgress holds pre-computed sub-issue completion data for a parent issue.
type SubIssueProgress struct {
	Done  int
	Total int
}

// BoardOptions configures board rendering behavior.
type BoardOptions struct {
	Expand   bool
	Progress map[int]SubIssueProgress // keyed by parent issue ID
}

// RenderBoard renders a list of issues as a Kanban board with columns per status.
func RenderBoard(issues []*model.Issue, opts BoardOptions) string {
	if len(issues) == 0 {
		return EmptyState("No issues on the board.", "Create one with: docket issue create", false)
	}

	if !ColorsEnabled() {
		return renderPlainBoard(issues, opts)
	}

	return renderColorBoard(issues, opts)
}

// terminalWidth returns the current terminal width, falling back to a default.
func terminalWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return defaultTermWidth
	}
	return w
}

// groupByStatus groups issues into a map keyed by status.
func groupByStatus(issues []*model.Issue) map[model.Status][]*model.Issue {
	groups := make(map[model.Status][]*model.Issue)
	for _, issue := range issues {
		groups[issue.Status] = append(groups[issue.Status], issue)
	}
	return groups
}

func renderColorBoard(issues []*model.Issue, opts BoardOptions) string {
	groups := groupByStatus(issues)

	// Determine which columns have issues.
	var activeStatuses []model.Status
	for _, s := range StatusOrder {
		if len(groups[s]) > 0 {
			activeStatuses = append(activeStatuses, s)
		}
	}

	if len(activeStatuses) == 0 {
		return ""
	}

	tw := terminalWidth()
	// Account for gaps between columns (1 space each).
	gaps := len(activeStatuses) - 1
	colWidth := (tw - gaps) / len(activeStatuses)
	if colWidth < minColumnWidth {
		colWidth = minColumnWidth
	}

	// Inner width available for card content (minus border/padding).
	cardContentWidth := max(colWidth-cardPadding-2, 5) // 2 for left+right border chars

	var columns []string
	for _, status := range activeStatuses {
		col := renderColorColumn(status, groups[status], colWidth, cardContentWidth, opts)
		columns = append(columns, col)
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, columns...)
}

func renderColorColumn(status model.Status, issues []*model.Issue, colWidth, contentWidth int, opts BoardOptions) string {
	// Column header
	headerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorFromName(status.Color())).
		Width(colWidth).
		Align(lipgloss.Center)

	header := headerStyle.Render(fmt.Sprintf("%s %s (%d)", status.Icon(), strings.ToUpper(string(status)), len(issues)))

	// Render cards up to the maximum.
	visible := issues
	overflow := 0
	if len(issues) > maxCardsPerColumn {
		visible = issues[:maxCardsPerColumn]
		overflow = len(issues) - maxCardsPerColumn
	}

	cards := make([]string, 0, len(visible)+2) // +2 for header and possible overflow
	cards = append(cards, header)

	for _, issue := range visible {
		card := renderColorCard(issue, colWidth, contentWidth, opts)
		cards = append(cards, card)
	}

	if overflow > 0 {
		moreStyle := lipgloss.NewStyle().
			Width(colWidth).
			Align(lipgloss.Center).
			Foreground(lipgloss.Color("8"))
		cards = append(cards, moreStyle.Render(fmt.Sprintf("+%d more", overflow)))
	}

	return lipgloss.JoinVertical(lipgloss.Left, cards...)
}

func renderColorCard(issue *model.Issue, colWidth, contentWidth int, opts BoardOptions) string {
	if contentWidth < 5 {
		contentWidth = 5
	}

	// Line 1: kind icon + ID + priority icon
	kindIcon := lipgloss.NewStyle().
		Foreground(ColorFromName(issue.Kind.Color())).
		Render(issue.Kind.Icon())
	idStr := model.FormatID(issue.ID)
	priIcon := lipgloss.NewStyle().
		Foreground(ColorFromName(issue.Priority.Color())).
		Render(issue.Priority.Icon())
	line1 := fmt.Sprintf("%s %s %s", kindIcon, idStr, priIcon)

	// Line 2: Title (truncated)
	line2 := truncate(issue.Title, contentWidth)

	// Line 3: Labels
	var line3 string
	if len(issue.Labels) > 0 {
		line3 = truncate(strings.Join(issue.Labels, ", "), contentWidth)
	}

	// Line 4: Sub-issue progress (if applicable)
	var line4 string
	if opts.Progress != nil {
		if prog, ok := opts.Progress[issue.ID]; ok && prog.Total > 0 {
			line4 = formatProgressBar(prog.Done, prog.Total, contentWidth)
		}
	}

	// Assemble card body.
	var lines []string
	lines = append(lines, line1, line2)
	if line3 != "" {
		lines = append(lines, line3)
	}
	if line4 != "" {
		lines = append(lines, line4)
	}
	body := strings.Join(lines, "\n")

	cardStyle := lipgloss.NewStyle().
		Width(colWidth - 2). // account for outer spacing
		Padding(0, 1).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(ColorFromName(issue.Status.Color()))

	return cardStyle.Render(body)
}

// formatProgressBar renders a text-based progress bar like "Sub: ###-- 3/5".
func formatProgressBar(done, total, maxWidth int) string {
	prefix := "Sub: "
	suffix := fmt.Sprintf(" %d/%d", done, total)
	barWidth := maxWidth - len(prefix) - len(suffix)
	if barWidth < 1 {
		return fmt.Sprintf("Sub: %d/%d", done, total)
	}
	if barWidth > total {
		barWidth = total
	}

	filled := 0
	if total > 0 {
		filled = (done * barWidth) / total
	}
	empty := barWidth - filled

	// U+25B0 (filled) and U+25B1 (empty) are widely supported but may render as
	// boxes on terminals with limited Unicode support. The plain-text fallback
	// in renderPlainCard avoids these characters entirely.
	bar := strings.Repeat("\u25B0", filled) + strings.Repeat("\u25B1", empty)
	return prefix + bar + suffix
}

// --- Plain text fallback ---

func renderPlainBoard(issues []*model.Issue, opts BoardOptions) string {
	groups := groupByStatus(issues)

	var activeStatuses []model.Status
	for _, s := range StatusOrder {
		if len(groups[s]) > 0 {
			activeStatuses = append(activeStatuses, s)
		}
	}

	if len(activeStatuses) == 0 {
		return ""
	}

	var b strings.Builder

	for i, status := range activeStatuses {
		if i > 0 {
			b.WriteString("\n")
		}

		issuesInCol := groups[status]
		fmt.Fprintf(&b, "=== %s %s (%d) ===\n", status.Icon(), strings.ToUpper(string(status)), len(issuesInCol))

		visible := issuesInCol
		overflow := 0
		if len(issuesInCol) > maxCardsPerColumn {
			visible = issuesInCol[:maxCardsPerColumn]
			overflow = len(issuesInCol) - maxCardsPerColumn
		}

		for _, issue := range visible {
			renderPlainCard(&b, issue, opts)
		}

		if overflow > 0 {
			fmt.Fprintf(&b, "  +%d more\n", overflow)
		}
	}

	return b.String()
}

func renderPlainCard(b *strings.Builder, issue *model.Issue, opts BoardOptions) {
	fmt.Fprintf(b, "  %s [%s] (%s)\n", model.FormatID(issue.ID), string(issue.Priority), string(issue.Kind))
	fmt.Fprintf(b, "  %s\n", truncate(issue.Title, maxTitleWidth))

	if len(issue.Labels) > 0 {
		fmt.Fprintf(b, "  %s\n", strings.Join(issue.Labels, ", "))
	}

	if opts.Progress != nil {
		if prog, ok := opts.Progress[issue.ID]; ok && prog.Total > 0 {
			fmt.Fprintf(b, "  Sub: %d/%d done\n", prog.Done, prog.Total)
		}
	}

	b.WriteString("\n")
}
