package db

import (
	"database/sql"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// v7Tables are the tables the v7 DDL creates, in the shape TDD §4.1, §5.1, and
// §6.1 specify. Phase 1 contributed `workflows`; phase 2 adds the activation
// slice; phase 3 adds the step lifecycle's `artifacts` and `step_inputs`;
// phase 4 (§7.1) adds `events`, alongside v7DDL and v7Sentinels.
var v7Tables = []string{
	"workflows",
	"runs", "run_issues", "pins", "run_fences", "steps",
	"artifacts", "step_inputs",
	"events",
}

func TestMigrateToV7(t *testing.T) {
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
	// The stage pin moved with stage 4: v7's own assertion lived here while S3
	// was the head stage, and S4 ships v8 (gates-trust §4.1). The pin now lives
	// in migrate_v8_test.go's TestMigrateToV8; what this test still owes is that
	// the v7 STRUCTURES survive a migration that now runs past them.
	if currentSchemaVersion < 7 {
		t.Errorf("currentSchemaVersion = %d; v7 structures are still required",
			currentSchemaVersion)
	}

	for _, table := range v7Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}

	for _, index := range []string{
		"idx_workflows_name", "idx_run_issues_issue", "idx_pins_run",
		"idx_steps_run_status", "idx_steps_expires_ms", "idx_steps_issue",
	} {
		if !hasIndex(t, db, index) {
			t.Errorf("%s missing after migration", index)
		}
	}

	// The one touch v7 makes to an existing table (TDD §5.1).
	if !hasIssueColumn(t, db, "scope_globs") {
		t.Error("issues.scope_globs missing after migration")
	}
}

// dropV7Tables removes every v7 table, dependents first. `run_issues`, `steps`,
// `pins`, and `run_fences` carry REFERENCES to `runs`/`workflows`/`issues`, and
// a drop that ignored the order would fail on the reference rather than on
// anything the test is about.
func dropV7Tables(t *testing.T, db *sql.DB) {
	t.Helper()
	// Reverse dependency order: leaves before the tables they reference.
	// `events` is the outermost leaf — it references runs, steps, AND issues —
	// so it goes first or the drop fails on the reference rather than on
	// anything a test is about. v8's `gate_results` and `trust_cache` reference
	// runs and steps too, so they join the leaves ahead of it, and v10's
	// `usage_ledger`, `dispatch_rows`, `dispatches`, and `reap_acks` reference
	// both as well. `dispatch_rows` precedes `dispatches` for the same reason
	// the whole list is ordered: it references it.
	for _, table := range []string{
		"usage_ledger", "dispatch_rows", "dispatches", "reap_acks",
		"action_results",
		"gate_override_grants",
		"gate_results", "trust_cache",
		"events",
		"step_inputs", "artifacts",
		"steps", "run_fences", "pins", "run_issues", "runs", "workflows",
	} {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("dropping %s: %v", table, err)
		}
	}
}

// restoreV7TablesExcept re-creates the v7 tables from v7DDL, omitting the one
// named — the half-migrated shape the rewind guard exists to detect.
//
// The DDL is replayed statement by statement rather than as one Exec, since
// omitting a table means omitting its CREATE and nothing else. Statements that
// reference the omitted table are skipped along with it: SQLite resolves a
// CREATE's foreign-key references lazily, but its INDEX statements do not
// resolve at all against a missing table.
func restoreV7TablesExcept(t *testing.T, db *sql.DB, omit string) {
	t.Helper()
	for _, stmt := range strings.Split(v7DDL, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		// Skip the omitted table's own CREATE and any index over it.
		if regexp.MustCompile(
			`(?i)(TABLE IF NOT EXISTS|ON)\s+` + regexp.QuoteMeta(omit) + `\b`,
		).MatchString(stmt) {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("restoring v7 DDL without %s: %v\nstatement: %s", omit, err, stmt)
		}
	}
}

