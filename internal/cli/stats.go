package cli

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

type labelStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type statsResult struct {
	Total      int            `json:"total"`
	RootIssues int            `json:"root_issues"`
	SubIssues  int            `json:"sub_issues"`
	ByStatus   map[string]int `json:"by_status"`
	ByPriority map[string]int `json:"by_priority"`
	Labels     []labelStat    `json:"labels"`
}

var statsCmd = &cobra.Command{
	Use:   "stats",
	Short: "Show summary statistics for the issue database",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		total, err := db.CountIssues(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("counting issues: %w", err), output.ErrGeneral)
		}

		rootCount, err := db.CountRootIssues(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("counting root issues: %w", err), output.ErrGeneral)
		}

		byStatus, err := db.CountByStatus(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("counting by status: %w", err), output.ErrGeneral)
		}

		byPriority, err := db.CountByPriority(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("counting by priority: %w", err), output.ErrGeneral)
		}

		labels, err := db.ListAllLabels(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("listing labels: %w", err), output.ErrGeneral)
		}

		labelStats := make([]labelStat, len(labels))
		for i, l := range labels {
			labelStats[i] = labelStat{Name: l.Name, Count: l.IssueCount}
		}

		result := statsResult{
			Total:      total,
			RootIssues: rootCount,
			SubIssues:  total - rootCount,
			ByStatus:   byStatus,
			ByPriority: byPriority,
			Labels:     labelStats,
		}

		var message string
		if !w.JSONMode {
			message = renderStats(result)
		}
		w.Success(result, message)

		return nil
	},
}

func init() {
	rootCmd.AddCommand(statsCmd)
}

// renderStats renders the stats result as a styled human-readable string.
func renderStats(s statsResult) string {
	if !render.ColorsEnabled() {
		return renderPlainStats(s)
	}

	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	valueStyle := lipgloss.NewStyle().Bold(true)

	var sections []string

	// Overview section
	overviewHeader := sectionStyle.Render("Overview")
	overviewLines := []string{
		overviewHeader,
		fmt.Sprintf("  %s %s", labelStyle.Render("Total issues:"), valueStyle.Render(fmt.Sprintf("%d", s.Total))),
		fmt.Sprintf("  %s %s", labelStyle.Render("Root issues:"), valueStyle.Render(fmt.Sprintf("%d", s.RootIssues))),
		fmt.Sprintf("  %s %s", labelStyle.Render("Sub-issues:"), valueStyle.Render(fmt.Sprintf("%d", s.SubIssues))),
	}
	sections = append(sections, strings.Join(overviewLines, "\n"))

	// By Status section
	statusHeader := sectionStyle.Render("By Status")
	statusLines := []string{statusHeader}
	for _, status := range render.StatusOrder {
		count := s.ByStatus[string(status)]
		countStyle := lipgloss.NewStyle().Bold(true).Foreground(render.ColorFromName(status.Color()))
		statusLines = append(statusLines,
			fmt.Sprintf("  %s %s", labelStyle.Render(fmt.Sprintf("%-14s", string(status)+":")), countStyle.Render(fmt.Sprintf("%d", count))),
		)
	}
	sections = append(sections, strings.Join(statusLines, "\n"))

	// By Priority section
	priorityHeader := sectionStyle.Render("By Priority")
	priorityLines := []string{priorityHeader}
	for _, priority := range render.PriorityOrder {
		count := s.ByPriority[string(priority)]
		countStyle := lipgloss.NewStyle().Bold(true).Foreground(render.ColorFromName(priority.Color()))
		priorityLines = append(priorityLines,
			fmt.Sprintf("  %s %s", labelStyle.Render(fmt.Sprintf("%-14s", string(priority)+":")), countStyle.Render(fmt.Sprintf("%d", count))),
		)
	}
	sections = append(sections, strings.Join(priorityLines, "\n"))

	// Labels section
	labelsHeader := sectionStyle.Render("Labels")
	labelLines := []string{labelsHeader}
	if len(s.Labels) == 0 {
		labelLines = append(labelLines, fmt.Sprintf("  %s", labelStyle.Render("(none)")))
	} else {
		for _, l := range s.Labels {
			labelLines = append(labelLines,
				fmt.Sprintf("  %s %s", labelStyle.Render(fmt.Sprintf("%-20s", l.Name+":")), valueStyle.Render(fmt.Sprintf("%d", l.Count))),
			)
		}
	}
	sections = append(sections, strings.Join(labelLines, "\n"))

	return strings.Join(sections, "\n\n")
}

// renderPlainStats renders the stats result as plain text without styling.
func renderPlainStats(s statsResult) string {
	var b strings.Builder

	// Overview
	b.WriteString("Overview\n")
	fmt.Fprintf(&b, "  Total issues:  %d\n", s.Total)
	fmt.Fprintf(&b, "  Root issues:   %d\n", s.RootIssues)
	fmt.Fprintf(&b, "  Sub-issues:    %d\n", s.SubIssues)

	// By Status
	b.WriteString("\nBy Status\n")
	for _, status := range render.StatusOrder {
		count := s.ByStatus[string(status)]
		fmt.Fprintf(&b, "  %-14s %d\n", string(status)+":", count)
	}

	// By Priority
	b.WriteString("\nBy Priority\n")
	for _, priority := range render.PriorityOrder {
		count := s.ByPriority[string(priority)]
		fmt.Fprintf(&b, "  %-14s %d\n", string(priority)+":", count)
	}

	// Labels
	b.WriteString("\nLabels\n")
	if len(s.Labels) == 0 {
		b.WriteString("  (none)\n")
	} else {
		for _, l := range s.Labels {
			fmt.Fprintf(&b, "  %-20s %d\n", l.Name+":", l.Count)
		}
	}

	return b.String()
}
