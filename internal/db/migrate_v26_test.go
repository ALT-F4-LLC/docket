package db

import (
	"regexp"
	"sort"
	"testing"
)

// v26 — the run-note table (DKT-1079). The same two obligations as v24 and
// v25: the table arrives on a migrated store, and the rewind guard converges a
// store stamped 26 without it. There is deliberately no back-fill to test — no
// dispatcher recorded a note before the verb for recording one existed. The
// behavior the table exists for — a note minted at `run note add`, carried by
// every later context bundle and packet of the run, dead with its run — is
// the engine's to prove.

// v26Tables is stated independently of v26Sentinels so the sentinel test
// cannot pass by both lists drifting together — the v7/v8 discipline.
var v26Tables = []string{
	"run_notes",
}

func TestMigrateToV26(t *testing.T) {
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
	for _, table := range v26Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}
	if !hasIndex(t, db, "idx_run_notes_run") {
		t.Error("idx_run_notes_run missing after migration")
	}
}

// TestRewindGuardProbesEveryV26Sentinel derives the table list from the DDL
// itself: a later edit that adds a CREATE TABLE to v26DDL and forgets the
// sentinel fails HERE rather than shipping a database the guard silently
// declines to repair.
func TestRewindGuardProbesEveryV26Sentinel(t *testing.T) {
	re := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+(\w+)`)
	var created []string
	for _, m := range re.FindAllStringSubmatch(v26DDL, -1) {
		created = append(created, m[1])
	}
	if len(created) == 0 {
		t.Fatal("no CREATE TABLE statements found in v26DDL")
	}
	sort.Strings(created)

	for _, pair := range []struct {
		name string
		list []string
	}{
		{"v26Sentinels", append([]string(nil), v26Sentinels...)},
		{"v26Tables", append([]string(nil), v26Tables...)},
	} {
		got := pair.list
		sort.Strings(got)
		if len(got) != len(created) {
			t.Fatalf("%s = %v, but v26DDL creates %v", pair.name, got, created)
		}
		for i := range created {
			if got[i] != created[i] {
				t.Errorf("%s = %v, but v26DDL creates %v", pair.name, got, created)
				break
			}
		}
	}
}

// TestV26RewindGuardConvergesAStampedStore drops the table while leaving the
// stamp at 26 — the mid-change-binary database the guard comments describe —
// and asserts Migrate converges it. The TABLE form matters: v26 adds no
// column, so every v25 sentinel is present on such a store and a column probe
// would never fire.
func TestV26RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `DROP TABLE run_notes`)
	if hasTable(t, db, "run_notes") {
		t.Fatal("the fixture did not remove the table it is testing the recovery of")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	for _, table := range v26Sentinels {
		if !hasTable(t, db, table) {
			t.Fatalf("the rewind guard did not converge %s back", table)
		}
	}
}

// TestRunNotesDieWithTheirRun pins the scope rule at the schema level: the
// run_id foreign key cascades, so a deleted run takes its notes with it and
// nothing can render a note for a run that no longer exists.
func TestRunNotesDieWithTheirRun(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	run, err := InsertRun(db, 1, "a run", 0, 1_000)
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	id, err := InsertRunNoteTx(tx, RunNote{RunID: run.ID, Text: "standing ruling", CreatedAtMS: 2_000})
	if err != nil {
		t.Fatalf("InsertRunNoteTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if id == 0 {
		t.Fatal("InsertRunNoteTx returned no id")
	}

	notes, err := ListRunNotes(db, run.ID)
	if err != nil {
		t.Fatalf("ListRunNotes: %v", err)
	}
	if len(notes) != 1 || notes[0].ID != id || notes[0].Text != "standing ruling" ||
		notes[0].RunID != run.ID || notes[0].CreatedAtMS != 2_000 {
		t.Fatalf("ListRunNotes = %+v, want the one note back verbatim", notes)
	}

	mustExec(t, db, `DELETE FROM runs WHERE id = ?`, run.ID)
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM run_notes`).Scan(&left); err != nil {
		t.Fatalf("counting notes: %v", err)
	}
	if left != 0 {
		t.Errorf("run_notes rows after deleting the run = %d, want 0 (ON DELETE CASCADE)", left)
	}
}
