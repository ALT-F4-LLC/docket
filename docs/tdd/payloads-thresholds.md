# TDD: payload schemas, ordered enums, live thresholds, and the `aggregate` action (stage 5)

Status: draft — 2026-08-03 · implements docs/design/engine-spec.md §2 (the
"Payloads and thresholds" block, verbatim and normatively), §11.1's `payload`
and `params` rows, **§11.2 (whole)**, and §11.4's payload/validation surface;
engine-core.md §6 supplies the semantics ("deterministic computations over
worker output"). Acceptance bar: **§9 item 5** (determinism — validation
verdicts and aggregation results are pinned facts) and **§9 item 1** (the
stranger test, re-earned over every noun this stage adds). Tracker unit:
**DKT-5**. Spec of record is engine-spec.md; deviations are DKT amendment
issues per docs/design/amendments.md citing the exact line, never silent
changes.

**This is the stage where core learns ORDER without MEANING.** Every decision
below is downstream of that sentence, and §1.1 is written first because it is
the sentence's enforcement.

## 1. Scope

engine-spec.md §10 stage 5, verbatim:

> 5. **Payload schemas + ordered enums + action steps** (thresholds,
>    reconcile-as-action).

In scope:

1. `internal/schema` — JSON Schema validation plus the `ordered_enum`
   annotation, the derived ordered-field index, and the embedded `aggregate@1`
   document. Pure: no database, no engine.
2. Schema **v9**: the `schemas` table (immutable `name@version` per the
   `workflows` precedent), `action_results`, `trust_cache.kind`,
   `steps.materialized`, `artifacts.stub`; the sentinel-guard extension; the
   upgrade-path proof against this repo's v8-stamped tracker.
3. `docket schema register|list|show`, and schemas joining the **activation pin
   set** (registered objects by version — §2).
4. **Payload validation at `complete`**: `--payload-file` validated against the
   step's declared `payload = schema@ver` in saga stage 0, before the artifact
   records.
5. The **V21/V25 upgrade**: threshold fields and literals cross-validated
   against the registered schema at `workflow register` time (§11.2), with
   ordered operators requiring an `ordered_enum` field.
6. **Live thresholds**: engine-spine §6.14's T1–T4 upgraded per that section's
   own "Where S5 upgrades this" table — T2/T3 come alive for `ordered_enum`
   fields, T3 survives for fields that are still schema-less.
7. The **real `ActionRunner`**: a builtin registry containing `aggregate`, with
   every non-builtin action name executing as a **user-trusted command** through
   S4's trust/exec machinery, step context on stdin (§2).
8. **`aggregate`, in full**: `params = { field, method = median|max|min,
   hold_spread, output }`; median/max/min over the declared order; spread as
   distance in that order; the `hold_spread` trip, the materialized
   `<step>-held` `type=human` step, `operator_resolved`, and the `demoted_from`
   trail.

Out of scope, explicitly, each with the stage that owns it:

| Deferred | Owner | Why not here |
|---|---|---|
| **Activation auto-registration** of `.docket/config/` (§2's zero-touch clause) | S6 | §9 item 11 is DKT-6's acceptance bar. §4.6 pins the **ordering contract** S6 must implement (schemas before workflows) because that ordering is what keeps the zero-touch path off §4.6's refusal; the scan itself is not built here |
| Budget enforcement and the floor; `run report`; `dispatch`; `guard spawn\|record`; `events list --since` | S6 | §10 stage 6 |
| `events --follow`, `events prune` | S7 | §10 stage 7 |
| Nested/­dotted threshold field paths (`a.b >= c`) | never in v1 | §11.2's grammar is `agg(field op literal)` with a bare field token; §13 records the shape a future amendment would take |
| An operator **setting a value** when resolving a held cluster (`approve --set`) | not this stage | §7.8: core cannot read a disposition out of a free-text note without interpreting instance meaning, which is the genericity rule's exact prohibition. §13 records the amendment shape |

**What this stage deliberately does not change**: the saga's stage table and its
transaction boundaries (§6.8 of engine-spine), the status machine's ten
statuses, the trust model (§4 of engine-spec) in any respect, and the
readiness predicate R1–R7. Every one of those is asserted by a test that
compares against an S4 golden, listed in §9.

### 1.1 Genericity check (CLAUDE.md PR bar, docs/design/genericity.md)

The rule, verbatim, with the clause this stage exists to implement in bold:

> Core Docket contains zero agent/LLM vocabulary — no node, model, prompt,
> brief, severity, or review concepts. Executors are opaque string hints;
> execution metadata is an opaque KV bag; **ordered meaning comes from
> user-registered schemas**; computations beyond scheduling arithmetic are
> user-trusted commands.

**Every noun this stage introduces into core surface**, per the §1.1 pattern
engine-spine and gates-trust established:

`schema`, `ordered_enum`, `payload`, `action result`, `builtin`, `aggregate`,
`method`, `median`, `max`, `min`, `field`, `spread`, `hold_spread`, `held`,
`members`, `demoted_from`, `operator_resolved`, `materialized`.

Every one is **ordering, statistics, or packaging vocabulary**. None names a
model, prompt, brief, node, or review concept, and none carries a domain: a
median over a declared order is the same computation for severities,
priorities, tiers, T-shirt sizes, or ripeness grades. The three places instance
meaning could leak are closed by construction, and each is a test:

- **`ordered_enum` declares position, never significance.** Core knows that
  `enum[i]` precedes `enum[i+1]`. It does not know which end is "worse",
  "higher", "more urgent", or "better", and **§7.3's tie rule is chosen
  precisely so that it never has to** — a rule justified by "take the more
  severe one" would be core holding an opinion about severities, which is the
  failure mode the rule exists to prevent, arriving through a statistics
  function rather than through a flag name.
- **`field`, `method`'s operands, and every literal stay opaque tokens.** The
  aggregate reads `params.field` as *a key to look up*, and the values it
  compares are compared **by their index in the user's declared order** and by
  nothing else. There is no table of known fields, no default order, and no
  fallback ordering — a field with no registered `ordered_enum` is an error at
  register (§4.9) or a park at runtime (§5, T3), never a guess.
- **A non-builtin `action` is a user-trusted command**, not an extension point
  core interprets. It receives the §11.4 context on stdin and returns a body
  and a payload; core never reads a key of its `params` (§6.2).

`severity`, `high`, `unmet`, and `findings` appear **only in fixtures** —
`docs/design/example-workflow.toml`, `scripts/qa/fixtures/schemas/`, and the
Go tests' table data. They are instance tokens, exactly as engine-spine §1.1
recorded them, and the genericity gate's existing exclusion for the committed
fixture is unchanged and still carries its §11.2 rationale in a comment.

The stranger test (§9 item 1) for this stage's headline verb, stated as a
stranger would read it: *"I registered a file that says my `risk` field goes
`low, medium, high`. Now my workflow can say `any(risk >= medium)` and Docket
routes on it."* No agent, no model, nothing AI-shaped, and it is the exact
sentence QA section ZH asserts (§8.3).

#### 1.1.1 The genericity gate gains `.json`, and that is not optional

`scripts/qa/genericity.sh` scans `internal/**` and `cmd/**` for `*.go` string
literals and struct tags plus `*.toml` templates. **This stage ships the first
embedded `*.json` core surface** — `aggregate@1` (§7.6) — and every byte of a
shipped schema is read by the first user who runs `docket schema show
aggregate@1`. A schema document is surface in exactly the sense the gate's own
comment gives for templates: *"every byte is read by the first user"*.

So the gate's `template_lines` treatment is extended to `*.json` under the
existing `SEARCH_PATHS` in **group 1**, in the same commit as the embedded
document. `scripts/qa/fixtures/**` is untouched by the change because the gate
never scanned outside `internal/` and `cmd/` — QA fixture schemas are instance
data by location, which is the cheapest possible enforcement and is stated
here so nobody later "tidies" the fixtures into `internal/`.

## 2. Schema version span

docs/tdd/reliability-delta.md §2's mapping is authoritative and is not
re-litigated. This stage occupies its **v9** row.

| Schema | Stage (§10) | Contents |
|---|---|---|
| v5–v8 | 1–4 | shipped |
| **v9** | 5 — payload schemas + ordered enums + action steps | **this stage**: `schemas`, `action_results`, `trust_cache.kind`, `steps.materialized`, `artifacts.stub` |
| v10 | 6 | later |

**v9 lands as one migration function** (`migrateV8ToV9`), sliced across this
stage's two commit groups exactly as v8 was across two and v7 across four: the
stamp moves to 9 in **group 1** (which needs `schemas`) and does not move
again, while `migrateV8ToV9` keeps growing through group 2.

**Therefore the v7 trap recurs, and the guard extension is not ceremony.** A
database migrated by a group-1 build — including this repo's own dogfooded
tracker, which is used across the stage — is stamped 9 with `schemas` present
and `action_results` absent. `v9Sentinels` is a single constant next to the
migration listing **every table the v9 DDL creates**:

```go
var v9Sentinels = []string{"schemas", "action_results"}
```

and `TestRewindGuardProbesEverySentinel` is extended to assert one entry per
v9-created table, so a group that adds a table without extending the list fails
its own test rather than shipping a half-migrated dogfood database.

**The column half is the part the sentinels cannot see**, and
docs/spec/architecture.md §3.1 already says so for v7. v9 adds four columns
(`trust_cache.kind`, `steps.materialized`, `artifacts.stub`, and — group 1 —
nothing else), each behind a `hasColumn` probe, and they arrive only because
the rewind re-runs the **whole** migration. §4.4's U-table proves each column's
arrival independently, reconstructing the group-1 shape exactly (table dropped,
columns removed, stamp left at 9).

**Never-mutate rule (inherited, reliability-delta §2.1).** Every timestamp
column created at v9 is `_ms INTEGER` epoch-milliseconds. No existing column's
format is touched, and **no existing row's bytes are rewritten** — including
the S3/S4 stubbed action artifacts, whose `{"stub":true,"payload":[…]}` wrapper
stays exactly as written (§6.3).

## 3. What is dormant, and how each group proves it

engine-spec §3 ("Dormant unless workflows are used") and §9 item 8. The claim
for this stage, precisely:

> **A repo with no *user-registered* schemas behaves byte-identically to v8 on
> every pre-existing verb, at every `--json` version, in human mode, and in
> exit code; and every S3/S4 semantic for schema-less fields is intact.**

The qualifier "user-registered" is load-bearing and is stated rather than
finessed: **v9 seeds exactly one row** — the builtin `aggregate@1` (§7.6). That
row is inert by construction (nothing reads it unless an `aggregate` action
runs) and is visible only through `docket schema list|show`, which are **new
verbs**. No pre-existing verb reads the `schemas` table, so the byte-compat
claim is untouched; claiming "zero rows" would have been the flattering
statement rather than the true one.

| Group | Dormancy proof |
|---|---|
| 1 | v8→v9 applied; `schemas` holds the one builtin row and nothing else; the full byte-compat sweep (engine-spine §8.5) passes; `docket workflow register` of a schema-less definition behaves **byte-identically to S4**, including its exit code; the 4→9 fixture protocol (§4.4 U6) |
| 2 | `action_results` empty; **a run with no `aggregate` step and no `payload` declarations produces byte-identical step/artifact/threshold behavior to S4**, asserted by re-running the S4 ZG loop golden unchanged; T1–T4's S3 semantics intact for schema-less fields (§5's survival suite) |

