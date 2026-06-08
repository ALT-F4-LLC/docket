package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// ErrValidation is returned when an input fails a validation precondition that
// the DB layer enforces (e.g., negative revision number). CLI surfaces map this
// to output.ErrValidation. See TDD docket-doc-cli §6.4.
var ErrValidation = errors.New("validation")

// Change-kind enum values for doc_revisions.change_kind. Free-form descriptor
// per TDD §5.1 / §5.4 C8. Combined edits join multiple kinds with "+", e.g.
// "status+body".
const (
	docChangeKindCreate = "create"
	docChangeKindBody   = "body"
	docChangeKindStatus = "status"
	docChangeKindTitle  = "title"
	docChangeKindType   = "type"
)

// docBodyEqual compares two doc bodies for equality after stripping trailing
// newlines. Mirrors the convention used by internal/cli/issue_edit.go:54 so
// that "no-op" edits (whitespace-only at end) do not append a revision.
// See TDD §4.4 / §5.4 C6.
func docBodyEqual(a, b string) bool {
	return strings.TrimRight(a, "\n") == strings.TrimRight(b, "\n")
}

// DocListOptions holds filtering, sorting, and pagination options for ListDocs
// and ListDocsWithCounts. Mirrors ListOptions/issues but with the doc-table
// columns.
type DocListOptions struct {
	Types    []string
	Statuses []string
	Author   string
	Sort     string
	SortDir  string
	Limit    int
	Offset   int
}

// validDocSortFields restricts which columns may appear in ORDER BY.
// WARNING: keys are interpolated directly into SQL.
var validDocSortFields = map[string]bool{
	"id":         true,
	"type":       true,
	"status":     true,
	"title":      true,
	"created_at": true,
	"updated_at": true,
}

// DocSummary is a row from ListDocsWithCounts: a Doc plus the JOIN-derived
// revision count and current revision number. Returned shape per TDD §6.3.
type DocSummary struct {
	Doc             *model.Doc
	RevisionsCount  int
	CurrentRevision int
}

