package db

import (
	"regexp"
	"sort"
	"testing"
)

// v25 — the stale-target waiver table (DKT-742). The same two obligations as
// v24: the table arrives on a migrated store, and the rewind guard converges a
// store stamped 25 without it. There is deliberately no back-fill to test — no
// operator waived a stale-target warning before the verb for waiving one
// existed. The behavior the table exists for — a waiver minted at
// `dispatch waive-target`, consulted read-only by the advisory, dead with its
// run — is the engine's to prove.

// v25Tables is stated independently of v25Sentinels so the sentinel test
// cannot pass by both lists drifting together — the v7/v8 discipline.
var v25Tables = []string{
	"stale_target_waivers",
}

func TestMigrateToV25(t *testing.T) {
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
	for _, table := range v25Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}
	if !hasIndex(t, db, "idx_stale_target_waivers_run") {
		t.Error("idx_stale_target_waivers_run missing after migration")
	}
}

// TestRewindGuardProbesEveryV25Sentinel derives the table list from the DDL
// itself: a later edit that adds a CREATE TABLE to v25DDL and forgets the
// sentinel fails HERE rather than shipping a database the guard silently
// declines to repair.
func TestRewindGuardProbesEveryV25Sentinel(t *testing.T) {
	re := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+(\w+)`)
	var created []string
	for _, m := range re.FindAllStringSubmatch(v25DDL, -1) {
		created = append(created, m[1])
	}
	if len(created) == 0 {
		t.Fatal("no CREATE TABLE statements found in v25DDL")
	}
	sort.Strings(created)

	for _, pair := range []struct {
		name string
		list []string
	}{
		{"v25Sentinels", append([]string(nil), v25Sentinels...)},
		{"v25Tables", append([]string(nil), v25Tables...)},
	} {
		got := pair.list
		sort.Strings(got)
		if len(got) != len(created) {
			t.Fatalf("%s = %v, but v25DDL creates %v", pair.name, got, created)
		}
		for i := range created {
			if got[i] != created[i] {
				t.Errorf("%s = %v, but v25DDL creates %v", pair.name, got, created)
				break
			}
		}
	}
}

// TestV25RewindGuardConvergesAStampedStore drops the table while leaving the
// stamp at 25 — the mid-change-binary database the guard comments describe —
// and asserts Migrate converges it. The TABLE form matters: v25 adds no
// column, so every v24 sentinel is present on such a store and a column probe
// would never fire.
func TestV25RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `DROP TABLE stale_target_waivers`)
	if hasTable(t, db, "stale_target_waivers") {
		t.Fatal("the fixture did not remove the table it is testing the recovery of")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	for _, table := range v25Sentinels {
		if !hasTable(t, db, table) {
			t.Fatalf("the rewind guard did not converge %s back", table)
		}
	}
}