**The strongest dormancy statement this stage can make**, and the one §9.3
asserts as a script: *a repo that never registers a schema and never writes an
`action` step cannot tell v9 from v8 by any observable behavior.*

---

# Group 1 — `internal/schema`, `schema register`, and payload validation

Commit group 1. Leaves the branch green and is independently stoppable: it
ships a usable `docket schema register|list|show` over a table nothing routes
on, plus payload validation at `complete` for steps that declare a `payload`.
No threshold behavior changes, no action behavior changes.

## 4.1 THE DEPENDENCY: a pure-Go JSON Schema library

**CGO stays off. That is a binding repo fact**, not a preference: the module
already takes `modernc.org/sqlite` — a pure-Go SQLite — precisely so the binary
cross-compiles and the Vorpal build stays hermetic. A validator that pulled in
cgo would undo that for a feature that needs none of it.

**Chosen: `github.com/santhosh-tekuri/jsonschema/v6`.**

| Criterion | Why it decides |
|---|---|
| **Pure Go, zero cgo, transitively** | the binding constraint. Verified in group 1 by `TestNoCgoInTheModuleGraph` — `go list -deps` over the built binary asserts no package sets `CGO_ENABLED`-dependent build constraints beyond what v8 already had. A library that fails this is rejected on the spot, whatever else it offers |
| **Draft coverage: 4, 6, 7, 2019-09, 2020-12** | instance schemas are machine-authored (§2's config lifecycle) and will be written to whatever draft the authoring skill knows. A draft-07-only validator makes the draft a hidden compatibility surface |
| **Maintenance** | actively maintained with a tagged v6 major; the two obvious alternatives are not (see below) |
| **Error detail with instance locations** | §4.8 requires a **path-precise** refusal. This library's `ValidationError` carries `InstanceLocation` as a JSON-Pointer path, which renders directly as `payload[3].severity` — the difference between an actionable refusal and "the payload is invalid" |
| **Transitive dependency footprint** | one: `golang.org/x/text`, **already in this module's graph** as an indirect dependency (go.mod, via glamour/bluemonday). So the effective new-dependency count is **zero** |

Rejected, with reasons recorded so a later reader does not re-open the
question blind:

| Alternative | Why not |
|---|---|
| `xeipuuv/gojsonschema` | draft-07 only and effectively unmaintained; its error type reports a schema-relative context rather than an instance pointer, which loses §4.8's precision |
| `qri-io/jsonschema` | maintenance is intermittent and its extension model (custom keywords as Go types) is more machinery than `ordered_enum` needs — and §4.2 deliberately does **not** implement `ordered_enum` as a library keyword |
| **Hand-rolling a validator** | tempting because the schemas in play are small, and wrong: a hand-rolled subset validator is a *silent* subset — an instance schema using a keyword we skipped validates vacuously, and the operator's first evidence is a bad payload that passed. The failure is invisible, which is the worst property a validator can have |

**Verification obligation, group 1, before the dependency is committed**: `go
mod why` and `go list -deps` output is pasted into the commit message, and
`TestNoCgoInTheModuleGraph` lands in the same commit. If the library turns out
to introduce any cgo-bearing transitive dependency, **the choice is void** and
the fallback is `kaptinlin/jsonschema` under the same test — the constraint
decides, not the shortlist.

**The renovate implication, recorded because it is a behavior-bearing
dependency.** `renovate.json` extends `config:recommended`, which already
proposes gomod updates. A JSON Schema validator is not an ordinary dependency:
**a patch bump can change a validation verdict**, which changes whether a
worker's `complete` is refused — a behavior change arriving through a
dependency PR nobody reads as a behavior change. Three mitigations, all in
group 1:

1. **A golden corpus.** `internal/schema/testdata/corpus/` holds
   (schema, document, verdict, message-substring) cases — valid, invalid, and
   the edge cases §4.8 names. `TestCorpus` is what a renovate PR must keep
   green, so a verdict change surfaces as a red CI on the bump rather than as
   a refused `complete` in production.
2. **The draft is pinned explicitly.** The compiler is constructed with an
   explicit default draft (2020-12) rather than the library's, so a library
   default change cannot shift semantics under an unchanged schema document.
   `TestDefaultDraftIsPinned` registers a schema with no `$schema` key and
   asserts the 2020-12 interpretation of a keyword that differs across drafts.
3. **`renovate.json` gains a `packageRules` entry** for the validator marking
   it `"minimumReleaseAge": "7 days"` and labelling it so the bump is reviewed
   against the corpus rather than auto-merged. That is a one-object edit,
   lands in group 1, and is the only change this stage makes to renovate.

## 4.2 The `ordered_enum` annotation

§2, verbatim: *"`schema register` accepts JSON Schema plus an `ordered_enum`
annotation"*.

**The form, pinned:**

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "array",
  "items": {
    "type": "object",
    "properties": {
      "severity": {
        "type": "string",
        "enum": ["info", "low", "medium", "high", "blocker"],
        "ordered_enum": true
      }
    },
    "required": ["severity"]
  }
}
```

| # | Clause |
|---|---|
| O1 | `ordered_enum` is a **boolean annotation** on a subschema that also declares `enum`. **The order is the `enum` array's document order, ascending.** There is no second list |
| O2 | A subschema with `"ordered_enum": true` and no sibling `enum`, or whose `enum` is not an array of ≥2 **unique strings**, is a `VALIDATION_ERROR` at `schema register`, naming the property path |
| O3 | `ordered_enum` is an **annotation, not an assertion**: it constrains nothing that `enum` does not already constrain, so a document valid under the schema-minus-annotation is valid under it. This is what makes the next clause safe |
| O4 | **The validator never sees it.** `ordered_enum` is not registered as a custom keyword; the library validates the document as ordinary JSON Schema (unknown keywords are annotations by specification and are ignored). Core extracts the order itself, at register time, by walking the schema document with `encoding/json` (§4.3) |
| O5 | Only **top-level properties of the array's item schema** are indexed. §11.2's grammar is `agg(field op literal)` with a bare field token, so a nested ordered enum is unreachable by any predicate and indexing it would build a map nothing can query. §13 records the amendment shape if dotted paths ever arrive |
| O6 | **AMENDMENT (DKT-267).** An optional sibling `"conservative_end"` names WHICH END of the declared order the author considers the cautious one. Its value is `"upper"` or `"lower"` — positions in the ascending `enum`, never values the enum holds. It must sit beside `"ordered_enum": true`; declared over an unordered subschema it is a `VALIDATION_ERROR` naming the property path. Absent means `"lower"`. Like `ordered_enum` it is an annotation the validator never sees (O4), and it constrains nothing (O3) |

**O6 does not weaken "order without meaning" — it relocates the meaning to the
only place that can hold it.** Core still knows no significance: it does not
read the values, does not decide which end is bad, and has exactly one reader
for the direction (§7.3's even-count tie). What changed is WHO IS ASKED. The
sentence "core does not know which end of a declared order is worse" was always
true and remains true; the sentence "therefore nobody does" was the mistake,
and it cost two operator corrections in the RUN-20 epoch (CL-4, CL-6), both
upward, both citing undisputed evidence for the higher band.

The vocabulary is positional on purpose. `"high"` and `"low"` would be drawn
from the same word-stock as a severity enum's own members, so a reader could
not tell the key's vocabulary from the field's, and a direction written as
`"blocker"` would look meaningful while naming nothing core can resolve.

**O1's single-list rule is the load-bearing one.** The obvious alternative —
`"ordered_enum": ["info","low","medium","high","blocker"]` alongside `enum` —
puts the same information in two places, and the two will eventually disagree:
someone adds a value to `enum` and not to the annotation, and core then orders
a value the validator accepts, or refuses to order one it does. With O1 there
is nothing to disagree with, and the failure mode ("the order is wrong") is
visible in the one array an author is already reading.

**O4 is why the library choice is replaceable.** Ordering is ours; validation
is theirs. Swapping validators changes which documents are accepted, never
which values are ordered — so the corpus (§4.1) fully covers the seam.

## 4.3 The ordered index, derived once and stored

At register time core walks the schema document and derives:

```
ordered := { "<property>": ["<value0>", "<value1>", …] }
```

stored in `schemas.ordered` as JSON text, next to the bytes it came from.

| # | Clause |
|---|---|
| I1 | The index is derived **once, at register**, and is thereafter read; nothing re-walks a schema document during evaluation. A comparison in a hot routing path must not depend on re-parsing a document |
| I2 | The index is a **pure function of the registered bytes**, so `TestOrderedIndexIsDerivedNotStored`-style round-tripping holds: re-deriving from `schemas.body` must equal `schemas.ordered`, asserted for every registered row by a QA check. The stored copy is a cache, never a second source of truth |
| I3 | `Position(field, value) (int, bool)` is the **only** ordering API. It returns the value's index in the declared order and whether the field is ordered at all. There is no `Less(a, b)` that could be called on an unordered field, and no comparison path that does not go through it — the type makes "compare two values of a field with no declared order" unrepresentable, which is T3's rule (engine-spine §6.14) enforced by the API rather than remembered by a caller |
| I4 | A value **present in the payload but absent from the declared order** is not orderable: `Position` returns `false`, and §5's evaluation treats it exactly as a schema-less field does — park with a reason naming the value. It is *not* sorted to the end and *not* treated as smallest. (Payload validation at `complete` (§4.8) makes this unreachable for validated payloads; it is reachable for a payload recorded before the schema was declared, which is precisely when guessing would be worst) |

## 4.4 v9 DDL slice — group 1

```sql
CREATE TABLE IF NOT EXISTS schemas (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL,
    version       INTEGER NOT NULL,
    source_path   TEXT,
    source_sha256 TEXT    NOT NULL,
    body          TEXT    NOT NULL,   -- the schema document, verbatim bytes
    ordered       TEXT    NOT NULL DEFAULT '{}',  -- the §4.3 derived index
    builtin       INTEGER NOT NULL DEFAULT 0,     -- 1 on aggregate@1 only
    created_at_ms INTEGER NOT NULL,
    row_version   INTEGER NOT NULL DEFAULT 1,
    UNIQUE(name, version)
);

CREATE INDEX IF NOT EXISTS idx_schemas_name ON schemas(name);

