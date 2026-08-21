package schema

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestNoCgoInTheModuleGraph is §4.1's binding constraint, mechanized.
//
// CGO STAYS OFF. That is a repo fact, not a preference: the module already
// takes `modernc.org/sqlite` — a pure-Go SQLite — precisely so the binary
// cross-compiles and the Vorpal build stays hermetic. A JSON Schema validator
// that pulled in cgo would undo that for a feature that needs none of it, and
// §4.1 says the choice is VOID if it does.
//
// THE SUBJECT IS THE MODULE GRAPH, NOT THE STANDARD LIBRARY, and the distinction
// is the whole guard. `net`, `os/user`, and `runtime/cgo` carry cgo sources on
// most platforms and always have; they are what `CGO_ENABLED=0` swaps for pure-Go
// implementations, which is exactly the hermetic build this constraint protects.
// Reporting them would be reporting the thing that already works. A DEPENDENCY
// bearing cgo is the failure — it has no pure-Go fallback to swap in.
//
// §4.1 says as much in its own words: "no package sets CGO_ENABLED-dependent
// build constraints BEYOND WHAT v8 ALREADY HAD".
//
// The child runs with CGO_ENABLED=1 FORCED, so the answer does not depend on the
// environment CI happens to set: with cgo disabled every package reports zero cgo
// files and the check would pass vacuously. The positive control below proves the
// query can report a non-zero answer at all.
func TestNoCgoInTheModuleGraph(t *testing.T) {
	goBin := testsupport.FindGo(t)
	root := testsupport.RepoRoot(t)

	// Positive control first: a package that is cgo BY DEFINITION must report
	// cgo files under this query. Without it, a query that silently stopped
	// reporting would turn this guard green forever.
	if control := listCgoFiles(t, goBin, root, "runtime/cgo"); control["runtime/cgo"] == 0 {
		t.Fatal("the control package reports no cgo files; the query proves nothing")
	}

	counts := listCgoFiles(t, goBin, root, "-deps", "./cmd/docket")
	if len(counts) < 50 {
		t.Fatalf("only %d packages in the binary's dependency graph; the query failed", len(counts))
	}

	// The stdlib count is REPORTED rather than asserted, because it is
	// platform-dependent: on linux `net`, `os/user`, and `runtime/cgo` carry cgo
	// sources and on darwin they do not. Asserting it either way would make this
	// guard fail on a platform for a reason that has nothing to do with the
	// constraint. The runtime/cgo control above is what proves the query is live.
	stdlibWithCgo := 0
	var offenders []string
	for pkg, n := range counts {
		if n == 0 {
			continue
		}
		if isModulePackage(pkg) {
			offenders = append(offenders, pkg)
			continue
		}
		stdlibWithCgo++
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("cgo entered the MODULE GRAPH via: %s\n"+
			"§4.1: a validator that introduces a cgo-bearing transitive dependency "+
			"VOIDS the choice — the constraint decides, not the shortlist.",
			strings.Join(offenders, ", "))
	}
	t.Logf("%d dependency packages, none cgo-bearing; %d stdlib packages carry "+
		"cgo sources and are what CGO_ENABLED=0 replaces", len(counts), stdlibWithCgo)
}

// isModulePackage reports whether an import path names a package from a MODULE
// rather than from the standard library.
//
// The rule is the toolchain's own: a standard-library import path's first
// element never contains a dot, because it is not a domain.
func isModulePackage(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return strings.Contains(first, ".")
}

// listCgoFiles runs `go list` with the given arguments and returns each
// package's cgo-file count.
func listCgoFiles(t *testing.T, goBin, root string, args ...string) map[string]int {
	t.Helper()

	full := append([]string{"list", "-f", "{{.ImportPath}} {{len .CgoFiles}}"}, args...)
	cmd := exec.Command(goBin, full...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.Output()
	testsupport.Must(t, err, "go list %s: %v", strings.Join(args, " "), err)

	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, count, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		n := 0
		for _, r := range count {
			n = n*10 + int(r-'0')
		}
		counts[pkg] = n
	}
	return counts
}

