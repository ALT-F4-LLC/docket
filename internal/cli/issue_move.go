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

var moveCmd = &cobra.Command{
	Use:   "move <id> <status>",
	Short: "Move an issue to a new status",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		newStatus := model.Status(args[1])
		if err := model.ValidateStatus(newStatus); err != nil {
			return cmdErr(err, output.ErrValidation)
		}

		issue, err := db.GetIssue(conn, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		if issue.Status == newStatus {
			if w.JSONMode {
				w.Success(issue, "")
			} else {
				w.Info("Issue %s is already %s", model.FormatID(id), newStatus)
			}
			return nil
		}

		if err := db.UpdateIssue(conn, id, map[string]interface{}{"status": string(newStatus)}, config.DefaultAuthor()); err != nil {
			return cmdErr(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
		}

		issue, err = db.GetIssue(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
		}

		w.Success(issue, fmt.Sprintf("Moved %s to %s", model.FormatID(id), newStatus))

		return nil
	},
}

func init() {
	issueCmd.AddCommand(moveCmd)
}
