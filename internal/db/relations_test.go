package db

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// mustCreateIssue creates a minimal issue via the production CreateIssue path
// and returns its integer ID.
func mustCreateIssue(t *testing.T, d *sql.DB, title string) int {
	t.Helper()
	id, err := CreateIssue(d, &model.Issue{
		Title:    title,
		Status:   model.StatusBacklog,
		Priority: model.PriorityNone,
		Kind:     model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "creating issue %q: %v", title, err)
	return id
}

// mustCreateRelation is a test helper that creates a relation and fails the
// test if an error is returned.
func mustCreateRelation(t *testing.T, d *sql.DB, source, target int, rt model.RelationType) int {
	t.Helper()
	id, err := CreateRelation(d, &model.Relation{
		SourceIssueID: source,
		TargetIssueID: target,
		RelationType:  rt,
	})
	testsupport.Must(t, err, "creating relation %d->%d (%s): %v", source, target, rt, err)
	return id
}

func TestCreateRelation(t *testing.T) {
	d := mustOpen(t)
	err := Initialize(d)
	testsupport.Must(t, err, "Initialize: %v", err)

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	id, err := CreateRelation(d, &model.Relation{
		SourceIssueID: a,
		TargetIssueID: b,
		RelationType:  model.RelationBlocks,
	})
	testsupport.Must(t, err, "CreateRelation: %v", err)
	if id <= 0 {
		t.Errorf("expected positive ID, got %d", id)
	}

	// Verify the row directly.
	var sourceID, targetID int
	var relType string
	err = d.QueryRow(
		`SELECT source_issue_id, target_issue_id, relation_type FROM issue_relations WHERE id = ?`, id,
	).Scan(&sourceID, &targetID, &relType)
	testsupport.Must(t, err, "querying relation row: %v", err)
	if sourceID != a {
		t.Errorf("source_issue_id = %d, want %d", sourceID, a)
	}
	if targetID != b {
		t.Errorf("target_issue_id = %d, want %d", targetID, b)
	}
	if relType != string(model.RelationBlocks) {
		t.Errorf("relation_type = %q, want %q", relType, model.RelationBlocks)
	}
}

func TestDeleteRelation(t *testing.T) {
	d := mustOpen(t)
	err := Initialize(d)
	testsupport.Must(t, err, "Initialize: %v", err)

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationBlocks)

	err = DeleteRelation(d, a, b, string(model.RelationBlocks))
	testsupport.Must(t, err, "DeleteRelation: %v", err)

	// Verify it is gone.
	var count int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM issue_relations WHERE source_issue_id = ? AND target_issue_id = ?`, a, b,
	).Scan(&count); err != nil {
		t.Fatalf("counting relations: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 relations after delete, got %d", count)
	}
}

func TestCreateRelationRecordsActivity(t *testing.T) {
	d := mustOpen(t)
	err := Initialize(d)
	testsupport.Must(t, err, "Initialize: %v", err)

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationBlocks)

	// Check activity on issue A (source).
	var countA int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM activity_log WHERE issue_id = ? AND field_changed = 'relation_added'`, a,
	).Scan(&countA); err != nil {
		t.Fatalf("querying activity for issue A: %v", err)
	}
	if countA != 1 {
		t.Errorf("expected 1 relation_added activity on issue A, got %d", countA)
	}

	// Check activity on issue B (target).
	var countB int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM activity_log WHERE issue_id = ? AND field_changed = 'relation_added'`, b,
	).Scan(&countB); err != nil {
		t.Fatalf("querying activity for issue B: %v", err)
	}
	if countB != 1 {
		t.Errorf("expected 1 relation_added activity on issue B, got %d", countB)
	}
}

func TestDeleteRelationRecordsActivity(t *testing.T) {
	d := mustOpen(t)
	err := Initialize(d)
	testsupport.Must(t, err, "Initialize: %v", err)

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationBlocks)

	err = DeleteRelation(d, a, b, string(model.RelationBlocks))
	testsupport.Must(t, err, "DeleteRelation: %v", err)

	// Check relation_removed activity on issue A (source).
	var countA int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM activity_log WHERE issue_id = ? AND field_changed = 'relation_removed'`, a,
	).Scan(&countA); err != nil {
		t.Fatalf("querying activity for issue A: %v", err)
	}
	if countA != 1 {
		t.Errorf("expected 1 relation_removed activity on issue A, got %d", countA)
	}

	// Check relation_removed activity on issue B (target).
	var countB int
	if err := d.QueryRow(
		`SELECT COUNT(*) FROM activity_log WHERE issue_id = ? AND field_changed = 'relation_removed'`, b,
	).Scan(&countB); err != nil {
		t.Fatalf("querying activity for issue B: %v", err)
	}
	if countB != 1 {
		t.Errorf("expected 1 relation_removed activity on issue B, got %d", countB)
	}
}
