package render

import (
	"fmt"
	"strings"
	"time"

	humanize "github.com/dustin/go-humanize"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/tree"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// RenderDetail renders a full issue detail view including metadata, the run
// disposition (when a run abandoned its work on the issue), description,
// sub-issues, relations, linked proposals, comments, and recent activity.
//
// disposition is nil on every issue no run ever abandoned, which is nearly all
// of them; the section then does not appear at all.
func RenderDetail(issue *model.Issue, subIssues []*model.Issue, relations []model.Relation, linkedProposals []model.Proposal, comments []*model.Comment, activity []model.Activity, disposition *model.IssueRunDisposition) string {
	if !ColorsEnabled() {
		return renderPlainDetail(issue, subIssues, relations, linkedProposals, comments, activity, disposition)
	}

	var sections []string

	// Header
	sections = append(sections, renderHeader(issue))

	// Metadata
	sections = append(sections, renderMetadata(issue))

	// The run disposition, ABOVE the description and everything under it
	// (DKT-404). A reader who stops at the top of this view is exactly the
	// reader who misreads a frozen `todo` or `review` as still-pending work,
	// so the fact that a run gave up on the issue sits with the metadata that
	// frames it — not below a description, a sub-issue tree and a comment
	// thread, where the trail comment the abandon already dropped was being
	// missed.
	if disposition != nil {
		sections = append(sections, renderRunDisposition(disposition))
	}

	// Files
	if len(issue.Files) > 0 {
		sections = append(sections, renderFiles(issue.Files))
	}

	if len(issue.Docs) > 0 {
		sections = append(sections, renderDocRefs(issue.Docs))
	}

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

	if len(linkedProposals) > 0 {
		sections = append(sections, renderLinkedProposals(linkedProposals))
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
	kindStyle := lipgloss.NewStyle().
		Foreground(ColorFromName(issue.Kind.Color())).
		Bold(true)
	statusStyle := lipgloss.NewStyle().
		Foreground(ColorFromName(issue.Status.Color())).
		Bold(true)
	priorityStyle := lipgloss.NewStyle().
		Foreground(ColorFromName(issue.Priority.Color())).
		Bold(true)

	// The detail view keeps the real status AND the resolution, side by side.
	// The listing's status column has room for one, so there the resolution
	// wins; here nothing has to be dropped, and an operator triaging an
	// abandoned issue needs both — what the run left it at, and that the run
	// gave up on it (DKT-245).
	status := statusStyle.Render(statusLabel(issue.Status))
	if issue.Resolution != "" {
		resolutionStyle := lipgloss.NewStyle().
			Foreground(ColorFromName("red")).
			Bold(true)
		status += "  " + resolutionStyle.Render("⊘ "+issue.Resolution)
	}

	return fmt.Sprintf("%s %s  %s\n%s  %s",
		kindStyle.Render(issue.Kind.Icon()),
		idStyle.Render(model.FormatID(issue.ID)),
		titleStyle.Render(issue.Title),
		status,
		priorityStyle.Render(fmt.Sprintf("%s %s", issue.Priority.Icon(), string(issue.Priority))),
	)
}

func renderMetadata(issue *model.Issue) string {
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var lines []string

	kindStyle := lipgloss.NewStyle().Foreground(ColorFromName(issue.Kind.Color()))
	lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Type:"), kindStyle.Render(fmt.Sprintf("%s %s", issue.Kind.Icon(), string(issue.Kind)))))

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

// RunDispositionHeading is the section title both detail renderers use, and the
// string a reader (or a test) looks for to decide whether a run ever stopped
// working an issue.
const RunDispositionHeading = "Run disposition"

// runDispositionHeadline is the one-line ruling: what happened, which run
// decided it, which step's routing carried it when one did, and when.
//
// The DATE, not a relative time, and not the issue's `updated_at`: the point of
// the line is provenance a reader can carry to `run report RUN-14` or `events
// list`, and "3 weeks ago" is not a key into either. It is also stable output,
// which humanize.Time is not.
func runDispositionHeadline(d *model.IssueRunDisposition) string {
	headline := fmt.Sprintf("work %s in %s", d.Disposition, model.FormatRunID(d.RunID))
	// `run abandon --issue` is an operator acting from OUTSIDE the graph, and
	// no step decided it. Naming one would be a fabrication, so the clause is
	// simply absent.
	if d.By != "" {
		headline += " by " + d.By
	}
	return fmt.Sprintf("%s (%s)", headline,
		time.UnixMilli(d.AtMS).UTC().Format(time.DateOnly))
}

// runDispositionReasonLines is the recorded ruling, VERBATIM and unabridged,
// one output line per input line.
//
// Not truncated: this is the whole reason the section exists. `issue show` is
// the surface a reader lands on when they want to know why an issue stopped,
// and a head that cut an operator's ruling mid-sentence would send them back to
// `events list` — the exact trip DKT-404 is closing.
func runDispositionReasonLines(reason string) []string {
	reason = strings.TrimRight(reason, "\n")
	if strings.TrimSpace(reason) == "" {
		return nil
	}
	return strings.Split(reason, "\n")
}

func renderRunDisposition(d *model.IssueRunDisposition) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	markerStyle := lipgloss.NewStyle().Foreground(ColorFromName("red")).Bold(true)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	lines := []string{
		sectionStyle.Render(RunDispositionHeading),
		"  " + markerStyle.Render("⊘ "+runDispositionHeadline(d)),
	}
	for i, line := range runDispositionReasonLines(d.Reason) {
		if i == 0 {
			lines = append(lines, "    "+dimStyle.Render("reason: ")+line)
			continue
		}
		lines = append(lines, "    "+line)
	}
	return strings.Join(lines, "\n")
}

func renderFiles(files []string) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	header := sectionStyle.Render("Files")

	var lines []string
	for _, f := range files {
		lines = append(lines, "  "+dimStyle.Render("▸ "+f))
	}

	return header + "\n" + strings.Join(lines, "\n")
}

func renderDocRefs(docs []model.DocRef) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	idStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Linked Docs")

	var idWidth, typeWidth, statusWidth int
	for _, d := range docs {
		idWidth = max(idWidth, len(model.FormatDocID(d.ID)))
		typeWidth = max(typeWidth, len(d.Type))
		statusWidth = max(statusWidth, len(d.Status))
	}

	var lines []string
	for _, d := range docs {
		id := model.FormatDocID(d.ID)
		line := fmt.Sprintf("  %s %s   %s   %s   %s",
			dimStyle.Render("▸"),
			idStyle.Render(id)+strings.Repeat(" ", idWidth-len(id)),
			d.Type+strings.Repeat(" ", typeWidth-len(d.Type)),
			d.Status+strings.Repeat(" ", statusWidth-len(d.Status)),
			d.Title,
		)
		lines = append(lines, line)
	}

	return header + "\n" + strings.Join(lines, "\n")
}