// CreateDoc inserts a new doc and appends revision #1 with change_kind="create"
// in a single transaction. Returns the new doc ID. The supplied doc must have
// Type, Status, Title, Body, and Author set; CreatedAt/UpdatedAt are
// stamped by this function.
func CreateDoc(db *sql.DB, doc *model.Doc) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)

	res, err := tx.Exec(
		`INSERT INTO docs (type, status, title, body, author, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		doc.Type, doc.Status, doc.Title, doc.Body, doc.Author, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting doc: %w", err)
	}

	id64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}
	id := int(id64)

	if _, err := appendDocRevision(tx, id, doc.Body, docChangeKindCreate, doc.Author, now); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	doc.ID = id
	// Reflect the values the DB now holds so callers don't see stale zero times.
	createdAt, _ := time.Parse(time.RFC3339, now)
	doc.CreatedAt = createdAt
	doc.UpdatedAt = createdAt

	return id, nil
}

// GetDoc returns the doc with the given ID, or ErrNotFound.
func GetDoc(db *sql.DB, id int) (*model.Doc, error) {
	row := db.QueryRow(
		`SELECT id, type, status, title, body, author, created_at, updated_at
		 FROM docs WHERE id = ?`, id,
	)
	d, err := scanDocFrom(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning doc: %w", err)
	}
	return d, nil
}

// ListDocs returns docs matching opts, ordered and paginated. Returns the
// matching rows and the total count before limit/offset.
func ListDocs(db *sql.DB, opts DocListOptions) ([]*model.Doc, int, error) {
	whereSQL, args, err := buildDocWhere(opts, "")
	if err != nil {
		return nil, 0, err
	}

	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM docs "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting docs: %w", err)
	}

	orderSQL, err := buildDocOrder(opts, "")
	if err != nil {
		return nil, 0, err
	}

	query := "SELECT id, type, status, title, body, author, created_at, updated_at FROM docs " +
		whereSQL + " " + orderSQL
	queryArgs := append([]any{}, args...)
	if opts.Limit > 0 {
		query += " LIMIT ?"
		queryArgs = append(queryArgs, opts.Limit)
		if opts.Offset > 0 {
			query += " OFFSET ?"
			queryArgs = append(queryArgs, opts.Offset)
		}
	} else if opts.Offset > 0 {
		query += " LIMIT -1 OFFSET ?"
		queryArgs = append(queryArgs, opts.Offset)
	}

	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing docs: %w", err)
	}
	defer rows.Close()

	var docs []*model.Doc
	for rows.Next() {
		d, err := scanDocFrom(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scanning doc row: %w", err)
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating doc rows: %w", err)
	}

	return docs, total, nil
}

// listDocsWithCountsBaseSQL is the single-query body used by ListDocsWithCounts
// and by tests that EXPLAIN it to verify the JOIN strategy (TDD §5.4 S1, hard
// constraint — must remain a single statement with no per-row sub-selects).
// Filters / ORDER / LIMIT are appended at call time.
const listDocsWithCountsBaseSQL = `SELECT d.id, d.type, d.status, d.title, d.body, d.author, d.created_at, d.updated_at,
       COALESCE(COUNT(r.id), 0) AS revisions_count,
       COALESCE(MAX(r.revision_number), 0) AS current_revision
FROM docs d
LEFT JOIN doc_revisions r ON r.doc_id = d.id`

// ListDocsWithCounts returns DocSummary rows including JOIN-derived
// revisions_count and current_revision in a single query (TDD §5.4 S1 — no
// N+1). Returns total count (before limit) as the second value.
func ListDocsWithCounts(db *sql.DB, opts DocListOptions) ([]*DocSummary, int, error) {
	whereSQL, args, err := buildDocWhere(opts, "d.")
	if err != nil {
		return nil, 0, err
	}

	countWhere, countArgs, err := buildDocWhere(opts, "")
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM docs "+countWhere, countArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting docs: %w", err)
	}

	orderSQL, err := buildDocOrder(opts, "d.")
	if err != nil {
		return nil, 0, err
	}

	query := listDocsWithCountsBaseSQL
	if whereSQL != "" {
		query += " " + whereSQL
	}
	query += " GROUP BY d.id " + orderSQL
	queryArgs := append([]any{}, args...)
	if opts.Limit > 0 {
		query += " LIMIT ?"
		queryArgs = append(queryArgs, opts.Limit)
		if opts.Offset > 0 {
			query += " OFFSET ?"
			queryArgs = append(queryArgs, opts.Offset)
		}
	} else if opts.Offset > 0 {
		query += " LIMIT -1 OFFSET ?"
		queryArgs = append(queryArgs, opts.Offset)
	}

	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing docs with counts: %w", err)
	}
	defer rows.Close()

	var out []*DocSummary
	for rows.Next() {
		var (
			d              model.Doc
			author         sql.NullString
			createdAt, upd string
			revsCount, cur int
		)
		if err := rows.Scan(
			&d.ID, &d.Type, &d.Status, &d.Title, &d.Body, &author,
			&createdAt, &upd, &revsCount, &cur,
		); err != nil {
			return nil, 0, fmt.Errorf("scanning doc summary: %w", err)
		}
		d.Author = author.String
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			d.CreatedAt = t
		}
		if t, err := time.Parse(time.RFC3339, upd); err == nil {
			d.UpdatedAt = t
		}
		out = append(out, &DocSummary{
			Doc:             &d,
			RevisionsCount:  revsCount,
			CurrentRevision: cur,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating doc summary rows: %w", err)
	}

	return out, total, nil
}

// DocUpdate is the set of fields UpdateDoc may change. Nil pointers mean
// "leave unchanged"; non-nil with the current value is detected as a no-op
// (body equality additionally applies trailing-newline trimming — C6).
type DocUpdate struct {
	Title  *string
	Body   *string
	Status *string
	Type   *string
	Author string
}

// UpdateDoc applies the changes in upd to the doc with the given ID and
// appends one revision row capturing the combined change_kind. Returns the
// new revision number (0 if no revision appended because nothing changed).
//
// Per TDD §5.4 C8, every persisted field change appends one revision; the
// change_kind is comma-joined ("+" separator) for multi-field edits. Body
// equality uses strings.TrimRight(s, "\n") so trailing-newline-only edits
// are no-ops and do NOT append a revision (C6).
//
// Returns ErrNotFound if id does not exist.
func UpdateDoc(db *sql.DB, id int, upd DocUpdate) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	current, err := getDocTx(tx, id)
	if err != nil {
		return 0, err
	}

	var setClauses []string
	var args []any
	var kinds []string

	newBody := current.Body

	if upd.Title != nil && *upd.Title != current.Title {
		setClauses = append(setClauses, "title = ?")
		args = append(args, *upd.Title)
		kinds = append(kinds, docChangeKindTitle)
	}
	if upd.Type != nil && *upd.Type != current.Type {
		setClauses = append(setClauses, "type = ?")
		args = append(args, *upd.Type)
		kinds = append(kinds, docChangeKindType)
	}
	if upd.Status != nil && *upd.Status != current.Status {
		setClauses = append(setClauses, "status = ?")
		args = append(args, *upd.Status)
		kinds = append(kinds, docChangeKindStatus)
	}
	if upd.Body != nil && !docBodyEqual(*upd.Body, current.Body) {
		setClauses = append(setClauses, "body = ?")
		args = append(args, *upd.Body)
		kinds = append(kinds, docChangeKindBody)
		newBody = *upd.Body
	}

	if len(kinds) == 0 {
		return 0, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, now)
	args = append(args, id)

	query := fmt.Sprintf("UPDATE docs SET %s WHERE id = ?", strings.Join(setClauses, ", "))
	if _, err := tx.Exec(query, args...); err != nil {
		return 0, fmt.Errorf("updating doc: %w", err)
	}

	author := upd.Author
	if author == "" {
		author = current.Author
	}

	rev, err := appendDocRevision(tx, id, newBody, strings.Join(kinds, "+"), author, now)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return rev, nil
}

// GetDocRevision returns revision rev of the doc with the given ID. Per TDD
// §3.3 / §6.3 (Q1): rev < 0 → ErrValidation; rev > MAX → ErrNotFound. rev == 0
// is treated as "the current revision" (the most-recent one).
func GetDocRevision(db *sql.DB, docID, rev int) (*model.DocRevision, error) {
	if rev < 0 {
		return nil, fmt.Errorf("%w: revision number must be >= 0, got %d", ErrValidation, rev)
	}

	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM docs WHERE id = ?)", docID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("checking doc existence: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}

	if rev == 0 {
		row := db.QueryRow(
			`SELECT id, doc_id, revision_number, body, change_kind, author, created_at
			 FROM doc_revisions WHERE doc_id = ? ORDER BY revision_number DESC LIMIT 1`,
			docID,
		)
		r, err := scanDocRevisionFrom(row)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("scanning doc revision: %w", err)
		}
		return r, nil
	}

	row := db.QueryRow(
		`SELECT id, doc_id, revision_number, body, change_kind, author, created_at
		 FROM doc_revisions WHERE doc_id = ? AND revision_number = ?`,
		docID, rev,
	)
	r, err := scanDocRevisionFrom(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning doc revision: %w", err)
	}
	return r, nil
}

// ListDocRevisions returns every revision row for the doc, ordered by
// revision_number ascending. Returns ErrNotFound when the doc itself is
// missing.
func ListDocRevisions(db *sql.DB, docID int) ([]*model.DocRevision, error) {
	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM docs WHERE id = ?)", docID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("checking doc existence: %w", err)
	}
	if !exists {
		return nil, ErrNotFound
	}

	rows, err := db.Query(
		`SELECT id, doc_id, revision_number, body, change_kind, author, created_at
		 FROM doc_revisions WHERE doc_id = ? ORDER BY revision_number ASC`,
		docID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying doc revisions: %w", err)
	}
	defer rows.Close()

	var out []*model.DocRevision
	for rows.Next() {
		r, err := scanDocRevisionFrom(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning doc revision row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating doc revision rows: %w", err)
	}
	return out, nil
}

// DeleteDoc removes the doc with the given ID. When cascade is true, FK
// cascades drop doc_revisions, doc_comments, doc_issue_links, proposal_docs.
// When cascade is false, the call returns ErrConflict if any links (issue or
// proposal) exist for the doc — comments and revisions are part of the doc's
// own history and never block deletion.
// Returns ErrNotFound if no doc with that ID exists.
func DeleteDoc(db *sql.DB, id int, cascade bool) error {
	if !cascade {
		var existing int
		err := db.QueryRow(
			`SELECT
			   (SELECT COUNT(*) FROM doc_issue_links WHERE doc_id = ?) +
			   (SELECT COUNT(*) FROM proposal_docs   WHERE doc_id = ?)`,
			id, id,
		).Scan(&existing)
		if err != nil {
			return fmt.Errorf("checking doc links: %w", err)
		}
		if existing > 0 {
			return fmt.Errorf("%w: doc %s has %d link(s); use --cascade to remove",
				ErrConflict, model.FormatDocID(id), existing)
		}
	}

	res, err := db.Exec("DELETE FROM docs WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("deleting doc: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// InsertDocWithID inserts a doc row with a caller-supplied ID, skipping if the
// ID already exists. Mirrors InsertIssueWithID (TDD §5.3 round-trip helpers).
// Must be called within an existing transaction. Returns true if inserted.
func InsertDocWithID(tx *sql.Tx, doc *model.Doc) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO docs (id, type, status, title, body, author, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.ID, doc.Type, doc.Status, doc.Title, doc.Body, doc.Author,
		doc.CreatedAt.UTC().Format(time.RFC3339),
		doc.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("inserting doc with id %d: %w", doc.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertDocRevisionWithID inserts a doc_revisions row with a caller-supplied
// ID, skipping if the ID already exists. Must be called within a transaction.
// Returns true if inserted. Mirrors InsertIssueWithID.
func InsertDocRevisionWithID(tx *sql.Tx, r *model.DocRevision) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO doc_revisions (id, doc_id, revision_number, body, change_kind, author, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.DocID, r.RevisionNumber, r.Body, r.ChangeKind, r.Author,
		r.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("inserting doc revision with id %d: %w", r.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListAllDocs returns every doc row ordered by id ASC, for a full export.
func ListAllDocs(db *sql.DB) ([]*model.Doc, error) {
	rows, err := db.Query(
		`SELECT id, type, status, title, body, author, created_at, updated_at
		 FROM docs ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all docs: %w", err)
	}
	defer rows.Close()

	var docs []*model.Doc
	for rows.Next() {
		d, err := scanDocFrom(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning doc row: %w", err)
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating doc rows: %w", err)
	}
	return docs, nil
}

