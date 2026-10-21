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
//   - repository hooks and the fsmonitor hook are off (core.hooksPath and
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
		"-c", "rerere.enabled=false", "-C", execRoot,
	}, args...)
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

// integrateGitEnv is the process environment without the variables that
// redirect git, plus the settings that keep it from waiting on a person.
func integrateGitEnv() []string {
	drop := []string{
		"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR=",
		"GIT_OBJECT_DIRECTORY=", "GIT_ALTERNATE_OBJECT_DIRECTORIES=",
		"GIT_NAMESPACE=", "GIT_PREFIX=", "GIT_EDITOR=", "GIT_SEQUENCE_EDITOR=",
		"GIT_TERMINAL_PROMPT=",
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
	return append(env, "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true", "GIT_TERMINAL_PROMPT=0")
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
		if headErr != nil || now != before || inProgress == nil {
			return GitPick{}, fmt.Errorf(
				"the pick failed and the checkout could not be verified as restored (HEAD was %.12s, is %.12s); "+
					"inspect it with `git status` before anything else: %w", before, now, pickErr)
		}
		return GitPick{}, fmt.Errorf("the pick did not apply and was aborted; the checkout's HEAD is unchanged at %.12s: %w", before, pickErr)
	}
	tip, err := integrateGit(execRoot, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return GitPick{}, err
	}
	added, err := integrateGit(execRoot, "rev-list", "--reverse", before+".."+tip)
	if err != nil {
		return GitPick{}, err
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
