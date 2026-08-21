package cli

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// claimResponse is the issue-level analog of engine-spec.md §11.4's claim
// response `{ step, token, lease_expires_ms, context }`.
//
// The subject key is `issue` rather than `step` — a nominal deviation recorded
// in docs/tdd/claims-leases.md §3.1. `context` is absent:
// §11.4 defines it over step, artifact, and pin concepts that do not exist
// until steps land, and emitting a degenerate shape now would freeze something
// that must then change.
//
// Token is the ONLY place the plaintext token ever appears. It is returned
// exactly once, here, and never read back from storage — only its hash is
// stored.
type claimResponse struct {
	Issue          string `json:"issue"`
	Token          string `json:"token"`
	LeaseExpiresMS int64  `json:"lease_expires_ms"`

	// version and attempt are surfaced under --json=v2 only, via
	// VersionedPayload. They are unexported so they cannot leak into the v1
	// shape by accident.
	version int
	attempt int
}

// claimResponseV2 adds the CAS version and the attempt count, per
// reliability-delta §6.3: a claim is a mutation and advances the version.
type claimResponseV2 struct {
	Issue          string `json:"issue"`
	Token          string `json:"token"`
	LeaseExpiresMS int64  `json:"lease_expires_ms"`
	Attempt        int    `json:"attempt"`
	Version        int    `json:"version"`
}

// VersionedPayload implements output.Versioned so --json=v2 carries the
// version; v1 never consults the interface, so its shape is unchanged.
func (c claimResponse) VersionedPayload() any {
	return claimResponseV2{
		Issue:          c.Issue,
		Token:          c.Token,
		LeaseExpiresMS: c.LeaseExpiresMS,
		Attempt:        c.attempt,
		Version:        c.version,
	}
}

var _ output.Versioned = claimResponse{}

var claimCmd = &cobra.Command{
	Use:   "claim <id>",
	Short: "Claim an issue, taking a lease and minting a capability token",
	Long: `Claim an issue, taking a lease on it and minting a capability token.

The claim is atomic: exactly one of any number of concurrent claimants wins,
and the losers are refused with CONFLICT (exit 4). The winner receives a
capability token that every subsequent lease-mediated verb requires.

The token is returned exactly once, here. Only its hash is stored, so it cannot
be recovered from the database — capture it from this response or claim again.
Pass it back via the DOCKET_TOKEN environment variable or on stdin; it is never
accepted in argv.

A lease that expires without release returns the issue to the unclaimed pool:
the next claim wins, and the attempt counter records that a claim was made. No
reaper runs and no read verb ever writes — expiry is resolved by the claim
itself.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := issueArg(args[0])
		if err != nil {
			return err
		}

		owner, err := cmd.Flags().GetString("owner")
		if err != nil || owner == "" {
			return cmdErr(
				fmt.Errorf("--owner is required: it identifies the lease holder"),
				output.ErrValidation,
			)
		}

		class, _ := cmd.Flags().GetString("class")

		ttl, err := resolveTTL(cmd, conn, class)
		if err != nil {
			return err
		}

		label := fmt.Sprintf("issue %s", model.FormatID(id))
		token, lease, err := db.ClaimIssue(conn, id, owner, ttl.Milliseconds(), model.NowMS())
		if err != nil {
			if e := leaseError(err, label); e != nil {
				return e
			}
			return cmdErr(fmt.Errorf("claiming issue: %w", err), output.ErrGeneral)
		}

		version, err := db.GetVersion(conn, "issues", id)
		if err != nil {
			return cmdErr(fmt.Errorf("reading issue version: %w", err), output.ErrGeneral)
		}

		resp := claimResponse{
			Issue:          model.FormatID(id),
			Token:          token,
			LeaseExpiresMS: lease.ExpiresMS,
			version:        version,
			attempt:        lease.Attempt,
		}

		// The human-mode message deliberately does NOT include the token: a
		// token echoed into a terminal transcript or CI log is a live
		// capability. Human mode prints it on its own line via Info so it is
		// still usable interactively, but never inside a message that gets
		// copied into an issue comment.
		if w.JSONMode {
			w.Success(resp, "")
			return nil
		}

		w.Success(nil, fmt.Sprintf(
			"Claimed %s for %s until %s (attempt %d)",
			model.FormatID(id), owner,
			time.UnixMilli(lease.ExpiresMS).UTC().Format(time.RFC3339),
			lease.Attempt,
		))
		fmt.Fprintln(w.Stdout, token)

		return nil
	},
}

// resolveTTL returns the lease TTL: an explicit --ttl wins, else the effective
// per-class configuration (which falls back to lease.ttl.default).
func resolveTTL(cmd *cobra.Command, conn *sql.DB, class string) (time.Duration, error) {
	if cmd.Flags().Changed("ttl") {
		ttl, err := cmd.Flags().GetDuration("ttl")
		if err != nil {
			return 0, cmdErr(err, output.ErrValidation)
		}
		if ttl <= 0 {
			return 0, cmdErr(
				fmt.Errorf("--ttl must be positive, got %s", ttl),
				output.ErrValidation,
			)
		}
		return ttl, nil
	}

	ttl, err := db.LeaseTTL(conn, getProjectID(cmd), class)
	if err != nil {
		return 0, cmdErr(fmt.Errorf("resolving lease TTL: %w", err), output.ErrGeneral)
	}
	return ttl, nil
}

func init() {
	claimCmd.Flags().String("owner", "", "Identity of the lease holder (required)")
	claimCmd.Flags().Duration("ttl", 0, "Lease duration; defaults to the configured TTL for --class")
	claimCmd.Flags().String("class", "", "Executor class whose configured TTL applies (opaque string)")
	issueCmd.AddCommand(claimCmd)
}
