# Stage review: M1.S4 range engine-s3..HEAD — security-led conformance verdict

Reviewer: design-context session · 2026-08-03 · Scope: security lens leading;
targeted reads of every wired security surface; CI carries the test evidence
(all checks green, qa 1271/1271, sweep 36/36, genericity 5/5).

## Verdict: ACCEPT

Zero blocking findings. One TDD self-contradiction (DKT-21) reconciled at
close-out; two spec amendments (DKT-19/DKT-20) applied to both copies.

## Surfaces examined, wired form

**The real runner (gate_exec.go).** M1's snapshot-once with matched-argv-is-
what-executes, exactly as specified; fail-closed on infrastructure errors (an
unreadable trust store or unresolvable repo path is `unmatched`, never a skip);
fence-row hash re-verified before spawn; N3's unmatched-is-a-failure in the
verdict path; §5.2.1 containment active across gate classes.

**Claim restructure (claim.go).** The three-phase structure with §7.6.1.1's
LR1 lease recompute in transaction B, plus a fast path this review endorses:
a step with no pre-gates takes the identical single-transaction path — the
restructure is paid for only where it buys something, and S3-shaped claims
stay byte-compatible.

**Resume decision (saga_resume.go).** A1–A6 exact; the park is recorded as a
gate RESULT as well as a step status; both operator remedies named in the
reason. Two cases beyond the TDD's enumeration, both correct: a non-real
runner (pass-through/test fake) is synthetically re-runnable — nothing
spawned, nothing to double-run — and a FENCE gate is treated as never
re-runnable, because re-running a partially-executed multi-line block would
re-run lines that already succeeded, which is precisely T12. The second is a
genuine specification improvement discovered in implementation; it is now
recorded here as reviewed-and-endorsed.

**Tree mutex (treelock.go).** flock + in-process sync.Map mutex (L5's
dual-lock), O_NOFOLLOW open with §3.2's language (L7), bounded poll against
the gate's timeout implementing L4's block-with-bound.

**Vote wiring (vote.go).** Wiring-only as required — CreateProposalIdempotent
keyed by (run, instance), tally READ not recomputed, routing per effective
on_fail.

**Escaping.** %q rendering at the trust CLI surfaces; ZH asserts the
disclosure lines.

**ZH (the stranger demo).** Unmatched-before-add asserted, argv+repo
disclosure asserted, done on exactly one trust add, banned-list sourced from
genericity.sh, negative clone half present. §9 items 1 and 6 are QA facts now.

**v8 migration.** G2 stub-stamping, G4 idempotence via UNIQUE + INSERT OR
IGNORE, G5 malformed-trail recording — all present; U1/U3 ran against this
repo's own tracker.

## Dispositions (applied at close-out)

- **DKT-19 / DKT-20**: §11.4 gains `pre_gates?` (context) and `reason?` (gate
  result) in both spec copies — applied 2026-08-03. Close both.
- **DKT-21**: gates-trust.md §6.4 reconciled — "exactly four" → six, naming
  vote-opened/vote-tallied per §8.1. The session's behavior-over-bookkeeping
  call was correct; the doc now agrees with itself. Close.
- Tag `engine-s4` (local); close DKT-4; DKT-16 already closed by the
  migration commit. 09 unit table ticked.

## Noted for S5

T3's parked ordered comparisons come alive at S5: the threshold evaluator's
schema-aware upgrade must preserve T3 for still-schema-less fields, and the
aggregate action replaces the stub runner — the stub:true discipline retires
for actions the same way it did for gates, with the migration-marking pattern
available if any stub action results exist in real DBs.
