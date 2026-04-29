package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/ALT-F4-LLC/docket/internal/app"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/watch"
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
		watchMode, _ := cmd.Flags().GetBool("watch")
		if watchMode {
			interval, _ := cmd.Flags().GetDuration("interval")
			jsonMode, _ := cmd.Flags().GetBool("json")
			quietMode, _ := cmd.Flags().GetBool("quiet")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return watch.RunWatch(ctx, watch.Options{
				Interval:  interval,
				JSONMode:  jsonMode,
				QuietMode: quietMode,
				IsTTY:     term.IsTerminal(int(os.Stdout.Fd())),
				Stdout:    os.Stdout,
				Stderr:    os.Stderr,
			}, func(ctx context.Context, w *output.Writer) error {
				return runBoard(cmd, args, w)
			})
		}
		return runBoard(cmd, args, getWriter(cmd))
	},
}

func runBoard(cmd *cobra.Command, args []string, w *output.Writer) error {
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

	data, err := app.LoadBoard(conn, app.BoardParams{
		Labels:     labels,
		Priorities: priorities,
		Assignee:   assignee,
		Expand:     expand,
	})
	if err != nil {
		return cmdErr(err, output.ErrGeneral)
	}

	if w.JSONMode {
		// Group issues by status for structured output.
		groups := make(map[model.Status][]*model.Issue)
		for _, issue := range data.Issues {
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

	boardOpts := render.BoardOptions{
		Expand:   expand,
		Progress: data.Progress,
	}
	message := render.RenderBoard(data.Issues, boardOpts)
	w.Success(nil, message)

	return nil
}

func init() {
	boardCmd.Flags().StringSliceP("label", "l", nil, "Filter by label (repeatable)")
	boardCmd.Flags().StringSliceP("priority", "p", nil, "Filter by priority (repeatable)")
	boardCmd.Flags().StringP("assignee", "a", "", "Filter by assignee")
	boardCmd.Flags().Bool("expand", false, "Show sub-issues individually instead of rolling up")
	rootCmd.AddCommand(boardCmd)
}
