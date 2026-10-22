package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitPick is what one range pick did to the shared checkout.
type GitPick struct {
	// Before is the checkout's HEAD ahead of the pick.
	Before string
	// Tip is its HEAD after, the commit a `step annotate --integrated-sha`
	// names.
	Tip string
	// Picked lists the commits the pick added, oldest first.
	Picked []string
}

// integrateGit runs git in the shared checkout for the integration verb, the
// engine's only git WRITE. It differs from the read-only probes in three ways:
//
//   - the environment loses every variable that redirects git away from
//     execRoot (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE and the object-store
//     overrides), so the checkout the run recorded is the one written;
//   - repository hooks, the hooks the config names (hook.<name>.command) and the
//     fsmonitor hook are off (core.hooksPath, hook.<name>.enabled and
//     core.fsmonitor), as in the engine-owned gate checkout: a hook is code the
//     repository's writers control, and the engine does not run it from a verb
//     the conductor typed;
//   - no editor, no terminal prompt, and rerere off, so nothing waits on a
//     person or resolves a conflict on its own.
//
// Filters, merge drivers and commit signing come from the repository's and the
// user's git config and still run: the pick is the same operation the
// conductor ran by hand.
func integrateGit(execRoot string, args ...string) (string, error) {
	full := append([]string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "rerere.enabled=false", "--no-replace-objects", "-C", execRoot,
	}, args...)
	full = append(configHookFlags(execRoot), full...)
	cmd := exec.Command("git", full...)
	cmd.Env = integrateGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = out
		}
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, truncateText(detail, 600))
	}
	return out, nil
}

// gitCommand builds EVERY git command the engine runs against a tree an
// executor can influence (the shared repository, a scratch checkout of a
// recorded commit, a writer's live worktree), so none of them is the weaker
// one:
//
//   - replace refs and grafts do not rewrite what a commit holds, because an
//     executor in a linked worktree writes refs/replace for the whole shared
//     repository and the gate would otherwise measure a different tree than
//     the one `step integrate` later picks (it already runs
//     --no-replace-objects);
//   - repository hooks (core.hooksPath and the hook.<name> keys of the config),
//     fsmonitor and submodule recursion are off, since each
//     runs a program the repository's config names, and a linked worktree's
//     `git config` writes the shared config;
//   - the environment loses the variables that redirect git or inject config
//     or an external diff (integrateGitEnv).
//
// Diff-producing callers add --no-ext-diff and --no-textconv themselves, as
// those are options of the subcommand. Clean/smudge filters still run for a
// command that reads a worktree, and a config hook that only an includeIf
// condition inside a new worktree selects still runs on `worktree add`; those
// are residuals, not closed doors.
func gitCommand(args ...string) *exec.Cmd {
	full := append([]string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "submodule.recurse=false", "-c", "log.showSignature=false",
		"-c", "gpg.program=false", "-c", "protocol.allow=never", "--no-replace-objects",
	}, args...)
	full = append(configHookFlags(gitDirOf(args)), full...)
	cmd := exec.Command("git", full...)
	cmd.Env = integrateGitEnv()
	return cmd
}

// hookOffVar is the environment variable the per-hook disable flags read their
// value from. The hook names come from a config the executor writes, so they
// are passed as --config-env KEYS (a name may hold spaces, tabs or '=' and
// cannot be expressed safely as a `-c key=value` argument) with a fixed value.
const hookOffVar = "DOCKET_HOOK_OFF"

