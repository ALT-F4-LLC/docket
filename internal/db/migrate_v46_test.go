package db

import "testing"

// v46 — `event_low_watermark`, the highest pruned event seq.

// TestV46RewindGuardConvergesAStampedStore drops the table while leaving the
// stamp at 46 and asserts Migrate recreates and seeds it.
func TestV46RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `DROP TABLE event_low_watermark`)
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d",
			v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	var watermark int64
	if err := db.QueryRow(
		`SELECT seq FROM event_low_watermark WHERE id = 1`).Scan(&watermark); err != nil {
		t.Fatalf("the rewind guard did not converge event_low_watermark back: %v", err)
	}
	if watermark != 0 {
		t.Errorf("watermark on a store with no events = %d, want 0", watermark)
	}
}
