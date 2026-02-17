package db

import (
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// AttachFiles inserts rows into issue_files for each file path. Duplicate
// attachments are silently ignored (INSERT OR IGNORE). Activity is recorded
// for each batch of newly attached files.
func AttachFiles(db *sql.DB, issueID int, filePaths []string, changedBy string) error {
	if len(filePaths) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	var added []string
	for _, fp := range filePaths {
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO issue_files (issue_id, file_path) VALUES (?, ?)`,
			issueID, fp,
		)
		if err != nil {
			return fmt.Errorf("attaching file %q: %w", fp, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			added = append(added, fp)
		}
	}

	if len(added) > 0 {
		if err := RecordActivity(tx, issueID, "files", "", strings.Join(added, ", "), changedBy); err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.Exec(`UPDATE issues SET updated_at = ? WHERE id = ?`, now, issueID); err != nil {
			return fmt.Errorf("updating issue timestamp: %w", err)
		}
	}

	return tx.Commit()
}

// DetachFiles deletes rows from issue_files matching the issue ID and file
// paths. Activity is recorded for removed files.
func DetachFiles(db *sql.DB, issueID int, filePaths []string, changedBy string) error {
	if len(filePaths) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	var removed []string
	for _, fp := range filePaths {
		res, err := tx.Exec(
			`DELETE FROM issue_files WHERE issue_id = ? AND file_path = ?`,
			issueID, fp,
		)
		if err != nil {
			return fmt.Errorf("detaching file %q: %w", fp, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			removed = append(removed, fp)
		}
	}

	if len(removed) > 0 {
		if err := RecordActivity(tx, issueID, "files", strings.Join(removed, ", "), "", changedBy); err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.Exec(`UPDATE issues SET updated_at = ? WHERE id = ?`, now, issueID); err != nil {
			return fmt.Errorf("updating issue timestamp: %w", err)
		}
	}

	return tx.Commit()
}

// GetIssueFiles returns the file paths attached to an issue, sorted alphabetically.
func GetIssueFiles(db *sql.DB, issueID int) ([]string, error) {
	rows, err := db.Query(
		`SELECT file_path FROM issue_files WHERE issue_id = ? ORDER BY file_path`,
		issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying issue files: %w", err)
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scanning file path: %w", err)
		}
		files = append(files, fp)
	}
	return files, rows.Err()
}

// SetIssueFiles replaces all files for an issue (delete existing, insert new).
// Activity is recorded showing the change from old files to new files.
func SetIssueFiles(db *sql.DB, issueID int, filePaths []string, changedBy string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Get old files for activity logging.
	oldFiles, err := queryFilePaths(tx, issueID)
	if err != nil {
		return err
	}

	// Delete all existing files.
	if _, err := tx.Exec(`DELETE FROM issue_files WHERE issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("clearing issue files: %w", err)
	}

	// Insert new files (clone to avoid mutating the caller's slice).
	sorted := slices.Clone(filePaths)
	sort.Strings(sorted)
	for _, fp := range sorted {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO issue_files (issue_id, file_path) VALUES (?, ?)`,
			issueID, fp,
		); err != nil {
			return fmt.Errorf("inserting file %q: %w", fp, err)
		}
	}

	// Record activity if files changed.
	oldStr := strings.Join(oldFiles, ", ")
	newStr := strings.Join(sorted, ", ")
	if oldStr != newStr {
		if err := RecordActivity(tx, issueID, "files", oldStr, newStr, changedBy); err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.Exec(`UPDATE issues SET updated_at = ? WHERE id = ?`, now, issueID); err != nil {
			return fmt.Errorf("updating issue timestamp: %w", err)
		}
	}

	return tx.Commit()
}

// HydrateFiles bulk-loads files for a set of issues, populating each issue's
// Files field. This avoids N+1 queries in list views and the planner.
func HydrateFiles(db *sql.DB, issues []*model.Issue) error {
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
		`SELECT issue_id, file_path FROM issue_files
		 WHERE issue_id IN (%s)
		 ORDER BY file_path`, placeholders,
	)

	rows, err := db.Query(query, ids...)
	if err != nil {
		return fmt.Errorf("querying files: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var issueID int
		var filePath string
		if err := rows.Scan(&issueID, &filePath); err != nil {
			return fmt.Errorf("scanning file: %w", err)
		}
		if issue, ok := issueMap[issueID]; ok {
			issue.Files = append(issue.Files, filePath)
		}
	}
	return rows.Err()
}

// ListAllIssueFileMappings returns all rows from issue_files as
// IssueFileMapping structs. This is needed by the export command.
func ListAllIssueFileMappings(db *sql.DB) ([]model.IssueFileMapping, error) {
	rows, err := db.Query(
		`SELECT issue_id, file_path FROM issue_files ORDER BY issue_id, file_path`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying issue-file mappings: %w", err)
	}
	defer rows.Close()

	var mappings []model.IssueFileMapping
	for rows.Next() {
		var m model.IssueFileMapping
		if err := rows.Scan(&m.IssueID, &m.FilePath); err != nil {
			return nil, fmt.Errorf("scanning issue-file mapping: %w", err)
		}
		mappings = append(mappings, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating issue-file mappings: %w", err)
	}

	return mappings, nil
}

// queryFilePaths returns file paths for an issue within a transaction.
func queryFilePaths(tx *sql.Tx, issueID int) ([]string, error) {
	rows, err := tx.Query(
		`SELECT file_path FROM issue_files WHERE issue_id = ? ORDER BY file_path`,
		issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying existing files: %w", err)
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scanning file path: %w", err)
		}
		files = append(files, fp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating file rows: %w", err)
	}
	return files, nil
}

// InsertIssueFileMapping inserts a single file mapping using INSERT OR IGNORE.
// Returns true if inserted, false if already existed. Must be called within
// an existing transaction.
func InsertIssueFileMapping(tx *sql.Tx, issueID int, filePath string) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO issue_files (issue_id, file_path) VALUES (?, ?)`,
		issueID, filePath,
	)
	if err != nil {
		return false, fmt.Errorf("inserting issue-file mapping (issue=%d, file=%q): %w", issueID, filePath, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
