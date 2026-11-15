package api

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

func TestIssueHandlersBindVerbLayer(t *testing.T) {
	f := newStepFixture(t)
	c, ctx := f.c, context.Background()

	created, err := c.IssueCreate(ctx, connect.NewRequest(&docketv1.IssueCreateRequest{
		Title: "parent", Description: "the body", Status: "todo", Priority: "medium", Type: "task",
		Label: []string{"alpha"}, File: []string{"a.go"},
	}))
	requireServed(t, "IssueCreate", err)
	parent := created.Msg.GetIssue()
	if parent.GetTitle() != "parent" || parent.GetDescription() != "the body" ||
		!slices.Equal(parent.GetLabels(), []string{"alpha"}) || !slices.Equal(parent.GetFiles(), []string{"a.go"}) {
		t.Fatalf("IssueCreate issue = %v, want the requested title, body, label and file", parent)
	}
	ref := parent.GetId()
	child := f.createIssue(t, &docketv1.IssueCreateRequest{Title: "child", Status: "todo", Priority: "low", Type: "task", Parent: ref})
	if child.GetParentId() != ref {
		t.Errorf("IssueCreate child parent_id = %q, want %q", child.GetParentId(), ref)
	}
	other := f.createIssue(t, &docketv1.IssueCreateRequest{Title: "other", Status: "todo", Priority: "low", Type: "task"})

	edited, err := c.IssueEdit(ctx, connect.NewRequest(&docketv1.IssueEditRequest{Issue: ref, Title: proto.String("renamed")}))
	requireServed(t, "IssueEdit", err)
	if got := edited.Msg.GetIssue().GetTitle(); got != "renamed" {
		t.Errorf("IssueEdit title = %q, want renamed", got)
	}

	moved, err := c.IssueMove(ctx, connect.NewRequest(&docketv1.IssueMoveRequest{Issue: other.GetId(), Status: "review"}))
	requireServed(t, "IssueMove", err)
	if got := moved.Msg.GetIssue().GetStatus(); got != "review" {
		t.Errorf("IssueMove status = %q, want review", got)
	}

	listed, err := c.IssueList(ctx, connect.NewRequest(&docketv1.IssueListRequest{
		Filter: &docketv1.IssueFilter{Status: []string{"todo"}}, Limit: 1,
	}))
	requireServed(t, "IssueList", err)
	if len(listed.Msg.GetItems()) != 1 || listed.Msg.GetTotal() != 2 || !listed.Msg.GetTruncated() {
		t.Errorf("IssueList = %v, want one of the two todo issues, truncated", listed.Msg)
	}

	comment, err := c.IssueCommentAdd(ctx, connect.NewRequest(&docketv1.IssueCommentAddRequest{Issue: ref, Message: "noted"}))
	requireServed(t, "IssueCommentAdd", err)
	if got := comment.Msg.GetComment(); got.GetBody() != "noted" || got.GetIssueId() != ref {
		t.Errorf("IssueCommentAdd comment = %v, want noted on %s", got, ref)
	}
	comments, err := c.IssueCommentList(ctx, connect.NewRequest(&docketv1.IssueCommentListRequest{Issue: ref}))
	requireServed(t, "IssueCommentList", err)
	if comments.Msg.GetTotal() != 1 || comments.Msg.GetItems()[0].GetBody() != "noted" {
		t.Errorf("IssueCommentList = %v, want the one comment", comments.Msg)
	}

	history, err := c.IssueLog(ctx, connect.NewRequest(&docketv1.IssueLogRequest{Issue: ref, Limit: 50}))
	requireServed(t, "IssueLog", err)
	if !slices.ContainsFunc(history.Msg.GetItems(), func(a *docketv1.Activity) bool { return a.GetNewValue() == "renamed" }) {
		t.Errorf("IssueLog items = %v, want the title edit", history.Msg.GetItems())
	}

	added, err := c.IssueFileAdd(ctx, connect.NewRequest(&docketv1.IssueFileAddRequest{Issue: ref, Paths: []string{"b.go"}}))
	requireServed(t, "IssueFileAdd", err)
	if !slices.Contains(added.Msg.GetFiles(), "b.go") {
		t.Errorf("IssueFileAdd files = %v, want b.go attached", added.Msg.GetFiles())
	}
	removed, err := c.IssueFileRemove(ctx, connect.NewRequest(&docketv1.IssueFileRemoveRequest{Issue: ref, Paths: []string{"a.go"}}))
	requireServed(t, "IssueFileRemove", err)
	if slices.Contains(removed.Msg.GetFiles(), "a.go") {
		t.Errorf("IssueFileRemove files = %v, want a.go detached", removed.Msg.GetFiles())
	}
	files, err := c.IssueFileList(ctx, connect.NewRequest(&docketv1.IssueFileListRequest{Issue: ref}))
	requireServed(t, "IssueFileList", err)
	if !slices.Equal(files.Msg.GetFiles(), []string{"b.go"}) || files.Msg.GetIssue() != ref {
		t.Errorf("IssueFileList = %v, want b.go on %s", files.Msg, ref)
	}

	labeled, err := c.IssueLabelAdd(ctx, connect.NewRequest(&docketv1.IssueLabelAddRequest{Issue: ref, Labels: []string{"beta"}}))
	requireServed(t, "IssueLabelAdd", err)
	if got := labeled.Msg.GetIssue().GetLabels(); !slices.Contains(got, "alpha") || !slices.Contains(got, "beta") {
		t.Errorf("IssueLabelAdd labels = %v, want alpha and beta", got)
	}
	unlabeled, err := c.IssueLabelRm(ctx, connect.NewRequest(&docketv1.IssueLabelRmRequest{Issue: ref, Labels: []string{"alpha"}}))
	requireServed(t, "IssueLabelRm", err)
	if got := unlabeled.Msg.GetIssue().GetLabels(); !slices.Equal(got, []string{"beta"}) {
		t.Errorf("IssueLabelRm labels = %v, want beta only", got)
	}
	labels, err := c.IssueLabelList(ctx, connect.NewRequest(&docketv1.IssueLabelListRequest{}))
	requireServed(t, "IssueLabelList", err)
	if !slices.ContainsFunc(labels.Msg.GetItems(), func(l *docketv1.Label) bool { return l.GetName() == "beta" && l.GetIssueCount() == 1 }) {
		t.Errorf("IssueLabelList items = %v, want beta on one issue", labels.Msg.GetItems())
	}
	deletedLabel, err := c.IssueLabelDelete(ctx, connect.NewRequest(&docketv1.IssueLabelDeleteRequest{Label: "beta", Force: true}))
	requireServed(t, "IssueLabelDelete", err)
	if deletedLabel.Msg.GetLabel() != "beta" || deletedLabel.Msg.GetDetached() != 1 {
		t.Errorf("IssueLabelDelete = %v, want beta detached from one issue", deletedLabel.Msg)
	}

	linked, err := c.IssueLinkAdd(ctx, connect.NewRequest(&docketv1.IssueLinkAddRequest{Issue: ref, Relation: "blocks", Target: other.GetId()}))
	requireServed(t, "IssueLinkAdd", err)
	if got := linked.Msg.GetRelation(); got.GetSourceIssueId() != ref || got.GetTargetIssueId() != other.GetId() || got.GetRelationType() != "blocks" {
		t.Errorf("IssueLinkAdd relation = %v, want %s blocks %s", got, ref, other.GetId())
	}
	links, err := c.IssueLinkList(ctx, connect.NewRequest(&docketv1.IssueLinkListRequest{Issue: ref}))
	requireServed(t, "IssueLinkList", err)
	if links.Msg.GetTotal() != 1 {
		t.Errorf("IssueLinkList = %v, want the one relation", links.Msg)
	}
	graph, err := c.IssueGraph(ctx, connect.NewRequest(&docketv1.IssueGraphRequest{Issue: ref, Direction: "down"}))
	requireServed(t, "IssueGraph", err)
	if graph.Msg.GetRoot() != ref || len(graph.Msg.GetNodes()) != 2 || len(graph.Msg.GetEdges()) != 1 ||
		graph.Msg.GetEdges()[0].GetTarget() != other.GetId() {
		t.Errorf("IssueGraph = %v, want %s blocking %s", graph.Msg, ref, other.GetId())
	}

	shown, err := c.IssueShow(ctx, connect.NewRequest(&docketv1.IssueShowRequest{Issues: []string{ref}}))
	requireServed(t, "IssueShow", err)
	if len(shown.Msg.GetIssues()) != 1 {
		t.Fatalf("IssueShow issues = %v, want one detail", shown.Msg.GetIssues())
	}
	detail := shown.Msg.GetIssues()[0]
	if detail.GetIssue().GetId() != ref || len(detail.GetChildren()) != 1 || len(detail.GetRelations()) != 1 || len(detail.GetComments()) != 1 {
		t.Errorf("IssueShow detail = %v, want %s with its child, relation and comment", detail, ref)
	}

	unlinked, err := c.IssueLinkRemove(ctx, connect.NewRequest(&docketv1.IssueLinkRemoveRequest{Issue: ref, Relation: "blocks", Target: other.GetId()}))
	requireServed(t, "IssueLinkRemove", err)
	if got := unlinked.Msg.GetRelation(); got.GetSourceIssueId() != ref || got.GetRelationType() != "blocks" {
		t.Errorf("IssueLinkRemove relation = %v, want %s blocks", got, ref)
	}

	f.leaseRows(t, ref)

	reopened, err := c.IssueReopen(ctx, connect.NewRequest(&docketv1.IssueReopenRequest{Issue: ref}))
	requireServed(t, "IssueReopen", err)
	if got := reopened.Msg.GetIssue().GetStatus(); got != "backlog" {
		t.Errorf("IssueReopen status = %q, want backlog", got)
	}

	deleted, err := c.IssueDelete(ctx, connect.NewRequest(&docketv1.IssueDeleteRequest{Issue: child.GetId()}))
	requireServed(t, "IssueDelete", err)
	if !slices.Equal(deleted.Msg.GetDeleted(), []string{child.GetId()}) {
		t.Errorf("IssueDelete deleted = %v, want %s", deleted.Msg.GetDeleted(), child.GetId())
	}
	if exists, err := f.st.IssueExists(issueID(t, child.GetId())); err != nil || exists {
		t.Errorf("IssueExists(%s) after delete = %v, %v; want gone", child.GetId(), exists, err)
	}
}

