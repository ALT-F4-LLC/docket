package cli

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestIssueCommentListOwnsItsProject: issue comment list resolves its project
// through internal/verb, so the root hook must skip its eager resolution.
func TestIssueCommentListOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(commentListCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", commentListCmd.CommandPath())
	}
}

// TestIssueCommentListReturnsTheIssuesComments: `issue comment list` emits
// every comment on the issue, oldest first, and none from another issue.
func TestIssueCommentListReturnsTheIssuesComments(t *testing.T) {
	conn := newTestDB(t)
	id := createIssue(t, conn, "commented", model.StatusTodo, model.PriorityNone)
	other := createIssue(t, conn, "other", model.StatusTodo, model.PriorityNone)
	for issue, bodies := range map[int][]string{id: {"first", "second"}, other: {"elsewhere"}} {
		for _, body := range bodies {
			_, err := db.CreateComment(conn, &model.Comment{IssueID: issue, Body: body, Author: "tester"})
			testsupport.Must(t, err, "CreateComment(%q): %v", body, err)
		}
	}

	out, err := runLeaseCommandOutput(t, conn, nil, commentListCmd, model.FormatID(id))
	testsupport.Must(t, err, "issue comment list: %v", err)
	var listed struct {
		Data []struct {
			IssueID string `json:"issue_id"`
			Body    string `json:"body"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &listed), "decoding %q", out)
	if len(listed.Data) != 2 || listed.Data[0].Body != "first" || listed.Data[1].Body != "second" {
		t.Fatalf("listed = %+v, want first then second", listed.Data)
	}
	if listed.Data[0].IssueID != model.FormatID(id) {
		t.Errorf("issue_id = %q, want %q", listed.Data[0].IssueID, model.FormatID(id))
	}
}

// TestIssueCommentListOfAMissingIssueIsNotFound names the missing issue.
func TestIssueCommentListOfAMissingIssueIsNotFound(t *testing.T) {
	conn := newTestDB(t)
	_, err := runLeaseCommandOutput(t, conn, nil, commentListCmd, "DKT-9999")
	var cmdError *CmdError
	if !errors.As(err, &cmdError) || cmdError.Code != output.ErrNotFound ||
		cmdError.Error() != "issue DKT-9999 not found" {
		t.Errorf("issue comment list DKT-9999 returned %v, want NOT_FOUND naming the issue", err)
	}
}
