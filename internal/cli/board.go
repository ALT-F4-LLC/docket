package cli

import (
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// boardColumn represents a single status column in the board JSON output.
//
// Issues is typed `any` for the same reason listResult's is: summary rows
// (issueRowsPayload) by default, the full issue shape (issueListPayload) under
// `--with-body` (DKT-1053; see issue_row.go).
type boardColumn struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
	Issues any    `json:"issues"`
}

// boardResult is the JSON output structure for the board command.
type boardResult struct {
	Columns []boardColumn `json:"columns"`
}

var boardCmd = &cobra.Command{
	Use:         "board",
	Short:       "Show Kanban board",
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runBoard)
	},
}

func runBoard(cmd *cobra.Command, args []string, w *output.Writer) error {
	labels, _ := cmd.Flags().GetStringSlice("label")
	priorities, _ := cmd.Flags().GetStringSlice("priority")
	sizes, _ := cmd.Flags().GetStringSlice("size")
	assignee, _ := cmd.Flags().GetString("assignee")
	expand, _ := cmd.Flags().GetBool("expand")
	withBody, _ := cmd.Flags().GetBool("with-body")

	board, err := verb.Board(cmd.Context(), verbEnv(cmd), verb.BoardInput{
		Labels:           labels,
		Priorities:       priorities,
		Sizes:            sizes,
		Assignee:         assignee,
		Expand:           expand,
		SubIssueProgress: !w.JSONMode,
	})
	if err != nil {
		return verbErr(err)
	}
	issues := board.Issues

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
				Issues: issuesPayload(col, withBody),
			})
		}

		w.Success(boardResult{Columns: columns}, "")
		return nil
	}

	progress := make(map[int]render.SubIssueProgress, len(board.Progress))
	for id, counts := range board.Progress {
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
}

func init() {
	boardCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (repeatable)")
	boardCmd.Flags().StringSliceP("priority", "p", nil, "Filter by priority (repeatable)")
	boardCmd.Flags().StringSlice("size", nil, "Filter by size (repeatable)")
	boardCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	boardCmd.Flags().Bool("expand", false, "Show sub-issues individually instead of rolling up")
	boardCmd.Flags().Bool("with-body", false, withBodyHelp)
	rootCmd.AddCommand(boardCmd)
}
