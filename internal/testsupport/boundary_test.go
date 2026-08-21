package testsupport

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAbsentFromProductionBinary is a source-level guard for the invariant
// stated in this package's doc comment: testsupport is a test-only helper
// and must never reach the shipped binary's dependency graph.
//
// testsupport is currently the only non-test package in the module that
// imports "testing" — a package that registers command-line flags in
// init(). That has no effect today because nothing in cmd/docket's
// dependency graph reaches it, but nothing structural stops a future
// non-test file from importing testsupport for convenience and quietly
// pulling testing into production. Six issues are about to make this
// package universally imported across the test corpus, which is exactly
// the condition under which an accidental production import is most
// likely to go unnoticed. This guard makes the boundary explicit rather
// than relying on reviewers to keep noticing it.
func TestAbsentFromProductionBinary(t *testing.T) {
	goBin := FindGo(t)
	root := RepoRoot(t)

	deps := listDeps(t, goBin, root, "./cmd/docket")

	// Positive control first: prove the query can report a dependency at
	// all, so a query that silently stopped reporting cannot pass this
	// guard vacuously. internal/db is a real, always-present dependency of
	// cmd/docket.
	if !deps["github.com/ALT-F4-LLC/docket/internal/db"] {
		t.Fatal("control package internal/db not found in cmd/docket's dependency graph; the query proves nothing")
	}

	if deps["github.com/ALT-F4-LLC/docket/internal/testsupport"] {
		t.Fatal("internal/testsupport is reachable from cmd/docket — a test-only helper has entered the production binary")
	}
	if deps["testing"] {
		t.Fatal("the standard library \"testing\" package is reachable from cmd/docket — it registers flags in init() and must stay test-only")
	}
}

// listDeps runs `go list -deps` and returns the set of import paths in the
// target's full dependency graph.
func listDeps(t *testing.T, goBin, root string, args ...string) map[string]bool {
	t.Helper()

	full := append([]string{"list", "-deps"}, args...)
	cmd := exec.Command(goBin, full...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v", strings.Join(args, " "), err)
	}

	deps := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			deps[line] = true
		}
	}
	return deps
}
