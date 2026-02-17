package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		// Verify issue exists.
		issue, err := db.GetIssue(conn, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		jsonMode, _ := cmd.Flags().GetBool("json")
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

		// In JSON mode there is no interactive editor fallback, so a body
		// must have been provided via -m or stdin.
		if body == "" && jsonMode {
			return cmdErr(fmt.Errorf("message is required in JSON mode"), output.ErrValidation)
		}

		if body == "" {
			// Open editor for interactive input.
			editor := os.Getenv("EDITOR")
			if editor == "" {
				editor = "vi"
			}

			tmpFile, err := os.CreateTemp("", "docket-comment-*.md")
			if err != nil {
				return cmdErr(fmt.Errorf("creating temp file: %w", err), output.ErrGeneral)
			}
			tmpPath := tmpFile.Name()
			if err := tmpFile.Close(); err != nil {
				os.Remove(tmpPath)
				return cmdErr(fmt.Errorf("closing temp file: %w", err), output.ErrGeneral)
			}
			defer os.Remove(tmpPath)

			editorCmd := exec.Command(editor, tmpPath)
			editorCmd.Stdin = os.Stdin
			editorCmd.Stdout = os.Stdout
			editorCmd.Stderr = os.Stderr

			if err := editorCmd.Run(); err != nil {
				return cmdErr(fmt.Errorf("editor exited with error: %w", err), output.ErrGeneral)
			}

			content, err := os.ReadFile(tmpPath)
			if err != nil {
				return cmdErr(fmt.Errorf("reading temp file: %w", err), output.ErrGeneral)
			}

			body = strings.TrimSpace(string(content))
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

		commentID, err := db.CreateComment(conn, &comment)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
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
	commentCmd.AddCommand(commentAddCmd)
	issueCmd.AddCommand(commentCmd)
}
