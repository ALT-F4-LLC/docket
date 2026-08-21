package testsupport

import (
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// FindGo locates a go toolchain, preferring PATH and falling back to GOROOT.
//
// Exported for source-level guards that shell out to `go list`/`go build` to
// inspect the module graph (e.g. internal/testsupport's own
// TestAbsentFromProductionBinary and internal/schema's
// TestNoCgoInTheModuleGraph) — one lookup shared by every such guard.
func FindGo(t *testing.T) string {
	t.Helper()
	if path, err := exec.LookPath("go"); err == nil {
		return path
	}
	candidate := filepath.Join(build.Default.GOROOT, "bin", "go")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	// A guard that cannot run has not passed.
	t.Fatal("no go toolchain found; this guard cannot run")
	return ""
}

// RepoRoot walks up from the package directory to the module root.
//
// Exported for the same reason as FindGo: any package's guard that needs the
// module root — to hand to `go list`, or to walk sources from — shares this
// one implementation.
func RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	Must(t, err, "locating the package directory: %v", err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}
