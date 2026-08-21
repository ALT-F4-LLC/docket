package db

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// v14 — the per-seat vote-spend ledger (DKT-95). Three obligations: the
// table arrives on a migrated store, the rewind guard converges a store
// stamped 14 without the v14 shape, and the cast path writes one row per
// seat per unit where usage_ledger's key could not.

func TestMigrateToV14(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", v, currentSchemaVersion)
	}
	for _, table := range v14Sentinels {
		exists, err := tableExists(db, table)
		if err != nil || !exists {
			t.Errorf("%s missing after migration (err %v)", table, err)
		}
	}
}

// TestV14RewindGuardConvergesAStampedStore drops the table while leaving the
// stamp — the mid-change-binary database — and asserts Migrate converges it.
func TestV14RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := db.Exec(`DROP TABLE vote_usage`); err != nil {
		t.Fatalf("dropping vote_usage: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	exists, err := tableExists(db, "vote_usage")
	if err != nil || !exists {
		t.Fatalf("the rewind guard did not converge vote_usage back (err %v)", err)
	}
}

// TestCastVoteRecordsPerSeatUsage pins DKT-95's key decision: two seats on
// ONE proposal each land their own usage rows — the collision that
// usage_ledger's UNIQUE(step_id, attempt, unit) made impossible for a vote
// step whose attempt is permanently 0 — and the rows ride the cast's own
// transaction.
func TestCastVoteRecordsPerSeatUsage(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	proposalID, err := CreateProposal(db, &model.Proposal{
		Description:    "spend per seat",
		RequiredVoters: 3,
		Threshold:      0.5,
		Status:         model.ProposalStatusOpen,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	for _, seat := range []string{"seat-a", "seat-b"} {
		_, err := CastVote(db, &model.Vote{
			ProposalID:      proposalID,
			VoterName:       seat,
			Verdict:         model.VerdictApprove,
			Confidence:      0.9,
			DomainRelevance: 0.8,
			Usage:           map[string]float64{"tokens": 100, "seconds": 7.5},
		})
		if err != nil {
			t.Fatalf("CastVote(%s): %v — two seats' spend must not collide", seat, err)
		}
	}

	var rows int
	err = db.QueryRow(
		`SELECT COUNT(*) FROM vote_usage vu JOIN votes v ON v.id = vu.vote_id
		  WHERE v.proposal_id = ?`, proposalID).Scan(&rows)
	if err != nil {
		t.Fatalf("counting vote_usage rows: %v", err)
	}
	if rows != 4 {
		t.Errorf("vote_usage rows = %d, want 4 (2 seats x 2 units)", rows)
	}

	var tokens float64
	err = db.QueryRow(
		`SELECT SUM(vu.quantity) FROM vote_usage vu JOIN votes v ON v.id = vu.vote_id
		  WHERE v.proposal_id = ? AND vu.unit = 'tokens'`, proposalID).Scan(&tokens)
	if err != nil || tokens != 200 {
		t.Errorf("summed tokens = %g (err %v), want 200 across the seats", tokens, err)
	}

	// The numeric rules hold at the write: a negative quantity refuses the
	// whole cast, vote included — the pair lands together or not at all.
	_, err = CastVote(db, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "seat-c",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
		Usage:           map[string]float64{"tokens": -1},
	})
	if err == nil {
		t.Fatal("a negative usage quantity was accepted")
	}
	var castRows int
	err = db.QueryRow(
		`SELECT COUNT(*) FROM votes WHERE proposal_id = ? AND voter_name = 'seat-c'`,
		proposalID).Scan(&castRows)
	if err != nil || castRows != 0 {
		t.Errorf("the refused cast still stored seat-c's vote (%d rows, err %v)",
			castRows, err)
	}
}
