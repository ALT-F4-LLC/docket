package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// v8Tables are the tables the v8 DDL creates (TDD docs/tdd/gates-trust.md §4.2,
// §4.5). Stated here independently of v8Sentinels so the sentinel test cannot
// pass by both lists drifting together — the same discipline v7Tables applies.
var v8Tables = []string{
	"gate_results",
	"trust_cache",
}

func TestMigrateToV8(t *testing.T) {
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
	// The stage pin moved with stage 5: v8's own assertion lived here while S4
	// was the head stage, and S5 ships v9 (payloads-thresholds §2). The pin now
	// lives in migrate_v9_test.go's TestMigrateToV9; what this test still owes is
	// that the v8 STRUCTURES survive a migration that now runs past them.
	if currentSchemaVersion < 8 {
		t.Errorf("currentSchemaVersion = %d; v8 structures are still required",
			currentSchemaVersion)
	}

	for _, table := range v8Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}
	for _, index := range []string{
		"idx_gate_results_step", "idx_gate_results_run", "idx_gate_results_identity",
	} {
		if !hasIndex(t, db, index) {
			t.Errorf("%s missing after migration", index)
		}
	}
}

// TestRewindGuardProbesEveryV8Sentinel is §4.1's obligation, and it derives the
// table list from the DDL itself: a later edit that adds a CREATE TABLE to v8DDL
// and forgets the sentinel fails HERE rather than shipping a database that the
// guard silently declines to repair.
func TestRewindGuardProbesEveryV8Sentinel(t *testing.T) {
	re := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+(\w+)`)
	var created []string
	for _, m := range re.FindAllStringSubmatch(v8DDL, -1) {
		created = append(created, m[1])
	}
	if len(created) == 0 {
		t.Fatal("no CREATE TABLE statements found in v8DDL")
	}
	sort.Strings(created)

	for _, pair := range []struct {
		name string
		list []string
	}{
		{"v8Sentinels", append([]string(nil), v8Sentinels...)},
		{"v8Tables", append([]string(nil), v8Tables...)},
	} {
		got := pair.list
		sort.Strings(got)
		if len(got) != len(created) {
			t.Fatalf("%s = %v, but v8DDL creates %v", pair.name, got, created)
		}
		for i := range created {
			if got[i] != created[i] {
				t.Errorf("%s = %v, but v8DDL creates %v", pair.name, got, created)
				break
			}
		}
	}
}

// stampVersion forces the recorded schema version, so a test can construct the
// half-migrated shapes U3 and U4 describe.
func stampVersion(t *testing.T, db *sql.DB, v int) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE meta SET value = ? WHERE key = 'schema_version'`, v); err != nil {
		t.Fatalf("stamping version %d: %v", v, err)
	}
}

// migratedToV7 builds a database at exactly v7: the full v7 structures, stamped
// 7, with none of v8's.
func migratedToV7(t *testing.T) *sql.DB {
	t.Helper()
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Migrate ran all the way to v8; drop v8's structures and rewind the stamp
	// so the test starts from the shape a v7-era binary left behind.
	for _, table := range v8Tables {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("dropping %s: %v", table, err)
		}
	}
	stampVersion(t, db, 7)
	return db
}

