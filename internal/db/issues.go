package db

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// safeIdentifier matches valid SQL column identifiers (lowercase letters and underscores only).
var safeIdentifier = regexp.MustCompile(`^[a-z_]+$`)

// ErrNotFound is returned when a requested issue does not exist.
var ErrNotFound = errors.New("issue not found")

// scanner abstracts *sql.Row and *sql.Rows for scanning a single row.
type scanner interface {
	Scan(dest ...any) error
}

// ListOptions holds filtering, sorting, and pagination options for ListIssues.
type ListOptions struct {
	Statuses    []string // filter by status (multiple = OR)
	Priorities  []string // filter by priority (multiple = OR)
	Labels      []string // filter by label name (multiple = AND)
	Types       []string // filter by kind (multiple = OR)
	Assignee    string   // filter by assignee
	ParentID    *int     // filter by parent issue ID
	RootsOnly   bool     // only issues with no parent
	IncludeDone bool     // include done status (default: exclude)
	Sort        string   // field name
	SortDir     string   // "asc" or "desc"
	Limit       int      // max results
	Offset      int      // for pagination
}

// validSortFields is the set of columns allowed for sorting.
// WARNING: These keys are interpolated directly into SQL ORDER BY clauses.
// Only add single-word column names that exactly match the issues table schema.
var validSortFields = map[string]bool{
	"id":         true,
	"title":      true,
	"status":     true,
	"priority":   true,
	"kind":       true,
	"assignee":   true,
	"created_at": true,
	"updated_at": true,
}

// validUpdateFields is the set of columns allowed in UpdateIssue.
var validUpdateFields = map[string]bool{
	"title":       true,
	"description": true,
	"status":      true,
	"priority":    true,
	"kind":        true,
	"assignee":    true,
	"parent_id":   true,
}

// CreateIssue inserts a new issue and returns its ID. Labels are created
// (find-or-create) and linked to the issue within the same transaction.
func CreateIssue(db *sql.DB, issue *model.Issue, labels []string) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO issues (parent_id, title, description, status, priority, kind, assignee, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nilIfZeroPtr(issue.ParentID),
		issue.Title,
		issue.Description,
		string(issue.Status),
		string(issue.Priority),
		string(issue.Kind),
		issue.Assignee,
		now,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting issue: %w", err)
	}

	id64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}
	id := int(id64)

	// Attach labels.
	for _, name := range labels {
		labelID, err := findOrCreateLabel(tx, name)
		if err != nil {
			return 0, fmt.Errorf("processing label %q: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO issue_labels (issue_id, label_id) VALUES (?, ?)`,
			id, labelID,
		); err != nil {
			return 0, fmt.Errorf("linking label %q: %w", name, err)
		}
	}

	// Record creation activity.
	if err := RecordActivity(tx, id, "created", "", "", ""); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return id, nil
}

// GetIssue retrieves an issue by ID.
func GetIssue(db *sql.DB, id int) (*model.Issue, error) {
	row := db.QueryRow(
		`SELECT id, parent_id, title, description, status, priority, kind, assignee, created_at, updated_at
		 FROM issues WHERE id = ?`, id,
	)
	return scanIssue(row)
}

