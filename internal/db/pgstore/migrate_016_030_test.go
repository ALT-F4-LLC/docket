package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// TestPgMigrate016030SchemaParity compares the ported schema with
// testdata/schema_v30.json, dumped from a SQLite database migrated to v30.
// That fixture holds six columns that SQLite migrations after v30 add:
// steps.park_reason (v32), steps.park_class (v33), issues.size (v34), and
// steps.loop_rounds_run, loop_trigger_step and loop_latest_verdict (v35).
// SQLite's creation DDL is written in current shape, so a database built and
// migrated to v30 already has them, and the PostgreSQL v1/v7 port mirrors
// that DDL. Both sides carry them, so the comparison keeps them.
func TestPgMigrate016030SchemaParity(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate on an empty database: %v", err)
	}
	assertSchemaVersion(t, db, 30)

	want := expectedColumns(t, "testdata/schema_v30.json")
	got := readColumns(t, db)
	if !slices.Equal(got, want) {
		t.Errorf("information_schema columns differ from testdata/schema_v30.json under the type map:\n%s",
			columnDiff(want, got))
	}
}

func TestPgMigrate016030Idempotent(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 30); err != nil {
		t.Fatalf("migrating to version 30: %v", err)
	}
	firstColumns := readColumns(t, db)
	firstForeignKeys := readForeignKeys(t, db)

	if err := applyMigrations(ctx, db, 15, 30); err != nil {
		t.Fatalf("rerunning v16-v30 over a version-30 database: %v", err)
	}
	assertSchemaVersion(t, db, 30)

	if got := readColumns(t, db); !slices.Equal(got, firstColumns) {
		t.Errorf("columns changed on the rerun:\n%s", columnDiff(firstColumns, got))
	}
	if got := readForeignKeys(t, db); !slices.Equal(got, firstForeignKeys) {
		t.Errorf("foreign keys changed on the rerun:\nfirst  %v\nsecond %v", firstForeignKeys, got)
	}
}

