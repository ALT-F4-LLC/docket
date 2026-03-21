package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/spf13/cobra"
)

// voteShowResult composes a proposal with its votes and linked issues
// into a single flat JSON object matching the TDD contract.
type voteShowResult struct {
	Proposal     *model.Proposal `json:"-"`
	Votes        []*model.Vote   `json:"-"`
	LinkedIssues []int           `json:"-"`
}

// voteShowResultJSON is the wire format for vote show.
type voteShowResultJSON struct {
	ID               string        `json:"id"`
	Description      string        `json:"description"`
	Rationale        string        `json:"rationale"`
	DomainTags       []string      `json:"domain_tags"`
	FilesChanged     []string      `json:"files_changed"`
	Criticality      string        `json:"criticality"`
	Status           string        `json:"status"`
	FinalOutcome     string        `json:"final_outcome"`
	EscalationReason *string       `json:"escalation_reason"`
	RequiredVoters   int           `json:"required_voters"`
	Threshold        float64       `json:"threshold"`
	WeightedScore    *float64      `json:"weighted_score"`
	CreatedBy        string        `json:"created_by"`
	CreatedAt        string        `json:"created_at"`
	UpdatedAt        string        `json:"updated_at"`
	Votes            []*model.Vote `json:"votes"`
	LinkedIssues     []string      `json:"linked_issues"`
}

func (r voteShowResult) MarshalJSON() ([]byte, error) {
	p := r.Proposal

	votes := r.Votes
	if votes == nil {
		votes = []*model.Vote{}
	}

	linkedIssues := make([]string, 0, len(r.LinkedIssues))
	for _, id := range r.LinkedIssues {
		linkedIssues = append(linkedIssues, model.FormatID(id))
	}

	domainTags := p.DomainTags
	if domainTags == nil {
		domainTags = []string{}
	}
	filesChanged := p.FilesChanged
	if filesChanged == nil {
		filesChanged = []string{}
	}

	j := voteShowResultJSON{
		ID:               model.FormatProposalID(p.ID),
		Description:      p.Description,
		Rationale:        p.Rationale,
		DomainTags:       domainTags,
		FilesChanged:     filesChanged,
		Criticality:      string(p.Criticality),
		Status:           string(p.Status),
		FinalOutcome:     p.FinalOutcome,
		EscalationReason: p.EscalationReason,
		RequiredVoters:   p.RequiredVoters,
		Threshold:        p.Threshold,
		WeightedScore:    p.WeightedScore,
		CreatedBy:        p.CreatedBy,
		CreatedAt:        p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:        p.UpdatedAt.UTC().Format(time.RFC3339),
		Votes:            votes,
		LinkedIssues:     linkedIssues,
	}

	return json.Marshal(j)
}

var voteShowCmd = &cobra.Command{
	Use:   "show [id]",
	Short: "Show proposal details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseProposalID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid proposal ID: %w", err), output.ErrValidation)
		}

		proposal, err := db.GetProposal(conn, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("proposal %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching proposal: %w", err), output.ErrGeneral)
		}

		votes, err := db.GetProposalVotes(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching votes: %w", err), output.ErrGeneral)
		}

		linkedIssues, err := db.GetProposalIssues(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching linked issues: %w", err), output.ErrGeneral)
		}

		result := voteShowResult{
			Proposal:     proposal,
			Votes:        votes,
			LinkedIssues: linkedIssues,
		}

		jsonMode, _ := cmd.Flags().GetBool("json")
		var message string
		if !jsonMode {
			message = render.RenderProposalDetail(proposal, votes, linkedIssues)
		}
		w.Success(result, message)

		return nil
	},
}

func init() {
	voteCmd.AddCommand(voteShowCmd)
}
