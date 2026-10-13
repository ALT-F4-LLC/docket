package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// DKT-609: `workflow list --orphans` — registered names that no file in any
// instance-config root declares any more.
//
// The state it exists for: a rename left four versions of the old name live in
// the store, and NOTHING IN ANY READ VERB SAID SO. `workflow list` showed them
// exactly as it showed the workflows that still had definitions, and the only
// way to tell the difference was to go and read the corpus's git history.

// renamedWorkflow is the definition that stays on disk. Its name differs from
// minimalWorkflow's ("unit"), which is what makes "unit" the stranded
// registration in these tests.
const renamedWorkflow = `
[pipeline]
name = "renamed"
version = 1
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
`

// configRootWith points DOCKET_PATH at a fresh store whose instance config
// holds exactly the workflow files named, and returns the store directory.
//
// THE TEMP DIR COMES FIRST, BEFORE t.Setenv — t.TempDir() reads TMPDIR, and a
// test that rewrote the environment before taking its directory would get a
// path that does not exist. The engine's own configRepo helper carries the
// same warning for the same reason.
func configRootWith(t *testing.T, files map[string]string) string {
	t.Helper()
	docketDir := filepath.Join(t.TempDir(), ".docket")
	workflows := filepath.Join(docketDir, "config", "workflows")
	testsupport.Must(t, os.MkdirAll(workflows, 0o755),
		"creating the instance-config workflows directory")
	for name, body := range files {
		testsupport.Must(t, os.WriteFile(filepath.Join(workflows, name), []byte(body), 0o644),
			"writing %s", name)
	}
	t.Setenv("DOCKET_PATH", docketDir)
	return docketDir
}

