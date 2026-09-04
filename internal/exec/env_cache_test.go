package exec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DKT-1166: a gate measuring a tree docket is about to DELETE gets linter
// result caches docket deletes with it.
//
// golangci-lint and staticcheck cache each reported issue keyed by package
// CONTENT, alongside the ABSOLUTE PATH it was found at, and re-open that path
// afterwards to look for the `//nolint` comment that would suppress it. The
// content hash outlives a throwaway tree; the path does not. Harness
// RUN-64/STEP-2939 is the observed cost: `make lint` under a pre-gate reported
// a forbidigo issue at `../docket-pregate-4091742512/...` — a directory an
// earlier reconstruction had already deleted — while the same sha linted in
// place reported none, so a clean tree drew `verdict: fail, exit 2`.

func TestEphemeralCachesAreScopedToTheTree(t *testing.T) {
	root := t.TempDir()
	env, err := BuildEnv(EnvPolicy{Gate: "ac-commands", Repo: "/repo", CacheRoot: root})
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}

	for _, c := range ephemeralCaches {
		got, ok := envValue(env, c.Name)
		if !ok {
			t.Errorf("%s is absent from a gate measuring a throwaway tree; its "+
				"cache would then persist, and every entry it writes names a "+
				"path that is about to stop existing", c.Name)
			continue
		}
		if !strings.HasPrefix(got, root+string(os.PathSeparator)) {
			t.Errorf("%s = %q, want a directory under the tree's own scratch "+
				"root %q", c.Name, got, root)
		}
		// The directory is READY, not merely named. A tool that finds its
		// cache path unusable falls back to the persistent default, which is
		// exactly the carrier this mechanism exists to remove.
		info, statErr := os.Stat(got)
		if statErr != nil || !info.IsDir() {
			t.Errorf("%s points at %q, which is not a directory: %v",
				c.Name, got, statErr)
		}
	}

	// Two tools, two on-disk layouts: sharing one directory invites one to
	// read the other's files.
	seen := map[string]bool{}
	for _, c := range ephemeralCaches {
		dir, _ := envValue(env, c.Name)
		if seen[dir] {
			t.Errorf("%s shares its cache directory %q with another tool",
				c.Name, dir)
		}
		seen[dir] = true
	}
}

// TestNoCacheRootLeavesTheSharedCachesAlone is the containment half.
//
// Re-analysis on every invocation is a real cost, and it is only worth paying
// where the tree is genuinely ephemeral. A gate over a tree that stays on disk
// — the shared checkout, a live worktree — must see exactly the environment it
// saw before, or this fix trades a rare false verdict for a permanent slowdown.
func TestNoCacheRootLeavesTheSharedCachesAlone(t *testing.T) {
	env, err := BuildEnv(EnvPolicy{Gate: "ac-commands", Repo: "/repo"})
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	for _, c := range ephemeralCaches {
		if got, ok := envValue(env, c.Name); ok {
			t.Errorf("%s = %q for a gate over a durable tree; its cache should "+
				"be reused there", c.Name, got)
		}
	}
}

// TestUnusableCacheRootRefusesRatherThanFallingBack: BuildEnv fails when the
// scratch cache cannot be created.
//
// The alternative — omit the variable and spawn anyway — is the silent version
// of the bug: the tool would resolve its default persistent cache, write
// entries naming a directory about to be deleted, and poison every later run
// over the same package content, including the operator's own.
func TestUnusableCacheRootRefusesRatherThanFallingBack(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("file, not a directory\n"), 0o644); err != nil {
		t.Fatalf("seeding the blocked path: %v", err)
	}
	if _, err := BuildEnv(EnvPolicy{Gate: "ac-commands", CacheRoot: blocked}); err == nil {
		t.Error("BuildEnv accepted a cache root it could not create, which " +
			"would send the gate back to its persistent cache with no warning")
	}
}
