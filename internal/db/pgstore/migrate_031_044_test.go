package pgstore

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"testing"

	sqlitestore "github.com/ALT-F4-LLC/docket/internal/db"
)

// TestPgMigrateSchemaParity compares the full ported chain with
// testdata/schema_latest.json, dumped from a SQLite database migrated to
// sqlitestore.LatestSchemaVersion().
func TestPgMigrateSchemaParity(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate on an empty database: %v", err)
	}
	assertSchemaVersion(t, db, sqlitestore.LatestSchemaVersion())

	want := expectedColumns(t, "testdata/schema_latest.json")
	got := readColumns(t, db)
	if !slices.Equal(got, want) {
		t.Errorf("information_schema columns differ from testdata/schema_latest.json under the type map:\n%s",
			columnDiff(want, got))
	}
}

func TestPgMigrateRerun(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)
	latest := sqlitestore.LatestSchemaVersion()

	if err := applyMigrations(ctx, db, 0, latest); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstColumns := readColumns(t, db)
	firstCounts := readRowCounts(t, db)

	if err := applyMigrations(ctx, db, 0, latest); err != nil {
		t.Fatalf("second run over a version-%d database: %v", latest, err)
	}
	assertSchemaVersion(t, db, latest)

	if got := readColumns(t, db); !slices.Equal(got, firstColumns) {
		t.Errorf("columns changed on the second run:\n%s", columnDiff(firstColumns, got))
	}
	if got := readRowCounts(t, db); !maps.Equal(got, firstCounts) {
		t.Errorf("row counts changed on the second run:\nfirst  %v\nsecond %v", firstCounts, got)
	}
}

