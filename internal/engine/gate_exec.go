package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The real gate runner (TDD docs/tdd/gates-trust.md §7.1–§7.3).
//
// S3's PassThroughRunner returned a stubbed pass without touching the process
// table. This is the implementation that runs things, and the saga is unchanged
// around it: engine-spine §5.6's promise — "S4 changes one constructor call" —
// holds for the saga, and §7.1's table records honestly the four places more
// moves (result persistence, the verdict reader, the claim restructure, and
// vote steps).
//
// EVERYTHING SECURITY-RELEVANT HERE IS A CALL INTO GROUP 1. The matcher is
// internal/trust's, the spawn is internal/exec's, the env allowlist and the
// containment check are theirs too. This file decides WHEN to call them and
// WHAT TO RECORD; it re-implements none of them, which is what keeps the
// pre-gate path (§7.6.2 PG1) provably identical to the saga's.

// ExecRunner is the GateRunner that actually executes.
type ExecRunner struct {
	// RepoRoot is the working-tree root — the cwd every gate runs in (§5.1)
	// and the containment boundary (§5.2.1 R2).
	RepoRoot string
	// Identity is the project identity trust entries bind to (§3.4 P1). It
	// equals RepoRoot for env/local stores; under the global store it is the
	// worktree-stable project path, which is what lets two checkouts of one
	// repository match the same entries.
	Identity string
	// LockPath is the tree lockfile serializing this project's tree gates
	// (§7.4).
	LockPath string
	// LoadStore reads the trust store. It is a FIELD so a test can supply a
	// sandbox store without the package growing a path-taking constructor —
	// §9.5 SB3 forbids one, because every additional way to point docket at a
	// trust file is another way for repo content to point it somewhere.
	LoadStore func() (*trust.Store, error)
	// NowMS stamps results. Injected so a test is not at the mercy of a clock.
	NowMS func() int64
}

// NewExecRunner builds the real runner. THIS IS THE CONSTRUCTOR SWAP §7.1
// promised: NewEngine names it instead of PassThroughRunner, and the saga does
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
// fence gate matches PER LINE (§7.3 step 4) and a flaky command records each
// attempt individually (§5.6 F3).
type GateExecution struct {
	// Results are the rows to record, in execution order.
	Results []GateResultRow
	// Verdict is the gate's routing verdict, conjunctive over Results.
	Verdict string
}

// GateResultRow is one result this runner produced, in §11.4's shape plus the
// `reason` amendment (A6) and the `pre` marker.
//
// Argv and Exit are POINTERS because an unmatched gate never ran: NULL is the
// honest encoding of "no process existed", and a zero exit on a gate that did
// not execute is the exact confusion T11 exists to prevent.
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
	// Pre marks a pre-gate result (§7.6): an input to the step rather than a
	// judgment of it, and excluded from the saga's verdict by PG4.
	Pre bool
	// Stub is 1 ONLY for results the S3 pass-through produced. NOTHING THIS
	// RUNNER PRODUCES SETS IT (T11, N4) — the field survives so a migrated S3
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
}

// Run executes one gate: match, then spawn only what matched.
//
// The ORDER is the security property. Matching happens against an immutable
// store snapshot read ONCE (M1), and the matched entry's OWN ARGV is what
// executes — matching does not produce a permission that is later applied to an
// argv read from somewhere else, so there is no TOCTOU window (T4).
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
	// M1: ONE read, into an immutable snapshot. Every match for this gate runs
	// against these bytes. A concurrent `trust rm` or a trust-file rewrite
	// cannot change what this gate executes, because there is no second read.
	store, err := r.LoadStore()
	if err != nil {
		// A store that cannot be read is not an empty allowlist — an empty
		// allowlist is a MISSING file (§3.2), which is a different fact. A
		// malformed or unsafe store fails closed with the reason.
		return r.unmatchedGate(g, fmt.Sprintf("the trust store could not be read: %v", err)), nil
	}

	identity, err := trust.RepoIdentity(r.Identity)
	if err != nil {
		return r.unmatchedGate(g, fmt.Sprintf("the repo path could not be resolved: %v", err)), nil
	}

	if g.Source == "" {
		return r.runNamedGate(g, sc, store, identity)
	}
	return r.runFenceGate(g, sc, store, identity)
}

// runNamedGate runs a gate whose command is the trust entry's own argv.
//
// The candidate argv is nil: for a named gate the ENTRY IS the command, so the
// match is by name and binding, and the stored hash is verified against the
// stored argv to catch a corrupted or hand-edited file (M3).
func (r *ExecRunner) runNamedGate(
	g GateSpec, sc StepContext, store *trust.Store, identity string,
) (GateExecution, error) {
	match := store.Lookup(identity, g.Name, nil)
	if !match.Matched {
		return r.unmatchedGate(g, match.Reason), nil
	}
	return r.spawnMatched(g, sc, match, 0)
}