// seedTrailStep inserts a run and a step carrying the given gate_trail bytes,
// returning the step id. It writes the columns directly because the point is to
// reproduce what an S3-era binary STORED, not what today's writers produce.
func seedTrailStep(t *testing.T, db *sql.DB, trail string, updatedMS int64) (runID, stepID int) {
	t.Helper()

	// `steps` carries foreign keys to runs, issues, and workflows, so the
	// referents have to exist before the row under test can.
	res, err := db.Exec(
		`INSERT INTO issues (title, status, created_at, updated_at)
		 VALUES ('seed', 'backlog', '2026-08-03', '2026-08-03')`)
	if err != nil {
		t.Fatalf("seeding an issue: %v", err)
	}
	issueID, _ := res.LastInsertId()

	res, err = db.Exec(
		`INSERT INTO workflows (name, version, source_sha256, body, parsed, created_at_ms)
		 VALUES ('seed', 1, 'x', '', '{}', 1)`)
	if err != nil {
		t.Fatalf("seeding a workflow: %v", err)
	}
	wfID, _ := res.LastInsertId()

	res, err = db.Exec(
		`INSERT INTO runs (request, status, created_at_ms, updated_at_ms)
		 VALUES ('', 'running', 1, 1)`)
	if err != nil {
		t.Fatalf("seeding a run: %v", err)
	}
	rid, _ := res.LastInsertId()

	res, err = db.Exec(
		`INSERT INTO steps
		   (run_id, issue_id, workflow_id, step_name, instance, kind, status,
		    gate_trail, created_at_ms, updated_at_ms)
		 VALUES (?, ?, ?, 'check', 'STEP-1', 'action', 'done', ?, 1, ?)`,
		rid, issueID, wfID, trail, updatedMS)
	if err != nil {
		t.Fatalf("seeding a step: %v", err)
	}
	sid, _ := res.LastInsertId()
	return int(rid), int(sid)
}

// TestMigrateV8U1TrackerShape is U1: this repo's own tracker — stamped 7, every
// v7 sentinel present, `steps` empty because the tracker has issues but no runs.
// The tracker is dogfooded across the stage, so this is the shape the operator's
// own database actually has when the group-2 binary first opens it.
func TestMigrateV8U1TrackerShape(t *testing.T) {
	db := migratedToV7(t)

	var steps int
	if err := db.QueryRow(`SELECT COUNT(*) FROM steps`).Scan(&steps); err != nil {
		t.Fatalf("counting steps: %v", err)
	}
	if steps != 0 {
		t.Fatalf("fixture has %d steps, want 0 (U1 is the no-runs shape)", steps)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v7->v8: %v", err)
	}

	for _, table := range v8Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}
	var backfilled int
	if err := db.QueryRow(`SELECT COUNT(*) FROM gate_results`).Scan(&backfilled); err != nil {
		t.Fatalf("counting gate_results: %v", err)
	}
	if backfilled != 0 {
		t.Errorf("gate_results has %d rows, want 0 — nothing to backfill", backfilled)
	}
}

// TestMigrateV8U2BackfillsTheTrail is U2 and the G1/G2/G3 clauses together: a
// populated trail becomes one row per element, in order, every row stamped
// `stub = 1`, and the trail column KEEPS its original bytes.
func TestMigrateV8U2BackfillsTheTrail(t *testing.T) {
	db := migratedToV7(t)

	trail := `[{"gate":"build","argv":["make","build"],"exit":0,"duration_ms":12,` +
		`"output":"ok","truncated":false,"verdict":"pass","stub":true},` +
		`{"gate":"tests","argv":["make","test"],"exit":1,"duration_ms":34,` +
		`"output":"boom","truncated":true,"verdict":"fail","stub":true}]`
	_, stepID := seedTrailStep(t, db, trail, 777)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v7->v8: %v", err)
	}

	got, err := GateResultsForStep(db, stepID)
	if err != nil {
		t.Fatalf("GateResultsForStep: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d migrated rows, want 2", len(got))
	}

	for i, want := range []struct {
		gate    string
		argv    []string
		exit    int
		dur     int64
		out     string
		trunc   bool
		verdict string
	}{
		{"build", []string{"make", "build"}, 0, 12, "ok", false, "pass"},
		{"tests", []string{"make", "test"}, 1, 34, "boom", true, "fail"},
	} {
		r := got[i]
		if r.Ordinal != i {
			t.Errorf("row %d: ordinal = %d, want %d (G1 is array order)", i, r.Ordinal, i)
		}
		if r.Gate != want.gate {
			t.Errorf("row %d: gate = %q, want %q", i, r.Gate, want.gate)
		}
		if strings.Join(r.Argv, " ") != strings.Join(want.argv, " ") {
			t.Errorf("row %d: argv = %v, want %v", i, r.Argv, want.argv)
		}
		if r.Exit == nil || *r.Exit != want.exit {
			t.Errorf("row %d: exit = %v, want %d", i, r.Exit, want.exit)
		}
		if r.DurationMS != want.dur {
			t.Errorf("row %d: duration_ms = %d, want %d", i, r.DurationMS, want.dur)
		}
		if r.Output != want.out {
			t.Errorf("row %d: output = %q, want %q", i, r.Output, want.out)
		}
		if r.Truncated != want.trunc {
			t.Errorf("row %d: truncated = %v, want %v", i, r.Truncated, want.trunc)
		}
		if r.Verdict != want.verdict {
			t.Errorf("row %d: verdict = %q, want %q", i, r.Verdict, want.verdict)
		}
		// G2: the stamp is a FACT — every trail result came from S3's
		// pass-through runner, which is the only thing that ever wrote one.
		if !r.Stub {
			t.Errorf("row %d: stub = false, want true (G2 stamps every migrated row)", i)
		}
		// The step's updated_at_ms is the honest approximation (G1).
		if r.CreatedAtMS != 777 {
			t.Errorf("row %d: created_at_ms = %d, want 777", i, r.CreatedAtMS)
		}
	}

	// G3: the trail column is RETAINED, byte-identical. It is the migration's
	// own evidence, and the never-mutate rule forbids dropping it.
	var kept string
	if err := db.QueryRow(
		`SELECT gate_trail FROM steps WHERE id = ?`, stepID).Scan(&kept); err != nil {
		t.Fatalf("reading the retained trail: %v", err)
	}
	if kept != trail {
		t.Errorf("gate_trail was mutated by the migration:\n got %s\nwant %s", kept, trail)
	}
}