// ListAllDocRevisions returns every doc_revisions row ordered by id ASC, for a
// full export.
func ListAllDocRevisions(db *sql.DB) ([]*model.DocRevision, error) {
	rows, err := db.Query(
		`SELECT id, doc_id, revision_number, body, change_kind, author, created_at
		 FROM doc_revisions ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all doc revisions: %w", err)
	}
	defer rows.Close()

	var revs []*model.DocRevision
	for rows.Next() {
		r, err := scanDocRevisionFrom(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning doc revision row: %w", err)
		}
		revs = append(revs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating doc revision rows: %w", err)
	}
	return revs, nil
}

// --- private helpers ---

// appendDocRevision inserts the next revision row for docID inside the supplied
// transaction. Revision numbers are monotonic per doc, computed as MAX(rev)+1
// inside the same transaction; UNIQUE(doc_id, revision_number) defends against
// external concurrent writers (the app pins MaxOpenConns=1, so this can only
// race with a sidecar process). Returns the assigned revision_number.
func appendDocRevision(tx *sql.Tx, docID int, body, changeKind, author, createdAt string) (int, error) {
	var maxRev sql.NullInt64
	if err := tx.QueryRow(
		`SELECT MAX(revision_number) FROM doc_revisions WHERE doc_id = ?`, docID,
	).Scan(&maxRev); err != nil {
		return 0, fmt.Errorf("computing next revision_number: %w", err)
	}
	next := 1
	if maxRev.Valid {
		next = int(maxRev.Int64) + 1
	}

	_, err := tx.Exec(
		`INSERT INTO doc_revisions (doc_id, revision_number, body, change_kind, author, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		docID, next, body, changeKind, author, createdAt,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return 0, ErrConflict
		}
		return 0, fmt.Errorf("inserting doc revision: %w", err)
	}
	return next, nil
}

