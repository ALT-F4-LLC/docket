package planner

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
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
