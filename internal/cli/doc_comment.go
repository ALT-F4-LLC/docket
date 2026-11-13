package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var docCommentCmd = &cobra.Command{
	Use:   "comment",
	Short: "Manage comments",
}

var docCommentAddCmd = &cobra.Command{
	Use:         "add [id]",
	Short:       "Add a comment to a document",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		jsonMode, _ := jsonModeOf(cmd)
		body, _ := cmd.Flags().GetString("message")

		if !cmd.Flags().Changed("message") {
			stat, err := os.Stdin.Stat()
			if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
				const maxStdinSize = 1 << 20
				lr := &io.LimitedReader{R: os.Stdin, N: maxStdinSize + 1}
				data, err := io.ReadAll(lr)
				if err != nil {
					return cmdErr(fmt.Errorf("reading comment from stdin: %w", err), output.ErrGeneral)
				}
				if int64(len(data)) > maxStdinSize {
					return cmdErr(fmt.Errorf("comment body exceeds %d bytes", maxStdinSize), output.ErrValidation)
				}
				body = strings.TrimSpace(string(data))
			}
		}

		body, err := commentBodyOrEditor(body, cmd.Flags().Changed("message"), jsonMode)
		if err != nil {
			return err
		}

		if body == "" {
			w.Info("Cancelled.")
			return nil
		}

		idempotencyKey, err := idempotencyKeyOf(cmd)
		if err != nil {
			return err
		}

		env := verbEnv(cmd)
		res, err := verb.DocCommentAdd(cmd.Context(), env, verb.DocCommentAddInput{
			Doc:            args[0],
			Body:           body,
			IdempotencyKey: idempotencyKey,
		})
		recordRegisteredProject(cmd, env, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		w.Success(res.Comment, fmt.Sprintf("Comment added to %s: %s", model.FormatDocID(res.Doc.ID), res.Doc.Title))

		return nil
	},
}

func init() {
	docCommentAddCmd.Flags().StringP("message", "m", "", "Comment body")
	addIdempotencyKeyFlag(docCommentAddCmd)
	docCommentCmd.AddCommand(docCommentAddCmd)
	docCmd.AddCommand(docCommentCmd)
}
