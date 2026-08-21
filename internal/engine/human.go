package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// indefiniteArticle picks "a"/"an" for a step kind in an error message
// (DKT-104: "a executor step" read as broken grammar an operator noticed
// before the substance of the refusal). The step-kind vocabulary is closed
// (executor, action, human, vote), so a first-letter-is-a-vowel check is
// exact, not a general-purpose English heuristic.
func indefiniteArticle(word string) string {
	if word == "" {
		return "a"
	}
	switch word[0] {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return "an"
	default:
		return "a"
	}
}

// Human and vote steps, and the `waiting-human` resolutions — TDD §6.15, §6.10.
//
// `type="human"` and `type="vote"` steps NEVER claim, never mint a token, and
// never run the saga: the saga's stage 0 authorizes a holder, and these steps
// have none. Their path through the §6.2 machine is short:
//
//	human  pending -> ready (computed) -> approve ⇒ done
//	                                   -> reject  ⇒ routed per EFFECTIVE on_fail
//	vote   pending -> ready -> PARKS at S3; only `step resolve` moves it
//
// Vote EXECUTION is deferred to S4 explicitly (§1's scope table). §2 drives
// votes as gates, and the gate machinery lands at stage 4 — so splitting votes
// from gates would ship half a feature. What S3 owns is V14, expansion, and the
// park.

// The `step resolve --as` vocabulary (§6.10, §2).
const (
	// ResolveRetry RESETS THE RETRY BUDGET for the step instance (§2: "retry =
	// attempts reset") by moving `attempt_base` to the current attempt —
	// `attempt` itself stays monotonic, because it is the usage ledger's key
	// half (DKT-86/DKT-90). This is a different counter from the issue-level
	// `attempt` v6 declared monotonic — claims-leases §5 anticipated exactly
	// this. The step's is a live retry budget against `max_attempts`; the
	// issue's is a permanent trail. Both statements stay true because they are
	// about different rows.
	ResolveRetry = "retry"
	// ResolveSkip routes the step `skipped`.
	ResolveSkip = "skip"
	// ResolveAbandonIssue abandons the issue within this run.
	ResolveAbandonIssue = "abandon-issue"
	// ResolveOverridePass passes the step as though its gates had allowed it —
	// the operator taking responsibility for a decision the engine would not
	// make. It records the GENERIC RoutingPass and does NOT evaluate the
	// step's threshold (DKT-470): a threshold that would have interposed
	// another step over the accepted payload is not consulted, and that
	// interposed step is skipped unconditionally. OverridePassSkipsInterposedTargets
	// names the steps this will skip, for a caller to warn with before this
	// commits.
	ResolveOverridePass = "override-pass"
	// ResolveFixRound authorizes ONE more fix loop for the issue and enters
	// it, minting a fresh fix+review round (DKT-237).
	//
	// It exists because exhausting `max_fix_loops` parked the issue with no
	// way back in. After HRN-26's third round verify-ac read 7/14 acceptance
	// criteria unmet and design-qa held 2 blockers, and the workflow scheduled
	// no further round — so the fix was built OUTSIDE the engine: a
	// general-purpose agent, a 1,128-insertion commit cherry-picked with no
	// judge review as a step, and ~100,923 output / 21.9M cache-read tokens in
	// no ledger. The engine offering no sanctioned re-entry is what made that
	// the reasonable move.
	//
	// It is DISTINCT FROM RETRY, and the distinction is the whole point.
	// `retry` re-runs the parked step — the check that reported the problem —
	// which answers the same question again. This says the problem is real and
	// authorizes another round of WORK on it: a new fix body, a new review,
	// judged like every other round.
	ResolveFixRound = "fix-round"
	// ResolveRerunGates RE-RUNS A COMPLETED STEP'S GATES without re-executing
	// the step (DKT-259).
	//
	// It exists because the only lever for "the gate was wrong, the work was
	// fine" was `retry`, which re-executes everything. Most of the RUN-13 epoch
	// retries were exactly this case — a gate that failed because a trust entry
	// was missing or an environment was broken, fixed out of band, with the
	// step's own output never in question. Paying a full re-execution for that
	// is expensive, and it is also DESTRUCTIVE: the re-execution diffs a tree
	// that already contains the work, which is how RUN-13 STEP-132's
	// `issue.diff` came to be replaced by 0 bytes.
	//
	// The mechanism is the saga's own: rewind `saga_stage` to `recorded` — the
	// point at which the artifact is in and the gates have not run — and
	// resume. Every gate stage re-runs in declared order and routing follows,
	// exactly as they would have the first time. The step never returns to
	// `pending`, so nothing re-offers it and no worker re-executes it, and the
	// recorded artifact is untouched.
	//
	// It is NOT `retry` with a flag. `retry` says "the work may be wrong, do it
	// again"; this says "the work stands, measure it again". They differ in
	// what they preserve, which is the only thing an operator picking between
	// them cares about.
	ResolveRerunGates = "rerun-gates"
)

