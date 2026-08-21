package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// CreateComment inserts a new comment for an issue, records activity, and
// returns its ID. The insert and activity log are wrapped in a single
// transaction so they succeed or fail together.
func CreateComment(db *sql.DB, comment *model.Comment) (int, error) {
	return CreateCommentIdempotent(db, comment, "")
}

// CreateCommentIdempotent is CreateComment with an optional idempotency key.
// A repeat call with the same key returns the original comment id and inserts
// nothing; the key is recorded in the same transaction as the insert.
func CreateCommentIdempotent(db *sql.DB, comment *model.Comment, idempotencyKey string) (int, error) {
	existingID, hit, tx, err := beginIdempotentCreate(db, ScopeIssueComment, idempotencyKey)
	if err != nil {
		return 0, err
	}
	if hit {
		return existingID, nil
	}
	defer tx.Rollback()

	// Verify the issue exists.
	var exists bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?)", comment.IssueID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("checking issue existence: %w", err)
	}
	if !exists {
		return 0, ErrNotFound
	}

	now := time.Now().UTC().Format(time.RFC3339)

	res, err := tx.Exec(
		`INSERT INTO comments (issue_id, body, author, created_at)
		 VALUES (?, ?, ?, ?)`,
		comment.IssueID,
		comment.Body,
		comment.Author,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting comment: %w", err)
	}

	id64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}

	// Touch the issue's updated_at so recently-commented issues surface in sorted lists.
	if _, err := tx.Exec(`UPDATE issues SET updated_at = ? WHERE id = ?`, now, comment.IssueID); err != nil {
		return 0, fmt.Errorf("updating issue timestamp: %w", err)
	}

	if err := RecordActivity(tx, comment.IssueID, "comment_added", "", comment.Body, comment.Author); err != nil {
		return 0, err
	}

	if idempotencyKey != "" {
		if err := RecordIdempotencyKeyTx(tx, ScopeIssueComment, idempotencyKey, int(id64)); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return int(id64), nil
}

// EngineAuthor is the fixed Author value for comments the engine writes
// itself (step claimed, gate/vote opened, step failed, issue completed or
// abandoned), so they read as machine-authored without a schema column to
// mark them.
const EngineAuthor = "docket-engine"

// InsertEngineComment inserts an engine-authored comment against the
// caller's already-open transaction and returns its auto-minted ID. It
// never begins or commits a transaction of its own, so engine code already
// inside a transaction can drop an activity-trail comment without nesting a
// second top-level transaction. The comment's Author is always
// EngineAuthor.
//
// The caller supplies the timestamp as epoch milliseconds rather than the
// writer reading the clock: an engine transaction stamps the issue row, the
// activity log and this comment from ONE `nowMS`, and a second clock read
// here would let the narration of a transition carry a different time than
// the transition itself.
func InsertEngineComment(tx *sql.Tx, issueID int, body string, nowMS int64) (int, error) {
	now := time.UnixMilli(nowMS).UTC().Format(time.RFC3339)

	res, err := tx.Exec(
		`INSERT INTO comments (issue_id, body, author, created_at)
		 VALUES (?, ?, ?, ?)`,
		issueID,
		body,
		EngineAuthor,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting engine comment: %w", err)
	}

	id64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}

	return int(id64), nil
}

// ListComments retrieves all comments for an issue, ordered by the auto-minted
// `id` alone — INSERTION ORDER, which for the activity trail is the only order
// that is always true (DKT-378).
//
// It was `created_at ASC, id ASC`, and the tiebreak was the whole reason: the
// column is RFC3339 at SECOND resolution and the engine writes several trail
// comments per transaction, so same-second rows came back in whatever order
// SQLite chose. But `created_at` stopped being monotonic with insertion when
// InsertEngineComment began taking the CALLER's `nowMS` instead of reading the
// clock (that change is right — see its doc comment — and is not what is being
// undone here). The saga threads ONE `nowMS` through a whole gate execution and
// `next` drives every ready action step on a single one, so two comments
// committed seconds apart can carry the same stamp, or the later one an EARLIER
// stamp. A tiebreak cannot repair a primary key that is itself out of order.
//
// `id` is `INTEGER PRIMARY KEY AUTOINCREMENT`: strictly ascending, never reused
// after a delete. Sorting by it alone makes the trail read in the order it was
// written, which is what a narrative of transitions IS.
//
// Clamping the stamp instead (the way engine/event.go's `monotonicAtMS` does
// for events, against 57s of measured drift) was the other workable remedy and
// was rejected here: a clamped `created_at` is no longer the transition's own
// time, which is precisely the invariant threading `nowMS` exists to hold.
// Events have no monotonic insertion key to fall back on; comments do.
func ListComments(db *sql.DB, issueID int) ([]*model.Comment, error) {
	rows, err := db.Query(
		`SELECT id, issue_id, body, author, created_at
		 FROM comments WHERE issue_id = ? ORDER BY id ASC`, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying comments: %w", err)
	}
	comments, err := scanRows(rows, "comment rows", func(r *sql.Rows) (*model.Comment, error) {
		c, err := scanCommentFrom(r)
		if err != nil {
			return nil, fmt.Errorf("scanning comment row: %w", err)
		}
		return c, nil
	})
	if err != nil {
		return nil, err
	}
	if comments == nil {
		comments = make([]*model.Comment, 0)
	}

	return comments, nil
}

// GetComment retrieves a comment by ID.
func GetComment(db *sql.DB, id int) (*model.Comment, error) {
	row := db.QueryRow(
		`SELECT id, issue_id, body, author, created_at
		 FROM comments WHERE id = ?`, id,
	)

	c, err := scanCommentFrom(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning comment: %w", err)
	}

	return c, nil
}

// ListAllComments returns every comment in the database across all issues,
// ordered by insertion (`id`) for the reason given above ListComments — the
// same table read through a second query, so the two agree on what "in order"
// means.
func ListAllComments(db *sql.DB, projectID int) ([]*model.Comment, error) {
	where, args := projectFilterVia(projectID, `issue_id IN (SELECT id FROM issues WHERE project_id = ?)`)
	rows, err := db.Query(
		`SELECT id, issue_id, body, author, created_at
		 FROM comments `+where+` ORDER BY id ASC`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all comments: %w", err)
	}
	comments, err := scanRows(rows, "comment rows", func(r *sql.Rows) (*model.Comment, error) {
		c, err := scanCommentFrom(r)
		if err != nil {
			return nil, fmt.Errorf("scanning comment row: %w", err)
		}
		return c, nil
	})
	if err != nil {
		return nil, err
	}

	return comments, nil
}

// InsertCommentWithID inserts a comment with a specific ID (not auto-increment),
// skipping if the ID already exists. Returns true if the row was inserted.
// Must be called within an existing transaction.
func InsertCommentWithID(tx *sql.Tx, comment *model.Comment) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO comments (id, issue_id, body, author, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		comment.ID,
		comment.IssueID,
		comment.Body,
		comment.Author,
		comment.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("inserting comment with id %d: %w", comment.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// scanCommentFrom scans a single comment from any scanner (*sql.Row or *sql.Rows).
func scanCommentFrom(s scanner) (*model.Comment, error) {
	var c model.Comment
	var author sql.NullString
	var createdAt string

	err := s.Scan(&c.ID, &c.IssueID, &c.Body, &author, &createdAt)
	if err != nil {
		return nil, err
	}

	c.Author = author.String

	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parsing created_at: %w", err)
	}
	c.CreatedAt = t

	return &c, nil
}
