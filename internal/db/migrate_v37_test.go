package db

import (
	"testing"
)

// v37 — the detached pre-gate target column (gates-trust §7.6.2 PG6). The
// column form's one obligation: the column arrives on a migrated store, and
// the rewind guard converges a store stamped 37 without it. There is
// deliberately NO back-fill to test — no existing row was measured by a
// detached run, so every one reads the empty string, which is never matched
// as a key. The behavior the column exists for — a claim serving a result
// keyed to its own target and running the gate for any other — is the
// engine's to prove.

func TestMigrateToV37(t *testing.T) {
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
	for _, col := range v37ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}

	// Empty, never NULL: a scan reads a plain string, and an unkeyed row must
	// compare unequal to every target rather than to nothing.
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM gate_results WHERE target_sha IS NULL`).Scan(&n); err != nil {
		t.Fatalf("counting NULL targets: %v", err)
	}
	if n != 0 {
		t.Errorf("%d row(s) carry a NULL target_sha; the column defaults to ''", n)
	}
}

// TestV37RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 37 — the mid-change-binary database v13's guard comment describes
// — and asserts Migrate converges it.
//
// The column form matters here: v37 adds no table and no index, so every v36
// sentinel is present on such a store and a table probe would never fire.
func TestV37RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE gate_results DROP COLUMN target_sha`)
	if exists, _ := hasColumnDB(db, "gate_results", "target_sha"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d",
			v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	for _, col := range v37ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Fatalf("the rewind guard did not converge %s.%s back (err %v)",
				col.table, col.column, err)
		}
	}
}
