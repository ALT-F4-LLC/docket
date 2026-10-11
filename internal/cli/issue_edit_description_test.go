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

// The description twin of issue_edit_scope_test.go: `issue edit --description`
// on an issue with live steps in a live run names the run and the verb that
// makes the amendment reach it, `run refresh-body`.

// describedIssueInLiveRun is scopedIssueInLiveRun with a frozen description:
// the run's body snapshot holds frozenBody, and the issue's pending step will
// render it.
func describedIssueInLiveRun(t *testing.T, conn *sql.DB, frozenBody string) (int, int) {
	t.Helper()
	runID, issueID := scopedIssueInLiveRun(t, conn, `["internal/a/**"]`, `["internal/a/**"]`)
	_, err := conn.Exec(
		`UPDATE run_issues SET body_snapshot = ? WHERE run_id = ? AND issue_id = ?`,
		frozenBody, runID, issueID)
	testsupport.Must(t, err, "freezing the description: %v", err)
	return runID, issueID
}

// editWithDescription drives the real verb body with a substituted Writer.
func editWithDescription(t *testing.T, conn *sql.DB, issueID int, jsonMode bool, description string) (string, string) {
	t.Helper()
	cmd := cmdWithDB(conn)
	cmd.Flags().StringP("description", "d", "", "")
	addScopeFlag(cmd)
	addIfVersionFlag(cmd)
	testsupport.Must(t, cmd.Flags().Set("description", description), "setting --description")

	w, stdout := bufWriter(jsonMode)
	stderr := &strings.Builder{}
	w.Stderr = stderr
	testsupport.Must(t,
		runIssueEdit(cmd, []string{model.FormatID(issueID)}, w), "issue edit")
	return stdout.String(), stderr.String()
}

func TestIssueEditHelpNamesRefreshBody(t *testing.T) {
	const want = "docket run refresh-body RUN-N --issue DKT-M --reason R"
	if !strings.Contains(editCmd.Long, want) {
		t.Errorf("issue edit --help does not name %q:\n%s", want, editCmd.Long)
	}
}

func TestIssueEditDescriptionWarnsOnALiveRun(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := describedIssueInLiveRun(t, conn, "the frozen criteria")

	_, stderr := editWithDescription(t, conn, issueID, false, "the amended criteria")

	for _, want := range []string{
		"Warning:",
		model.FormatRunID(runID),
		"docket run refresh-body " + model.FormatRunID(runID) + " --issue " + model.FormatID(issueID),
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not name %q:\n%s", want, stderr)
		}
	}

	// The edit is reported, not refused: the live column holds the amendment.
	got, err := db.GetIssue(conn, issueID)
	testsupport.Must(t, err, "reading the issue back: %v", err)
	if got.Description != "the amended criteria" {
		t.Errorf("description = %q, want the amendment written", got.Description)
	}
}

func TestIssueEditDescriptionWarningRidesTheJSONEnvelope(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := describedIssueInLiveRun(t, conn, "the frozen criteria")

	stdout, stderr := editWithDescription(t, conn, issueID, true, "the amended criteria")

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
		t.Errorf("data.id = %q, want %q", envelope.Data.ID, model.FormatID(issueID))
	}
	if len(envelope.Data.Warnings) != 1 {
		t.Fatalf("data.warnings = %v, want one warning", envelope.Data.Warnings)
	}
	for _, want := range []string{model.FormatRunID(runID), "docket run refresh-body"} {
		if !strings.Contains(envelope.Data.Warnings[0], want) {
			t.Errorf("the enveloped warning does not name %q:\n%s",
				want, envelope.Data.Warnings[0])
		}
	}
}

// TestIssueEditDescriptionMatchingTheFrozenBodyIsSilent: the issue still holds
// a pending step in the live run, but the written description is exactly the
// body the run froze, so that step already renders it and there is nothing for
// refresh-body to copy.
func TestIssueEditDescriptionMatchingTheFrozenBodyIsSilent(t *testing.T) {
	conn := newTestDB(t)
	const frozen = "the frozen criteria"
	_, issueID := describedIssueInLiveRun(t, conn, frozen)

	_, textStderr := editWithDescription(t, conn, issueID, false, frozen)
	if textStderr != "" {
		t.Errorf("stderr = %q, want silence", textStderr)
	}

	stdout, stderr := editWithDescription(t, conn, issueID, true, frozen)
	if stderr != "" {
		t.Errorf("stderr = %q in JSON mode, want silence", stderr)
	}
	var envelope struct {
		OK   bool                       `json:"ok"`
		Data map[string]json.RawMessage `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(stdout), &envelope),
		"decoding the envelope")
	if !envelope.OK {
		t.Fatalf("envelope is not ok: %s", stdout)
	}
	if _, ok := envelope.Data["warnings"]; ok {
		t.Errorf("the envelope grew a warnings key on an edit the run "+
			"already holds:\n%s", stdout)
	}
}

// TestIssueEditDescriptionWithoutALiveStepIsUnchanged: once every step of the
// issue has recorded, no packet will render the frozen description again, so
// the edit raises nothing and its output carries no warnings key.
func TestIssueEditDescriptionWithoutALiveStepIsUnchanged(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := describedIssueInLiveRun(t, conn, "the frozen criteria")
	_, err := conn.Exec(
		`UPDATE steps SET status = ? WHERE run_id = ? AND issue_id = ?`,
		db.StepDone, runID, issueID)
	testsupport.Must(t, err, "recording the step: %v", err)

	_, textStderr := editWithDescription(t, conn, issueID, false, "the amended criteria")
	if textStderr != "" {
		t.Errorf("stderr = %q, want silence", textStderr)
	}

	stdout, stderr := editWithDescription(t, conn, issueID, true, "amended again")
	if stderr != "" {
		t.Errorf("stderr = %q, want silence", stderr)
	}
	if strings.Contains(stdout, "warnings") {
		t.Errorf("the envelope grew a warnings key on an edit that hid "+
			"nothing:\n%s", stdout)
	}
}
