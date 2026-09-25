package cli

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `docket schema deprecate` — the schema half of workflow retirement.
//
// The state it exists for: `registry audit` reporting an orphaned schema with
// `retired: false` and no verb anywhere that could change that short of
// editing the store's database by hand.

// readsFindings is a definition whose one step names findings@1 as payload.
const readsFindings = `
[pipeline]
name = "reads"
version = 1
[[step]]
name = "a"
after = []
executor = "x"
emits = "k"
payload = "findings@1"
`

// deprecateSchema runs the verb on a bare command against the ambient project.
func deprecateSchema(t *testing.T, conn *sql.DB, ref string, restore bool) error {
	t.Helper()
	cmd := fanoutCmd(conn, "", false)
	if restore {
		testsupport.Must(t, cmd.Flags().Set("restore", "true"), "setting --restore")
	}
	w, _ := bufWriter(true)
	return runSchemaDeprecate(cmd, []string{ref}, w)
}

func orphanNamed(t *testing.T, audit engine.RegistryAudit, name string) engine.RegistryOrphan {
	t.Helper()
	for _, p := range audit.Projects {
		for _, o := range p.Orphaned {
			if o.Kind == engine.RegistrationKindSchema && o.Name == name {
				return o
			}
		}
	}
	t.Fatalf("no orphaned schema %q in the audit: %+v", name, audit.Projects)
	return engine.RegistryOrphan{}
}

// TestSchemaDeprecateRetiresAndTheAuditSaysSo is the acceptance criterion,
// verbatim: deprecate succeeds, the audit reports the orphan as retired: true,
// --restore reverses it and the audit reports retired: false again.
func TestSchemaDeprecateRetiresAndTheAuditSaysSo(t *testing.T) {
	conn := newTestDB(t)
	// A corpus that declares a workflow and NO schema, so findings is an
	// orphan the audit will classify.
	auditCorpusRoot(t, map[string]string{
		"workflows/investigation.toml": auditInvestigationV8,
	})
	testsupport.Must(t, registerSchema(t, conn, "findings@1", findingsSchema), "register")

	audit, _ := runRegistryAuditJSON(t, registryAuditCmdWithDB(conn))
	if o := orphanNamed(t, audit, "findings"); o.Retired {
		t.Fatalf("a schema nobody retired reads retired: %+v", o)
	}

	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", false), "deprecate")
	audit, human := runRegistryAuditJSON(t, registryAuditCmdWithDB(conn))
	if o := orphanNamed(t, audit, "findings"); !o.Retired {
		t.Errorf("after deprecate the audit still reports retired: false: %+v", o)
	}
	if !strings.Contains(human, "[all deprecated]") {
		t.Errorf("the human render does not mark the retired orphan:\n%s", human)
	}

	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", true), "restore")
	audit, _ = runRegistryAuditJSON(t, registryAuditCmdWithDB(conn))
	if o := orphanNamed(t, audit, "findings"); o.Retired {
		t.Errorf("after --restore the audit still reports retired: true: %+v", o)
	}
}

// TestSchemaDeprecateTwiceIsAConflictAndRestoreIsIdempotent keeps the
// asymmetry `workflow deprecate` established: the second retirement is wrong
// about the store, the second restore changes nothing and says so.
func TestSchemaDeprecateTwiceIsAConflictAndRestoreIsIdempotent(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, registerSchema(t, conn, "findings@1", findingsSchema), "register")

	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", false), "first deprecate")
	err := deprecateSchema(t, conn, "findings@1", false)
	if err == nil || codeOf(t, err) != output.ErrConflict {
		t.Errorf("second deprecate = %v, want CONFLICT", err)
	}
	if !strings.Contains(err.Error(), "already deprecated") {
		t.Errorf("refusal does not say the version is already deprecated: %v", err)
	}

	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", true), "restore")
	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", true), "second restore")

	err = deprecateSchema(t, conn, "nope@1", false)
	if err == nil || codeOf(t, err) != output.ErrNotFound {
		t.Errorf("deprecating an unregistered schema = %v, want NOT_FOUND", err)
	}
	err = deprecateSchema(t, conn, "findings", false)
	if err == nil || codeOf(t, err) != output.ErrValidation {
		t.Errorf("deprecating a bare name = %v, want VALIDATION_ERROR: retirement "+
			"applies to one version", err)
	}
}