// TestMigrateV8U3RewindsWhenGateResultsAbsent is U3 — the row that exists
// because of the v7 lesson. Stamped 8 with `gate_results` missing is exactly the
// group-2-partial dogfood shape, and a guard that trusted the stamp would leave
// that database permanently incomplete.
func TestMigrateV8U3RewindsWhenGateResultsAbsent(t *testing.T) {
	db := migratedToV7(t)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v7->v8: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE gate_results`); err != nil {
		t.Fatalf("dropping gate_results: %v", err)
	}
	stampVersion(t, db, 8)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate against the partial v8: %v", err)
	}
	for _, table := range v8Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s still missing — the rewind guard did not fire", table)
		}
	}
}

// TestMigrateV8U4RewindsWhenTrustCacheAbsent is U4: the same rewind, driven by
// the OTHER sentinel. Probing only the first table would pass U3 and fail here,
// which is why the guard probes the full set.
func TestMigrateV8U4RewindsWhenTrustCacheAbsent(t *testing.T) {
	db := migratedToV7(t)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate v7->v8: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE trust_cache`); err != nil {
		t.Fatalf("dropping trust_cache: %v", err)
	}
	stampVersion(t, db, 8)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate against the partial v8: %v", err)
	}
	for _, table := range v8Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s still missing — the rewind guard did not fire", table)
		}
	}
}

// TestMigrateV8U5IsIdempotent is U5 and G4: re-running the backfill against an
// already-migrated database duplicates nothing. Idempotence is not optional —
// the rewind guard re-runs this migration against a partially-migrated database
// by design.
func TestMigrateV8U5IsIdempotent(t *testing.T) {
	db := migratedToV7(t)
	trail := `[{"gate":"build","argv":["make"],"exit":0,"duration_ms":1,` +
		`"output":"","truncated":false,"verdict":"pass","stub":true}]`
	_, stepID := seedTrailStep(t, db, trail, 5)

	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	first, err := GateResultsForStep(db, stepID)
	if err != nil {
		t.Fatalf("GateResultsForStep: %v", err)
	}

	// Re-run the migration itself, exactly as the rewind guard would.
	stampVersion(t, db, 7)
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	second, err := GateResultsForStep(db, stepID)
	if err != nil {
		t.Fatalf("GateResultsForStep after re-run: %v", err)
	}

	if len(first) != len(second) {
		t.Errorf("re-running the migration changed the row count: %d -> %d (G4)",
			len(first), len(second))
	}
	if len(second) != 1 {
		t.Errorf("got %d rows, want 1", len(second))
	}
}

