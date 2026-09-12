package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	dexec "github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/trust"
)

// TRUST PROBE (DKT-1283) — run every trust-roster gate once against clean HEAD
// before the first dispatch of a run, so a gate that fails on clean HEAD is
// surfaced ONCE here instead of being rediscovered as a parked step per issue.
//
// This replaces dotfiles' src/user/claude_code/workflows/gate-probe.js: a
// scout agent read `docket trust list --json=v2`, spawned one isolated
// worktree agent per roster entry, and greped `action = "<name>"` out of the
// workflow TOMLs to skip engine actions because the roster carried no class
// marker (see action_names.go, DKT-1283 AC5). The engine already runs these
// gates at record time with the roster's own timeouts and stdin handling
// (gate_exec.go); this is the same execution path, run once against a
// throwaway checkout instead of once per step.
//
// NOTHING SHORT-CIRCUITS (AC1): every non-action roster entry runs, and one
// gate's failure never stops the rest — a gate that fails on clean HEAD was
// not caused by anything a run's steps changed, so the report is worth
// nothing unless it covers the whole roster.

// TrustProbeGate is one roster entry's probe result.
type TrustProbeGate struct {
	Name string `json:"name"`
	Stub bool   `json:"stub"`
	// Exit is nil when the entry was never run — an empty argv, or the probe
	// was interrupted before reaching it. A nil exit counts as a FAILURE, the
	// same rule gate-probe.js held: a vacuous pass must be impossible.
	Exit    *int   `json:"exit"`
	LogTail string `json:"log_tail"`
}

// TrustProbeSkip is one roster entry the probe never ran because it is a
// declared engine action (DKT-1283 AC2) — an engine ACTION is fed a JSON
// bundle on stdin at record time, so a stdin-less probe can only fail it.
type TrustProbeSkip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// TrustProbeResult is `docket trust probe`'s wire shape.
type TrustProbeResult struct {
	Head    string           `json:"head"`
	Gates   []TrustProbeGate `json:"gates"`
	Skipped []TrustProbeSkip `json:"skipped"`
	Failed  []string         `json:"failed"`
	Passed  bool             `json:"passed"`
}

// ErrEmptyTrustRoster means there was nothing to probe — every entry was
// filtered out (empty roster, or every entry a declared action). That is a
// finding, not a pass, and is refused rather than reported as an empty
// success (gate-probe.js's "throws when the roster is empty" rule).
var ErrEmptyTrustRoster = errors.New("the trust roster has nothing to probe")

// ProbeTrust runs every non-action entry of `entries` once, in one throwaway
// detached worktree of clean HEAD, and reports one row per gate.
//
// isAction classifies a roster entry BY NAME (DKT-1283 AC2/AC5) — built from
// ActionNames() by the caller so this function stays testable without a
// filesystem scan. repoRoot is the ORIGINAL checkout: it is the containment
// boundary exec.Resolve refuses a binary inside of and the value DOCKET_REPO
// carries, exactly as gate_exec.go's spawnMatched holds it fixed across a
// step's own worktree — only the working DIRECTORY moves to the throwaway
// checkout.
//
// THE WORKTREE IS REMOVED UNCONDITIONALLY (AC4): on a normal return, on any
// error, and when ctx is canceled — a caller that wires ctx to
// signal.NotifyContext gets removal on SIGINT/SIGTERM too, because a canceled
// context makes this function return rather than killing the process out from
// under its own defer.
func ProbeTrust(
	ctx context.Context, repoRoot string, entries []trust.Entry, isAction func(name string) bool,
) (*TrustProbeResult, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: the trust roster is empty", ErrEmptyTrustRoster)
	}

	head, err := probeHead(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving HEAD: %w", err)
	}

	var skipped []TrustProbeSkip
	var runnable []trust.Entry
	for _, e := range entries {
		if isAction != nil && isAction(e.Name) {
			skipped = append(skipped, TrustProbeSkip{
				Name: e.Name,
				Reason: fmt.Sprintf(
					"%q is declared as an engine action (action = %q in a workflow); "+
						"the engine feeds it a JSON bundle on stdin at record time, "+
						"so a stdin-less probe can only fail it", e.Name, e.Name),
			})
			continue
		}
		runnable = append(runnable, e)
	}
	if len(runnable) == 0 {
		names := make([]string, len(skipped))
		for i, s := range skipped {
			names[i] = s.Name
		}
		return nil, fmt.Errorf("%w: every entry is a declared engine action (%s)",
			ErrEmptyTrustRoster, strings.Join(names, ", "))
	}

	worktree, err := probeWorktree(repoRoot, head)
	if err != nil {
		return nil, fmt.Errorf("preparing a throwaway worktree of %s: %w", head, err)
	}
	defer worktree.release()

	result := &TrustProbeResult{Head: head, Skipped: skipped}
	for _, e := range runnable {
		if ctx.Err() != nil {
			break
		}
		result.Gates = append(result.Gates, runProbeGate(repoRoot, worktree.Dir, e))
	}

	for _, g := range result.Gates {
		if g.Exit == nil || *g.Exit != 0 {
			result.Failed = append(result.Failed, g.Name)
		}
	}
	result.Passed = len(result.Failed) == 0 && len(result.Gates) == len(runnable)
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

