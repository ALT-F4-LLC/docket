package db

import (
	"database/sql"
	"testing"
)

// v44 — `steps.loop_cap`, the bound a refused fix-loop entry was measured
// against. Zero means no recorded cap; the migration back-fills nothing.

func TestMigrateToV44ReadsZeroCapOnAPreExistingExhaustedStep(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A v43 store: the column absent, the stamp one back, and a step whose
	// fix loop exhausted, written in the v43 shape.
	mustExec(t, db, `ALTER TABLE steps DROP COLUMN loop_cap`)
	mustExec(t, db, `UPDATE meta SET value = '43' WHERE key = 'schema_version'`)
	_, stepID := seedRunAndStep(t, db)
	mustExec(t, db, `UPDATE steps SET loop_rounds_run = 3,
	        loop_trigger_step = 'judge-correctness@0',
	        loop_latest_verdict = 'concerns' WHERE id = ?`, stepID)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v43: %v", err)
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Errorf("schema_version after migrating from v43 = %d, want %d",
			v, currentSchemaVersion)
	}
	if exists, err := hasColumnDB(db, "steps", "loop_cap"); err != nil || !exists {
		t.Fatalf("steps.loop_cap missing after migrating from v43 (err %v)", err)
	}

	var loopCap sql.NullInt64
	if err := db.QueryRow(`SELECT loop_cap FROM steps WHERE id = ?`,
		stepID).Scan(&loopCap); err != nil {
		t.Fatalf("reading loop_cap: %v", err)
	}
	if !loopCap.Valid || loopCap.Int64 != 0 {
		t.Errorf("a pre-v44 exhausted step reads loop_cap %v, want 0", loopCap)
	}
}

// TestV44RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 44 and asserts Migrate converges it. v44 adds no table and no
// index, so only a column probe can notice.
func TestV44RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE steps DROP COLUMN loop_cap`)
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d",
			v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	if exists, err := hasColumnDB(db, "steps", "loop_cap"); err != nil || !exists {
		t.Fatalf("the rewind guard did not converge steps.loop_cap back (err %v)", err)
	}
}