// runFenceGate runs the harvested fence lines, EACH MATCHED INDEPENDENTLY.
//
// §7.3 step 4's independence is the anti-pattern's opposite: an implementation
// that matched the block as a unit, or that ran the matched lines and silently
// skipped the rest, would let an attacker append a line to a body that a prefix
// entry happens to cover. Per-line matching with per-line results makes each
// line its own decision with its own record.
func (r *ExecRunner) runFenceGate(
	g GateSpec, sc StepContext, store *trust.Store, identity string,
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
	// ordinal, and mark the gate FAILED for routing (N3: "we couldn't check, so
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
		// §7.3 step 3: re-verify the stored hash against the stored command
		// BEFORE spawning. S3's snapshot closes "the body cannot inject"; this
		// closes "the row cannot be swapped". A database write is out of §2's
		// trust boundary, but it costs one hash to detect anyway.
		if i < len(g.CommandHashes) && g.CommandHashes[i] != "" {
			if workflow.SHA256([]byte(command)) != g.CommandHashes[i] {
				fail(fmt.Sprintf(
					"the stored command %s does not match its recorded hash; "+
						"the record was altered after activation and will not be run",
					exec.Render(command)), "")
				continue
			}
		}

		// K2: tokenized ONCE, at execution, by a splitter that performs NO
		// expansion of any kind. A `$` is a literal dollar sign here.
		argv, err := exec.Split(command)
		if err != nil {
			fail(fmt.Sprintf("the command %s could not be read as an argv: %v",
				exec.Render(command), err), "")
			continue
		}

		match := store.Lookup(identity, g.Name, argv)
		if !match.Matched {
			fail(match.Reason, trust.ArgvSHA256(argv))
			continue
		}

		ex, err := r.spawnMatched(g, sc, match, ordinal)
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

// spawnMatched runs a matched command, taking the tree mutex when the entry
// declares `tree = true` and recording every flaky attempt individually.
func (r *ExecRunner) spawnMatched(
	g GateSpec, sc StepContext, match trust.Match, firstOrdinal int,
) (GateExecution, error) {
	entry := match.Entry
	timeout := exec.DefaultTimeout
	if entry.Timeout != "" {
		if d, err := time.ParseDuration(entry.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	// The MATCHED entry's own declaration is what reaches the child,
	// the same discipline §7.2 M1 applies to argv: the entry that authorized
	// the command is the entry that configures it, so there is no window in
	// which one entry's permission is applied to another's requirements.
	env, err := exec.BuildEnv(exec.EnvPolicy{
		Gate: g.Name, Repo: r.RepoRoot, Network: entry.Network,
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

	// §5.2.1 R5: the containment check is unconditional and lives in
	// internal/exec, so no path skips it — named gates, fence gates, and
	// pre-gates all reach it through Resolve.
	entryArgv0 := ""
	if len(entry.Argv) > 0 {
		entryArgv0 = entry.Argv[0]
	}
	resolved, err := exec.Resolve(match.Argv[0], r.RepoRoot, entryArgv0)
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

	argv := append([]string{resolved}, match.Argv[1:]...)

	// DKT-9: the gate runs in the step's worktree when one is known, mirroring
	// the diff stage's resolution (saga.go). RepoRoot stays the containment
	// boundary for the BINARY above — where a command may live and where it
	// runs are separate facts — but the tree it measures must be the tree the
	// step reported, or the recorded evidence describes a HEAD the step never
	// touched. A worktree that is already gone (they are swept at integration)
	// records an honest refusal rather than silently measuring the shared
	// checkout — and rather than an engine error, which on the pre-claim path
	// would block the claim PG2/PG3 forbid blocking. The ROW's verdict is
	// `skipped`, not `fail` (DKT-169): nothing ran, so "measured and failed"
	// would be a false record; the execution verdict stays fail so routing
	// remains fail-closed.
	dir := r.RepoRoot
	if sc.WorkRoot != "" {
		if info, statErr := os.Stat(sc.WorkRoot); statErr != nil || !info.IsDir() {
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
		dir = sc.WorkRoot
	}
	spec := exec.Spec{Argv: argv, Dir: dir, Env: env, Timeout: timeout}

	// L3: the lock is acquired IMMEDIATELY BEFORE the spawn and released
	// IMMEDIATELY AFTER, outside every transaction and never held across a
	// database write.
	if entry.Tree {
		lock, lockErr := acquireTreeLock(r.LockPath, timeout)
		if lockErr != nil {
			// L4/L7: the serialization the gate requires cannot be provided, so
			// it fails rather than running unserialized.
			return GateExecution{
				Verdict: VerdictFail,
				Results: []GateResultRow{{
					Gate: g.Name, Ordinal: firstOrdinal, Verdict: VerdictFail,
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

	attempts, err := exec.RunAttempts(spec, entry.Flaky, exec.ExitZeroIsPass)
	if err != nil {
		return GateExecution{}, fmt.Errorf("running gate %s: %w", g.Name, err)
	}

	out := GateExecution{Verdict: VerdictFail}
	for i, a := range attempts {
		exit := a.Result.Exit
		verdict := VerdictFail
		if exit == 0 && !a.Result.TimedOut {
			verdict = VerdictPass
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
			Reason:     networkAwareReason(a.Result.Reason, entry, verdict),
			TrustEntry: entry.Name,
			StubEntry:  entry.Stub,
			ArgvSHA256: trust.ArgvSHA256(match.Argv),
			Prefix:     entry.Prefix,
		})
		// F4: the verdict for routing is the LAST attempt's. A gate that passes
		// on attempt 2 passes; one that fails all three fails.
		out.Verdict = verdict
	}

	return out, nil
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
		// N3: unmatched is a FAILURE for routing, never a pass and never a skip.
		// A workflow whose check cannot run has not passed its check.
		Verdict: VerdictFail,
		Results: []GateResultRow{{
			Gate: g.Name, Ordinal: 0, Verdict: VerdictUnmatched, Reason: reason,
		}},
	}
}
