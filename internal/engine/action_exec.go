package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/exec"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The real action runner (docs/tdd/payloads-thresholds.md §6.1, §6.2).
//
// EVERYTHING SECURITY-RELEVANT HERE IS A CALL INTO S4's GROUP 1, exactly as
// gate_exec.go's header says of itself. The matcher is internal/trust's, the
// spawn is internal/exec's, the env allowlist and the containment check are
// theirs too. This file decides WHEN to call them and WHAT TO RECORD; it
// re-implements none of them, which is what makes "actions use the same trust
// path as gates" a fact about the call graph rather than a claim.

// builtinFunc is a computation core performs itself.
type builtinFunc func(a ActionSpec, sc StepContext) ActionResult

// builtins are the actions core computes itself. There is exactly one, and §2
// names it: "One is builtin and generic: action = \"aggregate\"".
//
// The keys come from workflow.BuiltinActions rather than from string literals
// here, so the register-time rule (V27) and the run-time dispatch cannot
// disagree about which names are core's.
var builtins = map[string]builtinFunc{
	workflow.ActionAggregate: runAggregate,
}

// ExecActionRunner is the ActionRunner that actually computes and, for a
// non-builtin, actually spawns.
type ExecActionRunner struct {
	// RepoRoot is the working-tree root — the cwd every action runs in and the
	// containment boundary (gates-trust §5.2.1 R2).
	RepoRoot string
	// Identity is the project identity trust entries bind to (§3.4 P1). It
	// equals RepoRoot for env/local stores; under the global store it is the
	// worktree-stable project path.
	Identity string
	// LockPath is the tree lockfile serializing this project's tree-declaring
	// commands (§7.4 / A7).
	LockPath string
	// LoadStore reads the trust store. It is a FIELD so a test can supply a
	// sandbox store without the package growing a path-taking constructor —
	// gates-trust §9.5 SB3 forbids one, because every additional way to point
	// docket at a trust file is another way for repo content to point it
	// somewhere.
	LoadStore func() (*trust.Store, error)
}

// NewActionRunner builds the real runner. THIS IS THE CONSTRUCTOR SWAP
// engine-spine §6.13 promised, and §6.4 records honestly what else moved.
func NewActionRunner(paths RepoPaths) *ExecActionRunner {
	return &ExecActionRunner{
		RepoRoot:  paths.ExecRoot,
		Identity:  paths.Identity,
		LockPath:  paths.LockPath,
		LoadStore: trust.Load,
	}
}

var _ ActionRunner = (*ExecActionRunner)(nil)

// Run computes one action step.
//
// B1: RESOLUTION IS BUILTIN FIRST. An action core computes is never looked up
// in the trust store, so a trust entry cannot shadow a builtin and a builtin
// cannot be disabled by removing one.
func (r *ExecActionRunner) Run(
	ctx context.Context, a ActionSpec, sc StepContext,
) (ActionResult, error) {
	if fn, ok := builtins[a.Name]; ok {
		return fn(a, sc), nil
	}
	return r.runTrustedCommand(ctx, a, sc), nil
}

// runAggregate is the builtin (§7).
//
// B2: IT SPAWNS NOTHING, so §6's "no subprocess ever executes inside a
// transaction" holds trivially for it. B3: a failure — bad params, an
// unorderable value — is a STEP failure routed per the step's effective
// `on_fail`, never an engine error that aborts the saga. A workflow authoring
// mistake must not wedge a run.
func runAggregate(a ActionSpec, sc StepContext) ActionResult {
	fail := func(format string, args ...any) ActionResult {
		reason := fmt.Sprintf(format, args...)
		return ActionResult{
			Kind: a.Output, Failed: true, Reason: reason,
			Results: []ActionResultRow{{
				Action: a.Name, Verdict: db.ActionVerdictFail,
				Builtin: true, Reason: reason,
			}},
		}
	}

	params, err := ParseAggregateParams(a.Params)
	if err != nil {
		return fail("step %s: %v", sc.Instance, err)
	}

	outcome, err := Aggregate(a.Inputs, params, a.Order)
	if err != nil {
		return fail("step %s: %v", sc.Instance, err)
	}

	payload, err := json.Marshal(outcome.Payload)
	if err != nil {
		return fail("step %s: encoding the aggregate output: %v", sc.Instance, err)
	}

	// E5: the output is validated against BOTH `aggregate@1` and the step's
	// declared schema. V30 already made the conjunction's satisfiability a
	// register-time question, so a refusal here means the computation produced
	// something neither document describes — a defect, reported as a step
	// failure rather than swallowed.
	if err := validateAggregateOutput(payload, a.Validate); err != nil {
		return fail("step %s: %v", sc.Instance, err)
	}

	return ActionResult{
		Kind:    params.Output,
		Body:    aggregateBody(sc.Instance, len(outcome.Payload), len(outcome.Held)),
		Payload: string(payload),
		Held:    outcome.Held,
		Results: []ActionResultRow{{
			Action: a.Name, Verdict: db.ActionVerdictPass, Builtin: true,
		}},
	}
}

