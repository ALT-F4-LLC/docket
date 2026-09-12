package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
	"github.com/spf13/cobra"
)

// DKT-614: `docket registry audit` — the store-wide comparison of every
// project's registries against the shared corpus.
//
// The state it exists for: eleven projects sharing one store, seven of them
// several versions behind the corpus and missing its newest names entirely,
// and no verb anywhere that would say so without cd'ing into each checkout.

const auditInvestigationV8 = `
[pipeline]
name = "investigation"
version = 8
[[step]]
name = "look"
after = []
executor = "w"
emits = "out"
`

const auditFindingsSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "findings",
  "type": "object",
  "properties": { "risk": { "type": "string" } }
}`

// auditCorpusRoot points DOCKET_PATH at a store whose instance config holds
// exactly the files named, keyed by their path under `config/`.
//
// THE TEMP DIR COMES FIRST, BEFORE t.Setenv — t.TempDir() reads TMPDIR, and a
// test that rewrote the environment before taking its directory would get a
// path that does not exist.
func auditCorpusRoot(t *testing.T, files map[string]string) {
	t.Helper()
	docketDir := filepath.Join(t.TempDir(), ".docket")
	for rel, body := range files {
		path := filepath.Join(docketDir, "config", rel)
		testsupport.Must(t, os.MkdirAll(filepath.Dir(path), 0o755),
			"creating %s", filepath.Dir(path))
		testsupport.Must(t, os.WriteFile(path, []byte(body), 0o644), "writing %s", rel)
	}
	t.Setenv("DOCKET_PATH", docketDir)
}

// auditStandardCorpus is the corpus these tests compare against: one workflow
// at version 8, one schema at version 2.
func auditStandardCorpus(t *testing.T) {
	t.Helper()
	auditCorpusRoot(t, map[string]string{
		"workflows/investigation.toml": auditInvestigationV8,
		"schemas/findings@2.json":      auditFindingsSchema,
	})
}

func auditWorkflowRow(t *testing.T, conn *sql.DB, projectID int, name string, version int) {
	t.Helper()
	body := "bytes for " + name
	_, _, err := db.InsertWorkflow(conn, &model.Workflow{
		ProjectID: projectID, Name: name, Version: version,
		SourcePath: "/nowhere/" + name + ".toml", SourceSHA256: workflow.SHA256([]byte(body)),
		Body: body, Parsed: "{}",
	}, model.NowMS())
	testsupport.Must(t, err, "registering %s@%d: %v", name, version, err)
}

func auditSchemaRow(t *testing.T, conn *sql.DB, projectID int, name string, version int) {
	t.Helper()
	_, _, err := db.InsertSchema(conn, &model.Schema{
		ProjectID: projectID, Name: name, Version: version,
		SourcePath:   "/nowhere/" + name + ".json",
		SourceSHA256: workflow.SHA256([]byte(auditFindingsSchema)),
		Body:         auditFindingsSchema, Ordered: "{}",
	}, model.NowMS())
	testsupport.Must(t, err, "registering schema %s@%d: %v", name, version, err)
}

func registryAuditCmdWithDB(conn *sql.DB) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("project", "", "")
	return cmd
}

// runRegistryAuditJSON runs the verb and returns both renders: the JSON
// payload a consumer parses and the text an operator reads.
func runRegistryAuditJSON(t *testing.T, cmd *cobra.Command) (engine.RegistryAudit, string) {
	t.Helper()

	w, buf := bufWriter(true)
	testsupport.Must(t, runRegistryAudit(cmd, nil, w), "registry audit --json")
	var envelope struct {
		Data engine.RegistryAudit `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	w, human := bufWriter(false)
	testsupport.Must(t, runRegistryAudit(cmd, nil, w), "registry audit")
	return envelope.Data, human.String()
}

