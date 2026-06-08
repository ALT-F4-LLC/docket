package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// CreateDocComment inserts a comment on a doc and returns its ID. The doc
// existence check and insert run in a single transaction. Returns
// ErrNotFound if the doc does not exist.
func CreateDocComment(db *sql.DB, c *model.DocComment) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM docs WHERE id = ?)", c.DocID).Scan(&exists); err != nil {
		return 0, fmt.Errorf("checking doc existence: %w", err)
	}
	if !exists {
		return 0, ErrNotFound
	}

	now := time.Now().UTC().Format(time.RFC3339)

	res, err := tx.Exec(
		`INSERT INTO doc_comments (doc_id, body, author, created_at)
		 VALUES (?, ?, ?, ?)`,
		c.DocID, c.Body, c.Author, now,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting doc comment: %w", err)
	}

	if _, err := tx.Exec(`UPDATE docs SET updated_at = ? WHERE id = ?`, now, c.DocID); err != nil {
		return 0, fmt.Errorf("touching doc updated_at: %w", err)
	}

	id64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return int(id64), nil
}

// ListDocComments returns all comments for a doc ordered by created_at ASC.
// Returns an empty slice (not nil) when the doc has no comments; returns
// ErrNotFound when the doc itself is missing.
func ListDocComments(db *sql.DB, docID int) ([]*model.DocComment, error) {
	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM docs WHERE id = ?)", docID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("checking doc existence: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}

	rows, err := db.Query(
		`SELECT id, doc_id, body, author, created_at
		 FROM doc_comments WHERE doc_id = ? ORDER BY created_at ASC`, docID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying doc comments: %w", err)
	}
	defer rows.Close()

	out := make([]*model.DocComment, 0)
	for rows.Next() {
		c, err := scanDocCommentFrom(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning doc comment row: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating doc comment rows: %w", err)
	}
	return out, nil
}

// GetDocComment returns a single doc comment by ID, or ErrNotFound.
func GetDocComment(db *sql.DB, id int) (*model.DocComment, error) {
	row := db.QueryRow(
		`SELECT id, doc_id, body, author, created_at
		 FROM doc_comments WHERE id = ?`, id,
	)
	c, err := scanDocCommentFrom(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning doc comment: %w", err)
	}
	return c, nil
}

// InsertDocCommentWithID inserts a doc_comments row with a caller-supplied ID,
// skipping if the ID already exists. Returns true if inserted. Must be called
// within an existing transaction. Mirrors InsertCommentWithID.
func InsertDocCommentWithID(tx *sql.Tx, c *model.DocComment) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO doc_comments (id, doc_id, body, author, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.DocID, c.Body, c.Author,
		c.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("inserting doc comment with id %d: %w", c.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListAllDocComments returns every doc_comments row ordered by id ASC, for a
// full export.
func ListAllDocComments(db *sql.DB) ([]*model.DocComment, error) {
	rows, err := db.Query(
		`SELECT id, doc_id, body, author, created_at
		 FROM doc_comments ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all doc comments: %w", err)
	}
	defer rows.Close()

	var comments []*model.DocComment
	for rows.Next() {
		c, err := scanDocCommentFrom(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning doc comment row: %w", err)
		}
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating doc comment rows: %w", err)
	}
	return comments, nil
}

// scanDocCommentFrom scans a single doc_comments row from any scanner. Author
// is stored as sql.NullString and projected to a plain string per S5.
func scanDocCommentFrom(s scanner) (*model.DocComment, error) {
	var c model.DocComment
	var author sql.NullString
	var createdAt string

	if err := s.Scan(&c.ID, &c.DocID, &c.Body, &author, &createdAt); err != nil {
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
