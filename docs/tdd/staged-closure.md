# Staged offers: the dependency closure

Status: implemented — 2026-08-15 · operator-requested (session evidence: a
conductor asked "is it not possible to have judges and synthesize in the same
wave?" and the honest answer was no — `next` only ever offered steps ready
RIGHT NOW, so every dependency edge cost one full open/dispatch/close round).

This note records the contract; the reasoning lives beside the code it
governs (`internal/engine/lookahead.go`, `internal/engine/drive.go`) per the
repo's comment discipline.

## 1. The rule

An offer (`next --run`, `dispatch open`) is widened to its own CLOSURE: a
pending step that is not ready joins the offer at a later `stage` when
everything still standing between it and readiness is itself in the offer at
a lower stage. Two ways in, one reading — "the offer promises only what its
own execution unlocks":

1. a READY step rationed out of the claimable cohort by ClaimablePrefix
   (class headroom, scope) — this offer's own admissions are what block it,
   so this offer's own completion frees it, one stage later;
2. a step whose R3 is unsatisfied where every non-terminal predecessor
   instance is in the offer — staged after the highest predecessor stage,
   transitively (`judges -> synthesize -> report` in one manifest).

The closure stops at everything a wave cannot resolve: a `type="human"` step
(and all its downstream), a predecessor outside the offer, an issue whose
`depends_on` is unsatisfied (R2 resolves at issue completion — rollup work no
wave owns), a budget the summed closure would breach (R7 stays WHOLE-offer),
and a bounded class holding an unacknowledged reap (the hold clears by a
relay's ack, not by anything the wave executes).

## 2. The wire

- A pre-offered row renders `status: "staged"` — a second COMPUTED status
  beside `ready` (db.StepStaged), never stored, never an answer `step show`
  gives. The wire does not lie about claimability: `claim` re-checks R1–R7
  and refuses a staged row until its predecessors actually record, so a
  stage-skipping dispatcher gets a named refusal, never a wrong result.
- `stage` is now the whole offer's leveling: the loop-re-entry ordering
  (precedesInSet) and the closure's `after` edges feed ONE longest-path
  computation. Ready rows keep the old contract exactly — hint only, stage 0
  or later, claimable at any moment.
- Rows are emitted stage-major (DKT-38's rule over the whole closure), so a
  `--limit` prefix cut cannot separate a row from a dependency, and the
  post-cut loop-body eviction the flat offer needed (DKT-58/DKT-75) is
  retired by construction.
- `Total` counts the whole offer, staged rows included.
- A staged VOTE row carries `voters` (the roster a dispatcher needs to plan
  the panel) and NO `proposal` — the ballot does not exist until the step is
  ready (§8.1 phase 2 is unchanged).
- Class headroom and scope disjointness hold PER STAGE (stages run
  sequentially and never compete); budget holds per offer. Occupancy outside
  the offer is not charged against later stages — `claim` answers that at
  the only moment the answer is real.
- `dispatch open`'s TTL floor was already the sum over stages of each
  stage's largest lease (stagedLeaseSumMS); a deep closure simply sums more
  stages, so a manifest outlives the whole wave it feeds.

## 3. Mid-wave lifecycle driving

`next` refuses while a dispatch is open (P24, unchanged), so the verbs a wave
actually calls become §8.1's "first engine invocation that observes the step
ready":

- `step record` / `step fail` (CLI layer) call
  `Engine.DriveRunLifecycles` after their write commits: newly-ready vote
  steps get their proposals opened, newly-ready action steps run
  engine-side, to a cascade fixed point.
- the `vote cast` that reaches quorum calls `Engine.DriveVoteProposal`,
  recovering the run from the proposal's `vote-step:<run>:<issue>:<instance>`
  idempotency key and routing the decided gate (phases 4/5) — downstream
  staged rows are claimable the moment the deciding cast returns. An ad-hoc
  proposal (no such key) is a no-op.

`engine.CompleteStep` itself is unchanged — the saga's own contract and its
test surface stay put; the driving is the VERB layer's act, exactly as it is
for `next` and `dispatch open`.

## 4. Verify

`dispatch verify` normalizes lifecycle progress the manifest scheduled
(comparableVerifyBytes): `Stage` cleared (DKT-19, unchanged), and when the
stored row was `staged` — status normalized to `ready` and `proposal` cleared
on both sides, because a staged row BECOMING ready, and a staged vote row
acquiring its proposal, are the batch working as scheduled, not conflicts.

## 5. Failure semantics

Failure fails soft and costs what it always did: a staged row whose
predecessor failed or was rejected never becomes ready, its claim refuses,
the dispatcher skips it, it is still `pending` at close (no discrepancy — a
manifest is not a lock, P28), and the next round offers whatever `on_fail`
routing produced. Success costs zero extra rounds.

## 6. Deviations recorded

- §6.2's "nine persisted statuses + computed `ready`" gains a SECOND
  computed, offer-only value, `staged`. Additive; never persisted
  (TestReadyIsNeverPersisted's discipline applies).
- §11.4's `next row` `status` vocabulary widens by the same value, and
  `stage` (already an A1-class additive field) now also labels staged rows.
- §6.3's offer ("the ready set") becomes "the ready set plus its staged
  closure"; the READINESS PREDICATE R1–R7 is untouched and remains the
  claim-time authority.
