package db

import (
	"testing"
)

// v37 — the detached pre-gate target column (gates-trust §7.6.2 PG6) and the
// proposal's pinned hold-on-dissent flag. The column form's one obligation:
// each column arrives on a migrated store, and the rewind guard converges a
// store stamped 37 without it. There is deliberately NO back-fill to test —
// no existing row was measured by a detached run, so every one reads the
// empty string, which is never matched as a key; and no existing proposal
// pinned a hold, so every one reads false. The behavior the columns exist for
// — a claim serving a result keyed to its own target, a ballot routing under
// the hold it was opened with — is the engine's to prove.

func TestMigrateToV37(t *testing.T) {
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
	for _, col := range v37ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Errorf("%s.%s missing after migration (err %v)", col.table, col.column, err)
		}
	}

	// Empty, never NULL: a scan reads a plain string, and an unkeyed row must
	// compare unequal to every target rather than to nothing.
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM gate_results WHERE target_sha IS NULL`).Scan(&n); err != nil {
		t.Fatalf("counting NULL targets: %v", err)
	}
	if n != 0 {
		t.Errorf("%d row(s) carry a NULL target_sha; the column defaults to ''", n)
	}

	// A proposal row written under v36 reads HoldOnDissent false after the
	// migration: no proposal opened before the column existed pinned a hold.
	mustExec(t, db, `ALTER TABLE proposals DROP COLUMN hold_on_dissent`)
	mustExec(t, db, `ALTER TABLE gate_results DROP COLUMN target_sha`)
	mustExec(t, db, `UPDATE meta SET value = '36' WHERE key = 'schema_version'`)
	res, err := db.Exec(
		`INSERT INTO proposals (description, criticality, status, required_voters, threshold, created_at, updated_at)
		 VALUES ('pre-v37', 'medium', 'open', 2, 0.5, '2026-09-23T10:00:00Z', '2026-09-23T10:00:00Z')`)
	if err != nil {
		t.Fatalf("seeding a v36 proposal row: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v36: %v", err)
	}
	if v, _ := SchemaVersion(db); v != 37 {
		t.Errorf("schema_version after migrating from v36 = %d, want 37", v)
	}
	p, err := GetProposal(db, int(id))
	if err != nil {
		t.Fatalf("GetProposal on the migrated v36 row: %v", err)
	}
	if p.HoldOnDissent {
		t.Error("a v36 proposal row reads HoldOnDissent true after migration, want false")
	}
}

// TestV37RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 37 — the mid-change-binary database v13's guard comment describes
// — and asserts Migrate converges it.
//
// The column form matters here: v37 adds no table and no index, so every v36
// sentinel is present on such a store and a table probe would never fire.
//
// Each v37 column is dropped on its own, so a store stamped 37 by a binary
// that carried only one of them still converges.
func TestV37RewindGuardConvergesAStampedStore(t *testing.T) {
	for _, dropped := range []struct{ table, column string }{
		{"gate_results", "target_sha"},
		{"proposals", "hold_on_dissent"},
	} {
		t.Run(dropped.table+"."+dropped.column, func(t *testing.T) {
			db := mustOpen(t)
			if err := Initialize(db); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if err := Migrate(db); err != nil {
				t.Fatalf("Migrate: %v", err)
			}

			mustExec(t, db, `ALTER TABLE `+dropped.table+` DROP COLUMN `+dropped.column)
			if exists, _ := hasColumnDB(db, dropped.table, dropped.column); exists {
				t.Fatal("the fixture did not remove the column it is testing the recovery of")
			}
			if v, _ := SchemaVersion(db); v != currentSchemaVersion {
				t.Fatalf("stamp = %d after the drop, want it left at %d",
					v, currentSchemaVersion)
			}

			if err := Migrate(db); err != nil {
				t.Fatalf("re-running Migrate on the stamped store: %v", err)
			}
			exists, err := hasColumnDB(db, dropped.table, dropped.column)
			if err != nil || !exists {
				t.Fatalf("the rewind guard did not converge %s.%s back (err %v)",
					dropped.table, dropped.column, err)
			}
		})
	}
}
