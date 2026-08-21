package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The single-connection discipline.
//
// internal/db opens the database with SetMaxOpenConns(1). That is deliberate —
// SQLite serializes writers anyway, and one connection makes the serialization
// explicit rather than emergent — but it has a consequence that is invisible
// until it bites:
//
//	A POOL QUERY ISSUED WHILE A TRANSACTION IS OPEN DEADLOCKS PERMANENTLY.
//
// The transaction holds the only connection; the pool read waits for a
// connection that cannot be released until the transaction commits; the
// transaction cannot commit until the read returns. Nothing times out and
// nothing errors — the process hangs, which in CI reads as a stuck job rather
// than a failed test.
//
// This phase hit it once for real: `step claim` resolved its lease TTL through
// a `*sql.DB` helper from inside its own transaction. The fix was to resolve
// the TTL from config loaded BEFORE the transaction opened.
//
// The rule, therefore: a function that opens a transaction must not call
// anything that reads the pool while it is open. Everything a transaction needs
// is either loaded before it opens or read through its own *sql.Tx.

// TestNoPoolReadsInsideTransactions is a source-level guard for that rule.
//
// It is a static check rather than a runtime one because the runtime symptom is
// a HANG: a test that exercised the bad path would have to be killed by a
// timeout, and a timeout failure does not say what went wrong. Reading the AST
// says exactly which function and which call.
//
// The check: within a function, the region between a `conn.Begin()` and the
// matching `Commit()` is a LIVE TRANSACTION, and no call inside that region may
// touch the pool — neither directly (`conn.Query`) nor by passing `conn` to a
// helper that might.
//
// Tracking the commit matters, and getting it wrong makes the guard useless in
// the noisy direction: `runGateStage` legitimately commits, runs a gate OUTSIDE
// any transaction (which is §6's whole point), and opens a second transaction;
// `Activate` legitimately re-reads through the pool after committing. A
// position-only check flags both and gets itself disabled.
func TestNoPoolReadsInsideTransactions(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	testsupport.Must(t, err, "parsing the package: %v", err)

	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}
				checkFunc(t, fset, path, fn)
				return true
			})
		}
	}
}

// liveRegion is a span in which a transaction is open: from a `conn.Begin()`
// to the `Commit()` that closes it, or to the end of the function when nothing
// commits (the read-only paths, which rely on `defer tx.Rollback()`).
type liveRegion struct{ from, to token.Pos }

// checkFunc reports a pool-taking call inside a live-transaction region.
func checkFunc(t *testing.T, fset *token.FileSet, path string, fn *ast.FuncDecl) {
	t.Helper()

	// Collect the Begin and Commit positions in source order.
	var begins, commits []token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case ident.Name == "conn" && sel.Sel.Name == "Begin":
			begins = append(begins, call.Pos())
		case strings.HasPrefix(ident.Name, "tx") && sel.Sel.Name == "Commit":
			commits = append(commits, call.Pos())
		}
		return true
	})

	if len(begins) == 0 {
		return // No transaction in this function.
	}

	sort.Slice(begins, func(i, j int) bool { return begins[i] < begins[j] })
	sort.Slice(commits, func(i, j int) bool { return commits[i] < commits[j] })

	// Pair each Begin with the first Commit after it. A Begin with no following
	// Commit stays live to the end of the function.
	var regions []liveRegion
	for _, begin := range begins {
		region := liveRegion{from: begin, to: fn.Body.End()}
		for _, commit := range commits {
			if commit > begin {
				region.to = commit
				break
			}
		}
		regions = append(regions, region)
	}

	inLiveRegion := func(pos token.Pos) bool {
		for _, r := range regions {
			if pos > r.from && pos < r.to {
				return true
			}
		}
		return false
	}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !inLiveRegion(call.Pos()) {
			return true
		}

		// A direct pool read: conn.Query / conn.QueryRow / conn.Exec.
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "conn" {
				switch sel.Sel.Name {
				case "Query", "QueryRow", "Exec":
					t.Errorf("%s: %s calls conn.%s AFTER conn.Begin() — the pool is "+
						"capped at ONE connection, so this deadlocks permanently "+
						"rather than failing. Load it before the transaction opens, "+
						"or read it through the *sql.Tx.",
						fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name)
				}
			}
		}

		// An indirect one: passing the pool to a helper.
		for _, arg := range call.Args {
			ident, ok := arg.(*ast.Ident)
			if !ok || ident.Name != "conn" {
				continue
			}
			name := callName(call)
			t.Errorf("%s: %s passes `conn` to %s AFTER conn.Begin() — if that "+
				"helper reads the pool, this deadlocks permanently (the pool is "+
				"capped at ONE connection). Load what it returns before the "+
				"transaction opens, or give the helper the *sql.Tx.",
				fset.Position(call.Pos()), fn.Name.Name, name)
		}
		return true
	})
}

// callName renders a call's function name for the diagnostic.
func callName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if ident, ok := fun.X.(*ast.Ident); ok {
			return ident.Name + "." + fun.Sel.Name
		}
		return fun.Sel.Name
	}
	return "a helper"
}
