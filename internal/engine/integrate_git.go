package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
// engine's only git WRITE. It is gitCommandHooksOff (the same -c overrides and
// the same environment as every engine read) with two additions for a command
// that commits:
//
//   - rerere is off, and there is no editor and no terminal prompt, so nothing
//     waits on a person or resolves a conflict on its own;
//   - commit signing is off (commit.gpgsign=false): gpg.program and the
//     gpg.<format>.program keys live in the shared repository config an
//     executor in a linked worktree writes, so a signed pick would run a
//     program of the executor's choosing in the conductor's session. The pick
//     lands unsigned; the conductor may sign it afterwards.
//
// Clean/smudge filters and merge drivers come from the repository's and the
// user's git config and still run: those are residuals, as for gitCommand.
func integrateGit(execRoot string, args ...string) (string, error) {
	full := append([]string{
		"-c", "rerere.enabled=false", "-c", "commit.gpgsign=false", "-C", execRoot,
	}, args...)
	cmd := gitCommandHooksOff(configHookFlags(execRoot), full...)
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
//   - diff headers keep the `a/P b/P` shape the engine's diff parser expects
//     (core.quotePath, diff.noprefix and diff.mnemonicPrefix are pinned), so a
//     repository or user config cannot change which path a section names;
//   - the environment loses the variables that redirect git, name a program or
//     an external diff, or change how a pathspec is read (integrateGitEnv);
//     the -c overrides above, not the environment, are what keep an injected
//     config from running a program.
//
// Diff-producing callers add --no-ext-diff and --no-textconv themselves, as
// those are options of the subcommand. Clean/smudge filters still run for a
// command that reads a worktree, and a config hook that only an includeIf
// condition inside a new worktree selects still runs on `worktree add`; those
// are residuals, not closed doors.
func gitCommand(args ...string) *exec.Cmd {
	return gitCommandHooksOff(configHookFlags(gitDirOf(args)), args...)
}

// gitCommandHooksOff is gitCommand with the config-named hooks already listed
// (configHookFlags), for a caller that holds the flags for one directory.
func gitCommandHooksOff(hookFlags []string, args ...string) *exec.Cmd {
	full := append([]string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "submodule.recurse=false", "-c", "log.showSignature=false",
		"-c", "gpg.program=false", "-c", "protocol.allow=never",
		"-c", "core.quotePath=false", "-c", "diff.noprefix=false", "-c", "diff.mnemonicPrefix=false",
		"--no-replace-objects",
	}, args...)
	full = append(append([]string(nil), hookFlags...), full...)
	cmd := exec.Command(gitBinary(), full...)
	cmd.Env = integrateGitEnv()
	return cmd
}

// gitBinary is the git the engine runs, resolved through PATH once per
// process and then named by absolute path, so a PATH an executor changes
// between two commands cannot swap the binary under a sequence that already
// started. A git that PATH does not hold leaves the bare name, and the command
// fails as it would have anyway. Which PATH the process starts with is the
// executor's; that is a residual the constructors state rather than close.
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

// configEnvSupported reports, once per process, whether this git accepts the
// --config-env global option (git 2.31). A git without it also predates the
// hook.<name> keys, so there is nothing for the flags to disable there, and
// passing them would make every command exit 129 ("unknown option").
func configEnvSupported() bool {
	configEnvOnce.Do(func() {
		cmd := exec.Command(gitBinary(), "--config-env=docket.probe="+hookOffVar, "--version")
		cmd.Env = integrateGitEnv()
		configEnvOK = cmd.Run() == nil
	})
	return configEnvOK
}

var (
	configEnvOnce sync.Once
	configEnvOK   bool
)

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
// command will run in, NUL-delimited. A listing that fails yields no flags: a
// repository git cannot read fails the command itself. A git that predates
// --config-env (and with it the hook.<name> keys) gets no flags either, since
// it would reject the option rather than ignore the keys (configEnvSupported).
//
// The listing is memoized per directory for the lifetime of the process (one
// CLI verb, or one saga), so a sequence of commands in one directory pays for
// one `git config` rather than one per command. The listing and the command
// are therefore two reads some distance apart: a name added after the first
// command in a directory is not covered until the next process, and neither is
// a key that only an `includeIf` condition true inside a NEW worktree selects
// (`worktree add` runs its hooks in that worktree's context, which the listing
// cannot see). This is best-effort defence in depth, not a boundary: clean and
// smudge filters named by the same executor-writable config run on that
// checkout regardless, so whoever closes the filter residual closes this one
// with it.
func configHookFlags(dir string) []string {
	if !configEnvSupported() {
		return nil
	}
	if cached, ok := configHookFlagsCache.Load(dir); ok {
		return append([]string(nil), cached.([]string)...)
	}
	flags := listConfigHookFlags(dir)
	configHookFlagsCache.Store(dir, flags)
	return append([]string(nil), flags...)
}

// configHookFlagsCache holds configHookFlags' result keyed by the directory
// string as given; it is never invalidated within a process.
var configHookFlagsCache sync.Map

