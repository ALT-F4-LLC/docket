package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// BUDGETS — engine-core §7, implemented clause by clause (TDD §4).
//
// The load-bearing decision of this whole file is §4.3: THE FLOOR IS NOT A
// STORED COUNTER, IT IS A QUERY OVER THE EVENT LOG. Everything else follows
// from it — the concurrency property (C4: a SUM over an append-only log has no
// read-modify-write, so there is no lost update to defend against), the
// engine-owned-facts argument (§7's floor rests on "facts the engine itself
// produced", and `step-claimed` events ARE those facts), the audit trail (§9
// item 2: a breach attributable to seventeen claim events is one an operator can
// read; a breach attributable to a counter is attributable to nothing), and
// replay (engine-core §9 reconstructs "costing what" from the log alone).
//
// `runs.usage_floor` exists and is written beside every accrual, but it is a
// CACHE FOR THE REPORT and is never read by enforcement.
// TestFloorIsNeverReadFromCache poisons it and asserts every decision below
// behaves as though it said the truth.

// budgetSnapshot is the budget half of the scheduler's snapshot: the effective
// cap, where it came from, and the two quantities `max(reported, floor)` is
// taken over.
//
// It is loaded ONCE PER LoadScheduler, for the reason the Scheduler is a value
// at all (§4.8 B31): R3, R4, R5, and now R7 are questions about the same rows at
// the same instant. A predicate that re-queried the floor between clauses could
// answer two steps against two different floors and offer a batch that cannot
// all be claimed.
type budgetSnapshot struct {
	// cap is the effective cap. ZERO MEANS UNLIMITED at both levels (B2) — the
	// flag's documented meaning since S3, unchanged.
	cap float64
	// source names where the cap came from, for the report's R6 line. An
	// operator asking "why didn't it stop?" gets the answer from the report
	// rather than from the source.
	source BudgetSource
	// unit is `budget.unit`: which recorded usage unit the cap counts. Empty —
	// the default and the only value core ships — means the cap rests on the
	// floor alone (B17).
	unit string
	// floor is §4.3's SUM over the run's `step-claimed` events, computed inside
	// the transaction that loaded this snapshot.
	floor float64
	// reported is the ledger sum for `unit`, or 0 when no unit is named (B16,
	// B17). Core NEVER sums across units: that would be core asserting two
	// opaque units are commensurable, which is an opinion about what they mean.
	reported float64

	// usageCap is the SECOND dimension: a cap over MEASURED usage rather than
	// declared costs (DKT-238). Zero means unlimited, like `cap`.
	//
	// It is separate rather than folded into `spend` because the two
	// quantities are not commensurable, and folding them would destroy the
	// declared-cost discipline the instant real tokens were armed: a raise
	// tribunal deliberated 140 -> 280 declared units against a run whose
	// measured spend ran to hundreds of millions of tokens, and `max()` over
	// those two numbers only ever answers the second.
	usageCap float64
	// usageUnit is `budget.usage.unit`: which recorded unit the measured cap
	// counts. Empty leaves the dimension DORMANT — a cap with nothing to count
	// enforces nothing — so both a cap and a unit are needed to arm it.
	usageUnit string
	// usageSpend is the ledger sum for `usageUnit`. Unlike `spend` it is NOT
	// maxed against the floor: this dimension is about what was measured, and
	// a declared-cost floor is not a measurement.
	usageSpend float64
}

// usageArmed reports whether the measured dimension is enforcing.
//
// BOTH halves are required, and the AND is the dormancy guarantee: a cap with
// no unit has nothing to count, and a unit with no cap has nothing to exceed.
// Every path that must not query asks this one question, the same discipline
// `unlimited` gives the declared dimension.
func (b budgetSnapshot) usageArmed() bool {
	return b.usageCap > 0 && b.usageUnit != ""
}

// usageAdmits answers the measured dimension's question — and it is a
// DIFFERENT question from admits().
//
// A step's declared cost is known before it runs, so the declared cap can
// RESERVE: `spend + cost <= cap`. A step's token spend is not knowable in
// advance — that is the whole reason this dimension reads the ledger rather
// than the definition — so the measured cap can only STOP: work continues
// while recorded usage is at or under the cap, and the first claim after it is
// exceeded is refused. Pretending a reservation exists here would mean
// inventing a per-step token estimate, which is exactly the declared-cost
// proxy DKT-238 filed against.
func (b budgetSnapshot) usageAdmits() bool {
	if !b.usageArmed() {
		return true
	}
	return b.usageSpend <= b.usageCap
}

