package cli

import (
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

type voteListResult struct {
	Proposals []*model.Proposal `json:"proposals"`
	Total     int               `json:"total"`
	// limit is the effective --limit, retained unexported for v2 truncation.
	limit int
}

// voteListResult implements output.Collection for the v2 envelope. Total is a
// pre-LIMIT count, so truncation is directly computable.
func (r voteListResult) CollectionItems() any { return r.Proposals }
func (r voteListResult) CollectionTotal() int { return r.Total }
func (r voteListResult) CollectionTruncated() bool {
	return output.IsTruncated(r.limit, r.Total, len(r.Proposals))
}

var voteListCmd = &cobra.Command{
	Use:         "list",
	Short:       "List proposals",
	Aliases:     []string{"ls"},
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runVoteList)
	},
}

// runVoteList contains the query + render + output logic for vote list.
func runVoteList(cmd *cobra.Command, args []string, w *output.Writer) error {
	in := verb.VoteListInput{VoteCounts: !w.JSONMode}
	in.Status, _ = cmd.Flags().GetString("status")
	in.Criticality, _ = cmd.Flags().GetString("criticality")
	in.DomainTag, _ = cmd.Flags().GetString("domain-tag")
	in.All, _ = cmd.Flags().GetBool("all")
	in.Limit, _ = cmd.Flags().GetInt("limit")

	if err := validateLimit(cmd, in.Limit); err != nil {
		return err
	}

	res, err := verb.VoteList(cmd.Context(), verbEnv(cmd), in)
	if err != nil {
		return verbErr(err)
	}

	result := voteListResult{Proposals: res.Proposals, Total: res.Total, limit: in.Limit}

	var message string
	if !w.JSONMode {
		rows := make([]render.ProposalRow, 0, len(res.Proposals))
		for _, p := range res.Proposals {
			rows = append(rows, render.ProposalRow{
				Proposal: p,
				VoteCast: res.VoteCounts[p.ID],
			})
		}
		message = render.RenderProposalTable(rows)
	}
	w.Success(result, message)

	return nil
}

func init() {
	voteListCmd.Flags().StringP("status", "s", "", "Filter by status: open|approved|rejected|committed")
	voteListCmd.Flags().StringP("criticality", "c", "", "Filter by criticality: low|medium|high|critical")
	voteListCmd.Flags().StringP("domain-tag", "d", "", "Filter by domain tag")
	voteListCmd.Flags().Bool("all", false, "Include resolved proposals (default: open only)")
	voteListCmd.Flags().Int("limit", 50, "Maximum number of results")
	voteCmd.AddCommand(voteListCmd)
}
