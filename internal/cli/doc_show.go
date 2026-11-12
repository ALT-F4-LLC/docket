package cli

import (
	"encoding/json"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/spf13/cobra"
)

type docShowResult struct {
	Doc             *model.Doc
	Revisions       []*model.DocRevision
	Comments        []*model.DocComment
	LinkedIssues    []model.IssueRef
	LinkedProposals []int
}

type docShowResultJSON struct {
	ID              string               `json:"id"`
	Type            string               `json:"type"`
	Status          string               `json:"status"`
	Title           string               `json:"title"`
	Body            string               `json:"body"`
	Author          string               `json:"author"`
	CreatedAt       string               `json:"created_at"`
	UpdatedAt       string               `json:"updated_at"`
	Revisions       []*model.DocRevision `json:"revisions"`
	Comments        []*model.DocComment  `json:"comments"`
	LinkedIssues    []model.IssueRef     `json:"linked_issues"`
	LinkedProposals []string             `json:"linked_proposals"`
}

func (s docShowResult) MarshalJSON() ([]byte, error) {
	d := s.Doc

	revisions := s.Revisions
	if revisions == nil {
		revisions = []*model.DocRevision{}
	}
	comments := s.Comments
	if comments == nil {
		comments = []*model.DocComment{}
	}

	linkedIssues := s.LinkedIssues
	if linkedIssues == nil {
		linkedIssues = []model.IssueRef{}
	}
	linkedProposals := make([]string, 0, len(s.LinkedProposals))
	for _, id := range s.LinkedProposals {
		linkedProposals = append(linkedProposals, model.FormatProposalID(id))
	}

	return json.Marshal(docShowResultJSON{
		ID:              model.FormatDocID(d.ID),
		Type:            d.Type,
		Status:          d.Status,
		Title:           d.Title,
		Body:            d.Body,
		Author:          d.Author,
		CreatedAt:       d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:       d.UpdatedAt.UTC().Format(time.RFC3339),
		Revisions:       revisions,
		Comments:        comments,
		LinkedIssues:    linkedIssues,
		LinkedProposals: linkedProposals,
	})
}

var docShowCmd = &cobra.Command{
	Use:         "show [id]",
	Short:       "Show document details",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{"verbOwnsProject": ""},
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runDocShow)
	},
}

func runDocShow(cmd *cobra.Command, args []string, w *output.Writer) error {
	in := verb.DocShowInput{Doc: args[0]}
	if cmd.Flags().Changed("rev") {
		rev, _ := cmd.Flags().GetInt("rev")
		in.Rev = &rev
	}

	shown, err := verb.DocShow(cmd.Context(), verbEnv(cmd), in)
	if err != nil {
		return verbErr(err)
	}

	if shown.Revision != nil {
		var message string
		if !w.JSONMode {
			message = render.RenderDocRevisionHistory([]*model.DocRevision{shown.Revision})
		}
		w.Success(shown.Revision, message)
		return nil
	}

	result := docShowResult{
		Doc:             shown.Doc,
		Revisions:       shown.Revisions,
		Comments:        shown.Comments,
		LinkedIssues:    shown.LinkedIssues,
		LinkedProposals: shown.LinkedProposals,
	}

	var message string
	if !w.JSONMode {
		message = render.RenderDocDetail(shown.Doc, shown.Revisions, shown.Comments,
			shown.LinkedIssues, shown.LinkedProposals)
	}
	w.Success(result, message)

	return nil
}

func init() {
	docShowCmd.Flags().Int("rev", 0, "Show a specific revision number")
	docCmd.AddCommand(docShowCmd)
}
