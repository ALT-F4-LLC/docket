package db

import (
	"database/sql"
	"testing"
)

// leaseColumnNames are the v6 additions, as the steps table will carry them.
var leaseColumnNames = []string{"owner", "token_hash", "expires_ms", "attempt"}

func TestMigrateToV6(t *testing.T) {
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
	// v6 is no longer the terminal version — stage 3 ships v7 — but the v6
	// structures must survive every later migration, which is what this
	// assertion now guards. The stage pin lives with the current stage's test
	// (TestMigrateToV7).
	if currentSchemaVersion < 6 {
		t.Errorf("currentSchemaVersion = %d; v6 structures must remain reachable", currentSchemaVersion)
	}

	for _, col := range leaseColumnNames {
		if n := columnCount(t, db, "issues", col); n != 1 {
			t.Errorf("issues.%s missing after migration (count %d)", col, n)
		}
	}

	if !hasIndex(t, db, "idx_issues_expires_ms") {
		t.Error("idx_issues_expires_ms missing after migration")
	}
}

// TestMigrateV5ToV6IsIdempotent covers the rewind guard: a DB stamped v6 whose
// lease columns are missing must be re-migrated rather than trusted.
func TestMigrateV5ToV6IsIdempotent(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Re-running changes nothing.
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	for _, col := range leaseColumnNames {
		if n := columnCount(t, db, "issues", col); n != 1 {
			t.Errorf("issues.%s count = %d after re-migration, want 1", col, n)
		}
	}

	// Simulate the stamped-but-missing case the guard exists for.
	// The index references expires_ms, so it must go first.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_issues_expires_ms`); err != nil {
		t.Fatalf("dropping index: %v", err)
	}
	for _, col := range leaseColumnNames {
		if _, err := db.Exec(`ALTER TABLE issues DROP COLUMN ` + col); err != nil {
			t.Fatalf("dropping issues.%s: %v", col, err)
		}
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after dropping lease columns: %v", err)
	}
	for _, col := range leaseColumnNames {
		if n := columnCount(t, db, "issues", col); n != 1 {
			t.Errorf("issues.%s not restored by the rewind guard", col)
		}
	}
}

// TestMigrateV5ToV6Dormancy is the §9 item 8 obligation at the storage layer:
// rows that predate v6 come through unclaimed, so nothing about them reads
// differently.
func TestMigrateV5ToV6Dormancy(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Rewind to a pre-v6 shape and seed a row that predates leases.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_issues_expires_ms`); err != nil {
		t.Fatalf("dropping index: %v", err)
	}
	for _, col := range leaseColumnNames {
		if _, err := db.Exec(`ALTER TABLE issues DROP COLUMN ` + col); err != nil {
			t.Fatalf("dropping issues.%s: %v", col, err)
		}
	}
	if _, err := db.Exec(`UPDATE meta SET value = '5' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("rewinding schema_version: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO issues (title, description, status, priority, kind, assignee, created_at, updated_at)
		 VALUES ('legacy', '', 'todo', 'none', 'task', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seeding legacy issue: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("v5->v6 Migrate: %v", err)
	}

	var (
		owner     sql.NullString
		tokenHash sql.NullString
		expiresMS sql.NullInt64
		attempt   int
	)
	err := db.QueryRow(
		`SELECT owner, token_hash, expires_ms, attempt FROM issues WHERE title = 'legacy'`,
	).Scan(&owner, &tokenHash, &expiresMS, &attempt)
	if err != nil {
		t.Fatalf("reading migrated row: %v", err)
	}

	if owner.Valid {
		t.Errorf("pre-v6 row has owner %q, want NULL", owner.String)
	}
	if tokenHash.Valid {
		t.Error("pre-v6 row has a token_hash, want NULL")
	}
	if expiresMS.Valid {
		t.Error("pre-v6 row has an expires_ms, want NULL")
	}
	if attempt != 0 {
		t.Errorf("pre-v6 row attempt = %d, want 0", attempt)
	}

	// And it reads back as unclaimed through the normal path.
	issue, err := GetIssue(db, 1)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.Lease != nil {
		t.Errorf("migrated row carries a lease: %+v", issue.Lease)
	}
}

func hasIndex(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='index' AND name=?)`, name,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("checking index %s: %v", name, err)
	}
	return exists
}
