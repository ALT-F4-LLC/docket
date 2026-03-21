package cli

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

type voteListResult struct {
	Proposals []*model.Proposal `json:"proposals"`
	Total     int               `json:"total"`
}

var voteListCmd = &cobra.Command{
	Use:     "list",
	Short:   "List proposals",
	Aliases: []string{"ls"},
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		status, _ := cmd.Flags().GetString("status")
		criticality, _ := cmd.Flags().GetString("criticality")
		domainTag, _ := cmd.Flags().GetString("domain-tag")
		all, _ := cmd.Flags().GetBool("all")
		limit, _ := cmd.Flags().GetInt("limit")

		// Validate filter enum values.
		if status != "" {
			if err := model.ValidateProposalStatus(model.ProposalStatus(status)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
		}
		if criticality != "" {
			if err := model.ValidateCriticality(model.Criticality(criticality)); err != nil {
				return cmdErr(err, output.ErrValidation)
			}
		}

		// Default behavior: show only open proposals unless --all or explicit --status.
		if !all && status == "" {
			status = string(model.ProposalStatusOpen)
		}
		// If --all is set with no explicit status, clear the status filter.
		if all && !cmd.Flags().Changed("status") {
			status = ""
		}

		proposals, total, err := db.ListProposals(conn, status, criticality, domainTag, limit)
		if err != nil {
			return cmdErr(fmt.Errorf("listing proposals: %w", err), output.ErrGeneral)
		}

		if proposals == nil {
			proposals = []*model.Proposal{}
		}

		result := voteListResult{Proposals: proposals, Total: total}

		jsonMode, _ := cmd.Flags().GetBool("json")
		var message string
		if !jsonMode {
			// Fetch vote counts per proposal for human-readable output.
			voteCounts, err := getVoteCounts(conn, proposals)
			if err != nil {
				return cmdErr(fmt.Errorf("fetching vote counts: %w", err), output.ErrGeneral)
			}
			rows := make([]render.ProposalRow, 0, len(proposals))
			for _, p := range proposals {
				rows = append(rows, render.ProposalRow{
					Proposal: p,
					VoteCast: voteCounts[p.ID],
				})
			}
			message = render.RenderProposalTable(rows)
		}
		w.Success(result, message)

		return nil
	},
}

// getVoteCounts returns a map of proposal ID to votes cast count.
func getVoteCounts(conn *sql.DB, proposals []*model.Proposal) (map[int]int, error) {
	counts := make(map[int]int, len(proposals))
	for _, p := range proposals {
		votes, err := db.GetProposalVotes(conn, p.ID)
		if err != nil {
			return nil, fmt.Errorf("fetching votes for %s: %w", model.FormatProposalID(p.ID), err)
		}
		counts[p.ID] = len(votes)
	}
	return counts, nil
}

func init() {
	voteListCmd.Flags().StringP("status", "s", "", "Filter by status: open|approved|rejected|committed")
	voteListCmd.Flags().StringP("criticality", "c", "", "Filter by criticality: low|medium|high|critical")
	voteListCmd.Flags().StringP("domain-tag", "d", "", "Filter by domain tag")
	voteListCmd.Flags().Bool("all", false, "Include resolved proposals (default: open only)")
	voteListCmd.Flags().Int("limit", 50, "Maximum number of results")
	voteCmd.AddCommand(voteListCmd)
}
