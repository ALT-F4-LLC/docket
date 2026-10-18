package db

import (
	"testing"
)

// v45 — `pins.content`, the pinned file's bytes. A pin recorded before the
// column existed reads NULL; the migration back-fills nothing.

func TestMigrateToV45KeepsPreExistingPinsWithNullContent(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A v44 store: the column absent, the stamp one back, and a pin written
	// in the v44 shape.
	mustExec(t, db, `ALTER TABLE pins DROP COLUMN content`)
	mustExec(t, db, `UPDATE meta SET value = '44' WHERE key = 'schema_version'`)
	runID, _ := seedRunAndStep(t, db)
	mustExec(t, db, `INSERT INTO pins (run_id, kind, ref, sha256)
	                 VALUES (?, 'file', 'contracts/implement.md', 'b52312cc')`, runID)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v44: %v", err)
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Errorf("schema_version after migrating from v44 = %d, want %d",
			v, currentSchemaVersion)
	}

	pins, err := ListPins(db, runID)
	if err != nil {
		t.Fatalf("ListPins: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("ListPins returned %d pins after migrating, want the 1 pre-v45 pin", len(pins))
	}
	if p := pins[0]; p.Ref != "contracts/implement.md" || p.SHA256 != "b52312cc" {
		t.Errorf("pre-v45 pin read back as %s@%s, want contracts/implement.md@b52312cc",
			p.Ref, p.SHA256)
	}
	if pins[0].Content != nil {
		t.Errorf("pre-v45 pin content = %q, want nil", pins[0].Content)
	}
	var nullContent bool
	if err := db.QueryRow(`SELECT content IS NULL FROM pins WHERE run_id = ?`,
		runID).Scan(&nullContent); err != nil {
		t.Fatalf("reading content: %v", err)
	}
	if !nullContent {
		t.Error("pre-v45 pin content is not NULL in the store")
	}
}

// TestV45RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 45 and asserts Migrate converges it. v45 adds no table and no
// index, so only a column probe can notice.
func TestV45RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE pins DROP COLUMN content`)
	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	if exists, err := hasColumnDB(db, "pins", "content"); err != nil || !exists {
		t.Fatalf("the rewind guard did not converge pins.content back (err %v)", err)
	}
}
