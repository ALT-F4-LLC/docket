package config

import (
	"context"
	"os"
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
	// This stays a FILESYSTEM PATH deliberately, for internal/trust's
	// reason: remote URLs and root commits are repo-controlled and survive a
	// hostile clone; where the repository sits on the operator's disk is not
	// and does not.
	//
	// It is empty unless Registered.
	Identity string
	// Registered reports whether Toplevel is a checkout the repository at the
	// common directory registered as its own (see isRegisteredCheckout). Only
	// a registered checkout may claim Identity; an unregistered one still has
	// a Toplevel, which bounds the local-store walk.
	Registered bool
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
//
// A toplevel that is not a checkout of the repository git reports still
// resolves, with Registered false and no Identity: a `.git` file or
// `commondir` is input from whoever wrote the directory, and can name any
// repository on disk. See isRegisteredCheckout.
func lookupGit(dir string) (gitContext, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir,
		"rev-parse", "--path-format=absolute",
		"--show-toplevel", "--git-common-dir", "--git-dir").Output()
	if err != nil {
		return gitContext{}, false
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 {
		return gitContext{}, false
	}
	var paths [3]string
	for i := range paths {
		resolved, err := filepath.EvalSymlinks(strings.TrimSpace(lines[i]))
		if err != nil {
			return gitContext{}, false
		}
		paths[i] = resolved
	}
	toplevel, commonDir, gitDir := paths[0], paths[1], paths[2]

	identity := commonDir
	if filepath.Base(commonDir) == ".git" {
		identity = filepath.Dir(commonDir)
	}
	if !isRegisteredCheckout(toplevel, gitDir, commonDir, identity) {
		return gitContext{Toplevel: toplevel}, true
	}
	return gitContext{Toplevel: toplevel, Identity: identity, Registered: true}, true
}

// isRegisteredCheckout reports whether toplevel is a checkout of the
// repository at commonDir: the identity root itself, or a linked worktree git
// registered there. Registration is proven by the back-link `git worktree add`
// writes to <commonDir>/worktrees/<name>/gitdir, inside the repository's own
// gitdir where the checkout cannot author it. Every path must be canonical. Any
// unreadable or mismatched back-link reports false.
func isRegisteredCheckout(toplevel, gitDir, commonDir, identity string) bool {
	if toplevel == identity {
		return true
	}
	if filepath.Dir(gitDir) != filepath.Join(commonDir, "worktrees") {
		return false
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return false
	}
	backLink := strings.TrimSpace(string(data))
	if backLink == "" {
		return false
	}
	// `git worktree add --relative-paths` writes the back-link relative to the
	// admin directory.
	if !filepath.IsAbs(backLink) {
		backLink = filepath.Join(gitDir, backLink)
	}
	resolved, err := filepath.EvalSymlinks(backLink)
	return err == nil && resolved == filepath.Join(toplevel, ".git")
}
