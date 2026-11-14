package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

const registryCorpusInvestigationV8 = `
[pipeline]
name = "investigation"
version = 8
[[step]]
name = "look"
after = []
executor = "w"
emits = "out"
`

// registryAuditStore returns a migrated store whose project stale.git
// registered investigation@4 and triage@1 and @2, under a store root whose
// shared config declares only investigation@8. The temp dirs are taken before t.Setenv, since t.TempDir
// reads TMPDIR.
func registryAuditStore(t *testing.T) store.Store {
	t.Helper()
	docketDir := filepath.Join(t.TempDir(), ".docket")
	path := filepath.Join(docketDir, "config", "workflows", "investigation.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(registryCorpusInvestigationV8), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	dbPath := filepath.Join(t.TempDir(), "issues.db")
	t.Setenv("DOCKET_PATH", docketDir)

	st, err := store.OpenSQLite(dbPath)
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
	projectID, err := st.EnsureProject("/repo/stale.git", "stale.git", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, reg := range []struct {
		name    string
		version int
	}{{"investigation", 4}, {"triage", 1}, {"triage", 2}} {
		body := "bytes for " + reg.name
		if _, _, err := st.InsertWorkflow(&model.Workflow{
			ProjectID: projectID, Name: reg.name, Version: reg.version,
			SourcePath: "/nowhere/" + reg.name + ".toml", SourceSHA256: workflow.SHA256([]byte(body)),
			Body: body, Parsed: "{}",
		}, 1); err != nil {
			t.Fatalf("registering %s@%d: %v", reg.name, reg.version, err)
		}
	}
	return st
}

// Each registry RPC is served by the verb layer: the call reaches the store
// and answers the verb's verdict, one drift row per behind name and per
// orphaned version, never unimplemented.
func TestRegistryHandlersBindVerbLayer(t *testing.T) {
	server := serve(t, NewService(registryAuditStore(t), "test"))
	rpc := client.NewClient(server.Client(), server.URL, discardLogger)

	res, err := rpc.RegistryAudit(context.Background(),
		connect.NewRequest(&docketv1.RegistryAuditRequest{Project: "stale.git"}))

	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("RegistryAudit answered unimplemented: %v", err)
	}
	if err != nil {
		t.Fatalf("RegistryAudit: %v", err)
	}
	want := []*docketv1.RegistryDrift{
		{Project: "stale.git", Kind: "workflow", Name: "investigation", Version: 4, Drift: "behind"},
		{Project: "stale.git", Kind: "workflow", Name: "triage", Version: 1, Drift: "orphaned"},
		{Project: "stale.git", Kind: "workflow", Name: "triage", Version: 2, Drift: "orphaned"},
	}
	got := res.Msg.GetDrift()
	if len(got) != len(want) {
		t.Fatalf("drift = %v, want %v", got, want)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("drift[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
