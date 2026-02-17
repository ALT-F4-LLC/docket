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
	defer rows.Close()

	var activities []model.Activity
	for rows.Next() {
		var a model.Activity
		var oldVal, newVal, changedBy sql.NullString
		var createdAt string
		if err := rows.Scan(&a.ID, &a.IssueID, &a.FieldChanged, &oldVal, &newVal, &changedBy, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning activity row: %w", err)
		}
		a.OldValue = oldVal.String
		a.NewValue = newVal.String
		a.ChangedBy = changedBy.String

		t, err := time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parsing activity created_at: %w", err)
		}
		a.CreatedAt = t

		activities = append(activities, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating activity rows: %w", err)
	}

	return activities, nil
}
