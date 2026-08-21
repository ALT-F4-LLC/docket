# Stage review: M1.S6 range engine-s5..c3e4a23 — verdict

Reviewer: design-context session · 2026-08-05 · CI green; qa 1623/1623;
sweep 36/36 vs engine-s5.

## Verdict: ACCEPT

**§9 item 10 is COMPLETE across two stages, checked together as required:**
the gate half (S4: crash-at-every-boundary never double-runs a
non-re-runnable gate; parks waiting-human) and the reap half (S6: a reaped
finite-max-class step gains no successor until an explicit, attributable,
corpse-independent acknowledgment). Items 2, 7, 9, and 11 are QA facts —
notably 11's trace-grep criterion (zero register invocations over an
asserted-non-empty trace) and 2's per-actor counts summing to the run's
event total. The v5–v10 schema span is complete with its own assertion, and
the tracker survived its FOURTH live upgrade rescue — this one teaching a
new lesson (sentinels must probe indexes, not only tables; a DDL comment is
not a mechanism), fixed with an index sentinel and a regression test built
from the found state.

## In-flight deviations — all endorsed, all reconciled in the TDD in place

- DKT-30 (reap precedes the discrepancy probe): default grace equals the
  default lease TTL, so the written order was a zero-config wedge. Reviewer
  miss, session catch.
- DKT-31 (the hold exempts the reaped step): A15's literal fold contradicted
  W3 and §9 item 4 at max=1. Same class.
- DKT-33 (GONE cannot take exit 6): an error-code collision with S1's
  STALE_LEASE, caught before shipping; GONE appends at 9. Spec §5 taxonomy
  amended in both copies.
- DKT-34 (event step_id): §11.4 amended in both copies.
- DKT-32 (events list naming): §1 surface amended in both copies.

## Open item, filed rather than adjusted

ZG15_losers_refused: pre-existing claim-contention flake (~1 in 3 under
parallel load; five losers exhaust busy_timeout instead of receiving
CONFLICT). Group 3's refusal to loosen a race assertion it didn't write is
correct — a real behavior question (should a busy-timeout loser surface as
CONFLICT?) deserves its own issue, not a quieter test.

## Dispositions

Tag engine-s6; close DKT-6, DKT-13 (stale since S1 — edits applied
2026-08-03), DKT-30, DKT-31, DKT-32, DKT-33, DKT-34; file the ZG15 issue;
DKT-29 (run budget verb) decided by the operator at this close; 09 ticked.
S7 remains — one small session — and M1 is complete.
