package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// AUTO-REGISTRATION — §9's F1-F23.

// configRepo builds a repo whose `.docket/` directory is the one activation will
// scan, and returns the connection and the config root to write files into.
//
// THE TEMP DIRS COME FIRST, BEFORE t.Setenv. `t.TempDir()` reads TMPDIR, and a
// test that rewrote the environment before taking its directories would get a
// path that does not exist — the trap that costs a debug cycle every time.
func configRepo(t *testing.T) (*sql.DB, string) {
	t.Helper()

	root := t.TempDir()
	docketDir := filepath.Join(root, ".docket")
	configDir := filepath.Join(docketDir, "config")
	err := os.MkdirAll(configDir, 0o755)
	testsupport.Must(t, err, "creating the config directory: %v", err)

	t.Setenv("DOCKET_PATH", docketDir)

	conn, err := sql.Open("sqlite", filepath.Join(docketDir, "issues.db"))
	testsupport.Must(t, err, "opening: %v", err)
	t.Cleanup(func() { conn.Close() })
	conn.SetMaxOpenConns(1)
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)
	return conn, configDir
}

// writeConfigFile drops one file into the config tree, creating its directory.
func writeConfigFile(t *testing.T, configDir, rel, body string) string {
	t.Helper()
	path := filepath.Join(configDir, rel)
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	testsupport.Must(t, err, "creating %s: %v", filepath.Dir(path), err)
	err = os.WriteFile(path, []byte(body), 0o644)
	testsupport.Must(t, err, "writing %s: %v", path, err)
	return path
}

// autoWorkflowSrc is a definition that matches a `task` issue and declares no
// payload — the simple case, for the F14/F17/F20 rows.
const autoWorkflowSrc = `
[pipeline]
name = "auto-dev"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "w"
emits = "out"
after = []
`

// payloadWorkflowSrc declares a `payload`, so its registration REQUIRES the
// schema to be registered first. This is F2's subject: it is the file that
// cannot register unless the ordering holds.
const payloadWorkflowSrc = `
[pipeline]
name = "auto-payload"
version = 1

[match]
kind = ["task"]

[[step]]
name = "assess"
executor = "w"
emits = "out"
payload = "risk@1"
after = []
`

