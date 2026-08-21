package cli

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export issues to JSON, CSV, or Markdown",
	RunE: func(cmd *cobra.Command, args []string) error {
		conn := getDB(cmd)

		format, _ := cmd.Flags().GetString("format")
		filePath, _ := cmd.Flags().GetString("file")
		statuses, _ := cmd.Flags().GetStringSlice("status")
		labels, _ := cmd.Flags().GetStringSlice("label")

		// Validate format.
		switch format {
		case "json", "csv", "markdown":
		default:
			return cmdErr(
				fmt.Errorf("invalid format %q: must be one of json, csv, markdown", format),
				output.ErrValidation,
			)
		}

		// Validate filter enum values.
		for _, s := range statuses {
			if err := model.ValidateStatus(model.Status(s)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
		}

		// Fetch all data.
		data := model.ExportData{
			Version:    1,
			ExportedAt: time.Now().UTC().Format(time.RFC3339),
		}
		// Scoped to THIS project (v12): an export is one project's tracker,
		// not the whole shared store.
		for _, c := range exportCollections {
			if err := c.fetch(conn, getProjectID(cmd), &data); err != nil {
				return cmdErr(fmt.Errorf("fetching %s: %w", c.name, err), output.ErrGeneral)
			}
		}

		// Apply filters if provided.
		if len(statuses) > 0 || len(labels) > 0 {
			filterExportData(&data, statuses, labels)
		}

		// Ensure nil slices become empty arrays in JSON.
		for _, c := range exportCollections {
			c.normalize(&data)
		}

		// Generate output based on format.
		var raw string
		var err error
		switch format {
		case "json":
			raw, err = renderExportJSON(data)
		case "csv":
			raw, err = renderExportCSV(data.Issues)
		case "markdown":
			raw, err = renderExportMarkdown(data.Issues, data.Comments)
		}
		if err != nil {
			return cmdErr(fmt.Errorf("rendering export: %w", err), output.ErrGeneral)
		}

		// Write to file or stdout.
		if filePath != "" {
			if err := os.WriteFile(filePath, []byte(raw), 0o644); err != nil {
				return cmdErr(fmt.Errorf("writing file: %w", err), output.ErrGeneral)
			}
			fmt.Fprintf(os.Stderr, "Exported to %s\n", filePath)
			return nil
		}

		fmt.Fprint(os.Stdout, raw)
		return nil
	},
}

func init() {
	exportCmd.Flags().StringP("format", "o", "json", "Export format: json, csv, markdown")
	exportCmd.Flags().StringP("file", "f", "", "Output file path (default: stdout)")
	exportCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (repeatable)")
	exportCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (OR, repeatable)")
	rootCmd.AddCommand(exportCmd)
}

