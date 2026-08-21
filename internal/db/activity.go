package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// execer abstracts *sql.DB and *sql.Tx for executing statements.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// RecordActivity logs a field change on an issue.
func RecordActivity(ex execer, issueID int, field, oldVal, newVal, changedBy string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := ex.Exec(
		`INSERT INTO activity_log (issue_id, field_changed, old_value, new_value, changed_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		issueID, field, oldVal, newVal, changedBy, now,
	)
	if err != nil {
		return fmt.Errorf("recording activity: %w", err)
	}
	return nil
}

// CountActivity returns the total number of activity log entries for an issue,
// ignoring any limit. Callers pair it with GetActivity to report an honest
// pre-limit total and flag truncation.
func CountActivity(db *sql.DB, issueID int) (int, error) {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM activity_log WHERE issue_id = ?`, issueID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting activity: %w", err)
	}
	return count, nil
}

// GetActivity retrieves activity log entries for an issue, ordered by most recent first.
func GetActivity(db *sql.DB, issueID int, limit int) ([]model.Activity, error) {
	query := `SELECT id, issue_id, field_changed, old_value, new_value, changed_by, created_at
	          FROM activity_log
	          WHERE issue_id = ?
	          ORDER BY created_at DESC`
	args := []interface{}{issueID}

	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying activity: %w", err)
	}
	activities, err := scanRows(rows, "activity rows", func(r *sql.Rows) (model.Activity, error) {
		return scanActivityFrom(r)
	})
	if err != nil {
		return nil, err
	}

	return activities, nil
}

// ListAllActivity returns every activity_log row ordered by id ASC, for a full
// export.
func ListAllActivity(db *sql.DB, projectID int) ([]*model.Activity, error) {
	where, args := projectFilterVia(projectID, `issue_id IN (SELECT id FROM issues WHERE project_id = ?)`)
	rows, err := db.Query(
		`SELECT id, issue_id, field_changed, old_value, new_value, changed_by, created_at
		 FROM activity_log `+where+` ORDER BY id ASC`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all activity: %w", err)
	}
	activities, err := scanRows(rows, "activity rows", func(r *sql.Rows) (*model.Activity, error) {
		a, err := scanActivityFrom(r)
		if err != nil {
			return nil, err
		}
		return &a, nil
	})
	if err != nil {
		return nil, err
	}

	return activities, nil
}

// scanActivityFrom scans one activity_log row. Shared by GetActivity (which
// returns values) and ListAllActivity (which returns pointers) — both read
// the same seven columns and apply the same NullString projection and
// created_at parse.
func scanActivityFrom(r *sql.Rows) (model.Activity, error) {
	var a model.Activity
	var oldVal, newVal, changedBy sql.NullString
	var createdAt string
	if err := r.Scan(&a.ID, &a.IssueID, &a.FieldChanged, &oldVal, &newVal, &changedBy, &createdAt); err != nil {
		return model.Activity{}, fmt.Errorf("scanning activity row: %w", err)
	}
	a.OldValue = oldVal.String
	a.NewValue = newVal.String
	a.ChangedBy = changedBy.String

	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return model.Activity{}, fmt.Errorf("parsing activity created_at: %w", err)
	}
	a.CreatedAt = t

	return a, nil
}

// InsertActivityWithID inserts an activity_log row with a caller-supplied ID,
// skipping if the ID already exists. Must be called within an existing
// transaction. Returns true if inserted. Mirrors InsertIssueWithID.
func InsertActivityWithID(tx *sql.Tx, a *model.Activity) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO activity_log
		 (id, issue_id, field_changed, old_value, new_value, changed_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.IssueID, a.FieldChanged, a.OldValue, a.NewValue, a.ChangedBy,
		a.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("inserting activity with id %d: %w", a.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