// usageHeadroom is what is left under the measured cap.
func (b budgetSnapshot) usageHeadroom() float64 {
	if !b.usageArmed() {
		return 0
	}
	return b.usageCap - b.usageSpend
}

// BudgetSource names where an effective cap came from — the report's R6 line
// (§4.10 R2) and nothing else. It is scheduling vocabulary: a flag, a config
// key, or neither.
type BudgetSource string

const (
	// BudgetFromRun is `run start --budget N` (B1's first branch).
	BudgetFromRun BudgetSource = "run"
	// BudgetFromConfig is `docket config budget.default` (B1's second branch).
	BudgetFromConfig BudgetSource = "config"
	// BudgetUnlimited is B1's third branch: 0 at both levels.
	BudgetUnlimited BudgetSource = "unlimited"
)

// spend is the enforced quantity: `max(reported, floor)` (B12).
//
// REPORTED USAGE CAN ONLY RAISE THE COUNTER (B13). That is what `max` means, and
// it is why a claimant reporting `0` — or nothing at all — cannot lower a run
// below its floor. §9 item 7's whole proof is this one line.
func (b budgetSnapshot) spend() float64 {
	if b.reported > b.floor {
		return b.reported
	}
	return b.floor
}

// unlimited reports B1's third branch. It is a method rather than an inline
// comparison so D1's dormancy has ONE definition: every path that must not query
// asks the same question.
func (b budgetSnapshot) unlimited() bool { return b.cap <= 0 }

// admits answers B14: would claiming a step of this cost CROSS the cap?
//
// "Crossing" is `>`, not `>=`. A cap of 12 and a spend that reaches exactly 12
// has SPENT its budget and not exceeded it, so a claim landing exactly on the
// cap is allowed and the next one is refused. Writing this as `>=` would make a
// cap of 12 mean "stop at 11.x", which is not what a number on a flag says.
// headroom is what is left under the cap: the number a withheld step's cost
// was compared against. Negative is possible in principle — a cap lowered
// under an existing spend — and is reported as-is rather than clamped, since
// "headroom -3" and "headroom 0" are different situations for an operator.
func (b budgetSnapshot) headroom() float64 {
	if b.unlimited() {
		return 0
	}
	return b.cap - b.spend()
}

func (b budgetSnapshot) admits(cost float64) bool {
	if b.unlimited() {
		return true
	}
	return b.spend()+cost <= b.cap
}

// loadBudget builds the snapshot inside the caller's transaction.
//
// B29's dormancy is enforced HERE as well as at the predicate: with an effective
// cap of 0, NEITHER the floor query NOR the ledger query executes. A run started
// without `--budget` in a repo that never set `budget.default` therefore runs
// exactly the queries v9 ran, which is D1 — and the assertion is a counting
// driver in the test rather than an inspection of this comment.
//
// THE CAP IS READ FROM THE RUN ROW AND FROM NOWHERE ELSE (B3). It is not
// re-read from config mid-run: a config change must not silently re-cap a live
// run, for the same reason a re-registered workflow does not reach one (RA2,
// engine-spine §5.4).
//
// B1's second branch — "else `docket config budget.default` when non-zero" — is
// therefore resolved at `run start`, which materializes the default INTO the
// row. That is what makes B3 and B1 one rule rather than two that disagree: by
// the time any claim reads it, the row already carries whichever of the two
// applied, and the run's cap cannot move underneath it.
//
// The consequence B3 states plainly: a run started before `budget.default` was
// set has 0 stored and stays UNLIMITED even after the default is set. That is
// the pinning property applied to budgets, and it is surprising enough that
// `run report` prints the effective cap AND where it came from (R6), so an
// operator asking "why didn't it stop?" gets the answer from a read verb.
// The SOURCE is not stored. §2.3's column table adds no `budget_source`, and
// inventing one would be a silent deviation — so the report derives it by
// comparing the stored cap to the config default it would have been given
// (BudgetSourceOf). That comparison can be wrong in exactly one harmless way: a
// run whose `--budget` happened to equal the default reads as `config`. Both
// answers name a number the operator can act on, which is what R6 is for.
func loadBudget(tx *sql.Tx, run *model.Run) (budgetSnapshot, error) {
	snap := budgetSnapshot{cap: run.Budget, source: BudgetFromRun}

	// The MEASURED dimension is loaded first and independently (DKT-238): the
	// two caps are separate controls, so a run capped on tokens but not on
	// declared cost must still be enforced. Its own dormancy is preserved —
	// with no `usage_budget` on the row, not one query runs.
	if run.UsageBudget > 0 {
		usageUnit, err := db.UsageBudgetUnitTx(tx, run.ProjectID)
		if err != nil {
			return budgetSnapshot{}, err
		}
		if usageUnit != "" {
			usageSpend, err := db.ReportedUsageTx(tx, run.ID, usageUnit)
			if err != nil {
				return budgetSnapshot{}, err
			}
			snap.usageCap, snap.usageUnit, snap.usageSpend =
				run.UsageBudget, usageUnit, usageSpend
		}
	}

	if snap.cap <= 0 {
		// D1: unlimited in the DECLARED dimension. No floor query and no
		// per-unit ledger read run from here — the measured dimension above
		// carries its own numbers and its own dormancy.
		return budgetSnapshot{
			source:   BudgetUnlimited,
			usageCap: snap.usageCap, usageUnit: snap.usageUnit,
			usageSpend: snap.usageSpend,
		}, nil
	}

	unit, err := db.BudgetUnitTx(tx, run.ProjectID)
	if err != nil {
		return budgetSnapshot{}, err
	}
	snap.unit = unit

	floor, err := RunFloorTx(tx, run.ID)
	if err != nil {
		return budgetSnapshot{}, err
	}
	snap.floor = floor

	if snap.unit != "" {
		reported, err := db.ReportedUsageTx(tx, run.ID, snap.unit)
		if err != nil {
			return budgetSnapshot{}, err
		}
		snap.reported = reported
	}
	return snap, nil
}

