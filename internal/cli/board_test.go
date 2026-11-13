package cli

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

func TestBoardCmdOwnsItsProject(t *testing.T) {
	if _, ok := boardCmd.Annotations["verbOwnsProject"]; !ok {
		t.Error("board lacks the verbOwnsProject annotation")
	}
}

func TestBoardTextShowsSubIssueProgressOnRolledUpParent(t *testing.T) {
	conn := newTestDB(t)
	parent := createIssue(t, conn, "parent", model.StatusInProgress, model.PriorityHigh)
	for _, status := range []model.Status{model.StatusDone, model.StatusTodo} {
		if _, err := db.CreateIssue(conn, &model.Issue{
			ParentID: &parent,
			Title:    "child " + string(status),
			Status:   status,
			Priority: model.PriorityHigh,
			Kind:     model.IssueKindTask,
		}, nil, nil); err != nil {
			t.Fatalf("CreateIssue(child %s): %v", status, err)
		}
	}

	w, buf := bufWriter(false)
	if err := runBoard(boardCmdWithDB(conn, false), nil, w); err != nil {
		t.Fatalf("runBoard: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "1/2") {
		t.Errorf("board output lacks the parent's 1/2 sub-issue progress:\n%s", out)
	}
	if strings.Contains(out, "child ") {
		t.Errorf("board output shows a sub-issue without --expand:\n%s", out)
	}
}

func TestBoardRejectsUnknownPriority(t *testing.T) {
	conn := newTestDB(t)
	cmd := boardCmdWithDB(conn, false)
	setFlags(t, cmd, map[string]string{"priority": "urgent"})

	w, _ := bufWriter(true)
	err := runBoard(cmd, nil, w)
	ce, ok := err.(*CmdError)
	if !ok || ce.Code != "VALIDATION_ERROR" {
		t.Errorf("runBoard --priority urgent: err = %#v, want a VALIDATION_ERROR CmdError", err)
	}
}