func renderLinkedProposals(proposals []model.Proposal) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	idStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Linked Proposals")

	var idWidth, statusWidth int
	for _, p := range proposals {
		idWidth = max(idWidth, len(model.FormatProposalID(p.ID)))
		statusWidth = max(statusWidth, len(string(p.Status)))
	}

	var lines []string
	for _, p := range proposals {
		id := model.FormatProposalID(p.ID)
		status := string(p.Status)
		line := fmt.Sprintf("  %s %s   %s   %s",
			dimStyle.Render("▸"),
			idStyle.Render(id)+strings.Repeat(" ", idWidth-len(id)),
			status+strings.Repeat(" ", statusWidth-len(status)),
			truncate(p.Description, maxTitleWidth),
		)
		lines = append(lines, line)
	}

	return header + "\n" + strings.Join(lines, "\n")
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
	statusStyle := lipgloss.NewStyle().Foreground(ColorFromName(issue.Status.Color()))
	priorityStyle := lipgloss.NewStyle().Foreground(ColorFromName(issue.Priority.Color()))
	kindStyle := lipgloss.NewStyle().Foreground(ColorFromName(issue.Kind.Color()))

	return fmt.Sprintf("%s %s %s %s %s",
		statusStyle.Render(issueStatusLabel(issue)),
		priorityStyle.Render(issue.Priority.Icon()),
		kindStyle.Render(issue.Kind.Icon()),
		model.FormatID(issue.ID),
		truncate(issue.Title, maxTitleWidth),
	)
}

// RelationArrow returns a directional arrow for the given relation type.
func RelationArrow(rt model.RelationType, isSource bool) string {
	if isSource {
		switch rt {
		case model.RelationBlocks:
			return "\u2192" // →
		case model.RelationDependsOn:
			return "\u2190" // ←
		case model.RelationRelatesTo:
			return "\u2194" // ↔
		case model.RelationDuplicates:
			return "\u2261" // ≡
		default:
			return "\u2192" // →
		}
	}
	// Inverse direction
	switch rt {
	case model.RelationBlocks:
		return "\u2190" // ←
	case model.RelationDependsOn:
		return "\u2192" // →
	case model.RelationRelatesTo:
		return "\u2194" // ↔
	case model.RelationDuplicates:
		return "\u2261" // ≡
	default:
		return "\u2190" // ←
	}
}

