package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

// voteCommitResult is the JSON wire format for the vote commit response.
type voteCommitResult struct {
	ID               string  `json:"id"`
	Status           string  `json:"status"`
	FinalOutcome     string  `json:"final_outcome"`
	EscalationReason *string `json:"escalation_reason"`
	UpdatedAt        string  `json:"updated_at"`
}

var voteCommitCmd = &cobra.Command{
	Use:   "commit <id>",
	Short: "Commit an approved proposal",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		outcome, _ := cmd.Flags().GetString("outcome")
		escalationReason, _ := cmd.Flags().GetString("escalation-reason")

		env := verbEnv(cmd)
		res, err := verb.VoteCommit(cmd.Context(), env, verb.VoteCommitInput{
			Proposal:         args[0],
			Outcome:          outcome,
			EscalationReason: escalationReason,
			ConductorToken:   func() string { return optionalToken(os.Stdin) },
		})
		recordRegisteredProject(cmd, env, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		fmtID := model.FormatProposalID(res.ProposalID)
		msg := fmt.Sprintf("%s committed: %s", fmtID, outcome)

		var escalationReasonPtr *string
		if escalationReason != "" {
			escalationReasonPtr = &escalationReason
		}

		data := voteCommitResult{
			ID:               fmtID,
			Status:           string(model.ProposalStatusCommitted),
			FinalOutcome:     outcome,
			EscalationReason: escalationReasonPtr,
			UpdatedAt:        time.Now().UTC().Format(time.RFC3339),
		}

		w.Success(data, msg)

		return nil
	},
}

func init() {
	voteCommitCmd.Flags().String("outcome", "Committed", "Final outcome description")
	voteCommitCmd.Flags().String("escalation-reason", "", "Reason for escalation (if applicable)")
	voteCmd.AddCommand(voteCommitCmd)
}
