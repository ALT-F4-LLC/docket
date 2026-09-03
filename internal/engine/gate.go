package engine

import "context"

// The gate seam (TDD §5.6). The rule it encodes:
//
//	At S3, everything about gates is real except the subprocess.
//
// Parsed and stored gates, `pre` ordering, fence harvesting, the snapshot and
// its hash, the saga's gate stage, the `gate-started` event, the recorded
// result, resume-after-crash re-entry, and routing over the verdict are ALL
// real at this stage. S4 adds the spawn, the trust matching, argv resolution,
// and capture — and it changes one constructor call, because the saga is
// written against this interface and nothing else.

// GateSpec is one gate to run, normalized from §11.1's two spellings.
type GateSpec struct {
	// Name is the trusted gate name.
	Name string
	// Source is `fence:<tag>` when the gate's commands come from an issue
	// body's fenced block, or "" for a trusted gate resolved from the trust
	// file. Core reads the tag; what the commands mean is never its business.
	Source string
	// Pre marks a gate that runs at claim, with its results included in the
	// context bundle, rather than in order inside `complete` (§11.1).
	Pre bool
	// Commands are the harvested fence lines, verbatim, for a `fence:` gate.
	// They are carried rather than re-read so what runs is what was hashed at
	// activation — a post-activation edit cannot inject (engine-spec §4).
	Commands []string
	// CommandHashes are the SHA-256s activation stored alongside each command,
	// positionally aligned with Commands.
	//
	// S4 re-verifies each one against its stored command BEFORE spawning
	// (gates-trust §7.3 step 3). S3's snapshot already closes "the issue body
	// cannot inject"; this closes the narrower "the stored row cannot be
	// swapped". A direct database write is outside §2's trust boundary, but it
	// costs one hash to detect and a mismatch is refused rather than run.
	CommandHashes []string
}

// StepContext is the step a gate runs for. It is deliberately thin at this
// stage: the pass-through runner reads none of it, and S4's real runner needs
// the identity and the working directory, not the closure.
type StepContext struct {
	Instance string
	RunID    int
	IssueID  int
	// Scope is the issue's SNAPSHOTTED scope globs (DKT-63), exported to the
	// child as DOCKET_SCOPE so a diff-shaped gate can evaluate the change it
	// is gating rather than the whole dirty tree. Per-step gates over a
	// shared tree see every issue's in-flight edits; without the scope, one
	// issue's uncommitted work failed the next issue's gate three times in
	// one run, parking the run each time on an already-adjudicated fact.
	Scope []string
	// WorkRoot is the private worktree the step's gates should measure, or ""
	// for the shared checkout (DKT-9). The saga fills it with the step's own
	// recorded worktree; the pre-claim path fills it with the resolved target
	// worktree — the tree the producing step declared. Before this field,
	// every gate spawned in the shared checkout unconditionally, so a gate's
	// evidence could describe a HEAD the step under review never touched.
	WorkRoot string
	// Base is the sha of the step's base commit — the commit WorkRoot was
	// created from — exported to the child as DOCKET_GATE_BASE (DKT-992) so a
	// gate can scan exactly the step's committed range (base..HEAD of the
	// tree it runs in). The saga fills it, for worktree-recorded steps only,
	// with the same fork-point resolution the diff stage uses (runDiffBase /
	// worktreeForkPoint); it stays "" — the var unset — for a shared-checkout
	// step and on the pre-claim path, where no committed range belongs to the
	// step being gated. Empty means unset, never an invented value: executors
	// commit before `step record`, so a gate that falls back to scanning the
	// working tree scans nothing, and before this field it had no way to
	// learn the range and guessed (`git diff HEAD~1`, wrong for multi-commit
	// steps) or measured the clean tree (always empty).
	Base string
	// CacheRoot is a scratch directory the caller destroys together with the
	// tree in WorkRoot, or "" when the tree outlives the spawn (DKT-1166).
	//
	// Only the pre-claim path fills it, and only when it RECONSTRUCTED the
	// tree under review: a reconstruction is deleted within the minute, and a
	// linter result cache keyed by package content but carrying absolute
	// source paths outlives it, so a later run over the same content re-opens
	// a path that is gone, finds no `//nolint` there, and re-emits an issue
	// the source suppressed. exec.EnvPolicy.CacheRoot documents the mechanism
	// and names the variables it redirects.
	CacheRoot string
}

