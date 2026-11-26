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

// TestIssueMoveThroughRootReportsTheMove: a status move run the way an
// operator runs it reports both statuses, and a move to the status the issue
// already has echoes the issue without writing.
func TestIssueMoveThroughRootReportsTheMove(t *testing.T) {
	conn := newVoteCreateStore(t)
	id := createIssue(t, conn, "in flight", model.StatusTodo, model.PriorityNone)
	ref := model.FormatID(id)

	out, err := runThroughRoot(t, moveCmd, "issue", "move", ref, "review")
	testsupport.Must(t, err, "issue move: %v", err)
	if want := "Moved " + ref + ": todo → review"; !strings.Contains(out, want) {
		t.Errorf("output %q does not report %q", out, want)
	}

	before, err := db.GetVersion(conn, "issues", id)
	testsupport.Must(t, err, "GetVersion: %v", err)
	out, err = runThroughRoot(t, moveCmd, "issue", "move", ref, "review", "--json")
	testsupport.Must(t, err, "no-op issue move: %v", err)
	if got := issueStatusEchoed(t, out); got != model.StatusReview {
		t.Errorf("echoed status = %q, want review", got)
	}
	if after, _ := db.GetVersion(conn, "issues", id); after != before {
		t.Errorf("version %d -> %d, want a no-op move to write nothing", before, after)
	}
}

// TestIssueMoveThroughRootRefusesAnEmptyStatus:an empty status positional is
// a status the caller gave, not an absent one, so it is refused as an invalid
// status, and with --project as a status move combined with a migration.
func TestIssueMoveThroughRootRefusesAnEmptyStatus(t *testing.T) {
	conn := newVoteCreateStore(t)
	id := createIssue(t, conn, "misfiled", model.StatusBacklog, model.PriorityNone)
	ref := model.FormatID(id)
	otherProject(t, conn)
	home := issueState(t, conn, id).ProjectID

	cases := []struct {
		name string
		args []string
		text string
	}{
		{"alone", []string{"issue", "move", ref, ""}, `invalid status ""`},
		{"with --project", []string{"issue", "move", ref, "", "--project", "/repo/other"}, "separate operations"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := runThroughRoot(t, moveCmd, c.args...)
			if got := codeOf(t, err); got != output.ErrValidation {
				t.Errorf("code = %s, want %s", got, output.ErrValidation)
			}
			if err == nil || !strings.Contains(err.Error(), c.text) {
				t.Errorf("error = %v, want it to contain %q", err, c.text)
			}
		})
	}
	if got := issueState(t, conn, id); got.ProjectID != home || got.Status != model.StatusBacklog {
		t.Errorf("issue = project %d status %q, want the refusals to leave it in place", got.ProjectID, got.Status)
	}
}

// TestIssueMoveThroughRootMigratesTheSubtree: `--project` re-homes a root
// issue and its sub-issues, and the JSON payload names the target project and
// every migrated issue.
func TestIssueMoveThroughRootMigratesTheSubtree(t *testing.T) {
	conn := newVoteCreateStore(t)
	root := createIssue(t, conn, "misfiled", model.StatusBacklog, model.PriorityNone)
	child := createIssue(t, conn, "misfiled child", model.StatusBacklog, model.PriorityNone)
	testsupport.Must(t, db.UpdateIssue(conn, child, map[string]interface{}{"parent_id": root}, "test"),
		"reparenting the child")
	target := otherProject(t, conn)

	out, err := runThroughRoot(t, moveCmd, "issue", "move", model.FormatID(root), "--project", "/repo/other", "--json")
	testsupport.Must(t, err, "issue move --project: %v", err)

	var env struct {
		Data struct {
			Project struct {
				ID int `json:"id"`
			} `json:"project"`
			Migrated []string `json:"migrated"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("migration payload is not JSON: %v\n%s", err, out)
	}
	if env.Data.Project.ID != target || len(env.Data.Migrated) != 2 {
		t.Errorf("payload project %d migrated %v, want project %d and both issues", env.Data.Project.ID, env.Data.Migrated, target)
	}
	for _, id := range []int{root, child} {
		if got := issueState(t, conn, id).ProjectID; got != target {
			t.Errorf("issue %d project = %d, want %d", id, got, target)
		}
	}
}
