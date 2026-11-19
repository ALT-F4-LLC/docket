package api

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

func openImportStore(t *testing.T) store.Store {
	t.Helper()
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
	return st
}

// exportedIssueDocument returns the JSON export of a store holding one issue
// titled title in the default project.
func exportedIssueDocument(t *testing.T, title string) string {
	t.Helper()
	src := openImportStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, _, err := src.CreateIssueIdempotent(&model.Issue{
		ProjectID: store.DefaultProjectID, Title: title,
		Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
		CreatedAt: now, UpdatedAt: now,
	}, nil, nil, ""); err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	exported, err := verb.Export(context.Background(), verb.Env{Store: src}, verb.ExportInput{Format: "json"})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	return exported.Document
}

func TestImportHandlersBindVerbLayer(t *testing.T) {
	const title = "imported over connect"
	document := exportedIssueDocument(t, title)
	dst := openImportStore(t)
	server := serve(t, NewService(dst, "dev"))
	client := docketv1connect.NewDocketServiceClient(server.Client(), server.URL)
	req := connect.NewRequest(&docketv1.ImportRequest{Document: document})
	req.Header().Set("Docket-Author", authorSentinel)
	req.Header().Set("Docket-Cwd", cwdSentinel)

	_, err := client.Import(context.Background(), req)

	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("Import answered %v: no *Service method serves it", err)
	}
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	issues, err := dst.ListAllIssues(store.DefaultProjectID)
	if err != nil {
		t.Fatalf("ListAllIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].Title != title {
		t.Errorf("default project issues = %v, want the one imported issue %q", issueTitlesOf(issues), title)
	}
}

func issueTitlesOf(issues []*model.Issue) []string {
	titles := make([]string, 0, len(issues))
	for _, issue := range issues {
		titles = append(titles, issue.Title)
	}
	return titles
}