// GateResult is one gate's outcome, in §11.4's `gate result` shape.
//
// `Stub` is the field that makes the S3->S4 window safe. Every result this
// stage records carries `stub: true`, so an operator inspecting a run can tell
// a stubbed gate from a real one. A silent pass-through that looked identical
// to a real pass would be a trap for exactly the window where gates are
// specified but not yet executed — a green run would read as gate coverage it
// does not have.
type GateResult struct {
	Gate       string   `json:"gate"`
	Argv       []string `json:"argv"`
	Exit       int      `json:"exit"`
	DurationMS int64    `json:"duration_ms"`
	Output     string   `json:"output"`
	Truncated  bool     `json:"truncated"`
	Verdict    string   `json:"verdict"`
	Stub       bool     `json:"stub,omitempty"`
}

// Gate verdicts, per §11.4.
const (
	VerdictPass = "pass"
	VerdictFail = "fail"
	// VerdictUnmatched is a FIRST-CLASS OUTCOME, added at S4 (gates-trust §6.2).
	//
	// §4, verbatim: "each must match a trust entry … or it is NOT EXECUTED and
	// reported as unmatched." It is not a pass, not a skip, and not an error
	// that aborts the run: the step routes per its `on_fail`, because a
	// workflow whose check cannot run has not passed its check. The opposite
	// reading — "we couldn't check, so carry on" — is precisely what makes a
	// security control decorative.
	VerdictUnmatched = "unmatched"
	// VerdictSkipped marks a gate that COULD NOT MEASURE its tree (DKT-169):
	// the step's recorded worktree was already swept, so nothing ran. It is
	// distinct from `fail` because a fail row with a null exit and empty
	// output is indistinguishable, to a verifier told to read recorded gate
	// output, from a gate that ran and the change failed it — and the two need
	// opposite responses (reconstruct-and-remeasure vs. judge the failure).
	// For routing it still counts as not-pass, same as `unmatched`: "we
	// couldn't check, so carry on" is what makes a control decorative.
	VerdictSkipped = "skipped"
)

// GateRunner executes one gate and returns its result. S3 ships
// PassThroughRunner; S4 ships the real one. The saga is written against this
// interface, so S4 changes one constructor call and nothing else.
type GateRunner interface {
	Run(ctx context.Context, g GateSpec, sc StepContext) (GateResult, error)
}

// PassThroughRunner is the S3 implementation: it returns a passing, stubbed
// result WITHOUT TOUCHING THE PROCESS TABLE.
//
// Two consequences are asserted as tests, and stated here so neither is
// mistaken for an oversight: (a) a workflow whose gate WOULD fail still passes
// at this stage, and the QA section says so in a comment so nobody reads a
// green run as gate coverage; (b) `stub: true` appears in every result this
// stage records.
type PassThroughRunner struct{}

// Run returns `{verdict: "pass", exit: 0, stub: true}`. It never spawns
// anything: §6's "No subprocess ever executes inside a transaction" holds
// trivially here, and it will still hold at S4 because the saga's gate stage is
// its own transaction-free stage.
func (PassThroughRunner) Run(_ context.Context, g GateSpec, _ StepContext) (GateResult, error) {
	return GateResult{
		Gate:    g.Name,
		Exit:    0,
		Verdict: VerdictPass,
		Stub:    true,
	}, nil
}

// compile-time proof the stub satisfies the seam, so a change to the interface
// breaks here rather than at the S4 swap.
var _ GateRunner = PassThroughRunner{}
