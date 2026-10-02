package cli

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-590: `workflow show` printed `source_path` and `source_sha256` side by
// side and never asked whether they still agreed. On the machine that filed the
// issue they had not agreed for four versions — the row said investigation@4 at
// 4cb066e3, the file at that very path was version 8 at 6ed74d17 — and the
// output looked exactly as it does when they do agree.
//
// These tests fix the three verdicts apart, because they send an operator to
// three different places: nothing to do, re-register through the install path,
// restore a missing file.

// registerSourceAt registers src from an absolute path the test controls, and
// returns that path. `workflow show`'s check declines relative paths on
// purpose, so the path a test registers from has to be a real absolute one.
func registerSourceAt(t *testing.T, conn *sql.DB, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wf.toml")
	err := os.WriteFile(path, []byte(src), 0o644)
	testsupport.Must(t, err, "writing the definition: %v", err)

	cmd := workflowRegisterCmdWithDB(conn)
	w, _ := bufWriter(true)
	err = runWorkflowRegister(cmd, []string{path}, w)
	testsupport.Must(t, err, "register: %v", err)
	return path
}

// showSourceStatus runs `workflow show --json` and returns the verdict.
func showSourceStatus(t *testing.T, conn *sql.DB, ref string) model.WorkflowSourceStatus {
	t.Helper()
	cmd := workflowShowCmdWithDB(conn, false)
	w, buf := bufWriter(true)
	err := runWorkflowShow(cmd, []string{ref}, w)
	testsupport.Must(t, err, "show: %v", err)

	var envelope struct {
		Data struct {
			SourcePath   string                     `json:"source_path"`
			SourceSHA256 string                     `json:"source_sha256"`
			SourceStatus model.WorkflowSourceStatus `json:"source_status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if envelope.Data.SourceStatus.RegisteredSHA256 != envelope.Data.SourceSHA256 {
		t.Errorf("source_status.registered_sha256 = %q, want the row's own %q",
			envelope.Data.SourceStatus.RegisteredSHA256, envelope.Data.SourceSHA256)
	}
	return envelope.Data.SourceStatus
}

// TestWorkflowShowReportsAMatchingSource: the clean case says so explicitly.
// Silence would be the defect all over again — "nothing was checked" and "the
// bytes still match" have to be distinguishable.
func TestWorkflowShowReportsAMatchingSource(t *testing.T) {
	conn := newTestDB(t)
	registerSourceAt(t, conn, minimalWorkflow)

	got := showSourceStatus(t, conn, "unit@1")
	if got.State != model.WorkflowSourceMatches {
		t.Errorf("state = %q, want %q (%+v)", got.State, model.WorkflowSourceMatches, got)
	}
	if got.CurrentSHA256 != got.RegisteredSHA256 {
		t.Errorf("a matching source reports two different hashes: %+v", got)
	}

	cmd := workflowShowCmdWithDB(conn, false)
	w, buf := bufWriter(false)
	err := runWorkflowShow(cmd, []string{"unit@1"}, w)
	testsupport.Must(t, err, "show: %v", err)
	if !strings.Contains(buf.String(), "source status: matches") {
		t.Errorf("the human summary does not report the source status:\n%s", buf.String())
	}
}

// TestWorkflowShowReportsADriftedSource is the filed defect: the file at the
// recorded path is now a DIFFERENT definition, and the row goes on reporting
// the hash it was registered with.
func TestWorkflowShowReportsADriftedSource(t *testing.T) {
	conn := newTestDB(t)
	path := registerSourceAt(t, conn, minimalWorkflow)

	// The v8-over-v4 shape: the file moves on, the registration does not.
	bumped := strings.Replace(minimalWorkflow, "version = 1", "version = 8", 1)
	testsupport.Must(t, os.WriteFile(path, []byte(bumped), 0o644), "rewriting %s", path)

	got := showSourceStatus(t, conn, "unit@1")
	if got.State != model.WorkflowSourceDrifted {
		t.Fatalf("state = %q, want %q (%+v)", got.State, model.WorkflowSourceDrifted, got)
	}
	// BOTH hashes, or the report cannot be acted on.
	if got.CurrentSHA256 == "" || got.CurrentSHA256 == got.RegisteredSHA256 {
		t.Errorf("drift reports one hash, not both: %+v", got)
	}
	if got.Path != path {
		t.Errorf("path = %q, want the recorded %q", got.Path, path)
	}

	cmd := workflowShowCmdWithDB(conn, false)
	w, buf := bufWriter(false)
	err := runWorkflowShow(cmd, []string{"unit@1"}, w)
	testsupport.Must(t, err, "show: %v", err)
	out := buf.String()
	for _, want := range []string{"DRIFTED", got.CurrentSHA256, got.RegisteredSHA256} {
		if !strings.Contains(out, want) {
			t.Errorf("the human summary is missing %q:\n%s", want, out)
		}
	}
}

// TestWorkflowShowReportsAMissingSourceDistinctly: a file that is GONE is not a
// file that CHANGED. Conflating them would tell an operator to bump a version
// when what they need is to restore a path — and would claim an on-disk hash
// that was never read.
func TestWorkflowShowReportsAMissingSourceDistinctly(t *testing.T) {
	conn := newTestDB(t)
	path := registerSourceAt(t, conn, minimalWorkflow)
	testsupport.Must(t, os.Remove(path), "removing %s", path)

	got := showSourceStatus(t, conn, "unit@1")
	if got.State != model.WorkflowSourceUnreadable {
		t.Fatalf("state = %q, want %q (%+v)", got.State, model.WorkflowSourceUnreadable, got)
	}
	if got.CurrentSHA256 != "" {
		t.Errorf("current_sha256 = %q on a file that was never read", got.CurrentSHA256)
	}
	if got.Reason == "" {
		t.Error("an unreadable source carries no reason; the operator learns nothing")
	}

	cmd := workflowShowCmdWithDB(conn, false)
	w, buf := bufWriter(false)
	err := runWorkflowShow(cmd, []string{"unit@1"}, w)
	testsupport.Must(t, err, "show: %v", err)
	if !strings.Contains(buf.String(), "UNREADABLE") {
		t.Errorf("the human summary does not report the missing file:\n%s", buf.String())
	}
}

// TestWorkflowShowReportsAnUncheckableSource: a definition registered from
// stdin has no file to compare against, and says so rather than reporting a
// verdict it did not reach.
func TestWorkflowShowReportsAnUncheckableSource(t *testing.T) {
	conn := newTestDB(t)
	cmd := workflowRegisterCmdWithDB(conn)
	cmd.SetIn(strings.NewReader(minimalWorkflow))
	w, _ := bufWriter(true)
	testsupport.Must(t, runWorkflowRegister(cmd, []string{"-"}, w), "register from stdin")

	got := showSourceStatus(t, conn, "unit@1")
	if got.State != model.WorkflowSourceUnchecked {
		t.Errorf("state = %q, want %q (%+v)", got.State, model.WorkflowSourceUnchecked, got)
	}
	if got.Reason == "" {
		t.Error("an unchecked source carries no reason")
	}
}
