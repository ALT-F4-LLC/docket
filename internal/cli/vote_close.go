package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// voteCloseResult is the JSON wire format for the vote close response.
type voteCloseResult struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	FinalOutcome string `json:"final_outcome"`
	UpdatedAt    string `json:"updated_at"`
}

var voteCloseCmd = &cobra.Command{
	Use:   "close <id>",
	Short: "Close an open proposal whose decision was made another way",
	Long: `Close an OPEN proposal without a tally (DKT-114).

For a proposal whose underlying decision was made out-of-band — an operator
authorized the guarded action directly, or the question was superseded — and
which would otherwise sit open forever. Closed is terminal and is never a
verdict: no vote was counted, and the required --reason lands in the
proposal's final_outcome so the row says how the question ended.

Only an open proposal closes; a decided one (approved, rejected, committed)
is a record, and records do not move. A proposal opened by an engine vote
step is refused too: the run is moved past an uncast vote with
'docket step resolve', not by closing the step's own machinery underneath
it.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		proposalID, err := model.ParseProposalID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid proposal ID: %w", err), output.ErrValidation)
		}

		reason, _ := cmd.Flags().GetString("reason")
		if reason == "" {
			return cmdErr(
				fmt.Errorf("--reason is required: a closure without a why is the audit gap this verb exists to close"),
				output.ErrValidation)
		}

		stepOwned, err := engine.IsVoteStepProposal(conn, proposalID)
		if err != nil {
			return cmdErr(err, output.ErrGeneral)
		}
		if stepOwned {
			return cmdErr(fmt.Errorf(
				"proposal %s belongs to a vote step; move the run past it with `docket step resolve` — closing the proposal underneath the step would not route it",
				model.FormatProposalID(proposalID)), output.ErrConflict)
		}

		if err := db.CloseProposal(conn, proposalID, reason); err != nil {
			if e := notFound(err, fmt.Sprintf("proposal %s", model.FormatProposalID(proposalID))); e != nil {
				return e
			}
			if errors.Is(err, db.ErrConflict) {
				return cmdErr(err, output.ErrConflict)
			}
			return cmdErr(fmt.Errorf("closing proposal: %w", err), output.ErrGeneral)
		}

		fmtID := model.FormatProposalID(proposalID)
		data := voteCloseResult{
			ID:           fmtID,
			Status:       string(model.ProposalStatusClosed),
			FinalOutcome: reason,
			UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
		}
		w.Success(data, fmt.Sprintf("%s closed: %s", fmtID, reason))
		return nil
	},
}

func init() {
	voteCloseCmd.Flags().String("reason", "", "Why the proposal is being closed without a tally (required)")
	voteCmd.AddCommand(voteCloseCmd)
}
