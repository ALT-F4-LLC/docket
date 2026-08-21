package exec

import (
	"strings"
	"testing"
)

// DOCKET_ISSUE / DOCKET_SCOPE (DKT-63): a gate learns which issue it is
// gating and what that issue declared it would touch, so a diff-shaped check
// can evaluate the change under review rather than the whole shared tree.

func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

func TestBuildEnvCarriesIssueAndScope(t *testing.T) {
	env, err := BuildEnv(EnvPolicy{
		Gate:  "hygiene",
		Repo:  "/repo",
		Issue: "DKT-7",
		Scope: []string{"internal/cli/**", "skills/docket/SKILL.md"},
	})
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}

	if got, ok := envValue(env, "DOCKET_ISSUE"); !ok || got != "DKT-7" {
		t.Errorf("DOCKET_ISSUE = %q, %v; want the gated issue's ref", got, ok)
	}
	scope, ok := envValue(env, "DOCKET_SCOPE")
	if !ok {
		t.Fatal("DOCKET_SCOPE is absent for a step with a declared scope")
	}
	if scope != "internal/cli/**\nskills/docket/SKILL.md" {
		t.Errorf("DOCKET_SCOPE = %q, want newline-joined globs", scope)
	}
}

// TestBuildEnvOmitsScopeWhenUndeclared: a scope-less issue gives the check no
// narrower answer than the tree, and inventing one would be docket deciding
// what the issue touches. Absence — not emptiness — is the encoding.
func TestBuildEnvOmitsScopeWhenUndeclared(t *testing.T) {
	env, err := BuildEnv(EnvPolicy{Gate: "hygiene", Repo: "/repo"})
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	if _, ok := envValue(env, "DOCKET_SCOPE"); ok {
		t.Error("DOCKET_SCOPE is set for a step with no declared scope")
	}
	if _, ok := envValue(env, "DOCKET_ISSUE"); ok {
		t.Error("DOCKET_ISSUE is set with no issue in the policy")
	}
}
