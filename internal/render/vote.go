package render

import (
	"fmt"
	"strings"

	humanize "github.com/dustin/go-humanize"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// ProposalRow pairs a proposal with its vote counts for table rendering.
type ProposalRow struct {
	Proposal *model.Proposal
	VoteCast int
}

// criticalityColor returns a color name for a given criticality level.
func criticalityColor(c model.Criticality) string {
	switch c {
	case model.CriticalityLow:
		return "gray"
	case model.CriticalityMedium:
		return "blue"
	case model.CriticalityHigh:
		return "yellow"
	case model.CriticalityCritical:
		return "red"
	default:
		return "white"
	}
}

// proposalStatusColor returns a color name for a given proposal status.
func proposalStatusColor(s model.ProposalStatus) string {
	switch s {
	case model.ProposalStatusOpen:
		return "blue"
	case model.ProposalStatusApproved:
		return "green"
	case model.ProposalStatusRejected:
		return "red"
	case model.ProposalStatusCommitted:
		return "magenta"
	default:
		return "white"
	}
}

// verdictColor returns a color name for a given verdict.
func verdictColor(v model.Verdict) string {
	switch v {
	case model.VerdictApprove:
		return "green"
	case model.VerdictReject:
		return "red"
	case model.VerdictApproveWithConcerns:
		return "yellow"
	default:
		return "white"
	}
}

// RenderProposalTable renders a list of proposals as a formatted table.
func RenderProposalTable(rows []ProposalRow) string {
	if len(rows) == 0 {
		return EmptyState("No proposals found.", "Create one with: docket vote create", false)
	}

	if !ColorsEnabled() {
		return renderPlainProposalTable(rows)
	}

	headers := []string{"ID", "Description", "Status", "Votes", "Criticality"}

	tableRows := make([][]string, 0, len(rows))
	for _, r := range rows {
		tableRows = append(tableRows, proposalToRow(r))
	}

	type rowColors struct {
		statusColor      string
		criticalityColor string
	}
	colorMap := make([]rowColors, len(rows))
	for i, r := range rows {
		colorMap[i] = rowColors{
			statusColor:      proposalStatusColor(r.Proposal.Status),
			criticalityColor: criticalityColor(r.Proposal.Criticality),
		}
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

			if row < 0 || row >= len(colorMap) {
				return s
			}

			rc := colorMap[row]
			switch col {
			case 0: // ID
				return s.Foreground(lipgloss.Color("15"))
			case 2: // Status
				return s.Foreground(ColorFromName(rc.statusColor))
			case 4: // Criticality
				return s.Foreground(ColorFromName(rc.criticalityColor))
			default:
				return s
			}
		})

	return t.Render()
}

func proposalToRow(r ProposalRow) []string {
	return []string{
		model.FormatProposalID(r.Proposal.ID),
		truncate(r.Proposal.Description, maxTitleWidth),
		string(r.Proposal.Status),
		fmt.Sprintf("%d/%d", r.VoteCast, r.Proposal.RequiredVoters),
		string(r.Proposal.Criticality),
	}
}

func renderPlainProposalTable(rows []ProposalRow) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%-10s %-42s %-12s %-10s %s\n",
		"ID", "Description", "Status", "Votes", "Criticality")
	fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 90))

	for _, r := range rows {
		fmt.Fprintf(&b, "%-10s %-42s %-12s %-10s %s\n",
			model.FormatProposalID(r.Proposal.ID),
			truncate(r.Proposal.Description, maxTitleWidth),
			string(r.Proposal.Status),
			fmt.Sprintf("%d/%d", r.VoteCast, r.Proposal.RequiredVoters),
			string(r.Proposal.Criticality),
		)
	}

	return b.String()
}

