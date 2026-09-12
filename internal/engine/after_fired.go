package engine

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `after_fired` (DKT-1085): a step that runs ONLY IF a named predecessor fired.
//
// The gap it closes. An interposed gate — a step some `threshold` routes to by
// name (§11.2) — ends `skipped` on every round its routing step resolved
// elsewhere (skipUnroutedTargets), and `skipped` is terminal, so an ordinary
// `after = ["security-vote"]` successor is released by the skip exactly as by
// an approval (J1). `when` reads issue kind and labels only (V22) and cannot
// say "the gate fired"; a second step-name routing beside the vote on the same
// threshold would be evaluated first (ThresholdOrder sorts step targets) and
// skip the vote itself. The corpus's `drain-highs` therefore ran on every
// skipped-vote round: 3 of 81 measured, one sonnet spawn each for an empty
// report.
//
// The rule. A step declaring `after_fired = [g, ...]` is terminalized
// `skipped` when any named predecessor DID NOT FIRE — every instance of it, at
// the ordinal R3 resolves (predecessorInstancesIn), ended `skipped` — and it
// is skipped IN THE SAME TRANSACTION as the predecessor's own skip. The
// propagation is transitive by construction: the sweep runs to a fixed point,
// so a step declaring `after_fired` on a step this pass skipped is skipped by
// the next pass, in the same transaction, however deep the chain.
//
// WHY the predecessor's whole instance set. For a single-instance predecessor
// the two readings coincide. For a fanout, "the step fired" means some
// sibling ran: a predecessor with one `done` sibling produced a result its
// `after_fired` successor has something to run over; only a predecessor NONE
// of whose siblings ran has not fired. And `skipped` LITERALLY: a predecessor
// that ended `failed-routed` or `superseded` was decided, not skipped, and the
// issue's own acceptance names `skipped` alone.
//
// WHERE it runs — every place the engine can end a step `skipped`:
//
//   - reconcileIssueAndRun, the tail of EVERY routing transaction: after
//     skipUnroutedTargets (the interposed-gate skip) and before the completion
//     check, so an issue whose last live steps were just skipped completes in
//     the same transaction. This also covers an `on_fail = "skip"` routing and
//     an operator's `step resolve --as skip`, both of which reconcile;
//   - resolveQuorumMisses (loop.go), the one routing path that terminalizes a
//     threshold-bearing step without reconciling;
//   - expandIssue (activate.go), where a false `when` CREATES a step skipped
//     (§5.3.1) and its `after_fired` successors must be created skipped with
//     it. Loop re-instantiation needs no fourth call: every loop entry commits
//     inside a routing transaction that ends in one of the first two.
//
// WHAT it does not do. It never touches a step that has left `pending` — a
// claimed, running, gated, parked, or terminal step has already answered the
// question — and it consults no readiness clause: V39a requires every
// `after_fired` entry to also appear in `after`, so R3 already holds the step
// behind its gate and the sweep only ever decides a step whose predecessor is
// terminal. `after` itself is untouched (done OR skipped still releases a
// join); this is an additive second list, not a new reading of the first.
//
// DORMANT UNLESS DECLARED. A definition with no `after_fired` anywhere costs
// no query at all — the same discipline the budget and reap-hold snapshots
// keep (§4.8 B29) — so a routing on a workflow that never heard of the key is
// byte-identical to what it was.

// propagateAfterFiredSkips terminalizes `skipped`, to a fixed point, every
// `pending` step of one issue whose `after_fired` names a predecessor that did
// not fire. It returns the rows it skipped, so a caller holding a loaded
// snapshot (resolveQuorumMisses) can mirror the change into it.
func propagateAfterFiredSkips(
	tx *sql.Tx, runID, issueID int, def *workflow.Definition, nowMS int64,
) ([]*db.Step, error) {
	if !declaresAfterFired(def) {
		return nil, nil
	}
	steps, err := db.ListIssueStepsTx(tx, runID, issueID)
	if err != nil {
		return nil, err
	}

	var skipped []*db.Step
	// Each pass either skips at least one more row or stops, so the bound is
	// the step count — the same argument downstreamClosure (loop.go) makes.
	for {
		changed := false
		for _, step := range steps {
			if step.Status != db.StepPending || step.Materialized {
				continue
			}
			spec := workflow.StepByName(def, step.StepName)
			if spec == nil || len(spec.AfterFired) == 0 {
				continue
			}
			unfired := unfiredPredecessor(steps, step, spec.AfterFired)
			if unfired == "" {
				continue
			}
			if err := db.SetStepStatusTx(tx, step.ID, db.StepSkipped, nowMS, nowMS); err != nil {
				return nil, err
			}
			if err := recordEvent(tx, eventRecord{
				Kind: EventStepSkipped, RunID: runID,
				Instance: step.Instance, IssueID: issueID,
				Data: fmt.Sprintf("after_fired predecessor did not fire: %s", unfired),
				AtMS: nowMS,
			}); err != nil {
				return nil, err
			}
			// Reflected in the loaded rows so the next pass — and the caller's
			// snapshot, which shares nothing with these rows but is repaired
			// from the returned set — sees the skip this pass just wrote.
			step.Status = db.StepSkipped
			skipped = append(skipped, step)
			changed = true
		}
		if !changed {
			return skipped, nil
		}
	}
}

// unfiredPredecessor names the first `after_fired` predecessor of a step that
// did not fire — every instance R3 resolves for it ended `skipped` — rendered
// as its instances for the event, or "" when each named predecessor fired, is
// still open, or has no instance yet.
//
// A predecessor with no instance at or below the step's ordinal has not been
// expanded (a loop body not yet entered): nothing is decided, so nothing is
// skipped. An instance in any non-`skipped` status — including a pending or
// running one — means the predecessor fired or may still fire, and the step
// waits exactly as R3 makes it wait.
func unfiredPredecessor(steps []*db.Step, step *db.Step, names []string) string {
	for _, name := range names {
		instances := predecessorInstancesIn(steps, step, name)
		if len(instances) == 0 {
			continue
		}
		fired := false
		rendered := make([]string, 0, len(instances))
		for _, inst := range instances {
			if inst.Status != db.StepSkipped {
				fired = true
				break
			}
			rendered = append(rendered, inst.Instance)
		}
		if !fired {
			return strings.Join(rendered, ", ")
		}
	}
	return ""
}

// declaresAfterFired reports whether any step of a definition declares
// `after_fired` — the sweep's dormancy test. A nil definition (a caller with
// none, such as the interrupted-gate park) declares nothing.
func declaresAfterFired(def *workflow.Definition) bool {
	if def == nil {
		return false
	}
	for _, step := range def.Steps {
		if len(step.AfterFired) > 0 {
			return true
		}
	}
	return false
}
