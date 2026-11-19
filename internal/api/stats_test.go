package api

import (
	"context"
	"reflect"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func createStatsIssue(t *testing.T, st store.Store, status model.Status, priority model.Priority, parentID *int, labels []string) int {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: store.DefaultProjectID,
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

func countsOf(counts []*docketv1.StatusCount) map[string]int32 {
	m := map[string]int32{}
	for _, c := range counts {
		m[c.GetStatus()] = c.GetCount()
	}
	return m
}

func TestStatsHandlersBindVerbLayer(t *testing.T) {
	st := openProjectStore(t)
	parent := createStatsIssue(t, st, model.StatusTodo, model.PriorityHigh, nil, []string{"bug"})
	createStatsIssue(t, st, model.StatusDone, model.PriorityLow, &parent, []string{"bug", "ui"})
	server := serve(t, NewService(st, "test"))
	client := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)

	req := connect.NewRequest(&docketv1.StatsRequest{})
	req.Header().Set(authorHeader, authorSentinel)
	req.Header().Set(cwdHeader, cwdSentinel)
	res, err := client.Stats(context.Background(), req)
	requireServed(t, "Stats", err)

	if got := res.Msg.GetTotal(); got != 2 {
		t.Errorf("total = %d, want 2", got)
	}
	if got, want := countsOf(res.Msg.GetByStatus()), map[string]int32{"done": 1, "todo": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("by_status = %v, want %v", got, want)
	}
	if got, want := countsOf(res.Msg.GetByPriority()), map[string]int32{"high": 1, "low": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("by_priority = %v, want %v", got, want)
	}
	if got, want := countsOf(res.Msg.GetByLabel()), map[string]int32{"bug": 2, "ui": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("by_label = %v, want %v", got, want)
	}
}

// Map-backed counts go on the wire in key order, so two calls over the same
// data answer byte-identical responses.
func TestStatsOrdersMapCountsByKey(t *testing.T) {
	got := encodeStats(verb.StatsResult{ByStatus: map[string]int{"todo": 1, "backlog": 2, "done": 3}})

	var keys []string
	for _, c := range got.GetByStatus() {
		keys = append(keys, c.GetStatus())
	}
	if want := []string{"backlog", "done", "todo"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("by_status keys = %v, want %v", keys, want)
	}
}