// RenderProposalDetail renders a full proposal detail view with votes and linked issues.
func RenderProposalDetail(proposal *model.Proposal, votes []*model.Vote, linkedIssues []int) string {
	if !ColorsEnabled() {
		return renderPlainProposalDetail(proposal, votes, linkedIssues)
	}

	var sections []string

	// Header
	sections = append(sections, renderProposalHeader(proposal))

	// Metadata
	sections = append(sections, renderProposalMetadata(proposal))

	// Description
	if proposal.Description != "" {
		sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
		header := sectionStyle.Render("Description")
		sections = append(sections, header+"\n"+proposal.Description)
	}

	// Linked Issues
	if len(linkedIssues) > 0 {
		sections = append(sections, renderLinkedIssues(linkedIssues))
	}

	// Votes
	if len(votes) > 0 {
		sections = append(sections, renderVoteList(votes))
	}

	return strings.Join(sections, "\n\n")
}

func renderProposalHeader(proposal *model.Proposal) string {
	idStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	statusStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorFromName(proposalStatusColor(proposal.Status)))
	critStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorFromName(criticalityColor(proposal.Criticality)))

	return fmt.Sprintf("%s  %s\n%s  %s",
		idStyle.Render(model.FormatProposalID(proposal.ID)),
		statusStyle.Render(strings.ToUpper(string(proposal.Status))),
		critStyle.Render(string(proposal.Criticality)),
		lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(
			fmt.Sprintf("threshold: %.0f%%", proposal.Threshold*100),
		),
	)
}

func renderProposalMetadata(proposal *model.Proposal) string {
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	var lines []string

	if proposal.CreatedBy != "" {
		lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Created by:"), proposal.CreatedBy))
	}
	lines = append(lines, fmt.Sprintf("%s %d", labelStyle.Render("Required voters:"), proposal.RequiredVoters))
	lines = append(lines, fmt.Sprintf("%s %.0f%%", labelStyle.Render("Threshold:"), proposal.Threshold*100))

	if proposal.WeightedScore != nil {
		lines = append(lines, fmt.Sprintf("%s %.2f", labelStyle.Render("Weighted score:"), *proposal.WeightedScore))
	}

	lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Created:"), humanize.Time(proposal.CreatedAt)))
	lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Updated:"), humanize.Time(proposal.UpdatedAt)))

	if proposal.FinalOutcome != "" {
		lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Final outcome:"), proposal.FinalOutcome))
	}
	if proposal.EscalationReason != nil {
		lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Escalation reason:"), *proposal.EscalationReason))
	}

	if proposal.Rationale != "" {
		lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Rationale:"), proposal.Rationale))
	}
	if len(proposal.DomainTags) > 0 {
		tagStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
		var tags []string
		for _, tag := range proposal.DomainTags {
			tags = append(tags, tagStyle.Render("["+tag+"]"))
		}
		lines = append(lines, fmt.Sprintf("%s %s", labelStyle.Render("Domain tags:"), strings.Join(tags, " ")))
	}
	if len(proposal.FilesChanged) > 0 {
		lines = append(lines, labelStyle.Render("Files changed:"))
		for _, f := range proposal.FilesChanged {
			lines = append(lines, "  "+f)
		}
	}

	return strings.Join(lines, "\n")
}

func renderLinkedIssues(issueIDs []int) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Linked Issues")

	var lines []string
	for _, id := range issueIDs {
		lines = append(lines, "  "+model.FormatID(id))
	}

	return header + "\n" + strings.Join(lines, "\n")
}

func renderVoteList(votes []*model.Vote) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Votes")

	var lines []string
	for _, v := range votes {
		verdictStyle := lipgloss.NewStyle().Foreground(ColorFromName(verdictColor(v.Verdict)))
		timeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		nameStyle := lipgloss.NewStyle().Bold(true)

		effectiveWeight := v.Confidence * v.DomainRelevance
		line := fmt.Sprintf("  %s  %s  conf=%.2f  rel=%.2f  weight=%.2f  %s",
			nameStyle.Render(v.VoterName),
			verdictStyle.Render(string(v.Verdict)),
			v.Confidence,
			v.DomainRelevance,
			effectiveWeight,
			timeStyle.Render(humanize.Time(v.CreatedAt)),
		)

		if v.FindingsJSON != nil {
			line += renderStructuredFindings(v.FindingsJSON)
		} else if v.Findings != "" {
			line += "\n    " + lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(truncate(v.Findings, 80))
		}

		if v.Summary != "" {
			line += "\n    " + lipgloss.NewStyle().Italic(true).Foreground(lipgloss.Color("8")).Render(v.Summary)
		}

		lines = append(lines, line)
	}

	return header + "\n" + strings.Join(lines, "\n")
}

