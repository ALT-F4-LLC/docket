package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestTrustListV2CarriesClass is DKT-1283 AC5: under --json=v2 every entry
// carries `class`, computed from what the workflow corpus declares — a name
// some workflow declares `action = "<name>"` classifies "action", everything
// else "gate" — so a caller no longer greps the TOMLs itself.
func TestTrustListV2CarriesClass(t *testing.T) {
	_, cfg := trustNoRepo(t)

	// DOCKET_PATH is already redirected by trustNoRepo's isolation (via
	// t.Setenv upstream in the XDG/HOME redirect); point it at a scratch store
	// so ActionNames() reads THIS test's corpus, never the repo's own.
	store := filepath.Join(t.TempDir(), ".docket")
	t.Setenv("DOCKET_PATH", store)
	workflows := filepath.Join(store, "config", "workflows")
	err := os.MkdirAll(workflows, 0o755)
	testsupport.Must(t, err, "mkdir %s: %v", workflows, err)
	err = os.WriteFile(filepath.Join(workflows, "sample.toml"), []byte(`
[pipeline]
name = "sample"
version = 1

[[step]]
name = "aggregate-findings"
after = []
action = "aggregate"
emits = "k"
`), 0o644)
	testsupport.Must(t, err, "writing sample.toml: %v", err)

	addTrustEntry(t, cfg, "aggregate", "true")
	addTrustEntry(t, cfg, "lint", "true")

	cmd := newTrustListCmd()
	jsonCmdWithVersion(t, cmd, "v2")

	stdout := captureStdout(t)
	err = runTrustVerb(t, cfg, cmd)
	out := stdout()
	testsupport.Must(t, err, "trust list: %v", err)

	var envelope struct {
		Data struct {
			Items []struct {
				Name  string `json:"name"`
				Class string `json:"class"`
			} `json:"items"`
		} `json:"data"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &envelope); jsonErr != nil {
		t.Fatalf("trust list's v2 JSON did not parse: %v\n%s", jsonErr, out)
	}

	classes := map[string]string{}
	for _, e := range envelope.Data.Items {
		classes[e.Name] = e.Class
	}
	if classes["aggregate"] != "action" {
		t.Errorf(`aggregate: class = %q, want "action"`, classes["aggregate"])
	}
	if classes["lint"] != "gate" {
		t.Errorf(`lint: class = %q, want "gate"`, classes["lint"])
	}
}

// TestTrustListV1OmitsClass pins the frozen v1 shape: adding the
// classification must not put a `class` byte anywhere in v1 output.
func TestTrustListV1OmitsClass(t *testing.T) {
	_, cfg := trustNoRepo(t)
	addTrustEntry(t, cfg, "lint", "true")

	cmd := newTrustListCmd()
	jsonCmdWithVersion(t, cmd, "v1")

	stdout := captureStdout(t)
	err := runTrustVerb(t, cfg, cmd)
	out := stdout()
	testsupport.Must(t, err, "trust list: %v", err)

	var envelope struct {
		Data struct {
			Entries []map[string]any `json:"entries"`
		} `json:"data"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &envelope); jsonErr != nil {
		t.Fatalf("trust list's v1 JSON did not parse: %v\n%s", jsonErr, out)
	}
	if len(envelope.Data.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(envelope.Data.Entries))
	}
	if _, has := envelope.Data.Entries[0]["class"]; has {
		t.Error("v1's frozen shape must not carry `class`")
	}
}
