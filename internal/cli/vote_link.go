package cli

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/spf13/cobra"
)

// voteLinkResult is the JSON-friendly structure returned by vote link/unlink.
type voteLinkResult struct {
	ProposalID string `json:"proposal_id"`
	IssueID    string `json:"issue_id"`
}

var voteLinkCmd = &cobra.Command{
	Use:   "link <proposal-id>",
	Short: "Link a proposal to an issue",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		proposalID, err := model.ParseProposalID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid proposal ID: %w", err), output.ErrValidation)
		}

		issueFlag, _ := cmd.Flags().GetString("issue")
		issueID, err := model.ParseID(issueFlag)
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		if err := db.LinkProposalIssue(conn, proposalID, issueID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("proposal or issue not found"), output.ErrNotFound)
			}
			if errors.Is(err, db.ErrConflict) {
				return cmdErr(fmt.Errorf("link already exists"), output.ErrConflict)
			}
			return cmdErr(fmt.Errorf("linking proposal to issue: %w", err), output.ErrGeneral)
		}

		result := voteLinkResult{
			ProposalID: model.FormatProposalID(proposalID),
			IssueID:    model.FormatID(issueID),
		}

		w.Success(result, fmt.Sprintf("Linked %s to proposal %s",
			model.FormatID(issueID), model.FormatProposalID(proposalID)))
		return nil
	},
}

var voteUnlinkCmd = &cobra.Command{
	Use:   "unlink <proposal-id>",
	Short: "Remove a proposal-issue link",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		proposalID, err := model.ParseProposalID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid proposal ID: %w", err), output.ErrValidation)
		}

		issueFlag, _ := cmd.Flags().GetString("issue")
		issueID, err := model.ParseID(issueFlag)
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		if err := db.UnlinkProposalIssue(conn, proposalID, issueID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("link not found"), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("unlinking proposal from issue: %w", err), output.ErrGeneral)
		}

		result := voteLinkResult{
			ProposalID: model.FormatProposalID(proposalID),
			IssueID:    model.FormatID(issueID),
		}

		w.Success(result, fmt.Sprintf("Unlinked %s from proposal %s",
			model.FormatID(issueID), model.FormatProposalID(proposalID)))
		return nil
	},
}

func init() {
	voteLinkCmd.Flags().String("issue", "", "Issue ID to link (required)")
	voteLinkCmd.MarkFlagRequired("issue")

	voteUnlinkCmd.Flags().String("issue", "", "Issue ID to unlink (required)")
	voteUnlinkCmd.MarkFlagRequired("issue")

	voteCmd.AddCommand(voteLinkCmd)
	voteCmd.AddCommand(voteUnlinkCmd)
}