// getDocTx loads a doc within a transaction; ErrNotFound when absent.
func getDocTx(tx *sql.Tx, id int) (*model.Doc, error) {
	row := tx.QueryRow(
		`SELECT id, type, status, title, body, author, created_at, updated_at
		 FROM docs WHERE id = ?`, id,
	)
	d, err := scanDocFrom(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning doc: %w", err)
	}
	return d, nil
}

// scanDocFrom scans a single doc from any scanner. Author is stored as
// sql.NullString and projected to a plain string per S5 (mirrors comments.go).
func scanDocFrom(s scanner) (*model.Doc, error) {
	var d model.Doc
	var author sql.NullString
	var createdAt, updatedAt string

	if err := s.Scan(&d.ID, &d.Type, &d.Status, &d.Title, &d.Body, &author, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	d.Author = author.String

	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parsing created_at: %w", err)
	}
	d.CreatedAt = t

	t, err = time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing updated_at: %w", err)
	}
	d.UpdatedAt = t

	return &d, nil
}

// scanDocRevisionFrom scans a single doc_revisions row from any scanner.
func scanDocRevisionFrom(s scanner) (*model.DocRevision, error) {
	var r model.DocRevision
	var author sql.NullString
	var createdAt string

	if err := s.Scan(&r.ID, &r.DocID, &r.RevisionNumber, &r.Body, &r.ChangeKind, &author, &createdAt); err != nil {
		return nil, err
	}

	r.Author = author.String

	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parsing created_at: %w", err)
	}
	r.CreatedAt = t

	return &r, nil
}

