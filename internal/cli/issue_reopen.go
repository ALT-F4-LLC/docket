package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var reopenCmd = &cobra.Command{
	Use:   "reopen [id]",
	Short: "Reopen a closed issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		res, err := verb.IssueReopen(cmd.Context(), verbEnv(cmd), verb.IssueReopenInput{
			Issue: args[0], IfVersion: ifVersionFlagValue(cmd),
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		issue := res.Issue
		if !res.Changed {
			if w.JSONMode {
				w.Success(withIssueVersion(issue), "")
			} else {
				w.Info("Issue %s is not closed", model.FormatID(issue.ID))
			}
			return nil
		}

		w.Success(withIssueVersion(issue), fmt.Sprintf("Reopened %s: %s", model.FormatID(issue.ID), issue.Title))

		return nil
	},
}

func init() {
	addIfVersionFlag(reopenCmd)
	issueCmd.AddCommand(reopenCmd)
}
