package render

import (
	"fmt"
	"strings"

	humanize "github.com/dustin/go-humanize"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

type DocRow struct {
	Doc             *model.Doc
	CurrentRevision int
	RevisionsCount  int
}

func RenderDocList(rows []DocRow) string {
	if len(rows) == 0 {
		return EmptyState("No documents found.", "Create one with: docket doc create", false)
	}

	if !ColorsEnabled() {
		return renderPlainDocList(rows)
	}

	headers := []string{"ID", "Type", "Status", "Title", "Author", "Revisions", "Updated"}

	tableRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		tableRows = append(tableRows, docToRow(r))
	}

	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderStyle(lipgloss.NewStyle().Foreground(lipgloss.Color("8"))).
		Headers(headers...).
		Rows(tableRows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			s := lipgloss.NewStyle().PaddingLeft(1).PaddingRight(1)

			if row == table.HeaderRow {
				return s.Bold(true).Foreground(lipgloss.Color("15"))
			}

			switch col {
			case 0:
				return s.Foreground(lipgloss.Color("15"))
			case 3:
				return s.Bold(true)
			default:
				return s
			}
		})

	return t.Render()
}

func docToRow(r DocRow) []string {
	return []string{
		model.FormatDocID(r.Doc.ID),
		r.Doc.Type,
		r.Doc.Status,
		truncate(r.Doc.Title, maxTitleWidth),
		r.Doc.Author,
		fmt.Sprintf("%d", r.RevisionsCount),
		humanize.Time(r.Doc.UpdatedAt),
	}
}

func renderPlainDocList(rows []DocRow) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%-10s %-10s %-12s %-42s %-15s %-10s %s\n",
		"ID", "Type", "Status", "Title", "Author", "Revisions", "Updated")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 110))

	for _, r := range rows {
		fmt.Fprintf(&b, "%-10s %-10s %-12s %-42s %-15s %-10d %s\n",
			model.FormatDocID(r.Doc.ID),
			r.Doc.Type,
			r.Doc.Status,
			truncate(r.Doc.Title, maxTitleWidth),
			r.Doc.Author,
			r.RevisionsCount,
			humanize.Time(r.Doc.UpdatedAt),
		)
	}

	return b.String()
}

func RenderDocDetail(doc *model.Doc, revisions []*model.DocRevision, comments []*model.DocComment, linkedIssues []int, linkedProposals []int) string {
	if !ColorsEnabled() {
		return renderPlainDocDetail(doc, revisions, comments, linkedIssues, linkedProposals)
	}

	var sections []string

	sections = append(sections, renderDocHeader(doc))
	sections = append(sections, renderDocMetadata(doc))

	if doc.Body != "" {
		sections = append(sections, renderDocBody(doc.Body))
	}

	if len(linkedIssues) > 0 {
		sections = append(sections, renderDocLinkedIssues(linkedIssues))
	}

	if len(linkedProposals) > 0 {
		sections = append(sections, renderDocLinkedProposals(linkedProposals))
	}

	if len(comments) > 0 {
		sections = append(sections, renderDocComments(comments))
	}

	if len(revisions) > 0 {
		sections = append(sections, RenderDocRevisionHistory(revisions))
	}

	return strings.Join(sections, "\n\n")
}

func renderDocHeader(doc *model.Doc) string {
	idStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	titleStyle := lipgloss.NewStyle().Bold(true)
	typeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	statusStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))

	return fmt.Sprintf("%s  %s\n%s  %s",
		idStyle.Render(model.FormatDocID(doc.ID)),
		titleStyle.Render(doc.Title),
		typeStyle.Render(doc.Type),
		statusStyle.Render(doc.Status),
	)
}

func renderDocMetadata(doc *model.Doc) string {
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var lines []string
	if doc.Author != "" {
		lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Author:"), doc.Author))
	}
	lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Created:"), humanize.Time(doc.CreatedAt)))
	lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Updated:"), humanize.Time(doc.UpdatedAt)))

	return strings.Join(lines, "\n")
}

func renderDocBody(body string) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Body")

	rendered, err := RenderMarkdown(body)
	if err != nil {
		rendered = body
	}

	return header + "\n" + rendered
}