// configHookFlags disables every hook the repository's CONFIG names. Git 2.54
// reads `hook.<name>.command` and `hook.<name>.event` from the repository
// config, which an executor writes through `git config`, and
// core.hooksPath=/dev/null does not stop them. There is no wildcard for
// `hook.<name>.enabled`, so the names are listed from the same directory the
// command will run in, NUL-delimited. A listing that fails yields no flags:
// older git versions ignore these keys, and a repository git cannot read fails
// the command itself. The listing and the command are two reads, so a name
// added between them is not covered, and neither is a key that only an
// `includeIf` condition true inside a NEW worktree (`worktree add` runs its
// hooks in that worktree's context, which the listing cannot see). This is
// best-effort defence in depth, not a boundary: clean and smudge filters named
// by the same executor-writable config run on that checkout regardless, so
// whoever closes the filter residual closes this one with it.
func configHookFlags(dir string) []string {
	args := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}
	if dir != "" {
		args = append(args, "-C", dir)
	}
	args = append(args, "config", "--null", "--name-only", "--get-regexp", `^hook\.(.*\.)?(command|event)$`)
	cmd := exec.Command("git", args...)
	cmd.Env = integrateGitEnv()
	out, _ := cmd.Output()
	seen := map[string]bool{}
	var flags []string
	for _, key := range strings.Split(string(out), "\x00") {
		name := strings.TrimPrefix(key, "hook.")
		// A key with no further dot is the subsection-less `[hook]` table, which git
		// treats as the hook named "".
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[:i]
		} else {
			name = ""
		}
		if key == "" || seen[name] {
			continue
		}
		seen[name] = true
		flags = append(flags, "--config-env=hook."+name+".enabled="+hookOffVar)
	}
	return flags
}

// gitDirOf returns the directory a git argument list runs in: the value of its
// -C among the leading global options, or "" for the process's own directory.
// It stops at the first argument that is not a global option, so a later
// argument that happens to read "-C" (a pathspec after the subcommand) is not
// taken for one.
func gitDirOf(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-C" && i+1 < len(args):
			return args[i+1]
		case a == "-c" && i+1 < len(args):
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return ""
		}
	}
	return ""
}

// integrateGitEnv is the process environment without the variables that
// redirect git, inject configuration or name an external diff, plus the
// settings that keep it from waiting on a person.
func integrateGitEnv() []string {
	drop := []string{
		"GIT_EXTERNAL_DIFF=", "GIT_CONFIG=", "GIT_CONFIG_COUNT=", "GIT_CONFIG_PARAMETERS=",
		"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "GIT_DIFF_OPTS=", "GIT_ASKPASS=",
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR=",
		"GIT_OBJECT_DIRECTORY=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=",
		"GIT_NAMESPACE=", "GIT_PREFIX=", "GIT_EDITOR=", "GIT_SEQUENCE_EDITOR=",
		"GIT_TERMINAL_PROMPT=", "GIT_REPLACE_REF_BASE=", "GIT_GRAFT_FILE=", "GIT_NO_REPLACE_OBJECTS=", "GIT_NO_LAZY_FETCH=",
		"GIT_SSH=", "GIT_SSH_COMMAND=", "GIT_PROXY_COMMAND=", "GIT_PAGER=", "GIT_ALLOW_PROTOCOL=",
	}
	var env []string
next:
	for _, kv := range os.Environ() {
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				continue next
			}
		}
		env = append(env, kv)
	}
	return append(env, "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", hookOffVar+"=false")
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// chainBase is the commit a writer chain starts after; an unrecorded base asks
// about the head commit alone.
func chainBase(base, head string) string {
	if base == "" {
		return head + "^"
	}
	return base
}

// gitIntegrationChain answers, read-only, whether base..head is a chain the
// engine will pick: both ends are commits the checkout holds, base is an
// ancestor of head, the range has no merge commit (a cherry-pick needs -m for
// one, and the engine chooses no parent), and it is not empty. It returns the
// chain's commits, oldest first.
func gitIntegrationChain(execRoot, base, head string) ([]string, error) {
	if err := gitCheckSharedCheckout(execRoot); err != nil {
		return nil, err
	}
	base = chainBase(base, head)
	for _, rev := range []string{head, base} {
		if _, err := integrateGit(execRoot, "cat-file", "-e", rev+"^{commit}"); err != nil {
			return nil, fmt.Errorf("the writer's chain is not in the shared checkout: %.12s does not resolve as a commit there", strings.TrimSuffix(rev, "^"))
		}
	}
	if _, err := integrateGit(execRoot, "merge-base", "--is-ancestor", base, head); err != nil {
		return nil, fmt.Errorf("the writer's base %.12s is not an ancestor of its head %.12s", base, head)
	}
	merges, err := integrateGit(execRoot, "rev-list", "--merges", base+".."+head)
	if err != nil {
		return nil, err
	}
	if merges != "" {
		return nil, errors.New("the writer's chain holds a merge commit, which the engine does not pick; integrate it by hand and annotate the result")
	}
	out, err := integrateGit(execRoot, "rev-list", "--reverse", base+".."+head)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, errors.New("the writer's chain is empty: its head is its base, so there is nothing to pick")
	}
	return strings.Split(out, "\n"), nil
}

