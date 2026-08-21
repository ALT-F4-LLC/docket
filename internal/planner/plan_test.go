package planner

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestSplitByFileCollisionEmpty(t *testing.T) {
	result := splitByFileCollision(nil)
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}

	result = splitByFileCollision([]*model.Issue{})
	if result != nil {
		t.Errorf("expected nil for empty slice, got %v", result)
	}
}

func TestSplitByFileCollisionNoFiles(t *testing.T) {
	issues := []*model.Issue{
		{ID: 1, Priority: model.PriorityHigh},
		{ID: 2, Priority: model.PriorityMedium},
		{ID: 3, Priority: model.PriorityLow},
	}

	result := splitByFileCollision(issues)
	if len(result) != 1 {
		t.Fatalf("expected 1 sub-phase, got %d", len(result))
	}
	if len(result[0]) != 3 {
		t.Errorf("expected 3 issues in sub-phase, got %d", len(result[0]))
	}
}

func TestSplitByFileCollisionNoConflict(t *testing.T) {
	issues := []*model.Issue{
		{ID: 1, Priority: model.PriorityHigh, Files: []string{"a.go"}},
		{ID: 2, Priority: model.PriorityMedium, Files: []string{"b.go"}},
		{ID: 3, Priority: model.PriorityLow, Files: []string{"c.go"}},
	}

	result := splitByFileCollision(issues)
	if len(result) != 1 {
		t.Fatalf("expected 1 sub-phase (no conflicts), got %d", len(result))
	}
	if len(result[0]) != 3 {
		t.Errorf("expected 3 issues in sub-phase, got %d", len(result[0]))
	}
}

func TestSplitByFileCollisionAllShareFile(t *testing.T) {
	issues := []*model.Issue{
		{ID: 1, Priority: model.PriorityHigh, Files: []string{"shared.go"}},
		{ID: 2, Priority: model.PriorityMedium, Files: []string{"shared.go"}},
		{ID: 3, Priority: model.PriorityLow, Files: []string{"shared.go"}},
	}

	result := splitByFileCollision(issues)
	if len(result) != 3 {
		t.Fatalf("expected 3 sub-phases (all conflict), got %d", len(result))
	}
	for i, phase := range result {
		if len(phase) != 1 {
			t.Errorf("sub-phase %d: expected 1 issue, got %d", i, len(phase))
		}
	}
	// Verify order preserved: highest priority first.
	if result[0][0].ID != 1 || result[1][0].ID != 2 || result[2][0].ID != 3 {
		t.Errorf("expected IDs [1, 2, 3], got [%d, %d, %d]",
			result[0][0].ID, result[1][0].ID, result[2][0].ID)
	}
}

func TestSplitByFileCollisionMixed(t *testing.T) {
	// Issue 1 and 3 share "shared.go"; issue 2 has no conflict; issue 4 has no files.
	issues := []*model.Issue{
		{ID: 1, Priority: model.PriorityHigh, Files: []string{"shared.go", "a.go"}},
		{ID: 2, Priority: model.PriorityMedium, Files: []string{"b.go"}},
		{ID: 3, Priority: model.PriorityLow, Files: []string{"shared.go"}},
		{ID: 4, Priority: model.PriorityNone},
	}

	result := splitByFileCollision(issues)
	if len(result) != 2 {
		t.Fatalf("expected 2 sub-phases, got %d", len(result))
	}

	// First phase: issues 1, 2, 4 (no collision among them).
	phase1IDs := make(map[int]bool)
	for _, iss := range result[0] {
		phase1IDs[iss.ID] = true
	}
	if !phase1IDs[1] || !phase1IDs[2] || !phase1IDs[4] {
		t.Errorf("phase 1 should contain issues 1, 2, 4; got %v", phase1IDs)
	}

	// Second phase: issue 3 (deferred due to shared.go collision).
	if len(result[1]) != 1 || result[1][0].ID != 3 {
		t.Errorf("phase 2 should contain only issue 3; got %v", result[1])
	}
}