ALTER TABLE artifacts ADD COLUMN stub INTEGER NOT NULL DEFAULT 0;  -- §6.3
```

**The shape is `workflows`, deliberately and line for line**: `name`,
`version`, `source_path`, `source_sha256`, `body`, a derived column
(`parsed` there, `ordered` here), `created_at_ms`, `row_version`,
`UNIQUE(name, version)`. Two registries with the same immutability contract
that looked different would invite two different implementations of that
contract.

**Immutability, per the `workflows` precedent (`db.InsertWorkflow`), verbatim
in behavior:**

| Outcome | Condition |
|---|---|
| insert, `created = true` | no row at `name@version` |
| return the existing row, `created = false` | a row with the **same** `source_sha256` — an idempotent success that inserts nothing and **does not bump `row_version`** |
| `CONFLICT` (exit 4), naming **both** hashes | a row with a **different** `source_sha256` |

Idempotency is decided on the **content hash**, not on a normalized form: two
documents that validate identically but differ in whitespace or key order are
different registered bytes, because `source_sha256` is what pins refer to
(§4.7) and what a run reproduces against. `ErrSchemaConflict` mirrors
`ErrWorkflowConflict` and maps to the same code and exit.

**Why a `CONFLICT` and not an overwrite**: engine-core §4's pinning property —
*"editing a pipeline never changes an in-flight run"* — is worth exactly
nothing if the pinned bytes can be swapped underneath the run. A schema decides
whether a worker's payload is accepted; a mutable `findings@1` means a run's
acceptance criteria change mid-flight. Bump the version.

### 4.4.1 The upgrade-path proof (this repo's v8-stamped tracker, a third time)

Each row is a case in `internal/db/migrate_v9_test.go`:

| # | Starting database | Required outcome |
|---|---|---|
| U1 | **This repo's tracker**: stamped **8**, all v7/v8 sentinels present, `steps` empty | migrates to 9; `schemas` created and seeded with `aggregate@1` only; `action_results` created; every existing verb byte-identical (§3) |
| U2 | stamped 8 with a **populated** run (the S4 QA fixture's completed loop) | migrates; every artifact keeps its bytes; the S3/S4 stubbed action artifacts keep their `{"stub":true,…}` wrapper **unmodified** and gain `stub = 1` in the new column (§6.3 A2) |
| U3 | stamped **9** but `action_results` **absent** — the group-1-partial dogfood shape, exactly the v7 and v8 trap | the rewind guard rewinds to 8 and re-runs; the database ends complete, **including the columns** (`trust_cache.kind`, `steps.materialized`, `artifacts.stub`), each asserted individually |
| U4 | stamped 9, both tables present, **`steps.materialized` absent** | same rewind; the column arrives; existing step rows are intact and read `0` |
| U5 | migration run **twice** against U2's database | no duplicate rows; `schemas` still holds exactly one builtin row; counts identical |
| U6 | **`scripts/qa/fixtures/v4-baseline.db`** | migrates **4→9 in one pass**; v9 structures asserted present **before** the golden diff is trusted (engine-spine §3's rule — a golden diff against a database that failed to migrate passes vacuously) |
| U7 | stamped 9 whose `schemas` holds an `aggregate@1` row with a **different** hash than the embedded bytes | the migration does **not** overwrite it and does **not** refuse to open: the seed is `INSERT OR IGNORE`. The divergence is caught at build time instead, by `TestEmbeddedAggregateSchemaMatchesItsGolden` (§7.6) — a database must never become unopenable because a binary changed |

U3 exists because it is not hypothetical: group 1 ships `schemas`, group 2
ships `action_results`, and the operator's own tracker is migrated by whichever
binary happens to be built between them.

## 4.5 `docket schema register|list|show`

| Verb | Flags | Effect |
|---|---|---|
| `docket schema register <name@version> <file.json>` | `--json[=v2]` | read; parse as JSON; validate as a schema document (the library compiles it — a schema that does not compile is refused here, not at first use); derive the ordered index (§4.3); insert per §4.4's three outcomes |
| `docket schema list` | `--json[=v2]` | registered schemas: `name`, `version`, `sha256`, `ordered_fields`, `builtin`, `created_at_ms`. A `Collection` envelope under v2, per the `workflow list` precedent |
| `docket schema show <name@version>` | `--json[=v2]`, `--body` | the row; `--body` emits the registered **bytes verbatim** (what a run validates against, not a re-serialization) |

**`name@version` is an argument, not two flags** — §1's surface line is
`docket schema register name@v schema.json` and it is followed exactly. The
name shape is `[A-Za-z0-9_-]+` and the version an integer ≥ 1, i.e. the same
`payloadShape` regexp `internal/workflow/validate.go` already uses for V25,
**shared rather than restated** so the grammar a workflow references and the
grammar a registry accepts cannot drift.

**Refusals**: unreadable/absent file ⇒ `NOT_FOUND` (2); malformed JSON, a
schema that fails to compile, or an O2 violation ⇒ `VALIDATION_ERROR` (3)
naming the property path; differing bytes at an existing `name@version` ⇒
`CONFLICT` (4) naming both hashes; `show` of an unregistered name ⇒
`NOT_FOUND` (2).

**`list` and `show` are an additive deviation from §1's surface line** and are
filed as an amendment (§11 A1) rather than slipped in. The argument for
shipping them: activation **auto-registers** (§2's zero-touch clause), so in
the intended flow no human ever types `schema register` — and without a read
verb the operator cannot see what was registered, cannot see what a run pinned,
and cannot recover the bytes a refusal is quoting. That is the same argument
that gave `workflow list|show`, applied to a registry with the same lifecycle.

## 4.6 REGISTRATION ORDER, pinned

§11.2, verbatim: *"Fields and literals are validated against the registered
schema at `workflow register` time."*

**The question the spec does not answer**: what happens when a workflow being
registered names a schema that is not registered yet?

| Option | Consequence |
|---|---|
| **(a) Hard `VALIDATION_ERROR` at register** — chosen | a workflow that cannot be validated is not registered. The refusal names the step, the `payload` value, and the remedy (`docket schema register findings@1 <file>`) |
| (b) Defer the check to activation | splits one validation table across two times, and produces a *registered* workflow that can never activate — the state V-rules exist to prevent. It also makes `workflow register`'s success meaningless, since the thing it validates is the thing it skipped |
| (c) Validate lazily at first threshold evaluation | the worst: the error surfaces hours into a run, on a step whose work is already done, and routes to `waiting-human` for a typo in a file |

**(a) is chosen, and it is the only reading under which §11.2's sentence is
true.** "Validated against the registered schema at register time" cannot be
satisfied when there is no registered schema; the alternatives all amount to
*not* validating at register time while claiming to.

**The zero-touch path never reaches this refusal, and that is a contract, not
luck.** §2: *"Activation auto-registers the config directory's current
contents"*. That auto-registration is S6's (§1's scope table), and this TDD
pins the ordering it must implement:

> **Auto-registration registers `.docket/config/schemas/` in full before it
> registers `.docket/config/workflows/`.** Within each directory the order is
> lexical by filename, for determinism. A workflow whose schema is in the same
> directory tree therefore always registers second.

It is written here, in the stage that creates the dependency, because S6 will
otherwise discover it as a bug. §9's group-1 test set includes
`TestSchemasRegisterBeforeWorkflows` as a **pending** test against the ordering
helper's signature — the helper exists here (it is three lines and one sort),
the directory scan does not.

**A later schema version does nothing to an already-registered workflow.**
`payload = "findings@1"` names a version; registering `findings@2` creates a
new row and touches nothing. A workflow that wants the new schema declares it
and bumps its own `[pipeline].version`, which a run then pins. This falls out
of version-pinning and is stated only because "the schema was updated" is the
first thing an operator will assume when a threshold behaves as it did
yesterday.

## 4.7 Schemas join the activation pin set

§2, verbatim: *"version pinning — registered objects by version"*. A schema is
a registered object; therefore it pins.

| # | Clause |
|---|---|
| P1 | Activation records one `pins` row per schema referenced by any **bound** workflow's steps (`payload` values), with `kind = 'schema'` (new `db.PinKindSchema`), `ref = "name@version"`, `sha256 = schemas.source_sha256` |
| P2 | A referenced schema that is **not registered** at activation is a `VALIDATION_ERROR` naming the workflow, the step, and the schema. This is unreachable through `workflow register` (§4.6 (a)) and reachable through a database restored from elsewhere — the same reasoning `ParsePredicate` keeps its own error for |
| P3 | Re-activation **inherits** the original pin set (RA2, engine-spine §5.4), so a schema registered mid-run does not enter a live run |
| P4 | **Validation at `complete` reads the pinned bytes** (§4.8), never the live table. Two runs of the same issue at the same pins therefore reach the same verdict on the same payload, which is what makes validation part of §9 item 5 rather than an environmental accident |
| P5 | The **builtin** `aggregate@1` pins like any other referenced schema when an `aggregate` step is bound. It is a registered object; that it ships in the binary changes where its bytes came from, not whether a run records what it used |

**§9.5 (engine-spec §9 item 5) goldens extend to cover schema pins**: the
activation topology golden (engine-spine §8.3 level 1) gains the pins table,
so a run that binds a workflow with `payload` declarations shows its schema
pins in the golden, and the goldens' **sensitivity** check is extended
correspondingly — re-registering the schema at a new version and re-activating
must change the golden, or the pin is not being recorded.

## 4.8 Payload validation at `complete`

§11.1's `payload` row: *"payload validated at `complete`"*. engine-spine §6.8's
stage-0 row says *"validate payload (**shape only at S3** — §6.14; the schema
register is S5's)"*. This is the promised upgrade, and it lands **inside stage
0, before the artifact records**.

| # | Clause |
|---|---|
| C1 | A step whose definition declares **no** `payload` is **unchanged**: shape-only validation (a JSON array of objects), exactly as S3/S4 (`parsePayload`). No new refusal exists for it |
| C2 | A step declaring `payload = "name@version"` has its `--payload-file` bytes validated against the **pinned** schema (§4.7 P4). Failure ⇒ `VALIDATION_ERROR` (exit 3), **before** the transaction opens, so nothing is written and the step's `row_version` is unchanged (the version-unchanged assertion every refusal in §6.9 carries) |
| C3 | The message is **path-precise**, rendered from the validator's instance location: `payload[3].severity: value "urgent" is not one of ["info","low","medium","high","blocker"]`. Multiple errors render as up to **five** lines, then `(+N more)` — a worker's log is not improved by a hundred |
| C4 | An **absent** `--payload-file` on a step that declares `payload` is a `VALIDATION_ERROR` naming the schema. A declared payload is a contract; silently recording no payload would make every threshold over it evaluate against the empty set and route `pass` (T4) — a silent misroute produced by an omission |
| C5 | Validation happens **before** the artifact size check is irrelevant — the order is: size cap (R12), then schema. Both are pre-transaction; the order is pinned so the error a 2 MiB invalid payload produces is stable |
| C6 | The token requirement is unchanged: stage 0 authorizes the holder first (R1–R4, R9). A validation refusal for a **non-holder** is `AUTH_ERROR`, not `VALIDATION_ERROR` — authorization precedes content, always, or an unauthenticated caller learns the schema by probing it |

**Where this does not apply**: `type="human"` and `type="vote"` steps record no
artifact and take no payload; `action` steps produce their payload internally
and are validated at §7.6 rather than here.

## 4.9 The V21/V25 upgrade, and the re-validation stance

### 4.9.1 The cross-validation table

engine-spine §4.3's V21 (grammar) and V25 (`name@version` shape) are upgraded.
The **new** checks, each a `VALIDATION_ERROR` naming the workflow, the step,
and the offending field, and each a test case:

| # | Rule | Spec line |
|---|---|---|
| V25a | `payload` names a **registered** `name@version` (§4.6 (a)) | §11.2 "validated … at `workflow register` time" |
| V21a | every `threshold` predicate's **field** exists as a top-level property of the declared schema's item schema | §11.2 "Fields … are validated against the registered schema" |
| V21b | every predicate's **literal** is valid for that field: a member of `enum` when the field declares one; parseable as the declared `type` otherwise (`number`, `integer`, `boolean`) | §11.2 "… and literals are validated" |
| V21c | an **ordered** operator (`>=`, `>`, `<=`, `<`) requires the field declare `ordered_enum` — the error names the field and the rule, and says which schema was consulted | §11.2 "ordered comparisons are defined **only** for fields whose registered schema declares `ordered_enum`" |
| V21d | a step with a `threshold` and **no** declared `payload` keeps S3 behavior: grammar-only validation, no field check, and T3 at runtime (§5). **Not** an error | §11.1 `payload` is optional; §11.2's restriction is about *evaluation* |

**V21d is the clause that keeps the design honest.** Making `payload` mandatory
wherever a `threshold` appears would be a tidier rule and would break every
S3-era definition, including `verify` in the committed fixture, whose
`any(status == unmet)` needs no schema to be correct (T1). Equality has never
needed an order and still does not.

### 4.9.2 The seam: `Validate` stays pure

`internal/workflow.Validate(def)` is a **pure function of bytes** and stays
one — engine-spine's own comment says so, and V26 (gates-trust §8.2) already
established the pattern for a rule that asks a question about the
environment:

```go
// SchemaResolver reports what is registered. V21a-V21d are the only rules
// that ask a question about the environment; they take it as a parameter, so
// Validate() stays a pure function of bytes and every other rule with it.
type SchemaResolver interface {
    Schema(name string, version int) (*Registered, error)  // NOT_FOUND if absent
}

