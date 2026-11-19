package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func newCommentSubject(t *testing.T, db *sql.DB) int {
	t.Helper()
	id, err := CreateIssue(db, probeIssue(), nil, nil)
	testsupport.Must(t, err, "creating the comment subject: %v", err)
	return id
}

func countCommentsOn(t *testing.T, db *sql.DB, issueID int) int {
	t.Helper()
	comments, err := ListComments(db, issueID)
	testsupport.Must(t, err, "ListComments: %v", err)
	return len(comments)
}

// TestCreateCommentIdempotentReplaysIdenticalContent: a key reused with the
// same issue, author and body returns the original comment and inserts nothing.
func TestCreateCommentIdempotentReplaysIdenticalContent(t *testing.T) {
	db := newIdemDB(t)
	issueID := newCommentSubject(t, db)

	first, err := CreateCommentIdempotent(db,
		&model.Comment{IssueID: issueID, Author: "alice", Body: "same body"}, "comment-key")
	testsupport.Must(t, err, "first comment: %v", err)
	replay, err := CreateCommentIdempotent(db,
		&model.Comment{IssueID: issueID, Author: "alice", Body: "same body"}, "comment-key")
	testsupport.Must(t, err, "identical replay: %v", err)

	if replay != first {
		t.Errorf("identical replay returned comment %d, want the original %d", replay, first)
	}
	if n := countCommentsOn(t, db, issueID); n != 1 {
		t.Errorf("comment count = %d, want 1: the replay inserted a duplicate", n)
	}
}

// TestCreateCommentIdempotentRejectsDifferentContent: a key reused with a
// different issue, author or body is a conflict naming the key, and inserts
// nothing.
func TestCreateCommentIdempotentRejectsDifferentContent(t *testing.T) {
	db := newIdemDB(t)
	issueID := newCommentSubject(t, db)
	otherIssueID := newCommentSubject(t, db)

	original := model.Comment{IssueID: issueID, Author: "alice", Body: "original body"}
	first, err := CreateCommentIdempotent(db, &original, "comment-key")
	testsupport.Must(t, err, "first comment: %v", err)

	cases := map[string]model.Comment{
		"different body":   {IssueID: issueID, Author: "alice", Body: "edited body"},
		"different author": {IssueID: issueID, Author: "bob", Body: "original body"},
		"different issue":  {IssueID: otherIssueID, Author: "alice", Body: "original body"},
	}
	for name, changed := range cases {
		t.Run(name, func(t *testing.T) {
			id, err := CreateCommentIdempotent(db, &changed, "comment-key")
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("reuse returned (%d, %v), want an ErrConflict", id, err)
			}
			if !strings.Contains(err.Error(), "comment-key") {
				t.Errorf("conflict %q does not name the key comment-key", err)
			}
		})
	}

	if n := countCommentsOn(t, db, issueID); n != 1 {
		t.Errorf("comments on the original issue = %d, want 1", n)
	}
	if n := countCommentsOn(t, db, otherIssueID); n != 0 {
		t.Errorf("comments on the other issue = %d, want 0", n)
	}
	replay, err := CreateCommentIdempotent(db, &original, "comment-key")
	testsupport.Must(t, err, "replay after the rejected reuses: %v", err)
	if replay != first {
		t.Errorf("replay after the rejected reuses returned %d, want %d", replay, first)
	}
}
