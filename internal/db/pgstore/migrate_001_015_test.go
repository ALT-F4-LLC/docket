package pgstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// sqliteToPostgresType maps the SQLite declared types in the schema fixtures
// to the information_schema data_type each ported column must have. SQLite
// integers are 64-bit, so INTEGER is bigint.
var sqliteToPostgresType = map[string]string{
	"INTEGER": "bigint",
	"TEXT":    "text",
	"REAL":    "double precision",
	"BLOB":    "bytea",
}

// jsonColumns are the TEXT columns whose every writer stores a JSON document;
// the port declares them jsonb. TEXT columns whose exact bytes are a contract
// stay text: artifacts.payload is covered by artifacts.sha256,
// dispatch_rows.row_json by row_sha256, and schemas.body by source_sha256,
// and jsonb does not keep the bytes it was given.
var jsonColumns = []string{
	"action_results.argv",
	"events.data",
	"gate_results.argv",
	"issues.scope_globs",
	"proposals.domain_tags",
	"proposals.files_changed",
	"run_issues.issue_snapshot",
	"schemas.ordered",
	"steps.gate_trail",
	"steps.metadata",
	"votes.findings_json",
	"votes.metadata",
	"workflows.parsed",
}

// schemaColumn is one column of a schema fixture or of information_schema.
// Nullable is false for a NOT NULL column and for a primary-key column, which
// PostgreSQL makes NOT NULL and SQLite enforces as such only for its rowid.
type schemaColumn struct {
	Table    string `json:"table"`
	Column   string `json:"column"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

func TestMigrateV15SchemaParity(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 15); err != nil {
		t.Fatalf("migrating an empty database to version 15: %v", err)
	}
	assertSchemaVersion(t, db, 15)

	want := expectedColumns(t, "testdata/schema_v15.json")
	got := readColumns(t, db)
	if !slices.Equal(got, want) {
		t.Errorf("information_schema columns differ from testdata/schema_v15.json under the type map:\n%s",
			columnDiff(want, got))
	}
}

func TestMigrateV15Rerun(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 15); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstColumns := readColumns(t, db)
	firstCounts := readRowCounts(t, db)
	firstForeignKeys := readForeignKeys(t, db)

	if err := applyMigrations(ctx, db, 0, 15); err != nil {
		t.Fatalf("second run over a version-15 database: %v", err)
	}
	assertSchemaVersion(t, db, 15)

	if got := readColumns(t, db); !slices.Equal(got, firstColumns) {
		t.Errorf("columns changed on the second run:\n%s", columnDiff(firstColumns, got))
	}
	if got := readRowCounts(t, db); !maps.Equal(got, firstCounts) {
		t.Errorf("row counts changed on the second run:\nfirst  %v\nsecond %v", firstCounts, got)
	}
	if got := readForeignKeys(t, db); !slices.Equal(got, firstForeignKeys) {
		t.Errorf("foreign keys changed on the second run:\nfirst  %v\nsecond %v", firstForeignKeys, got)
	}
}

func TestMigrateRefusesNewerStore(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate on an empty database: %v", err)
	}
	newer := fmt.Sprint(latestSchemaVersion + 1)
	if _, err := db.ExecContext(ctx, `UPDATE meta SET value = $1 WHERE key = 'schema_version'`, newer); err != nil {
		t.Fatal(err)
	}

	err := Migrate(ctx, db)
	if err == nil || !strings.Contains(err.Error(), newer) {
		t.Fatalf("Migrate on a store stamped %s returned %v, want a refusal naming %s", newer, err, newer)
	}
}

func TestMigrateV12RowsPreserved(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 11); err != nil {
		t.Fatalf("migrating to version 11: %v", err)
	}
	assertSchemaVersion(t, db, 11)

	// One row in each table SQLite's v12 rebuilds (labels, workflows,
	// schemas), each referenced by a child row so the foreign keys have
	// something to hold.
	for _, stmt := range []string{
		`INSERT INTO issues (id, title, created_at, updated_at) VALUES (1, 'a', 't', 't')`,
		`INSERT INTO labels (id, name, color) VALUES (7, 'bug', 'red')`,
		`INSERT INTO issue_labels (issue_id, label_id) VALUES (1, 7)`,
		`INSERT INTO workflows (id, name, version, description, source_sha256, body, parsed, created_at_ms, deprecated_at_ms)
			VALUES (5, 'review', 2, 'd', 'sha-w', 'b', '{"steps":[1]}', 100, 200)`,
		`INSERT INTO runs (id, created_at_ms, updated_at_ms) VALUES (3, 1, 1)`,
		`INSERT INTO run_issues (run_id, issue_id, workflow_id) VALUES (3, 1, 5)`,
		`INSERT INTO steps (id, run_id, issue_id, workflow_id, step_name, instance, kind, created_at_ms, updated_at_ms)
			VALUES (9, 3, 1, 5, 'implement', 'implement@0', 'executor', 1, 1)`,
		`INSERT INTO schemas (id, name, version, source_path, source_sha256, body, ordered, created_at_ms)
			VALUES (40, 'findings', 3, 'p', 'sha-s', '{"x":1}', '{"a":[1]}', 300)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seeding %q: %v", strings.Fields(stmt)[2], err)
		}
	}

	rebuilt := []string{"labels", "workflows", "schemas"}
	before := map[string][]string{}
	for _, table := range rebuilt {
		before[table] = readRowsWithoutProjectID(t, db, table)
	}
	fksBefore := readForeignKeys(t, db)

	if err := applyMigrations(ctx, db, 11, 12); err != nil {
		t.Fatalf("applying v12: %v", err)
	}
	assertSchemaVersion(t, db, 12)

	for _, table := range rebuilt {
		if got := readRowsWithoutProjectID(t, db, table); !slices.Equal(got, before[table]) {
			t.Errorf("%s rows changed across v12:\nbefore %v\nafter  %v", table, before[table], got)
		}
		var unscoped int
		if err := db.QueryRow(`SELECT count(*) FROM ` + quoteIdent(table) + ` WHERE project_id <> 1`).Scan(&unscoped); err != nil {
			t.Fatalf("reading %s.project_id: %v", table, err)
		}
		if unscoped != 0 {
			t.Errorf("%s has %d rows not backfilled to project 1", table, unscoped)
		}
	}

	fksAfter := readForeignKeys(t, db)
	for _, fk := range fksBefore {
		if !slices.Contains(fksAfter, fk) {
			t.Errorf("foreign key lost across v12: %s", fk)
		}
	}
	for _, join := range []string{
		`SELECT count(*) FROM issue_labels il JOIN labels l ON l.id = il.label_id`,
		`SELECT count(*) FROM steps s JOIN workflows w ON w.id = s.workflow_id`,
		`SELECT count(*) FROM run_issues ri JOIN workflows w ON w.id = ri.workflow_id`,
	} {
		var n int
		if err := db.QueryRow(join).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s = %d (err %v), want 1", join, n, err)
		}
	}
}

