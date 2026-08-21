package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain pins DOCKET_PATH for the whole package, for internal/engine's
// TestMain reason: the repo dogfoods docket, so unpinned resolution would find
// the repository's own shipped `.docket/config/` (or the operator's real
// ~/.docket) from inside a test. A test that needs a different store still
// wins with t.Setenv.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "docket-cli-test-*")
	if err != nil {
		panic(err)
	}
	os.Setenv("DOCKET_PATH", filepath.Join(dir, ".docket"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
