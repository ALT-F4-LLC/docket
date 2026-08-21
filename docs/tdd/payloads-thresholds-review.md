# Review: docs/tdd/payloads-thresholds.md — findings

Reviewer: design-context session · 2026-08-03 · Verdict: **SOUND WITH ONE
DECISION** — the strongest TDD of the arc. One genuine internal contradiction
(F1) needs an operator decision before group 2 (group 1 is unaffected and may
launch immediately); two low refinements ride along.

## Findings

**F1 (MEDIUM) — H11 and H12 pull in opposite directions, and the test as
written forces a mid-implementation discovery.** H11 exempts the routing step
(`saga_stage='held'`, status `gated`) from `guard stop`'s pending-work set,
arguing a harness must be able to stop while a decision is open. But the
materialized `<step>-held@k` that same transaction creates sits in status
`pending` (H4), and §6.12 denies stop on ANY pending step — so with H11 as
written, `guard stop` still denies while held, the exemption achieves nothing,
and `TestGuardStopAllowsWhileHeld` fails until the implementer also exempts
the materialized step. Exempting it, though, contradicts H12's own
consistency argument: H12 justifies the held design by matching "the existing
treatment of a declared type=human gate awaiting approval" — and a declared
human gate awaiting approval (the fixture's commit-gate) DOES block stop
today. Two coherent resolutions:

- **(a) Drop H11** — held blocks stop exactly as a declared human gate does.
  Zero new rules, H12's consistency preserved, and matches the existing
  conversational semantics: the harness surfaces the open decision rather
  than stopping around it; stopping requires resolving or abandoning.
  RECOMMENDED.
- **(b) Exempt ALL type=human pending steps** (declared and materialized)
  from guard stop — a deliberate change to S3's §6.12 semantics needing its
  own argument and an update to S3-era expectations.

Either is implementable; what is not acceptable is the current text, which
specifies (in H11) a behavior its own data model (H4) prevents.

**F2 (LOW) — Auto-registration ownership needs an explicit hand-off.** The
scope table assigns activation auto-registration to S6 via §9.11, and §4.6
pins the ordering contract with a pending test — all correct. But 09's S6
packet text never names auto-registration as work; it arrives at S6 only if
the S6 kickoff says so. Action: one line in the S6 kickoff (the reviewer's to
write); recorded here so it cannot fall between stages.

**F3 (LOW) — V30's probe should exercise the real conjunction.** The
satisfiability probe is built "from the schema's own declared enum values";
to catch `additionalProperties: false` it must also carry the aggregate
output keys (`members`, `held`, and at least one carried-through G3 key).
Almost certainly the intent — make the clause say so.

## Endorsed on the record (do not re-litigate)

O1's single-list rule; O4's validator-swappability (ordering is ours,
validation is theirs); I3's Position-only API — T3 enforced by type rather
than memory; I4's no-fallback rule; §4.1's dependency treatment (the golden
corpus as a renovate gate, the pinned draft, no-cgo-by-test, the void-choice
clause); §4.6(a) with the ordering contract and pending test; §4.9.3's
re-validation stance with the honest re-register consequence and its test
pair; V30's satisfiability probe (with F3); G2's identity property — the
behavior cliff an adopter never hits; §7.3's four-part tie-rule argument —
direction-agnosticism as the genericity rule applied to statistics; H10's
no-un-route argument; H13/H14 (immutability; the V13 non-violation reasoning
is subtle and correct); H17–H20 answering the loop-interplay probes before
they were asked; §6.2's stdin/stdout contract incl. escaping unparseable
output; §6.3's history-not-rewritten stub retirement; E4's embedded-bytes
golden; §3's "one seeded builtin row" honesty; A1–A6 all correctly scoped —
DKT-23 is a genuine spec-gap catch.

## Disposition

Group 1 launches now — F1 touches only group 2's H-clauses. The F1 decision
(operator's) plus F3's clause land as a small TDD edit before group 2; the
A1–A5 spec edits apply to both copies at the same moment so implementation
reads a spec that agrees with its TDD; DKT-27's fixture edit lands in group 2
per §8.2 with the upstream 05 mirror applied by the operator's reviewer.
