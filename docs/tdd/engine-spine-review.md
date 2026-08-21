# Review: docs/tdd/engine-spine.md — conformance findings

Reviewer: design-context session (full upstream design + quorum history) ·
2026-08-03 · Verdict: **SOUND WITH FIXES** — no architectural rework; three HIGH
findings must be fixed in the TDD (and one in the fixture) before the group-1
implementation session launches. Conformance-not-redesign framing throughout;
no ratified decision is questioned.

## HIGH

**F1 — Action-step execution and S3 threshold evaluation are unspecified, and
phase 4's QA depends on both.** (TDD scope table L43; §6.8; §7.7 QA "full loop
run") The gate seam got §5.6; action steps got nothing — yet the fixture's
`reconcile` is an action step sitting on the QA loop's critical path, and the
`aggregate` computation is S5's. Worse, `reconcile`'s threshold
`any(severity >= high)` uses an ordered comparison, which engine-spec §11.2
defines ONLY for `ordered_enum` schema fields — and schemas register at S5. The
scope table's pointer to "§7.3" for the degradation dangles: §7.3 is now the
supersede sweep. Required: (a) an `ActionRunner` seam mirroring `GateRunner` —
the S3 stub emits an artifact of kind `params.output`, empty payload, marked
`stub:true`, never touching the process table; (b) predicate evaluation pinned
exactly: `any()` over an empty payload set = false, `all()` = true, `==`/`!=`
evaluate at S3, and an *actually attempted* ordered comparison on a schema-less
field routes `waiting-human` with a recorded reason (never guesses an order);
(c) the phase-4 QA loop rewritten to drive `fix-loop` through `verify`'s
**equality** threshold (`any(status == unmet)`) with harness-crafted payloads —
with (a)+(b), the reconcile stub's empty payload makes its `>=` threshold
vacuously no-match, so the run flows and the loop is driven legally. Without
this, the group-4 session discovers mid-implementation that its specified QA
test cannot execute, and improvises exactly the semantics this TDD exists to
pre-decide.

**F2 — `run_issues` is missing the snapshot columns §6.6 asserts.** (§5.1 DDL
vs §6.6; §8.3) §6.6: title/kind/labels are "also snapshotted at activation into
run_issues" — but the §5.1 DDL carries only `body_snapshot`/`body_sha256`, and
§11.4's `context.issue` needs `{id, title, body_snapshot, kind, labels, scope}`.
As written, assembly must read live issue fields, and §8.3's own mid-run
edit-immunity test (edit the title, add a label) fails. Fix the DDL: either
discrete columns (`title`, `kind`, `labels`, `scope_globs` snapshot) or one
`issue_snapshot` JSON column, captured in activation stage 4. Include scope:
the bundle's `scope` must be the activation-time value even though R4's
scheduling check may legitimately read live.

