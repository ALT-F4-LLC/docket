package verb_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

const registryInvestigationV8 = `
[pipeline]
name = "investigation"
version = 8
[[step]]
name = "look"
after = []
executor = "w"
emits = "out"
`

// registryCorpus points DOCKET_PATH at a store root whose shared config holds
// one workflow, investigation@8. The temp dir is taken before t.Setenv, since
// t.TempDir reads TMPDIR.
func registryCorpus(t *testing.T) {
	t.Helper()
	docketDir := filepath.Join(t.TempDir(), ".docket")
	path := filepath.Join(docketDir, "config", "workflows", "investigation.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(registryInvestigationV8), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	t.Setenv("DOCKET_PATH", docketDir)
}

func registerWorkflow(t *testing.T, st store.Store, projectID int, name string, version int) {
	t.Helper()
	body := "bytes for " + name
	if _, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: projectID, Name: name, Version: version,
		SourcePath: "/nowhere/" + name + ".toml", SourceSHA256: workflow.SHA256([]byte(body)),
		Body: body, Parsed: "{}",
	}, 1); err != nil {
		t.Fatalf("registering %s@%d: %v", name, version, err)
	}
}

// The --project ref narrows the store-wide audit to one project, and the
// caller's own unregistered identity is never registered: the audit is a read.
func TestRegistryAuditNarrowsToTheNamedProjectWithoutRegisteringTheCaller(t *testing.T) {
	registryCorpus(t)
	st := openStore(t)
	stale := ensureProject(t, st, "/repo/stale.git", "stale.git")
	other := ensureProject(t, st, "/repo/other.git", "other.git")
	registerWorkflow(t, st, stale, "investigation", 4)
	registerWorkflow(t, st, other, "investigation", 4)

	env := projectEnv(st, "/repo/never-registered", true)
	got, err := verb.RegistryAudit(context.Background(), env, verb.RegistryAuditInput{Project: "stale.git"})
	if err != nil {
		t.Fatalf("RegistryAudit: %v", err)
	}

	audit := got.Audit
	if len(audit.Projects) != 1 || audit.Projects[0].ProjectID != stale {
		t.Fatalf("projects = %+v, want only project %d", audit.Projects, stale)
	}
	behind := audit.Projects[0].Behind
	if len(behind) != 1 || behind[0].Name != "investigation" ||
		behind[0].RegisteredVersion != 4 || behind[0].CurrentVersion != 8 {
		t.Errorf("behind = %+v, want investigation registered 4, current 8", behind)
	}
	if audit.BehindTotal != 1 {
		t.Errorf("behind total = %d, want 1", audit.BehindTotal)
	}
	requireUnregistered(t, st, "/repo/never-registered")
}

func TestRegistryAuditRefusesAnUnknownProjectRef(t *testing.T) {
	registryCorpus(t)
	st := openStore(t)
	_, err := verb.RegistryAudit(context.Background(), projectEnv(st, "", false),
		verb.RegistryAuditInput{Project: "999"})
	requireVerbCode(t, err, output.ErrNotFound, "no project with id 999")
}

// With no shared config directory every registered name would look orphaned,
// so the audit refuses instead of reporting.
func TestRegistryAuditRefusesWhenTheSharedRootHasNoConfig(t *testing.T) {
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	st := openStore(t)
	_, err := verb.RegistryAudit(context.Background(), projectEnv(st, "", false), verb.RegistryAuditInput{})
	requireVerbCode(t, err, output.ErrValidation, "the shared store root has no config directory")
}
