package db

import (
	"testing"
)

// TestV33RewindGuardConvergesAStampedStore is v27's trap (migrate_v27_test.go)
// applied to v33: a store stamped 33 by a binary built mid-change, missing
// `steps.park_class`, must be detected and re-migrated rather than trusted at
// face value.
func TestV33RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE steps DROP COLUMN park_class`)
	mustExec(t, db, `UPDATE meta SET value = '33' WHERE key = 'schema_version'`)
	if exists, err := hasColumnDB(db, "steps", "park_class"); err != nil || exists {
		t.Fatalf("steps.park_class present = %v (err %v) after the drop, want absent", exists, err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on the half-migrated store: %v", err)
	}

	for _, col := range v33ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil {
			t.Fatalf("probing %s.%s: %v", col.table, col.column, err)
		}
		if !exists {
			t.Errorf("%s.%s still missing after the rewind guard should have re-run v33",
				col.table, col.column)
		}
	}
}