**F3 — V13, the `on_fail` default, and the fixture contradict each other.**
(§4.3 V13; §11.1 default `waiting-human`; fixture `commit-gate`) `commit-gate`
declares no `on_fail`, so its effective reject routing is the default —
`waiting-human` — which V13 forbids on `type=human` steps. Either V13 checks
the effective value and `TestFixtureRegistersClean` fails, or V13 checks only
explicit values and the deadlock the rule exists to prevent returns through the
default. Fix in three places: V13 evaluates the **effective** routing, which
makes an explicit `on_fail` mandatory on `type=human` steps; the fixture's
`commit-gate` gains `on_fail = "fix-loop"`; and an amendment issue proposes the
§11.1 `on_fail` row note ("type=human steps must declare it explicitly;
`waiting-human` is invalid there"). Note: upstream 05 §1's example has the same
latent bug — same one-line edit there. While here, pin what nobody states:
`approve` ⇒ step `done`; `reject` ⇒ routes per the step's `on_fail`.

## MEDIUM

**F4 — The v7 rewind guard probes only `workflows`.** (§2) v7's stamp moves at
phase 1 while the migration function grows through phase 4. A dogfood DB —
including THIS repo's own DKT tracker — migrated by a phase-1 build is stamped
7 and never gains the later slices: the guard ("stamped ≥ 7 but `workflows`
absent") sees `workflows` present and does nothing. The DDL is IF-NOT-EXISTS
and re-runnable, so the fix is cheap: probe the full v7 sentinel set
(`workflows`, `runs`, `steps`, `events`) and re-run the migration if any is
absent.

**F5 — Human/vote step lifecycle is unspecified.** `type=human` steps never
claim and never run the saga — their path through the §6.2 machine
(`pending → ready → ?` on approve/reject) is nowhere stated; §6.10 gives the
verbs only. And `type=vote` steps validate at register (V14) but no stage is
assigned their execution — the scope table defers gates, schemas, budgets, but
not votes. Pin both: the human-step transition rule (ready → `done` on approve;
reject consumes per `on_fail` routing), and an explicit scope-table row
deferring vote-step execution (recommend S4, alongside gates, since votes are
"driven as gates").

**F6 — Two determinism pins missing.** (a) `issue.diff`: §6.7 says computed
"at the producing step's completion" but never defines WHICH step produces it.
Pin a rule — e.g. recomputed at completion of every executor step of the issue,
consumers resolving latest-done per the §7.4 fallback — any deterministic rule,
but stated. (b) `step render --template F` / pinned templates: rendering reads
the template file at render time; nothing verifies its bytes against the
activation pin. A post-activation template edit silently changes packets while
`context` stays byte-identical. Require: when the template path is pinned,
render verifies content hash and refuses on mismatch (taxonomy: CONFLICT).

## LOW

**F7 — "Nine statuses" enumerates ten.** (§6.2) Count or list, one is wrong —
fix the prose before someone sizes an enum from it.

**F8 — Stale internal refs after renumbering.** The scope table cites "§7.3"
for threshold degradation; §7.3 is the supersede sweep (the degradation section
does not exist — that's F1). Sweep all §-refs once F1's new sections land.

## Verified clean (no action; recorded so the fix session doesn't re-litigate)

- Committed fixture byte-identical to the delivered original; registers-clean
  claim consistent with V1–V25 **except** F3.
- V-table covers every §11.1 constraint I can derive; V13/V5/V8 correctly
  called out; exactly-one-match correctly deferred to activation; §4.3.1's
  produced-kind table is the right resolution of the emits/action corner.
- L1–L4 sound; L2's two exemptions (threshold-interposed, loop=true) are
  exactly right and fixture-proven.
- Saga table conforms to engine-spec §2 verbatim: token retirement at stage 1,
  per-stage transactions, CAS on saga_stage, gate-started before spawn,
  no-subprocess-in-transaction structurally enforced, routing as one
  transaction across step/issue/run/events. R9 is the right proof.
- DKT-14 closure (§6.4.1) sound; DKT-15/16 filed with correct scope; the
  amendment-trail-before-code discipline upheld.
- Serialized-writers-as-instance-config (§6.5) is the correct genericity call,
  matching engine-spec §2 against a hardcoded reading of engine-core §5.
- Planner reuse claims verified against code: `TopoSort`/`CycleError` and
  `splitByFileCollision` are as described; `next` mode-switch strategy
  (move-verbatim + untouched QA section X) is the strongest available proof.
- genericity.sh with the fixture exclusion and the §11.2 comment is right;
  review-strategy.md as a standing gate fulfills the work order.
- Reviewer note: an earlier draft of this review suspected a surviving
  `GetBool("json")` in next.go — that was a stale staged snapshot on the
  review side; the device file has `jsonModeOf` + the guard test. S1's claims
  stand. No finding.

## Disposition protocol

Fix F1–F3 (TDD + fixture) and F4–F6 (TDD) in a short revision session before
group 1 launches; F3 also files one amendment issue (§11.1 on_fail note) and
implies the same fixture-style edit upstream (05 §1). F7/F8 ride along. Then
group 1 proceeds against the revised TDD with no open questions.

## Review response — 2026-08-03

Revision session, same commit as this file. Verdict accepted as SOUND WITH FIXES;
the "verified clean" set was not re-litigated. One row per finding.

| # | Sev | What changed | Where |
|---|---|---|---|
| **F1** | HIGH | **(a)** New **§6.13 "The action seam (S5 boundary), specified"** — an `ActionRunner` interface mirroring `GateRunner` element-for-element in §5.6's table style (what is real at S3 / what S5 adds). The S3 `stubRunner` emits an artifact of kind `params.output`, **empty payload**, `stub: true`, and never touches the process table; `params.output` missing on an `action` step is a register-time error. **(b)** New **§6.14 "Threshold predicate evaluation at S3, exactly"** — four clauses: T1 `==`/`!=` evaluate at S3 (with JSON scalar normalization and the `null` rule); T2 ordered ops do not; T3 an *actually attempted* ordered comparison on a schema-less field routes `waiting-human` with a recorded reason naming step, routing key, predicate, and cause, and the engine **never guesses an order**; T4 `any()` over empty = **false**, `all()` = **true**, `count>=n` iff `n == 0`, evaluated **before** T3. **(c)** §7.7's QA loop rewritten: the loop is driven at **`verify`'s equality threshold** (`any(status == unmet)`) with harness-crafted payloads, 11 numbered steps; a table states which of the fixture's two `fix-loop` thresholds can legally fire at S3 and why `reconcile`'s cannot (T4 short-circuits its `any` to false over the stub's empty payload ⇒ no match ⇒ `pass`, per §11.2). A separate small case covers T3, which the fixture cannot reach. **(d)** The scope table's dangling "§7.3" now points at §6.13/§6.14. | §1 scope table; §6.13; §6.14; §7.7; test plans §6.16 (`action_test.go`, `threshold_test.go`) |
| **F2** | HIGH | §5.1's `run_issues` DDL gains **`issue_snapshot TEXT`** — one canonical-JSON column holding `{title, kind, labels, scope}` — argued in new **§5.1.1** (these fields are never queried or joined, only read back whole to materialize `context.issue`, so a blob costs nothing and avoids a migration per §11.4 shape change). Activation **stage 4** renamed and extended to capture it. §6.6's source list goes from four sources to five and now states assembly reads **no** live issue field at all, with the scheduler's live `scope_globs` read (R4) explicitly excluded and both behaviors justified. §8.3's test description adds `--scope` to the mutated set and requires per-field assertions. | §5.1 DDL; §5.1.1; §5.3 stage 4; §6.6; §8.3; §5.7 |
| **F3** | HIGH | New **§4.3.2** argues the effective-routing reading in a two-row table and takes it. **V13** now evaluates the effective `on_fail`; **V13a** added as its own row for the mandatory-explicit corollary, so the error message is specific. `TestValidationTableIsComplete` respecified to assert **set equality over documented rule IDs** — 26 rules across 25 numbered IDs — never a count, which is exactly the assertion a rule split breaks. §4.5's row updated. §4.3.2 also pins `approve` ⇒ `done` and `reject` ⇒ routes per effective `on_fail`, in a table. **Fixture edited**: `commit-gate` gains `on_fail = "fix-loop"` with a comment citing §11.1 and §2. **Amendment issue filed: DKT-17**, recorded as **A4** in §10; the §11.1 spec edit was applied by the operator (engine-spec §11.1 + upstream 06 §11.1, 05 §1) and rides in this commit. | §4.3 V13/V13a; §4.3.2; §4.5; §4.6; §10 A4; `docs/design/example-workflow.toml`; `docs/design/engine-spec.md` §11.1 |
| **F4** | MED | §2's rewind guard now probes the **full v7 sentinel set** (`workflows`, `runs`, `steps`, `events`) and re-runs the migration if **any** is absent, with the phase-1-stamp/phase-4-DDL trap spelled out (including this repo's own dogfood DKT tracker). The sentinel list is a constant beside the migration; `TestRewindGuardProbesEverySentinel` asserts one entry per table the v7 DDL creates, so a phase adding a table without extending the set fails its own test. | §2; §5.7 (`migrate_v7_test.go`) |
| **F5** | MED | New **§6.15 "Human and vote steps: the lifecycle, pinned"** — a table giving each class's path through the §6.2 machine (human: `pending → ready → done` on approve, or routed per effective `on_fail` on reject, with R4/R5 noted as inapplicable; vote: expands, becomes ready, **parks**). Neither class is offered as executor work: `claim` against one is `CONFLICT` naming the step class, asserted as a test. **Vote execution deferred to S4 as an explicit §1 scope-table row**, on §2's "votes are driven as gates" reasoning. | §1 scope table; §6.15; §6.16 (`human_test.go`) |
| **F6** | MED | **(a)** New **§6.7.1** pins the `issue.diff` producing-step rule as D1–D4: recomputed at the routing stage of **every executor step** of the issue (git subprocess outside the transaction), not action/human/vote steps; consumers resolve highest-ordinal-then-highest-id `done` per §7.4's existing fallback; an absent diff resolves to an **empty** artifact recorded at activation, never a live `git diff`. The fixture's `fix@1 → review@1` case is worked through. **(b)** New **§6.11.1**: when a template path is pinned, `render` hashes the file it read and refuses on mismatch — **`CONFLICT`** (exit 4) naming both hashes, in a five-case table (pinned-match, pinned-differ, pinned-absent ⇒ `NOT_FOUND`, unpinned ⇒ proceeds with `--meta` disclosing it, embedded default ⇒ cannot drift). `TestPinnedTemplateDriftIsRefused` is the sensitivity check. | §6.7 table; §6.7.1; §6.11.1; §6.16 (`context_test.go`, `render_test.go`) |
| **F7** | LOW | §6.2 now reads **"Ten statuses"** for the machine and states that `ready` is computed, so the **persisted enum has nine** — both numbers given, because sizing one from the other is the mistake. §7.7's supersede table row updated to "one row per persisted status (nine)" plus a row for a step *computed* ready at sweep time. | §6.2; §7.7 |
| **F8** | LOW | Full §-ref sweep after F1's sections landed, scripted (headings extracted, refs diffed). Renumbering: old §4.3.2 (DAG lints) → **§4.3.3**; old §6.13 (phase-3 test plan) → **§6.16**. Stale/ambiguous refs fixed: scope table §7.3 → §6.13/§6.14; V25 §7.3 → §6.14; saga stage 0 §7.3 → §6.14; §9-item-2 partial §6.13 → §6.16. Bare refs that collide with this TDD's own numbering disambiguated to `engine-spec §7` (VCS coupling, ×2), `engine-spec §9 item 8` (fixture protocol), and `scripts/qa/genericity.sh, §9`. Every remaining unmatched ref verified as an intentional external ref (engine-core §1.2/§1.3/§3.x, engine-spec §4/§5/§6/§9 items/§11.x, reliability-delta §2.1). | throughout |

**Gates re-run on the revision** (work order item 6):

- **Fixture vs. the revised V-table, by hand**: parsed with the repo's own
  `BurntSushi/toml` v1.6.0 to confirm it still decodes, then checked rule by rule.
  V13/V13a: `commit-gate` is the only `type="human"` step and now declares
  `on_fail = "fix-loop"` — explicit, and not `waiting-human`. V12: `"fix-loop"` is
  in the closed set; `implement`'s `on_fail = "waiting-human"` is an executor step
  and unaffected. V5: `commit-gate` still declares `type` alone. V11 + §4.3.1:
  `fix.inputs` still resolves `reconcile.findings` through `params.output`, and no
  `inputs` entry targets a human/vote step. V21: both threshold predicates still
  parse as `agg(field op literal)` — their *evaluation* is §6.14's concern, not
  register's. V8/V18/L1–L4: topology untouched by a one-key edit. **Registers clean.**
- **Genericity (§1.1)**: every backticked identifier the revision adds enumerated and
  checked. The new nouns — `action`, `stub`/`stubRunner`, `payload`, `predicate`,
  `issue_snapshot`, `sentinel`, `approve`/`reject`, `human`/`vote` — are all
  scheduling, packaging, or gate vocabulary already present in §11.1/§11.2. No
  model/prompt/LLM/brief/node concept enters a flag, JSON key, column name, error
  string, template, or help text. §1.1 gained a paragraph recording this re-check and
  arguing the two cases an implementer could get wrong: `action` steps stay opaque
  (the seam reads exactly one `params` key, `output`, which is packaging not meaning),
  and **§6.14 T3 is the genericity rule enforced at runtime** — an engine that ranked
  `high > medium` would have an opinion about severities baked into core, arriving
  through a comparison operator rather than a flag name. Every `severity`/`review`
  hit in the diff is either the fixture's own opaque token quoted verbatim (the
  documented `genericity.sh` exclusion) or the English word in prose.