func TestMigrateV8BackfillsGateTrail(t *testing.T) {
	ctx := context.Background()
	db := emptyTestSchema(t)

	if err := applyMigrations(ctx, db, 0, 7); err != nil {
		t.Fatalf("migrating to version 7: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO workflows (id, name, version, source_sha256, body, parsed, created_at_ms)
			VALUES (1, 'w', 1, 's', 'b', '{}', 1)`,
		`INSERT INTO runs (id, created_at_ms, updated_at_ms) VALUES (1, 1, 1)`,
		`INSERT INTO steps (id, run_id, workflow_id, step_name, instance, kind, gate_trail, created_at_ms, updated_at_ms)
			VALUES (1, 1, 1, 'a', 'a@0', 'executor',
			 '[{"gate":"build","argv":["make"],"exit":0,"duration_ms":5,"output":"ok","truncated":false,"verdict":"pass"},
			   {"gate":"tests","exit":2,"output":"x","truncated":true,"verdict":"fail"}]', 1, 50)`,
		`INSERT INTO steps (id, run_id, workflow_id, step_name, instance, kind, gate_trail, created_at_ms, updated_at_ms)
			VALUES (2, 1, 1, 'b', 'b@0', 'executor', '{"gate":"build"}', 1, 60)`,
		`INSERT INTO steps (id, run_id, workflow_id, step_name, instance, kind, created_at_ms, updated_at_ms)
			VALUES (3, 1, 1, 'c', 'c@0', 'executor', 1, 70)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	if err := applyMigrations(ctx, db, 7, 8); err != nil {
		t.Fatalf("applying v8: %v", err)
	}

	rows, err := db.Query(`SELECT step_id, gate, ordinal, coalesce(argv::text, 'NULL'), coalesce(exit, -1),
			duration_ms, output, truncated, verdict, pre, stub, coalesce(reason, ''), created_at_ms
		FROM gate_results ORDER BY step_id, ordinal`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var stepID, ordinal, exit, durationMS, truncated, pre, stub, createdAtMS int64
		var gate, argv, output, verdict, reason string
		if err := rows.Scan(&stepID, &gate, &ordinal, &argv, &exit, &durationMS, &output,
			&truncated, &verdict, &pre, &stub, &reason, &createdAtMS); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d %q %d %s %d %d %q %d %s %d %d %q %d",
			stepID, gate, ordinal, argv, exit, durationMS, output, truncated, verdict, pre, stub, reason, createdAtMS))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// SQLite's G1, G2 and G5: one stub row per trail element at its array
	// index, stamped with the step's updated_at_ms; one unmatched row for a
	// trail that is not an array; nothing for a step without a trail.
	want := []string{
		`1 "build" 0 ["make"] 0 5 "ok" 0 pass 0 1 "" 50`,
		`1 "tests" 1 NULL 2 0 "x" 1 fail 0 1 "" 50`,
		`2 "" 0 NULL -1 0 "" 0 unmatched 0 1 "gate trail is not a JSON array" 60`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("gate_results after v8:\ngot  %q\nwant %q", got, want)
	}
}

// emptyTestSchema returns a pool on DOCKET_TEST_POSTGRES_DSN whose every
// connection uses a fresh schema, dropped when the test ends, so each test
// starts from an empty database. It connects through the driver rather than
// Open: the migrations, not the DSN policy, are under test, and the test DSN
// may carry a password. Without a DSN it skips locally and fails under CI.
func emptyTestSchema(t *testing.T) *sql.DB {
	t.Helper()
	cfg, err := pgx.ParseConfig(testsupport.PostgresDSN(t))
	if err != nil {
		t.Fatalf("parsing DOCKET_TEST_POSTGRES_DSN: %v", err)
	}

	admin := stdlib.OpenDB(*cfg.Copy())
	t.Cleanup(func() { admin.Close() })
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "docket_test_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(`CREATE SCHEMA ` + name); err != nil {
		t.Fatalf("creating schema %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + name + ` CASCADE`); err != nil {
			t.Errorf("dropping schema %s: %v", name, err)
		}
	})

	cfg.RuntimeParams["search_path"] = name
	db := stdlib.OpenDB(*cfg)
	// Registered after the schema drop, so it runs first and the drop waits
	// on no open connection.
	t.Cleanup(func() { db.Close() })
	return db
}

func assertSchemaVersion(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	got, err := schemaVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("reading schema version: %v", err)
	}
	if got != want {
		t.Fatalf("schema version = %d, want %d", got, want)
	}
}

// expectedColumns reads a SQLite schema fixture and maps each column through
// sqliteToPostgresType, with jsonColumns as jsonb.
func expectedColumns(t *testing.T, fixturePath string) []schemaColumn {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture []schemaColumn
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parsing %s: %v", fixturePath, err)
	}

	unmatchedJSON := map[string]bool{}
	for _, c := range jsonColumns {
		unmatchedJSON[c] = true
	}
	want := make([]schemaColumn, 0, len(fixture))
	for _, c := range fixture {
		pgType, ok := sqliteToPostgresType[c.Type]
		if !ok {
			t.Fatalf("%s column %s.%s has SQLite type %q, which the type map does not cover", fixturePath, c.Table, c.Column, c.Type)
		}
		if key := c.Table + "." + c.Column; unmatchedJSON[key] {
			pgType = "jsonb"
			delete(unmatchedJSON, key)
		}
		want = append(want, schemaColumn{Table: c.Table, Column: c.Column, Type: pgType, Nullable: c.Nullable})
	}
	if len(unmatchedJSON) > 0 {
		t.Fatalf("jsonColumns names columns %s lacks: %v", fixturePath, unmatchedJSON)
	}
	sortColumns(want)
	return want
}

func readColumns(t *testing.T, db *sql.DB) []schemaColumn {
	t.Helper()
	rows, err := db.Query(`SELECT c.table_name, c.column_name, c.data_type, c.is_nullable = 'YES'
		FROM information_schema.columns c
		JOIN information_schema.tables tb
		  ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
		WHERE c.table_schema = current_schema() AND tb.table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatalf("reading information_schema.columns: %v", err)
	}
	defer rows.Close()
	var cols []schemaColumn
	for rows.Next() {
		var c schemaColumn
		if err := rows.Scan(&c.Table, &c.Column, &c.Type, &c.Nullable); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sortColumns(cols)
	return cols
}

func readRowCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, c := range readColumns(t, db) {
		if _, seen := counts[c.Table]; seen {
			continue
		}
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + quoteIdent(c.Table)).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", c.Table, err)
		}
		counts[c.Table] = n
	}
	return counts
}

