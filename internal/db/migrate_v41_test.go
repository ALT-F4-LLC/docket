package db

import "testing"

// v41 — `steps.claim_phase`, set while a pre-gated claim's context
// transaction has not committed. A store upgraded mid-run gains it empty.

func TestMigrateToV41AddsAnEmptyClaimPhase(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A v40 store: the column absent and the stamp one back.
	mustExec(t, db, `ALTER TABLE steps DROP COLUMN claim_phase`)
	mustExec(t, db, `UPDATE meta SET value = '40' WHERE key = 'schema_version'`)
	_, stepID := seedRunAndStep(t, db)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v40: %v", err)
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Errorf("schema_version after migrating from v40 = %d, want %d",
			v, currentSchemaVersion)
	}

	step, err := GetStep(db, stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if step.ClaimPhase != "" {
		t.Errorf("a pre-v41 step reads claim phase %q, want none", step.ClaimPhase)
	}
}

// TestV41RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 41 and asserts Migrate converges it. v41 adds no table and no
// index, so only a column probe can notice.
func TestV41RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE steps DROP COLUMN claim_phase`)
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d",
			v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	if exists, err := hasColumnDB(db, "steps", "claim_phase"); err != nil || !exists {
		t.Fatalf("the rewind guard did not converge steps.claim_phase back (err %v)", err)
	}
}