// rankVocabulary matches the names a ranking API would plausibly take.
var rankVocabulary = regexp.MustCompile(
	`(?i)^(less|greater|compare|cmp|rank|precedes|before|after|higher|lower|indexof|order(of)?|worse|severer)$`)

// TestNoComparisonAPIBypassesPosition is §4.10's source-level guard, in the
// family of gates-trust §5.1's no-shell check.
//
// Two assertions, both about the same rule — ORDER COMES FROM Position OR NOT
// AT ALL:
//
//  1. internal/schema declares no ranking function other than Position. A
//     `Less(a, b)` could be called on a field with no declared order, and would
//     have to answer something; Position's second result is how it declines.
//
//  2. internal/engine never reads a declared enum's VALUES. It asks Position
//     for a rank. An engine that reached into `Field.Enum` and compared indices
//     itself would be re-implementing the ordering rule outside the one place
//     the rule is written, and I4's no-fallback clause would be one careless
//     `slices.Index` away from being untrue.
//
// Comments are stripped before the source is read, because the prose that
// explains a rule has to name what it forbids (the same decision
// scripts/qa/genericity.sh makes, for the same reason).
func TestNoComparisonAPIBypassesPosition(t *testing.T) {
	root := testsupport.RepoRoot(t)

	t.Run("internal/schema declares no second ranking API", func(t *testing.T) {
		var offenders []string
		forEachDecl(t, filepath.Join(root, "internal", "schema"), func(file string, fn *ast.FuncDecl) {
			if rankVocabulary.MatchString(fn.Name.Name) {
				offenders = append(offenders, file+": "+fn.Name.Name)
			}
		})
		if len(offenders) > 0 {
			t.Errorf("a second ordering API exists alongside Position:\n  %s\n"+
				"I3: Position is the ONLY ordering API, so that comparing two values "+
				"of a field with no declared order is unrepresentable rather than "+
				"merely discouraged.", strings.Join(offenders, "\n  "))
		}
	})

	t.Run("internal/engine never reads a declared order's values", func(t *testing.T) {
		var offenders []string
		forEachFile(t, filepath.Join(root, "internal", "engine"), func(path string, fset *token.FileSet, file *ast.File) {
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// `Enum` alone: it is the accessor that hands out a declared
				// order's VALUES. Widening this to every plausible accessor name
				// would catch `strings.Fields` and train a reader to ignore the
				// guard, which costs more than it buys.
				if sel.Sel.Name == "Enum" {
					offenders = append(offenders, render(fset, path, sel))
				}
				return true
			})
		})
		if len(offenders) > 0 {
			t.Errorf("the engine reads a declared order's values directly:\n  %s\n"+
				"Every comparison goes through Position (I3); nothing else may "+
				"decide what precedes what.", strings.Join(offenders, "\n  "))
		}
	})
}

// forEachDecl visits every top-level function and method in a package's
// non-test sources, with comments stripped.
func forEachDecl(t *testing.T, dir string, visit func(file string, fn *ast.FuncDecl)) {
	t.Helper()
	forEachFile(t, dir, func(path string, _ *token.FileSet, file *ast.File) {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				visit(filepath.Base(path), fn)
			}
		}
	})
}

// forEachFile parses a package's non-test sources WITHOUT comments, so a guard
// never fires on the doc comment that states the rule it enforces.
func forEachFile(t *testing.T, dir string, visit func(path string, fset *token.FileSet, file *ast.File)) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	testsupport.Must(t, err, "reading %s: %v", dir, err)

	seen := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		testsupport.Must(t, err, "parsing %s: %v", name, err)
		file.Comments = nil
		seen++
		visit(name, fset, file)
	}
	// A guard that read nothing passes vacuously.
	if seen == 0 {
		t.Fatalf("no sources read from %s; the guard would pass vacuously", dir)
	}
}

// render prints one node for a guard's failure message.
func render(fset *token.FileSet, path string, node ast.Node) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, node); err != nil {
		return path
	}
	return path + ": " + b.String()
}
