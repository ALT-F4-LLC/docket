package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/schema"
)

const currentSchemaVersion = 27

// schemaDDL contains the CREATE TABLE statements for the initial schema.
//
// Note: this DDL is stamped as version 1 and then brought forward by Migrate,
// but it defines tables in their CURRENT shape — `issue_files` (post-v1) and
// the v5 `version` CAS columns are here for the same reason: a freshly
// Initialize()d database must be queryable before Migrate runs. The v5
// migration probes with hasColumn before adding a column, so a fresh DB that
// already has it is skipped rather than erroring.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT
);

CREATE TABLE IF NOT EXISTS projects (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	identity      TEXT NOT NULL UNIQUE,
	name          TEXT NOT NULL DEFAULT '',
	prefix        TEXT NOT NULL DEFAULT 'DKT',
	created_at_ms INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO projects (id, identity, name) VALUES (1, '', 'default');

CREATE TABLE IF NOT EXISTS issues (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id  INTEGER NOT NULL DEFAULT 1,
	parent_id   INTEGER REFERENCES issues(id) ON DELETE SET NULL,
	title       TEXT NOT NULL,
	description TEXT,
	status      TEXT NOT NULL DEFAULT 'backlog',
	priority    TEXT NOT NULL DEFAULT 'none',
	kind        TEXT NOT NULL DEFAULT 'task',
	assignee    TEXT,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL,
	version     INTEGER NOT NULL DEFAULT 1,
	owner       TEXT,
	token_hash  TEXT,
	expires_ms  INTEGER,
	attempt     INTEGER NOT NULL DEFAULT 0,
	scope_globs TEXT,
	resolution  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS comments (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	body       TEXT NOT NULL,
	author     TEXT,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS labels (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL DEFAULT 1 REFERENCES projects(id),
	name       TEXT NOT NULL,
	color      TEXT,
	version    INTEGER NOT NULL DEFAULT 1,
	UNIQUE(project_id, name)
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
CREATE INDEX IF NOT EXISTS idx_issues_expires_ms ON issues(expires_ms);
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
	2:  migrateV1ToV2,
	3:  migrateV2ToV3,
	4:  migrateV3ToV4,
	5:  migrateV4ToV5,
	6:  migrateV5ToV6,
	7:  migrateV6ToV7,
	8:  migrateV7ToV8,
	9:  migrateV8ToV9,
	10: migrateV9ToV10,
	11: migrateV10ToV11,
	12: migrateV11ToV12,
	13: migrateV12ToV13,
	14: migrateV13ToV14,
	15: migrateV14ToV15,
	16: migrateV15ToV16,
	17: migrateV16ToV17,
	18: migrateV17ToV18,
	19: migrateV18ToV19,
	20: migrateV19ToV20,
	21: migrateV20ToV21,
	22: migrateV21ToV22,
	23: migrateV22ToV23,
	24: migrateV23ToV24,
	25: migrateV24ToV25,
	26: migrateV25ToV26,
	27: migrateV26ToV27,
}

// migrationsNeedingFKOff names the migrations that REBUILD tables and so must
// run with foreign-key enforcement disabled around their transaction (the
// sqlite.org/lang_altertable.html#otheralter recipe). PRAGMA foreign_keys is a
// no-op inside a transaction, so Migrate toggles it outside and runs
// foreign_key_check before re-enabling.
var migrationsNeedingFKOff = map[int]bool{12: true}

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

// migrateV3ToV4 creates the docs, doc_revisions, doc_comments, doc_issue_links,
// and proposal_docs tables (TDD docket-doc-cli §5.1).
func migrateV3ToV4(tx *sql.Tx) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS docs (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	type        TEXT NOT NULL,
	status      TEXT NOT NULL DEFAULT 'draft',
	title       TEXT NOT NULL,
	body        TEXT NOT NULL DEFAULT '',
	author      TEXT,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS doc_revisions (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	doc_id          INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
	revision_number INTEGER NOT NULL,
	body            TEXT NOT NULL,
	change_kind     TEXT NOT NULL DEFAULT 'body',
	author          TEXT,
	created_at      TEXT NOT NULL,
	UNIQUE(doc_id, revision_number)
);

CREATE TABLE IF NOT EXISTS doc_comments (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	doc_id     INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
	body       TEXT NOT NULL,
	author     TEXT,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS doc_issue_links (
	doc_id     INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
	issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	PRIMARY KEY (doc_id, issue_id)
);

CREATE TABLE IF NOT EXISTS proposal_docs (
	proposal_id INTEGER NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
	doc_id      INTEGER NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
	created_at  TEXT NOT NULL,
	PRIMARY KEY (proposal_id, doc_id)
);

CREATE INDEX IF NOT EXISTS idx_docs_type ON docs(type);
CREATE INDEX IF NOT EXISTS idx_docs_status ON docs(status);
CREATE INDEX IF NOT EXISTS idx_docs_created_at ON docs(created_at);
CREATE INDEX IF NOT EXISTS idx_doc_revisions_doc_id ON doc_revisions(doc_id);
CREATE INDEX IF NOT EXISTS idx_doc_comments_doc_id ON doc_comments(doc_id);
CREATE INDEX IF NOT EXISTS idx_doc_issue_links_issue_id ON doc_issue_links(issue_id);
CREATE INDEX IF NOT EXISTS idx_proposal_docs_doc_id ON proposal_docs(doc_id);
`
	_, err := tx.Exec(ddl)
	return err
}

// versionedTables are the existing mutable entities that carry a CAS `version`
// column as of v5 (engine-spec.md §3, §5). Ordered for deterministic migration.
var versionedTables = []string{"issues", "docs", "proposals", "labels"}

// hasColumn reports whether a table already has the named column. ALTER TABLE
// ADD COLUMN is not idempotent in SQLite — it errors if the column exists — so
// the v5 migration probes first and stays re-runnable.
func hasColumn(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("inspecting %s.%s: %w", table, column, err)
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
}

// migrateV4ToV5 adds optimistic-concurrency and idempotency support
// (engine-spec.md §5, TDD docs/tdd/reliability-delta.md §2.1):
//
//   - a `version` CAS column on each existing mutable entity, backfilled to 1
//   - an `idempotency_keys` table for create-verb replay
//
// Additive only. Existing column formats are never mutated: `created_at` and
// `updated_at` stay RFC3339 TEXT, because rewriting them to epoch-ms would
// change every existing verb's output and break v4 compatibility (§9 item 8).
// Millisecond timestamps and the monotonic `seq` therefore appear only in the
// new table.
func migrateV4ToV5(tx *sql.Tx) error {
	for _, table := range versionedTables {
		exists, err := hasColumn(tx, table, "version")
		if err != nil {
			return fmt.Errorf("migrating v4 to v5: %w", err)
		}
		if exists {
			continue
		}
		stmt := fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN version INTEGER NOT NULL DEFAULT 1`, table,
		)
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrating v4 to v5: adding %s.version: %w", table, err)
		}
	}

	const ddl = `
CREATE TABLE IF NOT EXISTS idempotency_keys (
	scope         TEXT    NOT NULL,
	key           TEXT    NOT NULL,
	entity_id     INTEGER NOT NULL,
	created_at_ms INTEGER NOT NULL,
	seq           INTEGER NOT NULL,
	PRIMARY KEY (scope, key)
);

CREATE INDEX IF NOT EXISTS idx_idempotency_keys_seq ON idempotency_keys(seq);
`
	if _, err := tx.Exec(ddl); err != nil {
		return fmt.Errorf("migrating v4 to v5: creating idempotency_keys: %w", err)
	}

	return nil
}

// leaseColumns are the v6 lease fields, in migration order. They live on
// `issues` now and will be carried verbatim by the steps table when steps land
// (engine-spec.md §10 stage 2: "on issues and steps-to-be"), so the lease
// helpers written against them are reused rather than reimplemented.
//
// `expires_ms` is milliseconds because it is a NEW column. Existing column
// formats are never mutated — `created_at`/`updated_at` stay RFC3339 TEXT —
// since rewriting them would change every existing verb's output (§9 item 8).
var leaseColumns = []struct{ name, ddl string }{
	{"owner", `ALTER TABLE issues ADD COLUMN owner TEXT`},
	{"token_hash", `ALTER TABLE issues ADD COLUMN token_hash TEXT`},
	{"expires_ms", `ALTER TABLE issues ADD COLUMN expires_ms INTEGER`},
	{"attempt", `ALTER TABLE issues ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0`},
}

// migrateV5ToV6 adds lease and capability-token fields to issues
// (engine-spec.md §2 "Steps, claims, capabilities", engine-core.md §5;
// TDD docs/tdd/claims-leases.md §2.1).
//
// Only `token_hash` is stored, never the token itself: the token is returned
// exactly once at claim, so a stolen database file yields no live capability.
//
// Additive only, and dormant: every pre-existing row gets owner IS NULL and
// attempt = 0, so a repo that never claims reads byte-identically to v5.
func migrateV5ToV6(tx *sql.Tx) error {
	for _, col := range leaseColumns {
		exists, err := hasColumn(tx, "issues", col.name)
		if err != nil {
			return fmt.Errorf("migrating v5 to v6: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v5 to v6: adding issues.%s: %w", col.name, err)
		}
	}

	// The reap predicate's index — and the query shape steps will run at
	// scale once `next` exists.
	const ddl = `CREATE INDEX IF NOT EXISTS idx_issues_expires_ms ON issues(expires_ms);`
	if _, err := tx.Exec(ddl); err != nil {
		return fmt.Errorf("migrating v5 to v6: creating expires_ms index: %w", err)
	}

	return nil
}

// v7Sentinels are the tables the v7 DDL creates. The rewind guard probes ALL of
// them, not just the first (TDD docs/tdd/engine-spine.md §2).
//
// v7 ships as ONE migration function assembled across four phases: the version
// stamp moves to 7 in phase 1 while migrateV6ToV7 keeps growing through phase 4.
// So a database migrated by a phase-1 build — including this repo's own dogfood
// tracker — is stamped 7 and has `workflows` alone. A guard that asked only
// "stamped >= 7 but `workflows` absent?" would see the table present, do
// nothing, and that database would never gain the later phases' tables; the
// failure would surface much later as a missing table at activation.
//
// Probing every sentinel is free because the v7 DDL is CREATE TABLE IF NOT
// EXISTS throughout and the migration is re-runnable: re-running it against a
// partially-migrated database adds what is missing and touches nothing else.
//
// A phase adding a table to v7 adds it here in the same commit;
// TestRewindGuardProbesEverySentinel asserts one entry per table the v7 DDL
// creates, so the omission fails a test rather than shipping a half-migrated
// database.
var v7Sentinels = []string{
	"workflows",
	"runs", "run_issues", "pins", "run_fences", "steps",
	"artifacts", "step_inputs",
	"events",
}

// v7DDL is the v7 schema, assembled across the stage's four phases. Phase 1
// (TDD §4.1) contributed the `workflows` table; phase 2 (§5.1) adds `runs`,
// `run_issues`, `pins`, `run_fences`, and `steps`; phase 3 (§6.1) adds
// `artifacts` and `step_inputs`; phase 4 (§7.1) adds `events` and extends
// v7Sentinels alongside.
//
// `steps` lands with phase 2 rather than phase 3 because activation's stage 6
// EXPANDS into it (§5.3.1) — the rows exist and are inspectable before any
// verb claims one. Phase 3 adds the columns' readers and the step verb family,
// not the table.
//
// Every timestamp column created at v7 is `_ms INTEGER` epoch-milliseconds and
// no existing column's format is touched (reliability-delta §2.1): the
// never-mutate rule is what keeps engine-spec §9 item 8 true.
const v7DDL = `
CREATE TABLE IF NOT EXISTS workflows (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT    NOT NULL,
	version       INTEGER NOT NULL,
	description   TEXT,
	source_path   TEXT,
	source_sha256 TEXT    NOT NULL,
	body          TEXT    NOT NULL,
	parsed        TEXT    NOT NULL,
	created_at_ms INTEGER NOT NULL,
	row_version   INTEGER NOT NULL DEFAULT 1,
	UNIQUE(name, version)
);

CREATE INDEX IF NOT EXISTS idx_workflows_name ON workflows(name);

CREATE TABLE IF NOT EXISTS runs (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	request         TEXT    NOT NULL DEFAULT '',
	status          TEXT    NOT NULL DEFAULT 'planning',
	reason          TEXT,
	budget          REAL    NOT NULL DEFAULT 0,
	usage_budget    REAL    NOT NULL DEFAULT 0,
	pause_origin    TEXT    NOT NULL DEFAULT '',
	activated_at_ms INTEGER,
	created_at_ms   INTEGER NOT NULL,
	updated_at_ms   INTEGER NOT NULL,
	row_version     INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS run_issues (
	run_id         INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	issue_id       INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	workflow_id    INTEGER REFERENCES workflows(id),
	body_snapshot  TEXT,
	body_sha256    TEXT,
	issue_snapshot TEXT,
	expanded_at_ms INTEGER,
	PRIMARY KEY (run_id, issue_id)
);

CREATE TABLE IF NOT EXISTS pins (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id    INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	kind      TEXT    NOT NULL,
	ref       TEXT    NOT NULL,
	sha256    TEXT    NOT NULL,
	UNIQUE(run_id, kind, ref)
);

CREATE TABLE IF NOT EXISTS run_fences (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id   INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
	tag      TEXT    NOT NULL,
	ordinal  INTEGER NOT NULL,
	command  TEXT    NOT NULL,
	sha256   TEXT    NOT NULL,
	UNIQUE(run_id, issue_id, tag, ordinal)
);

CREATE TABLE IF NOT EXISTS steps (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id         INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	issue_id       INTEGER REFERENCES issues(id) ON DELETE CASCADE,
	workflow_id    INTEGER NOT NULL REFERENCES workflows(id),
	step_name      TEXT    NOT NULL,
	ordinal        INTEGER NOT NULL DEFAULT 0,
	sibling_index  INTEGER,
	instance       TEXT    NOT NULL,
	kind           TEXT    NOT NULL,
	executor       TEXT,
	class          TEXT,
	status         TEXT    NOT NULL DEFAULT 'pending',
	attempt        INTEGER NOT NULL DEFAULT 0,
	failed_attempts INTEGER NOT NULL DEFAULT 0,
	reaped_claims  INTEGER NOT NULL DEFAULT 0,
	last_claim_end TEXT    NOT NULL DEFAULT '',
	max_attempts   INTEGER,
	expected_cost  REAL    NOT NULL DEFAULT 0,
	owner          TEXT,
	token_hash     TEXT,
	expires_ms     INTEGER,
	started_ms     INTEGER,
	activity_ms    INTEGER,
	saga_stage     TEXT,
	gate_trail     TEXT,
	routing        TEXT,
	metadata       TEXT,
	context_bytes  INTEGER,
	created_at_ms  INTEGER NOT NULL,
	updated_at_ms  INTEGER NOT NULL,
	row_version    INTEGER NOT NULL DEFAULT 1,
	UNIQUE(run_id, issue_id, instance)
);

CREATE TABLE IF NOT EXISTS artifacts (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	step_id       INTEGER REFERENCES steps(id) ON DELETE CASCADE,
	kind          TEXT    NOT NULL,
	body          TEXT    NOT NULL,
	payload       TEXT,
	sha256        TEXT    NOT NULL,
	created_at_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS step_inputs (
	step_id     INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	position    INTEGER NOT NULL,
	artifact_id INTEGER REFERENCES artifacts(id) ON DELETE CASCADE,
	PRIMARY KEY (step_id, position, artifact_id)
);

CREATE TABLE IF NOT EXISTS events (
	seq       INTEGER PRIMARY KEY AUTOINCREMENT,
	at_ms     INTEGER NOT NULL,
	kind      TEXT    NOT NULL,
	run_id    INTEGER REFERENCES runs(id) ON DELETE CASCADE,
	step_id   INTEGER REFERENCES steps(id) ON DELETE CASCADE,
	issue_id  INTEGER REFERENCES issues(id) ON DELETE CASCADE,
	data      TEXT    NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_events_run_seq ON events(run_id, seq);

CREATE INDEX IF NOT EXISTS idx_run_issues_issue ON run_issues(issue_id);
CREATE INDEX IF NOT EXISTS idx_pins_run ON pins(run_id);
CREATE INDEX IF NOT EXISTS idx_steps_run_status ON steps(run_id, status);
CREATE INDEX IF NOT EXISTS idx_steps_expires_ms ON steps(expires_ms);
CREATE INDEX IF NOT EXISTS idx_steps_issue ON steps(issue_id);
CREATE INDEX IF NOT EXISTS idx_artifacts_step ON artifacts(step_id);
`

// migrateV6ToV7 creates the workflow-engine tables (engine-spec.md §2, §11.1;
// TDD docs/tdd/engine-spine.md §4.1).
//
// `version` and `row_version` are two different numbers and are named apart on
// purpose: `version` is the workflow author's [pipeline].version — the number
// runs pin — while `row_version` is the CAS column every mutable entity carries
// (reliability-delta §6.1). Collapsing them would make --if-version mean "the
// workflow definition version", which a re-register must never silently bump.
//
// Additive and dormant: v7 creates new tables and touches no existing one, so a
// repo with zero rows in `workflows` reads byte-identically to v6 on every
// pre-existing verb (§3).
func migrateV6ToV7(tx *sql.Tx) error {
	if _, err := tx.Exec(v7DDL); err != nil {
		return fmt.Errorf("migrating v6 to v7: %w", err)
	}

	// `issues.scope_globs` is the one touch v7 makes to an existing table
	// (TDD §5.1). It is a new NULLABLE column — NULL on every pre-existing row
	// and on every row an unmodified `issue create` writes — so a repo that
	// never declares a scope reads byte-identically to v6.
	//
	// It cannot ride in v7DDL: ALTER TABLE ADD COLUMN is not idempotent in
	// SQLite, so the migration probes first and stays re-runnable, exactly as
	// v5 and v6 do for their own columns.
	//
	// `issue_files` is NOT this column. `issue_files` holds concrete paths and
	// drives plan.go's splitByFileCollision; scope is a list of path GLOBS the
	// planner declares as a judgment about what an issue is EXPECTED to touch.
	// They differ in cardinality, in semantics (actual vs. intended), and in
	// matching rule (equality vs. glob intersection). Overloading issue_files
	// would make `plan` output depend on scope declarations for repos that
	// never activate a workflow — an engine-spec §9 item 8 violation.
	exists, err := hasColumn(tx, "issues", "scope_globs")
	if err != nil {
		return fmt.Errorf("migrating v6 to v7: %w", err)
	}
	if !exists {
		if _, err := tx.Exec(`ALTER TABLE issues ADD COLUMN scope_globs TEXT`); err != nil {
			return fmt.Errorf("migrating v6 to v7: adding issues.scope_globs: %w", err)
		}
	}

	// `run_issues.loop_count` is phase 4's column (TDD §7.1). It probes for the
	// same reason `scope_globs` does — ADD COLUMN is not idempotent — and it
	// lives on `run_issues` rather than on `steps` because §11.3 (1) makes the
	// counter THE ISSUE'S ("the issue's loop counter"), not the step's: one
	// issue loops, and every instance the loop creates is attributed to that one
	// count. A per-step counter would make `max_fix_loops` mean something
	// different for each instance and could not bound the loop at all.
	//
	// It is NOT a sentinel: sentinels are tables, and the rewind guard probes
	// `events` for this phase's slice. A database that reaches this line has
	// re-run the whole migration, so the column lands with the table.
	exists, err = hasColumn(tx, "run_issues", "loop_count")
	if err != nil {
		return fmt.Errorf("migrating v6 to v7: %w", err)
	}
	if !exists {
		if _, err := tx.Exec(
			`ALTER TABLE run_issues ADD COLUMN loop_count INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migrating v6 to v7: adding run_issues.loop_count: %w", err)
		}
	}

	return nil
}

// v8Sentinels are the tables the v8 DDL creates. The rewind guard probes ALL of
// them, for the reason v7Sentinels states and this stage re-earns (TDD
// docs/tdd/gates-trust.md §4.1, §4.4 U3).
//
// v8 ships as ONE migration function sliced across the stage's two commit
// groups: group 1 is pure Go with no DDL at all, and the stamp moves to 8 in
// group 2. So the operator's own dogfood tracker is migrated by whatever binary
// happens to be built between the two — U3's row is that shape exactly, and it
// is not hypothetical.
//
// TestRewindGuardProbesEverySentinel asserts one entry per table the v8 DDL
// creates, so an omission fails a test rather than shipping a half-migrated
// database.
var v8Sentinels = []string{
	"gate_results",
	"trust_cache",
}

// v8DDL is the v8 schema: §11.4's `gate result` shape as a table, and the
// trust-cache audit record (TDD §4.2, §4.5).
//
// Additive and dormant: v8 creates new tables and touches no existing one, so a
// repo that never runs a gate reads byte-identically to v7 on every pre-existing
// verb (§9.3).
const v8DDL = `
CREATE TABLE IF NOT EXISTS gate_results (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	step_id       INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	gate          TEXT    NOT NULL,
	ordinal       INTEGER NOT NULL DEFAULT 0,
	argv          TEXT,
	exit          INTEGER,
	duration_ms   INTEGER NOT NULL DEFAULT 0,
	output        TEXT    NOT NULL DEFAULT '',
	truncated     INTEGER NOT NULL DEFAULT 0,
	verdict       TEXT    NOT NULL,
	pre           INTEGER NOT NULL DEFAULT 0,
	stub          INTEGER NOT NULL DEFAULT 0,
	stub_entry    INTEGER NOT NULL DEFAULT 0,
	reason        TEXT,
	created_at_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_gate_results_step ON gate_results(step_id);
CREATE INDEX IF NOT EXISTS idx_gate_results_run ON gate_results(run_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_gate_results_identity
	ON gate_results(step_id, gate, ordinal);

CREATE TABLE IF NOT EXISTS trust_cache (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	gate          TEXT    NOT NULL,
	argv_sha256   TEXT    NOT NULL,
	entry_name    TEXT    NOT NULL,
	matched       INTEGER NOT NULL,
	prefix        INTEGER NOT NULL DEFAULT 0,
	at_ms         INTEGER NOT NULL,
	UNIQUE(run_id, gate, argv_sha256, at_ms)
);
`

// migrateV7ToV8 creates `gate_results` and `trust_cache` and backfills the
// S3-era `steps.gate_trail` into the new table (TDD §4.2, §4.3 — this migration
// is the recorded exit condition for the storage-location deviation).
//
// The backfill's five clauses, each load-bearing:
//
// G1: one `gate_results` row per trail element, in array order, `ordinal` = the
// array index. `created_at_ms` is the STEP's `updated_at_ms` — the trail carries
// no per-result timestamp, and the step's is the honest approximation rather
// than a fabricated `now`.
//
// G2: every migrated row is stamped `stub = 1`. Every result in a `gate_trail`
// was produced by S3's PassThroughRunner, which is the only thing that ever
// wrote one, so the stamp is a FACT rather than a guess. This is what keeps the
// gate-forgery audit (T11) true across the version boundary: a green result that
// never ran a process stays distinguishable from one that did, forever.
//
// G3: `steps.gate_trail` is RETAINED, not dropped. The never-mutate rule
// (reliability-delta §2.1) forbids destructive column changes, SQLite's DROP
// COLUMN would rewrite the table, and the column is this migration's own
// evidence. It stops being WRITTEN at v8; TestGateTrailIsNotWrittenAtV8 asserts
// that.
//
// G4: idempotent. The UNIQUE(step_id, gate, ordinal) index is created BEFORE the
// backfill and the backfill uses INSERT OR IGNORE, so re-running duplicates
// nothing. This is not optional: the v8 rewind guard can re-run this migration
// against a partially-migrated database BY DESIGN (§4.1).
//
// G5: a `gate_trail` that fails to parse is NOT a migration failure. The row is
// skipped and one `gate_results` row records the parse failure in `reason` with
// `verdict='unmatched'` and `stub=1`. A migration that aborted on one malformed
// JSON blob would make a database UNOPENABLE — the failure belongs where an
// operator will see it, not in a refusal to start.
func migrateV7ToV8(tx *sql.Tx) error {
	if _, err := tx.Exec(v8DDL); err != nil {
		return fmt.Errorf("migrating v7 to v8: %w", err)
	}

	rows, err := tx.Query(
		`SELECT id, run_id, gate_trail, updated_at_ms FROM steps
		  WHERE gate_trail IS NOT NULL AND gate_trail != ''`)
	if err != nil {
		return fmt.Errorf("migrating v7 to v8: reading gate trails: %w", err)
	}

	type trailRow struct {
		stepID, runID int
		trail         string
		updatedMS     int64
	}
	var trails []trailRow
	for rows.Next() {
		var t trailRow
		if err := rows.Scan(&t.stepID, &t.runID, &t.trail, &t.updatedMS); err != nil {
			rows.Close()
			return fmt.Errorf("migrating v7 to v8: reading a gate trail: %w", err)
		}
		trails = append(trails, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("migrating v7 to v8: reading gate trails: %w", err)
	}
	rows.Close()

	for _, t := range trails {
		if err := backfillGateTrail(tx, t.stepID, t.runID, t.trail, t.updatedMS); err != nil {
			return err
		}
	}

	return nil
}

// migratedGateResult is the §11.4 `gate result` shape as the S3 trail stored it.
// It is declared HERE, in the db package, rather than imported from the engine:
// a migration must read the bytes that were written at the time, and coupling it
// to a live struct means a future field change silently reinterprets history.
type migratedGateResult struct {
	Gate       string   `json:"gate"`
	Argv       []string `json:"argv"`
	Exit       int      `json:"exit"`
	DurationMS int64    `json:"duration_ms"`
	Output     string   `json:"output"`
	Truncated  bool     `json:"truncated"`
	Verdict    string   `json:"verdict"`
}

// backfillGateTrail inserts one gate_results row per trail element (G1), or one
// unmatched row recording the parse failure (G5).
func backfillGateTrail(tx *sql.Tx, stepID, runID int, trail string, updatedMS int64) error {
	var results []migratedGateResult
	if err := json.Unmarshal([]byte(trail), &results); err != nil {
		// G5: record the failure, do not abort the migration.
		_, insErr := tx.Exec(
			`INSERT OR IGNORE INTO gate_results
			   (run_id, step_id, gate, ordinal, verdict, stub, reason, created_at_ms)
			 VALUES (?, ?, ?, 0, 'unmatched', 1, ?, ?)`,
			runID, stepID, "", fmt.Sprintf("gate trail did not parse: %v", err), updatedMS)
		if insErr != nil {
			return fmt.Errorf("migrating v7 to v8: recording an unparseable trail: %w", insErr)
		}
		return nil
	}

	for i, r := range results {
		var argv any
		if r.Argv != nil {
			encoded, err := json.Marshal(r.Argv)
			if err != nil {
				return fmt.Errorf("migrating v7 to v8: encoding a migrated argv: %w", err)
			}
			argv = string(encoded)
		}

		truncated := 0
		if r.Truncated {
			truncated = 1
		}

		// G2: stub = 1, unconditionally. Every trail result came from the
		// pass-through runner.
		_, err := tx.Exec(
			`INSERT OR IGNORE INTO gate_results
			   (run_id, step_id, gate, ordinal, argv, exit, duration_ms, output,
			    truncated, verdict, pre, stub, created_at_ms)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 1, ?)`,
			runID, stepID, r.Gate, i, argv, r.Exit, r.DurationMS, r.Output,
			truncated, r.Verdict, updatedMS)
		if err != nil {
			return fmt.Errorf("migrating v7 to v8: backfilling a gate result: %w", err)
		}
	}

	return nil
}

// v9Sentinels are the tables the v9 DDL creates. The rewind guard probes ALL of
// them, for the reason v7Sentinels states and this stage re-earns a third time
// (TDD docs/tdd/payloads-thresholds.md §2, §4.4.1 U3).
//
// v9 ships as ONE migration function sliced across the stage's two commit
// groups: the stamp moves to 9 in GROUP 1 — which needs `schemas` — and does not
// move again, while migrateV8ToV9 keeps growing through group 2. So a database
// migrated by a group-1 build, INCLUDING this repo's own dogfooded tracker, is
// stamped 9 with `schemas` present and group 2's tables absent. That is U3's
// shape exactly, and it is not hypothetical: the operator's tracker is migrated
// by whichever binary happens to be built between the groups.
//
// The list therefore GROWS WITH THE GROUPS, exactly as v7Sentinels grew across
// its four phases: a group that adds a table to v9DDL adds it here in the same
// commit, and TestRewindGuardProbesEveryV9Sentinel derives the expected list
// from the DDL so the omission fails a test rather than shipping a
// half-migrated database. (Group 2 adds `action_results`.)
//
// The COLUMN half is the part sentinels cannot see. v9 adds `artifacts.stub`
// in group 1, and `trust_cache.kind` and `steps.materialized` in group 2, each
// behind a hasColumn probe; they arrive only because the rewind re-runs the
// WHOLE migration. §4.4.1's U-table proves each column's arrival independently.
var v9Sentinels = []string{
	"schemas",
	"action_results",
}

// v9DDL is the v9 schema, assembled across the stage's two commit groups. Group
// 1 (TDD §4.4) contributes `schemas`; group 2 (§6.3) adds `action_results`.
//
// `action_results` is `gate_results` FOR ACTIONS, deliberately: same shape, same
// `ordinal` semantics for flaky re-runs, same `reason` discipline. The
// alternative — recording nothing and leaving the routing reason as the only
// trace — makes an `unmatched` action invisible in exactly the way gates-trust
// T11's audit argument says it must not be, and makes a failed computation
// indistinguishable from a failed threshold in a run report. `argv` and `exit`
// are NULLABLE because a builtin and an unmatched action never touched the
// process table, and a zero exit on something that did not execute is the exact
// confusion the nullability exists to prevent.
//
// The `schemas` shape is `workflows`, deliberately and LINE FOR LINE: name,
// version, source_path, source_sha256, body, a derived column (`parsed` there,
// `ordered` here), created_at_ms, row_version, UNIQUE(name, version). Two
// registries with the same immutability contract that looked different would
// invite two different implementations of that contract.
//
// Additive and dormant: v9 creates a new table and seeds exactly ONE row — the
// builtin `aggregate@1`, which nothing reads unless an `aggregate` action runs
// and which is visible only through the NEW `schema list|show` verbs. No
// pre-existing verb reads `schemas`, so the byte-compat claim (§3) is untouched.
// Claiming "zero rows" would have been the flattering statement rather than the
// true one.
//
// Every timestamp column created at v9 is `_ms INTEGER` epoch-milliseconds and
// no existing column's format is touched (reliability-delta §2.1).
const v9DDL = `
CREATE TABLE IF NOT EXISTS schemas (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT    NOT NULL,
	version       INTEGER NOT NULL,
	source_path   TEXT,
	source_sha256 TEXT    NOT NULL,
	body          TEXT    NOT NULL,
	ordered       TEXT    NOT NULL DEFAULT '{}',
	builtin       INTEGER NOT NULL DEFAULT 0,
	created_at_ms INTEGER NOT NULL,
	row_version   INTEGER NOT NULL DEFAULT 1,
	UNIQUE(name, version)
);

CREATE INDEX IF NOT EXISTS idx_schemas_name ON schemas(name);

CREATE TABLE IF NOT EXISTS action_results (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	step_id       INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	action        TEXT    NOT NULL,
	ordinal       INTEGER NOT NULL DEFAULT 0,
	argv          TEXT,
	exit          INTEGER,
	duration_ms   INTEGER NOT NULL DEFAULT 0,
	output        TEXT    NOT NULL DEFAULT '',
	truncated     INTEGER NOT NULL DEFAULT 0,
	verdict       TEXT    NOT NULL,
	builtin       INTEGER NOT NULL DEFAULT 0,
	reason        TEXT,
	created_at_ms INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_action_results_identity
	ON action_results(step_id, action, ordinal);
`

// v9AddedColumns are the columns v9 adds to tables that already existed — the
// half the sentinels cannot see (TDD §2, §4.4, §6.3).
//
// They are a table rather than three inline blocks so the U-table's
// per-column assertions and this list cannot drift: a group that adds a column
// adds one row here, and TestMigrateV9GroupTwoColumnsArrive iterates the same
// set the migration does.
var v9AddedColumns = []struct{ table, column, ddl string }{
	{"artifacts", "stub",
		`ALTER TABLE artifacts ADD COLUMN stub INTEGER NOT NULL DEFAULT 0`},
	{"trust_cache", "kind",
		`ALTER TABLE trust_cache ADD COLUMN kind TEXT NOT NULL DEFAULT 'gate'`},
	{"steps", "materialized",
		`ALTER TABLE steps ADD COLUMN materialized INTEGER NOT NULL DEFAULT 0`},
}

// migrateV8ToV9 creates the payload-schema registry, seeds the one builtin
// document, and adds v9's columns (TDD §4.4, §6.3 S2).
//
// It RE-VALIDATES NOTHING. No row of `workflows` is revisited and no verb ever
// revisits a registered definition's validity (§4.9.3): a registered
// name@version is a historical fact — THESE BYTES WERE REGISTERED — not a live
// claim that they still satisfy today's rules. Retroactive invalidation would
// break the pinning property outright: if a migration could render a pinned
// definition illegal, upgrading the binary would stop an in-flight run, which is
// a strictly worse failure than the one re-validation would catch. What
// re-validation would have caught is caught anyway, at the only moment it
// matters — T3 parks an unorderable comparison with a reason rather than
// guessing.
func migrateV8ToV9(tx *sql.Tx) error {
	if _, err := tx.Exec(v9DDL); err != nil {
		return fmt.Errorf("migrating v8 to v9: %w", err)
	}

	// The columns cannot ride in v9DDL: ALTER TABLE ADD COLUMN is not idempotent
	// in SQLite, so the migration probes first and stays re-runnable, exactly as
	// v5, v6, and v7 do for their own columns.
	//
	// Each default is chosen so EVERY EXISTING ROW KEEPS ITS MEANING without
	// being rewritten (the never-mutate rule): `trust_cache.kind` defaults to
	// 'gate' because every row written before v9 recorded a gate's decision, and
	// `steps.materialized` defaults to 0 because every step that exists was
	// declared rather than minted by a hold.
	for _, col := range v9AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v8 to v9: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v8 to v9: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}

	if err := markStubbedArtifacts(tx); err != nil {
		return err
	}
	return seedBuiltinSchemas(tx)
}

// markStubbedArtifacts is §6.3 S2's migration half: the S3/S4 stubbed action
// artifacts gain `stub = 1` and their BYTES ARE NEVER REWRITTEN.
//
// The `{"stub":true,"payload":[…]}` wrapper stays exactly as it was written.
// Rewriting it would destroy the evidence that a computation did not run, which
// is the entire reason the marker exists (gates-trust §4.3 G2's argument,
// applied to artifacts) — and the never-mutate rule (reliability-delta §2.1)
// forbids it independently.
//
// The probe is a LIKE over the stored text rather than a JSON parse: the
// migration must read the bytes that were written at the time, and every writer
// of that wrapper produced the same literal prefix.
func markStubbedArtifacts(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`UPDATE artifacts SET stub = 1
		  WHERE stub = 0 AND payload IS NOT NULL AND payload LIKE '{"stub":true,%'`,
	); err != nil {
		return fmt.Errorf("migrating v8 to v9: marking stubbed artifacts: %w", err)
	}
	return nil
}

// seedBuiltinSchemas registers `aggregate@1` from the embedded document
// (TDD §7.6 E3).
//
// INSERT OR IGNORE, for two reasons that are one reason: the rewind guard
// re-runs this migration BY DESIGN, so a second pass must duplicate nothing
// (U5); and a database whose `aggregate@1` row diverges from the embedded bytes
// is LEFT ALONE (U7). A database must never become unopenable because a binary
// changed — the divergence is caught at build time instead, by
// TestEmbeddedAggregateSchemaMatchesItsGolden.
//
// Seeding here rather than registering lazily at first use: a lazy registration
// writes to the database from a read path, races itself under concurrent engine
// invocations, and makes `schema list` depend on what has HAPPENED rather than
// on what is INSTALLED. This is one idempotent insert in a transaction that
// already exists.
func seedBuiltinSchemas(tx *sql.Tx) error {
	body := schema.AggregateBody()
	index, err := schema.DeriveIndex(body)
	if err != nil {
		return fmt.Errorf("migrating v8 to v9: the embedded %s document is unusable: %w",
			schema.AggregateRef(), err)
	}
	ordered, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("migrating v8 to v9: encoding the builtin's ordered index: %w", err)
	}

	// `created_at_ms` is the seed's own clock rather than a fabricated constant:
	// the row records when THIS database acquired the builtin, which is the
	// honest fact and the one an operator comparing two repos would want.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schemas
		   (name, version, source_path, source_sha256, body, ordered, builtin,
		    created_at_ms, row_version)
		 VALUES (?, ?, NULL, ?, ?, ?, 1, ?, 1)`,
		schema.AggregateName, schema.AggregateVersion, schema.AggregateSHA256(),
		string(body), string(ordered), model.NowMS(),
	); err != nil {
		return fmt.Errorf("migrating v8 to v9: seeding %s: %w", schema.AggregateRef(), err)
	}
	return nil
}

// v10Sentinels are the tables the v10 DDL creates. The rewind guard probes ALL
// of them, for the reason v7Sentinels states and this stage re-earns a fourth
// time (TDD docs/tdd/runs-dispatch.md §2.2).
//
// v10 ships as ONE migration function sliced across the stage's THREE commit
// groups — the widest span yet. The stamp moves to 10 in GROUP 1 — which needs
// `usage_ledger` — and does not move again, while migrateV9ToV10 keeps growing
// through groups 2 and 3. So a database migrated by a group-1 build, INCLUDING
// this repo's own dogfooded tracker, is stamped 10 with `usage_ledger` present
// and the later groups' tables absent. That is U2's shape exactly, and it is
// not hypothetical: the operator's tracker is migrated by whichever binary
// happens to be built between the groups.
//
// The list therefore GROWS WITH THE GROUPS, exactly as v7Sentinels grew across
// its four phases and v9Sentinels across its two: a group that adds a table to
// v10DDL adds it here in the same commit, and
// TestRewindGuardProbesEveryV10Sentinel derives the expected list from the DDL
// so the omission fails a test rather than shipping a half-migrated database.
// (Group 2 adds `dispatches`, `dispatch_rows`, and `reap_acks`; group 3 adds no
// table.)
//
// The COLUMN half is the part sentinels cannot see. v10 adds `runs.usage_floor`,
// `runs.breach_reason`, and `steps.usage_recorded` in group 1, each behind a
// hasColumn probe; they arrive only because the rewind re-runs the WHOLE
// migration. §2.4's U-table proves each column's arrival independently, and U3
// states plainly what a sentinel probe cannot catch.
var v10Sentinels = []string{
	"usage_ledger",
	// Group 2 (TDD §2.2's table): the manifest, its ordered rows, and the
	// write-reap acknowledgment ledger. They are listed HERE, in the commit
	// that adds them to v10DDL, because a group-1 binary has already stamped
	// this repo's tracker at 10 — U2 exactly — and without these entries the
	// rewind guard would look at that database, see `usage_ledger`, conclude
	// v10 is complete, and leave a group-2 binary running against a schema with
	// no `dispatches` table.
	"dispatches",
	"dispatch_rows",
	"reap_acks",
}

// v10IndexSentinels are the INDEXES the rewind guard must probe as well as the
// tables — group 3's addition, and the first time the span has needed one.
//
// WHY A SECOND LIST RATHER THAN A LONGER FIRST ONE. `tableExists` filters
// `sqlite_master` on `type='table'`, so an index name passed to it is always
// absent and would rewind every database on every open, forever. The probe is a
// different query, so the list it reads is a different list.
//
// WHY IT IS NEEDED AT ALL — U2, discovered against the operator's own tracker.
// Group 3 adds no table, so the table sentinels were all present in a database a
// group-2 binary had already stamped 10; the rewind never fired, the migration
// never re-ran, and `idx_events_seq` never arrived. The unfiltered events feed
// would then scan rather than seek, silently, on exactly the database that gets
// dogfooded through the stage. `CREATE INDEX IF NOT EXISTS` in the DDL is not
// enough on its own: the DDL only executes when something decides to run the
// migration, and the sentinel probe is that decision.
var v10IndexSentinels = []string{
	// The unfiltered `events list --since` feed scans by seq alone, and
	// `idx_events_run_seq` cannot serve a query with no run in it.
	"idx_events_seq",
}

// v10DDL is the v10 schema, assembled across the stage's three commit groups.
// Group 1 (TDD §3.1) contributes `usage_ledger` and its run index; group 2
// contributes `dispatches`, `dispatch_rows`, and `reap_acks`.
//
// THE GROUP-2 SHAPE DECISIONS, each per §3.1's own paragraph:
//
//   - `idx_dispatches_one_open` is a PARTIAL UNIQUE INDEX, and it — not a
//     check-then-insert — is what makes C1 true. Two relays calling
//     `dispatch open` in the same millisecond both compute a ready set and both
//     INSERT; SQLite admits exactly one, the loser's transaction fails, and the
//     loser's computation is DISCARDED rather than merged. A merge would produce
//     a manifest neither relay saw. §2 says "exactly one dispatch open per run
//     (CAS/unique index)", and this is that index.
//   - `dispatch_rows` stores BOTH `row_json` and `row_sha256`. `verify` is
//     "byte-equality on rows" (§11.4), so the bytes must be there to compare
//     against; the hash rides beside them as the pair's integrity check —
//     `verify` refuses when the stored bytes no longer hash to it — and as
//     the spawn guard's verbatim comparison key. The bytes are what lets a
//     refusal SHOW the operator the differing row instead of reporting
//     "they differ".
//   - `reap_acks.reaped_seq` is UNIQUE and references the EVENT. §2 says the
//     relay acknowledges "the `reaped` event", so the event's `seq` is the ack's
//     identity. That makes the ack idempotent for free (C7) and makes a forged
//     ack impossible to construct without naming a real reap.
//   - `idx_reap_acks_open` is partial on `acked_at_ms IS NULL`, which is what
//     makes D3's dormancy structural: the hold predicate is one indexed lookup
//     that returns no row on a database where nothing was ever reaped.
//
// `usage_ledger` is keyed `UNIQUE(step_id, attempt, unit)`, NOT by step alone.
// A step that is reaped and re-claimed produces a SECOND attempt with its own
// usage; summing over a step-unique key would silently overwrite the first
// attempt's numbers, and the report's attempt trail would show two attempts and
// one attempt's usage. The `attempt` column is what makes "retries re-accrue"
// (engine-core §7) true on the REPORTED side as well as the floor side.
//
// `unit` is an OPAQUE STRING and `quantity` a bare number. Core never
// enumerates units, never has a default unit, and never converts between them
// (§1.1). `source` exists because engine-core §7 says "source recorded"
// verbatim; core writes `'reported'` and no verb at this stage writes any other
// value, so a harness back-filling from its own journal can record its own
// source later without a migration.
//
// Additive and dormant: v10 creates a table and seeds NOTHING. No pre-existing
// verb reads `usage_ledger`, so the byte-compat claim (§3) is untouched.
//
// Every timestamp column created at v10 is `_ms INTEGER` epoch-milliseconds and
// no existing column's format is touched (reliability-delta §2.1).
const v10DDL = `
CREATE TABLE IF NOT EXISTS usage_ledger (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	step_id       INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	attempt       INTEGER NOT NULL DEFAULT 0,
	unit          TEXT    NOT NULL,
	quantity      REAL    NOT NULL,
	source        TEXT    NOT NULL DEFAULT 'reported',
	created_at_ms INTEGER NOT NULL,
	UNIQUE(step_id, attempt, unit)
);

CREATE INDEX IF NOT EXISTS idx_usage_ledger_run ON usage_ledger(run_id);

CREATE TABLE IF NOT EXISTS dispatches (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	status        TEXT    NOT NULL DEFAULT 'open',
	opened_seq    INTEGER NOT NULL,
	expires_ms    INTEGER NOT NULL,
	closed_at_ms  INTEGER,
	close_reason  TEXT,
	created_at_ms INTEGER NOT NULL,
	row_version   INTEGER NOT NULL DEFAULT 1
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_dispatches_one_open
	ON dispatches(run_id) WHERE status = 'open';

CREATE TABLE IF NOT EXISTS dispatch_rows (
	dispatch_id INTEGER NOT NULL REFERENCES dispatches(id) ON DELETE CASCADE,
	position    INTEGER NOT NULL,
	step_id     INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	instance    TEXT    NOT NULL,
	row_json    TEXT    NOT NULL,
	row_sha256  TEXT    NOT NULL,
	PRIMARY KEY (dispatch_id, position)
);

CREATE TABLE IF NOT EXISTS reap_acks (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id         INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	step_id        INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	class          TEXT    NOT NULL,
	reaped_seq     INTEGER NOT NULL,
	acked_at_ms    INTEGER,
	acked_by       TEXT,
	created_at_ms  INTEGER NOT NULL,
	UNIQUE(reaped_seq)
);

CREATE INDEX IF NOT EXISTS idx_reap_acks_open
	ON reap_acks(run_id) WHERE acked_at_ms IS NULL;

-- Group 3 adds NO TABLE — a read verb, two guards, and a directory scan create
-- nothing. It needs ONE index the v7 DDL did not create, because the events
-- read surface WITHOUT a run filter scans by seq alone, and idx_events_run_seq
-- cannot serve a query with no run in it (a composite index is unusable when
-- its leading column is absent from the predicate).
--
-- It IS a sentinel — v10IndexSentinels — and it has to be. Group 3 adds no
-- table, so a database a group-2 binary already stamped 10 passes every table
-- probe, the rewind never fires, the migration never re-runs, and this index
-- never arrives. CREATE INDEX IF NOT EXISTS only helps once something decides to
-- run the DDL, and the sentinel probe is that decision. Found against the
-- operator's own tracker, which is exactly the U2 database the guard exists for.
CREATE INDEX IF NOT EXISTS idx_events_seq ON events(seq);
`

// v10AddedColumns are the columns v10 adds to tables that already existed — the
// half the sentinels cannot see (TDD §2.3).
//
// They are a table rather than inline blocks so the U-table's per-column
// assertions and this list cannot drift: a group that adds a column adds one
// row here, and the migration iterates the same set the tests do.
//
// Each default is chosen so EVERY EXISTING ROW KEEPS ITS MEANING without being
// rewritten (the never-mutate rule):
//
//   - `runs.usage_floor` defaults to 0 because every pre-v10 run accrued
//     nothing, so 0 is its TRUE floor rather than a placeholder. It is a CACHE
//     FOR THE REPORT ONLY and is never the enforcement input (§4.3) — the floor
//     that decides is a SUM over claim events, computed in the deciding
//     transaction, and TestFloorIsNeverReadFromCache asserts the separation by
//     poisoning this column.
//   - `runs.breach_reason` defaults to NULL because no pre-v10 run was paused by
//     a budget it did not enforce.
//   - `steps.usage_recorded` defaults to 0 because every pre-v10 step recorded
//     no usage. Group 2's discrepancy probe reads it; group 1 WRITES it, so the
//     ledger's fast path is populated from the moment the ledger exists.
var v10AddedColumns = []struct{ table, column, ddl string }{
	{"runs", "usage_floor",
		`ALTER TABLE runs ADD COLUMN usage_floor REAL NOT NULL DEFAULT 0`},
	{"runs", "breach_reason",
		`ALTER TABLE runs ADD COLUMN breach_reason TEXT`},
	{"steps", "usage_recorded",
		`ALTER TABLE steps ADD COLUMN usage_recorded INTEGER NOT NULL DEFAULT 0`},
}

// v11AddedColumns is v11's whole schema change: one column on `workflows`
// marking a registered version as retired from BINDING.
//
// `deprecated_at_ms` defaults to NULL, and NULL means "binding" — so every
// pre-v11 row keeps its exact current meaning without being rewritten, per the
// never-mutate rule. A timestamp rather than a boolean because WHEN a version
// was retired is the audit question an operator actually asks, and a bare flag
// cannot answer it.
//
// RETIREMENT IS NOT DELETION. There is no DELETE anywhere on this table and
// this migration adds none: a retired row stays readable by explicit
// `@version` and by definitionByID, so a run that already pinned it keeps
// resolving it. The column is a BINDING-TIME FILTER and nothing else.
var v11AddedColumns = []struct{ table, column, ddl string }{
	{"workflows", "deprecated_at_ms",
		`ALTER TABLE workflows ADD COLUMN deprecated_at_ms INTEGER`},
}

// v11ColumnSentinels are the COLUMNS the rewind guard must probe — the third
// probe kind this ladder has needed, after tables and indexes.
//
// WHY A THIRD LIST RATHER THAN AN ENTRY IN EITHER EXISTING ONE. `tableExists`
// filters `sqlite_master` on `type='table'` and `indexExists` on `type='index'`;
// a COLUMN name handed to either is always absent, and the guard would then
// rewind every database on every open, forever. That is the same trap
// `v10IndexSentinels` documents, one level further down: a different probe
// needs a different list.
//
// v11 adds no table and no index, so without this the guard cannot see an
// incomplete v11 at all — every v10 sentinel is present in a database a v11
// binary has stamped, the rewind never fires, and a repo that was migrated by
// a binary built mid-change would silently never get the column.
var v11ColumnSentinels = []struct{ table, column string }{
	{"workflows", "deprecated_at_ms"},
}

// migrateV10ToV11 adds the deprecation marker.
//
// It BACK-FILLS NOTHING: no existing registered version is retired by an
// upgrade. Retirement is an operator act, and a migration that retired
// anything on its own would be deciding routing policy for a repo it knows
// nothing about.
func migrateV10ToV11(tx *sql.Tx) error {
	// ALTER TABLE ADD COLUMN is not idempotent in SQLite, so the migration
	// probes first and stays re-runnable — the same shape v5, v6, v7, v9, and
	// v10 use for their own columns.
	for _, col := range v11AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v10 to v11: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v10 to v11: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// v12DDL creates the projects table — the dimension that turns the database
// from single-tenant into a shared store.
//
// `identity` is the canonical project path config.Resolve derives (the git
// common dir with a trailing /.git stripped; the store-parent for legacy
// stores). The seed row's identity is EMPTY: a legacy database has exactly one
// project's data but cannot know which repository it belongs to from inside a
// migration, so row 1 is created UNCLAIMED and the first invocation that opens
// the store claims it with its own identity (EnsureProject). Every
// pre-existing row backfills to project 1, which is exactly right for a store
// that has only ever served one repository.
const v12DDL = `
CREATE TABLE IF NOT EXISTS projects (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	identity      TEXT NOT NULL UNIQUE,
	name          TEXT NOT NULL DEFAULT '',
	prefix        TEXT NOT NULL DEFAULT 'DKT',
	created_at_ms INTEGER NOT NULL DEFAULT 0
);

INSERT OR IGNORE INTO projects (id, identity, name) VALUES (1, '', 'default');
`

// v12IndexDDL runs AFTER the project_id columns exist.
const v12IndexDDL = `
CREATE INDEX IF NOT EXISTS idx_issues_project_id ON issues(project_id);
CREATE INDEX IF NOT EXISTS idx_docs_project_id ON docs(project_id);
CREATE INDEX IF NOT EXISTS idx_proposals_project_id ON proposals(project_id);
CREATE INDEX IF NOT EXISTS idx_runs_project_id ON runs(project_id);
`

// v12AddedColumns are the probed ALTERs — the tables whose scoping needs no
// constraint change. The columns carry no REFERENCES clause because SQLite
// refuses ADD COLUMN with a foreign key and a non-NULL default; referential
// integrity for these is application-owned, as it already is for every
// project-derived read.
var v12AddedColumns = []struct{ table, column, ddl string }{
	{"issues", "project_id",
		`ALTER TABLE issues ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1`},
	{"docs", "project_id",
		`ALTER TABLE docs ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1`},
	{"proposals", "project_id",
		`ALTER TABLE proposals ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1`},
	{"runs", "project_id",
		`ALTER TABLE runs ADD COLUMN project_id INTEGER NOT NULL DEFAULT 1`},

	// Execution context (G8). Runs record where and on what they started —
	// several worktrees of one project share a store, and a record that cannot
	// say which checkout it came from cannot be audited. Steps record where
	// the WORK happened (`--worktree` on complete/record), which is also what
	// the diff stage reads so a resumed saga diffs the right tree.
	{"runs", "exec_root",
		`ALTER TABLE runs ADD COLUMN exec_root TEXT NOT NULL DEFAULT ''`},
	{"runs", "branch",
		`ALTER TABLE runs ADD COLUMN branch TEXT NOT NULL DEFAULT ''`},
	{"runs", "commit_sha",
		`ALTER TABLE runs ADD COLUMN commit_sha TEXT NOT NULL DEFAULT ''`},
	{"runs", "hostname",
		`ALTER TABLE runs ADD COLUMN hostname TEXT NOT NULL DEFAULT ''`},
	{"steps", "work_root",
		`ALTER TABLE steps ADD COLUMN work_root TEXT NOT NULL DEFAULT ''`},
}

// v12Rebuilds are the tables whose UNIQUE constraints change shape — SQLite
// cannot alter a constraint, so each is rebuilt via the documented recipe:
// create the new shape under a temp name, copy with ids preserved (children
// reference these ids), drop, rename. Migrate disables foreign-key
// enforcement around this migration's transaction and runs foreign_key_check
// after, which is what makes the drop-and-rename safe.
var v12Rebuilds = []struct {
	table   string
	newDDL  string
	copySQL string
	postDDL string
}{
	{
		table: "labels",
		newDDL: `CREATE TABLE labels_v12 (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL DEFAULT 1 REFERENCES projects(id),
			name       TEXT NOT NULL,
			color      TEXT,
			version    INTEGER NOT NULL DEFAULT 1,
			UNIQUE(project_id, name)
		)`,
		copySQL: `INSERT INTO labels_v12 (id, project_id, name, color, version)
			SELECT id, 1, name, color, version FROM labels`,
	},
	{
		table: "workflows",
		newDDL: `CREATE TABLE workflows_v12 (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id       INTEGER NOT NULL DEFAULT 1 REFERENCES projects(id),
			name             TEXT    NOT NULL,
			version          INTEGER NOT NULL,
			description      TEXT,
			source_path      TEXT,
			source_sha256    TEXT    NOT NULL,
			body             TEXT    NOT NULL,
			parsed           TEXT    NOT NULL,
			created_at_ms    INTEGER NOT NULL,
			row_version      INTEGER NOT NULL DEFAULT 1,
			deprecated_at_ms INTEGER,
			UNIQUE(project_id, name, version)
		)`,
		copySQL: `INSERT INTO workflows_v12 (id, project_id, name, version, description,
				source_path, source_sha256, body, parsed, created_at_ms, row_version,
				deprecated_at_ms)
			SELECT id, 1, name, version, description, source_path, source_sha256,
				body, parsed, created_at_ms, row_version, deprecated_at_ms
			FROM workflows`,
		postDDL: `CREATE INDEX IF NOT EXISTS idx_workflows_name ON workflows(name)`,
	},
	{
		table: "schemas",
		newDDL: `CREATE TABLE schemas_v12 (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id    INTEGER NOT NULL DEFAULT 1 REFERENCES projects(id),
			name          TEXT    NOT NULL,
			version       INTEGER NOT NULL,
			source_path   TEXT,
			source_sha256 TEXT    NOT NULL,
			body          TEXT    NOT NULL,
			ordered       TEXT    NOT NULL DEFAULT '{}',
			builtin       INTEGER NOT NULL DEFAULT 0,
			created_at_ms INTEGER NOT NULL,
			row_version   INTEGER NOT NULL DEFAULT 1,
			UNIQUE(project_id, name, version)
		)`,
		copySQL: `INSERT INTO schemas_v12 (id, project_id, name, version, source_path,
				source_sha256, body, ordered, builtin, created_at_ms, row_version)
			SELECT id, 1, name, version, source_path, source_sha256, body, ordered,
				builtin, created_at_ms, row_version
			FROM schemas`,
		postDDL: `CREATE INDEX IF NOT EXISTS idx_schemas_name ON schemas(name)`,
	},
}

// v12Sentinels and v12ColumnSentinels are the rewind guard's probes. v12 ships
// in ONE commit group, so a single all-or-nothing transaction makes a partial
// v12 impossible in normal operation — the probes are insurance against the
// ladder's known trap recurring if a later change splits the group.
var v12Sentinels = []string{"projects"}

var v12ColumnSentinels = []struct{ table, column string }{
	{"issues", "project_id"},
	{"docs", "project_id"},
	{"proposals", "project_id"},
	{"runs", "project_id"},
	{"labels", "project_id"},
	{"workflows", "project_id"},
	{"schemas", "project_id"},
	{"runs", "exec_root"},
	{"steps", "work_root"},
}

// migrateV11ToV12 adds the projects dimension.
//
// Everything here is idempotent: CREATE IF NOT EXISTS, INSERT OR IGNORE,
// hasColumn-probed ALTERs, and rebuilds guarded by the same column probe (a
// table that already carries project_id is already the new shape).
func migrateV11ToV12(tx *sql.Tx) error {
	if _, err := tx.Exec(v12DDL); err != nil {
		return fmt.Errorf("migrating v11 to v12: %w", err)
	}

	for _, col := range v12AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v11 to v12: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v11 to v12: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}

	for _, rb := range v12Rebuilds {
		exists, err := hasColumn(tx, rb.table, "project_id")
		if err != nil {
			return fmt.Errorf("migrating v11 to v12: %w", err)
		}
		if exists {
			continue
		}
		for _, stmt := range []string{
			rb.newDDL,
			rb.copySQL,
			`DROP TABLE ` + rb.table,
			`ALTER TABLE ` + rb.table + `_v12 RENAME TO ` + rb.table,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("migrating v11 to v12: rebuilding %s: %w", rb.table, err)
			}
		}
		if rb.postDDL != "" {
			if _, err := tx.Exec(rb.postDDL); err != nil {
				return fmt.Errorf("migrating v11 to v12: reindexing %s: %w", rb.table, err)
			}
		}
	}

	if _, err := tx.Exec(v12IndexDDL); err != nil {
		return fmt.Errorf("migrating v11 to v12: creating indexes: %w", err)
	}
	return nil
}

// v13AddedColumns is v13's whole schema change: one column on `votes` carrying
// the casting seat's own opaque claim about what produced the vote — the
// column DKT-71 asks for.
//
// It is named `metadata`, not `model`/`effort`, and it holds a JSON object
// rather than two scalar strings. `scripts/qa/genericity.sh` scans core
// surface — flag names, JSON keys, column names, and single-line ALTER
// literals — for `model`/`prompt`/`llm`/`agent`/`brief`, and a `votes.model`
// or `votes.effort` column (or a `--model`/`--effort` flag) trips it exactly
// as a v10-style single-line `ALTER TABLE ... ADD COLUMN model` does. The
// repo already has the answer for data shaped like this: `steps.metadata`
// (schema.go's `steps` DDL) is the same opaque KV bag, and
// docs/tdd/completion-metadata.md's alternative D states it in words —
// "`--metadata` bags can carry their own provenance keys opaquely, which is
// precisely what the KV bag is for". A seat that wants to record which model
// and effort level cast its vote writes
// `{"model_resolved":"...","effort_resolved":"..."}` into `--metadata`; core
// never reads those keys, exactly as it never reads a step's.
//
// `metadata` defaults to NULL because every pre-v13 vote asserted nothing
// about what cast it, and NULL is that vote's true, unenriched state — not a
// placeholder standing in for an unknown claim.
//
// THE CLAIM IS UNVERIFIED, exactly like `voter_name` beside it: `vote cast`
// takes no token and no lease, and a vote is write-once per voter, so the
// first claim is permanent and nothing attests that it is true. That is
// tolerable only while nothing GATES on the value — and it does: the tally
// reads verdict, confidence and domain relevance and never this column. The
// first policy, gate or report that reads a key out of this bag re-opens
// that question at a severity none of it carries today.
var v13AddedColumns = []struct{ table, column, ddl string }{
	{"votes", "metadata",
		`ALTER TABLE votes ADD COLUMN metadata TEXT`},
}

// v13ColumnSentinels are the COLUMNS the rewind guard must probe, the same
// probe kind v11 and v12 need and for the same reason: v13 adds no table and
// no index, so a database stamped 13 by a binary built mid-change carries
// every v12 sentinel, the rewind never fires, and `votes.metadata` never
// arrives — the exact U3 trap v11ColumnSentinels documents.
var v13ColumnSentinels = []struct{ table, column string }{
	{"votes", "metadata"},
}

// v14DDL is v14's whole schema change: the per-seat vote-spend ledger
// (DKT-95). A vote step is never claimed — its attempt is permanently 0 — so
// two seats' usage rows would collide on usage_ledger's UNIQUE(step_id,
// attempt, unit); the key that admits one row per seat is the VOTE's own id,
// which is per (proposal, voter) by construction. The vocabulary mirrors
// usage_ledger exactly — `unit` an opaque string core never interprets,
// `quantity` a finite non-negative REAL — so the run-level reader can sum
// both ledgers under one discipline. `docket dispatch backfill-usage`
// remains the writer for STEP-level vote spend; this table is the seat's own
// report at cast time, which is a fact backfill cannot reconstruct.
const v14DDL = `
CREATE TABLE IF NOT EXISTS vote_usage (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	vote_id       INTEGER NOT NULL REFERENCES votes(id) ON DELETE CASCADE,
	unit          TEXT    NOT NULL,
	quantity      REAL    NOT NULL,
	created_at_ms INTEGER NOT NULL,
	UNIQUE(vote_id, unit)
);

CREATE INDEX IF NOT EXISTS idx_vote_usage_vote ON vote_usage(vote_id);
`

// v14Sentinels are the tables the v14 DDL creates — the rewind guard's probe,
// in the TABLE form v5's guard uses: a database stamped 14 by a binary built
// mid-change that never created `vote_usage` must rewind to 13 and re-run,
// or every `vote cast --usage` fails with `no such table`. One sentinel, one
// commit group; index sentinels are unnecessary here because the index rides
// the same IF NOT EXISTS DDL in the same transaction as the table.
var v14Sentinels = []string{"vote_usage"}

// migrateV13ToV14 creates the per-seat vote-spend ledger. IF NOT EXISTS makes
// it idempotent, and it BACK-FILLS NOTHING: no pre-v14 seat reported spend,
// and inventing rows would make the ledger an opinion about history.
func migrateV13ToV14(tx *sql.Tx) error {
	if _, err := tx.Exec(v14DDL); err != nil {
		return fmt.Errorf("migrating v13 to v14: %w", err)
	}
	return nil
}

// v15AddedColumns is v15's whole schema change: one column on `artifacts`
// naming the artifact this one REVISES (DKT-70).
//
// A held cluster's resolution records a NEW artifact rather than annotating the
// old one — H13, deliberately: what the engine computed and what the operator
// accepted are two records, and rewriting the first would destroy the evidence
// of what was actually held. The two share a `kind`, and they share a `sha256`
// too, because the BODY is unchanged and only the structured payload gains the
// resolution markers.
//
// That made the pair indistinguishable to anything counting rows. A run report
// showed ARTIFACT-71 and ARTIFACT-81 both as `findings` from `reconcile@0`,
// same hash, same 79 bytes, and ledger mining — which counts artifacts as
// evidence of work — read one operator decision as a second unit of work.
//
// `supersedes` resolves it WITHOUT touching either record: the original is
// still immutable and still addressable, the revision still reads through the
// same `kind`, and a consumer counting work counts rows where `supersedes IS
// NULL`. NULL is the default and every pre-v15 artifact keeps it, because a
// migration cannot know which historical pairs were revisions — inferring them
// from matching hashes would be the migration asserting a fact about history
// it does not have.
var v15AddedColumns = []struct{ table, column, ddl string }{
	{"artifacts", "supersedes",
		`ALTER TABLE artifacts ADD COLUMN supersedes INTEGER REFERENCES artifacts(id)`},
}

// v15ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v13 needs and for the same reason: v15 adds no table and no index, so a
// database stamped 15 by a binary built mid-change carries every v14 sentinel,
// the rewind never fires, and `artifacts.supersedes` never arrives.
var v15ColumnSentinels = []struct{ table, column string }{
	{"artifacts", "supersedes"},
}

// migrateV14ToV15 adds the artifact revision column.
//
// It BACK-FILLS NOTHING, per v15AddedColumns' own reasoning. `ALTER TABLE ADD
// COLUMN` is not idempotent in SQLite, so the migration probes first and stays
// re-runnable, the same shape v10 through v13 use for their added columns.
func migrateV14ToV15(tx *sql.Tx) error {
	for _, col := range v15AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v14 to v15: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v14 to v15: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// v16AddedColumns is v16's whole schema change: one column on `steps` — the
// attempt the step's retry budget counts FROM (DKT-86, DKT-90).
//
// `step resolve --as retry` used to zero `attempt` itself, which collided two
// counters that had silently shared one column: the retry budget `max_attempts`
// bounds, and the ledger key `usage_ledger` records under (`(step, attempt,
// unit)`). A step that recorded, parked at a gate, and was retried re-executed
// under an attempt number the ledger had already seen, so the second
// execution's genuinely distinct usage was permanently unrecordable through
// `dispatch backfill-usage` — the refusal `backfill-usage`'s own help promised
// could not happen ("a retried step's second attempt records beside its
// first").
//
// `attempt_base` splits the counters: `attempt` is now MONOTONIC for the
// step's whole life (claims made, ever — the meaning §11.4's `next` row always
// documented), and retry resets the BUDGET by setting `attempt_base = attempt`
// so exhaustion compares `attempt - attempt_base` against `max_attempts`.
// DEFAULT 0 makes every pre-v16 row read exactly as it did: no retry has ever
// moved its base, so the difference IS the attempt count.
var v16AddedColumns = []struct{ table, column, ddl string }{
	{"steps", "attempt_base",
		`ALTER TABLE steps ADD COLUMN attempt_base INTEGER NOT NULL DEFAULT 0`},
}

// v16ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v13's and v15's use and for the same reason: v16 adds no table and no
// index, so a database stamped 16 by a binary built mid-change carries every
// v15 sentinel, the rewind never fires, and `steps.attempt_base` never
// arrives.
var v16ColumnSentinels = []struct{ table, column string }{
	{"steps", "attempt_base"},
}

// migrateV15ToV16 adds the retry-budget base column.
//
// It BACK-FILLS NOTHING: 0 is the correct base for every existing step — a
// base only ever moves at `step resolve --as retry`, and the old reset erased
// the very history a back-fill would need. `ALTER TABLE ADD COLUMN` is not
// idempotent in SQLite, so the migration probes first and stays re-runnable,
// the same shape v10 through v15 use for their added columns.
func migrateV15ToV16(tx *sql.Tx) error {
	for _, col := range v16AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v15 to v16: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v15 to v16: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// v17AddedColumns is v17's whole schema change: one column on `vote_usage` —
// who measured the row (DKT-115).
//
// The cast-time writer records a seat's OWN report; the vote-scoped back-fill
// (`docket vote backfill-usage`) records a relay's reconstruction from its
// transcripts. Without the column the two were indistinguishable, which is
// exactly the distinction `usage_ledger.source` exists to preserve on the step
// side. DEFAULT 'reported' makes every pre-v17 row read as what it was: only
// seats could write here before the back-fill existed.
var v17AddedColumns = []struct{ table, column, ddl string }{
	{"vote_usage", "source",
		`ALTER TABLE vote_usage ADD COLUMN source TEXT NOT NULL DEFAULT 'reported'`},
}

// v17ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v13's, v15's, and v16's use and for the same reason: v17 adds no table
// and no index, so a database stamped 17 by a binary built mid-change carries
// every v16 sentinel, the rewind never fires, and `vote_usage.source` never
// arrives.
var v17ColumnSentinels = []struct{ table, column string }{
	{"vote_usage", "source"},
}

// migrateV16ToV17 adds the vote-usage source column.
//
// It BACK-FILLS NOTHING: 'reported' is the correct source for every existing
// row — only the cast-time writer existed before v17, and a cast-time row is a
// seat's own report by definition. `ALTER TABLE ADD COLUMN` is not idempotent
// in SQLite, so the migration probes first and stays re-runnable, the same
// shape v10 through v16 use for their added columns.
func migrateV16ToV17(tx *sql.Tx) error {
	for _, col := range v17AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v16 to v17: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v16 to v17: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// v18AddedColumns is v18's whole schema change: one column on `issues` — how
// the issue left the machine's hands, when it left them abandoned (DKT-245).
//
// `status` answers what state the issue is in; it cannot also answer whether
// the run gave up on it. An issue whose fix step completed is `done`, and
// `abandon-issue` deliberately does not overwrite that — the run stopping work
// is a statement about the RUN, and forcing a terminal status would take the
// operator's triage decision away. But leaving only `done` behind meant the
// tracker rendered "✔ done" for work a review had reproduced as NOT fixed.
// The empty default makes every pre-v18 issue read as what it was: unresolved
// by any routing.
var v18AddedColumns = []struct{ table, column, ddl string }{
	{"issues", "resolution",
		`ALTER TABLE issues ADD COLUMN resolution TEXT NOT NULL DEFAULT ''`},
}

// v18ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v13's, v15's, v16's, and v17's use and for the same reason: v18 adds no
// table and no index, so a database stamped 18 by a binary built mid-change
// carries every v17 sentinel, the rewind never fires, and `issues.resolution`
// never arrives.
var v18ColumnSentinels = []struct{ table, column string }{
	{"issues", "resolution"},
}

// migrateV17ToV18 adds the issue resolution column.
//
// It BACK-FILLS NOTHING. An issue abandoned before v18 left an `issue-
// abandoned` event and nothing on the row, and the event log is where that
// history stays — inventing a resolution for it now would assert, on rows
// nobody re-examined, a fact the store never recorded. `ALTER TABLE ADD
// COLUMN` is not idempotent in SQLite, so the migration probes first and stays
// re-runnable, the same shape v10 through v17 use for their added columns.
func migrateV17ToV18(tx *sql.Tx) error {
	for _, col := range v18AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v17 to v18: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v17 to v18: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// v19AddedColumns is v19's whole schema change: one column on `runs` —
// `usage_budget`, the cap over MEASURED usage (DKT-238).
//
// It is a SECOND dimension, not a replacement. `runs.budget` counts declared
// expected costs; this counts what the ledger actually recorded. The two are
// not commensurable and must not be collapsed: a raise tribunal deliberated
// 140 -> 280 declared "units" while measured usage across the same run was
// 4,838,739 output / 689,187,075 cache-read tokens, and its own security seat
// said so — "the budget cap tracks declared step costs, not actual token
// spend ... so 280 is a floor-only proxy". One number cannot answer both
// questions, and `max()` over the two would let the token count swamp the
// declared discipline entirely.
//
// The zero default means unlimited, exactly as `budget` does, so every
// existing run is unchanged and the dimension is dormant until asked for.
var v19AddedColumns = []struct{ table, column, ddl string }{
	{"runs", "usage_budget",
		`ALTER TABLE runs ADD COLUMN usage_budget REAL NOT NULL DEFAULT 0`},
}

// v19ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v13 through v18 use and for the same reason: v19 adds no table and no
// index, so a database stamped 19 by a binary built mid-change carries every
// v18 sentinel, the rewind never fires, and `runs.usage_budget` never arrives.
var v19ColumnSentinels = []struct{ table, column string }{
	{"runs", "usage_budget"},
}

// migrateV18ToV19 adds the measured-usage cap column.
//
// It BACK-FILLS NOTHING, and the zero it defaults to is the honest answer: no
// run started before v19 declared a cap over measured usage, so every one of
// them is unlimited in that dimension — which is what they were enforced as.
// `ALTER TABLE ADD COLUMN` is not idempotent in SQLite, so the migration
// probes first and stays re-runnable, the same shape v10 through v18 use.
func migrateV18ToV19(tx *sql.Tx) error {
	for _, col := range v19AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v18 to v19: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v18 to v19: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// v20AddedColumns is v20's whole schema change: one column on `run_issues` —
// `loop_grants`, the number of ADDITIONAL fix loops an operator has authorized
// for one issue in one run (DKT-237).
//
// It exists because a fix loop that exhausts `max_fix_loops` parks, and
// nothing could then mint a fresh round. After HRN-26's third round verify-ac
// read 7/14 acceptance criteria unmet and design-qa held 2 blockers, the
// workflow scheduled no further fix round and no verb could ask for one — so
// the fix was built OUTSIDE the engine: a general-purpose agent, one commit of
// 1,128 insertions, cherry-picked with no judge review as a step, and ~100,923
// output / 21.9M cache-read tokens in no ledger.
//
// A GRANT rather than a raised `max_fix_loops`, because the two say different
// things. The workflow's bound is the author's standing policy over every
// issue it matches; a grant is one operator's decision about ONE issue, on one
// occasion, recorded where an auditor can see who reopened what. Editing the
// bound to unstick one issue would quietly loosen it for every issue after.
var v20AddedColumns = []struct{ table, column, ddl string }{
	{"run_issues", "loop_grants",
		`ALTER TABLE run_issues ADD COLUMN loop_grants INTEGER NOT NULL DEFAULT 0`},
}

// v20ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v13 through v19 use and for the same reason: v20 adds no table and no
// index, so a database stamped 20 by a binary built mid-change carries every
// v19 sentinel and `run_issues.loop_grants` never arrives.
var v20ColumnSentinels = []struct{ table, column string }{
	{"run_issues", "loop_grants"},
}

// v21AddedColumns is v21's whole schema change: one column on `gate_results` —
// `stub_entry`, whether the trust entry that authorized this gate declared
// itself a PLACEHOLDER (DKT-265).
//
// It exists because RUN-17's build, secret-scan, and tests all recorded `pass`
// and every one of them was an echo stub, while RUN-19's secret-scan and
// ac-commands passed via `/usr/bin/true`. Nothing in gate_results, the run
// report, or the review packet distinguished those rows from a scanner that ran
// and found nothing, and a reviewer reading "secret-scan: pass" reasonably
// concludes a secret scan happened.
//
// IT IS NOT `stub`, AND THE TWO MUST NOT BE CONFLATED. The existing
// `gate_results.stub` means "this row was migrated from an S3 `gate_trail`" — a
// fact about which era of this codebase produced the row, set by the migration
// and by nothing the real runner does. `stub_entry` means "the command that ran
// was a placeholder" — a fact about the assurance. A single column carrying
// both would answer neither question, which is the same erasure DKT-258 files
// against the run report's status column.
var v21AddedColumns = []struct{ table, column, ddl string }{
	{"gate_results", "stub_entry",
		`ALTER TABLE gate_results ADD COLUMN stub_entry INTEGER NOT NULL DEFAULT 0`},
}

// v21ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v13 through v20 use and for the same reason: v21 adds no table and no
// index, so a database stamped 21 by a binary built mid-change carries every
// v20 sentinel and `gate_results.stub_entry` never arrives.
var v21ColumnSentinels = []struct{ table, column string }{
	{"gate_results", "stub_entry"},
}

// migrateV20ToV21 adds the hollow-assurance column.
//
// It BACK-FILLS NOTHING, and zero is the only defensible value for every
// existing row. A pre-v21 `pass` is a row whose assurance is UNKNOWN — the
// trust store carried no stub marker when it was written, so nothing recorded
// whether a scanner or an echo produced it. Stamping those rows 1 would invent
// a hollowness nobody declared; stamping them 0 says "not declared hollow",
// which is exactly true and is what the default already means. The distinction
// only becomes readable going forward, which is the honest outcome for a fact
// that was never captured.
//
// `ALTER TABLE ADD COLUMN` is not idempotent in SQLite, so the migration probes
// first and stays re-runnable, the same shape v10 through v20 use.
func migrateV20ToV21(tx *sql.Tx) error {
	for _, col := range v21AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v20 to v21: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v20 to v21: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// v22AddedColumns is v22's whole schema change: one column on `runs` —
// `pause_origin`, WHERE a `waiting-human` run's park was decided (DKT-305).
//
// A run reaches `waiting-human` two ways, and until now the row recorded no
// difference between them. The rollup parks a run because a STEP is parked
// (`reconcileRun`, `parked > 0`), and that park un-parks itself: when the step
// resolves, the same rollup returns the run to `active`, which is right —
// nobody should have to type `run resume` for a park the engine created and
// the engine resolved. The other way is a RUN-LEVEL decision — `docket run
// pause`, or a budget breach — where no step is parked at all, so the rollup's
// `parked` count reads zero and its default branch resumed the run out from
// under the operator on the next step to route.
//
// DKT-68 caught half of this and guarded the budget half with `breach_reason`.
// The other half was live until DKT-305: RUN-31 was paused by its operator at
// seq 3054 and auto-resumed at seq 3077 with an empty-data `run-resumed`, then
// ran a four-judge review, a synthesize, a reconcile, and two fresh votes
// unattended. `breach_reason` could not be the marker for that pause — it is
// the budget's own record, and overloading it would make a non-budget pause
// claim a breach that never happened.
//
// So the fact gets its own column, stating WHICH decision parked the run:
// `operator` for the verb, `budget` for the breach, and the empty default for
// a run whose park (if any) came from its steps. The rollup reads exactly this
// and declines to resume any run-level park; the step-level park keeps its
// automatic resume unchanged.
var v22AddedColumns = []struct{ table, column, ddl string }{
	{"runs", "pause_origin",
		`ALTER TABLE runs ADD COLUMN pause_origin TEXT NOT NULL DEFAULT ''`},
}

// v22ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v13 through v21 use and for the same reason: v22 adds no table and no
// index, so a database stamped 22 by a binary built mid-change carries every
// v21 sentinel, the rewind never fires, and `runs.pause_origin` never arrives.
var v22ColumnSentinels = []struct{ table, column string }{
	{"runs", "pause_origin"},
}

// migrateV21ToV22 adds the pause-origin column and, unlike v11 through v21,
// DOES back-fill — for the one row shape whose origin the stored state already
// proves.
//
// The back-fill invents nothing. A run sitting at `waiting-human` RIGHT NOW
// with a live `breach_reason` was parked by the budget: that is what
// `BreachRunBudgetTx` writes and the only writer of that column. And a run at
// `waiting-human` with NO parked step and no breach can only have arrived
// there through a run-level verb — the rollup parks a run exclusively when it
// counts a parked step, so a park with none standing was somebody's `run
// pause`. Both facts are read off the rows, not guessed.
//
// Leaving those rows at the empty default would ship the defect one more time
// on exactly the databases that have it: a run parked at the moment of upgrade
// would come up unmarked, and the next step to route would resume it — the
// DKT-305 reproduction, now caused by the fix for it. Runs already resumed,
// done, or abandoned are untouched: their park is history, and history is in
// the event log.
//
// `ALTER TABLE ADD COLUMN` is not idempotent in SQLite, so the migration probes
// first and stays re-runnable, the same shape v10 through v21 use; the
// back-fill is a no-op on re-run because it only ever writes rows still holding
// the empty default.
func migrateV21ToV22(tx *sql.Tx) error {
	for _, col := range v22AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v21 to v22: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v21 to v22: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}

	if _, err := tx.Exec(
		`UPDATE runs
		    SET pause_origin = ?
		  WHERE status = ? AND pause_origin = ''
		    AND breach_reason IS NOT NULL AND breach_reason != ''`,
		string(model.RunPauseOriginBudget), string(model.RunWaitingHuman),
	); err != nil {
		return fmt.Errorf("migrating v21 to v22: marking budget-paused runs: %w", err)
	}

	if _, err := tx.Exec(
		`UPDATE runs
		    SET pause_origin = ?
		  WHERE status = ? AND pause_origin = ''
		    AND id NOT IN (SELECT run_id FROM steps WHERE status = ?)`,
		string(model.RunPauseOriginOperator), string(model.RunWaitingHuman),
		string(StepWaitingHuman),
	); err != nil {
		return fmt.Errorf("migrating v21 to v22: marking operator-paused runs: %w", err)
	}

	return nil
}

// v23AddedColumns is v23's whole schema change: two counters on `steps` —
// `failed_attempts` and `reaped_claims`, the OUTCOME breakdown of the claims
// `attempt` counts (DKT-490).
//
// `attempt` is a claims-so-far spent-count and says nothing about how each
// claim ended, and that silence misled two independent consumers in one run:
// an escalation policy read it as "attempts that failed" and walked one
// escalation hop too many when a claim had merely been reaped (a lease expiry
// spends a claim without anything failing), and a human surface presented the
// pre-claim sample as the count while `step show` reported the post-claim one.
// Documentation fixes the sampling half; the failure-vs-reap half needs a fact
// no column carried. The event log records both endings (`step-failed`,
// `lease-reaped`) but is prunable and per-row derivation from it is exactly
// the consumer-side counting these columns exist to replace.
//
// So the row states it: `failed_attempts` counts claims ended by an explicit
// `step fail`, `reaped_claims` counts claims reaped without one (lease expiry,
// `max_step_duration`, `step reap`). A claim that RECORDED counts in neither —
// its ending is the artifact — and `step resolve --as retry` touches neither,
// exactly as it leaves `attempt` alone. failed + reaped never exceeds
// `attempt`; the remainder is live claims, recorded completions, and pre-v23
// history.
var v23AddedColumns = []struct{ table, column, ddl string }{
	{"steps", "failed_attempts",
		`ALTER TABLE steps ADD COLUMN failed_attempts INTEGER NOT NULL DEFAULT 0`},
	{"steps", "reaped_claims",
		`ALTER TABLE steps ADD COLUMN reaped_claims INTEGER NOT NULL DEFAULT 0`},
}

// v23ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v13 through v22 use and for the same reason: v23 adds no table and no
// index, so a database stamped 23 by a binary built mid-change carries every
// v22 sentinel, the rewind never fires, and the breakdown columns never arrive.
var v23ColumnSentinels = []struct{ table, column string }{
	{"steps", "failed_attempts"},
	{"steps", "reaped_claims"},
}

// migrateV22ToV23 adds the attempt-breakdown columns.
//
// It BACK-FILLS NOTHING, back to the v11–v21 default, and deliberately so
// despite the event log holding `step-failed` and `lease-reaped` rows a count
// could be derived from: events are PRUNABLE, so a derived count is a lower
// bound that decays with retention, and stamping it into a column would make
// the row assert more than the store can promise. v22's precedent allows a
// back-fill that reads facts standing on the rows; a log is not that. Zero on
// a pre-v23 claim therefore means "no recorded breakdown", the same
// never-captured honesty as v21's stub marker — the counters are authoritative
// only for claims that ended after v23.
//
// `ALTER TABLE ADD COLUMN` is not idempotent in SQLite, so the migration probes
// first and stays re-runnable, the same shape v10 through v22 use.
func migrateV22ToV23(tx *sql.Tx) error {
	for _, col := range v23AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v22 to v23: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v22 to v23: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// v24Sentinels is the table the v24 DDL creates, probed by the rewind guard in
// the TABLE form v7 and v8 use. TestRewindGuardProbesEveryV24Sentinel derives
// the list from the DDL, so an added table cannot ship without its sentinel.
var v24Sentinels = []string{
	"gate_override_grants",
}

// v24DDL is the run-scoped batch gate-override grant (DKT-546): one operator
// ruling that a gate's failure signature is environmental, covering later
// steps of the SAME run that fail the same gate with the same exit and reason.
//
// It is a TABLE rather than a `run_issues` column (the v20 loop-grant shape)
// because a grant is keyed by (run, gate, signature), not by (run, issue) —
// one ruling covers every issue's steps in the run, which is the toil DKT-546
// measured. `exit` is nullable for gate_results' own reason: an `unmatched`
// gate never ran, and NULL is the honest encoding of "no process existed" —
// a NULL signature matches only NULL, never exit 0.
//
// `covered_steps` counts the steps the grant auto-passed, bumped in the same
// transaction as each auto-pass, so the ledger shows one tracked grant
// covering N steps rather than N unattributed passes.
const v24DDL = `
CREATE TABLE IF NOT EXISTS gate_override_grants (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id         INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	origin_step_id INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	gate           TEXT    NOT NULL,
	exit           INTEGER,
	reason         TEXT    NOT NULL DEFAULT '',
	note           TEXT    NOT NULL DEFAULT '',
	covered_steps  INTEGER NOT NULL DEFAULT 0,
	created_at_ms  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_gate_override_grants_run
	ON gate_override_grants(run_id);
`

// migrateV23ToV24 creates the batch gate-override grant table (DKT-546).
//
// Additive and dormant: it creates one new table and touches no existing one,
// so a run that never records a grant reads byte-identically to v23. There is
// nothing to back-fill — no operator has granted a batch override before the
// verb for granting one existed.
func migrateV23ToV24(tx *sql.Tx) error {
	if _, err := tx.Exec(v24DDL); err != nil {
		return fmt.Errorf("migrating v23 to v24: %w", err)
	}
	return nil
}

// v25Sentinels is the table the v25 DDL creates, probed by the rewind guard in
// the TABLE form v24 uses. TestRewindGuardProbesEveryV25Sentinel derives the
// list from the DDL, so an added table cannot ship without its sentinel.
var v25Sentinels = []string{
	"stale_target_waivers",
}

// v25DDL is the run-scoped stale-target waiver (DKT-742): one operator ruling
// that a specific (step instance, target sha) stale-target warning has been
// adjudicated, so dispatch open/verify stop re-firing it unchanged. RUN-52 saw
// the identical warning fire four times across DISPATCH-295/297/301, each
// firing costing an investigation, and the operator's standing waiver lived
// only in session memory where the engine could not see it.
//
// It is a TABLE beside gate_override_grants rather than a column because the
// two are the same shape — a standing adjudication matched by signature,
// run-scoped by the run_id foreign key, dead with its run. The signature here
// is (step_instance, target_sha): a different sha on the same row, or the same
// sha on a different row, is a different question and still warns.
//
// `target_sha` may be an unambiguous PREFIX of the recorded sha (>= 7 hex
// chars), because the warning an operator copies it from renders the sha at 12
// characters; matching is case-insensitive prefix.
const v25DDL = `
CREATE TABLE IF NOT EXISTS stale_target_waivers (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	step_instance TEXT    NOT NULL,
	target_sha    TEXT    NOT NULL,
	note          TEXT    NOT NULL DEFAULT '',
	created_at_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_stale_target_waivers_run
	ON stale_target_waivers(run_id);
`

// migrateV24ToV25 creates the stale-target waiver table (DKT-742).
//
// Additive and dormant, exactly as v24 was: it creates one new table and
// touches no existing one, so a run that never records a waiver reads
// byte-identically to v24. There is nothing to back-fill — no operator has
// waived a stale-target warning before the verb for waiving one existed.
func migrateV24ToV25(tx *sql.Tx) error {
	if _, err := tx.Exec(v25DDL); err != nil {
		return fmt.Errorf("migrating v24 to v25: %w", err)
	}
	return nil
}

// v26Sentinels is the table the v26 DDL creates, probed by the rewind guard in
// the TABLE form v24 and v25 use. TestRewindGuardProbesEveryV26Sentinel derives
// the list from the DDL, so an added table cannot ship without its sentinel.
var v26Sentinels = []string{
	"run_notes",
}

// v26DDL is the run-scoped note (DKT-1079): a standing statement whoever
// drives a run records ONCE, and every packet the run renders from then on
// carries — the channel a dispatcher had no way to reach a worker through.
// RUN-70's conductor learned before dispatch that a required gate fails on
// clean HEAD, got an operator disposition, and filed a tracking issue; nothing
// it could write reached the executor's packet (issue comments are an audit
// surface, never a context source, and the issue body froze at activation), so
// the executor re-derived the failure and filed a duplicate.
//
// It is a TABLE beside gate_override_grants and stale_target_waivers rather
// than a column because it is the same shape — standing operator context,
// run-scoped by the run_id foreign key, dead with its run. A note is
// append-only: there is no edit and no delete, because a packet that rendered
// a note and a packet that did not must be distinguishable in the record.
const v26DDL = `
CREATE TABLE IF NOT EXISTS run_notes (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	text          TEXT    NOT NULL,
	created_at_ms INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_run_notes_run
	ON run_notes(run_id);
`

// migrateV25ToV26 creates the run-note table (DKT-1079).
//
// Additive and dormant, exactly as v24 and v25 were: it creates one new table
// and touches no existing one, so a run that never records a note reads
// byte-identically to v25 — every context bundle and every packet. There is
// nothing to back-fill: no dispatcher recorded a note before the verb for
// recording one existed.
func migrateV25ToV26(tx *sql.Tx) error {
	if _, err := tx.Exec(v26DDL); err != nil {
		return fmt.Errorf("migrating v25 to v26: %w", err)
	}
	return nil
}

// v27AddedColumns is v27's whole schema change: one column on `steps` —
// `last_claim_end`, the END REASON of the most recent claim to leave this
// step, alongside the counters v23 already keeps (DKT-1279).
//
// `failed_attempts` and `reaped_claims` answer "how many claims of each kind
// has this step ever had", which is the wrong question when they disagree:
// RUN-80 DISPATCH-400 reaped ten leases after a session was killed mid-wave,
// the steps re-dispatched at `attempt` incremented, and wave.js/policy's
// on_failure escalation read that as "failed once" and routed all ten a tier
// up — the row said nothing about THIS re-offer following a reap rather than
// a `step fail`. A mixed history (one failure, then a reap, or the reverse)
// makes the aggregate counters ambiguous about which ending is the one this
// offer follows; a router needs the LAST one, not a tally.
//
// So the row states it directly: `last_claim_end` is overwritten by whichever
// of MarkStepAttemptFailedTx / MarkStepClaimReapedTx runs most recently,
// holding exactly `'failed'` or `'reaped'` — never a derived value, the same
// discipline v23's counters use. Empty for a step never claimed and for a
// step whose only claims recorded (there is nothing to attribute a "prior
// end" to).
var v27AddedColumns = []struct{ table, column, ddl string }{
	{"steps", "last_claim_end",
		`ALTER TABLE steps ADD COLUMN last_claim_end TEXT NOT NULL DEFAULT ''`},
}

// v27ColumnSentinels are the columns the rewind guard probes, the same probe
// kind v21 through v23 use and for the same reason: v27 adds no table and no
// index, so a database stamped 27 by a binary built mid-change carries every
// v26 sentinel and the claim-end column never arrives.
var v27ColumnSentinels = []struct{ table, column string }{
	{"steps", "last_claim_end"},
}

// migrateV26ToV27 adds the last-claim-end column (DKT-1279).
//
// It BACK-FILLS NOTHING, for v23's own reason: the event log holds
// `step-failed` and `lease-reaped` rows a value could be derived from, but
// events are prunable and a back-fill would assert more than the store can
// promise. An empty string on a pre-v27 claim means "no recorded ending",
// the same never-captured honesty v23's zero counters use — the column is
// authoritative only for claims that end after v27.
//
// `ALTER TABLE ADD COLUMN` is not idempotent in SQLite, so the migration
// probes first and stays re-runnable, the same shape v10 through v23 use.
func migrateV26ToV27(tx *sql.Tx) error {
	for _, col := range v27AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v26 to v27: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v26 to v27: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// migrateV19ToV20 adds the operator loop-grant column.
//
// It BACK-FILLS NOTHING, and zero is the correct value for every existing row:
// no operator has authorized an extra loop on an issue that predates the verb
// for authorizing one. `ALTER TABLE ADD COLUMN` is not idempotent in SQLite,
// so the migration probes first and stays re-runnable, the same shape v10
// through v19 use.
func migrateV19ToV20(tx *sql.Tx) error {
	for _, col := range v20AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v19 to v20: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v19 to v20: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// migrateV12ToV13 adds the vote provenance column.
//
// It BACK-FILLS NOTHING: no existing vote is retroactively credited with a
// model or effort it never asserted. `ALTER TABLE ADD COLUMN` is not
// idempotent in SQLite, so the migration probes first and stays re-runnable,
// the same shape v10, v11, and v12 use for their own added columns.
func migrateV12ToV13(tx *sql.Tx) error {
	for _, col := range v13AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v12 to v13: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v12 to v13: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// migrateV9ToV10 creates the usage ledger and adds v10's columns (TDD §3.1).
//
// It RE-VALIDATES NOTHING and BACK-FILLS NOTHING. A run that completed before
// v10 has no ledger rows and no floor, and inventing either would be the
// migration asserting a fact about history it cannot know — the same stance
// migrateV8ToV9 takes toward registered definitions, for the same reason: an
// upgrade that changed what a finished run cost would make the ledger a
// derived opinion rather than a record.
func migrateV9ToV10(tx *sql.Tx) error {
	if _, err := tx.Exec(v10DDL); err != nil {
		return fmt.Errorf("migrating v9 to v10: %w", err)
	}

	// The columns cannot ride in v10DDL: ALTER TABLE ADD COLUMN is not
	// idempotent in SQLite, so the migration probes first and stays re-runnable,
	// exactly as v5, v6, v7, and v9 do for their own columns.
	for _, col := range v10AddedColumns {
		exists, err := hasColumn(tx, col.table, col.column)
		if err != nil {
			return fmt.Errorf("migrating v9 to v10: %w", err)
		}
		if exists {
			continue
		}
		if _, err := tx.Exec(col.ddl); err != nil {
			return fmt.Errorf("migrating v9 to v10: adding %s.%s: %w",
				col.table, col.column, err)
		}
	}
	return nil
}

// tableExists reports whether the database has the named table.
func tableExists(db *sql.DB, name string) (bool, error) {
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`,
		name,
	).Scan(&exists)
	return exists, err
}

// indexExists is tableExists for the other `sqlite_master` type, and it is a
// SEPARATE function rather than a parameterized one so a sentinel list cannot be
// passed to the wrong probe. An index name checked against `type='table'` is
// always absent, which would rewind every database on every open.
func indexExists(db *sql.DB, name string) (bool, error) {
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='index' AND name=?)`,
		name,
	).Scan(&exists)
	return exists, err
}

// hasColumnDB is hasColumn against the DATABASE rather than a migration's
// transaction — the probe the v11 rewind guard needs, which runs before any
// migration transaction is open. Same query, same meaning; only the handle
// differs.
func hasColumnDB(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(
		`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column)
	if err != nil {
		return false, fmt.Errorf("inspecting %s.%s: %w", table, column, err)
	}
	defer rows.Close()
	return rows.Next(), rows.Err()
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

	// Same defensive guard for v4 (TDD §5.1 S10): if stamped >=4 but the docs
	// table is absent, rewind to v3 and re-run. v4 DDL uses IF NOT EXISTS.
	if version >= 4 {
		var hasDocs bool
		err := db.QueryRow(
			`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='docs')`,
		).Scan(&hasDocs)
		if err == nil && !hasDocs {
			version = 3
		}
	}

	// Same defensive guard for v5: if stamped >=5 but idempotency_keys is
	// absent, rewind to v4 and re-run. The v5 migration is idempotent.
	if version >= 5 {
		hasIdempotency, err := tableExists(db, "idempotency_keys")
		if err == nil && !hasIdempotency {
			version = 4
		}
	}

	// Same defensive guard for v6: if stamped >=6 but the lease columns are
	// absent, rewind to v5 and re-run. The v6 migration probes each column
	// with hasColumn, so re-running is safe. This guard probes a COLUMN rather
	// than a table, since v6 adds no tables.
	if version >= 6 {
		hasOwner, err := hasColumnDB(db, "issues", "owner")
		if err == nil && !hasOwner {
			version = 5
		}
	}

	// The v7 guard, following the same pattern but probing the FULL sentinel
	// set: stamped >= 7 with ANY sentinel absent rewinds to 6 and re-runs. The
	// v7 migration is CREATE TABLE IF NOT EXISTS throughout, so re-running it
	// against a partially-migrated database is safe. See v7Sentinels for why
	// probing only the first table would be a silent trap for this stage.
	if version >= 7 {
		for _, table := range v7Sentinels {
			exists, err := tableExists(db, table)
			if err != nil {
				break
			}
			if !exists {
				version = 6
				break
			}
		}
	}

	// The v8 guard, the same shape as v7's and for the same reason (TDD §4.1,
	// §4.4 U3): stamped >= 8 with ANY v8 sentinel absent rewinds to 7 and
	// re-runs. The v8 migration is CREATE TABLE IF NOT EXISTS plus an
	// INSERT OR IGNORE backfill (G4), so re-running it against a
	// partially-migrated database adds what is missing and duplicates nothing.
	if version >= 8 {
		for _, table := range v8Sentinels {
			exists, err := tableExists(db, table)
			if err != nil {
				break
			}
			if !exists {
				version = 7
				break
			}
		}
	}

	// The v9 guard, the same shape as v7's and v8's and for the same reason
	// (TDD docs/tdd/payloads-thresholds.md §2, §4.4.1 U3): stamped >= 9 with ANY
	// v9 sentinel absent rewinds to 8 and re-runs. The v9 migration is CREATE
	// TABLE IF NOT EXISTS, a hasColumn-probed ALTER, an idempotent UPDATE, and
	// an INSERT OR IGNORE seed, so re-running it against a partially-migrated
	// database adds what is missing and duplicates nothing.
	//
	// This is the group-1/group-2 trap made harmless: `schemas` ships in group 1
	// and `action_results` in group 2, and the operator's own tracker is
	// migrated by whichever binary was built between them.
	if version >= 9 {
		for _, table := range v9Sentinels {
			exists, err := tableExists(db, table)
			if err != nil {
				break
			}
			if !exists {
				version = 8
				break
			}
		}
	}

	// The v10 guard, the same shape as v7's, v8's and v9's and for the same
	// reason (TDD docs/tdd/runs-dispatch.md §2.2, §2.4 U2): stamped >= 10 with
	// ANY v10 sentinel absent rewinds to 9 and re-runs. The v10 migration is
	// CREATE TABLE IF NOT EXISTS plus hasColumn-probed ALTERs throughout, so
	// re-running it against a partially-migrated database adds what is missing
	// and touches nothing else.
	//
	// This is the group-1/2/3 trap made harmless, at its widest: `usage_ledger`
	// ships in group 1 and the dispatch tables in group 2, and the operator's own
	// tracker is migrated by whichever binary was built between them.
	if version >= 10 {
		for _, table := range v10Sentinels {
			exists, err := tableExists(db, table)
			if err != nil {
				break
			}
			if !exists {
				version = 9
				break
			}
		}
	}
	// The INDEX half of the same guard, which group 3 is the first group to
	// need: it adds no table, so the sentinels above are all present in a
	// database a group-2 binary already stamped 10, the rewind never fires, and
	// `idx_events_seq` never arrives. Probing the index is what makes a
	// dogfooded tracker converge on the complete v10 rather than on the subset
	// whichever binary happened to migrate it.
	if version >= 10 {
		for _, index := range v10IndexSentinels {
			exists, err := indexExists(db, index)
			if err != nil {
				break
			}
			if !exists {
				version = 9
				break
			}
		}
	}

	// The v11 guard, in its COLUMN form — the third probe kind, for the same
	// reason the index half exists. v11 adds no table and no index, so
	// neither list above can see an incomplete v11: a database stamped 11 by a
	// binary built mid-change carries every v10 sentinel, the rewind never
	// fires, and `workflows.deprecated_at_ms` never arrives. Probing the column
	// is what makes that database converge.
	//
	// migrateV10ToV11 is a hasColumn-probed ALTER, so re-running it against a
	// partially-migrated database adds what is missing and touches nothing else.
	if version >= 11 {
		for _, col := range v11ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				break
			}
			if !exists {
				version = 10
				break
			}
		}
	}

	// The v12 guard, probing its table and its columns (both probe kinds this
	// ladder has needed). v12 ships in one group; see v12Sentinels for why the
	// probes exist anyway.
	if version >= 12 {
		for _, table := range v12Sentinels {
			exists, err := tableExists(db, table)
			if err != nil {
				break
			}
			if !exists {
				version = 11
				break
			}
		}
	}
	if version >= 12 {
		for _, col := range v12ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				break
			}
			if !exists {
				version = 11
				break
			}
		}
	}

	// The v13 guard, in its COLUMN form — the same trap v11's and v12's own
	// column guards document. v13 adds no table and no index, so a database
	// stamped 13 by a binary built mid-change carries every v12 sentinel, the
	// rewind never fires, and `votes.metadata` never arrives. Probing the
	// column is what makes that database converge.
	//
	// migrateV12ToV13 is a hasColumn-probed ALTER, so re-running it against a
	// partially-migrated database adds what is missing and touches nothing else.
	//
	// A PROBE THAT CANNOT RUN IS AN ERROR, not a present column. The guards
	// above swallow theirs and carry on, which makes "the column is there" and
	// "I could not look" the same answer — on a store stamped 13 whose
	// `votes.metadata` never arrived, that answer returns nil here and every
	// later vote cast fails with `no such column`, the exact trap this guard
	// exists to prevent. Returning is the honest reading; the older guards are
	// left alone because changing them is a class change to migrations this
	// issue does not touch.
	if version >= 13 {
		for _, col := range v13ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v13 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 12
				break
			}
		}
	}

	// The v14 guard, back in the TABLE form (v14 creates one): stamped 14
	// with `vote_usage` absent rewinds to 13, and the IF NOT EXISTS DDL
	// converges the re-run. A failed probe is an error, per the v13 guard's
	// own reasoning — "the column is there" and "I could not look" must not
	// be the same answer.
	if version >= 14 {
		for _, table := range v14Sentinels {
			exists, err := tableExists(db, table)
			if err != nil {
				return fmt.Errorf("probing %s for the v14 guard: %w", table, err)
			}
			if !exists {
				version = 13
				break
			}
		}
	}

	// The v15 guard, in the COLUMN form v13's uses and for its reason: v15 adds
	// no table and no index, so a database stamped 15 by a binary built
	// mid-change carries every v14 sentinel and `artifacts.supersedes` never
	// arrives. A failed probe is an error here too — "the column is there" and
	// "I could not look" must not be the same answer.
	if version >= 15 {
		for _, col := range v15ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v15 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 14
				break
			}
		}
	}

	// The v16 guard, in the same COLUMN form and for the same reason: v16 adds
	// no table and no index, so a database stamped 16 by a binary built
	// mid-change carries every v15 sentinel and `steps.attempt_base` never
	// arrives. A failed probe is an error — "the column is there" and "I could
	// not look" must not be the same answer.
	if version >= 16 {
		for _, col := range v16ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v16 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 15
				break
			}
		}
	}

	// The v17 guard, in the same COLUMN form and for the same reason: v17 adds
	// no table and no index, so a database stamped 17 by a binary built
	// mid-change carries every v16 sentinel and `vote_usage.source` never
	// arrives. A failed probe is an error — "the column is there" and "I could
	// not look" must not be the same answer.
	if version >= 17 {
		for _, col := range v17ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v17 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 16
				break
			}
		}
	}

	// The v18 guard, same COLUMN form, same reason: v18 adds no table and no
	// index, so a database stamped 18 by a binary built mid-change carries
	// every v17 sentinel and `issues.resolution` never arrives.
	if version >= 18 {
		for _, col := range v18ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v18 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 17
				break
			}
		}
	}

	// The v19 guard, same COLUMN form, same reason.
	if version >= 19 {
		for _, col := range v19ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v19 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 18
				break
			}
		}
	}

	// The v20 guard, same COLUMN form, same reason.
	if version >= 20 {
		for _, col := range v20ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v20 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 19
				break
			}
		}
	}

	// The v21 guard, same COLUMN form, same reason.
	if version >= 21 {
		for _, col := range v21ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v21 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 20
				break
			}
		}
	}

	// The v22 guard, same COLUMN form, same reason.
	if version >= 22 {
		for _, col := range v22ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v22 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 21
				break
			}
		}
	}

	// The v23 guard, same COLUMN form, same reason.
	if version >= 23 {
		for _, col := range v23ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v23 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 22
				break
			}
		}
	}

	// The v24 guard, back in the TABLE form v7 and v8 use and for their reason:
	// v24 adds a table and no columns, so a database stamped 24 by a binary
	// built mid-change carries every v23 sentinel and the grants table never
	// arrives. The v24 migration is CREATE TABLE IF NOT EXISTS throughout, so
	// re-running it against such a store is safe.
	if version >= 24 {
		for _, table := range v24Sentinels {
			exists, err := tableExists(db, table)
			if err != nil {
				break
			}
			if !exists {
				version = 23
				break
			}
		}
	}

	// The v25 guard, same TABLE form as v24 and for its reason: v25 adds a
	// table and no columns, so a database stamped 25 by a binary built
	// mid-change carries every v24 sentinel and the waiver table never
	// arrives. The v25 migration is CREATE TABLE IF NOT EXISTS throughout, so
	// re-running it against such a store is safe.
	if version >= 25 {
		for _, table := range v25Sentinels {
			exists, err := tableExists(db, table)
			if err != nil {
				break
			}
			if !exists {
				version = 24
				break
			}
		}
	}

	// The v26 guard, same TABLE form as v24 and v25 and for their reason: v26
	// adds a table and no columns, so a database stamped 26 by a binary built
	// mid-change carries every v25 sentinel and the note table never arrives.
	// The v26 migration is CREATE TABLE IF NOT EXISTS throughout, so re-running
	// it against such a store is safe.
	if version >= 26 {
		for _, table := range v26Sentinels {
			exists, err := tableExists(db, table)
			if err != nil {
				break
			}
			if !exists {
				version = 25
				break
			}
		}
	}

	// The v27 guard, back in the COLUMN form v21 through v23 use and for
	// their reason: v27 adds a column and no table, so a database stamped 27
	// by a binary built mid-change carries every v26 sentinel and the
	// claim-end column never arrives.
	if version >= 27 {
		for _, col := range v27ColumnSentinels {
			exists, err := hasColumnDB(db, col.table, col.column)
			if err != nil {
				return fmt.Errorf("probing %s.%s for the v27 guard: %w",
					col.table, col.column, err)
			}
			if !exists {
				version = 26
				break
			}
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

		// A rebuild migration needs foreign-key enforcement off around its
		// transaction — PRAGMA foreign_keys is a no-op inside one — and a
		// foreign_key_check before enforcement returns, so a rebuild that
		// orphaned a child row is an error rather than a latent corruption.
		fkOff := migrationsNeedingFKOff[v]
		if fkOff {
			if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
				return fmt.Errorf("disabling foreign keys for migration %d: %w", v, err)
			}
		}

		if err := applyMigration(db, v, migrateFn); err != nil {
			if fkOff {
				db.Exec(`PRAGMA foreign_keys=ON`)
			}
			return err
		}

		if fkOff {
			if err := verifyForeignKeys(db, v); err != nil {
				return err
			}
			if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
				return fmt.Errorf("re-enabling foreign keys after migration %d: %w", v, err)
			}
		}
	}

	return nil
}

// applyMigration runs one migration in its own transaction and stamps the
// version — the loop body Migrate always had, factored so the foreign-key
// toggling around it stays readable.
func applyMigration(db *sql.DB, v int, migrateFn func(tx *sql.Tx) error) error {
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
	return nil
}

// verifyForeignKeys runs PRAGMA foreign_key_check and reports the first
// violation. A rebuild that dropped a referenced row must fail loudly here,
// not surface later as a constraint error in an unrelated write.
func verifyForeignKeys(db *sql.DB, v int) error {
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("checking foreign keys after migration %d: %w", v, err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowid, parent, fkid any
		_ = rows.Scan(&table, &rowid, &parent, &fkid)
		return fmt.Errorf(
			"migration %d left a foreign-key violation in %s (rowid %v referencing %v)",
			v, table, rowid, parent)
	}
	return rows.Err()
}
