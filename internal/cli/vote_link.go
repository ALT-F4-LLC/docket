package cli

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/verb"
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

		issue, _ := cmd.Flags().GetString("issue")
		env := verbEnv(cmd)
		res, err := verb.VoteLink(cmd.Context(), env, verb.VoteLinkInput{Proposal: args[0], Issue: issue})
		recordRegisteredProject(cmd, env, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		result := voteLinkResult{
			ProposalID: model.FormatProposalID(res.ProposalID),
			IssueID:    model.FormatID(res.IssueID),
		}

		w.Success(result, fmt.Sprintf("Linked %s to proposal %s",
			result.IssueID, result.ProposalID))
		return nil
	},
}

var voteUnlinkCmd = &cobra.Command{
	Use:   "unlink <proposal-id>",
	Short: "Remove a proposal-issue link",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)

		issue, _ := cmd.Flags().GetString("issue")
		env := verbEnv(cmd)
		res, err := verb.VoteUnlink(cmd.Context(), env, verb.VoteUnlinkInput{Proposal: args[0], Issue: issue})
		recordRegisteredProject(cmd, env, res.Registered)
		if err != nil {
			return verbErr(err)
		}

		result := voteLinkResult{
			ProposalID: model.FormatProposalID(res.ProposalID),
			IssueID:    model.FormatID(res.IssueID),
		}

		w.Success(result, fmt.Sprintf("Unlinked %s from proposal %s",
			result.IssueID, result.ProposalID))
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
