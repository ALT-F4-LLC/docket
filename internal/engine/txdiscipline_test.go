package engine

import (
	"fmt"
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
// The check: within a function, the region between a transaction opener
// (`conn.Begin()`, `conn.BeginTx()`, or a package helper returning *sql.Tx,
// such as beginRunReportSnapshot) and the matching `Commit()` is a LIVE
// TRANSACTION, and no call inside that region may touch the pool — neither
// directly (`conn.Query`) nor by passing `conn` to a helper that might.
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
		openers := txOpenerHelpers(pkg.Files)
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					return true
				}
				for _, read := range poolReadsInsideTransactions(fn, openers) {
					t.Errorf("%s: %s", fset.Position(read.pos), read.msg)
				}
				return true
			})
		}
	}
}

// poolRead is one pool-taking call found inside a live transaction.
type poolRead struct {
	pos token.Pos
	msg string
}

// liveRegion is a span in which a transaction is open: from its opener to the
// `Commit()` that closes it, or to the end of the function when nothing
// commits (the read-only paths, which rely on `defer tx.Rollback()`).
type liveRegion struct{ from, to token.Pos }

// txOpenerHelpers names the package functions that return a *sql.Tx, such as
// beginRunReportSnapshot. Calling one opens a transaction as conn.Begin does.
func txOpenerHelpers(files map[string]*ast.File) map[string]bool {
	openers := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Type.Results == nil {
				continue
			}
			for _, result := range fn.Type.Results.List {
				if isSQLTxPointer(result.Type) {
					openers[fn.Name.Name] = true
				}
			}
		}
	}
	return openers
}

func isSQLTxPointer(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "sql" && sel.Sel.Name == "Tx"
}

// poolReadsInsideTransactions returns every pool-taking call inside a
// live-transaction region of fn. A region opens at conn.Begin, conn.BeginTx,
// or a call to one of txOpeners.
func poolReadsInsideTransactions(fn *ast.FuncDecl, txOpeners map[string]bool) []poolRead {
	// Collect the Begin and Commit positions in source order.
	var begins, commits []token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if helper, ok := call.Fun.(*ast.Ident); ok && txOpeners[helper.Name] {
			begins = append(begins, call.Pos())
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
		case ident.Name == "conn" && (sel.Sel.Name == "Begin" || sel.Sel.Name == "BeginTx"):
			begins = append(begins, call.Pos())
		case strings.HasPrefix(ident.Name, "tx") && sel.Sel.Name == "Commit":
			commits = append(commits, call.Pos())
		}
		return true
	})

	if len(begins) == 0 {
		return nil // No transaction in this function.
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

	var reads []poolRead
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
					reads = append(reads, poolRead{call.Pos(), fmt.Sprintf(
						"%s calls conn.%s while its transaction is open — the pool is "+
							"capped at ONE connection, so this deadlocks permanently "+
							"rather than failing. Load it before the transaction opens, "+
							"or read it through the *sql.Tx.",
						fn.Name.Name, sel.Sel.Name)})
				}
			}
		}

		// An indirect one: passing the pool to a helper.
		for _, arg := range call.Args {
			ident, ok := arg.(*ast.Ident)
			if !ok || ident.Name != "conn" {
				continue
			}
			reads = append(reads, poolRead{call.Pos(), fmt.Sprintf(
				"%s passes `conn` to %s while its transaction is open — if that "+
					"helper reads the pool, this deadlocks permanently (the pool is "+
					"capped at ONE connection). Load what it returns before the "+
					"transaction opens, or give the helper the *sql.Tx.",
				fn.Name.Name, callName(call))})
		}
		return true
	})
	return reads
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

// TestPoolReadsInsideTransactionsOpeners pins which calls open a live region:
// a function that opens its transaction any of these ways and then reads the
// pool is flagged at exactly that read.
func TestPoolReadsInsideTransactionsOpeners(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantLine int // line of the single expected finding within body; 0 for none
		wantCall string
	}{
		{
			name:     "conn.Begin",
			body:     "tx, _ := conn.Begin()\nconn.QueryRow(\"q\")\ntx.Commit()",
			wantLine: 2, wantCall: "conn.QueryRow",
		},
		{
			name:     "conn.BeginTx",
			body:     "tx, _ := conn.BeginTx(ctx, nil)\nconn.QueryRow(\"q\")\ntx.Commit()",
			wantLine: 2, wantCall: "conn.QueryRow",
		},
		{
			name:     "package helper returning *sql.Tx",
			body:     "tx, _ := openSnapshot(conn)\ndefer tx.Rollback()\nlookup(conn)",
			wantLine: 3, wantCall: "lookup",
		},
		{
			name: "read after commit",
			body: "tx, _ := conn.BeginTx(ctx, nil)\ntx.Commit()\nconn.QueryRow(\"q\")",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reads, lineOf := synthesizedPoolReads(t, tc.body)
			if tc.wantLine == 0 {
				if len(reads) != 0 {
					t.Fatalf("want no finding, got %d: %+v", len(reads), reads)
				}
				return
			}
			if len(reads) != 1 {
				t.Fatalf("want exactly one finding, got %d: %+v", len(reads), reads)
			}
			if got := lineOf(reads[0].pos); got != tc.wantLine {
				t.Errorf("finding on body line %d, want %d: %s", got, tc.wantLine, reads[0].msg)
			}
			if !strings.Contains(reads[0].msg, tc.wantCall) {
				t.Errorf("finding does not name %s: %s", tc.wantCall, reads[0].msg)
			}
		})
	}
}

// synthesizedPoolReads runs the checker over body wrapped as function f in a
// file that also declares openSnapshot, a helper returning *sql.Tx. lineOf
// maps a finding's position to its 1-based line within body.
func synthesizedPoolReads(t *testing.T, body string) ([]poolRead, func(token.Pos) int) {
	t.Helper()
	const header = "package p\n\n" +
		"func openSnapshot(conn *sql.DB) (*sql.Tx, error) { return nil, nil }\n\n" +
		"func f() {\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", header+body+"\n}\n", 0)
	testsupport.Must(t, err, "parsing the synthetic source: %v", err)

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "f" {
			fn = d
		}
	}
	bodyStart := strings.Count(header, "\n")
	openers := txOpenerHelpers(map[string]*ast.File{"synthetic.go": file})
	return poolReadsInsideTransactions(fn, openers), func(pos token.Pos) int {
		return fset.Position(pos).Line - bodyStart
	}
}