// TestSchemaDeprecateRefusesTheBuiltin: aggregate@1 ships in the binary and is
// visible to every project through its flag, so retiring it "in this project"
// would retire it store-wide.
func TestSchemaDeprecateRefusesTheBuiltin(t *testing.T) {
	conn := newTestDB(t)
	err := deprecateSchema(t, conn, "aggregate@1", false)
	if err == nil || codeOf(t, err) != output.ErrValidation {
		t.Fatalf("deprecating the builtin = %v, want VALIDATION_ERROR", err)
	}
	if !strings.Contains(err.Error(), "builtin") {
		t.Errorf("refusal does not say why: %v", err)
	}
	s, err := db.GetSchema(conn, db.DefaultProjectID, "aggregate", 1)
	testsupport.Must(t, err, "the builtin vanished: %v", err)
	if s.Deprecated() {
		t.Error("the builtin was retired anyway")
	}
}

// TestSchemaDeprecateRefusesAVersionALiveWorkflowNames is the open question
// resolved: refuse, naming the referencing workflow versions, no override.
// Once the workflow is itself retired the schema can go.
func TestSchemaDeprecateRefusesAVersionALiveWorkflowNames(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, registerSchema(t, conn, "findings@1", findingsSchema), "register schema")
	testsupport.Must(t, registerSource(t, conn, readsFindings), "register workflow")

	err := deprecateSchema(t, conn, "findings@1", false)
	if err == nil || codeOf(t, err) != output.ErrConflict {
		t.Fatalf("deprecate with a live referencer = %v, want CONFLICT", err)
	}
	if !strings.Contains(err.Error(), "reads@1") {
		t.Errorf("refusal does not name the referencing workflow: %v", err)
	}
	s, err := db.GetSchema(conn, db.DefaultProjectID, "findings", 1)
	testsupport.Must(t, err, "GetSchema: %v", err)
	if s.Deprecated() {
		t.Error("the refusal retired the schema anyway")
	}

	// A RETIRED workflow does not count: it cannot be newly activated, and
	// counting it would leave a cleanup pass unable to finish.
	_, err = db.DeprecateWorkflow(conn, db.DefaultProjectID, "reads", 1, model.NowMS())
	testsupport.Must(t, err, "retiring the workflow: %v", err)
	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", false),
		"deprecate after the referencer was retired")
}

// TestWorkflowLintAndRegisterRefuseARetiredSchema: a NEW payload reference to a
// retired version is refused by both verbs, naming the schema and the remedy.
func TestWorkflowLintAndRegisterRefuseARetiredSchema(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, registerSchema(t, conn, "findings@1", findingsSchema), "register schema")
	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", false), "deprecate")

	err, _ := lintSource(t, conn, readsFindings)
	if err == nil || codeOf(t, err) != output.ErrValidation {
		t.Fatalf("lint of a definition naming a retired schema = %v, want VALIDATION_ERROR", err)
	}
	for _, want := range []string{"findings@1", "retired", "--restore"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("lint refusal does not mention %q: %v", want, err)
		}
	}

	err = registerSource(t, conn, readsFindings)
	if err == nil || codeOf(t, err) != output.ErrValidation {
		t.Fatalf("register of a definition naming a retired schema = %v, want VALIDATION_ERROR", err)
	}
	if countWorkflows(t, conn) != 0 {
		t.Error("the refused register left a row behind")
	}

	// Restored, the same bytes register.
	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", true), "restore")
	testsupport.Must(t, registerSource(t, conn, readsFindings), "register after restore")
}

