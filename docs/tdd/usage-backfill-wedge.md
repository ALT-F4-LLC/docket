# TDD: the wedge cluster — usage back-fill retires D2 (E-12 + E-4 + E-3)

Status: approved, revised — 2026-08-05 · design review APPROVED THE VERB
(§0-§1.4, §2, §3, §4.1, §4.2, §4.4, §5, §6) and **rejected §1.5's ordering fix
as designed**, on three mechanical grounds now recorded in §1.5 and severed to
**DKT-79**. Revisions applied: §1.5 replaced by the deferred-class record,
§2(E) corrected (ordering-alone would not limp — it is unimplementable as
drafted), §4.3's ordering bullets dropped, §1.3.1/§1.3.2 added (attempt
semantics and the always-explicit source). Tracker unit: **DKT-77** (the verb;
engine-spec verb-table amendment). Implements docs/design/engine-core.md §7's
back-fill promise ("the run skill back-fills from its dispatch journal, source
recorded") and closes the RUN-3 run-ending wedge recorded as E-12/E-4/E-3 in
the M4 findings register. Precedent: DKT-68/70, engine defects surfaced by
instance work and fixed in the engine. Spec of record is engine-spec.md;
deviations become DKT amendment issues per docs/design/amendments.md, never
silent changes.

**These three are one defect wearing three faces, and the register's own
priority order says so: (c) retires the case, (b) becomes unnecessary, (a)
becomes a safety net rather than a fix.** Read separately they invite three
mechanisms. Read together they need one write and one reordering.

## 0. The wedge, mechanically

RUN-3 ended here, and the chain is short enough to state in full:

1. An executor completes a step. It cannot pass `--usage`: the number it would
   report is its own token spend, which it does not know at completion time
   (H-6 is blocked on exactly this, not merely unimplemented).
2. `usage_recorded` stays 0 on that step. D2's probe
   (`missingUsage`, `dispatch.go:500-539`) reads that column and reports a
   discrepancy.
3. `refuseIfUnreconciledTx` (`next.go:137`) returns the refusal, and `next`
   returns early.
4. `driveActionSteps` (`next.go:205`) is **below** that return. Builtin action
   steps are driven from nowhere else — `claim` refuses them by design, because
   they are the engine's own work.
5. The run's `reconcile` step is an action step. It never runs. Nothing in the
   system can move it. RUN-3 wedged at DKT-75's reconcile with attempt 0.

`--accept-missing-usage` does not break the chain, and E-4 is precise about
why: acceptance is recorded in the **close event's data and the dispatch's
`close_reason`** (`dispatch.go:637-660`), while D2 is computed from
`steps.usage_recorded` and never reads either. Acceptance therefore does what
its contract says — the close succeeds — and cannot do what the operator
reasonably expects: the next `next` recomputes the same discrepancy from the
same column and refuses again, forever. Each subsequent close re-accepts the
entire set, so the audit trail grows one acceptance per completed step per
close (E-4's "audit noise" is this, and it is unbounded).

**The state that is missing is not acceptance. It is usage.** That reframing is
the whole design.

## 1. Mechanism

**Add `docket dispatch backfill-usage`: a verb that writes real usage rows for
steps whose claimant could not report them, with `source` naming the writer.**

### 1.1 Why this is the primary fix and not the workaround

The obvious reading is that E-12 is the run-ending bug and E-3 is a missing
convenience. It is the reverse. D2 exists to answer one question — "did the
work this relay dispatched get accounted for?" — and on RUN-3 the honest answer
was *no*: the usage was real, measured, and sitting in the wave journal with no
verb to carry it into the ledger. **D2 was not malfunctioning. It was correctly
reporting a gap the engine gave nobody a way to close.**

A step-scoped acceptance flag (the register's option (b)) would resolve the
wedge by teaching D2 to ignore that gap. It buys a green run by lowering the
bar, and every future run's spend data stays null while the ledger reports
success. The back-fill verb resolves it by closing the gap — the ledger gets
the numbers, D2 goes quiet because it is satisfied rather than suppressed, and
the budget's precision (E-3's "precision, not safety, is what degrades")
recovers.

### 1.2 No schema change — v10 already anticipated this

`currentSchemaVersion` stays 10. The `usage_ledger.source` column exists and
its own comment states the intent verbatim (`internal/db/usage.go:31-34`):

> Core writes `UsageSourceReported` and nothing else at this stage; the column
> exists so a harness back-filling from its own journal can record its own
> source later without a migration.

This TDD is that "later". The design was made in v10 and left one verb short.
No migration, no rewind-guard sentinel question (per the migration-sentinel
hazard: an index-only or column-only commit group would not migrate anyway).

The unique key `(step_id, attempt, unit)` already does the work a back-fill
needs: a re-claimed step's second attempt back-fills beside its first rather
than over it, and a double back-fill of the same attempt **fails loudly**
instead of silently merging. That is the correct behavior for a verb an
operator may re-run after a partial failure, and it is inherited, not added.

### 1.3 The verb

```
docket dispatch backfill-usage RUN-N --step STEP-N --unit U --quantity Q
                                     [--source S]   (default: "backfilled")
```

Repeatable `--step/--unit/--quantity` triples, or a JSON document on stdin for
the wave journal's whole batch — the harness has N spawns and should not need N
process launches:

```
docket dispatch backfill-usage RUN-N --from-json -
  [{"step":"STEP-12","unit":"tokens","quantity":48211}, …]
```

Each row is written through the existing `InsertUsageRowTx` and each named step
is marked via the existing `MarkStepUsageRecordedTx`, in **one transaction**:
a back-fill that half-applied would leave a dispatch that is neither closable
nor honestly re-runnable.

`source` defaults to `"backfilled"` and is free text, so the ledger
distinguishes a claimant's self-report from a relay's journal reconstruction
without core enumerating who the relays are.

#### 1.3.1 Attempt semantics: the recorded attempt, and no flag

The verb back-fills the step's **RECORDED attempt** — `steps.attempt` as it
stands at completion — and this is stated in `--help`, not left to inference.

**There is no `--attempt` flag, and its absence is the design.** Back-filling
an arbitrary historical attempt is rewriting history: the ledger's
`(step_id, attempt, unit)` key exists so a retried step's second attempt
records *beside* its first, and a verb that let an operator choose the attempt
number could forge a row against an attempt that never ran, or overwrite the
accounting of one that did. Refused by omission is the strongest refusal
available — there is no flag to misuse.

This composes with E-8. Once the attempt double-count is corrected, `attempt`
means what claims-leases §5 says it means (one per claim), so the attempt a
back-fill targets is unambiguous. That is why E-8 lands first (§7).

#### 1.3.2 The source is always explicit

The verb **always passes an explicit `source`** to `InsertUsageRowTx` — never
the empty string. `InsertUsageRowTx` defaults an empty source to
`UsageSourceReported` (`usage.go:51-54`), which means "a claimant said so". A
back-fill that fell through that default would label a relay's journal
reconstruction as a claimant's self-report, destroying the exact distinction
the `source` column exists to preserve. §4.2 pins this.

### 1.4 The genericity argument

The core surface gains no agent/LLM vocabulary. The verb names a **step**, a
**unit**, a **quantity**, and a **source** — the same four opaque terms the
ledger already carries. `"tokens"` never appears in core: it is a string an
instance chose, exactly as `"seconds"`, `"pages"`, or `"sheets"` would be
(`usage.go:14-21`). Nothing in the verb knows what a wave, a model, a prompt,
or an executor is; "the wave journal" is instance vocabulary that appears in
this document's rationale and in no identifier, flag, or message. A back-fill
from a CI runner's timing log is the identical call.

`--source` is deliberately unconstrained for the same reason `unit` is: core
enumerating valid sources would be core holding an opinion about who is allowed
to have measured the work.

### 1.5 (a): the ordering fix is SEVERED — deferred as DKT-79

An earlier draft of this TDD proposed moving `driveActionSteps` above
`refuseIfUnreconciledTx`, "joining the reap at `next.go:119`". **That is
rejected as designed, and the rejection is mechanical rather than a matter of
taste.** It is recorded here because the reasoning is not obvious and a future
round will otherwise rediscover it.

1. **Deadlock, not rollback.** `driveActionSteps` runs AFTER `tx.Commit()`, on
   the raw conn, and the adjacent comment (`next.go:178-188`) states why: the
   saga machinery opens its own transactions and `internal/db` caps the pool at
   ONE connection — "calling it from inside the transaction above would
   deadlock rather than fail." `RunActionStep` is saga work (`enterActionSaga`
   calls `conn.Begin()`; resume stages are separate CAS-guarded transactions by
   design). Joining the reap inside the transaction is unimplementable. The
   draft's rollback-safety argument borrowed `reapExpiredTx`'s semantics, and
   that function is tx-scoped precisely where `RunActionStep` cannot be without
   flattening the saga's crash-resume design.
2. **The `ready` slice does not exist yet.** `driveActionSteps` consumes
   `ready`, computed by the readiness pass that runs after the refusal — and
   after `resolveQuorumMisses`, whose resolutions change readiness. Driving
   pre-refusal needs its own action-readiness scan against a state the reap may
   still be mutating.
3. **No-half-advance cuts the other way.** The refusal's own comment says "a
   refused call must not half-advance the run", which is *why* the reap rolls
   back with it. Driving actions durably — in their own committed transactions
   — before returning a refusal is exactly that half-advance; and the rollback
   version that would honor the principle is what ground 1 rules out.

**The verb alone unwedges every known shape.** Back-fill quiets D2, `next`
stops refusing, and actions drive in their existing slot; §4.1's regression
passes with no reordering whatsoever. Nothing ordering-shaped ships in this
TDD.

The residual — a standing refusal starving engine self-work, should a future
discrepancy class arise with no operator-reachable resolution — is filed as
**DKT-79** with these three constraints recorded.

### 1.6 (b): step-scoped acceptance is REJECTED — with its verdict

See §2. It is the alternative that most deserves a verdict rather than
silence, because it is what E-4's title literally asks for.

## 2. Alternatives, with verdicts

**(A) Step-scoped acceptance the D2 probe consults — REJECTED.**
Add `steps.usage_accepted`, set it at close under `--accept-missing-usage`,
and teach `missingUsage` to read it. It resolves the wedge, retires the audit
noise (acceptance recorded once per step, not once per close), and is a small
diff.

It is rejected on **two** grounds, and the second is the disqualifying one.

First, it is a schema change (v11) to store a fact whose only purpose is to
suppress a probe, when the fact the system actually wants — the usage — needs
no schema change at all.

Second and decisively: **it makes the ledger lie by design.** A run closed
under blanket acceptance reports spend equal to declared `expected_cost` while
the real usage is discarded. E-3 already records that the budget floor is the
safety mechanism and precision is what degrades; acceptance-as-a-fix makes that
degradation *permanent and invisible*, because a suppressed probe never
recovers. Worse, it removes the pressure that produces the back-fill: a green
run is the strongest possible argument that nothing is missing.

Acceptance's real contract — "let this close proceed" — is already correctly
implemented, and this TDD leaves it exactly as it is.

**(B) Make `--accept-missing-usage` mint zero-quantity ledger rows — REJECTED.**
It needs no schema change and D2 goes quiet through the front door. But a
`quantity = 0` row is a **false measurement**, not an absent one, and every
per-unit sum silently absorbs it. "Nobody measured this" and "this cost
nothing" must not be the same row. Rejected on genericity's own terms: core
would be asserting a quantity it has no basis for.

**(C) Compute usage engine-side — REJECTED.** Core would have to know what a
token is. Disqualified by the genericity rule on sight.

**(D) Drop D2 for executor-claimed steps — REJECTED.** This deletes the only
accountability check over precisely the steps that spend the most. It solves
the refusal by abolishing the question.

**(E) Ordering fix alone, no back-fill — REJECTED, and it would not even
limp.** An earlier draft rejected this as "half the fix": actions would run,
but `next` would still refuse the readiness pass forever, so no *executor* step
is ever offered again.

That understated it. Per §1.5, a pre-refusal drive is not merely insufficient —
it is not implementable as described. Inside the transaction it deadlocks
against the one-connection pool; driven durably outside it, it violates the
no-half-advance rule the refusal exists to enforce. **Ordering-shaped fixes are
not a lesser version of this TDD; they are a different design problem**, filed
as DKT-79. Nothing ordering-shaped ships here.

## 3. Computed-never-stored, honored

engine-core §5 makes `ready` computed and never stored as intent, and D2's
probe follows the same discipline. **The instruction to mind that rationale is
what rules out (A) and rules in the back-fill**, so it is worth stating why the
proposed write is not the thing the rationale forbids.

The rationale forbids storing a **judgment** that can be derived — a cached
`ready` bit goes stale the moment the world moves, and a stored
`usage_accepted` bit is a stored opinion about a question the probe re-asks
every call.

`usage_ledger` rows are not judgments. They are **records of an event that
happened once**: this step, this attempt, consumed this quantity of this unit.
That is the same category as the artifact, the event, and the completion
metadata DKT-68 added — facts about a past transition, which is exactly what
the database is for. `usage_recorded` remains what it already is: a
denormalized fast path over rows that exist (`usage.go:66-73`), not a new
opinion. D2 stays computed; it simply starts finding what it was always looking
for.

## 4. Test plan

Sections in `scripts/qa.sh` with helpers under `scripts/qa/`, plus Go tests.

### 4.1 The wedge, end to end (the regression that would have caught RUN-3)

A run whose executor step completes **without** `--usage`, followed by a
`next`, asserting the refusal; then `backfill-usage`; then a `next` that
**offers rows and drives the action step**. This is RUN-3's exact shape and
fails on today's HEAD at the final assertion.

### 4.2 Back-fill unit tests

- writes `(step, attempt, unit, quantity, source)` and flips `usage_recorded`;
- `source` defaults to `"backfilled"`, and `--source` overrides it;
- the verb NEVER writes an empty source, so `InsertUsageRowTx`'s
  empty→`"reported"` default can never label a back-fill as claimant-reported
  (§1.3.2 — asserted at the DAO boundary, not only at the flag);
- rows land against the step's RECORDED attempt, and there is no flag to name
  a different one (§1.3.1);
- a second back-fill of the same `(step, attempt, unit)` **refuses** (the
  unique key asserted, not worked around);
- a back-fill naming a step of another run refuses;
- partial failure inside a batch writes **nothing** (one transaction);
- D2 goes quiet for exactly the back-filled steps and stays loud for others.

### 4.3 Standing regressions over the action/D2 boundary

The two ordering bullets are gone with §1.5. These two remain, and they are
worth asserting rather than assuming — they are what makes the back-fill
sufficient on its own:

- a step in a saga is not re-driven (the existing `InSaga` skip, unbroken);
- action steps remain D2-exempt (D5 regression) — the disjointness between
  action driving and the usage probe is asserted, not assumed, so a later
  change to D5's exempt set cannot silently reintroduce the wedge.

### 4.4 Acceptance is unchanged

`--accept-missing-usage` still closes and still records its event data. Its
existing tests must pass **untouched** — this TDD adds a path, it does not
alter that one.

## 5. Migration discipline

None required. `currentSchemaVersion` stays 10; no table, column, or index
changes. The verb writes through existing DAO functions to existing columns.

## 6. Surface documentation

`skills/docket/SKILL.md` gains `dispatch backfill-usage` in the same PR, per
the CLAUDE.md standing rule — flag table and verb table both, or the PR is
drift and blocks review.

## 7. Sequencing: E-8 lands first

The attempt double-count (E-8, `FailStep`'s bump over `ClaimStep`'s) is fixed
**before** this verb, and the reason is specific to this TDD's tests: §4.2
keys ledger rows by `(step_id, attempt, unit)` and §1.3.1 back-fills the
step's recorded attempt. Written against today's arithmetic, those tests would
pin attempt numbers that E-8's correction then changes — a test asserting a
number the next commit invalidates.

Order: **E-8 → this verb (DKT-77) → input payloads (DKT-78)**. Each is a
separate commit, each buildable with tests green.
