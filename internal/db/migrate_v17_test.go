package db

import (
	"testing"
)

// v17 — the vote-usage provenance column (DKT-115). Two obligations: the
// column arrives on a migrated store, and the rewind guard converges a store
// stamped 17 without it. The behavior the column exists for — a relay's
// back-fill staying distinguishable from a seat's cast-time report — is the
// engine's to prove.

func TestMigrateToV17(t *testing.T) {
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
	for _, col := range v17ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}
}

// TestV17RewindGuardConvergesAStampedStore rebuilds `vote_usage` without the
// column while leaving the stamp at 17 — the mid-change-binary database v13's
// guard comment describes — and asserts Migrate converges it.
//
// The column form matters here: v17 adds no table and no index, so every v16
// sentinel is present on such a store and a table probe would never fire.
func TestV17RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The column is removed by rebuilding the table — which is also what a
	// binary that never ran the v17 ALTER would have left behind.
	mustExec(t, db, `PRAGMA foreign_keys=OFF`)
	mustExec(t, db, `ALTER TABLE vote_usage RENAME TO vote_usage_pre_v17`)
	mustExec(t, db, `CREATE TABLE vote_usage (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		vote_id       INTEGER NOT NULL REFERENCES votes(id) ON DELETE CASCADE,
		unit          TEXT    NOT NULL,
		quantity      REAL    NOT NULL,
		created_at_ms INTEGER NOT NULL,
		UNIQUE(vote_id, unit)
	)`)
	mustExec(t, db, `DROP TABLE vote_usage_pre_v17`)
	mustExec(t, db, `CREATE INDEX IF NOT EXISTS idx_vote_usage_vote ON vote_usage(vote_id)`)
	mustExec(t, db, `PRAGMA foreign_keys=ON`)

	if exists, _ := hasColumnDB(db, "vote_usage", "source"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	exists, err := hasColumnDB(db, "vote_usage", "source")
	if err != nil || !exists {
		t.Fatalf("the rewind guard did not converge vote_usage.source back (err %v)", err)
	}
}
