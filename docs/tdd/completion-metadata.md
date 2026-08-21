# TDD: completion metadata — `step complete --metadata` reaches the step (DKT-68)

Status: approved — 2026-08-05 · design review APPROVED as designed, with one
required addition (§1.1.1: the byte cap and its refusal stated explicitly, with
its own test in §4.2). Implements docs/design/engine-spec.md §11.4's
`complete args` line (`[--metadata '{…}']`), §5's `metadata` column row
("recorded on the step; delivered in the context bundle"), and engine-core.md
§8's tier-drift sentence ("the step records requested vs resolved … in its
metadata"). Tracker unit: **DKT-68**. Precedent: the M2a bind-to-highest patch
(DKT-40) — an engine defect surfaced by instance work, fixed in the engine.
Spec of record is engine-spec.md; deviations become DKT amendment issues per
docs/design/amendments.md, never silent changes.

**The defect is not a missing feature; it is a missing assignment.** Every
other part of the path already exists and is correct: the flag parses
(`internal/cli/step.go:310`), the option field is declared and documented as
"opaque KV merged onto the step's own" (`internal/engine/saga.go:62`), the
column exists (`steps.metadata`), and the R7 rollup reads that column correctly
and generically (`internal/db/rollups.go:147`). What is absent is the one write
that connects them. `opts.Metadata` has no reader anywhere in the engine.

That shape is why it failed silently, and silence is the part worth fixing
carefully. An instance emitting the four routing keys got exit 0 and an empty
rollup, and reasonably concluded its own emission was at fault (DKT-68 records
a real debugging cycle spent exactly there).

## 1. Mechanism

**Merge the completing worker's bag over the step's definition bag, in stage
zero's existing transaction.**

`stageZero` (`internal/engine/saga.go:226`) already holds the transaction in
which the artifact records, the token retires, the status moves, the event
records, and the usage ledger row lands. Completion metadata is one more fact
about the same event, so it commits with them or not at all. No new stage, no
new transaction, no subprocess — the complete-saga rules are untouched.

### 1.1 Validation is pre-transaction, with the size cap

Parsing happens **before** `conn.Begin()`, alongside R12's artifact cap and the
payload shape check, because §6.9's assertion is that a refusal writes nothing
and leaves `row_version` where it was.

Three refusals, in this fixed order (the same rationale C5 gives for fixing
R12-then-schema: an operator debugging one input should not get a different
message depending on which check ran first):

1. **Not valid JSON** → `VALIDATION_ERROR`, naming the parse position.
2. **Not a JSON object** → `VALIDATION_ERROR`. The bag is a KV map; an array
   or scalar has no keys to merge and no keys to roll up. R7 already skips
   non-object rows at read time (`rollups.go:167`), and that tolerance exists
   for rows written before any writer validated them — it is not a licence to
   write such rows now. Refusing at the write end is what keeps the tolerance
   from becoming the norm.
3. **Over a byte cap** → `VALIDATION_ERROR`, naming both numbers, in R12's
   exact phrasing style.

#### 1.1.1 The cap, stated explicitly

The cap is the one genuinely new policy value this patch introduces, so it is
named here rather than left to implementation.

```go
// MetadataMaxBytes caps one completion's `--metadata` bag.
const MetadataMaxBytes = 16 << 10   // 16 KiB
```

It is measured on the **raw incoming JSON text**, before parsing and before the
merge — the bytes the caller supplied, which is the number the caller can act
on. (Measuring the *merged* result would make a refusal depend on what the
definition bag already held, so an unchanged command could start failing
because a workflow author edited an unrelated file.)

The refusal, in R12's phrasing style — both numbers, and a remedy:

```
metadata is 21504 bytes, over the 16384-byte cap; record the detail in the
artifact or the payload instead
```

`VALIDATION_ERROR` (exit 3), pre-transaction, writes nothing.

