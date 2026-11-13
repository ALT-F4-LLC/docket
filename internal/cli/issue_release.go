package cli

import (
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// releaseResponse reports the ended lease. Attempt survives the release: it
// counts claims for all time, so letting go does not erase the trail of what
// has already been tried.
type releaseResponse struct {
	Issue   string `json:"issue"`
	Attempt int    `json:"attempt"`
}

var releaseCmd = &cobra.Command{
	Use:   "release <id>",
	Short: "Release the lease on a claimed issue",
	Long: `Release the lease you hold on an issue, returning it to the unclaimed pool.

Requires the capability token minted by 'docket issue claim', supplied via the
DOCKET_TOKEN environment variable or on stdin. Tokens are never accepted in
argv.

Releasing retires the token: the issue becomes immediately claimable, and the
released token never works again. The attempt counter is preserved, so the
record of how many times the issue has been claimed survives.

Releasing is the orderly counterpart to letting a lease expire. Both return the
issue to the pool; releasing does it now and without waiting out the TTL.`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		// A malformed ref is reported before the token is read, so it is
		// not masked by a missing token and leaves stdin undrained.
		if _, err := verb.ParseIssueRef(args[0]); err != nil {
			return verbErr(err)
		}

		token, err := readToken(os.Stdin)
		if err != nil {
			return err
		}

		res, err := verb.IssueRelease(cmd.Context(), verbEnv(cmd), verb.IssueReleaseInput{
			Issue: args[0], Token: token,
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		w.Success(
			releaseResponse{Issue: model.FormatID(res.Issue), Attempt: res.Lease.Attempt},
			fmt.Sprintf("Released %s", model.FormatID(res.Issue)),
		)

		return nil
	},
}

func init() {
	issueCmd.AddCommand(releaseCmd)
}