func ValidateSchemas(def *Definition, r SchemaResolver) error
```

The CLI calls `Validate` → `ValidateVoteRules` → `ValidateSchemas`, in that
order, so an author sees grammar errors before environment errors. **This is
why `TestFixtureRegistersClean` survives**: it exercises `Validate` over the
committed fixture with no database, and continues to, even after the fixture
gains a `payload` (§8.2). Cross-validation gets its own test with a fake
resolver.

`RuleIDs` gains `V21a`–`V21d`, `V25a`, and §7.1's `V27`–`V30`;
`TestValidationTableIsComplete` asserts **set equality** against the documented
table, never a count — the assertion that survives a rule being split.

### 4.9.3 The re-validation stance: already-registered definitions are not retroactively invalidated

**The rule**: `migrateV8ToV9` does not re-validate a single row of `workflows`,
and no verb ever revisits a registered definition's validity. New registrations
validate under the new rules; existing rows keep working; runs pinning them
keep running.

**The argument**, in the order that decides it:

1. **A registered `name@version` is immutable and content-addressed** (§4.4).
   It is a historical fact — *these bytes were registered* — not a live claim
   that they are still fashionable.
2. **Retroactive invalidation would break the pinning property.** A run pins a
   workflow version at activation precisely so that later edits cannot change
   it (engine-core §4). If `docket migrate` could render a pinned definition
   illegal, then upgrading the binary would stop an in-flight run — a run that
   was reproducible by construction becomes unrunnable by upgrade. That is a
   strictly worse failure than the one re-validation would catch.
3. **The thing re-validation would catch is caught anyway, at the only moment
   it matters.** An S3-era `any(severity >= high)` on a schema-less field does
   not silently misroute: T3 parks it with a reason (§5). The engine never
   guesses; it declines. So the cost of not re-validating is a park an operator
   can see and fix, and the cost of re-validating is a dead run.

**The honest consequence, stated rather than hidden**: an S3-era definition
whose file is re-registered **verbatim** after this stage may now be refused
(V25a/V21c), because validation runs before the content-hash idempotency path
in `db.InsertWorkflow`. The already-registered row is untouched and every run
pinning it is untouched; only the act of registering that file again requires
bringing it up to standard. `TestS3RegisteredWorkflowsSurviveTheUpgrade` proves
the first half against a v8 database with the fixture registered and a run
mid-flight, and `TestReRegisteringAnS3FileIsRefused` proves the second half is
a refusal and not a corruption.

## 4.10 Test plan — group 1

**`internal/schema` (`schema_test.go`, `ordered_test.go`, `corpus_test.go`)** —
table-driven, `internal/planner/plan_test.go` style, no database:

- **O1–O5**: `ordered_enum` with no `enum`; with a non-array `enum`; with
  duplicate values; with one value; on a nested property (indexed? no — O5);
  valid five-value case.
- **I1–I4**: `Position` on an ordered field, an unordered field, an absent
  field, and a value **not in** the declared order (I4 — `false`, never a
  fallback). Plus `TestNoComparisonAPIBypassesPosition`, a source-level guard
  in the family of gates-trust §5.1's no-shell check: nothing in
  `internal/schema` or `internal/engine` compares two enum values except
  through `Position`.
- **`TestOrderedIndexRoundTrips`** (I2): derive → store → re-derive → equal,
  over every corpus schema.
- **`TestCorpus`** (§4.1): the golden corpus — valid/invalid documents,
  verdicts, and message substrings. This is the renovate gate.
- **`TestDefaultDraftIsPinned`** and **`TestNoCgoInTheModuleGraph`** (§4.1).
- **`TestEmbeddedAggregateSchemaMatchesItsGolden`** (§7.6), landing here with
  the embedded document even though its consumer is group 2.

**`internal/db` (`schemas_test.go`, `migrate_v9_test.go`)**: the three
registration outcomes of §4.4 (insert / idempotent identical / `CONFLICT`
naming both hashes, with `row_version` unchanged on the idempotent path); the
U1–U7 table of §4.4.1; `TestRewindGuardProbesEverySentinel` extended to v9.

**`internal/workflow` (`validate_test.go`)**: V25a, V21a–V21d with a fake
resolver — including **V21d's negative** (a threshold with no `payload`
registers clean) and the ordered-operator refusal naming the schema;
`TestFixtureRegistersClean` unchanged and passing; `TestValidationTableIsComplete`
as set equality.

**`internal/engine` (`saga_test.go`)**: C1–C6 of §4.8 — the unchanged
schema-less path, a path-precise refusal, the five-error cap, the missing-file
refusal, `AUTH_ERROR` precedence, and a **version-unchanged assertion** after
every refusal.

**`internal/cli` (`schema_test.go`)**: each verb's exit code and `.code`;
`schema list` as a `Collection` under `--json=v2`; `schema show --body` byte
equality with the registered file.

**QA (`test_zg_workflow.sh`)**: the group-1 dormancy proof (§3) and the 4→9
fixture protocol; `docket schema register|list|show` refusal matrix by exit
code **and** `.code`, each followed by a version-unchanged assertion.

---

# Group 2 — live thresholds, the real runner, `aggregate`, and held steps

Commit group 2. This is the stage's terminus: stopping after it leaves stage 5
shipped in full.

## 5. Threshold evaluation, upgraded

engine-spine §6.14 pre-decided this upgrade in its own words:

> **Where S5 upgrades this**: T1 gains schema-typed comparison, T2/T3 become
> live ordered comparisons for `ordered_enum` fields (T3 survives, applying
> only to fields that are *still* schema-less at S5), T4 is unchanged.

Implemented clause by clause, against that table and no other:

| # | S3/S4 behavior | S5 behavior |
|---|---|---|
| **T1** | `==`/`!=` compare as strings after JSON scalar normalization | **unchanged for schema-less fields.** For a **declared** field, comparison is schema-typed: a `string` enum compares by value identity; a `number`/`integer` field compares numerically after normalization; a `boolean` compares as a boolean. The `null` rule is **unchanged and still surprising on purpose**: null equals nothing, including null (a missing field and an explicit null are both "no value here") |
| **T2** | ordered operators do not evaluate | **they evaluate**, over `Position` (§4.3 I3), for fields whose pinned schema declares `ordered_enum`. `a >= b` is `Position(field, a) >= Position(field, b)`, and there is no other definition |
| **T3** | an ordered comparison attempted against a schema-less field routes `waiting-human` with a recorded reason | **survives, narrowed.** It now applies to (i) a step with no declared `payload`, (ii) a declared field with no `ordered_enum`, unreachable via V21c but reachable from a restored database, and (iii) I4's value-not-in-the-declared-order. The reason string names which of the three it was, and the S3 wording is kept for (i) minus its "which stage 5 supplies" tail — **that tail is removed in the same commit**, because a message promising a future stage after that stage has shipped is worse than no message |
| **T4** | aggregation over an empty payload set is the ordinary convention, evaluated **before** T3 | **unchanged, verbatim.** `any` over zero payloads is false, `all` is true, `count>=n` holds iff `n == 0`, and it short-circuits before any comparison is attempted. This is why an action step with no held clusters and an empty result set still routes `pass` without consulting an order |

**Every §6.14 test that exists keeps passing.** `internal/engine/threshold_test.go`
is **extended, never rewritten**, and the group's review bar is that the diff
adds cases and changes no existing expectation. Two additions make the survival
mechanical:

- `TestS3ThresholdSemanticsAreUnchangedForSchemalessFields` re-runs the full
  S3 case table against the S5 evaluator with an **empty** schema resolver, and
  asserts identical results — routing, reason (modulo the removed tail, which
  is asserted by prefix), and parked flag.
- `TestOrderedComparisonNeverGuesses` drives every operator against a field
  that is ordered, unordered, and absent, and asserts that the unordered and
  absent cases **park** rather than produce a verdict. A comparison that
  returned an answer here would be core inventing an order, which is the one
  thing this stage exists not to do.

**Evaluation order within `EvaluateThreshold` is unchanged**: routings in
`ThresholdOrder`'s deterministic sequence, first match routes, no match ⇒
`pass`. The schema is threaded in as a resolver parameter, not read from a
package-level variable — the same purity discipline as §4.9.2, and what lets
the table tests run without a database.

## 6. The real `ActionRunner`

`internal/engine/action.go`'s seam is honored: `NewEngine` swaps
`Actions: StubRunner{}` for `Actions: NewActionRunner(...)`. §6.4 records
honestly what else moves.

## 6.1 The builtin registry

```go
// Builtins are the actions core computes itself. There is exactly one, and
// §2 names it: "One is builtin and generic: action = \"aggregate\"".
var builtins = map[string]builtinFunc{"aggregate": runAggregate}
```

| # | Clause |
|---|---|
| B1 | Resolution is **builtin first**: an action named `aggregate` is computed by core and never consults the trust store. The name is therefore effectively reserved, and V27 (§7.1) says so at register time rather than letting an operator wonder why their trusted `aggregate` command never ran |
| B2 | A builtin **spawns nothing**. The §6 rule ("no subprocess ever executes inside a transaction") holds trivially, and the action stage's position — outside the routing transaction, exactly where S3 put it — is unchanged |
| B3 | A builtin's failure (bad params, an unorderable value) is a **step failure**, routed per the step's effective `on_fail`, with the reason recorded. It is **not** an engine error that aborts the saga: a workflow authoring mistake must not wedge a run |

## 6.2 A non-builtin action is a user-trusted command

§2, verbatim: *"Other computations remain user-trusted commands receiving step
context on stdin."* §4's trust model applies **with no exceptions**, and the
implementation is `internal/exec` and `internal/trust` **called, not
re-implemented**.

| # | Clause | Inherited from (docs/tdd/gates-trust.md) |
|---|---|---|
| A1 | The action name is looked up in the live trust store, once per action stage, as an immutable snapshot; the matched entry's own argv is what executes | §7.2 M1 |
| A2 | Candidates are entries whose `name` equals the action name **and** whose repo binding matches this repo | §7.2 M2 |
| A3 | **No entry ⇒ `unmatched`. Nothing spawns.** The action records `verdict = 'unmatched'` with NULL `argv`/`exit` and a `reason`, and **the step routes per its effective `on_fail`** — the same direction gates take (§6.2 N3): a computation that could not run has not succeeded | §4; §6.2 N3 |
| A4 | Env allowlist, `TERM=dumb`, `CI=1`, `DOCKET_REPO`, and the **`DOCKET_TOKEN` exclusion with its pre-spawn assertion** apply unchanged. (The token has already retired by the routing stage, so the exclusion is belt-and-braces here — which is exactly why it is asserted rather than assumed) | §5.3 |
| A5 | `argv[0]` resolution, `exec.ErrDot` refusal, and the **repo-containment rule R1–R5** apply unchanged and unconditionally | §5.2.1 |
| A6 | Timeout with **process-group kill**, capture at 256 KiB with `truncated`, and no shell, ever | §5.4, §5.5, §5.1 |
| A7 | A trust entry may declare `tree = true`; the action then takes the **same** per-repo `flock` (`.docket/tree.lock`) a tree gate takes. A computation that reads the working tree races a build exactly as a gate does | §7.4 |
| A8 | `flaky` re-runs apply, recorded individually with ascending `ordinal` in `action_results` | §5.6 |

**The stdin contract, pinned** (§2 says "receiving step context on stdin" and
stops; the rest is this TDD's to specify):

| Direction | Shape |
|---|---|
| **stdin** | the §11.4 `context` object, exactly as `docket step context --json` emits it, one JSON document, then EOF |
| **stdout** | one JSON object `{"body": "<string>", "payload": [ … ]}`. `body` defaults to `""` when absent; `payload` defaults to `[]` |
| **exit 0** | the action succeeded; the artifact records with `kind = params.output` |
| **non-zero exit** | the action **failed**; the step routes per its effective `on_fail`, the captured output is recorded in `action_results.output`, and **no artifact is written** |
| **unparseable stdout on exit 0** | a failure with `reason` naming the parse error and quoting the first 200 bytes **through gates-trust §5.7's renderer** — this output is attacker-influenced and reaches a terminal |

**Why an object rather than "stdout is the payload"**: an action that wants to
record a human-readable body (which every artifact has) would otherwise have no
channel for it, and inventing one later would be a wire-shape break for
commands already written. One shape, both halves, from the start.

**The payload is validated** exactly as a worker's is (§4.8): against the
step's declared `payload` schema when it declares one, plus §7.6's `aggregate@1`
when the action is `aggregate`. A non-builtin action's payload failing
validation is a step failure routed per `on_fail`, not a refusal to the
caller — there is no caller; the engine is the caller.

## 6.3 `action_results`, `trust_cache.kind`, and the stub's retirement

**v9 DDL slice — group 2:**

```sql
CREATE TABLE IF NOT EXISTS action_results (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step_id       INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
    action        TEXT    NOT NULL,
    ordinal       INTEGER NOT NULL DEFAULT 0,   -- flaky re-runs (§5.6 F3)
    argv          TEXT,                          -- NULL for a builtin/unmatched
    exit          INTEGER,                       -- NULL for a builtin/unmatched
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    output        TEXT    NOT NULL DEFAULT '',
    truncated     INTEGER NOT NULL DEFAULT 0,
    verdict       TEXT    NOT NULL,              -- 'pass' | 'fail' | 'unmatched'
    builtin       INTEGER NOT NULL DEFAULT 0,
    reason        TEXT,
    created_at_ms INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_action_results_identity
    ON action_results(step_id, action, ordinal);

ALTER TABLE trust_cache ADD COLUMN kind TEXT NOT NULL DEFAULT 'gate';
ALTER TABLE steps       ADD COLUMN materialized INTEGER NOT NULL DEFAULT 0;
```

**`action_results` is `gate_results` for actions, deliberately.** The
alternative — recording nothing and leaving the routing reason as the only
trace — makes an `unmatched` action invisible in exactly the way T11's audit
argument says it must not be, and makes a failed computation
indistinguishable from a failed threshold in a run report. Same shape, same
`ordinal` semantics, same `reason` discipline, so `run report` (S6) reads one
pattern twice rather than two patterns once.

**`trust_cache` gains `kind` rather than a second table.** The table answers
*"what did this run consider trusted, and when"* (gates-trust §4.5); splitting
that answer across two tables by the *shape of the caller* would make the one
question two queries. `kind` defaults to `'gate'`, so every existing row keeps
its meaning without being rewritten, and actions insert `'action'`. It remains
an **audit record, never an authorization shortcut** — every action consults
the live store on every execution, for the revocation reason §4.5 gives.

**The stub retires, per the S4 pattern**, and the pattern's important half is
that **history is not rewritten**:

| # | Clause |
|---|---|
| S1 | `StubRunner` is deleted with its constructor call. `TestNoStubActionRunnerRemains` is a source-level guard, in the family of gates-trust §5.1's |
| S2 | Real action artifacts record their payload as the **plain array**, with the marker in the new `artifacts.stub` column (`0`). The S3/S4 `{"stub":true,"payload":[…]}` **wrapper is never rewritten** — the migration sets `artifacts.stub = 1` for rows whose payload carries the wrapper, and leaves the bytes alone. Rewriting them would destroy the evidence that a computation did not run, which is the entire reason the marker exists (gates-trust §4.3 G2's argument, applied to artifacts) |
| S3 | `parsePayload` gains one documented tolerance: an object with exactly the keys `{stub, payload}` is unwrapped and its `payload` used. A bare array is used as is. This exists to read **history**, and its test drives a v8-era wrapped artifact |
| S4 | `stub` is `omitempty` in the artifact's JSON, so a result this stage produces serializes with **no `stub` key at all**, matching `gate_results`' rule exactly |

## 6.4 What moves beyond the constructor call — honestly

engine-spine §6.13 promised: *"the saga is written against the interface, so S5
changes one constructor call and nothing else."* gates-trust §7.1 set the
precedent of auditing such a promise rather than repeating it. The promise
**holds for the action seam's invocation** — `runRoutingStage` calls
`e.Actions.Run(...)` outside any transaction with a fully-populated
`ActionSpec`, and that call site is unchanged. Four things move, each a
requirement rather than drift:

| # | What moves | Why it is not avoidable |
|---|---|---|
| M-a | **`ActionResult` gains `Held []int` and `Results []ActionResultRow`** | §2 requires the held-cluster outcome and §6.3 requires per-attempt records; a seam that could only return a payload could express neither |
| M-b | **The routing stage gains a `held` branch** and the saga a `held` stage (§7.7) | §2's "materializes a `type=human` step … gating the routing step" is a *deferral* of routing, and a routing stage that always routes cannot defer |
| M-c | **`DecideStep` gains a materialized-step branch** (§7.7) | `approve`/`reject` on a materialized step resumes another step's saga, which the S3 path had no reason to do |
| M-d | **Stage 0 gains schema validation** (§4.8) and `EvaluateThreshold` gains a resolver parameter (§5) | both are this stage's subject |

Recorded as a **note on DKT-5** (§11 A7), not as a spec amendment: no
engine-spec line is deviated from. What does **not** move is asserted:
`TestSagaStageBoundariesUnchanged` is extended with the `held` stage and
otherwise compares the S4 golden byte for byte, and
`TestRoutingTransactionSpansFour` (step, issue mirror, run, events) is
unchanged.

## 7. `aggregate`, fully specified

§2, verbatim, is the specification:

> One is builtin and generic: `action = "aggregate"` with
> `params = { field, method = median|max|min, hold_spread, output }` computes
> over any ordered-enum payload field — median, spread-hold, and a recorded
> demotion trail work for severities, priorities, or tiers alike. When
> `hold_spread` trips, the engine materializes a `type=human` step named
> `<step>-held` gating the routing step, and the aggregate's output payload —
> per-cluster value, members, held flag, `demoted_from`, `operator_resolved` —
> validates against the shipped `aggregate@1` schema.

## 7.1 Params, and their register-time validation

| Param | Type | Required | Meaning |
|---|---|---|---|
| `field` | string | yes | the payload property aggregated over |
| `method` | `median` \| `max` \| `min` | yes | the reduction |
| `hold_spread` | integer ≥ 0, default 0 | no | hold when spread **≥** this; `0` never holds |
| `output` | string | yes (already V11's, §4.3.1) | the produced artifact kind |

New register-time rules, each a `VALIDATION_ERROR` naming workflow, step, and
param, each a test case:

| # | Rule | Argument |
|---|---|---|
| V27 | a step's `name` may not end in **`-held`**, and `action` may not name a builtin other than `aggregate` | the first reserves the materialized identity (§7.7) so a definition cannot collide with one; the second turns "my trusted `aggregate` command never runs" into a register-time sentence |
| V28 | `action = "aggregate"` requires `field`, `method ∈ {median,max,min}`, `output`; `hold_spread` an integer ≥ 0 if present; **no other keys** | the discipline every V-rule follows. A typo'd `method = "medain"` is otherwise discovered hours into a run, on a step whose inputs are already spent |
| V29 | an `aggregate` step must declare `payload = name@version`, and that schema must declare `params.field` as **`ordered_enum`** | median, max, and min are all defined **only** over an order. An aggregate without a declared order is a step that can never compute, and §11.2's own restriction is the same restriction |
| V30 | the declared schema must **accept an aggregate-shaped document**: a synthetic probe built from the schema's own declared enum values (§7.6) is validated against it at register time | the output must satisfy *both* the instance schema and `aggregate@1` (§7.6). An instance schema with `"additionalProperties": false` makes that conjunction unsatisfiable — and the failure would otherwise land at the end of a review fan-out, hours in. The probe is deterministic and invents nothing: every value in it comes from the schema being checked, and it carries the aggregate output keys (`members`, `held`) plus one carried-through extra key so the conjunction it tests is the real one (review F3) |

**V29 is a deviation** — §11.1 makes `payload` optional — and is filed as an
amendment (§11 A4). The alternative considered and rejected: inferring the
order from the schema of the step the aggregate reads its input from. That is
magic (nothing in the grammar says an action reads its predecessor's payload),
it is unstable under `inputs` edits, and it hides the one declaration an
author most needs to see.

## 7.2 The input shape: what a "cluster" is

§2 promises the output is "per-cluster value, members, held flag", and
`params` carries **no grouping key**. engine-core §6 explains why: clustering
is the *judged* half, performed by a synthesizing worker; reconciliation is the
*computed* half. So the grouping arrives in the payload, and this TDD pins how:

> **Each element of the input payload array is one cluster. The element's
> `field` value is either a scalar or an ARRAY of ordered-enum values — the
> cluster's members. `aggregate` reduces the array to a single value of the
> same field.**

| # | Clause |
|---|---|
| G1 | `field` holding an **array** ⇒ the members are its elements, in payload order (order does not affect any result; §7.3 sorts by position) |
| G2 | `field` holding a **scalar** ⇒ a one-member cluster. The median/max/min of one value is that value, the spread is 0, nothing is held, and nothing is demoted |
| G3 | Every **other key of the element is carried through verbatim** into the output element. Core does not read them (genericity), and dropping them would strip an instance's own identifiers from the very payload a downstream step consumes |
| G4 | A member value **not present** in the declared order (§4.3 I4) is a step failure with a reason naming the value and the field. **Not** sorted, not ignored |
| G5 | An **empty** members array is a step failure naming the element index — an empty cluster has no median, and inventing one (null? the lowest?) is exactly the kind of guess this stage exists to refuse |

**G2 is the property that makes the machinery safe to adopt**: over a flat,
unclustered payload — every element with a scalar field — `aggregate` is the
**identity**. Every value passes through, `held` is false everywhere, the
threshold sees what it would have seen without the action, and an operator can
introduce clustering later without a behavior cliff.

**Filed as an amendment (§11 A2)** proposing §2 gain one sentence naming the
member-array shape, because a reader of §2 alone cannot derive it.

## 7.3 median / max / min, and the even-count tie rule

Values are compared **only** by `Position` (§4.3 I3). Let *m* be the cluster's
member positions, sorted ascending.

| `method` | Result |
|---|---|
| `min` | the value at `m[0]` |
| `max` | the value at `m[len-1]` |
| `median` | the value at **`m[(len-1)/2]`**, integer division — or `m[len/2]` when the field declares `conservative_end = "upper"` (§4.2 O6) |

**The DEFAULT even-count tie rule is `m[(len-1)/2]` — the LOWER of the two
central values — and the argument is not "conservatism".**

1. **Core does not know which end is worse.** `ordered_enum` declares position,
   not significance (§1.1). A rule justified by "take the more severe of the
   two" would require core to believe the top of a declared order is the bad
   end — an opinion about severities baked into a statistics function. For a
   `ripeness` enum or a `confidence` enum that opinion is simply wrong, and it
   would be invisible.
2. **One expression, no special case.** `(len-1)/2` is the exact middle for odd
   counts and the lower middle for even ones. A rule with an `if len%2 == 0`
   branch has two behaviors to get wrong and two to test; this has one.
3. **It is the standard *lower median*** for ordinal data, where no arithmetic
   mean exists. There is nothing to average — "medium and high" has no midpoint
   unless the author declared one — so a positional convention is the only
   honest reduction, and the lower median is the conventional one.
4. **It is deterministic and order-independent**, which §9 item 5 requires:
   the same members in any input order give the same result.

The rule is stated in `SKILL.md` in exactly these terms, because an author
choosing `hold_spread` needs to know which way an even split falls.

### AMENDMENT (DKT-267) — a declared order may break its own tie

All four arguments above survive intact; only the first one's *conclusion* was
too strong. Core does not know which end is worse (1) — but the ORDER does, and
§4.2 O6 gives it a place to say so. When the field under aggregation declares
`conservative_end = "upper"`, `median` takes `m[len/2]`; otherwise it takes
`m[(len-1)/2]` exactly as before.

| Argument | Still holds? |
|---|---|
| 1. Core holds no opinion about severities | **Yes.** The opinion is read from the author's document, in one decision, and core still cannot enumerate or interpret the values |
| 2. One expression, no special case | **Weakened, deliberately.** There are now two expressions behind one `if`. The cost is one branch; the alternative was every severity workflow migrating to `method = "max"`, which over-reduces the non-tied clusters |
| 3. The standard lower median for ordinal data | **Yes, as the default.** An undeclared order computes the lower median, and `"lower"` declared explicitly must compute identically — pinned by a test, so the two can never become three behaviors |
| 4. Deterministic and order-independent (§9 item 5) | **Yes.** The tie-break is a function of the declaration and the member count. Neither is affected by arrival sequence |

**Containment.** The direction reaches the even-count median and nothing else:
not `min` or `max` (which name an end explicitly, so a direction there would
overrule the author with the author's own annotation), not an odd-count median
(no tie exists), not a threshold predicate, not `spread`, not a hold. A workflow
that wants the top of the order in every case still writes `method = "max"`.

**No schema version was required.** The direction is derived onto the FIELD
TABLE, never onto the ordered index, so `schemas.ordered` is byte-identical with
and without it (§4.3 I2) and every stored row and pinned re-derivation is
untouched. The engine recompiles a pinned schema from `schemas.body`, so a
direction added to a new schema version reaches a run through the ordinary
pin — never retroactively into a live one.

## 7.4 Spread, and the `hold_spread` trip

```
spread = Position(max member) − Position(min member)
held   = hold_spread > 0 && spread >= hold_spread
```

**Distance in the declared order**, never a count of distinct values and never
a value difference: with `["info","low","medium","high","blocker"]`, a cluster
of `{low, high}` has spread 2, and so does `{low, medium, high}`. The
comparison is `>=`, matching engine-core §6's *"clusters with spread ≥ 2 levels
are held"* with `hold_spread = 2` — the fixture's own number.

`hold_spread = 0` (the default, and the absent case) **never holds**, so an
aggregate step that does not opt in behaves exactly as it did before this
clause existed.

## 7.5 The demotion trail

> every demotion records `demoted_from`

| # | Clause |
|---|---|
| D1 | A cluster is **demoted** when its computed value's position is **strictly below** its maximum member's position |
| D2 | `demoted_from` records the **maximum member value** — the value that was not taken. Absent (omitted from the object) when there was no demotion |
| D3 | `method = "max"` never demotes; `min` demotes whenever the members differ; `median` demotes on any cluster whose upper half is non-trivial |
| D4 | The trail is per **cluster**, in the output element, and is therefore addressable forever: artifacts are immutable, so the record of what was not taken outlives the run |

`demoted_from` is engine-spec §2's own word and carries no domain: it names a
**position in the declared order that was not the result**, which is true for
ripeness grades and for severities alike.

## 7.6 The output payload, and `aggregate@1`

One output element per input element:

```json
{
  "<field>": "medium",
  "members": ["low", "medium", "high"],
  "held": true,
  "demoted_from": "high",
  "operator_resolved": false,
  "<…every other input key, verbatim…>": "…"
}
```

**`aggregate@1`, shipped and embedded** under `internal/schema/schemas/`
(`//go:embed` — under `internal/` because the Vorpal build's include list
requires it, the same binding repo fact that puts the workflow templates
there):

