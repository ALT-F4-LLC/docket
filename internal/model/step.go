package model

import (
	"fmt"
)

// StepIDPrefix is the prefix step IDs render with, kept apart from IDPrefix and
// RunIDPrefix so a `STEP-3` can never be mistaken for a `DKT-3` or a `RUN-3` in
// an error string, an event, or a wire shape.
const StepIDPrefix = "STEP"

// FormatStepID renders a step's display identity.
func FormatStepID(id int) string {
	return fmt.Sprintf("%s-%d", StepIDPrefix, id)
}

// ParseStepID accepts `STEP-3` or a bare `3`, mirroring ParseID and ParseRunID
// so an operator's muscle memory carries across all three entities.
func ParseStepID(s string) (int, error) {
	return parseRefID(s, StepIDPrefix, "step")
}

// StepRow is engine-spec §11.4's `next row`, field for field:
//
//	{ step, issue, run, executor, class, attempt, expected_cost,
//	  lease_ttl_s, metadata }
//
// `Instance` is the ONE addition, and it is a recorded amendment (TDD
// §10 A1): §11.4 names no field for the rendered `name@k#i` identity that §11.3
// makes the step's public name. The amendment proposes §11.4 name it
// explicitly; until then the field is additive and changes no semantics.
//
// `Kind` likewise rides here so a dispatcher can tell an executor step from a
// human gate BEFORE spawning a worker for one (§6.15: "a dispatcher that spawns
// on every next row must not spawn a worker for a gate").
type StepRow struct {
	Step     string `json:"step"`
	Instance string `json:"instance"`
	Issue    string `json:"issue"`
	Run      string `json:"run"`
	Kind     string `json:"kind"`
	// Executor is the opaque hint (§11.1). It is absent on human and vote
	// steps, which carry none — the wire shape says so by omission rather than
	// by an empty string a dispatcher might dispatch on.
	Executor string `json:"executor,omitempty"`
	Class    string `json:"class,omitempty"`
	// Labels is the issue's labels AS FROZEN AT ACTIVATION — the same
	// `run_issues.issue_snapshot` the context bundle reads, never a live join
	// against `issues`, so a mid-run relabel cannot change how a step routes.
	//
	// It rides here for the same reason `Kind` does: a dispatcher must decide
	// BEFORE it spawns. Routing policy is label-keyed (doc type, security
	// sensitivity, architecture gating), and a dispatcher holding only the row
	// had no way to see a label — so every label-keyed rule silently fell
	// through to its default, which is a routing decision that looks
	// authoritative and is wrong. Found 2026-08-06 against a live run.
	//
	// A recorded amendment in the same class as `Instance` and `Kind`: additive,
	// `omitempty` so a label-less issue serializes exactly as before.
	Labels []string `json:"labels,omitempty"`
	// Voters is the step's declared voter list, present ONLY on vote steps and
	// absent everywhere else.
	//
	// It rides here for `Kind`'s reason, one step further on: knowing a row is a
	// vote step tells a dispatcher not to spawn a worker for it, and knowing WHO
	// CASTS tells it what the vote is waiting for. Without the list, a caller
	// holding a `next` row could see a vote step sitting open and had no way to
	// learn who had not voted without reading the definition off disk — which
	// is the pinned artifact it is not supposed to need.
	//
	// The entries are OPAQUE (§11.1): core counts them and never interprets one.
	Voters []string `json:"voters,omitempty"`
	// Proposal is the display id of the proposal this vote step opened, once
	// one has been opened. It is absent before that — a vote step whose
	// proposal has not been created yet has no ballot to point at, and an
	// empty string would read as one that does.
	//
	// It closes the same gap as Voters from the other side: the vote lifecycle
	// happens through the EXISTING proposal machinery (`vote show`, `vote
	// cast`), and every one of those verbs is addressed by proposal id — which
	// was, until this field, derivable only from an idempotency key nothing on
	// the wire disclosed.
	Proposal string `json:"proposal,omitempty"`
	// Attempt is a monotonic, 0-based, spent-count of claims committed against
	// this step: 0 = never claimed, N = claimed N times. It increments only at
	// a winning claim's CAS commit and NOWHERE ELSE — a heartbeat, a reap, a
	// `step fail`, and a `step resolve --as retry` all leave it untouched (the
	// retry refreshes the budget by moving `attempt_base`; the counter itself
	// is never reset, DKT-86/DKT-90). It counts CLAIMS, not failures: a lease
	// that expired and was reaped spent an attempt with nothing failing, so a
	// consumer reading this as "attempts that failed" over-counts — that is
	// FailedAttempts/ReapedClaims below (DKT-490). `next --run` and `step
	// show` read this column at fetch time, which for `next` is necessarily
	// BEFORE that cycle's own claim — so a step about to be claimed for the
	// first time reads 0. A rendered packet (`claim --render`) reads the SAME
	// column AFTER the claim's increment has committed, so the packet for that
	// identical claim reads 1. There are not two counters or two bases; it is
	// one column sampled at two different moments. See
	// docs/tdd/attempt-numbering.md (DKT-64) before wiring any threshold
	// (e.g. "escalate when attempt > N") against this field — get the
	// pre-/post-claim read timing right first.
	Attempt int `json:"attempt"`
	// FailedAttempts and ReapedClaims are the OUTCOME breakdown of the claims
	// Attempt counts (DKT-490): FailedAttempts is how many ended in an
	// explicit `step fail` — the holder measured its own work and recorded the
	// failure — and ReapedClaims is how many were reaped WITHOUT one (lease
	// expiry, `max_step_duration`, a forced `step reap`): the holder went
	// silent and nothing measured anything.
	//
	// They exist because Attempt alone cannot carry the distinction, and a
	// consumer that needed it guessed: an escalation policy walking
	// `attempt - 1` hops as though every spent claim were a failure escalated
	// a tier on a step whose only prior claim had merely been reaped. With the
	// breakdown on the row, "how many attempts genuinely failed" is
	// FailedAttempts, read directly — no event-log reconstruction (the log is
	// prunable, and instance labels repeat across a run's issues).
	//
	// A claim that RECORDED counts in neither — its ending is the artifact —
	// and `step resolve --as retry` touches neither, exactly as it leaves
	// Attempt alone. FailedAttempts+ReapedClaims never exceeds Attempt; the
	// remainder is live claims, recorded completions, and pre-v23 history
	// (zero on a pre-v23 claim means "no recorded breakdown", not "nothing
	// happened"). Both sample at the same moment Attempt does. `omitempty`,
	// so a row with no counted outcome serializes exactly as before.
	FailedAttempts int     `json:"failed_attempts,omitempty"`
	ReapedClaims   int     `json:"reaped_claims,omitempty"`
	ExpectedCost   float64 `json:"expected_cost"`
	// LeaseTTLS is SECONDS, per §11.4's `_s` suffix. Resolved from the
	// workflow's [limits] for the step's class, then `lease.ttl.<class>`, then
	// `lease.ttl.default`.
	LeaseTTLS int `json:"lease_ttl_s"`
	// Stage is the ORDERING CONSTRAINT WITHIN ONE OFFER: a
	// dispatcher must not start a row until every row of a LOWER stage in the
	// same set has completed. Rows sharing a stage have no ordering between
	// them and run concurrently.
	//
	// It exists because an offer can contain rows that must not start in an
	// arbitrary order, and it carries two cases that share one leveling
	// (engine/lookahead.go):
	//
	//   - Rows that are each individually READY and still order-dependent.
	//     The motivating case is observed, not hypothetical: a loop's fixer
	//     and its re-review judges are offered TOGETHER (`fix@1` with
	//     `review@1#0..3`), because the judges' readiness is restored by the
	//     loop re-entry that also mints the fixer. A judge spawned before the
	//     fixer finishes reviews the PRE-FIX tree. These rows render
	//     `status: ready` and stay claimable at any moment — for them the
	//     stage is a hint and core enforces no barrier.
	//   - Rows offered AHEAD of their own readiness (`status: staged`): the
	//     offer's dependency closure, placed after the in-offer rows whose
	//     recordings will ready them. For these the ordering is enforced by
	//     the predicate itself — `claim` refuses until the predecessors
	//     actually record — so a stage-skipping dispatcher gets a refusal,
	//     not a wrong result.
	//
	// Until this field existed the ordering lived only in dispatcher
	// convention — a writers-first partition keyed on `class`, in a script
	// outside this repo. That is a correctness property resting on a heuristic
	// core never stated: core assigns no meaning to a class name (§6.5), so a
	// dispatcher reading one is guessing, and any dispatcher that guessed
	// differently reviewed the wrong tree silently.
	//
	// ZERO MEANS UNSTAGED and is the default, so a set with no ordering
	// constraint serializes exactly as it did before this field existed
	// (`omitempty`). It is NOT a priority, and not a claim about importance:
	// it constrains START ORDER within one dispatch, nothing else.
	Stage int `json:"stage,omitempty"`
	// Conditional marks a staged row whose readiness depends on a HOLD-CAPABLE
	// in-offer predecessor — a step whose completion may HOLD for an operator
	// (an `aggregate` declaring `hold_spread`) instead of routing (DKT-26). The
	// stage boundary alone is not enough for such a row: every lower stage can
	// finish and the row still not be claimable, because a held predecessor is
	// finished-but-undecided until an operator resolves it. A dispatcher should
	// confirm the predecessor actually ROUTED before spawning anything for a
	// conditional row, or defer it to the next offer; spawning at the stage
	// boundary risks paying a full boot for a claim refusal.
	//
	// Like Stage, it is SET-RELATIVE and advisory: core enforces nothing with
	// it, `claim` remains the authority, and `omitempty` keeps every
	// unconditional row's bytes exactly as before.
	Conditional bool `json:"conditional,omitempty"`
	// Status is the EFFECTIVE status (§6.2) — `ready` when the §6.3 predicate
	// holds, which is never a stored value — or `staged` on an offer row
	// carried ahead of its readiness (db.StepStaged): every row this offer
	// scheduled at a lower stage must complete before this one is claimable.
	Status string `json:"status"`
	// BlockedReason names the §6.3 clause holding a `pending` step back from
	// `ready`, "" for any other status (DKT-470). It is what `step show`/`step
	// list` were missing for a step whose stall has no future event that
	// resolves it — an interposed step named by a threshold that already
	// routed elsewhere reads as ordinary `pending` forever, indistinguishable
	// from a step whose predecessor merely has not finished yet, unless
	// something says which clause is holding it and why.
	BlockedReason string `json:"blocked_reason,omitempty"`
	// LeaseExpired marks the EXPIRED-BUT-UNREAPED window (DKT-489): the stored
	// row is still `claimed` — nothing has reaped it — but its lease lapsed (or
	// `max_step_duration` passed), so `Status` above already renders the reap's
	// answer (`ready`/`pending`, §6.2) while `run repin` still counts the claim
	// as mid-flight, exactly as its quiescence guard must (the executor that
	// claimed under the current pins may still be writing until the reap
	// actually lands). Both surfaces are right — `claim` on such a step reaps
	// lazily and SUCCEEDS, so `ready` is honest about claimability, and the
	// repin refusal is honest about provenance — but until this field a caller
	// holding both answers ("ready" here, CONFLICT from repin) had no way to
	// see the window that reconciles them. `omitempty`, so every row outside
	// the window serializes exactly as before; offer rows (`next`, `dispatch
	// open`) never carry it because the offer path reaps for real first.
	LeaseExpired bool `json:"lease_expired,omitempty"`
	// Metadata is the definition's opaque KV, verbatim. Core never reads a key
	// inside it (genericity.md).
	Metadata map[string]any `json:"metadata,omitempty"`
}