func TestPgMigrateRowPreservation(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 30); err != nil {
		t.Fatalf("migrating to version 30: %v", err)
	}
	// One row in every table a v31-v47 migration alters. The step is pending,
	// so v39's back-fill of terminal steps leaves recorded_at_ms at its default.
	for _, stmt := range []string{
		`INSERT INTO issues (id, title, created_at, updated_at) VALUES (1, 'a', 't', 't')`,
		`INSERT INTO workflows (id, name, version, source_sha256, body, parsed, created_at_ms)
			VALUES (1, 'w', 1, 's', 'b', '{}', 1)`,
		`INSERT INTO runs (id, request, created_at_ms, updated_at_ms) VALUES (1, 'r', 2, 3)`,
		`INSERT INTO steps (id, run_id, issue_id, workflow_id, step_name, instance, kind, attempt, created_at_ms, updated_at_ms)
			VALUES (1, 1, 1, 1, 'implement', 'implement@0', 'executor', 3, 4, 5)`,
		`INSERT INTO dispatches (id, run_id, opened_seq, expires_ms, created_at_ms) VALUES (1, 1, 9, 10, 11)`,
		`INSERT INTO schemas (id, name, version, source_sha256, body, created_at_ms)
			VALUES (40, 'findings', 3, 'sha-s', '{"x":1}', 12)`,
		`INSERT INTO gate_results (id, run_id, step_id, gate, exit, output, verdict, created_at_ms)
			VALUES (1, 1, 1, 'tests', 1, 'out', 'fail', 6)`,
		`INSERT INTO proposals (id, description, required_voters, created_at, updated_at)
			VALUES (1, 'p', 3, 't', 't')`,
		`INSERT INTO gate_override_grants (id, run_id, origin_step_id, gate, exit, reason, created_at_ms)
			VALUES (1, 1, 1, 'tests', 1, 'timeout', 8)`,
		`INSERT INTO run_notes (id, run_id, text, created_at_ms) VALUES (1, 1, 'n', 13)`,
		`INSERT INTO pins (id, run_id, kind, ref, sha256) VALUES (1, 1, 'file', 'a.md', 'sha-p')`,
		`INSERT INTO idempotency_keys (scope, key, entity_id, created_at_ms, seq) VALUES ('issue', 'k', 1, 14, 15)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seeding at version 30: %v\n%s", err, stmt)
		}
	}

	// The columns v31-v47 add to each seeded table, with the value their SQL
	// default gives an existing row, as to_jsonb renders it. issues and steps
	// gain none of v32-v35's columns: v1 and v7 already create them.
	added := map[string]map[string]any{
		"issues":               {},
		"dispatches":           {"extended_seq": 0.0},
		"schemas":              {"deprecated_at_ms": nil},
		"gate_results":         {"target_sha": ""},
		"proposals":            {"hold_on_dissent": 0.0},
		"gate_override_grants": {"argv": "legacy"},
		"steps": {
			"recorded_at_ms": 0.0,
			"authority":      "",
			"authority_ref":  "",
			"claim_phase":    "",
			"recovery_hash":  nil,
			"loop_cap":       0.0,
		},
		"run_notes":        {"authority": ""},
		"pins":             {"content": nil},
		"idempotency_keys": {"fingerprint": nil},
	}

	before := map[string][]map[string]any{}
	for table := range added {
		before[table] = readRowObjects(t, db, table)
		if len(before[table]) == 0 {
			t.Fatalf("%s holds no rows at version 30, want the seeded row", table)
		}
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("migrating version 30 to the latest version: %v", err)
	}

	for table, defaults := range added {
		want := before[table]
		for _, row := range want {
			for column, value := range defaults {
				row[column] = value
			}
		}
		if got := readRowObjects(t, db, table); !reflect.DeepEqual(got, want) {
			t.Errorf("%s rows after the latest version:\ngot  %v\nwant %v", table, got, want)
		}
	}
}

// TestPgMigrateVersionMatchesSQLite ties the ported chain's reach to the
// SQLite migrations map, so a SQLite migration without a port fails here.
func TestPgMigrateVersionMatchesSQLite(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate on an empty database: %v", err)
	}
	got, err := schemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("reading schema version: %v", err)
	}
	if want := sqlitestore.LatestSchemaVersion(); got != want {
		t.Errorf("PostgreSQL schema version after Migrate = %d, want the highest SQLite migration %d", got, want)
	}
}

// TestPgMigrateV39BackfillsRecordedAt checks that v39 stamps recorded_at_ms
// from updated_at_ms on terminal steps only, and leaves a stamp already set.
func TestPgMigrateV39BackfillsRecordedAt(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 38); err != nil {
		t.Fatalf("migrating to version 38: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO workflows (id, name, version, source_sha256, body, parsed, created_at_ms)
			VALUES (1, 'w', 1, 's', 'b', '{}', 1)`,
		`INSERT INTO runs (id, created_at_ms, updated_at_ms) VALUES (1, 1, 1)`,
		`INSERT INTO steps (id, run_id, workflow_id, step_name, instance, kind, status, created_at_ms, updated_at_ms) VALUES
			(1, 1, 1, 'a', 'a@0', 'executor', 'done', 1, 10),
			(2, 1, 1, 'b', 'b@0', 'executor', 'skipped', 1, 20),
			(3, 1, 1, 'c', 'c@0', 'executor', 'superseded', 1, 30),
			(4, 1, 1, 'd', 'd@0', 'executor', 'failed-routed', 1, 40),
			(5, 1, 1, 'e', 'e@0', 'executor', 'pending', 1, 50),
			(6, 1, 1, 'f', 'f@0', 'executor', 'waiting-human', 1, 60)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	if err := applyMigrations(ctx, db, 38, 39); err != nil {
		t.Fatalf("applying v39: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE steps SET recorded_at_ms = 99 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, db, 38, 39); err != nil {
		t.Fatalf("rerunning v39: %v", err)
	}

	got := queryStrings(t, db, `SELECT recorded_at_ms::text FROM steps ORDER BY id`)
	want := []string{"99", "20", "30", "40", "0", "0"}
	if !slices.Equal(got, want) {
		t.Errorf("steps.recorded_at_ms after v39 = %q, want %q", got, want)
	}
}

// TestPgMigrateV46SeedsEventWatermark checks that v46 seeds the watermark one
// below the lowest surviving event seq, and that a rerun keeps the row.
func TestPgMigrateV46SeedsEventWatermark(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 45); err != nil {
		t.Fatalf("migrating to version 45: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO events (seq, at_ms, kind) VALUES (7, 1, 'a'), (9, 1, 'b')`); err != nil {
		t.Fatalf("seeding events: %v", err)
	}

	if err := applyMigrations(ctx, db, 45, 46); err != nil {
		t.Fatalf("applying v46: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM events WHERE seq = 7`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, db, 45, 46); err != nil {
		t.Fatalf("rerunning v46: %v", err)
	}

	got := queryStrings(t, db, `SELECT id::text || ' ' || seq::text FROM event_low_watermark`)
	if want := []string{"1 6"}; !slices.Equal(got, want) {
		t.Errorf("event_low_watermark rows = %q, want %q", got, want)
	}
}

func TestPgMigrateV46SeedsZeroWithoutEvents(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 46); err != nil {
		t.Fatalf("migrating to version 46: %v", err)
	}
	got := queryStrings(t, db, `SELECT id::text || ' ' || seq::text FROM event_low_watermark`)
	if want := []string{"1 0"}; !slices.Equal(got, want) {
		t.Errorf("event_low_watermark rows = %q, want %q", got, want)
	}
}