// readRowsWithoutProjectID renders each row as jsonb text with project_id
// removed, so a row before v12 compares equal to the same row after it.
func readRowsWithoutProjectID(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT (to_jsonb(t) - 'project_id')::text FROM ` + quoteIdent(table) + ` t ORDER BY id`)
	if err != nil {
		t.Fatalf("reading %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func readForeignKeys(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT conrelid::regclass::text || ' ' || pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE contype = 'f' AND connamespace = current_schema()::regnamespace
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("reading foreign keys: %v", err)
	}
	defer rows.Close()
	var fks []string
	for rows.Next() {
		var fk string
		if err := rows.Scan(&fk); err != nil {
			t.Fatal(err)
		}
		fks = append(fks, fk)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fks
}

func sortColumns(cols []schemaColumn) {
	slices.SortFunc(cols, func(a, b schemaColumn) int {
		if c := strings.Compare(a.Table, b.Table); c != 0 {
			return c
		}
		return strings.Compare(a.Column, b.Column)
	})
}

func columnDiff(want, got []schemaColumn) string {
	var b strings.Builder
	for _, c := range want {
		if !slices.Contains(got, c) {
			fmt.Fprintf(&b, "  missing %s.%s %s nullable=%t\n", c.Table, c.Column, c.Type, c.Nullable)
		}
	}
	for _, c := range got {
		if !slices.Contains(want, c) {
			fmt.Fprintf(&b, "  extra   %s.%s %s nullable=%t\n", c.Table, c.Column, c.Type, c.Nullable)
		}
	}
	return b.String()
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
