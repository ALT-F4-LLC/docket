package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The engine half of the body refresh is pinned in
// internal/engine/refresh_body_test.go. What is pinned HERE is the wiring: the
// verb exists under `run`, refuses a missing --issue or --reason before reaching
// the engine, and hands the engine's answer back to the caller.

const frozenBody = "AC1: the original wording"

// bodyFrozenIssueInLiveRun is scopedIssueInLiveRun with a description frozen
// into the run's body snapshot, as activation would have written it.
func bodyFrozenIssueInLiveRun(t *testing.T, conn *sql.DB) (int, int) {
	t.Helper()
	runID, issueID := scopedIssueInLiveRun(t, conn, `["a/**"]`, `["a/**"]`)
	_, err := conn.Exec(`UPDATE issues SET description = ? WHERE id = ?`,
		frozenBody, issueID)
	testsupport.Must(t, err, "declaring the description: %v", err)
	_, err = conn.Exec(
		`UPDATE run_issues SET body_snapshot = ?, body_sha256 = ?
		  WHERE run_id = ? AND issue_id = ?`,
		frozenBody, workflow.SHA256([]byte(frozenBody)), runID, issueID)
	testsupport.Must(t, err, "freezing the body: %v", err)
	return runID, issueID
}

// refreshBodyVerb drives the real verb body with a substituted Writer.
func refreshBodyVerb(
	t *testing.T, conn *sql.DB, runID, issueID int, reason string,
) (string, error) {
	t.Helper()
	cmd := cmdWithDB(conn)
	cmd.Flags().String("issue", "", "")
	cmd.Flags().String("reason", "", "")
	if issueID != 0 {
		testsupport.Must(t, cmd.Flags().Set("issue", model.FormatID(issueID)),
			"setting --issue")
	}
	if reason != "" {
		testsupport.Must(t, cmd.Flags().Set("reason", reason), "setting --reason")
	}
	w, stdout := bufWriter(true)
	err := runRunRefreshBody(cmd, model.FormatRunID(runID), w)
	return stdout.String(), err
}

func TestRunRefreshBodyCLIWiring(t *testing.T) {
	t.Run("resolves under run", func(t *testing.T) {
		cmd, _, err := runCmd.Find([]string{"refresh-body"})
		if err != nil || cmd == runCmd || cmd.Name() != "refresh-body" {
			t.Fatalf("`run refresh-body` does not resolve to its own command "+
				"(got %v, err %v)", cmd.Name(), err)
		}
	})

	t.Run("no --issue", func(t *testing.T) {
		conn := newTestDB(t)
		runID, _ := bodyFrozenIssueInLiveRun(t, conn)
		_, err := refreshBodyVerb(t, conn, runID, 0, "operator amended AC1")
		assertRefreshCode(t, err, output.ErrValidation, "--issue")
	})

	t.Run("no --reason", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := bodyFrozenIssueInLiveRun(t, conn)
		_, err := refreshBodyVerb(t, conn, runID, issueID, "")
		assertRefreshCode(t, err, output.ErrValidation, "--reason")
	})

	t.Run("an amended description reaches the snapshot", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := bodyFrozenIssueInLiveRun(t, conn)
		const amended = "AC1: the operator's amended wording"
		_, err := conn.Exec(`UPDATE issues SET description = ? WHERE id = ?`,
			amended, issueID)
		testsupport.Must(t, err, "amending the description: %v", err)

		stdout, err := refreshBodyVerb(t, conn, runID, issueID, "operator amended AC1")
		testsupport.Must(t, err, "run refresh-body: %v", err)

		var envelope struct {
			OK   bool `json:"ok"`
			Data struct {
				Run        string   `json:"run"`
				Issue      string   `json:"issue"`
				FromSHA256 string   `json:"from_sha256"`
				ToSHA256   string   `json:"to_sha256"`
				Steps      []string `json:"steps"`
			} `json:"data"`
		}
		testsupport.Must(t, json.Unmarshal([]byte(stdout), &envelope), "decoding the envelope")
		if !envelope.OK {
			t.Fatalf("envelope is not ok: %s", stdout)
		}
		d := envelope.Data
		if d.Run != model.FormatRunID(runID) || d.Issue != model.FormatID(issueID) {
			t.Errorf("data = %+v, want the run and issue named", d)
		}
		if d.FromSHA256 != workflow.SHA256([]byte(frozenBody)) {
			t.Errorf("from_sha256 = %s, want the frozen body's digest", d.FromSHA256)
		}
		if d.ToSHA256 != workflow.SHA256([]byte(amended)) || d.ToSHA256 == d.FromSHA256 {
			t.Errorf("to_sha256 = %s, want the amended body's digest", d.ToSHA256)
		}
		if len(d.Steps) != 1 || d.Steps[0] != "fix@2" {
			t.Errorf("steps = %v, want the reached instance", d.Steps)
		}
	})
}

// TestRunRefreshBodyHelpStatesProperties: the four properties that keep the
// refresh from being a hole in the freeze are what an operator reads before
// using it, so each is pinned as its own phrase.
func TestRunRefreshBodyHelpStatesProperties(t *testing.T) {
	help := strings.Join(strings.Fields(helpOutput(t, "run", "refresh-body")), " ")
	for name, phrase := range map[string]string{
		"verbatim copy":       "copies the issue's live description verbatim",
		"equal refusal":       "refuses when the live description already equals the snapshot",
		"quiescence refusal":  "refuses while any of the issue's steps is claimed, running, or gated, or while a dispatch is open",
		"no history rewrites": "rewrites no already-recorded step context",
	} {
		if !strings.Contains(help, phrase) {
			t.Errorf("--help does not state the %s property (%q):\n%s", name, phrase, help)
		}
	}
}
