# Note: `attempt` numbering across surfaces (DKT-64)

Status: reconciliation note, not a new stage — 2026-08-21 · Pins the semantics
of the `steps.attempt` / `issues.attempt` column (introduced in
docs/tdd/claims-leases.md §2, schema v6) across every surface that renders it,
after RUN-5 (2026-08-08) found the rendering inconsistent enough to mislead
two independent consumers. Amended for DKT-490 (schema v23): the retry-reset
claim v16 obsoleted is corrected, and the failure-vs-reap marker §"What to
check" called for now exists (`failed_attempts` / `reaped_claims`).

## The one counter

There is exactly one counter, `attempt` (`internal/db/schema.go`), and one
place it changes:

- **Increment**: `internal/db/leases.go` — `attempt = attempt + 1` inside the
  CAS transaction of a *winning* claim. This is shared by issue-level and
  step-level leases (`docket issue claim`, `step claim`).
- **Nothing else ever changes it.** `heartbeat` and `release` leave it
  untouched (docs/tdd/claims-leases.md §9, R8/R9-adjacent rows), a reap leaves
  it untouched (the dead claim already counted itself), `step fail` leaves it
  untouched (E-8: bumping at failure made the counter measure claims PLUS
  failures), and — since v16, DKT-86/DKT-90 — `step resolve --as retry`
  leaves it untouched too: retry refreshes the budget by moving
  `steps.attempt_base` to the current attempt, so exhaustion compares
  `attempt - attempt_base` against `max_attempts` while the counter itself
  stays the usage ledger's key half. (This section originally said retry
  resets `attempt` to 0; that was true until v16 and is exactly the zeroing
  that made a retried step's second execution collide with the first's ledger
  slot.)

Semantically it is a **monotonic, 0-based, spent-count**: 0 means "never
claimed", N means "claimed N times so far". It is never a 1-based
"current attempt number" — there is no second field that means that.

## Why two surfaces render two different numbers for the "same" attempt

Every renderer — `next --run` (`internal/render/step.go:RenderStepRows`),
`step show` (`internal/render/step.go:RenderStepDetail`), and the work packet
header (`internal/engine/packets/default.tmpl`, `attempt: {{.Step.Attempt}}`)
— reads the identical `model.StepRow.Attempt` field. The difference readers
saw during RUN-5 is entirely a matter of **when** each call reads the row
relative to a claim:

- `next --run` lists steps that are ready to be claimed. It necessarily reads
  the row *before* that cycle's own claim exists, so a step about to be
  claimed for the first time reads `attempt: 0`.
- `claim --render` (`internal/cli/step.go`) calls
  `ClaimStepWithGates` — which commits the claim, bumping `attempt` — and only
  *then* calls `engine.RenderStep` to format the packet
  (`internal/cli/step.go`, comment: "the claim has already committed, so this
  is a pure formatting step over what it returned"). So the packet for that
  exact same first claim reads `attempt: 1`.
- `step show`, called against an already-claimed step, likewise reports the
  post-claim value.

Nothing here is a bug in the column itself: at the instant each call reads
it, the value is correct. The defect RUN-5 surfaced is that **no surface
documents the read-timing**, so two consumers drew incompatible conclusions
from the same number:

1. An external escalation policy that ascends a tier on `row.attempt > 1`,
   reading a *pre-claim* `next` row, never fires on a step's first retry —
   because the first retry's pre-claim read is still `1`, not `2`. Under the
   spent-count semantics above, "has failed once and is being retried" is
   `attempt == 1` at claim time for that retry, not `attempt > 1`. Any
   threshold logic against this field must state explicitly whether it is
   reading a pre-claim or post-claim sample, and pick its boundary (`>= 1` vs
   `> 1`) accordingly — this repository does not itself implement that
   policy (it lives in an external dispatcher), so the fix on that side is
   tracked against DKT-64's own history, not this codebase.
2. A packet's `attempt: 1` is normal for a step's *first* claim, not evidence
   the step "was retried" — retried work reads `attempt: 2` or higher in its
   packet.

## What to check before trusting `attempt` anywhere new

1. Is this read happening before or after the claim whose attempt you care
   about? `next`/`step show` pre-claim vs. a packet/`step show` taken after
   a `claim` call are different moments even though the field name is the
   same.
2. `attempt` alone cannot distinguish an infrastructure-retry (e.g. a reaped
   lease with no real work done) from a genuine quality-failure retry — both
   bump the same counter. The separate marker that distinction needs exists
   since schema v23 (DKT-490): `failed_attempts` counts claims ended by an
   explicit `step fail`, `reaped_claims` counts claims reaped without one
   (lease expiry, `max_step_duration`, forced `step reap`), and both ride on
   the same rows `attempt` does (`next`, `dispatch open`, `step show`,
   `step list`; `omitempty`, so they appear once nonzero). A claim that
   recorded counts in neither, and `step resolve --as retry` touches neither.
   `failed_attempts + reaped_claims` never exceeds `attempt`; the remainder
   is live claims, recorded completions, and pre-v23 history (the migration
   back-fills nothing, so zero on pre-v23 claims means "no recorded
   breakdown"). An escalation policy that means "escalate after N *failures*"
   reads `failed_attempts`, and never `attempt`.

## Regression coverage

`internal/engine/render_test.go` (`TestAttemptNumberingPreAndPostClaim`)
pins the pre-claim/post-claim behavior described above: a step's `attempt`
read via the ready-steps path before any claim is `0`; the packet rendered
by the claim that takes it to `1` shows `attempt: 1`; a subsequent read of
the same step (post-claim) also shows `1` — proving it is one field sampled
at two moments, not two counters.

`internal/engine/attempt_breakdown_test.go` pins the outcome breakdown
(DKT-490): an expiry reap counts `reaped_claims` and not `failed_attempts`,
`step fail` counts the reverse in both its branches, a lazy reap at `claim`
counts like the scheduler's, a recorded park and its `resolve --as retry`
touch neither, and `attempt` moves at claims alone throughout.
