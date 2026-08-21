package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

var commentListCmd = &cobra.Command{
	Use:   "list [id]",
	Short: "List comments on an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runIssueCommentList)
	},
}

func runIssueCommentList(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := issueArg(args[0])
	if err != nil {
		return err
	}

	// Verify the issue exists.
	if _, err := getIssueOrErr(conn, id, fmt.Sprintf("issue %s", args[0])); err != nil {
		return err
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
