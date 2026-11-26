package db

import "testing"

// TestV30RewindGuardConvergesAStampedStore drops one fingerprint column while
// leaving the stamp at the current version — the mid-change-binary database
// v13's guard comment describes — and asserts Migrate converges it.
//
// The column form matters: v30 adds no table and no index, so every v29
// sentinel is present on such a store and a table probe would never fire.
func TestV30RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE gate_override_grants DROP COLUMN fingerprint`)
	if exists, _ := hasColumnDB(db, "gate_override_grants", "fingerprint"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d",
			v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	for _, col := range v30ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Fatalf("the rewind guard did not converge %s.%s back (err %v)",
				col.table, col.column, err)
		}
	}
}