// leaseRows runs the threat model's adversarial token rows against issue ref,
// a member of a bound run, while it holds a live lease: a refused call must
// leave the issue's version and status as they were, and only the holder's
// token succeeds.
func (f *stepFixture) leaseRows(t *testing.T, ref string) {
	t.Helper()
	c, ctx, id := f.c, context.Background(), issueID(t, ref)
	f.bindRun(t, id)

	token := f.claimIssue(t, ref)
	for _, row := range []struct {
		name, token string
		code        connect.Code
		docket      output.ErrorCode
	}{
		{"wrong token", "not-the-token", connect.CodePermissionDenied, output.ErrAuth},
		{"empty token", "", connect.CodeInvalidArgument, output.ErrValidation},
	} {
		before := f.issueState(t, id)
		_, err := c.IssueHeartbeat(ctx, connect.NewRequest(&docketv1.IssueHeartbeatRequest{Issue: ref, Token: row.token}))
		requireRefusal(t, "IssueHeartbeat with "+row.name, err, row.code, row.docket)
		if after := f.issueState(t, id); after != before {
			t.Errorf("IssueHeartbeat with %s changed the issue: %+v -> %+v", row.name, before, after)
		}

		before = f.issueState(t, id)
		_, err = c.IssueRelease(ctx, connect.NewRequest(&docketv1.IssueReleaseRequest{Issue: ref, Token: row.token}))
		requireRefusal(t, "IssueRelease with "+row.name, err, row.code, row.docket)
		if after := f.issueState(t, id); after != before {
			t.Errorf("IssueRelease with %s changed the issue: %+v -> %+v", row.name, before, after)
		}
	}

	beat, err := c.IssueHeartbeat(ctx, connect.NewRequest(&docketv1.IssueHeartbeatRequest{Issue: ref, Token: token}))
	requireServed(t, "IssueHeartbeat", err)
	if beat.Msg.GetIssue() != ref || beat.Msg.GetLeaseExpiresMs() <= model.NowMS() {
		t.Errorf("IssueHeartbeat = %v, want a live lease on %s", beat.Msg, ref)
	}
	released, err := c.IssueRelease(ctx, connect.NewRequest(&docketv1.IssueReleaseRequest{Issue: ref, Token: token}))
	requireServed(t, "IssueRelease", err)
	if released.Msg.GetIssue() != ref {
		t.Errorf("IssueRelease issue = %q, want %s", released.Msg.GetIssue(), ref)
	}
	if lease, err := f.st.GetIssueLease(id); err != nil || lease.Live(model.NowMS()) {
		t.Errorf("lease after IssueRelease = %+v, %v; want none live", lease, err)
	}

	token = f.claimIssue(t, ref)
	for _, wrong := range []struct{ name, token string }{{"wrong token", "not-the-token"}, {"empty token", ""}} {
		before := f.issueState(t, id)
		_, err := c.IssueClose(ctx, connect.NewRequest(&docketv1.IssueCloseRequest{Issue: ref, Token: wrong.token}))
		requireRefusal(t, "IssueClose with "+wrong.name, err, connect.CodePermissionDenied, output.ErrAuth)
		after := f.issueState(t, id)
		if after != before || after.status == string(model.StatusDone) {
			t.Errorf("IssueClose with %s changed the issue: %+v -> %+v", wrong.name, before, after)
		}
	}
	closed, err := c.IssueClose(ctx, connect.NewRequest(&docketv1.IssueCloseRequest{Issue: ref, Token: token}))
	requireServed(t, "IssueClose", err)
	if got := closed.Msg.GetIssue().GetStatus(); got != string(model.StatusDone) {
		t.Errorf("IssueClose status = %q, want done", got)
	}
}

