package cli

import (
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

var closeCmd = &cobra.Command{
	Use:   "close [id]",
	Short: "Close an issue (shorthand for move <id> done)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		// Closing ends any live lease, so it is lease-mediated. The token is
		// OPTIONAL: the verb reads it only while a live lease is held, since
		// the stdin fallback drains its reader to EOF and an agent whose
		// inherited stdin pipe never closes would hang on an unclaimed issue,
		// which needs no token at all.
		res, err := verb.IssueClose(cmd.Context(), verbEnv(cmd), verb.IssueCloseInput{
			Issue:     args[0],
			IfVersion: ifVersionFlagValue(cmd),
			Token:     func() string { return optionalToken(os.Stdin) },
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
				w.Info("Issue %s is already closed", model.FormatID(issue.ID))
			}
			return nil
		}

		w.Success(withIssueVersion(issue), fmt.Sprintf("Closed %s: %s", model.FormatID(issue.ID), issue.Title))
		return nil
	},
}

// ifVersionFlagValue returns --if-version as given, or nil when it was not
// supplied. The verb validates it, after the checks that precede it, so a
// close or reopen that writes nothing does not refuse it.
func ifVersionFlagValue(cmd *cobra.Command) *int {
	if !cmd.Flags().Changed(ifVersionFlag) {
		return nil
	}
	v, _ := cmd.Flags().GetInt(ifVersionFlag)
	return &v
}

func init() {
	addIfVersionFlag(closeCmd)
	issueCmd.AddCommand(closeCmd)
}