func TestPgMigrate016030PreservesRows(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	// One row in every table a v16-v30 migration alters, seeded and captured
	// at version 15; gate_override_grants, which v24 creates, at version 24.
	stages := []struct {
		version  int
		captured []string
		seed     []string
	}{
		{15, []string{"issues", "runs", "run_issues", "steps", "gate_results", "proposals", "vote_usage"}, []string{
			`INSERT INTO issues (id, title, created_at, updated_at) VALUES (1, 'a', 't', 't')`,
			`INSERT INTO workflows (id, name, version, source_sha256, body, parsed, created_at_ms)
				VALUES (1, 'w', 1, 's', 'b', '{}', 1)`,
			`INSERT INTO runs (id, request, created_at_ms, updated_at_ms) VALUES (1, 'r', 2, 3)`,
			`INSERT INTO run_issues (run_id, issue_id, workflow_id, loop_count) VALUES (1, 1, 1, 2)`,
			`INSERT INTO steps (id, run_id, issue_id, workflow_id, step_name, instance, kind, attempt, created_at_ms, updated_at_ms)
				VALUES (1, 1, 1, 1, 'implement', 'implement@0', 'executor', 3, 4, 5)`,
			`INSERT INTO gate_results (id, run_id, step_id, gate, exit, output, verdict, created_at_ms)
				VALUES (1, 1, 1, 'tests', 1, 'out', 'fail', 6)`,
			`INSERT INTO proposals (id, description, required_voters, created_at, updated_at)
				VALUES (1, 'p', 3, 't', 't')`,
			`INSERT INTO votes (id, proposal_id, voter_name, verdict, confidence, domain_relevance, created_at)
				VALUES (1, 1, 'v', 'approve', 0.5, 0.25, 't')`,
			`INSERT INTO vote_usage (id, vote_id, unit, quantity, created_at_ms) VALUES (1, 1, 'tokens', 10, 7)`,
		}},
		{24, []string{"gate_override_grants"}, []string{
			`INSERT INTO gate_override_grants (id, run_id, origin_step_id, gate, exit, reason, note, covered_steps, created_at_ms)
				VALUES (1, 1, 1, 'tests', 1, 'timeout', 'env', 2, 8)`,
		}},
	}
	// The columns v16-v30 add to each seeded table, with the value their SQL
	// default gives an existing row, as to_jsonb renders it. issues gains none:
	// v1 already creates v18's resolution.
	added := map[string]map[string]any{
		"steps":                {"attempt_base": 0.0},
		"vote_usage":           {"source": "reported"},
		"issues":               {},
		"runs":                 {"conductor_token_hash": nil},
		"run_issues":           {"loop_grants": 0.0},
		"gate_results":         {"fingerprint": ""},
		"proposals":            {"sealed": 0.0},
		"gate_override_grants": {"fingerprint": ""},
	}

	before := map[string][]map[string]any{}
	from := 0
	for _, stage := range stages {
		if err := applyMigrations(ctx, db, from, stage.version); err != nil {
			t.Fatalf("migrating to version %d: %v", stage.version, err)
		}
		from = stage.version
		for _, stmt := range stage.seed {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("seeding at version %d: %v\n%s", stage.version, err, stmt)
			}
		}
		for _, table := range stage.captured {
			before[table] = readRowObjects(t, db, table)
			if len(before[table]) != 1 {
				t.Fatalf("%s holds %d rows at version %d, want the 1 seeded", table, len(before[table]), stage.version)
			}
		}
	}
	if len(before) != len(added) {
		t.Fatalf("captured tables %v, want every table in %v", before, added)
	}

	if err := applyMigrations(ctx, db, from, 30); err != nil {
		t.Fatalf("migrating to version 30: %v", err)
	}

	for table, defaults := range added {
		want := before[table]
		for _, row := range want {
			for column, value := range defaults {
				row[column] = value
			}
		}
		if got := readRowObjects(t, db, table); !reflect.DeepEqual(got, want) {
			t.Errorf("%s rows after v30:\ngot  %v\nwant %v", table, got, want)
		}
	}
}

