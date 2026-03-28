package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/app"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/render"
	"github.com/ALT-F4-LLC/docket/internal/watch"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type showResult struct {
	Issue     *model.Issue     `json:"-"`
	SubIssues []*model.Issue   `json:"sub_issues"`
	Relations []model.Relation `json:"relations"`
	Comments  []*model.Comment `json:"comments"`
	Activity  []model.Activity `json:"activity"`
}

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
	Files       []string         `json:"files"`
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
	files := i.Files
	if files == nil {
		files = []string{}
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
		Files:       files,
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
		watchMode, _ := cmd.Flags().GetBool("watch")
		if watchMode {
			interval, _ := cmd.Flags().GetDuration("interval")
			jsonMode, _ := cmd.Flags().GetBool("json")
			quietMode, _ := cmd.Flags().GetBool("quiet")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return watch.RunWatch(ctx, watch.Options{
				Interval:  interval,
				JSONMode:  jsonMode,
				QuietMode: quietMode,
				IsTTY:     term.IsTerminal(int(os.Stdout.Fd())),
				Stdout:    os.Stdout,
				Stderr:    os.Stderr,
			}, func(ctx context.Context, w *output.Writer) error {
				return runIssueShow(cmd, args, w)
			})
		}
		return runIssueShow(cmd, args, getWriter(cmd))
	},
}

func runIssueShow(cmd *cobra.Command, args []string, w *output.Writer) error {
	conn := getDB(cmd)

	id, err := model.ParseID(args[0])
	if err != nil {
		return cmdErr(fmt.Errorf("invalid issue ID: %w", err), output.ErrValidation)
	}

	data, err := app.GetIssueDetail(conn, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return cmdErr(fmt.Errorf("issue %s not found", args[0]), output.ErrNotFound)
		}
		return cmdErr(err, output.ErrGeneral)
	}

	result := showResult{
		Issue:     data.Issue,
		SubIssues: data.SubIssues,
		Relations: data.Relations,
		Comments:  data.Comments,
		Activity:  data.Activity,
	}

	var message string
	if !w.JSONMode {
		message = render.RenderDetail(data.Issue, data.SubIssues, data.Relations, data.Comments, data.Activity)
	}
	w.Success(result, message)

	return nil
}

func init() {
	issueCmd.AddCommand(showCmd)
}
