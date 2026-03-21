package db

import (
	"database/sql"
	"fmt"
	"strconv"
)

const currentSchemaVersion = 3

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

	// Set schema version to 1 (matching schemaDDL) only if not already set.
	// Migrate() will then apply any pending migrations (e.g. v1->v2).
	_, err = tx.Exec(
		`INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', '1')`,
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
	2: migrateV1ToV2,
	3: migrateV2ToV3,
}

// migrateV1ToV2 creates the proposals, votes, and proposal_issues tables.
func migrateV1ToV2(tx *sql.Tx) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS proposals (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	description     TEXT NOT NULL,
	criticality     TEXT NOT NULL DEFAULT 'medium',
	status          TEXT NOT NULL DEFAULT 'open',
	required_voters INTEGER NOT NULL,
	threshold       REAL NOT NULL DEFAULT 0.67,
	weighted_score  REAL,
	created_by      TEXT,
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS votes (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	proposal_id      INTEGER NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
	voter_name       TEXT NOT NULL,
	voter_role       TEXT NOT NULL DEFAULT '',
	verdict          TEXT NOT NULL,
	confidence       REAL NOT NULL,
	domain_relevance REAL NOT NULL,
	findings         TEXT NOT NULL DEFAULT '',
	created_at       TEXT NOT NULL,
	UNIQUE(proposal_id, voter_name)
);

CREATE TABLE IF NOT EXISTS proposal_issues (
	proposal_id INTEGER NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
	issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	PRIMARY KEY (proposal_id, issue_id)
);

CREATE INDEX IF NOT EXISTS idx_proposals_status ON proposals(status);
CREATE INDEX IF NOT EXISTS idx_proposals_created_at ON proposals(created_at);
CREATE INDEX IF NOT EXISTS idx_votes_proposal_id ON votes(proposal_id);
`
	_, err := tx.Exec(ddl)
	return err
}

// migrateV2ToV3 adds new columns to proposals and votes tables for enhanced
// vote tracking (rationale, domain tags, files changed, outcome, and findings).
func migrateV2ToV3(tx *sql.Tx) error {
	alterStmts := []struct {
		table string
		stmt  string
	}{
		{"proposals", `ALTER TABLE proposals ADD COLUMN rationale TEXT NOT NULL DEFAULT ''`},
		{"proposals", `ALTER TABLE proposals ADD COLUMN domain_tags TEXT NOT NULL DEFAULT '[]'`},
		{"proposals", `ALTER TABLE proposals ADD COLUMN files_changed TEXT NOT NULL DEFAULT '[]'`},
		{"proposals", `ALTER TABLE proposals ADD COLUMN final_outcome TEXT NOT NULL DEFAULT ''`},
		{"proposals", `ALTER TABLE proposals ADD COLUMN escalation_reason TEXT`},
		{"votes", `ALTER TABLE votes ADD COLUMN findings_json TEXT`},
		{"votes", `ALTER TABLE votes ADD COLUMN summary TEXT NOT NULL DEFAULT ''`},
	}

	for _, alt := range alterStmts {
		if _, err := tx.Exec(alt.stmt); err != nil {
			return fmt.Errorf("migrating v2 to v3: ALTER TABLE %s failed: %w", alt.table, err)
		}
	}

	return nil
}

// Migrate checks the current schema version and applies any pending migrations
// sequentially. It is a no-op when already at the latest version.
func Migrate(db *sql.DB) error {
	version, err := SchemaVersion(db)
	if err != nil {
		return err
	}

	// Handle databases that were stamped as v2 by a buggy Initialize() that
	// skipped the v2 migration. All v2 DDL uses IF NOT EXISTS, so re-running
	// is safe.
	if version >= 2 {
		var hasProposals bool
		err := db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='proposals')`,
		).Scan(&hasProposals)
		if err == nil && !hasProposals {
			version = 1
		}
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
