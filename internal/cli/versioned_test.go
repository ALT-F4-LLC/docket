package cli

import (
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// successData writes payload through a JSON writer and returns the envelope's
// decoded .data object.
func successData(t *testing.T, payload any) map[string]json.RawMessage {
	t.Helper()
	w, buf := bufWriter(true)
	w.JSONVersion = output.JSONV2
	w.Success(payload, "")
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("decoding envelope %s: %v", buf.String(), err)
	}
	return env.Data
}

// TestSingleIssueWriteCarriesItsVersion: a verb that answers with one issue
// (create, edit, move, close, reopen) puts the issue's CAS version in .data,
// so the caller can pass it straight to the next --if-version write.
func TestSingleIssueWriteCarriesItsVersion(t *testing.T) {
	data := successData(t, withIssueVersion(&model.Issue{ID: 3, Title: "t", Version: 7}))
	if got := string(data["version"]); got != "7" {
		t.Errorf(".data.version = %q, want 7; data = %v", got, data)
	}
	if got := string(data["id"]); got != `"DKT-3"` {
		t.Errorf(".data.id = %s, want \"DKT-3\"", got)
	}
}

// TestSingleRunWriteCarriesItsRowVersion: the run-side twin, keyed row_version.
func TestSingleRunWriteCarriesItsRowVersion(t *testing.T) {
	data := successData(t, withRunVersion(&model.Run{ID: 4, Status: model.RunActive, RowVersion: 5}))
	if got := string(data["row_version"]); got != "5" {
		t.Errorf(".data.row_version = %q, want 5; data = %v", got, data)
	}
}

// TestRunAbandonPayloadCarriesWorktrees: the abandoned run's orphaned
// checkouts ride beside the versioned run, and the key is absent when there
// are none.
func TestRunAbandonPayloadCarriesWorktrees(t *testing.T) {
	run := &model.Run{ID: 4, Status: model.RunAbandoned, RowVersion: 2}

	data := successData(t, runAbandonPayload{run: run, worktrees: []string{"/w/a"}})
	if got := string(data["worktrees"]); got != `["/w/a"]` {
		t.Errorf(".data.worktrees = %s, want [\"/w/a\"]", got)
	}
	if got := string(data["row_version"]); got != "2" {
		t.Errorf(".data.row_version = %q, want 2", got)
	}

	bare := successData(t, runAbandonPayload{run: run})
	if _, ok := bare["worktrees"]; ok {
		t.Errorf("an abandon with no worktrees carries the key: %v", bare)
	}
}