// BudgetSourceOf names where a run's stored cap came from — R6's line.
//
// It COMPARES rather than reads a stored source, because §2.3 adds no
// `budget_source` column and inventing one would be a silent deviation from the
// column table this stage ratified. The comparison is exact in every case that
// matters and ambiguous in exactly one that does not: a run whose `--budget`
// happened to equal the config default reads as `config`. Both answers name the
// same number, and the number is what an operator asking "why didn't it stop?"
// needs.
func BudgetSourceOf(cap, configDefault float64) BudgetSource {
	switch {
	case cap <= 0:
		return BudgetUnlimited
	case configDefault > 0 && cap == configDefault:
		return BudgetFromConfig
	default:
		return BudgetFromRun
	}
}

// RunFloorTx is §4.3, the whole of it:
//
//	SELECT COALESCE(SUM(s.expected_cost), 0)
//	  FROM events e JOIN steps s ON s.id = e.step_id
//	 WHERE e.run_id = ? AND e.kind = 'step-claimed'
//
// Every clause of §4.3's table falls out of this query rather than needing its
// own code:
//
//   - B9, retries re-accrue: a reaped step claimed again writes a SECOND
//     `step-claimed` event, and the SUM counts both. Nothing is released on
//     reap, on fail, or on abandon — the work was attempted and the attempt is
//     what cost something.
//   - B10, loop entries re-accrue: a `fix` step at ordinal 1 is a DIFFERENT step
//     row with its own `expected_cost`, claimed and event-logged independently.
//     `max_fix_loops` therefore bounds the floor BY CONSTRUCTION, which is
//     engine-core §7's "bounded loops bound the floor" arriving from §11.3
//     rather than from arithmetic here.
//   - B11, a superseded, skipped, or never-claimed step contributes nothing: the
//     accrual is per CLAIM EVENT, and a step never claimed produced none.
//   - B5, the value accrued is the STEP ROW's `expected_cost`, materialized at
//     expansion from the pinned definition and never re-read from the live
//     `workflows` table. A run pins its definitions; its floor is computed from
//     what it pinned.
//
// It is exported because `run report` computes the same number from outside this
// package and the two must not be able to disagree — a report that recomputed
// the floor its own way would be a second source of truth for the number a
// breach is attributed to.
func RunFloorTx(tx *sql.Tx, runID int) (float64, error) {
	var floor float64
	err := tx.QueryRow(
		`SELECT COALESCE(SUM(s.expected_cost), 0)
		   FROM events e JOIN steps s ON s.id = e.step_id
		  WHERE e.run_id = ? AND e.kind = ?`,
		runID, EventStepClaimed,
	).Scan(&floor)
	if err != nil {
		return 0, fmt.Errorf("computing the floor for %s: %w",
			model.FormatRunID(runID), err)
	}
	return floor, nil
}

// `--usage` — recorded, capped, opaque (§4.9).
//
// WHAT CORE NEVER DOES WITH THESE NUMBERS: interpret them, convert them,
// rate-limit on them, or route on them. They sum in the report and, when
// `budget.unit` names one, they participate in `max()`. That is the whole
// contract.

