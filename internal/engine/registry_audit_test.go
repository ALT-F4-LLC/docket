package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-614: the cross-project audit — every project's registry compared against
// ONE scan of the shared corpus.
//
// The state it exists for: of eleven projects sharing one store, exactly one
// carried every workflow at its current corpus version, and the only way to
// learn that was to cd into each checkout in turn or to read sqlite directly.

// investigationV8 is the corpus's CURRENT declaration. A project registered at
// version 4 is behind it — the shape the measured drift actually took, since a
// corpus bumps a workflow in place rather than adding a second file.
const investigationV8 = `
[pipeline]
name = "investigation"
version = 8

[match]
kind = ["task"]

[[step]]
name = "look"
executor = "w"
emits = "out"
after = []
`

const findingsSchemaV1 = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "findings@1",
  "type": "object",
  "properties": { "risk": { "type": "string" } }
}`

const findingsSchemaV2 = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "findings@2",
  "type": "object",
  "properties": { "risk": { "type": "string" }, "note": { "type": "string" } }
}`

// auditCorpus writes the two-registry corpus every test here compares against:
// investigation@8 (workflow) and findings@1 + findings@2 (schemas).
func auditCorpus(t *testing.T, configDir string) {
	t.Helper()
	writeConfigFile(t, configDir, "workflows/investigation.toml", investigationV8)
	writeConfigFile(t, configDir, "schemas/findings@1.json", findingsSchemaV1)
	writeConfigFile(t, configDir, "schemas/findings@2.json", findingsSchemaV2)
}

// registerWorkflowRow puts one workflow row in one project's registry, without
// going anywhere near activation: the audit's whole subject is registries that
// activation has NOT visited lately.
func registerWorkflowRow(t *testing.T, conn *sql.DB, projectID int, name string, version int) {
	t.Helper()
	body := "registered bytes for " + name
	_, _, err := db.InsertWorkflow(conn, &model.Workflow{
		ProjectID: projectID, Name: name, Version: version,
		SourcePath: "/nowhere/" + name + ".toml", SourceSHA256: workflow.SHA256([]byte(body)),
		Body: body, Parsed: "{}",
	}, model.NowMS())
	testsupport.Must(t, err, "registering %s@%d in project %d: %v", name, version, projectID, err)
}

func registerSchemaRow(t *testing.T, conn *sql.DB, projectID int, name string, version int) {
	t.Helper()
	body := findingsSchemaV1
	_, _, err := db.InsertSchema(conn, &model.Schema{
		ProjectID: projectID, Name: name, Version: version,
		SourcePath: "/nowhere/" + name + ".json", SourceSHA256: workflow.SHA256([]byte(body)),
		Body: body, Ordered: "{}",
	}, model.NowMS())
	testsupport.Must(t, err, "registering schema %s@%d in project %d: %v",
		name, version, projectID, err)
}

// auditOnce scans the corpus and audits the store, failing the test on either
// of the refusals the CLI words for an operator.
func auditOnce(t *testing.T, conn *sql.DB, opts RegistryAuditOptions) *RegistryAudit {
	t.Helper()
	index, err := ScanCorpus()
	testsupport.Must(t, err, "scanning the corpus: %v", err)
	if !index.Scanned() {
		t.Fatal("no instance-config root was scanned; every name would look orphaned")
	}
	testsupport.Must(t, index.Err(), "building the corpus index: %v", index.Err())

	audit, err := AuditRegistries(conn, index, opts)
	testsupport.Must(t, err, "auditing: %v", err)
	return audit
}

// projectAudit picks one project's verdict out of the store-wide report.
func projectAudit(t *testing.T, audit *RegistryAudit, id int) ProjectRegistryAudit {
	t.Helper()
	for _, p := range audit.Projects {
		if p.ProjectID == id {
			return p
		}
	}
	t.Fatalf("project %d is missing from the audit; it covers %d project(s)",
		id, len(audit.Projects))
	return ProjectRegistryAudit{}
}

