package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// storeFormSuffix is the naming rule store.SQLDB documents: the Store form of
// a LegacyDB entry point is its name with this suffix, on the same receiver.
const storeFormSuffix = "Store"

// parseNonTestFiles parses every non-test Go file in dir.
func parseNonTestFiles(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	return files
}

func isStoreType(expr ast.Expr, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "store" && sel.Sel.Name == name
}

// funcKey names a function or method as "Recv.Name" or "Name".
func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	return types.ExprString(typ) + "." + fd.Name.Name
}

func takesLegacyDB(fd *ast.FuncDecl) bool {
	for _, field := range fd.Type.Params.List {
		if isStoreType(field.Type, "LegacyDB") {
			return true
		}
	}
	return false
}

// cliEngineSelectors returns the names internal/cli's non-test files select:
// engine.Name for package functions, and any .Name for methods.
func cliEngineSelectors(t *testing.T) (funcs, methods map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	funcs, methods = map[string]bool{}, map[string]bool{}
	for _, f := range parseNonTestFiles(t, fset, filepath.Join("..", "cli")) {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			methods[sel.Sel.Name] = true
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "engine" {
				funcs[sel.Sel.Name] = true
			}
			return true
		})
	}
	return funcs, methods
}

// engineDecls parses internal/engine's non-test files and indexes every
// function and method by funcKey.
func engineDecls(t *testing.T) (*token.FileSet, map[string]*ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	decls := map[string]*ast.FuncDecl{}
	for _, f := range parseNonTestFiles(t, fset, ".") {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				decls[funcKey(fd)] = fd
			}
		}
	}
	return fset, decls
}