**Why a cap exists at all, and why 16 KiB.** The bag is opaque, so nothing else
bounds it: without a cap a worker can push an artifact-sized body into a column
the R7 rollup then groups *by distinct value*, turning a report into a wall of
unique multi-kilobyte strings. That is a slow, silent degradation of the one
read surface this patch exists to feed. 16 KiB is chosen as roughly three
orders of magnitude above the motivating use (four short routing keys, ~120
bytes) — generous enough that no legitimate KV bag meets it, small enough that
the rollup stays legible. It is deliberately a **constant, not config**: a new
engine-config key is a surface addition needing its own spec line, and no
evidence yet exists that any instance needs a different value. `ArtifactMaxBytes`
sets the precedent for a constant cap with a named remedy.

**The remedy is real, not rhetorical** — a worker with genuinely large output
has two purpose-built channels (`--artifact-file`, `--payload-file`), and the
error names them. The cap pushes data to the right place rather than refusing
the work.

An **absent** `--metadata` (empty string) is not a refusal and not a write: the
step keeps its definition bag untouched. This matters for every existing
caller, none of which passes the flag.

### 1.2 The merge is last-write-wins per top-level key

The stored value is the definition bag with the completion bag's keys
overlaid — exactly what `saga.go:62` already promises with the word "merged".

- A key only the definition declares survives.
- A key only the worker reports is added.
- A key in both takes the **worker's** value.

**The merge is shallow, one level.** Values are opaque — a nested object is a
value, not a subtree to descend into. Deep-merging would require core to have
an opinion about the *structure* of what an instance put in its bag, which is
the genericity line. Shallow is also what makes the operation associative
enough to reason about: the result depends on which keys were written, never on
how deeply an instance happened to nest them.

Re-completion cannot double-apply: R9 already makes a second `complete` an
`AUTH_ERROR` (a step past stage 1 holds no lease), so the merge runs at most
once per attempt. On a **retry after failure** the step's row carries whatever
the previous attempt merged; the next attempt's bag merges over that. That is
the honest record — a key reported at attempt 1 and not at attempt 2 was
genuinely reported once — and §3 covers it as a test.

### 1.3 The write

One new `db` helper, `SetStepMetadataTx`, in the shape of the existing
`SetStepStatusTx` / `SetStepRoutingTx` neighbours: a CAS-guarded `UPDATE steps
SET metadata = ?, updated_at_ms = ?, row_version = row_version + 1 WHERE id =
?`. It writes the **already-merged** JSON; the merge itself is engine-side, in
Go, so the SQL stays a plain assignment and no JSON function dependency enters
the schema layer.

Ordering inside the transaction: after the artifact insert and before the
`AdvanceSagaTx`, so the step's own row reaches its final shape before the saga
records that stage zero happened. A crash between them rolls back all of it.

### 1.4 No schema change

**This is the key scope finding, and it is deliberate.** `steps.metadata` (col
23) already exists and already holds JSON text. The rollup already reads it.
Nothing in this fix touches the schema, so **no migration, no version bump, no
sentinel** — the migration discipline applies by being correctly *not* invoked.

Had I chosen a separate `completion_metadata` column (§2, rejected), this would
be a v11 with a **column** sentinel (v6's shape, not v7–v10's table shape) plus
the index-guard lesson from DKT-6. Avoiding that is a substantive argument for
the merge design, not an accident of it.

### 1.5 Surfacing: R7 and `run report --json` need no change

This is worth stating precisely, because DKT-68's suggested-fix note says the
same and I verified it rather than inheriting it.

`MetadataRollup` selects `metadata` from `steps` where non-empty, unmarshals
each row as an opaque object, and counts key→value→n with both levels sorted.
It contains no key-name literal (asserted mechanically by
`TestMetadataRollupReadsNoKey`). `report.Metadata` is already wired
(`internal/engine/report.go:98,180`) and the text renderer already prints a
`Metadata` section (`internal/cli/run_report.go:252`). All three were correct
and starved of input.

So the surfacing work is **zero production lines** and entirely test lines. The
`data.metadata` section appears in `run report --json` the moment a step
carries a bag. The reason it reads `null` today is that the `omitempty` tag
elides an empty slice — with rows present it populates.

Likewise the **context bundle** delivers the merged bag automatically:
`context.go:155` decodes `step.Metadata`, so a step re-claimed after a
completed attempt sees the merged result with no further change.

### 1.6 The fail-side question (DKT-69) — designed for, not implemented

DKT-69 (`step fail --metadata` parity) is post-M4 backlog and this note does
**not** implement it. It is designed to slot in without rework:

The merge logic lands as a standalone pure function —

```
func mergeMetadata(definition, completion string) (string, error)
```

— taking the stored JSON text and the incoming flag, returning the merged text.
It lives in `internal/engine` beside the existing `decodeMetadata` /
`marshalMetadata` pair, and it knows nothing about sagas, transactions, or
which verb called it. `stageZero` calls it; a future `FailStep` calls the same
function and the same `SetStepMetadataTx`. DKT-69 then becomes: add the flag,
add the option field, call two existing functions, add tests.

The one question DKT-69 must answer that this note does not: **whether a failed
attempt's metadata should survive into the retry.** I recommend yes (same
last-write-wins, retry overlays), because a worker that reports *why* it failed
has produced the most valuable metadata in the run and discarding it at retry
would lose exactly the diagnostic an operator wants. But that is DKT-69's call
to make with its own acceptance criteria, and nothing here forecloses it: the
merge function is indifferent, and the decision is one call site.

## 2. Alternatives considered

**A. Separate `completion_metadata` column.** Rejected. It requires a v11
migration with a column sentinel for one column; it forces R7 either to ignore
runtime metadata (defeating the purpose) or to read two columns and merge at
read time (making every reader re-implement the merge); and it contradicts
`saga.go:62`, which already specifies a merge onto the step's own bag. The
column split buys provenance — knowing which keys the definition declared
versus which the worker reported — which §2.D revisits.

**B. A new `step_metadata` KV table.** Rejected, more strongly. It is the
correct shape if metadata were ever queried by key, and it is explicitly never
queried by key — genericity.md forbids core reading a key, and R7's whole
design is key-agnostic aggregation. A table would add joins, a migration, and
per-key rows to serve exactly no query that exists.

**C. Merge at read time, storing only the completion bag.** Rejected. It moves
work to every reader and makes the stored row a lie: `steps.metadata` would
mean "the definition's bag" in one code path and "the worker's" in another.
The context bundle would then need the same merge, duplicating it.

**D. Record provenance (which keys came from the worker).** Rejected for this
patch, and worth recording why since it is the most defensible alternative.
Tier-drift analysis — the motivating use case — compares `model_requested`
against `model_resolved`, and both are *worker*-reported; the definition side
does not participate. So provenance serves no query in the motivating case,
while costing a schema change and a wire-shape change. If a future need
appears, `--metadata` bags can carry their own provenance keys opaquely, which
is precisely what the KV bag is for. Filing it as a deferred question rather
than building it is the §2 discipline.

**E. Refuse when the worker overwrites a definition key.** Rejected. It sounds
protective and is actually core having an opinion about which keys matter. The
reference instance's `model_resolved` overwriting a declared `model_requested`
default is a *normal* and intended use.

## 3. Genericity argument

The rule (docs/design/genericity.md): core carries zero agent/LLM vocabulary;
execution metadata is an opaque KV bag.

This change is the *most* generic possible fix, and the argument is
mechanical rather than rhetorical:

- **No new vocabulary.** No flag, column, JSON key, or error string is added
  or renamed. The gate's banned set (`model`, `prompt`, `llm`, `agent`,
  `brief`) is untouched — and it must stay untouched in **tests too**, per the
  gate's scan of `internal/`. The four keys in DKT-68's reproduction
  (`model_requested`, …) are *instance* data and must **not** appear in any
  test fixture under `internal/`. Test bags use neutral keys (`tier`, `desk`).
- **Core still reads no key.** The merge iterates keys without interpreting
  any; the rollup's no-key-literal assertion continues to hold and is extended
  to cover the merge function.
- **Values stay opaque.** Strings, numbers, objects, and nulls are all stored
  and rolled up verbatim; `renderMetadataValue` already keeps `1` and `"1"`
  distinct precisely because merging them would be core deciding they mean the
  same thing.
- **Stranger test.** A team that has never run an LLM writes
  `docket step complete STEP-7 --artifact-file r.txt --metadata
  '{"desk":"front","rework":"true"}'` and reads `run report` to see how many
  steps each desk handled and how often rework occurred. Useful, and
  comprehensible from public docs with zero AI concepts.

## 4. Test plan

