package main

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var closeCmd = &cobra.Command{
	Use:   "close [id]",
	Short: "Close an issue (shorthand for move <id> done)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		issue, err := db.GetIssue(conn, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		if issue.Status == model.StatusDone {
			if w.JSONMode {
				w.Success(issue, "")
			} else {
				w.Info("Issue %s is already closed", model.FormatID(id))
			}
			return nil
		}

		err = db.UpdateIssue(conn, id, map[string]interface{}{"status": "done"}, config.DefaultAuthor())
		if err != nil {
			return cmdErr(fmt.Errorf("closing issue: %w", err), output.ErrGeneral)
		}

		issue, err = db.GetIssue(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
		}

		w.Success(issue, fmt.Sprintf("Closed %s", model.FormatID(id)))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(closeCmd)
}
