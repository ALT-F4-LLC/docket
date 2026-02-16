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
	if v != 2 {
		t.Errorf("schema_version = %d, want 2", v)
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
	if v != 2 {
		t.Errorf("schema_version = %d after double init, want 2", v)
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

	// Should be a no-op at version 2.
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion failed: %v", err)
	}
	if v != 2 {
		t.Errorf("schema_version = %d after Migrate, want 2", v)
	}
}

func TestMigrateFromV1ToV2(t *testing.T) {
	db := mustOpen(t)

	// Manually create a v1 schema (everything except issue_files).
	v1DDL := `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT
);
CREATE TABLE IF NOT EXISTS issues (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	parent_id   INTEGER REFERENCES issues(id) ON DELETE SET NULL,
	title       TEXT NOT NULL,
	description TEXT,
	status      TEXT NOT NULL DEFAULT 'backlog',
	priority    TEXT NOT NULL DEFAULT 'none',
	kind        TEXT NOT NULL DEFAULT 'task',
	assignee    TEXT,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS comments (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	body       TEXT NOT NULL,
	author     TEXT,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS labels (
	id    INTEGER PRIMARY KEY AUTOINCREMENT,
	name  TEXT NOT NULL UNIQUE,
	color TEXT
);
CREATE TABLE IF NOT EXISTS issue_labels (
	issue_id INTEGER REFERENCES issues(id) ON DELETE CASCADE,
	label_id INTEGER REFERENCES labels(id) ON DELETE CASCADE,
	PRIMARY KEY (issue_id, label_id)
);
CREATE TABLE IF NOT EXISTS issue_relations (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	source_issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	target_issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	relation_type   TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	UNIQUE(source_issue_id, target_issue_id, relation_type)
);
CREATE TABLE IF NOT EXISTS activity_log (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	issue_id      INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	field_changed TEXT NOT NULL,
	old_value     TEXT,
	new_value     TEXT,
	changed_by    TEXT,
	created_at    TEXT NOT NULL
);
`
	if _, err := db.Exec(v1DDL); err != nil {
		t.Fatalf("creating v1 schema: %v", err)
	}

	// Set schema version to 1.
	if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('schema_version', '1')`); err != nil {
		t.Fatalf("setting schema version: %v", err)
	}

	// Verify issue_files does NOT exist before migration.
	var name string
	err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='issue_files'",
	).Scan(&name)
	if err == nil {
		t.Fatal("issue_files table should not exist before migration")
	}

	// Run migration.
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify schema version is now 2.
	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 2 {
		t.Errorf("schema_version = %d after migration, want 2", v)
	}

	// Verify issue_files table exists.
	err = db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='issue_files'",
	).Scan(&name)
	if err != nil {
		t.Errorf("issue_files table should exist after migration: %v", err)
	}

	// Verify the table is functional by inserting data.
	now := "2024-01-01T00:00:00Z"
	if _, err := db.Exec(
		"INSERT INTO issues (title, status, priority, kind, created_at, updated_at) VALUES ('test', 'backlog', 'none', 'task', ?, ?)",
		now, now,
	); err != nil {
		t.Fatalf("inserting issue: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO issue_files (issue_id, file_path) VALUES (1, 'main.go')",
	); err != nil {
		t.Fatalf("inserting file mapping after migration: %v", err)
	}

	var fp string
	if err := db.QueryRow("SELECT file_path FROM issue_files WHERE issue_id = 1").Scan(&fp); err != nil {
		t.Fatalf("querying file mapping: %v", err)
	}
	if fp != "main.go" {
		t.Errorf("file_path = %q, want %q", fp, "main.go")
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
