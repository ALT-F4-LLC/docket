package cli

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-869 at the verb level. The engine half — that the refresh is real, that
// it copies only what the gated writer declared, and that it refuses every
// straddle — is pinned in internal/engine/dkt869_test.go. What is pinned HERE
// is the wiring an operator and a conductor actually meet, on the fixture
// RUN-52 was: an issue with a pending `fix@2` step in an activated run.

// refreshScope drives the real verb body with a substituted Writer.
func refreshScope(
	t *testing.T, conn *sql.DB, runID, issueID int, reason string, jsonMode bool,
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

	w, stdout := bufWriter(jsonMode)
	err := runRunRefreshScope(cmd, model.FormatRunID(runID), w)
	return stdout.String(), err
}

// TestRunRefreshScopeMakesTheWidenReachTheStep is the acceptance path: the
// operator widens, then refreshes, and the run's snapshot now carries what the
// issue declares.
func TestRunRefreshScopeMakesTheWidenReachTheStep(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := scopedIssueInLiveRun(t, conn,
		`["cli/src/command/start.rs"]`, `["cli/src/command/start.rs"]`)

	// The authorized widen, exactly as the conductor ran it.
	editWithScope(t, conn, issueID, false,
		"cli/src/command/start.rs", "script/install.sh", "makefile")

	stdout, err := refreshScope(t, conn, runID, issueID, "panel agreed to widen", false)
	testsupport.Must(t, err, "run refresh-scope: %v", err)

	for _, want := range []string{
		model.FormatID(issueID), model.FormatRunID(runID),
		"script/install.sh", "makefile", "fix@2",
		// The half an operator must not have to infer.
		"already recorded keep the scope they ran under",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not name %q:\n%s", want, stdout)
		}
	}

	if got := snapshotScopeJSON(t, conn, runID, issueID); got !=
		`["cli/src/command/start.rs","script/install.sh","makefile"]` {
		t.Errorf("snapshot scope = %s, want the widened declaration", got)
	}
}

// TestRunRefreshScopeJSONEnvelope is the other channel, and the one that
// matters most: RUN-52's widen was typed by a CONDUCTOR.
func TestRunRefreshScopeJSONEnvelope(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := scopedIssueInLiveRun(t, conn,
		`["internal/a/**"]`, `["internal/a/**"]`)
	editWithScope(t, conn, issueID, false, "internal/a/**", "internal/b/**")

	stdout, err := refreshScope(t, conn, runID, issueID, "authorized", true)
	testsupport.Must(t, err, "run refresh-scope: %v", err)

	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Run   string   `json:"run"`
			Issue string   `json:"issue"`
			From  []string `json:"from"`
			To    []string `json:"to"`
			Steps []string `json:"steps"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(stdout), &envelope), "decoding the envelope")
	if !envelope.OK {
		t.Fatalf("envelope is not ok: %s", stdout)
	}
	if envelope.Data.Run != model.FormatRunID(runID) ||
		envelope.Data.Issue != model.FormatID(issueID) {
		t.Errorf("data = %+v, want the run and issue named", envelope.Data)
	}
	if len(envelope.Data.From) != 1 || len(envelope.Data.To) != 2 {
		t.Errorf("data = %+v, want both scopes — a caller has to be able to "+
			"report what moved", envelope.Data)
	}
	if len(envelope.Data.Steps) != 1 || envelope.Data.Steps[0] != "fix@2" {
		t.Errorf("data.steps = %v, want the reached instance", envelope.Data.Steps)
	}
}

// TestRunRefreshScopeRequiresItsArguments: both flags are load-bearing and
// neither has a safe default. A guessed issue would move a snapshot nobody
// named; a missing reason leaves a trail that shows the scope changing with
// nothing saying why.
func TestRunRefreshScopeRequiresItsArguments(t *testing.T) {
	t.Run("no --issue", func(t *testing.T) {
		conn := newTestDB(t)
		runID, _ := scopedIssueInLiveRun(t, conn, `["a/**"]`, `["a/**"]`)
		_, err := refreshScope(t, conn, runID, 0, "authorized", false)
		assertRefreshCode(t, err, output.ErrValidation, "--issue")
	})

	t.Run("no --reason", func(t *testing.T) {
		conn := newTestDB(t)
		runID, issueID := scopedIssueInLiveRun(t, conn, `["a/**"]`, `["a/**"]`)
		editWithScope(t, conn, issueID, false, "a/**", "b/**")
		_, err := refreshScope(t, conn, runID, issueID, "", false)
		assertRefreshCode(t, err, output.ErrValidation, "--reason")

		// And nothing was written on the way to the refusal.
		if got := snapshotScopeJSON(t, conn, runID, issueID); got != `["a/**"]` {
			t.Errorf("snapshot scope = %s after a refused refresh, want it frozen", got)
		}
	})
}

// TestRunRefreshScopeWithoutAWidenIsRefused is the gate, at the verb: the only
// way to change what this verb copies is `issue edit --scope`, so a refresh
// with no widen behind it must fail rather than record a no-op ruling.
func TestRunRefreshScopeWithoutAWidenIsRefused(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := scopedIssueInLiveRun(t, conn, `["a/**"]`, `["a/**"]`)

	_, err := refreshScope(t, conn, runID, issueID, "authorized", false)
	assertRefreshCode(t, err, output.ErrConflict, "issue edit")
}

// TestRunRefreshScopeRefusesAClaimedStep is the straddle refusal reaching the
// operator with the right exit code — a CONFLICT is retryable once the
// executor records, and a caller keying on the code needs to see that.
func TestRunRefreshScopeRefusesAClaimedStep(t *testing.T) {
	conn := newTestDB(t)
	runID, issueID := scopedIssueInLiveRun(t, conn, `["a/**"]`, `["a/**"]`)
	editWithScope(t, conn, issueID, false, "a/**", "b/**")
	_, err := conn.Exec(`UPDATE steps SET status = ? WHERE run_id = ?`,
		db.StepClaimed, runID)
	testsupport.Must(t, err, "claiming the step: %v", err)

	_, refreshErr := refreshScope(t, conn, runID, issueID, "authorized", false)
	assertRefreshCode(t, refreshErr, output.ErrConflict, "fix@2")
}

func assertRefreshCode(t *testing.T, err error, want output.ErrorCode, names string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the refresh was accepted; want %v naming %q", want, names)
	}
	var ce *CmdError
	if !asCmdError(err, &ce) {
		t.Fatalf("error %v is not a CmdError", err)
	}
	if ce.Code != want {
		t.Errorf("error code = %v, want %v: %v", ce.Code, want, err)
	}
	if !strings.Contains(err.Error(), names) {
		t.Errorf("the refusal does not name %q:\n%s", names, err)
	}
}

func snapshotScopeJSON(t *testing.T, conn *sql.DB, runID, issueID int) string {
	t.Helper()
	var blob string
	err := conn.QueryRow(
		`SELECT issue_snapshot FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID).Scan(&blob)
	testsupport.Must(t, err, "reading the snapshot: %v", err)
	var frozen struct {
		Scope []string `json:"scope"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(blob), &frozen), "decoding the snapshot")
	out, err := json.Marshal(frozen.Scope)
	testsupport.Must(t, err, "encoding the scope: %v", err)
	return string(out)
}
