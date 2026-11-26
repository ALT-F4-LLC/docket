package api

import (
	"context"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/store/storetest"
)

func openDocStore(t *testing.T) store.Store {
	t.Helper()
	storePath := filepath.Join(t.TempDir(), "issues.db")
	storetest.SeedFile(t, storePath)
	st, err := store.OpenSQLite(storePath)
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
	return st
}

// attributed sets the headers unary requires on every RPC but Handshake.
func attributed[T any](msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Docket-Author", "doc-handler-author")
	req.Header().Set("Docket-Cwd", "/srv/doc-handler")
	return req
}

// mustServe fails the test on any RPC error, naming an unimplemented answer,
// which means the doc method is missing from *Service.
func mustServe(t *testing.T, rpc string, err error) {
	t.Helper()
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("%s answered unimplemented: *Service does not serve it", rpc)
	}
	if err != nil {
		t.Fatalf("%s: %v", rpc, err)
	}
}

func TestDocHandlersBindVerbLayer(t *testing.T) {
	st := openDocStore(t)
	issueID, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title: "linked issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("creating the issue: %v", err)
	}
	issueRef := model.FormatID(issueID)
	server := serve(t, NewService(st, "dev"))
	// The raw client sends the headers attributed sets; client.NewClient
	// would replace them with this host's identity.
	c := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)
	ctx := context.Background()

	created, err := c.DocCreate(ctx, attributed(&docketv1.DocCreateRequest{
		Title: "design", Description: "first body", Type: "tdd", Status: "draft",
	}))
	mustServe(t, "DocCreate", err)
	doc := created.Msg.GetDoc()
	if doc.GetTitle() != "design" || doc.GetBody() != "first body" || doc.GetAuthor() != "doc-handler-author" {
		t.Fatalf("DocCreate doc = %+v, want title design, body first body, author from Docket-Author", doc)
	}
	ref := doc.GetId()

	title := "design v2"
	edited, err := c.DocEdit(ctx, attributed(&docketv1.DocEditRequest{Doc: ref, Title: &title}))
	mustServe(t, "DocEdit", err)
	if got := edited.Msg.GetDoc().GetTitle(); got != title {
		t.Errorf("DocEdit title = %q, want %q", got, title)
	}

	listed, err := c.DocList(ctx, attributed(&docketv1.DocListRequest{Limit: 10}))
	mustServe(t, "DocList", err)
	if items := listed.Msg.GetItems(); len(items) != 1 || items[0].GetId() != ref || items[0].GetBody() != "" || listed.Msg.GetTotal() != 1 {
		t.Errorf("DocList = %+v, want the one doc %s without its body", listed.Msg, ref)
	}

	commented, err := c.DocCommentAdd(ctx, attributed(&docketv1.DocCommentAddRequest{Doc: ref, Message: "looks right"}))
	mustServe(t, "DocCommentAdd", err)
	if got := commented.Msg.GetComment(); got.GetBody() != "looks right" || got.GetDocId() != ref {
		t.Errorf("DocCommentAdd comment = %+v, want body looks right on %s", got, ref)
	}

	comments, err := c.DocCommentList(ctx, attributed(&docketv1.DocCommentListRequest{Doc: ref}))
	mustServe(t, "DocCommentList", err)
	if items := comments.Msg.GetItems(); len(items) != 1 || items[0].GetBody() != "looks right" {
		t.Errorf("DocCommentList = %+v, want the one comment", comments.Msg)
	}

	linked, err := c.DocLinkAdd(ctx, attributed(&docketv1.DocLinkAddRequest{Doc: ref, Issue: issueRef}))
	mustServe(t, "DocLinkAdd", err)
	if linked.Msg.GetDoc() != ref || linked.Msg.GetIssue() != issueRef {
		t.Errorf("DocLinkAdd = %+v, want %s and %s", linked.Msg, ref, issueRef)
	}

	shown, err := c.DocShow(ctx, attributed(&docketv1.DocShowRequest{Doc: ref}))
	mustServe(t, "DocShow", err)
	if got := shown.Msg.GetDoc(); got.GetTitle() != title || len(got.GetRevisions()) != 2 ||
		len(got.GetLinkedIssues()) != 1 || got.GetLinkedIssues()[0] != issueRef {
		t.Errorf("DocShow doc = %+v, want title %q, two revisions, linked to %s", got, title, issueRef)
	}

	unlinked, err := c.DocLinkRemove(ctx, attributed(&docketv1.DocLinkRemoveRequest{Doc: ref, Issue: issueRef}))
	mustServe(t, "DocLinkRemove", err)
	if unlinked.Msg.GetDoc() != ref || unlinked.Msg.GetIssue() != issueRef {
		t.Errorf("DocLinkRemove = %+v, want %s and %s", unlinked.Msg, ref, issueRef)
	}

	deleted, err := c.DocDelete(ctx, attributed(&docketv1.DocDeleteRequest{Doc: ref, Force: true}))
	mustServe(t, "DocDelete", err)
	if got := deleted.Msg.GetId(); got != ref {
		t.Errorf("DocDelete id = %q, want %q", got, ref)
	}
}
