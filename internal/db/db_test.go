package db

import (
	"database/sql"
	"testing"
	"time"
)

func mustOpen(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:) failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenSetsWALMode(t *testing.T) {
	db := mustOpen(t)

	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("querying journal_mode: %v", err)
	}
	// In-memory databases may report "memory" instead of "wal" since WAL
	// requires a file. Accept both.
	if mode != "wal" && mode != "memory" {
		t.Errorf("journal_mode = %q, want wal or memory", mode)
	}
}

func TestOpenSetsForeignKeys(t *testing.T) {
	db := mustOpen(t)

	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("querying foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

func TestOpenSetsBusyTimeout(t *testing.T) {
	db := mustOpen(t)

	var timeout int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("querying busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", timeout)
	}
}

func TestInitializeCreatesAllTables(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	tables := []string{
		"meta", "issues", "comments", "labels",
		"issue_labels", "issue_relations", "activity_log", "issue_files",
	}

	for _, table := range tables {
		var name string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found: %v", table, err)
		}
	}
}

func TestInitializeSetsSchemaVersion(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if v != 1 {
		t.Errorf("schema_version = %d, want 1", v)
	}
}

func TestInitializeIsIdempotent(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("first Initialize failed: %v", err)
	}
	if err := Initialize(db); err != nil {
		t.Fatalf("second Initialize failed: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if v != 1 {
		t.Errorf("schema_version = %d after double init, want 1", v)
	}
}

func TestForeignKeyEnforcement(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Try to insert a comment referencing a non-existent issue.
	_, err := db.Exec(
		"INSERT INTO comments (issue_id, body, created_at) VALUES (999, 'test', ?)",
		now,
	)
	if err == nil {
		t.Error("expected foreign key violation, got nil")
	}
}

func TestCascadeDeleteIssueRemovesComments(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Insert an issue.
	res, err := db.Exec(
		"INSERT INTO issues (title, status, priority, kind, created_at, updated_at) VALUES ('test', 'backlog', 'none', 'task', ?, ?)",
		now, now,
	)
	if err != nil {
		t.Fatalf("inserting issue: %v", err)
	}
	issueID, _ := res.LastInsertId()

	// Insert a comment on that issue.
	_, err = db.Exec(
		"INSERT INTO comments (issue_id, body, created_at) VALUES (?, 'a comment', ?)",
		issueID, now,
	)
	if err != nil {
		t.Fatalf("inserting comment: %v", err)
	}

	// Delete the issue.
	if _, err := db.Exec("DELETE FROM issues WHERE id = ?", issueID); err != nil {
		t.Fatalf("deleting issue: %v", err)
	}

	// Comment should be gone.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM comments WHERE issue_id = ?", issueID).Scan(&count); err != nil {
		t.Fatalf("counting comments: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 comments after cascade delete, got %d", count)
	}
}

func TestMigrateNoOpAtLatestVersion(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d after Migrate, want %d", v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}
}

// docV4Tables lists the five tables that v3→v4 must create.
var docV4Tables = []string{
	"docs",
	"doc_revisions",
	"doc_comments",
	"doc_issue_links",
	"proposal_docs",
}

func assertTableExists(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var got string
	err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name=?", name,
	).Scan(&got)
	if err != nil {
		t.Errorf("table %q not found: %v", name, err)
	}
}

func TestMigrateV3ToV4_CleanDB(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if v != 4 {
		t.Errorf("schema_version = %d, want 4", v)
	}

	for _, tbl := range docV4Tables {
		assertTableExists(t, db, tbl)
	}
}

