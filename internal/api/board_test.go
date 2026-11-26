package api

import (
	"context"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/store/storetest"
)

// boardClient serves a Service over a fresh, migrated SQLite store and returns
// the store too, so a test can arrange the issues the board lists.
func boardClient(t *testing.T) (docketv1connect.DocketServiceClient, store.Store) {
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
	server := serve(t, NewService(st, "dev"))
	return docketv1connect.NewDocketServiceClient(server.Client(), server.URL), st
}

func boardRequest(msg *docketv1.BoardRequest) *connect.Request[docketv1.BoardRequest] {
	req := connect.NewRequest(msg)
	req.Header().Set(authorHeader, authorSentinel)
	req.Header().Set(cwdHeader, cwdSentinel)
	return req
}

func createBoardCard(t *testing.T, st store.Store, title string, status model.Status, parent *int) int {
	t.Helper()
	id, _, err := st.CreateIssueIdempotent(&model.Issue{
		ParentID:    parent,
		Title:       title,
		Description: title + " body",
		Status:      status,
		Priority:    model.PriorityMedium,
		Kind:        model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent(%q): %v", title, err)
	}
	return id
}

// boardColumns maps each column's status to its issue titles, keeping the
// column order alongside.
func boardColumns(res *docketv1.BoardResponse) ([]string, map[string][]string) {
	var order []string
	titles := map[string][]string{}
	for _, col := range res.GetColumns() {
		order = append(order, col.GetStatus())
		for _, issue := range col.GetIssues() {
			titles[col.GetStatus()] = append(titles[col.GetStatus()], issue.GetTitle())
		}
	}
	return order, titles
}

func TestBoardHandlersBindVerbLayer(t *testing.T) {
	client, st := boardClient(t)
	parent := createBoardCard(t, st, "parent", model.StatusTodo, nil)
	createBoardCard(t, st, "child", model.StatusDone, &parent)
	createBoardCard(t, st, "loner", model.StatusReview, nil)

	res, err := client.Board(context.Background(), boardRequest(&docketv1.BoardRequest{}))

	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("Board answered unimplemented: %v", err)
	}
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	order, titles := boardColumns(res.Msg)
	wantOrder := []string{"backlog", "todo", "in-progress", "review", "done"}
	if len(order) != len(wantOrder) {
		t.Fatalf("columns = %v, want %v", order, wantOrder)
	}
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Fatalf("columns = %v, want %v", order, wantOrder)
		}
	}
	if got := titles["todo"]; len(got) != 1 || got[0] != "parent" {
		t.Errorf("todo column = %v, want [parent]", got)
	}
	if got := titles["review"]; len(got) != 1 || got[0] != "loner" {
		t.Errorf("review column = %v, want [loner]", got)
	}
	if got := titles["done"]; len(got) != 0 {
		t.Errorf("done column = %v, want the child rolled up into its parent", got)
	}
}

func TestBoardExpandShowsSubIssuesAndWithBodyCarriesDescriptions(t *testing.T) {
	client, st := boardClient(t)
	parent := createBoardCard(t, st, "parent", model.StatusTodo, nil)
	createBoardCard(t, st, "child", model.StatusDone, &parent)

	summary, err := client.Board(context.Background(), boardRequest(&docketv1.BoardRequest{Expand: true}))
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	full, err := client.Board(context.Background(), boardRequest(&docketv1.BoardRequest{Expand: true, WithBody: true}))
	if err != nil {
		t.Fatalf("Board with_body: %v", err)
	}

	var child *docketv1.Issue
	for _, col := range summary.Msg.GetColumns() {
		for _, issue := range col.GetIssues() {
			if issue.GetDescription() != "" {
				t.Errorf("%s carries a description without with_body", issue.GetTitle())
			}
			if issue.GetTitle() == "child" {
				child = issue
			}
		}
	}
	if child == nil {
		t.Fatal("expand left the child off the board")
	}
	if child.GetParentId() != model.FormatID(parent) {
		t.Errorf("child parent_id = %q, want %q", child.GetParentId(), model.FormatID(parent))
	}
	for _, col := range full.Msg.GetColumns() {
		for _, issue := range col.GetIssues() {
			if want := issue.GetTitle() + " body"; issue.GetDescription() != want {
				t.Errorf("%s description = %q, want %q", issue.GetTitle(), issue.GetDescription(), want)
			}
		}
	}
}

// Every status shows on the board and the verb filters no issue kind, so a
// status or type filter is refused rather than dropped.
func TestBoardRefusesFilterFieldsTheVerbCannotApply(t *testing.T) {
	client, _ := boardClient(t)
	for name, filter := range map[string]*docketv1.IssueFilter{
		"status": {Status: []string{"todo"}},
		"type":   {Type: []string{"bug"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Board(context.Background(), boardRequest(&docketv1.BoardRequest{Filter: filter}))

			if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
				t.Errorf("code = %q, want %q (err %v)", got, output.ErrValidation, err)
			}
		})
	}
}