// resolveValues is the closed vocabulary, in declaration order, for the error
// message a bad `--as` produces.
var resolveValues = []string{
	ResolveRetry, ResolveSkip, ResolveAbandonIssue, ResolveOverridePass,
	ResolveFixRound, ResolveRerunGates,
}

// DecideStep is `step approve` and `step reject` — §6.10's human-gate verbs.
//
// NO TOKEN. A human gate is not claimed, so there is no lease to authorize
// against; the authority is the operator's access to the repository, which is
// the same authority `issue close` has always relied on.
func (e *Engine) DecideStep(conn *sql.DB, stepID int, approve bool, note string, nowMS int64) error {
	return e.DecideStepValue(conn, stepID, approve, note, "", nowMS)
}

// DecideStepValue is DecideStep carrying `--value` (DKT-42): an operator's
// corrected value for a held cluster's aggregated field, validated against the
// pinned schema's declared enum and applied only on approve of a materialized
// held step. Every other decision passes "" and is DecideStep unchanged.
func (e *Engine) DecideStepValue(
	conn *sql.DB, stepID int, approve bool, note, value string, nowMS int64,
) error {
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return err
	}

	// A MATERIALIZED held step minted as a vote (see holdTally) is decided by
	// approve/reject ONCE THE TALLY HAS PARKED IT, and by nothing else before
	// then.
	//
	// The panel answers first and the operator answers last, which is the whole
	// shape of the escalation: a tally that reached its threshold routes the
	// aggregate itself, and a tally that did not parks the step `waiting-human`
	// so the question passes to a person — with the SAME verbs, including
	// `--value`, that a human-minted hold has always accepted.
	//
	// The check is on STATUS rather than on the vote's outcome because status is
	// what routeVoteStep wrote: before the park, the proposal is still open and
	// approving here would be an operator overruling a vote still being cast —
	// the panel's turn taken away mid-question, and an orphaned open proposal
	// left behind. `step resolve` remains the verb for a run that must move past
	// an open vote, which is what R11's vote clause already provides.
	if step.Materialized && step.Kind == workflow.TypeVote {
		if step.Status != db.StepWaitingHuman {
			return validationErr(
				"step %s is a held cluster decided by vote and is %s, not %s: "+
					"the vote has not finished. approve and reject apply once a "+
					"vote that did not pass parks it for you; to move the run past "+
					"an open vote, use `docket step resolve %s --as %s`",
				step.Instance, step.Status, db.StepWaitingHuman,
				step.Instance, strings.Join(resolveValues, "|"))
		}
		return e.decideMaterializedStep(conn, step, approve, note, value, nowMS)
	}

	// R10: approve/reject on a non-`human` step is VALIDATION_ERROR. The
	// refusal names the step's actual class, because an operator reaching for
	// `approve` on an executor step has usually mistaken which step is
	// blocking.
	//
	// DKT-104: when the step is PARKED waiting-human, the refusal names the
	// verb that actually moves it — `step resolve --as` — rather than only
	// the one that doesn't apply. An operator with approval in hand for a
	// parked executor step reads "not a human gate" and has no next move
	// without a help dig; naming `resolve` here saves that round-trip.
	if step.Kind != workflow.TypeHuman {
		if step.Status == db.StepWaitingHuman {
			return validationErr(
				"step %s is %s %s step, not a `type=\"human\"` gate, and is "+
					"parked waiting-human; use `docket step resolve %s --as "+
					"%s` instead of approve/reject",
				step.Instance, indefiniteArticle(step.Kind), step.Kind,
				step.Instance, strings.Join(resolveValues, "|"))
		}
		return validationErr(
			"step %s is %s %s step, not a `type=\"human\"` gate; "+
				"approve and reject apply only to human gates",
			step.Instance, indefiniteArticle(step.Kind), step.Kind)
	}

	// M-c (§6.4, §7.7.3): a MATERIALIZED held step resumes ANOTHER step's saga,
	// which the S3 path had no reason to do. It is branched here rather than
	// folded in below because the consequence lands somewhere else entirely:
	// approving a declared gate finishes that gate, and approving a held
	// cluster un-defers the aggregate step's routing.
	if step.Materialized {
		return e.decideMaterializedStep(conn, step, approve, note, value, nowMS)
	}

	// `--value` is a held-cluster correction: it sets the aggregated field of
	// the cluster the materialized step decides. A DECLARED human gate has no
	// payload of its own to correct, so accepting the flag there would record
	// a value that reaches nothing.
	if value != "" {
		return validationErr(
			"step %s is a declared human gate, not a materialized held cluster; "+
				"--value corrects a held cluster's aggregated field and applies "+
				"nowhere else", step.Instance)
	}

	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return err
	}
	spec := workflow.StepByName(defs[step.WorkflowID], step.StepName)
	if spec == nil {
		return validationErr("step %s: %q is not a step of its pinned workflow",
			step.Instance, step.StepName)
	}

	if db.StepTerminal(step.Status) {
		return conflictErr("step %s is already %s", step.Instance, step.Status)
	}

	// approve ⇒ done. reject ⇒ routed per the EFFECTIVE on_fail, which V13a
	// guarantees exists and V13 guarantees is not `waiting-human` — a human
	// gate that parked its own rejections would wait on the resolution of the
	// thing that just rejected.
	routing := RoutingPass
	event := EventStepApproved
	if !approve {
		routing = spec.EffectiveOnFail()
		event = EventStepRejected
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the decision: %w", err)
	}
	defer tx.Rollback()

	// DKT-168: a rejection whose effective on_fail is `fix-loop` enters the
	// loop here, in the decision's own transaction — the shipped standard-dev
	// template routes its commit-gate rejections exactly this way, and before
	// this the routing string was recorded with no fix step ever created.
	var loop *LoopOutcome
	routing, loop, err = applyFixLoop(tx, step, defs[step.WorkflowID], routing, nowMS)
	if err != nil {
		return err
	}
	if loop != nil && loop.Reason != "" {
		if note != "" {
			note += "; " + loop.Reason
		} else {
			note = loop.Reason
		}
	}
	status := statusForRouting(routing)

	if err := db.SetStepRoutingTx(tx, step.ID, routingRecord(routing, note), status, nowMS); err != nil {
		return err
	}
	if err := recordEvent(tx, eventRecord{
		Kind: event, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID, Data: note,
	}); err != nil {
		return err
	}
	if err := reconcileIssueAndRun(tx, step, spec, routing, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveStep is `step resolve --as retry|skip|abandon-issue|override-pass` —
// §6.10's `waiting-human` resolutions.
func (e *Engine) ResolveStep(
	conn *sql.DB, stepID int, as, note string, nowMS int64,
) error {
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return err
	}

	if !contains(resolveValues, as) {
		return validationErr("--as must be one of %v, got %q", resolveValues, as)
	}

	// R11: `resolve` on a step that is not `waiting-human` is
	// VALIDATION_ERROR — with ONE exception, stated rather than special-cased:
	// a `vote` step waits on ITS VOTERS, and nobody but them can advance it.
	// `step resolve` is explicitly the operator's way past one — the run must
	// not be hostage to a quorum that never arrives — so it is offered whatever
	// the step's status.
	resolvable := step.Status == db.StepWaitingHuman || step.Kind == workflow.TypeVote
	if !resolvable {
		return validationErr(
			"step %s is %s, not %s; resolve applies to parked steps "+
				"(and to `type=\"vote\"` steps, whose voters it moves a run past)",
			step.Instance, step.Status, db.StepWaitingHuman)
	}

	// `retry` CANNOT move a step parked by a REJECTED HOLD, so it is
	// refused rather than accepted as a silent no-op.
	//
	// The mechanism: a rejected hold routes the ROUTING step per its effective
	// `on_fail`, skipping the threshold (§7.7.3). `retry` resets the attempt
	// counter and re-runs the aggregate — but the attempt counter was never
	// what blocked it. `heldDecision` re-reads the SAME materialized
	// `reconcile-held@N` step, which is still terminal with a non-pass
	// routing, so `approved` is false again and the step routes to `on_fail`
	// again. Observed in RUN-2 as step-resolved retry -> step-recorded ->
	// step-routed waiting-human -> run-paused, with no operator input in
	// between and no new gate materialized.
	//
	// The park is real and the routing reason is honest; what was wrong was
	// offering the operator a remedy that cannot address the named cause. An
	// operator reaching for the least destructive option paid a dispatch cycle
	// to land exactly where they started.
	// The SAME refusal, one step over: a held cluster parked by a vote that did
	// not pass cannot be retried either, and for the identical reason. Retry
	// resets the attempt counter and returns the step to `pending`; the next
	// `next` re-reads the SAME proposal — the idempotency key is (run,
	// instance), so no second one is opened — sees the same finished tally, and
	// parks it again. An operator reaching for the least destructive option
	// would pay a poll cycle to land exactly where they started.
	//
	// The remedy here is different from the aggregate's, and better: the
	// question is still open and the operator can simply answer it, which is
	// what the park exists for.
	if as == ResolveRetry && step.Materialized &&
		step.Kind == workflow.TypeVote && step.Status == db.StepWaitingHuman {
		return validationErr(
			"step %s cannot be retried: it is parked because its vote did not "+
				"pass, and retry resets the retry budget, which is not what is "+
				"blocking it — the same tally would be read again. Answer it "+
				"instead: `docket step approve %s` (with --value to correct the "+
				"computed value) or `docket step reject %s`",
			step.Instance, step.Instance, step.Instance)
	}

	if as == ResolveRetry {
		rejected, heldInstance, err := parkedByRejectedHold(conn, step)
		if err != nil {
			return err
		}
		if rejected {
			return validationErr(
				"step %s cannot be retried: it was parked because the held "+
					"clusters on %s were REJECTED, and retry resets the retry "+
					"budget, which is not what is blocking it — the rejection "+
					"is sticky, so re-running would route here again. Use "+
					"--as %s to accept the step as passing, --as %s to route it "+
					"skipped, or --as %s to drop the issue from this run",
				step.Instance, heldInstance,
				ResolveOverridePass, ResolveSkip, ResolveAbandonIssue)
		}
	}

	// A MATERIALIZED held step resolved here ends ANOTHER step's wait, exactly
	// as `approve`/`reject` on one does — so the aggregate it gates must be
	// resumed once this commits, or the question is answered and the run sits
	// `gated` at `held` with nothing left in the system able to move it.
	//
	// This became reachable only when holds could be minted as votes. A
	// human-minted hold is created `pending` with kind `human`, which satisfies
	// neither arm of the `resolvable` test above, so `resolve` was refused on
	// one and this path could not be entered. A vote-minted hold parked by a
	// failed tally is `waiting-human`, and `resolve` is offered on it like any
	// other parked step.
	//
	// Read BEFORE the transaction opens: routingStepOf takes the pooled
	// connection, which inside the transaction would deadlock. `retry` is
	// excluded because it returns the step to `pending` rather than deciding
	// it — there is nothing for the aggregate to resume on.
	var heldRoutingStep *db.Step
	if step.Materialized && as != ResolveRetry {
		if heldRoutingStep, err = routingStepOf(conn, step); err != nil {
			return err
		}
	}

	// The pinned spec, for the reconcile's interposed-target read — loaded
	// before the transaction for the same pooled-connection reason as
	// routingStepOf above. Nil for a materialized step, whose minted name the
	// definition never declares; the reconcile treats nil as "no thresholds".
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return err
	}
	spec := workflow.StepByName(defs[step.WorkflowID], step.StepName)

	// `rerun-gates` needs gates to re-run (DKT-259). A step that declares none
	// would rewind to `recorded`, find nothing to measure, and route again on
	// the same evidence — an expensive no-op that looks like it did something.
	// Saying so names the alternative, because an operator who reached for this
	// verb has a real problem and needs the verb that solves it.
	if as == ResolveRerunGates {
		if spec == nil || len(completionGates(spec)) == 0 {
			return validationErr(
				"step %s declares no gates to re-run, so --as %s would rewind "+
					"it to routing and decide on the same evidence. If the "+
					"step's own output is what needs redoing, that is --as %s",
				step.Instance, ResolveRerunGates, ResolveRetry)
		}
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the resolution: %w", err)
	}
	defer tx.Rollback()

	var routing, status string
	switch as {
	case ResolveRetry:
		// The retry BUDGET resets, the step returns to `pending`, and the
		// saga's resume point is cleared: a retry re-does the work, so a
		// half-finished saga from the previous try must not resume under it.
		// `attempt` itself carries forward (DKT-86/DKT-90) — it is the usage
		// ledger's key half, so the re-execution's claim mints a FRESH attempt
		// number and its usage records beside the first execution's instead of
		// colliding with it.
		if err := db.ResetStepRetryBudgetTx(tx, step.ID, nowMS); err != nil {
			return err
		}
		// THE LEASE IS RELEASED (DKT-259). Without this the step went back to
		// `pending` still owned, which is a contradiction `claimPredicate`
		// resolves in the worst direction: no new claimant can take it, and the
		// ORIGINAL holder's token is still valid, so the re-execution records
		// without re-claiming. `attempt` increments only at claim, so both
		// executions land on one attempt number — RUN-13 STEP-132 ran twice
		// with two gate rounds and two artifact sets, `run report` said
		// `attempts: 1`, and one execution's usage was permanently
		// unrecordable because `UNIQUE(step_id, attempt, unit)` was taken.
		//
		// Releasing is what makes the comment below true rather than aspirational.
		if err := db.ReleaseStepLeaseTx(tx, step.ID, nowMS); err != nil {
			return err
		}
		if err := db.ClearStepStartTx(tx, step.ID); err != nil {
			return err
		}
		if err := db.AdvanceSagaTx(tx, step.ID, step.SagaStage, "", nowMS); err != nil &&
			!errors.Is(err, db.ErrSagaStageMoved) {
			return err
		}
		routing, status = ResolveRetry, db.StepPending
	case ResolveSkip:
		routing, status = workflow.OnFailSkip, db.StepSkipped
	case ResolveAbandonIssue:
		routing, status = workflow.OnFailAbandonIssue, db.StepFailedRouted
	case ResolveOverridePass:
		routing, status = RoutingPass, db.StepDone
	case ResolveFixRound:
		// The GRANT and the loop entry are one transaction (DKT-237). A grant
		// raised without a round entered would leave an issue authorized for
		// work nothing scheduled; a round entered without the grant recorded
		// would leave the bound looking untouched to every later reader.
		if _, err := db.GrantLoopTx(tx, step.RunID, step.IssueID); err != nil {
			return validationErr("%v", err)
		}
		// AUTHORIZED (DKT-340). The operator has read whatever park stands
		// and asked for the round; the non-convergence refusal must not fire
		// against the very verb that park names as its way out.
		outcome, err := EnterLoopAuthorized(tx, step, defs[step.WorkflowID], nowMS)
		if err != nil {
			return err
		}
		if !outcome.Entered {
			// The grant did not clear the bound. That is possible only if the
			// counter moved under this resolution, and reporting it is better
			// than reporting a round that was not minted.
			return conflictErr(
				"step %s: authorizing one more fix loop did not open a round: %s",
				step.Instance, outcome.Reason)
		}
		// The parked step itself is SUPERSEDED, not passed and not failed: the
		// question it asked is being answered by the new round's own work, and
		// calling it done would assert a verdict nobody reached.
		routing, status = workflow.OnFailFixLoop, db.StepSuperseded
	case ResolveRerunGates:
		// REWIND, DO NOT RE-EXECUTE (DKT-259). `recorded` is the saga stage at
		// which the artifact is in and no gate has run, so resuming from it
		// re-runs every completion gate in declared order and then routes —
		// exactly the first pass, minus the work.
		//
		// The step goes to `gated`, which is the status it carried through the
		// saga the first time, and NOT to `pending`. That difference is the
		// whole verb: `pending` re-offers the step, a worker claims it, and the
		// work is done again over a tree that already contains it — which is
		// how RUN-13 STEP-132's issue.diff was replaced by 0 bytes.
		//
		// The recorded routing is CLEARED rather than kept. The step is about
		// to be routed again, and leaving the old verdict standing until the
		// new one lands would let a reader in that window see a decision that
		// is no longer in force.
		if err := db.AdvanceSagaTx(tx, step.ID, "", db.SagaRecorded, nowMS); err != nil {
			if errors.Is(err, db.ErrSagaStageMoved) {
				return conflictErr(
					"step %s is already inside a saga stage; its gates are "+
						"running or about to, so there is nothing to rewind",
					step.Instance)
			}
			return err
		}
		routing, status = "", db.StepGated
	}

	if err := db.SetStepRoutingTx(tx, step.ID, routingRecord(routing, note), status, nowMS); err != nil {
		return err
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepResolved, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID, Data: as,
	}); err != nil {
		return err
	}
	// An `override-pass` on a held cluster IS the approve-computed answer — it
	// records `done` with a `pass` routing, which is exactly what heldDecision
	// reads back as an approval — so it must leave the same record `approve`
	// leaves. Marking the routing AND NOT the payload would let the run proceed
	// over a cluster the payload still reports as unresolved, which is the
	// silent half-write DKT-84 removed from the other path.
	//
	// The other two resolutions decide the cluster AGAINST the computed value
	// (`skip` routes it away, `abandon-issue` drops the issue), so there is no
	// endorsement to record and the payload correctly keeps `held`.
	if heldRoutingStep != nil && as == ResolveOverridePass {
		res := heldResolution{Element: -1, Note: note}
		if element, ok := heldClusterElementOf(
			step.Instance, heldStepInstance(heldRoutingStep)); ok {
			res.Element = element
		}
		if err := resolveHeldPayload(tx, heldRoutingStep, res, nowMS); err != nil {
			return err
		}
	}
	if err := reconcileIssueAndRun(tx, step, spec, routing, nowMS); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the resolution: %w", err)
	}

	// A SEPARATE transaction, for decideMaterializedStep's stated reason: the
	// routing stage owns its own four-table commit, and a crash between the two
	// leaves a resolved question and an unrouted step, which the next
	// invocation finishes.
	if heldRoutingStep != nil {
		return e.ResumeSaga(conn, heldRoutingStep.ID, nowMS)
	}
	// A SEPARATE transaction, for the same reason: the rewind is committed, so
	// a crash before the gates run leaves a step at `recorded` that the next
	// `step resume` finishes — which is the ordinary at-least-once shape rather
	// than a wedged step.
	if as == ResolveRerunGates {
		return e.ResumeSaga(conn, step.ID, nowMS)
	}
	return nil
}

