package db

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// v13 — the vote provenance column. Two obligations, one test each: the
// column arrives on a migrated store, and the rewind guard converges a store
// that carries the v13 STAMP without the v13 SHAPE (the U-case every
// migration from v5 on pins for itself).

func TestMigrateToV13(t *testing.T) {
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

	for _, col := range v13ColumnSentinels {
		if n := columnCount(t, db, col.table, col.column); n != 1 {
			t.Errorf("%s.%s missing after migration (count %d)", col.table, col.column, n)
		}
	}
}

// TestV13RewindGuardConvergesAStampedStore is the trap v13ColumnSentinels
// exists for: a database stamped 13 by a binary built mid-change carries
// every v12 sentinel, so nothing below the column level notices that
// `votes.metadata` never arrived, and Migrate's version check would return
// with the store one column short — every later cast failing on `no such
// column`. Dropping the column while leaving the stamp is that database.
//
// The pre-existing vote is here for the migration's other promise: converging
// the shape back must not disturb a row written before the column existed.
func TestV13RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	proposalID, err := CreateProposal(db, &model.Proposal{
		Description:    "a vote cast before the column existed",
		RequiredVoters: 2,
		Threshold:      0.5,
		Status:         model.ProposalStatusOpen,
	})
	if err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if _, err := CastVote(db, &model.Vote{
		ProposalID:      proposalID,
		VoterName:       "seat-a",
		Verdict:         model.VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.8,
	}); err != nil {
		t.Fatalf("CastVote: %v", err)
	}

	// The stamp stays at 13; only the shape regresses.
	mustExec(t, db, `ALTER TABLE votes DROP COLUMN metadata`)
	if n := columnCount(t, db, "votes", "metadata"); n != 0 {
		t.Fatalf("votes.metadata still present after the drop (count %d)", n)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate over the regressed shape: %v", err)
	}

	if n := columnCount(t, db, "votes", "metadata"); n != 1 {
		t.Errorf("votes.metadata missing after Migrate (count %d) — the v13 rewind guard did not fire", n)
	}
	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d after converging, want %d", v, currentSchemaVersion)
	}

	votes, err := GetProposalVotes(db, proposalID)
	if err != nil {
		t.Fatalf("GetProposalVotes: %v", err)
	}
	if len(votes) != 1 {
		t.Fatalf("got %d votes after converging, want 1", len(votes))
	}
	if votes[0].VoterName != "seat-a" || votes[0].Metadata != nil {
		t.Errorf("vote after converging = (%s, %#v), want (seat-a, nil metadata)",
			votes[0].VoterName, votes[0].Metadata)
	}
}