| # | Clause |
|---|---|
| E1 | It declares an **array** of objects requiring `members` (a non-empty array) and `held` (boolean), with `demoted_from` and `operator_resolved` optional, and **`additionalProperties: true`** — because the instance's own field key and G3's carried-through keys are the point |
| E2 | It **names no instance token.** It cannot name `field`, since the key is the author's. The genericity gate scans it (§1.1.1) |
| E3 | It is **auto-registered by seeding**: `migrateV8ToV9` inserts it with `builtin = 1` and `INSERT OR IGNORE`, so the rewind guard's re-run is safe (U5) and a divergent pre-existing row is left alone (U7) |
| E4 | It is **immutable like any other `name@version`**: editing the embedded bytes requires `aggregate@2` and a code change that consumes it. `TestEmbeddedAggregateSchemaMatchesItsGolden` compares the embedded bytes' SHA-256 against a constant in the test, so an edit is a deliberate act with a failing test attached rather than a silent drift from every already-seeded database |
| E5 | The aggregate's output is validated against **both** `aggregate@1` and the step's declared `payload` schema (V29 guarantees one exists). V30 makes the conjunction's satisfiability a register-time question rather than a runtime surprise |

**Why seed in the migration rather than register lazily at first use**: a lazy
registration writes to the database from a read path, races itself under
concurrent engine invocations, and makes `schema list` depend on what has
happened rather than on what is installed. Seeding is one idempotent insert in
the transaction that already exists.

