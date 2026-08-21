package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/schema"
)

// v9Tables are the tables the v9 DDL creates (TDD §4.4, §6.3). Stated here
// independently of v9Sentinels so the sentinel test cannot pass by both lists
// drifting together — the same discipline v7Tables and v8Tables apply. Group 1
// contributed `schemas`; group 2 adds `action_results`.
var v9Tables = []string{
	"schemas",
	"action_results",
}

// v9Columns are the columns the v9 migration adds to existing tables. Sentinels
// cannot see these — they are tables — and docs/spec/architecture.md §3.1
// already says so for v7. Each is probed individually here (§2).
//
// Stated independently of the migration's own v9AddedColumns table, for the
// reason v9Tables is stated independently of v9Sentinels: a list that derived
// itself from the thing it checks would move with it silently.
var v9Columns = []struct{ table, column string }{
	{"artifacts", "stub"},
	{"trust_cache", "kind"},
	{"steps", "materialized"},
}

func TestMigrateToV9(t *testing.T) {
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
	// The stage pin has MOVED ON: stage 5 shipped v9 and stage 6 ships v10, so
	// the assertion here is that v9's row still exists in the span rather than
	// that the span ends at it. The span's own completeness is asserted once, by
	// TestSchemaSpanIsComplete (runs-dispatch §2.4), which is where a stage-7
	// author reaching for v11 is stopped.
	if currentSchemaVersion < 9 {
		t.Errorf("currentSchemaVersion = %d; v9 is a shipped row of the span",
			currentSchemaVersion)
	}

	for _, table := range v9Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}
	if !hasIndex(t, db, "idx_schemas_name") {
		t.Error("idx_schemas_name missing after migration")
	}
	for _, col := range v9Columns {
		if !columnExists(t, db, col.table, col.column) {
			t.Errorf("%s.%s missing after migration", col.table, col.column)
		}
	}
}

// TestRewindGuardProbesEveryV9Sentinel is §2's obligation, and it derives the
// table list from the DDL itself: a group that adds a CREATE TABLE to v9DDL and
// forgets the sentinel fails HERE rather than shipping a database the guard
// silently declines to repair.
func TestRewindGuardProbesEveryV9Sentinel(t *testing.T) {
	re := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+(\w+)`)
	var created []string
	for _, m := range re.FindAllStringSubmatch(v9DDL, -1) {
		created = append(created, m[1])
	}
	if len(created) == 0 {
		t.Fatal("no CREATE TABLE statements found in v9DDL")
	}
	sort.Strings(created)

	for _, pair := range []struct {
		name string
		list []string
	}{
		{"v9Sentinels", append([]string(nil), v9Sentinels...)},
		{"v9Tables", append([]string(nil), v9Tables...)},
	} {
		got := pair.list
		sort.Strings(got)
		if len(got) != len(created) {
			t.Fatalf("%s = %v, but v9DDL creates %v", pair.name, got, created)
		}
		for i := range created {
			if got[i] != created[i] {
				t.Errorf("%s = %v, but v9DDL creates %v", pair.name, got, created)
				break
			}
		}
	}
}

// columnExists is the column-half probe the U-table needs. It asks SQLite
// directly rather than through hasColumn, which takes a transaction.
func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var found bool
	err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = ?)`,
		table, column).Scan(&found)
	if err != nil {
		t.Fatalf("inspecting %s.%s: %v", table, column, err)
	}
	return found
}

// migratedToV8 builds a database at exactly v8: the full v8 structures, stamped
// 8, with none of v9's.
func migratedToV8(t *testing.T) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Migrate ran all the way to v9; strip v9's structures and rewind the stamp
	// so the test starts from the shape a v8-era binary left behind.
	stripV9(t, db)
	stampVersion(t, db, 8)
	return db
}

