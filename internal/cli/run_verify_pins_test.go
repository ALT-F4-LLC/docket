package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
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

// ---------------------------------------------------------------------------
// Gate rows at the CLI: the remedy names only unpinned gates, and a sound run
// renders could-not-check gates rather than hiding them.
// ---------------------------------------------------------------------------

// gatedVerifyPinsWorkflow declares two gates on one step, so a report can
// hold an unpinned row beside an unchecked one.
const gatedVerifyPinsWorkflow = `
[pipeline]
name = "gated-cli"
version = 1

[match]
kind = ["feature"]

[[step]]
name = "judge"
executor = "judge"
emits = "change-summary"
after = []
packet = ["contracts/judge.md"]
gates = ["lint", "tests"]
`

// gatedVerifyPinsFixture activates a run of gatedVerifyPinsWorkflow with
// XDG_CONFIG_HOME pointed at an empty sandbox, so the trust store starts
// absent and every declared gate is unchecked. With trustTests set, a global
// `make tests` entry is added first: it matches the `tests` gate and names no
// script, so that gate alone becomes unpinned. It returns the connection,
// the config dir, and the run ref.
func gatedVerifyPinsFixture(t *testing.T, trustTests bool) (*sql.DB, string, string) {
	t.Helper()
	return gatedVerifyPinsFixtureWith(t, trustTests, nil)
}

