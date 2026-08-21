package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// safeIdentifier matches valid SQL column identifiers (lowercase letters and underscores only).
var safeIdentifier = regexp.MustCompile(`^[a-z_]+$`)

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("not found")

// requireAffected turns an Exec result into ErrNotFound when it touched no
// rows — the shape every unconditional DELETE/UPDATE-by-id verb in this
// package shares: the statement itself cannot distinguish "no such row" from
// "matched and changed nothing", so the row count is the only signal.
func requireAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListOptions holds filtering, sorting, and pagination options for ListIssues.
type ListOptions struct {
	ProjectID  int      // scope to one project (v12); 0 = every project
	Statuses   []string // filter by status (multiple = OR)
	Priorities []string // filter by priority (multiple = OR)
	Labels     []string // filter by label name (multiple = AND)
	Types      []string // filter by kind (multiple = OR)
	Assignee   string   // filter by assignee
	ParentID   *int     // filter by parent issue ID
	RootsOnly  bool     // only issues with no parent
	// RunID scopes the listing to one run's ROSTER — the issues bound to it in
	// `run_issues` (DKT-405); 0 = every issue. It is the same membership
	// ListRunIssues reads, expressed as a filter so it composes with the other
	// filters, the sort, and the pre-limit COUNT rather than being intersected
	// afterwards in the caller.
	RunID       int
	IncludeDone bool   // include done status (default: exclude)
	Sort        string // field name
	SortDir     string // "asc" or "desc"
	Limit       int    // max results
	Offset      int    // for pagination
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
	// `resolution` is writable so `issue reopen` can CLEAR it. An issue put
	// back on the board is one the operator has taken back off the machine's
	// hands, and leaving "abandoned" on it would be the mirror of the defect
	// that introduced the column — a stale terminal fact outliving the
	// decision that replaced it (DKT-245).
	"resolution": true,
}

// CreateIssue inserts a new issue and returns its ID. Labels are created
// (find-or-create) and linked to the issue within the same transaction.
// Files are attached to the issue if provided.
func CreateIssue(db *sql.DB, issue *model.Issue, labels []string, files []string) (int, error) {
	return CreateIssueIdempotent(db, issue, labels, files, "")
}

