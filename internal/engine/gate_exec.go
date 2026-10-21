package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The real gate runner.
//
// PassThroughRunner returns a stubbed pass without touching the process table.
// This is the implementation that runs things, and the saga is unchanged
// around it: the promise that swapping runners changes one constructor call
// holds for the saga. Four other places did move: result persistence, the
// verdict reader, the claim restructure, and vote steps.
//
// EVERYTHING SECURITY-RELEVANT HERE IS A CALL INTO GROUP 1. The matcher is
// internal/trust's, the spawn is internal/exec's, the env allowlist and the
// containment check are theirs too. This file decides WHEN to call them and
// WHAT TO RECORD; it re-implements none of them, which is what keeps the
// pre-gate path provably identical to the saga's.

// ExecRunner is the GateRunner that actually executes.
type ExecRunner struct {
	// RepoRoot is the working-tree root — the cwd every gate runs in — and
	// the containment boundary.
	RepoRoot string
	// Identity is the project identity trust entries bind to. It
	// equals RepoRoot for env/local stores; under the global store it is the
	// worktree-stable project path, which is what lets two checkouts of one
	// repository match the same entries.
	Identity string
	// LockPath is the tree lockfile serializing this project's tree gates.
	LockPath string
	// LoadStore reads the trust store. It is a FIELD so a test can supply a
	// sandbox store without the package growing a path-taking constructor —
	// one is forbidden, because every additional way to point docket at a
	// trust file is another way for repo content to point it somewhere.
	LoadStore func() (*trust.Store, error)
	// NowMS stamps results. Injected so a test is not at the mercy of a clock.
	NowMS func() int64
}

// NewExecRunner builds the real runner. THIS IS THE PROMISED CONSTRUCTOR
// SWAP: NewEngine names it instead of PassThroughRunner, and the saga does
// not change around it.
func NewExecRunner(paths RepoPaths) *ExecRunner {
	return &ExecRunner{
		RepoRoot:  paths.ExecRoot,
		Identity:  paths.Identity,
		LockPath:  paths.LockPath,
		LoadStore: trust.Load,
		NowMS:     func() int64 { return time.Now().UnixMilli() },
	}
}

var _ GateRunner = (*ExecRunner)(nil)

// GateExecution is what one gate produced: one or more attempt records, since a
// fence gate matches PER LINE and a flaky command records each attempt
// individually.
type GateExecution struct {
	// Results are the rows to record, in execution order.
	Results []GateResultRow
	// Verdict is the gate's routing verdict, conjunctive over Results.
	Verdict string
}

// GateResultRow is one result this runner produced, in the `gate result` shape
// plus `reason` and the `pre` marker.
//
// Argv and Exit are POINTERS because an unmatched gate never ran: NULL is the
// honest encoding of "no process existed", and a zero exit on a gate that did
// not execute is the exact confusion this encoding exists to prevent.
type GateResultRow struct {
	Gate       string
	Ordinal    int
	Argv       []string
	Exit       *int
	DurationMS int64
	Output     string
	Truncated  bool
	Verdict    string
	Reason     string
	// TrustEntry names the entry that authorized this, for the audit record.
	TrustEntry string
	// ArgvSHA256 is the canonical hash of the candidate argv, for trust_cache.
	ArgvSHA256 string
	// Prefix records that a prefix entry authorized it.
	Prefix bool
	// Pre marks a pre-gate result: an input to the step rather than a
	// judgment of it, and excluded from the saga's verdict.
	Pre bool
	// Stub is 1 ONLY for results the pass-through produced. NOTHING THIS
	// RUNNER PRODUCES SETS IT — the field survives so a migrated pass-through
	// row stays distinguishable forever, and so the fakes tests build on the
	// old seam keep recording honestly.
	Stub bool
	// StubEntry carries the matched entry's own `stub` declaration (DKT-265):
	// the command that ran was a placeholder, not the check its name implies.
	//
	// It is set on EVERY row that names a TrustEntry, including the ones whose
	// verdict is unmatched, skipped, or fail. The field describes the ENTRY,
	// not the outcome, and a reader diffing a gate's rows should not have to
	// wonder whether its absence on a failing row means "not a stub" or "we
	// only bother recording this when it passes".
	//
	// Distinct from Stub above, which is about which era of this codebase
	// produced the row. A row can be either, both, or neither.
	StubEntry bool
	// TargetSHA keys a PRE-gate row measured by a detached run to the commit
	// it measured, so a claim reuses it for that target only. The
	// runner never sets it: the detached phase stamps its rows after the
	// measurement, exactly as the claim path stamps Pre. Empty everywhere
	// else.
	TargetSHA string
}

