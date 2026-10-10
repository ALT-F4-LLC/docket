package db

import "testing"

// v40 — `steps.authority` and `steps.authority_ref`, the authority a ruling on
// the step was made under. A store upgraded mid-run gains both columns empty;
// rulings made before the upgrade keep their authority on their events.

func TestMigrateToV40AddsEmptyAuthorityColumns(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A v39 store: both columns absent and the stamp one back.
	mustExec(t, db, `ALTER TABLE steps DROP COLUMN authority_ref`)
	mustExec(t, db, `ALTER TABLE steps DROP COLUMN authority`)
	mustExec(t, db, `UPDATE meta SET value = '39' WHERE key = 'schema_version'`)
	_, stepID := seedRunAndStep(t, db)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v39: %v", err)
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Errorf("schema_version after migrating from v39 = %d, want %d",
			v, currentSchemaVersion)
	}

	step, err := GetStep(db, stepID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if step.Authority != "" || step.AuthorityRef != "" {
		t.Errorf("a pre-v40 step reads authority %q/%q, want none",
			step.Authority, step.AuthorityRef)
	}
}

// TestV40RewindGuardConvergesAStampedStore drops a column while leaving the
// stamp at 40 and asserts Migrate converges it. v40 adds no table and no
// index, so only a column probe can notice.
func TestV40RewindGuardConvergesAStampedStore(t *testing.T) {
	for _, column := range []string{"authority", "authority_ref"} {
		t.Run(column, func(t *testing.T) {
			db := mustOpen(t)
			if err := Initialize(db); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if err := Migrate(db); err != nil {
				t.Fatalf("Migrate: %v", err)
			}

			mustExec(t, db, `ALTER TABLE steps DROP COLUMN `+column)
			if v, _ := SchemaVersion(db); v != currentSchemaVersion {
				t.Fatalf("stamp = %d after the drop, want it left at %d",
					v, currentSchemaVersion)
			}

			if err := Migrate(db); err != nil {
				t.Fatalf("re-running Migrate on the stamped store: %v", err)
			}
			if exists, err := hasColumnDB(db, "steps", column); err != nil || !exists {
				t.Fatalf("the rewind guard did not converge steps.%s back (err %v)",
					column, err)
			}
		})
	}
}
