package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The resolution tests below chdir into temp directories, so each one states
// its whole world explicitly: DOCKET_PATH, HOME, and the directory tree. That
// is what makes them hermetic against both the developer's real ~/.docket and
// this repository's own shipped .docket/config.

// resolveAt runs Resolve as if invoked from dir, with DOCKET_PATH and HOME
// pinned. env == "" means unset.
func resolveAt(t *testing.T, dir, env, home string) *Config {
	t.Helper()
	t.Chdir(dir)
	t.Setenv("DOCKET_PATH", env)
	t.Setenv("HOME", home)
	cfg, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return cfg
}

// canon mirrors the canonicalization macOS forces on temp paths: /var is a
// symlink to /private/var, and any comparison against a resolved path must
// resolve its own side first.
func canon(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// mkStore creates a `.docket/issues.db` under root, making root a legacy
// local-store repo.
func mkStore(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, ".docket")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "issues.db"), []byte("x"), 0o600); err != nil {
		t.Fatalf("creating issues.db: %v", err)
	}
}

// TestResolveEnvKeepsLegacyShape: DOCKET_PATH wins over everything, and the
// whole fact set collapses to the legacy rule — exec root and identity are the
// parent of the store — so an existing setup resolves to byte-identical
// behavior. The value is also normalized to absolute (a relative DOCKET_PATH
// used to flow through resolution unanchored, absolutized later against
// whatever cwd happened to be current).
func TestResolveEnvKeepsLegacyShape(t *testing.T) {
	world := t.TempDir()
	store := filepath.Join(world, "custom-store")
	cfg := resolveAt(t, world, store, t.TempDir())

	if cfg.Source != SourceEnv || !cfg.EnvVarSet {
		t.Fatalf("source = %q, EnvVarSet = %v; DOCKET_PATH must win", cfg.Source, cfg.EnvVarSet)
	}
	if cfg.DocketDir != store {
		t.Errorf("DocketDir = %q, want %q", cfg.DocketDir, store)
	}
	if cfg.ExecRoot != world || cfg.Identity != world {
		t.Errorf("ExecRoot/Identity = %q/%q, want both %q (the parent of the store)",
			cfg.ExecRoot, cfg.Identity, world)
	}
	if got := cfg.InstanceConfigDir(); got != filepath.Join(store, "config") {
		t.Errorf("InstanceConfigDir = %q; an env store keeps config inside the store", got)
	}
	if got := cfg.TreeLockPath(); got != filepath.Join(store, "tree.lock") {
		t.Errorf("TreeLockPath = %q; an env store keeps the legacy lockfile", got)
	}
}

// TestResolveEnvNormalizesRelativePath: a relative DOCKET_PATH is anchored to
// the cwd at resolution, once, rather than escaping as a relative path.
func TestResolveEnvNormalizesRelativePath(t *testing.T) {
	world := t.TempDir()
	cfg := resolveAt(t, world, "rel-store", t.TempDir())
	if !filepath.IsAbs(cfg.DocketDir) {
		t.Errorf("DocketDir = %q is relative; a relative DOCKET_PATH must anchor at resolution", cfg.DocketDir)
	}
	want := filepath.Join(canon(t, world), "rel-store")
	if canon(t, filepath.Dir(cfg.DocketDir)) != canon(t, world) {
		t.Errorf("DocketDir = %q, want it anchored under %q", cfg.DocketDir, want)
	}
}

// TestResolveFindsLocalStoreFromSubdir is the G3 half the old resolver lacked:
// a repo-local store is discovered from anywhere inside the repository, not
// only from its root.
func TestResolveFindsLocalStoreFromSubdir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitInit(t, repo)
	mkStore(t, repo)
	sub := filepath.Join(repo, "internal", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := resolveAt(t, sub, "", t.TempDir())
	if cfg.Source != SourceLocal {
		t.Fatalf("source = %q, want local; the walk finds the repo's store from a subdirectory", cfg.Source)
	}
	if canon(t, cfg.DocketDir) != canon(t, filepath.Join(repo, ".docket")) {
		t.Errorf("DocketDir = %q, want the repo root's store", cfg.DocketDir)
	}
	if canon(t, cfg.ExecRoot) != canon(t, repo) || canon(t, cfg.Identity) != canon(t, repo) {
		t.Errorf("ExecRoot/Identity = %q/%q, want both the store's parent %q",
			cfg.ExecRoot, cfg.Identity, repo)
	}
}

