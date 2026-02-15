package main

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// boardColumn represents a single status column in the board JSON output.
type boardColumn struct {
	Status string         `json:"status"`
	Count  int            `json:"count"`
	Issues []*model.Issue `json:"issues"`
}

// boardResult is the JSON output structure for the board command.
type boardResult struct {
	Columns []boardColumn `json:"columns"`
}

var boardCmd = &cobra.Command{
	Use:   "board",
	Short: "Show Kanban board",
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		labels, _ := cmd.Flags().GetStringSlice("label")
		priorities, _ := cmd.Flags().GetStringSlice("priority")
		assignee, _ := cmd.Flags().GetString("assignee")
		expand, _ := cmd.Flags().GetBool("expand")

		// Validate filter enum values.
		for _, p := range priorities {
			if err := model.ValidatePriority(model.Priority(p)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
		}

		opts := db.ListOptions{
			Priorities:  priorities,
			Labels:      labels,
			Assignee:    assignee,
			IncludeDone: true,
		}

		issues, _, err := db.ListIssues(conn, opts)
		if err != nil {
			return cmdErr(fmt.Errorf("listing issues: %w", err), output.ErrGeneral)
		}

		// By default, roll up sub-issues into their parent (exclude issues that
		// have a parent). When --expand is set, show all issues individually.
		if !expand {
			var roots []*model.Issue
			for _, issue := range issues {
				if issue.ParentID == nil {
					roots = append(roots, issue)
				}
			}
			issues = roots
		}

		if w.JSONMode {
			// Group issues by status for structured output.
			groups := make(map[model.Status][]*model.Issue)
			for _, issue := range issues {
				groups[issue.Status] = append(groups[issue.Status], issue)
			}

			var columns []boardColumn
			for _, status := range render.StatusOrder {
				col := groups[status]
				if col == nil {
					col = []*model.Issue{}
				}
				columns = append(columns, boardColumn{
					Status: string(status),
					Count:  len(col),
					Issues: col,
				})
			}

			w.Success(boardResult{Columns: columns}, "")
			return nil
		}

		// Build sub-issue progress map for parent issues in a single query.
		parentIDs := make([]int, len(issues))
		for i, issue := range issues {
			parentIDs[i] = issue.ID
		}
		batchProgress, err := db.GetBatchSubIssueProgress(conn, parentIDs)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching sub-issue progress: %w", err), output.ErrGeneral)
		}
		progress := make(map[int]render.SubIssueProgress, len(batchProgress))
		for id, counts := range batchProgress {
			if counts[1] > 0 {
				progress[id] = render.SubIssueProgress{Done: counts[0], Total: counts[1]}
			}
		}

		boardOpts := render.BoardOptions{
			Expand:   expand,
			Progress: progress,
		}
		message := render.RenderBoard(issues, boardOpts)
		w.Success(nil, message)

		return nil
	},
}

func init() {
	boardCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (repeatable)")
	boardCmd.Flags().StringSliceP("priority", "p", nil, "Filter by priority (repeatable)")
	boardCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	boardCmd.Flags().Bool("expand", false, "Show sub-issues individually instead of rolling up")
	rootCmd.AddCommand(boardCmd)
}
