package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// v10Tables are the tables the v10 DDL creates (TDD docs/tdd/runs-dispatch.md
// §3.1). Stated here independently of v10Sentinels so the sentinel test cannot
// pass by both lists drifting together — the same discipline v7Tables, v8Tables,
// and v9Tables apply. Group 1 contributes `usage_ledger`; groups 2 and 3 append.
var v10Tables = []string{
	"usage_ledger",
	"dispatches",
	"dispatch_rows",
	"reap_acks",
}

// v10Columns are the columns the v10 migration adds to existing tables.
// Sentinels cannot see these — they are tables — which is exactly what §2.4's U3
// row says out loud rather than papering over.
//
// Stated independently of the migration's own v10AddedColumns table, for the
// reason v10Tables is stated independently of v10Sentinels: a list that derived
// itself from the thing it checks would move with it silently.
var v10Columns = []struct{ table, column string }{
	{"runs", "usage_floor"},
	{"runs", "breach_reason"},
	{"steps", "usage_recorded"},
}

func TestMigrateToV10(t *testing.T) {
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

	for _, table := range v10Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}
	// The indexes are asserted BY NAME rather than left to the table check,
	// because two of them carry semantics no table probe can see:
	// `idx_dispatches_one_open` is the partial UNIQUE index that IS C1, and
	// `idx_reap_acks_open` is the partial index that makes D3's dormancy one
	// lookup. A migration that created the tables and dropped either would leave
	// a schema whose shape looked right and whose guarantees were gone.
	for _, index := range []string{
		"idx_usage_ledger_run",
		"idx_dispatches_one_open",
		"idx_reap_acks_open",
	} {
		if !hasIndex(t, db, index) {
			t.Errorf("%s missing after migration", index)
		}
	}
	for _, col := range v10Columns {
		if !columnExists(t, db, col.table, col.column) {
			t.Errorf("%s.%s missing after migration", col.table, col.column)
		}
	}
}

// TestSchemaSpanIsComplete is §2.4's tripwire, and it is NOT a tautology check.
//
// docs/tdd/reliability-delta.md §2 ratified a v5–v10 span and ratified that
// stage 7 "reuses stage 6's tables and adds no version". Stages 4, 5, and 6 were
// built on that arithmetic. A stage-7 author who reaches for v11 without filing
// an amendment against that section fails HERE, with the section named, rather
// than discovering after the fact that a ratified table had been quietly
// extended.
//
// It also asserts the migration map has one entry per version 2..10 with NO
// GAPS, because a missing entry turns Migrate's loop into a runtime error on a
// user's database rather than a build-time failure on ours.
func TestSchemaSpanIsComplete(t *testing.T) {
	// The span now ends at v25. v11 through v25 are AMENDMENTS, not stages —
	// workflow retirement (DKT-21), the projects dimension (operator request,
	// 2026-08-09), vote provenance (DKT-71), per-seat vote spend (DKT-95),
	// artifact revisions (DKT-70), the retry-budget base (DKT-86/DKT-90),
	// vote-usage provenance (DKT-115), issue resolution (DKT-245), the
	// measured-usage cap (DKT-238), operator loop grants (DKT-237), the
	// hollow-assurance marker (DKT-265), the pause origin (DKT-305), the
	// attempt-outcome breakdown (DKT-490), the batch gate-override grant
	// (DKT-546), and the stale-target waiver (DKT-742) — each recorded in
	// docs/tdd/reliability-delta.md §2 under its own heading, with the reason
	// it needed a version and the argument that it leaves the ratified v5-v10
	// arithmetic untouched.
	//
	// The tripwire itself is UNCHANGED IN PURPOSE. It still fails the next
	// author who reaches for a version without filing an amendment first —
	// this test firing is exactly how v11 through v23 came to be documented
	// rather than discovered afterwards. Raising the number without editing
	// that section is the move it exists to stop.
	if currentSchemaVersion != 25 {
		t.Errorf("currentSchemaVersion = %d, want 25 — the span of "+
			"docs/tdd/reliability-delta.md §2 ends at v25 (the stale-target "+
			"waiver). Moving past 25 needs an amendment against that "+
			"section, per docs/design/amendments.md", currentSchemaVersion)
	}

	for v := 2; v <= currentSchemaVersion; v++ {
		if _, ok := migrations[v]; !ok {
			t.Errorf("migrations has no entry for version %d; the span must have "+
				"no gaps or Migrate fails at runtime on a user's database", v)
		}
	}
	if len(migrations) != currentSchemaVersion-1 {
		t.Errorf("migrations has %d entries, want %d (one per version 2..%d)",
			len(migrations), currentSchemaVersion-1, currentSchemaVersion)
	}
}