// parseUsage is B33, B35, and B36, delegated to db.ParseUsage — the single
// implementation the vote ledger's `--usage` shares (DKT-95) — with the
// refusal re-raised as this surface's VALIDATION_ERROR naming this surface's
// flag (db's refusals name only the offending key or value).
func parseUsage(raw string) ([]db.UsageRow, error) {
	rows, err := db.ParseUsage(raw)
	if err != nil {
		return nil, validationErr("--usage: %v", err)
	}
	return rows, nil
}

// recordUsageTx writes one `complete`'s `--usage` into the ledger (B34).
//
// The EVENT still carries the blob (`step-recorded`'s `data`), unchanged: this
// stage ADDS ledger rows rather than moving the usage into them. Removing it
// from the event would break the replay property — engine-core §9 reconstructs a
// run's history "costing what" from the log, and a log that dropped the numbers
// could not.
//
// `source` is `reported` because a claimant said so. engine-core §7's "the
// reference harness back-fills from its dispatch journal, source recorded"
// describes that harness's practice, not a core surface: no verb at this stage
// writes any other value.
func recordUsageTx(tx *sql.Tx, step *db.Step, raw string, nowMS int64) error {
	rows, err := parseUsage(raw)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	for _, row := range rows {
		row.RunID, row.StepID, row.Attempt = step.RunID, step.ID, step.Attempt
		if err := db.InsertUsageRowTx(tx, row, nowMS); err != nil {
			return err
		}
	}
	return db.MarkStepUsageRecordedTx(tx, step.ID)
}

// cacheRunFloorTx recomputes the floor and writes it to `runs.usage_floor`.
//
// Called after each accrual, in the accrual's own transaction. It is the ONLY
// writer of that column and it is a cache for the report's burn-rate line: the
// number it stores is the same query enforcement runs, so a report and a breach
// never disagree — but enforcement never READS it, so a poisoned cache changes
// no decision (§4.3, §3.2).
func cacheRunFloorTx(tx *sql.Tx, runID int) error {
	floor, err := RunFloorTx(tx, runID)
	if err != nil {
		return err
	}
	return db.CacheRunFloorTx(tx, runID, floor)
}

// budgetHeadroom is R7 — engine-spine §6.3's seam, now the real check (§4.8).
//
// B29: it returns true ON ITS FIRST LINE when the effective cap is 0, and the
// snapshot's own loader has already declined to query in that case, so D1's
// dormancy is structural rather than a fast path someone could reorder away.
//
// B30: otherwise it answers B14's arithmetic over the scheduler's snapshot —
// would claiming THIS step cross the cap?
//
// R7's POSITION IN THE CONJUNCTION DOES NOT MOVE. It stays last, per §6.3's
// stated ordering rationale (cheapest and most global first, most local last),
// and budget is the most local of all: it is the only clause whose answer
// depends on the specific step's cost.
func (s *Scheduler) budgetHeadroom(step *db.Step) bool {
	// BOTH dimensions must admit (DKT-238). They are independent caps over
	// different quantities, so neither can vouch for the other, and a step
	// passes only when it crosses neither.
	if !s.budget.usageAdmits() {
		if s.budgetHolds == nil {
			s.budgetHolds = make(map[string]float64)
		}
		s.budgetHolds[step.Instance] = step.ExpectedCost
		return false
	}
	if s.budget.unlimited() {
		return true
	}
	if s.budget.admits(step.ExpectedCost) {
		return true
	}
	// Record the withholding so the offer can SAY it (DKT-242). Denying a step
	// and reporting nothing is what let a round serialize behind an invisible
	// wall: the dispatcher saw one row where five were ready and had no way to
	// tell a budget hold from a graph that had genuinely run dry.
	if s.budgetHolds == nil {
		s.budgetHolds = make(map[string]float64)
	}
	s.budgetHolds[step.Instance] = step.ExpectedCost
	return false
}

// Budget exposes the loaded snapshot's decided numbers to callers outside this
// package — the claim's enforcement path and the report.
//
// It hands over the ANSWER rather than the apparatus, for the reason StepView
// does: a caller holding the raw rows could compute a different spend from the
// one the scheduler decided against, and the two would disagree exactly when it
// mattered.
func (s *Scheduler) Budget() (cap, floor, reported, spend float64, source BudgetSource, unit string) {
	return s.budget.cap, s.budget.floor, s.budget.reported, s.budget.spend(),
		s.budget.source, s.budget.unit
}

// UsageBudget exposes the MEASURED dimension's decided numbers (DKT-238), for
// the same reason Budget does: a caller re-deriving them from the ledger could
// compute a different spend than the one the scheduler enforced against.
func (s *Scheduler) UsageBudget() (cap, spend float64, unit string, armed bool) {
	return s.budget.usageCap, s.budget.usageSpend, s.budget.usageUnit,
		s.budget.usageArmed()
}

