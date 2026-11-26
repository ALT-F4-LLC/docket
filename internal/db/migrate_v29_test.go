package db

import (
	"testing"
)

// v29 — the conductor capability column (DKT-2465). The column form's one
// obligation: the column arrives on a migrated store, and the rewind guard
// converges a store stamped 29 without it. There is deliberately NO back-fill
// to test — a capability is returned once to whoever minted it, and a
// migration has nobody to return one to, so every pre-v29 run reads NULL and
// the verbs behave on it as they always did. The behavior the column exists
// for — a first activation minting, `run conduct` rotating, and the seven
// operator verbs refusing a caller without the capability — is the engine's
// to prove.

// TestV29RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 29 — the mid-change-binary database v13's guard comment describes
// — and asserts Migrate converges it.
//
// The column form matters here: v29 adds no table and no index, so every v28
// sentinel is present on such a store and a table probe would never fire.
func TestV29RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE runs DROP COLUMN conductor_token_hash`)
	if exists, _ := hasColumnDB(db, "runs", "conductor_token_hash"); exists {
		t.Fatal("the fixture did not remove the column it is testing the recovery of")
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d",
			v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	for _, col := range v29ColumnSentinels {
		exists, err := hasColumnDB(db, col.table, col.column)
		if err != nil || !exists {
			t.Fatalf("the rewind guard did not converge %s.%s back (err %v)",
				col.table, col.column, err)
		}
	}
}