// filterExportData narrows an already-fetched export document to the issues
// matching the given status and label filters, and to whatever else is still
// reachable once those issues are gone.
//
// Unlike fetching, normalizing, and restoring, this stays written out
// collection by collection, because no two collections reach the surviving set
// the same way: comments and activity follow their issue, relations need BOTH
// endpoints to survive, docs survive by having a surviving link rather than by
// any field of their own, revisions and doc comments follow the docs that
// survived that way, votes follow proposals which themselves survived by link,
// and labels survive only if a surviving mapping still points at them. The
// order matters too — each of the last few steps reads a set the step above it
// just narrowed. A table of predicates would have to encode that dependency
// anyway, and would hide it while doing so.
func filterExportData(data *model.ExportData, statuses, labels []string) {
	data.Issues = filterIssues(data.Issues, statuses, labels)

	// Build set of filtered issue IDs.
	issueIDs := make(map[int]bool, len(data.Issues))
	for _, issue := range data.Issues {
		issueIDs[issue.ID] = true
	}

	// Filter comments to only those belonging to filtered issues.
	filtered := make([]*model.Comment, 0, len(data.Comments))
	for _, c := range data.Comments {
		if issueIDs[c.IssueID] {
			filtered = append(filtered, c)
		}
	}
	data.Comments = filtered

	// Filter relations to only those where both sides are in the filtered set.
	filteredRels := make([]model.Relation, 0, len(data.Relations))
	for _, r := range data.Relations {
		if issueIDs[r.SourceIssueID] && issueIDs[r.TargetIssueID] {
			filteredRels = append(filteredRels, r)
		}
	}
	data.Relations = filteredRels

	// Filter label mappings to only those for filtered issues.
	filteredMappings := make([]model.IssueLabelMapping, 0, len(data.IssueLabelMappings))
	for _, m := range data.IssueLabelMappings {
		if issueIDs[m.IssueID] {
			filteredMappings = append(filteredMappings, m)
		}
	}
	data.IssueLabelMappings = filteredMappings

	// Filter file mappings to only those for filtered issues.
	filteredFileMappings := make([]model.IssueFileMapping, 0, len(data.IssueFileMappings))
	for _, m := range data.IssueFileMappings {
		if issueIDs[m.IssueID] {
			filteredFileMappings = append(filteredFileMappings, m)
		}
	}
	data.IssueFileMappings = filteredFileMappings

	// Filter activity log to only entries for filtered issues.
	filteredActivity := make([]*model.Activity, 0, len(data.ActivityLog))
	for _, a := range data.ActivityLog {
		if issueIDs[a.IssueID] {
			filteredActivity = append(filteredActivity, a)
		}
	}
	data.ActivityLog = filteredActivity

	// Filter doc-issue links to only those whose issue survives the filter.
	filteredDocIssueLinks := make([]model.DocIssueLink, 0, len(data.DocIssueLinks))
	for _, l := range data.DocIssueLinks {
		if issueIDs[l.IssueID] {
			filteredDocIssueLinks = append(filteredDocIssueLinks, l)
		}
	}
	data.DocIssueLinks = filteredDocIssueLinks

	// Filter proposal-issue links to only those whose issue survives the filter.
	filteredProposalIssues := make([]model.ProposalIssueLink, 0, len(data.ProposalIssues))
	for _, l := range data.ProposalIssues {
		if issueIDs[l.IssueID] {
			filteredProposalIssues = append(filteredProposalIssues, l)
		}
	}
	data.ProposalIssues = filteredProposalIssues

	survivingDocIDs := make(map[int]bool, len(data.DocIssueLinks))
	for _, l := range data.DocIssueLinks {
		survivingDocIDs[l.DocID] = true
	}
	filteredDocs := make([]*model.Doc, 0, len(data.Docs))
	for _, d := range data.Docs {
		if survivingDocIDs[d.ID] {
			filteredDocs = append(filteredDocs, d)
		}
	}
	data.Docs = filteredDocs

	filteredDocRevisions := make([]*model.DocRevision, 0, len(data.DocRevisions))
	for _, r := range data.DocRevisions {
		if survivingDocIDs[r.DocID] {
			filteredDocRevisions = append(filteredDocRevisions, r)
		}
	}
	data.DocRevisions = filteredDocRevisions

	filteredDocComments := make([]*model.DocComment, 0, len(data.DocComments))
	for _, c := range data.DocComments {
		if survivingDocIDs[c.DocID] {
			filteredDocComments = append(filteredDocComments, c)
		}
	}
	data.DocComments = filteredDocComments

	survivingProposalIDs := make(map[int]bool, len(data.ProposalIssues))
	for _, l := range data.ProposalIssues {
		survivingProposalIDs[l.ProposalID] = true
	}
	filteredProposals := make([]*model.Proposal, 0, len(data.Proposals))
	for _, p := range data.Proposals {
		if survivingProposalIDs[p.ID] {
			filteredProposals = append(filteredProposals, p)
		}
	}
	data.Proposals = filteredProposals

	filteredVotes := make([]*model.Vote, 0, len(data.Votes))
	for _, v := range data.Votes {
		if survivingProposalIDs[v.ProposalID] {
			filteredVotes = append(filteredVotes, v)
		}
	}
	data.Votes = filteredVotes

	filteredProposalDocs := make([]model.ProposalDocLink, 0, len(data.ProposalDocs))
	for _, l := range data.ProposalDocs {
		if survivingProposalIDs[l.ProposalID] && survivingDocIDs[l.DocID] {
			filteredProposalDocs = append(filteredProposalDocs, l)
		}
	}
	data.ProposalDocs = filteredProposalDocs

	// Filter labels to only those referenced by remaining mappings.
	usedLabelIDs := make(map[int]bool)
	for _, m := range data.IssueLabelMappings {
		usedLabelIDs[m.LabelID] = true
	}
	filteredLabels := make([]*model.Label, 0, len(data.Labels))
	for _, l := range data.Labels {
		if usedLabelIDs[l.ID] {
			filteredLabels = append(filteredLabels, l)
		}
	}
	data.Labels = filteredLabels
}

// filterIssues returns issues matching the given status and label filters.
func filterIssues(issues []*model.Issue, statuses, labels []string) []*model.Issue {
	statusSet := make(map[string]bool, len(statuses))
	for _, s := range statuses {
		statusSet[s] = true
	}
	labelSet := make(map[string]bool, len(labels))
	for _, l := range labels {
		labelSet[l] = true
	}

	filtered := make([]*model.Issue, 0, len(issues))
	for _, issue := range issues {
		if len(statusSet) > 0 && !statusSet[string(issue.Status)] {
			continue
		}
		if len(labelSet) > 0 {
			hasAny := false
			for _, il := range issue.Labels {
				if labelSet[il] {
					hasAny = true
					break
				}
			}
			if !hasAny {
				continue
			}
		}
		filtered = append(filtered, issue)
	}

	survivingIDs := make(map[int]bool, len(filtered))
	for _, issue := range filtered {
		survivingIDs[issue.ID] = true
	}
	for _, issue := range filtered {
		if issue.ParentID != nil && !survivingIDs[*issue.ParentID] {
			issue.ParentID = nil
		}
	}

	return filtered
}