// TestResolveLocalWalkStopsAtToplevel: a `.docket` ABOVE the repository can
// never capture it — the walk is bounded by the worktree toplevel.
func TestResolveLocalWalkStopsAtToplevel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	world := t.TempDir()
	mkStore(t, world) // a stray store ABOVE the repo
	repo := filepath.Join(world, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInit(t, repo)

	home := t.TempDir()
	cfg := resolveAt(t, repo, "", home)
	if cfg.Source != SourceGlobal {
		t.Fatalf("source = %q, want global; the stray store above the repo must not capture it", cfg.Source)
	}
	if canon(t, cfg.DocketDir) != canon(t, filepath.Join(home, ".docket")) {
		t.Errorf("DocketDir = %q, want the global store under HOME", cfg.DocketDir)
	}
}

// TestResolveGlobalOutsideARepo: no DOCKET_PATH, no local store, no git — the
// store is ~/.docket and the execution facts fall back to the cwd.
func TestResolveGlobalOutsideARepo(t *testing.T) {
	world := t.TempDir()
	home := t.TempDir()
	cfg := resolveAt(t, world, "", home)

	if cfg.Source != SourceGlobal {
		t.Fatalf("source = %q, want global", cfg.Source)
	}
	if canon(t, cfg.DocketDir) != canon(t, filepath.Join(home, ".docket")) {
		t.Errorf("DocketDir = %q, want ~/.docket", cfg.DocketDir)
	}
	if canon(t, cfg.ExecRoot) != canon(t, world) {
		t.Errorf("ExecRoot = %q, want the cwd %q", cfg.ExecRoot, world)
	}
	if lock := cfg.TreeLockPath(); filepath.Dir(lock) != filepath.Join(cfg.DocketDir, "locks") {
		t.Errorf("TreeLockPath = %q; a global store partitions locks per project under locks/", lock)
	}
}

// TestResolveGlobalSharesIdentityAcrossWorktrees is requirement 1 in one
// assertion: two worktrees of one repository resolve to the SAME identity —
// the common git dir with its /.git suffix stripped — while each keeps its own
// exec root. That shared identity is what lets them share trust entries, one
// tree lock, and (v12) one project row.
func TestResolveGlobalSharesIdentityAcrossWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitInit(t, repo)
	mustGit(t, repo, "commit", "--allow-empty", "-m", "seed")
	wt := filepath.Join(t.TempDir(), "wt")
	mustGit(t, repo, "worktree", "add", wt)

	home := t.TempDir()
	main := resolveAt(t, repo, "", home)
	other := resolveAt(t, wt, "", home)

	if main.Source != SourceGlobal || other.Source != SourceGlobal {
		t.Fatalf("sources = %q/%q, want global/global", main.Source, other.Source)
	}
	if main.Identity != other.Identity {
		t.Errorf("identities differ across worktrees:\n  main %q\n  wt   %q", main.Identity, other.Identity)
	}
	if canon(t, main.Identity) != canon(t, repo) {
		t.Errorf("identity = %q, want the repository path %q", main.Identity, repo)
	}
	if canon(t, other.ExecRoot) != canon(t, wt) {
		t.Errorf("the worktree's ExecRoot = %q, want its own checkout %q", other.ExecRoot, wt)
	}
	if main.TreeLockPath() != other.TreeLockPath() {
		t.Errorf("tree locks differ across worktrees:\n  %q\n  %q",
			main.TreeLockPath(), other.TreeLockPath())
	}
	if got, want := other.InstanceConfigDir(), filepath.Join(other.ExecRoot, ".docket", "config"); got != want {
		t.Errorf("InstanceConfigDir = %q, want %q (config stays with the checkout)", got, want)
	}
}

