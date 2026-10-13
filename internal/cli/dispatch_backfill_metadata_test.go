package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// TestBackfillFromJSONReadsTheMetadataBag pins the reader's half of the
// metadata contract. An absent or null bag is no bag. An object reaches the
// engine byte for byte. A non-object reaches the engine too, which refuses it:
// the reader does not decide the shape itself.
func TestBackfillFromJSONReadsTheMetadataBag(t *testing.T) {
	step := model.FormatStepID(1)
	cases := []struct {
		name        string
		metadata    string // the row's "metadata" value; "" omits the key
		want        string
		wantRefused bool
	}{
		{name: "absent", metadata: "", want: ""},
		{name: "null", metadata: "null", want: ""},
		{name: "object", metadata: `{"model_resolved":"x"}`,
			want: `{"model_resolved":"x"}`},
		{name: "number", metadata: "3", want: "3", wantRefused: true},
		{name: "string", metadata: `"x"`, want: `"x"`, wantRefused: true},
		{name: "array", metadata: `[1]`, want: `[1]`, wantRefused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := fmt.Sprintf(`{"step":%q,"unit":"tokens","quantity":1`, step)
			if tc.metadata != "" {
				row += `,"metadata":` + tc.metadata
			}
			path := usageJSON(t, "["+row+"}]")

			rows, err := backfillRowsFromJSON(&cobra.Command{}, path)
			testsupport.Must(t, err, "reading the batch: %v", err)
			if len(rows) != 1 {
				t.Fatalf("read %d rows, want 1", len(rows))
			}
			if got := rows[0].Metadata; got != tc.want {
				t.Errorf("Metadata = %q, want %q", got, tc.want)
			}
			_, err = engine.DecodeMetadataBag(rows[0].Metadata, "metadata")
			if refused := err != nil; refused != tc.wantRefused {
				t.Errorf("engine refused = %v (err %v), want %v",
					refused, err, tc.wantRefused)
			}
		})
	}
}

// TestBackfillFromJSONRefusesANonObjectBagInTheEngine is the engine's half: a
// non-object bag on a finished step is refused, and the refusal writes no
// usage row.
func TestBackfillFromJSONRefusesANonObjectBagInTheEngine(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	stepID := waveWithUnreportedUsage(t, conn, runID)
	path := usageJSON(t, fmt.Sprintf(
		`[{"step":%q,"unit":"tokens","quantity":900,"metadata":[1]}]`,
		model.FormatStepID(stepID)))

	cmd := backfillCmdWithDB(conn, model.FormatRunID(runID))
	testsupport.Must(t, cmd.Flags().Set("from-json", path),
		"setting --from-json: %v", nil)
	w, _ := bufWriter(true)

	err := runDispatchBackfillUsage(cmd, w)
	if err == nil {
		t.Fatal("a non-object metadata bag was accepted")
	}
	if !strings.Contains(err.Error(), "must be a JSON object") {
		t.Errorf("refusal %q does not name the metadata shape", err)
	}

	var n int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM usage_ledger WHERE step_id = ?`, stepID).Scan(&n)
	testsupport.Must(t, err, "reading usage_ledger: %v", err)
	if n != 0 {
		t.Errorf("ledger has %d rows, want 0 — a refused bag must not write", n)
	}
}
