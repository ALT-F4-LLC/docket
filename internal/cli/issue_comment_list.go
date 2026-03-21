package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/watch"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var commentListCmd = &cobra.Command{
	Use:   "list [id]",
	Short: "List comments on an issue",
	Args:  cobra.ExactArgs(1),
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
				return runIssueCommentList(cmd, args, w)
			})
		}
		return runIssueCommentList(cmd, args, getWriter(cmd))
	},
}

func runIssueCommentList(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := model.ParseID(args[0])
	if err != nil {
		return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
	}

	// Verify the issue exists.
	if _, err := db.GetIssue(conn, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
		}
		return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
	}

	comments, err := db.ListComments(conn, id)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching comments: %w", err), output.ErrGeneral)
	}

	if w.JSONMode {
		w.Success(comments, "")
		return nil
	}

	if len(comments) == 0 {
		msg := render.EmptyState(
			fmt.Sprintf("No comments on %s", model.FormatID(id)),
			fmt.Sprintf("Add one with: docket issue comment add %s -m \"...\"", model.FormatID(id)),
			w.QuietMode,
		)
		w.Success(nil, msg)
		return nil
	}

	w.Success(comments, render.RenderCommentList(comments))
	return nil
}

func init() {
	commentCmd.AddCommand(commentListCmd)
}
