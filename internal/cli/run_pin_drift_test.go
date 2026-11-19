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
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-408 remedy 2: the read surfaces a conductor consults before attaching to
// a parked run — `run status` and `run resume` — state pin drift UNPROMPTED.
// RUN-35 died because the drift was discoverable only by running a separate
// verb the skill prose had to remember to mention.

// driftedRun builds a run whose one file pin no longer matches disk: the store
// is this test's own (t.Setenv beats the package TestMain's), the pinned file
// lives under its config root, and the disk copy has been replaced — the exact
// state a corpus install leaves a parked run in.
//
// The temp dirs are created BEFORE t.Setenv, per the autoregister tests'
// ordering note. Returns the run and the pinned (pre-drift) hash.
func driftedRun(t *testing.T, conn *sql.DB, status model.RunStatus) (*model.Run, string) {
	t.Helper()
	store := t.TempDir()
	configRoot := filepath.Join(store, "config")
	t.Setenv("DOCKET_PATH", store)

	run, err := db.InsertRun(conn, 1, "a run", 0, 1000)
	testsupport.Must(t, err, "InsertRun: %v", err)
	testsupport.Must(t, db.SetRunStatus(conn, run.ID, status, "", 1000), "SetRunStatus")

	ref := "contracts/synthesize-findings.md"
	path := filepath.Join(configRoot, ref)
	testsupport.Must(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir")
	testsupport.Must(t, os.WriteFile(path, []byte("BEFORE\n"), 0o644), "write")
	pinned := workflow.SHA256([]byte("BEFORE\n"))

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	testsupport.Must(t, db.InsertPinTx(tx, db.Pin{
		RunID: run.ID, Kind: db.PinKindFile, Ref: ref, SHA256: pinned,
	}), "InsertPinTx")
	testsupport.Must(t, tx.Commit(), "Commit")

	// The corpus install.
	testsupport.Must(t, os.WriteFile(path, []byte("AFTER\n"), 0o644), "rewrite")

	fresh, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	return fresh, pinned
}

// TestRunStatusStatesPinDrift: the single-run status view names the drift in
// its human output without being asked, so a parked run warns before a
// conduct session dispatches anything.
func TestRunStatusStatesPinDrift(t *testing.T) {
	conn := newTestDB(t)
	run, pinned := driftedRun(t, conn, model.RunWaitingHuman)

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	testsupport.Must(t, runRunStatus(cmd, []string{run.Ref()}, w), "run status")

	got := buf.String()
	for _, want := range []string{
		"Pin drift",
		"contracts/synthesize-findings.md",
		pinned,
		"run repin " + run.Ref(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output does not state %q unprompted:\n%s", want, got)
		}
	}
}

// TestRunStatusStaysQuietWhenPinsAreSound is the lower bound: a sound run's
// status must not cry drift, or nobody will believe the warning when it comes
// — and the sound output stays byte-compatible with the pre-DKT-408 shape.
func TestRunStatusStaysQuietWhenPinsAreSound(t *testing.T) {
	conn := newTestDB(t)
	run, _ := driftedRun(t, conn, model.RunActive)

	// Restore the pinned bytes: drift resolved, warning gone.
	path := filepath.Join(os.Getenv("DOCKET_PATH"),
		"config", "contracts", "synthesize-findings.md")
	testsupport.Must(t, os.WriteFile(path, []byte("BEFORE\n"), 0o644), "restore")

	cmd := cmdWithDB(conn)
	w, buf := bufWriter(false)
	testsupport.Must(t, runRunStatus(cmd, []string{run.Ref()}, w), "run status")
	if strings.Contains(buf.String(), "Pin drift") {
		t.Errorf("a sound run's status warns about drift:\n%s", buf.String())
	}
}

// TestRunResumeWarnsOnPinDrift: resuming is the moment a session attaches to a
// parked run — the resume succeeds (the operator may be resuming precisely in
// order to repin) and its output states the drift.
func TestRunResumeWarnsOnPinDrift(t *testing.T) {
	conn := newTestDB(t)
	run, _ := driftedRun(t, conn, model.RunWaitingHuman)

	cmd := cmdWithDB(conn)
	cmd.Use = "resume"
	cmd.Flags().String("reason", "", "")
	w, buf := bufWriter(false)

	err := moveRun(cmd, run.Ref(), runMove{
		to:   model.RunActive,
		from: []model.RunStatus{model.RunWaitingHuman},
		verb: "Resumed",
	}, w)
	testsupport.Must(t, err, "resume: %v", err)

	got := buf.String()
	for _, want := range []string{
		"Resumed " + run.Ref(),
		"Pin drift",
		"contracts/synthesize-findings.md",
		"run repin " + run.Ref(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("resume output does not state %q unprompted:\n%s", want, got)
		}
	}

	// The resume itself still happened.
	fresh, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if fresh.Status != model.RunActive {
		t.Errorf("run status = %s after resume, want active — the warning must "+
			"not block the transition", fresh.Status)
	}
}

// TestRunResumePayloadCarriesPinDrift pins the JSON channel: `pin_drift` rides
// beside the versioned run — the same shape runAbandonPayload uses for
// worktrees.
func TestRunResumePayloadCarriesPinDrift(t *testing.T) {
	run := &model.Run{ID: 7, Status: model.RunActive}
	drift := []engine.PinVerdict{{
		Kind: "file", Ref: "contracts/a.md", Status: engine.PinChanged,
		Pinned: "aaa", Found: "bbb",
	}}

	drifted, err := json.Marshal(runResumePayload{run: run, pinDrift: drift})
	testsupport.Must(t, err, "marshal: %v", err)
	for _, want := range []string{`"pin_drift"`, `"contracts/a.md"`, `"aaa"`, `"bbb"`, `"row_version"`} {
		if !strings.Contains(string(drifted), want) {
			t.Errorf("payload missing %s: %s", want, drifted)
		}
	}

	// And with no drift, the payload is the plain versioned run.
	plain, err := json.Marshal(runResumePayload{run: run})
	testsupport.Must(t, err, "plain marshal: %v", err)
	if strings.Contains(string(plain), "pin_drift") {
		t.Errorf("a sound resume's payload carries pin_drift: %s", plain)
	}
	if !strings.Contains(string(plain), `"row_version"`) {
		t.Errorf("a sound resume's payload lacks the run version: %s", plain)
	}
}
