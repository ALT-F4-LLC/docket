package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
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

// TestDoctorEnvelopeShapesTheSixChecks drives the real command against an
// empty in-memory store outside a git repository, and asserts the wire shape
// AC1 promises: six rows, each carrying check/verdict/detail, plus clean and
// skipped.
func TestDoctorEnvelopeShapesTheSixChecks(t *testing.T) {
	conn := newTestDB(t)
	dbPath := t.TempDir() + "/issues.db"
	if f, err := os.Create(dbPath); err != nil {
		t.Fatalf("seeding %s: %v", dbPath, err)
	} else {
		f.Close()
	}
	cfg := &config.Config{DBPath: dbPath}
	w, buf := bufWriter(true)

	err := runDoctor(doctorCmdWithDB(conn, cfg), w)
	if err != nil {
		t.Fatalf("doctor returned an error: %v", err)
	}

	var envelope struct {
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
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}

	want := []string{"seat", "store", "install-drift", "pins", "link-farm", "stragglers"}
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
