package db

import (
	"testing"
)

// v15 — the artifact revision column (DKT-70). Three obligations: the column
// arrives on a migrated store, the rewind guard converges a store stamped 15
// without it, and the round-trip preserves both the pointer and its absence.

func TestMigrateToV15(t *testing.T) {
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
	for _, col := range v15ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}
}

// TestV15RewindGuardConvergesAStampedStore rebuilds `artifacts` without the
// column while leaving the stamp at 15 — the mid-change-binary database v13's
// guard comment describes — and asserts Migrate converges it.
//
// The column form matters here: v15 adds no table and no index, so every v14
// sentinel is present on such a store and a table probe would never fire.
func TestV15RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// SQLite has no DROP COLUMN in every version this targets, so the column is
	// removed by rebuilding the table — which is also what a binary that never
	// ran the v15 ALTER would have left behind.
	mustExec(t, db, `PRAGMA foreign_keys=OFF`)
	mustExec(t, db, `ALTER TABLE artifacts RENAME TO artifacts_pre_v15`)
	mustExec(t, db, `CREATE TABLE artifacts (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		step_id       INTEGER REFERENCES steps(id) ON DELETE CASCADE,
		kind          TEXT    NOT NULL,
		body          TEXT    NOT NULL,
		payload       TEXT,
		sha256        TEXT    NOT NULL,
		stub          INTEGER NOT NULL DEFAULT 0,
		created_at_ms INTEGER NOT NULL
	)`)
	mustExec(t, db, `DROP TABLE artifacts_pre_v15`)
	mustExec(t, db, `PRAGMA foreign_keys=ON`)

	if exists, _ := hasColumnDB(db, "artifacts", "supersedes"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	exists, err := hasColumnDB(db, "artifacts", "supersedes")
	if err != nil || !exists {
		t.Fatalf("the rewind guard did not converge artifacts.supersedes back (err %v)", err)
	}
}

// TestArtifactSupersedesRoundTrips pins BOTH values, because the absence is the
// one that carries meaning: NULL means "this artifact revises nothing", and a
// reader that decoded it as 0 would claim every original supersedes ARTIFACT-0.
func TestArtifactSupersedesRoundTrips(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db,
		`INSERT INTO runs (id, request, status, created_at_ms, updated_at_ms)
		 VALUES (1, 'r', 'active', 0, 0)`)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	originalID, err := InsertArtifactTx(tx, Artifact{
		RunID: 1, Kind: "findings", Body: "b", SHA256: "s",
	}, 1)
	if err != nil {
		t.Fatalf("inserting the original: %v", err)
	}
	if _, err := InsertArtifactTx(tx, Artifact{
		RunID: 1, Kind: "findings", Body: "b", SHA256: "s",
		Supersedes: &originalID,
	}, 2); err != nil {
		t.Fatalf("inserting the revision: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	artifacts, err := ListRunArtifacts(db, 1)
	if err != nil {
		t.Fatalf("ListRunArtifacts: %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("read %d artifacts, want 2", len(artifacts))
	}
	if artifacts[0].Supersedes != nil {
		t.Errorf("the original supersedes %d, want nil", *artifacts[0].Supersedes)
	}
	if artifacts[1].Supersedes == nil {
		t.Fatal("the revision's supersedes did not survive the round trip")
	}
	if *artifacts[1].Supersedes != originalID {
		t.Errorf("the revision supersedes %d, want %d", *artifacts[1].Supersedes, originalID)
	}
}
