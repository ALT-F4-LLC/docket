package db

import (
	"database/sql"
	"strings"
	"testing"
)

// v12 — the projects dimension. The migration's obligations, each tested
// below: the projects table and its unclaimed seed row; project_id on every
// root entity; the three rebuilds preserving ids so child references hold;
// per-project uniqueness where DB-wide uniqueness used to be; and the rewind
// guard converging a database that carries the stamp without the shape.

func TestMigrateToV12(t *testing.T) {
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

	for _, col := range v12ColumnSentinels {
		if n := columnCount(t, db, col.table, col.column); n != 1 {
			t.Errorf("%s.%s missing after migration (count %d)", col.table, col.column, n)
		}
	}

	// The seed row: id 1, UNCLAIMED. A legacy store's entire history
	// backfills here, and the first repository to open the store claims it.
	var identity, name string
	err = db.QueryRow(`SELECT identity, name FROM projects WHERE id = 1`).
		Scan(&identity, &name)
	if err != nil {
		t.Fatalf("reading the seed project: %v", err)
	}
	if identity != "" {
		t.Errorf("seed project identity = %q, want empty (unclaimed)", identity)
	}
}

// TestV12RebuildPreservesIdsAndBackfills drives the migration over a POPULATED
// pre-v12 database — old-shape labels and workflows carrying real rows with
// children referencing them — and asserts the rebuild kept every id, so
// issue_labels and steps references survive without rewriting.
func TestV12RebuildPreservesIdsAndBackfills(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Seed through the current shape: one issue, one label attached to it,
	// one registered workflow.
	mustExec(t, db, `INSERT INTO issues (title, created_at, updated_at) VALUES ('a', 't', 't')`)
	mustExec(t, db, `INSERT INTO labels (project_id, name, color) VALUES (1, 'bug', 'red')`)
	mustExec(t, db, `INSERT INTO issue_labels (issue_id, label_id) VALUES (1, 1)`)
	mustExec(t, db, `INSERT INTO workflows
		(project_id, name, version, source_sha256, body, parsed, created_at_ms)
		VALUES (1, 'review', 1, 'x', 'b', '{}', 0)`)

	// Regress the shape to pre-v12 BY HAND — the columns sit inside UNIQUE
	// constraints, so this is the same rebuild recipe in reverse, exactly what
	// a real v11 database looks like.
	mustExec(t, db, `PRAGMA foreign_keys=OFF`)
	for _, stmt := range []string{
		`CREATE TABLE labels_old (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE,
			color TEXT, version INTEGER NOT NULL DEFAULT 1)`,
		`INSERT INTO labels_old (id, name, color, version)
			SELECT id, name, color, version FROM labels`,
		`DROP TABLE labels`,
		`ALTER TABLE labels_old RENAME TO labels`,
		`CREATE TABLE workflows_old (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL,
			version INTEGER NOT NULL, description TEXT, source_path TEXT,
			source_sha256 TEXT NOT NULL, body TEXT NOT NULL, parsed TEXT NOT NULL,
			created_at_ms INTEGER NOT NULL, row_version INTEGER NOT NULL DEFAULT 1,
			deprecated_at_ms INTEGER, UNIQUE(name, version))`,
		`INSERT INTO workflows_old (id, name, version, description, source_path,
			source_sha256, body, parsed, created_at_ms, row_version, deprecated_at_ms)
			SELECT id, name, version, description, source_path, source_sha256,
			body, parsed, created_at_ms, row_version, deprecated_at_ms FROM workflows`,
		`DROP TABLE workflows`,
		`ALTER TABLE workflows_old RENAME TO workflows`,
	} {
		mustExec(t, db, stmt)
	}
	mustExec(t, db, `PRAGMA foreign_keys=ON`)

	// The database now claims v12 while carrying a v11 shape — the
	// stamped-but-missing case the column sentinels exist for.
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate over the regressed shape: %v", err)
	}

	var pid, labelID int
	err := db.QueryRow(`SELECT project_id, id FROM labels WHERE name = 'bug'`).
		Scan(&pid, &labelID)
	if err != nil {
		t.Fatalf("reading the rebuilt label: %v", err)
	}
	if pid != 1 || labelID != 1 {
		t.Errorf("rebuilt label = (project %d, id %d), want (1, 1)", pid, labelID)
	}

	// The child reference survived the rebuild — the reason ids are copied.
	var joined int
	err = db.QueryRow(`SELECT COUNT(*) FROM issue_labels il
		JOIN labels l ON l.id = il.label_id WHERE l.name = 'bug'`).Scan(&joined)
	if err != nil || joined != 1 {
		t.Errorf("issue_labels join after rebuild = %d (err %v), want 1", joined, err)
	}

	err = db.QueryRow(`SELECT project_id FROM workflows WHERE name = 'review' AND version = 1`).
		Scan(&pid)
	if err != nil || pid != 1 {
		t.Errorf("rebuilt workflow project = %d (err %v), want 1", pid, err)
	}
}