// TestRegistryAuditReportsEveryProjectFromOneScan is DKT-614's headline: one
// invocation, every project, both registries — the stale project named without
// having stood in it, and the current one not named alongside it.
func TestRegistryAuditReportsEveryProjectFromOneScan(t *testing.T) {
	conn, configDir := configRepo(t)
	auditCorpus(t, configDir)

	// TWO REAL PROJECT ROWS. The first identity to register CLAIMS the default
	// row (db.ensureProject), so naming only one and assuming the default is
	// the other would put both registries in one project and prove nothing.
	stale, err := db.EnsureProject(conn, "/repo/stale.git", "stale.git", model.NowMS())
	testsupport.Must(t, err, "creating the stale project: %v", err)
	current, err := db.EnsureProject(conn, "/repo/current.git", "current.git", model.NowMS())
	testsupport.Must(t, err, "creating the current project: %v", err)
	if stale == current {
		t.Fatalf("both identities resolved to project %d", stale)
	}

	// The stale project: behind on both registries, plus a name a corpus
	// rename stranded.
	registerWorkflowRow(t, conn, stale, "investigation", 4)
	registerWorkflowRow(t, conn, stale, "security-load-bearing", 12)
	registerSchemaRow(t, conn, stale, "findings", 1)

	// The current one: everything at the corpus's version.
	registerWorkflowRow(t, conn, current, "investigation", 8)
	registerSchemaRow(t, conn, current, "findings", 2)

	audit := auditOnce(t, conn, RegistryAuditOptions{})

	if len(audit.Roots) == 0 {
		t.Error("the audit names no scanned roots, so a reader cannot tell " +
			"where `current` was read from")
	}

	got := projectAudit(t, audit, stale)
	if len(got.Behind) != 2 {
		t.Fatalf("the stale project reports %d behind, want 2 (the workflow AND "+
			"the schema): %+v", len(got.Behind), got.Behind)
	}
	// Ordered by kind then name, so schema precedes workflow.
	if got.Behind[0].Kind != RegistrationKindSchema || got.Behind[0].Name != "findings" ||
		got.Behind[0].RegisteredVersion != 1 || got.Behind[0].CurrentVersion != 2 {
		t.Errorf("schema lag = %+v, want findings registered 1 / current 2 — "+
			"the audit must cover schemas, not workflows only", got.Behind[0])
	}
	if got.Behind[1].Name != "investigation" || got.Behind[1].RegisteredVersion != 4 ||
		got.Behind[1].CurrentVersion != 8 {
		t.Errorf("workflow lag = %+v, want investigation registered 4 / current 8",
			got.Behind[1])
	}
	if got.Behind[1].CurrentPath == "" {
		t.Error("the lag names no corpus file, so the operator cannot read the " +
			"definition they are behind without a second search")
	}

	if len(got.Orphaned) != 1 || got.Orphaned[0].Name != "security-load-bearing" {
		t.Fatalf("orphans = %+v, want the one stranded name", got.Orphaned)
	}
	if len(got.Orphaned[0].Versions) != 1 || got.Orphaned[0].Versions[0] != 12 {
		t.Errorf("the orphan lists %v, want the registered version(s) to deprecate",
			got.Orphaned[0].Versions)
	}
	if got.Orphaned[0].Retired {
		t.Error("the orphan reads as already retired; it still binds")
	}

	if other := projectAudit(t, audit, current); !other.Clean() {
		t.Errorf("the up-to-date project carries findings: %+v", other)
	} else if other.Compared == 0 {
		t.Error("the up-to-date project compared nothing, so `clean` says nothing")
	}

	if audit.BehindTotal != 2 || audit.OrphanedTotal != 1 {
		t.Errorf("store-wide totals = %d behind / %d orphaned, want 2 / 1",
			audit.BehindTotal, audit.OrphanedTotal)
	}
}

// TestRegistryAuditVerdictIsPerNameNotPerVersion: a superseded version whose
// name is registered at the current version too is ordinary lineage. The rule
// `workflow list --orphans` established, applied to the lag question — a
// project holding four versions of one name has one thing to fix, not four.
func TestRegistryAuditVerdictIsPerNameNotPerVersion(t *testing.T) {
	conn, configDir := configRepo(t)
	auditCorpus(t, configDir)

	registerWorkflowRow(t, conn, db.DefaultProjectID, "investigation", 4)
	registerWorkflowRow(t, conn, db.DefaultProjectID, "investigation", 8)

	got := projectAudit(t, auditOnce(t, conn, RegistryAuditOptions{}), db.DefaultProjectID)
	if len(got.Behind) != 0 {
		t.Errorf("a project holding the current version is reported behind on "+
			"the older one it also holds: %+v", got.Behind)
	}
}

// TestRegistryAuditTakesTheHighestVersionTheCorpusDeclares: schemas are
// versioned IN THE FILENAME, so `findings@1.json` and `findings@2.json` sit
// side by side and only the second is current. Taking the first file scanned
// would report every up-to-date project as ahead of a corpus it matches.
func TestRegistryAuditTakesTheHighestVersionTheCorpusDeclares(t *testing.T) {
	conn, configDir := configRepo(t)
	auditCorpus(t, configDir)

	index, err := ScanCorpus()
	testsupport.Must(t, err, "scanning: %v", err)
	entry, ok := index.Current(RegistrationKindSchema, "findings")
	if !ok {
		t.Fatal("the corpus declares findings in two files and the index found neither")
	}
	if entry.Version != 2 {
		t.Errorf("current findings = @%d, want @2 — the highest version any "+
			"root declares", entry.Version)
	}
	_ = conn
}

