package api

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

const planCallerIdentity = "/repos/plan-caller"

// planRequest addresses msg to the test's registered project with the
// attribution headers unary requires.
func planRequest(msg *docketv1.PlanRequest) *connect.Request[docketv1.PlanRequest] {
	req := connect.NewRequest(msg)
	req.Header().Set(authorHeader, authorSentinel)
	req.Header().Set(cwdHeader, cwdSentinel)
	req.Header().Set(projectHeader, planCallerIdentity)
	return req
}

func createPlannedIssue(t *testing.T, st store.Store, projectID int, title string) int {
	t.Helper()
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID:   projectID,
		Title:       title,
		Description: title + " body",
		Status:      model.StatusTodo,
		Priority:    model.PriorityHigh,
		Kind:        model.IssueKindFeature,
	}, []string{"api"}, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent(%q): %v", title, err)
	}
	return id
}

func TestPlanHandlersBindVerbLayer(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "issues.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// A default-project issue the plan must not carry: a handler that dropped
	// the Docket-Project header would plan the default project instead.
	if _, err := st.EnsureProject("/repos/default", "default", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	createPlannedIssue(t, st, store.DefaultProjectID, "default issue")
	projectID, err := st.EnsureProject(planCallerIdentity, "plan-caller", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	first := createPlannedIssue(t, st, projectID, "first")
	second := createPlannedIssue(t, st, projectID, "second")
	if _, err := st.CreateRelation(&model.Relation{
		SourceIssueID: second,
		TargetIssueID: first,
		RelationType:  model.RelationDependsOn,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}

	server := serve(t, NewService(st, "dev"))
	rpc := client.NewClient(server.Client(), server.URL, discardLogger)

	res, err := rpc.Plan(ctx, planRequest(&docketv1.PlanRequest{
		Filter: &docketv1.IssueFilter{Label: []string{"api"}},
	}))
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("Plan answered unimplemented: no *Service method serves it (%v)", err)
	}
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	var phaseIDs [][]string
	for i, phase := range res.Msg.GetPhases() {
		if phase.GetPhase() != int32(i+1) {
			t.Errorf("phase %d numbered %d, want %d", i, phase.GetPhase(), i+1)
		}
		var ids []string
		for _, issue := range phase.GetIssues() {
			ids = append(ids, issue.GetId())
		}
		phaseIDs = append(phaseIDs, ids)
	}
	want := [][]string{{model.FormatID(first)}, {model.FormatID(second)}}
	if !reflect.DeepEqual(phaseIDs, want) {
		t.Errorf("phases = %v, want %v: first, then the issue that depends on it", phaseIDs, want)
	}
	if got, want := res.Msg.GetBlocked(), []string{model.FormatID(second)}; !reflect.DeepEqual(got, want) {
		t.Errorf("blocked = %v, want %v", got, want)
	}
	if len(phaseIDs) == 2 {
		stored, err := st.GetIssue(first)
		if err != nil {
			t.Fatalf("GetIssue: %v", err)
		}
		issue := res.Msg.GetPhases()[0].GetIssues()[0]
		if issue.GetTitle() != "first" || issue.GetDescription() != "first body" || issue.GetStatus() != "todo" ||
			issue.GetPriority() != "high" || issue.GetKind() != "feature" || !reflect.DeepEqual(issue.GetLabels(), []string{"api"}) ||
			issue.GetCreatedAt() != stored.CreatedAt.UTC().Format(time.RFC3339) || issue.GetVersion() != int32(stored.Version) {
			t.Errorf("first issue = %v, want the stored row on the wire", issue)
		}
	}

	_, err = rpc.Plan(ctx, planRequest(&docketv1.PlanRequest{Root: "not-an-id"}))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("Plan root not-an-id code = %q, want %q: the verb's error code reaches the client (err %v)", got, output.ErrValidation, err)
	}
}
