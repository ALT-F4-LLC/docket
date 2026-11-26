package verb_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// boardIssue is one issue a board test arranges: only the fields the board
// filters or rolls up on are set.
type boardIssue struct {
	project  int
	title    string
	status   model.Status
	priority model.Priority
	parent   *int
}

func createBoardIssue(t *testing.T, st store.Store, in boardIssue) int {
	t.Helper()
	priority := in.priority
	if priority == "" {
		priority = model.PriorityMedium
	}
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: in.project,
		ParentID:  in.parent,
		Title:     in.title,
		Status:    in.status,
		Priority:  priority,
		Kind:      model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent(%q): %v", in.title, err)
	}
	return id
}

func boardTitles(res verb.BoardResult) []string {
	titles := make([]string, 0, len(res.Issues))
	for _, issue := range res.Issues {
		titles = append(titles, issue.Title)
	}
	slices.Sort(titles)
	return titles
}

func TestBoardRollsUpSubIssuesUnlessExpanded(t *testing.T) {
	st := openStore(t)
	parent := createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "parent", status: model.StatusTodo})
	createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "child", status: model.StatusDone, parent: &parent})
	createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "loner", status: model.StatusBacklog})

	rolled, err := verb.Board(context.Background(), verb.Env{Store: st}, verb.BoardInput{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if got, want := boardTitles(rolled), []string{"loner", "parent"}; !slices.Equal(got, want) {
		t.Errorf("Board = %v, want %v (sub-issues rolled up, done included)", got, want)
	}

	expanded, err := verb.Board(context.Background(), verb.Env{Store: st}, verb.BoardInput{Expand: true})
	if err != nil {
		t.Fatalf("Board(expand): %v", err)
	}
	if got, want := boardTitles(expanded), []string{"child", "loner", "parent"}; !slices.Equal(got, want) {
		t.Errorf("Board(expand) = %v, want %v", got, want)
	}
}

func TestBoardShowsOnlyTheResolvedProjectsIssues(t *testing.T) {
	st := openStore(t)
	first, err := st.EnsureProject("/repos/first", "first", 1)
	if err != nil {
		t.Fatalf("EnsureProject(first): %v", err)
	}
	other, err := st.EnsureProject("/repos/other", "other", 1)
	if err != nil {
		t.Fatalf("EnsureProject(other): %v", err)
	}
	createBoardIssue(t, st, boardIssue{project: first, title: "first issue", status: model.StatusTodo})
	createBoardIssue(t, st, boardIssue{project: other, title: "other issue", status: model.StatusTodo})

	res, err := verb.Board(context.Background(), projectEnv(st, "/repos/other", false), verb.BoardInput{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if got, want := boardTitles(res), []string{"other issue"}; !slices.Equal(got, want) {
		t.Errorf("Board in /repos/other = %v, want %v", got, want)
	}
}

func TestBoardUnregisteredIdentityShowsNothingAndRegistersNothing(t *testing.T) {
	st := openStore(t)
	createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "default issue", status: model.StatusTodo})

	res, err := verb.Board(context.Background(), projectEnv(st, "/repos/new", true), verb.BoardInput{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Errorf("Board for an unregistered identity = %v, want none", boardTitles(res))
	}
	requireUnregistered(t, st, "/repos/new")
}

func TestBoardFiltersByPriority(t *testing.T) {
	st := openStore(t)
	createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "urgent", status: model.StatusTodo, priority: model.PriorityHigh})
	createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "routine", status: model.StatusTodo, priority: model.PriorityLow})

	res, err := verb.Board(context.Background(), verb.Env{Store: st}, verb.BoardInput{Priorities: []string{"high"}})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if got, want := boardTitles(res), []string{"urgent"}; !slices.Equal(got, want) {
		t.Errorf("Board(priority=high) = %v, want %v", got, want)
	}
}

func TestBoardRejectsUnknownPriorityAndSize(t *testing.T) {
	st := openStore(t)
	for name, in := range map[string]verb.BoardInput{
		"priority": {Priorities: []string{"urgent"}},
		"size":     {Sizes: []string{"huge"}},
	} {
		_, err := verb.Board(context.Background(), verb.Env{Store: st}, in)
		var ve *verb.Error
		if !errors.As(err, &ve) || ve.Code != output.ErrValidation {
			t.Errorf("Board with an unknown %s: err = %v, want a VALIDATION_ERROR verb.Error", name, err)
		}
	}
}

func TestBoardReportsSubIssueProgressOnlyWhenAsked(t *testing.T) {
	st := openStore(t)
	parent := createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "parent", status: model.StatusInProgress})
	createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "done child", status: model.StatusDone, parent: &parent})
	createBoardIssue(t, st, boardIssue{project: store.DefaultProjectID, title: "open child", status: model.StatusTodo, parent: &parent})

	without, err := verb.Board(context.Background(), verb.Env{Store: st}, verb.BoardInput{})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if without.Progress != nil {
		t.Errorf("Board without SubIssueProgress: Progress = %v, want nil", without.Progress)
	}

	with, err := verb.Board(context.Background(), verb.Env{Store: st}, verb.BoardInput{SubIssueProgress: true})
	if err != nil {
		t.Fatalf("Board(progress): %v", err)
	}
	if got, want := with.Progress[parent], [2]int{1, 2}; got != want {
		t.Errorf("Progress[parent] = %v, want %v (done, total)", got, want)
	}
}