const riskSchemaSrc = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "risk@1",
  "type": "object",
  "properties": {
    "level": { "type": "string" }
  }
}`

// ---------------------------------------------------------------------------
// F1-F3 — the ordering, and the test that makes it mean something
// ---------------------------------------------------------------------------

// TestActivationAutoRegistersConfigDirectory is §9's headline: activation
// registers what it finds, with no register verb ever invoked.
func TestActivationAutoRegistersConfigDirectory(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml", autoWorkflowSrc)

	issue := createIssue(t, conn, "zero touch", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if len(result.Registered) != 1 {
		t.Fatalf("activation registered %d files, want the one workflow", len(result.Registered))
	}
	reg := result.Registered[0]
	if reg.Kind != RegistrationKindWorkflow || reg.Name != "auto-dev" || reg.Version != 1 {
		t.Errorf("registered %+v, want the workflow auto-dev@1", reg)
	}
	if reg.Outcome != RegistrationNew {
		t.Errorf("outcome is %q on a first registration, want %q", reg.Outcome, RegistrationNew)
	}

	// AND THE RUN BOUND TO IT. Registering a definition an activation cannot
	// then bind would be half the mechanism: Z5 requires the SAME activation to
	// register, bind, expand, and promote.
	if result.IssuesBound != 1 {
		t.Errorf("%d issues bound; the activation must bind against what it just "+
			"registered", result.IssuesBound)
	}
	if result.StepsCreated == 0 {
		t.Error("no steps were created; the run did not expand against the " +
			"auto-registered definition")
	}
}

// TestAutoRegistrationOrdersSchemasFirst is F2's BEHAVIORAL half, and it is the
// reason `TestSchemasRegisterBeforeWorkflows` could finally lose its skip.
//
// A workflow that names a schema in the SAME config tree registers second, so
// its `payload` reference resolves. If the order were lexical across everything,
// `auto-payload.toml` (w) would still sort after `risk@1.json` (r) — so the
// fixture deliberately uses a workflow whose name sorts BEFORE the schema's,
// making a lexical-across-everything implementation fail here.
func TestAutoRegistrationOrdersSchemasFirst(t *testing.T) {
	conn, configDir := configRepo(t)
	// `a-payload.toml` sorts before `risk@1.json` lexically. Only the two-group
	// order registers the schema first.
	writeConfigFile(t, configDir, "workflows/a-payload.toml", payloadWorkflowSrc)
	writeConfigFile(t, configDir, "schemas/risk@1.json", riskSchemaSrc)

	issue := createIssue(t, conn, "ordered", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "F2: activation failed, which is what a lexical-across-everything "+
		"order would produce — the workflow's `payload` would name an "+
		"unregistered schema: %v", err)

	if len(result.Registered) != 2 {
		t.Fatalf("registered %d files, want the schema and the workflow", len(result.Registered))
	}
	// THE ORDER IS IN THE REPORT, so the property is observable rather than
	// merely inferred from the success.
	if result.Registered[0].Kind != RegistrationKindSchema {
		t.Errorf("registered %s first; F2 registers SCHEMAS IN FULL before workflows",
			result.Registered[0].Kind)
	}
	if result.Registered[1].Kind != RegistrationKindWorkflow {
		t.Errorf("registered %s second, want the workflow", result.Registered[1].Kind)
	}
}

// TestAutoRegistrationWrongOrderFails is F3 — THE NEGATIVE TWIN, without which
// F2 is a tautology.
//
// It registers the same two files MANUALLY in the wrong order and asserts the
// workflow is refused. Without this, F2 could pass because the refusal does not
// exist rather than because the order avoids it.
func TestAutoRegistrationWrongOrderFails(t *testing.T) {
	conn, _ := configRepo(t)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	// The workflow FIRST, with its schema not yet registered.
	_, err = registerConfigWorkflowTx(tx, 1, "workflows/a-payload.toml",
		[]byte(payloadWorkflowSrc), nowMS)
	if err == nil {
		t.Fatal("F3: a workflow whose `payload` names an unregistered schema was " +
			"accepted; the refusal F2's ordering avoids does not exist, which " +
			"would make F2 prove nothing")
	}
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("F3: the refusal is %q, want VALIDATION_ERROR", code)
	}
	if !strings.Contains(err.Error(), "risk@1") {
		t.Errorf("F3: the refusal %q does not name the missing schema", err.Error())
	}
}

// ---------------------------------------------------------------------------
// F4-F6 — what is registered, what is pinned, what is skipped
// ---------------------------------------------------------------------------

// TestAutoRegistrationPinsWhatItDoesNotUnderstand is F4, F5, and F6 together.
//
// Only the two registries are REGISTERED. Everything else under
// `.docket/config/` is PINNED — which is exactly what §2 says core does with
// instance files it does not understand: "arbitrary operator-supplied file pins
// … which is how the reference instance pins its contracts, fragments, and
// policy WITHOUT CORE KNOWING WHAT THEY ARE".
func TestAutoRegistrationPinsWhatItDoesNotUnderstand(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml", autoWorkflowSrc)
	writeConfigFile(t, configDir, "policy.toml", "spawn = \"whatever the instance means\"\n")
	// F5: the pinned directories are scanned RECURSIVELY — a fragment three
	// levels deep is just a file to hash.
	writeConfigFile(t, configDir, "fragments/a/b/deep.md", "a fragment\n")
	writeConfigFile(t, configDir, "contracts/api.md", "a contract\n")
	// F6: a non-matching extension inside a REGISTRY directory is skipped
	// silently there and pinned like any other file. A refusal would make a
	// README a run-blocker.
	writeConfigFile(t, configDir, "workflows/README.md", "how to edit these\n")

	issue := createIssue(t, conn, "pinned", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "F6: a README in workflows/ blocked the run: %v", err)

	if len(result.Registered) != 1 {
		t.Fatalf("registered %d files, want only the workflow: %+v",
			len(result.Registered), result.Registered)
	}
	// The four non-registry files: policy.toml, the deep fragment, the
	// contract, and the README.
	if result.PinsFromConfig != 4 {
		t.Errorf("pinned %d config files, want 4 (policy, fragment, contract, README)",
			result.PinsFromConfig)
	}

	// The pins are RECORDED, so `context.pins` can serve them and a run can say
	// what it reproduced against.
	pins := pinsByKind(t, conn, run.ID, db.PinKindFile)
	var sawFragment bool
	for _, p := range pins {
		if strings.HasSuffix(p.Ref, filepath.Join("fragments", "a", "b", "deep.md")) {
			sawFragment = true
		}
	}
	if !sawFragment {
		t.Error("F5: a fragment three levels deep was not pinned; the pinned " +
			"directories are scanned recursively")
	}
}

// ---------------------------------------------------------------------------
// F9-F13 — the collision refusal
// ---------------------------------------------------------------------------

// TestChangedBytesAtSameVersionRefusesActivation is F9, F10, F12, and F13.
//
// §9.3 chose (a) REFUSE over auto-bump, overwrite, and ignore. This asserts the
// refusal AND its message, because F10's message is the whole reason (a) is
// compatible with the zero-touch tenet: a session reads it, acts on it, and
// brings "it needs a version bump to 2 — okay?" to the human.
func TestChangedBytesAtSameVersionRefusesActivation(t *testing.T) {
	conn, configDir := configRepo(t)
	path := writeConfigFile(t, configDir, "workflows/auto-dev.toml", autoWorkflowSrc)

	first := createIssue(t, conn, "first", "body", "task", nil)
	firstRun := startRun(t, conn, first)
	_, err := activate(conn, firstRun.ID)
	testsupport.Must(t, err, "the first activation: %v", err)

	// EDIT THE FILE WITHOUT BUMPING THE VERSION — the ordinary consequence of
	// editing a workflow, and the question §2's auto-registration sentence
	// leaves open.
	edited := strings.Replace(autoWorkflowSrc, `executor = "w"`, `executor = "x"`, 1)
	err = os.WriteFile(path, []byte(edited), 0o644)
	testsupport.Must(t, err, "editing: %v", err)

	second := createIssue(t, conn, "second", "body", "task", nil)
	secondRun := startRun(t, conn, second)
	_, err = activate(conn, secondRun.ID)
	if err == nil {
		t.Fatal("F9/F12: changed bytes at an unchanged name@version were accepted; " +
			"core must never auto-bump, overwrite, or silently use the " +
			"registered bytes")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("F9: the refusal is %q, want CONFLICT", code)
	}

	// F10: the message names the PATH, BOTH HASHES, the REGISTERED VERSION, the
	// LITERAL EDIT, and states that pinned runs are unaffected.
	msg := err.Error()
	for _, want := range []string{
		"auto-dev.toml", "sha256:", "auto-dev@1", "version to 2", "unaffected",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("F10: the refusal does not mention %q:\n%s", want, msg)
		}
	}

	// F13: it is a HARD refusal — the activation wrote NOTHING. A warning that
	// proceeded is one nobody reads until the run behaves oddly.
	run, err := db.GetRun(conn, secondRun.ID)
	testsupport.Must(t, err, "reading the run: %v", err)
	if run.Status == model.RunActive {
		t.Error("F8/F13: the run activated despite the collision; the refusal " +
			"must roll back the whole fat transaction")
	}

	// AND THE FIRST RUN IS UNAFFECTED, which is the sentence F10's message
	// makes and this asserts is true.
	if _, err := db.GetRun(conn, firstRun.ID); err != nil {
		t.Errorf("the already-pinned run was disturbed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// F14-F16 — idempotency, and the re-activation that must not re-scan
// ---------------------------------------------------------------------------

// TestUnchangedBytesReRegisterFreely is F11 and F14 (C10): identical bytes at
// the same `name@version` is a SUCCESS THAT CHANGES NOTHING.
//
// This is the case that makes re-activation of an unedited repo free, and the
// one that makes two activations racing over one directory both succeed.
func TestUnchangedBytesReRegisterFreely(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml", autoWorkflowSrc)

	first := createIssue(t, conn, "first", "body", "task", nil)
	firstRun := startRun(t, conn, first)
	_, err := activate(conn, firstRun.ID)
	testsupport.Must(t, err, "the first activation: %v", err)

	second := createIssue(t, conn, "second", "body", "task", nil)
	secondRun := startRun(t, conn, second)
	result, err := activate(conn, secondRun.ID)
	testsupport.Must(t, err, "F11: re-registering identical bytes failed: %v", err)

	if len(result.Registered) != 1 {
		t.Fatalf("registered %d files, want the one workflow", len(result.Registered))
	}
	// `unchanged` rather than omitted: the operator sees the file was considered
	// and found already registered.
	if result.Registered[0].Outcome != RegistrationUnchanged {
		t.Errorf("outcome is %q on identical bytes, want %q",
			result.Registered[0].Outcome, RegistrationUnchanged)
	}

	// And there is still exactly ONE row: an idempotent success inserts nothing.
	var n int
	err = conn.QueryRow(
		`SELECT COUNT(*) FROM workflows WHERE name = 'auto-dev'`).Scan(&n)
	testsupport.Must(t, err, "counting: %v", err)
	if n != 1 {
		t.Errorf("%d rows for auto-dev; F12 forbids minting a version nobody chose", n)
	}
}

// TestReactivationDoesNotRescan is F15, and §9.4 calls it "important and easy to
// get wrong".
//
// If re-activation re-scanned, an operator editing a workflow while a run
// expanded its second phase would hit F9's refusal on a run that was working
// fine, and the fix would be to revert their edit. Inheriting the pin set makes
// the edit A NON-EVENT — exactly as a re-registered workflow already is (RA2).
func TestReactivationDoesNotRescan(t *testing.T) {
	conn, configDir := configRepo(t)
	path := writeConfigFile(t, configDir, "workflows/auto-dev.toml", autoWorkflowSrc)

	issue := createIssue(t, conn, "mid-run edit", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "the first activation: %v", err)

	// A MID-RUN EDIT, without a version bump — the exact edit F9 refuses on a
	// FIRST activation.
	edited := strings.Replace(autoWorkflowSrc, `executor = "w"`, `executor = "x"`, 1)
	err = os.WriteFile(path, []byte(edited), 0o644)
	testsupport.Must(t, err, "editing: %v", err)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "F15: re-activation hit the collision refusal on a run that was "+
		"working fine; a mid-run edit must be invisible to a run already "+
		"under way: %v", err)

	if !result.Reactivation {
		t.Fatal("premise: the second activation must be a re-activation")
	}
	// F15: NO re-scan means NO registrations reported.
	if len(result.Registered) != 0 {
		t.Errorf("a re-activation registered %d files; it must inherit rather than "+
			"re-scan", len(result.Registered))
	}

	// And the REGISTERED BYTES are still the original ones: the edit reached
	// nothing.
	var body string
	err = conn.QueryRow(
		`SELECT body FROM workflows WHERE name = 'auto-dev' AND version = 1`,
	).Scan(&body)
	testsupport.Must(t, err, "reading the registered body: %v", err)
	if strings.Contains(body, `executor = "x"`) {
		t.Error("F15: the mid-run edit reached the registered definition")
	}
}

// ---------------------------------------------------------------------------
// F17-F19 — dormancy
// ---------------------------------------------------------------------------

// TestNoConfigDirectoryActivatesUnchanged is F17 and F18, and it is D6's
// dormancy proof at the Go level. F19's byte-diff twin is in QA.
//
// "A repo with no `.docket/config/` activates exactly as before" — the scan is
// skipped IN FULL after one `stat`, and the activation executes exactly the
// statements v9's did.
func TestNoConfigDirectoryActivatesUnchanged(t *testing.T) {
	// A repo with NO config directory at all. Note this uses the ordinary
	// fixture path rather than configRepo, precisely because the absence is the
	// subject.
	conn := mustDB(t)
	registerSource(t, conn, []byte(autoWorkflowSrc), "auto-dev.toml")

	issue := createIssue(t, conn, "dormant", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// F23: no registrations, so the verb prints no `Registered` block — the
	// dormancy is VISIBLE IN THE OUTPUT rather than merely true underneath it.
	if len(result.Registered) != 0 {
		t.Errorf("a repo with no config directory registered %d files",
			len(result.Registered))
	}
	if result.PinsFromConfig != 0 {
		t.Errorf("a repo with no config directory pinned %d files", result.PinsFromConfig)
	}
	// And the run still activated normally against its manually-registered
	// definition: dormant does not mean broken.
	if result.IssuesBound != 1 || result.StepsCreated == 0 {
		t.Errorf("the dormant path did not activate normally: bound=%d steps=%d",
			result.IssuesBound, result.StepsCreated)
	}
}

// TestEmptyConfigDirectoryRegistersNothing is F18: present but empty ⇒ the scan
// finds nothing and registers nothing.
func TestEmptyConfigDirectoryRegistersNothing(t *testing.T) {
	conn, _ := configRepo(t)
	registerSource(t, conn, []byte(autoWorkflowSrc), "auto-dev.toml")

	issue := createIssue(t, conn, "empty config", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	if len(result.Registered) != 0 || result.PinsFromConfig != 0 {
		t.Errorf("an empty config directory produced %d registrations and %d pins",
			len(result.Registered), result.PinsFromConfig)
	}
}

// ---------------------------------------------------------------------------
// F20-F23 — the report
// ---------------------------------------------------------------------------

// TestDryRunReportsRegistrationsAndWritesNothing is F22, which is what makes the
// zero-touch loop REVIEWABLE: the session runs `run activate --dry-run --json`,
// shows the human "this will register auto-dev@1 and run these commands", the
// human says yes, the session activates.
func TestDryRunReportsRegistrationsAndWritesNothing(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml", autoWorkflowSrc)

	issue := createIssue(t, conn, "dry", "body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, DryRun: true})
	testsupport.Must(t, err, "dry run: %v", err)
	if !result.DryRun {
		t.Fatal("the result does not report itself as a dry run")
	}
	// It reports what activation WOULD register — the same report a real one
	// prints, because it is the same computation rolled back rather than a
	// second read-only path that could drift.
	if len(result.Registered) != 1 {
		t.Fatalf("the dry run reported %d registrations, want 1", len(result.Registered))
	}
	if result.Registered[0].Name != "auto-dev" {
		t.Errorf("the dry run reported %q", result.Registered[0].Name)
	}

	// AND IT WROTE NOTHING: no workflow row exists afterwards.
	var n int
	err = conn.QueryRow(`SELECT COUNT(*) FROM workflows`).Scan(&n)
	testsupport.Must(t, err, "counting: %v", err)
	if n != 0 {
		t.Errorf("F22: the dry run registered %d workflows; it must write nothing", n)
	}
}

// TestRegistrationReportRendersNothingWhenNothingRegistered is F23: an
// activation that registered nothing prints NO block, not an empty one.
func TestRegistrationReportRendersNothingWhenNothingRegistered(t *testing.T) {
	var b strings.Builder
	RenderRegistrationReport(&b, nil, 0)
	if b.String() != "" {
		t.Errorf("F23: an empty registration set rendered %q; the dormancy must "+
			"be visible as ABSENCE", b.String())
	}

	RenderRegistrationReport(&b, []Registration{{
		Kind: RegistrationKindWorkflow, Name: "auto-dev", Version: 1,
		Path: ".docket/config/workflows/auto-dev.toml", Outcome: RegistrationNew,
	}}, 2)
	out := b.String()
	for _, want := range []string{"auto-dev", "new", "pinned 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("F20: the block does not mention %q:\n%s", want, out)
		}
	}
}

// TestRegistrationReportEscapesUntrustedNames is §9.5's rendering half.
//
// The path and the workflow name come from a CLONED REPO's filesystem and its
// own `[pipeline] name` — attacker-supplied strings heading for a terminal. A
// name carrying an escape sequence must show up AS an escape rather than repaint
// the line an operator is reading before they approve the run.
func TestRegistrationReportEscapesUntrustedNames(t *testing.T) {
	var b strings.Builder
	RenderRegistrationReport(&b, []Registration{{
		Kind: RegistrationKindWorkflow, Name: "evil\x1b[2K\rinnocent", Version: 1,
		Path: ".docket/config/workflows/evil.toml", Outcome: RegistrationNew,
	}}, 0)

	out := b.String()
	if strings.Contains(out, "\x1b") {
		t.Error("a raw escape byte reached the rendered report; T18 requires the " +
			"rendering to be what is approved")
	}
	if !strings.Contains(out, `\x1b`) {
		t.Errorf("the escape was not rendered visibly (lossless, per T18):\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// registration.auto — the operator's off switch
// ---------------------------------------------------------------------------

// TestAutoRegisterDisabledDoesNotAdoptANewVersion is the toggle's whole point:
// an operator who turns registration.auto off for a project is declining
// SILENT VERSION ADOPTION specifically, not the corpus generally. A run that
// activates while it is off must bind against whatever is ALREADY registered
// rather than the corpus's newer bytes, and must register nothing new in the
// attempt.
func TestAutoRegisterDisabledDoesNotAdoptANewVersion(t *testing.T) {
	conn, configDir := configRepo(t)
	path := writeConfigFile(t, configDir, "workflows/auto-dev.toml", autoWorkflowSrc)

	// First activation, registration.auto still at its default (true):
	// auto-dev@1 registers and binds, exactly as
	// TestActivationAutoRegistersConfigDirectory proves on its own.
	first := createIssue(t, conn, "first", "body", "task", nil)
	firstRun := startRun(t, conn, first)
	firstResult, err := activate(conn, firstRun.ID)
	testsupport.Must(t, err, "the first activation: %v", err)
	if len(firstResult.Registered) != 1 {
		t.Fatalf("setup: want auto-dev@1 registered, got %d registration(s)",
			len(firstResult.Registered))
	}

	// The operator turns it off for the project the runs above use —
	// startRun's hardcoded project 1 — the "a given project" half of the
	// requirement.
	err = db.SetConfig(conn, 1, db.KeyAutoRegister, "false")
	testsupport.Must(t, err, "SetConfig: %v", err)

	// The corpus bumps to @2 — the case that would otherwise silently rebind
	// every future issue to a version the operator never approved.
	bumped := strings.Replace(autoWorkflowSrc, "version = 1", "version = 2", 1)
	err = os.WriteFile(path, []byte(bumped), 0o644)
	testsupport.Must(t, err, "bumping the corpus file: %v", err)

	second := createIssue(t, conn, "second", "body", "task", nil)
	secondRun := startRun(t, conn, second)
	secondResult, err := activate(conn, secondRun.ID)
	testsupport.Must(t, err, "the second activation: %v", err)

	if len(secondResult.Registered) != 0 {
		t.Errorf("registration.auto=false still registered %d file(s); it must "+
			"register nothing", len(secondResult.Registered))
	}
	if len(secondResult.BoundIssues) != 1 || secondResult.BoundIssues[0].Workflow != "auto-dev@1" {
		t.Errorf("bound %+v, want the issue to stay on the ALREADY-registered "+
			"auto-dev@1 rather than adopt @2 from the corpus", secondResult.BoundIssues)
	}
}

// TestAutoRegisterDisabledStillPinsTheConfigDirectory proves the toggle is
// scoped to REGISTRATION alone, set --global (project 0) for the "vs all
// projects" half of the requirement. Pinning has no version to adopt — a
// project with registration.auto off still needs the corpus's contracts and
// fragments to render a step's `packet`, and turning both off together would
// leave it needing every one of them hand-supplied via --pin.
func TestAutoRegisterDisabledStillPinsTheConfigDirectory(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml", autoWorkflowSrc)
	writeConfigFile(t, configDir, "contracts/note.md", "a pinned fragment, not a registry file")

	err := db.SetConfig(conn, 0, db.KeyAutoRegister, "false")
	testsupport.Must(t, err, "SetConfig: %v", err)

	// With registration off, an operator registers by hand — the same path
	// `workflow register` runs, exactly as F7 requires auto-registration
	// itself to.
	registerSource(t, conn, []byte(autoWorkflowSrc), "auto-dev.toml")

	issue := createIssue(t, conn, "pin check", "body", "task", nil)
	run := startRun(t, conn, issue)
	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if len(result.Registered) != 0 {
		t.Errorf("registration.auto=false (global) still registered %d file(s)",
			len(result.Registered))
	}
	if result.PinsFromConfig == 0 {
		t.Error("registration.auto=false also stopped pinning; it must gate " +
			"registration only")
	}
}
