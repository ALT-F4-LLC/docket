package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// runIssueLogCmd drives `issue log <ref> --limit <limit>` with --json set to
// jsonFlag ("" for text), restoring both flags afterwards.
func runIssueLogCmd(t *testing.T, conn *sql.DB, jsonFlag string, ref string, limit int) (string, error) {
	t.Helper()
	testsupport.Must(t, rootCmd.PersistentFlags().Set("json", jsonFlag), "set --json")
	testsupport.Must(t, logCmd.Flags().Set("limit", strconv.Itoa(limit)), "set --limit")
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("json", "")
		f := logCmd.Flags().Lookup("limit")
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
	logCmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	stdout := captureStdout(t)
	err := logCmd.RunE(logCmd, []string{ref})
	return stdout(), err
}

// issueWithActivity creates an issue carrying more than two activity entries.
func issueWithActivity(t *testing.T, conn *sql.DB) int {
	t.Helper()
	id := createIssue(t, conn, "logged", model.StatusTodo, model.PriorityNone)
	for i := range 3 {
		_, err := db.CreateComment(conn, &model.Comment{IssueID: id, Body: fmt.Sprintf("c%d", i), Author: "tester"})
		testsupport.Must(t, err, "CreateComment: %v", err)
	}
	return id
}

// TestIssueLogOwnsItsProject: issue log resolves its project through
// internal/verb, so the root hook must skip its eager resolution.
func TestIssueLogOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(logCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", logCmd.CommandPath())
	}
}

// TestIssueLogV2ReportsThePreLimitTotal: under --json=v2 the envelope carries
// the capped entries, the full activity count and whether the cap dropped any.
func TestIssueLogV2ReportsThePreLimitTotal(t *testing.T) {
	conn := newTestDB(t)
	id := issueWithActivity(t, conn)
	all, err := db.CountActivity(conn, id)
	testsupport.Must(t, err, "CountActivity: %v", err)

	out, err := runIssueLogCmd(t, conn, "v2", model.FormatID(id), 2)
	testsupport.Must(t, err, "issue log: %v", err)
	var logged struct {
		Data struct {
			Items     []json.RawMessage `json:"items"`
			Total     int               `json:"total"`
			Truncated bool              `json:"truncated"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &logged), "decoding %q", out)
	if len(logged.Data.Items) != 2 || logged.Data.Total != all || !logged.Data.Truncated {
		t.Errorf("log = %d items, total %d, truncated %v; want 2, %d, true",
			len(logged.Data.Items), logged.Data.Total, logged.Data.Truncated, all)
	}
}

// TestIssueLogNegativeLimit: text mode clamps a negative --limit to one entry,
// as v1 always has, while --json=v2 refuses it.
func TestIssueLogNegativeLimit(t *testing.T) {
	conn := newTestDB(t)
	id := issueWithActivity(t, conn)
	ref := model.FormatID(id)

	out, err := runIssueLogCmd(t, conn, "", ref, -1)
	testsupport.Must(t, err, "issue log --limit -1: %v", err)
	if !strings.Contains(out, "Activity for "+ref+":") || strings.Count(out, "\n  ") != 1 {
		t.Errorf("text log with --limit -1 = %q, want the header and one entry", out)
	}

	_, err = runIssueLogCmd(t, conn, "v2", ref, -1)
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrValidation ||
		cmdError.Error() != "--limit must be >= 0, got -1 (0 means no limit)" {
		t.Errorf("issue log --json=v2 --limit -1 returned %v, want the limit refusal", err)
	}
}

// TestIssueLogOfAMissingIssueIsNotFound: the missing issue is reported before
// the limit is judged.
func TestIssueLogOfAMissingIssueIsNotFound(t *testing.T) {
	conn := newTestDB(t)
	_, err := runIssueLogCmd(t, conn, "v2", "DKT-9999", -1)
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrNotFound ||
		cmdError.Error() != "issue DKT-9999 not found" {
		t.Errorf("issue log DKT-9999 returned %v, want NOT_FOUND naming the issue", err)
	}
}
