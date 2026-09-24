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
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// pinnedHashOf is the sha256 the fixture run recorded for `policy.toml` — the
// oracle the printed bytes must hash to, read from the pin row rather than
// recomputed from the same file the verb reads.
func pinnedHashOf(t *testing.T, conn *sql.DB, runRef string) string {
	t.Helper()
	runID, err := model.ParseRunID(runRef)
	testsupport.Must(t, err, "ParseRunID: %v", err)
	pins, err := db.ListPins(conn, runID)
	testsupport.Must(t, err, "ListPins: %v", err)
	for _, p := range pins {
		if p.Ref == "policy.toml" {
			return p.SHA256
		}
	}
	t.Fatal("the fixture run does not pin policy.toml")
	return ""
}

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
		// BYTE-EXACT, not merely containing: the verb's promise is that what it
		// prints hashes to the pin, so a framing newline would break
		// `docket pin show ... | shasum`.
		if got := buf.String(); got != pinShowPolicy {
			t.Errorf("output = %q, want exactly the pinned policy bytes %q",
				got, pinShowPolicy)
		}
		if got := workflow.SHA256(buf.Bytes()); got != pinnedHashOf(t, conn, runRef) {
			t.Errorf("printed bytes hash to %q, want the pin's %q",
				got, pinnedHashOf(t, conn, runRef))
		}
	})

	t.Run("--json wraps the pinned bytes with the run and path", func(t *testing.T) {
		conn, _, runRef := pinShowFixture(t)

		w, buf := bufWriter(true)
		if err := runPinShow(cmdWithDB(conn), runRef, "policy.toml", w); err != nil {
			t.Fatalf("pin show --json refuses a pinned file: %v", err)
		}
		// THE KEYS ARE SPELLED HERE, not borrowed from pinShowPayload: decoding
		// into the production type would follow a renamed tag and pass.
		var envelope struct {
			OK   bool `json:"ok"`
			Data struct {
				Run  string `json:"run"`
				Path string `json:"path"`
				Body string `json:"body"`
			} `json:"data"`
		}
		testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope),
			"decoding the envelope: %s", buf.String())
		if !envelope.OK {
			t.Fatalf("envelope is not ok: %s", buf.String())
		}
		if envelope.Data.Body != pinShowPolicy {
			t.Errorf("data.body = %q, want exactly the pinned policy bytes %q",
				envelope.Data.Body, pinShowPolicy)
		}
		if envelope.Data.Run != runRef {
			t.Errorf("data.run = %q, want %q", envelope.Data.Run, runRef)
		}
		if envelope.Data.Path != "policy.toml" {
			t.Errorf("data.path = %q, want %q", envelope.Data.Path, "policy.toml")
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
	pinned := pinnedHashOf(t, conn, runRef)
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
