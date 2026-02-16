package db

import (
	"database/sql"
	"fmt"
	"strconv"
)

const currentSchemaVersion = 2

// schemaDDL contains the CREATE TABLE statements for the initial schema.
const schemaDDL = `
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

CREATE TRIGGER IF NOT EXISTS trg_no_inverse_duplicate_relation
BEFORE INSERT ON issue_relations
WHEN EXISTS (
	SELECT 1 FROM issue_relations
	WHERE relation_type = NEW.relation_type
	  AND source_issue_id = NEW.target_issue_id
	  AND target_issue_id = NEW.source_issue_id
)
BEGIN
	SELECT RAISE(ABORT, 'inverse duplicate relation');
END;

CREATE TABLE IF NOT EXISTS activity_log (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	issue_id      INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	field_changed TEXT NOT NULL,
	old_value     TEXT,
	new_value     TEXT,
	changed_by    TEXT,
	created_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_issues_status ON issues(status);
CREATE INDEX IF NOT EXISTS idx_issues_priority ON issues(priority);
CREATE INDEX IF NOT EXISTS idx_issues_assignee ON issues(assignee);
CREATE INDEX IF NOT EXISTS idx_issues_parent_id ON issues(parent_id);
CREATE INDEX IF NOT EXISTS idx_issues_created_at ON issues(created_at);
CREATE INDEX IF NOT EXISTS idx_issues_updated_at ON issues(updated_at);

CREATE TABLE IF NOT EXISTS issue_files (
	issue_id  INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	file_path TEXT NOT NULL,
	PRIMARY KEY (issue_id, file_path)
);
CREATE INDEX IF NOT EXISTS idx_issue_files_file_path ON issue_files(file_path);
`

// Initialize creates all tables if they don't exist and sets the schema version.
func Initialize(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(schemaDDL); err != nil {
		return fmt.Errorf("creating schema: %w", err)
	}

	// Set schema version only if not already set.
	_, err = tx.Exec(
		`INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', ?)`,
		strconv.Itoa(currentSchemaVersion),
	)
	if err != nil {
		return fmt.Errorf("setting schema version: %w", err)
	}

	return tx.Commit()
}

// SchemaVersion returns the current schema version from the meta table.
func SchemaVersion(db *sql.DB) (int, error) {
	var val string
	err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&val)
	if err != nil {
		return 0, fmt.Errorf("reading schema version: %w", err)
	}

	v, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("parsing schema version %q: %w", val, err)
	}

	return v, nil
}

// migrations is a list of migration functions keyed by the version they migrate TO.
// For example, migrations[2] migrates from version 1 to version 2.
var migrations = map[int]func(tx *sql.Tx) error{
	2: func(tx *sql.Tx) error {
		_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS issue_files (
	issue_id  INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	file_path TEXT NOT NULL,
	PRIMARY KEY (issue_id, file_path)
);
CREATE INDEX IF NOT EXISTS idx_issue_files_file_path ON issue_files(file_path);
`)
		return err
	},
}

// Migrate checks the current schema version and applies any pending migrations
// sequentially. It is a no-op when already at the latest version.
func Migrate(db *sql.DB) error {
	version, err := SchemaVersion(db)
	if err != nil {
		return err
	}

	if version == currentSchemaVersion {
		return nil
	}

	for v := version + 1; v <= currentSchemaVersion; v++ {
		migrateFn, ok := migrations[v]
		if !ok {
			return fmt.Errorf("missing migration for version %d", v)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("beginning migration %d transaction: %w", v, err)
		}

		if err := migrateFn(tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("applying migration %d: %w", v, err)
		}

		if _, err := tx.Exec(
			`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
			strconv.Itoa(v),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("updating schema version to %d: %w", v, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %d: %w", v, err)
		}
	}

	return nil
}