// validateAggregateOutput applies §7.6 E5's conjunction.
func validateAggregateOutput(payload []byte, declared func([]byte) error) error {
	builtin, err := aggregateSchema()
	if err != nil {
		return fmt.Errorf("the embedded %s document is unusable: %w",
			schemaAggregateRef, err)
	}
	if err := builtin.ValidatePayload(payload); err != nil {
		return fmt.Errorf("the computed output does not satisfy %s: %w",
			schemaAggregateRef, err)
	}
	if declared == nil {
		// V29 guarantees an `aggregate` step declares a schema, so this is
		// reachable only from a definition that arrived some other way. Half the
		// conjunction is better than none.
		return nil
	}
	if err := declared(payload); err != nil {
		return fmt.Errorf("the computed output does not satisfy the step's "+
			"declared payload schema: %w", err)
	}
	return nil
}

// schemaAggregateRef is `aggregate@1` for the messages above. It is a package
// constant rather than a call so a failure to compile the document does not
// have to be handled twice inside one error path.
const schemaAggregateRef = "aggregate@1"

// runTrustedCommand is §6.2: a non-builtin action is a USER-TRUSTED COMMAND,
// through §4's trust model WITH NO EXCEPTIONS.
//
// A1/A2: the store is read ONCE into an immutable snapshot and the matched
// entry's OWN ARGV is what executes, so matching never produces a permission
// later applied to an argv read from somewhere else (gates-trust T4's TOCTOU
// window, closed the same way here).
func (r *ExecActionRunner) runTrustedCommand(
	_ context.Context, a ActionSpec, sc StepContext,
) ActionResult {
	unmatched := func(format string, args ...any) ActionResult {
		reason := fmt.Sprintf(format, args...)
		return ActionResult{
			Kind: a.Output, Failed: true, Reason: reason,
			Results: []ActionResultRow{{
				Action: a.Name, Verdict: db.ActionVerdictUnmatched, Reason: reason,
			}},
		}
	}

	store, err := r.LoadStore()
	if err != nil {
		// A store that cannot be read is not an empty allowlist — an empty
		// allowlist is a MISSING file, which is a different fact. A malformed or
		// unsafe store fails closed with the reason.
		return unmatched("the trust store could not be read: %v", err)
	}
	identity, err := trust.RepoIdentity(r.Identity)
	if err != nil {
		return unmatched("the repo path could not be resolved: %v", err)
	}

	// A2: candidates are entries whose name equals the action name AND whose
	// repo binding matches this repo. A candidate argv of nil means "the ENTRY
	// is the command", exactly as a named gate resolves.
	match := store.Lookup(identity, a.Name, nil)
	if !match.Matched {
		// A3: NO ENTRY ⇒ `unmatched`. NOTHING SPAWNS, and the step routes per
		// its effective `on_fail` — the same direction gates take (§6.2 N3),
		// because a computation that could not run has not succeeded.
		return unmatched("%s", match.Reason)
	}

	return r.spawnMatched(a, sc, match)
}