func TestSplitByFileCollisionMultipleFiles(t *testing.T) {
	// Issue 1 touches a.go and b.go; issue 2 touches b.go and c.go (collision on b.go).
	issues := []*model.Issue{
		{ID: 1, Priority: model.PriorityHigh, Files: []string{"a.go", "b.go"}},
		{ID: 2, Priority: model.PriorityMedium, Files: []string{"b.go", "c.go"}},
	}

	result := splitByFileCollision(issues)
	if len(result) != 2 {
		t.Fatalf("expected 2 sub-phases, got %d", len(result))
	}
	if result[0][0].ID != 1 {
		t.Errorf("phase 1 should contain issue 1, got %d", result[0][0].ID)
	}
	if result[1][0].ID != 2 {
		t.Errorf("phase 2 should contain issue 2, got %d", result[1][0].ID)
	}
}

func TestGeneratePlanFilterByPriority(t *testing.T) {
	issues := []*model.Issue{
		{ID: 1, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindTask},
		{ID: 2, Status: model.StatusTodo, Priority: model.PriorityLow, Kind: model.IssueKindTask},
	}
	dag := BuildDAG(issues, nil)

	plan, err := GeneratePlan(dag, PlanFilters{Priorities: []string{"high"}})
	testsupport.Must(t, err, "unexpected error: %v", err)
	if plan.TotalIssues != 1 {
		t.Fatalf("expected 1 issue, got %d", plan.TotalIssues)
	}
	if plan.Phases[0].Issues[0].ID != 1 {
		t.Errorf("expected issue 1 (high priority), got %d", plan.Phases[0].Issues[0].ID)
	}
}

func TestGeneratePlanFilterByType(t *testing.T) {
	issues := []*model.Issue{
		{ID: 1, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindBug},
		{ID: 2, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindFeature},
	}
	dag := BuildDAG(issues, nil)

	plan, err := GeneratePlan(dag, PlanFilters{Types: []string{"bug"}})
	testsupport.Must(t, err, "unexpected error: %v", err)
	if plan.TotalIssues != 1 {
		t.Fatalf("expected 1 issue, got %d", plan.TotalIssues)
	}
	if plan.Phases[0].Issues[0].ID != 1 {
		t.Errorf("expected issue 1 (bug), got %d", plan.Phases[0].Issues[0].ID)
	}
}

func TestGeneratePlanFilterByAssignee(t *testing.T) {
	issues := []*model.Issue{
		{ID: 1, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindTask, Assignee: "alice"},
		{ID: 2, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindTask, Assignee: "bob"},
	}
	dag := BuildDAG(issues, nil)

	plan, err := GeneratePlan(dag, PlanFilters{Assignee: "alice"})
	testsupport.Must(t, err, "unexpected error: %v", err)
	if plan.TotalIssues != 1 {
		t.Fatalf("expected 1 issue, got %d", plan.TotalIssues)
	}
	if plan.Phases[0].Issues[0].ID != 1 {
		t.Errorf("expected issue 1 (alice), got %d", plan.Phases[0].Issues[0].ID)
	}
}

func TestGeneratePlanFiltersAndCompose(t *testing.T) {
	// Issue 1 matches both priority and type filters; issue 2 matches priority
	// only; issue 3 matches type only. Only issue 1 should survive.
	issues := []*model.Issue{
		{ID: 1, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindBug},
		{ID: 2, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindFeature},
		{ID: 3, Status: model.StatusTodo, Priority: model.PriorityLow, Kind: model.IssueKindBug},
	}
	dag := BuildDAG(issues, nil)

	plan, err := GeneratePlan(dag, PlanFilters{Priorities: []string{"high"}, Types: []string{"bug"}})
	testsupport.Must(t, err, "unexpected error: %v", err)
	if plan.TotalIssues != 1 {
		t.Fatalf("expected 1 issue, got %d", plan.TotalIssues)
	}
	if plan.Phases[0].Issues[0].ID != 1 {
		t.Errorf("expected issue 1, got %d", plan.Phases[0].Issues[0].ID)
	}
}

