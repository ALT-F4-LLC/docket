package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

var docCommentListCmd = &cobra.Command{
	Use:   "list [id]",
	Short: "List comments on a document",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runDocCommentList)
	},
}

func runDocCommentList(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := model.ParseDocID(args[0])
	if err != nil {
		return cmdErr(fmt.Errorf("invalid doc ID: %w", err), output.ErrValidation)
	}

	comments, err := db.ListDocComments(conn, id)
	if err != nil {
		if e := notFound(err, fmt.Sprintf("doc %s", args[0])); e != nil {
			return e
		}
		return cmdErr(fmt.Errorf("fetching comments: %w", err), output.ErrGeneral)
	}

	if w.JSONMode {
		w.Success(comments, "")
		return nil
	}

	if len(comments) == 0 {
		msg := render.EmptyState(
			fmt.Sprintf("No comments on %s", model.FormatDocID(id)),
			fmt.Sprintf("Add one with: docket doc comment add %s -m \"...\"", model.FormatDocID(id)),
			w.QuietMode,
		)
		w.Success(nil, msg)
		return nil
	}

	w.Success(comments, render.RenderDocCommentList(comments))
	return nil
}

func init() {
	docCommentCmd.AddCommand(docCommentListCmd)
}
