package cli

import (
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

		id, err := issueArg(args[0])
		if err != nil {
			return err
		}

		issue, err := getIssueOrErr(conn, id, fmt.Sprintf("issue %s", args[0]))
		if err != nil {
			return err
		}

		if issue.Status != model.StatusDone {
			if w.JSONMode {
				w.Success(withIssueVersion(issue), "")
			} else {
				w.Info("Issue %s is not closed", model.FormatID(id))
			}
			return nil
		}

		ifVersion, err := ifVersionOf(cmd)
		if err != nil {
			return err
		}

		// Reopening clears any resolution alongside the status: the issue is
		// back on the board, so "the machine abandoned this" is no longer the
		// current fact about it (DKT-245).
		fields := map[string]interface{}{"status": "backlog", "resolution": ""}
		if err := db.UpdateIssueCAS(conn, id, fields, config.DefaultAuthor(), ifVersion); err != nil {
			if e := casError(err, fmt.Sprintf("issue %s", model.FormatID(id))); e != nil {
				return e
			}
			return cmdErr(fmt.Errorf("updating issue: %w", err), output.ErrGeneral)
		}

		issue, err = db.GetIssue(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching updated issue: %w", err), output.ErrGeneral)
		}

		if err := hydrateIssueAssociations(conn, issue); err != nil {
			return err
		}

		w.Success(withIssueVersion(issue), fmt.Sprintf("Reopened %s: %s", model.FormatID(id), issue.Title))

		return nil
	},
}

func init() {
	addIfVersionFlag(reopenCmd)
	issueCmd.AddCommand(reopenCmd)
}