// TestInstanceConfigDirsPerSource pins the ROOT LISTS, which are the whole
// contract the scanner and the packet resolver share.
//
// Only the global store has more than one: `~/.docket/config/` is the corpus
// every project on the machine draws from, and a repository's own
// `.docket/config/` adds to it. The ORDER is the precedence, so it is asserted
// rather than merely the membership.
func TestInstanceConfigDirsPerSource(t *testing.T) {
	t.Run("env resolves to the store's config alone", func(t *testing.T) {
		world := t.TempDir()
		store := filepath.Join(world, "custom-store")
		cfg := resolveAt(t, world, store, t.TempDir())

		want := []string{filepath.Join(store, "config")}
		if got := cfg.InstanceConfigDirs(); !equalPaths(got, want) {
			t.Errorf("InstanceConfigDirs = %v, want %v — an env store IS the instance", got, want)
		}
	})

	t.Run("local resolves to the repo's store config alone", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not available")
		}
		repo := t.TempDir()
		gitInit(t, repo)
		mkStore(t, repo)

		cfg := resolveAt(t, repo, "", t.TempDir())
		if cfg.Source != SourceLocal {
			t.Fatalf("source = %q, want local", cfg.Source)
		}
		want := []string{filepath.Join(cfg.DocketDir, "config")}
		if got := cfg.InstanceConfigDirs(); !equalPaths(got, want) {
			t.Errorf("InstanceConfigDirs = %v, want %v", got, want)
		}
	})

	t.Run("global resolves to the shared corpus THEN the checkout", func(t *testing.T) {
		world := t.TempDir()
		home := t.TempDir()
		cfg := resolveAt(t, world, "", home)
		if cfg.Source != SourceGlobal {
			t.Fatalf("source = %q, want global", cfg.Source)
		}

		got := cfg.InstanceConfigDirs()
		if len(got) != 2 {
			t.Fatalf("InstanceConfigDirs = %v, want two roots", got)
		}
		if canon(t, got[0]) != canon(t, filepath.Join(home, ".docket", "config")) {
			t.Errorf("the FIRST root is %q, want the shared corpus under HOME", got[0])
		}
		if canon(t, got[1]) != canon(t, filepath.Join(cfg.ExecRoot, ".docket", "config")) {
			t.Errorf("the SECOND root is %q, want the checkout's own additions", got[1])
		}
	})

	// A checkout that IS the home directory names ONE root, not the same one
	// twice. This is the exact-string case; the engine canonicalizes each root
	// before scanning and deduplicates again there, which is what catches the
	// two spellings a symlinked ancestor produces.
	t.Run("a checkout that is HOME names one root, not two", func(t *testing.T) {
		home := canon(t, t.TempDir())
		cfg := resolveAt(t, home, "", home)
		if got := cfg.InstanceConfigDirs(); len(got) != 1 {
			t.Errorf("InstanceConfigDirs = %v, want one root — the two coincide", got)
		}
	})
}

// TestInstanceConfigDirWritesToTheRepository: READING unions the roots, WRITING
// has one answer, and under the global store it is the repository.
//
// InstanceConfigDir is deliberately NOT InstanceConfigDirs()[0]. A template
// `workflow init` wrote into `~/.docket/config/` would appear in every project
// on the machine, which is never what running it inside a repo means.
func TestInstanceConfigDirWritesToTheRepository(t *testing.T) {
	world := t.TempDir()
	home := t.TempDir()
	cfg := resolveAt(t, world, "", home)

	want := filepath.Join(cfg.ExecRoot, ".docket", "config")
	got := cfg.InstanceConfigDir()
	if got != want {
		t.Errorf("InstanceConfigDir = %q, want %q (authored config belongs to the checkout)", got, want)
	}
	if roots := cfg.InstanceConfigDirs(); roots[0] == got {
		t.Errorf("the write directory is the FIRST read root %q; a definition typed "+
			"in a repo must not land in the shared corpus", roots[0])
	}
}

// equalPaths compares two root lists element-wise, order included: the order IS
// the precedence.
func equalPaths(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestLocalAtMirrorsResolution: what `init --local` builds is what Resolve
// would find once the store exists.
func TestLocalAtMirrorsResolution(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitInit(t, repo)

	built := LocalAt(repo)
	if built.Source != SourceLocal {
		t.Fatalf("LocalAt source = %q, want local", built.Source)
	}
	mkStore(t, repo)
	resolved := resolveAt(t, repo, "", t.TempDir())
	if canon(t, resolved.DocketDir) != canon(t, built.DocketDir) ||
		canon(t, resolved.Identity) != canon(t, built.Identity) {
		t.Errorf("Resolve after LocalAt disagrees:\n  built    %+v\n  resolved %+v", built, resolved)
	}
}

// TestGlobalStoreCoexistsWithExistingContent is G12: on this machine
// ~/.docket already carries a vorpal-managed contracts/fragments library the
// binary never reads. The global store must land BESIDE that content — the
// directory existing without issues.db reads as "not yet initialized", never
// as an obstacle and never as something to clobber.
func TestGlobalStoreCoexistsWithExistingContent(t *testing.T) {
	world := t.TempDir()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".docket", "contracts"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(home, ".docket", "contracts", "fix.md")
	if err := os.WriteFile(marker, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := resolveAt(t, world, "", home)
	if cfg.Source != SourceGlobal {
		t.Fatalf("source = %q, want global", cfg.Source)
	}
	exists, err := cfg.Exists()
	if err != nil || exists {
		t.Errorf("Exists() = (%v, %v); a store dir without issues.db is uninitialized, not occupied", exists, err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "existing" {
		t.Errorf("resolution disturbed pre-existing ~/.docket content: %q, %v", body, err)
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "config", "user.email", "test@example.invalid")
	mustGit(t, dir, "config", "user.name", "test")
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Hermetic like internal/engine's gitEnv(): the operator's global config
	// must not reach the subprocess, or commit signing (and whatever else it
	// enables) fails these tests on machines where it is on.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=qa", "GIT_AUTHOR_EMAIL=qa@example.invalid",
		"GIT_COMMITTER_NAME=qa", "GIT_COMMITTER_EMAIL=qa@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
