package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Pre-gates — claim-time execution (TDD docs/tdd/gates-trust.md §7.6, T14).
//
// §11.1, verbatim: "`pre = true` gates run at claim with results included in
// the context bundle (measure-then-judge steps); the rest run in order inside
// `complete`."
//
// THE SAME TRUST MODEL, NO EXCEPTIONS (§7.6.2 PG1). A pre-gate resolves through
// the identical matcher, the identical env allowlist, the identical timeout and
// process-group kill, the identical capture, and the identical tree mutex. There
// is no pre-gate-specific path in internal/exec or internal/trust, and that is
// deliberate: pre-gates run earlier in the lifecycle and on a different code
// path than the saga's gates, which is exactly the shape an implementation
// "simplifies" into a trusted path. It reaches the same runner or it does not
// run.

// preClaimGates returns the gates that run at claim, in declared order.
//
// It is completionGates' exact complement: that function drops `pre` gates, and
// this one keeps only them. Together they partition the step's gates, so no
// gate can be silently skipped by both or run by both.
func preClaimGates(spec *workflow.Step) []workflow.Gate {
	if spec == nil {
		return nil
	}
	out := make([]workflow.Gate, 0, len(spec.Gates))
	for _, g := range spec.Gates {
		if g.Pre {
			out = append(out, g)
		}
	}
	return out
}

// preGateTarget resolves WHAT a step's pre-gates are supposed to measure,
// BEFORE any pre-gate spawns (DKT-9, DKT-254).
//
// At the moment a pre-gate runs, AssembleContext has not: the claim assembles
// the bundle only after the pre-gate phase, so the target ref the bundle will
// carry (resolveTarget's TargetWorktree) does not exist yet. This duplicates
// exactly the input resolution that computation needs — the resolved
// `issue.diff` round record's declared worktree, the tree whose `target_sha`
// the bundle names as the object under review. The step's OWN recorded
// worktree wins when present (a re-offer after a completion that already
// declared one), because that is the tree this step's work touched.
//
// IT RETURNS BOTH HALVES, and the pair is the whole point (DKT-254). The
// worktree alone cannot distinguish the two cases that need opposite handling:
//
//	sha == "", worktree == ""   nothing is under review yet. The shared
//	                            checkout IS the subject — a pre-gate on a
//	                            first implement step measuring "is the tree
//	                            clean before we start" is doing exactly its
//	                            job. Run it.
//	sha != "", worktree == ""   there IS an object under review and no tree
//	                            holds it. Measuring the shared checkout here
//	                            produces a verdict about a different tree
//	                            than the one under review, which is RUN-2's
//	                            ac-commands recording PASS at 76f5d0c while
//	                            the sha under review was 2b9d9c8.
//
// Before this, both cases arrived as the empty string and both fell back to the
// shared checkout, so every recorded PASS of the second kind had zero evidence
// value and nothing said so.
//
// It runs inside the claim's transaction A, where the resolver's snapshot
// reads belong; the paths it returns are consumed outside it.
func preGateTarget(
	tx *sql.Tx, sched *Scheduler, step *db.Step, spec *workflow.Step,
) (sha, worktree string, err error) {
	if step.WorkRoot != "" {
		// The step's own recorded worktree wins, and it settles the question:
		// a tree this step's work touched is the tree to measure, whatever the
		// inputs resolve to.
		return "", step.WorkRoot, nil
	}
	artifacts, err := db.ListRunArtifactsTx(tx, step.RunID)
	if err != nil {
		return "", "", err
	}
	return resolvedTargetFor(tx, sched, step, spec, artifacts)
}