// renderExportJSON produces a pretty-printed JSON string of the export data.
func renderExportJSON(data model.ExportData) (string, error) {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// renderExportCSV produces a CSV string with a header row and one row per issue.
func renderExportCSV(issues []*model.Issue) (string, error) {
	var buf strings.Builder
	cw := csv.NewWriter(&buf)

	header := []string{"id", "parent_id", "title", "description", "status", "priority", "type", "assignee", "labels", "files", "created_at", "updated_at"}
	if err := cw.Write(header); err != nil {
		return "", err
	}

	for _, issue := range issues {
		parentID := ""
		if issue.ParentID != nil {
			parentID = model.FormatID(*issue.ParentID)
		}

		labelsStr := strings.Join(issue.Labels, ",")
		// Use ";" to separate file paths since paths may contain commas.
		filesStr := strings.Join(issue.Files, ";")

		row := []string{
			model.FormatID(issue.ID),
			parentID,
			csvSafe(issue.Title),
			csvSafe(issue.Description),
			string(issue.Status),
			string(issue.Priority),
			string(issue.Kind),
			csvSafe(issue.Assignee),
			csvSafe(labelsStr),
			csvSafe(filesStr),
			issue.CreatedAt.UTC().Format(time.RFC3339),
			issue.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if err := cw.Write(row); err != nil {
			return "", err
		}
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		return "", err
	}

	return buf.String(), nil
}

func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}

// escapeMarkdown replaces characters that have special meaning in Markdown so
// that arbitrary user text can be safely embedded in headings and inline spans.
func escapeMarkdown(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`#`, `\#`,
		`*`, `\*`,
		`_`, `\_`,
		`[`, `\[`,
		`]`, `\]`,
		`<`, `\<`,
		`>`, `\>`,
		"`", "\\`",
		`|`, `\|`,
	)
	return r.Replace(s)
}

// renderExportMarkdown produces a Markdown string grouping issues by status.
func renderExportMarkdown(issues []*model.Issue, comments []*model.Comment) (string, error) {
	// Group issues by status.
	statusOrder := []model.Status{
		model.StatusBacklog,
		model.StatusTodo,
		model.StatusInProgress,
		model.StatusReview,
		model.StatusDone,
	}

	grouped := make(map[model.Status][]*model.Issue)
	for _, issue := range issues {
		grouped[issue.Status] = append(grouped[issue.Status], issue)
	}

	// Build comment lookup by issue ID.
	commentsByIssue := make(map[int][]*model.Comment)
	for _, c := range comments {
		commentsByIssue[c.IssueID] = append(commentsByIssue[c.IssueID], c)
	}

	var buf strings.Builder
	buf.WriteString("# Docket Export\n\n")

	for _, status := range statusOrder {
		group := grouped[status]
		if len(group) == 0 {
			continue
		}

		buf.WriteString(fmt.Sprintf("## %s\n\n", string(status)))

		for _, issue := range group {
			buf.WriteString(fmt.Sprintf("### %s: %s\n\n", model.FormatID(issue.ID), escapeMarkdown(issue.Title)))

			// Metadata.
			buf.WriteString(fmt.Sprintf("- **Priority:** %s\n", escapeMarkdown(string(issue.Priority))))
			buf.WriteString(fmt.Sprintf("- **Type:** %s\n", escapeMarkdown(string(issue.Kind))))
			if issue.Assignee != "" {
				buf.WriteString(fmt.Sprintf("- **Assignee:** %s\n", escapeMarkdown(issue.Assignee)))
			}
			if len(issue.Labels) > 0 {
				escaped := make([]string, len(issue.Labels))
				for i, l := range issue.Labels {
					escaped[i] = escapeMarkdown(l)
				}
				buf.WriteString(fmt.Sprintf("- **Labels:** %s\n", strings.Join(escaped, ", ")))
			}
			if len(issue.Files) > 0 {
				escapedFiles := make([]string, len(issue.Files))
				for i, f := range issue.Files {
					escapedFiles[i] = escapeMarkdown(f)
				}
				buf.WriteString(fmt.Sprintf("- **Files:** %s\n", strings.Join(escapedFiles, ", ")))
			}
			buf.WriteString("\n")

			// Description.
			if issue.Description != "" {
				buf.WriteString(escapeMarkdown(issue.Description) + "\n\n")
			}

			// Comments.
			issueComments := commentsByIssue[issue.ID]
			if len(issueComments) > 0 {
				buf.WriteString("**Comments:**\n\n")
				for _, c := range issueComments {
					buf.WriteString(fmt.Sprintf("> **%s** (%s):\n> %s\n\n",
						escapeMarkdown(c.AuthorOrAnonymous()),
						c.CreatedAt.UTC().Format(time.RFC3339),
						escapeMarkdown(c.Body),
					))
				}
			}
		}
	}

	return buf.String(), nil
}