// TestRegistryAuditNamesEveryProjectsDrift is DKT-614's acceptance criterion:
// ONE command, every project in the store, BOTH registries — the behind names
// and the orphaned ones — with no cd and no sqlite.
func TestRegistryAuditNamesEveryProjectsDrift(t *testing.T) {
	conn := newTestDB(t)
	auditStandardCorpus(t)

	// The first identity to register claims the default row, so both projects
	// are named rather than assuming one of them is the default.
	stale, err := db.EnsureProject(conn, "/repo/stale.git", "stale.git", model.NowMS())
	testsupport.Must(t, err, "creating the stale project: %v", err)
	current, err := db.EnsureProject(conn, "/repo/current.git", "current.git", model.NowMS())
	testsupport.Must(t, err, "creating the current project: %v", err)

	auditWorkflowRow(t, conn, stale, "investigation", 4)
	auditWorkflowRow(t, conn, stale, "security-load-bearing", 12)
	auditSchemaRow(t, conn, stale, "findings", 1)
	auditWorkflowRow(t, conn, current, "investigation", 8)
	auditSchemaRow(t, conn, current, "findings", 2)

	audit, human := runRegistryAuditJSON(t, registryAuditCmdWithDB(conn))

	if len(audit.Projects) != 2 {
		t.Fatalf("the audit covers %d project(s), want both: %+v",
			len(audit.Projects), audit.Projects)
	}
	if len(audit.Roots) == 0 {
		t.Error("the payload names no scanned roots, so a consumer cannot tell " +
			"where `current` was read from")
	}
	if audit.BehindTotal != 2 || audit.OrphanedTotal != 1 {
		t.Errorf("totals = %d behind / %d orphaned, want 2 / 1 (a workflow AND a "+
			"schema behind, one stranded name)", audit.BehindTotal, audit.OrphanedTotal)
	}

	// The human render is the whole point of the verb — the operator asked
	// because they did not want to read sqlite.
	for _, want := range []string{
		"stale.git", "current.git",
		"investigation", "findings", "security-load-bearing",
		"behind", "orphaned", "up to date",
	} {
		if !strings.Contains(human, want) {
			t.Errorf("the render never mentions %q:\n%s", want, human)
		}
	}
	if !strings.Contains(human, "registered 4, current 8") {
		t.Errorf("the render does not state both versions of the lag:\n%s", human)
	}
}

// TestRegistryAuditNarrowsToOneProject: every project is the default, since
// that is the question the verb exists for, but --project resolves the same
// four keys `project list` prints.
func TestRegistryAuditNarrowsToOneProject(t *testing.T) {
	conn := newTestDB(t)
	auditStandardCorpus(t)

	first, err := db.EnsureProject(conn, "/repo/first.git", "first.git", model.NowMS())
	testsupport.Must(t, err, "creating the first project: %v", err)
	other, err := db.EnsureProject(conn, "/repo/other.git", "other.git", model.NowMS())
	testsupport.Must(t, err, "creating the second project: %v", err)
	auditWorkflowRow(t, conn, first, "investigation", 4)
	auditWorkflowRow(t, conn, other, "investigation", 4)

	cmd := registryAuditCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("project", "other.git"), "setting --project")

	audit, _ := runRegistryAuditJSON(t, cmd)
	if len(audit.Projects) != 1 || audit.Projects[0].ProjectID != other {
		t.Fatalf("--project audited %+v, want only other.git", audit.Projects)
	}
}

// TestRegistryAuditRefusesAnUnknownProject: the flag's whole purpose is to say
// WHICH project, so a ref that names none is refused rather than silently
// widened back to the store.
func TestRegistryAuditRefusesAnUnknownProject(t *testing.T) {
	conn := newTestDB(t)
	auditStandardCorpus(t)

	cmd := registryAuditCmdWithDB(conn)
	testsupport.Must(t, cmd.Flags().Set("project", "no-such-repo"), "setting --project")

	w, _ := bufWriter(true)
	err := runRegistryAudit(cmd, nil, w)
	if err == nil {
		t.Fatal("an unknown --project was accepted")
	}
	if got := codeOf(t, err); got != output.ErrNotFound {
		t.Errorf("error code = %q, want %q", got, output.ErrNotFound)
	}
}

// TestRegistryAuditRefusesWithNothingToScan: with no instance-config root,
// every registered name in the store trivially has no file declaring it and no
// current version to be behind. Rendering that as a report would be the verb
// inventing its entire result out of having looked nowhere — the refusal
// `workflow list --orphans` already makes, with the same code.
func TestRegistryAuditRefusesWithNothingToScan(t *testing.T) {
	conn := newTestDB(t)
	docketDir := filepath.Join(t.TempDir(), ".docket")
	testsupport.Must(t, os.MkdirAll(docketDir, 0o755), "creating the store directory")
	t.Setenv("DOCKET_PATH", docketDir)
	auditWorkflowRow(t, conn, db.DefaultProjectID, "investigation", 4)

	w, _ := bufWriter(true)
	err := runRegistryAudit(registryAuditCmdWithDB(conn), nil, w)
	if err == nil {
		t.Fatal("the audit reported drift with no instance-config root to scan; " +
			"every registration in the store would qualify")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("error code = %q, want %q", got, output.ErrValidation)
	}
}

// TestRegistryAuditReportsACleanStore: an empty result is a CLEAN BILL OF
// HEALTH, not "nothing found" — the two readings are opposite, and the render
// must not leave an operator guessing which one they got.
func TestRegistryAuditReportsACleanStore(t *testing.T) {
	conn := newTestDB(t)
	auditStandardCorpus(t)
	auditWorkflowRow(t, conn, db.DefaultProjectID, "investigation", 8)

	audit, human := runRegistryAuditJSON(t, registryAuditCmdWithDB(conn))
	if audit.BehindTotal != 0 || audit.OrphanedTotal != 0 {
		t.Fatalf("a matching registry reports findings: %+v", audit.Projects)
	}
	if !strings.Contains(human, "matches the corpus") {
		t.Errorf("the render does not say the store is clean:\n%s", human)
	}
}