// TestV12PerProjectUniqueness is G5's fix, asserted at the constraint: the
// same label name and the same workflow name@version coexist across projects
// and still collide within one.
func TestV12PerProjectUniqueness(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	mustExec(t, db, `INSERT INTO projects (id, identity, name) VALUES (2, '/repo/two', 'two')`)

	mustExec(t, db, `INSERT INTO labels (project_id, name) VALUES (1, 'bug')`)
	mustExec(t, db, `INSERT INTO labels (project_id, name) VALUES (2, 'bug')`)
	if _, err := db.Exec(`INSERT INTO labels (project_id, name) VALUES (1, 'bug')`); err == nil {
		t.Error("a duplicate label within one project inserted; UNIQUE(project_id, name) is not enforced")
	}

	const wf = `INSERT INTO workflows
		(project_id, name, version, source_sha256, body, parsed, created_at_ms)
		VALUES (?, 'review', 1, ?, 'b', '{}', 0)`
	mustExec(t, db, wf, 1, "sha-one")
	// The SAME name@version with DIFFERENT bytes in another project — the
	// exact collision a shared single-tenant registry refused.
	mustExec(t, db, wf, 2, "sha-two")
	if _, err := db.Exec(wf, 1, "sha-three"); err == nil {
		t.Error("a duplicate workflow name@version within one project inserted; UNIQUE(project_id, name, version) is not enforced")
	}
}

// TestEnsureProject covers the claim ladder: first repo claims the unclaimed
// seed row, a second repo gets its own row, resolution is stable, and an
// empty identity falls back to the default without claiming anything.
func TestEnsureProject(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// An empty identity resolves to the default row and claims nothing.
	id, err := EnsureProject(db, "", "", 100)
	if err != nil || id != DefaultProjectID {
		t.Fatalf("EnsureProject(\"\") = (%d, %v), want (1, nil)", id, err)
	}
	var identity string
	if err := db.QueryRow(`SELECT identity FROM projects WHERE id = 1`).Scan(&identity); err != nil || identity != "" {
		t.Fatalf("the empty identity claimed the default row (identity %q)", identity)
	}

	// The first real repository claims row 1 — a legacy store's history binds
	// to the repo that has always owned it.
	id, err = EnsureProject(db, "/repo/one", "one", 100)
	if err != nil || id != 1 {
		t.Fatalf("first EnsureProject = (%d, %v), want (1, nil)", id, err)
	}

	// The second gets its own row; both resolutions are stable.
	id2, err := EnsureProject(db, "/repo/two", "two", 200)
	if err != nil || id2 == 1 {
		t.Fatalf("second EnsureProject = (%d, %v), want a new row", id2, err)
	}
	again, err := EnsureProject(db, "/repo/one", "one", 300)
	if err != nil || again != 1 {
		t.Errorf("re-resolving /repo/one = (%d, %v), want (1, nil)", again, err)
	}
	again2, err := EnsureProject(db, "/repo/two", "two", 300)
	if err != nil || again2 != id2 {
		t.Errorf("re-resolving /repo/two = (%d, %v), want (%d, nil)", again2, err, id2)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", strings.SplitN(query, "\n", 2)[0], err)
	}
}