// TestRewindGuardProbesEveryV10Sentinel is §2.2's obligation, and it derives the
// table list from the DDL itself: a group that adds a CREATE TABLE to v10DDL and
// forgets the sentinel fails HERE rather than shipping a database the guard
// silently declines to repair. This stage slices v10 across THREE groups, so the
// window in which that mistake is possible is the widest yet.
func TestRewindGuardProbesEveryV10Sentinel(t *testing.T) {
	re := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+(\w+)`)
	var created []string
	for _, m := range re.FindAllStringSubmatch(v10DDL, -1) {
		created = append(created, m[1])
	}
	if len(created) == 0 {
		t.Fatal("no CREATE TABLE statements found in v10DDL")
	}
	sort.Strings(created)

	for _, pair := range []struct {
		name string
		list []string
	}{
		{"v10Sentinels", append([]string(nil), v10Sentinels...)},
		{"v10Tables", append([]string(nil), v10Tables...)},
	} {
		got := pair.list
		sort.Strings(got)
		if len(got) != len(created) {
			t.Fatalf("%s = %v, but v10DDL creates %v", pair.name, got, created)
		}
		for i := range created {
			if got[i] != created[i] {
				t.Errorf("%s = %v, but v10DDL creates %v", pair.name, got, created)
				break
			}
		}
	}

	// THE INDEX HALF, which group 3 is the first group to need.
	//
	// It is NOT derived from the DDL the way the tables are, because most v10
	// indexes do not need to be sentinels: an index created in the same group as
	// the table it covers arrives with that table, and the table's own sentinel
	// already forces the rewind. `idx_events_seq` is different — group 3 added
	// it and added NO table, so nothing else would ever trigger the re-run.
	//
	// What this asserts is that every index NAMED as a sentinel actually appears
	// in the DDL. A sentinel probing an index the migration never creates would
	// rewind every database on every open, forever.
	for _, index := range v10IndexSentinels {
		if !strings.Contains(v10DDL, index) {
			t.Errorf("v10IndexSentinels names %q, which v10DDL does not create; "+
				"a sentinel for an index that never arrives rewinds every "+
				"database on every open", index)
		}
	}
	if len(v10IndexSentinels) == 0 {
		t.Error("v10IndexSentinels is empty; group 3 adds no table, so the index " +
			"probe is the ONLY thing that makes a group-2-stamped database " +
			"converge on the complete v10")
	}
}

// TestRewindGuardRepairsAGroup2StampedDatabase is U2 for the group-3 case, and
// it is a REGRESSION TEST: the state it constructs was found on the operator's
// own dogfooded tracker, not imagined.
//
// THE SHAPE OF THE BUG. Group 3 adds no table. So a database that a group-2
// binary stamped 10 has every TABLE sentinel present, the rewind guard concludes
// v10 is complete, the migration never re-runs, and `idx_events_seq` never
// arrives — silently, on exactly the database that gets dogfooded through the
// stage. `CREATE INDEX IF NOT EXISTS` in the DDL does not help on its own,
// because the DDL only executes when something decides to run the migration.
//
// The fix is the index probe, and this asserts it: a stamped-10 database missing
// only the index must gain it on the next open.
func TestRewindGuardRepairsAGroup2StampedDatabase(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A GROUP-2 BINARY'S OUTPUT, reconstructed: every v10 table present, stamped
	// 10, and group 3's index absent.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_events_seq`); err != nil {
		t.Fatalf("dropping the index: %v", err)
	}
	stampVersion(t, db, 10)

	for _, table := range v10Sentinels {
		exists, err := tableExists(db, table)
		if err != nil {
			t.Fatalf("probing %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("premise: %s must be present, or the table probe would have "+
				"caught this and the index probe would be untested", table)
		}
	}

	// The next open repairs it.
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate over a group-2-stamped database: %v", err)
	}

	for _, index := range v10IndexSentinels {
		exists, err := indexExists(db, index)
		if err != nil {
			t.Fatalf("probing %s: %v", index, err)
		}
		if !exists {
			t.Errorf("%s is still absent after a migrate; a database stamped 10 by "+
				"a group-2 binary never converges on the complete v10", index)
		}
	}

	// And the stamp is back at the current version, with every table intact —
	// the rewind re-ran the migration, it did not damage what was already there.
	version, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("stamped %d after the repair, want %d", version, currentSchemaVersion)
	}
	for _, table := range v10Sentinels {
		exists, _ := tableExists(db, table)
		if !exists {
			t.Errorf("%s was lost by the rewind; re-running the migration must add "+
				"what is missing and touch nothing else", table)
		}
	}
}

