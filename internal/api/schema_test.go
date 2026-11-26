package api

import (
	"context"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/store/storetest"
)

const schemaCallerIdentity = "/repos/schema-caller"

// schemaService serves a Service over a fresh store holding one registered,
// non-default project, and returns a client, the store and that project's id.
// The project is not the default one, so a handler that dropped the
// Docket-Project header would write somewhere the test does not look.
func schemaService(t *testing.T) (docketv1connect.DocketServiceClient, store.Store, int) {
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
	if _, err := st.EnsureProject("/repos/default", "default", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	projectID, err := st.EnsureProject(schemaCallerIdentity, "schema-caller", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	server := serve(t, NewService(st, "dev"))
	return client.NewClient(server.Client(), server.URL, discardLogger), st, projectID
}

// inCallerProject addresses req to the test's registered project.
func inCallerProject[T any](msg *T) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Docket-Project", schemaCallerIdentity)
	return req
}

// requireSchemaServed fails when rpc answered unimplemented, the answer of an RPC
// no Service method serves, and on any other error.
func requireSchemaServed(t *testing.T, rpc string, err error) {
	t.Helper()
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("%s answered unimplemented: no *Service method serves it (%v)", rpc, err)
	}
	if err != nil {
		t.Fatalf("%s: %v", rpc, err)
	}
}

func TestSchemaHandlersBindVerbLayer(t *testing.T) {
	ctx := context.Background()
	rpc, st, projectID := schemaService(t)
	const body = `{"type":"object"}`

	registered, err := rpc.SchemaRegister(ctx, inCallerProject(&docketv1.SchemaRegisterRequest{
		Name: "findings", Version: 1, Body: body, SourcePath: "findings.json",
	}))
	requireSchemaServed(t, "SchemaRegister", err)
	got := registered.Msg.GetSchema()
	if got.GetName() != "findings" || got.GetVersion() != 1 || got.GetBody() != body || got.GetSourcePath() != "findings.json" {
		t.Errorf("SchemaRegister schema = %v, want findings@1 with the sent body and source_path", got)
	}
	stored, err := st.GetSchema(projectID, "findings", 1)
	if err != nil {
		t.Fatalf("findings@1 is not in the Docket-Project project: %v", err)
	}
	if got.GetSourceSha256() != stored.SourceSHA256 || got.GetSourceSha256() == "" {
		t.Errorf("source_sha256 = %q, want the stored %q", got.GetSourceSha256(), stored.SourceSHA256)
	}

	listed, err := rpc.SchemaList(ctx, inCallerProject(&docketv1.SchemaListRequest{Name: "findings"}))
	requireSchemaServed(t, "SchemaList", err)
	if items := listed.Msg.GetItems(); len(items) != 1 || items[0].GetName() != "findings" || listed.Msg.GetTotal() != 1 || listed.Msg.GetTruncated() {
		t.Errorf("SchemaList = %v, want findings@1 alone, total 1, not truncated", listed.Msg)
	}

	shown, err := rpc.SchemaShow(ctx, inCallerProject(&docketv1.SchemaShowRequest{Name: "findings"}))
	requireSchemaServed(t, "SchemaShow", err)
	if s := shown.Msg.GetSchema(); s.GetVersion() != 1 || s.GetBody() != body || s.GetDeprecated() {
		t.Errorf("SchemaShow findings = %v, want version 1 in service with the registered body", s)
	}

	deprecated, err := rpc.SchemaDeprecate(ctx, inCallerProject(&docketv1.SchemaDeprecateRequest{Name: "findings", Version: 1}))
	requireSchemaServed(t, "SchemaDeprecate", err)
	if !deprecated.Msg.GetSchema().GetDeprecated() {
		t.Errorf("SchemaDeprecate schema = %v, want it retired", deprecated.Msg.GetSchema())
	}

	version := int32(1)
	_, err = rpc.SchemaShow(ctx, inCallerProject(&docketv1.SchemaShowRequest{Name: "missing", Version: &version}))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrNotFound {
		t.Errorf("SchemaShow missing@1 code = %q, want %q: the verb's error code reaches the client (err %v)", got, output.ErrNotFound, err)
	}
}
