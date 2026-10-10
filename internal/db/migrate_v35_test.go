package db

import (
	"database/sql"
	"testing"
)

// v35 — the loop-history columns on `steps`. The base DDL already carries
// them, so a fresh store never reaches migrateV34ToV35's ALTER branch; each
// test drops the three columns and re-stamps the version to put the store in
// the state the code path under test exists for. The stamp is what separates
// the two tests: at 34 only the migration can add the columns, at 35 only the
// rewind guard can send the store back through it.

// loopHistoryColumns names the columns by literal rather than ranging over
// v35AddedColumns or v35ColumnSentinels, so a column dropped from those lists
// still fails the assertion.
var loopHistoryColumns = []string{
	"loop_rounds_run",
	"loop_trigger_step",
	"loop_latest_verdict",
}

// stampWithoutLoopHistory returns a fully migrated store with the
// loop-history columns dropped and schema_version re-stamped to version.
func stampWithoutLoopHistory(t *testing.T, version string) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, column := range loopHistoryColumns {
		if _, err := db.Exec(`ALTER TABLE steps DROP COLUMN ` + column); err != nil {
			t.Fatalf("dropping steps.%s: %v", column, err)
		}
	}
	if _, err := db.Exec(
		`UPDATE meta SET value = ? WHERE key = 'schema_version'`, version); err != nil {
		t.Fatalf("re-stamping schema_version to %s: %v", version, err)
	}
	return db
}

func assertLoopHistoryColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, column := range loopHistoryColumns {
		exists, err := hasColumnDB(db, "steps", column)
		if err != nil {
			t.Fatalf("hasColumnDB(steps, %s): %v", column, err)
		}
		if !exists {
			t.Errorf("steps.%s missing after Migrate", column)
		}
	}
}

// TestMigrateV34StoreGainsLoopHistoryColumns upgrades a store stamped 34 that
// lacks the columns: the v35 guard does not apply, so the columns can only
// arrive through migrateV34ToV35's ALTER TABLE statements.
func TestMigrateV34StoreGainsLoopHistoryColumns(t *testing.T) {
	db := stampWithoutLoopHistory(t, "34")

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v34: %v", err)
	}

	assertLoopHistoryColumns(t, db)
	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", v, currentSchemaVersion)
	}
}

// TestV35RewindGuardConvergesAStampedStore is v27's trap applied to v35: a
// store stamped 35 by a binary built mid-change, missing the loop-history
// columns, must be detected and re-migrated rather than trusted at face value.
func TestV35RewindGuardConvergesAStampedStore(t *testing.T) {
	db := stampWithoutLoopHistory(t, "35")

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate on the half-migrated store: %v", err)
	}

	assertLoopHistoryColumns(t, db)
}
