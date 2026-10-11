package db

import (
	"database/sql"
	"testing"
)

// v42 — `run_notes.authority`, the authority a note's writer asserted. A note
// recorded before the column existed reads "" (unknown), never a default
// authority.

func TestMigrateToV42LeavesLegacyNoteAuthorityUnknown(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// A v41 store: the column absent, the stamp one back, and a note written
	// in the v41 shape.
	mustExec(t, db, `ALTER TABLE run_notes DROP COLUMN authority`)
	mustExec(t, db, `UPDATE meta SET value = '41' WHERE key = 'schema_version'`)
	runID, _ := seedRunAndStep(t, db)
	mustExec(t, db,
		`INSERT INTO run_notes (run_id, text, created_at_ms) VALUES (?, 'legacy ruling', 1)`,
		runID)

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate from v41: %v", err)
	}
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Errorf("schema_version after migrating from v41 = %d, want %d",
			v, currentSchemaVersion)
	}

	notes := listRunNotesInTx(t, db, runID)
	if len(notes) != 1 || notes[0].Text != "legacy ruling" {
		t.Fatalf("ListRunNotesTx = %+v, want the one legacy note", notes)
	}
	if notes[0].Authority != "" {
		t.Errorf("a pre-v42 note reads authority %q, want unknown (\"\")", notes[0].Authority)
	}

	// quote() tells an empty string from NULL, which the Go scan above cannot
	// show once it succeeds.
	var quoted string
	if err := db.QueryRow(`SELECT quote(authority) FROM run_notes WHERE run_id = ?`,
		runID).Scan(&quoted); err != nil {
		t.Fatalf("reading the stored authority: %v", err)
	}
	if quoted != "''" {
		t.Errorf("stored authority = %s, want ''", quoted)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on a v42 store: %v", err)
	}
}

func TestRunNoteAuthorityReadsBackOnBothListPaths(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	runID, _ := seedRunAndStep(t, db)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i, authority := range []string{"operator", "conductor"} {
		if _, err := InsertRunNoteTx(tx, RunNote{
			RunID: runID, Text: authority + " ruling", Authority: authority,
			CreatedAtMS: int64(i + 1),
		}); err != nil {
			t.Fatalf("InsertRunNoteTx(%s): %v", authority, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	pooled, err := ListRunNotes(db, runID)
	if err != nil {
		t.Fatalf("ListRunNotes: %v", err)
	}
	for name, notes := range map[string][]RunNote{
		"ListRunNotesTx": listRunNotesInTx(t, db, runID),
		"ListRunNotes":   pooled,
	} {
		if len(notes) != 2 {
			t.Fatalf("%s returned %d notes, want 2: %+v", name, len(notes), notes)
		}
		if notes[0].Authority != "operator" || notes[1].Authority != "conductor" {
			t.Errorf("%s authorities = [%q %q], want [operator conductor]",
				name, notes[0].Authority, notes[1].Authority)
		}
	}
}

// TestV42RewindGuardConvergesAStampedStore drops the column while leaving the
// stamp at 42 and asserts Migrate converges it. v42 adds no table and no
// index, so only a column probe can notice.
func TestV42RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `ALTER TABLE run_notes DROP COLUMN authority`)
	if v, _ := SchemaVersion(db); v != currentSchemaVersion {
		t.Fatalf("stamp = %d after the drop, want it left at %d",
			v, currentSchemaVersion)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	if exists, err := hasColumnDB(db, "run_notes", "authority"); err != nil || !exists {
		t.Fatalf("the rewind guard did not converge run_notes.authority back (err %v)", err)
	}
}

func listRunNotesInTx(t *testing.T, db *sql.DB, runID int) []RunNote {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	notes, err := ListRunNotesTx(tx, runID)
	if err != nil {
		t.Fatalf("ListRunNotesTx: %v", err)
	}
	return notes
}