// gitCheckSharedCheckout refuses a checkout the engine must not commit onto: a
// detached HEAD (a pick there lands on no branch), tracked or staged changes
// (the pick would mix with, or be blocked by, someone's work), or an operation
// already in progress (the abort below would cancel it). Untracked files do not
// count; a pick that would overwrite one stops and is aborted.
func gitCheckSharedCheckout(execRoot string) error {
	if _, err := integrateGit(execRoot, "symbolic-ref", "-q", "HEAD"); err != nil {
		return errors.New("the shared checkout's HEAD is detached; check out the branch the work integrates onto")
	}
	for _, name := range []string{
		"CHERRY_PICK_HEAD", "MERGE_HEAD", "REVERT_HEAD", "sequencer", "rebase-merge", "rebase-apply",
	} {
		p, err := integrateGit(execRoot, "rev-parse", "--git-path", name)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(execRoot, p)
		}
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("the shared checkout has an operation in progress (%s); finish or abort it first", name)
		}
	}
	// A grafts file rewrites parentage for every read, and --no-replace-objects
	// does not turn it off (git still honors the deprecated file), so a chain
	// measured through it is not the chain the repository holds.
	if p, err := integrateGit(execRoot, "rev-parse", "--git-path", "info/grafts"); err == nil {
		if !filepath.IsAbs(p) {
			p = filepath.Join(execRoot, p)
		}
		if _, statErr := os.Stat(p); statErr == nil {
			return errors.New("the shared checkout has a grafts file (info/grafts), which rewrites commit parentage; remove it or convert it to replace refs before the engine picks")
		}
	}
	status, err := integrateGit(execRoot, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("the shared checkout has uncommitted changes to tracked files; commit or stash them first")
	}
	return nil
}

// gitCherryPickRange cherry-picks base..head onto the shared checkout's branch
// with -x, all or nothing: on any failure the pick is aborted and the checkout
// is verified to stand where it started. It never resets, amends or discards
// anything.
func gitCherryPickRange(execRoot, base, head string) (GitPick, error) {
	chain, err := gitIntegrationChain(execRoot, base, head)
	if err != nil {
		return GitPick{}, err
	}
	if err := gitCheckSharedCheckout(execRoot); err != nil {
		return GitPick{}, err
	}
	before, err := integrateGit(execRoot, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return GitPick{}, err
	}
	_, pickErr := integrateGit(execRoot, "cherry-pick", "-x", "--no-edit", chainBase(base, head)+".."+head)
	if pickErr != nil {
		_, _ = integrateGit(execRoot, "cherry-pick", "--abort")
		now, headErr := integrateGit(execRoot, "rev-parse", "--verify", "HEAD")
		_, inProgress := integrateGit(execRoot, "rev-parse", "-q", "--verify", "CHERRY_PICK_HEAD")
		dirty, statusErr := integrateGit(execRoot, "status", "--porcelain", "--untracked-files=no")
		if headErr != nil || now != before || inProgress == nil || statusErr != nil || dirty != "" {
			return GitPick{Before: before, Tip: now}, fmt.Errorf(
				"the pick failed and the checkout could not be verified as restored (HEAD was %.12s, is %.12s); "+
					"inspect it with `git status` before anything else: %w", before, now, pickErr)
		}
		return GitPick{}, fmt.Errorf("the pick did not apply and was aborted; the checkout's HEAD is unchanged at %.12s: %w", before, pickErr)
	}
	tip, err := integrateGit(execRoot, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return GitPick{Before: before, Tip: "(unreadable)"}, err
	}
	added, err := integrateGit(execRoot, "rev-list", "--reverse", before+".."+tip)
	if err != nil {
		return GitPick{Before: before, Tip: tip}, err
	}
	var picked []string
	if added != "" {
		picked = strings.Split(added, "\n")
	}
	if len(picked) != len(chain) {
		return GitPick{Before: before, Tip: tip, Picked: picked}, fmt.Errorf(
			"the pick added %d commit(s) for a chain of %d; HEAD moved %.12s -> %.12s and nothing was reset",
			len(picked), len(chain), before, tip)
	}
	return GitPick{Before: before, Tip: tip, Picked: picked}, nil
}
