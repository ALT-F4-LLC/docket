package store

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestStoreCoversHeadDBCalls pins the Store surface to the internal/db
// storage functions internal/engine and internal/cli called when the seam was
// cut. testdata/db_calls_head.txt lists them, one per line: every exported
// internal/db function those packages' non-test files reference, minus the
// helpers that take no database handle and the Open/OpenReader constructors
// that OpenSQLite replaces. Each entry must be a method of Store or of Tx
// (whose method set includes ReadTx's).
func TestStoreCoversHeadDBCalls(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "db_calls_head.txt"))
	testsupport.Must(t, err, "opening the call snapshot: %v", err)
	defer f.Close()

	methods := map[string]bool{}
	for _, iface := range []reflect.Type{
		reflect.TypeFor[Store](),
		reflect.TypeFor[Tx](),
	} {
		for i := range iface.NumMethod() {
			methods[iface.Method(i).Name] = true
		}
	}

	entries := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if name == "" {
			continue
		}
		entries++
		if !methods[name] {
			t.Errorf("db.%s is called by internal/engine or internal/cli but is neither a Store nor a Tx method", name)
		}
	}
	testsupport.Must(t, scanner.Err(), "reading the call snapshot: %v", scanner.Err())
	if entries == 0 {
		t.Fatal("the call snapshot is empty, so the check covered nothing")
	}
}

// TestStoreSignaturesNameNoDatabaseSQL keeps database/sql behind the seam: no
// method of Store or of the transaction types it returns may take or return a
// database/sql type.
func TestStoreSignaturesNameNoDatabaseSQL(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	testsupport.Must(t, err, "listing package files: %v", err)

	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		testsupport.Must(t, err, "parsing %s: %v", path, err)
		sqlName := databaseSQLImportName(file)

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				switch ts.Name.Name {
				case "Store", "ReadTx", "Tx":
				default:
					continue
				}
				iface, ok := ts.Type.(*ast.InterfaceType)
				if !ok {
					t.Fatalf("%s is declared in %s but is not an interface", ts.Name.Name, path)
				}
				found[ts.Name.Name] = true
				if sqlName == "" {
					continue
				}
				for _, field := range iface.Methods.List {
					fn, ok := field.Type.(*ast.FuncType)
					if !ok {
						continue
					}
					if ref := sqlReference(fn, sqlName); ref != "" {
						t.Errorf("%s.%s references %s: database/sql must stay inside the implementation",
							ts.Name.Name, field.Names[0].Name, ref)
					}
				}
			}
		}
	}
	for _, name := range []string{"Store", "ReadTx", "Tx"} {
		if !found[name] {
			t.Errorf("interface %s was not found, so its signatures went unchecked", name)
		}
	}
}

func databaseSQLImportName(file *ast.File) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "database/sql" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "sql"
	}
	return ""
}

func sqlReference(fn *ast.FuncType, sqlName string) string {
	var ref string
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || ref != "" {
			return ref == ""
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == sqlName {
			ref = sqlName + "." + sel.Sel.Name
		}
		return true
	})
	return ref
}

// TestBeginReadSnapshotDoesNotWaitForOpenWrite holds a write transaction open
// and requires a snapshot to open and read committed state before a deadline
// shorter than busy_timeout, so a snapshot that queued behind the write lock
// misses it instead of eventually succeeding.
func TestBeginReadSnapshotDoesNotWaitForOpenWrite(t *testing.T) {
	const deadline = 2 * time.Second
	const key = db.KeyVoteHoldVoters

	st, err := OpenSQLite(filepath.Join(t.TempDir(), "issues.db"))
	testsupport.Must(t, err, "opening the store: %v", err)
	t.Cleanup(func() { st.Close() })
	err = st.Initialize()
	testsupport.Must(t, err, "Initialize: %v", err)
	err = st.Migrate()
	testsupport.Must(t, err, "Migrate: %v", err)
	err = st.SetConfig(0, key, "alice")
	testsupport.Must(t, err, "committing the starting value: %v", err)

	write, err := st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { write.Rollback() })
	_, err = write.SetConfigTx(0, key, "bob")
	testsupport.Must(t, err, "writing inside the open transaction: %v", err)

	type result struct {
		value string
		err   error
	}
	read := make(chan result, 1)
	go func() {
		snap, err := st.BeginReadSnapshot()
		if err != nil {
			read <- result{err: err}
			return
		}
		defer snap.Rollback()
		entry, err := snap.GetConfigTx(0, key)
		read <- result{value: entry.Value, err: err}
	}()

	select {
	case r := <-read:
		testsupport.Must(t, r.err, "reading through the snapshot: %v", r.err)
		if r.value != "alice" {
			t.Fatalf("snapshot read %s = %q, want the committed %q", key, r.value, "alice")
		}
	case <-time.After(deadline):
		t.Fatalf("BeginReadSnapshot did not return a readable handle within %v while a write transaction was open", deadline)
	}
}