// stripV9 removes everything the v9 migration adds — the tables AND the
// columns — so a test starts from a genuine v8 shape.
//
// Removing the columns is not fussiness: a fixture that kept `artifacts.stub`
// would make U3's "the column arrived with the rewind" assertion pass
// VACUOUSLY, which is the failure mode §2 wrote the column half of the U-table
// to prevent.
func stripV9(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range v9Tables {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("dropping %s: %v", table, err)
		}
	}
	for _, col := range v9Columns {
		if !columnExists(t, db, col.table, col.column) {
			continue
		}
		if _, err := db.Exec(
			`ALTER TABLE ` + col.table + ` DROP COLUMN ` + col.column); err != nil {
			t.Fatalf("dropping %s.%s: %v", col.table, col.column, err)
		}
	}
}

// countSchemas is the assertion §3 rests on: v9 seeds EXACTLY ONE row.
func countSchemas(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schemas`).Scan(&n); err != nil {
		t.Fatalf("counting schemas: %v", err)
	}
	return n
}

// TestMigrateV9U1TrackerShape is U1: this repo's own tracker — stamped 8, every
// v7/v8 sentinel present, `steps` empty because the tracker has issues but no
// runs. The tracker is dogfooded across the stage, so this is the shape the
// operator's own database actually has when a group-1 binary first opens it.
func TestMigrateV9U1TrackerShape(t *testing.T) {
	db := migratedToV8(t)

	var steps int
	if err := db.QueryRow(`SELECT COUNT(*) FROM steps`).Scan(&steps); err != nil {
		t.Fatalf("counting steps: %v", err)
	}
	if steps != 0 {
		t.Fatalf("fixture has %d steps, want 0 (U1 is the no-runs shape)", steps)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v8->v9: %v", err)
	}

	for _, table := range v9Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}

	// `schemas` holds the one builtin row and NOTHING ELSE. §3's dormancy claim
	// names this number rather than claiming zero, which would have been the
	// flattering statement rather than the true one.
	if n := countSchemas(t, db); n != 1 {
		t.Fatalf("schemas has %d rows after migration, want exactly the one builtin", n)
	}

	got, err := GetSchema(db, 1, schema.AggregateName, schema.AggregateVersion)
	if err != nil {
		t.Fatalf("GetSchema(%s): %v", schema.AggregateRef(), err)
	}
	if !got.Builtin {
		t.Error("the seeded row is not marked builtin")
	}
	if got.SourceSHA256 != schema.AggregateSHA256() {
		t.Errorf("seeded sha256 = %s, want the embedded document's %s",
			got.SourceSHA256, schema.AggregateSHA256())
	}
	if got.Body != string(schema.AggregateBody()) {
		t.Error("the seeded body is not the embedded document, verbatim")
	}
	if got.SourcePath != "" {
		t.Errorf("seeded source_path = %q, want empty — the bytes came from the binary",
			got.SourcePath)
	}
}

// TestMigrateV9U2PopulatedRunKeepsItsArtifactBytes is U2 and §6.3 S2 together:
// a populated database migrates, every artifact keeps its bytes, and the S3/S4
// stubbed action artifacts gain `stub = 1` with their `{"stub":true,…}` wrapper
// UNMODIFIED.
//
// Rewriting the wrapper would destroy the evidence that a computation did not
// run, which is the entire reason the marker exists.
func TestMigrateV9U2PopulatedRunKeepsItsArtifactBytes(t *testing.T) {
	db := migratedToV8(t)

	const wrapped = `{"stub":true,"payload":[{"risk":"low"}]}`
	const plain = `[{"risk":"high"}]`
	stubbed := seedArtifact(t, db, "findings", wrapped)
	real := seedArtifact(t, db, "review", plain)
	empty := seedArtifact(t, db, "notes", "")

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v8->v9: %v", err)
	}

	for _, tc := range []struct {
		id       int
		wantBody string
		wantStub bool
		why      string
	}{
		{stubbed, wrapped, true, "a stubbed artifact keeps its wrapper and gains the column"},
		{real, plain, false, "a plain payload is untouched and unmarked"},
		{empty, "", false, "an artifact with no payload is unmarked"},
	} {
		var (
			payload sql.NullString
			stub    int
		)
		err := db.QueryRow(`SELECT payload, stub FROM artifacts WHERE id = ?`, tc.id).
			Scan(&payload, &stub)
		if err != nil {
			t.Fatalf("reading artifact %d: %v", tc.id, err)
		}
		if payload.String != tc.wantBody {
			t.Errorf("%s: payload = %q, want %q (the never-mutate rule)",
				tc.why, payload.String, tc.wantBody)
		}
		if (stub != 0) != tc.wantStub {
			t.Errorf("%s: stub = %d, want %v", tc.why, stub, tc.wantStub)
		}
	}
}

// seedArtifact writes one artifact row directly, because the point is to
// reproduce what an S3/S4-era binary STORED rather than what today's writers
// produce.
func seedArtifact(t *testing.T, db *sql.DB, kind, payload string) int {
	t.Helper()

	res, err := db.Exec(
		`INSERT INTO runs (request, status, created_at_ms, updated_at_ms)
		 VALUES ('', 'running', 1, 1)`)
	if err != nil {
		t.Fatalf("seeding a run: %v", err)
	}
	runID, _ := res.LastInsertId()

	var stored any
	if payload != "" {
		stored = payload
	}
	res, err = db.Exec(
		`INSERT INTO artifacts (run_id, step_id, kind, body, payload, sha256, created_at_ms)
		 VALUES (?, NULL, ?, 'body', ?, 'x', 1)`, runID, kind, stored)
	if err != nil {
		t.Fatalf("seeding an artifact: %v", err)
	}
	id, _ := res.LastInsertId()
	return int(id)
}

// TestMigrateV9U3RewindsWhenASentinelIsAbsent is U3: the group-1-partial
// dogfood shape, which is exactly the v7 and v8 trap a third time.
//
// A database stamped 9 with a v9 sentinel missing is NOT trusted: the guard
// rewinds to 8 and re-runs, and the database ends complete INCLUDING the
// columns, which arrive only because the rewind re-runs the WHOLE migration.
func TestMigrateV9U3RewindsWhenASentinelIsAbsent(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The trap: the stamp still says 9. The group-1 shape is reconstructed
	// exactly — table dropped, columns removed, stamp AT 9 (§2).
	//
	// The stamp is set rather than merely left alone because Migrate now runs on
	// to v10: the shape under test is a v9-era binary's, and reading a 10 here
	// would be testing the v10 guard instead of the v9 one.
	stripV9(t, db)
	stampVersion(t, db, 9)
	if v, err := SchemaVersion(db); err != nil || v != 9 {
		t.Fatalf("premise: schema_version = %d (err %v), want 9", v, err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after the sentinel went missing: %v", err)
	}

	for _, table := range v9Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s was not restored by the rewind guard", table)
		}
	}
	// Each column asserted INDIVIDUALLY: the sentinels cannot see them, so a
	// rewind that restored the tables and skipped the ALTERs would look green.
	for _, col := range v9Columns {
		if !columnExists(t, db, col.table, col.column) {
			t.Errorf("%s.%s did not arrive with the rewind", col.table, col.column)
		}
	}
	if n := countSchemas(t, db); n != 1 {
		t.Errorf("schemas has %d rows after the rewind, want the one builtin", n)
	}
}

// TestMigrateV9U4ReRunsOverAMissingColumn is U4's group-1 row: the column half,
// proven INDEPENDENTLY of the table half.
//
// The rewind guard probes tables, so a database whose table is present and whose
// column is not is not detected by it — and does not need to be, since the two
// land in one transaction. What has to hold is that the migration is re-runnable
// over the shape anyway: it probes before the ALTER, which SQLite would
// otherwise reject on the second pass.
func TestMigrateV9U4ReRunsOverAMissingColumn(t *testing.T) {
	db := migratedToV8(t)

	// v8's own `artifacts` has no `stub`, which is exactly the shape under test.
	if columnExists(t, db, "artifacts", "stub") {
		t.Fatal("premise: the v8 shape must not already carry artifacts.stub")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v8->v9: %v", err)
	}
	if !columnExists(t, db, "artifacts", "stub") {
		t.Fatal("artifacts.stub did not arrive")
	}

	// And a forced re-run does not trip over the ALTER, which SQLite would
	// reject if the migration did not probe first.
	stampVersion(t, db, 8)
	if err := Migrate(db); err != nil {
		t.Fatalf("re-running the migration over the added column: %v", err)
	}
}

// TestMigrateV9U5IsIdempotent is U5: the migration run twice against a
// populated database duplicates nothing.
func TestMigrateV9U5IsIdempotent(t *testing.T) {
	db := migratedToV8(t)
	seedArtifact(t, db, "findings", `{"stub":true,"payload":[]}`)

	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	before := countSchemas(t, db)
	var artifactsBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&artifactsBefore); err != nil {
		t.Fatalf("counting artifacts: %v", err)
	}

	stampVersion(t, db, 8)
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	if after := countSchemas(t, db); after != before || after != 1 {
		t.Errorf("schemas holds %d rows after a second pass, want %d (exactly the builtin)",
			after, before)
	}
	var artifactsAfter int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&artifactsAfter); err != nil {
		t.Fatalf("counting artifacts: %v", err)
	}
	if artifactsAfter != artifactsBefore {
		t.Errorf("artifacts = %d after a second pass, want %d", artifactsAfter, artifactsBefore)
	}
}

// TestMigrateV9U6FromTheV4Baseline is U6: the committed fixture migrates 4->9 in
// ONE pass, and the v9 structures are asserted present BEFORE any golden diff is
// trusted — engine-spine §3's rule, because a golden diff against a database
// that failed to migrate passes vacuously.
func TestMigrateV9U6FromTheV4Baseline(t *testing.T) {
	src := filepath.Join("..", "..", "scripts", "qa", "fixtures", "v4-baseline.db")
	if _, err := os.Stat(src); err != nil {
		t.Skipf("v4 baseline fixture unavailable: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "v4-baseline.db")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("copying the fixture: %v", err)
	}

	db, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate 4->9: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", v, currentSchemaVersion)
	}
	for _, table := range append(append(append([]string(nil), v7Tables...), v8Tables...), v9Tables...) {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after the 4->9 migration", table)
		}
	}
	for _, col := range v9Columns {
		if !columnExists(t, db, col.table, col.column) {
			t.Errorf("%s.%s missing after the 4->9 migration", col.table, col.column)
		}
	}
	// A v4 repo has never registered a schema, so the registry is dormant on it
	// apart from the seed.
	if n := countSchemas(t, db); n != 1 {
		t.Errorf("schemas has %d rows in a migrated v4 repo, want the one builtin", n)
	}
}

// TestMigrateV9U4GroupTwoColumnsArrive is U4's group-2 row: the columns
// `trust_cache.kind` and `steps.materialized` arrive, EXISTING ROWS ARE INTACT,
// and each reads the default that preserves what it already meant.
//
// The defaults are the whole of the never-mutate rule here. `kind` defaults to
// `gate` because every row written before v9 recorded a gate's decision, and
// `materialized` defaults to 0 because every step that existed was declared
// rather than minted by a hold. A migration that rewrote either would be
// asserting something about history it cannot know.
func TestMigrateV9U4GroupTwoColumnsArrive(t *testing.T) {
	db := migratedToV8(t)

	runID, stepID := seedRunAndStep(t, db)
	if _, err := db.Exec(
		`INSERT INTO trust_cache (run_id, gate, argv_sha256, entry_name, matched, prefix, at_ms)
		 VALUES (?, 'build', 'deadbeef', 'build', 1, 0, 1000)`, runID); err != nil {
		t.Fatalf("seeding a v8 trust_cache row: %v", err)
	}

	for _, col := range []struct{ table, column string }{
		{"trust_cache", "kind"}, {"steps", "materialized"},
	} {
		if columnExists(t, db, col.table, col.column) {
			t.Fatalf("premise: the v8 shape must not already carry %s.%s",
				col.table, col.column)
		}
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v8->v9: %v", err)
	}

	var kind string
	if err := db.QueryRow(`SELECT kind FROM trust_cache WHERE run_id = ?`, runID).
		Scan(&kind); err != nil {
		t.Fatalf("reading the migrated trust_cache row: %v", err)
	}
	if kind != TrustKindGate {
		t.Errorf("a pre-v9 trust_cache row reads kind = %q, want %q — every row "+
			"written before v9 recorded a gate", kind, TrustKindGate)
	}

	step, err := GetStep(db, stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if step.Materialized {
		t.Error("a pre-v9 step reads materialized = 1; every step that existed " +
			"was declared, not minted by a hold")
	}
	if step.Instance != "implement@0" || step.Status != StepPending {
		t.Errorf("the migrated step row was not intact: %+v", step)
	}

	// And a forced re-run does not trip over the ALTERs, which SQLite would
	// reject if the migration did not probe first.
	stampVersion(t, db, 8)
	if err := Migrate(db); err != nil {
		t.Fatalf("re-running the migration over the added columns: %v", err)
	}
}

// TestActionResultsAreEmptyOnAFreshDatabase is §3's group-2 dormancy claim at
// the storage layer: `action_results` exists and holds nothing until an action
// step actually runs.
func TestActionResultsAreEmptyOnAFreshDatabase(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM action_results`).Scan(&n); err != nil {
		t.Fatalf("counting action results: %v", err)
	}
	if n != 0 {
		t.Errorf("action_results holds %d rows in a fresh repo, want 0", n)
	}
}

