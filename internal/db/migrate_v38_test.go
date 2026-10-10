package db

import (
	"slices"
	"testing"
)

// v38 — the failing command's argv on `gate_override_grants`. The column's
// obligation is three grant states that read back apart: a grant that existed
// before the column (legacy: its command is unknown), a grant minted from a
// row with no recorded argv (an unmatched gate), and a grant minted from a
// row with one. Comparing argv at routing is the engine's to prove.

func TestMigrateToV38SeparatesTheThreeGrantArgvStates(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A v37 store: the column absent and the stamp one back.
	mustExec(t, db, `ALTER TABLE gate_override_grants DROP COLUMN argv`)
	mustExec(t, db, `UPDATE meta SET value = '37' WHERE key = 'schema_version'`)
	runID, stepID := seedRunAndStep(t, db)
	mustExec(t, db,
		`INSERT INTO gate_override_grants
		   (run_id, origin_step_id, gate, exit, reason, fingerprint, note,
		    covered_steps, created_at_ms)
		 VALUES (?, ?, 'tests', 1, '', 'fp-pre', 'pre-v38', 0, 1)`,
		runID, stepID)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v37: %v", err)
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Errorf("schema_version after migrating from v37 = %d, want %d",
			v, currentSchemaVersion)
	}
	if exists, err := hasColumnDB(db, "gate_override_grants", "argv"); err != nil || !exists {
		t.Fatalf("gate_override_grants.argv missing after migration (err %v)", err)
	}

	recorded := []string{"go", "test", "-run", "a b"}
	exitOne := 1
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, g := range []GateOverrideGrant{
		{RunID: runID, OriginStepID: stepID, Gate: "ac-commands", Note: "nil argv"},
		{RunID: runID, OriginStepID: stepID, Gate: "tests", Exit: &exitOne,
			Fingerprint: "fp-post", Argv: recorded, Note: "recorded argv"},
	} {
		if _, err := InsertGateOverrideGrantTx(tx, g); err != nil {
			tx.Rollback()
			t.Fatalf("InsertGateOverrideGrantTx(%s): %v", g.Note, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	grants, err := GateOverrideGrantsForRun(db, runID)
	if err != nil {
		t.Fatalf("GateOverrideGrantsForRun: %v", err)
	}
	if len(grants) != 3 {
		t.Fatalf("read %d grants, want 3: %+v", len(grants), grants)
	}
	pre, nilMint, recordedMint := grants[0], grants[1], grants[2]

	if !pre.ArgvLegacy || pre.Argv != nil {
		t.Errorf("pre-migration grant: ArgvLegacy=%v Argv=%q, want legacy with no argv",
			pre.ArgvLegacy, pre.Argv)
	}
	if pre.Fingerprint != "fp-pre" || pre.Note != "pre-v38" || pre.Exit == nil || *pre.Exit != 1 {
		t.Errorf("pre-migration grant's other fields changed: %+v", pre)
	}

	if nilMint.ArgvLegacy || nilMint.Argv != nil {
		t.Errorf("grant minted from a NULL argv: ArgvLegacy=%v Argv=%q, want not legacy with nil argv",
			nilMint.ArgvLegacy, nilMint.Argv)
	}

	if recordedMint.ArgvLegacy || !slices.Equal(recordedMint.Argv, recorded) {
		t.Errorf("grant minted from a recorded argv: ArgvLegacy=%v Argv=%q, want not legacy with %q",
			recordedMint.ArgvLegacy, recordedMint.Argv, recorded)
	}
}

// TestV38RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 38 — the mid-change-binary database v13's guard comment describes
// — and asserts Migrate converges it. v38 adds no table and no index, so only
// a column probe can notice.
func TestV38RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE gate_override_grants DROP COLUMN argv`)
	if exists, _ := hasColumnDB(db, "gate_override_grants", "argv"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d", v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	if exists, err := hasColumnDB(db, "gate_override_grants", "argv"); err != nil || !exists {
		t.Fatalf("the rewind guard did not converge gate_override_grants.argv back (err %v)", err)
	}
}
