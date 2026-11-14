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

var commentCmd = &cobra.Command{
	Use:   "comment",
	Short: "Manage comments",
}

var commentAddCmd = &cobra.Command{
	Use:         "add [id]",
	Short:       "Add a comment to an issue",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		env := verbEnv(cmd)
		target, err := verb.IssueCommentTarget(cmd.Context(), env, verb.IssueCommentTargetInput{Issue: args[0]})
		recordProjectRegistration(cmd, target.Registered)
		if err != nil {
			return verbErr(err)
		}

		jsonMode, _ := jsonModeOf(cmd)
		body, _ := cmd.Flags().GetString("message")

		// Resolve message body: flag > stdin pipe > editor.
		if !cmd.Flags().Changed("message") {
			// Check if stdin is a pipe.
			stat, err := os.Stdin.Stat()
			if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
				const maxStdinSize = 1 << 20 // 1 MiB
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

		body, err = commentBodyOrEditor(body, cmd.Flags().Changed("message"), jsonMode)
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

		res, err := verb.IssueCommentAdd(cmd.Context(), env, verb.IssueCommentAddInput{
			Issue:          args[0],
			Body:           body,
			IdempotencyKey: idempotencyKey,
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		w.Success(res.Comment, fmt.Sprintf("Comment added to %s: %s", model.FormatID(res.Issue.ID), res.Issue.Title))

		return nil
	},
}

func init() {
	commentAddCmd.Flags().StringP("message", "m", "", "Comment body")
	addIdempotencyKeyFlag(commentAddCmd)
	commentCmd.AddCommand(commentAddCmd)
	issueCmd.AddCommand(commentCmd)
}
