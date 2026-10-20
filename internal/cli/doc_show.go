package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/store"
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
	Use:   "show [id]",
	Short: "Show document details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return watchable(cmd, args, runDocShow)
	},
}

func runDocShow(cmd *cobra.Command, args []string, w *output.Writer) error {
	st := store.StoreOn(getDB(cmd))

	id, err := model.ParseDocID(args[0])
	if err != nil {
		return cmdErr(fmt.Errorf("invalid doc ID: %w", err), output.ErrValidation)
	}

	if cmd.Flags().Changed("rev") {
		rev, _ := cmd.Flags().GetInt("rev")
		return runDocShowRevision(st, w, args[0], id, rev)
	}

	doc, err := st.GetDoc(id)
	if err != nil {
		if e := notFound(err, fmt.Sprintf("doc %s", args[0])); e != nil {
			return e
		}
		return cmdErr(fmt.Errorf("fetching doc: %w", err), output.ErrGeneral)
	}

	revisions, err := st.ListDocRevisions(id)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching revisions: %w", err), output.ErrGeneral)
	}

	comments, err := st.ListDocComments(id)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching comments: %w", err), output.ErrGeneral)
	}

	linkedIssuesByDoc, err := st.HydrateLinkedIssues([]int{id})
	if err != nil {
		return cmdErr(fmt.Errorf("fetching linked issues: %w", err), output.ErrGeneral)
	}
	linkedIssues := linkedIssuesByDoc[id]

	linkedProposals, err := st.GetDocProposals(id)
	if err != nil {
		return cmdErr(fmt.Errorf("fetching linked proposals: %w", err), output.ErrGeneral)
	}

	result := docShowResult{
		Doc:             doc,
		Revisions:       revisions,
		Comments:        comments,
		LinkedIssues:    linkedIssues,
		LinkedProposals: linkedProposals,
	}

	var message string
	if !w.JSONMode {
		message = render.RenderDocDetail(doc, revisions, comments, linkedIssues, linkedProposals)
	}
	w.Success(result, message)

	return nil
}

func runDocShowRevision(st store.Store, w *output.Writer, idArg string, id, rev int) error {
	revision, err := st.GetDocRevision(id, rev)
	if err != nil {
		if errors.Is(err, store.ErrValidation) {
			return cmdErr(err, output.ErrValidation)
		}
		if e := notFound(err, fmt.Sprintf("doc %s revision %d", idArg, rev)); e != nil {
			return e
		}
		return cmdErr(fmt.Errorf("fetching revision: %w", err), output.ErrGeneral)
	}

	var message string
	if !w.JSONMode {
		message = render.RenderDocRevisionHistory([]*model.DocRevision{revision})
	}
	w.Success(revision, message)

	return nil
}

func init() {
	docShowCmd.Flags().Int("rev", 0, "Show a specific revision number")
	docCmd.AddCommand(docShowCmd)
}
