package verb

import (
	"context"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// StatsInput is empty: stats always summarize the caller's whole project.
type StatsInput struct{}

// StatsResult summarizes the issues in the caller's project. ByStatus and
// ByPriority hold only the values at least one issue carries.
type StatsResult struct {
	Total      int
	RootIssues int
	SubIssues  int
	ByStatus   map[string]int
	ByPriority map[string]int
	Labels     []*model.LabelWithCount
}

// Stats counts the caller's project's issues by shape, status, priority and
// label. It is a read: an identity no project holds counts nothing.
func Stats(ctx context.Context, env Env, in StatsInput) (StatsResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return StatsResult{}, err
	}

	total, err := env.Store.CountIssues(projectID)
	if err != nil {
		return StatsResult{}, newError(fmt.Errorf("counting issues: %w", err), output.ErrGeneral)
	}
	rootCount, err := env.Store.CountRootIssues(projectID)
	if err != nil {
		return StatsResult{}, newError(fmt.Errorf("counting root issues: %w", err), output.ErrGeneral)
	}
	byStatus, err := env.Store.CountByStatus(projectID)
	if err != nil {
		return StatsResult{}, newError(fmt.Errorf("counting by status: %w", err), output.ErrGeneral)
	}
	byPriority, err := env.Store.CountByPriority(projectID)
	if err != nil {
		return StatsResult{}, newError(fmt.Errorf("counting by priority: %w", err), output.ErrGeneral)
	}
	labels, err := env.Store.ListAllLabels(projectID)
	if err != nil {
		return StatsResult{}, newError(fmt.Errorf("listing labels: %w", err), output.ErrGeneral)
	}

	return StatsResult{
		Total:      total,
		RootIssues: rootCount,
		SubIssues:  total - rootCount,
		ByStatus:   byStatus,
		ByPriority: byPriority,
		Labels:     labels,
	}, nil
}
