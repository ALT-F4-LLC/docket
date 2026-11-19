package verb_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func createStatsIssue(t *testing.T, st store.Store, projectID int, status model.Status, priority model.Priority, parentID *int, labels []string) int {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: projectID,
		ParentID:  parentID,
		Title:     "issue",
		Status:    status,
		Priority:  priority,
		Kind:      model.IssueKindFeature,
		CreatedAt: now,
		UpdatedAt: now,
	}, labels, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	return id
}

func TestStatsCountsTheCallersProject(t *testing.T) {
	st := openStore(t)
	parent := createStatsIssue(t, st, store.DefaultProjectID, model.StatusTodo, model.PriorityHigh, nil, []string{"bug"})
	createStatsIssue(t, st, store.DefaultProjectID, model.StatusDone, model.PriorityLow, &parent, []string{"bug", "ui"})

	got, err := verb.Stats(context.Background(), projectEnv(st, "", false), verb.StatsInput{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if got.Total != 2 || got.RootIssues != 1 || got.SubIssues != 1 {
		t.Errorf("total, root, sub = %d, %d, %d; want 2, 1, 1", got.Total, got.RootIssues, got.SubIssues)
	}
	if want := map[string]int{"todo": 1, "done": 1}; !reflect.DeepEqual(got.ByStatus, want) {
		t.Errorf("ByStatus = %v, want %v", got.ByStatus, want)
	}
	if want := map[string]int{"high": 1, "low": 1}; !reflect.DeepEqual(got.ByPriority, want) {
		t.Errorf("ByPriority = %v, want %v", got.ByPriority, want)
	}
	counts := map[string]int{}
	for _, l := range got.Labels {
		counts[l.Name] = l.IssueCount
	}
	if want := map[string]int{"bug": 2, "ui": 1}; !reflect.DeepEqual(counts, want) {
		t.Errorf("label counts = %v, want %v", counts, want)
	}
}

// Stats is a read: an anchored identity no project holds counts nothing and
// is not registered.
func TestStatsLeavesAnUnregisteredIdentityUnregistered(t *testing.T) {
	st := openStore(t)
	createStatsIssue(t, st, store.DefaultProjectID, model.StatusTodo, model.PriorityHigh, nil, nil)

	got, err := verb.Stats(context.Background(), projectEnv(st, "/work/elsewhere", true), verb.StatsInput{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if got.Total != 0 {
		t.Errorf("Total = %d, want 0", got.Total)
	}
	requireUnregistered(t, st, "/work/elsewhere")
}