// buildDocWhere constructs the WHERE clause + args for a DocListOptions
// filter. tablePrefix (e.g. "d.") is prepended to column references so the
// same builder can serve aliased JOIN queries and bare single-table queries.
func buildDocWhere(opts DocListOptions, tablePrefix string) (string, []any, error) {
	var clauses []string
	var args []any

	if len(opts.Types) > 0 {
		placeholders := makePlaceholders(len(opts.Types))
		clauses = append(clauses, fmt.Sprintf("%stype IN (%s)", tablePrefix, placeholders))
		for _, t := range opts.Types {
			args = append(args, t)
		}
	}
	if len(opts.Statuses) > 0 {
		placeholders := makePlaceholders(len(opts.Statuses))
		clauses = append(clauses, fmt.Sprintf("%sstatus IN (%s)", tablePrefix, placeholders))
		for _, s := range opts.Statuses {
			args = append(args, s)
		}
	}
	if opts.Author != "" {
		clauses = append(clauses, tablePrefix+"author = ?")
		args = append(args, opts.Author)
	}

	if len(clauses) == 0 {
		return "", nil, nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args, nil
}

// buildDocOrder constructs the ORDER BY clause from a DocListOptions. tablePrefix
// (e.g. "d.") is prepended to column names when the query uses table aliases.
func buildDocOrder(opts DocListOptions, tablePrefix string) (string, error) {
	field := opts.Sort
	if field == "" {
		field = "created_at"
	}
	if !validDocSortFields[field] {
		return "", fmt.Errorf("%w: invalid sort field %q", ErrValidation, field)
	}
	// Defense-in-depth: reject any sort field that doesn't look like a plain
	// column name, even if it passed the allowlist check above.
	if !safeIdentifier.MatchString(field) {
		return "", fmt.Errorf("%w: invalid sort field %q", ErrValidation, field)
	}

	dir := strings.ToUpper(opts.SortDir)
	if dir == "" {
		dir = "DESC"
	}
	if dir != "ASC" && dir != "DESC" {
		return "", fmt.Errorf("%w: invalid sort dir %q", ErrValidation, opts.SortDir)
	}

	return fmt.Sprintf("ORDER BY %s%s %s", tablePrefix, field, dir), nil
}
