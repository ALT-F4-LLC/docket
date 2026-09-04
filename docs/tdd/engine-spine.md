# TDD: the engine spine — workflows, steps, `next`, activation/pinning (stage 3)

Status: draft — 2026-08-02, revised 2026-08-03 per the conformance review
(docs/tdd/engine-spine-review.md, findings F1–F8; verdict SOUND WITH FIXES, response
table appended there) · implements docs/design/engine-spec.md §2 (workflows,
activation, steps/claims, scheduling, fanout joins, rendering, guards), §11.1–§11.3
**verbatim and normatively**, §11.4 (wire shapes), and §9 items 2, 4, 5 as the
acceptance bar; engine-core.md §§2–5 supplies the semantics. Tracker unit: DKT-3.
Spec of record is engine-spec.md; deviations are DKT amendment issues per
docs/design/amendments.md citing the exact line, never silent changes.

## 1. Scope

engine-spec.md §10 stage 3, verbatim:

> 3. **Workflows, steps, `next`, activation/pinning** — the engine's spine (largest
>    stage; loop semantics land here).

and, from the same section:

> Guard verbs land with their underlying features (`stop`/`gate` at stage 3,
> `record`/`spawn` at stage 6); stage 3 carries the minimal run subset activation
> needs (run row, status, pins) with report/budgets completing at stage 6.

In scope:

1. `internal/workflow` — the §11.1 TOML grammar, parsed and validated at register
   time, plus `workflow register|list|show|init` with embedded templates.
2. Activation — the fat transaction of engine-core §3: DAG lints, binding, version
   pinning + operator file pins, issue-body snapshots, lazy phase expansion; the
   minimal run subset (run row, status, pins); re-activation rules.
3. Step lifecycle — the engine-core §1.3 status machine; `next --run` readiness;
   `claim` returning token + the §11.4 context bundle; the `complete` saga with each
   stage its own transaction; routing; `step` verb family including `render`.
4. Loops (§11.3 verbatim), fanout joins + `min_siblings`, `guard stop|gate`, and
   events **written** (the read surface is S6).
5. Schema **v7**.

Out of scope, explicitly, and each with the stage that owns it:

| Deferred | Owner | Why not here |
|---|---|---|
| Gate **execution** (subprocess, trust matching, argv resolution, capture) | S4 | §10 stage 4 is "gates + trust model"; this stage lands the saga's gate *stage* as a specified pass-through seam (§5.6) |
| `trust add\|list\|rm`, `~/.config/docket/trust.toml` | S4 | §4 is stage 4's subject |
| Threshold **field** validation against payload schemas; `schema register`; ordered enums; the builtin `aggregate` action's computation | S5 | the schema register does not exist until S5; §6.14 specifies exactly which predicates evaluate at S3, which route `waiting-human` instead, and how S5 upgrades them; §6.13 specifies the action seam that carries `aggregate` until then |
| **Vote-step execution** (casting, tallying, `vote_rule` resolution) | S4 | `type="vote"` steps validate at register (V14) and expand (§5.3.1), but votes are "driven as gates" (§2), so their execution lands with the gate machinery at §10 stage 4; at S3 a `vote` step is expanded and parks (§6.15) |
| Budget enforcement and the floor; `run report`; `dispatch open\|close\|verify\|abandon`; `guard spawn\|record`; `events list --since` | S6 | §10 stage 6 |
| `events --follow`, `events prune` | S7 | §10 stage 7 |

**`expected_cost` is parsed, validated, stored, and emitted on `next` rows at this
stage but enforces nothing** — §11.4 lists it in the next row and S6 owns the floor.
Parsing it now costs nothing and keeps the wire shape whole; a later stage adding a
field to `next` rows would be a compat event for dispatchers already consuming them.

### 1.1 Genericity check (CLAUDE.md PR bar, docs/design/genericity.md)

Every noun this stage introduces into core surface: `workflow`, `pipeline`, `step`,
`run`, `artifact`, `pin`, `activation`, `executor`, `class`, `fanout`, `sibling`,
`after`, `inputs`, `emits`, `threshold`, `routing`, `loop`, `ordinal`, `superseded`,
`guard`, `event`, `scope`, `context bundle`, `render`, `template`. Every one is
scheduling, dependency, or packaging vocabulary. **No model, prompt, brief, node,
severity, review, agent, or LLM concept appears anywhere in the surface** — not in a
flag, a JSON key, a column name, an error string, an embedded template, or a help
text. The two places instance meaning could leak are closed by construction:

- `executor` is an opaque string the engine only ever uses as a map key for `class`
  lookup and as an echo on the `next` row. There is no registry of known executors,
  no validation of the value, and no behavior keyed on it.
- `metadata` and `params` are opaque KV bags, stored as JSON text and returned
  verbatim. The engine never reads a key inside them.

The stranger test (§9 item 1): the shipped `standard-dev` template (§4.4) is a
two-step docs-review workflow with a fenced-command gate and no agents anywhere,
runnable and comprehensible from `docket workflow init --template standard-dev` plus
SKILL.md. It is what a human-only team gets by typing one command, and it is a test
(§8, `ZG1`), not an aspiration.

`severity` and `status` appear in the committed fixture
`docs/design/example-workflow.toml` as *field names inside an instance's threshold
predicate strings*. They are opaque tokens to the parser — the grammar knows
`agg(field op literal)`, never what a field means. That is the genericity line
holding exactly where §11.2 draws it.

**Nouns added by the 2026-08-03 revision** (§6.13–§6.15, §6.7.1, §6.11.1, §5.1.1),
re-checked against the same bar: `action`, `stub`, `payload`, `predicate`,
`aggregation`, `issue snapshot`, `sentinel`, `approve`, `reject`, `vote`, `human`.
Every one is scheduling, packaging, or gate vocabulary already in §11.1/§11.2, and
none names a model, prompt, brief, node, or review concept. Two are worth stating
explicitly because they are the ones an implementer could get wrong:

- **`action` steps stay opaque.** §6.13's seam carries `params` verbatim and reads
  exactly one key from it — `params.output`, the produced artifact *kind* (§4.3.1) —
  which is packaging, not meaning. The S3 stub never reads `field`, `method`, or
  `hold_spread`; the S5 computation reads them only as the generic `aggregate`
  parameters §2 defines, over whatever ordered enum a *user's* schema declared.
- **§6.14 T3 is the genericity rule doing work at runtime.** Refusing to guess an
  order for a schema-less field is precisely "ordered meaning comes from
  user-registered schemas" (genericity.md) enforced rather than assumed. An engine
  that ranked `high > medium` would have an opinion about severities baked into core
  — the failure mode the rule exists to prevent, arriving through a comparison
  operator rather than a flag name.

`type="human"` / `type="vote"` and `approve`/`reject` are §11.1's and §2's own words
for operator gates; they describe *who decides*, not *what is decided*, and the
stranger test holds — a human-only team approving a docs change reads them exactly as
written.

## 2. Schema version span