// legacyFormsUnderTheRule returns every exported engine function or method in
// a non-test file that takes a store.LegacyDB and that criterion 1's rule
// selects or that already has a Store form. The rule selects what a non-test
// internal/cli file calls; the Store-form clause keeps a form checked after
// its CLI call moves to internal/verb.
func legacyFormsUnderTheRule(t *testing.T, decls map[string]*ast.FuncDecl) []string {
	t.Helper()
	funcs, methods := cliEngineSelectors(t)
	var keys []string
	for key, fd := range decls {
		if !fd.Name.IsExported() || !takesLegacyDB(fd) {
			continue
		}
		called := funcs[fd.Name.Name]
		if fd.Recv != nil {
			called = methods[fd.Name.Name]
		}
		_, hasStoreForm := decls[key+storeFormSuffix]
		if called || hasStoreForm {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// returnsError reports whether fd's last result is an error.
func returnsError(fd *ast.FuncDecl) bool {
	results := flattenParams(fd.Type.Results)
	if len(results) == 0 {
		return false
	}
	id, ok := results[len(results)-1].typ.(*ast.Ident)
	return ok && id.Name == "error"
}

// checkSQLDBPrologue reports how storeForm departs from the prologue store.SQLDB
// documents: `conn, err := store.SQLDB(<its store.Store parameter>)`, then an
// `if err != nil` whose body is one return that, for a form returning an
// error, returns err as that error.
func checkSQLDBPrologue(legacy, storeForm *ast.FuncDecl) string {
	var storeParam string
	for i, p := range flattenParams(legacy.Type.Params) {
		if isStoreType(p.typ, "LegacyDB") {
			storeParam = flattenParams(storeForm.Type.Params)[i].name
		}
	}
	body := storeForm.Body.List
	if len(body) < 2 {
		return "body is shorter than the prologue"
	}
	assign, ok := body[0].(*ast.AssignStmt)
	if !ok || assign.Tok != token.DEFINE || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
		return "first statement is not `conn, err := store.SQLDB(...)`"
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok || !isStoreType(call.Fun, "SQLDB") || len(call.Args) != 1 || !isIdent(call.Args[0], storeParam) {
		return "first statement does not call store.SQLDB(" + storeParam + ")"
	}
	errName, ok := assign.Lhs[1].(*ast.Ident)
	if !ok {
		return "SQLDB's error is not bound to a name"
	}
	check, ok := body[1].(*ast.IfStmt)
	if !ok || check.Init != nil || check.Else != nil {
		return "second statement is not `if " + errName.Name + " != nil`"
	}
	cond, ok := check.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ || !isIdent(cond.X, errName.Name) || !isIdent(cond.Y, "nil") {
		return "second statement is not `if " + errName.Name + " != nil`"
	}
	if len(check.Body.List) != 1 {
		return "refusal branch is not a single return"
	}
	ret, ok := check.Body.List[0].(*ast.ReturnStmt)
	if !ok {
		return "refusal branch is not a single return"
	}
	if returnsError(storeForm) {
		if len(ret.Results) == 0 || !isIdent(ret.Results[len(ret.Results)-1], errName.Name) {
			return "refusal branch does not return " + errName.Name + " as the error"
		}
	}
	return ""
}

type param struct {
	name string
	typ  ast.Expr
}

func flattenParams(fl *ast.FieldList) []param {
	var out []param
	if fl == nil {
		return out
	}
	for _, field := range fl.List {
		if len(field.Names) == 0 {
			out = append(out, param{typ: field.Type})
			continue
		}
		for _, n := range field.Names {
			out = append(out, param{name: n.Name, typ: field.Type})
		}
	}
	return out
}

// checkStoreFormSignature reports how store's signature departs from legacy's
// with every store.LegacyDB parameter replaced by store.Store.
func checkStoreFormSignature(legacy, storeForm *ast.FuncDecl) string {
	lp, sp := flattenParams(legacy.Type.Params), flattenParams(storeForm.Type.Params)
	if len(lp) != len(sp) {
		return "parameter count differs"
	}
	for i := range lp {
		if isStoreType(lp[i].typ, "LegacyDB") {
			if !isStoreType(sp[i].typ, "Store") {
				return "parameter " + lp[i].name + " is not store.Store"
			}
			continue
		}
		if types.ExprString(lp[i].typ) != types.ExprString(sp[i].typ) {
			return "parameter " + lp[i].name + " type differs"
		}
	}
	lr, sr := flattenParams(legacy.Type.Results), flattenParams(storeForm.Type.Results)
	if len(lr) != len(sr) {
		return "result count differs"
	}
	for i := range lr {
		if types.ExprString(lr[i].typ) != types.ExprString(sr[i].typ) {
			return "result types differ"
		}
	}
	return ""
}

// checkSingleForward reports how legacy's body departs from one statement that
// calls its Store form with store.StoreOn on each LegacyDB parameter and every
// other parameter passed through.
func checkSingleForward(legacy *ast.FuncDecl) string {
	if legacy.Body == nil || len(legacy.Body.List) != 1 {
		return "body is not a single statement"
	}
	var call *ast.CallExpr
	switch stmt := legacy.Body.List[0].(type) {
	case *ast.ReturnStmt:
		if legacy.Type.Results == nil || len(stmt.Results) != 1 {
			return "return does not carry exactly one call"
		}
		call, _ = stmt.Results[0].(*ast.CallExpr)
	case *ast.ExprStmt:
		if legacy.Type.Results != nil {
			return "body discards the Store form's results"
		}
		call, _ = stmt.X.(*ast.CallExpr)
	}
	if call == nil {
		return "statement is not a call"
	}
	want := legacy.Name.Name + storeFormSuffix
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if legacy.Recv != nil || fun.Name != want {
			return "call does not name " + want
		}
	case *ast.SelectorExpr:
		recv := legacy.Recv
		if recv == nil || len(recv.List[0].Names) != 1 || fun.Sel.Name != want {
			return "call does not name " + want
		}
		x, ok := fun.X.(*ast.Ident)
		if !ok || x.Name != recv.List[0].Names[0].Name {
			return "call is not on the receiver"
		}
	default:
		return "call does not name " + want
	}
	params := flattenParams(legacy.Type.Params)
	if len(call.Args) != len(params) {
		return "call passes a different number of arguments"
	}
	variadic := len(params) > 0 && isEllipsis(params[len(params)-1].typ)
	if variadic != call.Ellipsis.IsValid() {
		return "variadic parameter is not spread"
	}
	for i, p := range params {
		arg := call.Args[i]
		if isStoreType(p.typ, "LegacyDB") {
			wrap, ok := arg.(*ast.CallExpr)
			if !ok || !isStoreType(wrap.Fun, "StoreOn") || len(wrap.Args) != 1 || !isIdent(wrap.Args[0], p.name) {
				return "argument " + p.name + " is not store.StoreOn(" + p.name + ")"
			}
			continue
		}
		if !isIdent(arg, p.name) {
			return "argument " + p.name + " is not passed through"
		}
	}
	return ""
}

func isEllipsis(expr ast.Expr) bool {
	_, ok := expr.(*ast.Ellipsis)
	return ok
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && name != "" && id.Name == name
}

// measuredStoreForms is the issue's list of LegacyDB forms the rule selected
// when it was filed. The rule governs; the list guards against the rule
// silently selecting fewer functions.
var measuredStoreForms = strings.Fields(`
AbandonDispatchWith AbandonIssueInRunWith Activate AddRunNote AnnotateIntegration
AnnotateStep AuditRegistries AuthorizeConductor BackfillUsage BackfillVoteUsage
CastVote ClaimStepRendered ClaimStepWithGates CloseDispatch CompleteStep ConductRun
DecideStepWith DescriptionEditFrozenForActiveRuns Doctor DriveRunLifecycles
DriveVoteProposal EffectiveStatusCounts ExtendDispatch FailStep FailedGates
ForceReapStepWith GateStatus GetRunBudget GuardGate GuardRecord GuardSpawn
GuardSpawnActive GuardStop HeartbeatStep HeldResolutionStaleTargets HoldIssue
HoldStep IntegrateSteps IsVoteStepProposal IssueStepList LatestActivationHead
LatestIssueDisposition ListEvents ListRunNotes ListStepArtifacts LoadExecutorLedger
LoadRunReport LoadStepView MoveRunWith NextSteps OpenDispatch
OverridePassSkipsInterposedTargets PinContent PinDrift PruneEvents ReadArtifact
ReconcileDispatch RecordConductorPanelOpened RecordProjectRegisteredEvent
RecordRunFact RecordTrustEvent RefreshIssueBodyInRun RefreshIssueFilesInRun
RefreshIssueScopeInRun RenderStepAs RepinRunWith ResolveRunFilter ResolveSeats
ResolveStepFilter ResolveStepWith RunAdvisories RunDetachedPreGates RunDetachedSaga
RunStepList ScopeEditFrozenForActiveRuns SetConfig SetRunBudget VerifyDispatch
VerifyPins VoteProposalRun WaiveStaleTargets WorkflowsReferencingSchema
`)

func TestLegacyDBFormsForwardToStoreForms(t *testing.T) {
	fset, decls := engineDecls(t)
	selected := legacyFormsUnderTheRule(t, decls)

	selectedNames := map[string]bool{}
	for _, key := range selected {
		selectedNames[decls[key].Name.Name] = true
	}
	for _, name := range measuredStoreForms {
		if !selectedNames[name] {
			t.Errorf("%s is on the measured list but no exported LegacyDB form of that name is called from internal/cli or has a Store form", name)
		}
	}

	for _, key := range selected {
		legacy := decls[key]
		storeForm, ok := decls[key+storeFormSuffix]
		if !ok {
			t.Errorf("%s has no Store form %s", key, key+storeFormSuffix)
			continue
		}
		if why := checkStoreFormSignature(legacy, storeForm); why != "" {
			t.Errorf("%s: Store form signature: %s", key, why)
		}
		if why := checkSingleForward(legacy); why != "" {
			t.Errorf("%s (%s): LegacyDB form is not a single forward: %s",
				key, fset.Position(legacy.Pos()), why)
		}
		if why := checkSQLDBPrologue(legacy, storeForm); why != "" {
			t.Errorf("%s (%s): Store form prologue: %s",
				key+storeFormSuffix, fset.Position(storeForm.Pos()), why)
		}
	}
}

// storeImportPath is internal/store's import path, for resolving the local name
// a file imports it under.
const storeImportPath = "github.com/ALT-F4-LLC/docket/internal/store"

func TestSQLDBIsCalledOnlyFromEngineAndStore(t *testing.T) {
	root := filepath.Join("..", "..")
	owners := map[string]bool{
		filepath.Join(root, "internal", "engine"): true,
		filepath.Join(root, "internal", "store"):  true,
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if owners[path] || name == "testdata" || name == "vendor" || (path != root && strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		local := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) != storeImportPath {
				continue
			}
			local = "store"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		if local == "" || local == "_" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "SQLDB" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == local {
				t.Errorf("%s: store.SQLDB used outside internal/engine and internal/store", fset.Position(sel.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// storeFormBackends are the two SQLite Stores a verb can hold: one opened by
// store.OpenSQLite and one wrapping a pool with store.StoreOn. Each fixture is
// an activated run on a fresh database file, returned with its implement@0
// step's id.
var storeFormBackends = []struct {
	name string
	open func(t *testing.T) (store.Store, int)
}{
	{"OpenSQLite", func(t *testing.T) (store.Store, int) {
		path := filepath.Join(t.TempDir(), "issues.db")
		conn, err := db.Open(path)
		testsupport.Must(t, err, "opening database: %v", err)
		testsupport.Must(t, db.Initialize(conn), "Initialize")
		testsupport.Must(t, db.Migrate(conn), "Migrate")
		activatedRun(t, conn)
		stepID := stepIDByInstance(t, conn, "implement@0")
		testsupport.Must(t, conn.Close(), "closing the fixture pool")
		st, err := store.OpenSQLite(path)
		testsupport.Must(t, err, "OpenSQLite: %v", err)
		t.Cleanup(func() { st.Close() })
		return st, stepID
	}},
	{"StoreOn", func(t *testing.T) (store.Store, int) {
		conn := mustDB(t)
		activatedRun(t, conn)
		return store.StoreOn(conn), stepIDByInstance(t, conn, "implement@0")
	}},
}

func TestStoreFormsClaimCompleteAndFailOnSQLiteStores(t *testing.T) {
	for _, backend := range storeFormBackends {
		t.Run(backend.name+"/claim with gates and complete", func(t *testing.T) {
			st, stepID := backend.open(t)
			e := testEngine()

			claim, err := e.ClaimStepWithGatesStore(st, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
			testsupport.Must(t, err, "ClaimStepWithGatesStore: %v", err)
			err = e.CompleteStepStore(st, stepID, CompleteOptions{
				Token: claim.Token, Artifact: []byte("the change summary"), NowMS: nowMS,
			})
			testsupport.Must(t, err, "CompleteStepStore: %v", err)

			step, err := st.GetStep(stepID)
			testsupport.Must(t, err, "GetStep: %v", err)
			if step.Status != store.StepDone {
				t.Errorf("status = %q, want %q", step.Status, store.StepDone)
			}
		})
		t.Run(backend.name+"/rendered claim and fail", func(t *testing.T) {
			st, stepID := backend.open(t)
			e := testEngine()

			claim, _, err := e.ClaimStepRenderedStore(st, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS}, "", "")
			testsupport.Must(t, err, "ClaimStepRenderedStore: %v", err)
			err = e.FailStepStore(st, stepID, claim.Token, "went wrong", "", nowMS+1)
			testsupport.Must(t, err, "FailStepStore: %v", err)

			step, err := st.GetStep(stepID)
			testsupport.Must(t, err, "GetStep: %v", err)
			if step.Status == store.StepClaimed {
				t.Errorf("status = %q after fail, want the claim released", step.Status)
			}
			if step.FailedAttempts != 1 {
				t.Errorf("failed attempts = %d, want 1", step.FailedAttempts)
			}
		})
		t.Run(backend.name+"/read snapshot", func(t *testing.T) {
			st, stepID := backend.open(t)
			step, err := st.GetStep(stepID)
			testsupport.Must(t, err, "GetStep: %v", err)

			budget, err := GetRunBudgetStore(st, step.RunID)
			testsupport.Must(t, err, "GetRunBudgetStore: %v", err)
			if budget == nil {
				t.Fatal("GetRunBudgetStore returned no budget")
			}
		})
	}
}

// notSQLiteStore is a Store with no SQLite handle under it, as pgstore's is.
// Its methods panic on the nil embedded interface, so a form that reaches a
// store call instead of stopping at SQLDB's refusal fails the test.
type notSQLiteStore struct{ store.Store }

const notSQLiteRefusal = "is not a SQLite store"

func isNotSQLiteRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), notSQLiteRefusal)
}

func TestStoreFormsRefuseANonSQLiteStore(t *testing.T) {
	st := notSQLiteStore{}
	err := testEngine().CompleteStepStore(st, 1, CompleteOptions{NowMS: nowMS})
	if !isNotSQLiteRefusal(err) {
		t.Errorf("CompleteStepStore on a non-SQLite store = %v, want SQLDB's refusal", err)
	}

	// Each no-error Store form, reduced to the text it hands its caller.
	noError := map[string]func() []string{
		"DoctorStore": func() []string {
			report := DoctorStore(st, DoctorOptions{})
			if report.Clean {
				return nil
			}
			var failed []string
			for _, c := range report.Checks {
				if c.Verdict == DoctorFail {
					failed = append(failed, c.Detail)
				}
			}
			return failed
		},
		"ScopeEditFrozenForActiveRunsStore": func() []string {
			return ScopeEditFrozenForActiveRunsStore(st, 1)
		},
		"DescriptionEditFrozenForActiveRunsStore": func() []string {
			return DescriptionEditFrozenForActiveRunsStore(st, 1)
		},
		"OverridePassSkipsInterposedTargetsStore": func() []string {
			return OverridePassSkipsInterposedTargetsStore(st, 1)
		},
		"Engine.HeldResolutionStaleTargetsStore": func() []string {
			var reasons []string
			for _, s := range testEngine().HeldResolutionStaleTargetsStore(st, 1, nowMS) {
				reasons = append(reasons, s.Reason)
			}
			return reasons
		},
	}

	_, decls := engineDecls(t)
	for _, key := range legacyFormsUnderTheRule(t, decls) {
		storeKey := key + storeFormSuffix
		if storeForm, ok := decls[storeKey]; ok && !returnsError(storeForm) {
			if _, covered := noError[storeKey]; !covered {
				t.Errorf("%s returns no error and has no non-SQLite case here", storeKey)
			}
		}
	}

	for name, call := range noError {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s on a non-SQLite store panicked: %v", name, r)
				}
			}()
			got := call()
			if len(got) != 1 || !strings.Contains(got[0], notSQLiteRefusal) {
				t.Errorf("%s on a non-SQLite store = %q, want one entry carrying SQLDB's refusal", name, got)
			}
		})
	}
}