// TestMigrateV8U6FromTheV4Baseline is U6: the committed fixture migrates 4->8 in
// ONE pass, and the v8 structures are asserted present BEFORE any golden diff is
// trusted — engine-spine §3's rule, because a golden diff against a database
// that failed to migrate passes vacuously.
func TestMigrateV8U6FromTheV4Baseline(t *testing.T) {
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
		t.Fatalf("Migrate 4->8: %v", err)
	}

	v, err := SchemaVersion(db)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != currentSchemaVersion {
		t.Errorf("schema_version = %d, want %d", v, currentSchemaVersion)
	}
	for _, table := range append(append([]string(nil), v7Tables...), v8Tables...) {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after the 4->8 migration", table)
		}
	}
}

// TestMigrateV8U7MalformedTrail is U7 and G5: a trail that does not parse is
// RECORDED, not fatal. A migration that aborted on one malformed JSON blob would
// make the database unopenable — the failure belongs where an operator sees it.
func TestMigrateV8U7MalformedTrail(t *testing.T) {
	db := migratedToV7(t)
	_, stepID := seedTrailStep(t, db, `{"not":"an array"`, 42)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate must succeed despite a malformed trail (G5): %v", err)
	}

	got, err := GateResultsForStep(db, stepID)
	if err != nil {
		t.Fatalf("GateResultsForStep: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want exactly 1 recording the parse failure", len(got))
	}
	r := got[0]
	if r.Verdict != GateVerdictUnmatched {
		t.Errorf("verdict = %q, want %q", r.Verdict, GateVerdictUnmatched)
	}
	if !r.Stub {
		t.Errorf("stub = false, want true")
	}
	if r.Reason == "" || !strings.Contains(r.Reason, "did not parse") {
		t.Errorf("reason = %q, want it to name the parse failure", r.Reason)
	}
}

// TestGateTrailIsNotWrittenAtV8 is G3's other half: the column is retained, but
// nothing at v8 WRITES it. The assertion is source-level because the property is
// about which writer the runner reaches, and a runtime assertion would only
// cover the paths a test happened to drive.
func TestGateTrailIsNotWrittenAtV8(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "engine", "saga.go"))
	if err != nil {
		t.Fatalf("reading saga.go: %v", err)
	}
	if strings.Contains(string(src), "SetStepGateTrailTx") {
		t.Error("saga.go still calls SetStepGateTrailTx; at v8 the runner writes " +
			"gate_results (G3)")
	}
}

// TestUnmatchedGateResultStoresNulls is §4.2's field-shape decision, which is a
// security property rather than a nicety: `argv` and `exit` are NULL on an
// unmatched result, not `[]` and `0`. A zero exit code on a gate that never ran
// is exactly the confusion T11 exists to prevent.
func TestUnmatchedGateResultStoresNulls(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	runID, stepID := seedTrailStep(t, db, "", 1)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = InsertGateResultTx(tx, GateResultRow{
		RunID: runID, StepID: stepID, Gate: "tests", Ordinal: 0,
		Verdict: GateVerdictUnmatched, Reason: "no trust entry", CreatedAtMS: 9,
	})
	if err != nil {
		t.Fatalf("InsertGateResultTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var argv, exit sql.NullString
	if err := db.QueryRow(
		`SELECT argv, exit FROM gate_results WHERE step_id = ?`, stepID,
	).Scan(&argv, &exit); err != nil {
		t.Fatalf("reading the row: %v", err)
	}
	if argv.Valid {
		t.Errorf("argv = %q, want NULL on an unmatched result", argv.String)
	}
	if exit.Valid {
		t.Errorf("exit = %q, want NULL on an unmatched result", exit.String)
	}

	got, err := GateResultsForStep(db, stepID)
	if err != nil {
		t.Fatalf("GateResultsForStep: %v", err)
	}
	if len(got) != 1 || got[0].Exit != nil || got[0].Argv != nil {
		t.Errorf("round-tripped result = %+v, want nil Argv and nil Exit", got[0])
	}
	// N4: nothing this stage records is a stub.
	if got[0].Stub {
		t.Error("stub = true on a result this stage recorded; stub means S3 pass-through")
	}
}
