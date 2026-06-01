package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/watch"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type docListResult struct {
	Docs  []*model.Doc `json:"docs"`
	Total int          `json:"total"`
}

var docListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List documents",
	Aliases: []string{"ls"},
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
				return runDocList(cmd, args, w)
			})
		}
		return runDocList(cmd, args, getWriter(cmd))
	},
}

func runDocList(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	types, _ := cmd.Flags().GetStringSlice("type")
	statuses, _ := cmd.Flags().GetStringSlice("status")
	author, _ := cmd.Flags().GetString("author")
	sortFlag, _ := cmd.Flags().GetString("sort")
	limit, _ := cmd.Flags().GetInt("limit")

	opts := db.DocListOptions{
		Types:    types,
		Statuses: statuses,
		Author:   author,
		Limit:    limit,
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

	result := docListResult{Docs: docs, Total: total}

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