This stage ships **v7**, the third of the v5–v10 span fixed in
docs/tdd/reliability-delta.md §2. That table is authoritative and is not
re-litigated; this stage occupies its v7 row ("workflows, steps, runs (minimal),
pins").

| Schema | Stage (§10) | Contents |
|---|---|---|
| v5 | 1 — reliability delta | shipped |
| v6 | 2 — claims/leases + capability tokens | shipped |
| **v7** | 3 — the spine | **this stage**; DDL sliced per phase in §4.1, §5.1, §6.1, §7.1 |
| v8–v10 | 4–6 | later |

**v7 lands as one migration function** (`migrateV6ToV7`), assembled across the four
phases. Each phase adds its DDL slice to that function; the schema version stamp
moves to 7 in **phase 1** and does not move again. This is deliberate: four schema
versions for one stage would break the reliability-delta §2 mapping that stages 4–6
depend on, and a half-applied v7 is impossible because each phase's DDL is
`CREATE TABLE IF NOT EXISTS` / `ADD COLUMN` additive and the migration is
re-runnable. The **defensive rewind guard** follows the established per-version
pattern, but probes the **full v7 sentinel set** — `workflows`, `runs`, `steps`,
`events` — not just `workflows`: stamped ≥ 7 but **any** sentinel absent ⇒ rewind to
6 and re-run the migration.

**Probing only `workflows` would be a silent trap for exactly this stage's shape.**
The stamp moves at phase 1 while `migrateV6ToV7` keeps growing through phase 4, so a
DB migrated by a phase-1 build — including this repo's own DKT tracker, which is
dogfooded across the stage — is stamped 7 and has `workflows` only. A guard that
asks "stamped ≥ 7 but `workflows` absent?" sees the table present, does nothing, and
that DB never gains `runs`/`steps`/`events`; the failure surfaces later as a missing
table at activation. The fix is free because the DDL is `CREATE TABLE IF NOT EXISTS`
and the migration is re-runnable: re-running it against a partially-migrated DB adds
what is missing and touches nothing else. The sentinel list is a single constant next
to the migration function, and `TestRewindGuardProbesEverySentinel` asserts one entry
per table the v7 DDL creates — so a phase adding a table without extending the
sentinel set fails its own test rather than shipping a half-migrated dogfood DB.

**Never-mutate rule (inherited, reliability-delta §2.1).** Every timestamp column
created at v7 is `_ms INTEGER` epoch-milliseconds. No existing column's format is
touched — `issues.created_at`/`updated_at` stay RFC3339 TEXT forever. This is what
keeps §9 item 8 true.

**The lease-field reuse contract (binding repo fact, claims-leases §2.1).** The
`steps` table carries `owner`, `token_hash`, `expires_ms`, `attempt` with the
**same names, types, and nullability** as `issues` got at v6, so the v6 lease
helpers are reused rather than reimplemented. §6.3 specifies the refactor exactly.

## 3. What is dormant, and how each phase proves it

engine-spec §3: "Dormant unless workflows are used"; §9 item 8: "a workflow-free
repo shows no behavioral change on any existing verb." The proof obligation is
per-phase, because a phase that leaves the branch green must also leave it dormant.

The dormancy claim for this stage, precisely: **a repo with zero rows in
`workflows` behaves byte-identically to v6 on every pre-existing verb, at every
`--json` version, in human mode, and in exit code.** The three mechanisms:

1. **Additive DDL only.** v7 creates new tables. The only touch to an existing
   table is `issues.scope_globs` (§5.1) — a new nullable column, `NULL` on every
   pre-existing row and on every row created by an unmodified `issue create`.
2. **`next` mode switching is flag-gated.** `docket next` without `--run` executes
   the existing issue-mode code path, unmodified, and returns byte-identical output.
   §6.4 pins this as a code-structure requirement, not an intention.
3. **No existing verb gains a new field by default.** Step/run/scope data surfaces
   on `issue show` only under `--json=v2` and only when non-empty, matching the v6
   `lease` object's rule.

**Per-phase dormancy proof** (each is a QA check, each runs in the phase's own
commit group):

| Phase | Proof |
|---|---|
| 1 | v6→v7 migration applied; `workflows` empty; the full byte-compat sweep (§8.5) passes; `docket next` output byte-identical to the pre-phase baseline |
| 2 | a repo with a registered-but-never-activated workflow is still byte-identical on every existing verb; `issues.scope_globs` NULL everywhere |
| 3 | an **activated** run's issues read byte-identically under `--json=v1` and in human mode; step/lease data appears only under `--json=v2` |
| 4 | loops/joins/events add no output to any existing verb; the events table is written but has no read verb, so it cannot change any output |

Each proof re-runs the engine-spec §9 item 8 fixture protocol: `scripts/qa/fixtures/v4-baseline.db`
opens, migrates **4→7 in one pass**, and golden-diffs — with the v7 structures
asserted present *before* the diff is trusted, since a golden diff against a DB that
failed to migrate passes vacuously.

---

# Phase 1 — `internal/workflow`: grammar, validation, and the workflow verbs

Commit group 1. Leaves the branch green and is independently stoppable: it ships a
usable `workflow register|list|show|init` over a dormant table. Nothing activates,
nothing schedules, no step exists.

## 4.1 v7 DDL slice

```sql
CREATE TABLE IF NOT EXISTS workflows (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL,
    version       INTEGER NOT NULL,           -- the DEFINITION's version (§11.1 [pipeline].version)
    description   TEXT,
    source_path   TEXT,                       -- path as given at register; provenance only
    source_sha256 TEXT    NOT NULL,           -- content hash of the registered bytes
    body          TEXT    NOT NULL,           -- the TOML source, verbatim
    parsed        TEXT    NOT NULL,           -- canonical JSON of the parsed definition
    created_at_ms INTEGER NOT NULL,
    row_version   INTEGER NOT NULL DEFAULT 1, -- the CAS column (reliability-delta §6)
    UNIQUE(name, version)
);

CREATE INDEX IF NOT EXISTS idx_workflows_name ON workflows(name);
```

**Two version columns, named apart on purpose.** `version` is the workflow author's
`[pipeline].version` from §11.1 — the number runs pin. `row_version` is the CAS
column every mutable entity carries per reliability-delta §6.1. Collapsing them
would make `--if-version` mean "the workflow definition version", which is a
different thing that a re-register must not silently bump.

**`UNIQUE(name, version)` is the immutability rule.** A registered `name@version`
is frozen: re-registering identical bytes is an idempotent success (returns the
existing row, exit 0); re-registering *different* bytes under the same
`name@version` is `CONFLICT` (exit 4) naming both hashes. Version pinning
(engine-core §4: "editing a pipeline never changes an in-flight run") is worth
nothing if the pinned bytes can be swapped underneath a run.

**`parsed` is stored, not just `body`.** Activation and `next` read the parsed form
on a hot path; re-parsing TOML per read is both slower and a correctness hazard (a
parser change would silently re-interpret a pinned definition). The canonical JSON
is the pinned interpretation. `body` is retained for `workflow show --source` and
for the hash to mean something auditable.

## 4.2 The grammar, per §11.1

`internal/workflow/parse.go` decodes TOML into typed Go structs mirroring §11.1
exactly, field for field. The `[[step]]` struct carries every row of §11.1's table:
`name`, `executor`, `action`, `type`, `fanout`, `class`, `emits`, `payload`,
`voters`, `vote_rule`, `after`, `inputs`, `gates`, `params`, `min_siblings`,
`threshold`, `on_fail`, `loop`, `after_loop`, `max_attempts`, `max_fix_loops`,
`expected_cost`, `when`, `metadata`. Plus `[pipeline]`, `[match]`, and `[limits]`.

**Unknown keys are a hard `VALIDATION_ERROR`,** naming the key and the step. TOML
decoding is configured strict. A typo'd `max_attempt` silently defaulting to the
config value is exactly the class of bug that makes a workflow behave differently
from what its author read — and it is invisible until a run misroutes.

`gates` accepts both §11.1 spellings in one list: a bare string (a trusted gate
name) or an inline table `{name, source="fence:<tag>", pre=bool}`. A bare string
normalizes to `{name, source=null, pre=false}` in `parsed`, so downstream code has
one shape.

`[limits]` accepts `{max = N, lease_ttl = "45m", max_step_duration = "2h"}` or the
bare-int shorthand (`"write" = 1` ⇒ `{max: 1}`), per §11.1.

## 4.3 Register-time validation table

Every row is a `VALIDATION_ERROR` (exit 3) naming the workflow, the step, and the
offending field. Every row is a test case (§4.6). The table is the phase's contract:

| # | Rule | Spec line |
|---|---|---|
| V1 | `[pipeline].name` present, non-empty, unique-in-file | §11.1 `[pipeline]` |
| V2 | `[pipeline].version` present, integer ≥ 1 | §11.1 `[pipeline]` |
| V3 | at least one `[[step]]` | implied by §11.1; a zero-step workflow can never activate |
| V4 | `name` present and **unique within the workflow** | §11.1 `name`: "unique in workflow" |
| V5 | **exactly one of** `executor` / `action` / `type` / `fanout` per step | §11.1: "exactly one of `executor` / `action` / `type` / `fanout` per step" |
| V6 | `type` ∈ {`human`, `vote`} | §11.1 `type` |
| V7 | `emits` **required on executor steps** | §11.1 `emits`: "required on executor steps" |
| V8 | `after` **required** except on the first step and on `loop = true` steps | §11.1 `after`: "required except the first step and `loop = true` steps" |
| V9 | every `after` entry names a step in this workflow | §11.1 `after`: "intra-workflow predecessors" |
| V10 | `after = []` is legal and means root; a **missing** `after` on a non-exempt step is the error | §11.1: "`[]` = root (implicit topology was a footgun)" |
| V11 | `inputs` entries match `<step>.<kind>` \| `<step>.*` \| `issue.body` \| `issue.diff`; the named step exists; a `<step>.<kind>` names a kind that step actually produces (§4.3.1) | §11.1 `inputs` |
| V12 | `on_fail` ∈ {`fix-loop`, `waiting-human`, `skip`, `abandon-issue`} | §11.1 `on_fail`; engine-core §4 "closed vocabulary" |
| V13 | **a `type="human"` step's reject routing may not be `waiting-human`** — evaluated against the **effective** routing (declared `on_fail`, else the §11.1 default), so the default cannot smuggle the deadlock back in (§4.3.2) | §2: "a human gate's reject routing may not itself be `waiting-human` (register-time VALIDATION_ERROR)"; §11.1 `on_fail` (amended 2026-08-03) |
| V13a | **`on_fail` is required, explicitly, on `type="human"` steps** — the corollary of V13 over the effective value; the error names the step and the three legal values | §11.1 `on_fail`: "`type="human"` steps must declare it explicitly and `"waiting-human"` is invalid there" (amended 2026-08-03) |
| V14 | `voters` and `vote_rule` required on `type="vote"`, forbidden elsewhere | §11.1 `voters`, `vote_rule` |
| V15 | `fanout` non-empty when present | §11.1 `fanout` |
| V16 | `min_siblings` ≥ 1 and ≤ `len(fanout)`; only on fanout steps | §11.1 `min_siblings`; §2 fanout joins |
| V17 | `after_loop` names an existing step; only meaningful with `loop = true` in the workflow | §11.3 |
| V17b | a step that can route `fix-loop` (via `on_fail` or `threshold`) requires a `loop = true` step in the workflow (DKT-196) | §11.3 |
| V18 | `loop = true` steps have no `after` (their ordering comes from loop entry) | §11.1 `after`; §11.3 (3) |
| V19 | `max_attempts` ≥ 1; `max_fix_loops` ≥ 0; `expected_cost` ≥ 0 | §11.1 |
| V20 | `threshold` keys ∈ {`fix-loop`, `waiting-human`, `pass`} ∪ step names in this workflow | §11.2 |
| V21 | `threshold` predicate parses as `agg(field op literal)`, `agg ∈ {any, all, count>=n}`, `op ∈ {==, !=, >=, >, <=, <}` | §11.2 |
| V22 | `when` parses as a predicate over `kind`/`labels` only, its clauses joined by `and` throughout or `or` throughout — a mix of the two is refused (DKT-548). A clause is `<kind\|labels> <==\|!=\|contains> <value>` or the set form `labels contains-any (a, b, c)`, whose list must be non-empty, comma-separated, and free of whitespace inside its values; `contains-any` is `labels`-only (DKT-550). The set operator is equivalently spelled `contains_any` and its list equivalently delimited `[a, b, c]`, with the delimiters required to pair (DKT-1000) | §11.1 `when`; engine-core §4 "conditions (predicates over issue kind/labels only)" |
| V23 | `class` defaults to the `executor` value when unset | §11.1 `class`: "default = executor value" |
| V24 | `[limits]` values: `max` ≥ 1, `lease_ttl`/`max_step_duration` parse as durations | §11.1 `[limits]` |
| V25 | `payload` matches `name@version` shape (**shape only** at S3 — §6.14) | §11.1 `payload` |

**V5 and V8 are the two the spec calls out by name**, and they are the two that
make a workflow's topology unambiguous. V13/V13a are called out because they are the
one validation whose absence produces a *deadlock*: a human gate that routes rejects
to `waiting-human` parks forever on the resolution of the thing that just rejected.

**Exactly-one-match is not in this table** — it is an *activation*-time
VALIDATION_ERROR over the whole registered set (§5.3), not a property of one file.

### 4.3.1 A step's produced artifact kind, per step class

V11 resolves `<step>.<kind>` against the kind a step actually produces. That kind
comes from a different field depending on the step class, and getting this wrong
rejects the canonical fixture — so it is pinned here:

| Step class | Produced kind | Source |
|---|---|---|
| `executor` | `emits` (**required** — V7) | §11.1 `emits`: "required on executor steps" |
| `action` | `params.output` | §2: the builtin `aggregate` takes `params = { field, method, hold_spread, output }` |
| `fanout` | `emits`, applying to every sibling | §11.1 `emits`; siblings are instances of one step |
| `type = "human"` \| `"vote"` | **none** — produces no artifact | §11.1 restricts `emits` to executor steps; a gate records a decision, not an artifact |

`emits` is therefore **required on executor steps, optional on fanout steps that
declare it, and absent on action/human/vote steps** — the spec makes it required
only for executors, and V7 says exactly that and no more.

**This is the rule the committed fixture exercises**, and it was found by running
the fixture against a draft of this table: `fix` declares
`inputs = ["reconcile.findings", …]`, and `reconcile` is an `action` step with no
`emits` — it names its kind in `params.output = "findings"`. A V11 that looked only
at `emits` would reject the canonical register-test fixture. An `inputs` reference
to a `human`/`vote` step is a `VALIDATION_ERROR` naming the step class, since such a
step produces nothing to resolve.

A test asserts each row of this table, and `TestFixtureRegistersClean` (§4.6) is the
regression guard: the fixture must survive every future edit to the validation table.

### 4.3.2 V13 evaluates the *effective* routing, which makes `on_fail` mandatory on human steps

The rule as §2 states it ("a human gate's reject routing may not itself be
`waiting-human`") is about the routing a reject actually takes. §11.1's `on_fail`
default is `waiting-human`. Those two facts together mean a `type="human"` step that
declares no `on_fail` **has** the forbidden routing — by default, silently. So the
validator has exactly two options, and only one of them is sound:

| Reading | Consequence |
|---|---|
| V13 checks only the **explicitly declared** value | a human step that omits `on_fail` passes register, then deadlocks on its first reject — the exact failure the rule exists to prevent, reintroduced through the default |
| V13 checks the **effective** value (declared, else default) | a human step that omits `on_fail` is a `VALIDATION_ERROR`, i.e. **explicit `on_fail` becomes mandatory on `type="human"` steps** |

This TDD takes the second reading, and V13a states its corollary as its own rule so
the error message can be specific ("`type=\"human\"` step `commit-gate` must declare
`on_fail`; legal values here: `fix-loop`, `skip`, `abandon-issue`") rather than the
confusing "your reject routing is `waiting-human`" for a field the author never
wrote. The two rules are one check in the implementation and two rows in the table
because they are two distinct author-facing errors.

**This is a spec amendment, not an implementation choice**, and it is filed as one:
§11.1's `on_fail` row now carries the note "`type="human"` steps must declare it
explicitly and `"waiting-human"` is invalid there", applied by the operator on
2026-08-03 in engine-spec §11.1 and upstream (06 §11.1, 05 §1); the tracker issue is
recorded in §10 (A4). The amendment trail precedes the code, per
docs/design/amendments.md.

**The committed fixture is edited to match** (§4.6, `TestFixtureRegistersClean`): its
`commit-gate` step gains `on_fail = "fix-loop"`, which is the routing the reference
instance's flow actually wants — a rejected commit gate sends the issue back through
the fix loop rather than parking it. The upstream example (05 §1) carries the same
latent bug and gets the same one-line edit; that is the operator's, and it is noted
in the amendment issue.

**What `approve`/`reject` do, pinned** (nobody states it, and §6.10 gives only the
verbs):

| Verb | Effect on a `ready` `type="human"` step |
|---|---|
| `docket step approve STEP-N [--note …]` | step → `done`, `step-approved` event; downstream `after` successors become ready by the ordinary §6.3 predicate. An approve records **no artifact** (§4.3.1: human steps produce none) |
| `docket step reject STEP-N [--note …]` | step → routed per the step's **effective `on_fail`** — which V13/V13a guarantee is one of `fix-loop`, `skip`, `abandon-issue`, never `waiting-human`; `step-rejected` then `step-routed` events, in the one routing transaction of §6.8 stage N+1 |

Both refuse a non-`human` step with `VALIDATION_ERROR` (§6.9 R10) and both are
token-free (§6.10): a human gate is resolved by an operator, who never claimed it.

### 4.3.3 Register-time DAG lints

`planner.BuildDAG` + `planner.TopoSort` (Kahn, with `CycleError`) are **reused, not
duplicated** (binding repo fact). The reuse is direct: the `after` edges form a DAG
over step names exactly as `depends_on` edges form one over issue IDs.

The adapter is small and lives in `internal/workflow/lint.go`: step names are
assigned dense integer indices, `after` becomes reverse edges, and `TopoSort`'s
`CycleError` is caught and re-rendered with **step names** instead of `DKT-N`
(`CycleError.IDs` carries indices; the workflow layer maps them back). No change to
`internal/planner` is needed or made — a `FormatID`-shaped rendering difference is
the workflow layer's business.

| # | Lint | Error |
|---|---|---|
| L1 | the `after` graph is acyclic | `VALIDATION_ERROR` naming the step-name cycle |
| L2 | every non-root step is reachable from some root | `VALIDATION_ERROR` naming the orphans — except steps reached only by a `threshold` step-name routing (§11.2's interposed gate), which are legitimately unreached in the ordinary topology, and `loop = true` steps, which are excluded from ordinary expansion by §11.3 (3) |
| L3 | at least one root (`after = []` or first-step exemption) exists | `VALIDATION_ERROR` |
| L4 | `inputs` reference only steps that are **topological predecessors** (transitively `after`), so an input can never resolve to an artifact that does not exist yet | `VALIDATION_ERROR` — except `loop = true` steps, whose inputs bind per §11.3 (3) within their ordinal |

L2's exception list is the load-bearing detail. §11.2 says routing to a step name
interposes that declared step as a successor gate — so "unreached" is a
*legitimate* state for such a step, and a naive reachability lint would reject the
reference instance's own security workflow. The exemption keys on **being named as
a routing target**, never on unreachability: the canonical authoring shape is
`after = [routing-step]` (V8-compliant), which is reached ordinarily, and the same
one classification feeds the lint, expansion, and the engine's readiness latch
(DKT-38).

## 4.4 Verb surface — `workflow register|list|show|init`

| Verb | Flags | Effect |
|---|---|---|
| `docket workflow register <file.toml>` | `--json[=v2]` | parse + validate + lint; insert `name@version`; idempotent on identical bytes; `CONFLICT` on differing bytes at an existing `name@version` |
| `docket workflow list` | `--name`, `--limit`, `--orphans`, `--json[=v2]` | registered workflows; a `Collection` (reliability-delta §4.1) so v2 renders `{items,total,truncated}`; `--orphans` narrows to registrations whose NAME no file in any instance-config root declares any more (DKT-609), stamping each row with an `origin` verdict and refusing outright when there is no root to scan |
| `docket workflow show <name>[@<version>]` | `--source`, `--json[=v2]` | the parsed definition; `@version` omitted ⇒ highest registered; `--source` emits the stored TOML verbatim |
| `docket workflow init` | `--template NAME`, `--dir PATH`, `--force` | writes template files into `.docket/config/` (default), refusing to overwrite without `--force` |

`register` accepts `-` as the file argument to read stdin, so config generated in a
pipeline needs no temp file.

**`workflow init` templates are embedded, under `internal/`** (binding repo fact:
the Vorpal build's include list requires embeds there). They live in
`internal/workflow/templates/` with `//go:embed templates`. Shipped set at S3:

| Template | Contents | Purpose |
|---|---|---|
| `standard-dev` | two steps, one fenced-command gate, no fanout | the §9 item 1 stranger-test artifact: a two-step check-then-approve workflow a human-only team runs |
| `parallel-check` | prepare → fanout checks → summarize → verify | the general shape of the canonical fixture (fanout + join + threshold), with no instance vocabulary |

**Template names and their step names are core surface and carry the genericity
rule.** `parallel-check` is deliberately *not* named for what the reference
instance does with that shape; its steps are `prepare`, `check`, `summarize`,
`verify`, and its executor hints are role-shaped strings a human-only team would
recognize. A template shipped as `review-fanout` with a `review` step would put an
instance concept into core surface via the one file every new user reads first —
which fails the PR bar on its face (§1.1).

Both templates are **byte-identical to files a user could have typed**, and both are
asserted to `register` cleanly by a test (`TestEmbeddedTemplatesRegister`) — a
shipped template that fails its own validator is the worst possible first
experience, and it is the kind of thing that rots silently as the validator grows.

`workflow init` writes to `.docket/config/workflows/` per §2's instance-config
lifecycle. It creates the directory tree if absent and never overwrites without
`--force`; the refusal is `CONFLICT` (exit 4) naming the existing paths.

## 4.5 Error taxonomy usage (phase 1)

| Situation | Code | Exit |
|---|---|---|
| grammar/validation/lint failure (V1–V25 incl. V13a, L1–L4) | `VALIDATION_ERROR` | 3 |
| file unreadable / not found | `NOT_FOUND` | 2 |
| re-register differing bytes at existing `name@version` | `CONFLICT` | 4 |
| `workflow show` on an unregistered name/version | `NOT_FOUND` | 2 |
| `workflow init` target exists without `--force` | `CONFLICT` | 4 |
| `--if-version` mismatch on a workflow row | `CONFLICT` | 4 |

No new codes. The taxonomy was extended once at S1 (reliability-delta §8) and this
stage adds none — every situation above maps to a code that already exists.

## 4.6 Test plan (phase 1)

**Go unit tests** (`internal/workflow/parse_test.go`, `lint_test.go`) — table-driven
in the style of `internal/planner/plan_test.go`:

- **The validation table as a table test** — one case per row, each asserting the
  error mentions the offending step and field. A row without a case fails review: the
  table in §4.3 and the test table are checked against each other by
  `TestValidationTableIsComplete`, which asserts one test case per documented rule
  ID. (A validation table that drifts from its tests is a table that documents
  behavior the code does not have.) **The rule IDs are the authority, not a count**:
  the test enumerates `V1…V25` *including* `V13a`, i.e. **26 rules across 25 numbered
  IDs**, and asserts set equality between the documented IDs and the test cases'
  IDs — never `len(cases) == 25`, which is exactly the assertion that breaks when a
  rule is split. V13 and V13a are separate cases: V13 feeds a human step with an
  explicit `on_fail = "waiting-human"`, V13a feeds one that declares no `on_fail`,
  and each asserts its own distinct message (§4.3.2).
- **The lint table L1–L4**, including L2's two exceptions (a threshold-interposed
  step and a `loop = true` step are *not* orphans) — the exceptions are where a
  naive implementation breaks the fixture.
- Cycle rendering: a 3-step cycle reports **step names**, not `DKT-N`.
- Strict decoding: an unknown key at `[pipeline]`, `[match]`, `[limits]`, and
  `[[step]]` level each error.
- `class` defaulting to `executor`; `min_siblings` defaulting to `len(fanout)`;
  `on_fail` defaulting to `waiting-human`; `loop` defaulting to false — the §11.1
  default column, asserted per field.
- Canonical-JSON stability: parse → serialize → parse round-trips, and the
  serialization is byte-stable across runs (map iteration order cannot leak into
  `parsed`, or the same file would hash differently on re-register).
- **The committed fixture registers clean**: `docs/design/example-workflow.toml`
  parses, validates, and lints with zero errors. This is the canonical register
  test named in the work order, and it exercises fanout, loops, `after_loop`,
  thresholds, pre-gates, `action`, and `type="human"` in one file.
- `TestEmbeddedTemplatesRegister`: every embedded template parses, validates, lints.

**Go unit tests** (`internal/db/workflows_test.go`): insert/read round-trip;
`UNIQUE(name, version)` idempotent re-register returns the original row and inserts
nothing; differing bytes at the same `name@version` errors distinguishably from
not-found; v6→v7 migration adds the tables and is idempotent.

**Go unit tests** (`internal/cli`, in the style of `next_test.go`): each verb's exit
code and `.code` field per §4.5; `register -` reads stdin; `workflow show` without
`@version` selects the highest.

**QA** (`scripts/qa/test_zg_workflow.sh`, `SECTIONS` entry `ZG:test_zg_workflow`):
register the committed fixture end-to-end; the §4.5 refusal matrix by exit code
**and** `.code`; `workflow init --template standard-dev` into a temp dir then
register what it wrote (the stranger-test loop, closed mechanically); v2 envelope
shape on `workflow list`; **phase-1 dormancy** (§3).

---

# Phase 2 — Activation: the fat transaction, pins, snapshots, expansion

Commit group 2. Leaves the branch green: a run can be started and activated, and its
steps exist and are inspectable. Nothing claims or executes yet — `next --run` lands
in phase 3.

## 5.1 v7 DDL slice

```sql
CREATE TABLE IF NOT EXISTS runs (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    request        TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL DEFAULT 'planning',
    reason         TEXT,                        -- why waiting-human / abandoned
    activated_at_ms INTEGER,
    created_at_ms  INTEGER NOT NULL,
    updated_at_ms  INTEGER NOT NULL,
    row_version    INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS run_issues (
    run_id      INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    workflow_id INTEGER REFERENCES workflows(id),  -- the binding (engine-core §1.2)
    body_snapshot TEXT,                            -- activation-time issue body
    body_sha256   TEXT,
    issue_snapshot TEXT,                           -- activation-time {title, kind, labels, scope} as JSON (§5.1.1)
    expanded_at_ms INTEGER,                        -- NULL until this issue's phase expands
    PRIMARY KEY (run_id, issue_id)
);

CREATE TABLE IF NOT EXISTS pins (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id    INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind      TEXT    NOT NULL,     -- 'workflow' | 'file'
    ref       TEXT    NOT NULL,     -- 'name@version' for workflow; path for file
    sha256    TEXT    NOT NULL,
    UNIQUE(run_id, kind, ref)
);

CREATE TABLE IF NOT EXISTS run_fences (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    tag        TEXT    NOT NULL,    -- the fence tag, e.g. 'ac'
    ordinal    INTEGER NOT NULL,    -- position within the issue body
    command    TEXT    NOT NULL,    -- the harvested line, verbatim
    sha256     TEXT    NOT NULL,
    UNIQUE(run_id, issue_id, tag, ordinal)
);

ALTER TABLE issues ADD COLUMN scope_globs TEXT;   -- JSON array; NULL = no scope declared

CREATE INDEX IF NOT EXISTS idx_run_issues_issue ON run_issues(issue_id);
CREATE INDEX IF NOT EXISTS idx_pins_run ON pins(run_id);
```

**`issues.scope_globs` is the new column the binding repo fact requires.**
`issue_files` is not it: `issue_files` is a set of concrete paths used by
`plan.go`'s `splitByFileCollision`, while scope (engine-core §1.2) is a list of path
**globs** declared by the planner as a judgment about what an issue is *expected* to
touch. They differ in cardinality, in semantics (actual vs. intended), and in
matching rule (equality vs. glob intersection). Overloading `issue_files` would make
`plan`'s existing file-collision behavior depend on scope declarations and change
`plan` output for repos that never activate a workflow — a §9-item-8 violation.

The relationship is that `splitByFileCollision` is scope exclusion's **precursor**:
same idea (never run two things that touch the same stuff concurrently), one level
up in generality. §6.5 specifies the glob-intersection rule that replaces string
equality for steps. `plan` itself is untouched.

`scope_globs` is `NULL` on every pre-existing row and on every row created by an
unmodified `issue create`. `docket issue create|edit --scope 'internal/db/**'`
(repeatable) sets it; that flag is the only writer.

### 5.1.1 `run_issues.issue_snapshot` — the rest of the issue snapshot

§11.4's `context.issue` is `{id, title, body_snapshot, kind, labels, scope}`, and §6.6
requires assembly to read **no** live issue field. `body_snapshot`/`body_sha256` cover
one of those six; `id` is the key. The remaining four — `title`, `kind`, `labels`,
`scope` — are captured at **activation stage 4** (§5.3) into a single
`issue_snapshot` TEXT column holding canonical JSON:

```json
{"title":"…","kind":"task","labels":["a","b"],"scope":["internal/db/**"]}
```

**One JSON column rather than four discrete ones**, because these fields are never
queried, filtered, or joined on — they are read back whole, exactly once, to
materialize `context.issue` — so discrete columns would buy nothing over a blob while
costing a schema migration every time §11.4's issue shape gains a field.

Canonical JSON (sorted keys, `labels`/`scope` in their stored order) is required, not
incidental: `context` bundles are golden-diffed byte-for-byte (§8.3), so a
map-iteration-ordered serialization here would make the goldens flap.

**`scope` is snapshotted even though R4 reads live.** These are two different
questions and they legitimately have two different answers: the *scheduling* check
(§6.3 R4, mutual exclusion) asks "what does this issue touch **now**", so it reads
`issues.scope_globs` live and picks up an operator's mid-run correction; the *context
bundle* asks "what was this issue declared to touch when the run was activated", and
§9 item 5 requires that answer to be frozen. Reading live for the bundle would break
mid-run edit immunity; freezing the scheduler would ignore a correction that exists
precisely to prevent a collision. Both are stated so neither is "fixed" into the
other later.

**No AUTOMATIC path refreshes the snapshot** (DKT-741). It is written once, at
activation stage 4, and nothing rewrites it for the life of the run — not a claim, not
a fix-round, not a re-instantiation, and not an `issue edit`. So the consumers that
read it — the packet's `context.issue.scope` (§6.6) and the recorded `issue.diff`
scope (§6.7.1 D1) — cannot drift apart from each other or from what the run was
activated on. Re-snapshotting at claim time instead would break exactly that: two
steps of one run would render two different declared scopes and record their diffs
over two different path sets, and a packet would stop being reproducible from the
ledger. `docket issue edit --scope` therefore reaches the live column and nothing
else, which is correct and is also a trap:

| what the operator wants | what `issue edit --scope` does |
|---|---|
| stop a collision the scheduler is about to allow | works, immediately — R4 reads live |
| widen an authorized scope so a live step's packet says so | **does nothing**; the packet renders the frozen snapshot |

The second row has **two** dispositions, and which one is right depends on whether the
run's premise changed or one declaration was corrected.

**Where the premise changed**, take the issue out of the run and re-plan it —
`docket run abandon RUN-N --issue DKT-M --reason "scope widened"`, then plan it into a
new run, whose activation snapshots the widened scope afresh. It is expensive on
purpose: a mid-run scope widen can invalidate the premise every step of that issue
already executed under, and re-planning is what re-establishes it.

**Where the premise is intact**, `docket run refresh-scope RUN-N --issue DKT-M
--reason R` copies `issues.scope_globs` into that one run-issue's snapshot and
rewrites nothing else in it (DKT-869, `RefreshIssueScopeInRun`). DKT-741 had ruled out
any refresh verb; RUN-52 (VPL-434) then charged twice for that ruling on an intact
premise — the panel rejected work as out of scope, the operator agreed and widened it,
the already-minted `fix@2` step still rendered the old scope, and the issue was
abandoned mid-loop. The freeze keeps its default and gains an explicit exception whose
four properties are what keep it from being a hole in §9 item 5:

1. **It carries no scope of its own.** There is no `--scope` on it; `issue create|edit
   --scope` stays the sole writer of the column it copies, so the refresh cannot make
   real a scope that was not declared through the one gate widening has always had. A
   refresh with no widen behind it is **refused** (CONFLICT), not silently no-op'd.
2. **No step straddles it.** It refuses while any of the issue's steps is `claimed`,
   `running`, or `gated`, and while a dispatch is open — the repin quiescence rule
   (DKT-408) applied to the other frozen premise. `pending` and `waiting-human` are the
   refreshable states.
3. **It rewrites no history.** Terminal steps keep their artifacts and the scope their
   diffs were computed over; only the remaining steps' renders move.
4. **The discontinuity is in the ledger.** One `issue-scope-refreshed` event (actor
   `human`) carries the old scope, the new scope, the instances reached, and the
   operator's reason — so two steps of one run declaring two different scopes is a
   dated, attributable fact rather than drift a reader must infer.

`issue edit --scope` **warns**, naming the run, the frozen scope, the count of live
steps, and **both** verbs, whenever the edit changes the scope of an issue that still
has non-terminal steps in a non-terminal run (`ScopeEditFrozenForActiveRuns`). It
reports rather than refuses, because the write is real for the scheduling half.

## 5.2 Run status and the minimal subset

engine-core §1.1: `planning → active ⇄ waiting-human → done | abandoned`. All five
statuses exist at S3 because the step lifecycle routes into `waiting-human` and
`abandoned` in phase 3; the *ledger rollup* is what S6 adds, not the statuses.

| Verb | Effect |
|---|---|
| `docket run start [--request-file F] [--json]` | creates a run in `planning`; `RUN-N` |
| `docket run activate RUN-N` | the fat transaction (§5.3) |
| `docket run pause\|resume RUN-N [--reason R]` | `active ⇄ waiting-human`; pause blocks new claims, honors in-flight completes (engine-core §3.4) |
| `docket run abandon RUN-N --reason R` | terminal; revokes live leases |
| `docket run status [RUN-N] [--active]` | read-only; effective status computed at read |

`--budget N` on `run start` is **accepted and stored** but enforces nothing until S6.
Accepting it now means the S6 upgrade adds enforcement, not a flag — and a flag
appearing later would break the `run start` invocation an S3-era harness scripted.

**`run report` is not here.** §10 assigns it to stage 6 explicitly.

## 5.3 The fat transaction (engine-core §3.2, engine-spec §2)

`docket run activate RUN-N` is **one transaction**. Its stages, in order, each
failing the whole activation:

1. **Bind.** For each issue in the run, evaluate every registered workflow's
   `[match]` (`kind`, `labels_any`, `labels_all`, `unless_labels`) against the
   issue. **Exactly one must match.** Zero or multiple ⇒ `VALIDATION_ERROR`
   "naming the issue and the candidate workflows" (§11.1, verbatim). Match
   evaluation: `kind` ∈ list (absent ⇒ any); `labels_any` intersects; `labels_all`
   subset; `unless_labels` disjoint — `unless_labels` is evaluated **last and wins**,
   so an exclusion cannot be defeated by an inclusion.
2. **Lint the work DAG.** `planner.BuildDAG` + `TopoSort` over the run's issues and
   their `depends_on` relations — reused directly, no adaptation needed at this
   level. A cycle is `VALIDATION_ERROR` with the existing `CycleError` rendering.
3. **Pin.** Insert a `pins` row per bound workflow (`kind='workflow'`,
   `ref='name@version'`, `sha256=workflows.source_sha256`), and one per
   **operator-supplied file pin**: `--pin PATH` (repeatable) reads the file, hashes
   it, and records `kind='file'`. §2 is explicit that this is "how the reference
   instance pins its contracts, fragments, and policy without core knowing what they
   are" — so core reads bytes, hashes them, stores the path, and never opens the
   content again except to serve `context.pins`. A `--pin` path that does not exist
   or is not a regular file is `NOT_FOUND` (exit 2); pinning is never partial.
4. **Snapshot issue bodies and fields.** `run_issues.body_snapshot` + `body_sha256`
   for every issue in the run, **and** `issue_snapshot` — the canonical JSON of
   `{title, kind, labels, scope}` (§5.1.1). Together these are the sole issue state
   the context bundle ever reads (§6.6); the mid-run-edit immunity of §9 item 5 rests
   entirely on this row, and §8.3's own test edits the title and adds a label, so a
   body-only snapshot fails it.
5. **Harvest fenced blocks.** From each snapshot, extract fenced code blocks whose
   info string matches a tag declared by any bound workflow's gates
   (`source = "fence:<tag>"`). Extraction is **literal** (engine-core §6: "no prose
   parsing"): the block's lines, verbatim, one `run_fences` row each, each hashed.
   Harvesting happens **at activation** so "post-activation edits cannot inject"
   (§4). Nothing is executed at S3 (§5.6) — but the harvest, the snapshot, and the
   hash are all real now, which is what makes S4 a pure execution addition.
6. **Expand phase 1 lazily.** Expand Level-2 steps for **phase-1 issues only** —
   those whose `depends_on` predecessors are all satisfied at activation. Later
   phases expand when their predecessors complete (§6.7). `run_issues.expanded_at_ms`
   records which issues have been expanded, so expansion is idempotent and
   re-entrant.
7. **Promote and flip.** Issues move `backlog → todo` via the existing issue verbs
   (§2: "promotion via the issue verbs"); run status → `active`; `activated_at_ms`
   set; events written (phase 4 adds the events table; phase 2 writes through a seam
   that is a no-op until then — §7.6).

**Nothing executes inside this transaction** (§6: "No subprocess ever executes
inside a transaction"). Activation runs no gate, no action, no command. It reads
files (for pins and their hashes), which is not execution.

### 5.3.1 Expansion is a pure function

engine-core §2: "Expansion is a pure function of (issue kind, labels, pipeline
definitions @ pinned version). Same issue, same pipeline version ⇒ identical steps,
every time." Concretely, expansion of one issue produces, deterministically:

- one step row per non-`loop` `[[step]]`, at ordinal `k=0`, named `name@0`;
- for a `fanout` step, one row per hint: `name@0#0 … name@0#n-1`, in **declared hint
  order** — the index is the position in the `fanout` array, never a map iteration;
- steps whose `when` predicate is false are created with status `skipped`, not
  omitted — §11.1 says "step is `skipped` when false", and a *created* skipped step
  is what makes a downstream `after` resolvable and the topology identical
  regardless of the predicate's value;
- `loop = true` steps are **not** created (§11.3 (3): "excluded from ordinary
  expansion");
- steps named as a `threshold` step-name routing target **are** created, in
  status `pending`; readiness latches them until a routing predecessor's
  recorded routing names them, and a routing that resolves anywhere else
  terminalizes them `skipped` (§11.2's interposed gate, DKT-38).

Determinism is asserted directly: `TestExpansionIsDeterministic` expands the fixture
100 times and requires byte-identical step rows (names, order, statuses) every time.
Go map iteration order is randomized, so this test fails loudly the moment expansion
walks a map without sorting — which is the only realistic way this property breaks.

## 5.4 Re-activation rules (§2)

> Re-activation lints and expands new phases only, inherits the original pin set,
> and is refused while a dispatch is open.

| Rule | Behavior |
|---|---|
| RA1 | re-activating an `active` run lints again and expands **only** issues with `expanded_at_ms IS NULL` whose predecessors are now satisfied |
| RA2 | the pin set is **inherited, never recomputed** — a re-activation after a workflow re-register or a pinned file edit uses the original `pins` rows, unchanged |
| RA3 | new issues added to the run since activation are bound and snapshotted at re-activation, and pinned against the **already-pinned** workflow version if one exists for that name |
| RA4 | refused while a dispatch is open — **vacuously true at S3** (dispatches are S6); the check is written as a seam that queries a not-yet-existing table via a helper returning `false`, so S6 adds a query, not a call site |
| RA5 | re-activating a `done` or `abandoned` run is `CONFLICT` (exit 4) |

RA2 is the reproducibility guarantee. If re-activation re-pinned, an in-flight run
would silently adopt an edited workflow — precisely what engine-core §4 forbids.

## 5.5 Error taxonomy usage (phase 2)

| Situation | Code | Exit |
|---|---|---|
| zero or multiple workflows match an issue | `VALIDATION_ERROR` (names issue + candidates) | 3 |
| work-DAG cycle | `VALIDATION_ERROR` | 3 |
| `--pin` path missing/unreadable | `NOT_FOUND` | 2 |
| activating a `done`/`abandoned` run | `CONFLICT` | 4 |
| activating a run with no issues | `VALIDATION_ERROR` | 3 |
| context bundle exceeds `context.error_bytes` at expansion | `VALIDATION_ERROR` | 3 |
| run not found | `NOT_FOUND` | 2 |

The context-size check at expansion is engine-core §8, verbatim: "Oversized context
bundles (config caps) are an engine error **at expansion time** — the fix is a
pipeline/contract change, visible before spend." The caps are the v6
`context.warn_bytes` / `context.error_bytes` keys, already shipped (claims-leases
§3.3) — this stage is their first reader. Warn emits on stderr in human mode and
sets a `context_warn` flag on the step row in JSON mode; error refuses activation.

## 5.6 The gate seam (S4 boundary), specified

The work order requires this seam be specified, not improvised. The rule:

**At S3, everything about gates is real except the subprocess.**

| Element | S3 | S4 adds |
|---|---|---|
| `gates` parsed, validated, stored in `parsed` | real | — |
| `pre=true` vs. ordinary ordering | real | — |
| fence harvesting, snapshot, hash (`run_fences`) | real | trust matching against harvested rows |
| the saga's gate stage exists as its own transaction | real | the spawn |
| `gate-started` event precedes each gate | real (written) | — |
| gate result recorded | real, with `verdict='pass'`, `exit=0`, `argv=null`, `output=''`, and **`stub=true`** | actual argv/exit/output/duration; `stub` absent |
| resume-after-crash re-enters at the gate stage | real | the re-runnable/`waiting-human` decision (§2) |
| routing consumes the gate verdict | real | — |

The seam is a single Go interface with two implementations:

```go
// GateRunner executes one gate and returns its result. S3 ships passThroughRunner;
// S4 ships the real one. The saga is written against the interface, so S4 changes
// one constructor call and nothing else.
type GateRunner interface {
    Run(ctx context.Context, g GateSpec, sc StepContext) (GateResult, error)
}
```

`passThroughRunner` returns `{verdict: "pass", exit: 0, stub: true}` **without
touching the process table**. Two consequences are asserted as tests: (a) a
workflow whose gate would fail still passes at S3, and the QA section says so in a
comment so nobody reads a green run as gate coverage; (b) `stub: true` appears in
every S3 gate result, so an operator inspecting a run can tell stubbed gates from
real ones. A silent pass-through that looked identical to a real pass would be a
trap for exactly the S3→S4 window.

`gate_results` **is not created at S3** — it is v8's, per reliability-delta §2's
mapping. S3's stub results ride in the step row's `gate_trail` JSON column (§6.1),
and S4 migrates that trail into the real table. Creating v8's table early would
break the version mapping stages 4–6 depend on.

## 5.7 Test plan (phase 2)

**Go unit tests** (`internal/workflow/match_test.go`): the `[match]` predicate
matrix — `kind`/`labels_any`/`labels_all`/`unless_labels` in every combination,
including `unless_labels` beating `labels_any`; absent clauses matching anything.

**Go unit tests** (`internal/engine/activate_test.go`):
- exactly-one-match: zero matches and two matches each `VALIDATION_ERROR`, each
  naming the issue **and** every candidate workflow (asserted by substring).
- **orphan annotation** (DKT-609, `internal/engine/dkt609_test.go`): each named
  candidate whose NAME no file in any instance-config root declares any more is
  marked `(no source on disk — orphaned registration, deprecation candidate)`,
  and the refusal carries the remedy. It DECORATES the candidate set and never
  changes it — an orphaned registration still binds, because a registration is
  a row and not a file. With no root to scan the verdict is `unchecked` and the
  message is byte-identical to the pre-DKT-609 one: "nothing was checked" must
  never render as "nothing is orphaned".
- **bind-to-highest** (§11.1 as amended 2026-08-05, DKT-40): the candidate set is
  the **highest registered version of each name**, so exactly-one-match applies
  across NAMES. `TestBindingUsesHighestVersionOfEachName` is DKT-8's M2a wedge as
  a regression — `docs-review@1` + `@2`, one issue, activation binds `@2` — which
  is what lets the version-bump evolution path (D15, the retro skill) complete.
  `TestBindingRefusesTwoDifferentNames` holds the refusal where it belongs and
  asserts superseded versions are *not* named. `TestBindingAgreesWithWorkflowShowResolution`
  is the one helper asserting binding and `workflow show`'s resolution agree —
  their DISAGREEING was the defect. Superseded versions stay registered and
  resolvable by explicit `@version` for the runs that pinned them; RA2's
  `definitionByID` path reads the unreduced set. QA section ZG30 drives the whole
  bump-then-reactivate sequence through the CLI.
- `TestExpansionIsDeterministic` (§5.3.1), 100 iterations.
- fanout expands in declared hint order, `#0..#n-1`.
- `when` false ⇒ step created `skipped`, not omitted; topology identical either way.
- `loop = true` steps absent at ordinal 0.
- threshold-interposed steps created `pending`.
- pins: workflow pin hash equals `workflows.source_sha256`; `--pin` file hash equals
  the file's SHA-256; a missing `--pin` aborts the whole activation (assert **no**
  run rows, no steps, no pins written — the transaction is fat, so a failure leaves
  nothing).
- snapshot: `body_snapshot` equals the body at activation; editing the issue body
  afterward leaves it unchanged. **`issue_snapshot`** (§5.1.1) captures
  `title`/`kind`/`labels`/`scope` at activation, is canonical JSON (asserted stable
  across 100 serializations, same discipline as `parsed`), and is unchanged by an
  edit to any of the four afterward.
- fence harvesting: multiple blocks, multiple tags, ordinals stable, hashes correct;
  a block whose tag no workflow declares is **not** harvested.
- re-activation RA1–RA5.
- oversized context ⇒ `VALIDATION_ERROR` at expansion, nothing written.

**Go unit tests** (`internal/db/migrate_v7_test.go`): 6→7 adds every table and
`issues.scope_globs`; idempotent; pre-existing rows have `scope_globs IS NULL`.
**The rewind guard** (§2): a DB stamped 7 carrying `workflows` **but not** `runs` /
`steps` / `events` — the phase-1-build dogfood shape — is detected, rewound, and
re-migrated to completeness; one case per sentinel, plus
`TestRewindGuardProbesEverySentinel` asserting the sentinel constant lists every
table the v7 DDL creates.

**QA** (`test_zg_workflow.sh`, extended): activate the fixture workflow against a
real issue graph; assert step rows exist with expected names; `--pin` round-trip;
**phase-2 dormancy** (§3) — a registered-but-unactivated workflow changes no
existing verb's output.

---

# Phase 3 — Step lifecycle: `next --run`, claim, the complete saga, routing

Commit group 3. The largest phase. Leaves the branch green with a full
claim→complete cycle over a real run.

## 6.1 v7 DDL slice

```sql
CREATE TABLE IF NOT EXISTS steps (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id         INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    issue_id       INTEGER REFERENCES issues(id) ON DELETE CASCADE,  -- NULL = run-level step
    workflow_id    INTEGER NOT NULL REFERENCES workflows(id),
    step_name      TEXT    NOT NULL,          -- the [[step]].name
    ordinal        INTEGER NOT NULL DEFAULT 0,-- loop ordinal k (§11.3)
    sibling_index  INTEGER,                   -- fanout #i; NULL when not fanned out
    instance       TEXT    NOT NULL,          -- rendered 'name@k#i' (§11.3), the identity
    kind           TEXT    NOT NULL,          -- 'executor' | 'action' | 'human' | 'vote'
    executor       TEXT,                      -- opaque hint; NULL for non-executor kinds
    class          TEXT,                      -- concurrency-accounting key
    status         TEXT    NOT NULL DEFAULT 'pending',
    attempt        INTEGER NOT NULL DEFAULT 0,
    max_attempts   INTEGER,
    expected_cost  REAL    NOT NULL DEFAULT 0,
    owner          TEXT,                      -- lease fields, verbatim from v6 (§2)
    token_hash     TEXT,
    expires_ms     INTEGER,
    started_ms     INTEGER,                   -- schedule-to-close clock (max_step_duration)
    activity_ms    INTEGER,                   -- refreshed by every saga stage commit (§2)
    saga_stage     TEXT,                      -- resume point; NULL when not in the saga
    gate_trail     TEXT,                      -- JSON array of gate results (S4 → gate_results)
    routing        TEXT,                      -- the routing this step resolved to
    metadata       TEXT,                      -- opaque KV, verbatim from the definition
    context_bytes  INTEGER,                   -- closure size (engine-core §8)
    created_at_ms  INTEGER NOT NULL,
    updated_at_ms  INTEGER NOT NULL,
    row_version    INTEGER NOT NULL DEFAULT 1,
    UNIQUE(run_id, issue_id, instance)
);

CREATE TABLE IF NOT EXISTS artifacts (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_id       INTEGER REFERENCES steps(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL,
    body          TEXT    NOT NULL,
    payload       TEXT,                      -- JSON; validated at S5
    sha256        TEXT    NOT NULL,
    created_at_ms INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS step_inputs (
    step_id     INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,            -- declared position (§2 ordering)
    artifact_id INTEGER REFERENCES artifacts(id) ON DELETE CASCADE,
    PRIMARY KEY (step_id, position, artifact_id)
);

CREATE INDEX IF NOT EXISTS idx_steps_run_status ON steps(run_id, status);
CREATE INDEX IF NOT EXISTS idx_steps_expires_ms ON steps(expires_ms);
CREATE INDEX IF NOT EXISTS idx_steps_issue ON steps(issue_id);
CREATE INDEX IF NOT EXISTS idx_artifacts_step ON artifacts(step_id);
```

`instance` is stored **rendered** (`<step>@<k>#<i>`) as well as decomposed
(`step_name`/`ordinal`/`sibling_index`) because §11.3 makes the rendered form the
step's public identity — it appears in wire shapes, events, and error strings — and
a stored rendering cannot drift from a formatting helper someone edits later. The
`UNIQUE(run_id, issue_id, instance)` index is the loop/fanout correctness guard: two
rows claiming the same identity is the bug §11.3 exists to prevent.

**Artifacts are capped at 1MiB with explicit refusal** (§3): a `--artifact-file`
exceeding it is `VALIDATION_ERROR` naming the size and the cap.

## 6.2 The status machine (engine-core §1.3, verbatim)

```
pending ──ready──> ready ──claim──> claimed ──start──> running ──record──> gated
   ▲                 ▲                                    │                  │
   │                 └──lease expiry / executor death─────┘         gates run by engine
   │                                                                 │           │
   └────────── new attempt (attempt++ < max) ──────────── fail ◄──gate fail   gate pass
                                                            │                  │
                                              attempts exhausted               ▼
                                                            ▼                done
                                                     waiting-human
skipped: reachable from pending when the pipeline's condition for the step is false
superseded: terminal; prior-ordinal instances at loop entry (engine-spec §11.3)
failed-routed: terminal record of a fanout sibling routed by on_fail (skip ⇒ skipped)
```

**Ten statuses**: `pending`, `ready`, `claimed`, `running`, `gated`, `done`,
`waiting-human`, `skipped`, `superseded`, `failed-routed` — every transition
CAS-guarded on `row_version` and event-logged. Of these, `ready` is **computed, never
stored** (below), so the persisted enum has **nine** members; both numbers are stated
because the enum's size and the machine's size are different questions and an
implementer sizing one from the other gets it wrong.

**`ready` is computed, never stored as intent** (engine-core §1.3). The `ready`
status in the machine is the *observable* result of the computation at read time,
not a scheduling decision someone wrote down. `next` computes it; nothing persists a
step as "ready" and later discovers it wasn't. Concretely: a step's stored status
stays `pending` until it is claimed, and `next`/read verbs render it as `ready` when
the §6.3 predicate holds. This is the same discipline as v6's effective lease status
— computed at read, never written back — and it is what makes "status never lies
because nobody called `next`" true for steps as well as leases.

## 6.3 `next --run`: the readiness predicate

engine-core §5 and §1.3, as a conjunction. A step is ready iff **all** hold:

| # | Condition | Source |
|---|---|---|
| R1 | the run is `active` | §1.3; §2 scheduling |
| R2 | the issue's `depends_on` predecessors are satisfied | §1.3 "its issue's dependencies are satisfied" |
| R3 | its intra-workflow predecessors (`after`) are **done** — and for a fanned-out predecessor, **joined** (§7.4) | §1.3; §2 fanout joins |
| R4 | its scope conflicts with no `claimed`/`running` step (glob intersection) | §1.3; §5 mutual exclusion |
| R5 | per-class concurrency headroom exists | §2 "concurrency headroom per executor-hint class" |
| R6 | the step's status is `pending` and its `when` did not skip it | §1.3 |
| R7 | budget headroom exists | §1.3 — **S6**; at S3 the check is a seam returning `true` (§6.8) |

Ordering: **priority then age** (§2: "Ordering: priority then age"). Priority is the
issue's; age is the step's `created_at_ms`, tie-broken by `id` so the order is total
and reproducible. `sortIssues`'s existing `priorityRank` is reused for the priority
half — same ranking, no second definition to drift.

**`--limit`** applies after ordering, with the v2 truncation contract
(reliability-delta §4.2/§5): `readyTotal` before slicing, `truncated` computed,
negative limit `VALIDATION_ERROR` under v2. `next --run` is a *new* verb surface, so
it has no v1 legacy to preserve — but it implements the same `Collection` interface
so its envelope is uniform with every other list verb.

**Lazy reaping happens here and at `claim`, and nowhere else** (§6: "lazy lease
reaping confined to `next`/`claim`; reads never write"). `next --run` may write (it
reaps expired step leases, returning them to ready with `attempt++`). Every read
verb — `step show`, `step context`, `run status` — computes effective status and
writes nothing, exactly as v6 established for issues.

**`max_step_duration`** (§11.1 `[limits]`) is a schedule-to-close bound evaluated
against `started_ms`, independent of heartbeats — "so a runaway executor cannot
renew forever." A step past it is reaped by `next` even with a live heartbeat.

### 6.3.1 Mode switching — the byte-identical guarantee

**`docket next` without `--run` is the existing issue-mode verb, byte-identical**
(binding repo fact). Structurally:

```go
func runNext(cmd *cobra.Command, args []string, w *output.Writer) error {
    if runRef, _ := cmd.Flags().GetString("run"); runRef != "" {
        return runNextSteps(cmd, runRef, w)   // new: step mode
    }
    return runNextIssues(cmd, args, w)        // existing body, moved verbatim
}
```

The existing function body is **moved, not edited** — the phase-3 commit's diff on
that code is a pure relocation, which review can verify at a glance. Every existing
`next` flag keeps its meaning in issue mode. In step mode, `--status`/`--priority`/
`--label`/`--type` are refused with `VALIDATION_ERROR` naming `--run` as the
conflict, rather than silently ignored: an issue-mode filter that silently does
nothing in step mode is a filter a dispatcher will trust and be wrong about.

The proof is `TestNextIssueModeByteIdentical` plus QA section X (`test_x_next.sh`)
passing **untouched** — an existing section that requires no edit is the strongest
available evidence that behavior did not move.

## 6.4 `next` row and claim wire shapes (§11.4, verbatim)

```
next row        { step, issue, run, executor, class, attempt, expected_cost,
                  lease_ttl_s, metadata }
claim response  { step, token, lease_expires_ms, context }
context         { step: <next row>, issue: {id, title, body_snapshot, kind, labels,
                  scope}, inputs: [{artifact, kind, producer_step, body}],
                  pins: [{path, sha256}], loop_entry, metadata }
```

Implemented field-for-field, with these bindings:

- `step` is the rendered instance identity (`STEP-N` for the id; the `instance`
  string appears as `instance`). **Nominal deviation, recorded — see §6.4.1.**
- `issue`/`run` are `DKT-N`/`RUN-N` formatted ids.
- `lease_ttl_s` is **seconds** (the spec's `_s` suffix), resolved from `[limits]`
  per class then `docket config` `lease.ttl.<class>` then `lease.ttl.default`.
- `metadata` is the definition's opaque KV, verbatim.
- `loop_entry` is the ordinal `k` and the routing that entered it, `null` at `k=0`.
- `pins` carries **both** workflow and file pins as `{path, sha256}`; a workflow pin
  renders its `path` as `workflow:name@version`, since §11.4 gives pins one shape.

**`docket step context STEP-N` re-emits `context` read-only, no token required**
(§11.4), and **`--meta` reports per-section byte counts** — the closure-size record
of engine-core §8. `--meta` is implemented as a sibling object, not a mutation of
`context`, so the golden bundles (§8.3) are unaffected by asking for it.

### 6.4.1 DKT-14 closes here

The S2 claims TDD recorded a nominal field-name deviation: §11.4 names the claim
response's subject key `step`, but S2 landed claims on **issues**, where it is
`issue`. That was filed as **DKT-14**.

**This stage lands `step claim`, whose subject key is `step` exactly as §11.4
specifies, and whose response carries the full `context` bundle.** The spec's shape
is therefore satisfied verbatim at the step level, which is the level it was written
for. DKT-14's proposed spec note — that the subject key names the leased entity, and
`context` is present only where a context bundle is defined — is now demonstrated by
two concrete implementations rather than one. **DKT-14 is closed by this stage**,
and the phase-3 commit that lands `step claim` closes it, citing this section.

## 6.5 Scope non-overlap (R4)

Scope is a list of path **globs** (`issues.scope_globs`, §5.1). Two steps conflict
iff their issues' scope globs intersect. Rules, each a test case:

| # | Rule |
|---|---|
| S1 | `scope_globs` NULL or `[]` ⇒ the issue **never excludes and is never excluded** (engine-core §5: "Scope-less issues declare `scope = []` and never exclude") |
| S2 | two globs intersect iff some path could match both; implemented conservatively — a false "they intersect" costs scheduling parallelism, a false "they don't" costs a race |
| S3 | exclusion is against `claimed` and `running` steps only; a `done` or `pending` step excludes nothing |
| S4 | exclusion is symmetric and computed fresh per `next` call; nothing is stored |

**S2's conservatism is the design decision.** Exact glob-intersection is undecidable
in general for arbitrary patterns; the implementation decomposes each glob into a
literal prefix and a wildcard tail and reports intersection when either prefix is a
prefix of the other. `internal/db/**` and `internal/db/leases.go` intersect;
`internal/db/**` and `internal/cli/**` do not; `**/foo.go` intersects everything
(empty literal prefix), which is correct and conservative. The direction of the
error is chosen deliberately and stated in the code comment: over-reporting delays a
step, under-reporting corrupts a working tree.

**Serialized writers.** engine-core §5 ratifies "at most one write-class step is
claimable at a time." At S3 this is **not** a hardcoded rule — it is R5 with
`[limits]` `"write" = { max = 1 }`, which is exactly what §2 says ("the reference
instance's config sets its write class to 1 — serialization is *instance policy*,
not core behavior"). Core ships no class named `write` and no default of 1. A
hardcoded writer serialization would be an instance policy in core, failing the
genericity rule on its face.

## 6.6 Claim, and the context bundle's determinism

`step claim STEP-N` is one CAS transaction, reusing v6's `claimPredicate`
(`owner IS NULL OR owner = '' OR expires_ms <= ?`) against `steps`. The v6 helpers
in `internal/db/leases.go` are **generalized over a table name**, not copied: a
`leaseTarget{table string}` value parameterizes the existing SQL, and
`ClaimIssue`/`HeartbeatIssue`/`ReleaseIssue` become thin wrappers over the shared
implementation. `authorizeHolder`'s three-way refusal (ErrNotHolder / ErrLeaseExpired
/ ErrLeaseHeld) is reused unchanged — which is why the S3 refusal matrix (§6.9) is
the S2 matrix with step verbs substituted, and why it cannot drift between the two
entities.

The claim response returns token **and** context in one response — "one atomic
mediation: an unclaimed executor has nothing, a claimed one has everything"
(engine-core §8).

**Context assembly is pure and snapshot-pinned** (engine-core §8; §9 item 5). It
reads, and may read, exactly four sources:

1. the step row and its definition, from the **pinned** workflow's `parsed`;
2. `run_issues.body_snapshot` — never the live issue body;
3. `run_issues.issue_snapshot` — `title`/`kind`/`labels`/`scope` as of activation
   (§5.1.1), never the live `issues` row;
4. recorded `artifacts` bodies, resolved per §6.7's input rule;
5. `pins` rows (path + hash) — the *list*, not the file contents.

It never reads the working tree, never re-reads a file a pin names, and reads **no
live issue field at all** — `id` is the `run_issues` key and every other member of
§11.4's `context.issue` comes from a snapshot column. The scheduler's live read of
`issues.scope_globs` (§6.3 R4) is not context assembly and is deliberately excluded
from this list (§5.1.1). `issue.diff` is served from the artifact snapshotted and
fingerprinted when its producing step completed (§6.7.1), never computed live.

A test enforces this at the code level: `TestContextAssemblyReadsNoLiveState` runs
assembly with the issues table mutated and the working tree mutated between two
calls and requires **byte-identical** output. The golden-bundle test (§8.3) is the
same property at the CLI level.

`claim --render` returns the assembled packet instead, atomically (§2).

**A read-back replays what the claim recorded** (DKT-1054). Source 4 is resolved
over run state, and run state keeps moving after a step is handed out: the
fixture's `fix@1` binds `reconcile@0` at claim, then `review@1`, `synthesize@1`,
and `reconcile@1` complete at fix@1's own ordinal, and a live re-resolution binds
`reconcile@1` — an artifact produced by reviewing fix@1's diff. The claim writes
the bindings it handed over to `step_inputs` (§6.1) in its own transaction, and
`step context`, `step render`, and `step show`'s target ref read a claimed step
(`attempt > 0`, not back at `pending`) over exactly that set, through the same
resolver, so the read-back is the claim-time bundle however far the run has moved.
A re-claim (a reaped lease, `resolve --as retry`) records its own bindings in place
of the last attempt's. A step not yet handed out — every `action`, `human`, and
`vote` step, and an executor step still `pending` — reads live, since the claim
that will hand it out is what a read of it previews; `step context --live` asks
that question of any step.

## 6.7 Input resolution

§2, verbatim: "Downstream `inputs` resolve over siblings that RECORDED their work
— `done`, and `superseded` — ordered by (declared position, sibling index, artifact
id)." `superseded` is admitted because `resolve --as fix-round` supersedes the parked
instance that asked the question, and the operator authorized the round on the strength
of what that instance emitted; excluding it made `ordinalScoped` fall back a round in
silence (DKT-375). The loop's own supersede sweep takes only `pending` instances, which
own no artifact, so admitting them costs nothing. Rules:

| Form | Resolves to |
|---|---|
| `<step>.<kind>` | artifacts of that kind from `done`/`superseded` instances of that step |
| `<step>.*` | all artifacts from `done`/`superseded` instances of that step |
| `issue.body` | the activation-time body snapshot |
| `issue.diff` | the engine-computed VCS diff artifact for the issue's scope, snapshotted and fingerprinted when its producing step completed (git in v1 — the one declared VCS coupling, engine-spec §7); which step produces it is pinned in §6.7.1 |

Ordering is `(declared position, sibling index, artifact id)` — **never event
order** (§11.3 (3) says so explicitly for loops; §2 says so for fanout joins). A
test shuffles artifact insertion order and requires identical resolution, since
"ordered by artifact id" is trivially satisfiable by accident when insertion order
happens to match.

`issue.diff` is computed by shelling out to git **outside any transaction**, at the
producing step's completion, and stored as an artifact with its fingerprint. That is
the only VCS coupling, and it is the one engine-spec §7 declares.

### 6.7.1 Which step produces `issue.diff`

§11.1 says `issue.diff` is "snapshotted and fingerprinted when its producing step
completed" but never says which step that is — and it must be a stated rule, because
the fixture has two candidate producers (`implement` and `fix`, both write-class) and
two consumers (`review` and `verify`) whose bundles are golden-diffed for byte
identity (§8.3). "Whichever step happened to write last" is not a rule.

**The rule: `issue.diff` is recomputed at the completion of every *executor* step of
that issue, and consumers resolve the latest `done` producer per the §7.4 fallback.**
Precisely:

| # | Clause |
|---|---|
| D1 | On the routing stage (§6.8, stage N+1) of any step whose class is `executor` — including fanout siblings and `loop = true` instances — the engine computes the issue's diff over its **snapshotted** scope (§5.1.1) and records it as an artifact of kind `issue.diff`, attributed to that step instance, with its SHA-256 fingerprint. The computation is a git subprocess and therefore happens **outside** the transaction, before it opens (§6.8's no-subprocess rule); the artifact insert is inside it. |
| D2 | `action`, `human`, and `vote` steps produce no diff — they change no tree. Recomputing after them would produce a byte-identical artifact and a spurious ledger entry. |
| D3 | A consumer's `issue.diff` input resolves to the **highest-ordinal, then highest-id `done`-attributed** `issue.diff` artifact for that issue at the moment the consumer's context is assembled — which is exactly §7.4's ordinal-scoped rule with its per-input fallback, applied to a kind the engine produces rather than a step does. No special case. |
| D4 | If no `issue.diff` artifact exists yet (a consumer downstream of no executor step — legal, e.g. a workflow whose first step consumes `issue.diff`), the input resolves to an **empty diff artifact** recorded at activation, not to an error and not to a live `git diff`. Assembly reads no live state (§6.6), and "empty" is the truthful answer: nothing has changed the tree in this run. |
| D5 | The diff (`GitDiff`, `internal/engine/saga.go`) always names an explicit `dir` (the step's recorded `--worktree`, falling back to the run's own exec root — never the bare process cwd) and an explicit `base` (the run's `commit_sha`, PINNED once at `docket run start` and stored on the `runs` row — never a live HEAD read taken at record time, and never trusted from a caller-supplied string). A run started before `commit_sha` was recorded, or outside a checkout, falls back to a live read of the run's own exec root. |

D5 closes the hazard this section originally recorded — a CONDUCTOR (or executor) recording a step on behalf of work done in a private worktree capturing the WRONG tree, silently — in two layers. First, `dir` is read from the step's own recorded worktree rather than the invoking process's cwd, so a private-worktree executor's tree is the one diffed regardless of where `step complete`/`step record` happens to run. Second, `base` is the commit the RUN pinned at its own start, not a live read of any checkout's current HEAD: a live read drifts with the checkout it reads, either erasing the diff (the shared checkout fast-forwards onto this step's own commit) or attributing a sibling issue's unrelated commits to this step as deletions (the checkout advances past the fork point on other work in the meantime) — a pinned commit predates every step's work by construction and never moves. Both `dir` and `base` resolve to the same values as before whenever there was never a separate worktree, so this changes nothing for the ordinary, no-worktree recording case.

D1's "every executor step" is deliberately broader than "the last one": it makes the
artifact trail complete (the ledger attributes every instance — §11.3) and makes D3's
resolution a lookup rather than a judgment. The cost is one git invocation per
executor completion, which is the same order as the gates already running there.

This is what makes the fixture's `fix@1 → review@1` cycle correct: `review@1` resolves
`issue.diff` to the artifact `fix@1` produced, not the one `implement@0` produced,
because ordinal 1 beats ordinal 0 under D3 — without any rule specific to loops.

## 6.8 The complete saga

§2, verbatim, is the specification:

> `complete` is the specified saga — artifact+payload validation → gates one-by-one
> → routing — with panel-hardened semantics: **the token retires when the artifact
> records**; from that commit the saga is engine-owned and resumes lazily under any
> later engine invocation, each stage its own transaction, no subprocess ever inside
> a transaction, every stage commit refreshing the step's activity clock.

Stages, each **its own transaction**, each recording `saga_stage` as its resume
point:

| Stage | `saga_stage` | Transaction contents | Token |
|---|---|---|---|
| 0 | `null` | authorize holder; validate artifact size/shape; validate payload (**shape only at S3** — §6.14; the schema register is S5's) | **required** |
| 1 | `recorded` | insert artifact; **retire the token** (clear `owner`/`token_hash`/`expires_ms`); status → `gated`; refresh `activity_ms` | last stage requiring it |
| 2..N | `gate:<name>` | one gate: write `gate-started` event, run it **outside the transaction** (S3: pass-through, §5.6), then commit the result into `gate_trail`; refresh `activity_ms` | **not required** — engine-owned |
| N+1 | `routing` | resolve routing; update step, issue mirror, run, events — **one transaction spanning all four** (§2) | not required |
| — | `null` | saga complete | — |

**The issue mirror is LIVE, not only terminal.** Before DKT-294, stage N+1's "update
... issue mirror" meant exactly two writes — `promoteIssueTx`'s `backlog -> todo` at
activation, and `completeIssue`'s `-> done` here — and `in-progress`/`review` were
validated and rendered but never written. Now the mirror covers the full range:
claiming an issue's first step flips it `todo -> in-progress`, in CLAIM'S OWN
transaction (a claim never reaches this stage — nothing routes yet); stage N+1 that
leaves ANY step of the issue `waiting-human` flips the issue `-> review` — the
predicate is the STATUS, not the step's kind, so a gate, a vote and a step parked by
its own threshold all count; an OPEN HOLD counts as a second shape (`enterHeld` calls
the mirror directly, because H8's whole point is that the hold path does NOT route: a
materialized held step is minted `type=human`/`pending`, never `waiting-human`, and
its routing step stays `gated`, so without this the entire hold path read
`in-progress` — the status for work in flight, of which a hold has none). A DECLARED
`type=human`/`type=vote` step whose TURN HAS COME is the third shape (DKT-334): an open
gate awaiting `approve|reject`, or an open vote proposal, for the whole window it is
open. Neither writes a status when it opens — the operator's decision is what moves the
row — so waiting for one meant the issue read `review` only after the window had
CLOSED.

Separating "its turn has come" from "its predecessors are still running" is R3, so the
mirror asks the Scheduler (`AwaitingDecision`) rather than rewriting the join rules
beside itself. It asks for R2, R3 and R3's two interposition clauses and NOT for R1,
R4, R5 or R7: those are questions about scheduling executor work, and a gate nobody
claims holds no tree, consumes no class or budget headroom, and is outstanding against
a human whether or not the run is paused. Affordability comes from an indexed count of
candidate rows ahead of it — a workflow declaring no gates never loads a scheduler, and
one that does never loads one at an ordinal before the gate's turn. The mirror
recomputes on every routing transaction, and a gate's turn coming is always CAUSED by a
predecessor routing, so the flip lands in the same transaction as the cause. The one
window this does not reach is a gate at ordinal 0 with no predecessors: nothing routes,
so nothing recomputes, and the issue has not left `todo` for the mirror's live range
anyway; and the return trip is bidirectional and computed fresh
on every stage-N+1 transaction — the issue leaves `review` the moment none of its
steps is `waiting-human` any longer, which a retry or a resolution does and a loop
entry does not (§7.3 (2)'s supersede sweep takes `pending` instances only, so a park
survives into the next round).

The `review -> in-progress` RETURN records an `issue-in-progress` event and drops no
comment. That is deliberate and the docs previously claimed otherwise: the trail
narrates what a human is being asked to decide, and "you are no longer being asked" is
the absence of a question rather than a new one. A `--as fix-round` resolution is the
exception — it comments unconditionally, because another round is itself a new fact
(DKT-334 AC4).

ABANDONMENT is the third way out of `review`, and the only one that does not route
(DKT-377). Both `abandon-issue` (`abandonIssue`, reconcile.go) and `run abandon
--issue` (`AbandonIssueInRun`, run_lifecycle.go) terminalize every remaining step of
the issue in one cascade — `waiting-human` is not excluded — so the park is cleared
with no stage-N+1 transaction left to recompute the mirror. Each therefore does the
release itself, via `releaseAbandonedIssueTx`: `review`/`in-progress` -> `todo`, the
status the run found the issue in. `todo` is not a verdict — the fact is carried by
`resolution = abandoned`, the `issue-abandoned` event, and the trail comment — but it
is what keeps the issue inside `[backlog, todo]`, the ready-set filter both
internal/cli/next.go and internal/planner/plan.go apply, so the triage the routing
defers to the operator is triage they can actually reach. The routing path also
RETURNS before `issueStepsComplete`: its own cascade makes that check true by
construction, and `completeIssue` guards on nothing but `status != done`, so an
abandoned issue used to render "✔ done".

Each of these
writes, and the two pre-existing ones, drops a short engine-authored comment on the
issue narrating the transition (via DKT-293's tx-safe writer, in the same
transaction) — `AbandonIssueInRun` included, which dropped none until DKT-377.

**Token retirement at artifact record is the hinge.** Before stage 1 commits, a
non-holder cannot record (AUTH_ERROR) and a stale holder is refused (STALE_LEASE).
After it commits, the step needs no lease at all: any later engine invocation
(`next`, another `claim`, `step show --resume`) advances the saga from
`saga_stage`. This is what makes crash-at-any-boundary safe (§9 item 10) — there is
no lease to lose and no owner to wait for.

**Resume is lazy and idempotent.** Each stage's transaction is `WHERE saga_stage =
<expected>` CAS-guarded, so two concurrent engine invocations resuming the same saga
produce exactly one advance; the loser's UPDATE matches zero rows and it re-reads.

**No subprocess inside a transaction** (§6) is structural: the gate stage commits
its `gate-started` event, **closes the transaction**, invokes the runner, then opens
a new transaction to record the result. At S3 the runner does nothing, but the
transaction boundaries are already where S4 needs them — which is the entire point
of landing the saga now.

`step fail` is the explicit-failure counterpart: it consumes an attempt and routes
per `on_fail` when attempts are exhausted, per the status machine.

## 6.9 Refusal matrix (§9 item 3, at step level)

The S2 matrix with step verbs substituted — same helper, same codes, so it cannot
drift:

| # | Situation | Verb | Code | Exit |
|---|---|---|---|---|
| R1 | no token supplied | heartbeat/complete/fail | `VALIDATION_ERROR` | 3 |
| R2 | token supplied, step unclaimed | heartbeat/complete/fail | `AUTH_ERROR` | 5 |
| R3 | token supplied, wrong value | heartbeat/complete/fail | `AUTH_ERROR` | 5 |
| R4 | correct token, lease expired | heartbeat/complete/fail | `STALE_LEASE` | 6 |
| R5 | claim against a live lease | claim | `CONFLICT` | 4 |
| R6 | N concurrent claims on one ready step | claim | 1 × exit 0, N−1 × `CONFLICT` | 4 |
| R7 | claim against an expired lease | claim | **succeeds**, `attempt++` | 0 |
| R8 | claim a step that is not ready (R1–R7 of §6.3 unmet) | claim | `CONFLICT` naming the unmet condition | 4 |
| R9 | `complete` after the token retired (saga past stage 1) | complete | `AUTH_ERROR` | 5 |
| R10 | `approve`/`reject` on a non-`human` step | approve/reject | `VALIDATION_ERROR` | 3 |
| R11 | `resolve` on a step not in `waiting-human` | resolve | `VALIDATION_ERROR` | 3 |
| R12 | artifact exceeding 1MiB | complete | `VALIDATION_ERROR` | 3 |

**R8 is new relative to S2** and it matters: `claim` must enforce readiness itself,
not trust that the caller ran `next`. A dispatcher racing a scope conflict would
otherwise claim a step `next` would never have offered. The refusal names which
condition failed, so a stalled dispatcher can diagnose itself.

**R9 is the token-retirement proof.** A worker that completes twice — a plausible
retry after a dropped response — gets `AUTH_ERROR` on the second call rather than
recording a duplicate artifact.

## 6.10 The step verb surface

| Verb | Token | Effect |
|---|---|---|
| `docket step claim STEP-N [--render] [--template F]` | no | CAS claim; mints token; returns token + context (or packet) |
| `docket step heartbeat STEP-N` | yes | extends the lease; does not touch `attempt` |
| `docket step complete STEP-N --artifact-file F [--payload-file F] [--usage '{…}'] [--metadata '{…}']` | yes (stage 0–1) | the saga |
| `docket step fail STEP-N [--note …] [--metadata '{…}']` | yes | records the failure; routes per `on_fail` when attempts are exhausted (the counter is bumped by CLAIMS, not by `fail` — E-8) |
| `docket step approve\|reject STEP-N [--note …]` | no | `type=human` gate steps **only** (§2) |
| `docket step resolve STEP-N --as retry\|skip\|abandon-issue\|override-pass [--note …]` | no | `waiting-human` resolutions (§2); `retry` **resets attempts** |
| `docket step show STEP-N` | no | read-only; effective status |
| `docket step list (--run RUN-N \| --issue ISSUE-N)` | no | read-only; steps with id, run, instance, issue, kind, effective status, attempt, expected_cost (DKT-54: step ids are a store-wide sequence, so nothing else enumerates a run). `--issue` lists one issue's steps across every run holding one, since a re-activation mints a fresh round under a new run (DKT-244) |
| `docket step context STEP-N [--meta]` | no | re-emits `context` read-only (§11.4) |
| `docket step render STEP-N [--template F]` | no | context bundle → rendered work packet |
| `docket step artifacts STEP-N` | no | read-only; lists what the step PRODUCED — reference, kind, size, hash, never bodies |
| `docket step artifact ARTIFACT-N [--payload]` | no | read-only; one artifact in full, body and payload |

**`step artifacts` / `step artifact` are the OUTPUT side of the read surface**
(DKT-17). `step context` re-emits what a step CONSUMED; until these landed,
nothing re-emitted what it PRODUCED. That gap was load-bearing rather than
cosmetic: an action step's verdict and an aggregate's held-cluster payload are
both artifacts, so reading either meant opening `.docket/issues.db` with
`sqlite3` — which is what RUN-1's conductor had to do.

The split is deliberate. A listing reports sizes and no bodies, because
`ArtifactMaxBytes` is 1MiB and an inlined body would make the listing
unreadable exactly when a step produced something large. A fetch returns one
artifact whole. `--payload` narrows to the structured half and, under `--json`,
emits it as parsed JSON rather than a string, so a verdict pipes into `jq`
without the caller first extracting a document out of a string field.

A step that produced nothing lists nothing and exits 0 — producing no artifact
is ordinary. A step that does not EXIST is `NOT_FOUND`, so a mistyped reference
never reads as a fact about a step that is not there.

`--usage` is accepted, stored, and **enforces nothing** until S6 — same reasoning as
`--budget` (§5.2): the wire shape is §11.4's and it lands whole.

**`step resolve --as retry` resets attempts** (§2: "retry = attempts reset"). This
is a different counter from the issue-level `attempt` that v6 declared monotonic —
claims-leases §5 anticipated exactly this ("Stage 3's `step resolve --as retry`
resets attempts *for a step instance*; that is a different counter on a different
entity"). The step's `attempt` is a live retry budget against `max_attempts`; the
issue's is a permanent trail. Both statements stay true.

*Amended (DKT-86, DKT-90, schema v16):* what resets is the **budget**, not the
column. The step's `attempt` is also the usage ledger's key half
(`UNIQUE(step_id, attempt, unit)`, v10), and zeroing it made a retried step's
re-execution claim under an attempt number whose ledger slot was already taken —
the second execution's usage was permanently unrecordable through
`dispatch backfill-usage`. `attempt` is now monotonic for the step's whole life
("claims made against this step, ever", §11.4's own wording), retry moves
`steps.attempt_base` to the current attempt, and exhaustion compares
`attempt - attempt_base` against `max_attempts`. The budget still reads
zero-spent after every retry, so §2's sentence keeps its meaning.

## 6.11 Rendering (§2)

`step render` formats the context bundle into a work packet through a template:

- **shipped generic default**, embedded under `internal/workflow/templates/`
  (Vorpal include-list constraint again);
- or `--template F`, a file path — which activation may pin, so the packet is
  reproducible.

The default template is `text/template` over the context bundle's fields, emitting
framing (run/issue/step ids, scope), the input artifacts each delimited and labeled
in declared order, the pinned-file list with hashes, and the output instruction
(artifact kind to emit; payload schema name if any). **Packet layout is core
mechanics; packet content is instance data** (§2) — the default template names no
instance concept, and the genericity gate (`scripts/qa/genericity.sh`, §9) checks its
bytes like any other
surface.

`claim --render` returns the packet instead of the bundle, in the same atomic call.

### 6.11.1 A pinned template's bytes are verified at render

`--template F` reads a file **at render time**, but activation may have pinned that
path (§5.3 stage 3). Without a check, a post-activation edit to the template silently
changes every packet the run renders from then on, while `step context` stays
byte-identical — so the §9-item-5 determinism goldens keep passing and the thing
actually handed to a worker has changed. That is the one hole in the reproducibility
story, and it closes here:

**When the template path is pinned, `render` hashes the file it just read and
compares against the `pins` row. A mismatch is a refusal — `CONFLICT` (exit 4),
naming the path, the pinned hash, and the hash on disk.** Never a warning, never a
silent re-pin.

| Case | Behavior |
|---|---|
| path pinned, bytes match | render proceeds |
| path pinned, bytes differ | `CONFLICT` (exit 4), both hashes named; nothing rendered, nothing written |
| path pinned, file now absent | `NOT_FOUND` (exit 2) — the pin says the run depends on it |
| path **not** pinned | render proceeds unverified; the packet is reproducible only to the extent the operator chose. `--meta` reports the template as unpinned so the gap is visible rather than assumed |
| the shipped default (embedded) | always reproducible — it ships in the binary, so there is no file to drift (§6.11) |

The refusal is `CONFLICT` and not `VALIDATION_ERROR` because the request is
well-formed; the state disagrees with the pin. That is the same reading as a
re-register with differing bytes (§4.5) and an `--if-version` mismatch, so the
taxonomy stays consistent and no new code is introduced. `TestPinnedTemplateDriftIsRefused`
edits a pinned template between two renders and asserts the second refuses — the same
shape as §8.3's golden-sensitivity check, and for the same reason: a verification
that cannot fail is not a verification.

## 6.12 `guard stop|gate`

§10 puts `stop` and `gate` at this stage. Both are deterministic predicates over
engine state: **exit 0 allow / exit 2 deny with reason** (§2).

| Guard | Allows when |
|---|---|
| `docket guard stop` | no pending work outside `waiting-human` — i.e. no step in `pending`/`ready`/`claimed`/`running`/`gated` for any active run |
| `docket guard gate --step NAME` | an **approved** `type=human` step of that name exists for the active run |

Exit 2 collides numerically with `NOT_FOUND`'s exit 2, and that is **intentional and
specified**: §2 defines the guard contract as "exit 0/2 + reason", independent of
the error taxonomy, because guards are hook predicates whose caller tests a boolean,
not a CLI verb whose caller maps a code. The reason goes to stderr in human mode and
into the JSON envelope's `error` under `--json`. This is recorded here because a
reviewer will otherwise read it as a taxonomy violation; it is the spec's own
contract, quoted.

`guard spawn|record` are **not** here — §10 assigns them to stage 6.

## 6.13 The action seam (S5 boundary), specified

Gates got §5.6. **Action steps get the same treatment here**, and for the same
reason: the fixture's `reconcile` is an `action` step sitting on the QA loop's
critical path (§7.7), while the computation it names — the builtin `aggregate` — is
S5's by the §1 scope table. Without a specified seam, the group-3 or group-4 session
improvises one mid-implementation.

**At S3, everything about action steps is real except the computation.**

| Element | S3 | S5 adds |
|---|---|---|
| `action` parsed, validated, stored in `parsed`; `params` carried opaquely | real | — |
| the step expands, schedules, and is claimable like any other step (§5.3.1, §6.3) | real | — |
| produced artifact **kind** = `params.output` (§4.3.1) | real | — |
| the artifact's **payload** | empty (`[]`), marked `stub: true` | the computed aggregation — per-cluster value, members, held flag, `demoted_from`, `operator_resolved` (§2) |
| payload validation against `aggregate@1` | shape only (§6.8 stage 0) | the registered schema |
| `hold_spread` materializing a `<step>-held` `type=human` step | **not** at S3 — `params` is opaque, nothing reads `hold_spread` | the materialization (§2) |
| the saga, gates, routing over the action step's result | real | — |
| threshold evaluation over its payload | real, per §6.14 — including over an empty payload set, which §6.14 T4 pins | evaluation over computed values |

The seam is a single Go interface, mirroring `GateRunner` exactly:

```go
// ActionRunner computes one action step's payload. S3 ships stubRunner; S5 ships
// the real one (aggregate and any later builtin). The saga is written against the
// interface, so S5 changes one constructor call and nothing else.
type ActionRunner interface {
    Run(ctx context.Context, a ActionSpec, sc StepContext) (ActionResult, error)
}
```

`stubRunner` returns an artifact of kind `params.output`, **empty payload**, with
`stub: true` recorded on the artifact row, and — like `passThroughRunner` — **never
touches the process table**. An action step at S3 therefore completes normally,
records a real artifact under a real kind (so `inputs` resolution against it works,
which is what the fixture's `fix` step needs), and contributes an empty payload set
to any threshold evaluated over it.

Three consequences are asserted as tests, exactly as for the gate seam: (a) an action
step's artifact is present, addressable, and of the declared kind; (b) `stub: true`
appears on every S3 action artifact, so an operator can tell a stubbed computation
from a real one; (c) the QA section carries a comment saying a green action step is
**not** evidence the computation works, so nobody reads phase 4's loop run as
`aggregate` coverage.

**`params.output` missing on an `action` step is a `VALIDATION_ERROR` at register**
— it is the step's produced kind (§4.3.1), so a step without it can never satisfy V11
for a downstream consumer. This is V5's neighbourhood but a distinct check; it rides
in the V11 case set, since V11 is where the produced-kind table is enforced.

## 6.14 Threshold predicate evaluation at S3, exactly

§11.2 gives the grammar (`agg(field op literal)`, `agg ∈ {any, all, count>=n}`,
`op ∈ {==, !=, >=, >, <=, <}`) and one hard restriction: **ordered comparisons are
defined only for fields whose registered schema declares `ordered_enum`.** Schemas
register at S5. So S3 can parse every predicate (V21) but cannot legally *evaluate*
every predicate, and what it does with the ones it cannot is a decision that must be
pre-made — the fixture contains one (`any(severity >= high)`).

**The rule, in four clauses:**

| # | Clause |
|---|---|
| T1 | **Equality operators evaluate at S3.** `==` and `!=` compare the payload field's value to the literal as strings, after JSON scalar normalization (numbers canonicalized, booleans `true`/`false`, `null` never equal to anything including `null`). No schema is required to know whether two values are the same value. |
| T2 | **Ordered operators (`>=`, `>`, `<=`, `<`) do not evaluate at S3.** A predicate using one is parsed and stored, and its *attempted evaluation* is what T3 governs. |
| T3 | **An ordered comparison actually attempted against a schema-less field routes `waiting-human`, with a recorded reason.** The step's `routing` is `waiting-human`, the reason string names the step, the routing key, the predicate verbatim, and the cause — e.g. `threshold "fix-loop" on step reconcile@0 requires an ordered comparison (severity >= high); ordered comparisons need a registered ordered_enum schema (engine-spec §11.2), which stage 5 supplies`. The engine **never guesses an order** — not lexicographic, not enum-declaration order, not "high > medium because someone will assume so". A guessed order is a silent misroute, which is strictly worse than a park an operator can see and resolve with `step resolve`. |
| T4 | **Evaluation over an empty payload set is defined, and it is the ordinary mathematical convention**: `any(...)` over zero payloads is **false**; `all(...)` over zero payloads is **true**; `count>=n(...)` over zero payloads is true iff `n == 0`. This is evaluated *before* T3 — an `any()` over an empty set is false without ever attempting the comparison, so no `waiting-human` park occurs. |

**T4-before-T3 is the clause that makes S3 runnable, and it is not a convenience
hack.** `any(P)` asserts "some element satisfies P"; with no elements there is no
witness, so it is false, whatever P is — the predicate is never consulted. `all(P)`
asserts "no element violates P"; with no elements there is no violation, so it is
true. Short-circuiting an aggregation over an empty set before touching its predicate
is what every implementation of `any`/`all` does; stating it here just means the S3
behavior is derived, not invented, and stays correct verbatim when S5 supplies real
payloads.

The consequence for the fixture, spelled out because phase 4's QA depends on it: at
S3 `reconcile` is an action step whose stub payload is empty (§6.13), so its
`threshold = { "fix-loop" = "any(severity >= high)" }` evaluates `any` over zero
payloads ⇒ **false** by T4, never reaching the `>=` that T3 would park on ⇒ no
threshold matches ⇒ §11.2's "no match ⇒ pass". The run flows through `reconcile`
legally, without the engine having guessed anything about severities.

**T3 is still reachable and is still tested** — a harness-crafted payload set with
one non-empty element and an ordered predicate parks the step in `waiting-human` with
the reason string above. Its being unreachable *in the fixture* is a property of the
fixture, not of the rule, and the test does not rely on the fixture to prove it.

**Where S5 upgrades this**: T1 gains schema-typed comparison, T2/T3 become live
ordered comparisons for `ordered_enum` fields (T3 survives, applying only to fields
that are *still* schema-less at S5), T4 is unchanged. Field *existence* validation
against the payload schema at register time is S5's per §1's scope table; V21 at S3
validates grammar only.

## 6.15 Human and vote steps: the lifecycle, pinned

`type="human"` and `type="vote"` steps never claim, never mint a token, and never run
the saga (§6.8) — the saga's stage 0 authorizes a holder, and these steps have none.
Their path through the §6.2 machine is short, and it is stated here because §6.10
gives only the verbs:

| Step class | Path | Terminal |
|---|---|---|
| `type="human"` | `pending` → `ready` (computed, §6.3 — R1/R2/R3/R6 apply; R4/R5 do not, since a human step claims no scope and consumes no class headroom) → **`approve`** ⇒ `done`, or **`reject`** ⇒ routed per effective `on_fail` (§4.3.2) | `done`, or whatever the routing produces (`fix-loop` re-enters the loop, `skip` ⇒ `skipped`, `abandon-issue` ⇒ the issue abandons) |
| `type="vote"` | `pending` → `ready` → **parks** at S3: no verb casts or tallies a vote until S4 (§1 scope table). A `ready` vote step is offered by `next --run` with `kind: "vote"` and is claimable by nothing; an operator may `step resolve --as skip\|override-pass\|abandon-issue` to move a run past one | S4 supplies casting/tallying; at S3 only operator resolution moves it |
| `action` *(added S5, DKT-28)* | `pending` → `ready` → **the engine runs it**: the saga computes the builtin (or spawns the trusted command), records the artifact, and routes — no claim, no token, no worker. Refusal names no verb, because none exists | whatever the routing produces |

**None of the three is offered as executor work.** `next --run` includes them (a
dispatcher needs to know the run is waiting on a human, and needs the engine's own
steps in the feed), but their rows carry no `executor` and a `claim` against them is
`CONFLICT` naming the step class — a dispatcher that spawns on every `next` row must
not spawn a worker for a gate. This is asserted as a test rather than left to the
reader.

**`action` joins the branch at S5** and the reason is D13's, not §6.15's original
one. Action steps are the engine's DETERMINISTIC HALF (AC-2); a claimable action
step is a relay a dispatcher would write to copy a predecessor's payload onto it,
which is reconcile.py reborn as a claim+complete shim. Its refusal therefore names
the ENGINE rather than a verb — "resolved by the engine, not by a worker" — because
offering a verb would invite exactly the shim.

**`type="vote"` execution is deferred to S4, explicitly** (§1 scope table). §2 drives
votes as gates ("`vote_rule`, which existing Docket threshold config tallies"), and
the gate machinery lands at §10 stage 4 — so splitting votes from gates would ship
half a feature. What S3 owns is V14 (register-time validation), expansion, and the
park; nothing about `voters`/`vote_rule` is interpreted here.

## 6.16 Test plan (phase 3)

**Go unit tests** (`internal/engine/ready_test.go`) — table-driven, `plan_test.go`
style: R1–R7 of §6.3 each independently falsified and satisfied; ordering by
priority then age with a total-order tie-break; per-class headroom with `[limits]`
`max`; `max_step_duration` reaping past a live heartbeat.

**Go unit tests** (`internal/engine/scope_test.go`): S1–S4, with the glob
intersection table (`internal/db/**` vs `internal/db/leases.go` vs `internal/cli/**`
vs `**/foo.go`) as explicit cases, including the conservative direction.

**Go unit tests** (`internal/db/steps_test.go`): the claim CAS against `steps`;
**the mutation test repeated for the step table** (rewrite the predicate to
`WHERE id = ?`, assert the concurrency test then fails — a guard that cannot fail is
not a guard); N-goroutine claim with exactly one winner; lease helpers shared with
issues proven by asserting both call the same function (no duplicated SQL).

**Go unit tests** (`internal/engine/saga_test.go`): **crash at every stage
boundary** (§9 item 10) — for each `saga_stage` value, kill after commit and resume,
asserting exactly-once advance; token retired exactly at stage 1; `complete` twice
⇒ `AUTH_ERROR` (R9); concurrent resumes ⇒ one advance; `activity_ms` refreshed by
every stage commit.

**Go unit tests** (`internal/engine/context_test.go`):
`TestContextAssemblyReadsNoLiveState` (§6.6), with `title`/`kind`/`labels`/`scope`
each mutated live and each asserted unchanged in the bundle (§5.1.1); input ordering
under shuffled insertion (§6.7); `issue.diff` served from the snapshot, not computed
live, and resolved per D1–D4 (§6.7.1) — including D3's highest-ordinal pick across a
loop and D4's empty-diff fallback.

**Go unit tests** (`internal/engine/action_test.go`, §6.13): the stub records an
artifact of kind `params.output` with an empty payload and `stub: true`; it spawns no
process (asserted the same way §5.6's gate stub is); an `action` step without
`params.output` fails register.

**Go unit tests** (`internal/engine/threshold_test.go`, §6.14) — one case per clause:
T1 equality over crafted payloads including JSON scalar normalization and the `null`
rule; T2/T3 an attempted ordered comparison parking `waiting-human` with the reason
string asserted by substring (step, routing key, predicate, cause) and **no** guessed
order in any direction; T4 `any`⇒false / `all`⇒true / `count>=0`⇒true over an empty
set, asserted **before** T3 can fire (an `any(x >= y)` over an empty payload set must
return false, not park — this is the exact case the phase-4 QA loop depends on);
first-match-wins ordering over a multi-routing table; no match ⇒ `pass`.

**Go unit tests** (`internal/engine/human_test.go`, §6.15): approve ⇒ `done`; reject
⇒ routes per effective `on_fail` and never `waiting-human`; both refuse a non-human
step (R10); `claim` against a `human` or `vote` step is `CONFLICT`; a `vote` step
expands, becomes ready, and parks with no verb able to advance it but
`step resolve`.

**Go unit tests** (`internal/cli/render_test.go`, §6.11.1):
`TestPinnedTemplateDriftIsRefused` — a pinned template edited between two renders,
second render `CONFLICT` (exit 4) naming both hashes; unpinned template renders
unverified and `--meta` says so; an absent pinned template is `NOT_FOUND`.

**Go unit tests** (`internal/cli/next_test.go`, extended):
`TestNextIssueModeByteIdentical`; step-mode filter refusal; the §6.9 matrix per verb.

**QA** (`test_zg_workflow.sh`, extended):
- Full cycle: activate → `next --run` → `claim` → `complete` → next step ready.
- **§9 item 4 in full**: kill a claimer mid-step (`kill -9`, no release), assert
  lease expiry **alone** re-readies the step with no operator action, the run
  completes, and the attempt trail is complete. The S2 QA proved the half of this
  that existed at issue level; this is the whole of it at step level.
- **§9 item 2 (partial)**: for a full run, every transition in the events table is
  attributable to `next`, a gate, a threshold, or human input — asserted by
  requiring every event row's `kind` to be in the closed set and every step status
  change to have a producing event. It is *partial* at S3 because gates are stubbed
  and thresholds are field-unvalidated; S4 and S5 complete it.
- The §6.9 refusal matrix by exit code **and** `.code`, each followed by a
  version-unchanged assertion (a refusal never writes).
- The claim race at step level: N background processes, one winner, `attempt == 1`.
- **Phase-3 dormancy** (§3): an activated run's issues read byte-identically under
  `--json=v1` and human mode.

**Goldens** (§8.3): the §9 item 5 byte-identical context bundles land here.

---

# Phase 4 — Loops, joins, guards' completion, and events written

Commit group 4. Closes the stage.

## 7.1 v7 DDL slice

```sql
CREATE TABLE IF NOT EXISTS events (
    seq       INTEGER PRIMARY KEY AUTOINCREMENT,   -- monotonic (§3)
    at_ms     INTEGER NOT NULL,
    kind      TEXT    NOT NULL,
    run_id    INTEGER REFERENCES runs(id) ON DELETE CASCADE,
    step_id   INTEGER REFERENCES steps(id) ON DELETE CASCADE,
    issue_id  INTEGER REFERENCES issues(id) ON DELETE CASCADE,
    data      TEXT    NOT NULL DEFAULT '{}'        -- opaque JSON
);

CREATE INDEX IF NOT EXISTS idx_events_run_seq ON events(run_id, seq);

ALTER TABLE run_issues ADD COLUMN loop_count INTEGER NOT NULL DEFAULT 0;  -- §11.3 (1)
```

`seq` is `INTEGER PRIMARY KEY AUTOINCREMENT`, which in SQLite is monotonic and never
reused even after deletion — exactly what §3's "events carry monotonic `seq`" and
S7's `--since` require. Using `AUTOINCREMENT` rather than plain rowid is the
difference between a `seq` that is monotonic and one that merely usually is.

**The loop counter is per-issue** (§11.3 (1): "the issue's loop counter"), so it
lives on `run_issues`, not on the step.

## 7.2 Loop semantics — §11.3, implemented clause by clause

The spec paragraph is normative; each numbered clause maps to an implementation
obligation and at least one test:

| §11.3 clause | Obligation |
|---|---|
| identity `name@k#i`; `k` = loop ordinal (0 at initial expansion), `#i` = fanout index, **absent when not fanned out** | `instance` rendering: `implement@0`, `review@0#2`, `fix@1`. A rendering test covers all four combinations of (ordinal 0/n) × (fanned/not) |
| (1) on `fix-loop` routing, the **issue's** loop counter increments; exceeding `max_fix_loops` routes `waiting-human` instead | `run_issues.loop_count + 1 > max_fix_loops` ⇒ the routing becomes `waiting-human`, event-logged with the reason. "Loops are bounded by construction" is a test that a workflow with `max_fix_loops = 2` cannot produce a third loop entry regardless of how many times routing fires |
| (1b) a round whose predecessor moved nothing in scope is refused as non-convergent (DKT-340) | The newest `issue.diff` fingerprint at ordinal ≤ k-1 equals the one at ordinal ≤ k-2 ⇒ the routing becomes `waiting-human`, counter restored, nothing superseded or instantiated — the bound's exact shape, because the reasoning is the same: another round cannot change what the next check measures. Compared UP TO an ordinal rather than AT it, because the engine suppresses an identical or empty `issue.diff` write (DKT-258/DKT-259), so a stalled round records no artifact rather than a matching one. Waived by `step resolve --as fix-round` (`EnterLoopAuthorized`) — without which the park would name a way out that its own guard closes, which is DKT-237's failure reproduced. Never fires when the compared diff `diffRecordsNoChange` — an empty body or one carrying only the unresolvable-base `#` marker says the tree was not measured, and parking a loop on that turns a broken diff setup into a stalled run |
| (2) not-yet-claimed instances **downstream of `after_loop`** transition to terminal **`superseded`** (event-logged); claimed/running instances finish, but routing from a superseded lineage is **inert** | the supersede sweep (§7.3) |
| (3) `loop = true` steps — excluded from ordinary expansion — instantiate at ordinal `k`, `inputs` bound **within ordinal `k`**, falling back to the **highest earlier ordinal** per input, ordered by (declared position, sibling index, artifact id) — **never event order** | the ordinal-scoped input binder (§7.4) |
| (4) when the loop body's gates pass, `after_loop` **and its downstream chain** re-instantiate at ordinal `k`; gates re-run; thresholds re-apply | re-expansion at ordinal `k` of the `after_loop` step and everything transitively `after` it |
| issue completion is evaluated over **highest-ordinal instances only** | the completion predicate ignores lower ordinals entirely |
| prior instances and artifacts remain **immutable and addressable**; the ledger attributes every instance | nothing is deleted or rewritten on loop entry; `superseded` is a status, not a deletion |
| "There is no other loop construct" | no other re-entry path exists; a `threshold` routing to a step name is an interposed gate (§11.2), **not** a loop, and does not touch `loop_count` |

### 7.3 The supersede sweep, exactly

On `fix-loop` entry at ordinal `k → k+1`, in the routing transaction:

1. compute the downstream set: the `after_loop` step and every step transitively
   `after` it, within the issue's bound workflow;
2. for every step **instance** of that set at ordinal ≤ `k` whose status is
   `pending` or `ready` (i.e. **not yet claimed**): set status `superseded`, write a
   `step-superseded` event;
3. instances in `claimed`, `running`, or `gated` are **left alone** — they finish.
   Their eventual routing is **inert**: the routing transaction checks whether the
   step's ordinal is below the issue's current `loop_count` and, if so, records the
   routing on the step for the ledger but applies **no** downstream effect (no
   supersede, no re-expansion, no issue status change, no loop increment);
4. `done`/`skipped`/`failed-routed`/`superseded` instances are terminal already and
   untouched.

**"Inert" is the subtle half of clause (2)**, and it is where a naive implementation
races: a slow `verify@0` completing after `fix@1` has started must not re-route the
issue based on stale ordinal-0 findings. The ordinal comparison is the whole guard,
and `TestStaleLineageRoutingIsInert` is its test.

### 7.4 Ordinal-scoped input binding

For a step at ordinal `k`, each declared input resolves:

1. among `done` instances of the named step **at ordinal `k`**;
2. if none, fall back to the **highest ordinal `< k`** that has a `done` instance
   ("falling back to the highest earlier ordinal per input" — §11.3 (3));
3. order by (declared position, sibling index, artifact id).

The fallback is **per input**, not per step: a `fix@1` step whose `inputs` are
`["reconcile.findings", "implement.change-summary"]` binds `reconcile.findings` at
ordinal 1 (fresh) and `implement.change-summary` at ordinal 0 (never re-run) — which
is exactly the committed fixture's `fix` step, and exactly why the clause says "per
input."

### 7.5 Fanout joins and `min_siblings` (§2)

| # | Rule | Source |
|---|---|---|
| J1 | a fanned-out step joins when **every** sibling is terminal (`done` \| `skipped` \| `superseded` \| `failed-routed`) | §2 |
| J2 | a sibling in `waiting-human` **parks the issue** | §2 |
| J3 | downstream `inputs` resolve over `done` siblings **only** | §2 |
| J4 | `on_fail` applies **per sibling** | §2 |
| J5 | `min_siblings` permits quorum joins: the join **still waits for all siblings** to reach a terminal state (**no early cancel in v1**), and if the `done` count < `min_siblings` at join, the fanned step routes per its `on_fail` | §2, verbatim |

**J5's "no early cancel in v1" is the clause an optimizer breaks.** Reaching the
quorum early does not release the join; the join waits for every sibling and *then*
compares. A test asserts that a 4-way fanout with `min_siblings = 2` and two early
`done` siblings does **not** advance until the other two are terminal.

## 7.6 Events written (read surface is S6)

Every transition writes an event, in the transaction that performs it. The event
kinds at S3 form a **closed set**, which is what makes §9 item 2 checkable:

`run-started`, `run-activated`, `run-paused`, `run-resumed`, `run-abandoned`,
`run-done`, `step-ready`, `step-claimed`, `step-heartbeat`, `step-recorded`,
`gate-started`, `gate-recorded`, `step-routed`, `step-failed`, `step-superseded`,
`step-skipped`, `step-resolved`, `step-approved`, `step-rejected`, `loop-entered`,
`join-completed`, `lease-reaped`, `issue-promoted`, `issue-abandoned`.

**`gate-started` precedes each gate spawn** (§2: "a `gate-started` event precedes
each spawn"), including the S3 pass-through, so the at-least-once gate discipline is
observable and its resume points are real before S4 supplies the spawn.

**No read verb for events lands here.** §10 puts `events list --since` at stage 6
and `--follow`/`prune` at stage 7. The table is written and indexed; tests and QA
read it with `sqlite3` directly. This is deliberate: shipping a read verb now would
freeze a shape S6 is specified to define.

Phase 2 and 3 write events through a small `recordEvent(tx, …)` seam that is a no-op
until this phase creates the table — so phases 2 and 3 are independently stoppable
without leaving dangling event calls, and phase 4 is a one-line switch plus the DDL.

## 7.7 Test plan (phase 4)

**Go unit tests** (`internal/engine/loop_test.go`) — the loop table, one case per
§7.2 row:
- instance rendering across (ordinal 0/n) × (fanned/not).
- `loop_count` increments per fix-loop entry; `max_fix_loops` exceeded ⇒
  `waiting-human`, and a third entry is impossible at `max_fix_loops = 2`.
- **the supersede table**: for each status in the machine, whether a downstream
  instance at ordinal ≤ k is superseded or left alone — one row per **persisted**
  status (nine; `ready` is computed, §6.2), plus a row for a step that is *computed*
  ready at sweep time, which must be superseded like any unclaimed instance.
- `TestStaleLineageRoutingIsInert` (§7.3).
- ordinal-scoped input binding including **per-input fallback** to different
  ordinals in one step (the fixture's `fix` step, exactly).
- re-instantiation of `after_loop` **and its transitive downstream** at ordinal `k`;
  gates re-run; thresholds re-apply.
- issue completion evaluated over highest-ordinal instances only — an issue with a
  `done` `verify@0` and a `pending` `verify@1` is **not** complete.
- prior artifacts remain addressable after supersede.

**Go unit tests** (`internal/engine/join_test.go`): J1–J5, with J5's no-early-cancel
case explicit.

**Go unit tests** (`internal/engine/events_test.go`): every transition writes
exactly one event of the expected kind; `seq` is strictly increasing across
concurrent writers; event kinds are drawn only from the closed set (a guard test
enumerating the constant list and asserting no other literal reaches the writer).

**QA** (`test_zg_workflow.sh`, extended):

#### The full loop run — driven by `verify`'s equality threshold

The fixture has two `fix-loop` thresholds, and **only one of them can legally fire at
S3**. The QA loop is specified against that one, explicitly, so the implementing
session does not discover the constraint mid-run:

| Step | Threshold | Evaluates at S3? |
|---|---|---|
| `reconcile` (action) | `{ "fix-loop" = "any(severity >= high)" }` | **No — and it does not park either.** Its stub payload is empty (§6.13), so §6.14 T4 short-circuits `any` over zero payloads to **false** before the `>=` is attempted. No match ⇒ `pass` (§11.2). The step flows through; the ordered comparison is never evaluated and nothing is guessed |
| `verify` (executor) | `{ "fix-loop" = "any(status == unmet)", "waiting-human" = "any(status == unverifiable)" }` | **Yes — `==` is an equality operator, live at S3 per §6.14 T1**, over a payload the harness supplies on `step complete --payload-file` |

So the loop is driven **at `verify`, by a harness-crafted payload**, not at
`reconcile`. The run, step by step:

1. Activate the fixture against a one-issue graph; `next --run` → `implement@0`;
   claim, complete with an artifact of kind `change-summary`. Gates are stubbed
   (§5.6) and the section says so in a comment.
2. `review@0#0..#3` become ready (fanout); claim and complete each with a `findings`
   artifact; the join (§7.5 J1) waits for all four.
3. `synthesize@0` → `findings`. `reconcile@0` is an **action** step: it is claimed and
   completed like any other step, and the S3 `stubRunner` records a `findings`
   artifact with an **empty** payload (§6.13). Assert the artifact exists, its kind is
   `params.output` = `findings`, and it carries `stub: true`.
4. Assert `reconcile@0` routed `pass` — the T4-short-circuit above, asserted directly
   so a future change to T4 breaks *this* test rather than the whole loop mysteriously.
5. `verify@0` becomes ready. Complete it with a **crafted payload** of
   `[{"status":"unmet"}]`. `any(status == unmet)` is true by T1 ⇒ routing `fix-loop`.
6. Assert loop entry: `run_issues.loop_count == 1`; a `loop-entered` event;
   `commit-gate@0` and `commit@0` — downstream of `after_loop` and unclaimed — are
   `superseded` (§7.3); `fix@1` instantiates (§11.3 (3)).
7. `fix@1`'s inputs bind per-input across ordinals (§7.4): `reconcile.findings` at
   ordinal 0 (nothing re-ran it at ordinal 1 yet) and `implement.change-summary` at
   ordinal 0. Assert both, by artifact id.
8. Complete `fix@1`; `after_loop = "review"` re-instantiates `review@1#0..#3` and its
   downstream chain at ordinal 1 (§11.3 (4)). Run the second pass through
   `synthesize@1` → `reconcile@1` → `verify@1`.
9. Complete `verify@1` with a payload of `[{"status":"met"}]` ⇒ no threshold matches
   ⇒ `pass` ⇒ `commit-gate@1` becomes ready.
10. `commit-gate@1` is `type="human"`: assert `claim` refuses it (§6.15), then
    `docket step approve` ⇒ `done` (§4.3.2). Assert the **reject** path in a separate
    disposable run: `docket step reject` routes per the fixture's now-explicit
    `on_fail = "fix-loop"` (§4.3.2, F3), **not** `waiting-human`.
11. `commit@1` completes; the issue's completion is evaluated over highest-ordinal
    instances only (§11.3) — assert a `done` `verify@0` alongside a `done` `verify@1`
    does not double-count, and that the ordinal-0 `commit-gate`/`commit` remaining
    `superseded` does not block completion. Run reaches `done`.

Asserted throughout: instance names at each ordinal, `superseded` rows, the
highest-ordinal completion rule, and `loop_count` never exceeding `max_fix_loops = 2`.

**A separate small case covers §6.14 T3**, which the fixture cannot reach: a
throwaway workflow whose executor step carries `threshold = { "fix-loop" = "any(x >= y)" }`,
completed with a **non-empty** payload, must park `waiting-human` with the reason
string naming the predicate and the missing schema — proving the engine refuses to
guess an order rather than proving it never has to.

- `guard stop` / `guard gate` allow/deny, by exit code 0/2 and reason text —
  `guard gate --step commit-gate` denies before the approve in step 10 and allows
  after it.
- **§9 item 2** over the completed run (partial per §6.16).
- **Phase-4 dormancy** (§3).

---

## 8. Cross-phase test obligations

### 8.1 Where tests live

| Kind | Location | Style reference |
|---|---|---|
| grammar/lint/loop/superseded tables | `internal/workflow/*_test.go`, `internal/engine/*_test.go` | `internal/planner/plan_test.go` — table-driven, one case per documented rule |
| CLI verb behavior | `internal/cli/*_test.go` | `internal/cli/next_test.go` |
| end-to-end + refusal matrices + races | `scripts/qa/test_zg_workflow.sh` | `scripts/qa/test_zf_claims.sh` |

`SECTIONS` gains `ZG:test_zg_workflow` after `ZF:test_zf_claims`.

### 8.2 §9 item 4 — in full

> 4. Kill a worker mid-step: lease expiry alone re-readies it; the run completes;
>    the attempt trail is complete.

The S2 QA proved the issue-level half. Here it is proven at step level and **to
completion**: claim a step with a short TTL from a subshell, `kill -9` it without
releasing, assert (a) the step reads effective-not-live with **no intervening
command**, (b) `next --run` re-offers it with `attempt` incremented, (c) a second
worker claims and completes it, (d) **the run reaches `done`**, and (e) the attempt
trail shows both claims. Item (d) is the part S2 could not prove and is why this is
"§9.4-full."

### 8.3 §9 item 5 — byte-identical context goldens and mid-run edit immunity

> 5. Determinism: same run at same pins ⇒ identical step topology and byte-identical
>    context bundles, immune to mid-run issue edits and working-tree changes.

The strongest available proof, at three levels:

1. **Topology golden.** Activate the fixture against a fixed issue graph; the full
   step table (instance, kind, status, order) is golden-diffed. Re-activating a
   fresh DB from the same inputs reproduces it byte-for-byte.
2. **Context-bundle goldens.** `docket step context STEP-N --json` for every step,
   golden-diffed. Goldens are committed under `scripts/qa/fixtures/context/`.
3. **Mid-run edit immunity.** Between two identical `step context` calls: edit the
   issue body, edit its title, add a label, **change its `--scope`**, modify a pinned
   file **on disk**, and modify the working tree. The second call must be
   **byte-identical** to the first. This is the test that catches an implementation
   reading a live field "just for the title" — the single most likely way this AC
   breaks, and the reason `title`/`kind`/`labels`/`scope` are snapshot columns
   (§5.1.1) rather than a join against `issues`. Each mutated field is asserted
   individually, so a partial snapshot fails on the specific field it missed instead
   of on an opaque whole-bundle diff.

The goldens' sensitivity is itself verified, per the ZF4b precedent: a deliberate
change to a pinned input **must** change the golden, so a passing diff is not
vacuous.

### 8.4 The dormancy proof

§3 specifies it per phase. It is not a single end-of-stage check: each phase's
commit group carries its own, so a stopped stage is a dormant stage.

### 8.5 The byte-compat sweep

The existing sweep script (committed at S2) re-runs at every phase: every existing
verb, against the fixture, under `--json`, from the S2-era binary and the current
one, diffed byte-for-byte. Zero diffs required; non-empty output asserted.

Phase 3 additionally requires **QA section X (`test_x_next.sh`) to pass untouched**
— the `next` mode-switch proof (§6.3.1).

## 9. Documentation, scheduled per phase

CLAUDE.md's same-PR rule: a stale table blocks review. Per phase:

| Phase | Docs in the same commit group |
|---|---|
| 1 | SKILL.md: `docket workflow` verb/flag tables, the grammar reference, error codes; **docs/spec/architecture.md** — the workflow/step/run entity model and the two-level graph |
| 2 | SKILL.md: `docket run` tables, `--pin`, `--scope` on `issue create\|edit`; architecture.md — activation, pinning, snapshots |
| 3 | SKILL.md: `docket step` tables, `next --run`, `guard`, token transport (pointing at the existing `docs/spec/security.md` §, extended for steps); architecture.md — the status machine and the saga |
| 4 | SKILL.md: loops, joins, `min_siblings`; architecture.md — loop semantics and the event kinds |

**docs/spec/review-strategy.md** (new, phase 1) encodes the genericity rule as a
**standing review gate**, per the work order. It states the rule, the stranger test,
and the mechanical check: a reviewer greps the diff's core-surface files for the
banned vocabulary set (`model`, `prompt`, `llm`, `agent`, `brief`, `node`,
`severity`, `review` as a *concept*, not the English word) and a PR that adds one to
a flag name, JSON key, column name, error string, template, or help text fails by
definition. The check is scripted as `scripts/qa/genericity.sh` and wired into the
QA suite so it runs on every push rather than depending on a reviewer's memory —
the same discipline as the `--token`-flag guard test S2 established.

The banned-word check runs against **core surface only** (flag names, JSON keys,
column names, embedded templates, help text, error strings) and explicitly **not**
against the committed instance fixture, which legitimately contains `severity` as an
opaque threshold field name (§1.1). That exclusion is written into the script with a
comment naming §11.2, so nobody later "fixes" the exclusion and breaks the fixture.

## 10. Amendment issues filed by this stage

Per docs/design/amendments.md and the work order: nominal field-name deviations are
recorded here, filed as DKT issues citing the exact line, and the slice proceeds.

| # | Issue | Deviation | Spec line cited | Disposition |
|---|---|---|---|---|
| A1 | **DKT-15** | §11.4's `next row`/`context` do not name a field for the **rendered instance identity** (`name@k#i`), which §11.3 makes the step's public identity. The implementation carries `step` (the `STEP-N` id) **and** `instance` (the rendered identity) on the next row and in `context.step`. | §11.4 line `next row { step, issue, run, executor, class, attempt, expected_cost, lease_ttl_s, metadata }` and §11.3 line "Step instances are identified `name@k#i`" | Additive field, no semantic change; the amendment proposes §11.4 name `instance` explicitly. Recorded here; slice proceeds. |
| A2 | **DKT-16** | §11.4's `gate result` shape is specified, but `gate_results` is v8's table (reliability-delta §2). S3's stub results ride in `steps.gate_trail` and S4 migrates them. | §11.4 line `gate result { step, gate, argv, exit, duration_ms, output, truncated, verdict }` | Storage-location deviation only; the wire shape is unchanged and gains `stub: true` at S3 (§5.6). Recorded here; slice proceeds. |
| A3 | **DKT-14** | S2's `claim response` subject-key deviation is **closed** by this stage, not amended: `step claim` implements §11.4 verbatim, subject key and `context` bundle both. | §11.4 line `claim response { step, token, lease_expires_ms, context }` | Closure, not a new deviation (§6.4.1). Closed by the phase-3 commit that lands `step claim`. |
| A4 | **DKT-17** | §11.1's `on_fail` default (`waiting-human`) contradicts §2's rule that a human gate's reject routing may not be `waiting-human`: a `type="human"` step declaring no `on_fail` silently *has* the forbidden routing. V13 therefore evaluates the **effective** value, making explicit `on_fail` mandatory on human steps (V13a). | §11.1 `on_fail` row and §2 "a human gate's reject routing may not itself be `waiting-human` (register-time VALIDATION_ERROR)" | **Spec edit already applied by the operator, 2026-08-03**, in engine-spec §11.1 and upstream (06 §11.1, 05 §1); the committed fixture's `commit-gate` gains `on_fail = "fix-loop"` in the same commit. Argued in §4.3.2. |

A1 and A2 are filed **before** this TDD is committed, so the amendment trail exists
ahead of the code that embodies it. A3 closes during phase 3. A4 was raised by the
S3 TDD's conformance review (docs/tdd/engine-spine-review.md, F3) and its spec edit
is applied ahead of any code, which is the discipline working as intended: the
contradiction was found in review of a design document, not in a deadlocked run.

## 11. Delivery

Direct commits on `feature/graph-engine` per the operator decision recorded in
reliability-delta §11: no branches, no PRs, **never** tags, linear history. Draft
PR #33 provides CI on every push. Every commit leaves the branch green.

Commit groups, in order — each independently stoppable, each green, each carrying
its own dormancy proof and its own docs:

1. **Phase 1** — TDD (this file) + the committed fixture; v7 migration (workflows
   table) + `internal/workflow` grammar/validation/lint + `workflow` verbs +
   embedded templates; unit tables; QA section ZG; SKILL.md + architecture.md +
   review-strategy.md + `genericity.sh`; DKT amendment issues A1, A2.
2. **Phase 2** — activation DDL slice; the fat transaction, pins, snapshots, fence
   harvest, lazy expansion, re-activation; `run` verbs; the gate seam; unit tests;
   QA extension; docs.
3. **Phase 3** — steps/artifacts DDL slice; `next --run`; claim + context bundle;
   the saga; routing; the `step` verb family incl. `approve`/`reject` semantics
   (§4.3.2) and the human/vote lifecycle (§6.15); the **action seam** (§6.13) and
   **threshold evaluation** (§6.14); `issue.diff` production (§6.7.1); pinned-template
   verification (§6.11.1); `guard stop|gate`; the lease-helper generalization; unit
   tests + mutation test; QA extension incl. §9.4-full and the §9.5 goldens; docs.
   **Closes DKT-14.**
4. **Phase 4** — events DDL slice + `loop_count`; loops §11.3; joins + `min_siblings`;
   events written; unit tables; QA full-loop run and §9.2-partial; docs.
