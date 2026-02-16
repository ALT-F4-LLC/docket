package cli

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
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
		issues, err := db.ListAllIssues(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching issues: %w", err), output.ErrGeneral)
		}

		comments, err := db.ListAllComments(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching comments: %w", err), output.ErrGeneral)
		}

		relations, err := db.GetAllRelations(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
		}

		allLabels, err := db.ListAllLabelsRaw(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching labels: %w", err), output.ErrGeneral)
		}

		mappings, err := db.ListAllIssueLabelMappings(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching label mappings: %w", err), output.ErrGeneral)
		}

		fileMappings, err := db.ListAllIssueFileMappings(conn)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching file mappings: %w", err), output.ErrGeneral)
		}

		// Apply filters if provided.
		if len(statuses) > 0 || len(labels) > 0 {
			issues = filterIssues(issues, statuses, labels)

			// Build set of filtered issue IDs.
			issueIDs := make(map[int]bool, len(issues))
			for _, issue := range issues {
				issueIDs[issue.ID] = true
			}

			// Filter comments to only those belonging to filtered issues.
			filtered := make([]*model.Comment, 0, len(comments))
			for _, c := range comments {
				if issueIDs[c.IssueID] {
					filtered = append(filtered, c)
				}
			}
			comments = filtered

			// Filter relations to only those where both sides are in the filtered set.
			filteredRels := make([]model.Relation, 0, len(relations))
			for _, r := range relations {
				if issueIDs[r.SourceIssueID] && issueIDs[r.TargetIssueID] {
					filteredRels = append(filteredRels, r)
				}
			}
			relations = filteredRels

			// Filter label mappings to only those for filtered issues.
			filteredMappings := make([]model.IssueLabelMapping, 0, len(mappings))
			for _, m := range mappings {
				if issueIDs[m.IssueID] {
					filteredMappings = append(filteredMappings, m)
				}
			}
			mappings = filteredMappings

			// Filter file mappings to only those for filtered issues.
			filteredFileMappings := make([]model.IssueFileMapping, 0, len(fileMappings))
			for _, m := range fileMappings {
				if issueIDs[m.IssueID] {
					filteredFileMappings = append(filteredFileMappings, m)
				}
			}
			fileMappings = filteredFileMappings

			// Filter labels to only those referenced by remaining mappings.
			usedLabelIDs := make(map[int]bool)
			for _, m := range mappings {
				usedLabelIDs[m.LabelID] = true
			}
			filteredLabels := make([]*model.Label, 0, len(allLabels))
			for _, l := range allLabels {
				if usedLabelIDs[l.ID] {
					filteredLabels = append(filteredLabels, l)
				}
			}
			allLabels = filteredLabels
		}

		// Build export data.
		data := model.ExportData{
			Version:            1,
			ExportedAt:         time.Now().UTC().Format(time.RFC3339),
			Issues:             issues,
			Comments:           comments,
			Relations:          relations,
			Labels:             allLabels,
			IssueLabelMappings: mappings,
			IssueFileMappings:  fileMappings,
		}

		// Ensure nil slices become empty arrays in JSON.
		if data.Issues == nil {
			data.Issues = []*model.Issue{}
		}
		if data.Comments == nil {
			data.Comments = []*model.Comment{}
		}
		if data.Relations == nil {
			data.Relations = []model.Relation{}
		}
		if data.Labels == nil {
			data.Labels = []*model.Label{}
		}
		if data.IssueLabelMappings == nil {
			data.IssueLabelMappings = []model.IssueLabelMapping{}
		}
		if data.IssueFileMappings == nil {
			data.IssueFileMappings = []model.IssueFileMapping{}
		}

		// Generate output based on format.
		var raw string
		switch format {
		case "json":
			raw, err = renderExportJSON(data)
		case "csv":
			raw, err = renderExportCSV(issues)
		case "markdown":
			raw, err = renderExportMarkdown(issues, comments)
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
			issue.Title,
			issue.Description,
			string(issue.Status),
			string(issue.Priority),
			string(issue.Kind),
			issue.Assignee,
			labelsStr,
			filesStr,
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
