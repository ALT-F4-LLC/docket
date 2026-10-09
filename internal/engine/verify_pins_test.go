package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-297: no verb answered "is this run's pin state sound". `step render`
// checks only the pins its own step reads, so it returned exit 0 and a full
// packet while a contract another step depended on had already drifted — and
// an hour later every step that DID read it was unclaimable.

// pinAFile records a file pin on a run, exactly as activation does.
func pinAFile(t *testing.T, conn *sql.DB, runID int, root, ref, body string) string {
	t.Helper()
	path := filepath.Join(root, ref)
	testsupport.Must(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir")
	testsupport.Must(t, os.WriteFile(path, []byte(body), 0o644), "writing the fixture")

	hash := workflow.SHA256([]byte(body))
	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	testsupport.Must(t, db.InsertPinTx(tx, db.Pin{
		RunID: runID, Kind: db.PinKindFile, Ref: ref, SHA256: hash,
	}), "InsertPinTx")
	testsupport.Must(t, tx.Commit(), "Commit")
	return hash
}

// TestVerifyPinsReportsDriftAcrossTheWholeRun is the verb's whole point: it
// finds a changed pin whether or not any particular step reads it.
func TestVerifyPinsReportsDriftAcrossTheWholeRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	steady := pinAFile(t, conn, run.ID, root, "contracts/implement.md", "STEADY\n")
	drifted := pinAFile(t, conn, run.ID, root, "contracts/synthesize-findings.md", "BEFORE\n")

	// The `just activate` that replaced the installed copy.
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/synthesize-findings.md"), []byte("AFTER\n"), 0o644),
		"rewriting the contract")

	report, err := verifyPinsIn(conn, run.ID, []string{root})
	testsupport.Must(t, err, "verifyPinsIn: %v", err)

	if report.Sound() {
		t.Fatal("the report reads sound with a contract already replaced on disk")
	}
	if report.Changed != 1 {
		t.Errorf("changed = %d, want 1", report.Changed)
	}

	byRef := map[string]PinVerdict{}
	for _, v := range report.Pins {
		byRef[v.Ref] = v
	}
	if got := byRef["contracts/implement.md"]; got.Status != PinOK || got.Found != steady {
		t.Errorf("the unchanged contract reads %+v, want ok at %s", got, steady)
	}
	drift := byRef["contracts/synthesize-findings.md"]
	if drift.Status != PinChanged {
		t.Errorf("the replaced contract reads %q, want %q", drift.Status, PinChanged)
	}
	if drift.Pinned != drifted || drift.Found == drifted {
		t.Errorf("verdict = %+v, want the pinned hash %s beside a different one "+
			"on disk — an operator needs both to choose between restoring the "+
			"file and starting a new run", drift, drifted)
	}
	if drift.Path == "" {
		t.Error("the verdict names no path; an operator restoring the file " +
			"should not have to guess which config root won")
	}
}

// TestVerifyPinsReportsAMissingPin separates "changed" from "gone": they have
// different remedies and different exit codes.
func TestVerifyPinsReportsAMissingPin(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	pinAFile(t, conn, run.ID, root, "contracts/gone.md", "BODY\n")
	testsupport.Must(t, os.Remove(filepath.Join(root, "contracts/gone.md")),
		"removing the contract")

	report, err := verifyPinsIn(conn, run.ID, []string{root})
	testsupport.Must(t, err, "verifyPinsIn: %v", err)

	if report.Missing != 1 || report.Changed != 0 {
		t.Errorf("missing = %d, changed = %d, want 1 and 0",
			report.Missing, report.Changed)
	}
	if report.Sound() {
		t.Error("a run missing a file it depends on reads sound")
	}
}

// TestVerifyPinsIsSoundOnAnUntouchedRun is the lower bound: the verb must not
// cry drift over a run nobody touched, or nobody will believe it when it does.
func TestVerifyPinsIsSoundOnAnUntouchedRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()

	pinAFile(t, conn, run.ID, root, "contracts/a.md", "A\n")
	pinAFile(t, conn, run.ID, root, "contracts/b.md", "B\n")

	report, err := verifyPinsIn(conn, run.ID, []string{root})
	testsupport.Must(t, err, "verifyPinsIn: %v", err)
	if !report.Sound() {
		t.Errorf("an untouched run reads unsound: %s", PinReportReason(report))
	}
	if len(report.Pins) < 2 {
		t.Errorf("checked %d pins, want at least the two file pins", len(report.Pins))
	}
}

