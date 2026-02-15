package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Sentinel errors for relation operations.
var (
	ErrSelfRelation    = errors.New("self-referential relation")
	ErrDuplicateRelation = errors.New("duplicate relation")
	ErrCycleDetected   = errors.New("cycle detected")
)

// CycleError wraps ErrCycleDetected and carries the path of IDs forming the cycle.
type CycleError struct {
	Path []int
}

func (e *CycleError) Error() string {
	parts := make([]string, len(e.Path))
	for i, id := range e.Path {
		parts[i] = model.FormatID(id)
	}
	return fmt.Sprintf("Cannot link: %s would create a cycle", strings.Join(parts, " -> "))
}

func (e *CycleError) Unwrap() error { return ErrCycleDetected }

// CreateRelation inserts a new relation between two issues within a single
// transaction. It validates that both issues exist, rejects self-referential
// and duplicate relations, runs cycle detection for blocks/depends_on types,
// and records activity on both issues.
func CreateRelation(db *sql.DB, rel *model.Relation) (int, error) {
	// Reject self-referential relations before starting a transaction.
	if rel.SourceIssueID == rel.TargetIssueID {
		return 0, ErrSelfRelation
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Verify both issues exist.
	for _, issueID := range []int{rel.SourceIssueID, rel.TargetIssueID} {
		var exists bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?)", issueID).Scan(&exists); err != nil {
			return 0, fmt.Errorf("checking issue existence: %w", err)
		}
		if !exists {
			return 0, ErrNotFound
		}
	}

	// Check for duplicate relations including inverse duplicates.
	if err := checkDuplicateTx(tx, rel.SourceIssueID, rel.TargetIssueID, rel.RelationType); err != nil {
		return 0, err
	}

	// Cycle detection for directional relation types only. Symmetric types
	// (relates_to, duplicates) do not form DAGs, so cycles are meaningless.
	if rel.RelationType == model.RelationBlocks || rel.RelationType == model.RelationDependsOn {
		hasCycle, path, err := checkCycleTx(tx, rel.SourceIssueID, rel.TargetIssueID, string(rel.RelationType))
		if err != nil {
			return 0, fmt.Errorf("checking for cycles: %w", err)
		}
		if hasCycle {
			return 0, &CycleError{Path: path}
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	res, err := tx.Exec(
		`INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, ?, ?)`,
		rel.SourceIssueID,
		rel.TargetIssueID,
		string(rel.RelationType),
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting relation: %w", err)
	}

	id64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}

	// Record activity on the source issue.
	sourceActivity := fmt.Sprintf("%s %s", string(rel.RelationType), model.FormatID(rel.TargetIssueID))
	if err := RecordActivity(tx, rel.SourceIssueID, "relation_added", "", sourceActivity, ""); err != nil {
		return 0, err
	}

	// Record activity on the target issue with the inverse relation type.
	targetActivity := fmt.Sprintf("%s %s", rel.RelationType.Inverse(), model.FormatID(rel.SourceIssueID))
	if err := RecordActivity(tx, rel.TargetIssueID, "relation_added", "", targetActivity, ""); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return int(id64), nil
}

// DeleteRelation removes a relation matching the given source, target, and type.
// Activity is recorded on both issues within a single transaction.
func DeleteRelation(db *sql.DB, sourceID, targetID int, relType string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`DELETE FROM issue_relations WHERE source_issue_id = ? AND target_issue_id = ? AND relation_type = ?`,
		sourceID, targetID, relType,
	)
	if err != nil {
		return fmt.Errorf("deleting relation: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}

	rt := model.RelationType(relType)

	// Record activity on the source issue.
	sourceActivity := fmt.Sprintf("%s %s", relType, model.FormatID(targetID))
	if err := RecordActivity(tx, sourceID, "relation_removed", sourceActivity, "", ""); err != nil {
		return err
	}

	// Record activity on the target issue with the inverse relation type.
	targetActivity := fmt.Sprintf("%s %s", rt.Inverse(), model.FormatID(sourceID))
	if err := RecordActivity(tx, targetID, "relation_removed", targetActivity, "", ""); err != nil {
		return err
	}

	return tx.Commit()
}

// IssueExists returns true if an issue with the given ID exists.
func IssueExists(db *sql.DB, issueID int) (bool, error) {
	var exists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?)", issueID).Scan(&exists); err != nil {
		return false, fmt.Errorf("checking issue existence: %w", err)
	}
	return exists, nil
}

// GetIssueRelations returns all relations where the given issue is either the
// source or the target, ordered by creation time ascending.
func GetIssueRelations(db *sql.DB, issueID int) ([]model.Relation, error) {
	rows, err := db.Query(
		`SELECT id, source_issue_id, target_issue_id, relation_type, created_at
		 FROM issue_relations
		 WHERE source_issue_id = ? OR target_issue_id = ?
		 ORDER BY created_at ASC`,
		issueID, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying relations: %w", err)
	}
	defer rows.Close()

	var relations []model.Relation
	for rows.Next() {
		var r model.Relation
		var relType string
		var createdAt string
		if err := rows.Scan(&r.ID, &r.SourceIssueID, &r.TargetIssueID, &relType, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning relation row: %w", err)
		}
		r.RelationType = model.RelationType(relType)
		t, err := time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parsing created_at: %w", err)
		}
		r.CreatedAt = t
		relations = append(relations, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating relation rows: %w", err)
	}

	return relations, nil
}

