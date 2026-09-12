package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// DKT-742 at the CLI boundary: `dispatch waive-target` records the waivers,
// reports them on both channels, and refuses garbage with the flag named.
// WHICH warnings a waiver then suppresses is the engine's to prove
// (internal/engine/stale_waiver_test.go); what this file asserts is the verb.

func waiveTargetCmdWithDB(conn *sql.DB, runRef, target, note string, steps []string) *cobra.Command {
	cmd := cmdWithDB(conn)
	cmd.Flags().String("run", runRef, "")
	cmd.Flags().StringSlice("step", steps, "")
	cmd.Flags().String("target", target, "")
	cmd.Flags().String("note", note, "")
	return cmd
}

func TestDispatchWaiveTargetCLIRecordsAndReports(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)
	runRef := model.FormatRunID(runID)

	w, buf := bufWriter(true)
	cmd := waiveTargetCmdWithDB(conn, runRef, "cafe1234cafe",
		"adjudicated: the divergence is the later format pass",
		[]string{"review@0#0", "review@0#1"})
	err := runDispatchWaiveTarget(cmd, w)
	testsupport.Must(t, err, "dispatch waive-target: %v", err)

	var env struct {
		Data struct {
			Run     string                `json:"run"`
			Waivers []engine.WaivedTarget `json:"waivers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.Data.Run != runRef || len(env.Data.Waivers) != 2 {
		t.Fatalf("payload = %+v, want two waivers on %s", env.Data, runRef)
	}
	for i, instance := range []string{"review@0#0", "review@0#1"} {
		wv := env.Data.Waivers[i]
		if wv.Instance != instance || wv.Target != "cafe1234cafe" || wv.ID == 0 {
			t.Errorf("waiver %d = %+v, want %s on cafe1234cafe with a real id",
				i, wv, instance)
		}
	}

	// The rows really landed, run-scoped.
	var count int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM stale_target_waivers WHERE run_id = ?`, runID,
	).Scan(&count)
	testsupport.Must(t, err, "counting waivers: %v", err)
	if count != 2 {
		t.Errorf("stale_target_waivers rows = %d, want 2", count)
	}
}

func TestDispatchWaiveTargetCLIHumanMessageNamesTheSteps(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)

	w, buf := bufWriter(false)
	cmd := waiveTargetCmdWithDB(conn, model.FormatRunID(runID),
		"cafe1234cafe", "", []string{"review@0#0"})
	err := runDispatchWaiveTarget(cmd, w)
	testsupport.Must(t, err, "dispatch waive-target: %v", err)

	out := buf.String()
	if !strings.Contains(out, "cafe1234cafe") || !strings.Contains(out, "review@0#0") {
		t.Errorf("the human message does not name the target and step:\n%s", out)
	}
}

func TestDispatchWaiveTargetCLIRefusesANonHexTarget(t *testing.T) {
	conn := newTestDB(t)
	runID := activatedDispatchRunForCLI(t, conn)

	w, _ := bufWriter(true)
	cmd := waiveTargetCmdWithDB(conn, model.FormatRunID(runID),
		"not-a-sha", "", []string{"review@0#0"})
	err := runDispatchWaiveTarget(cmd, w)
	if err == nil {
		t.Fatal("a non-hex target was accepted")
	}
	if !strings.Contains(err.Error(), "hex") {
		t.Errorf("the refusal %q does not say what a target must look like", err)
	}
}