func renderStructuredFindings(f *model.Findings) string {
	var parts []string
	blockerStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("red"))
	concernStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("yellow"))
	suggestionStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	for _, b := range f.Blockers {
		parts = append(parts, "    "+blockerStyle.Render("BLOCKER: "+b))
	}
	for _, c := range f.Concerns {
		parts = append(parts, "    "+concernStyle.Render("CONCERN: "+c))
	}
	for _, s := range f.Suggestions {
		parts = append(parts, "    "+suggestionStyle.Render("SUGGESTION: "+s))
	}

	if len(parts) == 0 {
		return ""
	}
	return "\n" + strings.Join(parts, "\n")
}

func renderPlainProposalDetail(proposal *model.Proposal, votes []*model.Vote, linkedIssues []int) string {
	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "%s  %s\n", model.FormatProposalID(proposal.ID), strings.ToUpper(string(proposal.Status)))
	fmt.Fprintf(&b, "%s  threshold: %.0f%%\n", string(proposal.Criticality), proposal.Threshold*100)

	// Metadata
	b.WriteString("\n")
	if proposal.CreatedBy != "" {
		fmt.Fprintf(&b, "Created by: %s\n", proposal.CreatedBy)
	}
	fmt.Fprintf(&b, "Required voters: %d\n", proposal.RequiredVoters)
	fmt.Fprintf(&b, "Threshold: %.0f%%\n", proposal.Threshold*100)
	if proposal.WeightedScore != nil {
		fmt.Fprintf(&b, "Weighted score: %.2f\n", *proposal.WeightedScore)
	}
	fmt.Fprintf(&b, "Created: %s\n", humanize.Time(proposal.CreatedAt))
	fmt.Fprintf(&b, "Updated: %s\n", humanize.Time(proposal.UpdatedAt))
	if proposal.FinalOutcome != "" {
		fmt.Fprintf(&b, "Final outcome: %s\n", proposal.FinalOutcome)
	}
	if proposal.EscalationReason != nil {
		fmt.Fprintf(&b, "Escalation reason: %s\n", *proposal.EscalationReason)
	}
	if proposal.Rationale != "" {
		fmt.Fprintf(&b, "Rationale: %s\n", proposal.Rationale)
	}
	if len(proposal.DomainTags) > 0 {
		fmt.Fprintf(&b, "Domain tags: %s\n", strings.Join(proposal.DomainTags, ", "))
	}
	if len(proposal.FilesChanged) > 0 {
		b.WriteString("Files changed:\n")
		for _, f := range proposal.FilesChanged {
			fmt.Fprintf(&b, "  %s\n", f)
		}
	}

	// Description
	if proposal.Description != "" {
		fmt.Fprintf(&b, "\nDescription\n%s\n", proposal.Description)
	}

	// Linked Issues
	if len(linkedIssues) > 0 {
		b.WriteString("\nLinked Issues\n")
		for _, id := range linkedIssues {
			fmt.Fprintf(&b, "  %s\n", model.FormatID(id))
		}
	}

	// Votes
	if len(votes) > 0 {
		b.WriteString("\nVotes\n")
		for _, v := range votes {
			effectiveWeight := v.Confidence * v.DomainRelevance
			fmt.Fprintf(&b, "  %s  %s  conf=%.2f  rel=%.2f  weight=%.2f  %s\n",
				v.VoterName,
				string(v.Verdict),
				v.Confidence,
				v.DomainRelevance,
				effectiveWeight,
				humanize.Time(v.CreatedAt),
			)
			if v.FindingsJSON != nil {
				for _, bl := range v.FindingsJSON.Blockers {
					fmt.Fprintf(&b, "    BLOCKER: %s\n", bl)
				}
				for _, c := range v.FindingsJSON.Concerns {
					fmt.Fprintf(&b, "    CONCERN: %s\n", c)
				}
				for _, s := range v.FindingsJSON.Suggestions {
					fmt.Fprintf(&b, "    SUGGESTION: %s\n", s)
				}
			} else if v.Findings != "" {
				fmt.Fprintf(&b, "    %s\n", truncate(v.Findings, 80))
			}
			if v.Summary != "" {
				fmt.Fprintf(&b, "    %s\n", v.Summary)
			}
		}
	}

	return b.String()
}

