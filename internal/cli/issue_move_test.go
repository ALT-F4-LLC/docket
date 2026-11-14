package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestIssueMoveOwnsItsProject: issue move resolves its project through
// internal/verb, so the root hook must skip its eager resolution.
func TestIssueMoveOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(moveCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", moveCmd.CommandPath())
	}
}

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

// TestIssueMoveThroughRootMigratesTheSubtree: `--project` re-homes a root
// issue and its sub-issues, and the JSON payload names the target project and
// every migrated issue.
func TestIssueMoveThroughRootMigratesTheSubtree(t *testing.T) {
	conn := newVoteCreateStore(t)
	root := createIssue(t, conn, "misfiled", model.StatusBacklog, model.PriorityNone)
	child := createIssue(t, conn, "misfiled child", model.StatusBacklog, model.PriorityNone)
	testsupport.Must(t, db.UpdateIssue(conn, child, map[string]interface{}{"parent_id": root}, "test"),
		"reparenting the child")
	target, err := db.EnsureProject(conn, "/repos/elsewhere", "elsewhere", model.NowMS())
	testsupport.Must(t, err, "EnsureProject: %v", err)

	out, err := runThroughRoot(t, moveCmd, "issue", "move", model.FormatID(root), "--project", "/repos/elsewhere", "--json")
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
