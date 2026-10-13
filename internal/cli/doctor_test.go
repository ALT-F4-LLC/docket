package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// doctorCmdWithDB is cmdWithDB plus a resolved config in context and doctor's
// own --run/--source flags, so a test drives the same flag reads the real
// command does. The config matters for the store check's DBPath — a bare
// cmdWithDB leaves getCfg(cmd) nil, unlike a real invocation where
// PersistentPreRunE always resolves one first.
func doctorCmdWithDB(conn *sql.DB, cfg *config.Config) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("run", "", "")
	cmd.Flags().String("source", "", "")
	cmd.SetContext(context.WithValue(cmd.Context(), cfgKey, cfg))
	return cmd
}

// `docket doctor` — DKT-1285.

// TestDoctorCmdRegistersRunAndSourceFlags pins the flag surface the Long text
// documents: --run (optional; pins SKIPs without it) and --source (optional;
// install-drift SKIPs without it).
func TestDoctorCmdRegistersRunAndSourceFlags(t *testing.T) {
	for _, name := range []string{"run", "source"} {
		if doctorCmd.Flags().Lookup(name) == nil {
			t.Errorf("doctor is missing the --%s flag", name)
		}
	}
}

// doctorEnvelope is doctor's `--json` data, as a test reads it back.
type doctorEnvelope struct {
	Data struct {
		Clean   bool `json:"clean"`
		Skipped bool `json:"skipped"`
		Checks  []struct {
			Check   string `json:"check"`
			Verdict string `json:"verdict"`
			Detail  string `json:"detail"`
		} `json:"checks"`
	} `json:"data"`
}

// seededDBPath is an empty file standing in for the store on disk, so the
// store check has something to open read-write.
func seededDBPath(t *testing.T) string {
	t.Helper()
	dbPath := t.TempDir() + "/issues.db"
	f, err := os.Create(dbPath)
	if err != nil {
		t.Fatalf("seeding %s: %v", dbPath, err)
	}
	f.Close()
	return dbPath
}

// TestDoctorEnvelopeShapesTheSevenChecks drives the real command against an
// empty in-memory store outside a git repository, and asserts the wire shape
// AC1 promises: seven rows, each carrying check/verdict/detail, plus clean and
// skipped.
func TestDoctorEnvelopeShapesTheSevenChecks(t *testing.T) {
	conn := newTestDB(t)
	cfg := &config.Config{DBPath: seededDBPath(t)}
	w, buf := bufWriter(true)

	err := runDoctor(doctorCmdWithDB(conn, cfg), w)
	if err != nil {
		t.Fatalf("doctor returned an error: %v", err)
	}

	var envelope doctorEnvelope
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	want := []string{"seat", "store", "project", "install-drift", "pins", "link-farm", "stragglers"}
	if len(envelope.Data.Checks) != len(want) {
		t.Fatalf("checks = %v, want %d rows", envelope.Data.Checks, len(want))
	}
	for i, name := range want {
		if envelope.Data.Checks[i].Check != name {
			t.Errorf("checks[%d] = %q, want %q", i, envelope.Data.Checks[i].Check, name)
		}
		if envelope.Data.Checks[i].Verdict == "" {
			t.Errorf("checks[%d] (%s) has no verdict", i, name)
		}
	}

	// No --run and no --source: both AC2's SKIP and clean/skipped follow.
	if !envelope.Data.Skipped {
		t.Error("skipped = false with neither --run nor --source given, want true")
	}
	if envelope.Data.Clean {
		t.Error("clean = true with checks SKIPped, want false")
	}

	// The human-mode rendering carries the same summary.
	textW, out := bufWriter(false)
	if err := runDoctor(doctorCmdWithDB(conn, cfg), textW); err != nil {
		t.Fatalf("doctor (human mode) returned an error: %v", err)
	}
	if !strings.Contains(out.String(), "clean: false") {
		t.Errorf("human output %q does not report clean: false", out.String())
	}
}

// TestDoctorReportsAnUnboundCwdWithoutRegistering is the measured defect: from
// a git worktree whose identity has no project row, `docket doctor` once
// claimed the store's default row for that identity and logged a
// project-registered event with verb doctor. It resolves the project the way
// the root hook does for the real `doctor` command, then runs doctor, and
// asserts the report says unbound while the projects table, the default row,
// and the event log are untouched.
func TestDoctorReportsAnUnboundCwdWithoutRegistering(t *testing.T) {
	conn := newTestDB(t)
	cfg := &config.Config{
		DBPath:   seededDBPath(t),
		Identity: "/repo/never-registered",
		Source:   config.SourceGlobal,
		Anchored: true,
	}
	projectsBefore := projectCount(t, conn)
	eventsBefore := eventRowCount(t, conn)
	defaultBefore := defaultProjectIdentity(t, conn)

	id, err := resolveInvocationProject(commandAt(t, "doctor"), conn, cfg)
	testsupport.Must(t, err, "resolving doctor's project: %v", err)
	if id != db.UnregisteredProjectID {
		t.Fatalf("doctor resolved project %d, want the unregistered sentinel %d",
			id, db.UnregisteredProjectID)
	}

	cmd := doctorCmdWithDB(conn, cfg)
	cmd.SetContext(context.WithValue(cmd.Context(), projectKey, id))
	w, buf := bufWriter(true)
	testsupport.Must(t, runDoctor(cmd, w), "doctor: %v", nil)

	var envelope doctorEnvelope
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	var found bool
	for _, c := range envelope.Data.Checks {
		if c.Check != "project" {
			continue
		}
		found = true
		if c.Verdict != "FAIL" {
			t.Errorf("project verdict = %s, want FAIL for an unbound cwd: %s", c.Verdict, c.Detail)
		}
		if !strings.Contains(c.Detail, cfg.Identity) {
			t.Errorf("project detail %q does not name the unbound identity", c.Detail)
		}
	}
	if !found {
		t.Fatalf("no project row in %+v", envelope.Data.Checks)
	}
	if envelope.Data.Clean {
		t.Error("clean = true from an unbound cwd, want false")
	}

	if got := projectCount(t, conn); got != projectsBefore {
		t.Errorf("projects: before %d, after %d; doctor registered one", projectsBefore, got)
	}
	if got := eventRowCount(t, conn); got != eventsBefore {
		t.Errorf("events: before %d, after %d; doctor logged one", eventsBefore, got)
	}
	if got := defaultProjectIdentity(t, conn); got != defaultBefore {
		t.Errorf("default project identity: before %q, after %q; doctor claimed the placeholder row",
			defaultBefore, got)
	}
}

func eventRowCount(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	testsupport.Must(t, err, "counting events: %v", err)
	return n
}

func defaultProjectIdentity(t *testing.T, conn *sql.DB) string {
	t.Helper()
	var identity string
	err := conn.QueryRow(`SELECT identity FROM projects WHERE id = ?`, db.DefaultProjectID).Scan(&identity)
	testsupport.Must(t, err, "reading the default project: %v", err)
	return identity
}
