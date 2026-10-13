package db

import (
	"database/sql"
	"testing"
)

// TestV31AddsTheExtendCursorAndIsReRunnable: v31 adds `dispatches.extended_seq`
// and must stay re-runnable over a store that already carries it.
//
// A second Migrate on a store stamped at the current version runs no migration
// at all, so it cannot exercise the probe-first ALTER. Rewinding the stamp to 30
// with the column still present makes migrateV30ToV31 run against it.
func TestV31AddsTheExtendCursorAndIsReRunnable(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertV31ColumnsPresent(t, db, "after Migrate")

	mustExec(t, db, `UPDATE meta SET value = '30' WHERE key = 'schema_version'`)
	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate from a stamp of 30 with the column present: %v", err)
	}
	assertV31ColumnsPresent(t, db, "after re-running Migrate from 30")
}

// TestV31RewindGuardConvergesAStampedStore drops `extended_seq` while leaving
// the stamp at the current version, the mid-change-binary database v13's guard
// comment describes, and asserts Migrate converges it.
func TestV31RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE dispatches DROP COLUMN extended_seq`)
	if exists, _ := hasColumnDB(db, "dispatches", "extended_seq"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d",
			v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	assertV31ColumnsPresent(t, db, "after the rewind guard ran")
}

func assertV31ColumnsPresent(t *testing.T, db *sql.DB, when string) {
	t.Helper()
	for _, col := range v31ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil {
			t.Fatalf("probing %s.%s %s: %v", col.table, col.column, when, err)
		}
		if !exists {
			t.Fatalf("%s.%s is absent %s", col.table, col.column, when)
		}
	}
}