// gatedVerifyPinsFixtureWith is gatedVerifyPinsFixture with a hook that runs
// before activation and receives the run's exec root, so a test can place a
// gate script there for activation to pin.
func gatedVerifyPinsFixtureWith(
	t *testing.T, trustTests bool, beforeActivate func(execRoot string),
) (*sql.DB, string, string) {
	t.Helper()
	// THE TEMP DIRS COME FIRST, BEFORE t.Setenv — `t.TempDir()` reads TMPDIR.
	root := t.TempDir()
	xdg := t.TempDir()
	execRoot := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("DOCKET_PATH", filepath.Join(root, ".docket"))

	configDir := filepath.Join(root, ".docket", "config")
	for rel, body := range map[string]string{
		"workflows/gated-cli.toml": gatedVerifyPinsWorkflow,
		"contracts/judge.md":       "the judge contract\n",
		"policy.toml":              "opaque = \"instance policy\"\n",
	} {
		full := filepath.Join(configDir, rel)
		testsupport.Must(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir %s", rel)
		testsupport.Must(t, os.WriteFile(full, []byte(body), 0o644), "writing %s", rel)
	}
	if trustTests {
		_, err := trust.Add(trust.AddRequest{
			Name: "tests", Argv: []string{"make", "tests"}, Global: true,
			NowMS: time.Now().UnixMilli(),
		})
		testsupport.Must(t, err, "trust.Add(tests): %v", err)
	}

	conn := newTestDB(t)
	issueID := createIssue(t, conn, "gated", model.StatusBacklog, model.PriorityNone)
	run, err := db.InsertRunWithContext(conn, 1, "test run", 0, model.NowMS(),
		db.RunContext{ExecRoot: execRoot})
	testsupport.Must(t, err, "InsertRunWithContext: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "AddRunIssue")
	if beforeActivate != nil {
		beforeActivate(execRoot)
	}
	_, err = engine.Activate(conn, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	testsupport.Must(t, err, "Activate: %v", err)
	return conn, configDir, run.Ref()
}

// driftJudgeContract rewrites the pinned contract after activation, so the
// run is unsound on a changed pin.
func driftJudgeContract(t *testing.T, configDir string) {
	t.Helper()
	testsupport.Must(t, os.WriteFile(filepath.Join(configDir, "contracts/judge.md"),
		[]byte("the judge contract, edited after activation\n"), 0o644),
		"drifting the contract")
}

// gateStatuses is the engine's verdict per declared gate, the precondition
// each CLI assertion below rests on.
func gateStatuses(t *testing.T, conn *sql.DB, runRef string) map[string]string {
	t.Helper()
	runID, err := model.ParseRunID(runRef)
	testsupport.Must(t, err, "ParseRunID: %v", err)
	report, err := engine.VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	got := map[string]string{}
	for _, g := range report.Gates {
		got[g.Gate] = g.Status
	}
	return got
}

// TestVerifyPinsRemedyOmitsUncheckedGates: on an unsound run whose gates
// could not be checked, the error must not list them as having no pinned
// script. That list is what an operator reads on an unsound run, and an
// unchecked gate may well be pinned.
func TestVerifyPinsRemedyOmitsUncheckedGates(t *testing.T) {
	conn, configDir, runRef := gatedVerifyPinsFixture(t, false)
	driftJudgeContract(t, configDir)

	statuses := gateStatuses(t, conn, runRef)
	if len(statuses) != 2 || statuses["lint"] != engine.GateUnchecked ||
		statuses["tests"] != engine.GateUnchecked {
		t.Fatalf("gate statuses = %v, want lint and tests both %q", statuses, engine.GateUnchecked)
	}

	w, _ := bufWriter(false)
	err := runRunVerifyPins(cmdWithDB(conn), runRef, w)
	if err == nil {
		t.Fatal("verify-pins reported clean on a run whose pinned contract drifted")
	}
	if strings.Contains(err.Error(), "gates with no pinned script") {
		t.Errorf("err = %q lists could-not-check gates as having no pinned script",
			err.Error())
	}
}

// TestVerifyPinsRemedyNamesUnpinnedGates: on an unsound run, a gate whose
// trusted argv names no script is named in the remedy, and an unchecked gate
// beside it is not.
func TestVerifyPinsRemedyNamesUnpinnedGates(t *testing.T) {
	conn, configDir, runRef := gatedVerifyPinsFixture(t, true)
	driftJudgeContract(t, configDir)

	statuses := gateStatuses(t, conn, runRef)
	if statuses["tests"] != engine.GateUnpinned || statuses["lint"] != engine.GateUnchecked {
		t.Fatalf("gate statuses = %v, want tests %q and lint %q",
			statuses, engine.GateUnpinned, engine.GateUnchecked)
	}

	w, _ := bufWriter(false)
	err := runRunVerifyPins(cmdWithDB(conn), runRef, w)
	if err == nil {
		t.Fatal("verify-pins reported clean on a run whose pinned contract drifted")
	}
	msg := err.Error()
	if !strings.Contains(msg, `gates with no pinned script: "tests"`) {
		t.Errorf("err = %q, want it to name the unpinned gate \"tests\"", msg)
	}
	if strings.Contains(msg, `"lint"`) {
		t.Errorf("err = %q names the could-not-check gate \"lint\" as unpinned", msg)
	}
}

// TestVerifyPinsRendersUncheckedGatesOnASoundRun: an absent trust store makes
// every declared gate unchecked, which never makes the run unsound, so the
// verb exits 0 and states each gate with its reason.
func TestVerifyPinsRendersUncheckedGatesOnASoundRun(t *testing.T) {
	conn, _, runRef := gatedVerifyPinsFixture(t, false)

	w, buf := bufWriter(false)
	if err := runRunVerifyPins(cmdWithDB(conn), runRef, w); err != nil {
		t.Fatalf("verify-pins refuses a sound run with unchecked gates: %v", err)
	}
	out := buf.String()
	for _, gate := range []string{"lint", "tests"} {
		prefix := `gate "` + gate + `": unchecked (`
		i := strings.Index(out, prefix)
		if i < 0 {
			t.Errorf("output = %q, want a line starting %q", out, prefix)
			continue
		}
		line := out[i+len(prefix):]
		if j := strings.IndexByte(line, '\n'); j >= 0 {
			line = line[:j]
		}
		if !strings.HasSuffix(line, ")") || strings.TrimSuffix(line, ")") == "" {
			t.Errorf("gate %q line ends %q, want a non-empty reason in parentheses",
				gate, line)
		}
	}
}

// TestVerifyPinsJSONCarriesUncheckedGates is the same sound run under --json:
// each declared gate has a row with status "unchecked" and a non-empty reason.
func TestVerifyPinsJSONCarriesUncheckedGates(t *testing.T) {
	conn, _, runRef := gatedVerifyPinsFixture(t, false)

	w, buf := bufWriter(true)
	if err := runRunVerifyPins(cmdWithDB(conn), runRef, w); err != nil {
		t.Fatalf("verify-pins --json refuses a sound run with unchecked gates: %v", err)
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Gates []engine.GateVerdict `json:"gates"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal(buf.Bytes(), &envelope), "decoding %q", buf.String())
	if !envelope.OK {
		t.Fatalf("envelope = %s, want ok", buf.String())
	}
	rows := map[string]engine.GateVerdict{}
	for _, g := range envelope.Data.Gates {
		rows[g.Gate] = g
	}
	for _, gate := range []string{"lint", "tests"} {
		row, ok := rows[gate]
		switch {
		case !ok:
			t.Errorf("gates = %+v, want a row for %q", envelope.Data.Gates, gate)
		case row.Status != engine.GateUnchecked:
			t.Errorf("gate %q status = %q, want %q", gate, row.Status, engine.GateUnchecked)
		case row.Reason == "":
			t.Errorf("gate %q has an empty reason", gate)
		}
	}
}

// ---------------------------------------------------------------------------
// The executed copy alone differs: exit 4 with the executed-copy remedy
// ---------------------------------------------------------------------------

// executedCopyFixture activates a gated run whose `lint` gate runs the
// relatively named scripts/qa/lint.sh, pinned from the run's exec root, then
// writes different bytes to the copy under the invoking exec root (the parent
// of DOCKET_PATH). Only that gate's executed copy differs from its pin.
func executedCopyFixture(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	const script = "scripts/qa/lint.sh"
	writeScript := func(root, body string) {
		full := filepath.Join(root, script)
		testsupport.Must(t, os.MkdirAll(filepath.Dir(full), 0o755), "mkdir %s", full)
		testsupport.Must(t, os.WriteFile(full, []byte(body), 0o755), "writing %s", full)
	}
	conn, configDir, runRef := gatedVerifyPinsFixtureWith(t, false, func(execRoot string) {
		writeScript(execRoot, "echo lint\n")
		_, err := trust.Add(trust.AddRequest{
			Name: "lint", Argv: []string{"bash", script}, Global: true,
			NowMS: time.Now().UnixMilli(),
		})
		testsupport.Must(t, err, "trust.Add(lint): %v", err)
	})
	writeScript(filepath.Dir(filepath.Dir(configDir)), "exit 0\n")

	if got := gateStatuses(t, conn, runRef)["lint"]; got != engine.GateExecutedCopyChanged {
		t.Fatalf("gate lint status = %q, want %q", got, engine.GateExecutedCopyChanged)
	}
	return conn, configDir, runRef
}

// TestVerifyPinsExecutedCopyChangedExitsConflict: nothing drifted, nothing is
// missing, and the pin set is closed, but a gate run from the invoking
// checkout would execute other bytes than the pinned script. That is exit 4,
// and the remedy is about the invoking copy, not the closure.
func TestVerifyPinsExecutedCopyChangedExitsConflict(t *testing.T) {
	conn, _, runRef := executedCopyFixture(t)

	w, _ := bufWriter(false)
	err := runRunVerifyPins(cmdWithDB(conn), runRef, w)
	if err == nil {
		t.Fatal("verify-pins reported clean on a run whose invoking copy of a gate script differs from its pin")
	}
	cerr, ok := err.(*CmdError)
	if !ok {
		t.Fatalf("err = %T, want *CmdError", err)
	}
	if cerr.Code != output.ErrConflict {
		t.Errorf("code = %q, want %q", cerr.Code, output.ErrConflict)
	}
	msg := err.Error()
	for _, want := range []string{
		`gate "lint"`,
		"restore the gate script in the invoking checkout",
		"run the gate from the run's checkout",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("err = %q, want it to say %q", msg, want)
		}
	}
	for _, unwanted := range []string{"the pin set froze", "start a new run", "docket run repin"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("err = %q says %q, which cannot fix an invoking copy that differs", msg, unwanted)
		}
	}
}

// TestVerifyPinsMissingOutranksExecutedCopyChanged: a missing pin keeps exit 2
// even when a gate's executed copy also differs.
func TestVerifyPinsMissingOutranksExecutedCopyChanged(t *testing.T) {
	conn, configDir, runRef := executedCopyFixture(t)
	testsupport.Must(t, os.Remove(filepath.Join(configDir, "contracts/judge.md")),
		"removing the pinned contract")

	w, _ := bufWriter(false)
	err := runRunVerifyPins(cmdWithDB(conn), runRef, w)
	cerr, ok := err.(*CmdError)
	if !ok {
		t.Fatalf("err = %v (%T), want *CmdError", err, err)
	}
	if cerr.Code != output.ErrNotFound {
		t.Errorf("code = %q, want %q", cerr.Code, output.ErrNotFound)
	}
}

// TestVerifyPinsChangedOutranksExecutedCopyChanged: a changed pin keeps exit 4
// with the repin remedy even when a gate's executed copy also differs.
func TestVerifyPinsChangedOutranksExecutedCopyChanged(t *testing.T) {
	conn, configDir, runRef := executedCopyFixture(t)
	driftJudgeContract(t, configDir)

	w, _ := bufWriter(false)
	err := runRunVerifyPins(cmdWithDB(conn), runRef, w)
	cerr, ok := err.(*CmdError)
	if !ok {
		t.Fatalf("err = %v (%T), want *CmdError", err, err)
	}
	if cerr.Code != output.ErrConflict {
		t.Errorf("code = %q, want %q", cerr.Code, output.ErrConflict)
	}
	if !strings.Contains(err.Error(), "docket run repin") {
		t.Errorf("err = %q, want the repin remedy", err.Error())
	}
}

// TestVerifyPinsMissingExitsNotFound: a missing pin with nothing changed is
// exit 2.
func TestVerifyPinsMissingExitsNotFound(t *testing.T) {
	conn, configDir, runRef := verifyPinsFixture(t, "", false)
	testsupport.Must(t, os.Remove(filepath.Join(configDir, "contracts/judge.md")),
		"removing the pinned contract")

	w, _ := bufWriter(false)
	err := runRunVerifyPins(cmdWithDB(conn), runRef, w)
	cerr, ok := err.(*CmdError)
	if !ok {
		t.Fatalf("err = %v (%T), want *CmdError", err, err)
	}
	if cerr.Code != output.ErrNotFound {
		t.Errorf("code = %q, want %q", cerr.Code, output.ErrNotFound)
	}
}

// TestVerifyPinsHelpNamesExecutedCopyChanged: the exit-code paragraph of the
// Long help lists executed-copy-changed with exit 4.
func TestVerifyPinsHelpNamesExecutedCopyChanged(t *testing.T) {
	long := runVerifyPinsCmd.Long
	var exitPara string
	for _, para := range strings.Split(long, "\n\n") {
		if strings.HasPrefix(para, "Exit 4") {
			exitPara = para
		}
	}
	if exitPara == "" {
		t.Fatalf("Long help has no paragraph starting %q:\n%s", "Exit 4", long)
	}
	if !strings.Contains(exitPara, "executed-copy-changed") {
		t.Errorf("exit-code paragraph = %q, want it to name executed-copy-changed", exitPara)
	}
	// The help is wrapped, so the remedy is matched across line breaks.
	if !strings.Contains(strings.Join(strings.Fields(long), " "),
		"restore the gate script in the invoking checkout, or run the gate from the run's checkout") {
		t.Errorf("Long help does not give the executed-copy remedy:\n%s", long)
	}
}