// TestSchemaShowAndPinsStillResolveARetiredVersion: retirement never deletes.
// An explicit @version still resolves and renders as retired; the bare name
// skips retired versions, as `workflow show NAME` does.
func TestSchemaShowAndPinsStillResolveARetiredVersion(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, registerSchema(t, conn, "findings@1", findingsSchema), "register")
	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", false), "deprecate")

	w, buf := bufWriter(false)
	testsupport.Must(t, runSchemaShow(schemaShowCmdWithDB(conn, false), []string{"findings@1"}, w),
		"show of a retired version")
	if !strings.Contains(buf.String(), "DEPRECATED") {
		t.Errorf("show does not render the retirement:\n%s", buf.String())
	}

	w, buf = bufWriter(false)
	testsupport.Must(t, runSchemaShow(schemaShowCmdWithDB(conn, true), []string{"findings@1"}, w),
		"show --body of a retired version")
	if strings.TrimSpace(buf.String()) != strings.TrimSpace(findingsSchema) {
		t.Error("--body of a retired version is not the registered bytes")
	}

	// The pin path: an explicit version through the store, which is what a
	// run's pinned reference resolves through.
	s, err := db.GetSchema(conn, db.DefaultProjectID, "findings", 1)
	testsupport.Must(t, err, "a pinned lookup of a retired version failed: %v", err)
	if !s.Deprecated() {
		t.Error("the row does not carry its retirement")
	}

	w, _ = bufWriter(true)
	err = runSchemaShow(schemaShowCmdWithDB(conn, false), []string{"findings"}, w)
	if !errors.Is(err, db.ErrSchemaNotFound) && (err == nil || codeOf(t, err) != output.ErrNotFound) {
		t.Errorf("bare-name show of a fully retired name = %v, want NOT_FOUND", err)
	}
}

// TestSchemaListHidesRetiredVersionsByDefault mirrors `workflow list`: the
// default shows what may still be referenced, --deprecated shows the lineage,
// v2 carries the fact, v1 stays frozen.
func TestSchemaListHidesRetiredVersionsByDefault(t *testing.T) {
	conn := newTestDB(t)
	testsupport.Must(t, registerSchema(t, conn, "findings@1", findingsSchema), "register")
	testsupport.Must(t, deprecateSchema(t, conn, "findings@1", false), "deprecate")

	list := func(deprecated bool, jsonMode bool) string {
		cmd := schemaListCmdWithDB(conn, 50)
		cmd.Flags().Bool("deprecated", deprecated, "")
		w, buf := bufWriter(jsonMode)
		testsupport.Must(t, runSchemaList(cmd, nil, w), "list")
		return buf.String()
	}

	if out := list(false, true); strings.Contains(out, `"name":"findings"`) {
		t.Errorf("the default listing shows a retired version:\n%s", out)
	}
	out := list(true, true)
	if !strings.Contains(out, `"name":"findings"`) {
		t.Errorf("--deprecated does not show the retired version:\n%s", out)
	}
	if strings.Contains(out, "deprecated_at_ms") {
		t.Errorf("v1 items gained deprecated_at_ms; v1 is frozen:\n%s", out)
	}
	if human := list(true, false); !strings.Contains(human, "[deprecated]") {
		t.Errorf("the human render does not mark the retired version:\n%s", human)
	}

	schemas, _, err := db.ListSchemas(conn, db.SchemaListOptions{Name: "findings"})
	testsupport.Must(t, err, "ListSchemas: %v", err)
	if v2 := mustMarshal(t, model.SchemasWithVersion(schemas)); !strings.Contains(v2, `"deprecated_at_ms"`) {
		t.Errorf("v2 item %q does not carry deprecated_at_ms", v2)
	}
}

