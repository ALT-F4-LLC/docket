package cli

import (
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

type docListResult struct {
	Docs  []*model.Doc `json:"docs"`
	Total int          `json:"total"`
	// limit is the effective --limit, retained unexported for v2 truncation.
	limit int
}

// docListResult implements output.Collection for the v2 envelope. Total is a
// pre-LIMIT count, so truncation is directly computable.
func (r docListResult) CollectionItems() any { return r.Docs }
func (r docListResult) CollectionTotal() int { return r.Total }
func (r docListResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.Total, len(r.Docs))
}

var docListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List documents",
	Aliases: []string{"ls"},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runDocList)
	},
}

func runDocList(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	types, _ := cmd.Flags().GetStringSlice("type")
	statuses, _ := cmd.Flags().GetStringSlice("status")
	author, _ := cmd.Flags().GetString("author")
	sortFlag, _ := cmd.Flags().GetString("sort")
	limit, _ := cmd.Flags().GetInt("limit")

	if err := validateLimit(cmd, limit); err != nil {
		return err
	}

	opts := db.DocListOptions{
		ProjectID: getProjectID(cmd),
		Types:     types,
		Statuses:  statuses,
		Author:    author,
		Limit:     limit,
	}

	if sortFlag != "" {
		parts := strings.SplitN(sortFlag, ":", 2)
		opts.Sort = parts[0]
		if len(parts) > 1 {
			opts.SortDir = parts[1]
		}
	}

	summaries, total, err := db.ListDocsWithCounts(conn, opts)
	if err != nil {
		return cmdErr(fmt.Errorf("listing docs: %w", err), output.ErrGeneral)
	}

	docs := make([]*model.Doc, 0, len(summaries))
	for _, s := range summaries {
		docs = append(docs, s.Doc)
	}

	result := docListResult{Docs: docs, Total: total, limit: limit}

	var message string
	if !w.JSONMode {
		rows := make([]render.DocRow, 0, len(summaries))
		for _, s := range summaries {
			rows = append(rows, render.DocRow{
				Doc:             s.Doc,
				CurrentRevision: s.CurrentRevision,
				RevisionsCount:  s.RevisionsCount,
			})
		}
		message = render.RenderDocList(rows)
	}
	w.Success(result, message)

	return nil
}

func init() {
	docListCmd.Flags().StringSliceP("type", "T", nil, "Filter by type (repeatable)")
	docListCmd.Flags().StringSliceP("status", "s", nil, "Filter by status (repeatable)")
	docListCmd.Flags().StringP("author", "a", "", "Filter by author")
	docListCmd.Flags().String("sort", "", "Sort by field:direction (e.g. updated_at:desc)")
	docListCmd.Flags().Int("limit", 50, "Maximum number of results")
	docCmd.AddCommand(docListCmd)
}
