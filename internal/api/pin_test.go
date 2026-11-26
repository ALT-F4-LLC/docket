package api

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/store/storetest"
)

const pinShowWorkflow = `
[pipeline]
name = "api-pin-show"
version = 1
[match]
kind = ["task"]
[[step]]
name = "judge"
after = []
executor = "judge"
emits = "change-summary"
packet = ["contracts/judge.md"]
`

const pinShowPolicy = "opaque = \"instance policy\"\n"

// pinShowStore returns a migrated store holding one run activated over a
// config tree that carries policy.toml, and that run's ref. The temp dirs are
// taken before t.Setenv, since t.TempDir reads TMPDIR.
func pinShowStore(t *testing.T) (store.Store, string) {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), ".docket", "config")
	dbPath := filepath.Join(t.TempDir(), "issues.db")
	for path, body := range map[string]string{
		"workflows/api-pin-show.toml": pinShowWorkflow,
		"contracts/judge.md":          "the judge contract\n",
		"policy.toml":                 pinShowPolicy,
	} {
		full := filepath.Join(configDir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", full, err)
		}
	}
	t.Setenv("DOCKET_PATH", filepath.Dir(configDir))

	storetest.SeedFile(t, dbPath)
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
	issue, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title: "pinned", Status: model.StatusTodo,
		Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	run, _, err := st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{}, "", issue)
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	if _, err := engine.ActivateStore(st, run.ID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}
	return st, run.Ref()
}

// Each pin RPC is served by the verb layer: the call reaches the store and
// answers the bytes the run pinned with their hash, never unimplemented.
func TestPinHandlersBindVerbLayer(t *testing.T) {
	st, runRef := pinShowStore(t)
	server := serve(t, NewService(st, "test"))
	rpc := client.NewClient(server.Client(), server.URL, discardLogger)

	res, err := rpc.PinShow(context.Background(),
		connect.NewRequest(&docketv1.PinShowRequest{Run: runRef, Path: "policy.toml"}))

	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("PinShow answered unimplemented: %v", err)
	}
	if err != nil {
		t.Fatalf("PinShow: %v", err)
	}
	if got := res.Msg.GetPath(); got != "policy.toml" {
		t.Errorf("path = %q, want policy.toml", got)
	}
	if got := res.Msg.GetContent(); !bytes.Equal(got, []byte(pinShowPolicy)) {
		t.Errorf("content = %q, want the pinned %q", got, pinShowPolicy)
	}
	// The oracle is the pin row activation wrote, not a hash of the answer.
	runID, err := model.ParseRunID(runRef)
	if err != nil {
		t.Fatalf("ParseRunID: %v", err)
	}
	pins, err := st.ListPins(runID)
	if err != nil {
		t.Fatalf("ListPins: %v", err)
	}
	var pinned string
	for _, p := range pins {
		if filepath.Base(p.Ref) == "policy.toml" {
			pinned = p.SHA256
		}
	}
	if pinned == "" {
		t.Fatalf("activation pinned no policy.toml among %d pins", len(pins))
	}
	if got := res.Msg.GetSha256(); got != pinned {
		t.Errorf("sha256 = %q, want the pinned %q", got, pinned)
	}
}