// ListIssues retrieves issues matching the given filters. It returns the
// matching issues, the total count of matching rows (ignoring Limit/Offset),
// and an error.
func ListIssues(db *sql.DB, opts ListOptions) ([]*model.Issue, int, error) {
	var (
		whereClauses []string
		args         []interface{}
		joinClause   string
	)

	// Auto-include done if the status filter explicitly requests it.
	if !opts.IncludeDone {
		for _, s := range opts.Statuses {
			if s == string(model.StatusDone) {
				opts.IncludeDone = true
				break
			}
		}
	}

	// Exclude "done" by default.
	if !opts.IncludeDone {
		whereClauses = append(whereClauses, "i.status != 'done'")
	}

	if len(opts.Statuses) > 0 {
		placeholders := makePlaceholders(len(opts.Statuses))
		whereClauses = append(whereClauses, fmt.Sprintf("i.status IN (%s)", placeholders))
		for _, s := range opts.Statuses {
			args = append(args, s)
		}
	}

	if len(opts.Priorities) > 0 {
		placeholders := makePlaceholders(len(opts.Priorities))
		whereClauses = append(whereClauses, fmt.Sprintf("i.priority IN (%s)", placeholders))
		for _, p := range opts.Priorities {
			args = append(args, p)
		}
	}

	if len(opts.Types) > 0 {
		placeholders := makePlaceholders(len(opts.Types))
		whereClauses = append(whereClauses, fmt.Sprintf("i.kind IN (%s)", placeholders))
		for _, t := range opts.Types {
			args = append(args, t)
		}
	}

	if opts.Assignee != "" {
		whereClauses = append(whereClauses, "i.assignee = ?")
		args = append(args, opts.Assignee)
	}

	if opts.ParentID != nil {
		whereClauses = append(whereClauses, "i.parent_id = ?")
		args = append(args, *opts.ParentID)
	}

	if opts.RootsOnly {
		whereClauses = append(whereClauses, "i.parent_id IS NULL")
	}

	// Labels filter: AND logic — issue must have ALL specified labels.
	if len(opts.Labels) > 0 {
		joinClause = `JOIN issue_labels il ON il.issue_id = i.id
		              JOIN labels l ON l.id = il.label_id`
		placeholders := makePlaceholders(len(opts.Labels))
		whereClauses = append(whereClauses, fmt.Sprintf("l.name IN (%s)", placeholders))
		for _, l := range opts.Labels {
			args = append(args, l)
		}
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// For AND label logic, we need HAVING COUNT = number of labels.
	havingSQL := ""
	groupBySQL := ""
	if len(opts.Labels) > 0 {
		groupBySQL = "GROUP BY i.id"
		havingSQL = fmt.Sprintf("HAVING COUNT(DISTINCT l.name) = %d", len(opts.Labels))
	}

	// Count query (total matching rows for pagination).
	countQuery := fmt.Sprintf(
		`SELECT COUNT(*) FROM (SELECT i.id FROM issues i %s %s %s %s)`,
		joinClause, whereSQL, groupBySQL, havingSQL,
	)
	var totalCount int
	if err := db.QueryRow(countQuery, args...).Scan(&totalCount); err != nil {
		return nil, 0, fmt.Errorf("counting issues: %w", err)
	}

	// Determine sort.
	sortField := "created_at"
	if opts.Sort != "" && validSortFields[opts.Sort] {
		sortField = opts.Sort
	}
	// Defense-in-depth: reject any sort field that doesn't look like a plain column name,
	// even if it passed the allowlist check above.
	if !safeIdentifier.MatchString(sortField) {
		return nil, 0, fmt.Errorf("invalid sort field %q", sortField)
	}
	sortDir := "DESC"
	if strings.EqualFold(opts.SortDir, "asc") {
		sortDir = "ASC"
	}

	// Main query.
	// Safe: sortField validated against validSortFields and safeIdentifier; sortDir is "ASC" or "DESC".
	mainQuery := fmt.Sprintf(
		`SELECT i.id, i.parent_id, i.title, i.description, i.status, i.priority, i.kind, i.assignee, i.created_at, i.updated_at
		 FROM issues i %s %s %s %s ORDER BY i.%s %s`,
		joinClause, whereSQL, groupBySQL, havingSQL, sortField, sortDir,
	)

	mainArgs := make([]interface{}, len(args))
	copy(mainArgs, args)

	if opts.Limit > 0 {
		mainQuery += " LIMIT ?"
		mainArgs = append(mainArgs, opts.Limit)
	}
	if opts.Offset > 0 {
		mainQuery += " OFFSET ?"
		mainArgs = append(mainArgs, opts.Offset)
	}

	rows, err := db.Query(mainQuery, mainArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying issues: %w", err)
	}
	defer rows.Close()

	issues := make([]*model.Issue, 0)
	for rows.Next() {
		issue, err := scanIssueRow(rows)
		if err != nil {
			return nil, 0, err
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating issue rows: %w", err)
	}

	// Hydrate labels for all returned issues to avoid N+1 queries in callers.
	if err := HydrateLabels(db, issues); err != nil {
		return nil, 0, fmt.Errorf("hydrating labels: %w", err)
	}

	return issues, totalCount, nil
}

// UpdateIssue updates an existing issue. Only keys present in the updates map
// are modified. The updated_at timestamp is always set to the current time.
// Activity is recorded for each changed field within the same transaction.
//
// Field names are validated against validUpdateFields, but callers are responsible
// for validating field values (e.g. ensuring status/priority/kind are valid enums)
// before calling this function.
func UpdateIssue(db *sql.DB, id int, updates map[string]interface{}, changedBy string) error {
	if len(updates) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Fetch old values for activity logging.
	oldIssue, err := getIssueTx(tx, id)
	if err != nil {
		return err
	}

	var setClauses []string
	var args []interface{}

	// Sort keys for deterministic query generation.
	fields := make([]string, 0, len(updates))
	for field := range updates {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	for _, field := range fields {
		if !validUpdateFields[field] {
			return fmt.Errorf("invalid update field %q", field)
		}
		setClauses = append(setClauses, field+" = ?")
		args = append(args, updates[field])
	}

	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	args = append(args, id)

	query := fmt.Sprintf(
		"UPDATE issues SET %s WHERE id = ?",
		strings.Join(setClauses, ", "),
	)

	res, err := tx.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("updating issue: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}

	// Record activity for each changed field.
	for _, field := range fields {
		oldVal := getFieldValue(oldIssue, field)
		newVal := fmt.Sprintf("%v", updates[field])
		if oldVal != newVal {
			if err := RecordActivity(tx, id, field, oldVal, newVal, changedBy); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// getIssueTx retrieves an issue by ID within a transaction.
func getIssueTx(tx *sql.Tx, id int) (*model.Issue, error) {
	row := tx.QueryRow(
		`SELECT id, parent_id, title, description, status, priority, kind, assignee, created_at, updated_at
		 FROM issues WHERE id = ?`, id,
	)
	issue, err := scanIssueFrom(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning issue: %w", err)
	}
	return issue, nil
}

// getFieldValue extracts a string representation of a field from an issue for activity logging.
func getFieldValue(issue *model.Issue, field string) string {
	switch field {
	case "title":
		return issue.Title
	case "description":
		return issue.Description
	case "status":
		return string(issue.Status)
	case "priority":
		return string(issue.Priority)
	case "kind":
		return string(issue.Kind)
	case "assignee":
		return issue.Assignee
	case "parent_id":
		if issue.ParentID != nil {
			return fmt.Sprintf("%d", *issue.ParentID)
		}
		return ""
	default:
		return ""
	}
}

// DeleteIssue removes an issue by ID. Foreign key cascades handle cleanup of
// related rows (comments, labels, activity, relations).
func DeleteIssue(db *sql.DB, id int) error {
	res, err := db.Exec("DELETE FROM issues WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("deleting issue: %w", err)
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

// GetSubIssues returns all direct children of an issue.
func GetSubIssues(db *sql.DB, parentID int) ([]*model.Issue, error) {
	rows, err := db.Query(
		`SELECT id, parent_id, title, description, status, priority, kind, assignee, created_at, updated_at
		 FROM issues WHERE parent_id = ? ORDER BY created_at ASC`, parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying sub-issues: %w", err)
	}
	defer rows.Close()

	var issues []*model.Issue
	for rows.Next() {
		issue, err := scanIssueRow(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// GetSubIssueTree returns the full recursive tree of all descendants under an issue.
func GetSubIssueTree(db *sql.DB, parentID int) ([]*model.Issue, error) {
	rows, err := db.Query(
		`WITH RECURSIVE tree(id) AS (
			SELECT id FROM issues WHERE parent_id = ?
			UNION ALL
			SELECT i.id FROM issues i JOIN tree t ON i.parent_id = t.id
		)
		SELECT i.id, i.parent_id, i.title, i.description, i.status, i.priority, i.kind, i.assignee, i.created_at, i.updated_at
		FROM issues i JOIN tree t ON i.id = t.id
		ORDER BY i.created_at ASC`, parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying sub-issue tree: %w", err)
	}
	defer rows.Close()

	var issues []*model.Issue
	for rows.Next() {
		issue, err := scanIssueRow(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// GetSubIssueProgress returns (done, total) counts for all descendants of an issue.
func GetSubIssueProgress(db *sql.DB, parentID int) (int, int, error) {
	var done, total int
	err := db.QueryRow(
		`WITH RECURSIVE tree(id) AS (
			SELECT id FROM issues WHERE parent_id = ?
			UNION ALL
			SELECT i.id FROM issues i JOIN tree t ON i.parent_id = t.id
		)
		SELECT
			COALESCE(SUM(CASE WHEN i.status = 'done' THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM issues i JOIN tree t ON i.id = t.id`, parentID,
	).Scan(&done, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("querying sub-issue progress: %w", err)
	}
	return done, total, nil
}

// --- helpers ---

// scanIssueFrom scans a single issue from any scanner (*sql.Row or *sql.Rows).
func scanIssueFrom(s scanner) (*model.Issue, error) {
	var i model.Issue
	var parentID sql.NullInt64
	var description, assignee sql.NullString
	var createdAt, updatedAt string

	err := s.Scan(
		&i.ID, &parentID, &i.Title, &description,
		&i.Status, &i.Priority, &i.Kind, &assignee,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	if parentID.Valid {
		pid := int(parentID.Int64)
		i.ParentID = &pid
	}
	i.Description = description.String
	i.Assignee = assignee.String

	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parsing created_at: %w", err)
	}
	i.CreatedAt = t

	t, err = time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing updated_at: %w", err)
	}
	i.UpdatedAt = t

	return &i, nil
}

// scanIssue scans a single issue from a *sql.Row, returning ErrNotFound
// for sql.ErrNoRows so callers can distinguish "not found" from other errors.
func scanIssue(row *sql.Row) (*model.Issue, error) {
	issue, err := scanIssueFrom(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scanning issue: %w", err)
	}
	return issue, nil
}

// scanIssueRow scans a single issue from a *sql.Rows cursor.
func scanIssueRow(rows *sql.Rows) (*model.Issue, error) {
	issue, err := scanIssueFrom(rows)
	if err != nil {
		return nil, fmt.Errorf("scanning issue row: %w", err)
	}
	return issue, nil
}

// findOrCreateLabel looks up a label by name, creating it if it doesn't exist,
// and returns the label ID.
func findOrCreateLabel(tx *sql.Tx, name string) (int, error) {
	var id int
	err := tx.QueryRow("SELECT id FROM labels WHERE name = ?", name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("querying label: %w", err)
	}

	res, err := tx.Exec("INSERT INTO labels (name) VALUES (?)", name)
	if err != nil {
		return 0, fmt.Errorf("inserting label: %w", err)
	}
	id64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting label id: %w", err)
	}
	return int(id64), nil
}

// nilIfZeroPtr returns nil if p is nil, otherwise returns *p (for sql parameter binding).
func nilIfZeroPtr(p *int) interface{} {
	if p == nil {
		return nil
	}
	return *p
}

// makePlaceholders returns "?, ?, ..." with n placeholders.
func makePlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}

// GetIssueLabels returns the label names attached to an issue, sorted alphabetically.
func GetIssueLabels(db *sql.DB, issueID int) ([]string, error) {
	rows, err := db.Query(
		`SELECT l.name FROM issue_labels il
		 JOIN labels l ON l.id = il.label_id
		 WHERE il.issue_id = ?
		 ORDER BY l.name`, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying labels: %w", err)
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning label: %w", err)
		}
		labels = append(labels, name)
	}
	return labels, rows.Err()
}

// HydrateLabels bulk-loads labels for a set of issues, populating each issue's
// Labels field. This avoids N+1 queries when displaying lists.
func HydrateLabels(db *sql.DB, issues []*model.Issue) error {
	if len(issues) == 0 {
		return nil
	}

	ids := make([]interface{}, len(issues))
	issueMap := make(map[int]*model.Issue, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
		issueMap[issue.ID] = issue
	}

	placeholders := makePlaceholders(len(ids))
	query := fmt.Sprintf(
		`SELECT il.issue_id, l.name FROM issue_labels il
		 JOIN labels l ON l.id = il.label_id
		 WHERE il.issue_id IN (%s)
		 ORDER BY l.name`, placeholders,
	)

	rows, err := db.Query(query, ids...)
	if err != nil {
		return fmt.Errorf("querying labels: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var issueID int
		var name string
		if err := rows.Scan(&issueID, &name); err != nil {
			return fmt.Errorf("scanning label: %w", err)
		}
		if issue, ok := issueMap[issueID]; ok {
			issue.Labels = append(issue.Labels, name)
		}
	}
	return rows.Err()
}
