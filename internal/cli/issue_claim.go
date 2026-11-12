package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// claimResponse is the issue-level analog of the step claim response
// `{ step, token, lease_expires_ms, context }`.
//
// The subject key is `issue` rather than `step` — a recorded nominal
// deviation. `context` is absent: it is defined over step, artifact, and pin
// concepts that do not exist until steps land, and emitting a degenerate shape
// now would freeze something that must then change.
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

// claimResponseV2 adds the CAS version and the attempt count: a claim is a
// mutation and advances the version.
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
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		owner, _ := cmd.Flags().GetString("owner")
		class, _ := cmd.Flags().GetString("class")
		ttl, err := ttlFlag(cmd)
		if err != nil {
			return err
		}

		res, err := verb.IssueClaim(cmd.Context(), verbEnv(cmd), verb.IssueClaimInput{
			Issue: args[0], Owner: owner, TTL: ttl, Class: class,
		})
		recordProjectRegistration(cmd, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		id, token, lease := res.Issue, res.Token, res.Lease
		resp := claimResponse{
			Issue:          model.FormatID(id),
			Token:          token,
			LeaseExpiresMS: lease.ExpiresMS,
			version:        res.Version,
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

// ttlFlag returns the explicit --ttl, or nil when it was not given so the
// verb takes the configured TTL for --class.
func ttlFlag(cmd *cobra.Command) (*time.Duration, error) {
	if !cmd.Flags().Changed("ttl") {
		return nil, nil
	}
	ttl, err := cmd.Flags().GetDuration("ttl")
	if err != nil {
		return nil, cmdErr(err, output.ErrValidation)
	}
	return &ttl, nil
}

// verbEnv builds the Env an internal/verb call runs in: this invocation's
// store, author, working directory and project identity. A command wired
// without the root hook has no config and runs in the default project.
func verbEnv(cmd *cobra.Command) verb.Env {
	cwd, _ := os.Getwd()
	env := verb.Env{
		Store:  store.StoreOn(getDB(cmd)),
		Author: config.DefaultAuthor(),
		Cwd:    cwd,
	}
	if cfg := getCfg(cmd); cfg != nil {
		env.Project = verb.ProjectInput{Identity: cfg.Identity, Anchored: cfg.Anchored}
	}
	return env
}

// recordProjectRegistration records the project-registered event for a
// project an issue lease verb registered, as the root hook does for the
// projects it registers. A failed write is reported and does not fail the
// command: the project exists either way.
func recordProjectRegistration(cmd *cobra.Command, reg *engine.ProjectRegistration) {
	if reg == nil {
		return
	}
	if err := engine.RecordProjectRegisteredEvent(getDB(cmd), *reg, time.Now().UnixMilli()); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"docket: registered project %q but could not record the event: %v\n",
			reg.Name, err)
	}
}

func init() {
	claimCmd.Flags().String("owner", "", "Identity of the lease holder (required)")
	claimCmd.Flags().Duration("ttl", 0, "Lease duration; defaults to the configured TTL for --class")
	claimCmd.Flags().String("class", "", "Executor class whose configured TTL applies (opaque string)")
	issueCmd.AddCommand(claimCmd)
}
