package engine

import (
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DEPENDENCY-STAGED READY SETS.
//
// THE PROBLEM. A ready set can hold rows that are each individually ready and
// still order-dependent. The observed case is a loop's re-entry: completing
// `verify@0` with a failing threshold routes to `fix-loop`, which mints `fix@1`
// AND restores the `after_loop` step's siblings, so one `next` offers
//
//	review@1#0, review@1#1, review@1#2, review@1#3, fix@1
//
// — the judges FIRST, because SortSteps orders by (priority, age, id) and says
// nothing about dependency. A dispatcher that spawns rows in the order it
// received them reviews the tree the fixer is about to rewrite, and reports on
// code that no longer exists by the time anyone reads the findings.
//
// WHY READINESS DOES NOT ALREADY COVER THIS. R1-R7 answer "may this step be
// claimed now", and the honest answer for every row above is yes: `review@1`'s
// `after = ["implement"]` is satisfied, and nothing in the DAG says a judge
// waits for a fixer. The dependency is not in `after` at all — it is created by
// the LOOP RE-ENTRY, which is why the definition's DAG cannot express it and
// why a readiness fix would be wrong. These steps really are ready; what they
// are not is safe to START in an arbitrary order.
//
// WHERE THE ORDERING LIVED BEFORE. In dispatcher convention: a writers-first
// partition keyed on `row.class == "write"`, in a script outside this
// repository. That is a correctness property resting on a string core assigns
// no meaning to (§6.5 — classes are opaque, and the reference instance's `write`
// is INSTANCE POLICY). Any dispatcher that partitioned differently, or not at
// all, got pre-fix reviews with no error and no event.
//
// THE RULE, and it is deliberately the narrowest one that covers the case:
//
//	A row is staged AFTER the rows in its ready set that (a) belong to the
//	same issue, (b) HOLD THE TREE, and (c) are LOOP STEPS whose `after_loop`
//	target is this row's step — or a step transitively `after` that target.
//
// Every clause is read from the PINNED DEFINITION, never from a class name, an
// executor hint, or a step name. `holds_tree` is the instance's own declaration
// that a step writes the tree (workflow.Step.HoldsTree); `after_loop` is the
// instance's own declaration of where a loop re-enters. Core reads structure
// the author wrote, which is what keeps this generic.
//
// WHAT THIS IS NOT. Not a priority, not a barrier, and not a readiness clause.
// Core enforces nothing FOR READY ROWS: it labels them and every one stays
// claimable at any moment. A dispatcher that ignores `stage` behaves exactly as
// it does today. That is the deliberate limit of a scheduling hint — core
// cannot police what a dispatcher spawns, and pretending otherwise (by refusing
// a claim on a higher-staged step) would break the fixture's own tests, which
// legitimately claim steps in whatever order they please. (STAGED rows —
// lookahead.go's closure, rendered `staged` rather than `ready` — are the
// separate case where the predicate itself refuses an early claim, because
// those rows genuinely are not ready yet.)
//
// The LEVELING lives in lookahead.go now: the same precedesInSet edges feed
// the closure's longest-path pass alongside `after` edges, so the
// fixer-then-judges ordering and the judges-then-synthesize ordering are one
// computation. This file keeps the two PREDICATES that define the loop rule.

// precedesInSet reports whether `earlier` must complete before `later` starts,
// for two steps offered in the SAME ready set.
//
// Both steps must belong to one issue: a fixer rewrites its OWN issue's tree,
// so it cannot stale another issue's view of one. Staging across issues would
// serialize work whose scopes are unrelated, which is latency for no
// correctness — and both offer paths (`next` and readyRows, since DKT-101)
// have already excluded the scope-sharing case outright via ClaimablePrefix.
//
// This clause still stands on its own reasoning rather than leaning on that
// guarantee: the argument above is about which tree a step writes, not about
// which rows were admitted together.
func (s *Scheduler) precedesInSet(earlier, later *db.Step) bool {
	if earlier.IssueID != later.IssueID {
		return false
	}

	// Only a tree-holding step can invalidate another step's view of the tree.
	// A step the author declared `holds_tree = false` reads and does not write,
	// so nothing it does can stale a sibling's inputs.
	if !s.holdsTree(earlier) {
		return false
	}

	def := s.defs[earlier.WorkflowID]
	if def == nil || earlier.WorkflowID != later.WorkflowID {
		return false
	}
	spec := workflow.StepByName(def, earlier.StepName)
	if spec == nil || spec.AfterLoop == "" {
		// Not a loop step, so this pair is not the re-entry case and core has
		// no declared reason to order them. Ordinary DAG dependencies are
		// already handled by readiness — a step whose `after` is unsatisfied is
		// not in this set at all.
		return false
	}

	// `later` is in the re-review the loop re-enters at: the `after_loop`
	// target itself, or anything transitively `after` it. Reusing
	// afterLoopDownstream is deliberate — it is the same §7.3 (1) set the loop
	// machinery supersedes on re-entry, so "what the loop re-runs" has ONE
	// definition rather than two that can drift.
	return s.loopClosure(earlier.WorkflowID, def)[later.StepName]
}

// blockingLoopBodyAbsent is precedesInSet's definition-keyed sibling
// (DKT-48), and the ONLY predicate ClaimablePrefix's eviction pass consults
// (ready.go) — precedesInSet's own `sorted` scan only ever saw a loop body
// that reached the ready set and was then rejected there, a proper subset of
// what scanning every status here already catches, so keeping both was two
// predicates answering one question (and they had drifted: this one lacked
// precedesInSet's `holds_tree` clause until it was added here too).
//
// This asks the PINNED DEFINITION instead of the offered set: does a
// same-issue, same-ordinal, tree-holding loop step whose `after_loop` chain
// contains `later`'s step still exist as an open (non-terminal) row, and is
// it absent from the offer? If so, `later` is blocked regardless of WHY the
// body never reached `sorted` — rationed out of this offer, or excluded from
// readiness (R7 budget, R5 class headroom, R4 scope) before it ever got
// there.
//
// The `after_loop` chain is afterLoopDownstream's MERGED closure over every
// loop root in the definition, deliberately: precedesInSet reads the same
// merged set, for the same reason — "what the loop re-runs" has one
// definition, not two that can drift. A definition with two independent
// loops would over-gate here (a body of loop 1 could withhold loop 2's
// dependents), which is accepted for now: no such definition exists in this
// tree, and the alternative (a per-loop chain) would break that shared
// reading.
//
// Scans Scheduler.steps — every step of `later`'s own run, any status —
// rather than `sorted`, which is the entire point: `sorted` never held the
// row this asks about. steps and later.RunID are both guarded explicitly
// even though today's snapshot only populates `steps` with this run's own
// rows (LoadScheduler), so a future snapshot that folds foreign rows in
// cannot silently match one.
//
// A definition can never place a loop step inside its own after_loop
// downstream closure without an `after` cycle, which L1 already rejects at
// register time — but even if it could, `inOffer` guards the self-match:
// the sole caller (ClaimablePrefix) only ever passes a `later` drawn from
// the offered set itself, so a candidate equal to `later` is always in
// `inOffer` and skipped.
func (s *Scheduler) blockingLoopBodyAbsent(later *db.Step, inOffer map[int]bool) bool {
	def := s.defs[later.WorkflowID]
	if def == nil {
		return false
	}
	if !s.loopClosure(later.WorkflowID, def)[later.StepName] {
		// Not in the re-review any loop re-enters at, so there is no loop
		// body whose absence could block it.
		return false
	}

	for _, candidate := range s.steps {
		if candidate.RunID != later.RunID || candidate.IssueID != later.IssueID {
			continue
		}
		if candidate.WorkflowID != later.WorkflowID || candidate.Ordinal != later.Ordinal {
			continue
		}
		if db.StepTerminal(candidate.Status) || inOffer[candidate.ID] {
			continue
		}
		if !s.holdsTree(candidate) {
			// Only a tree-holding step can invalidate another step's view of
			// the tree — the same reasoning precedesInSet states in full.
			continue
		}
		spec := workflow.StepByName(def, candidate.StepName)
		if spec == nil || spec.AfterLoop == "" {
			continue
		}
		// The fact behind the eviction, recorded for LoopHoldReason (DKT-61):
		// without it, the withholding reaches no field a dispatcher can read.
		if s.loopHolds == nil {
			s.loopHolds = make(map[string]string)
		}
		s.loopHolds[later.Instance] =
			fmt.Sprintf("%s (%s)", candidate.Instance, candidate.Status)
		return true
	}
	return false
}

// The post-limit eviction pass (evictAbsentLoopBodies) and the two-level
// assignStages/stageVector rendering that once lived here are retired by the
// closure (DKT-58/DKT-75's truncation hole included): lookahead.go emits the
// offer in stage-major order with every dependency at a strictly lower stage,
// so a `--limit` prefix cut cannot separate a row from its loop body, and
// membership itself refuses a row whose open body is absent from the offer.