// runProbeGate runs one roster entry's own argv in the throwaway worktree,
// through the SAME spawn primitive gate_exec.go's spawnMatched uses, so a
// probed gate runs exactly as it would at record time: resolved argv, the
// allowlisted environment, the entry's own timeout and process-group kill.
func runProbeGate(repoRoot, worktreeDir string, e trust.Entry) TrustProbeGate {
	row := TrustProbeGate{Name: e.Name, Stub: e.Stub}
	if len(e.Argv) == 0 {
		row.LogTail = "roster entry has no argv — nothing to run"
		return row
	}

	timeout := dexec.DefaultTimeout
	if e.Timeout != "" {
		if d, err := time.ParseDuration(e.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	resolved, err := dexec.Resolve(e.Argv[0], repoRoot, e.Argv[0])
	if err != nil {
		row.LogTail = err.Error()
		return row
	}
	argv := append([]string{resolved}, e.Argv[1:]...)

	env, err := dexec.BuildEnv(dexec.EnvPolicy{Gate: e.Name, Repo: repoRoot, Network: e.Network})
	if err != nil {
		row.LogTail = err.Error()
		return row
	}

	res, err := dexec.Run(dexec.Spec{Argv: argv, Dir: worktreeDir, Env: env, Timeout: timeout})
	if err != nil {
		row.LogTail = err.Error()
		return row
	}

	exit := res.Exit
	row.Exit = &exit
	row.LogTail = tailLines(res.Output, 5)
	if res.TimedOut {
		if row.LogTail == "" {
			row.LogTail = res.Reason
		} else {
			row.LogTail += "\n" + res.Reason
		}
	}
	return row
}

// tailLines returns the last n non-empty-trimmed lines of s, the Go
// equivalent of `tail -n <n>` over a captured log.
func tailLines(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// probeHead resolves the repo's current HEAD sha.
func probeHead(repoRoot string) (string, error) {
	out, err := exec.Command("git", gitDirArgs(repoRoot, "rev-parse", "HEAD")...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// probeWorktreeHandle is the throwaway detached checkout ProbeTrust measures
// every gate in.
type probeWorktreeHandle struct {
	Dir    string
	parent string
}

// probeWorktree is probe's analog of pregate_scratch.go's reconstructTarget:
// a fresh, detached `git worktree add` of one sha, made to be thrown away.
// Unlike reconstructTarget it RETURNS AN ERROR on failure — a pre-gate
// falls back to `skipped` because a step's own tree not being reconstructible
// is an ordinary gap, but `trust probe` has nothing else to report and no
// step to park, so a checkout it cannot build is refused rather than silently
// reported as zero gates run.
func probeWorktree(repoRoot, sha string) (probeWorktreeHandle, error) {
	dir, err := os.MkdirTemp("", "docket-trust-probe-")
	if err != nil {
		return probeWorktreeHandle{}, err
	}
	// MkdirTemp creates the directory; `git worktree add` requires it not to.
	// Removing it and handing git the path keeps the name reserved for the
	// length of one syscall (pregate_scratch.go's same trick).
	if err := os.Remove(dir); err != nil {
		return probeWorktreeHandle{}, err
	}

	cmd := exec.Command("git", gitDirArgs(repoRoot, "worktree", "add", "--detach", dir, sha)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return probeWorktreeHandle{}, fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return probeWorktreeHandle{Dir: dir, parent: repoRoot}, nil
}

// release removes the worktree and its administrative record, in both
// directions and unconditionally (AC4) — `pregate_scratch.go`'s release does
// the identical two-step for the identical reason: `os.RemoveAll` alone
// leaves a stale `.git/worktrees` entry `git worktree list` reports forever,
// and `git worktree remove` alone can decline over a gate's stray output
// file, which `--force` covers.
func (h probeWorktreeHandle) release() {
	if h.Dir == "" {
		return
	}
	if h.parent != "" {
		_ = exec.Command("git", gitDirArgs(h.parent, "worktree", "remove", "--force", h.Dir)...).Run()
	}
	_ = os.RemoveAll(h.Dir)
}