// resolvedTargetFor resolves the target ref a step's bundle will carry —
// resolveTarget over the step's resolved inputs — without assembling the
// bundle. preGateWorkRoot consumes the worktree half; the dispatch verbs'
// stale-target check (DKT-193) consumes the sha half. The artifact snapshot is
// passed in so a caller iterating a whole manifest loads the table once.
func resolvedTargetFor(
	tx *sql.Tx, sched *Scheduler, step *db.Step, spec *workflow.Step,
	artifacts []*db.Artifact,
) (sha, worktree string, err error) {
	if spec == nil || len(spec.Inputs) == 0 {
		return "", "", nil
	}
	issue, linked, err := contextIssue(tx, step.RunID, step.IssueID)
	if err != nil {
		return "", "", err
	}
	inputs, err := resolveInputs(tx, sched, step, spec, issue.BodySnapshot, artifacts, linked)
	if err != nil {
		return "", "", err
	}
	sha, worktree = resolveTarget(inputs)
	return sha, worktree, nil
}

// stepTargetRef is resolvedTargetFor for a reader holding ONLY the step —
// `step show` (DKT-1056). It loads the artifact snapshot itself, because this
// caller answers about one step rather than iterating a manifest.
//
// THE GUARD IS THE POINT. A step whose definition does not declare `issue.diff`
// can carry no round record, so it pays for no input resolution at all and
// reports nothing: `step show` is a conductor's most-repeated read, and making
// every one of them resolve a whole input set to learn "no target" would tax
// the common case for the rare one. The same predicate the dispatch verbs'
// stale-target collector uses (staleTargetCandidates), for the same reason.
//
// It returns the empty pair — never a fabricated one — for a step with no
// resolvable round record. A vote panel seats itself on what this says; a
// plausible-looking sha invented here would seat judges on the wrong tree,
// which is worse than seating them on their own HEAD, because it is silent.
func stepTargetRef(
	tx *sql.Tx, sched *Scheduler, step *db.Step,
) (sha, worktree string, err error) {
	spec := materializedSpec(sched.defs[step.WorkflowID], step, sched.holdTally)
	if spec == nil || !consumesIssueDiff(spec) {
		return "", "", nil
	}
	artifacts, err := db.ListRunArtifactsTx(tx, step.RunID)
	if err != nil {
		return "", "", err
	}
	return resolvedTargetFor(tx, sched, step, spec, artifacts)
}

