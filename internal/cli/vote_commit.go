package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// voteCommitResult is the JSON wire format for the vote commit response.
type voteCommitResult struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	FinalOutcome string `json:"final_outcome"`
	UpdatedAt    string `json:"updated_at"`
}

var voteCommitCmd = &cobra.Command{
	Use:   "commit <id>",
	Short: "Commit an approved proposal",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		proposalID, err := model.ParseProposalID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid proposal ID: %w", err), output.ErrValidation)
		}

		outcome, _ := cmd.Flags().GetString("outcome")

		err = db.CommitProposal(conn, proposalID, outcome)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("proposal %s not found", model.FormatProposalID(proposalID)), output.ErrNotFound)
			}
			if errors.Is(err, db.ErrConflict) {
				return cmdErr(err, output.ErrConflict)
			}
			return cmdErr(fmt.Errorf("committing proposal: %w", err), output.ErrGeneral)
		}

		fmtID := model.FormatProposalID(proposalID)
		msg := fmt.Sprintf("%s committed: %s", fmtID, outcome)

		data := voteCommitResult{
			ID:           fmtID,
			Status:       string(model.ProposalStatusCommitted),
			FinalOutcome: outcome,
			UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
		}

		w.Success(data, msg)

		return nil
	},
}

func init() {
	voteCommitCmd.Flags().String("outcome", "Committed", "Final outcome description")
	voteCmd.AddCommand(voteCommitCmd)
}