// Run executes one gate: match, then spawn only what matched.
//
// The ORDER is the security property. Matching happens against an immutable
// store snapshot read ONCE, and the matched entry's OWN ARGV is what executes
// — matching does not produce a permission that is later applied to an argv
// read from somewhere else, so there is no TOCTOU window.
func (r *ExecRunner) Run(ctx context.Context, g GateSpec, sc StepContext) (GateResult, error) {
	ex, err := r.Execute(ctx, g, sc)
	if err != nil {
		return GateResult{}, err
	}
	// The seam's single-result shape is preserved for the saga, which consumes
	// a verdict. The full per-attempt rows ride in Execute's return and are
	// what the saga records.
	out := GateResult{Gate: g.Name, Verdict: ex.Verdict}
	if len(ex.Results) > 0 {
		last := ex.Results[len(ex.Results)-1]
		out.Argv = last.Argv
		if last.Exit != nil {
			out.Exit = *last.Exit
		}
		out.DurationMS = last.DurationMS
		out.Output = last.Output
		out.Truncated = last.Truncated
	}
	return out, nil
}

// Execute is Run's full-fidelity form: it returns every row to record rather
// than collapsing to one.
func (r *ExecRunner) Execute(_ context.Context, g GateSpec, sc StepContext) (GateExecution, error) {
	// ONE read, into an immutable snapshot. Every match for this gate runs
	// against these bytes. A concurrent `trust rm` or a trust-file rewrite
	// cannot change what this gate executes, because there is no second read.
	store, err := r.LoadStore()
	if err != nil {
		// A store that cannot be read is not an empty allowlist — an empty
		// allowlist is a MISSING file, which is a different fact. A
		// malformed or unsafe store fails closed with the reason.
		return r.unmatchedGate(g, fmt.Sprintf("the trust store could not be read: %v", err)), nil
	}

	identity, err := trust.RepoIdentity(r.Identity)
	if err != nil {
		return r.unmatchedGate(g, fmt.Sprintf("the repo path could not be resolved: %v", err)), nil
	}

	dir := r.spawnDir(sc)
	if g.Source == "" {
		return r.runNamedGate(g, sc, dir, store, identity)
	}
	return r.runFenceGate(g, sc, dir, store, identity)
}

// spawnDir is the directory a gate spawns in, and therefore the root its
// entry's declared files are hashed under: the step's worktree when one is
// known, otherwise the shared checkout. Never the trust identity, which names
// the project rather than the tree the gate reads.
func (r *ExecRunner) spawnDir(sc StepContext) string {
	if sc.WorkRoot != "" {
		return sc.WorkRoot
	}
	return r.root(sc)
}

// root is the checkout a gate's binary is contained against and, with no
// worktree in hand, the cwd it runs in (DKT-4250).
//
// A saga's gates take the RUN's recorded exec root (StepContext.PinRoot), not
// the runner's own. The runner's root is the invoking cwd's repository, and a
// writer records from inside its isolated worktree, so a detached gate run
// launched there inherited THAT worktree as its root. The workflow removes the
// worktree once the writer returns, and the gate then refused with a repo path
// that no longer resolved. The run's exec root was recorded by the operator's
// activation, outlives every executor worktree, and is not a tree an executor
// controls, so it is also the safer containment boundary: a binary an
// executor wrote into its own worktree is not "inside the repository" by the
// invoker's reckoning but is by the run's. A caller with no run in hand
// (PinRoot empty) keeps the runner's root exactly as before.
func (r *ExecRunner) root(sc StepContext) string {
	if sc.PinRoot != "" {
		return sc.PinRoot
	}
	return r.RepoRoot
}

// runNamedGate runs a gate whose command is the trust entry's own argv.
//
// The candidate argv is nil: for a named gate the ENTRY IS the command, so the
// match is by name and binding, and the stored hash is verified against the
// stored argv to catch a corrupted or hand-edited file.
func (r *ExecRunner) runNamedGate(
	g GateSpec, sc StepContext, dir string, store *trust.Store, identity string,
) (GateExecution, error) {
	match := store.LookupAt(identity, g.Name, nil, dir)
	if !match.Matched {
		return r.unmatchedGate(g, match.Reason), nil
	}
	return r.spawnMatched(g, sc, dir, match, 0, func() trust.Match {
		return store.LookupAt(identity, g.Name, nil, dir)
	})
}

