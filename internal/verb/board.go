package verb

import (
	"context"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// BoardInput filters the board. Every status is shown, done included.
type BoardInput struct {
	Labels     []string
	Priorities []string
	Sizes      []string
	Assignee   string
	// Expand shows sub-issues as cards of their own instead of rolling them
	// up into their parent.
	Expand bool
	// SubIssueProgress asks for Progress, which only a rendered board shows.
	SubIssueProgress bool
}

// BoardResult is the board's issues in listing order; grouping them into
// status columns is the transport's.
type BoardResult struct {
	Issues []*model.Issue
	// Progress maps a shown issue's ID to its sub-issues' {done, total}
	// counts. It is nil unless the input asked for it.
	Progress map[int][2]int
}

// Board lists the caller's project's issues for the Kanban board.
func Board(ctx context.Context, env Env, in BoardInput) (BoardResult, error) {
	for _, p := range in.Priorities {
		if err := model.ValidatePriority(model.Priority(p)); err != nil {
			return BoardResult{}, newError(err, output.ErrValidation)
		}
	}
	for _, sz := range in.Sizes {
		if err := model.ValidateSize(model.Size(sz)); err != nil {
			return BoardResult{}, newError(err, output.ErrValidation)
		}
	}

	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return BoardResult{}, err
	}

	issues, _, err := env.Store.ListIssues(store.ListOptions{
		ProjectID:   projectID,
		Priorities:  in.Priorities,
		Sizes:       in.Sizes,
		Labels:      in.Labels,
		Assignee:    in.Assignee,
		IncludeDone: true,
	})
	if err != nil {
		return BoardResult{}, newError(fmt.Errorf("listing issues: %w", err), output.ErrGeneral)
	}

	if !in.Expand {
		var roots []*model.Issue
		for _, issue := range issues {
			if issue.ParentID == nil {
				roots = append(roots, issue)
			}
		}
		issues = roots
	}

	res := BoardResult{Issues: issues}
	if in.SubIssueProgress {
		ids := make([]int, len(issues))
		for i, issue := range issues {
			ids[i] = issue.ID
		}
		res.Progress, err = env.Store.GetBatchSubIssueProgress(ids)
		if err != nil {
			return BoardResult{}, newError(fmt.Errorf("fetching sub-issue progress: %w", err), output.ErrGeneral)
		}
	}
	return res, nil
}