func TestGeneratePlanLevelsNoCollisions(t *testing.T) {
	// Two independent issues at the same topo-level, no file collisions:
	// TotalLevels should equal TotalPhases (one phase per level).
	issues := []*model.Issue{
		{ID: 1, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindTask, Files: []string{"a.go"}},
		{ID: 2, Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask, Files: []string{"b.go"}},
	}
	dag := BuildDAG(issues, nil)

	plan, err := GeneratePlan(dag, PlanFilters{})
	testsupport.Must(t, err, "unexpected error: %v", err)
	if plan.TotalPhases != 1 {
		t.Fatalf("expected 1 phase, got %d", plan.TotalPhases)
	}
	if plan.TotalLevels != plan.TotalPhases {
		t.Errorf("expected TotalLevels (%d) == TotalPhases (%d)", plan.TotalLevels, plan.TotalPhases)
	}
	if plan.Phases[0].Level != 1 {
		t.Errorf("expected phase 1 to have Level 1, got %d", plan.Phases[0].Level)
	}
}

func TestGeneratePlanLevelsFileCollisionSplit(t *testing.T) {
	// Two independent issues (no dependency between them) that share a file:
	// same topo-level, split into 2 sub-phases by file collision.
	// TotalLevels should be less than TotalPhases, and both sub-phases share Level 1.
	issues := []*model.Issue{
		{ID: 1, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindTask, Files: []string{"shared.go"}},
		{ID: 2, Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask, Files: []string{"shared.go"}},
	}
	dag := BuildDAG(issues, nil)

	plan, err := GeneratePlan(dag, PlanFilters{})
	testsupport.Must(t, err, "unexpected error: %v", err)
	if plan.TotalPhases != 2 {
		t.Fatalf("expected 2 phases (file collision split), got %d", plan.TotalPhases)
	}
	if plan.TotalLevels != 1 {
		t.Fatalf("expected 1 level, got %d", plan.TotalLevels)
	}
	if plan.TotalLevels >= plan.TotalPhases {
		t.Errorf("expected TotalLevels (%d) < TotalPhases (%d)", plan.TotalLevels, plan.TotalPhases)
	}
	if plan.Phases[0].Level != plan.Phases[1].Level {
		t.Errorf("expected both sub-phases to share Level, got %d and %d", plan.Phases[0].Level, plan.Phases[1].Level)
	}
}

func TestGeneratePlanLevelsAcrossDependency(t *testing.T) {
	// Issue 2 depends on issue 1: two distinct topo-levels, no file collision.
	// TotalLevels should equal TotalPhases (2), and levels should increment.
	issues := []*model.Issue{
		{ID: 1, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindTask},
		{ID: 2, Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindTask},
	}
	relations := []model.Relation{
		{SourceIssueID: 2, TargetIssueID: 1, RelationType: model.RelationDependsOn},
	}
	dag := BuildDAG(issues, relations)

	plan, err := GeneratePlan(dag, PlanFilters{})
	testsupport.Must(t, err, "unexpected error: %v", err)
	if plan.TotalPhases != 2 {
		t.Fatalf("expected 2 phases, got %d", plan.TotalPhases)
	}
	if plan.TotalLevels != 2 {
		t.Fatalf("expected 2 levels, got %d", plan.TotalLevels)
	}
	if plan.Phases[0].Level != 1 || plan.Phases[1].Level != 2 {
		t.Errorf("expected Levels [1, 2], got [%d, %d]", plan.Phases[0].Level, plan.Phases[1].Level)
	}
}

