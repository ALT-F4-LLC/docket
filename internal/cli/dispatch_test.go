package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// dispatch verify has no CLI-level test pre-fix, so the surface where DKT-10
// was originally reported stayed unasserted; these exercise runDispatchVerify
// directly: exit code, --json rendering, and a mismatch's rendered content.

func dispatchCmdWithDB(conn *sql.DB, runRef string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("run", runRef, "")
	cmd.Flags().Int("limit", 0, "")
	cmd.Flags().Int64Slice("ack-reap", nil, "")
	return cmd
}

func activatedDispatchRunForCLI(t *testing.T, conn *sql.DB) int {
	t.Helper()
	runID, _ := seedRun(t, conn)
	if _, err := engine.Activate(conn, runID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	return runID
}

func TestDispatchVerifyCLIEqualExitsClean(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)

	_, err := engine.NewEngine().OpenDispatch(conn, runID, 0, nil, model.NowMS())
	testsupport.Must(t, err, "dispatch open: %v", err)

	w, buf := bufWriter(true)
	err = runDispatchVerify(dispatchCmdWithDB(conn, runRef), w)
	testsupport.Must(t, err, "dispatch verify: %v", err)

	var env struct {
		Data struct {
			Verified bool   `json:"verified"`
			Dispatch string `json:"dispatch"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if !env.Data.Verified {
		t.Errorf("verified = false on an untouched manifest:\n%s", buf.String())
	}
	if env.Data.Dispatch == "" {
		t.Errorf("the payload does not name the dispatch:\n%s", buf.String())
	}
}

func TestDispatchVerifyCLIConflictRendersRowsAndExitCode(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)

	manifest, err := engine.NewEngine().OpenDispatch(conn, runID, 0, nil, model.NowMS())
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(manifest.Rows) == 0 {
		t.Fatal("premise: the manifest must have rows")
	}

	stepID, err := model.ParseStepID(manifest.Rows[0].Step)
	testsupport.Must(t, err, "parsing %q: %v", manifest.Rows[0].Step, err)
	_, err = engine.ClaimStep(conn, stepID, engine.ClaimOptions{
		Owner: "worker", NowMS: model.NowMS(),
	})
	testsupport.Must(t, err, "claim: %v", err)

	w, buf := bufWriter(true)
	err = runDispatchVerify(dispatchCmdWithDB(conn, runRef), w)
	if err == nil {
		t.Fatal("a claimed manifest row verified clean")
	}
	cerr, ok := err.(*CmdError)
	if !ok {
		t.Fatalf("error is %T, want *CmdError: %v", err, err)
	}
	if cerr.Code != output.ErrConflict {
		t.Errorf("code = %q, want %q", cerr.Code, output.ErrConflict)
	}
	if buf.Len() != 0 {
		t.Errorf("a refused verify wrote to the success channel: %s", buf.String())
	}

	got := cerr.Error()
	// A claimed (not completed) step is still NON-TERMINAL and yet no longer
	// ready, so it vanishes from the recomputed set entirely — the
	// missing-row branch, rendered as an explicit absence rather than a
	// blank line, and the case renderRowOrAbsent's doc now calls out as the
	// alarming one (a completed step never reaches here at all).
	for _, want := range []string{
		"manifest row 0", manifest.Rows[0].Instance, "(no row at this position)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal %q does not mention %q", got, want)
		}
	}
}

// ---- DKT-993: the never-claimed back-fill target ---------------------------

// backfillCmdWithDB builds a `dispatch backfill-usage` command carrying every
// flag the real one declares, so the test exercises the same flag lookups
// runDispatchBackfillUsage makes.
func backfillCmdWithDB(conn *sql.DB, runRef string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("run", runRef, "")
	cmd.Flags().StringSlice("step", nil, "")
	cmd.Flags().StringSlice("unit", nil, "")
	cmd.Flags().Float64Slice("quantity", nil, "")
	cmd.Flags().String("from-json", "", "")
	cmd.Flags().String("on-duplicate", "refuse", "")
	cmd.Flags().String("source", "", "")
	return cmd
}

// stderrWriter is bufWriter with the warning channel visible: DKT-993's whole
// subject is what reaches stderr, and bufWriter discards it.
func stderrWriter(jsonMode bool) (*output.Writer, *bytes.Buffer, *bytes.Buffer) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	return &output.Writer{JSONMode: jsonMode, Stdout: stdout, Stderr: stderr},
		stdout, stderr
}

// pendingStepID returns a step of the run that nothing has ever claimed —
// `second@0` in the CLI fixture, the shape RUN-66's STEP-3146 had.
func pendingStepID(t *testing.T, conn *sql.DB, runID int) int {
	t.Helper()
	var id int
	err := conn.QueryRow(
		`SELECT id FROM steps WHERE run_id = ? AND status = 'pending' AND attempt = 0
		  ORDER BY id LIMIT 1`, runID).Scan(&id)
	testsupport.Must(t, err, "finding a never-claimed step: %v", err)
	return id
}

// TestBackfillUsageCLIWarnsOnANeverClaimedStep is DKT-993's acceptance at the
// edge the operator actually reads: the warning is PRINTED, it names the step
// and its status, and the row is still recorded.
func TestBackfillUsageCLIWarnsOnANeverClaimedStep(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	stepID := pendingStepID(t, conn, runID)

	cmd := backfillCmdWithDB(conn, model.FormatRunID(runID))
	testsupport.Must(t, cmd.Flags().Set("step", model.FormatStepID(stepID)),
		"setting --step: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("unit", "tokens"), "setting --unit: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("quantity", "8724"),
		"setting --quantity: %v", nil)

	w, stdout, stderr := stderrWriter(false)
	testsupport.Must(t, runDispatchBackfillUsage(cmd, w),
		"backfill-usage: %v", nil)

	warning := stderr.String()
	for _, want := range []string{
		"Warning", model.FormatStepID(stepID), "pending", "never claimed",
	} {
		if !strings.Contains(warning, want) {
			t.Errorf("stderr %q does not mention %q", warning, want)
		}
	}

	// Warn-and-record: the verb still reports the row it wrote.
	if !strings.Contains(stdout.String(), "1 usage row") {
		t.Errorf("stdout %q does not report the recorded row; the contract is "+
			"warn-and-record, not reject", stdout.String())
	}
}

// TestBackfillUsageCLIJSONCarriesUnclaimed is the other half of the channel
// rule: `Warn` is suppressed in JSON mode, and a conductor runs with `--json`
// — an advisory only a human could see is the same silence DKT-993 ends.
func TestBackfillUsageCLIJSONCarriesUnclaimed(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	stepID := pendingStepID(t, conn, runID)

	cmd := backfillCmdWithDB(conn, model.FormatRunID(runID))
	testsupport.Must(t, cmd.Flags().Set("step", model.FormatStepID(stepID)),
		"setting --step: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("unit", "tokens"), "setting --unit: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("quantity", "18"),
		"setting --quantity: %v", nil)

	w, stdout, _ := stderrWriter(true)
	testsupport.Must(t, runDispatchBackfillUsage(cmd, w),
		"backfill-usage: %v", nil)

	var env struct {
		Data struct {
			Rows      int                      `json:"rows"`
			Unclaimed []engine.UnclaimedTarget `json:"unclaimed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if env.Data.Rows != 1 {
		t.Errorf("rows = %d, want 1 — the row is recorded", env.Data.Rows)
	}
	if len(env.Data.Unclaimed) != 1 ||
		env.Data.Unclaimed[0].Step != model.FormatStepID(stepID) ||
		env.Data.Unclaimed[0].Status != "pending" {
		t.Fatalf("`unclaimed` = %+v, want one entry naming %s as pending:\n%s",
			env.Data.Unclaimed, model.FormatStepID(stepID), stdout.String())
	}
}

// TestBackfillUsageCLIQuietForAClaimedStep is the "existing valid back-fills
// unchanged" half at the CLI: no warning, and a payload with no `unclaimed`
// key at all — a conductor parsing this shape must not have to learn a new one.
func TestBackfillUsageCLIQuietForAClaimedStep(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	stepID := waveWithUnreportedUsage(t, conn, runID)

	cmd := backfillCmdWithDB(conn, model.FormatRunID(runID))
	testsupport.Must(t, cmd.Flags().Set("step", model.FormatStepID(stepID)),
		"setting --step: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("unit", "tokens"), "setting --unit: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("quantity", "48211"),
		"setting --quantity: %v", nil)

	w, stdout, stderr := stderrWriter(true)
	testsupport.Must(t, runDispatchBackfillUsage(cmd, w),
		"backfill-usage: %v", nil)

	if stderr.Len() != 0 {
		t.Errorf("a claimed step's back-fill warned: %q", stderr.String())
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout.String())
	}
	if _, ok := env.Data["unclaimed"]; ok {
		t.Errorf("an ordinary back-fill's payload grew an `unclaimed` key:\n%s",
			stdout.String())
	}
}
