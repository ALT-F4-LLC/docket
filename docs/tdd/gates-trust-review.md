# Review: docs/tdd/gates-trust.md — security-lens findings

Reviewer: design-context session · 2026-08-03 · Verdict: **SOUND WITH FIXES** —
no architectural rework. One real threat-model gap (F1), one spoofing surface
(F2), three refinements. All five are additive clauses + tests; fold them into
the TDD before group 1 launches. The three author-flagged arguments were pushed
on hardest and all three hold (judgments below).

## Findings

**F1 (HIGH-MEDIUM) — Repo-resident PATH resolution is an open hole T15 doesn't
cover.** T15 closes `./make` and PATH-contains-dot, but not a PATH entry that
points INSIDE the repo — which is exactly what an allowed `.envrc` (this repo
ships one; direnv is in live use on this machine) or any checked-in bin/ dir on
PATH produces. Sequence: operator direnv-allows a repo's `.envrc` for legitimate
dev tooling; it prepends `$PWD/bin`; a trusted argv `["make","test"]` now
resolves argv[0] to a repo-controlled binary. The trust entry names the command;
the repo supplies the executable. Fix, cheap and structural: after `LookPath`
resolution, if the resolved binary's symlink-resolved path lies under the repo
root, REFUSE (verdict `unmatched`, reason naming the resolved path and the rule)
unless the trust entry's argv[0] is itself an absolute path under the repo —
i.e. running repo-owned scripts stays possible, but only by trusting the
absolute path explicitly, never via name resolution. Add the direnv vector to
T15's row and the test (`TestArgv0NeverResolvesIntoTheRepoByName`).

**F2 (MEDIUM) — Terminal-escape injection in every verbatim print.** §7.7
prints harvested fence commands "verbatim"; `trust add` prints argv; `reason`
strings carry fence content. All of that is attacker-influenced bytes written
to the operator's TTY — an embedded `\x1b[2K\r…` sequence can visually rewrite
the very line the operator is approving, and the D14 backstop depends entirely
on displayed == approved. Fix: every rendering of argv, fence commands, or
reasons in human mode escapes non-printables (Go `%q` semantics or C0/C1
stripping — pick one, state it); JSON mode is inherently safe. One test: a
fence command containing escape sequences renders escaped in the §7.7 report
and in `trust add`'s disclosure.

**F3 (LOW-MEDIUM) — Pre-gate wall time silently consumes the worker's lease.**
§7.6.1's phase 2 can run minutes of subprocess (default timeout 5m per gate)
between the CAS claim (which sets `expires_ms`) and the response the caller
receives. A pre-gate-heavy step can hand its worker a mostly-spent lease. Fix:
transaction B refreshes `expires_ms` (the claimant still holds the CAS; this is
a heartbeat by another name). One clause, one test (short TTL + slow fake
pre-gate ⇒ caller still receives a full lease).

**F4 (LOW) — `.docket/tree.lock` can be repo-shipped hostile content.** A
hostile repo can commit `tree.lock` as a symlink; L1's open would follow it.
Blast radius is small (an flock on an arbitrary file — DoS-shaped), but the fix
is the discipline §3.2 already established: open with O_NOFOLLOW / refuse a
non-regular existing file, same I1 language.

**F5 (LOW) — Concurrent `trust add` is a lost-update race.** I5's
temp-file-plus-rename makes each write atomic but two concurrent
read-modify-writes drop one entry silently. Flock the store file (or a
sibling .lock) around read-modify-write in `trust add|rm`. One clause.

## The three author-flagged arguments — judged

- **§7.6.1 claim restructure: HOLDS.** The property split is argued correctly —
  single-winner exclusion lives in transaction A and is untouched; token+context
  in one response is a caller-observable property and is preserved; the
  crash-between-phases window is the pre-existing crash-before-response class,
  handled by lease expiry. F3 is the one refinement. A7's filing (a TDD-promise
  qualification, not a spec deviation) is the correct classification.
- **GD1–GD3 pre-gate determinism: VALID, adopt as argued.** A pre-gate exists to
  observe the working tree; requiring its output be immune to working-tree
  changes requires it to observe nothing. §9 item 5's immunity survives for
  everything it was written about (snapshot, inputs, pins, topology). The
  golden-elision + structural assertion is the right mechanization.
- **§7.5 flaky × re_runnable: CORRECT and necessary.** Row 2 (flaky but not
  re-runnable parks on crash-resume) is exactly right — non-deterministic exit
  is not crash-idempotence, and the four-way table prevents the conflation.

