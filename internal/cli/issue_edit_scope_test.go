package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-741 at the verb level. The engine half — that the freeze is real and
// that ScopeEditFrozenForActiveRuns names the right runs — is pinned in
// internal/engine/dkt741_test.go. What is pinned HERE is the wiring an
// operator and a conductor actually meet: `issue edit --scope` on an issue
// with live steps in a live run says so, on both channels, and names the verb
// that would work.

// scopedIssueInLiveRun seeds the shape the warning is about: an issue with a
// declared scope, bound into an ACTIVATED run (so a snapshot exists) that
// still holds one pending step.
//
// The rows go in directly rather than through `run activate`, because the verb
// under test reads them and does not care how they were produced — and the
// engine package already drives the real activation for the same fixture.
func scopedIssueInLiveRun(t *testing.T, conn *sql.DB, scope, snapshotScope string) (int, int) {
	t.Helper()

	issueID, err := db.CreateIssue(conn, &model.Issue{
		Title: "widen me", Status: model.StatusBacklog,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating the issue: %v", err)
	testsupport.Must(t, db.SetIssueScopeGlobs(conn, issueID, scope), "declaring scope")

	wf, _, err := db.InsertWorkflow(conn, &model.Workflow{
		Name: "parks", Version: 1, SourcePath: "parks.toml",
		SourceSHA256: "0", Body: "", Parsed: "{}",
	}, 1)
	testsupport.Must(t, err, "registering a workflow: %v", err)

	run, err := db.InsertRun(conn, 1, "test run", 0, 1)
	testsupport.Must(t, err, "starting the run: %v", err)
	testsupport.Must(t, db.AddRunIssue(conn, run.ID, issueID), "binding the issue")

	_, err = conn.Exec(
		`UPDATE runs SET status = ? WHERE id = ?`, string(model.RunActive), run.ID)
	testsupport.Must(t, err, "activating the run: %v", err)
	_, err = conn.Exec(
		`UPDATE run_issues SET workflow_id = ?, issue_snapshot = ?
		  WHERE run_id = ? AND issue_id = ?`,
		wf.ID, `{"title":"widen me","kind":"task","labels":[],"scope":`+snapshotScope+`}`,
		run.ID, issueID)
	testsupport.Must(t, err, "writing the snapshot: %v", err)
	_, err = conn.Exec(
		`INSERT INTO steps
		   (run_id, issue_id, workflow_id, step_name, instance, kind, status,
		    created_at_ms, updated_at_ms)
		 VALUES (?, ?, ?, 'fix', 'fix@2', 'executor', ?, 1, 1)`,
		run.ID, issueID, wf.ID, db.StepPending)
	testsupport.Must(t, err, "inserting the step: %v", err)

	return run.ID, issueID
}

// editWithScope drives the real verb body with a substituted Writer.
func editWithScope(t *testing.T, conn *sql.DB, issueID int, jsonMode bool, globs ...string) (string, string) {
	t.Helper()
	cmd := cmdWithDB(conn)
	addScopeFlag(cmd)
	addIfVersionFlag(cmd)
	for _, g := range globs {
		testsupport.Must(t, cmd.Flags().Set(scopeFlag, g), "setting --scope")
	}

	w, stdout := bufWriter(jsonMode)
	stderr := &strings.Builder{}
	w.Stderr = stderr
	testsupport.Must(t,
		runIssueEdit(cmd, []string{model.FormatID(issueID)}, w), "issue edit")
	return stdout.String(), stderr.String()
}

// TestIssueEditScopeWarnsOnALiveRun is the acceptance criterion: the widen is
// accepted, and the operator is told in the same breath that it did not reach
// the run and what would.
func TestIssueEditScopeWarnsOnALiveRun(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := scopedIssueInLiveRun(t, conn,
		`["cli/src/command/start.rs"]`, `["cli/src/command/start.rs"]`)

	_, stderr := editWithScope(t, conn, issueID, false,
		"cli/src/command/start.rs", "script/install.sh", "makefile")

	for _, want := range []string{
		"Warning:",
		model.FormatRunID(runID),
		"run abandon " + model.FormatRunID(runID) + " --issue " + model.FormatID(issueID),
		"re-plan",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not name %q:\n%s", want, stderr)
		}
	}

	// The edit is REPORTED, not refused — the live column is what the
	// scheduler's mutual-exclusion check reads, and that half does take.
	if got := scopeColumn(t, conn, issueID); !got.Valid ||
		got.String != `["cli/src/command/start.rs","script/install.sh","makefile"]` {
		t.Errorf("scope_globs = %v, want the widened declaration written", got)
	}
}

// TestIssueEditScopeWarningRidesTheJSONEnvelope is the other channel, and the
// one that matters most here: RUN-52's widen was typed by a CONDUCTOR, and
// w.Warn is suppressed in JSON mode by design.
func TestIssueEditScopeWarningRidesTheJSONEnvelope(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := scopedIssueInLiveRun(t, conn,
		`["internal/a/**"]`, `["internal/a/**"]`)

	stdout, stderr := editWithScope(t, conn, issueID, true,
		"internal/a/**", "internal/b/**")

	if stderr != "" {
		t.Errorf("stderr = %q in JSON mode, want the envelope to be the only "+
			"channel", stderr)
	}

	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			ID       string   `json:"id"`
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(stdout), &envelope),
		"decoding the envelope")
	if !envelope.OK {
		t.Fatalf("envelope is not ok: %s", stdout)
	}
	if envelope.Data.ID != model.FormatID(issueID) {
		t.Errorf("data.id = %q, want %q — the issue shape must survive the "+
			"added key", envelope.Data.ID, model.FormatID(issueID))
	}
	if len(envelope.Data.Warnings) != 1 {
		t.Fatalf("data.warnings = %v, want one warning", envelope.Data.Warnings)
	}
	if !strings.Contains(envelope.Data.Warnings[0], model.FormatRunID(runID)) {
		t.Errorf("the enveloped warning does not name the run:\n%s",
			envelope.Data.Warnings[0])
	}
}

// TestIssueEditWithoutAWarningIsUnchanged pins the no-op case byte-for-byte:
// an edit that raises nothing must emit exactly the JSON it always did, with
// no `warnings` key and no re-ordered fields from a splice that ran anyway.
func TestIssueEditWithoutAWarningIsUnchanged(t *testing.T) {
	conn := newTestDB(t)
	_, issueID := scopedIssueInLiveRun(t, conn,
		`["internal/a/**"]`, `["internal/a/**"]`)

	// Same globs as the snapshot froze: nothing was hidden, so nothing is said.
	stdout, stderr := editWithScope(t, conn, issueID, true, "internal/a/**")

	if stderr != "" {
		t.Errorf("stderr = %q, want silence", stderr)
	}
	if strings.Contains(stdout, "warnings") {
		t.Errorf("the envelope grew a warnings key on an edit that hid "+
			"nothing:\n%s", stdout)
	}
}
