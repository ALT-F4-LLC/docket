package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain pins DOCKET_PATH for the whole package. THE REPO DOGFOODS DOCKET:
// its own shipped `.docket/config/` sits at the worktree root, and a test that
// let config.Resolve fall through to global resolution would scan those real
// workflows and schemas into whatever activation it happened to run. One
// ambient env var makes every test hermetic without each DB helper knowing
// why; a test that needs a different store still wins with t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "docket-engine-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("DOCKET_PATH", filepath.Join(dir, ".docket"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
