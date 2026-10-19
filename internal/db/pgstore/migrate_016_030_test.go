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
