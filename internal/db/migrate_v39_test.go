package db

import "testing"

// v39 — `steps.recorded_at_ms`, the usage grace's clock. A store upgraded
// mid-run must keep a non-zero clock on every step that already recorded, or
// the grace would read as long lapsed the moment the binary changed.

func TestMigrateToV39BackfillsRecordedAtFromTerminalRows(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A v38 store: the column absent and the stamp one back.
	mustExec(t, db, `ALTER TABLE steps DROP COLUMN recorded_at_ms`)
	mustExec(t, db, `UPDATE meta SET value = '38' WHERE key = 'schema_version'`)
	_, pendingID := seedRunAndStep(t, db)
	mustExec(t, db, `UPDATE steps SET updated_at_ms = 4000 WHERE id = ?`, pendingID)
	mustExec(t, db,
		`INSERT INTO steps (run_id, issue_id, workflow_id, step_name, ordinal,
		                    instance, kind, status, attempt, created_at_ms,
		                    updated_at_ms, row_version)
		 SELECT run_id, issue_id, workflow_id, 'review', 1, 'review@0', 'executor',
		        ?, 1, 1, 5000, 1
		   FROM steps WHERE id = ?`,
		StepDone, pendingID)
	var doneID int
	if err := db.QueryRow(`SELECT id FROM steps WHERE instance = 'review@0'`).Scan(&doneID); err != nil {
		t.Fatalf("reading the seeded done step: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v38: %v", err)
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Errorf("schema_version after migrating from v38 = %d, want %d",
			v, currentSchemaVersion)
	}

	done, err := GetStep(db, doneID)
	if err != nil {
		t.Fatalf("GetStep(done): %v", err)
	}
	if done.RecordedAtMS != 5000 {
		t.Errorf("a step done before v39 has recorded_at_ms = %d, want its updated_at_ms 5000",
			done.RecordedAtMS)
	}
	pending, err := GetStep(db, pendingID)
	if err != nil {
		t.Fatalf("GetStep(pending): %v", err)
	}
	if pending.RecordedAtMS != 0 {
		t.Errorf("a pending step has recorded_at_ms = %d after the back-fill, want 0",
			pending.RecordedAtMS)
	}
}

// TestV39RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 39 and asserts Migrate converges it. v39 adds no table and no
// index, so only a column probe can notice.
func TestV39RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE steps DROP COLUMN recorded_at_ms`)
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d", v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	if exists, err := hasColumnDB(db, "steps", "recorded_at_ms"); err != nil || !exists {
		t.Fatalf("the rewind guard did not converge steps.recorded_at_ms back (err %v)", err)
	}
}