// CreateIssueIdempotent is CreateIssue with an optional idempotency key.
//
// When idempotencyKey is non-empty and was already used for this scope, the
// original issue's id is returned and nothing is inserted — a retried create
// after a dropped response must succeed, not fail. The key record and the
// insert commit in the SAME transaction, so a crash between them cannot
// orphan either.
func CreateIssueIdempotent(db *sql.DB, issue *model.Issue, labels []string, files []string, idempotencyKey string) (int, error) {
	if idempotencyKey != "" {
		existingID, found, err := LookupIdempotencyKey(db, ScopeIssueCreate, idempotencyKey)
		if err != nil {
			return 0, err
		}
		if found {
			return existingID, nil
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	projectID := projectOrDefault(issue.ProjectID)
	res, err := tx.Exec(
		`INSERT INTO issues (project_id, parent_id, title, description, status, priority, kind, assignee, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID,
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

	// Attach labels, in the issue's own project.
	for _, name := range labels {
		labelID, err := findOrCreateLabel(tx, projectID, name)
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

	// Attach files.
	for _, fp := range files {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO issue_files (issue_id, file_path) VALUES (?, ?)`,
			id, fp,
		); err != nil {
			return 0, fmt.Errorf("attaching file %q: %w", fp, err)
		}
	}

	// Record creation activity.
	if err := RecordActivity(tx, id, "created", "", "", ""); err != nil {
		return 0, err
	}

	// Record file attachment activity if files were provided at creation.
	if len(files) > 0 {
		sorted := slices.Clone(files)
		sort.Strings(sorted)
		if err := RecordActivity(tx, id, "files", "", strings.Join(sorted, ", "), ""); err != nil {
			return 0, err
		}
	}

	// Same transaction as the insert: a crash between the two would otherwise
	// leave a created issue whose key is unrecorded, so the retry duplicates.
	if idempotencyKey != "" {
		if err := RecordIdempotencyKeyTx(tx, ScopeIssueCreate, idempotencyKey, id); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return id, nil
}

// issueColumns is the column list every issue SELECT uses, in the exact order
// scanIssueFrom scans them. It exists as one constant so a column added here
// cannot drift from the scanner — the v6 lease columns are appended at the end
// for exactly that reason.
//
// issueColumnsQualified is the same list for queries that alias issues as `i`.
const (
	issueColumns = `id, parent_id, title, description, status, priority, kind, assignee,
	                created_at, updated_at, version, owner, token_hash, expires_ms, attempt,
	                scope_globs, project_id, resolution`

	issueColumnsQualified = `i.id, i.parent_id, i.title, i.description, i.status, i.priority, i.kind, i.assignee,
	                         i.created_at, i.updated_at, i.version, i.owner, i.token_hash, i.expires_ms, i.attempt,
	                         i.scope_globs, i.project_id, i.resolution`
)

// IssueResolutionAbandoned is the resolution the `abandon-issue` routing and
// `run abandon --issue` write: the machine stopped working this issue and did
// not finish it. It is model.ResolutionAbandoned rather than a second literal,
// so the value the engine writes and the value the renderer tests for cannot
// drift apart.
const IssueResolutionAbandoned = model.ResolutionAbandoned

// SetIssueResolutionTx records how a routing left an issue.
//
// It touches ONLY `resolution`. `abandon-issue` is deliberate about not
// forcing the issue's status — the run stopping work is a statement about the
// run, and closing the issue here would take the operator's triage decision
// away — so the resolution is an additional fact beside the status, never a
// replacement for it (DKT-245). `updated_at` moves because the row changed;
// `version` does not, because this is the machine recording an outcome rather
// than a CAS-guarded edit competing with a caller's read.
func SetIssueResolutionTx(tx *sql.Tx, issueID int, resolution string) error {
	if _, err := tx.Exec(
		`UPDATE issues SET resolution = ?, updated_at = ? WHERE id = ?`,
		resolution, time.Now().UTC().Format(time.RFC3339), issueID,
	); err != nil {
		return fmt.Errorf("recording the resolution of issue %d: %w", issueID, err)
	}
	return nil
}

// IssueProjectID returns the project an issue is homed in, or ErrNotFound.
//
// It exists for validations that need the project WITHOUT the issue's whole
// row: attaching issues to runs checks a whole set before writing anything
// (DKT-21), and loading each candidate's labels and lease state to read one
// column would make that loop's cost proportional to data it discards.
func IssueProjectID(db *sql.DB, issueID int) (int, error) {
	var projectID int
	err := db.QueryRow(`SELECT project_id FROM issues WHERE id = ?`, issueID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("reading issue %d's project: %w", issueID, err)
	}
	return projectID, nil
}

// GetIssue retrieves an issue by ID.
func GetIssue(db *sql.DB, id int) (*model.Issue, error) {
	row := db.QueryRow(
		`SELECT `+issueColumns+` FROM issues WHERE id = ?`, id,
	)
	return scanIssue(row)
}

// GetIssuesByIDs retrieves multiple issues by their IDs in a single query.
// The returned map is keyed by issue ID. IDs that don't exist are silently
// skipped (no error for missing rows). Labels are hydrated on all returned issues.
func GetIssuesByIDs(db *sql.DB, ids []int) (map[int]*model.Issue, error) {
	if len(ids) == 0 {
		return make(map[int]*model.Issue), nil
	}

	placeholders := makePlaceholders(len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	query := fmt.Sprintf(
		`SELECT `+issueColumns+`
		 FROM issues WHERE id IN (%s)`, placeholders,
	)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying issues by IDs: %w", err)
	}
	issues, err := scanRows(rows, "issue rows", scanIssueRow)
	if err != nil {
		return nil, err
	}

	if err := HydrateLabels(db, issues); err != nil {
		return nil, fmt.Errorf("hydrating labels: %w", err)
	}

	if err := HydrateFiles(db, issues); err != nil {
		return nil, fmt.Errorf("hydrating files: %w", err)
	}

	result := make(map[int]*model.Issue, len(issues))
	for _, issue := range issues {
		result[issue.ID] = issue
	}
	return result, nil
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

	if opts.ProjectID != 0 {
		whereClauses = append(whereClauses, "i.project_id = ?")
		args = append(args, opts.ProjectID)
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

	// The run roster (DKT-405). A subquery rather than a join: `run_issues`
	// holds at most one row per (run, issue), but the labels join above can
	// already multiply rows, and an EXISTS-shaped filter cannot add to that.
	if opts.RunID != 0 {
		whereClauses = append(whereClauses,
			"i.id IN (SELECT issue_id FROM run_issues WHERE run_id = ?)")
		args = append(args, opts.RunID)
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
	var orderBySQL string
	if opts.Sort != "" && validSortFields[opts.Sort] {
		sortField := opts.Sort
		// Defense-in-depth: reject any sort field that doesn't look like a plain column name,
		// even if it passed the allowlist check above.
		if !safeIdentifier.MatchString(sortField) {
			return nil, 0, fmt.Errorf("invalid sort field %q", sortField)
		}
		sortDir := "DESC"
		if strings.EqualFold(opts.SortDir, "asc") {
			sortDir = "ASC"
		}
		// Safe: sortField validated against validSortFields and safeIdentifier; sortDir is "ASC" or "DESC".
		orderBySQL = fmt.Sprintf("ORDER BY i.%s %s", sortField, sortDir)
	} else {
		// Default composite sort: status rank, then priority rank, then newest first.
		orderBySQL = `ORDER BY
			CASE i.status
				WHEN 'in-progress' THEN 0
				WHEN 'review'      THEN 1
				WHEN 'todo'        THEN 2
				WHEN 'backlog'     THEN 3
				WHEN 'done'        THEN 4
				ELSE 5
			END ASC,
			CASE i.priority
				WHEN 'critical' THEN 0
				WHEN 'high'     THEN 1
				WHEN 'medium'   THEN 2
				WHEN 'low'      THEN 3
				WHEN 'none'     THEN 4
				ELSE 5
			END ASC,
			i.created_at DESC`
	}

	// Main query.
	mainQuery := fmt.Sprintf(
		`SELECT `+issueColumnsQualified+`
		 FROM issues i %s %s %s %s %s`,
		joinClause, whereSQL, groupBySQL, havingSQL, orderBySQL,
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
	issues, err := scanRows(rows, "issue rows", scanIssueRow)
	if err != nil {
		return nil, 0, err
	}
	if issues == nil {
		issues = make([]*model.Issue, 0)
	}

	// Hydrate labels for all returned issues to avoid N+1 queries in callers.
	if err := HydrateLabels(db, issues); err != nil {
		return nil, 0, fmt.Errorf("hydrating labels: %w", err)
	}

	if err := HydrateFiles(db, issues); err != nil {
		return nil, 0, fmt.Errorf("hydrating files: %w", err)
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
	return UpdateIssueCAS(db, id, updates, changedBy, nil)
}

// UpdateIssueCAS is UpdateIssue with an optional optimistic-concurrency
// precondition. When ifVersion is non-nil the update applies only if the
// issue's current version matches, and returns ErrVersionConflict otherwise.
// The version is bumped either way, so concurrent CAS writers are detected
// even when this caller did not supply a precondition.
func UpdateIssueCAS(db *sql.DB, id int, updates map[string]interface{}, changedBy string, ifVersion *int) error {
	return updateIssueCASLease(db, id, updates, changedBy, ifVersion, nil)
}

// UpdateIssueCASLease is UpdateIssueCAS for a verb that ends a lease: when the
// issue carries a LIVE lease, the caller must hold it (token) and the lease is
// cleared as part of the same transaction — the issue-level analog of a step's
// token retiring when its artifact records (engine-spec.md §2).
//
// When no live lease exists the token is ignored entirely and behavior is
// exactly UpdateIssueCAS's. That is the dormancy guarantee: an unclaimed issue
// is outside the lease mechanism, so a repo that never claims sees no change on
// any verb (engine-spec.md §9 item 8).
func UpdateIssueCASLease(db *sql.DB, id int, updates map[string]interface{}, changedBy string, ifVersion *int, token string) error {
	return updateIssueCASLease(db, id, updates, changedBy, ifVersion, &token)
}

// updateIssueCASLease is the shared implementation. A nil token means the verb
// is not lease-mediated and no lease check runs at all.
func updateIssueCASLease(db *sql.DB, id int, updates map[string]interface{}, changedBy string, ifVersion *int, token *string) error {
	if len(updates) == 0 {
		// Still enforce the precondition: `--if-version` on a no-op edit must
		// not silently succeed against a row that moved underneath the caller.
		if ifVersion == nil {
			return nil
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("beginning transaction: %w", err)
		}
		defer tx.Rollback()
		if err := CheckAndBumpVersion(tx, "issues", id, ifVersion); err != nil {
			return err
		}
		if token != nil {
			if err := AuthorizeLeaseMutation(tx, id, *token, model.NowMS()); err != nil {
				return err
			}
		}
		return tx.Commit()
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// CAS first: a version mismatch must abort before any field is written.
	if err := CheckAndBumpVersion(tx, "issues", id, ifVersion); err != nil {
		return err
	}

	// Then the lease: a non-holder must be refused before any field is
	// written, so a refusal never leaves a partial mutation behind. The whole
	// check and mutation share this transaction, so they commit or roll back
	// together.
	if token != nil {
		if err := AuthorizeLeaseMutation(tx, id, *token, model.NowMS()); err != nil {
			return err
		}
	}

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

	if err := requireAffected(res); err != nil {
		return err
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
		`SELECT `+issueColumns+`
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
	return requireAffected(res)
}

// GetSubIssues returns all direct children of an issue.
func GetSubIssues(db *sql.DB, parentID int) ([]*model.Issue, error) {
	rows, err := db.Query(
		`SELECT `+issueColumns+`
		 FROM issues WHERE parent_id = ? ORDER BY created_at ASC`, parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying sub-issues: %w", err)
	}
	return scanRows(rows, "sub-issues", scanIssueRow)
}

// GetSubIssueTree returns the full recursive tree of all descendants under an issue.
func GetSubIssueTree(db *sql.DB, parentID int) ([]*model.Issue, error) {
	rows, err := db.Query(
		`WITH RECURSIVE tree(id) AS (
			SELECT id FROM issues WHERE parent_id = ?
			UNION ALL
			SELECT i.id FROM issues i JOIN tree t ON i.parent_id = t.id
		)
		SELECT `+issueColumnsQualified+`
		FROM issues i JOIN tree t ON i.id = t.id
		ORDER BY i.created_at ASC`, parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying sub-issue tree: %w", err)
	}
	return scanRows(rows, "sub-issue tree", scanIssueRow)
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

// GetBatchSubIssueProgress returns (done, total) counts for descendants of each
// given parent ID in a single query, avoiding N+1 overhead.
func GetBatchSubIssueProgress(conn *sql.DB, parentIDs []int) (map[int][2]int, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}

	placeholders := make([]string, len(parentIDs))
	args := make([]interface{}, len(parentIDs))
	for i, id := range parentIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `WITH RECURSIVE tree(id, root_parent_id) AS (
		SELECT id, parent_id FROM issues WHERE parent_id IN (` + strings.Join(placeholders, ",") + `)
		UNION ALL
		SELECT i.id, t.root_parent_id FROM issues i JOIN tree t ON i.parent_id = t.id
	)
	SELECT
		t.root_parent_id,
		COALESCE(SUM(CASE WHEN i.status = 'done' THEN 1 ELSE 0 END), 0),
		COUNT(*)
	FROM issues i JOIN tree t ON i.id = t.id
	GROUP BY t.root_parent_id`

	rows, err := conn.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying batch sub-issue progress: %w", err)
	}
	defer rows.Close()

	result := make(map[int][2]int)
	for rows.Next() {
		var parentID, done, total int
		if err := rows.Scan(&parentID, &done, &total); err != nil {
			return nil, fmt.Errorf("scanning batch sub-issue progress: %w", err)
		}
		result[parentID] = [2]int{done, total}
	}
	return result, rows.Err()
}

// IsDescendant returns true if potentialDescendantID is a descendant of issueID.
// This is used to detect cycles when reparenting an issue.
func IsDescendant(db *sql.DB, issueID, potentialDescendantID int) (bool, error) {
	var found bool
	err := db.QueryRow(
		`WITH RECURSIVE tree(id) AS (
			SELECT id FROM issues WHERE parent_id = ?
			UNION ALL
			SELECT i.id FROM issues i JOIN tree t ON i.parent_id = t.id
		)
		SELECT EXISTS(SELECT 1 FROM tree WHERE id = ?)`, issueID, potentialDescendantID,
	).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("checking descendant: %w", err)
	}
	return found, nil
}

// OrphanSubIssues sets parent_id to NULL for all direct children of the given issue.
// Activity is recorded for each affected child within a transaction.
func OrphanSubIssues(db *sql.DB, parentID int, author string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Find all direct children before updating.
	rows, err := tx.Query("SELECT id FROM issues WHERE parent_id = ?", parentID)
	if err != nil {
		return fmt.Errorf("querying children: %w", err)
	}
	childIDs, err := scanRows(rows, "children", func(r *sql.Rows) (int, error) {
		var id int
		if err := r.Scan(&id); err != nil {
			return 0, fmt.Errorf("scanning child id: %w", err)
		}
		return id, nil
	})
	if err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.Exec(
		`UPDATE issues SET parent_id = NULL, updated_at = ? WHERE parent_id = ?`,
		now, parentID,
	)
	if err != nil {
		return fmt.Errorf("orphaning sub-issues: %w", err)
	}

	// Record activity for each orphaned child.
	oldParent := fmt.Sprintf("%d", parentID)
	for _, childID := range childIDs {
		if err := RecordActivity(tx, childID, "parent_id", oldParent, "", author); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// CascadeDeleteIssue deletes an issue and all its descendants recursively
// in a single transaction. The recursive CTE finds all descendant issues;
// ON DELETE CASCADE constraints on comments, issue_labels, issue_relations,
// and activity_log handle cleanup of related rows automatically.
func CascadeDeleteIssue(db *sql.DB, id int) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Verify the root issue exists.
	var exists bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?)", id).Scan(&exists); err != nil {
		return fmt.Errorf("checking issue existence: %w", err)
	}
	if !exists {
		return ErrNotFound
	}

	// Delete all descendants and the root issue itself using a recursive CTE.
	_, err = tx.Exec(
		`WITH RECURSIVE tree(id) AS (
			SELECT id FROM issues WHERE id = ?
			UNION ALL
			SELECT i.id FROM issues i JOIN tree t ON i.parent_id = t.id
		)
		DELETE FROM issues WHERE id IN (SELECT id FROM tree)`, id,
	)
	if err != nil {
		return fmt.Errorf("cascade deleting issue: %w", err)
	}

	return tx.Commit()
}

// --- helpers ---

// scanIssueFrom scans a single issue from any scanner (*sql.Row or *sql.Rows).
func scanIssueFrom(s scanner) (*model.Issue, error) {
	var i model.Issue
	var parentID sql.NullInt64
	var description, assignee sql.NullString
	var createdAt, updatedAt string
	var owner, tokenHash sql.NullString
	var expiresMS sql.NullInt64
	var attempt int
	var scopeGlobs sql.NullString

	err := s.Scan(
		&i.ID, &parentID, &i.Title, &description,
		&i.Status, &i.Priority, &i.Kind, &assignee,
		&createdAt, &updatedAt, &i.Version,
		&owner, &tokenHash, &expiresMS, &attempt,
		&scopeGlobs, &i.ProjectID, &i.Resolution,
	)
	if err != nil {
		return nil, err
	}

	// The declared scope rides on every issue read (DKT-55). NULL stays a nil
	// pointer — "no scope declared" — while a stored array, `[]` included,
	// parses into a non-nil slice.
	if scopeGlobs.Valid && scopeGlobs.String != "" {
		globs := []string{}
		if err := json.Unmarshal([]byte(scopeGlobs.String), &globs); err != nil {
			return nil, fmt.Errorf(
				"issue %d: stored scope_globs is not a JSON array: %w", i.ID, err)
		}
		i.Scope = &globs
	}

	// The lease is attached only when one is actually held. An unclaimed issue
	// carries a nil Lease, so nothing about its rendering changes at any JSON
	// version — the dormancy guarantee at the read path (engine-spec.md §9
	// item 8). Liveness is NOT computed here: it is derived per read from
	// ExpiresMS at marshal time, and nothing on this path writes.
	if owner.Valid && owner.String != "" {
		i.Lease = &model.Lease{
			Owner:     owner.String,
			TokenHash: tokenHash.String,
			ExpiresMS: expiresMS.Int64,
			Attempt:   attempt,
		}
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

// findOrCreateLabel looks up a label by name WITHIN ONE PROJECT, creating it
// if it doesn't exist, and returns the label ID. Label names are a per-project
// namespace (v12): two projects' `bug` labels are different rows.
func findOrCreateLabel(tx *sql.Tx, projectID int, name string) (int, error) {
	projectID = projectOrDefault(projectID)
	var id int
	err := tx.QueryRow(
		"SELECT id FROM labels WHERE project_id = ? AND name = ?",
		projectID, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("querying label: %w", err)
	}

	res, err := tx.Exec(
		"INSERT INTO labels (project_id, name) VALUES (?, ?)", projectID, name)
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
	return scanRows(rows, "labels", func(r *sql.Rows) (string, error) {
		var name string
		if err := r.Scan(&name); err != nil {
			return "", fmt.Errorf("scanning label: %w", err)
		}
		return name, nil
	})
}

// ListAllIssues returns every issue in the database, including done issues,
// with no filters, sorting, or pagination. Labels are hydrated on all results.
func ListAllIssues(db *sql.DB, projectID int) ([]*model.Issue, error) {
	where, args := projectFilter(projectID, "WHERE")
	rows, err := db.Query(
		`SELECT `+issueColumns+`
		 FROM issues `+where+` ORDER BY id ASC`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all issues: %w", err)
	}
	issues, err := scanRows(rows, "issue rows", scanIssueRow)
	if err != nil {
		return nil, err
	}

	if err := HydrateLabels(db, issues); err != nil {
		return nil, fmt.Errorf("hydrating labels: %w", err)
	}

	if err := HydrateFiles(db, issues); err != nil {
		return nil, fmt.Errorf("hydrating files: %w", err)
	}

	return issues, nil
}

// projectFilter renders the optional per-project predicate the count queries
// share. A zero projectID means every project — the pre-v12 reading.
func projectFilter(projectID int, prefix string) (string, []any) {
	if projectID == 0 {
		return "", nil
	}
	return prefix + " project_id = ?", []any{projectID}
}

// projectFilterVia is projectFilter for a table with no project_id column of
// its own: it scopes through a foreign key instead, via a subquery clause
// like "issue_id IN (SELECT id FROM issues WHERE project_id = ?)". Every one
// of its callers (the full-export listers across activity.go, comments.go,
// doc_comments.go, docs.go, doc_links.go, files.go, labels.go, proposals.go,
// and relations.go) needs this as the query's OWN WHERE clause rather than an
// "AND" onto an existing one, so unlike projectFilter it takes no prefix.
func projectFilterVia(projectID int, clause string) (string, []any) {
	if projectID == 0 {
		return "", nil
	}
	return "WHERE " + clause, []any{projectID}
}

// CountIssues returns the number of issues, scoped to a project when
// projectID is non-zero.
func CountIssues(db *sql.DB, projectID int) (int, error) {
	where, args := projectFilter(projectID, "WHERE")
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM issues `+where, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting issues: %w", err)
	}
	return count, nil
}

// CountRootIssues returns the number of issues with no parent, scoped to a
// project when projectID is non-zero.
func CountRootIssues(db *sql.DB, projectID int) (int, error) {
	where, args := projectFilter(projectID, "AND")
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM issues WHERE parent_id IS NULL `+where, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting root issues: %w", err)
	}
	return count, nil
}

// countByColumn returns a map of value -> count for the given column grouped by that column.
func countByColumn(db *sql.DB, projectID int, column string) (map[string]int, error) {
	where, args := projectFilter(projectID, "WHERE")
	rows, err := db.Query(
		fmt.Sprintf(`SELECT %s, COUNT(*) FROM issues %s GROUP BY %s`, column, where, column),
		args...)
	if err != nil {
		return nil, fmt.Errorf("counting by %s: %w", column, err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			return nil, fmt.Errorf("scanning %s count: %w", column, err)
		}
		result[key] = count
	}
	return result, rows.Err()
}

// CountByStatus returns a map of status -> count, scoped to a project when
// projectID is non-zero.
func CountByStatus(db *sql.DB, projectID int) (map[string]int, error) {
	return countByColumn(db, projectID, "status")
}

// CountByPriority returns a map of priority -> count, scoped to a project when
// projectID is non-zero.
func CountByPriority(db *sql.DB, projectID int) (map[string]int, error) {
	return countByColumn(db, projectID, "priority")
}

// ClearAllData deletes all data from every persistent table within a single
// transaction. The schema and meta table are preserved.
//
// Tables are deleted in FK-correct order — children before parents. FK CASCADE
// would handle dependents implicitly, but the explicit ordering keeps
// behaviour identical to the pre-v4 function and makes the contract auditable
// from the function body.
//
// Doc tables and the pre-existing proposals/votes/proposal_issues tables are
// included; prior to v4 the latter three were silently omitted, which broke
// `--replace` import on any DB containing proposals (TDD §5.4 S4 / R7).
func ClearAllData(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	if err := ClearAllDataTx(tx); err != nil {
		return err
	}

	return tx.Commit()
}

func ClearAllDataTx(tx *sql.Tx) error {
	tables := []string{
		"doc_comments",
		"doc_revisions",
		"proposal_docs",
		"doc_issue_links",
		"docs",
		"proposal_issues",
		"votes",
		"proposals",
		"activity_log",
		"issue_relations",
		"issue_files",
		"issue_labels",
		"comments",
		"issues",
		"labels",
	}
	for _, table := range tables {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			if strings.Contains(err.Error(), "no such table") {
				continue
			}
			return fmt.Errorf("clearing %s: %w", table, err)
		}
	}

	return nil
}

// ClearProjectDataTx is ClearAllDataTx scoped to ONE project (v12): the same
// tracker tables, children before parents, but only the rows reachable from
// this project's roots. Under the shared store, "replace everything" scoped
// any wider would delete projects the operator was not looking at.
func ClearProjectDataTx(tx *sql.Tx, projectID int) error {
	projectID = projectOrDefault(projectID)

	// Children keyed through issues.
	issueChildren := []string{
		"doc_issue_links", "proposal_issues",
		"activity_log", "issue_files", "issue_labels", "comments",
	}
	issueCols := map[string]string{
		"doc_issue_links": "issue_id", "proposal_issues": "issue_id",
		"activity_log": "issue_id", "issue_files": "issue_id",
		"issue_labels": "issue_id", "comments": "issue_id",
	}
	for _, table := range issueChildren {
		if _, err := tx.Exec(
			`DELETE FROM `+table+` WHERE `+issueCols[table]+` IN
			   (SELECT id FROM issues WHERE project_id = ?)`, projectID); err != nil {
			return fmt.Errorf("clearing %s: %w", table, err)
		}
	}
	// Relations touch two issues; either end in the project removes the row —
	// a cross-project relation cannot survive one side's replacement.
	if _, err := tx.Exec(
		`DELETE FROM issue_relations WHERE
		   source_issue_id IN (SELECT id FROM issues WHERE project_id = ?) OR
		   target_issue_id IN (SELECT id FROM issues WHERE project_id = ?)`,
		projectID, projectID); err != nil {
		return fmt.Errorf("clearing issue_relations: %w", err)
	}

	// Children keyed through docs and proposals.
	for table, parent := range map[string]string{
		"doc_comments":  "doc_id IN (SELECT id FROM docs WHERE project_id = ?)",
		"doc_revisions": "doc_id IN (SELECT id FROM docs WHERE project_id = ?)",
		"proposal_docs": "doc_id IN (SELECT id FROM docs WHERE project_id = ?)",
		"votes":         "proposal_id IN (SELECT id FROM proposals WHERE project_id = ?)",
	} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE `+parent, projectID); err != nil {
			return fmt.Errorf("clearing %s: %w", table, err)
		}
	}

	// The roots.
	for _, table := range []string{"docs", "proposals", "issues", "labels"} {
		if _, err := tx.Exec(
			`DELETE FROM `+table+` WHERE project_id = ?`, projectID); err != nil {
			return fmt.Errorf("clearing %s: %w", table, err)
		}
	}
	return nil
}

// InsertGapIssueTx materializes a backlog issue from a recorded gap artifact
// (DKT-72), inside the completion saga's transaction, and relates it to the
// issue whose step recorded the gap.
//
// One transaction with the artifact, because the pair is the whole point: a
// gap that recorded an artifact but no issue is residue nothing re-reads —
// the failure mode this replaces — and an issue without its artifact is a
// claim with no record behind it. `relates_to` rather than a directional
// relation: a gap is out-of-scope BY DEFINITION, so it must not block the
// issue that surfaced it.
func InsertGapIssueTx(tx *sql.Tx, projectID int, title, description string, relatedIssueID int) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.Exec(
		`INSERT INTO issues (project_id, title, description, status, priority, kind, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		projectOrDefault(projectID), title, description,
		string(model.StatusBacklog), string(model.PriorityNone), string(model.IssueKindTask),
		now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("materializing a gap issue: %w", err)
	}
	id64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("materializing a gap issue: %w", err)
	}
	id := int(id64)

	if _, err := tx.Exec(
		`INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, ?, ?)`,
		id, relatedIssueID, string(model.RelationRelatesTo), now,
	); err != nil {
		return 0, fmt.Errorf("relating gap issue %d to %d: %w", id, relatedIssueID, err)
	}
	return id, nil
}

// InsertIssueWithID inserts an issue with a specific ID (not auto-increment),
// skipping if the ID already exists. Returns true if the row was inserted.
// Must be called within an existing transaction.
func InsertIssueWithID(tx *sql.Tx, issue *model.Issue) (bool, error) {
	// The declared scope round-trips (DKT-55): export carries `scope` for
	// issues that declared one, so import must restore it — writing the other
	// nine fields and dropping this one would be a silent export/import skew.
	// A nil Scope stores SQL NULL, preserving the declared/undeclared
	// distinction across the round trip.
	var scopeGlobs any
	if issue.Scope != nil {
		encoded, err := json.Marshal(*issue.Scope)
		if err != nil {
			return false, fmt.Errorf("serializing scope for issue %d: %w", issue.ID, err)
		}
		scopeGlobs = string(encoded)
	}

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO issues (id, project_id, parent_id, title, description, status, priority, kind, assignee, created_at, updated_at, scope_globs)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		issue.ID,
		projectOrDefault(issue.ProjectID),
		nilIfZeroPtr(issue.ParentID),
		issue.Title,
		issue.Description,
		string(issue.Status),
		string(issue.Priority),
		string(issue.Kind),
		issue.Assignee,
		issue.CreatedAt.UTC().Format(time.RFC3339),
		issue.UpdatedAt.UTC().Format(time.RFC3339),
		scopeGlobs,
	)
	if err != nil {
		return false, fmt.Errorf("inserting issue with id %d: %w", issue.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// HydrateLabels bulk-loads labels for a set of issues, populating each issue's
// Labels field. This avoids N+1 queries when displaying lists.
func HydrateLabels(db *sql.DB, issues []*model.Issue) error {
	if len(issues) == 0 {
		return nil
	}

	ids := make([]any, len(issues))
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