func renderDocLinkedIssues(issueIDs []int) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Linked Issues")

	var lines []string
	for _, id := range issueIDs {
		lines = append(lines, "  "+model.FormatID(id))
	}

	return header + "\n" + strings.Join(lines, "\n")
}

func renderDocLinkedProposals(proposalIDs []int) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Linked Proposals")

	var lines []string
	for _, id := range proposalIDs {
		lines = append(lines, "  "+model.FormatProposalID(id))
	}

	return header + "\n" + strings.Join(lines, "\n")
}

func renderDocComments(comments []*model.DocComment) string {
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

		author := c.Author
		if author == "" {
			author = "anonymous"
		}

		commentHeader := fmt.Sprintf("%s  %s",
			authorStyle.Render(author),
			timeStyle.Render(humanize.Time(c.CreatedAt)),
		)

		parts = append(parts, commentHeader+"\n"+body)
	}

	return header + "\n" + strings.Join(parts, "\n\n")
}

func RenderDocRevisionHistory(revisions []*model.DocRevision) string {
	if len(revisions) == 0 {
		return EmptyState("No revisions yet.", "", true)
	}

	if !ColorsEnabled() {
		return renderPlainRevisionHistory(revisions)
	}

	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	revStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	kindStyle := lipgloss.NewStyle().Bold(true)
	timeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	header := sectionStyle.Render("Revisions")

	var lines []string
	for _, r := range revisions {
		author := r.Author
		if author == "" {
			author = "system"
		}
		line := fmt.Sprintf("  %s  %s  %s  %s",
			revStyle.Render(fmt.Sprintf("r%d", r.RevisionNumber)),
			kindStyle.Render(r.ChangeKind),
			author,
			timeStyle.Render(humanize.Time(r.CreatedAt)),
		)
		lines = append(lines, line)
	}

	return header + "\n" + strings.Join(lines, "\n")
}

func renderPlainRevisionHistory(revisions []*model.DocRevision) string {
	var b strings.Builder

	b.WriteString("Revisions\n")
	for _, r := range revisions {
		author := r.Author
		if author == "" {
			author = "system"
		}
		fmt.Fprintf(&b, "  r%d  %s  %s  %s\n",
			r.RevisionNumber,
			r.ChangeKind,
			author,
			humanize.Time(r.CreatedAt),
		)
	}

	return strings.TrimRight(b.String(), "\n")
}

func renderPlainDocDetail(doc *model.Doc, revisions []*model.DocRevision, comments []*model.DocComment, linkedIssues []int, linkedProposals []int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s  %s\n", model.FormatDocID(doc.ID), doc.Title)
	fmt.Fprintf(&b, "%s  %s\n", doc.Type, doc.Status)

	b.WriteString("\n")
	if doc.Author != "" {
		fmt.Fprintf(&b, "Author: %s\n", doc.Author)
	}
	fmt.Fprintf(&b, "Created: %s\n", humanize.Time(doc.CreatedAt))
	fmt.Fprintf(&b, "Updated: %s\n", humanize.Time(doc.UpdatedAt))

	if doc.Body != "" {
		fmt.Fprintf(&b, "\nBody\n%s\n", doc.Body)
	}

	if len(linkedIssues) > 0 {
		b.WriteString("\nLinked Issues\n")
		for _, id := range linkedIssues {
			fmt.Fprintf(&b, "  %s\n", model.FormatID(id))
		}
	}

	if len(linkedProposals) > 0 {
		b.WriteString("\nLinked Proposals\n")
		for _, id := range linkedProposals {
			fmt.Fprintf(&b, "  %s\n", model.FormatProposalID(id))
		}
	}

	if len(comments) > 0 {
		b.WriteString("\nComments\n")
		for _, c := range comments {
			author := c.Author
			if author == "" {
				author = "anonymous"
			}
			fmt.Fprintf(&b, "  %s  %s\n  %s\n\n", author, humanize.Time(c.CreatedAt), c.Body)
		}
	}

	if len(revisions) > 0 {
		b.WriteString("\n")
		b.WriteString(renderPlainRevisionHistory(revisions))
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}