// OverridePassSkipsInterposedTargets names the interposed step(s) an
// `override-pass` on stepID will never route to (DKT-470).
//
// ResolveOverridePass always records the GENERIC RoutingPass, whatever the
// step's threshold would have decided over the accepted payload — it does not
// evaluate the threshold at all. For a step with no threshold, or a threshold
// with no step-name routing target, that generic pass IS the right answer and
// there is nothing to warn about. For a step whose threshold interposes
// another step (§11.2's "route to a step name"), it is a silent bypass: the
// interposed step is unconditionally skipped as soon as this resolution
// commits (reconcile.go's skipUnroutedTargets), REGARDLESS of whether the
// threshold's condition was actually met. RUN-36/VPL-153 hit exactly this —
// override-pass on an aggregate whose accepted payload had 17 findings
// `>= high`, against a threshold that would have routed to `security-vote` on
// that condition, instead silently skipped the vote with no warning that this
// is what override-pass does.
//
// It answers a question about the step's DEFINITION, which the resolution it
// precedes does not change, so it is independent of ResolveStep and safe to
// call and print before that call commits anything — the operator sees the
// blast radius of what they are about to approve.
func OverridePassSkipsInterposedTargets(conn *sql.DB, stepID int) []string {
	step, err := db.GetStep(conn, stepID)
	if err != nil {
		return nil
	}
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil
	}
	spec := workflow.StepByName(defs[step.WorkflowID], step.StepName)
	if spec == nil {
		return nil
	}
	targets := workflow.ThresholdTargets(spec.Threshold)
	if len(targets) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"override-pass on %s records a generic %q routing and does not "+
			"evaluate its threshold — interposed step(s) %s will NOT be "+
			"routed to as a result, whatever the (unevaluated) payload would "+
			"have decided; resolve them directly if their condition should "+
			"still apply",
		step.Instance, RoutingPass, strings.Join(targets, ", "))}
}

