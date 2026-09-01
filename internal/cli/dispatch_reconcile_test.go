package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// `dispatch close --backfill-from` — DKT-580's one-verb wave reconcile at the
// CLI edge: the flag's parsing, its per-stage refusals, and the promise that
// the flagless close is untouched.

// dispatchCloseCmdWithDB builds a `dispatch close` command carrying every flag
// the real one declares, so a test exercises the same GetString/GetBool
// lookups runDispatchClose makes.
func dispatchCloseCmdWithDB(conn *sql.DB, runRef string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("run", runRef, "")
	cmd.Flags().Bool("accept-missing-usage", false, "")
	cmd.Flags().String("backfill-from", "", "")
	cmd.Flags().String("on-duplicate", "refuse", "")
	cmd.Flags().String("source", "", "")
	return cmd
}

// waveWithUnreportedUsage is the state every wave close starts from: a
// dispatch is open and its one claimable step finished WITHOUT reporting
// usage, which is what an executor that cannot observe its own spend does.
// It returns the manifest's step id.
func waveWithUnreportedUsage(t *testing.T, conn *sql.DB, runID int) int {
	t.Helper()

	manifest, err := engine.NewEngine().OpenDispatch(conn, runID, 0, nil, model.NowMS())
	testsupport.Must(t, err, "dispatch open: %v", err)
	if len(manifest.Rows) == 0 {
		t.Fatal("premise: the manifest must have a row to complete")
	}
	stepID, err := model.ParseStepID(manifest.Rows[0].Step)
	testsupport.Must(t, err, "parsing %q: %v", manifest.Rows[0].Step, err)

	claim, err := engine.ClaimStep(conn, stepID, engine.ClaimOptions{
		Owner: "worker", NowMS: model.NowMS(),
	})
	testsupport.Must(t, err, "claim: %v", err)
	err = engine.NewEngine().CompleteStep(conn, stepID, engine.CompleteOptions{
		Token: claim.Token, Artifact: []byte("done"), NowMS: model.NowMS(),
	})
	testsupport.Must(t, err, "complete: %v", err)
	return stepID
}

// usageJSON writes the batch a wave journal tool emits.
func usageJSON(t *testing.T, rows string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "usage.json")
	testsupport.Must(t, os.WriteFile(path, []byte(rows), 0o600),
		"writing the usage batch: %v", nil)
	return path
}

