package api

import (
	"context"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

func TestNextHandlersBindVerbLayer(t *testing.T) {
	st := openDocStore(t)
	for _, title := range []string{"first ready", "second ready"} {
		if _, _, err := st.CreateIssueIdempotent(&model.Issue{
			Title: title, Description: "body of " + title,
			Status: model.StatusTodo, Priority: model.PriorityHigh, Kind: model.IssueKindTask,
		}, nil, nil, ""); err != nil {
			t.Fatalf("creating %q: %v", title, err)
		}
	}
	server := serve(t, NewService(st, "dev"))
	c := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)

	res, err := c.Next(context.Background(), attributed(&docketv1.NextRequest{Limit: 1}))
	mustServe(t, "Next", err)

	issues := res.Msg.GetIssues()
	if len(issues) != 1 {
		t.Fatalf("Next issues = %+v, want one under limit 1", issues)
	}
	if got := issues[0]; got.GetId() == "" || got.GetStatus() != "todo" || got.GetPriority() != "high" || got.GetDescription() != "" {
		t.Errorf("Next issue = %+v, want a todo, high issue with no body", got)
	}
	if res.Msg.GetTotal() != 2 || !res.Msg.GetTruncated() {
		t.Errorf("Next total = %d truncated = %v, want 2 and true", res.Msg.GetTotal(), res.Msg.GetTruncated())
	}
	if len(res.Msg.GetSteps()) != 0 {
		t.Errorf("Next steps = %+v, want none in issue mode", res.Msg.GetSteps())
	}
}

func TestNextCarriesBodiesOnlyWithBody(t *testing.T) {
	st := openDocStore(t)
	if _, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title: "ready", Description: "the body",
		Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, ""); err != nil {
		t.Fatalf("creating the issue: %v", err)
	}
	server := serve(t, NewService(st, "dev"))
	c := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)

	res, err := c.Next(context.Background(), attributed(&docketv1.NextRequest{WithBody: true}))
	mustServe(t, "Next", err)

	if issues := res.Msg.GetIssues(); len(issues) != 1 || issues[0].GetDescription() != "the body" {
		t.Errorf("Next issues = %+v, want the one issue with its body", issues)
	}
	if res.Msg.GetTruncated() {
		t.Errorf("Next truncated with no limit")
	}
}

// The verb has no assignee filter; the handler refuses it rather than
// returning issues the caller filtered out.
func TestNextRefusesAssigneeFilter(t *testing.T) {
	server := serve(t, NewService(openDocStore(t), "dev"))
	c := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)

	_, err := c.Next(context.Background(), attributed(&docketv1.NextRequest{
		Filter: &docketv1.IssueFilter{Assignee: "@alice"},
	}))

	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("code = %q, want %q (err %v)", got, output.ErrValidation, err)
	}
}

// Run mode refuses an issue filter in the verb, so the refusal crossing the
// wire shows the run reached verb.Next's step path.
func TestNextRunModeRefusesIssueFilters(t *testing.T) {
	server := serve(t, NewService(openDocStore(t), "dev"))
	c := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)

	_, err := c.Next(context.Background(), attributed(&docketv1.NextRequest{
		Run: "RUN-1", Filter: &docketv1.IssueFilter{Priority: []string{"high"}},
	}))

	if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
		t.Errorf("code = %q, want %q (err %v)", got, output.ErrValidation, err)
	}
}
