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

// TestStoreImportBoundary holds the persistence boundary: only internal/db
// (with its subpackages) and internal/store may import internal/db directly,
// and no package under internal/engine or internal/cli may import
// database/sql directly. Reaching either transitively through internal/store
// is the intended shape, so only each package's direct, non-test imports
// count.
func TestStoreImportBoundary(t *testing.T) {
	const (
		dbPkg     = "github.com/ALT-F4-LLC/docket/internal/db"
		storePkg  = "github.com/ALT-F4-LLC/docket/internal/store"
		enginePkg = "github.com/ALT-F4-LLC/docket/internal/engine"
		cliPkg    = "github.com/ALT-F4-LLC/docket/internal/cli"
	)

	pkgs := listDirectImports(t, FindGo(t), RepoRoot(t), "./internal/...", "./cmd/...")

	// Positive control: internal/store's direct import of internal/db must
	// be visible, or the query cannot report a violation either.
	storeFound := false
	for _, p := range pkgs {
		if p.path == storePkg {
			storeFound = p.imports[dbPkg]
		}
	}
	if !storeFound {
		t.Fatal("control import internal/store -> internal/db not reported by go list; the query proves nothing")
	}

	for _, p := range pkgs {
		if p.imports[dbPkg] && !isPackageOrSub(p.path, dbPkg) && p.path != storePkg {
			t.Errorf("%s imports %s directly; only internal/db and internal/store may", p.path, dbPkg)
		}
		if p.imports["database/sql"] && (isPackageOrSub(p.path, enginePkg) || isPackageOrSub(p.path, cliPkg)) {
			t.Errorf("%s imports database/sql directly; go through internal/store instead", p.path)
		}
	}
}

type packageImports struct {
	path    string
	imports map[string]bool
}

// listDirectImports runs `go list` and returns each matched package with the
// set of its direct imports from non-test files.
func listDirectImports(t *testing.T, goBin, root string, patterns ...string) []packageImports {
	t.Helper()

	args := append([]string{"list", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}"}, patterns...)
	cmd := exec.Command(goBin, args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s: %v", strings.Join(patterns, " "), err)
	}

	var pkgs []packageImports
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		p := packageImports{path: fields[0], imports: make(map[string]bool)}
		for _, imp := range fields[1:] {
			p.imports[imp] = true
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func isPackageOrSub(pkg, root string) bool {
	return pkg == root || strings.HasPrefix(pkg, root+"/")
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