Test-first. Go tests in `internal/engine` and `internal/db`; the end-to-end
proof in `scripts/qa/` per the repo convention, with `XDG_CONFIG_HOME`
sandboxed (never the real `~/.config/docket`, never the real trust store).

### 4.1 Go unit — the merge function (pure, table-driven)

| Case | Assertion |
|---|---|
| definition empty, completion set | result = completion |
| definition set, completion empty | result = definition, byte-identical |
| disjoint keys | union, both present |
| overlapping key | completion's value wins |
| nested object value | replaced wholesale, not deep-merged |
| both empty | result empty; no write attempted |
| completion invalid JSON | error, definition unchanged |
| completion JSON array / scalar / `null` | error (not an object) |
| key order varies in input | output key order stable (determinism, R9) |

### 4.2 Go — `stageZero` integration

- **The regression test, written first and failing:** complete a step with
  `--metadata`, assert `steps.metadata` non-empty and equal to the merge. This
  is DKT-68 reduced to one assertion.
- Definition-side bag present → merged, not clobbered.
- No `--metadata` → definition bag byte-identical afterwards.
- Invalid metadata → `VALIDATION_ERROR`, **and** `row_version` unchanged, **and**
  no artifact row exists (the §6.9 refusal-writes-nothing assertion, which is
  what places validation before `Begin()`).
- **The cap's own test** (§1.1.1), as a named case rather than a line in a
  table:
  - exactly `MetadataMaxBytes` bytes → **accepted** (the boundary is inclusive,
    asserted so an off-by-one cannot pass silently);
  - `MetadataMaxBytes + 1` → `VALIDATION_ERROR` whose message contains **both**
    the actual size and the cap, and names the artifact/payload remedy;
  - `row_version` unchanged, no artifact row, step still completable afterwards
    — the refusal wrote nothing;
  - measured on the **raw input**: a bag under the cap is accepted even when the
    definition bag it merges over pushes the stored result past it, proving the
    cap is not silently applied to the merged text.
- Refusal ordering: an input that is both oversized and invalid JSON yields the
  size error, deterministically (C5's stability property).
- Second `complete` → `AUTH_ERROR` (R9), metadata unchanged: the merge does not
  double-apply.
- Retry: attempt 1 merges `{a:1}`, attempt 2 merges `{b:2}` → both present,
  documenting the overlay semantics DKT-69 will inherit.
- Transactionality: a forced failure after the metadata write (existing
  `txdiscipline_test.go` harness) leaves `steps.metadata` at its pre-call value.

### 4.3 Go — R7 and the report

- Rollup over three steps with overlapping keys → correct key→value→count
  nesting, both levels sorted.
- `report.Metadata` non-empty after a completed step with a bag; `run report
  --json` `data.metadata` present rather than `null` (the exact observation
  DKT-68 recorded).
- The existing `TestMetadataRollupReadsNoKey` still passes, and an analogous
  no-key-literal assertion covers the merge function.
- A step whose stored metadata is a non-object (only reachable for rows written
  before this fix) is still skipped by the rollup rather than failing the
  report — R10's "useless during exactly the run an operator wants to inspect"
  property is preserved.

### 4.4 QA section — the CLI proof

A new `scripts/qa/` section following the existing helper conventions
(`set -e` behaviour in subshells per the harness's established gotcha):

1. Sandboxed init; activate a toy workflow; claim a step.
2. `step complete --metadata '{"desk":"front"}'` → exit 0.
3. `run report --json | jq .data.metadata` → **non-null**, containing `desk` →
   `front` → 1. This is the literal inverse of DKT-68's reproduction and is the
   acceptance bar.
4. `step complete --metadata 'not json'` on a second step → exit 3
   (VALIDATION_ERROR), and the step is **still claimable/complete-able**
   afterwards — proving the refusal wrote nothing.
5. Genericity: `grep` the section's own fixtures for the banned words → zero
   hits, since the gate scans tests too.

### 4.5 Gates that must stay green

Full `scripts/qa.sh` plus `scripts/qa/genericity.sh`. `go build ./...` and
`go test ./...`. No migration test changes, because there is no migration —
`currentSchemaVersion` stays 10, and that constant staying put is itself an
assertion this design makes.