## Verified clean (do not re-litigate)

- Threat model T1–T16: mechanisms and proofs check out; the five
  author-discovered entries (path hijack, fence-row tamper, pre-gate bypass,
  prefix over-auth, self-trust audit) are real and correctly closed; §2.1's
  three residuals are honestly argued and consistent with the ratified D14.
- §3.4 repo identity: the path rule is correct, the candidate rejections are
  the right argument (root-commit called out as worse for looking
  cryptographic — good), P4's fail-closed malformed-entry rule and the
  moved-repo cost with its diagnostic are exactly right.
- §3.3 canonical-argv JSON hashing (the `["a b"]` vs `["a","b"]` collision
  test), element-wise-never-string prefix matching, M1–M6 including
  match-returns-the-argv (T4's structural closure).
- §4 v8: gate_results shape per §11.4 + DKT-15 pattern; G1–G5 migration incl.
  stub=1 stamping and idempotence; U1–U7 with U3 encoding the v7 lesson;
  trust_cache pinned as audit-never-authorization.
- §5: Spec type making shell unrepresentable; K1/K2 tokenization boundaries;
  the env allowlist with set-equality testing and the DOCKET_TOKEN
  belt-and-braces; X1–X5 group kill incl. the pipe-hang clause; C1–C5 capture.
- §6.2 unmatched-is-a-failure (N3) and §6.3's reason argument (DKT-20 —
  correctly filed); §6.4's four new event kinds keeping the closed set closed.
- §7.3 fence chain with the hash re-verify addition; §7.4 flock choice with
  the L5 dual-lock subtlety; §7.5 A1–A6; §7.6.2 PG1–PG4; §7.7 incl. --dry-run.
- §8 vote wiring: lifecycle lazy/idempotent, tally-by-identity test, V26 at
  register, config-key rule storage — all reuse, no reimplementation.
