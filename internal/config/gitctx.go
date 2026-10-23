package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// gitContext is what one `git rev-parse` invocation establishes about the
// working directory: which checkout the operator is standing in, and which
// project that checkout belongs to.
type gitContext struct {
	// Toplevel is the absolute path of the working-tree root — the checkout,
	// not the project.
	Toplevel string
	// Identity is the stable project identity: the git common directory with a
	// trailing "/.git" stripped. Every worktree of a repository — including a
	// throwaway one under a temp directory — shares its common directory, which
	// is what makes this the one path that means "the project" rather than
	// "this checkout". A normal clone resolves to the repository root; a
	// bare-repo layout (repo.git with worktree subdirectories) resolves to the
	// bare directory itself.
	//
	// This stays a FILESYSTEM PATH deliberately, for internal/trust's
	// reason: remote URLs and root commits are repo-controlled and survive a
	// hostile clone; where the repository sits on the operator's disk is not
	// and does not.
	Identity string
}

// GitHead reports dir's checked-out branch and commit, best-effort: a
// directory that is not a checkout (or a git that is absent or slow) yields
// empty strings, because a record should carry "unknown" as absence rather
// than as an invented value. A detached HEAD reports branch "HEAD", which is
// git's own honest spelling for it.
func GitHead(dir string) (branch, commit string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := gitHeadCmd(ctx, dir,
		"rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	if out, err := gitHeadCmd(ctx, dir,
		"rev-parse", "HEAD").Output(); err == nil {
		commit = strings.TrimSpace(string(out))
	}
	return branch, commit
}

// lookupGit resolves the git context for dir. The second return is false when
// dir is not inside a git working tree (or git is missing, times out, or
// predates --path-format) — callers then fall back to cwd-derived values,
// which is the correct answer for a directory that simply is not a checkout.
func lookupGit(dir string) (gitContext, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir,
		"rev-parse", "--path-format=absolute",
		"--show-toplevel", "--git-common-dir").Output()
	if err != nil {
		return gitContext{}, false
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return gitContext{}, false
	}
	toplevel := strings.TrimSpace(lines[0])
	commonDir := strings.TrimSpace(lines[1])
	if toplevel == "" || commonDir == "" {
		return gitContext{}, false
	}

	identity := commonDir
	if filepath.Base(commonDir) == ".git" {
		identity = filepath.Dir(commonDir)
	}
	return gitContext{Toplevel: toplevel, Identity: identity}, true
}

// gitHeadCmd builds the git command GitHead runs. The caller of `docket step
// complete` is the executor, so this process inherits an environment the
// executor controls: the variables that redirect git (GIT_DIR, GIT_WORK_TREE and
// the object-store overrides) would change which HEAD a record binds to, the
// ones that name a program (GIT_EXEC_PATH for git's own sub-commands) would run
// it, and the hooks and fsmonitor a repository's config names would run one
// too. The redirects and program names are dropped here and the hooks turned
// off by -c, as the engine's own git calls do. Dropping the GIT_CONFIG*
// variables removes one place a config could come from, not every one: HOME
// stays, and the repository config is the executor's to write, so it is the -c
// overrides and not the scrubbing that keep a config from running a program.
// The git binary is resolved through PATH once per process (gitBinary).
func gitHeadCmd(ctx context.Context, dir string, args ...string) *exec.Cmd {
	full := append([]string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"--no-replace-objects", "-C", dir,
	}, args...)
	cmd := exec.CommandContext(ctx, gitBinary(), full...)
	drop := []string{
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR=",
		"GIT_OBJECT_DIRECTORY=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=", "GIT_NAMESPACE=",
		"GIT_PREFIX=", "GIT_CONFIG=", "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
		"GIT_CONFIG_COUNT=", "GIT_CONFIG_PARAMETERS=", "GIT_CONFIG_KEY_",
		"GIT_CONFIG_VALUE_", "GIT_REPLACE_REF_BASE=", "GIT_GRAFT_FILE=",
		"GIT_EXEC_PATH=", "GIT_TEMPLATE_DIR=",
	}
next:
	for _, kv := range os.Environ() {
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				continue next
			}
		}
		cmd.Env = append(cmd.Env, kv)
	}
	return cmd
}

// gitBinary is the git this package runs, resolved through PATH once per
// process and then named by absolute path, so a PATH that changes between two
// commands cannot swap the binary under a sequence that already started. A git
// that PATH does not hold leaves the bare name, and the command fails as it
// would have anyway. (The engine has its own copy; this package cannot import
// it.)
func gitBinary() string {
	gitBinaryOnce.Do(func() {
		gitBinaryPath = "git"
		if p, err := exec.LookPath("git"); err == nil {
			gitBinaryPath = p
		}
	})
	return gitBinaryPath
}

var (
	gitBinaryOnce sync.Once
	gitBinaryPath string
)