## 7.7 HELD: materialization, and how a materialized step enters the status machine

This is the stage's genuinely new machinery, and §2 gives it one sentence. Every
clause below is this TDD's, and each is a test.

### 7.7.1 Identity and creation

| # | Clause |
|---|---|
| H1 | **Name**: `<step>-held`, where `<step>` is the routing step's `step_name`. V27 reserves the suffix, so it cannot collide with a declared step |
| H2 | **Ordinal**: the routing step's own ordinal *k*, rendering `reconcile-held@1` for a hold at loop ordinal 1. **Suffixed with the held cluster's payload index**, rendering `reconcile-held@1#2` — see H2a *(amended 2026-08-07, DKT-15; this clause previously read "No sibling index, ever")* |
| H2a | **One step PER HELD CLUSTER** (DKT-15). The original clause reasoned from §11.1's exactly-one-of rule: an `action` step is never fanned out, so its hold has no sibling. That is true of the ROUTING step and does not follow for the clusters inside its payload, which are genuinely plural. With one step for the whole hold, `approve`/`reject` is a single bit spent on several independent questions — RUN-1's round-2 hold carried four clusters, the operator wanted two escalated and two accepted, and recorded the protest in the approve note because it was inexpressible. The index is the element's **position in the payload**, not a counter over held elements: position is stable across re-reads of the same immutable artifact, so a resumed saga re-derives the same instance for the same cluster, whereas a counter would renumber and silently reattach an answer to a different cluster. A cluster that was never held has no step, so a hold tripping only on the second element materializes `#1` and no `#0` |
| H3 | **Kind**: `human`. It is `type="human"` in every respect the verbs can observe: `approve`/`reject` apply, `claim` refuses with `CONFLICT` naming the class, and it is offered by `next --run` with no `executor` |
| H4 | **Row**: `steps.materialized = 1`, `workflow_id` and `run_id`/`issue_id` from the routing step, status `pending`, created in the **same transaction** that records the aggregate's artifact and enters the `held` stage. One transaction, or a crash leaves a held payload with nobody able to resolve it |
| H5 | **Spec**: synthesized by `workflow.MaterializedHeldStep(def, name)` from the **pinned** definition — kind `human`, no `after`, no gates, no threshold. Nothing unpinned enters a run: the synthetic spec is a pure function of the pinned bytes plus the suffix, so §9 item 5's determinism is untouched. Every `spec == nil` site (`saga.go`, `human.go`, `ready.go`) routes through this resolver instead of erroring, and `TestMaterializedStepResolvesFromThePinnedDefinition` proves the derivation |
| H6 | **Readiness**: R1/R2/R6 apply; R3 is vacuous (no `after`); R4/R5 do not apply to a human step (engine-spine §6.15). So it is ready the moment it exists, which is correct: the question it asks is already answered by data on disk |
| H7 | **Idempotent**: at most one `<step>-held@k` per (run, issue, step, ordinal), enforced by the existing `UNIQUE(run_id, issue_id, instance)` index. A resumed saga that re-enters the held branch finds it and does not duplicate |

### 7.7.2 Gating the routing step

