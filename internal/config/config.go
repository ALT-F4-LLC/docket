package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	osexec "os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/exec"
)

const dbFileName = "issues.db"

// Source names which rule resolved the store directory.
type Source string

const (
	// SourceEnv means DOCKET_PATH named the store directly.
	SourceEnv Source = "env"
	// SourceLocal means a repo-local .docket store was discovered at or above
	// the working directory.
	SourceLocal Source = "local"
	// SourceGlobal means the shared per-user store, ~/.docket.
	SourceGlobal Source = "global"
)

// Config holds the resolved execution facts for one invocation.
//
// These USED TO BE ONE VALUE: the parent of the .docket directory stood in for
// the working-tree root, the trust identity, and the lock partition all at
// once. A store shared by every project breaks that apart — where a command
// runs, which project it belongs to, and where its database lives are three
// separate facts, and each field below answers exactly one of them.
type Config struct {
	// DocketDir is the store directory — the one holding issues.db.
	DocketDir string
	// DBPath is the full path to issues.db.
	DBPath string
	// ExecRoot is the working-tree root of the invocation: the cwd every gate
	// and action runs in, and the containment boundary for their binaries. For
	// an env- or locally-resolved store it is the parent of the store, exactly
	// the value the engine always used; for the global store it is the git
	// worktree toplevel (or the cwd outside a repository).
	ExecRoot string
	// Identity is the stable project identity path: what trust entries bind to
	// and (v12) what keys the project row. For env/local stores it equals
	// ExecRoot — the legacy rule, unchanged. For the global store it is the
	// git common directory with a trailing "/.git" stripped, which is the one
	// path every worktree of a repository shares; a bare-repo layout
	// (repo.git/branch-worktrees) resolves to the bare directory itself.
	Identity string
	// Source records which rule resolved the store.
	Source Source
	// EnvVarSet reports whether DOCKET_PATH was used.
	EnvVarSet bool
	// Anchored reports whether Identity names a DELIBERATE project rather than
	// whatever directory the process happened to start in (DKT-58).
	//
	// It is true for the env and local stores — a `.docket` directory is an act
	// of intent, and its parent is the project by construction — and for the
	// global store only INSIDE a git worktree, where the identity is the
	// repository's own common directory.
	//
	// Outside a repository the global store still fills Identity with the cwd,
	// because a project row already bound to that path must keep resolving; but
	// an unanchored identity MUST NOT MINT ONE. A judge executor running from
	// the shared scratchpad root minted a permanent project row named
	// `claude-501` on exactly that path, and the row then collided on the
	// hardcoded prefix and could not be deleted. Registration reads this field.
	Anchored bool
}