// GetAllDirectionalRelations returns all relations where the relation type is
// "blocks" or "depends_on", ordered by creation time ascending.
func GetAllDirectionalRelations(db *sql.DB) ([]model.Relation, error) {
	rows, err := db.Query(
		`SELECT id, source_issue_id, target_issue_id, relation_type, created_at
		 FROM issue_relations
		 WHERE relation_type IN (?, ?)
		 ORDER BY created_at ASC`,
		string(model.RelationBlocks), string(model.RelationDependsOn),
	)
	if err != nil {
		return nil, fmt.Errorf("querying directional relations: %w", err)
	}
	defer rows.Close()

	var relations []model.Relation
	for rows.Next() {
		var r model.Relation
		var relType string
		var createdAt string
		if err := rows.Scan(&r.ID, &r.SourceIssueID, &r.TargetIssueID, &relType, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning relation row: %w", err)
		}
		r.RelationType = model.RelationType(relType)
		t, err := time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parsing created_at: %w", err)
		}
		r.CreatedAt = t
		relations = append(relations, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating relation rows: %w", err)
	}

	return relations, nil
}

// GetAllRelations returns every relation in the database, ordered by creation
// time ascending.
func GetAllRelations(db *sql.DB) ([]model.Relation, error) {
	rows, err := db.Query(
		`SELECT id, source_issue_id, target_issue_id, relation_type, created_at
		 FROM issue_relations
		 ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all relations: %w", err)
	}
	defer rows.Close()

	var relations []model.Relation
	for rows.Next() {
		var r model.Relation
		var relType string
		var createdAt string
		if err := rows.Scan(&r.ID, &r.SourceIssueID, &r.TargetIssueID, &relType, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning relation row: %w", err)
		}
		r.RelationType = model.RelationType(relType)
		t, err := time.Parse(time.RFC3339, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parsing created_at: %w", err)
		}
		r.CreatedAt = t
		relations = append(relations, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating relation rows: %w", err)
	}

	return relations, nil
}

// InsertRelationWithID inserts a relation with a specific ID (not auto-increment),
// skipping if the ID already exists. Returns true if the row was inserted.
// Must be called within an existing transaction.
func InsertRelationWithID(tx *sql.Tx, rel *model.Relation) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO issue_relations (id, source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		rel.ID,
		rel.SourceIssueID,
		rel.TargetIssueID,
		string(rel.RelationType),
		rel.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("inserting relation with id %d: %w", rel.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// checkDuplicateTx checks for existing relations that would conflict with a new
// relation between sourceID and targetID of the given type. For any relation
// type, both the exact direction and the reverse direction between the same pair
// are considered duplicates (e.g. "A blocks B" conflicts with "B blocks A").
//
// The schema enforces both levels: a UNIQUE constraint prevents exact-direction
// duplicates, and a BEFORE INSERT trigger (trg_no_inverse_duplicate_relation)
// rejects inverse pairs. This application-level check provides a friendlier
// error message and avoids relying solely on constraint violations.
func checkDuplicateTx(tx *sql.Tx, sourceID, targetID int, relType model.RelationType) error {
	var count int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM issue_relations
		 WHERE relation_type = ?
		   AND ((source_issue_id = ? AND target_issue_id = ?)
		     OR (source_issue_id = ? AND target_issue_id = ?))`,
		string(relType), sourceID, targetID, targetID, sourceID,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("checking duplicate relation: %w", err)
	}
	if count > 0 {
		return ErrDuplicateRelation
	}

	return nil
}

// checkCycleTx uses a recursive CTE to detect whether adding an edge from
// sourceID to targetID of the given relType would create a cycle. Only called
// for "blocks" and "depends_on" relation types.
//
// Note: cycle detection is scoped to a single relation type. Cross-type cycles
// (e.g. "A blocks B" + "B depends_on A") are not detected because blocks and
// depends_on represent independent DAGs.
//
// Starting from targetID the CTE follows outgoing edges of the same relation
// type. If sourceID is reachable, the proposed edge would close a cycle.
func checkCycleTx(tx *sql.Tx, sourceID, targetID int, relType string) (bool, []int, error) {
	rows, err := tx.Query(
		`WITH RECURSIVE reachable(id, path) AS (
			SELECT ?, CAST(? AS TEXT) || ',' || CAST(? AS TEXT)
			UNION ALL
			SELECT ir.target_issue_id,
			       r.path || ',' || CAST(ir.target_issue_id AS TEXT)
			FROM issue_relations ir
			JOIN reachable r ON ir.source_issue_id = r.id
			WHERE ir.relation_type = ?
		)
		SELECT path FROM reachable WHERE id = ? LIMIT 1`,
		targetID, sourceID, targetID,
		relType,
		sourceID,
	)
	if err != nil {
		return false, nil, fmt.Errorf("checking for cycles: %w", err)
	}
	defer rows.Close()

	if rows.Next() {
		var pathStr string
		if err := rows.Scan(&pathStr); err != nil {
			return false, nil, fmt.Errorf("scanning cycle path: %w", err)
		}

		// Parse the comma-separated path of IDs.
		parts := strings.Split(pathStr, ",")
		path := make([]int, 0, len(parts))
		for _, p := range parts {
			var id int
			if _, err := fmt.Sscanf(p, "%d", &id); err != nil {
				return false, nil, fmt.Errorf("parsing path element %q: %w", p, err)
			}
			path = append(path, id)
		}
		return true, path, nil
	}
	if err := rows.Err(); err != nil {
		return false, nil, fmt.Errorf("iterating cycle results: %w", err)
	}

	return false, nil, nil
}
