package render

import (
	"fmt"
	"strings"
	"unicode/utf8"

	humanize "github.com/dustin/go-humanize"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/lipgloss/tree"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

const maxTitleWidth = 40

// ColorFromName maps model color name strings to lipgloss colors.
func ColorFromName(name string) lipgloss.Color {
	switch name {
	case "red":
		return lipgloss.Color("9")
	case "yellow":
		return lipgloss.Color("11")
	case "blue":
		return lipgloss.Color("12")
	case "green":
		return lipgloss.Color("10")
	case "magenta":
		return lipgloss.Color("13")
	case "gray":
		return lipgloss.Color("8")
	case "white":
		return lipgloss.Color("15")
	default:
		return lipgloss.Color("15")
	}
}

// truncate shortens a string to maxLen runes, appending an ellipsis if truncated.
func truncate(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-3]) + "..."
}

// statusLabel returns a bracketed status string, e.g. "[in-progress]".
func statusLabel(s model.Status) string {
	return "[" + string(s) + "]"
}

// RenderTable renders a list of issues as a formatted table.
// If treeMode is true, issues are rendered as an indented hierarchy instead.
func RenderTable(issues []*model.Issue, treeMode bool) string {
	if len(issues) == 0 {
		return "No issues found."
	}

	if treeMode {
		return RenderTreeList(issues)
	}

	if !ColorsEnabled() {
		return renderPlainTable(issues)
	}

	headers := []string{"ID", "Status", "Priority", "Type", "Title", "Assignee", "Updated"}

	rows := make([][]string, 0, len(issues))
	for _, issue := range issues {
		rows = append(rows, issueToRow(issue))
	}

	// Build color lookup for styling
	type rowColors struct {
		statusColor   string
		priorityColor string
	}
	colorMap := make([]rowColors, len(issues))
	for i, issue := range issues {
		colorMap[i] = rowColors{
			statusColor:   issue.Status.Color(),
			priorityColor: issue.Priority.Color(),
		}
	}

	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("8"))).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().PaddingLeft(1).PaddingRight(1)

			if row == table.HeaderRow {
				return s.Bold(true).Foreground(lipgloss.Color("15"))
			}

			if row < 0 || row >= len(colorMap) {
				return s
			}

			rc := colorMap[row]
			switch col {
			case 0: // ID
				return s.Foreground(lipgloss.Color("15"))
			case 1: // Status
				return s.Foreground(ColorFromName(rc.statusColor))
			case 2: // Priority
				return s.Foreground(ColorFromName(rc.priorityColor))
			case 3: // Type
				return s
			case 4: // Title
				return s.Bold(true)
			default:
				return s
			}
		})

	return t.Render()
}

func issueToRow(issue *model.Issue) []string {
	return []string{
		model.FormatID(issue.ID),
		string(issue.Status),
		fmt.Sprintf("%s %s", issue.Priority.Emoji(), string(issue.Priority)),
		string(issue.Kind),
		truncate(issue.Title, maxTitleWidth),
		issue.Assignee,
		humanize.Time(issue.UpdatedAt),
	}
}

func renderPlainTable(issues []*model.Issue) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%-10s %-14s %-18s %-10s %-40s %-15s %s\n",
		"ID", "Status", "Priority", "Type", "Title", "Assignee", "Updated")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 120))

	for _, issue := range issues {
		fmt.Fprintf(&b, "%-10s %-14s %-18s %-10s %-40s %-15s %s\n",
			model.FormatID(issue.ID),
			string(issue.Status),
			fmt.Sprintf("%s %s", issue.Priority.Emoji(), string(issue.Priority)),
			string(issue.Kind),
			truncate(issue.Title, maxTitleWidth),
			issue.Assignee,
			humanize.Time(issue.UpdatedAt),
		)
	}

	return b.String()
}

// RenderTreeList renders issues as an indented hierarchy using tree lines.
// Root issues (no parent) are top-level nodes; sub-issues are children.
func RenderTreeList(issues []*model.Issue) string {
	if len(issues) == 0 {
		return "No issues found."
	}

	if !ColorsEnabled() {
		return renderPlainTree(issues)
	}

	// Group children by parent.
	children := make(map[int][]*model.Issue)
	var roots []*model.Issue

	for _, issue := range issues {
		if issue.ParentID == nil {
			roots = append(roots, issue)
		} else {
			children[*issue.ParentID] = append(children[*issue.ParentID], issue)
		}
	}

	// If no roots found (all issues have parents not in the set), treat all as roots.
	if len(roots) == 0 {
		roots = issues
	}

	t := tree.New().Root("Issues")

	for _, root := range roots {
		node := tree.Root(formatTreeNode(root))
		addTreeChildren(node, root.ID, children)
		t.Child(node)
	}

	return t.String()
}

func formatTreeNode(issue *model.Issue) string {
	if !ColorsEnabled() {
		return fmt.Sprintf("%s %s %s %s",
			model.FormatID(issue.ID),
			statusLabel(issue.Status),
			issue.Priority.Emoji(),
			truncate(issue.Title, maxTitleWidth),
		)
	}

	idStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	statusStyle := lipgloss.NewStyle().Foreground(ColorFromName(issue.Status.Color()))
	priorityStyle := lipgloss.NewStyle().Foreground(ColorFromName(issue.Priority.Color()))
	titleStyle := lipgloss.NewStyle().Bold(true)

	return fmt.Sprintf("%s %s %s %s",
		idStyle.Render(model.FormatID(issue.ID)),
		statusStyle.Render(statusLabel(issue.Status)),
		priorityStyle.Render(issue.Priority.Emoji()),
		titleStyle.Render(truncate(issue.Title, maxTitleWidth)),
	)
}

func addTreeChildren(node *tree.Tree, parentID int, children map[int][]*model.Issue) {
	for _, child := range children[parentID] {
		childNode := tree.Root(formatTreeNode(child))
		addTreeChildren(childNode, child.ID, children)
		node.Child(childNode)
	}
}

func renderPlainTree(issues []*model.Issue) string {
	// Index issues by ID and group children by parent.
	children := make(map[int][]*model.Issue)
	var roots []*model.Issue

	for _, issue := range issues {
		if issue.ParentID == nil {
			roots = append(roots, issue)
		} else {
			children[*issue.ParentID] = append(children[*issue.ParentID], issue)
		}
	}

	if len(roots) == 0 {
		roots = issues
	}

	var b strings.Builder
	for _, root := range roots {
		renderPlainTreeNode(&b, root, children, 0)
	}
	return b.String()
}

func renderPlainTreeNode(b *strings.Builder, issue *model.Issue, children map[int][]*model.Issue, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(b, "%s%s %s %s %s\n",
		indent,
		model.FormatID(issue.ID),
		statusLabel(issue.Status),
		issue.Priority.Emoji(),
		truncate(issue.Title, maxTitleWidth),
	)
	for _, child := range children[issue.ID] {
		renderPlainTreeNode(b, child, children, depth+1)
	}
}