// migratedToV9 builds a database at exactly v9: the full v9 structures, stamped
// 9, with none of v10's.
func migratedToV9(t *testing.T) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	stripV10(t, db)
	stampVersion(t, db, 9)
	return db
}

// stripV10 removes everything the v10 migration adds — the tables AND the
// columns — so a test starts from a genuine v9 shape.
//
// Removing the columns is not fussiness: a fixture that kept `runs.usage_floor`
// would make "the column arrived with the rewind" pass VACUOUSLY, which is the
// failure mode §2.3's column half exists to prevent.
// stampSchemaVersion forces the recorded schema version, so a test can
// reconstruct a past ERA rather than inheriting whatever the current ladder
// climbed to. Needed once the ladder grew past the version under test.
func stampSchemaVersion(t *testing.T, db *sql.DB, version int) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
		strconv.Itoa(version),
	); err != nil {
		t.Fatalf("stamping schema_version = %d: %v", version, err)
	}
}

func stripV10(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range v10Tables {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("dropping %s: %v", table, err)
		}
	}
	for _, col := range v10Columns {
		if !columnExists(t, db, col.table, col.column) {
			continue
		}
		if _, err := db.Exec(
			`ALTER TABLE ` + col.table + ` DROP COLUMN ` + col.column); err != nil {
			t.Fatalf("dropping %s.%s: %v", col.table, col.column, err)
		}
	}
}

