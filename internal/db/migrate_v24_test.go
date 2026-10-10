package db

import (
	"regexp"
	"sort"
	"testing"
)

// v24 — the batch gate-override grant table (DKT-546). Two obligations: the
// table arrives on a migrated store, and the rewind guard converges a store
// stamped 24 without it. There is deliberately no back-fill to test — no
// operator granted a batch override before the verb for granting one existed.
// The behavior the table exists for — a grant minted at resolve, spent at
// routing, dead with its run — is the engine's to prove.

// v24Tables is stated independently of v24Sentinels so the sentinel test
// cannot pass by both lists drifting together — the v7/v8 discipline.
var v24Tables = []string{
	"gate_override_grants",
}

func TestMigrateToV24(t *testing.T) {
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
	for _, table := range v24Tables {
		if !hasTable(t, db, table) {
			t.Errorf("%s missing after migration", table)
		}
	}
	if !hasIndex(t, db, "idx_gate_override_grants_run") {
		t.Error("idx_gate_override_grants_run missing after migration")
	}
}

// TestRewindGuardProbesEveryV24Sentinel derives the table list from the DDL
// itself: a later edit that adds a CREATE TABLE to v24DDL and forgets the
// sentinel fails HERE rather than shipping a database the guard silently
// declines to repair.
func TestRewindGuardProbesEveryV24Sentinel(t *testing.T) {
	re := regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+(\w+)`)
	var created []string
	for _, m := range re.FindAllStringSubmatch(v24DDL, -1) {
		created = append(created, m[1])
	}
	if len(created) == 0 {
		t.Fatal("no CREATE TABLE statements found in v24DDL")
	}
	sort.Strings(created)

	for _, pair := range []struct {
		name string
		list []string
	}{
		{"v24Sentinels", append([]string(nil), v24Sentinels...)},
		{"v24Tables", append([]string(nil), v24Tables...)},
	} {
		got := pair.list
		sort.Strings(got)
		if len(got) != len(created) {
			t.Fatalf("%s = %v, but v24DDL creates %v", pair.name, got, created)
		}
		for i := range created {
			if got[i] != created[i] {
				t.Errorf("%s = %v, but v24DDL creates %v", pair.name, got, created)
				break
			}
		}
	}
}

// TestV24RewindGuardConvergesAStampedStore drops the table while leaving the
// stamp at 24 — the mid-change-binary database the guard comments describe —
// and asserts Migrate converges it. The TABLE form matters: v24 adds no
// column, so every v23 column sentinel is present on such a store and a
// column probe would never fire.
func TestV24RewindGuardConvergesAStampedStore(t *testing.T) {
	db := mustOpen(t)
	if err := Initialize(db); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	mustExec(t, db, `DROP TABLE gate_override_grants`)
	if hasTable(t, db, "gate_override_grants") {
		t.Fatal("the fixture did not remove the table it is testing the recovery of")
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("re-running Migrate on the stamped store: %v", err)
	}
	for _, table := range v24Sentinels {
		if !hasTable(t, db, table) {
			t.Fatalf("the rewind guard did not converge %s back", table)
		}
	}
}
