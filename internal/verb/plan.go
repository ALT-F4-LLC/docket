package verb

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// PlanInput narrows the plan to the issues matching every filter set. Root,
// when set, is an issue ref that scopes the plan to that issue and its
// descendants.
type PlanInput struct {
	Statuses   []string
	Labels     []string
	Priorities []string
	Sizes      []string
	Types      []string
	Assignee   string
	Root       string
}

// PlanResult is the phased execution plan. BlockedBy maps each planned
// issue's id to the formatted ids of the issues blocking it, in numeric
// order; an issue nothing blocks has no entry.
type PlanResult struct {
	Plan      *planner.Plan
	BlockedBy map[int][]string
}

// Plan groups the caller's open issues into phases that can each be worked in
// parallel. It is a read: an identity no project holds plans nothing.
func Plan(ctx context.Context, env Env, in PlanInput) (PlanResult, error) {
	for _, s := range in.Statuses {
		if err := model.ValidateStatus(model.Status(s)); err != nil {
			return PlanResult{}, newError(err, output.ErrValidation)
		}
	}
	for _, p := range in.Priorities {
		if err := model.ValidatePriority(model.Priority(p)); err != nil {
			return PlanResult{}, newError(err, output.ErrValidation)
		}
	}
	for _, sz := range in.Sizes {
		if err := model.ValidateSize(model.Size(sz)); err != nil {
			return PlanResult{}, newError(err, output.ErrValidation)
		}
	}
	for _, t := range in.Types {
		if err := model.ValidateIssueKind(model.IssueKind(t)); err != nil {
			return PlanResult{}, newError(err, output.ErrValidation)
		}
	}
	filters := planner.PlanFilters{
		Statuses:   in.Statuses,
		Labels:     in.Labels,
		Priorities: in.Priorities,
		Sizes:      in.Sizes,
		Types:      in.Types,
		Assignee:   in.Assignee,
	}
	if in.Root != "" {
		rootID, err := model.ParseID(in.Root)
		if err != nil {
			return PlanResult{}, newError(fmt.Errorf("invalid root ID: %w", err), output.ErrValidation)
		}
		filters.RootID = &rootID
	}

	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return PlanResult{}, err
	}

	issues, _, err := env.Store.ListIssues(store.ListOptions{
		ProjectID:   projectID,
		IncludeDone: false,
		Limit:       0,
	})
	if err != nil {
		return PlanResult{}, newError(fmt.Errorf("listing issues: %w", err), output.ErrGeneral)
	}
	if err := env.Store.HydrateDocs(issues); err != nil {
		return PlanResult{}, newError(fmt.Errorf("hydrating docs: %w", err), output.ErrGeneral)
	}
	relations, err := env.Store.GetAllDirectionalRelations()
	if err != nil {
		return PlanResult{}, newError(fmt.Errorf("fetching relations: %w", err), output.ErrGeneral)
	}

	dag := planner.BuildDAG(issues, relations)
	plan, err := planner.GeneratePlan(dag, filters)
	if err != nil {
		var cycleErr *planner.CycleError
		if errors.As(err, &cycleErr) {
			return PlanResult{}, newError(err, output.ErrConflict)
		}
		return PlanResult{}, newError(fmt.Errorf("generating plan: %w", err), output.ErrGeneral)
	}

	blockedBy := make(map[int][]string)
	for _, phase := range plan.Phases {
		for _, issue := range phase.Issues {
			if deps := collectDeps(issue.ID, dag); len(deps) > 0 {
				blockedBy[issue.ID] = deps
			}
		}
	}
	return PlanResult{Plan: plan, BlockedBy: blockedBy}, nil
}

// collectDeps returns formatted IDs of issues that block the given issue.
func collectDeps(issueID int, dag *planner.DAG) []string {
	node, ok := dag.Nodes[issueID]
	if !ok {
		return nil
	}
	if len(node.Reverse) == 0 {
		return nil
	}
	deps := make([]string, 0, len(node.Reverse))
	for blockerID := range node.Reverse {
		deps = append(deps, model.FormatID(blockerID))
	}
	sortDeps(deps)
	return deps
}

// sortDeps sorts formatted IDs (e.g. "DKT-7", "DKT-12") by their numeric value.
func sortDeps(deps []string) {
	sort.Slice(deps, func(i, j int) bool {
		a, errA := model.ParseID(deps[i])
		b, errB := model.ParseID(deps[j])
		if errA != nil || errB != nil {
			return deps[i] < deps[j]
		}
		return a < b
	})
}