// runPreGates executes the step's pre-gates, one at a time, each result
// committing in its own small transaction.
//
// It runs OUTSIDE any transaction — that is the whole reason the claim was
// split into phases — and it returns the results for the context bundle.
//
// PG2/PG3: neither an unmatched pre-gate nor a FAILING one refuses the claim.
// The result rides in the bundle and the step's worker sees that its
// measurement did not run, or ran and failed. Refusing the claim would let an
// untrusted or failing command BLOCK WORK, which is a denial of service an
// issue author should not have — and §11.1's "measure-then-judge" says the
// judging is the step's job, which is exactly why the failure is data rather
// than a refusal.
func runPreGates(
	conn *sql.DB, e *Engine, step *db.Step, gates []workflow.Gate,
	targetSHA, workRoot string, nowMS int64,
) ([]PreGateResult, error) {
	out := make([]PreGateResult, 0, len(gates))

	// DKT-254: BIND THE TREE UNDER REVIEW, OR MEASURE NOTHING.
	//
	// This is decided ONCE for the step rather than per gate, because the
	// condition is a property of the step's inputs and re-deciding it per gate
	// would invite two gates of one step to answer differently.
	//
	// A step with no target at all (`targetSHA == ""` and no worktree) keeps
	// the shared checkout, and that is correct rather than tolerated: nothing
	// is under review yet, so the checkout IS the subject — a pre-gate on a
	// first implement step asking "is the tree clean before we start" is doing
	// exactly its job.
	var scratch scratchTree
	if targetSHA != "" || workRoot != "" {
		bound, s := bindablePreGateRoot(conn, step.RunID, targetSHA, workRoot)
		scratch = s
		defer scratch.release()
		if bound == "" {
			// Neither the resolved worktree nor a reconstruction could serve.
			// Every pre-gate of this step records `skipped` and NOTHING SPAWNS:
			// the shared checkout is a different tree, and a verdict measured
			// there is not evidence about this change however green it returns.
			//
			// The verdict is `skipped` and not `unmatched`: the trust entry is
			// fine and the command would have run. What is missing is the
			// SUBJECT. DKT-169 drew exactly this line for the swept-worktree
			// case; this is the same fact one step earlier.
			return unbindablePreGates(conn, step, gates, targetSHA, workRoot, nowMS)
		}
		workRoot = bound
	}

	for _, gate := range gates {
		commands, hashes, err := gateCommands(conn, step, gate)
		if err != nil {
			return nil, err
		}

		spec := GateSpec{
			Name: gate.Name, Source: gate.Source, Pre: true,
			Commands: commands, CommandHashes: hashes,
		}
		// The snapshotted scope rides along (DKT-63), exactly as it does on
		// the completion-side gates: a pre-gate measuring the tree deserves
		// the same narrowing as a gate judging it.
		scope, err := snapshotScope(conn, step.RunID, step.IssueID)
		if err != nil {
			return nil, err
		}
		sc := StepContext{
			Instance: step.Instance, RunID: step.RunID, IssueID: step.IssueID,
			Scope: scope, WorkRoot: workRoot,
		}

		var rows []GateResultRow
		if e == nil || e.Gates == nil {
			// No runner: a declared pre-gate records `unmatched` rather than
			// executing. Fail-closed, and the same direction as every other
			// unknown in this stage.
			rows = []GateResultRow{{
				Gate: gate.Name, Verdict: VerdictUnmatched,
				Reason: fmt.Sprintf(
					"pre-gate %q did not run: this invocation has no gate runner",
					gate.Name),
			}}
		} else {
			// The announcement precedes the spawn here for the same
			// at-least-once reason it does in the saga (§7.5 A1).
			if err := announcePreGate(conn, step, gate.Name, nowMS); err != nil {
				return nil, err
			}
			rows, err = runGate(e.Gates, spec, sc)
			if err != nil {
				return nil, err
			}
		}

		// Mark every row as a pre-gate result BEFORE recording, so PG4's
		// read-side filter has something to filter on.
		for i := range rows {
			rows[i].Pre = true
			// A row measured in a reconstruction says so (DKT-254). The verdict
			// is exactly as good as one from the original tree — same objects,
			// same content — but a reader should not have to infer that, and
			// the note also explains why the tree it names is not on disk.
			if scratch.Dir != "" {
				rows[i].Reason = withScratchNote(
					rows[i].Reason, scratchNote(targetSHA, scratch.Dir))
			}
		}

		if err := recordPreGateRows(conn, step, gate.Name, rows, nowMS); err != nil {
			return nil, err
		}

		for _, r := range rows {
			out = append(out, preGateResultOf(r))
		}
	}

	return out, nil
}

// unbindablePreGates records every pre-gate of a step as `skipped`, spawning
// nothing, when the tree under review cannot be bound (DKT-254 mode 1).
//
// NO `gate-started` EVENT IS EMITTED. That announcement exists because a spawn
// is about to happen and might not report back (§7.5 A1's at-least-once
// reason); here nothing is going to spawn, and announcing a start that is
// immediately followed by a skip would put a gate in the feed that never ran.
//
// The rows are still RECORDED. A pre-gate that declined to measure is a fact
// the step's worker needs — the bundle carries the reason, so a worker reading
// "ac-commands: pass" versus "ac-commands: skipped, the tree under review could
// not be bound" can tell an assurance from an absence. Recording nothing would
// leave the bundle looking like a step that declared no pre-gates at all.
func unbindablePreGates(
	conn *sql.DB, step *db.Step, gates []workflow.Gate,
	targetSHA, worktree string, nowMS int64,
) ([]PreGateResult, error) {
	out := make([]PreGateResult, 0, len(gates))
	reason := unbindableReason(targetSHA, worktree)
	for _, gate := range gates {
		rows := []GateResultRow{{
			Gate: gate.Name, Verdict: VerdictSkipped, Pre: true, Reason: reason,
		}}
		if err := recordPreGateRows(conn, step, gate.Name, rows, nowMS); err != nil {
			return nil, err
		}
		out = append(out, preGateResultOf(rows[0]))
	}
	return out, nil
}

