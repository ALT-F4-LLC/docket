package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dustin/go-humanize"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var commentsCmd = &cobra.Command{
	Use:   "comments [id]",
	Short: "List comments on an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
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

		jsonMode, _ := cmd.Flags().GetBool("json")
		if jsonMode {
			w.Success(comments, "")
			return nil
		}

		if len(comments) == 0 {
			w.Success(nil, fmt.Sprintf("No comments on %s", model.FormatID(id)))
			return nil
		}

		var parts []string
		for _, c := range comments {
			parts = append(parts, fmt.Sprintf("%s  %s\n  %s", c.AuthorOrAnonymous(), humanize.Time(c.CreatedAt), c.Body))
		}
		message := strings.Join(parts, "\n\n")

		w.Success(comments, message)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(commentsCmd)
}