// BudgetBreachReason is B21's shape:
//
//	budget: spend <N> of cap <M> reached at <instance>
//
// A BARE-NUMBER STATEMENT NAMING NO UNIT (§1.1's second leak, closed). There is
// no currency, no token, and no rate: `--budget 12` means "stop when the accrued
// number reaches 12", and what the number counts is the workflow author's
// business. `%g` renders 12 as "12" and 12.5 as "12.5" rather than padding
// either into a fixed-point shape that would imply a denomination.
func BudgetBreachReason(spend, cap float64, instance string) string {
	return fmt.Sprintf("budget: spend %g of cap %g reached at %s", spend, cap, instance)
}

// UsageBudgetBreachReason is BudgetBreachReason for the MEASURED dimension
// (DKT-238).
//
// It NAMES THE UNIT, and that is the one place these two messages differ. The
// declared cap counts a number whose meaning is the workflow author's business,
// so naming a denomination there would be core inventing one; the measured cap
// counts a unit the operator themselves configured, and a breach that did not
// say which one leaves them unable to tell a token cap from a seconds cap — or
// to tell this stop from a declared-cost stop, which a different verb fixes.
func UsageBudgetBreachReason(spend, cap float64, unit, instance string) string {
	return fmt.Sprintf(
		"usage budget: measured %s spend %g of cap %g reached at %s",
		unit, spend, cap, instance)
}

// enforceBudgetTx is B14, B15, B20, and B22 in one function, called from inside
// the claim's transaction (C5).
//
// THE CHECK AND THE ACCRUAL ARE THE SAME TRANSACTION AS THE CLAIM'S CAS. There
// is no window in which a claim is authorized and the cap is then crossed by
// another: a claim that would cross does not commit, and the serialization is
// SQLite's rather than ours.
//
// The RELATIONSHIP TO R7, stated because it looks like duplication: R7 makes a
// step not APPEAR in `next`; this makes a claim REFUSE. They are the same
// arithmetic at two moments and both are needed — R7 alone would let a relay
// claim a step it already held a stale `next` row for, and this alone would
// offer steps that cannot be claimed. TestBudgetR7AndClaimAgree asserts they
// never disagree over one snapshot.
//
// It returns the refusal, having ALREADY flipped the run, so the caller commits
// the flip and returns the error. That ordering is deliberate: the pause and the
// refusal are one fact, and a refusal that rolled back its own pause would leave
// a run that refuses every claim while reporting itself active.
func enforceBudgetTx(tx *sql.Tx, sched *Scheduler, step *db.Step, nowMS int64) error {
	if sched.budgetHeadroom(step) {
		return nil
	}

	// WHICH cap was crossed is named, because the two are different controls
	// with different answers: raising a declared-cost cap does nothing for a
	// run stopped on measured tokens (DKT-238).
	var reason string
	if !sched.budget.usageAdmits() {
		reason = UsageBudgetBreachReason(
			sched.budget.usageSpend, sched.budget.usageCap,
			sched.budget.usageUnit, step.Instance)
	} else {
		reason = BudgetBreachReason(
			sched.budget.spend(), sched.budget.cap, step.Instance)
	}

	// B22: the flip is CAS on (run_id, status='active') (C6). A concurrent
	// invocation that already flipped it matches zero rows and writes neither a
	// second reason nor a second event — the transition is idempotent because
	// the guard is the status itself rather than a flag we maintain.
	flipped, err := db.BreachRunBudgetTx(tx, step.RunID, reason, nowMS)
	if err != nil {
		return err
	}
	if flipped {
		// B23: the event is `run-paused` with `data.reason = "budget"` — an
		// EXISTING closed-set kind, so the set does not widen. A new
		// `budget-breached` kind would say the same thing in a word only this
		// mechanism uses, and §9 item 2's attribution table would gain a row for
		// a transition that is already attributable as a pause.
		if err := recordEvent(tx, eventRecord{
			Kind: EventRunPaused, RunID: step.RunID,
			Instance: step.Instance, IssueID: step.IssueID,
			Data: `{"reason":"budget"}`, AtMS: nowMS,
		}); err != nil {
			return err
		}
	}

	// B24 lives in what this does NOT do: nothing here clears the cap, so a
	// `run resume` returns the run to `active` and the very next claim reaches
	// this same refusal. That reads as a bug and is not one — the cap has not
	// moved, so the condition has not changed. Raising it is `run start`-time
	// only in v1 (B25).
	return conflictErr(
		"step %s is not ready to claim: %s (%s)",
		step.Instance, CondBudget, reason)
}