// TestDispatchCloseBackfillFromRunsAllThreeStages is criterion 1 at the CLI:
// one invocation, three stages, and a payload that reports each of them.
func TestDispatchCloseBackfillFromRunsAllThreeStages(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)
	stepID := waveWithUnreportedUsage(t, conn, runID)

	// The premise: the flagless close REFUSES here. Without that, the
	// back-fill stage running would be unobservable.
	bare := dispatchCloseCmdWithDB(conn, runRef)
	wBare, _ := bufWriter(true)
	if err := runDispatchClose(bare, wBare); err == nil {
		t.Fatal("premise: a plain close must refuse over the missing usage")
	}

	path := usageJSON(t, fmt.Sprintf(
		`[{"step":%q,"unit":"tokens","quantity":48211}]`,
		model.FormatStepID(stepID)))

	cmd := dispatchCloseCmdWithDB(conn, runRef)
	testsupport.Must(t, cmd.Flags().Set("backfill-from", path),
		"setting --backfill-from: %v", nil)
	testsupport.Must(t, cmd.Flags().Set("source", "wave-journal:wf-7"),
		"setting --source: %v", nil)

	w, buf := bufWriter(true)
	err := runDispatchClose(cmd, w)
	testsupport.Must(t, err, "dispatch close --backfill-from: %v", err)

	var env struct {
		Data engine.ReconcileOutcome `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.Data.Backfill == nil || env.Data.Backfill.Written != 1 {
		t.Fatalf("the payload's backfill stage is %+v, want 1 row written:\n%s",
			env.Data.Backfill, buf.String())
	}
	if env.Data.Backfill.Source != "wave-journal:wf-7" {
		t.Errorf("source = %q, want the one passed", env.Data.Backfill.Source)
	}
	if env.Data.Verify == nil || env.Data.Verify.Dispatch == "" {
		t.Errorf("the payload's verify stage names no dispatch:\n%s", buf.String())
	}
	if env.Data.Close == nil || env.Data.Close.Status == "" {
		t.Fatalf("the payload's close stage is %+v:\n%s", env.Data.Close, buf.String())
	}
}

// TestDispatchCloseUnchangedWithoutTheFlag is criterion 2: the flagless close
// still emits the plain CloseOutcome, not the reconcile's three-stage shape.
// A conductor parsing `.data.dispatch` must not have to learn a new payload
// because a flag it does not pass exists.
func TestDispatchCloseUnchangedWithoutTheFlag(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)
	stepID := waveWithUnreportedUsage(t, conn, runID)

	// Back-fill through the STANDALONE verb, exactly as before this change.
	_, err := engine.NewEngine().BackfillUsage(conn, runID, []engine.BackfillRow{
		{Step: stepID, Unit: "tokens", Quantity: 7},
	}, "", "", model.NowMS())
	testsupport.Must(t, err, "backfill-usage: %v", err)

	cmd := dispatchCloseCmdWithDB(conn, runRef)
	w, buf := bufWriter(true)
	testsupport.Must(t, runDispatchClose(cmd, w), "dispatch close: %v", nil)

	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if _, ok := env.Data["dispatch"]; !ok {
		t.Errorf("the plain close payload lost its `dispatch` key:\n%s", buf.String())
	}
	for _, key := range []string{"backfill", "verify", "close"} {
		if _, ok := env.Data[key]; ok {
			t.Errorf("the plain close payload grew a %q key; the reconcile "+
				"shape must ride on the flag only:\n%s", key, buf.String())
		}
	}
}

// TestDispatchCloseBackfillFromNamesTheFailedStage is the per-stage half of
// criterion 1: a back-fill that refuses names ITS OWN stage, exits on the
// stage's own code, and leaves the dispatch open — no partial close.
func TestDispatchCloseBackfillFromNamesTheFailedStage(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)
	waveWithUnreportedUsage(t, conn, runID)

	// A step that does not exist: the back-fill resolves every step before it
	// writes anything, so this refuses NOT_FOUND with nothing written.
	path := usageJSON(t, `[{"step":"STEP-999999","unit":"tokens","quantity":1}]`)

	cmd := dispatchCloseCmdWithDB(conn, runRef)
	testsupport.Must(t, cmd.Flags().Set("backfill-from", path),
		"setting --backfill-from: %v", nil)

	w, _ := bufWriter(true)
	err := runDispatchClose(cmd, w)
	if err == nil {
		t.Fatal("the close succeeded over a back-fill naming a missing step")
	}
	if !strings.Contains(err.Error(), engine.StageBackfill+" stage") {
		t.Errorf("the refusal %q does not name the stage that failed", err)
	}
	var cerr *CmdError
	if !errors.As(err, &cerr) {
		t.Fatalf("error %v carries no exit code", err)
	}
	if cerr.Code != output.ErrNotFound {
		t.Errorf("code = %q, want %q — a staged failure keeps the stage's own "+
			"taxonomy code", cerr.Code, output.ErrNotFound)
	}

	// The dispatch is untouched: a later `dispatch close` still has something
	// to close, which it would not if the failure had half-closed the run.
	var status string
	testsupport.Must(t, conn.QueryRow(
		`SELECT status FROM dispatches WHERE run_id = ? ORDER BY id DESC LIMIT 1`,
		runID).Scan(&status), "reading the dispatch row: %v", nil)
	if status != "open" {
		t.Errorf("the dispatch is %q after a failed back-fill stage, want open", status)
	}
}

// TestDispatchCloseBackfillFromRefusesADirectory pins the diagnostic for the
// other half of DKT-580's flag description. Core reads usage rows, not
// transcript trees, and the refusal must say so and name the way across rather
// than failing as an unreadable file.
func TestDispatchCloseBackfillFromRefusesADirectory(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)
	waveWithUnreportedUsage(t, conn, runID)

	cmd := dispatchCloseCmdWithDB(conn, runRef)
	testsupport.Must(t, cmd.Flags().Set("backfill-from", t.TempDir()),
		"setting --backfill-from: %v", nil)

	w, _ := bufWriter(true)
	err := runDispatchClose(cmd, w)
	if err == nil {
		t.Fatal("a directory was accepted as a usage batch")
	}
	if !strings.Contains(err.Error(), "is a directory") ||
		!strings.Contains(err.Error(), "wave-usage") {
		t.Errorf("the refusal %q neither names the mistake nor the way out", err)
	}
	var cerr *CmdError
	if !errors.As(err, &cerr) || cerr.Code != output.ErrValidation {
		t.Errorf("code = %v, want %q", cerr, output.ErrValidation)
	}
}

// TestDispatchCloseDeclaresTheReconcileFlags walks the REAL cobra command, not
// a test-built stand-in: every other test here constructs its own flag set, so
// without this one a flag could be missing from `init()` and the suite would
// still be green while the shipped binary had no `--backfill-from`.
//
// It also pins criterion 2 at the surface: `backfill-usage` keeps every flag it
// had, and `close` gained exactly three.
func TestDispatchCloseDeclaresTheReconcileFlags(t *testing.T) {
	for _, name := range []string{"backfill-from", "on-duplicate", "source",
		"accept-missing-usage", "run"} {
		if dispatchCloseCmd.Flags().Lookup(name) == nil {
			t.Errorf("`dispatch close` declares no --%s", name)
		}
	}
	// The standalone verb is untouched.
	for _, name := range []string{"step", "unit", "quantity", "from-json",
		"on-duplicate", "source", "run"} {
		if dispatchBackfillUsageCmd.Flags().Lookup(name) == nil {
			t.Errorf("`dispatch backfill-usage` lost --%s", name)
		}
	}
	// And `verify` did not quietly acquire the reconcile's flags: the stages
	// stay separately invocable, they do not merge.
	if dispatchVerifyCmd.Flags().Lookup("backfill-from") != nil {
		t.Error("`dispatch verify` declares --backfill-from; the flag belongs " +
			"to close alone")
	}
	if got := dispatchCloseCmd.Flags().Lookup("on-duplicate").DefValue; got != "refuse" {
		t.Errorf("close's --on-duplicate defaults to %q, want %q — it must "+
			"mean what it means on backfill-usage", got, "refuse")
	}
}

// TestDispatchCloseBackfillFromReadsStdin pins the "-" form, which is what a
// conductor piping a journal tool's output uses — the temp file between two
// commands is one of the drift surfaces DKT-580 is about.
func TestDispatchCloseBackfillFromReadsStdin(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)
	stepID := waveWithUnreportedUsage(t, conn, runID)

	cmd := dispatchCloseCmdWithDB(conn, runRef)
	testsupport.Must(t, cmd.Flags().Set("backfill-from", "-"),
		"setting --backfill-from: %v", nil)
	cmd.SetIn(strings.NewReader(fmt.Sprintf(
		`[{"step":%q,"unit":"tokens","quantity":900}]`,
		model.FormatStepID(stepID))))

	w, buf := bufWriter(true)
	testsupport.Must(t, runDispatchClose(cmd, w),
		"dispatch close --backfill-from -: %v", nil)

	var env struct {
		Data engine.ReconcileOutcome `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.Data.Backfill == nil || env.Data.Backfill.Written != 1 {
		t.Fatalf("stdin batch wrote %+v, want 1 row:\n%s",
			env.Data.Backfill, buf.String())
	}
	if env.Data.Close == nil {
		t.Fatalf("the close stage did not run:\n%s", buf.String())
	}
}
