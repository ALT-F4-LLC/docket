package engine

import (
	"sort"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// LOOKAHEAD-STAGED OFFERS — the dependency closure of one offer.
//
// THE PROBLEM. An offer only ever contained steps that are ready RIGHT NOW, so
// every dependency edge cost a full dispatch round-trip: four judges and the
// `synthesize` that consumes them could never share a wave, because
// `synthesize` only becomes ready once `step record` has run for all four —
// which happens DURING the wave the judges ride in. A dispatcher asking
// "what's next" between waves paid one whole open/dispatch/close cycle per
// GRAPH LEVEL, not per unit of independent work.
//
// THE RULE. An offer is widened to its own closure: a pending step that is not
// ready joins the offer at a LATER STAGE when everything still standing
// between it and readiness is itself in the offer at a lower stage. A
// dispatcher that runs stages as awaited groups — record the results of stage
// k, then start stage k+1 — finds each staged row claimable exactly when its
// stage begins, because the recordings of the earlier stages are what satisfy
// its predicate. Nothing about READINESS changes: R1-R7 stand, `claim`
// re-checks them (claim.go), and a dispatcher that starts a staged row early
// gets the same refusal it always would. The offer promises only what its own
// execution unlocks; anything blocked on the WORLD OUTSIDE the offer — a
// foreign run's claim, an operator's open question — is not staged, because no
// amount of running this wave resolves it.
//
// TWO WAYS INTO A LATER STAGE, one shared reading:
//
//  1. A step that is READY but was rationed out of the claimable cohort by
//     ClaimablePrefix — class headroom or a scope grant consumed by an earlier
//     row. The rationing is THIS OFFER's own admissions, so this offer's own
//     completion frees it: it runs one stage later instead of waiting a whole
//     round (the DKT-23 shape, five writers over one slot, now one wave of
//     five stages instead of five waves).
//  2. A step whose R3 is unsatisfied, where every non-terminal predecessor
//     instance is in the offer. It is staged after the highest predecessor
//     stage. Chains extend transitively — judges, then the synthesize over
//     them, then the report over that — until they hit a step the wave cannot
//     resolve: a HUMAN gate (nothing in a wave answers an operator's
//     question), a predecessor outside the offer, or an issue whose
//     `depends_on` is unsatisfied (cross-issue edges resolve at issue
//     completion, which is rollup work no single wave owns).
//
// Vote and action steps ARE stageable: `step record` now drives both
// lifecycles the moment a recording readies them (record.go's driver), so a
// mid-wave gate opens its proposal, a quorum cast routes it, and a mid-chain
// action runs engine-side — all without a `next` in between. A staged row's
// rendered status is db.StepStaged, never `ready`: the wire does not lie
// about claimability, and a dispatcher that filters on `ready` simply ignores
// the closure and behaves exactly as before.
//
// PER-STAGE ACCOUNTING, deliberately. Stages run sequentially, so two rows in
// DIFFERENT stages never run concurrently and must not compete for class
// headroom or scope. Each stage is its own cohort: bounded classes are packed
// up to `max` per stage, tree-holding scopes must be disjoint within a stage,
// and a row that does not fit its earliest legal stage is bumped to the next
// (never earlier — dependencies only push forward). Occupancy OUTSIDE the
// offer (a foreign run's claimed writer, a lease mid-drain) is deliberately
// not charged against later stages: by the time stage k starts the world has
// moved, the charge would be stale either way, and `claim` is the authority
// that answers it at the only moment the answer is real. The BUDGET (R7) is
// the exception and stays whole-offer: every admitted row will spend if the
// wave succeeds, so the closure's summed expected cost must fit remaining
// headroom regardless of which stage spends it.
//
// WHAT FAILURE COSTS. A stage-k row whose predecessor failed or was rejected
// never becomes ready; its claim refuses, the dispatcher skips it, and it is
// still `pending` at close — which is expected, not a discrepancy, and the
// next round offers whatever `on_fail` routing produced. Success costs zero
// extra rounds; failure costs the one round it always did.

// offerEntry is one offered step with its wave level.
type offerEntry struct {
	step *db.Step
	// stage is the wave level: a dispatcher must not start this row until
	// every row of a lower stage in the same offer has completed. Stage 0 is
	// the claimable-now cohort.
	stage int
	// staged reports the row is offered AHEAD of its own readiness — rendered
	// db.StepStaged, not claimable until the lower stages record. False for
	// every row ClaimablePrefix admitted, including loop-staged ready rows,
	// which remain claimable at any moment exactly as before (stage.go's
	// "core enforces nothing" contract is unchanged for them).
	staged bool
	// conditional marks a staged row sitting downstream of a HOLD-CAPABLE
	// in-offer predecessor (DKT-26) — see model.StepRow.Conditional.
	conditional bool
}

// lookaheadOffer widens one claimable cohort to its staged closure and levels
// the result: membership first (who can this offer unlock?), then longest-path
// stage levels over the offer's own edges, then per-stage cohort packing.
// `admitted` is ClaimablePrefix's output — the co-claimable stage-0-and-loop
// cohort — and rides through unchanged in membership and claimability.
func (s *Scheduler) lookaheadOffer(admitted []*db.Step) []offerEntry {
	entries := make([]offerEntry, 0, len(admitted))
	member := make(map[int]bool, len(admitted))
	var cost float64
	for _, step := range admitted {
		entries = append(entries, offerEntry{step: step})
		member[step.ID] = true
		// reservableCost: a vote step's declared cost is already in the floor
		// at materialization (DKT-584), so the closure must not re-reserve it.
		cost += reservableCost(step)
	}

	// ---- Phase A: membership, to a fixed point. ---------------------------
	//
	// Only an ACTIVE run stages ahead: on any other status the ready set was
	// empty (R1) and a closure over an empty promise is noise. Candidates are
	// visited in SortSteps order every pass, so admission — and therefore the
	// stage a bumped row lands in — is deterministic and fair the same way
	// ClaimablePrefix's greed is: the oldest eligible row wins every tie.
	if s.run != nil && s.run.Status == model.RunActive {
		var candidates []*db.Step
		for _, step := range s.steps {
			if step.Status != db.StepPending || member[step.ID] {
				continue
			}
			// A human step never joins a later stage: the wave has no verb
			// that answers an operator. Its downstream is fenced off by the
			// membership rule itself (a non-terminal predecessor outside the
			// offer refuses staging).
			if step.Kind == workflow.TypeHuman {
				continue
			}
			// Cross-issue edges stop the closure: R2 resolves at ISSUE
			// completion, a rollup no single wave performs, so a step whose
			// issue still waits on another issue is blocked on the world
			// outside the offer by definition.
			if f := s.issues[step.IssueID]; f == nil || !f.depsOK {
				continue
			}
			candidates = append(candidates, step)
		}
		s.SortSteps(candidates)

		for changed := true; changed; {
			changed = false
			for _, cand := range candidates {
				if member[cand.ID] {
					continue
				}
				// The loop-body rule applies to staged admission exactly as
				// it does to ready admission (stage.go): a row whose open
				// loop body is absent from the offer is blocked no matter
				// how its `after` reads. A body that IS in the offer (ready
				// or staged) does not block — the leveling below orders the
				// pair via precedesInSet.
				if s.blockingLoopBodyAbsent(cand, member) {
					continue
				}
				if !s.stageableFrom(cand, member) {
					continue
				}
				// R7 stays WHOLE-OFFER: every admitted row spends if the
				// wave succeeds, whichever stage it runs in.
				if !s.offerBudget(cand, cost) {
					continue
				}
				member[cand.ID] = true
				cost += reservableCost(cand)
				entries = append(entries, offerEntry{step: cand, staged: true})
				changed = true
			}
		}
	}

	// A loop hold recorded on an earlier, narrower pass is stale for any row
	// the closure went on to admit — reporting "withheld" for a row this same
	// answer offers would be the exact confusion LoopHoldReason exists to
	// prevent, inverted.
	for _, e := range entries {
		delete(s.loopHolds, e.step.Instance)
	}

	// ---- Phase B: levels — longest path over the offer's own edges. -------
	//
	// Two edge kinds, one relaxation: a staged row sits after its non-terminal
	// `after` predecessors, and ANY row sits after an in-offer loop body whose
	// re-entry covers it (precedesInSet — the same predicate that staged
	// ready-only offers before the closure existed, so the fixer-then-judges
	// ordering is one rule, not two). The pass count is bounded by the entry
	// count: the edges are acyclic for any definition L1 admits, and the bound
	// makes a hypothetical cycle terminate instead of spin.
	stageOf := make(map[int]int, len(entries))
	for _, e := range entries {
		if e.staged {
			stageOf[e.step.ID] = 1
		}
	}
	for pass := 0; pass < len(entries); pass++ {
		changed := false
		for i := range entries {
			if lvl := s.entryFloor(&entries[i], entries, stageOf); lvl > stageOf[entries[i].step.ID] {
				stageOf[entries[i].step.ID] = lvl
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// ---- Phase C: per-stage cohort packing. -------------------------------
	//
	// Entries are finalized in ascending level order, so every dependency is
	// placed before its dependents and a bump can only push work later, never
	// invalidate an already-placed row. Ready rows are never bumped — their
	// co-claimability was ClaimablePrefix's whole-offer decision and stands —
	// but they DO occupy their stage's cohort, so a staged row of the same
	// bounded class or an intersecting scope packs around them.
	sort.SliceStable(entries, func(i, j int) bool {
		si, sj := stageOf[entries[i].step.ID], stageOf[entries[j].step.ID]
		if si != sj {
			return si < sj
		}
		return s.sortsBefore(entries[i].step, entries[j].step)
	})
	classCount := map[int]map[string]int{}
	scopeHeld := map[int]map[int][]string{}
	final := make(map[int]int, len(entries))
	for i := range entries {
		e := &entries[i]
		k := 0
		if e.staged {
			k = 1
		}
		if floor := s.dependencyFloor(e, entries, final); floor > k {
			k = floor
		}
		if e.staged {
			for !s.cohortFits(e.step, k, classCount, scopeHeld) {
				k++
			}
		}
		final[e.step.ID] = k
		e.stage = k
		s.chargeCohort(e.step, k, classCount, scopeHeld)
	}

	// ---- Conditional marking (DKT-26). ------------------------------------
	//
	// A staged row behind a HOLD-CAPABLE predecessor is a weaker promise than
	// the closure's usual one: a hold-capable step can finish its work and
	// still not route — `hold_spread` trips, the saga parks in `held`, and the
	// step stays non-terminal until an operator answers. Every stage below
	// the row completes and its claim still refuses; a dispatcher spawning at
	// the stage boundary pays one full boot per downstream row for nothing
	// (measured: DISPATCH-11's verify executor, spawned and refused while its
	// reconcile held). The mark propagates transitively — everything behind a
	// conditional row inherits the same weaker promise. `final` has every
	// entry placed, so membership reads the whole offer.
	conditional := make(map[int]bool, len(entries))
	for changed := true; changed; {
		changed = false
		for i := range entries {
			e := &entries[i]
			if !e.staged || conditional[e.step.ID] {
				continue
			}
			// An interposed threshold target is conditional AT THE SOURCE
			// (DKT-38): a staged one is by construction not yet routed to, and
			// its routing predecessor may route anywhere else — the same
			// finish-without-routing promise a hold-capable step makes, decided
			// by a threshold instead of an operator. Its downstream inherits
			// the mark through the ordinary propagation below.
			if s.thresholdGated(e.step) {
				conditional[e.step.ID] = true
				changed = true
				continue
			}
			for _, pred := range s.offerPredecessors(e.step, final) {
				if conditional[pred.ID] || s.holdCapable(pred) {
					conditional[e.step.ID] = true
					changed = true
					break
				}
			}
		}
	}
	for i := range entries {
		entries[i].conditional = conditional[entries[i].step.ID]
	}

	// The wire order: stage-major, SortSteps within a stage — the same
	// "predecessors first, then priority-age-id" reading DKT-38 established
	// for the loop case, now over the whole closure, so a `--limit` cut keeps
	// a runnable prefix and a top-to-bottom dispatcher starts nothing early.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].stage != entries[j].stage {
			return entries[i].stage < entries[j].stage
		}
		return s.sortsBefore(entries[i].step, entries[j].step)
	})
	return entries
}

// holdCapable reports whether a step's completion can HOLD instead of routing:
// an `aggregate` action declaring `hold_spread` (§7.7.1 H1). The declaration's
// PRESENCE is the test — whether it trips is knowable only from the payloads
// at routing time, and conditional marks the possibility, not the outcome.
func (s *Scheduler) holdCapable(step *db.Step) bool {
	if step.Kind != workflow.ClassAction {
		return false
	}
	def := s.defs[step.WorkflowID]
	if def == nil {
		return false
	}
	spec := materializedSpec(def, step, s.holdTally)
	if spec == nil {
		return false
	}
	_, present := spec.Params[ParamHoldSpread]
	return present
}

// thresholdGated reports whether a step is an interposed threshold target —
// some step of its pinned definition routes to it by name (DKT-38). A
// materialized step's minted name is never declared, so it answers false
// without a special case.
func (s *Scheduler) thresholdGated(step *db.Step) bool {
	def := s.defs[step.WorkflowID]
	if def == nil {
		return false
	}
	return len(workflow.RoutingPredecessors(def, step.StepName)) > 0
}

// stageableFrom is Phase A's satisfiability rule: can THIS OFFER's own
// execution make `cand` claimable?
//
// A READY candidate qualifies outright — readyRows collected every ready step
// and ClaimablePrefix narrowed, so ready-but-absent means rationed by this
// offer's own admissions, which this offer's own completion frees. Otherwise
// only an unsatisfied R3 qualifies, and only when every non-terminal
// predecessor instance is in the offer and none is a human gate: any other
// failed condition (scope against a live foreign claim, headroom against a
// draining lease, budget) is the world outside the offer, and staging against
// it would promise what the wave cannot deliver.
//
// Quorum joins are admitted OPTIMISTICALLY: whether `min_siblings` is met is
// knowable only at join time, and the claim's own R3 re-check is the authority
// if it is missed — the staged row simply never becomes claimable, which is
// the ordinary failure path.
func (s *Scheduler) stageableFrom(cand *db.Step, member map[int]bool) bool {
	ok, cond := s.Ready(cand)
	if ok {
		return true
	}
	// CondGateOpen qualifies beside CondPredecessors (DKT-168): the gate that
	// holds the candidate rides this same offer (interposed targets are staged
	// as conditional), so its resolution IS this offer's own execution — the
	// closure must not silently shrink behind it. The gate-in-offer check
	// below is what backs the promise.
	if cond != CondPredecessors && cond != CondGateOpen {
		return false
	}
	// An UNACKNOWLEDGED reap holds a slot in the candidate's bounded class
	// (A15), and nothing a wave executes clears it — the hold ends when a
	// relay ACKNOWLEDGES the reap, an act outside the offer's own execution.
	// A staged row behind that hold would be a promise the wave cannot keep:
	// its claim refuses on R5 however many predecessors record. The reaped
	// step itself is unaffected (the hold never counts against it — W3's
	// re-offer) because it reaches here as READY, through the branch above.
	if limit, bounded := s.limits[cand.Class]; bounded && limit.Max > 0 {
		if s.unacknowledgedReapsInClass(cand.Class, cand.ID) > 0 {
			return false
		}
	}
	def := s.defs[cand.WorkflowID]
	if def == nil {
		return false
	}
	spec := materializedSpec(def, cand, s.holdTally)
	if spec == nil {
		return false
	}
	for _, predName := range spec.After {
		sibs := s.predecessorInstances(cand, predName)
		if len(sibs) == 0 {
			// Not expanded (or a loop body not yet entered): nothing in this
			// offer resolves an instance that does not exist.
			return false
		}
		for _, pred := range sibs {
			if db.StepTerminal(pred.Status) {
				continue
			}
			if !member[pred.ID] || pred.Kind == workflow.TypeHuman {
				return false
			}
		}
	}
	// The open interposed gates holding the candidate (DKT-168) obey the same
	// rule as its predecessors: in the offer and not a human gate, or the wave
	// cannot resolve them and the staged row is a promise it cannot keep.
	for _, gate := range s.openInterposedGates(cand) {
		if !member[gate.ID] || gate.Kind == workflow.TypeHuman {
			return false
		}
	}
	return true
}

// entryFloor is Phase B's relaxation step: the lowest stage `e` may occupy
// given the CURRENT levels of the rows it sits after.
func (s *Scheduler) entryFloor(e *offerEntry, entries []offerEntry, stageOf map[int]int) int {
	floor := 0
	if e.staged {
		floor = 1
	}
	for i := range entries {
		other := entries[i].step
		if other.ID == e.step.ID {
			continue
		}
		if s.precedesInSet(other, e.step) {
			if lvl := stageOf[other.ID] + 1; lvl > floor {
				floor = lvl
			}
		}
	}
	if !e.staged {
		return floor
	}
	for _, pred := range s.offerPredecessors(e.step, stageOf) {
		if lvl := stageOf[pred.ID] + 1; lvl > floor {
			floor = lvl
		}
	}
	return floor
}

// dependencyFloor is Phase C's re-read of the same two edge kinds against
// FINAL stages. Entries are finalized in ascending Phase-B level order and an
// edge always spans levels, so every predecessor consulted here has already
// been placed.
func (s *Scheduler) dependencyFloor(e *offerEntry, entries []offerEntry, final map[int]int) int {
	floor := 0
	for i := range entries {
		other := entries[i].step
		if other.ID == e.step.ID {
			continue
		}
		if _, placed := final[other.ID]; !placed {
			continue
		}
		if s.precedesInSet(other, e.step) {
			if lvl := final[other.ID] + 1; lvl > floor {
				floor = lvl
			}
		}
	}
	if !e.staged {
		return floor
	}
	for _, pred := range s.offerPredecessors(e.step, final) {
		if lvl, placed := final[pred.ID]; placed && lvl+1 > floor {
			floor = lvl + 1
		}
	}
	return floor
}

// offerPredecessors lists the non-terminal `after` predecessor instances of a
// STAGED entry that are themselves in the offer — the rows whose recordings
// are what make the entry claimable. `stageOf` is consulted only for
// membership, so Phase B and Phase C read one definition of "in the offer".
func (s *Scheduler) offerPredecessors(step *db.Step, stageOf map[int]int) []*db.Step {
	def := s.defs[step.WorkflowID]
	if def == nil {
		return nil
	}
	spec := materializedSpec(def, step, s.holdTally)
	if spec == nil {
		return nil
	}
	var out []*db.Step
	for _, predName := range spec.After {
		for _, pred := range s.predecessorInstances(step, predName) {
			if db.StepTerminal(pred.Status) {
				continue
			}
			if _, in := stageOf[pred.ID]; in {
				out = append(out, pred)
			}
		}
	}
	// An open interposed gate holding the step (DKT-168) is a predecessor for
	// leveling purposes: the step's claim refuses until the gate resolves, so
	// a stage that ran them side by side would spawn an executor only to have
	// it refused — the exact boot-for-nothing the conditional mark exists to
	// price, made structural here instead.
	for _, gate := range s.openInterposedGates(step) {
		if _, in := stageOf[gate.ID]; in {
			out = append(out, gate)
		}
	}
	return out
}

// cohortFits answers whether one more row fits stage k's cohort: bounded-class
// headroom within the stage, and scope disjointness within the stage (same
// issue rides along, non-tree-holders are exempt — scopeConflict's own
// exemptions, applied per stage). Deliberately no charge from occupancy
// outside the offer; the file comment carries the reasoning.
func (s *Scheduler) cohortFits(
	step *db.Step, k int, classCount map[int]map[string]int, scopeHeld map[int]map[int][]string,
) bool {
	if limit, ok := s.limits[step.Class]; ok && limit.Max > 0 {
		if classCount[k][step.Class] >= limit.Max {
			return false
		}
	}
	if !s.holdsTree(step) {
		return true
	}
	scope := s.foreignScope(step.IssueID)
	if len(scope) == 0 {
		return true
	}
	for issueID, held := range scopeHeld[k] {
		if issueID != step.IssueID && ScopesIntersect(scope, held) {
			return false
		}
	}
	return true
}

// chargeCohort records a placed row against its stage's cohort, so later
// placements pack around it. Ready rows charge too: a staged row must not
// share a stage-1 slot a loop-staged judge already occupies.
func (s *Scheduler) chargeCohort(
	step *db.Step, k int, classCount map[int]map[string]int, scopeHeld map[int]map[int][]string,
) {
	if classCount[k] == nil {
		classCount[k] = map[string]int{}
	}
	classCount[k][step.Class]++
	if !s.holdsTree(step) {
		return
	}
	scope := s.foreignScope(step.IssueID)
	if len(scope) == 0 {
		return
	}
	if scopeHeld[k] == nil {
		scopeHeld[k] = map[int][]string{}
	}
	if _, held := scopeHeld[k][step.IssueID]; !held {
		scopeHeld[k][step.IssueID] = scope
	}
}

// sortsBefore is SortSteps' (priority, age, id) order as a pairwise predicate,
// for sorts that interleave it with stage — one definition of the within-stage
// order, shared with the slice sort SortSteps performs.
func (s *Scheduler) sortsBefore(a, b *db.Step) bool {
	pa, pb := s.priorityOf(a), s.priorityOf(b)
	if pa != pb {
		return pa < pb
	}
	if a.CreatedAtMS != b.CreatedAtMS {
		return a.CreatedAtMS < b.CreatedAtMS
	}
	return a.ID < b.ID
}