// listOrphans runs `workflow list --orphans` and returns the human render and
// the parsed JSON rows.
func listOrphans(t *testing.T, conn *sql.DB) (string, []orphanRow) {
	t.Helper()
	human := runOrphanList(t, conn, false)
	raw := runOrphanList(t, conn, true)

	var envelope struct {
		Data struct {
			Workflows []orphanRow `json:"workflows"`
			Total     int         `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if envelope.Data.Total != len(envelope.Data.Workflows) {
		// The Collection contract's `total` is the TRUE pre-limit count, and
		// under --orphans the population it counts is the orphans, not the
		// registry: a total that still counted workflows would report
		// truncation that never happened.
		t.Errorf("total = %d over %d listed orphans", envelope.Data.Total,
			len(envelope.Data.Workflows))
	}
	return human, envelope.Data.Workflows
}

type orphanRow struct {
	Name    string                      `json:"name"`
	Version int                         `json:"version"`
	Origin  *model.WorkflowOriginStatus `json:"origin"`
}

func runOrphanList(t *testing.T, conn *sql.DB, jsonMode bool) string {
	t.Helper()
	cmd := orphanListCmd(t, conn)
	w, buf := bufWriter(jsonMode)
	testsupport.Must(t, runWorkflowList(cmd, nil, w), "workflow list --orphans")
	return buf.String()
}

func orphanListCmd(t *testing.T, conn *sql.DB) *cobra.Command {
	t.Helper()
	cmd := workflowListCmdWithDB(conn, 50)
	testsupport.Must(t, cmd.Flags().Set("orphans", "true"), "setting --orphans")
	return cmd
}

// TestWorkflowListOrphansNamesTheStrandedRegistration is DKT-609's first
// acceptance criterion: the orphan is identifiable through a READ VERB, with no
// git archaeology, and the workflow that still has a file is not listed
// alongside it.
func TestWorkflowListOrphansNamesTheStrandedRegistration(t *testing.T) {
	conn := newTestDB(t)
	configRootWith(t, map[string]string{"renamed.toml": renamedWorkflow})
	testsupport.Must(t, registerSource(t, conn, minimalWorkflow), "registering unit@1")
	testsupport.Must(t, registerSource(t, conn, renamedWorkflow), "registering renamed@1")

	human, rows := listOrphans(t, conn)

	if len(rows) != 1 {
		t.Fatalf("listed %d orphans, want exactly 1 (unit@1): %+v", len(rows), rows)
	}
	if rows[0].Name != "unit" || rows[0].Version != 1 {
		t.Errorf("listed %s@%d, want unit@1 — the name whose file is gone",
			rows[0].Name, rows[0].Version)
	}
	// The row carries the VERDICT, not merely the fact that a flag was passed:
	// a JSON consumer can tell what was checked and where.
	if !rows[0].Origin.Orphaned() {
		t.Errorf("the listed row carries no orphan verdict: %+v", rows[0].Origin)
	}
	if len(rows[0].Origin.Roots) == 0 {
		t.Error("the verdict names no roots, so a reader cannot tell where the " +
			"name was looked for")
	}
	if !strings.Contains(human, "unit@1") || !strings.Contains(human, "[orphaned]") {
		t.Errorf("the human render does not mark the orphan: %q", human)
	}
	if strings.Contains(human, "renamed@1") {
		t.Errorf("the human render lists a workflow that still has a file: %q", human)
	}
}

// TestWorkflowListLeavesTheDefaultListingAlone: the orphan verdict costs a
// filesystem scan, so it is computed ONLY where it was asked for. An unmarked
// row in the default listing therefore means "not asked", never "checked and
// fine" — the rule `source_status` already follows, and the reason the default
// output is byte-identical to what it was.
func TestWorkflowListLeavesTheDefaultListingAlone(t *testing.T) {
	conn := newTestDB(t)
	configRootWith(t, map[string]string{"renamed.toml": renamedWorkflow})
	testsupport.Must(t, registerSource(t, conn, minimalWorkflow), "registering unit@1")

	cmd := workflowListCmdWithDB(conn, 50)
	w, buf := bufWriter(true)
	testsupport.Must(t, runWorkflowList(cmd, nil, w), "workflow list")
	if strings.Contains(buf.String(), "origin") {
		t.Errorf("the default listing carries an origin verdict nobody asked "+
			"for: %s", buf.String())
	}

	w, buf = bufWriter(false)
	testsupport.Must(t, runWorkflowList(cmd, nil, w), "workflow list")
	if strings.Contains(buf.String(), "[orphaned]") {
		t.Errorf("the default listing marks an orphan it never checked: %q", buf.String())
	}
}

// TestWorkflowListOrphansIncludesDeprecatedRows: a retired orphan is still
// listed, marked as retired.
//
// `workflow list` shows every registered version and marks the deprecated ones
// rather than hiding them, and --orphans follows that convention rather than
// diverging: the rows an operator is mid-way through cleaning up are exactly
// the ones a cleanup verb must keep showing, and hiding them would make the
// verb unable to show its own work.
func TestWorkflowListOrphansIncludesDeprecatedRows(t *testing.T) {
	conn := newTestDB(t)
	configRootWith(t, map[string]string{"renamed.toml": renamedWorkflow})
	testsupport.Must(t, registerSource(t, conn, minimalWorkflow), "registering unit@1")
	_, err := db.DeprecateWorkflow(conn, 1, "unit", 1, model.NowMS())
	testsupport.Must(t, err, "deprecating unit@1: %v", err)

	human, rows := listOrphans(t, conn)
	if len(rows) != 1 {
		t.Fatalf("listed %d orphans, want 1: a retired orphan is still an "+
			"orphan, and is what a cleanup pass wants to see", len(rows))
	}
	if !strings.Contains(human, "[orphaned]") || !strings.Contains(human, "[deprecated]") {
		t.Errorf("the render does not carry both facts: %q", human)
	}
}

// TestWorkflowListOrphansRefusesWithNothingToScan: with no instance-config
// root, EVERY registered name trivially has no file in any root. Rendering
// that as a list of orphans would be the verb inventing its whole result out
// of having looked nowhere, so it refuses instead — VALIDATION, the same code
// every other "this invocation cannot answer that" refusal carries.
func TestWorkflowListOrphansRefusesWithNothingToScan(t *testing.T) {
	conn := newTestDB(t)
	// A store directory with NO config/ subdirectory: the dormancy case.
	docketDir := filepath.Join(t.TempDir(), ".docket")
	testsupport.Must(t, os.MkdirAll(docketDir, 0o755), "creating the store directory")
	t.Setenv("DOCKET_PATH", docketDir)
	testsupport.Must(t, registerSource(t, conn, minimalWorkflow), "registering unit@1")

	cmd := orphanListCmd(t, conn)
	w, _ := bufWriter(true)
	err := runWorkflowList(cmd, nil, w)
	if err == nil {
		t.Fatal("--orphans reported orphans with no instance-config root to " +
			"scan; every registration in the store would qualify")
	}
	if got := codeOf(t, err); got != output.ErrValidation {
		t.Errorf("error code = %q, want %q", got, output.ErrValidation)
	}
}
