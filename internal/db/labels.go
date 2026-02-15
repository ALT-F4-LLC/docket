package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// ErrNotAttached is returned when a label is not attached to the specified issue.
var ErrNotAttached = errors.New("label not attached")

// ErrLabelColorConflict is returned when --color specifies a different color than
// an existing label already has.
var ErrLabelColorConflict = errors.New("label color conflict")

// GetLabelByName retrieves a label by its unique name, including the count of
// issues currently attached to it. Returns ErrNotFound if no label with that
// name exists.
func GetLabelByName(db *sql.DB, name string) (*model.LabelWithCount, error) {
	var lc model.LabelWithCount
	var color sql.NullString

	err := db.QueryRow(
		`SELECT l.id, l.name, l.color, COUNT(il.issue_id) AS issue_count
		 FROM labels l
		 LEFT JOIN issue_labels il ON il.label_id = l.id
		 WHERE l.name = ?
		 GROUP BY l.id`, name,
	).Scan(&lc.ID, &lc.Name, &color, &lc.IssueCount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("querying label: %w", err)
	}

	lc.Color = color.String
	return &lc, nil
}

// ListAllLabels returns every label along with the count of issues using it,
// sorted alphabetically by name.
func ListAllLabels(db *sql.DB) ([]*model.LabelWithCount, error) {
	rows, err := db.Query(
		`SELECT l.id, l.name, l.color, COUNT(il.issue_id) AS issue_count
		 FROM labels l
		 LEFT JOIN issue_labels il ON il.label_id = l.id
		 GROUP BY l.id
		 ORDER BY l.name`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying labels: %w", err)
	}
	defer rows.Close()

	var labels []*model.LabelWithCount
	for rows.Next() {
		var lc model.LabelWithCount
		var color sql.NullString
		if err := rows.Scan(&lc.ID, &lc.Name, &color, &lc.IssueCount); err != nil {
			return nil, fmt.Errorf("scanning label: %w", err)
		}
		lc.Color = color.String
		labels = append(labels, &lc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating label rows: %w", err)
	}

	return labels, nil
}

// ListAllLabelsRaw returns every label as a model.Label object (without issue
// counts), sorted alphabetically by name.
func ListAllLabelsRaw(db *sql.DB) ([]*model.Label, error) {
	rows, err := db.Query(
		`SELECT id, name, color FROM labels ORDER BY name`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all labels: %w", err)
	}
	defer rows.Close()

	var labels []*model.Label
	for rows.Next() {
		var l model.Label
		var color sql.NullString
		if err := rows.Scan(&l.ID, &l.Name, &color); err != nil {
			return nil, fmt.Errorf("scanning label: %w", err)
		}
		l.Color = color.String
		labels = append(labels, &l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating label rows: %w", err)
	}

	return labels, nil
}

// ListAllIssueLabelMappings returns all (issue_id, label_id) pairs from the
// issue_labels table.
func ListAllIssueLabelMappings(db *sql.DB) ([]model.IssueLabelMapping, error) {
	rows, err := db.Query(
		`SELECT issue_id, label_id FROM issue_labels ORDER BY issue_id, label_id`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying issue-label mappings: %w", err)
	}
	defer rows.Close()

	var mappings []model.IssueLabelMapping
	for rows.Next() {
		var m model.IssueLabelMapping
		if err := rows.Scan(&m.IssueID, &m.LabelID); err != nil {
			return nil, fmt.Errorf("scanning issue-label mapping: %w", err)
		}
		mappings = append(mappings, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating issue-label mappings: %w", err)
	}

	return mappings, nil
}

// InsertLabelWithID inserts a label with a specific ID (not auto-increment),
// skipping if the ID already exists. Returns true if the row was inserted.
// Must be called within an existing transaction.
func InsertLabelWithID(tx *sql.Tx, label *model.Label) (bool, error) {
	var colorVal any
	if label.Color != "" {
		colorVal = label.Color
	}
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO labels (id, name, color) VALUES (?, ?, ?)`,
		label.ID,
		label.Name,
		colorVal,
	)
	if err != nil {
		return false, fmt.Errorf("inserting label with id %d: %w", label.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertIssueLabelMapping inserts an issue_labels row linking an issue to a label,
// skipping if the mapping already exists. Returns true if the row was inserted.
// Must be called within an existing transaction.
func InsertIssueLabelMapping(tx *sql.Tx, issueID, labelID int) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO issue_labels (issue_id, label_id) VALUES (?, ?)`,
		issueID, labelID,
	)
	if err != nil {
		return false, fmt.Errorf("inserting issue-label mapping (issue=%d, label=%d): %w", issueID, labelID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteLabel removes a label by ID. CASCADE constraints handle cleanup of
// issue_labels rows. Activity is recorded for each affected issue using the
// provided name. Returns the list of issue IDs that were attached to the label.
func DeleteLabel(db *sql.DB, labelID int, name, author string) ([]int, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Collect attached issue IDs before deletion.
	rows, err := tx.Query(`SELECT issue_id FROM issue_labels WHERE label_id = ?`, labelID)
	if err != nil {
		return nil, fmt.Errorf("querying attached issues: %w", err)
	}
	defer rows.Close()

	var issueIDs []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning issue id: %w", err)
		}
		issueIDs = append(issueIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating issue ids: %w", err)
	}
	rows.Close()

	// Record activity for each affected issue before CASCADE deletes the links.
	now := time.Now().UTC().Format(time.RFC3339)
	for _, issueID := range issueIDs {
		if err := RecordActivity(tx, issueID, "label_removed", name, "", author); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE issues SET updated_at = ? WHERE id = ?`, now, issueID); err != nil {
			return nil, fmt.Errorf("updating issue timestamp: %w", err)
		}
	}

	// Delete the label; CASCADE removes issue_labels rows.
	if _, err := tx.Exec(`DELETE FROM labels WHERE id = ?`, labelID); err != nil {
		return nil, fmt.Errorf("deleting label: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing transaction: %w", err)
	}

	return issueIDs, nil
}

// AddLabelToIssue attaches a label to an issue within a transaction. The label
// is created if it does not already exist (with the given color). Activity is
// recorded and the issue's updated_at timestamp is touched.
func AddLabelToIssue(db *sql.DB, issueID int, labelName, color string, author string) error {
	return AddLabelsToIssue(db, issueID, []string{labelName}, color, author)
}

// AddLabelsToIssue attaches multiple labels to an issue atomically within a
// single transaction. Labels are created if they do not already exist (with the
// given color). Activity is recorded for each newly attached label and the
// issue's updated_at timestamp is touched once.
func AddLabelsToIssue(db *sql.DB, issueID int, labelNames []string, color string, author string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Verify the issue exists.
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?)`, issueID).Scan(&exists); err != nil {
		return fmt.Errorf("checking issue existence: %w", err)
	}
	if !exists {
		return ErrNotFound
	}

	var anyAdded bool
	for _, labelName := range labelNames {
		// Find or create the label.
		var labelID int
		var existingColor sql.NullString
		err = tx.QueryRow(`SELECT id, color FROM labels WHERE name = ?`, labelName).Scan(&labelID, &existingColor)
		if errors.Is(err, sql.ErrNoRows) {
			var colorVal any
			if color != "" {
				colorVal = color
			}
			res, err := tx.Exec(`INSERT INTO labels (name, color) VALUES (?, ?)`, labelName, colorVal)
			if err != nil {
				return fmt.Errorf("inserting label: %w", err)
			}
			id64, err := res.LastInsertId()
			if err != nil {
				return fmt.Errorf("getting label id: %w", err)
			}
			labelID = int(id64)
		} else if err != nil {
			return fmt.Errorf("querying label: %w", err)
		} else if color != "" && existingColor.Valid && existingColor.String != color {
			return ErrLabelColorConflict
		} else if color != "" && !existingColor.Valid {
			if _, err := tx.Exec(`UPDATE labels SET color = ? WHERE id = ?`, color, labelID); err != nil {
				return fmt.Errorf("updating label color: %w", err)
			}
		}

		// Link the label to the issue (ignore if already attached).
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO issue_labels (issue_id, label_id) VALUES (?, ?)`,
			issueID, labelID,
		)
		if err != nil {
			return fmt.Errorf("linking label: %w", err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking rows affected: %w", err)
		}

		if n > 0 {
			if err := RecordActivity(tx, issueID, "label_added", "", labelName, author); err != nil {
				return err
			}
			anyAdded = true
		}
	}

	// Touch updated_at once if any labels were actually added.
	if anyAdded {
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.Exec(`UPDATE issues SET updated_at = ? WHERE id = ?`, now, issueID); err != nil {
			return fmt.Errorf("updating issue timestamp: %w", err)
		}
	}

	return tx.Commit()
}

// RemoveLabelFromIssue detaches a label from an issue. Returns an error if the
// label is not found or is not attached to the issue. Activity is recorded and
// the issue's updated_at timestamp is touched.
func RemoveLabelFromIssue(db *sql.DB, issueID int, labelName string, author string) error {
	return RemoveLabelsFromIssue(db, issueID, []string{labelName}, author)
}

// RemoveLabelsFromIssue detaches multiple labels from an issue atomically
// within a single transaction. Returns an error if any label is not found or
// not attached — no labels are removed on failure. Activity is recorded for
// each removed label and the issue's updated_at timestamp is touched once.
func RemoveLabelsFromIssue(db *sql.DB, issueID int, labelNames []string, author string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Verify the issue exists.
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?)`, issueID).Scan(&exists); err != nil {
		return fmt.Errorf("checking issue existence: %w", err)
	}
	if !exists {
		return ErrNotFound
	}

	for _, labelName := range labelNames {
		// Find the label.
		var labelID int
		err = tx.QueryRow(`SELECT id FROM labels WHERE name = ?`, labelName).Scan(&labelID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("querying label: %w", err)
		}

		// Remove the link.
		res, err := tx.Exec(
			`DELETE FROM issue_labels WHERE issue_id = ? AND label_id = ?`,
			issueID, labelID,
		)
		if err != nil {
			return fmt.Errorf("removing label link: %w", err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking rows affected: %w", err)
		}
		if n == 0 {
			return ErrNotAttached
		}

		// Record activity.
		if err := RecordActivity(tx, issueID, "label_removed", labelName, "", author); err != nil {
			return err
		}
	}

	// Touch updated_at once.
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`UPDATE issues SET updated_at = ? WHERE id = ?`, now, issueID); err != nil {
		return fmt.Errorf("updating issue timestamp: %w", err)
	}

	return tx.Commit()
}

// GetIssueLabelObjects returns the full Label objects attached to an issue,
// sorted alphabetically by name.
func GetIssueLabelObjects(db *sql.DB, issueID int) ([]*model.Label, error) {
	rows, err := db.Query(
		`SELECT l.id, l.name, l.color FROM labels l
		 JOIN issue_labels il ON il.label_id = l.id
		 WHERE il.issue_id = ?
		 ORDER BY l.name`, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying issue labels: %w", err)
	}
	defer rows.Close()

	var labels []*model.Label
	for rows.Next() {
		var l model.Label
		var color sql.NullString
		if err := rows.Scan(&l.ID, &l.Name, &color); err != nil {
			return nil, fmt.Errorf("scanning label: %w", err)
		}
		l.Color = color.String
		labels = append(labels, &l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating label rows: %w", err)
	}

	return labels, nil
}