// hasIssueColumn reports whether the issues table carries the named column.
func hasIssueColumn(t *testing.T, db *sql.DB, column string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pragma_table_info('issues') WHERE name = ?)`, column,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("probing issues.%s: %v", column, err)
	}
	return exists
}

// TestMigrateV7UpgradesAPhase1Database is the group-1 -> group-2 upgrade path,
// proven rather than assumed (TDD §2, §5.7).
//
// A database migrated by the PHASE-1 build is stamped 7 and carries `workflows`
// alone: the stamp moved at phase 1 while migrateV6ToV7 kept growing. That is
// the exact shape this repo's own dogfood tracker has when this group lands, so
// the guard either fires here or that database silently never gains the
// activation tables and fails much later with a missing table at `run activate`.
//
// The guard's sentinel set is what makes it fire — a guard probing only
// `workflows` would see the table present, do nothing, and leave the database
// half-migrated with a stamp claiming otherwise.
func TestMigrateV7UpgradesAPhase1Database(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Reconstruct the phase-1 shape: everything phase 2 added is dropped, the
	// stamp stays at 7, and a registered workflow row survives — a phase-1
	// database is not an empty one, and the upgrade must not lose its rows.
	// Reverse dependency order, as dropV7Tables uses: SQLite resolves a DROP's
	// foreign-key references, so a leaf must go before the table it points at.
	for _, table := range []string{
		"usage_ledger", "dispatch_rows", "dispatches", "reap_acks",
		"action_results",
		"gate_override_grants",
		"gate_results", "trust_cache",
		"events",
		"step_inputs", "artifacts",
		"steps", "run_fences", "pins", "run_issues", "runs",
	} {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("dropping %s: %v", table, err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO workflows
		   (name, version, source_sha256, body, parsed, created_at_ms, row_version)
		 VALUES ('phase-one', 1, 'abc', 'body', '{}', 1000, 1)`,
	); err != nil {
		t.Fatalf("seeding a phase-1 workflow row: %v", err)
	}

	// Restore the phase-1 STAMP. Migrate now runs past 7 to v8, so a database
	// built by running it is stamped 8; this test is about the shape a
	// phase-1 v7 binary left behind, which is stamped 7.
	if _, err := db.Exec(
		`UPDATE meta SET value = '7' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("restoring the phase-1 stamp: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 7 {
		t.Fatalf("schema_version = %d before the upgrade, want 7 (the phase-1 stamp)", v)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("upgrading a phase-1 database: %v", err)
	}

	for _, table := range v7Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after the phase-1 -> phase-2 upgrade", table)
		}
	}
	if !hasIssueColumn(t, db, "scope_globs") {
		t.Error("issues.scope_globs missing after the phase-1 -> phase-2 upgrade")
	}

	// The workflow registered by the phase-1 build survives: the upgrade adds
	// what is missing and touches nothing else.
	var name string
	if err := db.QueryRow(`SELECT name FROM workflows WHERE version = 1`).Scan(&name); err != nil {
		t.Fatalf("reading the phase-1 workflow row after the upgrade: %v", err)
	}
	if name != "phase-one" {
		t.Errorf("workflow name = %q after the upgrade, want %q", name, "phase-one")
	}
}

// TestMigrateV7UpgradesAPhase3Database is the group-3 -> group-4 upgrade path,
// proven rather than assumed (TDD §2, §7.1).
//
// It is the phase-1 test's situation one phase later and with a sharper edge. A
// database migrated by the PHASE-3 build is stamped 7 and has every table
// EXCEPT `events`, plus a `run_issues` table that predates `loop_count`. That is
// the exact shape this repo's own dogfood tracker has when this group lands —
// again — so the upgrade either works here or that database fails later with a
// missing `events` table on the first transition it tries to record.
//
// THE COLUMN IS THE PART THAT COULD SILENTLY NOT HAPPEN. `events` is a
// sentinel, so its absence fires the rewind guard; `loop_count` is a column on a
// table that already exists, so nothing probes for it directly. It arrives only
// because the guard re-runs the WHOLE migration and the ADD COLUMN is behind its
// own hasColumn probe. A future phase that added a column while dropping the
// re-run would leave `loop_count` missing on every dogfooded database, and the
// first fix-loop routing would fail on a missing column. This test is what
// notices.
func TestMigrateV7UpgradesAPhase3Database(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Reconstruct the phase-3 shape: `events` dropped, `loop_count` removed from
	// `run_issues`, the stamp left at 7. SQLite's DROP COLUMN (3.35+) does the
	// second; the whole point is a `run_issues` that exists WITHOUT the column,
	// which is what a phase-3 build produced.
	if _, err := db.Exec(`DROP TABLE IF EXISTS events`); err != nil {
		t.Fatalf("dropping events: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE run_issues DROP COLUMN loop_count`); err != nil {
		t.Fatalf("removing run_issues.loop_count: %v", err)
	}

	// A phase-3 database is not an empty one: it carries a run mid-flight. The
	// upgrade must add what is missing and lose none of it.
	seedPhase3Rows(t, db)

	// Restore the phase-3 STAMP: Migrate now runs past 7 to v8, so a database
	// built by running it is stamped 8, while this test is about the shape a
	// phase-3 v7 binary left behind.
	if _, err := db.Exec(
		`UPDATE meta SET value = '7' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("restoring the phase-3 stamp: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != 7 {
		t.Fatalf("schema_version = %d before the upgrade, want 7 (the phase-3 stamp)", v)
	}
	if hasColumn2(t, db, "run_issues", "loop_count") {
		t.Fatal("run_issues.loop_count present before the upgrade; the phase-3 shape was not reconstructed")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("upgrading a phase-3 database: %v", err)
	}

	// The slice arrived: the table, its index, and the column.
	for _, table := range v7Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after the phase-3 -> phase-4 upgrade", table)
		}
	}
	if !hasIndex(t, db, "idx_events_run_seq") {
		t.Error("idx_events_run_seq missing after the phase-3 -> phase-4 upgrade")
	}
	if !hasColumn2(t, db, "run_issues", "loop_count") {
		t.Error("run_issues.loop_count missing after the phase-3 -> phase-4 upgrade")
	}

	// DATA INTACT. The mid-flight run, its issue binding, and its step survive;
	// the new counter defaults to 0 on the pre-existing row, which is the
	// truthful reading — that issue has entered no loops.
	var (
		gotWorkflow string
		gotSnapshot string
		gotLoops    int
	)
	err = db.QueryRow(
		`SELECT w.name, ri.body_snapshot, ri.loop_count
		   FROM run_issues ri JOIN workflows w ON w.id = ri.workflow_id`,
	).Scan(&gotWorkflow, &gotSnapshot, &gotLoops)
	if err != nil {
		t.Fatalf("reading the phase-3 run_issues row after the upgrade: %v", err)
	}
	if gotWorkflow != "phase-three" || gotSnapshot != "the body" {
		t.Errorf("run_issues row = (%q, %q) after the upgrade, want (%q, %q)",
			gotWorkflow, gotSnapshot, "phase-three", "the body")
	}
	if gotLoops != 0 {
		t.Errorf("run_issues.loop_count = %d on a pre-existing row, want 0", gotLoops)
	}

	var gotInstance, gotStatus string
	err = db.QueryRow(`SELECT instance, status FROM steps`).Scan(&gotInstance, &gotStatus)
	if err != nil {
		t.Fatalf("reading the phase-3 step after the upgrade: %v", err)
	}
	if gotInstance != "implement@0" || gotStatus != "done" {
		t.Errorf("step = (%q, %q) after the upgrade, want (%q, %q)",
			gotInstance, gotStatus, "implement@0", "done")
	}

	// And the table the upgrade added is writable: a database that gained an
	// `events` table it cannot insert into has not really been upgraded.
	if _, err := db.Exec(
		`INSERT INTO events (at_ms, kind, run_id, data) VALUES (1000, 'run-started', 1, '{}')`,
	); err != nil {
		t.Fatalf("writing an event after the upgrade: %v", err)
	}
}

// seedPhase3Rows writes the rows a phase-3 build would have left behind: a
// registered workflow, an activated run, its bound issue, and one completed
// step.
func seedPhase3Rows(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO workflows (id, name, version, source_sha256, body, parsed, created_at_ms, row_version)
		 VALUES (1, 'phase-three', 1, 'abc', 'body', '{}', 1000, 1)`,
		`INSERT INTO issues (id, title, description, status, priority, kind, assignee, created_at, updated_at)
		 VALUES (1, 'mid-flight', 'body', 'in-progress', 'none', 'task', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO runs (id, request, status, created_at_ms, updated_at_ms)
		 VALUES (1, 'req', 'active', 1000, 1000)`,
		`INSERT INTO run_issues (run_id, issue_id, workflow_id, body_snapshot, body_sha256, expanded_at_ms)
		 VALUES (1, 1, 1, 'the body', 'sha', 1000)`,
		`INSERT INTO steps (id, run_id, issue_id, workflow_id, step_name, ordinal, instance,
		                    kind, status, created_at_ms, updated_at_ms)
		 VALUES (1, 1, 1, 1, 'implement', 0, 'implement@0', 'executor', 'done', 1000, 1000)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seeding a phase-3 row: %v\nstatement: %s", err, stmt)
		}
	}
}

// hasColumn2 reports whether a table carries the named column. It reads
// pragma_table_info off a *sql.DB, where the migration's own hasColumn takes a
// *sql.Tx.
func hasColumn2(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = ?)`, table, column,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("probing %s.%s: %v", table, column, err)
	}
	return exists
}