func renderRelations(issueID int, relations []model.Relation) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Relations")

	var lines []string
	for _, rel := range relations {
		var line string
		if rel.SourceIssueID == issueID {
			typeStyle := lipgloss.NewStyle().Foreground(ColorFromName(RelationColor(rel.RelationType)))
			arrow := RelationArrow(rel.RelationType, true)
			line = fmt.Sprintf("  %s %s %s",
				arrow,
				typeStyle.Render(string(rel.RelationType)),
				model.FormatID(rel.TargetIssueID),
			)
		} else {
			typeStyle := lipgloss.NewStyle().Foreground(ColorFromName(RelationColor(rel.RelationType)))
			arrow := RelationArrow(rel.RelationType, false)
			line = fmt.Sprintf("  %s %s %s",
				arrow,
				typeStyle.Render(rel.RelationType.Inverse()),
				model.FormatID(rel.SourceIssueID),
			)
		}
		lines = append(lines, line)
	}

	return header + "\n" + strings.Join(lines, "\n")
}

// RelationColor returns a color name for the given relation type.
func RelationColor(rt model.RelationType) string {
	switch rt {
	case model.RelationBlocks:
		return "red"
	case model.RelationDependsOn:
		return "yellow"
	case model.RelationRelatesTo:
		return "blue"
	case model.RelationDuplicates:
		return "gray"
	default:
		return "white"
	}
}

// RenderCommentList renders a styled comment list. Exported for reuse by the
// comment list CLI command.
func RenderCommentList(comments []*model.Comment) string {
	return renderComments(comments)
}

// commentStyles holds the lipgloss styles shared by every rendered comment
// section (issue comments and doc comments alike).
type commentStyles struct {
	author lipgloss.Style
	time   lipgloss.Style
}

// commentSectionHeader returns the styled "Comments" header and the styles
// used to render each entry beneath it.
func commentSectionHeader() (string, commentStyles) {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	styles := commentStyles{
		author: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12")),
		time:   lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	}
	return sectionStyle.Render("Comments"), styles
}

func renderComments(comments []*model.Comment) string {
	header, styles := commentSectionHeader()

	var parts []string
	for _, c := range comments {
		body, err := RenderMarkdown(c.Body)
		if err != nil {
			body = c.Body
		}

		commentHeader := fmt.Sprintf("%s  %s",
			styles.author.Render(c.AuthorOrAnonymous()),
			styles.time.Render(humanize.Time(c.CreatedAt)),
		)

		parts = append(parts, commentHeader+"\n"+body)
	}

	return header + "\n" + strings.Join(parts, "\n\n")
}

// activityIcon returns a semantic icon for an activity entry.
func activityIcon(a model.Activity) string {
	if a.FieldChanged == "created" {
		return "\u2728" // ✨
	}
	if a.FieldChanged == "status" {
		if a.NewValue != "" {
			return model.Status(a.NewValue).Icon()
		}
		return "\u25cb" // ○
	}
	return "\u270e" // ✎
}

func renderActivity(activity []model.Activity) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	fieldStyle := lipgloss.NewStyle().Bold(true)
	timeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	header := sectionStyle.Render("Activity")

	var lines []string
	for _, a := range activity {
		icon := activityIcon(a)
		var line string
		if a.FieldChanged == "created" {
			line = fmt.Sprintf("  %s Issue created  %s",
				icon,
				timeStyle.Render(humanize.Time(a.CreatedAt)),
			)
		} else {
			actor := a.ChangedBy
			if actor == "" {
				actor = "system"
			}
			line = fmt.Sprintf("  %s %s changed %s  %s",
				icon,
				actor,
				fieldStyle.Render(a.FieldChanged),
				timeStyle.Render(humanize.Time(a.CreatedAt)),
			)
		}
		lines = append(lines, line)
	}

	return header + "\n" + strings.Join(lines, "\n")
}

