package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// exportClient serves a Service over a fresh, migrated SQLite store.
func exportClient(t *testing.T) docketv1connect.DocketServiceClient {
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
	server := serve(t, NewService(st, "dev"))
	return docketv1connect.NewDocketServiceClient(server.Client(), server.URL)
}

func exportRequest(msg *docketv1.ExportRequest) *connect.Request[docketv1.ExportRequest] {
	req := connect.NewRequest(msg)
	req.Header().Set(authorHeader, authorSentinel)
	req.Header().Set(cwdHeader, cwdSentinel)
	return req
}

func TestExportHandlersBindVerbLayer(t *testing.T) {
	client := exportClient(t)

	res, err := client.Export(context.Background(), exportRequest(&docketv1.ExportRequest{
		Format: "json",
		Filter: &docketv1.IssueFilter{Status: []string{"todo"}, Label: []string{"api"}},
	}))

	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("Export answered unimplemented: %v", err)
	}
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := res.Msg.GetFormat(); got != "json" {
		t.Errorf("format = %q, want json", got)
	}
	var document struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal([]byte(res.Msg.GetDocument()), &document); err != nil {
		t.Fatalf("document is not JSON: %v\n%s", err, res.Msg.GetDocument())
	}
	if document.Version != 1 {
		t.Errorf("document version = %d, want 1", document.Version)
	}
}

// The verb filters by status and label only; a filter field it cannot apply
// is refused rather than dropped, so the caller never receives issues it
// filtered out.
func TestExportRefusesFilterFieldsTheVerbCannotApply(t *testing.T) {
	client := exportClient(t)
	for name, filter := range map[string]*docketv1.IssueFilter{
		"priority": {Priority: []string{"high"}},
		"type":     {Type: []string{"bug"}},
		"size":     {Size: []string{"s"}},
		"assignee": {Assignee: "@alice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Export(context.Background(), exportRequest(&docketv1.ExportRequest{
				Format: "json", Filter: filter,
			}))

			if got := output.ErrorCodeFromConnectError(err); got != output.ErrValidation {
				t.Errorf("code = %q, want %q (err %v)", got, output.ErrValidation, err)
			}
		})
	}
}