// TestRegistryAuditIgnoresTheBuiltinSchema: `aggregate@1` ships in the binary,
// is visible to every project by design, and no file in any root declares it.
// Classifying it would report one unfixable orphan for every project in the
// store on every run of this verb.
func TestRegistryAuditIgnoresTheBuiltinSchema(t *testing.T) {
	conn, configDir := configRepo(t)
	auditCorpus(t, configDir)

	got := projectAudit(t, auditOnce(t, conn, RegistryAuditOptions{}), db.DefaultProjectID)
	for _, orphan := range got.Orphaned {
		if orphan.Name == "aggregate" {
			t.Fatalf("the builtin schema is reported orphaned: %+v", orphan)
		}
	}
}

// TestRegistryAuditMarksAnOrphanWhoseVersionsAreAllRetired: the orphan an
// operator has finished with stays listed, marked — a cleanup pass has to be
// able to see its own work, which is why `workflow list --orphans` shows
// deprecated rows too.
func TestRegistryAuditMarksAnOrphanWhoseVersionsAreAllRetired(t *testing.T) {
	conn, configDir := configRepo(t)
	auditCorpus(t, configDir)

	registerWorkflowRow(t, conn, db.DefaultProjectID, "security-load-bearing", 11)
	registerWorkflowRow(t, conn, db.DefaultProjectID, "security-load-bearing", 12)
	for _, version := range []int{11, 12} {
		_, err := db.DeprecateWorkflow(
			conn, db.DefaultProjectID, "security-load-bearing", version, model.NowMS())
		testsupport.Must(t, err, "deprecating @%d: %v", version, err)
	}

	got := projectAudit(t, auditOnce(t, conn, RegistryAuditOptions{}), db.DefaultProjectID)
	if len(got.Orphaned) != 1 {
		t.Fatalf("orphans = %+v, want the retired one still listed", got.Orphaned)
	}
	if !got.Orphaned[0].Retired {
		t.Error("every version is deprecated and the orphan is not marked retired, " +
			"so a finished cleanup reads as outstanding work")
	}
	if len(got.Orphaned[0].Versions) != 2 {
		t.Errorf("versions = %v, want both retired registrations listed",
			got.Orphaned[0].Versions)
	}
}

// TestRegistryAuditNarrowsToOneProject: the default is every project — that is
// the verb's whole point — but `--project` exists for the operator who already
// knows which one they are asking about.
func TestRegistryAuditNarrowsToOneProject(t *testing.T) {
	conn, configDir := configRepo(t)
	auditCorpus(t, configDir)

	first, err := db.EnsureProject(conn, "/repo/first.git", "first.git", model.NowMS())
	testsupport.Must(t, err, "creating the first project: %v", err)
	other, err := db.EnsureProject(conn, "/repo/other.git", "other.git", model.NowMS())
	testsupport.Must(t, err, "creating the second project: %v", err)
	registerWorkflowRow(t, conn, other, "investigation", 4)
	registerWorkflowRow(t, conn, first, "investigation", 4)

	audit := auditOnce(t, conn, RegistryAuditOptions{ProjectID: other})
	if len(audit.Projects) != 1 || audit.Projects[0].ProjectID != other {
		t.Fatalf("--project audited %d project(s), want only %d", len(audit.Projects), other)
	}
	if audit.BehindTotal != 1 {
		t.Errorf("behind_total = %d, want 1: the totals count the AUDITED "+
			"population, not the store", audit.BehindTotal)
	}
}

// TestCorpusIndexReportsNothingScannedWithNoRoot is the state every caller has
// to check before believing a verdict: with no root, every registered name in
// the store trivially has no file declaring it.
func TestCorpusIndexReportsNothingScannedWithNoRoot(t *testing.T) {
	docketDir := t.TempDir()
	t.Setenv("DOCKET_PATH", docketDir)

	index, err := ScanCorpus()
	testsupport.Must(t, err, "scanning an empty store: %v", err)
	if index.Scanned() {
		t.Fatal("a store with no config directory reports a scanned corpus")
	}
	if _, ok := index.Current(RegistrationKindWorkflow, "investigation"); ok {
		t.Error("an unscanned index answered a `current version` question")
	}
}
