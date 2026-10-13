package db

import (
	"testing"
)

// v34 — the issue size column (operator request, 2026-09-16). One
// obligation the column form always carries: the column arrives on a
// migrated store, and the rewind guard converges a store stamped 34
// without it. There is deliberately NO back-fill to test — pre-v34 issues
// read "" ("no size declared"), the same honest-absence reading `resolution`
// and `scope_globs` use, because no size was ever recorded under the
// label convention this column replaces as workflow-routing input.

func TestMigrateToV34(t *testing.T) {
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
	for _, col := range v34ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}
}

// TestV34RewindGuardConvergesAStampedStore is v27's trap (migrate_v27_test.go)
// applied to v34: a store stamped 34 by a binary built mid-change, missing
// `issues.size`, must be detected and re-migrated rather than trusted at face
// value.
func TestV34RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Simulate a store stamped 34 that never actually got the column: drop
	// it, then re-stamp the meta row to 34 as a half-migrated binary would
	// leave it.
	if _, err := db.Exec(`ALTER TABLE issues DROP COLUMN size`); err != nil {
		t.Fatalf("dropping issues.size to simulate a half-migrated store: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE meta SET value = '34' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("re-stamping schema_version: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on the half-migrated store: %v", err)
	}

	exists, err := hasColumnDB(db, "issues", "size")
	if err != nil {
		t.Fatalf("hasColumnDB: %v", err)
	}
	if !exists {
		t.Error("issues.size still missing after the rewind guard should have re-run v34")
	}
}
