package cli

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

var reopenCmd = &cobra.Command{
	Use:   "reopen [id]",
	Short: "Reopen a closed issue",
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

		if issue.Status != model.StatusDone {
			if w.JSONMode {
				w.Success(issue, "")
			} else {
				w.Info("Issue %s is not closed", model.FormatID(id))
			}
			return nil
		}

		if err := db.UpdateIssue(conn, id, map[string]interface{}{"status": "backlog"}, config.DefaultAuthor()); err != nil {
			return cmdErr(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
		}

		issue, err = db.GetIssue(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
		}

		w.Success(issue, fmt.Sprintf("Reopened %s: %s", model.FormatID(id), issue.Title))

		return nil
	},
}

func init() {
	issueCmd.AddCommand(reopenCmd)
}
