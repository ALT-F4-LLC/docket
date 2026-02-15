package db

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
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
	}, nil)
	if err != nil {
		t.Fatalf("creating issue %q: %v", title, err)
	}
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
	if err != nil {
		t.Fatalf("creating relation %d->%d (%s): %v", source, target, rt, err)
	}
	return id
}

func TestCreateRelation(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	id, err := CreateRelation(d, &model.Relation{
		SourceIssueID: a,
		TargetIssueID: b,
		RelationType:  model.RelationBlocks,
	})
	if err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive ID, got %d", id)
	}

	// Verify the row directly.
	var sourceID, targetID int
	var relType string
	err = d.QueryRow(
		`SELECT source_issue_id, target_issue_id, relation_type FROM issue_relations WHERE id = ?`, id,
	).Scan(&sourceID, &targetID, &relType)
	if err != nil {
		t.Fatalf("querying relation row: %v", err)
	}
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

func TestCreateRelationSelfReferential(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")

	_, err := CreateRelation(d, &model.Relation{
		SourceIssueID: a,
		TargetIssueID: a,
		RelationType:  model.RelationBlocks,
	})
	if !errors.Is(err, ErrSelfRelation) {
		t.Errorf("expected ErrSelfRelation, got %v", err)
	}
}

func TestCreateRelationDuplicate(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationBlocks)

	_, err := CreateRelation(d, &model.Relation{
		SourceIssueID: a,
		TargetIssueID: b,
		RelationType:  model.RelationBlocks,
	})
	if !errors.Is(err, ErrDuplicateRelation) {
		t.Errorf("expected ErrDuplicateRelation, got %v", err)
	}
}

func TestCreateRelationInverseDuplicate(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationBlocks)

	_, err := CreateRelation(d, &model.Relation{
		SourceIssueID: b,
		TargetIssueID: a,
		RelationType:  model.RelationBlocks,
	})
	if !errors.Is(err, ErrDuplicateRelation) {
		t.Errorf("expected ErrDuplicateRelation, got %v", err)
	}
}

func TestCreateRelationDependsOnInverseDuplicate(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationDependsOn)

	_, err := CreateRelation(d, &model.Relation{
		SourceIssueID: b,
		TargetIssueID: a,
		RelationType:  model.RelationDependsOn,
	})
	if !errors.Is(err, ErrDuplicateRelation) {
		t.Errorf("expected ErrDuplicateRelation, got %v", err)
	}
}

func TestCreateRelationRelatesToBothDirections(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationRelatesTo)

	_, err := CreateRelation(d, &model.Relation{
		SourceIssueID: b,
		TargetIssueID: a,
		RelationType:  model.RelationRelatesTo,
	})
	if !errors.Is(err, ErrDuplicateRelation) {
		t.Errorf("expected ErrDuplicateRelation for symmetric relation, got %v", err)
	}
}

func TestCreateRelationDifferentTypesSamePair(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationBlocks)

	// A different relation type on the same pair should succeed.
	id, err := CreateRelation(d, &model.Relation{
		SourceIssueID: a,
		TargetIssueID: b,
		RelationType:  model.RelationRelatesTo,
	})
	if err != nil {
		t.Fatalf("expected success for different type on same pair, got %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive ID, got %d", id)
	}
}

