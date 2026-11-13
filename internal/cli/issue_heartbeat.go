package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// heartbeatResponse reports the extended lease. It carries no token: the token
// is returned exactly once, at claim.
type heartbeatResponse struct {
	Issue          string `json:"issue"`
	LeaseExpiresMS int64  `json:"lease_expires_ms"`
	Attempt        int    `json:"attempt"`
}

var heartbeatCmd = &cobra.Command{
	Use:   "heartbeat <id>",
	Short: "Extend the lease on a claimed issue",
	Long: `Extend the lease on an issue you hold.

Requires the capability token minted by 'docket issue claim', supplied via the
DOCKET_TOKEN environment variable or on stdin. Tokens are never accepted in
argv, where 'ps' would expose them to every user on the host.

Renewal is how liveness is signalled: a holder that keeps working keeps its
lease, while one that wedges or dies stops heartbeating and lets the lease
lapse, at which point the issue is claimable again. The attempt counter is not
touched — a heartbeat is not a new claim.

Refusals: a token that does not hold this lease is AUTH_ERROR (exit 5); a
correct token on a lease that has already expired is STALE_LEASE (exit 6),
which tells the holder to claim again rather than that it was never trusted.`,
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

		class, _ := cmd.Flags().GetString("class")
		ttl, err := ttlFlag(cmd)
		if err != nil {
			return err
		}

		res, err := verb.IssueHeartbeat(cmd.Context(), verbEnv(cmd), verb.IssueHeartbeatInput{
			Issue: args[0], Token: token, TTL: ttl, Class: class,
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		id, lease := res.Issue, res.Lease
		w.Success(
			heartbeatResponse{
				Issue:          model.FormatID(id),
				LeaseExpiresMS: lease.ExpiresMS,
				Attempt:        lease.Attempt,
			},
			fmt.Sprintf("Extended lease on %s until %s",
				model.FormatID(id),
				time.UnixMilli(lease.ExpiresMS).UTC().Format(time.RFC3339)),
		)

		return nil
	},
}

func init() {
	heartbeatCmd.Flags().Duration("ttl", 0, "Lease extension; defaults to the configured TTL for --class")
	heartbeatCmd.Flags().String("class", "", "Executor class whose configured TTL applies (opaque string)")
	issueCmd.AddCommand(heartbeatCmd)
}
