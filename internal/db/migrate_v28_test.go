package db

import (
	"testing"
)

// TestV28RewindGuardConvergesAStampedStore is v27's trap (migrate_v27_test.go)
// applied to v28: a store stamped 28 by a binary built mid-change, missing
// `proposals.sealed`, must be detected and re-migrated rather than trusted at
// face value.
func TestV28RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE proposals DROP COLUMN sealed`)
	mustExec(t, db, `UPDATE meta SET value = '28' WHERE key = 'schema_version'`)
	if exists, err := hasColumnDB(db, "proposals", "sealed"); err != nil || exists {
		t.Fatalf("proposals.sealed present = %v (err %v) after the drop, want absent", exists, err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on the half-migrated store: %v", err)
	}

	for _, col := range v28ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil {
			t.Fatalf("probing %s.%s: %v", col.table, col.column, err)
		}
		if !exists {
			t.Errorf("%s.%s still missing after the rewind guard should have re-run v28",
				col.table, col.column)
		}
	}
}
