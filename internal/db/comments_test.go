package db

import (
	"errors"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// engineCommentNowMS is a fixed epoch-ms stamp standing in for the caller's
// transaction clock, so a comment's recorded time is a property of the input
// rather than of when the test ran.
const engineCommentNowMS int64 = 1_700_000_000_000

// TestListComments_EmptyIsNotNil pins ListComments' zero-result contract: an
// issue with no comments returns a non-nil empty slice, not nil. A caller
// that serializes the result straight to JSON (internal/cli/issue_comment_list.go)
// depends on this — `null` and `[]` are different wire values for the same
// "no comments yet" state.
func TestListComments_EmptyIsNotNil(t *testing.T) {
	db := mustOpen(t)
	testsupport.Must(t, Initialize(db), "Initialize")
	issueID := createTestIssue(t, db, "no comments", model.StatusTodo, model.PriorityMedium)

	got, err := ListComments(db, issueID)
	testsupport.Must(t, err, "ListComments: %v", err)
	if got == nil {
		t.Fatalf("ListComments returned nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

// TestInsertEngineComment_CommittedRowLands pins that InsertEngineComment
// writes through the caller's transaction: after commit, the row is visible
// with an auto-minted ID and the fixed EngineAuthor convention.
func TestInsertEngineComment_CommittedRowLands(t *testing.T) {
	db := mustOpen(t)
	testsupport.Must(t, Initialize(db), "Initialize")
	issueID := createTestIssue(t, db, "engine comment target", model.StatusTodo, model.PriorityMedium)

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)

	id, err := InsertEngineComment(tx, issueID, "step claimed", engineCommentNowMS)
	testsupport.Must(t, err, "InsertEngineComment: %v", err)
	if id == 0 {
		t.Fatalf("InsertEngineComment returned id 0, want a minted id")
	}

	testsupport.Must(t, tx.Commit(), "Commit")

	got, err := GetComment(db, id)
	testsupport.Must(t, err, "GetComment: %v", err)
	if got.Body != "step claimed" {
		t.Fatalf("Body = %q, want %q", got.Body, "step claimed")
	}
	if got.Author != EngineAuthor {
		t.Fatalf("Author = %q, want %q", got.Author, EngineAuthor)
	}
	if got.IssueID != issueID {
		t.Fatalf("IssueID = %d, want %d", got.IssueID, issueID)
	}
}

// TestInsertEngineComment_RollbackLeavesNoRow pins that InsertEngineComment
// opens no transaction of its own: aborting the caller's transaction rolls
// the insert back cleanly, leaving nothing to see.
func TestInsertEngineComment_RollbackLeavesNoRow(t *testing.T) {
	db := mustOpen(t)
	testsupport.Must(t, Initialize(db), "Initialize")
	issueID := createTestIssue(t, db, "engine comment rollback target", model.StatusTodo, model.PriorityMedium)

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)

	id, err := InsertEngineComment(tx, issueID, "gate opened", engineCommentNowMS)
	testsupport.Must(t, err, "InsertEngineComment: %v", err)

	testsupport.Must(t, tx.Rollback(), "Rollback")

	if _, err := GetComment(db, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetComment after rollback: err = %v, want ErrNotFound", err)
	}
}

// TestInsertEngineComment_StampsTheCallersTime pins that the comment carries
// the caller's transaction time, not a second reading of the wall clock: an
// engine transaction stamps the issue row, the activity log and this comment
// from one nowMS, and a trail whose times drift from the transitions they
// narrate cannot be read back in order.
func TestInsertEngineComment_StampsTheCallersTime(t *testing.T) {
	db := mustOpen(t)
	testsupport.Must(t, Initialize(db), "Initialize")
	issueID := createTestIssue(t, db, "engine comment clock", model.StatusTodo, model.PriorityMedium)

	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	id, err := InsertEngineComment(tx, issueID, "step claimed", engineCommentNowMS)
	testsupport.Must(t, err, "InsertEngineComment: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit")

	got, err := GetComment(db, id)
	testsupport.Must(t, err, "GetComment: %v", err)

	want := time.UnixMilli(engineCommentNowMS).UTC()
	if !got.CreatedAt.Equal(want) {
		t.Fatalf("CreatedAt = %s, want %s", got.CreatedAt.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// TestListComments_SameSecondCommentsKeepInsertionOrder pins the trail's read
// order for the common case: one engine transaction dropping several comments
// on a single `nowMS`, which `created_at` at RFC3339 SECOND resolution cannot
// tell apart.
//
// ON ITS OWN THIS TEST CANNOT FAIL, and that was a finding (DKT-380, finding
// 5): it passed against the old `created_at ASC, id ASC` sort and against an
// untiebroken one alike, because SQLite returns rowid order for an unstable
// sort over a table with no index on the sort key. It pins nothing by itself.
// It is kept as the readable statement of the property; the assertion with
// teeth is TestListComments_StaleStampDoesNotReorderTheTrail, which is
// mutation-verified against the sort it is about.
func TestListComments_SameSecondCommentsKeepInsertionOrder(t *testing.T) {
	db := mustOpen(t)
	testsupport.Must(t, Initialize(db), "Initialize")
	issueID := createTestIssue(t, db, "engine trail order", model.StatusTodo, model.PriorityMedium)

	want := []string{"implement@0 claimed.", "implement@0 is awaiting review.", "implement@0 completed the issue."}
	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	for _, body := range want {
		_, err := InsertEngineComment(tx, issueID, body, engineCommentNowMS)
		testsupport.Must(t, err, "InsertEngineComment: %v", err)
	}
	testsupport.Must(t, tx.Commit(), "Commit")

	got, err := ListComments(db, issueID)
	testsupport.Must(t, err, "ListComments: %v", err)
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i, body := range want {
		if got[i].Body != body {
			t.Fatalf("comment %d = %q, want %q", i, got[i].Body, body)
		}
	}
}

// TestListComments_StaleStampDoesNotReorderTheTrail is DKT-378.
//
// InsertEngineComment takes the CALLER's nowMS, which is right — the
// transition and its narration must carry one time — but it cost the column
// its monotonicity with insertion. The saga threads one nowMS through a whole
// gate execution and `next` drives every ready action step on a single one, so
// a comment committed LATER can carry an EARLIER stamp than one already on the
// table. Sorting by that stamp put the trail out of the order it was written,
// and the `id` tiebreak could not help: a tiebreak cannot repair a primary key
// that is itself out of order.
func TestListComments_StaleStampDoesNotReorderTheTrail(t *testing.T) {
	db := mustOpen(t)
	testsupport.Must(t, Initialize(db), "Initialize")
	issueID := createTestIssue(t, db, "stale stamp", model.StatusTodo, model.PriorityMedium)

	// First written, stamped a minute AHEAD — a gate execution's threaded
	// nowMS, committed before the next transaction's.
	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	_, err = InsertEngineComment(tx, issueID, "first written", engineCommentNowMS+60_000)
	testsupport.Must(t, err, "InsertEngineComment: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit")

	// Second written, stamped from an OLDER clock read.
	tx, err = db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	_, err = InsertEngineComment(tx, issueID, "second written", engineCommentNowMS)
	testsupport.Must(t, err, "InsertEngineComment: %v", err)
	testsupport.Must(t, tx.Commit(), "Commit")

	got, err := ListComments(db, issueID)
	testsupport.Must(t, err, "ListComments: %v", err)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Body != "first written" || got[1].Body != "second written" {
		t.Errorf("trail reads %q then %q; want the order it was WRITTEN in — "+
			"`id` is AUTOINCREMENT and strictly ascending, `created_at` is a "+
			"caller-chosen stamp and is not", got[0].Body, got[1].Body)
	}
	if !got[0].CreatedAt.After(got[1].CreatedAt) {
		t.Fatal("premise: the first row must carry the LATER stamp, or this " +
			"test is not exercising the out-of-order case")
	}
}
