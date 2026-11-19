package testsupport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"testing"
)

// TestLayerImportDirection holds the import direction of the verb layer
// (docs/design/connect-verb-layer.md, "Import direction"): internal/cli and
// internal/api both sit on internal/verb, verb reaches neither of them nor any
// transport or database/sql, and internal/cli never enters the dependency
// graph of internal/api or internal/verb. Only direct, non-test imports count
// for the edge rules; the cycle-freedom rule reads the full graph.
func TestLayerImportDirection(t *testing.T) {
	const (
		module   = "github.com/ALT-F4-LLC/docket"
		verbPkg  = module + "/internal/verb"
		apiPkg   = module + "/internal/api"
		cliPkg   = module + "/internal/cli"
		storePkg = module + "/internal/store"
	)

	goBin := FindGo(t)
	root := RepoRoot(t)
	pkgs := listPackagesTolerant(t, goBin, root, "./internal/...")

	// Positive control: a known edge must be visible, or the query cannot
	// report a forbidden one either.
	controlFound := false
	for _, p := range pkgs {
		if p.ImportPath == apiPkg {
			controlFound = slices.Contains(p.Imports, storePkg)
		}
	}
	if !controlFound {
		t.Fatal("control import internal/api -> internal/store not reported by go list; the query proves nothing")
	}

	for _, p := range pkgs {
		for _, imp := range p.Imports {
			switch {
			case isPackageOrSub(p.ImportPath, verbPkg) && forbiddenForVerb(imp, apiPkg, cliPkg):
				t.Errorf("%s imports %s; internal/verb must not depend on the CLI, the API, a transport or database/sql", p.ImportPath, imp)
			case isPackageOrSub(p.ImportPath, apiPkg) && isPackageOrSub(imp, cliPkg):
				t.Errorf("%s imports %s; internal/api must never import internal/cli", p.ImportPath, imp)
			case isPackageOrSub(p.ImportPath, cliPkg) && imp == apiPkg:
				t.Errorf("%s imports %s; internal/cli reaches the daemon through internal/api/client, never the internal/api package", p.ImportPath, imp)
			}
		}
		if p.Error != nil {
			t.Errorf("go list could not load %s: %s", p.ImportPath, p.Error.Err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	deps := listDeps(t, goBin, root, "./internal/api", "./internal/verb")
	if !deps[storePkg] {
		t.Fatal("control package internal/store not found in the dependency graph of internal/api and internal/verb; the query proves nothing")
	}
	for dep := range deps {
		if isPackageOrSub(dep, cliPkg) {
			t.Errorf("%s is in the transitive imports of internal/api or internal/verb; the layers must stay free of the CLI", dep)
		}
	}
}

func forbiddenForVerb(imp, apiPkg, cliPkg string) bool {
	return isPackageOrSub(imp, cliPkg) ||
		isPackageOrSub(imp, apiPkg) ||
		isPackageOrSub(imp, "github.com/spf13/cobra") ||
		isPackageOrSub(imp, "connectrpc.com/connect") ||
		imp == "database/sql"
}

type listedPackage struct {
	ImportPath string
	Imports    []string
	Error      *struct{ Err string }
}

// listPackagesTolerant runs `go list -e` so a package that fails to load, as
// one does when a forbidden edge closes an import cycle, is still reported
// with its direct non-test imports and its load error.
func listPackagesTolerant(t *testing.T, goBin, root string, patterns ...string) []listedPackage {
	t.Helper()

	args := append([]string{"list", "-e", "-json=ImportPath,Imports,Error"}, patterns...)
	cmd := exec.Command(goBin, args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -e %v: %v", patterns, err)
	}

	var pkgs []listedPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}
