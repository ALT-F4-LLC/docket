package render

import (
	"fmt"
	"strings"

	humanize "github.com/dustin/go-humanize"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/tree"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// RenderDetail renders a full issue detail view including metadata, description,
// sub-issues, relations, comments, and recent activity.
func RenderDetail(issue *model.Issue, subIssues []*model.Issue, relations []model.Relation, comments []model.Comment, activity []model.Activity) string {
	if !ColorsEnabled() {
		return renderPlainDetail(issue, subIssues, relations, comments, activity)
	}

	var sections []string

	// Header
	sections = append(sections, renderHeader(issue))

	// Metadata
	sections = append(sections, renderMetadata(issue))

	// Description
	if issue.Description != "" {
		sections = append(sections, renderDescription(issue.Description))
	}

	// Sub-issues
	if len(subIssues) > 0 {
		sections = append(sections, renderSubIssues(subIssues))
	}

	// Relations
	if len(relations) > 0 {
		sections = append(sections, renderRelations(issue.ID, relations))
	}

	// Comments
	if len(comments) > 0 {
		sections = append(sections, renderComments(comments))
	}

	// Activity
	if len(activity) > 0 {
		sections = append(sections, renderActivity(activity))
	}

	return strings.Join(sections, "\n\n")
}

func renderHeader(issue *model.Issue) string {
	idStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	titleStyle := lipgloss.NewStyle().Bold(true)
	statusStyle := lipgloss.NewStyle().
		Foreground(colorFromName(issue.Status.Color())).
		Bold(true)
	priorityStyle := lipgloss.NewStyle().
		Foreground(colorFromName(issue.Priority.Color())).
		Bold(true)

	return fmt.Sprintf("%s  %s\n%s  %s",
		idStyle.Render(model.FormatID(issue.ID)),
		titleStyle.Render(issue.Title),
		statusStyle.Render(statusLabel(issue.Status)),
		priorityStyle.Render(fmt.Sprintf("%s %s", issue.Priority.Emoji(), string(issue.Priority))),
	)
}

func renderMetadata(issue *model.Issue) string {
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var lines []string

	lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Type:"), string(issue.Kind)))

	if issue.Assignee != "" {
		lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Assignee:"), issue.Assignee))
	}

	if len(issue.Labels) > 0 {
		lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Labels:"), strings.Join(issue.Labels, ", ")))
	}

	if issue.ParentID != nil {
		lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Parent:"), model.FormatID(*issue.ParentID)))
	}

	lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Created:"), humanize.Time(issue.CreatedAt)))
	lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Updated:"), humanize.Time(issue.UpdatedAt)))

	return strings.Join(lines, "\n")
}

func renderDescription(description string) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Description")

	rendered, err := RenderMarkdown(description)
	if err != nil {
		rendered = description
	}

	return header + "\n" + rendered
}

func renderSubIssues(subIssues []*model.Issue) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))

	// Count done issues for progress summary.
	doneCount := 0
	for _, sub := range subIssues {
		if sub.Status == model.StatusDone {
			doneCount++
		}
	}

	rootLabel := fmt.Sprintf("%s (%d/%d done)",
		sectionStyle.Render("Sub-issues"),
		doneCount,
		len(subIssues),
	)

	t := tree.New().Root(rootLabel)
	for _, sub := range subIssues {
		label := formatSubIssueNode(sub)
		t.Child(label)
	}

	return t.String()
}

func formatSubIssueNode(issue *model.Issue) string {
	statusStyle := lipgloss.NewStyle().Foreground(colorFromName(issue.Status.Color()))
	priorityStyle := lipgloss.NewStyle().Foreground(colorFromName(issue.Priority.Color()))

	return fmt.Sprintf("%s %s %s %s",
		statusStyle.Render(statusLabel(issue.Status)),
		priorityStyle.Render(issue.Priority.Emoji()),
		model.FormatID(issue.ID),
		truncate(issue.Title, maxTitleWidth),
	)
}

func renderRelations(issueID int, relations []model.Relation) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Relations")

	var lines []string
	for _, rel := range relations {
		var line string
		if rel.SourceIssueID == issueID {
			line = fmt.Sprintf("  %s %s",
				string(rel.RelationType),
				model.FormatID(rel.TargetIssueID),
			)
		} else {
			line = fmt.Sprintf("  %s %s",
				rel.RelationType.Inverse(),
				model.FormatID(rel.SourceIssueID),
			)
		}
		lines = append(lines, line)
	}

	return header + "\n" + strings.Join(lines, "\n")
}

