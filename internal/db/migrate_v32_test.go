package db

import (
	"testing"
)

// TestV32RewindGuardConvergesAStampedStore is v27's trap (migrate_v27_test.go)
// applied to v32: a store stamped 32 by a binary built mid-change, missing
// `steps.park_reason`, must be detected and re-migrated rather than trusted at
// face value.
func TestV32RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE steps DROP COLUMN park_reason`)
	mustExec(t, db, `UPDATE meta SET value = '32' WHERE key = 'schema_version'`)
	if exists, err := hasColumnDB(db, "steps", "park_reason"); err != nil || exists {
		t.Fatalf("steps.park_reason present = %v (err %v) after the drop, want absent", exists, err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on the half-migrated store: %v", err)
	}

	for _, col := range v32ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil {
			t.Fatalf("probing %s.%s: %v", col.table, col.column, err)
		}
		if !exists {
			t.Errorf("%s.%s still missing after the rewind guard should have re-run v32",
				col.table, col.column)
		}
	}
}