| # | Clause |
|---|---|
| H8 | The routing step **does not route while held.** Its saga advances to a new stage `held` and stops; its status stays **`gated`**, which is non-terminal, so **every downstream successor fails R3** and nothing proceeds. That is "gating the routing step" implemented with the machinery that already exists — no new status, no synthetic `after` edge, no second readiness rule |
| H9 | `ResumeSaga` treats `held` as **"no advance"**: `advanceOne` returns `advanced = false` and the resume returns cleanly. Every later `next`/`claim`/`complete` invocation therefore costs one read and changes nothing, rather than spinning |
| H10 | **The threshold is evaluated exactly once, after resolution — not before.** engine-core §6 says held clusters are *"excluded from automatic routing"*; the alternative reading (route now over the unheld subset, re-route after resolution) requires **un-routing**, and there is no un-route: a `fix-loop` entry supersedes instances and increments a counter. One evaluation over the resolved set is the only reading that does not require undoing a loop entry |
| H11 | **`guard stop` treats a held decision as pending work — it DENIES.** The materialized step is `pending`, and §6.12's rule stands unchanged: a held cluster blocks stop exactly as a declared `type=human` gate awaiting approval does — H12's consistency argument, applied in both directions. Stopping requires resolving or abandoning. `TestGuardStopDeniesWhileHeld` pins it. *(Reviewed decision 2026-08-03, replacing an earlier exemption that contradicted H4/H12 — payloads-thresholds-review.md F1)* |
| H12 | The **run rollup is unchanged**: a held routing step is `gated` and its materialized step is `pending`, so the run stays `active`. This matches the existing treatment of a declared `type="human"` gate awaiting approval, and matching it is the point — two kinds of open human decision that rolled up differently would be a bug waiting for a report to expose |

### 7.7.3 Approve and reject

| Verb on `<step>-held@k#i` | Effect |
|---|---|
| `docket step approve` | (1) a **new artifact** of the same kind is recorded on the **routing step**, whose payload is the held one with **cluster *i*** carrying `"operator_resolved": true`; (2) the materialized step ⇒ `done`, `step-approved` event with the note; (3) once **every** cluster step is terminal, the routing step's saga advances `held` → `routing` and **the threshold applies over the resolved set** — `stepPayloads` already reads the step's latest artifact (`ORDER BY id DESC LIMIT 1`), so the resolved payload arrives with **no new read path** |
| `docket step reject` | (1) **no new artifact** for that cluster, which keeps `"operator_resolved": false`; (2) the materialized step ⇒ `done`, `step-rejected` event with the note; (3) once every cluster step is terminal, the routing step routes per its **effective `on_fail`**, skipping the threshold |

| # | Clause |
|---|---|
| H12a | **The reduction over clusters** *(DKT-15)*. RESOLVED means every cluster step is terminal — a partial answer leaves the saga waiting, because routing on it would route over a cluster nobody decided. APPROVED means every one was approved; **one rejection routes per `on_fail`**, which is both the conservative reduction (a mixed answer must not silently pass) and the same outcome a single rejection produced when there was one step. The per-cluster dispositions survive the reduction: each step keeps its own status, routing, and note, and only approved clusters are marked `operator_resolved`, so what routes is one decision while the record stays per-cluster |
| H12b | **Ordering inside the approve transaction** *(DKT-15)*. The decision is written to the cluster step **before** the payload is resolved, because resolution now reads each cluster step's own status to decide which elements to mark — with the old order the cluster being approved reads as undecided and nothing is marked. Under the previous whole-hold shape the order was immaterial, since approval was inferred from the caller rather than read back. Both remain in one transaction, so §7.7.3's atomicity is unchanged |
| H13 | **Artifacts stay immutable** (engine-core §1.4). Approval records a **new** artifact rather than annotating the old one, which is the same rule a step re-run follows. The held payload remains addressable forever: what the engine computed and what the operator accepted are two records, not one overwritten one |
| H14 | The materialized step ends **`done` on both** approve and reject — it recorded a decision, which is what a gate does. The *consequence* lands on the routing step. This is also why V13's rule (a human gate may not route rejects to `waiting-human`) is not violated when the routing step's `on_fail` **is** `waiting-human`: the park is on a **different** step, resolvable by `step resolve`, not a step parking on its own decision |
| H15 | Both verbs are **token-free**, per §2 — a human gate is resolved by an operator who never claimed it |
| H16 | `approve`/`reject` on a materialized step whose routing step is **not** in `held` (a double approve, a resumed race) is `CONFLICT` naming both steps. The saga advance is CAS-guarded on `saga_stage = 'held'`, so the loser writes nothing |

### 7.7.4 §11.3 interplay

| # | Clause |
|---|---|
| H17 | A materialized step is **in its routing step's lineage**: the supersede sweep at a loop entry treats `<step>-held@k` exactly as it treats any non-terminal instance downstream of `after_loop` — it becomes `superseded` (terminal, event-logged), and an open held question from a superseded ordinal does not survive to block ordinal *k+1* |
| H18 | **Re-instantiation never creates a held step.** `ExpandOrdinal` walks the pinned definition, and a materialized step is not in it. A hold at ordinal *k+1* materializes only if the aggregate at ordinal *k+1* trips `hold_spread` again — which is correct: the question is about *that* ordinal's computation |
| H19 | **Issue completion** (`issueStepsComplete`) already groups by `step_name` and takes the highest ordinal per name, so `reconcile-held@0` left `superseded` by a loop entry is not consulted, and `reconcile-held@1` is. No change to that query is needed, and `TestHeldStepsParticipateInCompletion` proves it rather than assuming it |
| H20 | A **stale-lineage** hold is inert exactly as a stale-lineage routing is (`StaleLineage`): the aggregate still records its artifact and its `action_results` row (history is attributed), and it neither materializes a held step nor gates anything, because nothing downstream of it will run |

## 7.8 What core will not do: read the operator's note

engine-core §6 says the operator's note *"records the disposition, e.g. an
accepted severity"*. **Core does not parse it, and this stage says so
explicitly**: extracting `"high"` from a free-text note would be core reading
instance meaning out of prose — the exact prohibition the genericity rule
exists to enforce, arriving through a convenience.

So `approve` means **accept the computed value** despite the spread; `reject`
means **route per `on_fail`**. The note is recorded verbatim on the step's
routing and in the event, for humans and for a retro to read. §13 records the
shape a future amendment would take (`step approve --set <field>=<value>`,
validated against the pinned schema's declared order — a *typed* channel, not a
parsed one).

## 8. QA: the fixture comes alive

### 8.1 `test_zg_workflow.sh` — the loop driven as designed

The S3/S4 loop run is driven by `verify`'s **equality** threshold
(`any(status == unmet)`), because at S3 `reconcile`'s ordered predicate could
not evaluate and its stub payload was empty. **That case stays, unchanged** —
it is the T1 path and the only proof that equality still needs no schema.

Added alongside it (ZG gains a second driver, not a replacement):

1. `docket schema register findings@1 scripts/qa/fixtures/schemas/findings@1.json`
   — an **instance** schema declaring `severity` as an `ordered_enum` over five
   values. It lives in `scripts/qa/fixtures/`, which the genericity gate does
   not scan, because it is instance data (§1.1.1).
2. Activate; assert the run's pins include `schema findings@1` **and**
   `schema aggregate@1` with the right hashes (§4.7 P1/P5).
3. Drive `implement` → the `review` fan-out → `synthesize`, whose payload
   carries **clustered** findings: elements whose `severity` is an array of the
   reviewers' values (§7.2).
4. `reconcile` — the **real** `aggregate` — computes. Assert against a golden:
   per-cluster values, `members`, `demoted_from` on the demoted clusters, and
   `held` on the one whose spread ≥ 2.
5. Assert `reconcile-held@0` exists, is `type=human`, is offered by
   `next --run`, and that **no downstream step is ready** (H8).
6. `docket step approve` it; assert the resolved artifact carries
   `operator_resolved`, the saga advanced, and the threshold
   `any(severity >= high)` **routed `fix-loop`** — the ordered comparison
   evaluating for real, over a user-registered order, which is the sentence
   this whole stage exists to make true.
7. The loop entry proceeds exactly as the S4 golden has it, and the run
   reaches `done`.
8. **The negative twin**: a second issue whose clusters have spread 1 — nothing
   held, no materialized step, one threshold evaluation, straight through.
9. **The rejection twin**: approve replaced by `reject`; assert the routing
   step took its `on_fail`, no resolved artifact was written, and the held
   artifact is still addressable.
10. The comment in the ZG helper saying a green action step is **not** evidence
    the computation works (engine-spine §6.13 (c)) is **removed in the same
    commit** — it now asserts the opposite of the truth, which is worse than no
    comment.

### 8.2 The fixture edit

`docs/design/example-workflow.toml`'s `reconcile` step gains
`payload = "findings@1"` (V29). Its stage-sequencing NOTE at the top — *"threshold FIELD
validation against registered payload schemas arrives at stage 5; at stage 3
thresholds parse grammatically only"* — is rewritten to say that stage 5 has
landed and what the file now requires. A committed fixture whose comment
describes a superseded stage is drift of the same kind a stale SKILL.md table
is.

The edit is filed as an amendment (§11 A6) and is the operator's to apply
upstream (05 §1), exactly as engine-spine §4.3.2's `on_fail` edit was.
`TestFixtureRegistersClean` (pure, no resolver) is unaffected (§4.9.2); a new
`TestFixtureCrossValidatesAgainstTheQASchemas` covers the resolver path.

### 8.3 `test_zh_stranger.sh` — the stranger test re-earned

ZH is **extended, not replaced**. Its docs-review demo gains one schema and one
ordered threshold, in a domain with no agents anywhere: a `risk` field ordered
`low, medium, high`, a two-line schema file, and a workflow that routes
`any(risk >= medium)` to the human approval step. Assertions:

- the whole script's commands and every string the run printed contain **none**
  of `scripts/qa/genericity.sh`'s `BANNED` list, **sourced from the gate script
  itself** rather than restated (the S4 lesson — a restated list is how the gate
  erodes);
- the ordered comparison routes correctly with **no agent vocabulary anywhere**
  in the fixture, the output, or the docs the stranger read.

## 9. Test plan, per commit group

### 9.1 Group 1

§4.10 in full. Summarized obligations: the `internal/schema` tables (O1–O5,
I1–I4), the golden corpus, the no-cgo and pinned-draft guards, the three
registration outcomes, U1–U7, the V21a–V21d/V25a cross-validation table with a
fake resolver, C1–C6 of payload-at-complete, the CLI refusal matrix, and the
group-1 dormancy proof.

### 9.2 Group 2

**Threshold (`internal/engine/threshold_test.go`, extended)**: T1–T4 upgraded;
`TestS3ThresholdSemanticsAreUnchangedForSchemalessFields` (the §6.14 survival
suite); `TestOrderedComparisonNeverGuesses`; the three T3 sub-cases (no
`payload`, no `ordered_enum`, value outside the order).

**Aggregate (`internal/engine/aggregate_test.go`)** — table-driven goldens:

| Case class | Cases |
|---|---|
| median | odd counts; **even counts** (the `(len-1)/2` rule asserted directly, including the two-member `{low, blocker}` case an implementer would most likely get wrong); a one-member cluster (G2 identity); permuted input order giving an identical result |
| max / min | including "max never demotes" (D3) |
| spread | distance in the declared order, with `{low, high}` and `{low, medium, high}` both spread 2 |
| hold | `hold_spread` 0/absent never holds; `= spread` holds (the `>=` boundary); `> spread` does not |
| demotion | `demoted_from` present exactly when D1 holds, absent otherwise (asserted as **key absence**, not as an empty string) |
| failures | G4 (value outside the order), G5 (empty members), a non-array non-scalar field — each a **step failure routed per `on_fail`** with a reason, never an engine error (B3) |

