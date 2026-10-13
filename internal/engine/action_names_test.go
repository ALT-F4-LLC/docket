package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// setInstanceConfig points config.Resolve at a fresh, isolated store for one
// test — DOCKET_PATH names the store directly (SourceEnv), so
// InstanceConfigDirs() resolves to exactly the one root this test wrote.
func setInstanceConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store := filepath.Join(dir, ".docket")
	t.Setenv("DOCKET_PATH", store)
	root := filepath.Join(store, "config", "workflows")
	err := os.MkdirAll(root, 0o755)
	testsupport.Must(t, err, "mkdir %s: %v", root, err)
	return root
}

func writeWorkflowFile(t *testing.T, dir, name, body string) {
	t.Helper()
	err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	testsupport.Must(t, err, "writing %s: %v", name, err)
}

// TestActionNamesCollectsDeclaredActionsOnly is DKT-1283 AC5's classification
// source: a step declaring `action = "<name>"` names an engine action; a step
// declaring `executor` (and every gate name it lists) is not one, no matter
// what it is named.
func TestActionNamesCollectsDeclaredActionsOnly(t *testing.T) {
	dir := setInstanceConfig(t)
	writeWorkflowFile(t, dir, "sample.toml", `
[pipeline]
name = "sample"
version = 1

[[step]]
name = "aggregate-findings"
after = []
action = "aggregate"
emits = "k"

[[step]]
name = "build"
after = []
executor = "write"
emits = "k"
`)

	names, err := ActionNames()
	testsupport.Must(t, err, "ActionNames: %v", err)

	if !names["aggregate"] {
		t.Errorf("aggregate: want declared as an action, got %v", names)
	}
	if names["build"] {
		t.Errorf("build: an executor step's NAME must not classify as an action, got %v", names)
	}
	if names["aggregate-findings"] {
		t.Errorf("aggregate-findings: the STEP name is not the action name, got %v", names)
	}
}

// TestActionNamesUnionsRoots: the corpus and a repo's own additions are BOTH
// scanned (mirroring scanConfigDirs' union), and a file that fails to parse
// is skipped rather than aborting the whole classification.
func TestActionNamesSkipsUnparseableFiles(t *testing.T) {
	dir := setInstanceConfig(t)
	writeWorkflowFile(t, dir, "broken.toml", "this is not [valid toml")
	writeWorkflowFile(t, dir, "ok.toml", `
[pipeline]
name = "ok"
version = 1

[[step]]
name = "s"
after = []
action = "reconcile"
emits = "k"
`)

	names, err := ActionNames()
	testsupport.Must(t, err, "ActionNames: %v", err)
	if !names["reconcile"] {
		t.Errorf("a broken sibling file must not stop the readable one from classifying: %v", names)
	}
}

// TestActionNamesEmptyWithNoRoot is F17 dormancy: no instance-config root at
// all returns an empty set, not an error.
func TestActionNamesEmptyWithNoRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKET_PATH", filepath.Join(dir, "nonexistent", ".docket"))

	names, err := ActionNames()
	testsupport.Must(t, err, "ActionNames: %v", err)
	if len(names) != 0 {
		t.Errorf("want an empty set with no instance-config root, got %v", names)
	}
}