// RenderVoteResult renders a status banner with weighted score and vote breakdown.
func RenderVoteResult(proposal *model.Proposal, votes []*model.Vote) string {
	if !ColorsEnabled() {
		return renderPlainVoteResult(proposal, votes)
	}

	var sections []string

	// Status banner
	sections = append(sections, renderStatusBanner(proposal))

	// Score summary
	sections = append(sections, renderScoreSummary(proposal))

	// Vote breakdown table
	if len(votes) > 0 {
		sections = append(sections, renderVoteBreakdownTable(votes))
	}

	return strings.Join(sections, "\n\n")
}

func renderStatusBanner(proposal *model.Proposal) string {
	label := strings.ToUpper(string(proposal.Status))
	color := ColorFromName(proposalStatusColor(proposal.Status))

	bannerStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(color).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(color).
		Padding(0, 2)

	return bannerStyle.Render(label)
}

func renderScoreSummary(proposal *model.Proposal) string {
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	scoreStr := "N/A"
	if proposal.WeightedScore != nil {
		scoreStr = fmt.Sprintf("%.2f", *proposal.WeightedScore)
	}
	thresholdStr := fmt.Sprintf("%.0f%%", proposal.Threshold*100)

	return fmt.Sprintf("%s %s  %s %s",
		labelStyle.Render("Weighted score:"),
		scoreStr,
		labelStyle.Render("Threshold:"),
		thresholdStr,
	)
}

func renderVoteBreakdownTable(votes []*model.Vote) string {
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	header := sectionStyle.Render("Vote Breakdown")

	headers := []string{"Voter", "Role", "Verdict", "Confidence", "Relevance", "Weight"}

	rows := make([][]string, 0, len(votes))
	for _, v := range votes {
		effectiveWeight := v.Confidence * v.DomainRelevance
		rows = append(rows, []string{
			v.VoterName,
			v.VoterRole,
			string(v.Verdict),
			fmt.Sprintf("%.2f", v.Confidence),
			fmt.Sprintf("%.2f", v.DomainRelevance),
			fmt.Sprintf("%.2f", effectiveWeight),
		})
	}

	type rowColor struct {
		verdictColor string
	}
	colorMap := make([]rowColor, len(votes))
	for i, v := range votes {
		colorMap[i] = rowColor{
			verdictColor: verdictColor(v.Verdict),
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

			if col == 2 { // Verdict
				return s.Foreground(ColorFromName(colorMap[row].verdictColor))
			}

			return s
		})

	return header + "\n" + t.Render()
}

func renderPlainVoteResult(proposal *model.Proposal, votes []*model.Vote) string {
	var b strings.Builder

	// Status banner
	fmt.Fprintf(&b, "=== %s ===\n", strings.ToUpper(string(proposal.Status)))

	// Score summary
	scoreStr := "N/A"
	if proposal.WeightedScore != nil {
		scoreStr = fmt.Sprintf("%.2f", *proposal.WeightedScore)
	}
	fmt.Fprintf(&b, "Weighted score: %s  Threshold: %.0f%%\n", scoreStr, proposal.Threshold*100)

	// Vote breakdown
	if len(votes) > 0 {
		b.WriteString("\nVote Breakdown\n")
		fmt.Fprintf(&b, "%-20s %-15s %-22s %-12s %-12s %s\n",
			"Voter", "Role", "Verdict", "Confidence", "Relevance", "Weight")
		fmt.Fprintf(&b, "%s\n", strings.Repeat("-", 90))

		for _, v := range votes {
			effectiveWeight := v.Confidence * v.DomainRelevance
			fmt.Fprintf(&b, "%-20s %-15s %-22s %-12.2f %-12.2f %.2f\n",
				v.VoterName,
				v.VoterRole,
				string(v.Verdict),
				v.Confidence,
				v.DomainRelevance,
				effectiveWeight,
			)
		}
	}

	return b.String()
}
