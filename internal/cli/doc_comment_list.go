package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var docCommentListCmd = &cobra.Command{
	Use:         "list [id]",
	Short:       "List comments on a document",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runDocCommentList)
	},
}

func runDocCommentList(cmd *cobra.Command, args []string, w *output.Writer) error {
	listed, err := verb.DocCommentList(cmd.Context(), verbEnv(cmd), verb.DocCommentListInput{Doc: args[0]})
	if err != nil {
		return verbErr(err)
	}
	comments := listed.Comments

	if w.JSONMode {
		w.Success(comments, "")
		return nil
	}

	if len(comments) == 0 {
		docID := model.FormatDocID(listed.DocID)
		msg := render.EmptyState(
			fmt.Sprintf("No comments on %s", docID),
			fmt.Sprintf("Add one with: docket doc comment add %s -m \"...\"", docID),
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
