# Review: docs/tdd/runs-dispatch.md — concurrency + security lens findings

Reviewer: design-context session · 2026-08-03 · Verdict: **SOUND, ONE FIX
APPLIED IN PLACE** — the strongest TDD of the arc. Group 1 may launch
immediately.

## The fix (applied 2026-08-03, in place)

**F1 (HIGH, fixed) — §5.8 D2 contradicted the TDD's own rehearsal.** As
written, D2 (usage rows missing) fired on any terminal step with zero ledger
rows, and D6 probed it with no dispatch open — but ZK's Z6 and ZH's stranger
demo both complete WITHOUT --usage and open no dispatch, so Z8's `next` would
be refused with a resolution verb (`dispatch close --accept-missing-usage`)
requiring a dispatch that never existed. Fixed by scoping D2 to **runs that
have ever opened a dispatch** (a dispatch-opened event exists): usage
completeness is a RELAY contract — a relay that ever dispatched is accountable
for every step of that run including out-of-manifest spawns (the drift D6
exists to catch), while a run no relay drove has nobody owing usage. This also
makes never-dispatch repos perfectly dormant, strengthening §3's claim. D1
(lease liveness) stays universal and self-resolving. Also applied: B21's
breach reason now names "spend" (the enforced max(reported, floor)) rather
than "floor", which misled whenever budget.unit was set and reported exceeded
the floor.

## Endorsed on the record (do not re-litigate)

- **Floor-as-query** (§4.3): the four-part argument is complete — structural
  race-freedom (no read-modify-write to lose), the engine-owned-facts claim
  made literal, §9.2 attributability, replay. The poisoned-cache test
  (TestFloorIsNeverReadFromCache) is the right proof that the cache is a
  cache. B14's boundary semantics (crossing is >, landing exactly spends) and
  the R7/B14 two-moment duality with TestBudgetR7AndClaimAgree.
- **budget.unit with default ""** (§4.5): summing opaque units would be core
  asserting they add; resting on the floor by default makes §9 item 7 the
  DEFAULT configuration rather than a special path. B24's honest
  "resume re-pauses" clause; B25's DKT-29 filed rather than a verb invented.
- **P28** (claim not refused by an open dispatch): correct, argued, and — a
  stronger point than the TDD makes — it is the DESIGNED flow: executors
  claim precisely while the dispatch is open.
- **The ack mechanism** (§6.2): A1–A6 all satisfied; the alternatives table
  is right, especially rejecting dispatch-close (the crash case) and
  timer-ack (A5 — "confirmed gone" vs "probably gone"). The hold as a
  headroom predicate rather than a flag (A12–A15) is the correct concurrency
  shape — no window, and TestReapHoldIsHeadroom proves the fold.
- **§6.5's finite-max translation** of "write-class": the argument holds — an
  unbounded class is author-declared parallel-safe; a bounded class is the
  instance's serialization declaration; core keying on the name would be
  unimplementable. TestNoWriteClassLiteral.
- **§9.3's refusal** for changed-bytes@unchanged-version, and the T9 argument
  ("zero-touch governs who does the work, not whether the machine may ask")
  — the refusal message IS the conversational loop working. F15
  (re-activation never re-scans) is crucial and correctly derived from RA2.
- **§9.5**: auto-registration reads-and-hashes, executes nothing; ZH10's
  malicious-clone extension so the widened surface is tested hostile.
- **Z9's trace-grep** as item 11's criterion — cannot pass by accident.
- DKT-29/30/31/32 all correctly filed; the DKT-21 precedent correctly
  applied to 31/32.

## Recorded residuals and notes (no action this stage)

- **Zombie-vs-re-claim of the SAME step**: §9 item 4 re-readies a reaped
  step; a still-alive prior executor of that same step can race its
  replacement on the tree. This predates S6 (it is §9.4's semantics since
  S2), the write-headroom hold narrows the blast radius to same-step-only,
  and the reference harness's process-group kills are the instance
  mitigation. Recorded so it is a known fact, not a discovery.
- **Finite-max over-breadth**: an instance capping a class purely for COST
  (not tree safety) inherits reap-ack friction. Acceptable now (the ack is
  one flag, the denial names it); the amendment shape if it ever bites is a
  `[limits] fence = bool` opt-out. One line for the future, not a change.
- **§8.7's "human" bucket** for step-claimed reads as "external caller";
  the reasoning (attributing to next would hide the boundary item 2 exposes)
  is accepted — the naming is noted, not contested.

## Disposition

Group 1 launches now. The three groups proceed per §11; the stage closes with
both §9 item 10 halves checked together (S4's gate half + this stage's reap
half), the v5–v10 span completeness assertion, and DKT-29's resolution
decided by the operator at close (the run budget verb vs abandon-restart).