**Held lifecycle (`internal/engine/held_test.go`)**: H1–H20, one case each.
The ones that carry the design: H8 (downstream not ready while held), H9 (a
resume while held advances nothing and writes nothing), H10 (the threshold
evaluates **once**, after resolution — asserted by counting evaluations), H13
(the held artifact is still addressable after approval), H16 (double approve is
`CONFLICT`), H17/H18 (loop interplay), H11 (`guard stop`).

**Actions (`internal/engine/action_exec_test.go`)**: A1–A8 driven through the
**same** `internal/exec` tables gates use — `TestActionsUseTheSameTrustPath`
asserts by call-graph that no second exec path exists;
`TestUnmatchedActionRoutesPerOnFail` with a witness command proving **nothing
spawned**; `TestDocketTokenNeverReachesAnActionChild`; the stdin contract
(context in, `{body, payload}` out); a non-zero exit; unparseable stdout
rendered through gates-trust §5.7's control-byte renderer; `tree = true`
serialization.

**Migration (`internal/db/migrate_v9_test.go`)**: the group-2 half of U1–U7 —
`action_results`, `trust_cache.kind`, `steps.materialized` — plus
`TestStubIsNotWrittenAtV9` and `TestWrappedStubPayloadsStillRead` (S3).

**QA**: §8's ZG additions, ZH's extension, the group-2 dormancy proof, and the
§9.5 golden extension (pins incl. schemas, and the sensitivity check).

**Feeds §9.5 (engine-spec §9 item 5)**: with schemas pinned (§4.7) and
aggregation deterministic and order-independent (§7.3, clause 4), *same run at
same pins ⇒ identical
validation verdicts and identical aggregate output*, which is the half of
determinism the engine could not previously claim about payloads.

## 10. Documentation, scheduled per group

CLAUDE.md's same-PR rule: a stale table blocks review.

| Group | Docs in the same commit |
|---|---|
| 1 | **SKILL.md**: a new `docket schema` verb/flag table in the reference section; `schema register|list|show` in the workflow-definitions narrative; the `ordered_enum` annotation with the §4.2 example; the `payload` row of the grammar table updated from "validated for shape now, against a registered schema later" to what it now does; the error codes for the new refusals. **architecture.md**: a new **§2.13 Payload schemas and thresholds** — the registry, the annotation, the derived order, pinning, and validation at `complete`; §3's version table gains the v9 row |
| 2 | **SKILL.md**: the `aggregate` params table, the **even-count median rule** in the terms §7.3 argues, `hold_spread` and the `<step>-held` step's lifecycle (what an operator sees and what `approve`/`reject` do), non-builtin actions as trusted commands with the stdin/stdout contract, and the `action result` shape. **architecture.md**: §2.8 "The action seam" rewritten from "everything is real except the computation" to the computation; the held-step machinery; `action_results`. **security.md**: §7's execution-trust section gains actions — *the same model, no exceptions* — and §8's env table is cited rather than duplicated |

## 11. Amendment issues filed by this stage

Per docs/design/amendments.md and CLAUDE.md: deviations become DKT issues
citing the exact line, never silent changes. A1–A6 are filed **before** this
TDD is committed, so the amendment trail exists ahead of the code, per the
A1/A2 precedent of engine-spine §10.

| # | Issue | Deviation | Spec line cited | Disposition |
|---|---|---|---|---|
| A1 | **DKT-22** | `docket schema list` and `schema show` are additive verbs; §1's surface line names only `schema register name@v schema.json` | §1 line `docket schema   register name@v schema.json  # payloads; ordered enums supported` | Additive read verbs, no semantic change. Argued in §4.5: activation auto-registers, so without a read verb an operator cannot see what a run pinned. Proposes §1 read `register\|list\|show` |
| A2 | **DKT-23** | §2's `aggregate` describes "per-cluster value, members" but `params` carries no grouping key and §2 never says how members arrive | §2 line "computes over any ordered-enum payload field … per-cluster value, members, held flag, `demoted_from`, `operator_resolved`" | Pins the member-array input shape (§7.2), with the scalar case as the identity. Proposes one sentence in §2 |
| A3 | **DKT-24** | §11.4 has no `action result` wire shape, while it has one for `gate result`; the implementation adds `action_results` and an `action result` shape, and extends `trust_cache` with `kind` | §11.4 block `gate result { step, gate, argv, exit, duration_ms, output, truncated, verdict, reason? }` | Additive shape mirroring `gate result`. Argued in §6.3: an `unmatched` action is otherwise invisible, which is the failure `verdict='unmatched'` exists to prevent for gates |
| A4 | **DKT-25** | §11.1 makes `payload` optional; V29 makes it **required** on `action = "aggregate"` steps | §11.1 `payload` row: "`schema@ver`, optional" | Narrow additional constraint on one step class. Argued in §7.1: median/max/min are defined only over a declared order, so an aggregate without one can never compute. Proposes the `payload` row gain "required on `action = \"aggregate\"` steps" |
| A5 | **DKT-26** | The step-name suffix `-held` becomes **reserved** (V27), a constraint §11.1's `name` row does not carry | §11.1 `name` row: "string, required, unique in workflow" | Reserves the identity §2 itself mints (`<step>-held`). Without it a definition can declare a step whose name collides with a materialized one. Proposes the `name` row gain the reservation |
| A6 | **DKT-27** | The committed fixture `docs/design/example-workflow.toml` gains `payload = "findings@1"` on `reconcile`, and its stage-sequencing note is rewritten | §11.1 `payload` row; the fixture's own NOTE block | Fixture edit, the operator's to mirror upstream (05 §1), exactly as engine-spine §4.3.2's `on_fail` edit (DKT-17). Argued in §8.2 |
| A7 | **note on DKT-5** | engine-spine §6.13's promise — "S5 changes one constructor call and nothing else" — holds for the action seam's **invocation**, and four things move beyond it (§6.4 M-a..M-d) | docs/tdd/engine-spine.md §6.13 | **No engine-spec line is deviated from.** Recorded as a comment on DKT-5 so the S3→S5 boundary claim is not quietly overstated, exactly as gates-trust §11 A7 did for DKT-3 |

## 12. Implementation phases — TWO commit groups

Direct commits on `feature/graph-engine` per the operator decision recorded in
reliability-delta §11: no branches, no PRs, **never** tags, linear history.
Draft PR #33 provides CI on every push. **Every commit leaves the branch
green.**

### Group 1 — `internal/schema` + `schema register` + payload-at-complete

| | |
|---|---|
| **Contents** | the validator dependency and its guards (§4.1); `internal/schema` — `ordered_enum`, the derived index, `Position`, the embedded `aggregate@1` (§4.2, §4.3, §7.6); v9's group-1 DDL — `schemas`, `artifacts.stub`, the seed, the sentinel/rewind extension, U1–U7 (§4.4); `docket schema register\|list\|show` (§4.5); schemas in the pin set (§4.7); payload validation at `complete` (§4.8); the V21a–V21d/V25a cross-validation seam (§4.9); the genericity gate's `.json` extension (§1.1.1); the `renovate.json` `packageRules` entry; §4.10's tests; §10's group-1 docs; amendment issues A1–A6 filed |
| **Not in it** | No threshold behavior change. No action behavior change. `StubRunner` still ships. No `action_results`, no held steps |
| **Green** | `go test ./...` plus `scripts/qa.sh` including the genericity gate |
| **Independently stoppable** | Yes: it ships a usable, documented schema registry and real payload validation over declared payloads. A repo stopping here has strictly more than S4 and nothing half-built |
| **Dormancy proof** (§3) | v8→v9 applied; `schemas` holds the one builtin row; the byte-compat sweep passes; a workflow with no `payload` declarations registers, activates, and runs **byte-identically to S4**, asserted against the S4 ZG golden; T1–T4 untouched because no threshold code changed |

### Group 2 — thresholds live, `aggregate`, held steps, trusted-command actions

| | |
|---|---|
| **Contents** | the §5 threshold upgrade and its survival suite; the real `ActionRunner` and its constructor swap (§6); non-builtin actions through `internal/trust` + `internal/exec` (§6.2); v9's group-2 DDL — `action_results`, `trust_cache.kind`, `steps.materialized` (§6.3); `aggregate` in full (§7.1–§7.6); held materialization and its lifecycle (§7.7); V27–V30; `guard stop`'s held behavior (H11 — held blocks stop, per review); the stub's retirement (§6.3 S1–S4); §8's QA (ZG's live loop, the fixture edit, ZH's extension); §9.2's tests; §10's group-2 docs |
| **Green** | `go test ./...` plus `scripts/qa.sh` including ZG, ZH, and the genericity gate |
| **Independently stoppable** | It is the stage's terminus; stopping here means stage 5 is shipped |
| **Dormancy proof** (§3) | `action_results` empty; a run with no `aggregate` step and no `payload` declarations reproduces the S4 ZG golden byte for byte; the T1–T4 survival suite passes with an empty resolver; a workflow-free repo unchanged by the byte-compat sweep |

**Why two groups and not four.** The seam is the one the work order names and
the one review actually wants: **a pure, dependency-bearing library plus a
registry** (reviewable without any engine context, and where the security and
supply-chain lens belongs) and **the engine wiring** (reviewable only against
the engine). Slicing group 2 further produces a commit with an `aggregate` that
computes and no way to resolve a held cluster, or a held step nothing
materializes — neither of which leaves the branch green in any meaningful
sense.

## 13. Deliberate non-goals

Recorded so a reviewer can see they were considered, and so a later amendment
starts from a shape rather than a blank page.

| Non-goal | Why not now | What a future amendment would look like |
|---|---|---|
| **Dotted/nested threshold field paths** | §11.2's grammar is a bare field token; indexing nested orders would build a map no predicate can query (§4.2 O5) | extend `predicateShape` to `a.b`, and the ordered index to JSON-Pointer keys, in one change |
| **`step approve --set <field>=<value>`** for a held cluster | engine-core §6's "an accepted severity" needs a **typed** channel; parsing it from a note is core reading instance meaning out of prose (§7.8) | `--set field=value`, validated against the pinned schema's declared order, recorded as the resolved value with the computed one retained |
| **More builtins** (`mean`, `mode`, `percentile`) | §2 names exactly one builtin and says the rest are user-trusted commands. Each addition is core acquiring an opinion about what is worth computing | a named builtin plus its params in §2, with the same register-time validation table |
| **A schema `deprecate`/`rm` verb** | schemas are immutable and pinned; deleting one breaks a completed run's ability to explain itself | a `retired_at_ms` column and a `list --all`, never a delete |
| **Cross-field predicates** (`any(a >= b)`) | §11.2's right-hand side is a literal, and comparing two fields requires deciding whether their orders are commensurable — a question only the schema author can answer | an explicit `comparable_with` annotation, refused by default |
| **Custom JSON Schema vocabularies** beyond `ordered_enum` | one annotation, one purpose, and it is not registered as a library keyword (§4.2 O4) so the validator stays swappable | a vocabulary URI and a keyword registry, with the corpus extended first |
