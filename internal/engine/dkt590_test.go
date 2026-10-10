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
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-590: a registered workflow's `source_path` and `source_sha256` are two
// columns nothing compared against each other, so the file at the recorded path
// could become a different definition entirely and every read verb went on
// reporting the registered hash beside the stale path.
//
// The observed state: `workflow show investigation --json` reported version 4
// at 4cb066e3 while the file at its own source_path was version 8 at 6ed74d17,
// on six registered workflows at once — and RUN-40 bound investigation@4 and
// ran it. These tests fix the two dispositions apart: a binding this activation
// MAKES over a drifted source refuses, a binding it INHERITS warns.

// registerFixtureAtTemp registers the committed fixture from a WRITABLE
// absolute path, and returns that path so a test can drift it.
//
// The path matters twice over: the check declines relative paths on purpose
// (they name no particular file from another cwd), and the file has to be one a
// test may rewrite — the committed fixture is neither.
func registerFixtureAtTemp(t *testing.T, conn *sql.DB) (*model.Workflow, string) {
	t.Helper()
	registerFixtureSchema(t, conn)

	src, err := os.ReadFile(fixturePath)
	testsupport.Must(t, err, "reading fixture: %v", err)

	path := filepath.Join(t.TempDir(), "example-workflow.toml")
	err = os.WriteFile(path, src, 0o644)
	testsupport.Must(t, err, "writing the fixture copy: %v", err)

	return registerSource(t, conn, src, path), path
}

// driftSource rewrites the file at path so its bytes no longer hash to what was
// registered, WITHOUT changing what the definition means — the point is the
// hash, and a test that also changed the topology would be testing two things.
func driftSource(t *testing.T, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	testsupport.Must(t, err, "reading %s: %v", path, err)
	err = os.WriteFile(path, append(src, []byte("\n# edited after registration\n")...), 0o644)
	testsupport.Must(t, err, "drifting %s: %v", path, err)
}

// TestActivateRefusesADriftedWorkflowSource is the RUN-40 shape: the file at
// the bound workflow's own source_path is no longer the file that was
// registered, and this activation is the one making the binding.
func TestActivateRefusesADriftedWorkflowSource(t *testing.T) {
	conn := mustDB(t)
	wf, path := registerFixtureAtTemp(t, conn)
	driftSource(t, path)

	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := activate(conn, run.ID)
	if err == nil {
		t.Fatal("activation succeeded over a workflow whose source file no " +
			"longer holds the registered bytes; the run would bind and pin a " +
			"definition nobody can read at that path")
	}
	code, ok := CodeOf(err)
	if !ok || code != CodeConflict {
		t.Errorf("code = %q (engine error: %v), want %s", code, ok, CodeConflict)
	}

	// The message has to be actionable without a second command: which
	// workflow, which file, and BOTH hashes.
	for _, want := range []string{wf.Ref(), path, wf.SourceSHA256} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}

	// The fat transaction rolled back: a refusal at binding leaves no steps.
	if n := countRows(t, conn, "steps"); n != 0 {
		t.Errorf("%d step(s) written by a refused activation, want 0", n)
	}
}

// TestDryRunRefusesADriftedWorkflowSource: `--dry-run` is the real activation
// rolled back, so it must report the same refusal rather than printing a
// roster an operator would then fail to activate.
func TestDryRunRefusesADriftedWorkflowSource(t *testing.T) {
	conn := mustDB(t)
	_, path := registerFixtureAtTemp(t, conn)
	driftSource(t, path)

	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	_, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS, DryRun: true})
	if err == nil {
		t.Fatal("--dry-run reported an activation the real verb would refuse")
	}
	if code, _ := CodeOf(err); code != CodeConflict {
		t.Errorf("code = %q, want %s", code, CodeConflict)
	}
}

// TestActivateAcceptsAnUndriftedWorkflowSource is the baseline the refusals are
// measured against: the same setup with the file left alone activates, and
// reports nothing.
func TestActivateAcceptsAnUndriftedWorkflowSource(t *testing.T) {
	conn := mustDB(t)
	registerFixtureAtTemp(t, conn)

	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	if len(result.SourceWarnings) != 0 {
		t.Errorf("source warnings on a source that matches: %+v", result.SourceWarnings)
	}
	if result.Run.Status != model.RunActive {
		t.Errorf("run status = %q, want %q", result.Run.Status, model.RunActive)
	}
}

