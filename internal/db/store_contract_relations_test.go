package db_test

import (
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// relationContract pins the issue-relation graph: which relations
// CreateRelation refuses, which it accepts, and the order the readers return.
var relationContract = []contractCase{
	{"RelationCreateSelfReferential", contractRelationCreateSelfReferential},
	{"RelationCreateDuplicate", contractRelationCreateDuplicate},
	{"RelationCreateInverseDuplicate", contractRelationCreateInverseDuplicate},
	{"RelationCreateDependsOnInverseDuplicate", contractRelationCreateDependsOnInverseDuplicate},
	{"RelationCreateRelatesToBothDirections", contractRelationCreateRelatesToBothDirections},
	{"RelationCreateDifferentTypesSamePair", contractRelationCreateDifferentTypesSamePair},
	{"RelationCreateIssueNotFound", contractRelationCreateIssueNotFound},
	{"RelationCycleDetection", contractRelationCycleDetection},
	{"RelationCycleDetectionDependsOn", contractRelationCycleDetectionDependsOn},
	{"RelationDeleteNotFound", contractRelationDeleteNotFound},
	{"RelationGetIssueRelations", contractRelationGetIssueRelations},
	{"RelationGetIssueRelationsEmpty", contractRelationGetIssueRelationsEmpty},
	{"RelationGetAllRelationsIsTotallyOrdered", contractRelationGetAllRelationsIsTotallyOrdered},
	{"RelationGetIssueRelationsIsTotallyOrdered", contractRelationGetIssueRelationsIsTotallyOrdered},
}

// issueContract pins the issue listing's zero-result shape.
var issueContract = []contractCase{
	{"ListIssuesEmptyIsNotNil", contractListIssuesEmptyIsNotNil},
}

// createContractRelation creates a relation and returns its id.
func createContractRelation(t *testing.T, st store.Store, source, target int, rt model.RelationType) int {
	t.Helper()
	id, err := st.CreateRelation(&model.Relation{
		SourceIssueID: source,
		TargetIssueID: target,
		RelationType:  rt,
	})
	testsupport.Must(t, err, "creating relation %d->%d (%s): %v", source, target, rt, err)
	return id
}

// attemptContractRelation tries to create a relation and returns only the error.
func attemptContractRelation(st store.Store, source, target int, rt model.RelationType) error {
	_, err := st.CreateRelation(&model.Relation{
		SourceIssueID: source,
		TargetIssueID: target,
		RelationType:  rt,
	})
	return err
}

func contractRelationCreateSelfReferential(t *testing.T, st store.Store) {
	a := createContractIssue(t, st, "issue A")

	if err := attemptContractRelation(st, a, a, model.RelationBlocks); !errors.Is(err, db.ErrSelfRelation) {
		t.Errorf("expected ErrSelfRelation, got %v", err)
	}
}

func contractRelationCreateDuplicate(t *testing.T, st store.Store) {
	a := createContractIssue(t, st, "issue A")
	b := createContractIssue(t, st, "issue B")
	createContractRelation(t, st, a, b, model.RelationBlocks)

	if err := attemptContractRelation(st, a, b, model.RelationBlocks); !errors.Is(err, db.ErrDuplicateRelation) {
		t.Errorf("expected ErrDuplicateRelation, got %v", err)
	}
}

func contractRelationCreateInverseDuplicate(t *testing.T, st store.Store) {
	a := createContractIssue(t, st, "issue A")
	b := createContractIssue(t, st, "issue B")
	createContractRelation(t, st, a, b, model.RelationBlocks)

	if err := attemptContractRelation(st, b, a, model.RelationBlocks); !errors.Is(err, db.ErrDuplicateRelation) {
		t.Errorf("expected ErrDuplicateRelation, got %v", err)
	}
}

func contractRelationCreateDependsOnInverseDuplicate(t *testing.T, st store.Store) {
	a := createContractIssue(t, st, "issue A")
	b := createContractIssue(t, st, "issue B")
	createContractRelation(t, st, a, b, model.RelationDependsOn)

	if err := attemptContractRelation(st, b, a, model.RelationDependsOn); !errors.Is(err, db.ErrDuplicateRelation) {
		t.Errorf("expected ErrDuplicateRelation, got %v", err)
	}
}

func contractRelationCreateRelatesToBothDirections(t *testing.T, st store.Store) {
	a := createContractIssue(t, st, "issue A")
	b := createContractIssue(t, st, "issue B")
	createContractRelation(t, st, a, b, model.RelationRelatesTo)

	if err := attemptContractRelation(st, b, a, model.RelationRelatesTo); !errors.Is(err, db.ErrDuplicateRelation) {
		t.Errorf("expected ErrDuplicateRelation for symmetric relation, got %v", err)
	}
}

func contractRelationCreateDifferentTypesSamePair(t *testing.T, st store.Store) {
	a := createContractIssue(t, st, "issue A")
	b := createContractIssue(t, st, "issue B")
	createContractRelation(t, st, a, b, model.RelationBlocks)

	id, err := st.CreateRelation(&model.Relation{
		SourceIssueID: a,
		TargetIssueID: b,
		RelationType:  model.RelationRelatesTo,
	})
	testsupport.Must(t, err, "expected success for different type on same pair, got %v", err)
	if id <= 0 {
		t.Errorf("expected positive ID, got %d", id)
	}
}

func contractRelationCreateIssueNotFound(t *testing.T, st store.Store) {
	a := createContractIssue(t, st, "issue A")

	if err := attemptContractRelation(st, a, 9999, model.RelationBlocks); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing target, got %v", err)
	}
	if err := attemptContractRelation(st, 9999, a, model.RelationBlocks); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing source, got %v", err)
	}
}

