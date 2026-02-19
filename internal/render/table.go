package render

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	humanize "github.com/dustin/go-humanize"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/charmbracelet/lipgloss/tree"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

const maxTitleWidth = 40

// StyledText applies a lipgloss style to text when colors are enabled.
// When colors are disabled, it returns the plain text unchanged.
func StyledText(text string, style lipgloss.Style) string {
	if ColorsEnabled() {
		return style.Render(text)
	}
	return text
}

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

// statusLabel returns a status string with icon, e.g. "✔ done".
func statusLabel(s model.Status) string {
	return s.Icon() + " " + string(s)
}

// EmptyState renders a styled empty-state message with an optional contextual hint.
// When colors are enabled the message is rendered in dim gray and the hint is italic.
// When quiet is true the hint is suppressed.
func EmptyState(message, hint string, quiet bool) string {
	if !ColorsEnabled() {
		if quiet || hint == "" {
			return message
		}
		return message + "\n" + hint
	}

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	hintStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Italic(true)

	result := dimStyle.Render(message)
	if !quiet && hint != "" {
		result += "\n" + hintStyle.Render(hint)
	}
	return result
}

// RenderTable renders a list of issues as a formatted table.
// If treeMode is true, issues are rendered as an indented hierarchy instead.
func RenderTable(issues []*model.Issue, treeMode bool) string {
	if len(issues) == 0 {
		return EmptyState("No issues found.", "Create one with: docket issue create", false)
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
		kindColor     string
	}
	colorMap := make([]rowColors, len(issues))
	for i, issue := range issues {
		colorMap[i] = rowColors{
			statusColor:   issue.Status.Color(),
			priorityColor: issue.Priority.Color(),
			kindColor:     issue.Kind.Color(),
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
				return s.Foreground(ColorFromName(rc.kindColor))
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
		statusLabel(issue.Status),
		fmt.Sprintf("%s %s", issue.Priority.Icon(), string(issue.Priority)),
		fmt.Sprintf("%s %s", issue.Kind.Icon(), string(issue.Kind)),
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
		fmt.Fprintf(&b, "%-10s %-16s %-18s %-12s %-40s %-15s %s\n",
			model.FormatID(issue.ID),
			statusLabel(issue.Status),
			fmt.Sprintf("%s %s", issue.Priority.Icon(), string(issue.Priority)),
			fmt.Sprintf("%s %s", issue.Kind.Icon(), string(issue.Kind)),
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
		return EmptyState("No issues found.", "Create one with: docket issue create", false)
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
		return fmt.Sprintf("%s %s %s %s %s",
			model.FormatID(issue.ID),
			statusLabel(issue.Status),
			issue.Priority.Icon(),
			fmt.Sprintf("%s %s", issue.Kind.Icon(), string(issue.Kind)),
			truncate(issue.Title, maxTitleWidth),
		)
	}

	idStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	statusStyle := lipgloss.NewStyle().Foreground(ColorFromName(issue.Status.Color()))
	priorityStyle := lipgloss.NewStyle().Foreground(ColorFromName(issue.Priority.Color()))
	kindStyle := lipgloss.NewStyle().Foreground(ColorFromName(issue.Kind.Color()))
	titleStyle := lipgloss.NewStyle().Bold(true)

	return fmt.Sprintf("%s %s %s %s %s",
		idStyle.Render(model.FormatID(issue.ID)),
		statusStyle.Render(statusLabel(issue.Status)),
		priorityStyle.Render(issue.Priority.Icon()),
		kindStyle.Render(fmt.Sprintf("%s %s", issue.Kind.Icon(), string(issue.Kind))),
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
	fmt.Fprintf(b, "%s%s %s %s %s %s\n",
		indent,
		model.FormatID(issue.ID),
		statusLabel(issue.Status),
		issue.Priority.Icon(),
		fmt.Sprintf("%s %s", issue.Kind.Icon(), string(issue.Kind)),
		truncate(issue.Title, maxTitleWidth),
	)
	for _, child := range children[issue.ID] {
		renderPlainTreeNode(b, child, children, depth+1)
	}
}

// statusRank returns a numeric rank for sorting by status: lower = higher priority.
func statusRank(s model.Status) int {
	switch s {
	case model.StatusInProgress:
		return 0
	case model.StatusReview:
		return 1
	case model.StatusTodo:
		return 2
	case model.StatusBacklog:
		return 3
	case model.StatusDone:
		return 4
	default:
		return 5
	}
}

// priorityRank returns a numeric rank for sorting by priority: lower = higher priority.
func priorityRank(p model.Priority) int {
	switch p {
	case model.PriorityCritical:
		return 0
	case model.PriorityHigh:
		return 1
	case model.PriorityMedium:
		return 2
	case model.PriorityLow:
		return 3
	case model.PriorityNone:
		return 4
	default:
		return 5
	}
}

// sortIssuesByRank sorts issues by status rank, then priority rank, then created_at DESC.
func sortIssuesByRank(issues []*model.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		si, sj := statusRank(issues[i].Status), statusRank(issues[j].Status)
		if si != sj {
			return si < sj
		}
		pi, pj := priorityRank(issues[i].Priority), priorityRank(issues[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return issues[i].CreatedAt.After(issues[j].CreatedAt)
	})
}

// parentGroup holds a parent issue and its children for grouped rendering.
type parentGroup struct {
	parent   *model.Issue
	children []*model.Issue
}

// RenderGroupedTable renders issues grouped by parent-child relationships.
// Parent issues are displayed as section headers with progress indicators,
// and their children are rendered as indented table rows beneath them.
// Standalone issues (no parent and no children) appear in a separate section at the bottom.
//
// Parameters:
//   - issues: the filtered result set from the query.
//   - parentMap: parent issues fetched separately that are NOT in the filtered set.
//   - progress: sub-issue progress data keyed by parent issue ID.
func RenderGroupedTable(issues []*model.Issue, parentMap map[int]*model.Issue, progress map[int]SubIssueProgress) string {
	if len(issues) == 0 {
		return EmptyState("No issues found.", "Create one with: docket issue create", false)
	}

	// Build a set of issue IDs in the result set for fast lookup.
	issueSet := make(map[int]bool, len(issues))
	for _, issue := range issues {
		issueSet[issue.ID] = true
	}

	// Identify which issues in the result set are parents (have children pointing to them).
	parentIDs := make(map[int]bool)
	for _, issue := range issues {
		if issue.ParentID != nil {
			parentIDs[*issue.ParentID] = true
		}
	}

	// Classify issues into three buckets.
	childrenByParent := make(map[int][]*model.Issue)
	var standalone []*model.Issue

	for _, issue := range issues {
		if issue.ParentID != nil {
			childrenByParent[*issue.ParentID] = append(childrenByParent[*issue.ParentID], issue)
		} else if parentIDs[issue.ID] {
			// This issue is a parent (has children in the result set).
			// It will be used as a group header; don't add to standalone.
		} else {
			standalone = append(standalone, issue)
		}
	}

	// Build parent groups.
	var groups []parentGroup

	// Collect unique parent IDs that actually have children in the result.
	seenParents := make(map[int]bool)
	for parentID, children := range childrenByParent {
		if seenParents[parentID] {
			continue
		}
		seenParents[parentID] = true

		var parent *model.Issue
		if issueSet[parentID] {
			// Parent is in the result set -- find it.
			for _, issue := range issues {
				if issue.ID == parentID {
					parent = issue
					break
				}
			}
		} else if parentMap != nil {
			// Parent was fetched separately.
			parent = parentMap[parentID]
		}

		if parent == nil {
			// Parent not available anywhere -- treat children as standalone.
			standalone = append(standalone, children...)
			continue
		}

		groups = append(groups, parentGroup{
			parent:   parent,
			children: children,
		})
	}

	// Handle parents in the result set that have zero children in the result.
	// They should be treated as standalone.
	for _, issue := range issues {
		if issue.ParentID == nil && parentIDs[issue.ID] {
			if _, hasChildren := childrenByParent[issue.ID]; !hasChildren {
				standalone = append(standalone, issue)
			}
		}
	}

	// If there are no groups at all, render as a flat table.
	if len(groups) == 0 {
		return RenderTable(issues, false)
	}

	// Sort parent groups by status rank, priority rank, created_at DESC.
	sort.SliceStable(groups, func(i, j int) bool {
		si, sj := statusRank(groups[i].parent.Status), statusRank(groups[j].parent.Status)
		if si != sj {
			return si < sj
		}
		pi, pj := priorityRank(groups[i].parent.Priority), priorityRank(groups[j].parent.Priority)
		if pi != pj {
			return pi < pj
		}
		return groups[i].parent.CreatedAt.After(groups[j].parent.CreatedAt)
	})

	// Sort children within each group.
	for i := range groups {
		sortIssuesByRank(groups[i].children)
	}

	// Sort standalone issues.
	sortIssuesByRank(standalone)

	if !ColorsEnabled() {
		return renderGroupedPlainTable(groups, standalone, progress)
	}

	return renderGroupedColorTable(groups, standalone, progress)
}

// renderGroupedColorTable renders grouped issues with lipgloss styling.
func renderGroupedColorTable(groups []parentGroup, standalone []*model.Issue, progress map[int]SubIssueProgress) string {
	var sections []string

	headerBoldStyle := lipgloss.NewStyle().Bold(true)
	idStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	for _, g := range groups {
		// Build section header.
		kindStyle := lipgloss.NewStyle().
			Foreground(ColorFromName(g.parent.Kind.Color())).
			Bold(true)
		statusStyle := lipgloss.NewStyle().
			Foreground(ColorFromName(g.parent.Status.Color())).
			Bold(true)
		priorityStyle := lipgloss.NewStyle().
			Foreground(ColorFromName(g.parent.Priority.Color())).
			Bold(true)

		// Progress indicator.
		prog := ""
		if progress != nil {
			if p, ok := progress[g.parent.ID]; ok && p.Total > 0 {
				prog = dimStyle.Render(fmt.Sprintf("(%d/%d done)", p.Done, p.Total))
			}
		}

		header := fmt.Sprintf("%s %s  %s  %s  %s",
			kindStyle.Render(g.parent.Kind.Icon()),
			idStyle.Render(model.FormatID(g.parent.ID)),
			headerBoldStyle.Render(g.parent.Title),
			statusStyle.Render(fmt.Sprintf("%s %s", g.parent.Status.Icon(), string(g.parent.Status))),
			priorityStyle.Render(fmt.Sprintf("%s %s", g.parent.Priority.Icon(), string(g.parent.Priority))),
		)
		if prog != "" {
			header += "  " + prog
		}

		// Render children as an indented table.
		childTable := renderColorChildTable(g.children)

		sections = append(sections, header+"\n"+childTable)
	}

	// Render standalone issues.
	if len(standalone) > 0 {
		sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
		standaloneHeader := sectionStyle.Render("Standalone Issues")
		childTable := renderColorChildTable(standalone)
		sections = append(sections, standaloneHeader+"\n"+childTable)
	}

	return strings.Join(sections, "\n\n")
}

// renderColorChildTable renders a set of issues as a lipgloss-styled table with 2-space indent.
func renderColorChildTable(issues []*model.Issue) string {
	headers := []string{"ID", "Status", "Priority", "Type", "Title", "Assignee", "Updated"}

	rows := make([][]string, 0, len(issues))
	for _, issue := range issues {
		rows = append(rows, issueToRow(issue))
	}

	type rowColors struct {
		statusColor   string
		priorityColor string
		kindColor     string
	}
	colorMap := make([]rowColors, len(issues))
	for i, issue := range issues {
		colorMap[i] = rowColors{
			statusColor:   issue.Status.Color(),
			priorityColor: issue.Priority.Color(),
			kindColor:     issue.Kind.Color(),
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
				return s.Foreground(ColorFromName(rc.kindColor))
			case 4: // Title
				return s.Bold(true)
			default:
				return s
			}
		})

	// Indent the entire table by 2 spaces.
	rendered := t.Render()
	var indented strings.Builder
	for i, line := range strings.Split(rendered, "\n") {
		if i > 0 {
			indented.WriteString("\n")
		}
		indented.WriteString("  ")
		indented.WriteString(line)
	}

	return indented.String()
}

// renderGroupedPlainTable renders grouped issues as plain text without color.
func renderGroupedPlainTable(groups []parentGroup, standalone []*model.Issue, progress map[int]SubIssueProgress) string {
	var b strings.Builder

	for i, g := range groups {
		if i > 0 {
			b.WriteString("\n")
		}

		// Progress indicator.
		prog := ""
		if progress != nil {
			if p, ok := progress[g.parent.ID]; ok && p.Total > 0 {
				prog = fmt.Sprintf("  (%d/%d done)", p.Done, p.Total)
			}
		}

		// Section header.
		fmt.Fprintf(&b, "=== %s %s  %s  %s %s  %s %s%s ===\n",
			g.parent.Kind.Icon(),
			model.FormatID(g.parent.ID),
			g.parent.Title,
			g.parent.Status.Icon(), string(g.parent.Status),
			g.parent.Priority.Icon(), string(g.parent.Priority),
			prog,
		)

		// Children as indented plain rows.
		renderPlainChildRows(&b, g.children)
	}

	// Standalone issues.
	if len(standalone) > 0 {
		if len(groups) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("=== Standalone Issues ===\n")
		renderPlainChildRows(&b, standalone)
	}

	return b.String()
}

// renderPlainChildRows renders issue rows with 2-space indent in plain-text format.
func renderPlainChildRows(b *strings.Builder, issues []*model.Issue) {
	for _, issue := range issues {
		fmt.Fprintf(b, "  %-10s %-16s %-18s %-12s %-40s %-15s %s\n",
			model.FormatID(issue.ID),
			statusLabel(issue.Status),
			fmt.Sprintf("%s %s", issue.Priority.Icon(), string(issue.Priority)),
			fmt.Sprintf("%s %s", issue.Kind.Icon(), string(issue.Kind)),
			truncate(issue.Title, maxTitleWidth),
			issue.Assignee,
			humanize.Time(issue.UpdatedAt),
		)
	}
}
