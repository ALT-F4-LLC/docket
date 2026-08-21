package db

import (
	"testing"
)

// v23 — the attempt-outcome breakdown (DKT-490). Two obligations: the columns
// arrive on a migrated store, and the rewind guard converges a store stamped 23
// without them. There is deliberately NO back-fill to test — pre-v23 claims
// read zero ("no recorded breakdown"), because the only derivation available is
// the prunable event log, and a count that decays with retention is not a fact
// a column may assert. The behavior the columns exist for — fail bumping one,
// reap bumping the other, retry touching neither — is the engine's to prove.

func TestMigrateToV23(t *testing.T) {
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
	for _, col := range v23ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}
}

// TestV23RewindGuardConvergesAStampedStore drops the columns while leaving the
// stamp at 23 — the mid-change-binary database v13's guard comment describes —
// and asserts Migrate converges it.
//
// The column form matters here: v23 adds no table and no index, so every v22
// sentinel is present on such a store and a table probe would never fire.
func TestV23RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE steps DROP COLUMN failed_attempts`)
	mustExec(t, db, `ALTER TABLE steps DROP COLUMN reaped_claims`)
	if exists, _ := hasColumnDB(db, "steps", "failed_attempts"); exists {
		t.Fatal("the fixture did not remove the columns it is testing the recovery of")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	for _, col := range v23ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Fatalf("the rewind guard did not converge %s.%s back (err %v)",
				col.table, col.column, err)
		}
	}
}
