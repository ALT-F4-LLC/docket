package db

import (
	"testing"
)

// v36 — the schema retirement marker (`schemas.deprecated_at_ms`), the schema
// half of what v11 gave workflows. One obligation the column form always
// carries: the column arrives on a migrated store, and the rewind guard
// converges a store stamped 36 without it. There is deliberately NO back-fill
// to test — pre-v36 rows read NULL ("in service"), because no operator has
// retired a schema under a verb that did not exist.

func TestMigrateToV36(t *testing.T) {
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
	for _, col := range v36ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}
}

// TestV36RewindGuardConvergesAStampedStore is v27's trap applied to v36: a
// store stamped 36 by a binary built mid-change, missing
// `schemas.deprecated_at_ms`, must be detected and re-migrated rather than
// trusted at face value.
func TestV36RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := db.Exec(`ALTER TABLE schemas DROP COLUMN deprecated_at_ms`); err != nil {
		t.Fatalf("dropping schemas.deprecated_at_ms to simulate a half-migrated store: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE meta SET value = '36' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("re-stamping schema_version: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on the half-migrated store: %v", err)
	}

	exists, err := hasColumnDB(db, "schemas", "deprecated_at_ms")
	if err != nil {
		t.Fatalf("hasColumnDB: %v", err)
	}
	if !exists {
		t.Error("schemas.deprecated_at_ms still missing after the rewind guard should have re-run v36")
	}
}
