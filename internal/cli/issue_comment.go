package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var commentCmd = &cobra.Command{
	Use:   "comment",
	Short: "Manage comments",
}

var commentAddCmd = &cobra.Command{
	Use:   "add [id]",
	Short: "Add a comment to an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := issueArg(args[0])
		if err != nil {
			return err
		}

		// Verify issue exists.
		issue, err := getIssueOrErr(conn, id, fmt.Sprintf("issue %s", args[0]))
		if err != nil {
			return err
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

		author := config.DefaultAuthor()

		comment := model.Comment{
			IssueID: id,
			Body:    body,
			Author:  author,
		}

		idempotencyKey, err := idempotencyKeyOf(cmd)
		if err != nil {
			return err
		}

		commentID, err := db.CreateCommentIdempotent(conn, &comment, idempotencyKey)
		if err != nil {
			if e := notFound(err, fmt.Sprintf("issue %s", args[0])); e != nil {
				return e
			}
			return cmdErr(fmt.Errorf("creating comment: %w", err), output.ErrGeneral)
		}

		created, err := db.GetComment(conn, commentID)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching created comment: %w", err), output.ErrGeneral)
		}

		w.Success(created, fmt.Sprintf("Comment added to %s: %s", model.FormatID(id), issue.Title))

		return nil
	},
}

func init() {
	commentAddCmd.Flags().StringP("message", "m", "", "Comment body")
	addIdempotencyKeyFlag(commentAddCmd)
	commentCmd.AddCommand(commentAddCmd)
	issueCmd.AddCommand(commentCmd)
}
