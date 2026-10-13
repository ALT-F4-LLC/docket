package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
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

// liveWaveStep opens the wave and claims its first row without completing it,
// so the step is non-terminal when the back-fill runs.
func liveWaveStep(t *testing.T, conn *sql.DB, runID int) int {
	t.Helper()
	manifest, err := engine.NewEngine().OpenDispatch(conn, runID, 0, nil, model.NowMS())
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(manifest.Rows) == 0 {
		t.Fatal("premise: the manifest must have a row to claim")
	}
	stepID, err := model.ParseStepID(manifest.Rows[0].Step)
	testsupport.Must(t, err, "parsing %q: %v", manifest.Rows[0].Step, err)
	_, err = engine.ClaimStep(conn, stepID, engine.ClaimOptions{
		Owner: "worker", NowMS: model.NowMS(),
	})
	testsupport.Must(t, err, "claim: %v", err)
	return stepID
}

// TestBackfillFromJSONDropsALiveStepsBagAndNamesItInThePayload pins the JSON
// surface: the batch succeeds, and the dropped step is named under
// metadata_dropped, since a JSON consumer never sees the stderr warning.
func TestBackfillFromJSONDropsALiveStepsBagAndNamesItInThePayload(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	stepID := liveWaveStep(t, conn, runID)
	path := usageJSON(t, fmt.Sprintf(
		`[{"step":%q,"unit":"tokens","quantity":900,"metadata":{"model_resolved":"x"}}]`,
		model.FormatStepID(stepID)))

	cmd := backfillCmdWithDB(conn, model.FormatRunID(runID))
	testsupport.Must(t, cmd.Flags().Set("from-json", path),
		"setting --from-json: %v", nil)
	w, buf := bufWriter(true)

	testsupport.Must(t, runDispatchBackfillUsage(cmd, w),
		"backfill-usage on a live step: %v", nil)
	if !strings.Contains(buf.String(), `"metadata_dropped"`) {
		t.Errorf("payload %s does not carry metadata_dropped", buf.String())
	}
	if !strings.Contains(buf.String(), model.FormatStepID(stepID)) {
		t.Errorf("payload %s does not name step %s", buf.String(),
			model.FormatStepID(stepID))
	}
}

// TestWarnDroppedMetadataNamesTheStepOnStderr pins the human surface: the drop
// is named on stderr with the step's status, and says the usage row is recorded.
func TestWarnDroppedMetadataNamesTheStepOnStderr(t *testing.T) {
	stderr := &bytes.Buffer{}
	w := &output.Writer{Stdout: &bytes.Buffer{}, Stderr: stderr}

	warnDroppedMetadata(w, []engine.DroppedMetadataTarget{{
		Step: "STEP-9", Instance: "implement@0", Status: "pending",
	}})

	got := stderr.String()
	for _, want := range []string{"STEP-9", "pending", "dropped", "recorded"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q does not contain %q", got, want)
		}
	}
}
