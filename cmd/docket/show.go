package main

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

// showResult composes the issue fields with additional detail fields
// (sub-issues, relations, comments, activity) into a single flat JSON object.
type showResult struct {
	Issue     *model.Issue     `json:"-"`
	SubIssues []*model.Issue   `json:"sub_issues"`
	Relations []model.Relation `json:"relations"`
	Comments  []*model.Comment `json:"comments"`
	Activity  []model.Activity `json:"activity"`
}

// showResultJSON is the wire format that explicitly lists all fields,
// avoiding the fragile marshal-unmarshal-remarshal pattern.
type showResultJSON struct {
	ID          string           `json:"id"`
	ParentID    *string          `json:"parent_id,omitempty"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	Status      string           `json:"status"`
	Priority    string           `json:"priority"`
	Kind        string           `json:"kind"`
	Assignee    string           `json:"assignee"`
	Labels      []string         `json:"labels"`
	CreatedAt   string           `json:"created_at"`
	UpdatedAt   string           `json:"updated_at"`
	SubIssues   []*model.Issue   `json:"sub_issues"`
	Relations   []model.Relation `json:"relations"`
	Comments    []*model.Comment `json:"comments"`
	Activity    []model.Activity `json:"activity"`
}

func (s showResult) MarshalJSON() ([]byte, error) {
	i := s.Issue

	labels := i.Labels
	if labels == nil {
		labels = []string{}
	}
	subIssues := s.SubIssues
	if subIssues == nil {
		subIssues = []*model.Issue{}
	}
	relations := s.Relations
	if relations == nil {
		relations = []model.Relation{}
	}
	comments := s.Comments
	if comments == nil {
		comments = []*model.Comment{}
	}
	activity := s.Activity
	if activity == nil {
		activity = []model.Activity{}
	}

	j := showResultJSON{
		ID:          model.FormatID(i.ID),
		Title:       i.Title,
		Description: i.Description,
		Status:      string(i.Status),
		Priority:    string(i.Priority),
		Kind:        string(i.Kind),
		Assignee:    i.Assignee,
		Labels:      labels,
		CreatedAt:   i.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   i.UpdatedAt.UTC().Format(time.RFC3339),
		SubIssues:   subIssues,
		Relations:   relations,
		Comments:    comments,
		Activity:    activity,
	}

	if i.ParentID != nil {
		pid := model.FormatID(*i.ParentID)
		j.ParentID = &pid
	}

	return json.Marshal(j)
}

var showCmd = &cobra.Command{
	Use:   "show [id]",
	Short: "Show issue details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		w := getWriter(cmd)
		conn := getDB(cmd)

		id, err := model.ParseID(args[0])
		if err != nil {
			return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
		}

		issue, err := db.GetIssue(conn, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
			}
			return cmdErr(fmt.Errorf("fetching issue: %w", err), output.ErrGeneral)
		}

		// Hydrate labels.
		issue.Labels, err = db.GetIssueLabels(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching labels: %w", err), output.ErrGeneral)
		}

		subIssues, err := db.GetSubIssues(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching sub-issues: %w", err), output.ErrGeneral)
		}

		relations, err := db.GetIssueRelations(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
		}

		comments, err := db.ListComments(conn, id)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching comments: %w", err), output.ErrGeneral)
		}

		activity, err := db.GetActivity(conn, id, 10)
		if err != nil {
			return cmdErr(fmt.Errorf("fetching activity: %w", err), output.ErrGeneral)
		}

		result := showResult{
			Issue:     issue,
			SubIssues: subIssues,
			Relations: relations,
			Comments:  comments,
			Activity:  activity,
		}

		jsonMode, _ := cmd.Flags().GetBool("json")
		var message string
		if !jsonMode {
			message = render.RenderDetail(issue, subIssues, relations, comments, activity)
		}
		w.Success(result, message)

		return nil
	},
}

func init() {
	rootCmd.AddCommand(showCmd)
}
