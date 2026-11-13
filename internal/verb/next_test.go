package verb_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func nextTitles(res verb.NextResult) []string {
	titles := make([]string, 0, len(res.Issues))
	for _, issue := range res.Issues {
		titles = append(titles, issue.Title)
	}
	slices.Sort(titles)
	return titles
}

func TestNextListsTheResolvedProjectsReadyIssues(t *testing.T) {
	st := openStore(t)
	first, err := st.EnsureProject("/repos/first", "first", 1)
	if err != nil {
		t.Fatalf("EnsureProject(first): %v", err)
	}
	other, err := st.EnsureProject("/repos/other", "other", 1)
	if err != nil {
		t.Fatalf("EnsureProject(other): %v", err)
	}
	createBoardIssue(t, st, boardIssue{project: other, title: "ready", status: model.StatusTodo})
	createBoardIssue(t, st, boardIssue{project: other, title: "busy", status: model.StatusInProgress})
	createBoardIssue(t, st, boardIssue{project: first, title: "elsewhere", status: model.StatusTodo})

	res, err := verb.Next(context.Background(), projectEnv(st, "/repos/other", false), verb.NextInput{})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got, want := nextTitles(res), []string{"ready"}; !slices.Equal(got, want) {
		t.Errorf("Next in /repos/other = %v, want %v (its backlog and todo issues only)", got, want)
	}
	if res.Steps != nil {
		t.Errorf("Steps = %+v, want nil in issue mode", res.Steps)
	}
}

func TestNextNeverRegistersTheCallersProject(t *testing.T) {
	st := openStore(t)
	createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "ready", status: model.StatusTodo})
	const identity = "/work/new-repo"

	res, err := verb.Next(context.Background(), projectEnv(st, identity, true), verb.NextInput{})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Errorf("Next = %v, want nothing for an unregistered identity", nextTitles(res))
	}
	requireUnregistered(t, st, identity)
}

func TestNextLimitKeepsThePreLimitTotal(t *testing.T) {
	st := openStore(t)
	for _, title := range []string{"a", "b", "c"} {
		createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: title, status: model.StatusTodo})
	}

	res, err := verb.Next(context.Background(), verb.Env{Store: st}, verb.NextInput{Limit: 2})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(res.Issues) != 2 || res.Total != 3 {
		t.Errorf("Next(limit 2) = %d issues, total %d; want 2 issues, total 3", len(res.Issues), res.Total)
	}
}

func TestNextEmptyReadySetIsAnEmptySlice(t *testing.T) {
	res, err := verb.Next(context.Background(), verb.Env{Store: openStore(t)}, verb.NextInput{})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if res.Issues == nil || len(res.Issues) != 0 || res.Total != 0 {
		t.Errorf("Next = %#v, want a non-nil empty slice and total 0", res)
	}
}

func TestNextRefusesInvalidInput(t *testing.T) {
	cases := map[string]verb.NextInput{
		"unknown status":       {Statuses: []string{"bogus"}},
		"unknown priority":     {Priorities: []string{"bogus"}},
		"unknown size":         {Sizes: []string{"bogus"}},
		"unknown type":         {Types: []string{"bogus"}},
		"malformed run":        {Run: "not-a-run"},
		"status with run":      {Run: "RUN-1", Statuses: []string{"todo"}},
		"priority with run":    {Run: "RUN-1", Priorities: []string{"high"}},
		"label with run":       {Run: "RUN-1", Labels: []string{"x"}},
		"type with run":        {Run: "RUN-1", Types: []string{"task"}},
		"two filters with run": {Run: "RUN-1", Statuses: []string{"todo"}, Labels: []string{"x"}},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := verb.Next(context.Background(), verb.Env{Store: openStore(t)}, in)
			requireCode(t, err, output.ErrValidation)
		})
	}
}

func TestNextRunModeListsTheRunsReadySteps(t *testing.T) {
	st := openStore(t)
	stepID := activatedStep(t, st)
	step, err := st.GetStep(stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	const identity = "/work/scratch"

	res, err := verb.Next(context.Background(), projectEnv(st, identity, true),
		verb.NextInput{Run: model.FormatRunID(step.RunID)})
	if err != nil {
		t.Fatalf("Next(run): %v", err)
	}
	if res.Steps == nil {
		t.Fatal("Steps = nil, want the run's ready steps")
	}
	var instances []string
	for _, row := range res.Steps.Steps {
		instances = append(instances, row.Instance)
	}
	if !slices.Contains(instances, "first@0") || res.Total != len(res.Steps.Steps) {
		t.Errorf("Steps = %v, total %d; want first@0 among them and the full count", instances, res.Total)
	}
	if res.Issues != nil {
		t.Errorf("Issues = %v, want nil in run mode", nextTitles(res))
	}
	requireUnregistered(t, st, identity)
}

func TestNextRunModeOnAMissingRunIsNotFound(t *testing.T) {
	_, err := verb.Next(context.Background(), verb.Env{Store: openStore(t)}, verb.NextInput{Run: "RUN-999"})
	requireCode(t, err, output.ErrNotFound)
}