// listConfigHookFlags runs the `git config` listing behind configHookFlags.
func listConfigHookFlags(dir string) []string {
	args := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}
	if dir != "" {
		args = append(args, "-C", dir)
	}
	args = append(args, "config", "--null", "--name-only", "--get-regexp", `^hook\.(.*\.)?(command|event)$`)
	cmd := exec.Command(gitBinary(), args...)
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
// redirect git, name a program it would run (its own sub-commands through
// GIT_EXEC_PATH, an external diff, an askpass or transport helper), point it at
// another config file, or change how it reads a pathspec or attributes, plus
// the settings that keep it from waiting on a person.
//
// Scrubbing the environment does not by itself neutralize injected
// configuration: HOME and XDG_CONFIG_HOME stay, and the shared repository
// config is the executor's to write. The -c overrides in gitCommandHooksOff
// are what keep such a config from running a program; dropping the GIT_CONFIG*
// variables only removes one more place a config could come from.
func integrateGitEnv() []string {
	drop := []string{
		"GIT_EXTERNAL_DIFF=", "GIT_CONFIG=", "GIT_CONFIG_GLOBAL=", "GIT_CONFIG_SYSTEM=",
		"GIT_CONFIG_COUNT=", "GIT_CONFIG_PARAMETERS=",
		"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_", "GIT_DIFF_OPTS=", "GIT_ASKPASS=",
		"GIT_EXEC_PATH=", "GIT_TEMPLATE_DIR=",
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR=",
		"GIT_OBJECT_DIRECTORY=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=",
		"GIT_NAMESPACE=", "GIT_PREFIX=", "GIT_EDITOR=", "GIT_SEQUENCE_EDITOR=",
		"GIT_TERMINAL_PROMPT=", "GIT_REPLACE_REF_BASE=", "GIT_GRAFT_FILE=", "GIT_NO_REPLACE_OBJECTS=", "GIT_NO_LAZY_FETCH=",
		"GIT_SSH=", "GIT_SSH_COMMAND=", "GIT_PROXY_COMMAND=", "GIT_PAGER=", "GIT_ALLOW_PROTOCOL=",
		"GIT_LITERAL_PATHSPECS=", "GIT_GLOB_PATHSPECS=", "GIT_NOGLOB_PATHSPECS=", "GIT_ICASE_PATHSPECS=",
		"GIT_ATTR_SOURCE=",
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

// partialCloneNote is the clause a refusal adds when the shared checkout is a
// partial clone. Engine git never fetches (protocol.allow=never and
// GIT_NO_LAZY_FETCH=1, because the remote it would fetch from is named by a
// config the executor writes), so an object the clone never downloaded reads
// as missing, and the probe that needed it cannot answer. "" for a full clone.
func partialCloneNote(execRoot string) string {
	if execRoot == "" {
		return ""
	}
	out, err := gitCommand("-C", execRoot, "config", "--get", "extensions.partialClone").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return ""
	}
	return "; the shared checkout is a partial clone, and engine git does not fetch the objects it lacks: " +
		"fetch them first (`git backfill`, or remove the clone's filter and `git fetch --refetch`), then retry"
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
	// Each entry is a path under the git directory whose presence refuses the
	// pick, with the refusal it earns. The first six mark an operation in
	// progress. A grafts file rewrites parentage for every read, and
	// --no-replace-objects does not turn it off (git still honors the deprecated
	// file), so a chain measured through it is not the chain the repository
	// holds. A shallow file cuts parentage the same way, and an executor in a
	// linked worktree can write either into the shared repository.
	inProgress := func(name string) string {
		return fmt.Sprintf("the shared checkout has an operation in progress (%s); finish or abort it first", name)
	}
	parentage := func(name, fix string) string {
		return fmt.Sprintf("the shared checkout has a %s file, which rewrites commit parentage; %s before the engine picks", name, fix)
	}
	checks := []struct{ name, msg string }{
		{"CHERRY_PICK_HEAD", inProgress("CHERRY_PICK_HEAD")},
		{"MERGE_HEAD", inProgress("MERGE_HEAD")},
		{"REVERT_HEAD", inProgress("REVERT_HEAD")},
		{"sequencer", inProgress("sequencer")},
		{"rebase-merge", inProgress("rebase-merge")},
		{"rebase-apply", inProgress("rebase-apply")},
		{"info/grafts", parentage("info/grafts", "remove it or convert it to replace refs")},
		{"shallow", parentage("shallow", "deepen the clone (`git fetch --unshallow`) or remove a shallow file nobody fetched")},
	}
	// One rev-parse resolves every path: --git-path repeats, and the paths come
	// back one per line in argument order, in a main or a linked worktree.
	args := []string{"rev-parse"}
	for _, c := range checks {
		args = append(args, "--git-path", c.name)
	}
	out, err := integrateGit(execRoot, args...)
	if err != nil {
		return err
	}
	paths := strings.Split(out, "\n")
	if len(paths) != len(checks) {
		return fmt.Errorf("git rev-parse --git-path answered %d path(s) for %d names", len(paths), len(checks))
	}
	for i, c := range checks {
		p := strings.TrimSpace(paths[i])
		if !filepath.IsAbs(p) {
			p = filepath.Join(execRoot, p)
		}
		if _, statErr := os.Stat(p); statErr == nil {
			return errors.New(c.msg)
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
//
// A chain commit that was already carried by hand becomes empty when picked;
// git would stop there and ask for --skip, so the pick runs with
// --keep-redundant-commits and such a commit lands as an empty commit carrying
// its -x trailer. The chain's length is then still the number of commits the
// pick adds, which the check below relies on.
func gitCherryPickRange(execRoot, base, head string) (GitPick, error) {
	// The chain check runs gitCheckSharedCheckout first.
	chain, err := gitIntegrationChain(execRoot, base, head)
	if err != nil {
		return GitPick{}, err
	}
	before, err := integrateGit(execRoot, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return GitPick{}, err
	}
	_, pickErr := integrateGit(execRoot, "cherry-pick", "-x", "--no-edit", "--keep-redundant-commits", chainBase(base, head)+".."+head)
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
		return GitPick{}, fmt.Errorf("the pick did not apply and was aborted; the checkout's HEAD is unchanged at %.12s "+
			"(a chain whose commits were partly carried by hand conflicts where the hand-made copy differs from the writer's; "+
			"integrate the rest by hand and annotate the result): %w", before, pickErr)
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