func TestMigrateV3ToV4_FromExistingV3DB(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Bring DB up to v3 explicitly, then stamp schema_version=3 so Migrate's
	// next invocation will only run v3→v4 (simulates a real upgrade path).
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	if err := migrateV1ToV2(tx); err != nil {
		t.Fatalf("migrateV1ToV2 failed: %v", err)
	}
	if err := migrateV2ToV3(tx); err != nil {
		t.Fatalf("migrateV2ToV3 failed: %v", err)
	}
	if _, err := tx.Exec(`UPDATE meta SET value = '3' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("stamping v3 failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(
		"INSERT INTO issues (title, status, priority, kind, created_at, updated_at) VALUES ('preexisting', 'backlog', 'none', 'task', ?, ?)",
		now, now,
	)
	if err != nil {
		t.Fatalf("inserting pre-v4 issue failed: %v", err)
	}
	preID, _ := res.LastInsertId()

	if err := Migrate(db); err != nil {
		t.Fatalf("v3→v4 Migrate failed: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if v != 4 {
		t.Errorf("schema_version = %d after v3→v4 Migrate, want 4", v)
	}
	for _, tbl := range docV4Tables {
		assertTableExists(t, db, tbl)
	}

	var title string
	if err := db.QueryRow("SELECT title FROM issues WHERE id = ?", preID).Scan(&title); err != nil {
		t.Fatalf("pre-existing issue row lost after migrate: %v", err)
	}
	if title != "preexisting" {
		t.Errorf("pre-existing issue title = %q, want %q", title, "preexisting")
	}
}

func TestMigrateV3ToV4_Idempotent(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate failed: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if v != 4 {
		t.Errorf("schema_version = %d after two Migrates, want 4", v)
	}
	for _, tbl := range docV4Tables {
		assertTableExists(t, db, tbl)
	}
}

func TestMigrateV3ToV4_BuggyStampDefensive(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Bring DB through v2 and v3 (proposals must exist so the v2 defensive
	// guard does not also fire), but skip v4 DDL and forge schema_version=4.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	if err := migrateV1ToV2(tx); err != nil {
		t.Fatalf("migrateV1ToV2 failed: %v", err)
	}
	if err := migrateV2ToV3(tx); err != nil {
		t.Fatalf("migrateV2ToV3 failed: %v", err)
	}
	if _, err := tx.Exec(`UPDATE meta SET value = '4' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("buggy v4 stamp failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	var hasDocs bool
	if err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='docs')`,
	).Scan(&hasDocs); err != nil {
		t.Fatalf("checking docs presence failed: %v", err)
	}
	if hasDocs {
		t.Fatal("precondition violated: docs table exists before defensive Migrate")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("defensive Migrate failed: %v", err)
	}

	for _, tbl := range docV4Tables {
		assertTableExists(t, db, tbl)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if v != 4 {
		t.Errorf("schema_version = %d after defensive Migrate, want 4", v)
	}
}

func TestDB_PinnedToSingleConnection(t *testing.T) {
	db := mustOpen(t)

	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("MaxOpenConnections = %d, want 1 "+
			"(load-bearing for the single-writer invariant — see TDD §5.4)", got)
	}
}

func TestIssueRelationsUniqueConstraint(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Create two issues.
	for i := 0; i < 2; i++ {
		_, err := db.Exec(
			"INSERT INTO issues (title, status, priority, kind, created_at, updated_at) VALUES (?, 'backlog', 'none', 'task', ?, ?)",
			"issue", now, now,
		)
		if err != nil {
			t.Fatalf("inserting issue %d: %v", i, err)
		}
	}

	// Insert a relation.
	_, err := db.Exec(
		"INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at) VALUES (1, 2, 'blocks', ?)",
		now,
	)
	if err != nil {
		t.Fatalf("inserting relation: %v", err)
	}

	// Duplicate should fail.
	_, err = db.Exec(
		"INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at) VALUES (1, 2, 'blocks', ?)",
		now,
	)
	if err == nil {
		t.Error("expected unique constraint violation, got nil")
	}
}

func TestParentIDSetNullOnDelete(t *testing.T) {
	db := mustOpen(t)

	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Create parent issue.
	res, err := db.Exec(
		"INSERT INTO issues (title, status, priority, kind, created_at, updated_at) VALUES ('parent', 'backlog', 'none', 'task', ?, ?)",
		now, now,
	)
	if err != nil {
		t.Fatalf("inserting parent: %v", err)
	}
	parentID, _ := res.LastInsertId()

	// Create child issue with parent_id.
	res, err = db.Exec(
		"INSERT INTO issues (parent_id, title, status, priority, kind, created_at, updated_at) VALUES (?, 'child', 'backlog', 'none', 'task', ?, ?)",
		parentID, now, now,
	)
	if err != nil {
		t.Fatalf("inserting child: %v", err)
	}
	childID, _ := res.LastInsertId()

	// Delete the parent.
	if _, err := db.Exec("DELETE FROM issues WHERE id = ?", parentID); err != nil {
		t.Fatalf("deleting parent: %v", err)
	}

	// Child should still exist with NULL parent_id.
	var parentIDVal sql.NullInt64
	if err := db.QueryRow("SELECT parent_id FROM issues WHERE id = ?", childID).Scan(&parentIDVal); err != nil {
		t.Fatalf("querying child: %v", err)
	}
	if parentIDVal.Valid {
		t.Errorf("expected NULL parent_id after parent delete, got %d", parentIDVal.Int64)
	}
}
