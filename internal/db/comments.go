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
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
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

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return int(id64), nil
}

// ListComments retrieves all comments for an issue, ordered by creation time ascending.
func ListComments(db *sql.DB, issueID int) ([]*model.Comment, error) {
	rows, err := db.Query(
		`SELECT id, issue_id, body, author, created_at
		 FROM comments WHERE issue_id = ? ORDER BY created_at ASC`, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying comments: %w", err)
	}
	defer rows.Close()

	comments := make([]*model.Comment, 0)
	for rows.Next() {
		c, err := scanCommentFrom(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning comment row: %w", err)
		}
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating comment rows: %w", err)
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
// ordered by created_at ascending.
func ListAllComments(db *sql.DB) ([]*model.Comment, error) {
	rows, err := db.Query(
		`SELECT id, issue_id, body, author, created_at
		 FROM comments ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all comments: %w", err)
	}
	defer rows.Close()

	var comments []*model.Comment
	for rows.Next() {
		c, err := scanCommentFrom(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning comment row: %w", err)
		}
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating comment rows: %w", err)
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