// announcePreGate commits the `gate-started` event before the spawn.
func announcePreGate(conn *sql.DB, step *db.Step, gate string, nowMS int64) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("announcing pre-gate %s: %w", gate, err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventGateStarted, RunID: step.RunID, Instance: step.Instance,
		IssueID: step.IssueID, Data: gate, AtMS: nowMS,
	}); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the pre-gate announcement: %w", err)
	}
	return nil
}

// recordPreGateRows commits one pre-gate's results in their own transaction.
func recordPreGateRows(
	conn *sql.DB, step *db.Step, gate string, rows []GateResultRow, nowMS int64,
) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("recording pre-gate %s: %w", gate, err)
	}
	defer tx.Rollback()

	if err := recordGateRows(tx, step, gate, rows, nowMS); err != nil {
		return err
	}
	// The SAME event writer the saga's gates use (§7.6.1 phase 2, PG1). This
	// path once emitted `gate-unmatched` alone, which left a passing pre-gate
	// announced by `gate-started` and closed by nothing — and dropped
	// `gate-rerun` for the flaky attempts §7.5 A3 records at ordinal > 0.
	if err := recordGateEvents(tx, step, gate, rows); err != nil {
		return err
	}
	return tx.Commit()
}

// PreGateResult is a §11.4-shaped gate result as it rides in the context
// bundle (§7.6.3, amendment A5).
//
// It is a distinct type from the row because the BUNDLE's shape is a wire
// contract: `argv` and `exit` are pointers so an unmatched pre-gate serializes
// them as null rather than as `[]` and `0`, which is the same honesty the table
// enforces (§4.2).
type PreGateResult struct {
	Gate       string   `json:"gate"`
	Argv       []string `json:"argv"`
	Exit       *int     `json:"exit"`
	DurationMS int64    `json:"duration_ms"`
	Output     string   `json:"output"`
	Truncated  bool     `json:"truncated"`
	Verdict    string   `json:"verdict"`
	Reason     string   `json:"reason,omitempty"`
}

func preGateResultOf(r GateResultRow) PreGateResult {
	return PreGateResult{
		Gate: r.Gate, Argv: r.Argv, Exit: r.Exit, DurationMS: r.DurationMS,
		Output: r.Output, Truncated: r.Truncated, Verdict: r.Verdict,
		Reason: r.Reason,
	}
}

// refreshed2ExpiresMS is the lease the caller actually receives after the
// refresh: a FULL ttl ahead of transaction B's own time, not of the CAS's
// (§7.6.1.1 LR1).
func refreshed2ExpiresMS(nowMS, ttlMS int64) int64 {
	return nowMS + ttlMS
}

// unbindableReason names WHY the tree could not be bound, distinguishing the
// three ways it happens so an operator knows which remedy applies.
//
// The distinctions are not cosmetic. A missing OBJECT means the producing
// step's commit never reached this machine — the remedy is to fetch it. A
// present object with a swept worktree means reconstruction was tried and
// failed, which points at the checkout rather than at the run. A step with a
// worktree path and no sha at all is a producer that declared a tree and no
// commit, which is a workflow-authoring problem. One sentence for all three
// would send every operator down the same wrong path two times in three.
func unbindableReason(targetSHA, worktree string) string {
	switch {
	case targetSHA != "" && worktree != "":
		return fmt.Sprintf(
			"the step's worktree %s no longer exists and %s could not be "+
				"reconstructed from the object database; not measuring the "+
				"shared checkout in its place",
			worktree, targetSHA)
	case targetSHA != "":
		return fmt.Sprintf(
			"the tree under review (%s) is not checked out anywhere this step "+
				"can reach and the commit is not in this repository's object "+
				"database; not measuring the shared checkout in its place",
			targetSHA)
	default:
		return fmt.Sprintf(
			"the step's worktree %s no longer exists and its producer declared "+
				"no commit to reconstruct from; not measuring the shared "+
				"checkout in its place",
			worktree)
	}
}