// runFenceGate runs the harvested fence lines, EACH MATCHED INDEPENDENTLY.
//
// Per-line independence is the anti-pattern's opposite: an implementation
// that matched the block as a unit, or that ran the matched lines and silently
// skipped the rest, would let an attacker append a line to a body that a prefix
// entry happens to cover. Per-line matching with per-line results makes each
// line its own decision with its own record.
func (r *ExecRunner) runFenceGate(
	g GateSpec, sc StepContext, dir string, store *trust.Store, identity string,
) (GateExecution, error) {
	if len(g.Commands) == 0 {
		return r.unmatchedGate(g,
			fmt.Sprintf("gate %q harvested no commands from %s", g.Name, g.Source)), nil
	}

	var out GateExecution
	out.Verdict = VerdictPass
	ordinal := 0

	// fail is the three near-identical "unmatched" outcomes below, factored to
	// ONE recording site: append the row at the current ordinal, advance the
	// ordinal, and mark the gate FAILED for routing ("we couldn't check, so
	// carry on" is what makes a control decorative). `argvSHA` is "" for the two
	// cases that never reached a parsed argv to hash (the hash-mismatch and the
	// split-failure branches).
	fail := func(reason, argvSHA string) {
		out.Results = append(out.Results, GateResultRow{
			Gate: g.Name, Ordinal: ordinal, Verdict: VerdictUnmatched,
			Reason:     reason,
			ArgvSHA256: argvSHA,
		})
		ordinal++
		out.Verdict = VerdictFail
	}

	for i, command := range g.Commands {
		// Re-verify the stored hash against the stored command BEFORE
		// spawning. The activation snapshot closes "the body cannot inject";
		// this closes "the row cannot be swapped". A database write is outside
		// the trust boundary, but it costs one hash to detect anyway.
		if i < len(g.CommandHashes) && g.CommandHashes[i] != "" {
			if workflow.SHA256([]byte(command)) != g.CommandHashes[i] {
				fail(fmt.Sprintf(
					"the stored command %s does not match its recorded hash; "+
						"the record was altered after activation and will not be run",
					exec.Render(command)), "")
				continue
			}
		}

		// Tokenized ONCE, at execution, by a splitter that performs NO
		// expansion of any kind. A `$` is a literal dollar sign here.
		argv, err := exec.Split(command)
		if err != nil {
			fail(fmt.Sprintf("the command %s could not be read as an argv: %v",
				exec.Render(command), err), "")
			continue
		}

		match := store.LookupAt(identity, g.Name, argv, dir)
		if !match.Matched {
			fail(match.Reason, trust.ArgvSHA256(argv))
			continue
		}

		ex, err := r.spawnMatched(g, sc, dir, match, ordinal, func() trust.Match {
			return store.LookupAt(identity, g.Name, argv, dir)
		})
		if err != nil {
			return GateExecution{}, err
		}
		out.Results = append(out.Results, ex.Results...)
		ordinal += len(ex.Results)
		if ex.Verdict == VerdictFail {
			out.Verdict = VerdictFail
		}
	}

	return out, nil
}

// spawnMatched runs a matched command and appends the match's warning to the
// reason of every row it records, so each attempt and each early refusal of a
// gate trusted without a content pin says so where its result is read.
func (r *ExecRunner) spawnMatched(
	g GateSpec, sc StepContext, dir string, match trust.Match, firstOrdinal int,
	relookup func() trust.Match,
) (GateExecution, error) {
	ex, err := r.spawnMatchedRows(g, sc, dir, match, firstOrdinal, relookup)
	if err != nil || match.Warning == "" {
		return ex, err
	}
	for i := range ex.Results {
		ex.Results[i].Reason = withScratchNote(ex.Results[i].Reason, match.Warning)
	}
	return ex, nil
}

