package verb_test

import (
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

var allProjectPolicies = []verb.ProjectPolicy{
	verb.ProjectReadOnly, verb.ProjectMayRegister, verb.ProjectRunAddressed,
}

func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "issues.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

func projectEnv(st store.Store, identity string, anchored bool) verb.Env {
	return verb.Env{
		Store:   st,
		Cwd:     "/work/scratch",
		Project: verb.ProjectInput{Identity: identity, Anchored: anchored},
	}
}

// requireUnregistered asserts identity has no project row.
func requireUnregistered(t *testing.T, st store.Store, identity string) {
	t.Helper()
	_, found, err := st.LookupProject(identity)
	if err != nil {
		t.Fatalf("LookupProject: %v", err)
	}
	if found {
		t.Errorf("identity %q was registered, want no project row", identity)
	}
}

func TestResolveProjectEmptyIdentityIsTheDefaultProject(t *testing.T) {
	st := openStore(t)
	for _, policy := range allProjectPolicies {
		id, created, err := projectEnv(st, "", false).ResolveProject(policy)
		if err != nil {
			t.Fatalf("policy %v: error = %v", policy, err)
		}
		if id != store.DefaultProjectID || created {
			t.Errorf("policy %v: got (%d, %v), want (%d, false)", policy, id, created, store.DefaultProjectID)
		}
	}
}

func TestResolveProjectFindsARegisteredProjectUnderEveryPolicy(t *testing.T) {
	st := openStore(t)
	want, err := st.EnsureProject("/repos/known", "known", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, policy := range allProjectPolicies {
		// Unanchored: an existing row resolves even where registration would not.
		id, created, err := projectEnv(st, "/repos/known", false).ResolveProject(policy)
		if err != nil {
			t.Fatalf("policy %v: error = %v", policy, err)
		}
		if id != want || created {
			t.Errorf("policy %v: got (%d, %v), want (%d, false)", policy, id, created, want)
		}
	}
}

func TestResolveProjectReadOnlyNeverRegisters(t *testing.T) {
	st := openStore(t)
	for _, anchored := range []bool{true, false} {
		id, created, err := projectEnv(st, "/repos/new", anchored).ResolveProject(verb.ProjectReadOnly)
		if err != nil {
			t.Fatalf("anchored=%v: error = %v", anchored, err)
		}
		if id != store.UnregisteredProjectID || created {
			t.Errorf("anchored=%v: got (%d, %v), want (%d, false)", anchored, id, created, store.UnregisteredProjectID)
		}
	}
	requireUnregistered(t, st, "/repos/new")
}

func TestResolveProjectMayRegisterRegistersAnAnchoredIdentityOnce(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "/repos/new", true)

	id, created, err := env.ResolveProject(verb.ProjectMayRegister)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if !created {
		t.Errorf("first resolve: created = false, want true")
	}
	stored, found, err := st.LookupProject("/repos/new")
	if err != nil || !found || stored != id {
		t.Fatalf("LookupProject = (%d, %v, %v), want (%d, true, nil)", stored, found, err, id)
	}
	project, err := st.GetProject(id)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if project.Name != "new" {
		t.Errorf("project name = %q, want %q", project.Name, "new")
	}

	again, created, err := env.ResolveProject(verb.ProjectMayRegister)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if again != id || created {
		t.Errorf("second resolve: got (%d, %v), want (%d, false)", again, created, id)
	}
}

func TestResolveProjectMayRegisterRefusesAnUnanchoredIdentity(t *testing.T) {
	st := openStore(t)
	_, _, err := projectEnv(st, "/work/scratch", false).ResolveProject(verb.ProjectMayRegister)
	requireVerbError(t, err, output.ErrValidation,
		"cannot tell which project this belongs to: /work/scratch is not a git worktree")
	requireUnregistered(t, st, "/work/scratch")
}

func TestResolveProjectRunAddressedCarriesOnUnanchored(t *testing.T) {
	st := openStore(t)
	id, created, err := projectEnv(st, "/work/scratch", false).ResolveProject(verb.ProjectRunAddressed)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if id != store.UnregisteredProjectID || created {
		t.Errorf("got (%d, %v), want (%d, false)", id, created, store.UnregisteredProjectID)
	}
	requireUnregistered(t, st, "/work/scratch")
}

func TestResolveProjectRunAddressedRegistersAnAnchoredIdentity(t *testing.T) {
	st := openStore(t)
	id, created, err := projectEnv(st, "/repos/new", true).ResolveProject(verb.ProjectRunAddressed)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	stored, found, lookupErr := st.LookupProject("/repos/new")
	if lookupErr != nil || !found || stored != id || !created {
		t.Errorf("got (%d, %v), lookup (%d, %v, %v); want a new row", id, created, stored, found, lookupErr)
	}
}

func TestResolveProjectRejectsAnUnknownPolicy(t *testing.T) {
	st := openStore(t)
	if _, _, err := projectEnv(st, "/repos/new", true).ResolveProject(verb.ProjectPolicy(0)); err == nil {
		t.Fatal("error = nil, want an unknown-policy error")
	}
	requireUnregistered(t, st, "/repos/new")
}