func (f *stepFixture) createIssue(t *testing.T, msg *docketv1.IssueCreateRequest) *docketv1.Issue {
	t.Helper()
	res, err := f.c.IssueCreate(context.Background(), connect.NewRequest(msg))
	requireServed(t, "IssueCreate", err)
	return res.Msg.GetIssue()
}

// bindRun activates a run of the fixture's workflow over issue id.
func (f *stepFixture) bindRun(t *testing.T, id int) {
	t.Helper()
	run, _, err := f.st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{ExecRoot: f.execRoot}, "", id)
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	if _, err := engine.ActivateStore(f.st, run.ID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}
}

func (f *stepFixture) claimIssue(t *testing.T, ref string) string {
	t.Helper()
	res, err := f.c.IssueClaim(context.Background(), connect.NewRequest(&docketv1.IssueClaimRequest{
		Issue: ref, Owner: "worker", TtlMs: proto.Int64(60_000),
	}))
	requireServed(t, "IssueClaim", err)
	if res.Msg.GetToken() == "" || res.Msg.GetIssue() != ref {
		t.Fatalf("IssueClaim = %v, want a token on %s", res.Msg, ref)
	}
	return res.Msg.GetToken()
}

func (f *stepFixture) issueState(t *testing.T, id int) rowState {
	t.Helper()
	issue, err := f.st.GetIssue(id)
	if err != nil {
		t.Fatalf("GetIssue(%d): %v", id, err)
	}
	return rowState{status: string(issue.Status), version: issue.Version}
}

func issueID(t *testing.T, ref string) int {
	t.Helper()
	id, err := model.ParseID(ref)
	if err != nil {
		t.Fatalf("ParseID(%q): %v", ref, err)
	}
	return id
}