// TestVerifyPinsWritesNothing pins the read-only contract. A verb that re-pinned
// on drift would silently rewrite the agreement instead of reporting it broke —
// which is the failure `step render`'s own "never a silent re-pin" rule names.
func TestVerifyPinsWritesNothing(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()
	pinned := pinAFile(t, conn, run.ID, root, "contracts/a.md", "A\n")
	testsupport.Must(t, os.WriteFile(
		filepath.Join(root, "contracts/a.md"), []byte("EDITED\n"), 0o644),
		"editing the contract")

	for range 3 {
		if _, err := verifyPinsIn(conn, run.ID, []string{root}); err != nil {
			t.Fatalf("verifyPinsIn: %v", err)
		}
	}

	var stored string
	err := conn.QueryRow(
		`SELECT sha256 FROM pins WHERE run_id = ? AND ref = ?`,
		run.ID, "contracts/a.md").Scan(&stored)
	testsupport.Must(t, err, "reading the pin back: %v", err)
	if stored != pinned {
		t.Errorf("the pin was rewritten to %s; it must still record %s",
			stored, pinned)
	}
}

// TestVerifyPinsIsDeterministic keeps the report golden-stable: two checks of
// one unchanged run produce the same rows in the same order.
func TestVerifyPinsIsDeterministic(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	root := t.TempDir()
	for _, ref := range []string{"c/z.md", "c/a.md", "c/m.md"} {
		pinAFile(t, conn, run.ID, root, ref, ref+"\n")
	}

	var first []PinVerdict
	for range 8 {
		report, err := verifyPinsIn(conn, run.ID, []string{root})
		testsupport.Must(t, err, "verifyPinsIn: %v", err)
		if first == nil {
			first = report.Pins
			continue
		}
		if len(report.Pins) != len(first) {
			t.Fatalf("pin count moved %d -> %d", len(first), len(report.Pins))
		}
		for i := range first {
			if report.Pins[i] != first[i] {
				t.Fatalf("row %d moved: %+v -> %+v", i, first[i], report.Pins[i])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Gate scripts are pinned at activation and reported when unpinned
// ---------------------------------------------------------------------------

// gateScriptFixture builds a repo whose gate scripts a run's gates execute and
// swaps in a trust store whose entries name them three ways: a relative argv,
// an absolute argv under the exec root, and a build tool naming no script.
func gateScriptFixture(t *testing.T) (repo string, conn *sql.DB, runID int) {
	t.Helper()
	conn = mustDB(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "resolving the repo root: %v", err)
	for name, body := range map[string]string{
		"scripts/qa/build.sh":        "echo build\n",
		"scripts/qa/ac-commands.sh":  "echo ac\n",
		"scripts/qa/self-hygiene.sh": "echo hygiene\n",
	} {
		path := filepath.Join(repo, name)
		testsupport.Must(t, os.MkdirAll(filepath.Dir(path), 0o755), "mkdir")
		testsupport.Must(t, os.WriteFile(path, []byte(body), 0o755), "writing %s", name)
	}

	entry := func(name string, argv ...string) trust.Entry {
		return trust.Entry{
			Name: name, Argv: argv, ArgvSHA256: trust.ArgvSHA256(argv), Global: true,
		}
	}
	prior := gatePinStore
	gatePinStore = sandboxTrust(t,
		entry("build", "bash", "scripts/qa/build.sh"),
		entry("ac-commands", "bash", filepath.Join(repo, "scripts/qa/ac-commands.sh")),
		entry("tests", "make", "tests"),
	)
	t.Cleanup(func() { gatePinStore = prior })

	registerFixture(t, conn)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "test run", 0, nowMS,
		db.RunContext{ExecRoot: repo})
	testsupport.Must(t, err, "starting run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "adding the issue")
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return repo, conn, run.ID
}

func filePinFor(t *testing.T, conn *sql.DB, runID int, ref string) (db.Pin, bool) {
	t.Helper()
	pins, err := db.ListPins(conn, runID)
	testsupport.Must(t, err, "ListPins: %v", err)
	for _, p := range pins {
		if p.Kind == db.PinKindFile && p.Ref == ref {
			return p, true
		}
	}
	return db.Pin{}, false
}

// TestActivationPinsGateScripts: each gate whose trusted argv names a file
// under the exec root gets a file pin holding that file's SHA-256, whether the
// argv spells the path relatively or as an absolute path under the root.
func TestActivationPinsGateScripts(t *testing.T) {
	repo, conn, runID := gateScriptFixture(t)

	for rel, body := range map[string]string{
		"scripts/qa/build.sh":       "echo build\n",
		"scripts/qa/ac-commands.sh": "echo ac\n",
	} {
		ref := filepath.Join(repo, rel)
		p, ok := filePinFor(t, conn, runID, ref)
		if !ok {
			t.Fatalf("no file pin for gate script %s; the gate's argv names it", ref)
		}
		if want := workflow.SHA256([]byte(body)); p.SHA256 != want {
			t.Errorf("pin for %s holds %s, want %s", ref, p.SHA256, want)
		}
	}
	if _, ok := filePinFor(t, conn, runID,
		filepath.Join(repo, "scripts/qa/self-hygiene.sh")); ok {
		t.Error("a script no declared gate's trusted argv names was pinned")
	}
}

// TestVerifyPinsReportsAnEditedGateScriptAsDrift: a script changed after
// activation is a changed pin, so the report is not sound and names the ref.
func TestVerifyPinsReportsAnEditedGateScriptAsDrift(t *testing.T) {
	repo, conn, runID := gateScriptFixture(t)
	script := filepath.Join(repo, "scripts/qa/ac-commands.sh")

	report, err := VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if !report.Sound() {
		t.Fatalf("an untouched run reads unsound: %s", PinReportReason(report))
	}

	testsupport.Must(t, os.WriteFile(script, []byte("echo edited\n"), 0o755), "editing the script")
	report, err = VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if report.Sound() {
		t.Fatal("the report reads sound with a pinned gate script edited after activation")
	}
	var drift *PinVerdict
	for i := range report.Pins {
		if report.Pins[i].Ref == script {
			drift = &report.Pins[i]
		}
	}
	if drift == nil || drift.Status != PinChanged {
		t.Errorf("the edited script's verdict is %+v, want %q for ref %s", drift, PinChanged, script)
	}
}

// TestVerifyPinsReportsUnpinnedGates: a gate whose trusted argv names no file
// under the exec root is listed as unpinned by name, and that report never
// makes the run unsound, because most trust entries are `make <target>`.
func TestVerifyPinsReportsUnpinnedGates(t *testing.T) {
	_, conn, runID := gateScriptFixture(t)

	report, err := VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins: %v", err)

	got := map[string]GateVerdict{}
	for _, g := range report.Gates {
		got[g.Gate] = g
	}
	if g, ok := got["tests"]; !ok || g.Status != GateUnpinned {
		t.Errorf("the make-style gate `tests` is not listed as unpinned: %+v", report.Gates)
	}
	for _, name := range []string{"build", "ac-commands"} {
		if _, ok := got[name]; ok {
			t.Errorf("gate %q names a pinned script but is listed as unpinned", name)
		}
	}
	if report.UnpinnedGates != len(report.Gates) {
		t.Errorf("unpinned_gates = %d, want %d", report.UnpinnedGates, len(report.Gates))
	}
	if !report.Sound() {
		t.Errorf("an unpinned gate made the report unsound: %s", PinReportReason(report))
	}
}

// TestRepoLocalGateFilesRefusesPathsOutsideTheRoot: a path that resolves
// outside the exec root, by `..` or by symlink, is not repo-local.
func TestRepoLocalGateFilesRefusesPathsOutsideTheRoot(t *testing.T) {
	outside, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "resolving: %v", err)
	secret := filepath.Join(outside, "gate.sh")
	testsupport.Must(t, os.WriteFile(secret, []byte("x\n"), 0o755), "writing")

	repo, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "resolving: %v", err)
	testsupport.Must(t, os.Symlink(secret, filepath.Join(repo, "link.sh")), "symlink")

	for _, argv := range [][]string{
		{"bash", secret},
		{"bash", "link.sh"},
		{"bash", "../" + filepath.Base(outside) + "/gate.sh"},
		{"make", "tests"},
		{"bash", "-c", "true"},
	} {
		if got := repoLocalGateFiles(argv, repo); len(got) != 0 {
			t.Errorf("argv %v resolved to repo-local files %v, want none", argv, got)
		}
	}
}

// TestRepoLocalGateFilesPinsOnlyTheScript: a file a tool merely receives as an
// argument (a test a run is meant to edit) is not the gate's script, so it is
// not pinned and its edit is not reported as drift. An interpreter's first
// non-flag argument is the script.
func TestRepoLocalGateFilesPinsOnlyTheScript(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "resolving: %v", err)
	file := filepath.Join(repo, "tests", "x.py")
	testsupport.Must(t, os.MkdirAll(filepath.Dir(file), 0o755), "mkdir")
	testsupport.Must(t, os.WriteFile(file, []byte("x\n"), 0o755), "writing")

	if got := repoLocalGateFiles([]string{"pytest", "tests/x.py"}, repo); len(got) != 0 {
		t.Errorf("a tool's argument was pinned as the gate script: %v", got)
	}
	if got := repoLocalGateFiles([]string{"bash", "tests/x.py"}, repo); len(got) != 1 || got[0] != file {
		t.Errorf("an interpreter's script argument resolved to %v, want [%s]", got, file)
	}
}

// ---------------------------------------------------------------------------
// Pins resolve against the run's checkout, not the invoking cwd
// ---------------------------------------------------------------------------

// onGlobalStore moves the process onto the global store from cwd, with an
// empty HOME and an empty trust store. Call it AFTER registerFixture, whose
// fixture paths are relative to the package directory.
func onGlobalStore(t *testing.T, cwd string) {
	t.Helper()
	home := t.TempDir()
	// DOCKET_PATH is pinned package-wide by TestMain; clearing it puts
	// resolution on the global store, the only source with a repo-side root.
	t.Setenv("DOCKET_PATH", "")
	t.Setenv("HOME", home)
	t.Chdir(cwd)

	prior := gatePinStore
	gatePinStore = sandboxTrust(t)
	t.Cleanup(func() { gatePinStore = prior })
}

// unanchoredRunFixture activates a run recorded in a checkout and pins one
// file under checkout/.docket/config, then leaves the process in a non-git
// directory on the global store. Neither invoking root (HOME's, the cwd's)
// holds the pinned ref, so only the run's recorded exec root resolves it.
func unanchoredRunFixture(t *testing.T) (conn *sql.DB, runID int, repoConfig string) {
	t.Helper()
	elsewhere := t.TempDir()
	checkout, err := filepath.EvalSymlinks(t.TempDir())
	testsupport.Must(t, err, "resolving the checkout: %v", err)
	conn = mustDB(t)
	registerFixture(t, conn)
	onGlobalStore(t, elsewhere)

	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run, err := db.InsertRunWithContext(conn, 1, "test run", 0, nowMS,
		db.RunContext{ExecRoot: checkout})
	testsupport.Must(t, err, "starting run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issue), "adding the issue")
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	repoConfig = filepath.Join(checkout, ".docket", "config")
	pinAFile(t, conn, run.ID, repoConfig, "contracts/project.md", "the repo's contract\n")

	if cfg := resolvePaths(); cfg.Anchored || contains(instanceConfigRoots(), repoConfig) {
		t.Fatalf("premise: the invocation must be unanchored and blind to the "+
			"checkout's config (anchored %v, roots %v)", cfg.Anchored, instanceConfigRoots())
	}
	return conn, run.ID, repoConfig
}

// TestVerifyPinsResolvesTheRunsCheckoutFromAnUnanchoredCwd: a run whose pins
// live under its checkout's `.docket/config` verifies clean from a non-git
// directory, because the roots come from the run's recorded exec root.
func TestVerifyPinsResolvesTheRunsCheckoutFromAnUnanchoredCwd(t *testing.T) {
	conn, runID, _ := unanchoredRunFixture(t)

	report, err := VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if report.Missing != 0 || report.Changed != 0 {
		t.Errorf("missing = %d, changed = %d, want 0 and 0: the run's repo-side "+
			"pin was resolved against the invoking cwd, not the run's checkout",
			report.Missing, report.Changed)
	}
	if !report.Sound() {
		t.Errorf("an untouched run reads unsound from an unanchored cwd: %s",
			PinReportReason(report))
	}
}

// TestVerifyPinsReportsRepoSideDriftFromAnUnanchoredCwd: an edit to the run's
// repo-side pinned file is drift, not a missing file, from the same cwd.
func TestVerifyPinsReportsRepoSideDriftFromAnUnanchoredCwd(t *testing.T) {
	conn, runID, repoConfig := unanchoredRunFixture(t)
	testsupport.Must(t, os.WriteFile(filepath.Join(repoConfig, "contracts/project.md"),
		[]byte("edited\n"), 0o644), "editing the contract")

	report, err := VerifyPins(conn, runID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if report.Changed != 1 || report.Missing != 0 {
		t.Errorf("changed = %d, missing = %d, want 1 and 0: an edited repo-side "+
			"pin must read as drift", report.Changed, report.Missing)
	}
}

// TestVerifyPinsFallsBackToTheInvokingRootsWithNoRecordedExecRoot: a run that
// recorded no exec root still resolves its repo-side pins against the
// checkout VerifyPins is invoked from.
func TestVerifyPinsFallsBackToTheInvokingRootsWithNoRecordedExecRoot(t *testing.T) {
	checkout := t.TempDir()
	conn := mustDB(t)
	registerFixture(t, conn)
	onGlobalStore(t, checkout)

	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)
	if run.ExecRoot != "" {
		t.Fatalf("premise: the run must record no exec root, got %q", run.ExecRoot)
	}
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	pinAFile(t, conn, run.ID, filepath.Join(checkout, ".docket", "config"),
		"contracts/project.md", "the repo's contract\n")

	report, err := VerifyPins(conn, run.ID)
	testsupport.Must(t, err, "VerifyPins: %v", err)
	if !report.Sound() {
		t.Errorf("a run with no recorded exec root reads unsound from its own "+
			"checkout (missing = %d): %s", report.Missing, PinReportReason(report))
	}
}