- §9: the stranger script is the §9.1 AC made executable, with the
  comprehensibility half mechanized against genericity.sh's own list and the
  §9.6 negative folded in; SB1–SB5 (SB4's poisoned-store guard especially).
- §10 non-goals each carry a future-amendment shape; §12's two-group argument
  is right (the security lens reviews group 1 without engine context).

## Disposition

Fold F1–F5 into the TDD (short revision session — they are five crisply
scoped additive clauses + tests; no restructuring). F1 and F2 also land as
threat-model rows so security.md §13.1 inherits them. Then group 1 launches.

---

## Review response — 2026-08-03

All five findings applied to docs/tdd/gates-trust.md as specified. No
architectural change, no restructuring, no re-litigation of the verified-clean
list. One row per finding.

| # | Verdict | What changed in the TDD | Sections |
|---|---|---|---|
| **F1** | applied | New threat row **T17** naming the repo-resident `PATH` vector explicitly, including the `.envrc`/direnv sequence. New **§5.2.1**, the post-resolution containment rule as R1–R5: `EvalSymlinks` both the resolved binary and the repo root (R1/R2, the same function on both sides); a resolved `argv[0]` at or under the repo root is refused `unmatched` with a reason naming the resolved path and the rule (R3, path-component-wise containment, **not** string-prefix — `/src/docket-evil` must not count as under `/src/docket`); the sole exception is an entry whose own `argv[0]` is that absolute path (R4), so repo-owned scripts stay trustable by path; the check is unconditional across named, fence, and pre-gates (R5). Records why refusal beats sanitizing `PATH`, and what it deliberately does not close (a repo-supplied *library* is §2.1 residual 1, untouched). Test **`TestArgv0NeverResolvesIntoTheRepoByName`** as a six-row table: by-name refused, absolute-path permitted, the symlink-indirection variant, a symlinked repo root, the `docket-evil` sibling that must **run**, and an ordinary `/usr/bin/make`. | T17; §5.2.1 R1–R5; §9.1; §12 group 1 |
| **F2** | applied | New threat row **T18**. New **§5.7** as E1–E5. **Mechanism chosen and stated: Go `%q` / `strconv.Quote`, not C0/C1 stripping** — the reason is recorded (E1): `%q` is lossless and reversible, so the operator sees that hostile bytes are present, whereas stripping renders the attack line as plausible-but-wrong text. One renderer (E2) used by every human-mode print of the three attacker-influenced value classes — argv, fence command, `reason` (E3). **JSON mode is inherently safe and is left alone** (E4): `encoding/json` escapes controls by contract and the consumer is a program, so double-escaping would corrupt it. Stored bytes are never modified (E5), since §7.3's hash re-verification depends on it. §7.7 S1's "verbatim" is reconciled with escaping rather than left contradictory. Tests: **`TestOperatorFacingRenderingEscapesControlBytes`** (§9.1, asserting on written bytes plus the JSON round-trip and post-render hash) and **`TestActivationReportEscapesFenceControlBytes`** (§9.2, end to end from an issue body). | T18; §5.7 E1–E5; §7.7 S1/S2; §9.1, §9.2; §12 group 1 |
| **F3** | applied | New **§7.6.1.1**: transaction B recomputes `expires_ms` from its own commit time using the same TTL resolution transaction A uses (LR1), guarded by the claimant's still-held claim identity so it is a heartbeat and never a second authorization decision — a lost claim matches zero rows and yields `CONFLICT` (LR2). No new event (LR3, the goldens are unchanged). Single-winner exclusion stays entirely in transaction A (LR4). Phase-3 row updated to name the refresh. Test **`TestPreGateWallTimeDoesNotConsumeTheLease`** with a TTL deliberately shorter than the pre-gate, asserting a full lease measured from the **response**, plus LR2's negative case. | §7.6.1 phase table; §7.6.1.1 LR1–LR4; §9.2; §12 group 2 |
| **F4** | applied | New **§7.4 L7**: `.docket/tree.lock` opened `O_RDWR\|O_CREAT\|O_NOFOLLOW` mode `0600`; an existing non-regular file (symlink, FIFO, directory) is a `VALIDATION_ERROR` naming the path and what it found, **in §3.2 I1's identical language and disposition**, as the finding asked. The `tree = true` gate records `verdict='fail'` with that reason and does not spawn. The small blast radius is stated rather than hidden, with the reason for closing it anyway (one `Lstat`, one flag, and consistency with the discipline §3.2 already set). Test **`TestTreeLockRefusesNonRegularFile`**. | §7.4 L7; §9.2; §12 group 2 |
| **F5** | applied | New **§3.5.1** as W1–W5: an exclusive `flock(2)` spans the whole read-modify-write of `trust add\|rm` (W1). **The lock is on a sibling `trust.toml.lock`, not on the store file** (W2) — with the reason, which the finding left open: rename-based writes replace the inode, so a lock on `trust.toml` itself excludes nothing between two writers. The lockfile carries §3.2's integrity discipline (W3). **Reads stay lock-free** (W4) — I5's rename already publishes a complete version, and locking the read path would let a hung `trust add` block gate execution. Blocking acquisition with a 5s bound ⇒ `CONFLICT` (W5). Test **`TestConcurrentTrustAddsBothLand`** with an N-**subprocess** case, which is the one an in-process-mutex implementation fails. | §3.5.1 W1–W5; §9.1; §12 group 1 |

**Genericity re-check** (CLAUDE.md PR bar): re-run over everything added, and
recorded in the TDD as **§1.1.1** rather than done silently. The revision's new
nouns — `resolved path`, `repo root`, `containment`, `symlink`, `lockfile`,
`lock`, `escape`, `control character`, `render`, `terminal`, `lease refresh`,
`expires_ms`, `regular file` — are filesystem, process, terminal, and lease
vocabulary, the same families §1.1 already covers. No new family, no
agent/LLM concept, and no reader is required to know what a model is.
`lease refresh`/`expires_ms` are pre-existing v6 surface; the revision changes
*when an existing field is written*, not the surface. **No new flag, config key,
column, event kind, or `trust.toml` key**, so §6.4's closed set, the §9-item-2
check, and the surface guards are all unaffected; the new error strings reuse
§3.2's `VALIDATION_ERROR` language.

**Not re-litigated**, per the review's own instruction: the verified-clean list
and the three author-flagged arguments (§7.6.1's claim restructure, GD1–GD3's
pre-gate determinism, §7.5's flaky × re_runnable table) stand as judged. F3 is
folded in as the one refinement the restructure was found to need.

**Amendments**: none added. F1–F5 are additive clauses and tests within surfaces
this TDD already files (A5/DKT-19, A6/DKT-20, A7 on DKT-3); none of them deviates
from an engine-spec line, so §11's table is unchanged.

Next: group 1 launches. Implementation is a separate session — this revision
ships documentation only.