func TestPgMigrateV22MarksPausedRuns(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 21); err != nil {
		t.Fatalf("migrating to version 21: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO workflows (id, name, version, source_sha256, body, parsed, created_at_ms)
			VALUES (1, 'w', 1, 's', 'b', '{}', 1)`,
		`INSERT INTO runs (id, status, breach_reason, created_at_ms, updated_at_ms) VALUES
			(1, 'waiting-human', 'over budget', 1, 1),
			(2, 'waiting-human', NULL, 1, 1),
			(3, 'waiting-human', NULL, 1, 1),
			(4, 'active', 'over budget', 1, 1),
			(5, 'waiting-human', '', 1, 1)`,
		`INSERT INTO steps (run_id, workflow_id, step_name, instance, kind, status, created_at_ms, updated_at_ms)
			VALUES (3, 1, 'a', 'a@0', 'executor', 'waiting-human', 1, 1)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	if err := applyMigrations(ctx, db, 21, 22); err != nil {
		t.Fatalf("applying v22: %v", err)
	}

	rows, err := db.Query(`SELECT pause_origin FROM runs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var origin string
		if err := rows.Scan(&origin); err != nil {
			t.Fatal(err)
		}
		got = append(got, origin)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// A breach marks a parked run budget; a parked run with no breach and no
	// parked step was paused by its operator; a run with a parked step, or one
	// not parked, keeps the empty origin.
	want := []string{"budget", "operator", "", "", "operator"}
	if !slices.Equal(got, want) {
		t.Errorf("runs.pause_origin after v22 = %q, want %q", got, want)
	}
}

// TestPgMigrate016030ColumnMigrationsAddTheirColumns drops each column a
// v16-v30 migration adds and reapplies that migration alone. v1 and v7 already
// create several of these columns, so the schema parity test cannot tell
// whether their migration still adds them. The expected types, nullability and
// defaults are the SQLite migrations' DDL under the type map.
func TestPgMigrate016030ColumnMigrationsAddTheirColumns(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate on an empty database: %v", err)
	}

	type column struct{ table, name, spec string }
	for _, tc := range []struct {
		version int
		columns []column
	}{
		{16, []column{{"steps", "attempt_base", "bigint NOT NULL DEFAULT 0"}}},
		{17, []column{{"vote_usage", "source", "text NOT NULL DEFAULT 'reported'::text"}}},
		{18, []column{{"issues", "resolution", "text NOT NULL DEFAULT ''::text"}}},
		{19, []column{{"runs", "usage_budget", "double precision NOT NULL DEFAULT 0"}}},
		{20, []column{{"run_issues", "loop_grants", "bigint NOT NULL DEFAULT 0"}}},
		{21, []column{{"gate_results", "stub_entry", "bigint NOT NULL DEFAULT 0"}}},
		{22, []column{{"runs", "pause_origin", "text NOT NULL DEFAULT ''::text"}}},
		{23, []column{
			{"steps", "failed_attempts", "bigint NOT NULL DEFAULT 0"},
			{"steps", "reaped_claims", "bigint NOT NULL DEFAULT 0"},
		}},
		{27, []column{{"steps", "last_claim_end", "text NOT NULL DEFAULT ''::text"}}},
		{28, []column{{"proposals", "sealed", "bigint NOT NULL DEFAULT 0"}}},
		{29, []column{{"runs", "conductor_token_hash", "text"}}},
		{30, []column{
			{"gate_results", "fingerprint", "text NOT NULL DEFAULT ''::text"},
			{"gate_override_grants", "fingerprint", "text NOT NULL DEFAULT ''::text"},
		}},
	} {
		for _, c := range tc.columns {
			if _, err := db.ExecContext(ctx, `ALTER TABLE `+quoteIdent(c.table)+` DROP COLUMN `+quoteIdent(c.name)); err != nil {
				t.Fatalf("dropping %s.%s: %v", c.table, c.name, err)
			}
		}
		if err := applyMigrations(ctx, db, tc.version-1, tc.version); err != nil {
			t.Fatalf("reapplying v%d: %v", tc.version, err)
		}
		for _, c := range tc.columns {
			if got := readColumnSpec(t, db, c.table, c.name); got != c.spec {
				t.Errorf("v%d %s.%s = %q, want %q", tc.version, c.table, c.name, got, c.spec)
			}
		}
	}
}

// TestPgMigrate016030NewTablesCascadeDeletes checks the ON DELETE CASCADE
// rules SQLite's v24-v26 declare. Run 1 has no steps, so its grant can go
// only through run_id; run 2's rows show that a delete reaches no other run's
// rows.
func TestPgMigrate016030NewTablesCascadeDeletes(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate on an empty database: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO workflows (id, name, version, source_sha256, body, parsed, created_at_ms)
			VALUES (1, 'w', 1, 's', 'b', '{}', 1)`,
		`INSERT INTO runs (id, created_at_ms, updated_at_ms) VALUES (1, 1, 1), (2, 1, 1)`,
		`INSERT INTO steps (id, run_id, workflow_id, step_name, instance, kind, created_at_ms, updated_at_ms)
			VALUES (2, 2, 1, 'a', 'a@0', 'executor', 1, 1), (3, 2, 1, 'b', 'b@0', 'executor', 1, 1)`,
		`INSERT INTO gate_override_grants (id, run_id, origin_step_id, gate, created_at_ms)
			VALUES (1, 1, 2, 'tests', 1), (2, 2, 2, 'tests', 1), (3, 2, 3, 'tests', 1)`,
		`INSERT INTO stale_target_waivers (id, run_id, step_instance, target_sha, created_at_ms)
			VALUES (1, 1, 'a@0', 'sha', 1), (2, 2, 'a@0', 'sha', 1)`,
		`INSERT INTO run_notes (id, run_id, text, created_at_ms) VALUES (1, 1, 'n', 1), (2, 2, 'n', 1)`,
		`DELETE FROM runs WHERE id = 1`,
		`DELETE FROM steps WHERE id = 2`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}

	for table, want := range map[string][]int64{
		"gate_override_grants": {3},
		"stale_target_waivers": {2},
		"run_notes":            {2},
	} {
		var got []int64
		for _, row := range readRowObjects(t, db, table) {
			got = append(got, int64(row["id"].(float64)))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s ids after deleting run 1 and step 2 = %v, want %v", table, got, want)
		}
	}
}

// TestPgMigrate016030NewTablesIndexesAndDefaults checks the indexes and column
// defaults of the tables v24-v26 create against SQLite's DDL for them.
func TestPgMigrate016030NewTablesIndexesAndDefaults(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate on an empty database: %v", err)
	}
	tables := []string{"gate_override_grants", "stale_target_waivers", "run_notes"}

	wantIndexes := []string{
		"CREATE INDEX idx_gate_override_grants_run ON gate_override_grants USING btree (run_id)",
		"CREATE INDEX idx_run_notes_run ON run_notes USING btree (run_id)",
		"CREATE INDEX idx_stale_target_waivers_run ON stale_target_waivers USING btree (run_id)",
		"CREATE UNIQUE INDEX gate_override_grants_pkey ON gate_override_grants USING btree (id)",
		"CREATE UNIQUE INDEX run_notes_pkey ON run_notes USING btree (id)",
		"CREATE UNIQUE INDEX stale_target_waivers_pkey ON stale_target_waivers USING btree (id)",
	}
	gotIndexes := queryStrings(t, db, `SELECT replace(indexdef, current_schema() || '.', '')
		FROM pg_indexes WHERE schemaname = current_schema() AND tablename = ANY($1)`, tables)
	slices.Sort(gotIndexes)
	if !slices.Equal(gotIndexes, wantIndexes) {
		t.Errorf("indexes = %q\nwant %q", gotIndexes, wantIndexes)
	}

	wantDefaults := []string{
		"gate_override_grants.covered_steps 0",
		"gate_override_grants.fingerprint ''::text",
		"gate_override_grants.note ''::text",
		"gate_override_grants.reason ''::text",
		"stale_target_waivers.note ''::text",
	}
	gotDefaults := queryStrings(t, db, `SELECT table_name || '.' || column_name || ' ' || column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ANY($1) AND column_default IS NOT NULL`, tables)
	slices.Sort(gotDefaults)
	if !slices.Equal(gotDefaults, wantDefaults) {
		t.Errorf("column defaults = %q\nwant %q", gotDefaults, wantDefaults)
	}
}

// readColumnSpec renders a column as its data type, NOT NULL when it has
// that constraint, and its default expression when it has one.
func readColumnSpec(t *testing.T, db *sql.DB, table, column string) string {
	t.Helper()
	specs := queryStrings(t, db, `SELECT data_type
			|| CASE WHEN is_nullable = 'NO' THEN ' NOT NULL' ELSE '' END
			|| coalesce(' DEFAULT ' || column_default, '')
		FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, table, column)
	if len(specs) != 1 {
		t.Fatalf("%s.%s matches %d columns, want 1", table, column, len(specs))
	}
	return specs[0]
}

func queryStrings(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// readRowObjects decodes each row of table as to_jsonb renders it, ordered by
// that rendering.
func readRowObjects(t *testing.T, db *sql.DB, table string) []map[string]any {
	t.Helper()
	rows, err := db.Query(`SELECT to_jsonb(t)::text FROM ` + quoteIdent(table) + ` t ORDER BY 1`)
	if err != nil {
		t.Fatalf("reading %s: %v", table, err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatalf("decoding a %s row: %v", table, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
