package db

import (
	"testing"
)

// v27 — the prior-claim-end marker (DKT-1279). One obligation the column
// form always carries: the column arrives on a migrated store, and the
// rewind guard converges a store stamped 27 without it. There is
// deliberately NO back-fill to test — pre-v27 claims read "" ("no recorded
// ending"), the same never-captured honesty v23's zero counters use, because
// the only derivation available is the prunable event log. The behavior the
// column exists for — fail stamping one value, reap stamping the other, and
// the reaping transaction's own re-offer seeing what it just wrote — is the
// engine's to prove.

func TestMigrateToV27(t *testing.T) {
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
	for _, col := range v27ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}
}

// TestV27RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 27 — the mid-change-binary database v13's guard comment describes
// — and asserts Migrate converges it.
//
// The column form matters here: v27 adds no table and no index, so every v26
// sentinel is present on such a store and a table probe would never fire.
func TestV27RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE steps DROP COLUMN last_claim_end`)
	if exists, _ := hasColumnDB(db, "steps", "last_claim_end"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	for _, col := range v27ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Fatalf("the rewind guard did not converge %s.%s back (err %v)",
				col.table, col.column, err)
		}
	}
}
