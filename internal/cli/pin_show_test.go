package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

func equalActivatedAt(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// DKT-2026 — a step's packet lists `policy.toml <sha256>` and nothing else, so a
// contract requiring the pinned text itself had no verb to read it. These pin
// the read: the PINNED bytes, refused when the run never pinned the path, and
// refused again when the file drifted after activation.

const pinShowWorkflow = `
[pipeline]
name = "pin-show"
version = 1

[match]
kind = ["feature"]

[[step]]
name = "judge"
executor = "judge"
emits = "change-summary"
after = []
packet = ["contracts/judge.md"]
`

const pinShowPolicy = "opaque = \"instance policy\"\n"

// pinShowFixture activates a run over a config tree holding `policy.toml`, and
// returns the config dir and the run ref.
func pinShowFixture(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	// THE TEMP DIR COMES FIRST, BEFORE t.Setenv — `t.TempDir()` reads TMPDIR.
	root := t.TempDir()
	configDir := filepath.Join(root, ".docket", "config")
	testsupport.Must(t, os.MkdirAll(filepath.Join(configDir, "workflows"), 0o755),
		"creating the config dir")
	testsupport.Must(t, os.MkdirAll(filepath.Join(configDir, "contracts"), 0o755),
		"creating the contracts dir")
	t.Setenv("DOCKET_PATH", filepath.Join(root, ".docket"))

	testsupport.Must(t, os.WriteFile(
		filepath.Join(configDir, "workflows/pin-show.toml"),
		[]byte(pinShowWorkflow), 0o644), "writing the workflow")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(configDir, "contracts/judge.md"),
		[]byte("the judge contract\n"), 0o644), "writing the contract")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(configDir, "policy.toml"),
		[]byte(pinShowPolicy), 0o644), "writing the policy")

	conn := newTestDB(t)
	issueID := createIssue(t, conn, "pin show", model.StatusBacklog, model.PriorityNone)
	run, err := db.InsertRun(conn, 1, "test run", 0, model.NowMS())
	testsupport.Must(t, err, "InsertRun: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "AddRunIssue")
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "Activate: %v", err)

	return conn, configDir, run.Ref()
}

// TestPinShowPrintsPinnedContent is AC1: the pinned bytes come back, and a path
// the run never pinned is refused by name.
func TestPinShowPrintsPinnedContent(t *testing.T) {
	t.Run("a pinned file prints", func(t *testing.T) {
		conn, _, runRef := pinShowFixture(t)

		w, buf := bufWriter(false)
		if err := runPinShow(cmdWithDB(conn), runRef, "policy.toml", w); err != nil {
			t.Fatalf("pin show refuses a pinned file: %v", err)
		}
		if got := buf.String(); !strings.Contains(got, pinShowPolicy) {
			t.Errorf("output = %q, want the pinned policy bytes %q", got, pinShowPolicy)
		}
	})

	t.Run("an unpinned path is refused by name", func(t *testing.T) {
		conn, configDir, runRef := pinShowFixture(t)
		testsupport.Must(t, os.WriteFile(
			filepath.Join(configDir, "unpinned.toml"),
			[]byte("never pinned\n"), 0o644), "writing an unpinned file")

		w, buf := bufWriter(false)
		err := runPinShow(cmdWithDB(conn), runRef, "unpinned.toml", w)
		if err == nil {
			t.Fatal("pin show printed a path the run never pinned")
		}
		cerr, ok := err.(*CmdError)
		if !ok {
			t.Fatalf("err = %T, want *CmdError", err)
		}
		if cerr.Code != output.ErrValidation {
			t.Errorf("code = %q, want %q", cerr.Code, output.ErrValidation)
		}
		if !strings.Contains(err.Error(), "unpinned.toml") {
			t.Errorf("err = %q, want it to name the path", err.Error())
		}
		if strings.Contains(buf.String(), "never pinned") {
			t.Errorf("output = %q, want no bytes printed for an unpinned path",
				buf.String())
		}
	})
}

// TestPinShowRefusesDriftedBytes is AC2: the printed bytes hash to the pin, so a
// file edited after activation refuses with both hashes rather than printing.
func TestPinShowRefusesDriftedBytes(t *testing.T) {
	conn, configDir, runRef := pinShowFixture(t)
	drifted := "opaque = \"edited after activation\"\n"
	testsupport.Must(t, os.WriteFile(
		filepath.Join(configDir, "policy.toml"), []byte(drifted), 0o644),
		"rewriting the policy after activation")

	w, buf := bufWriter(false)
	err := runPinShow(cmdWithDB(conn), runRef, "policy.toml", w)
	if err == nil {
		t.Fatal("pin show printed bytes that no longer match the pin")
	}
	if strings.Contains(buf.String(), drifted) {
		t.Errorf("output = %q, want the drifted bytes withheld", buf.String())
	}
	if !strings.Contains(err.Error(), "pin drift") {
		t.Errorf("err = %q, want it to say \"pin drift\"", err.Error())
	}
	runID, perr := model.ParseRunID(runRef)
	testsupport.Must(t, perr, "ParseRunID: %v", perr)
	pins, lerr := db.ListPins(conn, runID)
	testsupport.Must(t, lerr, "ListPins: %v", lerr)
	var pinned string
	for _, p := range pins {
		if p.Ref == "policy.toml" {
			pinned = p.SHA256
		}
	}
	if pinned == "" {
		t.Fatal("the fixture run does not pin policy.toml")
	}
	if !strings.Contains(err.Error(), pinned) {
		t.Errorf("err = %q, want the pinned hash %q", err.Error(), pinned)
	}
	if !strings.Contains(err.Error(), workflow.SHA256([]byte(drifted))) {
		t.Errorf("err = %q, want the on-disk hash too", err.Error())
	}
}

// TestPinShowWritesNothing is AC3: a read verb. No event, no change to the run
// row, and the verb is registered where `docket --help` lists it.
func TestPinShowWritesNothing(t *testing.T) {
	conn, _, runRef := pinShowFixture(t)
	runID, perr := model.ParseRunID(runRef)
	testsupport.Must(t, perr, "ParseRunID: %v", perr)

	before, err := engine.ListEvents(conn, engine.EventQuery{})
	testsupport.Must(t, err, "ListEvents: %v", err)
	runBefore, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)

	w, _ := bufWriter(false)
	testsupport.Must(t, runPinShow(cmdWithDB(conn), runRef, "policy.toml", w),
		"pin show refuses a pinned file")

	after, err := engine.ListEvents(conn, engine.EventQuery{})
	testsupport.Must(t, err, "ListEvents: %v", err)
	if after.Total != before.Total {
		t.Errorf("events = %d, want %d — pin show is a read", after.Total, before.Total)
	}
	runAfter, err := db.GetRun(conn, runID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if runAfter.Status != runBefore.Status ||
		runAfter.Reason != runBefore.Reason ||
		!equalActivatedAt(runAfter.ActivatedAtMS, runBefore.ActivatedAtMS) {
		t.Errorf("run row = %+v, want it unchanged at %+v", *runAfter, *runBefore)
	}

	var found bool
	for _, child := range rootCmd.Commands() {
		if child.Name() == "pin" {
			found = true
		}
	}
	if !found {
		t.Error("`pin` is absent from `docket --help`'s command list, so a " +
			"claimed step's brief cannot name it")
	}
}
