package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-821 — `verify-pins` answered exit 0 on RUN-59 while every remaining judge
// step was unclaimable: the pins all matched disk, and the bytes they pinned
// referenced two fragments the run never pinned. The exit code is the part of
// this verb's answer a conductor's pre-dispatch check cannot miss, so it is
// asserted here rather than only in the engine.

const dkt821CLIWorkflow = `
[pipeline]
name = "dkt821-cli"
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

// verifyPinsFixture activates a run against a config tree whose one contract
// declares `includes` as its `packet_includes`, and returns the config dir with
// the run ref.
//
// `fragmentAtActivation` is the whole variable: activation pins the closure it
// can SEE, so a `packet_includes` naming a file no root holds yet is pinned by
// nothing and the run activates clean — the pin set froze open, with no repin
// anywhere in the story.
func verifyPinsFixture(
	t *testing.T, includes string, fragmentAtActivation bool,
) (*sql.DB, string, string) {
	t.Helper()
	// THE TEMP DIR COMES FIRST, BEFORE t.Setenv — `t.TempDir()` reads TMPDIR.
	root := t.TempDir()
	configDir := filepath.Join(root, ".docket", "config")
	testsupport.Must(t, os.MkdirAll(configDir, 0o755), "creating the config dir")
	t.Setenv("DOCKET_PATH", filepath.Join(root, ".docket"))

	write := func(rel, body string) {
		full := filepath.Join(configDir, rel)
		testsupport.Must(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir %s", rel)
		testsupport.Must(t, os.WriteFile(full, []byte(body), 0o644), "writing %s", rel)
	}
	write("workflows/dkt821-cli.toml", dkt821CLIWorkflow)
	write("contracts/judge.md", includes+"the judge contract\n")
	write("policy.toml", "opaque = \"instance policy\"\n")
	if fragmentAtActivation {
		write("fragments/ladder.md", "the laziness ladder\n")
	}

	conn := newTestDB(t)
	issueID := createIssue(t, conn, "closure", model.StatusBacklog, model.PriorityNone)
	run, err := db.InsertRun(conn, 1, "test run", 0, model.NowMS())
	testsupport.Must(t, err, "InsertRun: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "AddRunIssue")
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "Activate: %v", err)

	return conn, configDir, run.Ref()
}

// TestVerifyPinsExitsNonZeroOnAnUnpinnedReference is AC1's exit code: the pins
// all match, so the old verb said exit 0 — a caller's only unmissable signal
// pointed the wrong way.
func TestVerifyPinsExitsNonZeroOnAnUnpinnedReference(t *testing.T) {
	conn, configDir, runRef := verifyPinsFixture(t,
		"---\npacket_includes:\n  - fragments/ladder.md\n---\n", false)
	// The fragment lands after the pin set froze — RUN-59's state exactly: the
	// file is right there, and the run does not pin it.
	testsupport.Must(t, os.MkdirAll(filepath.Join(configDir, "fragments"), 0o755),
		"mkdir fragments")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(configDir, "fragments/ladder.md"),
		[]byte("the laziness ladder\n"), 0o644), "writing the fragment")

	w, _ := bufWriter(false)
	err := runRunVerifyPins(cmdWithDB(conn), runRef, w)
	if err == nil {
		t.Fatal("verify-pins reported clean on a run whose pinned contract " +
			"references a fragment the run does not pin")
	}
	cerr, ok := err.(*CmdError)
	if !ok {
		t.Fatalf("err = %T, want *CmdError", err)
	}
	// The code the BLOCKED CLAIMS themselves return, which is this verb's rule:
	// a caller that handles the refusal handles the prediction of it.
	if cerr.Code != output.ErrValidation {
		t.Errorf("code = %q, want %q", cerr.Code, output.ErrValidation)
	}
	for _, want := range []string{
		"contracts/judge.md", "fragments/ladder.md", "does not pin",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to say %q", err.Error(), want)
		}
	}
	// The remedy must not send anyone to repin: nothing drifted, so a repin on
	// this run is a clean no-op and adopts nothing.
	if strings.Contains(err.Error(), "docket run repin") {
		t.Errorf("err = %q names repin, which cannot fix a run with no drift "+
			"to adopt", err.Error())
	}
}

// TestVerifyPinsRunsFromAnUnanchoredCwd: verify-pins names its run explicitly
// and writes nothing, so a cwd that is neither a git worktree nor a docket
// store must not stop it. It used to refuse there with "not a git worktree",
// while the same capture from the checkout reported a sound pin set.
func TestVerifyPinsRunsFromAnUnanchoredCwd(t *testing.T) {
	// THE TEMP DIRS COME FIRST, BEFORE t.Setenv — `t.TempDir()` reads TMPDIR.
	home := t.TempDir()
	elsewhere := t.TempDir()
	t.Setenv("DOCKET_PATH", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Chdir(elsewhere)

	configDir := filepath.Join(home, ".docket", "config")
	for rel, body := range map[string]string{
		"workflows/dkt821-cli.toml": dkt821CLIWorkflow,
		"contracts/judge.md":        "the judge contract\n",
		"policy.toml":               "opaque = \"instance policy\"\n",
	} {
		full := filepath.Join(configDir, rel)
		testsupport.Must(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir %s", rel)
		testsupport.Must(t, os.WriteFile(full, []byte(body), 0o644), "writing %s", rel)
	}

	conn := newTestDB(t)
	issueID := createIssue(t, conn, "pins", model.StatusBacklog, model.PriorityNone)
	run, err := db.InsertRun(conn, 1, "test run", 0, model.NowMS())
	testsupport.Must(t, err, "InsertRun: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "AddRunIssue")
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "Activate: %v", err)

	cfg := &config.Config{
		Identity: elsewhere,
		Source:   config.SourceGlobal,
		Anchored: false,
	}
	projectsBefore := projectCount(t, conn)

	id, err := resolveInvocationProject(commandAt(t, "run verify-pins"), conn, cfg)
	testsupport.Must(t, err, "resolving `docket run verify-pins` from a non-repo cwd: %v", err)
	if id != db.UnregisteredProjectID {
		t.Errorf("resolved project %d, want the unregistered sentinel %d",
			id, db.UnregisteredProjectID)
	}

	w, buf := bufWriter(true)
	if err := runRunVerifyPins(cmdWithDB(conn), run.Ref(), w); err != nil {
		t.Fatalf("verify-pins refuses a sound run from a non-repo cwd: %v", err)
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Pins    []json.RawMessage `json:"pins"`
			Changed int               `json:"changed"`
			Missing int               `json:"missing"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope),
		"decoding %q", buf.String())
	if !envelope.OK || len(envelope.Data.Pins) == 0 ||
		envelope.Data.Changed != 0 || envelope.Data.Missing != 0 {
		t.Errorf("report = %s, want ok with pins, changed 0 and missing 0", buf.String())
	}

	if got := projectCount(t, conn); got != projectsBefore {
		t.Errorf("verify-pins created %d project row(s) from a non-repo cwd",
			got-projectsBefore)
	}
}

// TestVerifyPinsExitsZeroOnAClosedPinSet is AC3, at the exit code that matters.
func TestVerifyPinsExitsZeroOnAClosedPinSet(t *testing.T) {
	conn, _, runRef := verifyPinsFixture(t, "", false)

	w, buf := bufWriter(false)
	if err := runRunVerifyPins(cmdWithDB(conn), runRef, w); err != nil {
		t.Fatalf("verify-pins refuses a fully closed run: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "all sound") ||
		!strings.Contains(got, "every ref they reference is pinned") {
		t.Errorf("output = %q, want it to state BOTH halves — a reader told "+
			"only \"all sound\" cannot tell whether the closure was checked",
			got)
	}
}
