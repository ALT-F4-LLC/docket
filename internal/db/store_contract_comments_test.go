package db_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// engineCommentNowMS is a fixed epoch-ms stamp standing in for the caller's
// transaction clock, so a comment's recorded time is a property of the input
// rather than of when the test ran.
const engineCommentNowMS int64 = 1_700_000_000_000

// commentContract pins the comment trail: engine comments written through the
// caller's transaction, and the order ListComments reads them back in.
var commentContract = []contractCase{
	{"ListCommentsEmptyIsNotNil", contractListCommentsEmptyIsNotNil},
	{"InsertEngineCommentCommittedRowLands", contractInsertEngineCommentCommittedRowLands},
	{"InsertEngineCommentRollbackLeavesNoRow", contractInsertEngineCommentRollbackLeavesNoRow},
	{"InsertEngineCommentStampsTheCallersTime", contractInsertEngineCommentStampsTheCallersTime},
	{"ListCommentsSameSecondCommentsKeepInsertionOrder", contractListCommentsSameSecondCommentsKeepInsertionOrder},
	{"ListCommentsStaleStampDoesNotReorderTheTrail", contractListCommentsStaleStampDoesNotReorderTheTrail},
}

// insertEngineComments writes bodies as engine comments in one committed
// transaction, all stamped nowMS.
func insertEngineComments(t *testing.T, st store.Store, issueID int, nowMS int64, bodies ...string) {
	t.Helper()
	tx, err := st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()
	for _, body := range bodies {
		_, err := tx.InsertEngineComment(issueID, body, nowMS)
		testsupport.Must(t, err, "InsertEngineComment: %v", err)
	}
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)
}

// contractListCommentsEmptyIsNotNil pins ListComments' zero-result contract:
// an issue with no comments returns a non-nil empty slice, not nil. A caller
// that serializes the result straight to JSON (internal/cli/issue_comment_list.go)
// depends on this — `null` and `[]` are different wire values for the same
// "no comments yet" state.
func contractListCommentsEmptyIsNotNil(t *testing.T, st store.Store) {
	issueID := createContractIssue(t, st, "no comments")

	got, err := st.ListComments(issueID)
	testsupport.Must(t, err, "ListComments: %v", err)
	if got == nil {
		t.Fatalf("ListComments returned nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

// contractInsertEngineCommentCommittedRowLands pins that InsertEngineComment
// writes through the caller's transaction: after commit, the row is visible
// with an auto-minted ID and the fixed EngineAuthor convention.
func contractInsertEngineCommentCommittedRowLands(t *testing.T, st store.Store) {
	issueID := createContractIssue(t, st, "engine comment target")

	tx, err := st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)

	id, err := tx.InsertEngineComment(issueID, "step claimed", engineCommentNowMS)
	testsupport.Must(t, err, "InsertEngineComment: %v", err)
	if id == 0 {
		t.Fatalf("InsertEngineComment returned id 0, want a minted id")
	}

	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	got, err := st.GetComment(id)
	testsupport.Must(t, err, "GetComment: %v", err)
	if got.Body != "step claimed" {
		t.Fatalf("Body = %q, want %q", got.Body, "step claimed")
	}
	if got.Author != db.EngineAuthor {
		t.Fatalf("Author = %q, want %q", got.Author, db.EngineAuthor)
	}
	if got.IssueID != issueID {
		t.Fatalf("IssueID = %d, want %d", got.IssueID, issueID)
	}
}

// contractInsertEngineCommentRollbackLeavesNoRow pins that InsertEngineComment
// opens no transaction of its own: aborting the caller's transaction rolls
// the insert back cleanly, leaving nothing to see.
func contractInsertEngineCommentRollbackLeavesNoRow(t *testing.T, st store.Store) {
	issueID := createContractIssue(t, st, "engine comment rollback target")

	tx, err := st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)

	id, err := tx.InsertEngineComment(issueID, "gate opened", engineCommentNowMS)
	testsupport.Must(t, err, "InsertEngineComment: %v", err)

	err = tx.Rollback()
	testsupport.Must(t, err, "Rollback: %v", err)

	if _, err := st.GetComment(id); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("GetComment after rollback: err = %v, want ErrNotFound", err)
	}
}

// contractInsertEngineCommentStampsTheCallersTime pins that the comment
// carries the caller's transaction time, not a second reading of the wall
// clock: an engine transaction stamps the issue row, the activity log and this
// comment from one nowMS, and a trail whose times drift from the transitions
// they narrate cannot be read back in order.
func contractInsertEngineCommentStampsTheCallersTime(t *testing.T, st store.Store) {
	issueID := createContractIssue(t, st, "engine comment clock")

	tx, err := st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	id, err := tx.InsertEngineComment(issueID, "step claimed", engineCommentNowMS)
	testsupport.Must(t, err, "InsertEngineComment: %v", err)
	err = tx.Commit()
	testsupport.Must(t, err, "Commit: %v", err)

	got, err := st.GetComment(id)
	testsupport.Must(t, err, "GetComment: %v", err)

	want := time.UnixMilli(engineCommentNowMS).UTC()
	if !got.CreatedAt.Equal(want) {
		t.Fatalf("CreatedAt = %s, want %s", got.CreatedAt.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// contractListCommentsSameSecondCommentsKeepInsertionOrder pins the trail's
// read order for the common case: one engine transaction dropping several
// comments on a single `nowMS`, which `created_at` at RFC3339 SECOND
// resolution cannot tell apart.
//
// ON ITS OWN THIS CASE CANNOT FAIL ON SQLITE, and that was a finding (DKT-380,
// finding 5): it passed against the old `created_at ASC, id ASC` sort and
// against an untiebroken one alike, because SQLite returns rowid order for an
// unstable sort over a table with no index on the sort key. It is kept as the
// readable statement of the property; the assertion with teeth is
// ListCommentsStaleStampDoesNotReorderTheTrail, which is mutation-verified
// against the sort it is about.
func contractListCommentsSameSecondCommentsKeepInsertionOrder(t *testing.T, st store.Store) {
	issueID := createContractIssue(t, st, "engine trail order")

	want := []string{"implement@0 claimed.", "implement@0 is awaiting review.", "implement@0 completed the issue."}
	insertEngineComments(t, st, issueID, engineCommentNowMS, want...)

	got, err := st.ListComments(issueID)
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

// contractListCommentsStaleStampDoesNotReorderTheTrail is DKT-378.
//
// InsertEngineComment takes the CALLER's nowMS, which is right — the
// transition and its narration must carry one time — but it cost the column
// its monotonicity with insertion. The saga threads one nowMS through a whole
// gate execution and `next` drives every ready action step on a single one, so
// a comment committed LATER can carry an EARLIER stamp than one already on the
// table. Sorting by that stamp put the trail out of the order it was written,
// and the `id` tiebreak could not help: a tiebreak cannot repair a primary key
// that is itself out of order.
func contractListCommentsStaleStampDoesNotReorderTheTrail(t *testing.T, st store.Store) {
	issueID := createContractIssue(t, st, "stale stamp")

	// First written, stamped a minute AHEAD — a gate execution's threaded
	// nowMS, committed before the next transaction's.
	insertEngineComments(t, st, issueID, engineCommentNowMS+60_000, "first written")
	// Second written, stamped from an OLDER clock read.
	insertEngineComments(t, st, issueID, engineCommentNowMS, "second written")

	got, err := st.ListComments(issueID)
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
