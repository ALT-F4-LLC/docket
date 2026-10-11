package cli

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The engine half of the attachment-list refresh is pinned in
// internal/engine/refresh_files_test.go. What is pinned HERE is the wiring: the
// verb refuses a missing --issue or --reason before reaching the engine, passes
// an engine refusal through with its code, and prints both lists on success.

// filesFrozenIssueInLiveRun is scopedIssueInLiveRun with docs/a.md and
// docs/b.md frozen into the snapshot's files key, as activation would have
// written it, and `attached` as the issue's live attachment list.
func filesFrozenIssueInLiveRun(t *testing.T, conn *sql.DB, attached ...string) (int, int) {
	t.Helper()
	runID, issueID := scopedIssueInLiveRun(t, conn, `["a/**"]`, `["a/**"]`)
	_, err := conn.Exec(
		`UPDATE run_issues SET issue_snapshot = ? WHERE run_id = ? AND issue_id = ?`,
		`{"title":"widen me","kind":"task","labels":[],"scope":["a/**"],`+
			`"files":["docs/a.md","docs/b.md"]}`,
		runID, issueID)
	testsupport.Must(t, err, "freezing the attachment list: %v", err)
	testsupport.Must(t, db.AttachFiles(conn, issueID, attached, ""), "attaching files")
	return runID, issueID
}

// refreshFilesVerb drives the real verb body with a substituted Writer.
func refreshFilesVerb(
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
	w, stdout := bufWriter(false)
	err := runRunRefreshFiles(cmd, model.FormatRunID(runID), w)
	return stdout.String(), err
}

func snapshotFiles(t *testing.T, conn *sql.DB, runID, issueID int) []string {
	t.Helper()
	var blob string
	err := conn.QueryRow(
		`SELECT issue_snapshot FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID).Scan(&blob)
	testsupport.Must(t, err, "reading the snapshot: %v", err)
	var frozen struct {
		Files []string `json:"files"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(blob), &frozen), "decoding the snapshot")
	return frozen.Files
}

// assertFilesFrozen checks that a refused refresh wrote nothing: the snapshot
// keeps its frozen list and no issue-files-refreshed event exists.
func assertFilesFrozen(t *testing.T, conn *sql.DB, runID, issueID int) {
	t.Helper()
	if got := snapshotFiles(t, conn, runID, issueID); !slices.Equal(got,
		[]string{"docs/a.md", "docs/b.md"}) {
		t.Errorf("snapshot files = %v after a refused refresh, want them frozen", got)
	}
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM events WHERE kind = ? AND run_id = ?`,
		engine.EventIssueFilesRefreshed, runID).Scan(&n)
	testsupport.Must(t, err, "counting refresh events: %v", err)
	if n != 0 {
		t.Errorf("%d %s event(s) recorded by a refused refresh",
			n, engine.EventIssueFilesRefreshed)
	}
}

func TestRunRefreshFiles(t *testing.T) {
	t.Run("a detach reaches the snapshot", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := filesFrozenIssueInLiveRun(t, conn, "docs/a.md")

		stdout, err := refreshFilesVerb(t, conn, runID, issueID, "operator detached b")
		testsupport.Must(t, err, "run refresh-files: %v", err)

		for _, want := range []string{
			model.FormatID(issueID), model.FormatRunID(runID),
			"[docs/a.md, docs/b.md]", "[docs/a.md]\n", "fix@2",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout does not name %q:\n%s", want, stdout)
			}
		}
		if got := snapshotFiles(t, conn, runID, issueID); !slices.Equal(got,
			[]string{"docs/a.md"}) {
			t.Errorf("snapshot files = %v, want the live list [docs/a.md]", got)
		}
	})

	t.Run("no --reason", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := filesFrozenIssueInLiveRun(t, conn, "docs/a.md")
		_, err := refreshFilesVerb(t, conn, runID, issueID, "")
		assertRefreshCode(t, err, output.ErrValidation, "--reason")
		assertFilesFrozen(t, conn, runID, issueID)
	})

	t.Run("no --issue", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := filesFrozenIssueInLiveRun(t, conn, "docs/a.md")
		_, err := refreshFilesVerb(t, conn, runID, 0, "operator detached b")
		assertRefreshCode(t, err, output.ErrValidation, "--issue")
		assertFilesFrozen(t, conn, runID, issueID)
	})

	t.Run("an engine refusal passes through with its code", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := filesFrozenIssueInLiveRun(t, conn, "docs/a.md", "docs/b.md")
		stdout, err := refreshFilesVerb(t, conn, runID, issueID, "nothing changed")
		assertRefreshCode(t, err, output.ErrConflict, "issue file add|remove")
		if stdout != "" {
			t.Errorf("a refused refresh printed a success line:\n%s", stdout)
		}
		assertFilesFrozen(t, conn, runID, issueID)
	})
}