func TestCreateRelationIssueNotFound(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")

	// Target does not exist.
	_, err := CreateRelation(d, &model.Relation{
		SourceIssueID: a,
		TargetIssueID: 9999,
		RelationType:  model.RelationBlocks,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing target, got %v", err)
	}

	// Source does not exist.
	_, err = CreateRelation(d, &model.Relation{
		SourceIssueID: 9999,
		TargetIssueID: a,
		RelationType:  model.RelationBlocks,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing source, got %v", err)
	}
}

func TestCycleDetection(t *testing.T) {
	type edge struct {
		source string
		target string
	}

	tests := []struct {
		name       string
		setupEdges []edge
		attempt    edge
		wantErr    error // nil means success expected
	}{
		{
			name:       "direct_cycle",
			setupEdges: []edge{{"A", "B"}},
			attempt:    edge{"B", "A"},
			// Caught by inverse duplicate check before cycle detection.
			wantErr: ErrDuplicateRelation,
		},
		{
			name:       "three_node_cycle",
			setupEdges: []edge{{"A", "B"}, {"B", "C"}},
			attempt:    edge{"C", "A"},
			wantErr:    ErrCycleDetected,
		},
		{
			name:       "four_node_cycle",
			setupEdges: []edge{{"A", "B"}, {"B", "C"}, {"C", "D"}},
			attempt:    edge{"D", "A"},
			wantErr:    ErrCycleDetected,
		},
		{
			name:       "no_cycle_separate_chains",
			setupEdges: []edge{{"A", "B"}, {"C", "D"}},
			attempt:    edge{"D", "A"},
		},
		{
			name:       "no_cycle_converging",
			setupEdges: []edge{{"A", "C"}, {"B", "C"}},
			attempt:    edge{"C", "D"},
		},
		{
			name:       "diamond_no_cycle",
			setupEdges: []edge{{"A", "B"}, {"A", "C"}, {"B", "D"}, {"C", "D"}},
			attempt:    edge{"D", "E"},
		},
		{
			name:       "diamond_with_cycle",
			setupEdges: []edge{{"A", "B"}, {"A", "C"}, {"B", "D"}, {"C", "D"}},
			attempt:    edge{"D", "A"},
			wantErr:    ErrCycleDetected,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mustOpen(t)
			if err := Initialize(d); err != nil {
				t.Fatalf("Initialize: %v", err)
			}

			// Create issues for all unique node names.
			nodeNames := map[string]bool{}
			for _, e := range tt.setupEdges {
				nodeNames[e.source] = true
				nodeNames[e.target] = true
			}
			nodeNames[tt.attempt.source] = true
			nodeNames[tt.attempt.target] = true

			ids := map[string]int{}
			for name := range nodeNames {
				ids[name] = mustCreateIssue(t, d, "issue "+name)
			}

			// Set up existing edges.
			for _, e := range tt.setupEdges {
				mustCreateRelation(t, d, ids[e.source], ids[e.target], model.RelationBlocks)
			}

			// Attempt the new relation.
			_, err := CreateRelation(d, &model.Relation{
				SourceIssueID: ids[tt.attempt.source],
				TargetIssueID: ids[tt.attempt.target],
				RelationType:  model.RelationBlocks,
			})

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("expected %v, got %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

func TestCycleDetectionDependsOn(t *testing.T) {
	tests := []struct {
		name    string
		chain   int   // length of chain A->B->C->...
		wantErr error // nil means success expected
	}{
		{name: "three_node_cycle", chain: 3, wantErr: ErrCycleDetected},
		{name: "four_node_cycle", chain: 4, wantErr: ErrCycleDetected},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := mustOpen(t)
			if err := Initialize(d); err != nil {
				t.Fatalf("Initialize: %v", err)
			}

			// Create chain of issues: 0 -> 1 -> 2 -> ... -> (chain-1)
			ids := make([]int, tt.chain)
			for i := range ids {
				ids[i] = mustCreateIssue(t, d, "issue")
			}

			// Create depends_on edges along the chain.
			for i := 0; i < tt.chain-1; i++ {
				mustCreateRelation(t, d, ids[i], ids[i+1], model.RelationDependsOn)
			}

			// Attempt to close the cycle: last -> first.
			_, err := CreateRelation(d, &model.Relation{
				SourceIssueID: ids[tt.chain-1],
				TargetIssueID: ids[0],
				RelationType:  model.RelationDependsOn,
			})

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("expected %v, got %v", tt.wantErr, err)
				}
			} else if err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

func TestDeleteRelation(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationBlocks)

	if err := DeleteRelation(d, a, b, string(model.RelationBlocks)); err != nil {
		t.Fatalf("DeleteRelation: %v", err)
	}

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

func TestDeleteRelationNotFound(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	err := DeleteRelation(d, 999, 888, string(model.RelationBlocks))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGetIssueRelations(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")
	c := mustCreateIssue(t, d, "issue C")
	dd := mustCreateIssue(t, d, "issue D")

	// A blocks B
	mustCreateRelation(t, d, a, b, model.RelationBlocks)
	// C depends_on A
	mustCreateRelation(t, d, c, a, model.RelationDependsOn)
	// A relates_to D
	mustCreateRelation(t, d, a, dd, model.RelationRelatesTo)

	relations, err := GetIssueRelations(d, a)
	if err != nil {
		t.Fatalf("GetIssueRelations: %v", err)
	}

	if len(relations) != 3 {
		t.Fatalf("expected 3 relations, got %d", len(relations))
	}

	// Verify each relation has correct fields. Order is by created_at ASC.
	// Relation 0: A blocks B
	if relations[0].SourceIssueID != a || relations[0].TargetIssueID != b {
		t.Errorf("relation[0]: source=%d target=%d, want source=%d target=%d",
			relations[0].SourceIssueID, relations[0].TargetIssueID, a, b)
	}
	if relations[0].RelationType != model.RelationBlocks {
		t.Errorf("relation[0]: type=%q, want %q", relations[0].RelationType, model.RelationBlocks)
	}

	// Relation 1: C depends_on A
	if relations[1].SourceIssueID != c || relations[1].TargetIssueID != a {
		t.Errorf("relation[1]: source=%d target=%d, want source=%d target=%d",
			relations[1].SourceIssueID, relations[1].TargetIssueID, c, a)
	}
	if relations[1].RelationType != model.RelationDependsOn {
		t.Errorf("relation[1]: type=%q, want %q", relations[1].RelationType, model.RelationDependsOn)
	}

	// Relation 2: A relates_to D
	if relations[2].SourceIssueID != a || relations[2].TargetIssueID != dd {
		t.Errorf("relation[2]: source=%d target=%d, want source=%d target=%d",
			relations[2].SourceIssueID, relations[2].TargetIssueID, a, dd)
	}
	if relations[2].RelationType != model.RelationRelatesTo {
		t.Errorf("relation[2]: type=%q, want %q", relations[2].RelationType, model.RelationRelatesTo)
	}
}

func TestGetIssueRelationsEmpty(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "lonely issue")

	relations, err := GetIssueRelations(d, a)
	if err != nil {
		t.Fatalf("GetIssueRelations: %v", err)
	}
	if len(relations) != 0 {
		t.Errorf("expected 0 relations, got %d", len(relations))
	}
}

func TestCreateRelationRecordsActivity(t *testing.T) {
	d := mustOpen(t)
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

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
	if err := Initialize(d); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a := mustCreateIssue(t, d, "issue A")
	b := mustCreateIssue(t, d, "issue B")

	mustCreateRelation(t, d, a, b, model.RelationBlocks)

	if err := DeleteRelation(d, a, b, string(model.RelationBlocks)); err != nil {
		t.Fatalf("DeleteRelation: %v", err)
	}

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
