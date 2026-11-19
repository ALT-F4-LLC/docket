package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// Warning is the trust lookup's warning for an entry that still ran, such
	// as an absolute argv[0] with no content pin. It is kept apart from
	// LogTail so the gate's own output is never mixed with engine text.
	Warning string `json:"warning,omitempty"`
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
// checkout of clean HEAD (probeWorktree), and reports one row per gate.
//
// isAction classifies a roster entry BY NAME (DKT-1283 AC2/AC5) — built from
// ActionNames() by the caller so this function stays testable without a
// filesystem scan. repoRoot is the ORIGINAL checkout: it is the containment
// boundary exec.Resolve refuses a binary inside of and the value DOCKET_REPO
// carries, exactly as gate_exec.go's spawnMatched holds it fixed across a
// step's own worktree — only the working DIRECTORY moves to the throwaway
// checkout.
//
// THE CHECKOUT IS REMOVED UNCONDITIONALLY (AC4): on a normal return, on any
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
		return nil, fmt.Errorf("preparing a throwaway checkout of %s: %w", head, err)
	}
	defer worktree.release()

	result := &TrustProbeResult{Head: head, Skipped: skipped}
	for _, e := range runnable {
		if ctx.Err() != nil {
			break
		}
		result.Gates = append(result.Gates, runProbeGate(repoRoot, worktree, head, e))
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

// runProbeGate runs one roster entry's own argv in the throwaway checkout,
// through the SAME spawn primitive gate_exec.go's spawnMatched uses, so a
// probed gate runs exactly as it would at record time: resolved argv, the
// allowlisted environment, the entry's own timeout and process-group kill.
//
// `base` is the probed HEAD, exported as DOCKET_GATE_BASE. Every gate that
// selects a committed footprint refuses without it, so a probe that left it
// unset reported eight healthy base-diff gates as failed on clean HEAD and
// every conductor re-derived the same fact by hand before a run. The value is
// deliberately NOT gateBaseSHA's: that resolver leaves the var unset for a
// shared-checkout step because a live HEAD is not a range endpoint docket can
// vouch for as THAT STEP's base. The probe is the one case where HEAD is
// exactly right — the throwaway checkout sits at the probed sha, nothing has
// been committed on top of it, and an empty footprint is the answer a healthy
// gate should give. A later refactor must not unify the two.
//
// The linter caches go to the checkout's own cache root for the reason
// exec.EnvPolicy.CacheRoot documents: the checkout is deleted when the probe
// returns, so a cache entry naming its paths must be deleted with it.
func runProbeGate(repoRoot string, worktree probeWorktreeHandle, base string, e trust.Entry) TrustProbeGate {
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

	env, err := dexec.BuildEnv(dexec.EnvPolicy{
		Gate: e.Name, Repo: repoRoot, Network: e.Network, Base: base,
		CacheRoot: worktree.Cache,
	})
	if err != nil {
		row.LogTail = err.Error()
		return row
	}

	match := lookupProbeEntry(e, worktree.Dir)
	if !match.Matched {
		row.LogTail = match.Reason
		return row
	}
	row.Warning = match.Warning

	res, err := spawnProbeGate(match, repoRoot,
		dexec.Spec{Argv: match.Argv, Dir: worktree.Dir, Env: env, Timeout: timeout})
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

// spawnProbeGate runs spec, whose argv[0] is still unresolved. A pinned entry
// spawns through exec.RunPinned, which resolves argv[0] once, hashes that
// path, and executes the same path string, so a symlink on the way to the
// executable that is retargeted after Lookup's check cannot land the spawn on
// unpinned bytes. RunPinned binds a path, not an inode: a rename over the
// resolved file, or a directory component swapped for a symlink, between its
// hash and execve can still run other bytes. An entry with no pin has nothing
// to bind and runs through Resolve and Run.
func spawnProbeGate(match trust.Match, repoRoot string, spec dexec.Spec) (dexec.Result, error) {
	if pin := match.Entry.Argv0SHA256; pin != "" {
		return dexec.RunPinned(spec, pin, repoRoot, match.Entry.Argv[0])
	}
	resolved, err := dexec.Resolve(spec.Argv[0], repoRoot, match.Entry.Argv[0])
	if err != nil {
		return dexec.Result{}, err
	}
	spec.Argv = append([]string{resolved}, spec.Argv[1:]...)
	return dexec.Run(spec)
}

// lookupProbeEntry holds one roster entry to the same match rules the
// record-time gate runs under. A refusal comes back unmatched with its reason;
// a match carries the argv that must execute. The argv[0] pin that authorizes
// the spawn is checked again by spawnProbeGate.
//
// root is the directory the gate will spawn in: the throwaway worktree, never
// the shared checkout. LookupAt re-hashes every declared file of the entry
// under root, so an entry that declares files is verified against the bytes
// the gate will read, and Lookup, which refuses such an entry for want of a
// root, is not used.
//
// The snapshot holds only this entry, so LookupAt cannot select a different
// entry of the same name. The entry is marked global because the caller
// already filtered the roster by repo binding; repeating that check here
// would need an identity the probe does not hold.
func lookupProbeEntry(e trust.Entry, root string) trust.Match {
	e.Global = true
	snapshot := &trust.Store{Entries: []trust.Entry{e}}
	return snapshot.LookupAt("", e.Name, nil, root)
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
	out, err := gitCommand(gitDirArgs(repoRoot, "rev-parse", "HEAD")...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// probeWorktreeHandle is the throwaway checkout ProbeTrust measures every
// gate in.
type probeWorktreeHandle struct {
	Dir string
	// Cache is the linter cache root that lives exactly as long as Dir. It is
	// a sibling of the tree, never inside it, so the cache stays out of the
	// tree the gates measure (scratchTree.Cache holds the same rule).
	Cache string
	// tree owns the sidecar's flock. It is kept here, not copied out, so the
	// lock file stays reachable for as long as the handle is: a dropped
	// *os.File is closed by its finalizer, and a sweep that then takes the
	// flock would delete a tree the probe is still measuring.
	tree scratchTree
}

// probeWorktree checks sha out into a throwaway repository of its own that
// borrows repoRoot's objects, the checkout pregate_scratch.go makes for
// pre-gates (see that file's comment for the layout and what the tree does
// not share). Nothing is written to the repository's git directory, so the
// probe runs where the repository's worktrees/ directory is read-only.
//
// The steps and their failure paths are reconstructTarget's, in its order:
// name reserved, sidecar locked and recorded, git directory and cache made,
// then the checkout. The tree takes a scratchPrefix name and a sidecar on
// purpose: sweepStalePreGateScratch finds a tree left by a killed probe and
// removes it once the flock is free, and leaves a tree the probe still holds.
// A drift from reconstructTarget's order breaks the sweep's ownership proof.
//
// sha must be a full commit id; probeHead's is. Unlike reconstructTarget this
// RETURNS AN ERROR naming the failed stage: a pre-gate falls back to
// `skipped`, but `trust probe` has nothing else to report and no step to
// park, so a checkout it cannot build is refused rather than reported as zero
// gates run.
//
// WHO CAN WRITE THE TREE'S GIT CONFIG. Every roster argv runs in this tree,
// and the tree's git directory (config, info/attributes, HEAD, index) sits in
// the temp directory, mode 0700. A process of this user that can write the
// temp directory can plant a filter there that the gate commands' own git
// calls run while the probe stands. After the checkout the probe runs no git
// command in the tree; the checkout's own `read-tree` is the one engine git
// call such a filter can reach. The operator accepted this exposure for
// pre-gate and gate-resume trees (DKT-4626) and extended it to the probe
// (DKT-4627). The tree's files sat in the temp directory before this change
// too, and every gate runs scripts from them.
func probeWorktree(repoRoot, sha string) (probeWorktreeHandle, error) {
	source, ok := scratchSourceOf(repoRoot)
	if !ok {
		return probeWorktreeHandle{}, errors.New("reading the repository's object store and object format")
	}
	dir, err := os.MkdirTemp("", scratchPrefix)
	if err != nil {
		return probeWorktreeHandle{}, fmt.Errorf("reserving the tree's name: %w", err)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	lock, err := holdScratchLock(dir, source.objects)
	if err != nil {
		_ = os.Remove(dir)
		return probeWorktreeHandle{}, fmt.Errorf("taking the tree's sidecar lock: %w", err)
	}
	if !reserveScratchSiblings(dir) {
		_ = os.Remove(dir)
		_ = os.Remove(scratchLockPath(dir))
		lock.Close()
		return probeWorktreeHandle{}, fmt.Errorf("making the git directory and cache root beside %s", dir)
	}
	tree := scratchTree{Dir: dir, Cache: scratchCachePath(dir), lock: lock}
	if !checkoutScratch(dir, source, sha) {
		tree.release()
		return probeWorktreeHandle{}, fmt.Errorf("checking the commit out into %s", dir)
	}
	return probeWorktreeHandle{Dir: dir, Cache: tree.Cache, tree: tree}, nil
}

// release removes the tree, its git directory and its cache root, then the
// sidecar, unconditionally (AC4), through scratchTree.release. A part that
// will not delete keeps its sidecar, and the pre-gate sweep retries it.
func (h probeWorktreeHandle) release() {
	h.tree.release()
}