// contractRelationCycleDetection builds each graph from issues of its own, so
// the subcases share the store without sharing an edge.
func contractRelationCycleDetection(t *testing.T, st store.Store) {
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
			wantErr: db.ErrDuplicateRelation,
		},
		{
			name:       "three_node_cycle",
			setupEdges: []edge{{"A", "B"}, {"B", "C"}},
			attempt:    edge{"C", "A"},
			wantErr:    db.ErrCycleDetected,
		},
		{
			name:       "four_node_cycle",
			setupEdges: []edge{{"A", "B"}, {"B", "C"}, {"C", "D"}},
			attempt:    edge{"D", "A"},
			wantErr:    db.ErrCycleDetected,
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
			wantErr:    db.ErrCycleDetected,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids := map[string]int{}
			node := func(name string) int {
				if _, ok := ids[name]; !ok {
					ids[name] = createContractIssue(t, st, tt.name+" issue "+name)
				}
				return ids[name]
			}

			for _, e := range tt.setupEdges {
				createContractRelation(t, st, node(e.source), node(e.target), model.RelationBlocks)
			}

			err := attemptContractRelation(st, node(tt.attempt.source), node(tt.attempt.target), model.RelationBlocks)
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

func contractRelationCycleDetectionDependsOn(t *testing.T, st store.Store) {
	tests := []struct {
		name  string
		chain int // length of chain A->B->C->...
	}{
		{name: "three_node_cycle", chain: 3},
		{name: "four_node_cycle", chain: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids := make([]int, tt.chain)
			for i := range ids {
				ids[i] = createContractIssue(t, st, tt.name+" issue")
			}
			for i := 0; i < tt.chain-1; i++ {
				createContractRelation(t, st, ids[i], ids[i+1], model.RelationDependsOn)
			}

			// Closing the chain, last -> first, would make a cycle.
			err := attemptContractRelation(st, ids[tt.chain-1], ids[0], model.RelationDependsOn)
			if !errors.Is(err, db.ErrCycleDetected) {
				t.Errorf("expected %v, got %v", db.ErrCycleDetected, err)
			}
		})
	}
}

