package db

import (
	"database/sql"
	"testing"
)

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, name,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("checking table %s: %v", name, err)
	}
	return exists
}

func columnCount(t *testing.T, db *sql.DB, table, column string) int {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column,
	).Scan(&n)
	if err != nil {
		t.Fatalf("inspecting %s.%s: %v", table, column, err)
	}
	return n
}

func TestMigrateToV5(t *testing.T) {
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
	// The stage pin moved to TestMigrateToV6 when stage 2 shipped v6. What
	// this test still guards is that v5's structures survive every later
	// migration — a v5 obligation does not expire because v6 exists.
	if currentSchemaVersion < 5 {
		t.Errorf("currentSchemaVersion = %d; v5 structures must persist", currentSchemaVersion)
	}

	if !hasTable(t, db, "idempotency_keys") {
		t.Error("idempotency_keys table missing after migration")
	}
	for _, table := range versionedTables {
		if n := columnCount(t, db, table, "version"); n != 1 {
			t.Errorf("%s.version column count = %d, want 1", table, n)
		}
	}
}

// The migration must be re-runnable: ALTER TABLE ADD COLUMN is not idempotent
// in SQLite, so migrateV4ToV5 probes before adding.
func TestMigrateV5Idempotent(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	for i := range 3 {
		if err := Migrate(db); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d after 3 migrates, want %d", v, currentSchemaVersion)
	}
	for _, table := range versionedTables {
		if n := columnCount(t, db, table, "version"); n != 1 {
			t.Errorf("%s.version count = %d after repeated migration, want 1", table, n)
		}
	}
}

// A v4 database — no version columns, no idempotency_keys — must migrate
// cleanly, with every existing row backfilled to version 1.
func TestMigrateV4ToV5BackfillsExistingRows(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Simulate a v4 DB: drop the v5 additions and rewind the stamp.
	for _, table := range versionedTables {
		if _, err := db.Exec(`ALTER TABLE ` + table + ` DROP COLUMN version`); err != nil {
			t.Fatalf("dropping %s.version: %v", table, err)
		}
	}
	if _, err := db.Exec(`DROP TABLE idempotency_keys`); err != nil {
		t.Fatalf("dropping idempotency_keys: %v", err)
	}
	if _, err := db.Exec(`UPDATE meta SET value = '4' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("rewinding schema_version: %v", err)
	}

	// Seed a row that predates v5.
	if _, err := db.Exec(
		`INSERT INTO issues (title, description, status, priority, kind, assignee, created_at, updated_at)
		 VALUES ('legacy', '', 'todo', 'none', 'task', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seeding legacy issue: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("v4->current Migrate: %v", err)
	}

	// The migration runs forward to the CURRENT version, not to v5: a v4 DB
	// opened by a v6 binary lands at v6 in one pass. What this test asserts is
	// that v5's backfill still happens correctly along that path.
	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", v, currentSchemaVersion)
	}

	var version int
	if err := db.QueryRow(`SELECT version FROM issues WHERE title = 'legacy'`).Scan(&version); err != nil {
		t.Fatalf("reading backfilled version: %v", err)
	}
	if version != 1 {
		t.Errorf("pre-v5 row version = %d, want 1 (backfilled)", version)
	}
	if !hasTable(t, db, "idempotency_keys") {
		t.Error("idempotency_keys not created by v4->v5")
	}
}

// The defensive rewind: a DB stamped >=5 but missing idempotency_keys is
// rewound to v4 and re-migrated, matching the existing v2/v4 guards.
func TestMigrateV5DefensiveRewind(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if _, err := db.Exec(`DROP TABLE idempotency_keys`); err != nil {
		t.Fatalf("dropping idempotency_keys: %v", err)
	}
	// Stamp stays at 5 while the table is absent — the buggy-stamp case.

	if err := Migrate(db); err != nil {
		t.Fatalf("defensive Migrate: %v", err)
	}
	if !hasTable(t, db, "idempotency_keys") {
		t.Error("defensive rewind did not recreate idempotency_keys")
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d after defensive Migrate, want %d", v, currentSchemaVersion)
	}
}

// Existing timestamp columns must keep their RFC3339 TEXT format. Millisecond
// timestamps belong only in tables created at v5+ — rewriting an existing
// column would change every existing verb's output (engine-spec §9 item 8).
func TestV5DoesNotMutateExistingTimestampFormats(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, col := range []string{"created_at", "updated_at"} {
		var declType string
		err := db.QueryRow(
			`SELECT type FROM pragma_table_info('issues') WHERE name = ?`, col,
		).Scan(&declType)
		if err != nil {
			t.Fatalf("inspecting issues.%s: %v", col, err)
		}
		if declType != "TEXT" {
			t.Errorf("issues.%s type = %q, want TEXT — existing formats are frozen", col, declType)
		}
	}

	// The ms/seq columns live in the new table only.
	if n := columnCount(t, db, "idempotency_keys", "created_at_ms"); n != 1 {
		t.Error("idempotency_keys.created_at_ms missing")
	}
	if n := columnCount(t, db, "idempotency_keys", "seq"); n != 1 {
		t.Error("idempotency_keys.seq missing")
	}
	if n := columnCount(t, db, "issues", "created_at_ms"); n != 0 {
		t.Error("issues gained a created_at_ms column; existing tables must not change")
	}
}