// FailStep is `step fail` — the explicit-failure counterpart to `complete`.
//
// It consumes an attempt and routes per `on_fail` when attempts are EXHAUSTED,
// per the status machine (§6.2: "attempts exhausted ⇒ waiting-human", via the
// step's effective routing). Below the limit the step returns to `pending` and
// is re-offered, which is the ordinary retry path.
//
// metadata is `--metadata` (parity with `complete`'s): the same
// opaque KV bag, merged over the step's own with the same mergeMetadata and
// written with the same db.SetStepMetadataTx stageZero uses — a shared write
// path, not a duplicated one. It SURVIVES INTO A RETRY (§1.6): a failed
// attempt's bag merges into the step's row like any other, so the next
// attempt's completion or failure overlays on top of it. A worker that
// reports why it failed has produced the most valuable metadata in the run,
// and discarding it at retry would lose exactly the diagnostic an operator
// wants.
//
// Validated BEFORE the transaction opens, same as stage zero's C5 ordering: a
// refusal costs the caller nothing and consumes no attempt.
func (e *Engine) FailStep(conn *sql.DB, stepID int, token, note, metadata string, nowMS int64) error {
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return err
	}

	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return err
	}
	spec := workflow.StepByName(defs[step.WorkflowID], step.StepName)
	if spec == nil {
		return validationErr("step %s: %q is not a step of its pinned workflow",
			step.Instance, step.StepName)
	}

	// The same cap and shape checks stageZero applies to `--metadata`,
	// pre-transaction so a refusal writes nothing and consumes no attempt.
	// The remedy differs from stageZero's because the channels differ: `fail`
	// offers no `--artifact-file`/`--payload-file` (C14).
	if err := validateFailMetadataSize(metadata); err != nil {
		return err
	}
	if _, err := DecodeMetadataBag(metadata, "metadata"); err != nil {
		return err
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the failure: %w", err)
	}
	defer tx.Rollback()

	// The same three-way authorize every lease-mediated verb passes through,
	// so R1-R4 hold identically here and at `complete`.
	if _, err := db.AuthorizeStepTx(tx, step.ID, token, nowMS); err != nil {
		return err
	}

	// The failing worker's opaque KV bag, merged over the step's own
	// (parity with `complete`). It lands here, inside the one
	// transaction, before either branch below commits — a step that recorded
	// a failure and dropped the bag reported with it is the same silent
	// half-write removed at `complete`.
	if metadata != "" {
		merged, err := mergeMetadata(step.Metadata, metadata)
		if err != nil {
			return err
		}
		if err := db.SetStepMetadataTx(tx, step.ID, merged, nowMS); err != nil {
			return err
		}
	}

	// `attempt` IS NOT BUMPED HERE. It counts CLAIMS, and the claim that led to
	// this failure already counted itself (claims-leases §5: "claims made
	// against this issue, ever"; §6.3's re-offer "with attempt++" is satisfied
	// by the re-claim's own increment). `ReapStepTx` states the same rule from
	// the other side — attempt is left alone on reap because "incrementing
	// again here would double-count a single death".
	//
	// A bump here made the counter measure claims PLUS failures, so
	// `max_attempts = N` exhausted after N-1 failures and the retry branch below
	// was unreachable at N = 2 (E-8).
	//
	// The budget counts from `attempt_base` (v16): `attempt` is monotonic for
	// the ledger's sake, and `step resolve --as retry` refreshes the budget by
	// moving the base rather than zeroing the counter.
	attempt := step.Attempt - step.AttemptBase

	max := 0
	if spec.MaxAttempts != nil {
		max = *spec.MaxAttempts
	}

	// A step with NO declared `max_attempts` retries indefinitely: core ships
	// no default limit, the same reasoning as R5's unbounded class. An
	// operator who wants a bound declares one.
	exhausted := max > 0 && attempt >= max

	if !exhausted {
		// Back to the pool, lease cleared, schedule-to-close clock reset.
		// ReapStepTx is the MECHANISM here, not the classification: this claim
		// ended in a recorded failure, so it counts into `failed_attempts`,
		// never `reaped_claims` (DKT-490) — the breakdown is what lets a
		// dispatcher escalate on measured failures and shrug at silences.
		if err := db.ReapStepTx(tx, step.ID, nowMS); err != nil {
			return err
		}
		if err := db.MarkStepAttemptFailedTx(tx, step.ID, nowMS); err != nil {
			return err
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventStepFailed, RunID: step.RunID,
			Instance: step.Instance, IssueID: step.IssueID, Data: note,
		}); err != nil {
			return err
		}
		// AND IT IS NARRATED (DKT-380). This branch does not route, so it
		// never reaches reconcileIssueAndRun and nothing else was going to
		// write the trail: an issue whose step had failed twice still showed
		// the original "claimed" comment as its most recent line, and the only
		// record that anything had gone wrong was in the event feed.
		//
		// The retried attempt is exactly the case worth narrating. An
		// exhausted failure ends up somewhere an operator can see —
		// `waiting-human`, a fix round, a terminal routing — and a failure
		// that silently re-offers ends up looking like a step that is merely
		// slow.
		return commit(tx, commentEngineEvent(tx, step.IssueID,
			failureNote(step.Instance, note, attempt, max), nowMS))
	}

	routing := spec.EffectiveOnFail()

	// DKT-168: an exhausted step whose on_fail is `fix-loop` enters the loop
	// rather than recording a routing nothing consumes.
	var loop *LoopOutcome
	routing, loop, err = applyFixLoop(tx, step, defs[step.WorkflowID], routing, nowMS)
	if err != nil {
		return err
	}
	if loop != nil && loop.Reason != "" {
		if note != "" {
			note += "; " + loop.Reason
		} else {
			note = loop.Reason
		}
	}
	status := statusForRouting(routing)

	if err := db.RetireStepTokenTx(tx, step.ID); err != nil {
		return err
	}
	// The exhausting failure counts like every other (DKT-490): the routed
	// step's row still answers "how many attempts failed" — which is exactly
	// what an operator reading the park, or a `step show` beside it, asks.
	if err := db.MarkStepAttemptFailedTx(tx, step.ID, nowMS); err != nil {
		return err
	}
	if err := db.SetStepRoutingTx(tx, step.ID, routingRecord(routing, note), status, nowMS); err != nil {
		return err
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepFailed, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID, Data: note,
	}); err != nil {
		return err
	}
	// Narrated before the reconciliation, so the trail reads in the order the
	// facts occurred: the step failed, and THEN the routing that failure
	// produced moved the issue.
	if err := commentEngineEvent(tx, step.IssueID,
		failureNote(step.Instance, note, attempt, max), nowMS); err != nil {
		return err
	}
	if err := reconcileIssueAndRun(tx, step, spec, routing, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

// failureNote is one line of the activity trail for a failed step: what failed,
// why if a reason was given, and whether the step is coming back.
//
// `attempt` is the attempt that FAILED, not the next one: the counter is
// `step.Attempt - step.AttemptBase` and a claim increments it before the work
// starts, so the number already names the try that just ended.
//
// `max == 0` is the unbounded case — core ships no default attempt limit — so
// the count is stated without a denominator rather than as "attempt 3 of 0".
func failureNote(instance, note string, attempt, max int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s failed on attempt %d", instance, attempt)
	if max > 0 {
		fmt.Fprintf(&b, " of %d", max)
	}
	if note != "" {
		fmt.Fprintf(&b, ": %s", note)
	} else {
		b.WriteString(".")
	}
	return b.String()
}

// commit closes the transaction when the preceding step succeeded, so a branch
// whose last action is one fallible write does not need a three-line tail.
func commit(tx *sql.Tx, err error) error {
	if err != nil {
		return err
	}
	return tx.Commit()
}

// HeartbeatStep extends a live lease held by token (§6.10). It does not touch
// `attempt`: a heartbeat is not a new claim.
func HeartbeatStep(
	conn *sql.DB, stepID int, token string, nowMS int64,
) (*model.Lease, error) {
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return nil, notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return nil, err
	}

	ttls, err := loadTTLConfig(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil, err
	}

	limit := workflow.Limit{}
	if def := defs[step.WorkflowID]; def != nil {
		limit = def.Limits[step.Class]
	}
	ttl := ttls.forClass(limit, step.Class)

	return db.HeartbeatStep(conn, stepID, token, ttl.Milliseconds(), nowMS)
}

// contains reports slice membership, for the closed `--as` vocabulary.
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