// renderPlainDetail renders a detail view without any color or styling.
func renderPlainDetail(issue *model.Issue, subIssues []*model.Issue, relations []model.Relation, linkedProposals []model.Proposal, comments []*model.Comment, activity []model.Activity, disposition *model.IssueRunDisposition) string {
	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "%s %s  %s\n", issue.Kind.Icon(), model.FormatID(issue.ID), issue.Title)
	// The resolution rides beside the status HERE TOO (DKT-245 said `issue
	// show` prints both; only the styled renderer did). NO_COLOR is what every
	// piped reader gets — CI, an agent's `docket issue show` — so the surface
	// that dropped the "⊘ abandoned" marker was the one read by the readers
	// DKT-404 caught misreading these issues as pending.
	status := statusLabel(issue.Status)
	if issue.Resolution != "" {
		status += "  ⊘ " + issue.Resolution
	}
	fmt.Fprintf(&b, "%s  %s %s\n", status, issue.Priority.Icon(), string(issue.Priority))

	// Metadata
	b.WriteString("\n")
	fmt.Fprintf(&b, "Type: %s %s\n", issue.Kind.Icon(), string(issue.Kind))
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

	// Run disposition (DKT-404), in the same place the styled view puts it.
	if disposition != nil {
		fmt.Fprintf(&b, "\n%s\n", RunDispositionHeading)
		fmt.Fprintf(&b, "  %s\n", runDispositionHeadline(disposition))
		for i, line := range runDispositionReasonLines(disposition.Reason) {
			if i == 0 {
				fmt.Fprintf(&b, "    reason: %s\n", line)
				continue
			}
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}

	// Files
	if len(issue.Files) > 0 {
		b.WriteString("\nFiles\n")
		for _, f := range issue.Files {
			fmt.Fprintf(&b, "  > %s\n", f)
		}
	}

	if len(issue.Docs) > 0 {
		var idWidth, typeWidth, statusWidth int
		for _, d := range issue.Docs {
			idWidth = max(idWidth, len(model.FormatDocID(d.ID)))
			typeWidth = max(typeWidth, len(d.Type))
			statusWidth = max(statusWidth, len(d.Status))
		}
		b.WriteString("\nLinked Docs\n")
		for _, d := range issue.Docs {
			fmt.Fprintf(&b, "  > %-*s   %-*s   %-*s   %s\n",
				idWidth, model.FormatDocID(d.ID),
				typeWidth, d.Type,
				statusWidth, d.Status,
				d.Title,
			)
		}
	}

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
			fmt.Fprintf(&b, "  %s %s %s %s %s\n",
				statusLabel(sub.Status),
				sub.Priority.Icon(),
				sub.Kind.Icon(),
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
				arrow := RelationArrow(rel.RelationType, true)
				fmt.Fprintf(&b, "  %s %s %s\n", arrow, string(rel.RelationType), model.FormatID(rel.TargetIssueID))
			} else {
				arrow := RelationArrow(rel.RelationType, false)
				fmt.Fprintf(&b, "  %s %s %s\n", arrow, rel.RelationType.Inverse(), model.FormatID(rel.SourceIssueID))
			}
		}
	}

	if len(linkedProposals) > 0 {
		var idWidth, statusWidth int
		for _, p := range linkedProposals {
			idWidth = max(idWidth, len(model.FormatProposalID(p.ID)))
			statusWidth = max(statusWidth, len(string(p.Status)))
		}
		b.WriteString("\nLinked Proposals\n")
		for _, p := range linkedProposals {
			fmt.Fprintf(&b, "  > %-*s   %-*s   %s\n",
				idWidth, model.FormatProposalID(p.ID),
				statusWidth, string(p.Status),
				truncate(p.Description, maxTitleWidth),
			)
		}
	}

	// Comments
	if len(comments) > 0 {
		b.WriteString("\nComments\n")
		for _, c := range comments {
			fmt.Fprintf(&b, "  %s  %s\n  %s\n\n", c.AuthorOrAnonymous(), humanize.Time(c.CreatedAt), c.Body)
		}
	}

	// Activity
	if len(activity) > 0 {
		b.WriteString("\nActivity\n")
		for _, a := range activity {
			icon := activityIcon(a)
			if a.FieldChanged == "created" {
				fmt.Fprintf(&b, "  %s Issue created  %s\n", icon, humanize.Time(a.CreatedAt))
			} else {
				actor := a.ChangedBy
				if actor == "" {
					actor = "system"
				}
				fmt.Fprintf(&b, "  %s %s changed %s  %s\n",
					icon, actor, a.FieldChanged, humanize.Time(a.CreatedAt))
			}
		}
	}

	return b.String()
}
