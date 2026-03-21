package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/watch"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// voteResultData is the JSON wire format for vote result output.
type voteResultData struct {
	ID               string        `json:"id"`
	Status           string        `json:"status"`
	FinalOutcome     string        `json:"final_outcome"`
	EscalationReason *string       `json:"escalation_reason"`
	WeightedScore    *float64      `json:"weighted_score"`
	Threshold        float64       `json:"threshold"`
	VotesCast        int           `json:"votes_cast"`
	VotesRequired    int           `json:"votes_required"`
	QuorumReached    bool          `json:"quorum_reached"`
	Votes            []*model.Vote `json:"votes"`
}

func (d voteResultData) MarshalJSON() ([]byte, error) {
	type Alias voteResultData
	a := Alias(d)
	if a.Votes == nil {
		a.Votes = []*model.Vote{}
	}
	return json.Marshal(a)
}

var voteResultCmd = &cobra.Command{
	Use:   "result <id>",
	Short: "Show consensus result for a proposal",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		watchMode, _ := cmd.Flags().GetBool("watch")
		if watchMode {
			interval, _ := cmd.Flags().GetDuration("interval")
			jsonMode, _ := cmd.Flags().GetBool("json")
			quietMode, _ := cmd.Flags().GetBool("quiet")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return watch.RunWatch(ctx, watch.Options{
				Interval:  interval,
				JSONMode:  jsonMode,
				QuietMode: quietMode,
				IsTTY:     term.IsTerminal(int(os.Stdout.Fd())),
				Stdout:    os.Stdout,
				Stderr:    os.Stderr,
			}, func(ctx context.Context, w *output.Writer) error {
				return runVoteResult(cmd, args, w)
			})
		}
		return runVoteResult(cmd, args, getWriter(cmd))
	},
}

// runVoteResult contains the query + render + output logic for vote result.
func runVoteResult(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	proposalID, err := model.ParseProposalID(args[0])
	if err != nil {
		return cmdErr(fmt.Errorf("invalid proposal ID: %w", err), output.ErrValidation)
	}

	proposal, err := db.GetProposal(conn, proposalID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return cmdErr(fmt.Errorf("proposal %s not found", args[0]), output.ErrNotFound)
		}
		return cmdErr(fmt.Errorf("fetching proposal: %w", err), output.ErrGeneral)
	}

	votes, err := db.GetProposalVotes(conn, proposalID)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching votes: %w", err), output.ErrGeneral)
	}

	votesCast := len(votes)
	quorumReached := votesCast >= proposal.RequiredVoters

	result := voteResultData{
		ID:               model.FormatProposalID(proposal.ID),
		Status:           string(proposal.Status),
		FinalOutcome:     proposal.FinalOutcome,
		EscalationReason: proposal.EscalationReason,
		WeightedScore:    proposal.WeightedScore,
		Threshold:        proposal.Threshold,
		VotesCast:        votesCast,
		VotesRequired:    proposal.RequiredVoters,
		QuorumReached:    quorumReached,
		Votes:            votes,
	}

	var message string
	if !w.JSONMode {
		message = render.RenderVoteResult(proposal, votes)
	}
	w.Success(result, message)

	return nil
}

func init() {
	voteCmd.AddCommand(voteResultCmd)
}
