package config

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
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
	// This stays a FILESYSTEM PATH deliberately, for internal/trust's §3.4 P1
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
	if out, err := exec.CommandContext(ctx, "git", "-C", dir,
		"rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", dir,
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
