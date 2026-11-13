package verb_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func createPlanIssue(t *testing.T, st store.Store, projectID int, title string, priority model.Priority) int {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: projectID,
		Title:     title,
		Status:    model.StatusTodo,
		Priority:  priority,
		Kind:      model.IssueKindFeature,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent(%q): %v", title, err)
	}
	return id
}

func relate(t *testing.T, st store.Store, source int, relType model.RelationType, target int) {
	t.Helper()
	if _, err := st.CreateRelation(&model.Relation{
		SourceIssueID: source,
		TargetIssueID: target,
		RelationType:  relType,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
}

func plannedTitles(result verb.PlanResult) []string {
	var titles []string
	for _, phase := range result.Plan.Phases {
		for _, issue := range phase.Issues {
			titles = append(titles, issue.Title)
		}
	}
	return titles
}

func TestPlanReadsOnlyTheCallersProject(t *testing.T) {
	st := openStore(t)
	mine := ensureProject(t, st, "/repos/mine", "mine")
	theirs := ensureProject(t, st, "/repos/theirs", "theirs")
	createPlanIssue(t, st, mine, "my issue", model.PriorityHigh)
	createPlanIssue(t, st, theirs, "their issue", model.PriorityHigh)

	result, err := verb.Plan(context.Background(), projectEnv(st, "/repos/mine", true), verb.PlanInput{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got, want := plannedTitles(result), []string{"my issue"}; !reflect.DeepEqual(got, want) {
		t.Errorf("planned issues = %v, want %v", got, want)
	}
}

func TestPlanOfAnUnregisteredIdentityIsEmptyAndRegistersNothing(t *testing.T) {
	st := openStore(t)
	createPlanIssue(t, st, store.DefaultProjectID, "default issue", model.PriorityHigh)

	result, err := verb.Plan(context.Background(), projectEnv(st, "/repos/new", true), verb.PlanInput{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if result.Plan.TotalIssues != 0 {
		t.Errorf("total issues = %d, want 0", result.Plan.TotalIssues)
	}
	requireUnregistered(t, st, "/repos/new")
}

func TestPlanAppliesFiltersAndRoot(t *testing.T) {
	st := openStore(t)
	parent := createPlanIssue(t, st, store.DefaultProjectID, "parent", model.PriorityHigh)
	child, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: store.DefaultProjectID,
		ParentID:  &parent,
		Title:     "child",
		Status:    model.StatusTodo,
		Priority:  model.PriorityLow,
		Kind:      model.IssueKindFeature,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent(child): %v", err)
	}
	createPlanIssue(t, st, store.DefaultProjectID, "unrelated", model.PriorityLow)

	result, err := verb.Plan(context.Background(), projectEnv(st, "", false), verb.PlanInput{
		Priorities: []string{"low"},
		Root:       model.FormatID(parent),
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got, want := plannedTitles(result), []string{"child"}; !reflect.DeepEqual(got, want) {
		t.Errorf("planned issues = %v, want %v (child %d)", got, want, child)
	}
}

func TestPlanReportsEachIssuesBlockersInNumericOrder(t *testing.T) {
	st := openStore(t)
	var blockers []int
	for range 10 {
		blockers = append(blockers, createPlanIssue(t, st, store.DefaultProjectID, "blocker", model.PriorityHigh))
	}
	blocked := createPlanIssue(t, st, store.DefaultProjectID, "blocked", model.PriorityHigh)
	// Relations are written highest id first, so an order that follows
	// insertion or compares the formatted ids as text fails.
	for i := len(blockers) - 1; i >= 0; i-- {
		relate(t, st, blocked, model.RelationDependsOn, blockers[i])
	}

	result, err := verb.Plan(context.Background(), projectEnv(st, "", false), verb.PlanInput{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := make([]string, 0, len(blockers))
	for _, id := range blockers {
		want = append(want, model.FormatID(id))
	}
	if got := result.BlockedBy[blocked]; !reflect.DeepEqual(got, want) {
		t.Errorf("BlockedBy[blocked] = %v, want %v", got, want)
	}
	if got := result.BlockedBy[blockers[0]]; len(got) != 0 {
		t.Errorf("BlockedBy[blocker] = %v, want none", got)
	}
}

func TestPlanRefusesInvalidInputAsValidation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         verb.PlanInput
		wantPrefix string
	}{
		{"status", verb.PlanInput{Statuses: []string{"shipped"}}, `invalid status "shipped"`},
		{"priority", verb.PlanInput{Priorities: []string{"urgent"}}, `invalid priority "urgent"`},
		{"size", verb.PlanInput{Sizes: []string{"huge"}}, `invalid size "huge"`},
		{"type", verb.PlanInput{Types: []string{"epic-ish"}}, `invalid issue kind "epic-ish"`},
		{"root", verb.PlanInput{Root: "not-an-id"}, "invalid root ID: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verb.Plan(context.Background(), projectEnv(openStore(t), "", false), tc.in)
			requireVerbError(t, err, output.ErrValidation, tc.wantPrefix)
		})
	}
}

func TestPlanReportsADependencyCycleAsConflict(t *testing.T) {
	st := openStore(t)
	a := createPlanIssue(t, st, store.DefaultProjectID, "a", model.PriorityHigh)
	b := createPlanIssue(t, st, store.DefaultProjectID, "b", model.PriorityHigh)
	// The store refuses a cycle within one relation type, so this one crosses
	// two: b blocks a by a's depends_on, and a blocks b directly.
	relate(t, st, a, model.RelationDependsOn, b)
	relate(t, st, a, model.RelationBlocks, b)

	_, err := verb.Plan(context.Background(), projectEnv(st, "", false), verb.PlanInput{})
	requireCode(t, err, output.ErrConflict)
}