// Resolve returns the current configuration.
//
// Store resolution order:
//  1. DOCKET_PATH, taken as the store directory (normalized to absolute).
//  2. A repo-local `.docket` store containing issues.db, discovered by walking
//     from the cwd up to the git worktree toplevel (just the cwd outside a
//     repository) — the legacy per-repo layout keeps working wherever it
//     already exists, now also from subdirectories.
//  3. The shared per-user store, ~/.docket.
//
// For sources 1 and 2 the ExecRoot/Identity pair is the parent of the store —
// the rule the engine and the trust matcher have always shared — so an
// existing setup resolves to byte-identical facts. Only the global store
// introduces git-derived values, because only there can two checkouts of one
// project share a database.
func Resolve() (*Config, error) {
	if envPath := os.Getenv("DOCKET_PATH"); envPath != "" {
		abs, err := filepath.Abs(envPath)
		if err != nil {
			return nil, err
		}
		root := filepath.Dir(abs)
		return &Config{
			DocketDir: abs,
			DBPath:    filepath.Join(abs, dbFileName),
			ExecRoot:  root,
			Identity:  root,
			Source:    SourceEnv,
			EnvVarSet: true,
			Anchored:  true,
		}, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	// Canonicalize BEFORE any comparison. Getwd reports the logical path the
	// process chdir'd to while git reports resolved paths (macOS: /var vs
	// /private/var), and a walk bounded by a path in the other form never
	// meets its bound.
	cwd = canonicalPath(cwd)

	git, inRepo := lookupGit(cwd)
	if inRepo {
		git.Toplevel = canonicalPath(git.Toplevel)
	}

	// Legacy repo-local store. The walk is bounded by the worktree toplevel
	// (or the cwd itself outside a repository) so a stray `.docket` above the
	// repository can never capture it. The store counts only when issues.db
	// exists: a repo that ships `.docket/config/` but keeps its database
	// elsewhere is a global-store repo with repo-side instance config, not a
	// local-store repo.
	walkTop := cwd
	if inRepo && exec.Under(git.Toplevel, cwd) {
		walkTop = git.Toplevel
	}
	if root, ok := findLocalStore(cwd, walkTop); ok {
		docketDir := filepath.Join(root, ".docket")
		return &Config{
			DocketDir: docketDir,
			DBPath:    filepath.Join(docketDir, dbFileName),
			ExecRoot:  root,
			Identity:  root,
			Source:    SourceLocal,
			Anchored:  true,
		}, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	store := filepath.Join(home, ".docket")
	execRoot, identity := cwd, cwd
	if inRepo {
		execRoot = git.Toplevel
		identity = git.Identity
	}
	return &Config{
		DocketDir: store,
		DBPath:    filepath.Join(store, dbFileName),
		ExecRoot:  execRoot,
		Identity:  canonicalPath(identity),
		Source:    SourceGlobal,
		// Only a git-derived identity is anchored here: the cwd fallback above
		// is a guess about which project this is, and a guess must not create
		// one (DKT-58).
		Anchored: inRepo,
	}, nil
}

// LocalAt builds the configuration for an explicitly repo-local store rooted
// at dir — what `docket init --local` creates. It mirrors what Resolve would
// return once that store exists.
func LocalAt(dir string) *Config {
	docketDir := filepath.Join(dir, ".docket")
	return &Config{
		DocketDir: docketDir,
		DBPath:    filepath.Join(docketDir, dbFileName),
		ExecRoot:  dir,
		Identity:  dir,
		Source:    SourceLocal,
		Anchored:  true,
	}
}

// findLocalStore walks from start up to and including top, returning the first
// directory whose `.docket/issues.db` exists. Both paths must already be
// canonical — the bound is compared by identity.
func findLocalStore(start, top string) (string, bool) {
	dir := start
	for {
		if fileExists(filepath.Join(dir, ".docket", dbFileName)) {
			return dir, true
		}
		if dir == top {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Filesystem root without meeting top: the bound was not an
			// ancestor of start. Stop rather than walk the whole disk.
			return "", false
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// canonicalPath resolves symlinks best-effort: the trust matcher re-resolves
// what it binds, so this only has to be stable, not perfect. A path that does
// not resolve (not yet created, permission) is returned as-is.
func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// InstanceConfigDirs returns the ORDERED roots the instance-config tree
// (`workflows/`, `schemas/`, `contracts/`, `fragments/`, `templates/`,
// `policy.toml`) is read from.
//
// It is a LIST rather than a single directory because the shared store now
// carries a shared corpus. `~/.docket/config/` holds the definitions every
// project on the machine draws from; a repository may still ship its own
// additions at `<worktree>/.docket/config/`. Both are read, SHARED FIRST, and
// a repo that ships nothing needs no `.docket/` directory at all — which is
// what lets a linked worktree with no config of its own resolve the same
// packet files as the checkout the run was activated in.
//
//	env    [ <store>/config ]                        — the store IS the instance
//	local  [ <store>/config ]                        — likewise; the store is the repo's
//	global [ ~/.docket/config, <worktree>/.docket/config ]
//
// The ORDER IS THE PRECEDENCE and it is fixed: resolution takes the first root
// holding a ref. That is only deterministic because activation refuses a ref
// (or a `name@version`) present in two roots with DIFFERENT bytes — see
// scanConfigDirs — so first-found can never mean "silently shadowed".
func (c *Config) InstanceConfigDirs() []string {
	if c.Source != SourceGlobal {
		if c.DocketDir == "" {
			return nil
		}
		return []string{filepath.Join(c.DocketDir, "config")}
	}

	var roots []string
	if c.DocketDir != "" {
		roots = append(roots, filepath.Join(c.DocketDir, "config"))
	}
	if c.ExecRoot != "" {
		repo := filepath.Join(c.ExecRoot, ".docket", "config")
		// A checkout that IS the home directory would name one directory
		// twice. Deduplicating here keeps every consumer free of the case.
		if len(roots) == 0 || roots[0] != repo {
			roots = append(roots, repo)
		}
	}
	return roots
}

// InstanceConfigDir returns where instance config this invocation AUTHORS is
// WRITTEN — `workflow init`'s target directory.
//
// It is deliberately NOT InstanceConfigDirs()[0]. Reading unions the shared
// corpus with the repository's additions; WRITING has one answer, and for the
// global store it is the repository, not `~/.docket/config`. A template written
// into the shared corpus would appear in every project on the machine, which is
// never what `workflow init` inside a repo means.
func (c *Config) InstanceConfigDir() string {
	if c.Source == SourceGlobal {
		if c.ExecRoot == "" {
			return ""
		}
		return filepath.Join(c.ExecRoot, ".docket", "config")
	}
	if c.DocketDir == "" {
		return ""
	}
	return filepath.Join(c.DocketDir, "config")
}

// TreeLockPath returns the tree-mutex lockfile for this project.
//
// Env/local stores keep `.docket/tree.lock`, the repo-resident location the
// engine has always locked. The global store gives each project its own file
// under `locks/`, keyed by a digest of the project identity — one shared
// lockfile would serialize tree gates across every repository on the machine,
// which is not what `tree = true` declares.
func (c *Config) TreeLockPath() string {
	if c.DocketDir == "" {
		return ""
	}
	if c.Source == SourceGlobal {
		sum := sha256.Sum256([]byte(c.Identity))
		return filepath.Join(c.DocketDir, "locks", hex.EncodeToString(sum[:8])+".lock")
	}
	return filepath.Join(c.DocketDir, "tree.lock")
}

// Exists checks if the docket directory and DB file both exist.
// It returns an error for non-existence failures (e.g. permission errors).
func (c *Config) Exists() (bool, error) {
	if _, err := os.Stat(c.DocketDir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if _, err := os.Stat(c.DBPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

var (
	defaultAuthor     string
	defaultAuthorOnce sync.Once
)

// DefaultAuthor returns the default author for comments and activity.
// It tries git config user.name first and falls back to the OS username.
// The result is cached for the lifetime of the process.
func DefaultAuthor() string {
	defaultAuthorOnce.Do(func() {
		defaultAuthor = resolveAuthor()
	})
	return defaultAuthor
}

func resolveAuthor() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := osexec.CommandContext(ctx, "git", "config", "user.name").Output()
	if err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name
		}
	}

	u, err := user.Current()
	if err == nil && u.Username != "" {
		return u.Username
	}

	return "unknown"
}