// TestSchemaDeprecateAllProjectsReportsPerProjectOutcomes: one invocation,
// every project, each judged on its own — deprecated, already-deprecated,
// not-registered, and in-use side by side, none cancelling another.
func TestSchemaDeprecateAllProjectsReportsPerProjectOutcomes(t *testing.T) {
	conn := newTestDB(t)
	one, two, three := threeProjects(t, conn)
	four, err := db.EnsureProject(conn, "/repo/four.git", "four.git", model.NowMS())
	testsupport.Must(t, err, "creating four.git: %v", err)

	for _, id := range []int{one, two, four} {
		_, _, err := db.InsertSchema(conn, &model.Schema{
			ProjectID: id, Name: "findings", Version: 1,
			SourceSHA256: "sha", Body: findingsSchema, Ordered: "{}",
		}, model.NowMS())
		testsupport.Must(t, err, "seeding findings@1 in project %d: %v", id, err)
	}
	_, err = db.DeprecateSchema(conn, two, "findings", 1, model.NowMS())
	testsupport.Must(t, err, "pre-retiring in two.git: %v", err)
	// four.git holds a live workflow that names the schema.
	w, _ := bufWriter(true)
	testsupport.Must(t, runWorkflowRegister(fanoutCmd(conn, "four.git", false),
		[]string{writeWorkflowFile(t, readsFindings)}, w), "registering the referencer in four.git")

	w, buf := bufWriter(true)
	err = runSchemaDeprecate(fanoutCmd(conn, "", true), []string{"findings@1"}, w)
	if err == nil {
		t.Fatal("a sweep with two refusals and a miss exited clean")
	}
	// Mixed failure codes have no single honest exit, so GENERAL_ERROR sends
	// the reader to the report — which holds every outcome.
	if got := reportedCodeOf(t, err); got != output.ErrGeneral {
		t.Errorf("exit code = %q, want %q for mixed CONFLICT and NOT_FOUND", got, output.ErrGeneral)
	}
	report := fanoutReportOf(t, buf.Bytes())

	for _, tc := range []struct {
		project int
		outcome string
		code    output.ErrorCode
	}{
		{one, outcomeDeprecated, ""},
		{two, outcomeAlreadyDeprecated, output.ErrConflict},
		{three, outcomeNotFound, output.ErrNotFound},
		{four, outcomeInUse, output.ErrConflict},
	} {
		got := outcomeIn(t, report, tc.project)
		if got.Outcome != tc.outcome || got.Code != tc.code {
			t.Errorf("project %d reported %+v, want %s / %q", tc.project, got, tc.outcome, tc.code)
		}
	}
	if got := outcomeIn(t, report, four); !strings.Contains(got.Detail, "reads@1") {
		t.Errorf("the in-use row does not name the referencing workflow: %+v", got)
	}

	s, err := db.GetSchema(conn, one, "findings", 1)
	testsupport.Must(t, err, "GetSchema in one.git: %v", err)
	if !s.Deprecated() {
		t.Error("the refusals elsewhere cancelled one.git's retirement")
	}
	s, err = db.GetSchema(conn, four, "findings", 1)
	testsupport.Must(t, err, "GetSchema in four.git: %v", err)
	if s.Deprecated() {
		t.Error("four.git's in-use refusal retired the schema anyway")
	}

	// --project targets exactly one, and its restore reports the pre-state.
	w, buf = bufWriter(true)
	cmd := fanoutCmd(conn, "two.git", false)
	testsupport.Must(t, cmd.Flags().Set("restore", "true"), "setting --restore")
	testsupport.Must(t, runSchemaDeprecate(cmd, []string{"findings@1"}, w), "restore in two.git")
	report = fanoutReportOf(t, buf.Bytes())
	if len(report.Results) != 1 || report.Results[0].Outcome != outcomeRestored {
		t.Errorf("--project restore reported %+v, want one restored row", report.Results)
	}
}