// pinStillBindsSpawn refuses a pinned entry whose argv[0] no longer resolves
// to spawned, the path the runner will execute. The content pin hashes the
// file argv[0] resolves to now, while the spawn runs the path resolved
// earlier, so a symlinked component retargeted in between would let the pin
// hash one file while another runs. Pins exist only on absolute argv[0],
// which Resolve's absolute-path exception normalized to spawned. It runs before every attempt,
// ahead of the pin's own re-lookup.
func pinStillBindsSpawn(entry *trust.Entry, spawned string) error {
	argv0 := ""
	if len(entry.Argv) > 0 {
		argv0 = entry.Argv[0]
	}
	if entry.Argv0SHA256 == "" || !filepath.IsAbs(argv0) {
		return nil
	}
	now, err := exec.NormalizePath(argv0)
	if err != nil {
		return fmt.Errorf("cannot re-resolve %s before spawning %s: %v",
			exec.Render(argv0), exec.Render(spawned), err)
	}
	if now != spawned {
		return fmt.Errorf("%s now resolves to %s, not to %s, the file that would spawn; "+
			"its content pin would be checked against a file that does not run",
			exec.Render(argv0), exec.Render(now), exec.Render(spawned))
	}
	return nil
}

// spawnMatchedRows runs a matched command, taking the tree mutex when the entry
// declares `tree = true` and recording every flaky attempt individually.
//
// dir is the spawn directory from spawnDir, the same root match's declared
// files were hashed under.
//
// relookup repeats the LookupAt that produced match against the same store
// snapshot and dir. It runs before every attempt, after any tree-lock wait,
// because the content pins (argv0_sha256 and the entry's declared files)
// are checked "before every spawn": a pinned file can change during the lock
// wait or between flaky attempts, and only the pin half of the lookup can
// change its answer. That narrows the window between the hash and the
// child's own read of the file; it does not close it. The pin half
// re-resolves the entry's argv[0] afresh, so pinStillBindsSpawn runs first.
func (r *ExecRunner) spawnMatchedRows(
	g GateSpec, sc StepContext, dir string, match trust.Match, firstOrdinal int,
	relookup func() trust.Match,
) (GateExecution, error) {
	entry := match.Entry
	timeout := exec.DefaultTimeout
	if entry.Timeout != "" {
		if d, err := time.ParseDuration(entry.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	// THE CLAIM'S PRE-GATE BUDGET. On the pre-claim path the
	// whole phase is bounded so the claim returns inside the executor's tool
	// timeout: this gate gets what remains of the budget, or nothing. A
	// budget already spent records `skipped` — nothing spawned, nothing
	// measured, the same shape as a lock that never came free — and the
	// claim still succeeds, because a pre-gate never blocks work.
	entryTimeout := timeout
	if !sc.Deadline.IsZero() {
		remaining := time.Until(sc.Deadline)
		if remaining <= 0 {
			return GateExecution{
				Verdict: VerdictFail,
				Results: []GateResultRow{{
					Gate: g.Name, Ordinal: firstOrdinal, Verdict: VerdictSkipped,
					Argv: match.Argv,
					Reason: fmt.Sprintf(
						"the claim's %s pre-gate budget was spent before this gate "+
							"could run; nothing was measured", claimPreGateBudget),
					TrustEntry: entry.Name,
					StubEntry:  entry.Stub,
					ArgvSHA256: trust.ArgvSHA256(match.Argv),
					Prefix:     entry.Prefix,
				}},
			}, nil
		}
		if remaining < timeout {
			timeout = remaining
		}
	}

	// The MATCHED entry's own declaration is what reaches the child,
	// the same discipline the store snapshot applies to argv: the entry that authorized
	// the command is the entry that configures it, so there is no window in
	// which one entry's permission is applied to another's requirements.
	env, err := exec.BuildEnv(exec.EnvPolicy{
		Gate: g.Name, Repo: r.root(sc), Network: entry.Network,
		Issue: model.FormatID(sc.IssueID), Scope: sc.Scope,
		// DKT-1186: the step's own reference, so the gate can ask the engine
		// for its inputs (`docket step context $DOCKET_STEP`) instead of
		// rediscovering which step it is from the issue plus a convention.
		// Rendered here and nowhere else, and only when a step is actually in
		// hand — a zero id would render `STEP-0`, which names no row.
		Step:      stepRef(sc.StepID),
		Base:      sc.Base,
		CacheRoot: sc.CacheRoot,
	})
	if err != nil {
		return GateExecution{}, err
	}

	// The containment check is unconditional and lives in
	// internal/exec, so no path skips it — named gates, fence gates, and
	// pre-gates all reach it through Resolve.
	entryArgv0 := ""
	if len(entry.Argv) > 0 {
		entryArgv0 = entry.Argv[0]
	}
	resolved, err := exec.Resolve(match.Argv[0], r.root(sc), entryArgv0)
	if err != nil {
		// A refusal means NOTHING RAN: verdict is unmatched and the process
		// table was never touched.
		return GateExecution{
			Verdict: VerdictFail,
			Results: []GateResultRow{{
				Gate: g.Name, Ordinal: firstOrdinal, Verdict: VerdictUnmatched,
				Reason:     err.Error(),
				TrustEntry: entry.Name,
				StubEntry:  entry.Stub,
				ArgvSHA256: trust.ArgvSHA256(match.Argv),
				Prefix:     entry.Prefix,
			}},
		}, nil
	}

	// A content-pinned entry spawns through exec.RunPinned, which resolves the
	// unresolved argv[0] once and executes the path it hashed, so a symlink
	// retargeted after `resolved` above can make it refuse but cannot make it
	// run unpinned bytes. A rename over the hashed file, a write into it, or an
	// interpreter re-opening it by name after the hash is not bound (DKT-3919).
	// A flaky pinned entry's re-attempts spawn through RunPinned too.
	pinnedSpawn := entry.Argv0SHA256 != "" && filepath.IsAbs(entryArgv0)
	argv := append([]string{resolved}, match.Argv[1:]...)
	if pinnedSpawn {
		argv = match.Argv
	}

	// DKT-9: the gate runs in the step's worktree when one is known, mirroring
	// the diff stage's resolution (saga.go). RepoRoot stays the containment
	// boundary for the BINARY above — where a command may live and where it
	// runs are separate facts — but the tree it measures must be the tree the
	// step reported, or the recorded evidence describes a HEAD the step never
	// touched. A worktree that is already gone (they are swept at integration)
	// records an honest refusal rather than silently measuring the shared
	// checkout — and rather than an engine error, which on the pre-claim path
	// would block a claim that pre-gates must never block. The ROW's verdict is
	// `skipped`, not `fail` (DKT-169): nothing ran, so "measured and failed"
	// would be a false record; the execution verdict stays fail so routing
	// remains fail-closed.
	if sc.WorkRoot != "" {
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			return GateExecution{
				Verdict: VerdictFail,
				Results: []GateResultRow{{
					Gate: g.Name, Ordinal: firstOrdinal, Verdict: VerdictSkipped,
					Argv: match.Argv,
					Reason: fmt.Sprintf(
						"the step's worktree %s no longer exists; "+
							"not running the gate in the shared checkout in its place",
						sc.WorkRoot),
					TrustEntry: entry.Name,
					StubEntry:  entry.Stub,
					ArgvSHA256: trust.ArgvSHA256(match.Argv),
					Prefix:     entry.Prefix,
				}},
			}, nil
		}
	}
	// The deadline rides on the spec as well as clamping the timeout above:
	// a flaky entry re-runs inside exec.RunSpawnedAttempts, and each attempt must
	// see what is left of the budget, not a fresh copy of the first clamp.
	spec := exec.Spec{Argv: argv, Dir: dir, Env: env, Timeout: timeout, Deadline: sc.Deadline}

	// The lock is acquired IMMEDIATELY BEFORE the spawn and released
	// IMMEDIATELY AFTER, outside every transaction and never held across a
	// database write.
	//
	// ONLY A GATE ON THE SHARED CHECKOUT TAKES IT. The mutex exists so
	// two builds cannot race one tree; a gate measuring a step's own worktree
	// or a pre-gate's scratch reconstruction has that tree to itself, and
	// serializing it against every other worktree's gate on one per-project
	// lock is what queued a wave's records behind each other until the
	// 5m bound expired and parked correct work as unmeasured. Two gates on
	// the SAME isolated worktree are not serialized either; that tree has one
	// step behind it, and the trade is accepted.
	if entry.Tree && sharesCheckout(dir, r.root(sc)) {
		lock, lockErr := acquireTreeLock(r.LockPath, timeout)
		if lockErr != nil {
			// The serialization the gate requires cannot be provided, so
			// it does not run unserialized. Nothing spawned and no tree was
			// read, which is the vanished-worktree fact above wearing a
			// different cause, so it takes the same shape (DKT-91): the ROW is
			// `skipped` and routes as a gate that measured nothing, while the
			// execution verdict stays fail so routing remains fail-closed.
			return GateExecution{
				Verdict: VerdictFail,
				Results: []GateResultRow{{
					Gate: g.Name, Ordinal: firstOrdinal, Verdict: VerdictSkipped,
					Argv: match.Argv, Reason: lockErr.Error(),
					TrustEntry: entry.Name,
					StubEntry:  entry.Stub,
					ArgvSHA256: trust.ArgvSHA256(match.Argv),
					Prefix:     entry.Prefix,
				}},
			}, nil
		}
		defer lock.release()
	}

	// The gate-script check joins the pin recheck so it runs before every
	// attempt, after any lock wait, against the tree the attempt will open.
	var scriptNotes []string
	recheckPin := func() error {
		if err := pinStillBindsSpawn(entry, resolved); err != nil {
			return fmt.Errorf("gate %q: %w", g.Name, err)
		}
		if again := relookup(); !again.Matched {
			return errors.New(again.Reason)
		}
		notes, err := checkGateScriptCopies(match.Argv, dir, sc)
		scriptNotes = notes
		return err
	}
	var (
		attempts []exec.Attempt
		refusal  *exec.Refusal
	)
	if pinnedSpawn {
		runPinned := func(s exec.Spec) (exec.Result, error) {
			return exec.RunPinned(s, entry.Argv0SHA256, r.root(sc), entryArgv0)
		}
		attempts, refusal, err = exec.RunSpawnedAttempts(spec, entry.Flaky, exec.ExitZeroIsPass, recheckPin, runPinned)
		// A pin refusal from RunPinned spawned nothing, so it records like the
		// re-check's refusal: an unmatched row at the declined ordinal, after
		// the attempts that ran, not an engine error.
		if errors.Is(err, exec.ErrRefused) {
			refusal = &exec.Refusal{Ordinal: len(attempts), Err: fmt.Errorf("gate %q: %w", g.Name, err)}
		} else if err != nil {
			return GateExecution{}, fmt.Errorf("running gate %s: %w", g.Name, err)
		}
	} else {
		attempts, refusal, err = exec.RunVerifiedAttempts(spec, entry.Flaky, exec.ExitZeroIsPass, recheckPin)
		if err != nil {
			return GateExecution{}, fmt.Errorf("running gate %s: %w", g.Name, err)
		}
	}

	out := GateExecution{Verdict: VerdictFail}
	for i, a := range attempts {
		exit := a.Result.Exit
		verdict := VerdictFail
		if exit == 0 && !a.Result.TimedOut {
			verdict = VerdictPass
		}
		reason := networkAwareReason(a.Result.Reason, entry, verdict)
		if a.Result.TimedOut && !sc.Deadline.IsZero() &&
			a.Result.DurationMS < entryTimeout.Milliseconds() {
			// The timeout the child hit was the budget's remainder, not the
			// entry's own bound. Say so, or a reader of the row concludes the
			// trust entry's timeout changed. Decided from the attempt's own
			// duration rather than from the clamp above, because a lock wait
			// or an earlier flaky attempt can spend the budget after that
			// clamp was computed and exec.Run clamps again from the deadline.
			reason = withScratchNote(reason, fmt.Sprintf(
				"the claim's %s pre-gate budget bounded this gate below its "+
					"entry's %s timeout, so the claim returns inside the "+
					"executor's tool timeout", claimPreGateBudget, entryTimeout))
		}
		out.Results = append(out.Results, GateResultRow{
			Gate:       g.Name,
			Ordinal:    firstOrdinal + i,
			Argv:       match.Argv,
			Exit:       &exit,
			DurationMS: a.Result.DurationMS,
			Output:     a.Result.Output,
			Truncated:  a.Result.Truncated,
			Verdict:    verdict,
			Reason:     reason,
			TrustEntry: entry.Name,
			StubEntry:  entry.Stub,
			ArgvSHA256: trust.ArgvSHA256(match.Argv),
			Prefix:     entry.Prefix,
		})
		// The verdict for routing is the LAST attempt's. A gate that passes
		// on attempt 2 passes; one that fails all three fails.
		out.Verdict = verdict
	}
	if refusal != nil {
		// The pin no longer holds: nothing spawned for this ordinal, so the
		// row carries no argv and no exit, like a Resolve refusal.
		row := GateResultRow{
			Gate: g.Name, Ordinal: firstOrdinal + refusal.Ordinal, Verdict: VerdictUnmatched,
			Reason:     refusal.Err.Error(),
			TrustEntry: entry.Name,
			StubEntry:  entry.Stub,
			ArgvSHA256: trust.ArgvSHA256(match.Argv),
			Prefix:     entry.Prefix,
		}
		// The entry still matches, but the tree holds a script the run
		// did not pin. Nothing ran, so the row is `skipped` (DKT-169's
		// shape), not `unmatched`: the trust decision did not change.
		var refused *gateScriptCopyRefused
		if errors.As(refusal.Err, &refused) {
			row.Verdict = VerdictSkipped
			row.Argv = match.Argv
		}
		out.Results = append(out.Results, row)
		out.Verdict = VerdictFail
	}
	for _, note := range scriptNotes {
		for i := range out.Results {
			out.Results[i].Reason = withScratchNote(out.Results[i].Reason, note)
		}
	}

	return out, nil
}

// gateScriptCopyRefused is checkGateScriptCopies' refusal: the copy of a
// pinned gate script under the gate's cwd is not the pinned bytes, and the
// issue's scope does not cover it.
type gateScriptCopyRefused struct{ reason string }

func (e *gateScriptCopyRefused) Error() string { return e.reason }

// checkGateScriptCopies compares the copy under dir of each relatively named
// script operand of argv with the run's pin for the same path under
// sc.PinRoot. Bash opens such an operand against its cwd, so the copy under
// dir is the one that would run, and the executor of a step can write it.
//
// It refuses the first copy that differs, is absent, or cannot be checked,
// unless sc.Scope covers its path: an issue whose scope edits a gate script
// has to be able to run its own edit. It returns a note for each such exempt
// copy and for each operand the run holds no pin for (a run activated before
// gate scripts were pinned), which runs as before.
func checkGateScriptCopies(argv []string, dir string, sc StepContext) ([]string, error) {
	tokens := gateScriptTokens(argv)
	if len(tokens) < 2 {
		return nil, nil
	}
	var notes []string
	// tokens[0] is the program. A relatively named one never reaches here:
	// exec.Resolve refuses it before the spawn is prepared.
	for _, tok := range tokens[1:] {
		if filepath.IsAbs(tok) {
			continue
		}
		pin, ok := runPinFor(tok, sc)
		if !ok {
			notes = append(notes, fmt.Sprintf(
				"the run holds no pin for the gate script %q, so the copy under %q ran unchecked", tok, dir))
			continue
		}
		cmp, detail := compareCopy(filepath.Join(dir, tok), pin.want)
		if cmp == copyMatches {
			continue
		}
		if scopeCovers(sc.Scope, tok) {
			notes = append(notes, fmt.Sprintf(
				"the copy of %q under %q differs from the run's pin and was not checked "+
					"because the issue's scope edits it", tok, dir))
			continue
		}
		where := fmt.Sprintf("%q, the copy this gate would execute from %q,", tok, dir)
		return notes, &gateScriptCopyRefused{reason: copyReason(cmp, detail, where, pin) +
			"; not running it, because the issue's scope does not cover that path"}
	}
	return notes, nil
}

// runPinFor finds the run's pin for a relatively named gate script. Pins are
// keyed by the path activation resolved, so the spelled path under the pin
// root is tried first, then the same path with symlinks resolved now: a
// script replaced by a symlink after activation still finds its pin, and one
// reached through a directory symlink that existed at activation does too.
func runPinFor(tok string, sc StepContext) (pinnedRelativeScript, bool) {
	if sc.PinRoot == "" {
		return pinnedRelativeScript{}, false
	}
	spelled := filepath.Join(resolvedPath(sc.PinRoot), tok)
	candidates := []string{spelled}
	if resolved, err := filepath.EvalSymlinks(spelled); err == nil {
		candidates = append(candidates, resolved)
	}
	for _, file := range candidates {
		if want, ok := sc.FilePins[file]; ok {
			return pinnedRelativeScript{tok: tok, pinnedFile: file, want: want}, true
		}
	}
	return pinnedRelativeScript{}, false
}

// scopeCovers reports whether any scope entry, read as git reads a pathspec,
// matches path: the diff stage hands scope to git as pathspecs, so the
// exemption means what "in scope" means there. An entry without wildcards
// matches itself and everything under it. One with wildcards matches as
// fnmatch without FNM_PATHNAME, so `*` also matches `/`. An empty scope
// covers nothing, and an entry that does not compile covers nothing.
func scopeCovers(scope []string, path string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	for _, entry := range scope {
		if !strings.ContainsAny(entry, `*?[\`) {
			entry = strings.TrimSuffix(entry, "/")
			if entry != "" && (path == entry || strings.HasPrefix(path, entry+"/")) {
				return true
			}
			continue
		}
		if re, err := pathspecRegexp(entry); err == nil && re.MatchString(path) {
			return true
		}
	}
	return false
}

// pathspecRegexp translates a wildcard pathspec to an anchored regexp: `*`
// matches any run of characters, `?` any one, `[...]` (or `[!...]`) a class,
// and a backslash escapes the next character.
func pathspecRegexp(spec string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	for i := 0; i < len(spec); i++ {
		switch c := spec[i]; c {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		case '[':
			end := strings.IndexByte(spec[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := spec[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end + 1
		case '\\':
			if i+1 < len(spec) {
				i++
			}
			b.WriteString(regexp.QuoteMeta(spec[i : i+1]))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`$`)
	return regexp.Compile(b.String())
}

// sharesCheckout reports whether a gate's working directory IS the shared
// checkout, the one tree more than one step can be measuring at once.
//
// A plain cleaned-path comparison, the same test gateBaseSHA applies to a
// step's work root, and with the same limit: a worktree reached through a
// symlink to the checkout reads as isolated. That errs toward not locking,
// which is the direction the per-project lock's own failure taught — an
// unlocked gate on a shared tree risks a racing build, a locked gate on an
// isolated tree risks a wave parked behind a lock nobody needed.
func sharesCheckout(dir, repoRoot string) bool {
	return dir == "" || filepath.Clean(dir) == filepath.Clean(repoRoot)
}

// stepRef renders a step id for the child environment, or "" when there is no
// step to name (DKT-1186).
//
// The guard is the point: model.FormatStepID(0) is a perfectly well-formed
// `STEP-0` that resolves to nothing, and handing a gate an identifier that
// looks valid and answers NOT_FOUND is strictly worse than handing it nothing
// — the first fails inside the gate's own tooling with a misleading message,
// the second is a condition the gate can test for. Absence is the encoding
// DOCKET_SCOPE and DOCKET_GATE_BASE already use for "docket does not know".
func stepRef(stepID int) string {
	if stepID <= 0 {
		return ""
	}
	return model.FormatStepID(stepID)
}

// networkAwareReason annotates a FAILING gate that declared a network
// requirement, so the recorded reason names reachability as a candidate cause.
//
// This is DIAGNOSIS, not diagnosis-by-guessing. Core does not inspect the
// output for DNS errors or try to classify the failure — it cannot know why a
// command exited non-zero. What it knows is a fact the operator stated: this
// command needs those hosts. Surfacing that next to the failure is what turns
// "govulncheck exited 1, two process layers down, after the step was recorded"
// into a first question worth asking.
//
// A PASSING gate is never annotated: a gate that reached its hosts has nothing
// to explain.
func networkAwareReason(reason string, entry *trust.Entry, verdict string) string {
	if entry == nil {
		return reason
	}
	if verdict != VerdictFail || !entry.NeedsNetwork() {
		return reason
	}
	note := fmt.Sprintf(
		"this gate declares network access to %s; if the failure is a "+
			"connection or DNS error, the sandbox it ran in could not reach them",
		strings.Join(entry.Network, ", "))
	if reason == "" {
		return note
	}
	return reason + " — " + note
}

// unmatchedGate is the whole-gate refusal: nothing spawned, one recorded row.
func (r *ExecRunner) unmatchedGate(g GateSpec, reason string) GateExecution {
	return GateExecution{
		// Unmatched is a FAILURE for routing, never a pass and never a skip.
		// A workflow whose check cannot run has not passed its check.
		Verdict: VerdictFail,
		Results: []GateResultRow{{
			Gate: g.Name, Ordinal: 0, Verdict: VerdictUnmatched, Reason: reason,
		}},
	}
}