// spawnMatched runs a matched action command and shapes its output into the
// §6.2 contract.
func (r *ExecActionRunner) spawnMatched(
	a ActionSpec, sc StepContext, match trust.Match,
) ActionResult {
	entry := match.Entry

	timeout := exec.DefaultTimeout
	if entry.Timeout != "" {
		if d, err := time.ParseDuration(entry.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	// A4: the env allowlist, TERM/CI, DOCKET_REPO, and the DOCKET_TOKEN
	// exclusion with its pre-spawn assertion, unchanged. The token has already
	// retired by the routing stage, so the exclusion is belt-and-braces here —
	// which is exactly why it is asserted rather than assumed.
	//
	// The policy's gate field carries the ACTION name, so a command can tell
	// which step invoked it. It is an opaque hint either way.
	env, err := exec.BuildEnv(exec.EnvPolicy{Gate: a.Name, Repo: r.RepoRoot})
	if err != nil {
		return r.failed(a, fmt.Sprintf("building the child environment: %v", err), match)
	}

	// A5: argv[0] resolution, exec.ErrDot, and the repo-containment rule
	// R1-R5, unconditionally and in internal/exec so no path skips them.
	entryArgv0 := ""
	if len(entry.Argv) > 0 {
		entryArgv0 = entry.Argv[0]
	}
	resolved, err := exec.Resolve(match.Argv[0], r.RepoRoot, entryArgv0)
	if err != nil {
		// A refusal means NOTHING RAN: verdict is unmatched and the process
		// table was never touched.
		reason := err.Error()
		return ActionResult{
			Kind: a.Output, Failed: true, Reason: reason,
			Results: []ActionResultRow{{
				Action: a.Name, Verdict: db.ActionVerdictUnmatched, Reason: reason,
				TrustEntry: entry.Name, ArgvSHA256: trust.ArgvSHA256(match.Argv),
				Prefix: entry.Prefix,
			}},
		}
	}

	spec := exec.Spec{
		Argv: append([]string{resolved}, match.Argv[1:]...),
		Dir:  r.RepoRoot, Env: env, Timeout: timeout,
		// §6.2's stdin half: the §11.4 context object, one JSON document, then
		// EOF. SplitStdout separates the structured reply from the diagnostic
		// stream, because a command that logs to stderr must not corrupt the
		// document core parses.
		Stdin:       a.Context,
		SplitStdout: true,
	}

	// A7: a `tree = true` entry takes the same per-repo flock a tree gate
	// takes. A computation that reads the working tree races a build exactly as
	// a gate does, and the lock is acquired immediately before the spawn and
	// released immediately after — never held across a database write.
	if entry.Tree {
		lock, lockErr := acquireTreeLock(r.LockPath, timeout)
		if lockErr != nil {
			return r.failed(a, lockErr.Error(), match)
		}
		defer lock.release()
	}

	// A8: flaky re-runs apply and are recorded individually with ascending
	// ordinal. The pass predicate is exit-zero AND a parseable reply — a
	// command that exits 0 and prints garbage has not produced a result, and
	// re-running it is exactly what `flaky` is for.
	attempts, err := exec.RunAttempts(spec, entry.Flaky, actionAttemptIsPass)
	if err != nil {
		return r.failed(a, fmt.Sprintf("running action %s: %v", a.Name, err), match)
	}

	out := ActionResult{Kind: a.Output}
	var last *exec.Attempt
	for i := range attempts {
		attempt := attempts[i]
		exit := attempt.Result.Exit
		verdict := db.ActionVerdictFail
		if actionAttemptIsPass(attempt.Result) {
			verdict = db.ActionVerdictPass
		}
		out.Results = append(out.Results, ActionResultRow{
			Action: a.Name, Ordinal: i, Argv: match.Argv, Exit: &exit,
			DurationMS: attempt.Result.DurationMS,
			Output:     actionOutput(attempt.Result),
			Truncated:  attempt.Result.Truncated,
			Verdict:    verdict, Reason: attempt.Result.Reason,
			TrustEntry: entry.Name, ArgvSHA256: trust.ArgvSHA256(match.Argv),
			Prefix: entry.Prefix,
		})
		last = &attempt
	}
	if last == nil {
		return r.failed(a, "the command produced no attempts", match)
	}

	// F4's rule, inherited: the LAST attempt's outcome is the one that routes.
	// A command that passes on attempt 2 passes.
	if last.Result.Exit != 0 || last.Result.TimedOut {
		reason := last.Result.Reason
		if reason == "" {
			reason = fmt.Sprintf("the action exited %d", last.Result.Exit)
		}
		out.Failed, out.Reason = true, reason
		out.Results[len(out.Results)-1].Reason = reason
		return out
	}

	reply, err := parseActionReply(last.Result.Stdout)
	if err != nil {
		// The unparseable-stdout case. This output is ATTACKER-INFLUENCED and
		// reaches a terminal, so it is quoted through gates-trust §5.7's
		// renderer rather than printed raw.
		reason := err.Error()
		out.Failed, out.Reason = true, reason
		out.Results[len(out.Results)-1].Verdict = db.ActionVerdictFail
		out.Results[len(out.Results)-1].Reason = reason
		return out
	}

	// The payload is validated exactly as a worker's is (§4.8): against the
	// step's declared schema when it declares one. A failure is a STEP failure
	// routed per `on_fail`, not a refusal to a caller — there is no caller; the
	// engine is the caller.
	if a.Validate != nil {
		if err := a.Validate([]byte(reply.Payload)); err != nil {
			reason := fmt.Sprintf(
				"the action's payload does not satisfy the step's declared schema: %v", err)
			out.Failed, out.Reason = true, reason
			out.Results[len(out.Results)-1].Verdict = db.ActionVerdictFail
			out.Results[len(out.Results)-1].Reason = reason
			return out
		}
	}

	out.Body, out.Payload = reply.Body, reply.Payload
	return out
}

// failed shapes a whole-action failure that is not a trust refusal.
func (r *ExecActionRunner) failed(
	a ActionSpec, reason string, match trust.Match,
) ActionResult {
	return ActionResult{
		Kind: a.Output, Failed: true, Reason: reason,
		Results: []ActionResultRow{{
			Action: a.Name, Verdict: db.ActionVerdictFail, Reason: reason,
			TrustEntry: match.Entry.Name, ArgvSHA256: trust.ArgvSHA256(match.Argv),
			Prefix: match.Entry.Prefix,
		}},
	}
}

// actionAttemptIsPass is the flaky-retry predicate: exit zero, no timeout, and
// a reply core can read.
//
// The last clause is what makes `flaky` mean something for an action. A gate's
// whole answer is its exit code, so exit-zero is the whole predicate there; an
// action's answer is a document, and a command that exits 0 having printed
// nothing usable has not answered.
func actionAttemptIsPass(res exec.Result) bool {
	if res.Exit != 0 || res.TimedOut {
		return false
	}
	_, err := parseActionReply(res.Stdout)
	return err == nil
}

// actionOutput is what `action_results.output` records for one attempt.
//
// The DIAGNOSTIC stream, not the structured one: stdout is the wire and
// duplicating it into the audit column would store every action's reply twice.
// When a command wrote nothing to stderr its stdout is recorded instead, so a
// tool that reports its errors on stdout does not leave an empty trace.
func actionOutput(res exec.Result) string {
	if res.Output != "" {
		return res.Output
	}
	return res.Stdout
}

// actionReply is §6.2's stdout contract: ONE JSON object,
// `{"body": "<string>", "payload": [ … ]}`.
//
// An object rather than "stdout is the payload" because an action that wants to
// record a human-readable body — which every artifact has — would otherwise have
// no channel for it, and inventing one later would be a wire-shape break for
// commands already written. One shape, both halves, from the start.
type actionReply struct {
	Body    string
	Payload string
}

// parseActionReply reads the contract, applying its defaults: `body` defaults
// to "" when absent, `payload` to [].
//
// Unknown keys are REFUSED, and a second document after the first is refused
// too. Both follow V28's discipline: a key the contract does not name is a
// declaration the command's author believes core is reading, and saying so is
// better than dropping it silently.
func parseActionReply(stdout string) (actionReply, error) {
	var wire struct {
		Body    *string          `json:"body"`
		Payload *json.RawMessage `json:"payload"`
	}
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(stdout)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return actionReply{}, actionParseError(stdout, err)
	}
	if decoder.More() {
		return actionReply{}, actionParseError(stdout,
			errors.New("the contract is ONE JSON document, then EOF"))
	}

	out := actionReply{Payload: "[]"}
	if wire.Body != nil {
		out.Body = *wire.Body
	}
	if wire.Payload != nil {
		// The payload must be the ARRAY OF OBJECTS a threshold aggregates over.
		// Checking it here rather than at the artifact insert keeps the failure
		// attributable to the command that produced it.
		var elements []map[string]any
		if err := json.Unmarshal(*wire.Payload, &elements); err != nil {
			return actionReply{}, actionParseError(stdout,
				fmt.Errorf("`payload` must be an array of objects: %w", err))
		}
		out.Payload = string(*wire.Payload)
	}
	return out, nil
}

// actionParseError renders the unparseable-stdout refusal, quoting the first
// 200 bytes THROUGH gates-trust §5.7's renderer.
//
// The quoting is not cosmetic. This output is attacker-influenced — it is
// whatever a command printed — and it reaches an operator's terminal in a
// routing reason, so control bytes are escaped by the one renderer that exists
// for the purpose rather than by a second implementation here.
func actionParseError(stdout string, cause error) error {
	const quoted = 200
	head := stdout
	suffix := ""
	if len(head) > quoted {
		head, suffix = head[:quoted], "…"
	}
	return fmt.Errorf(
		"the action's stdout is not the `{\"body\": …, \"payload\": [ … ]}` "+
			"object the contract requires: %w; it began %s%s",
		cause, exec.Render(head), suffix)
}
