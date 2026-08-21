package db

import (
	"testing"
)

// v16 — the retry-budget base column (DKT-86, DKT-90). Two obligations: the
// column arrives on a migrated store, and the rewind guard converges a store
// stamped 16 without it. The behavior the column exists for — a retried step's
// next claim minting a fresh ledger attempt — is the engine's to prove.

func TestMigrateToV16(t *testing.T) {
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
	for _, col := range v16ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}
}

// TestV16RewindGuardConvergesAStampedStore rebuilds `steps` without the column
// while leaving the stamp at 16 — the mid-change-binary database v13's guard
// comment describes — and asserts Migrate converges it.
//
// The column form matters here: v16 adds no table and no index, so every v15
// sentinel is present on such a store and a table probe would never fire.
func TestV16RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// SQLite has no DROP COLUMN in every version this targets, so the column is
	// removed by rebuilding the table — which is also what a binary that never
	// ran the v16 ALTER would have left behind.
	mustExec(t, db, `PRAGMA foreign_keys=OFF`)
	mustExec(t, db, `ALTER TABLE steps RENAME TO steps_pre_v16`)
	mustExec(t, db, `CREATE TABLE steps (
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
		materialized   INTEGER NOT NULL DEFAULT 0,
		usage_recorded INTEGER NOT NULL DEFAULT 0,
		work_root      TEXT,
		created_at_ms  INTEGER NOT NULL,
		updated_at_ms  INTEGER NOT NULL,
		row_version    INTEGER NOT NULL DEFAULT 1,
		UNIQUE(run_id, issue_id, instance)
	)`)
	mustExec(t, db, `DROP TABLE steps_pre_v16`)
	mustExec(t, db, `PRAGMA foreign_keys=ON`)

	if exists, _ := hasColumnDB(db, "steps", "attempt_base"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	exists, err := hasColumnDB(db, "steps", "attempt_base")
	if err != nil || !exists {
		t.Fatalf("the rewind guard did not converge steps.attempt_base back (err %v)", err)
	}
}