func TestSplitByFileCollisionNoFilesNeverCollide(t *testing.T) {
	// Multiple issues without files should all land in the same phase.
	issues := []*model.Issue{
		{ID: 1, Priority: model.PriorityHigh, Files: []string{"shared.go"}},
		{ID: 2, Priority: model.PriorityMedium},
		{ID: 3, Priority: model.PriorityLow},
		{ID: 4, Priority: model.PriorityNone, Files: []string{"shared.go"}},
	}

	result := splitByFileCollision(issues)
	if len(result) != 2 {
		t.Fatalf("expected 2 sub-phases, got %d", len(result))
	}

	// Phase 1: issues 1, 2, 3 (no-file issues don't collide).
	if len(result[0]) != 3 {
		t.Errorf("phase 1: expected 3 issues, got %d", len(result[0]))
	}

	// Phase 2: issue 4 (deferred due to shared.go).
	if len(result[1]) != 1 || result[1][0].ID != 4 {
		t.Errorf("phase 2: expected issue 4, got %v", result[1])
	}
}

// TestFindReadyHandlesRoutableEpics is AC-2.
//
// Before RUN-2 no workflow's `[match].kind` listed `epic`, so an epic could
// never bind a workflow. Epics were then made routable, and the concern raised
// was that the container guard in FindReady had been relying on epics never
// getting that far.
//
// It had not. The guard keys on OTHER issues' ParentID and never reads
// Issue.Kind, so both epic shapes get the same answer their non-epic
// equivalents always got. Asserting the two shapes side by side is what makes
// that concrete: if someone later adds a kind test to the guard, the childless
// case fails immediately.
func TestFindReadyHandlesRoutableEpics(t *testing.T) {
	parentID := 1

	tests := []struct {
		name   string
		issues []*model.Issue
		want   []int
	}{
		{
			name: "epic WITH sub-issues is excluded as a container",
			issues: []*model.Issue{
				{ID: 1, Kind: model.IssueKindEpic, Status: model.StatusTodo},
				{ID: 2, Kind: model.IssueKindTask, Status: model.StatusTodo, ParentID: &parentID},
			},
			want: []int{2},
		},
		{
			name: "CHILDLESS epic is ready, exactly like a childless task",
			issues: []*model.Issue{
				{ID: 1, Kind: model.IssueKindEpic, Status: model.StatusTodo},
			},
			want: []int{1},
		},
		{
			name: "a childless TASK is ready — the control for the case above",
			issues: []*model.Issue{
				{ID: 1, Kind: model.IssueKindTask, Status: model.StatusTodo},
			},
			want: []int{1},
		},
		{
			name: "a parent TASK is excluded too — the guard is about children, not kind",
			issues: []*model.Issue{
				{ID: 1, Kind: model.IssueKindTask, Status: model.StatusTodo},
				{ID: 2, Kind: model.IssueKindTask, Status: model.StatusTodo, ParentID: &parentID},
			},
			want: []int{2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready := FindReady(BuildDAG(tt.issues, nil), nil)

			got := make([]int, 0, len(ready))
			for _, issue := range ready {
				got = append(got, issue.ID)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("FindReady returned %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("FindReady returned %v, want %v", got, tt.want)
					break
				}
			}
		})
	}
}

// TestFindReadyIgnoresIssueKind states the guard's invariant directly: the
// ready set is a function of structure and status, never of kind. Every kind
// in the closed set, childless and in an allowed status, is ready.
//
// This is the assertion that would catch a future "epics are special" branch
// added to FindReady without the author realising `next` is a read path that
// activation does not share.
func TestFindReadyIgnoresIssueKind(t *testing.T) {
	for _, kind := range model.ValidIssueKinds() {
		t.Run(string(kind), func(t *testing.T) {
			issues := []*model.Issue{{ID: 1, Kind: kind, Status: model.StatusTodo}}
			if ready := FindReady(BuildDAG(issues, nil), nil); len(ready) != 1 {
				t.Errorf("a childless %s issue is not work-ready; FindReady "+
					"must not discriminate on kind", kind)
			}
		})
	}
}