// TestRewindGuardProbesEverySentinel is the guard TDD §2 requires: one sentinel
// per table the v7 DDL creates. v7 ships as ONE migration assembled across four
// phases, so a phase that adds a table without extending v7Sentinels would
// leave the guard blind to a database this build stamped 7 and half-migrated —
// including this repo's own dogfood tracker. That failure is silent until
// activation hits a missing table, so it is caught here instead.
func TestRewindGuardProbesEverySentinel(t *testing.T) {
	// Derive the table names from the DDL itself rather than restating them:
	// a phase that adds a CREATE TABLE and forgets the sentinel fails here.
	re := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+(\w+)`)
	var created []string
	for _, m := range re.FindAllStringSubmatch(v7DDL, -1) {
		created = append(created, m[1])
	}
	if len(created) == 0 {
		t.Fatal("no CREATE TABLE statements found in v7DDL")
	}

	got := append([]string(nil), v7Sentinels...)
	sort.Strings(created)
	sort.Strings(got)

	if len(got) != len(created) {
		t.Fatalf("v7Sentinels = %v, but v7DDL creates %v", got, created)
	}
	for i := range created {
		if got[i] != created[i] {
			t.Errorf("v7Sentinels = %v, but v7DDL creates %v", got, created)
			break
		}
	}

	// And the documented table list agrees with the DDL, so the test above
	// cannot pass by both drifting together.
	want := append([]string(nil), v7Tables...)
	sort.Strings(want)
	if len(want) != len(created) {
		t.Fatalf("v7Tables = %v, but v7DDL creates %v", want, created)
	}
	for i := range created {
		if want[i] != created[i] {
			t.Errorf("v7Tables = %v, but v7DDL creates %v", want, created)
			break
		}
	}
}

// TestMigrateV6ToV7IsIdempotent covers the rewind guard end to end: a database
// stamped 7 with a sentinel missing is re-migrated rather than trusted. This is
// the exact shape a phase-1-migrated database has when phases 2-4 land.
func TestMigrateV6ToV7IsIdempotent(t *testing.T) {
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
	for _, table := range v7Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after re-migration", table)
		}
	}

	// Simulate the stamped-but-missing case the guard exists for: every
	// sentinel in turn, since the guard must probe all of them and not just
	// the first.
	//
	// Each iteration drops the WHOLE v7 set and then restores every table
	// except the one under test, so the database is left missing exactly that
	// sentinel with no dangling reference from a survivor. Dropping one table
	// in isolation is not available: SQLite resolves a DROP's foreign-key
	// references, so removing `workflows` while `steps` still points at it
	// fails on the reference rather than producing the shape being tested.
	for _, sentinel := range v7Sentinels {
		dropV7Tables(t, db)
		restoreV7TablesExcept(t, db, sentinel)
		// The stamp is forced to 7 — that is the trap this test builds: the
		// version says migrated, the schema says otherwise. It is set rather
		// than merely left alone because Migrate now runs on to v8.
		if _, err := db.Exec(
			`UPDATE meta SET value = '7' WHERE key = 'schema_version'`); err != nil {
			t.Fatalf("stamping the half-migrated database: %v", err)
		}
		v, err := SchemaVersion(db)
		if err != nil {
			t.Fatalf("SchemaVersion: %v", err)
		}
		if v != 7 {
			t.Fatalf("schema_version = %d before the guard runs, want 7", v)
		}

		if err := Migrate(db); err != nil {
			t.Fatalf("Migrate after dropping %s: %v", sentinel, err)
		}
		if !hasTable(t, db, sentinel) {
			t.Errorf("%s not restored by the rewind guard", sentinel)
		}
	}
}

// TestMigrateV6ToV7Dormancy is the §3 phase-1 obligation at the storage layer:
// v7 adds tables and touches no existing one, so a pre-v7 row survives byte for
// byte and the workflows table is empty on a repo that never registered one.
func TestMigrateV6ToV7Dormancy(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Rewind to a pre-v7 shape and seed a row that predates workflows.
	// Dropped in reverse dependency order: `run_issues` and `steps` reference
	// `workflows`, and SQLite refuses to drop a table another still references.
	dropV7Tables(t, db)
	if _, err := db.Exec(`UPDATE meta SET value = '6' WHERE key = 'schema_version'`); err != nil {
		t.Fatalf("rewinding schema_version: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO issues (title, description, status, priority, kind, assignee, created_at, updated_at)
		 VALUES ('legacy', 'body', 'todo', 'none', 'task', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seeding legacy issue: %v", err)
	}

	before, err := GetIssue(db, 1)
	if err != nil {
		t.Fatalf("GetIssue before: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("v6->v7 Migrate: %v", err)
	}

	after, err := GetIssue(db, 1)
	if err != nil {
		t.Fatalf("GetIssue after: %v", err)
	}

	if before.Title != after.Title || before.Status != after.Status ||
		before.CreatedAt != after.CreatedAt || before.UpdatedAt != after.UpdatedAt ||
		before.Version != after.Version {
		t.Errorf("pre-v7 issue changed across the migration:\nbefore %+v\nafter  %+v", before, after)
	}

	// Every new table exists and is empty: dormant by construction.
	for _, table := range v7Tables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s has %d rows in a repo that never used a workflow", table, n)
		}
	}

	// And the one new COLUMN on an existing table is NULL on the pre-existing
	// row — the §3 phase-2 dormancy obligation at the storage layer. A default
	// of '[]' here would make every issue read as "scope declared, empty",
	// which is a different fact from "no scope declared".
	var scope sql.NullString
	if err := db.QueryRow(`SELECT scope_globs FROM issues WHERE id = 1`).Scan(&scope); err != nil {
		t.Fatalf("reading issues.scope_globs: %v", err)
	}
	if scope.Valid {
		t.Errorf("issues.scope_globs = %q on a pre-v7 row, want NULL", scope.String)
	}

	// `run_issues.loop_count` exists and defaults to 0 (TDD §7.1). Unlike
	// `scope_globs` its NOT NULL DEFAULT 0 is correct rather than a lie: an
	// issue that has entered no loops has entered zero of them, where an issue
	// that declared no scope has NOT declared an empty one.
	if !hasColumn2(t, db, "run_issues", "loop_count") {
		t.Error("run_issues.loop_count missing after the migration")
	}
}