// TestActivateWarnsWhenTheSourceFileIsGone: an unreadable source is NOT drift
// and must not be refused as if it were. The registered bytes are intact and
// still reproduce — only their provenance is gone — and refusing would wedge
// every activation in a store holding a workflow whose file was moved, a state
// only the install path can resolve.
func TestActivateWarnsWhenTheSourceFileIsGone(t *testing.T) {
	conn := mustDB(t)
	wf, path := registerFixtureAtTemp(t, conn)
	testsupport.Must(t, os.Remove(path), "removing the source file %s", path)

	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if len(result.SourceWarnings) != 1 {
		t.Fatalf("source warnings = %+v, want exactly one", result.SourceWarnings)
	}
	got := result.SourceWarnings[0]
	if got.State != string(model.WorkflowSourceUnreadable) {
		t.Errorf("state = %q, want %q — a missing file and edited bytes are "+
			"different facts and send an operator to different places",
			got.State, model.WorkflowSourceUnreadable)
	}
	if got.Workflow != wf.Ref() {
		t.Errorf("workflow = %q, want %q", got.Workflow, wf.Ref())
	}
	if got.CurrentSHA256 != "" {
		t.Errorf("current_sha256 = %q on a file that was never read", got.CurrentSHA256)
	}
}

// TestReactivationWarnsRatherThanRefusingOnAnInheritedBinding is RA2/F15 held
// intact: a workflow edited after a run bound it must not reach that run, and
// must not stop it either. Refusing here would wedge every in-flight run the
// moment its workflow was superseded — the ordinary retro-loop bump leaves the
// old version's recorded path holding the new version's bytes.
func TestReactivationWarnsRatherThanRefusingOnAnInheritedBinding(t *testing.T) {
	conn := mustDB(t)
	wf, path := registerFixtureAtTemp(t, conn)

	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "first activation: %v", err)

	// The edit lands AFTER the run bound the definition.
	driftSource(t, path)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "re-activation: %v", err)
	if !result.Reactivation {
		t.Fatal("premise: the second call must be a re-activation")
	}
	if len(result.SourceWarnings) != 1 {
		t.Fatalf("source warnings = %+v, want exactly one", result.SourceWarnings)
	}
	got := result.SourceWarnings[0]
	if got.State != string(model.WorkflowSourceDrifted) {
		t.Errorf("state = %q, want %q", got.State, model.WorkflowSourceDrifted)
	}
	if got.Workflow != wf.Ref() {
		t.Errorf("workflow = %q, want %q", got.Workflow, wf.Ref())
	}
	if got.CurrentSHA256 == "" || got.CurrentSHA256 == got.RegisteredSHA256 {
		t.Errorf("the warning carries no distinct on-disk hash: %+v", got)
	}
}

// TestCheckWorkflowSourceStates fixes the four verdicts apart at the helper
// both call sites share. They are different facts: an operator restores a
// missing file, re-registers an edited one, and does nothing at all about a
// path that was never absolute.
func TestCheckWorkflowSourceStates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.toml")
	body := []byte("[pipeline]\nname = \"unit\"\nversion = 1\n")
	err := os.WriteFile(path, body, 0o644)
	testsupport.Must(t, err, "writing %s: %v", path, err)

	registered := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	if s := CheckWorkflowSource(path, workflow.SHA256(body)); s.State != model.WorkflowSourceMatches {
		t.Errorf("identical bytes = %q, want matches (%+v)", s.State, s)
	}
	s := CheckWorkflowSource(path, registered)
	if s.State != model.WorkflowSourceDrifted {
		t.Errorf("different bytes = %q, want drifted", s.State)
	}
	if s.CurrentSHA256 != workflow.SHA256(body) || s.RegisteredSHA256 != registered {
		t.Errorf("drift reports one hash, not both: %+v", s)
	}

	if s := CheckWorkflowSource(filepath.Join(dir, "gone.toml"), registered); s.State !=
		model.WorkflowSourceUnreadable {
		t.Errorf("a missing file = %q, want unreadable", s.State)
	}
	if s := CheckWorkflowSource("wf.toml", registered); s.State !=
		model.WorkflowSourceUnchecked {
		t.Errorf("a relative path = %q, want unchecked — resolving it against "+
			"an unrelated cwd would manufacture drift out of a namesake", s.State)
	}
	if s := CheckWorkflowSource("", registered); s.State != model.WorkflowSourceUnchecked {
		t.Errorf("an unrecorded path = %q, want unchecked", s.State)
	}
}

// TestAdoptionDeclinedWarnsRatherThanRefusing: `registration.auto = false`
// means "bind what is REGISTERED, not what the corpus now says", so a registry
// that lags its files is the state the operator ASKED FOR. Refusing over it
// would turn the documented off switch into a wedge the moment the corpus
// moved — the run could not activate at all, and the only remedy would be the
// version adoption the operator just declined.
func TestAdoptionDeclinedWarnsRatherThanRefusing(t *testing.T) {
	conn := mustDB(t)
	wf, path := registerFixtureAtTemp(t, conn)
	testsupport.Must(t, db.SetConfig(conn, 1, db.KeyAutoRegister, "false"),
		"turning registration.auto off")
	driftSource(t, path)

	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)

	result, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate with adoption declined: %v", err)

	if len(result.SourceWarnings) != 1 {
		t.Fatalf("source warnings = %+v, want exactly one", result.SourceWarnings)
	}
	if got := result.SourceWarnings[0]; got.State != string(model.WorkflowSourceDrifted) ||
		got.Workflow != wf.Ref() {
		t.Errorf("warning = %+v, want drifted on %s", got, wf.Ref())
	}
}
