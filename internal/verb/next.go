package verb

import (
	"context"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/filter"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// NextInput asks for work-ready issues matching every filter set, or, with
// Run set, for that run's ready steps. Limit caps the rows returned; 0 means
// no limit.
type NextInput struct {
	// Statuses are the statuses a ready issue may hold; none means backlog
	// and todo.
	Statuses   []string
	Priorities []string
	Sizes      []string
	Labels     []string
	Types      []string
	// Run switches to step mode, where every issue filter is refused. A
	// filter counts as set when it is non-nil, so an empty but non-nil one is
	// refused too.
	Run   string
	Limit int
}

// Validate checks in without reading the store: the filter values in issue
// mode; in run mode the run reference, then the refused filters. Next runs it
// itself. A caller with checks of its own that must report after these runs
// it first.
func (in NextInput) Validate() error {
	if in.Run != "" {
		if _, err := ParseRunRef(in.Run); err != nil {
			return err
		}
		return refuseIssueFilters(in)
	}
	for _, s := range in.Statuses {
		if err := model.ValidateStatus(model.Status(s)); err != nil {
			return newError(err, output.ErrValidation)
		}
	}
	for _, p := range in.Priorities {
		if err := model.ValidatePriority(model.Priority(p)); err != nil {
			return newError(err, output.ErrValidation)
		}
	}
	for _, sz := range in.Sizes {
		if err := model.ValidateSize(model.Size(sz)); err != nil {
			return newError(err, output.ErrValidation)
		}
	}
	for _, t := range in.Types {
		if err := model.ValidateIssueKind(model.IssueKind(t)); err != nil {
			return newError(err, output.ErrValidation)
		}
	}
	return nil
}

// refuseIssueFilters refuses the issue filters in step mode rather than
// ignoring them: a dispatcher asking for `--priority critical` steps that got
// every step would dispatch work it meant to defer. Each is named by the CLI
// flag that sets it.
func refuseIssueFilters(in NextInput) error {
	var conflicting []string
	for _, f := range []struct {
		flag   string
		values []string
	}{
		{"--status", in.Statuses},
		{"--priority", in.Priorities},
		{"--size", in.Sizes},
		{"--label", in.Labels},
		{"--type", in.Types},
	} {
		if f.values != nil {
			conflicting = append(conflicting, f.flag)
		}
	}
	if len(conflicting) == 0 {
		return nil
	}
	subject := "is an issue filter"
	if len(conflicting) > 1 {
		subject = "are issue filters"
	}
	return newError(
		fmt.Errorf(
			"%s %s and cannot be combined with --run, which lists a run's "+
				"ready STEPS; inspect the run with `docket run status` instead",
			strings.Join(conflicting, ", "), subject,
		),
		output.ErrValidation,
	)
}

// NextResult is the ready set: Issues in issue mode, Steps in run mode, the
// other nil. Total is the size of the ready set before Limit cut it.
type NextResult struct {
	Issues []*model.Issue
	Steps  *engine.ReadySteps
	Total  int
}

// Next answers what is ready to work on. It is a read in both modes: an
// identity no project holds lists no issues, and nothing is registered. Run
// mode reaps expired leases on the way, the one write it makes.
func Next(ctx context.Context, env Env, in NextInput) (NextResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return NextResult{}, err
	}
	if err := in.Validate(); err != nil {
		return NextResult{}, err
	}
	if in.Run != "" {
		return nextSteps(env, in)
	}
	return nextIssues(env, in, projectID)
}

func nextSteps(env Env, in NextInput) (NextResult, error) {
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return NextResult{}, err
	}
	ready, err := engine.NewEngine().NextStepsStore(env.Store, runID, in.Limit, model.NowMS())
	if err != nil {
		return NextResult{}, engineError(err)
	}
	return NextResult{Steps: ready, Total: ready.Total}, nil
}

func nextIssues(env Env, in NextInput, projectID int) (NextResult, error) {
	// Every open issue, unlimited: the DAG needs all of them.
	allIssues, _, err := env.Store.ListIssues(store.ListOptions{
		ProjectID:   projectID,
		IncludeDone: false,
	})
	if err != nil {
		return NextResult{}, newError(fmt.Errorf("listing issues: %w", err), output.ErrGeneral)
	}
	relations, err := env.Store.GetAllDirectionalRelations()
	if err != nil {
		return NextResult{}, newError(fmt.Errorf("loading relations: %w", err), output.ErrGeneral)
	}

	readyStatuses := in.Statuses
	if len(readyStatuses) == 0 {
		readyStatuses = []string{string(model.StatusBacklog), string(model.StatusTodo)}
	}
	ready := planner.FindReady(planner.BuildDAG(allIssues, relations), readyStatuses)
	ready = filterReady(ready, in.Priorities, in.Sizes, in.Labels, in.Types)

	total := len(ready)
	if in.Limit > 0 && len(ready) > in.Limit {
		ready = ready[:in.Limit]
	}

	if err := env.Store.HydrateDocs(ready); err != nil {
		return NextResult{}, newError(fmt.Errorf("fetching linked docs: %w", err), output.ErrGeneral)
	}

	// FindReady and filterReady return nil when nothing is ready; the result
	// is an empty slice so every transport renders `[]`.
	if ready == nil {
		ready = []*model.Issue{}
	}
	return NextResult{Issues: ready, Total: total}, nil
}

// filterReady applies priority, size, label, and type filters to a slice of
// ready issues.
func filterReady(issues []*model.Issue, priorities, sizes, labels, types []string) []*model.Issue {
	if len(priorities) == 0 && len(sizes) == 0 && len(labels) == 0 && len(types) == 0 {
		return issues
	}

	prioritySet := filter.ToStringSet(priorities)
	sizeSet := filter.ToStringSet(sizes)
	labelSet := filter.ToStringSet(labels)
	typeSet := filter.ToStringSet(types)

	var filtered []*model.Issue
	for _, issue := range issues {
		if len(prioritySet) > 0 {
			if _, ok := prioritySet[string(issue.Priority)]; !ok {
				continue
			}
		}
		if len(sizeSet) > 0 {
			if _, ok := sizeSet[string(issue.Size)]; !ok {
				continue
			}
		}
		if len(typeSet) > 0 {
			if _, ok := typeSet[string(issue.Kind)]; !ok {
				continue
			}
		}
		if len(labelSet) > 0 && !filter.HasAllLabels(issue, labelSet) {
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered
}