func renderComments(comments []model.Comment) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	authorStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	timeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	header := sectionStyle.Render("Comments")

	var parts []string
	for _, c := range comments {
		body, err := RenderMarkdown(c.Body)
		if err != nil {
			body = c.Body
		}

		commentHeader := fmt.Sprintf("%s  %s",
			authorStyle.Render(c.Author),
			timeStyle.Render(humanize.Time(c.CreatedAt)),
		)

		parts = append(parts, commentHeader+"\n"+body)
	}

	return header + "\n" + strings.Join(parts, "\n\n")
}

func renderActivity(activity []model.Activity) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	fieldStyle := lipgloss.NewStyle().Bold(true)
	timeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	header := sectionStyle.Render("Activity")

	var lines []string
	for _, a := range activity {
		var line string
		if a.FieldChanged == "created" {
			line = fmt.Sprintf("  Issue created  %s",
				timeStyle.Render(humanize.Time(a.CreatedAt)),
			)
		} else {
			actor := a.ChangedBy
			if actor == "" {
				actor = "system"
			}
			line = fmt.Sprintf("  %s changed %s: %s -> %s  %s",
				actor,
				fieldStyle.Render(a.FieldChanged),
				a.OldValue,
				a.NewValue,
				timeStyle.Render(humanize.Time(a.CreatedAt)),
			)
		}
		lines = append(lines, line)
	}

	return header + "\n" + strings.Join(lines, "\n")
}

// renderPlainDetail renders a detail view without any color or styling.
func renderPlainDetail(issue *model.Issue, subIssues []*model.Issue, relations []model.Relation, comments []model.Comment, activity []model.Activity) string {
	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "%s  %s\n", model.FormatID(issue.ID), issue.Title)
	fmt.Fprintf(&b, "%s  %s %s\n", statusLabel(issue.Status), issue.Priority.Emoji(), string(issue.Priority))

	// Metadata
	b.WriteString("\n")
	fmt.Fprintf(&b, "Type: %s\n", string(issue.Kind))
	if issue.Assignee != "" {
		fmt.Fprintf(&b, "Assignee: %s\n", issue.Assignee)
	}
	if len(issue.Labels) > 0 {
		fmt.Fprintf(&b, "Labels: %s\n", strings.Join(issue.Labels, ", "))
	}
	if issue.ParentID != nil {
		fmt.Fprintf(&b, "Parent: %s\n", model.FormatID(*issue.ParentID))
	}
	fmt.Fprintf(&b, "Created: %s\n", humanize.Time(issue.CreatedAt))
	fmt.Fprintf(&b, "Updated: %s\n", humanize.Time(issue.UpdatedAt))

	// Description
	if issue.Description != "" {
		fmt.Fprintf(&b, "\nDescription\n%s\n", issue.Description)
	}

	// Sub-issues
	if len(subIssues) > 0 {
		doneCount := 0
		for _, sub := range subIssues {
			if sub.Status == model.StatusDone {
				doneCount++
			}
		}
		fmt.Fprintf(&b, "\nSub-issues (%d/%d done)\n", doneCount, len(subIssues))
		for _, sub := range subIssues {
			fmt.Fprintf(&b, "  %s %s %s %s\n",
				statusLabel(sub.Status),
				sub.Priority.Emoji(),
				model.FormatID(sub.ID),
				truncate(sub.Title, maxTitleWidth),
			)
		}
	}

	// Relations
	if len(relations) > 0 {
		b.WriteString("\nRelations\n")
		for _, rel := range relations {
			if rel.SourceIssueID == issue.ID {
				fmt.Fprintf(&b, "  %s %s\n", string(rel.RelationType), model.FormatID(rel.TargetIssueID))
			} else {
				fmt.Fprintf(&b, "  %s %s\n", rel.RelationType.Inverse(), model.FormatID(rel.SourceIssueID))
			}
		}
	}

	// Comments
	if len(comments) > 0 {
		b.WriteString("\nComments\n")
		for _, c := range comments {
			fmt.Fprintf(&b, "  %s  %s\n  %s\n\n", c.Author, humanize.Time(c.CreatedAt), c.Body)
		}
	}

	// Activity
	if len(activity) > 0 {
		b.WriteString("\nActivity\n")
		for _, a := range activity {
			if a.FieldChanged == "created" {
				fmt.Fprintf(&b, "  Issue created  %s\n", humanize.Time(a.CreatedAt))
			} else {
				actor := a.ChangedBy
				if actor == "" {
					actor = "system"
				}
				fmt.Fprintf(&b, "  %s changed %s: %s -> %s  %s\n",
					actor, a.FieldChanged, a.OldValue, a.NewValue, humanize.Time(a.CreatedAt))
			}
		}
	}

	return b.String()
}