func contractRelationDeleteNotFound(t *testing.T, st store.Store) {
	err := st.DeleteRelation(999, 888, string(model.RelationBlocks))
	if !errors.Is(err, db.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func contractRelationGetIssueRelations(t *testing.T, st store.Store) {
	a := createContractIssue(t, st, "issue A")
	b := createContractIssue(t, st, "issue B")
	c := createContractIssue(t, st, "issue C")
	d := createContractIssue(t, st, "issue D")

	createContractRelation(t, st, a, b, model.RelationBlocks)
	createContractRelation(t, st, c, a, model.RelationDependsOn)
	createContractRelation(t, st, a, d, model.RelationRelatesTo)

	relations, err := st.GetIssueRelations(a)
	testsupport.Must(t, err, "GetIssueRelations: %v", err)

	// Creation order: A blocks B, C depends_on A, A relates_to D.
	want := []struct {
		source, target int
		rt             model.RelationType
	}{
		{a, b, model.RelationBlocks},
		{c, a, model.RelationDependsOn},
		{a, d, model.RelationRelatesTo},
	}
	if len(relations) != len(want) {
		t.Fatalf("expected %d relations, got %d", len(want), len(relations))
	}
	for i, w := range want {
		got := relations[i]
		if got.SourceIssueID != w.source || got.TargetIssueID != w.target {
			t.Errorf("relation[%d]: source=%d target=%d, want source=%d target=%d",
				i, got.SourceIssueID, got.TargetIssueID, w.source, w.target)
		}
		if got.RelationType != w.rt {
			t.Errorf("relation[%d]: type=%q, want %q", i, got.RelationType, w.rt)
		}
	}
}

func contractRelationGetIssueRelationsEmpty(t *testing.T, st store.Store) {
	a := createContractIssue(t, st, "lonely issue")

	relations, err := st.GetIssueRelations(a)
	testsupport.Must(t, err, "GetIssueRelations: %v", err)
	if len(relations) != 0 {
		t.Errorf("expected 0 relations, got %d", len(relations))
	}
}

// contractRelationGetAllRelationsIsTotallyOrdered is DKT-330.
//
// `ORDER BY created_at ASC` with no tiebreak, over a column written at RFC3339
// SECOND resolution, left relations created inside one second with no defined
// relative order — so `docket export` run twice against an unchanged database
// could emit the array two different ways and a manifest diff would carry
// noise that is not a change.
//
// Every relation here is created in the same second, which is the case the old
// query could not order. The assertion is that the order is INSERTION order
// and that repeating the read reproduces it exactly.
func contractRelationGetAllRelationsIsTotallyOrdered(t *testing.T, st store.Store) {
	issues := make([]int, 0, 8)
	for i := range 8 {
		issues = append(issues, createContractIssue(t, st, "issue "+string(rune('a'+i))))
	}
	// A chain, so every relation is a distinct pair and none trips the
	// duplicate or inverse-duplicate guards.
	want := make([]int, 0, 7)
	for i := range 7 {
		want = append(want, createContractRelation(t, st, issues[i], issues[i+1], model.RelationBlocks))
	}

	for round := range 8 {
		got, err := st.GetAllRelations(0)
		testsupport.Must(t, err, "GetAllRelations: %v", err)
		if len(got) != len(want) {
			t.Fatalf("round %d: %d relations, want %d", round, len(got), len(want))
		}
		for i, id := range want {
			if got[i].ID != id {
				t.Fatalf("round %d, position %d: relation %d, want %d — the "+
					"export order must be the same on every read of an "+
					"unchanged database", round, i, got[i].ID, id)
			}
		}
	}
}

// contractRelationGetIssueRelationsIsTotallyOrdered covers the per-issue
// reader, which carried the same untiebroken sort.
func contractRelationGetIssueRelationsIsTotallyOrdered(t *testing.T, st store.Store) {
	hub := createContractIssue(t, st, "hub")
	var want []int
	for i := range 6 {
		other := createContractIssue(t, st, "spoke "+string(rune('a'+i)))
		want = append(want, createContractRelation(t, st, hub, other, model.RelationBlocks))
	}

	for round := range 4 {
		got, err := st.GetIssueRelations(hub)
		testsupport.Must(t, err, "GetIssueRelations: %v", err)
		if len(got) != len(want) {
			t.Fatalf("round %d: %d relations, want %d", round, len(got), len(want))
		}
		for i, id := range want {
			if got[i].ID != id {
				t.Fatalf("round %d, position %d: relation %d, want %d",
					round, i, got[i].ID, id)
			}
		}
	}
}

// contractListIssuesEmptyIsNotNil pins ListIssues' zero-result contract: a
// store with no issues returns a non-nil empty slice, not nil.
func contractListIssuesEmptyIsNotNil(t *testing.T, st store.Store) {
	issues, total, err := st.ListIssues(db.ListOptions{})
	testsupport.Must(t, err, "ListIssues: %v", err)
	if total != 0 {
		t.Fatalf("total = %d, want 0", total)
	}
	if issues == nil {
		t.Fatalf("ListIssues returned nil, want a non-nil empty slice")
	}
	if len(issues) != 0 {
		t.Fatalf("len(issues) = %d, want 0", len(issues))
	}
}