// countLedgerRows is the assertion §3's group-1 dormancy row rests on: v10 seeds
// NOTHING, so the ledger is empty until a `--usage` actually records.
func countLedgerRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM usage_ledger`).Scan(&n); err != nil {
		t.Fatalf("counting ledger rows: %v", err)
	}
	return n
}

// TestMigrateV10U1TrackerShape is U1: this repo's own tracker — stamped 9, every
// v7/v8/v9 sentinel present, `steps` empty because the tracker has issues but no
// runs. The tracker is dogfooded across the stage, so this is the shape the
// operator's own database actually has when a group-1 binary first opens it, and
// it is the fourth time this proof has been written.
func TestMigrateV10U1TrackerShape(t *testing.T) {
	db := migratedToV9(t)

	var steps int
	if err := db.QueryRow(`SELECT COUNT(*) FROM steps`).Scan(&steps); err != nil {
		t.Fatalf("counting steps: %v", err)
	}
	if steps != 0 {
		t.Fatalf("fixture has %d steps, want 0 (U1 is the no-runs shape)", steps)
	}

	// The pre-migration facts the upgrade must not disturb: every v9 table is
	// present and the issue rows read exactly as they did.
	if _, err := db.Exec(
		`INSERT INTO issues (title, description, status, priority, kind, created_at, updated_at)
		 VALUES ('DKT-tracked', 'body', 'todo', 'high', 'task', '2026-08-04', '2026-08-04')`,
	); err != nil {
		t.Fatalf("seeding a tracker issue: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v9->v10: %v", err)
	}

	// Migrate climbs to the TOP of the ladder, which is no longer 10 (v11).
	// What this assertion means is "the migration ran to completion",
	// so it names currentSchemaVersion rather than a literal that has to be
	// edited every time a version is added.
	if v, err := SchemaVersion(db); err != nil || v != currentSchemaVersion {
		t.Fatalf("schema_version = %d (err %v), want %d",
			v, err, currentSchemaVersion)
	}
	for _, table := range append(append([]string(nil), v9Tables...), v10Tables...) {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}
	// The ledger arrives EMPTY. v10 seeds nothing, unlike v9's one builtin
	// schema row, and saying "zero" here is the true statement rather than the
	// flattering one.
	if n := countLedgerRows(t, db); n != 0 {
		t.Errorf("usage_ledger holds %d rows after the migration, want 0", n)
	}

	// The tracker's own rows read byte-identically.
	var title, status, priority string
	if err := db.QueryRow(
		`SELECT title, status, priority FROM issues WHERE title = 'DKT-tracked'`,
	).Scan(&title, &status, &priority); err != nil {
		t.Fatalf("reading the tracker issue back: %v", err)
	}
	if title != "DKT-tracked" || status != "todo" || priority != "high" {
		t.Errorf("the migration disturbed an issue row: %s/%s/%s", title, status, priority)
	}
}

// TestMigrateV10U2RewindsWhenASentinelIsAbsent is U2: the group-1-partial
// dogfood shape, which is the v7, v8, and v9 trap a fourth time.
//
// A database stamped 10 with a v10 sentinel missing is NOT trusted: the guard
// rewinds to 9 and re-runs, and the database ends complete INCLUDING the
// columns, which arrive only because the rewind re-runs the WHOLE migration.
func TestMigrateV10U2RewindsWhenASentinelIsAbsent(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The trap: the stamp still says 10. The pre-group shape is reconstructed
	// exactly — tables dropped, columns removed, stamp left at 10 (§2.2).
	//
	// The stamp is set back to 10 EXPLICITLY rather than inherited from
	// Migrate, because the ladder now continues past 10 (v11) and this
	// test is about the v10 rewind specifically. Reconstructing the era means
	// reconstructing its stamp too.
	stripV10(t, db)
	stampSchemaVersion(t, db, 10)
	if v, err := SchemaVersion(db); err != nil || v != 10 {
		t.Fatalf("premise: schema_version = %d (err %v), want 10", v, err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after the sentinel went missing: %v", err)
	}

	for _, table := range v10Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s was not restored by the rewind guard", table)
		}
	}
	// Each column asserted INDIVIDUALLY: the sentinels cannot see them, so a
	// rewind that restored the tables and skipped the ALTERs would look green.
	for _, col := range v10Columns {
		if !columnExists(t, db, col.table, col.column) {
			t.Errorf("%s.%s did not arrive with the rewind", col.table, col.column)
		}
	}
}

// TestMigrateV10U2GroupOneToGroupTwoDogfoodShape is U2 at the EXACT intermediate
// state group 2 creates, rather than at the synthetic all-tables-gone shape.
//
// §2.1 warns that three commit groups mean TWO intermediate states in which the
// operator's own dogfooded tracker can sit, stamped 10 with only part of v10
// present. This is the first of them, and it is not hypothetical: the tracker was
// migrated by a group-1 binary at commit 55c983b, so every database this repo's
// author touched between that commit and this one has `usage_ledger` and no
// `dispatches`.
//
// The premise is asserted before the repair, so the test cannot pass by starting
// from a shape that was already complete.
func TestMigrateV10U2GroupOneToGroupTwoDogfoodShape(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Reconstruct precisely what a group-1 binary leaves behind: the ledger and
	// the columns present, the group-2 tables absent, the stamp at 10.
	for _, table := range []string{"dispatch_rows", "dispatches", "reap_acks"} {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("dropping %s to build the group-1 shape: %v", table, err)
		}
	}
	if !hasTable(t, db, "usage_ledger") {
		t.Fatal("premise: usage_ledger must be present — this is the GROUP-1 shape")
	}
	if hasTable(t, db, "dispatches") {
		t.Fatal("premise: dispatches must be absent — this is the group-1 shape")
	}
	// Stamped back to 10 explicitly: the ladder continues past 10 now (v11),
	// and this test reconstructs the GROUP-1 ERA, whose stamp was 10.
	stampSchemaVersion(t, db, 10)
	if v, err := SchemaVersion(db); err != nil || v != 10 {
		t.Fatalf("premise: schema_version = %d (err %v), want 10 — the stamp moved "+
			"in group 1 and does not move again (§2.1)", v, err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("a group-2 binary opening the group-1 tracker: %v", err)
	}

	// The rewind saw the missing sentinel, went back to 9, and re-ran the whole
	// migration: the dispatch family arrives.
	for _, table := range v10Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s did not arrive when a group-2 binary opened the "+
				"group-1 tracker", table)
		}
	}
	for _, index := range []string{"idx_dispatches_one_open", "idx_reap_acks_open"} {
		if !hasIndex(t, db, index) {
			t.Errorf("%s did not arrive with the rewind", index)
		}
	}
	// And group 1's own work is untouched — the re-run is CREATE TABLE IF NOT
	// EXISTS plus hasColumn-probed ALTERs throughout (§2.2), so it adds what is
	// missing and touches nothing else.
	for _, col := range v10Columns {
		if !columnExists(t, db, col.table, col.column) {
			t.Errorf("%s.%s was lost by the group-2 rewind", col.table, col.column)
		}
	}
	// As above: the repair leaves the database at the top of the ladder, which
	// the v11 amendment moved past 10.
	if v, err := SchemaVersion(db); err != nil || v != currentSchemaVersion {
		t.Errorf("schema_version = %d (err %v) after the repair, want %d",
			v, err, currentSchemaVersion)
	}
}

// TestMigrateV10U3ColumnHalfIsNotSeenBySentinels is U3, and it asserts the
// HONEST statement rather than a flattering one: a database whose tables are all
// present but whose column is missing is NOT repaired by the sentinel probe,
// because sentinels are tables.
//
// That shape is impossible in practice — the tables and the columns land in one
// transaction — and reachable only by a hand-edit. U3 exists to record which
// mechanism covers the column half: U2's rewind, not the probe. A test that
// claimed otherwise would be documenting a guarantee the code does not make.
func TestMigrateV10U3ColumnHalfIsNotSeenBySentinels(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The impossible-in-practice hand-edit: every table present, one column
	// removed, the stamp left at 10.
	if _, err := db.Exec(`ALTER TABLE runs DROP COLUMN breach_reason`); err != nil {
		t.Fatalf("hand-editing the column away: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate over the hand-edited shape: %v", err)
	}

	if columnExists(t, db, "runs", "breach_reason") {
		t.Error("runs.breach_reason arrived from a sentinel probe; sentinels are " +
			"TABLES and cannot see a column. If this now passes, the guard grew a " +
			"column probe and §2.3's U3 row needs rewriting rather than this test " +
			"quietly flipping")
	}
}

// TestMigrateV10U4FromTheV4Baseline is U4: the committed fixture migrates 4->10
// in ONE pass, and the v10 structures are asserted present BEFORE any golden
// diff is trusted — engine-spine §3's rule, because a golden diff against a
// database that failed to migrate passes vacuously.
func TestMigrateV10U4FromTheV4Baseline(t *testing.T) {
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
		t.Fatalf("Migrate 4->10: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", v, currentSchemaVersion)
	}
	// EVERY intermediate version's tables, not just this stage's: a one-pass
	// 4->10 that skipped v8 would leave a database no verb can use, and the
	// stamp alone would not say so.
	all := append([]string(nil), v7Tables...)
	all = append(all, v8Tables...)
	all = append(all, v9Tables...)
	all = append(all, v10Tables...)
	for _, table := range all {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after the 4->10 migration", table)
		}
	}
	for _, col := range v10Columns {
		if !columnExists(t, db, col.table, col.column) {
			t.Errorf("%s.%s missing after the 4->10 migration", col.table, col.column)
		}
	}
	// A v4 repo has never recorded usage, so the ledger is dormant on it.
	if n := countLedgerRows(t, db); n != 0 {
		t.Errorf("usage_ledger holds %d rows in a migrated v4 repo, want 0", n)
	}
}

// TestMigrateV10IsIdempotent: the migration run twice against a populated
// database duplicates nothing and rewrites nothing. The rewind guard re-runs it
// BY DESIGN, so this is a property the guard depends on rather than a courtesy.
func TestMigrateV10IsIdempotent(t *testing.T) {
	db := migratedToV9(t)
	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}

	runID, stepID := seedRunAndStep(t, db)
	if _, err := db.Exec(
		`INSERT INTO usage_ledger (run_id, step_id, attempt, unit, quantity, source, created_at_ms)
		 VALUES (?, ?, 0, 'pages', 12.5, 'reported', 1000)`, runID, stepID); err != nil {
		t.Fatalf("seeding a ledger row: %v", err)
	}

	stampVersion(t, db, 9)
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	if n := countLedgerRows(t, db); n != 1 {
		t.Errorf("usage_ledger holds %d rows after a second pass, want 1", n)
	}
	var (
		unit     string
		quantity float64
		source   string
	)
	if err := db.QueryRow(
		`SELECT unit, quantity, source FROM usage_ledger WHERE step_id = ?`, stepID,
	).Scan(&unit, &quantity, &source); err != nil {
		t.Fatalf("reading the ledger row back: %v", err)
	}
	if unit != "pages" || quantity != 12.5 || source != "reported" {
		t.Errorf("the second pass rewrote a ledger row: %s/%g/%s", unit, quantity, source)
	}
}

// TestUsageLedgerIsKeyedByAttempt is §3.1's first shape decision, proven: the
// unique key is (step_id, attempt, unit), so a reaped-and-reclaimed step's
// SECOND attempt records its own usage beside the first rather than overwriting
// it.
//
// A step-unique key would silently lose the first attempt's numbers, and the
// report's attempt trail would then show two attempts and one attempt's usage —
// which is the exact drift between the floor side and the reported side that the
// `attempt` column exists to prevent.
func TestUsageLedgerIsKeyedByAttempt(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	runID, stepID := seedRunAndStep(t, db)

	insert := func(attempt int, unit string, qty float64) error {
		_, err := db.Exec(
			`INSERT INTO usage_ledger (run_id, step_id, attempt, unit, quantity, source, created_at_ms)
			 VALUES (?, ?, ?, ?, ?, 'reported', 1000)`,
			runID, stepID, attempt, unit, qty)
		return err
	}

	if err := insert(0, "pages", 3); err != nil {
		t.Fatalf("attempt 0: %v", err)
	}
	if err := insert(1, "pages", 5); err != nil {
		t.Fatalf("attempt 1 was refused; retries must re-accrue on the reported "+
			"side as well as the floor side: %v", err)
	}
	// The same (step, attempt, unit) twice IS refused — belt and braces against a
	// second `complete` for one attempt, which the saga's stage-0 CAS already
	// makes impossible.
	if err := insert(0, "pages", 9); err == nil {
		t.Error("a duplicate (step_id, attempt, unit) was accepted")
	}
	// A different unit at the same attempt is a different row: units are opaque
	// and never commensurable, so they never merge.
	if err := insert(0, "sheets", 2); err != nil {
		t.Fatalf("a second unit at the same attempt was refused: %v", err)
	}

	var total float64
	if err := db.QueryRow(
		`SELECT SUM(quantity) FROM usage_ledger WHERE step_id = ? AND unit = 'pages'`,
		stepID).Scan(&total); err != nil {
		t.Fatalf("summing pages: %v", err)
	}
	if total != 8 {
		t.Errorf("pages sum = %g, want 8 (both attempts)", total)
	}
}
