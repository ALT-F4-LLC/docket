package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestIssueEditOwnsItsProject: issue edit resolves its project through
// internal/verb, so the root hook must skip its eager resolution.
func TestIssueEditOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(editCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", editCmd.CommandPath())
	}
}

// TestIssueEditThroughRootReportsTheEdit: an edit run the way an operator
// runs it writes the change and reports it in human mode, and an empty edit
// echoes the issue without writing.
func TestIssueEditThroughRootReportsTheEdit(t *testing.T) {
	conn := newVoteCreateStore(t)
	id := createIssue(t, conn, "before", model.StatusTodo, model.PriorityNone)
	ref := model.FormatID(id)

	out, err := runThroughRoot(t, editCmd, "issue", "edit", ref, "-t", "after", "-s", "in-progress")
	testsupport.Must(t, err, "issue edit: %v", err)
	if want := "Updated " + ref + ": after"; !strings.Contains(out, want) {
		t.Errorf("output %q does not report %q", out, want)
	}
	stored, err := db.GetIssue(conn, id)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if stored.Title != "after" || stored.Status != model.StatusInProgress {
		t.Errorf("stored = %q/%s, want after/in-progress", stored.Title, stored.Status)
	}

	before, err := db.GetVersion(conn, "issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	out, err = runThroughRoot(t, editCmd, "issue", "edit", ref, "--json")
	testsupport.Must(t, err, "empty issue edit: %v", err)
	var env struct {
		Data struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Data.Title != "after" {
		t.Errorf("empty edit printed %q (%v), want the unchanged issue", out, err)
	}
	if after, _ := db.GetVersion(conn, "issues", id); after != before {
		t.Errorf("version %d -> %d, want an empty edit to write nothing", before, after)
	}
}

// TestIssueEditThroughRootMapsAStaleVersion: the verb's CAS refusal reaches
// the operator as CONFLICT naming the issue as they typed it.
func TestIssueEditThroughRootMapsAStaleVersion(t *testing.T) {
	conn := newVoteCreateStore(t)
	id := createIssue(t, conn, "contended", model.StatusTodo, model.PriorityNone)
	ref := model.FormatID(id)

	_, err := runThroughRoot(t, editCmd, "issue", "edit", ref, "-t", "late", "--if-version", "99")
	if got := codeOf(t, err); got != output.ErrConflict {
		t.Errorf("code = %s, want %s", got, output.ErrConflict)
	}
	if want := "issue " + ref + " was modified by another writer"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err, want)
	}
}
