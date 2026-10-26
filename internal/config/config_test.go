package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/trust"
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

// TestResolveFindsLocalStoreFromSubdir is the half the old resolver lacked:
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

// TestResolveLocalStoreFromSubdirOfUnregisteredCheckout: a checkout whose git
// directory lives elsewhere (`git init --separate-git-dir`) is not registered
// for identity, but its toplevel still bounds the local-store walk, so the
// store at the toplevel is found from a nested subdirectory.
func TestResolveLocalStoreFromSubdirOfUnregisteredCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	world := canon(t, t.TempDir())
	work := filepath.Join(world, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, work, "init", "-q", "--separate-git-dir", filepath.Join(world, "sepgit.d"))
	mkStore(t, work)
	sub := filepath.Join(work, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if git, ok := lookupGit(sub); !ok || git.Registered || git.Toplevel != work {
		t.Fatalf("lookupGit = %+v/%v; the scenario needs an unregistered checkout with toplevel %q", git, ok, work)
	}

	cfg := resolveAt(t, sub, "", t.TempDir())
	if cfg.Source != SourceLocal {
		t.Fatalf("source = %q, want local; the toplevel bounds the walk even when the checkout is unregistered", cfg.Source)
	}
	if cfg.DocketDir != filepath.Join(work, ".docket") || cfg.ExecRoot != work {
		t.Errorf("DocketDir/ExecRoot = %q/%q, want %q/%q",
			cfg.DocketDir, cfg.ExecRoot, filepath.Join(work, ".docket"), work)
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

// seededRepo creates a git repository with one commit, so worktrees can be
// added to it, and returns its canonical path.
func seededRepo(t *testing.T) string {
	t.Helper()
	repo := canon(t, t.TempDir())
	gitInit(t, repo)
	mustGit(t, repo, "commit", "--allow-empty", "-m", "seed")
	return repo
}

// writeFile writes body to path, creating its parent directories.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertUnanchoredAt checks that resolving from dir kept none of the trusted
// repository's facts: the identity and exec root are dir itself, unanchored.
func assertUnanchoredAt(t *testing.T, dir, trusted string) {
	t.Helper()
	cfg := resolveAt(t, dir, "", t.TempDir())
	if cfg.Identity == trusted {
		t.Fatalf("Identity = %q: a foreign directory resolved the trusted repository's identity", cfg.Identity)
	}
	if cfg.Anchored {
		t.Errorf("Anchored = true; a directory git does not register for the repository must not anchor")
	}
	if cfg.Identity != dir || cfg.ExecRoot != dir {
		t.Errorf("Identity/ExecRoot = %q/%q, want both the cwd %q", cfg.Identity, cfg.ExecRoot, dir)
	}
}

// TestForeignGitPointer: a directory outside the repository whose .git points
// into the repository's gitdir is not one of its checkouts, so it must not
// resolve the repository's identity, which is what trust entries bind to.
func TestForeignGitPointer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	trusted := seededRepo(t)

	t.Run(".git file pointing at the repository's gitdir", func(t *testing.T) {
		foreign := canon(t, t.TempDir())
		writeFile(t, filepath.Join(foreign, ".git"), "gitdir: "+filepath.Join(trusted, ".git")+"\n")
		assertUnanchoredAt(t, foreign, trusted)
	})

	t.Run(".git file pointing at a registered worktree's admin dir", func(t *testing.T) {
		linked := filepath.Join(canon(t, t.TempDir()), "linked")
		mustGit(t, trusted, "worktree", "add", linked)
		foreign := canon(t, t.TempDir())
		admin := filepath.Join(trusted, ".git", "worktrees", "linked")
		writeFile(t, filepath.Join(foreign, ".git"), "gitdir: "+admin+"\n")
		assertUnanchoredAt(t, foreign, trusted)
	})

	t.Run(".git directory whose commondir names the repository's gitdir", func(t *testing.T) {
		foreign := canon(t, t.TempDir())
		gitDir := filepath.Join(foreign, ".git")
		writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")
		writeFile(t, filepath.Join(gitDir, "commondir"), filepath.Join(trusted, ".git")+"\n")
		for _, sub := range []string{"refs", "objects"} {
			if err := os.MkdirAll(filepath.Join(gitDir, sub), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		assertUnanchoredAt(t, foreign, trusted)
	})
}

// TestMissingBackLinkDropsGitContext: when a registered worktree's back-link
// cannot be read, the check fails closed rather than keeping the identity.
func TestMissingBackLinkDropsGitContext(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	trusted := seededRepo(t)
	linked := filepath.Join(canon(t, t.TempDir()), "linked")
	mustGit(t, trusted, "worktree", "add", linked)
	if err := os.Remove(filepath.Join(trusted, ".git", "worktrees", "linked", "gitdir")); err != nil {
		t.Fatal(err)
	}
	assertUnanchoredAt(t, linked, trusted)
}

// TestLinkedWorktreeOutsideIdentityRoot: a worktree git registered under the
// common dir keeps the repository's identity wherever it sits on disk, with
// either back-link spelling git writes.
func TestLinkedWorktreeOutsideIdentityRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cases := map[string][]string{
		"absolute back-link": nil,
		"relative back-link": {"--relative-paths"},
	}
	for name, flags := range cases {
		t.Run(name, func(t *testing.T) {
			trusted := seededRepo(t)
			sibling := filepath.Join(canon(t, t.TempDir()), "sibling")
			args := append(append([]string{"worktree", "add"}, flags...), sibling)
			mustGit(t, trusted, args...)

			cfg := resolveAt(t, sibling, "", t.TempDir())
			if cfg.Identity != trusted || cfg.ExecRoot != sibling || !cfg.Anchored {
				t.Errorf("Identity/ExecRoot/Anchored = %q/%q/%v, want %q/%q/true",
					cfg.Identity, cfg.ExecRoot, cfg.Anchored, trusted, sibling)
			}
		})
	}
}

// TestBareLayoutExecRoot: in a bare-repository layout every checkout is a
// worktree of repo.git, none of them the identity root, and each resolves to
// the bare directory's identity with its own exec root.
func TestBareLayoutExecRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	source := seededRepo(t)
	bare := filepath.Join(canon(t, t.TempDir()), "repo.git")
	mustGit(t, source, "clone", "-q", "--bare", source, bare)
	main := filepath.Join(bare, "main")
	mustGit(t, bare, "worktree", "add", main)

	cfg := resolveAt(t, main, "", t.TempDir())
	if cfg.Identity != bare || cfg.ExecRoot != main || !cfg.Anchored {
		t.Errorf("Identity/ExecRoot/Anchored = %q/%q/%v, want %q/%q/true",
			cfg.Identity, cfg.ExecRoot, cfg.Anchored, bare, main)
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

// TestSharedConfigDir pins the store root's config path every source reads
// first, and its absence when no store resolved.
func TestSharedConfigDir(t *testing.T) {
	t.Run("a store names its config directory", func(t *testing.T) {
		cfg := &Config{DocketDir: filepath.Join("srv", "store")}
		want := filepath.Join("srv", "store", "config")
		if got := cfg.SharedConfigDir(); got != want {
			t.Errorf("SharedConfigDir = %q, want %q", got, want)
		}
	})

	t.Run("no store names no directory", func(t *testing.T) {
		cfg := &Config{}
		if got := cfg.SharedConfigDir(); got != "" {
			t.Errorf("SharedConfigDir = %q, want empty when DocketDir is empty", got)
		}
	})
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

// TestGlobalStoreCoexistsWithExistingContent: on this machine
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

// writeClientConfig writes body as the client config under a sandboxed
// XDG_CONFIG_HOME, inside a 0700 docket directory, with the given file mode.
func writeClientConfig(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	return writeUserConfigFile(t, "client.toml", body, mode)
}

// writeUserConfigFile writes body as the named user-level config file under a
// sandboxed XDG_CONFIG_HOME, inside a 0700 docket directory, with the given
// file mode.
func writeUserConfigFile(t *testing.T, name, body string, mode os.FileMode) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "docket")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod after the write: WriteFile's mode is filtered by the umask.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

const sampleClientConfig = `version = 1
default_profile = "work"

[profile.work]
url = "https://docket.example.invalid:8443"
`

// TestLoadClientConfigRequiresPrivateMode: the client config lives next to the
// trust file and is held to its integrity rules. A group- or world-readable
// file is refused with the trust package's integrity error; 0600 loads.
func TestLoadClientConfigRequiresPrivateMode(t *testing.T) {
	path := writeClientConfig(t, sampleClientConfig, 0o644)

	trustPath, err := trust.StorePath()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ClientConfigPath(); err != nil || got != path || filepath.Dir(got) != filepath.Dir(trustPath) {
		t.Fatalf("ClientConfigPath = %q, %v; want %q beside the trust file %q", got, err, path, trustPath)
	}

	if _, err := LoadClientConfig(); !errors.Is(err, trust.ErrIntegrity) {
		t.Fatalf("mode 0644: LoadClientConfig err = %v, want the integrity error", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cc, err := LoadClientConfig()
	if err != nil {
		t.Fatalf("mode 0600: LoadClientConfig: %v", err)
	}
	if cc.DefaultProfile != "work" || cc.Profiles["work"].URL != "https://docket.example.invalid:8443" {
		t.Errorf("loaded %+v, want default_profile work with its url", cc)
	}
}

// TestLoadClientConfigMissingIsEmpty: no file is no configuration, not an
// error — the client then falls back to the local socket.
func TestLoadClientConfigMissingIsEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cc, err := LoadClientConfig()
	if err != nil {
		t.Fatalf("LoadClientConfig with no file: %v", err)
	}
	if cc.DefaultProfile != "" || len(cc.Profiles) != 0 {
		t.Errorf("loaded %+v from no file, want the empty configuration", cc)
	}
}

// TestLoadClientConfigRefusesMalformed: the file is parsed strictly, like the
// trust file, so a typo is an error instead of a silent default.
func TestLoadClientConfigRefusesMalformed(t *testing.T) {
	cases := map[string]string{
		"unknown key":              "version = 1\ndefault_profle = \"work\"\n",
		"unknown version":          "version = 2\n",
		"default names no profile": "version = 1\ndefault_profile = \"work\"\n",
		"profile without url":      "version = 1\n[profile.work]\n",
		"url not http or https":    "version = 1\n[profile.work]\nurl = \"ftp://docket.example.invalid\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			writeClientConfig(t, body, 0o600)
			if _, err := LoadClientConfig(); !errors.Is(err, ErrClientConfigParse) {
				t.Errorf("LoadClientConfig err = %v, want ErrClientConfigParse", err)
			}
		})
	}
}

// testBearerSecret is a synthetic 64-character secret drawn at runtime, so no
// credential-shaped literal is committed.
func testBearerSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func daemonConfigBody(secretLine string) string {
	return "version = 1\n" +
		"listen = \"0.0.0.0:8443\"\n" +
		"tls_cert_file = \"/etc/docket/cert.pem\"\n" +
		"tls_key_file = \"/etc/docket/key.pem\"\n" +
		"min_client_api_major = 2\n" +
		"max_client_api_major = 3\n" +
		secretLine + "\n"
}

// TestLoadDaemonConfigReadsPrivateFile: the daemon config sits beside the
// trust file under its integrity rules, and its secret is usable for
// comparison but never printed.
func TestLoadDaemonConfigReadsPrivateFile(t *testing.T) {
	secret := testBearerSecret(t)
	path := writeUserConfigFile(t, "daemon.toml", daemonConfigBody(fmt.Sprintf("bearer_secret = %q", secret)), 0o644)

	trustPath, err := trust.StorePath()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DaemonConfigPath(); err != nil || got != path || filepath.Dir(got) != filepath.Dir(trustPath) {
		t.Fatalf("DaemonConfigPath = %q, %v; want %q beside the trust file %q", got, err, path, trustPath)
	}

	if _, err := LoadDaemonConfig(); !errors.Is(err, trust.ErrIntegrity) {
		t.Fatalf("mode 0644: LoadDaemonConfig err = %v, want the integrity error", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	dc, err := LoadDaemonConfig()
	if err != nil {
		t.Fatalf("mode 0600: LoadDaemonConfig: %v", err)
	}
	if dc.Listen != "0.0.0.0:8443" || dc.TLSCertFile != "/etc/docket/cert.pem" || dc.TLSKeyFile != "/etc/docket/key.pem" {
		t.Errorf("loaded %+v, want the listen address and TLS paths from the file", dc)
	}
	if dc.MinClientAPIMajor != 2 || dc.MaxClientAPIMajor != 3 {
		t.Errorf("client API major window = [%d, %d], want [2, 3] from the file", dc.MinClientAPIMajor, dc.MaxClientAPIMajor)
	}
	if !dc.BearerSecret.Matches(secret) {
		t.Error("the loaded secret does not match the configured value")
	}
	if dc.BearerSecret.Matches(secret[:len(secret)-1]+"x") || dc.BearerSecret.Matches("") {
		t.Error("the loaded secret matches a value other than the configured one")
	}
	if printed := fmt.Sprintf("%v %+v %#v %s", dc, *dc, *dc, dc.BearerSecret); strings.Contains(printed, secret) {
		t.Errorf("formatting the daemon config printed the secret: %s", printed)
	}
}

// TestLoadDaemonConfigMissingRefusesStart: unlike the client config, no file
// is not an empty configuration; a daemon with no secret must not start.
func TestLoadDaemonConfigMissingRefusesStart(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if dc, err := LoadDaemonConfig(); err == nil {
		t.Fatalf("LoadDaemonConfig with no file = %+v, want an error", dc)
	}
}

// TestZeroBearerSecretMatchesNothing: a secret that was never configured
// admits no request, including one presenting an empty token.
func TestZeroBearerSecretMatchesNothing(t *testing.T) {
	var zero BearerSecret
	if zero.Matches("") {
		t.Error("the zero BearerSecret matches the empty token")
	}
}

// TestLoadDaemonConfigRefusesMalformed: the file is parsed strictly, and a
// refused secret is never echoed in the error.
func TestLoadDaemonConfigRefusesMalformed(t *testing.T) {
	secret := testBearerSecret(t)
	valid := daemonConfigBody(fmt.Sprintf("bearer_secret = %q", secret))
	cases := map[string]string{
		"unknown key":          valid + "bearer_secert = \"x\"\n",
		"unknown version":      strings.Replace(valid, "version = 1", "version = 2", 1),
		"no listen":            strings.Replace(valid, "listen = \"0.0.0.0:8443\"\n", "", 1),
		"listen without port":  strings.Replace(valid, "0.0.0.0:8443", "0.0.0.0", 1),
		"no cert":              strings.Replace(valid, "tls_cert_file = \"/etc/docket/cert.pem\"\n", "", 1),
		"no key":               strings.Replace(valid, "tls_key_file = \"/etc/docket/key.pem\"\n", "", 1),
		"no window min":        strings.Replace(valid, "min_client_api_major = 2\n", "", 1),
		"no window max":        strings.Replace(valid, "max_client_api_major = 3\n", "", 1),
		"window min above max": strings.Replace(valid, "min_client_api_major = 2", "min_client_api_major = 4", 1),
		"no secret":            daemonConfigBody(""),
		"empty secret":         daemonConfigBody(`bearer_secret = ""`),
		"short secret":         daemonConfigBody(fmt.Sprintf("bearer_secret = %q", secret[:31])),
		"secret with space":    daemonConfigBody(fmt.Sprintf("bearer_secret = %q", secret[:20]+" "+secret[20:])),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			writeUserConfigFile(t, "daemon.toml", body, 0o600)
			_, err := LoadDaemonConfig()
			if !errors.Is(err, ErrDaemonConfigParse) {
				t.Fatalf("LoadDaemonConfig err = %v, want ErrDaemonConfigParse", err)
			}
			if strings.Contains(err.Error(), secret[:20]) {
				t.Errorf("error %q echoes the secret", err)
			}
		})
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