// TestActionResultsRecordEveryAttempt covers the table's own contract: NULL
// argv/exit for something that never spawned, ascending ordinals per
// (step, action), and the unique identity index refusing a duplicate.
func TestActionResultsRecordEveryAttempt(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	runID, stepID := seedRunAndStep(t, db)

	exit := 3
	rows := []ActionResultRow{
		{RunID: runID, StepID: stepID, Action: "aggregate", Ordinal: 0,
			Verdict: ActionVerdictPass, Builtin: true, CreatedAtMS: 1000},
		{RunID: runID, StepID: stepID, Action: "reconcile-cmd", Ordinal: 0,
			Argv: []string{"reconcile", "--json"}, Exit: &exit,
			Verdict: ActionVerdictFail, Output: "boom", CreatedAtMS: 1001},
		{RunID: runID, StepID: stepID, Action: "reconcile-cmd", Ordinal: 1,
			Verdict: ActionVerdictUnmatched, Reason: "no trust entry",
			CreatedAtMS: 1002},
	}
	for _, r := range rows {
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := InsertActionResultTx(tx, r); err != nil {
			t.Fatalf("InsertActionResultTx(%s@%d): %v", r.Action, r.Ordinal, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	got, err := ActionResultsForStep(db, stepID)
	if err != nil {
		t.Fatalf("ActionResultsForStep: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d rows, want 3", len(got))
	}
	if got[0].Argv != nil || got[0].Exit != nil {
		t.Error("a builtin recorded an argv or an exit; it spawns nothing (B2), " +
			"and a zero exit on something that did not run reads as success")
	}
	if !got[0].Builtin {
		t.Error("the builtin row does not read back as builtin")
	}
	if got[1].Exit == nil || *got[1].Exit != 3 || len(got[1].Argv) != 2 {
		t.Errorf("the spawned row did not round-trip: %+v", got[1])
	}
	if got[2].Verdict != ActionVerdictUnmatched || got[2].Reason != "no trust entry" {
		t.Errorf("the unmatched row lost its reason: %+v", got[2])
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	next, err := NextActionOrdinalTx(tx, stepID, "reconcile-cmd")
	if err != nil {
		t.Fatalf("NextActionOrdinalTx: %v", err)
	}
	if next != 2 {
		t.Errorf("next ordinal = %d, want 2", next)
	}
	// The identity index is what keeps a resumed routing stage from colliding
	// with the attempt it is re-running.
	if err := InsertActionResultTx(tx, rows[2]); err == nil {
		t.Error("a duplicate (step, action, ordinal) was accepted")
	}
}

// seedRunAndStep writes the minimum run+step pair the result tables reference.
func seedRunAndStep(t *testing.T, db *sql.DB) (runID, stepID int) {
	t.Helper()

	res, err := db.Exec(
		`INSERT INTO runs (request, status, created_at_ms, updated_at_ms)
		 VALUES ('', 'active', 1, 1)`)
	if err != nil {
		t.Fatalf("seeding a run: %v", err)
	}
	rid, _ := res.LastInsertId()

	res, err = db.Exec(
		`INSERT INTO workflows (name, version, source_sha256, body, parsed,
		                        created_at_ms, row_version)
		 VALUES ('seed', 1, 'abc', 'body', '{}', 1, 1)`)
	if err != nil {
		t.Fatalf("seeding a workflow: %v", err)
	}
	wfID, _ := res.LastInsertId()

	res, err = db.Exec(
		`INSERT INTO issues (title, status, priority, kind, created_at, updated_at)
		 VALUES ('seed', 'todo', 'none', 'task', 1, 1)`)
	if err != nil {
		t.Fatalf("seeding an issue: %v", err)
	}
	issueID, _ := res.LastInsertId()

	res, err = db.Exec(
		`INSERT INTO steps (run_id, issue_id, workflow_id, step_name, ordinal,
		                    instance, kind, status, attempt, created_at_ms,
		                    updated_at_ms, row_version)
		 VALUES (?, ?, ?, 'implement', 0, 'implement@0', 'executor', 'pending', 0, 1, 1, 1)`,
		rid, issueID, wfID)
	if err != nil {
		t.Fatalf("seeding a step: %v", err)
	}
	sid, _ := res.LastInsertId()
	return int(rid), int(sid)
}

// TestMigrateV9U7DivergentBuiltinIsLeftAlone is U7: a database whose
// `aggregate@1` row carries a DIFFERENT hash than the embedded bytes is neither
// overwritten nor refused.
//
// The seed is INSERT OR IGNORE, so the migration leaves it exactly as it found
// it. A database must never become unopenable because a binary changed, and the
// divergence is caught at build time instead, by
// TestEmbeddedAggregateSchemaMatchesItsGolden.
func TestMigrateV9U7DivergentBuiltinIsLeftAlone(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const divergent = `{"type":"array","description":"a build that is not this one"}`
	if _, err := db.Exec(
		`UPDATE schemas SET body = ?, source_sha256 = 'divergent'
		  WHERE name = ? AND version = ?`,
		divergent, schema.AggregateName, schema.AggregateVersion); err != nil {
		t.Fatalf("diverging the seeded row: %v", err)
	}

	stampVersion(t, db, 8)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate over a divergent builtin: %v", err)
	}

	got, err := GetSchema(db, 1, schema.AggregateName, schema.AggregateVersion)
	if err != nil {
		t.Fatalf("GetSchema: %v", err)
	}
	if got.Body != divergent || got.SourceSHA256 != "divergent" {
		t.Errorf("the migration overwrote a pre-existing %s row; the seed is "+
			"INSERT OR IGNORE precisely so it cannot", schema.AggregateRef())
	}
	if n := countSchemas(t, db); n != 1 {
		t.Errorf("schemas holds %d rows, want 1 — the seed inserted a duplicate", n)
	}
}

// TestS3RegisteredWorkflowsSurviveTheUpgrade is §4.9.3's rule, proven where it
// could break: the migration does not re-validate a single row of `workflows`,
// and no verb ever revisits a registered definition's validity.
//
// The argument, in the order that decides it:
//
//  1. A registered name@version is immutable and content-addressed. It is a
//     historical fact — THESE BYTES WERE REGISTERED — not a live claim that they
//     are still fashionable.
//  2. Retroactive invalidation would break the pinning property outright. A run
//     pins a workflow version at activation precisely so later edits cannot
//     change it; if `docket migrate` could render a pinned definition illegal,
//     upgrading the binary would STOP AN IN-FLIGHT RUN. That is strictly worse
//     than the failure re-validation would catch.
//  3. What re-validation would catch is caught anyway, at the only moment it
//     matters: an ordered comparison over a schema-less field parks with a
//     reason. The engine never guesses; it declines.
func TestS3RegisteredWorkflowsSurviveTheUpgrade(t *testing.T) {
	db := migratedToV8(t)

	// An S3-era definition: `payload` passed V25's SHAPE check when nothing
	// could resolve the name, and no schema is registered for it. Registering
	// this file again after v9 is refused (V25a); the ROW must not be.
	const s3Era = `{"pipeline":{"name":"s3-era","version":1},` +
		`"steps":[{"name":"a","executor":"x","emits":"k","payload":"findings@1"}]}`

	res, err := db.Exec(
		`INSERT INTO workflows (name, version, source_path, source_sha256, body, parsed,
		                        created_at_ms, row_version)
		 VALUES ('s3-era', 1, 's3-era.toml', 'the-s3-hash', 'the s3 bytes', ?, 1, 1)`,
		s3Era)
	if err != nil {
		t.Fatalf("seeding the S3-era workflow: %v", err)
	}
	wfID, _ := res.LastInsertId()

	// And a run mid-flight against it: pinned, expanded, one step still pending.
	res, err = db.Exec(
		`INSERT INTO issues (title, status, created_at, updated_at)
		 VALUES ('mid-flight', 'todo', '2026-08-04', '2026-08-04')`)
	if err != nil {
		t.Fatalf("seeding an issue: %v", err)
	}
	issueID, _ := res.LastInsertId()

	res, err = db.Exec(
		`INSERT INTO runs (request, status, created_at_ms, updated_at_ms)
		 VALUES ('', 'active', 1, 1)`)
	if err != nil {
		t.Fatalf("seeding a run: %v", err)
	}
	runID, _ := res.LastInsertId()

	if _, err := db.Exec(
		`INSERT INTO pins (run_id, kind, ref, sha256)
		 VALUES (?, 'workflow', 's3-era@1', 'the-s3-hash')`, runID); err != nil {
		t.Fatalf("seeding the pin: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO steps (run_id, issue_id, workflow_id, step_name, instance, kind,
		                    status, created_at_ms, updated_at_ms)
		 VALUES (?, ?, ?, 'a', 'a@0', 'executor', 'pending', 1, 1)`,
		runID, issueID, wfID); err != nil {
		t.Fatalf("seeding the step: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v8->v9 over an S3-era registration: %v", err)
	}

	// The ROW is untouched, byte for byte and version for version.
	var (
		body, hash, parsed string
		rowVersion         int
	)
	if err := db.QueryRow(
		`SELECT body, source_sha256, parsed, row_version FROM workflows WHERE name = 's3-era'`,
	).Scan(&body, &hash, &parsed, &rowVersion); err != nil {
		t.Fatalf("reading the workflow back: %v", err)
	}
	if body != "the s3 bytes" || hash != "the-s3-hash" || parsed != s3Era {
		t.Error("the migration rewrote a registered definition")
	}
	if rowVersion != 1 {
		t.Errorf("row_version = %d; the migration touched a row it must not", rowVersion)
	}

	// The run is still mid-flight: its pin, its step, and its status are as the
	// v8 binary left them. An upgrade that stopped a run is the failure this
	// stance exists to prevent.
	var pin, status string
	if err := db.QueryRow(
		`SELECT sha256 FROM pins WHERE run_id = ?`, runID).Scan(&pin); err != nil {
		t.Fatalf("reading the pin: %v", err)
	}
	if pin != "the-s3-hash" {
		t.Errorf("pin hash = %q, want the one the run recorded", pin)
	}
	if err := db.QueryRow(
		`SELECT status FROM steps WHERE run_id = ?`, runID).Scan(&status); err != nil {
		t.Fatalf("reading the step: %v", err)
	}
	if status != StepPending {
		t.Errorf("step status = %q after the migration, want %q", status, StepPending)
	}
}
