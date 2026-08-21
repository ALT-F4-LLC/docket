# TDD: runs, budgets/floor, report; dispatch manifests; the events read surface (stage 6)

Status: draft — 2026-08-04 · implements docs/design/engine-spec.md §2 (the
**Scheduling**, **Budgets**, **Runs/report/events**, and **Instance config
lifecycle** blocks, verbatim and normatively), §3's events/retention clauses,
§11.1's `[limits]` and `expected_cost` rows, and §11.4's `dispatch` and `event`
shapes; engine-core.md §7 supplies the budget semantics, §3 the run lifecycle,
and §1.6 the event model. Acceptance bar: **§9 items 2, 7, 9, 10 (complete),
and 11**. Tracker unit: **DKT-6**. Spec of record is engine-spec.md; deviations
are DKT amendment issues per docs/design/amendments.md citing the exact line,
never silent changes.

**This is the stage where the engine learns to STOP.** Every mechanism below is
a refusal: a budget that pauses a run from facts the engine produced itself, a
`next` that refuses to schedule around its own unreconciled mess, a successor
that refuses to write until a reaped writer is confirmed gone. §1.1 is written
first because a refusal surface is exactly where instance vocabulary leaks in —
"pause because the model overspent" is one careless error string away.

**This document is reviewed with a concurrency lens and a security lens before
any code exists.** §1.2 is the concurrency inventory — every new CAS, every
lazy path, every place two engine invocations race — and is written to be read
first by that reviewer. §1.3 is the security note, which is short because this
stage executes nothing new.

## 1. Scope

engine-spec.md §10 stage 6, verbatim:

> 6. **Runs, budgets/floor, report; `dispatch` manifests; the events *read*
>    surface** (`events list --since`) — the upstream migration's AC-integrity
>    audit needs it.

and, from the same section:

> Guard verbs land with their underlying features (`stop`/`gate` at stage 3,
> `record`/`spawn` at stage 6); stage 3 carries the minimal run subset
> activation needs (run row, status, pins) with report/budgets completing at
> stage 6.

> **The v1 shadow run (upstream 07 §4) becomes runnable after stage 6**; stage
> 7 may trail it.

In scope:

1. **Budgets** exactly per engine-core §7: the floor accrued per claim event,
   enforcement against `max(reported, floor)`, the cap flipping the run
   `waiting-human` with a budget-breach reason, R7's seam becoming the real
   check, `--budget` enforcing, `--usage` recording into a ledger.
2. **`run report`** — the ledger rollup of engine-core §1.1/§3.5 (usage sums,
   attempts, gate/action trails, artifact index, metadata rollups), read-only.
3. **`dispatch open|close|verify|abandon`** — the batch manifest, byte-equality
   verification, one-open-per-run, the TTL lazily auto-abandoned by `next`, and
   `next`'s refusal while open or while discrepancies exist.
4. **The write-reap acknowledgment** — §9 item 10's deferred half, whose ack
   *mechanism* this TDD pins (§6).
5. **`events list --since`** — the §11.4 event shape as a read surface, with
   seq-cursor semantics and the `Collection` envelope.
6. **`guard record|spawn`** — the two predicates §2 assigns to this stage.
7. **Auto-registration** — the `.docket/config/` scan at activation (§9 item
   11's machinery), with the F2 carry-forward's ordering.
8. Schema **v10** — the span's final row.

Out of scope, explicitly:

| Deferred | Owner | Why not here |
|---|---|---|
| `events --follow` (streaming, tail semantics) | S7 | §10 stage 7: "Events `--follow` + prune (observability tail)" |
| `events prune --before …`, artifact GC, the retention boundary's *enforcement* | S7 | same line. **§8.6 specifies `GONE`'s shape here** because `--since` is the verb that must return it, and says plainly that its TRIGGER — a prune having removed events below the cursor — cannot fire until S7 ships the pruner |
| Worktree-isolated parallel writes (upstream D9) | never core | engine-core §5: "an optional instance optimization" |
| Budget **warn** thresholds (60%/100% in §8's instance table) | never core | §4.7: warn is instance policy; core ships the cap only |
| A daemon, a scheduler process, background reaping | never core | §7 hard boundary; every lazy path in §1.2 is driven by an operator's invocation |

**After this stage the v5–v10 map of docs/tdd/reliability-delta.md §2 is
COMPLETE.** That table is asserted, not merely satisfied: §2.4 specifies
`TestSchemaSpanIsComplete`, which fails if `currentSchemaVersion` ever moves
past 10 without an amendment issue, because the span was ratified at stage 1
and stages 4–6 were built on its arithmetic.

### 1.1 Genericity check (CLAUDE.md PR bar, docs/design/genericity.md)

Per the S3/S4/S5 pattern, over **every** noun this stage introduces into core
surface — flag names, JSON keys, column names, error strings, help text, event
kinds, and the config keys:

`dispatch`, `manifest`, `open`, `close`, `verify`, `abandon`, `discrepancy`,
`reconcile`, `grace`, `TTL`, `budget`, `cap`, `floor`, `accrual`, `ledger`,
`usage`, `unit`, `report`, `rollup`, `burn rate`, `breach`, `headroom`,
`acknowledge`, `reap`, `unacknowledged`, `seq`, `cursor`, `since`, `GONE`,
`auto-register`, `config directory`, `scan`.

Every one is **scheduling, accounting, batching, or log-cursor vocabulary**. No
model, prompt, brief, node, severity, review, agent, or LLM concept appears
anywhere in the surface. The four places instance meaning could leak are closed
by construction:

- **`usage` is a bag of opaque numbers keyed by opaque strings.** The ledger
  stores `(step, unit, quantity)` where `unit` is whatever the caller wrote —
  `tokens`, `seconds`, `pages`, `sheets`. Core never enumerates units, never
  has a default unit, and never converts between them. §4.5 makes the sums
  per-unit for exactly this reason: a core that summed across units would have
  decided they were commensurable, which is an opinion about what they mean.
- **`expected_cost` and the budget cap are BARE NUMBERS with no dimension.**
  There is no currency, no token, no rate. `--budget 12` means "stop when the
  accrued number reaches 12", and what the number counts is the workflow
  author's business. This is why §4.2's floor is stated as arithmetic over
  `expected_cost` and never as "cost in X".
- **A discrepancy is a COUNTING FACT, not a judgment.** "claimed but not
  recorded past grace" and "no usage row for a completed step" are both
  questions about rows in tables. Neither reads an artifact, a payload, or a
  metadata key.
- **`guard spawn` names no spawner.** It answers "do the rows you propose match
  the open dispatch, and are there unacknowledged write reaps?" — a comparison
  of two lists and a count. The word `spawn` is §2's own, names *when* a
  harness asks, and a human-only team wiring it to a "before you start the next
  batch" hook reads it exactly as written.

**`budget` and `report` are pre-existing English, and worth one sentence each.**
A team tracking a documentation sprint sets `--budget 40`, gives each step an
`expected_cost` of 1, and gets a run that pauses for a person after forty
claimed steps. That is the stranger test passing on the headline feature of
this stage, and §9.3's `ZI` section is that team's run, executed.

**The one word this stage deliberately does NOT introduce is `warn`.** §8's
instance table lists "budget warn 60% / pause 100%", and §4.7 argues at length
that only the second is core's. A `budget.warn` config key would be core
shipping a policy about when a human should look, which is precisely the
"opinions here" §8 says core ships none of.

### 1.2 CONCURRENCY INVENTORY

**This is the section the concurrency review reads first.** Every mechanism
this stage adds is enumerated here with the race it must survive, and each row
names the section that closes it. The engine has no daemon and no background
thread: every path below is driven by two or more *operator invocations* of the
CLI racing over one SQLite file (WAL, busy_timeout, single-transaction
mutations — engine-spec §6).

| # | Path | The race | Closed by |
|---|---|---|---|
| C1 | `dispatch open` | two relays open a dispatch for one run simultaneously | §5.3: `UNIQUE` partial index on `(run_id)` where status = `open`; the loser gets `CONFLICT`, never a second manifest |
| C2 | `dispatch close` | close races the TTL auto-abandon in a concurrent `next` | §5.6: close is CAS on `(id, status='open')`; the loser observes `abandoned` and reports `CONFLICT` with the abandoning seq |
| C3 | TTL auto-abandon | two `next` invocations both find the dispatch expired | §5.5: the abandon is `UPDATE … WHERE id = ? AND status = 'open'`; exactly one row matches, exactly one event is written |
| C4 | Budget accrual | two claims of two steps commit concurrently, each reading the same pre-claim floor | §4.3: the floor is **never stored as a running total**. It is `SUM(expected_cost)` over claim *events*, computed inside the deciding transaction. There is no read-modify-write to lose |
| C5 | Budget enforcement | a claim passes the headroom check, then another claim commits and the cap is breached | §4.4: the check and the accrual are **the same transaction** as the claim's CAS. A claim that would cross the cap does not commit. The serialization is SQLite's, not ours |
| C6 | The breach transition | two invocations both observe the cap crossed and both flip the run | §4.6: the flip is CAS on `(run_id, status='active')` and the reason is idempotent; the second matches zero rows |
| C7 | Write-reap ack | the relay acknowledges a reap while `next` is reaping another | §6.4: the ack is a CAS on one `reaps` row by `seq`; reaping and acking touch different rows and never the same one twice |
| C8 | Write headroom | a successor is offered between the reap and the ack | §6.3: the hold is a **predicate evaluated in the readiness transaction**, not a flag written at reap time. There is no window where the row says "reaped" and the predicate says "free" |
| C9 | `events list --since` | events are inserted while the list is being read | §8.3: the read is one `SELECT … WHERE seq > ?` in one transaction; `seq` is `AUTOINCREMENT` and monotonic, so a concurrent insert lands *above* the cursor and is returned by the next call, never skipped |
| C10 | Auto-registration | two activations of two runs scan the same config directory | §9.4: registration is content-hash idempotent (`CONFLICT` only on *different* bytes at the same `name@version`), so the loser's identical registration is a success that changes nothing |
| C11 | `guard spawn` | the dispatch is abandoned between the guard's allow and the relay's spawn | §7.3: acknowledged as a **residual**, bounded by the fact that the spawn's own `step claim` is CAS-guarded and a claim against an abandoned dispatch's step is either legal (the step is still ready) or refused. The guard is an early check, not a lock |
| C12 | `next` refusal | the refusal is computed, then a discrepancy resolves | §5.7: benign — the next invocation proceeds. A refusal that is stale by a millisecond costs one poll, and the alternative (holding a lock across the relay's decision) is the wedge §2's recovery design exists to prevent |

**The standing rule this stage does not break:** lazy lease reaping stays
confined to `next`/`claim` (engine-spec §6). This stage adds *one* lazy path —
the dispatch TTL auto-abandon — and puts it in `next`, which §2 names
explicitly ("a dispatch TTL lazily auto-abandoned by `next`"). No read verb
this stage adds writes: `run report`, `events list`, `dispatch verify`, and
both guards are pure reads, asserted by §10.3's `TestReadVerbsWriteNothing`.

### 1.3 Security note

This stage **executes nothing new**. It adds no trusted command, no argv
resolution, no subprocess, and no path into `internal/exec`. The gates-trust
threat model (§2 of that TDD) is therefore unchanged, with three additions
worth stating because a reviewer will look for them:

- **`--usage` is attacker-controlled JSON from a claimant.** It lands in a
  ledger and in a report. §4.9 caps it (count of units, length of a unit name,
  numeric range) and renders it through the §5.7 escaping renderer that
  gates-trust established for gate output, because a run report is bytes going
  to a terminal.
- **The events read surface exposes `data` blobs verbatim.** Those blobs
  contain `--usage`, gate names, and routings. §8.5 routes every human-mode
  rendering through the same escaper, and leaves `--json` raw (encoding/json's
  job, the consumer is a program) — gates-trust §5.7 E4's rule, applied to the
  one new read verb.
- **Auto-registration reads repo files at activation.** It reads and hashes;
  it does not execute. The fenced commands those workflows declare still
  require a trust entry, unchanged — §9.5 makes the point explicitly, because
  "activation now registers a workflow it found in the repo" is exactly the
  sentence that sounds like drive-by execution and is not.

## 2. Schema version span

This stage ships **v10**, the **final** row of the v5–v10 span fixed in
docs/tdd/reliability-delta.md §2. That table is authoritative and is not
re-litigated; this stage occupies its v10 row ("dispatches, events, usage
ledger").

| Schema | Stage (§10) | Contents |
|---|---|---|
| v5 | 1 — reliability delta | shipped |
| v6 | 2 — claims/leases + capability tokens | shipped |
| v7 | 3 — the spine | shipped (workflows, steps, runs minimal, pins, **events table**) |
| v8 | 4 — gates + trust | shipped (gate_results, trust_cache) |
| v9 | 5 — payloads + actions | shipped (schemas, action_results) |
| **v10** | 6 — runs/budgets/dispatch/events-read | **this stage**; DDL sliced per group in §3.1 |

**The v10 row says "events" and the events TABLE already exists.** v7 created
it (engine-spine §7.1) because the spine had transitions to log; what v10 adds
is the *ledger* and the *dispatch* tables, plus the indexes the read surface
needs. This is stated because the reliability-delta row reads as though events
arrive here, and an implementer following that table would create a table that
exists. §3.1's DDL is the authority.

### 2.1 v10 lands as ONE migration function across THREE commit groups

`migrateV9ToV10` is assembled across §11's three groups, exactly as v7 was
assembled across four phases and v9 across two. **The stamp moves to 10 in
group 1** — which needs `usage_ledger` — and does not move again.

This is the same trap v7Sentinels and v9Sentinels were built for, at its
widest yet: three groups means two intermediate states in which the operator's
own dogfooded DKT tracker can sit, stamped 10 with only part of v10 present.

### 2.2 `v10Sentinels`, and the rewind guard

`v10Sentinels` follows the established constant-beside-the-migration pattern
and **grows with the groups**:

| Group | Sentinel added | Why the group needs it |
|---|---|---|
| 1 | `usage_ledger` | `--usage` records into it; the floor reads claim events, not this table, but the report sums it |
| 2 | `dispatches`, `dispatch_rows` | the manifest and its ordered rows |
| 2 | `reap_acks` | the write-reap acknowledgment ledger (§6.4) |
| 3 | *(none)* | group 3 adds a read verb, two guards, and a directory scan — no table |

The guard is the v7/v8/v9 shape verbatim: **stamped ≥ 10 with ANY v10 sentinel
absent rewinds to 9 and re-runs.** `migrateV9ToV10` is `CREATE TABLE IF NOT
EXISTS` plus `hasColumn`-probed `ALTER`s throughout, so re-running against a
partially-migrated database adds what is missing and touches nothing else.

`TestRewindGuardProbesEveryV10Sentinel` derives the expected list from the DDL
— the v9 test's shape — so a group that adds a table without extending the
sentinel list fails its own test rather than shipping a half-migrated tracker.

### 2.3 The column half, which sentinels cannot see

`v10AddedColumns`, in the v9 table's shape, one row per column:

| Table | Column | Default | Why the default preserves meaning |
|---|---|---|---|
| `runs` | `usage_floor` | `0` | **cached** floor for the report's burn-rate line only — never the enforcement input (§4.3). Every pre-v10 run accrued nothing, so 0 is its true floor |
| `runs` | `breach_reason` | `NULL` | no pre-v10 run was paused by a budget it did not enforce |
| `steps` | `usage_recorded` | `0` | the discrepancy probe's fast path (§5.8 D3). Every pre-v10 step recorded no usage, and a step completed before v10 is not a discrepancy — §5.8's D3 scopes the probe to steps completed *after* the run's activation under a v10 binary, which §5.8 argues explicitly |

**`runs.budget` already exists** (v7 DDL) and needs no column. That is the
`--budget`-stored-since-S3 contract paying off exactly as engine-spine §1
predicted: this stage adds enforcement, not a flag.

### 2.4 The 4→10 fixture protocol, and the completeness assertion

Each group re-runs the engine-spec §9 item 8 fixture protocol, now at its full
span: `scripts/qa/fixtures/v4-baseline.db` opens, migrates **4→10 in one
pass**, and golden-diffs — with the v10 structures asserted present *before*
the diff is trusted, since a golden diff against a DB that failed to migrate
passes vacuously.

**The upgrade proof against this repo's v9-stamped tracker** (the fourth time
this proof has been written, and the one that matters most because the tracker
is dogfooded through the stage):

| # | Start state | Action | Assertion |
|---|---|---|---|
| U1 | this repo's tracker, stamped 9, all v9 tables present | `docket migrate` | stamped 10; `usage_ledger`, `dispatches`, `dispatch_rows`, `reap_acks` present; every v9 table untouched; DKT-1..N read byte-identically |
| U2 | stamped 10, `usage_ledger` present, `dispatches` absent (**a group-1 binary migrated it**) | a group-2 binary opens it | the rewind guard sees the missing sentinel, rewinds to 9, re-runs, and the dispatch tables arrive |
| U3 | stamped 10, all tables present, `steps.usage_recorded` absent (**an impossible-in-practice hand-edit**) | `docket migrate` | the sentinel probe passes (tables are all there) and the column does **not** arrive — so U3 asserts the column half is covered by U2's rewind rather than by the sentinel probe, which is the honest statement of what sentinels can and cannot see |
| U4 | a v4 fixture | `docket migrate` | 4→10 in one pass; every intermediate version's tables present; the byte-compat sweep passes |

**`TestSchemaSpanIsComplete`** asserts `currentSchemaVersion == 10` and that
`migrations` has exactly one entry per version 2..10 with no gaps. It is not a
tautology check: it is the tripwire for a stage-7 author who reaches for v11
without filing an amendment against reliability-delta §2, which ratified that
stage 7 "reuses stage 6's tables and adds no version".

## 3. What is dormant, and how each group proves it

engine-spec §3: "Dormant unless workflows are used"; §9 item 8: "a
workflow-free repo shows no behavioral change on any existing verb."

The dormancy claim for this stage, precisely: **a repo with zero rows in
`workflows` behaves byte-identically to v9 on every pre-existing verb; and a
repo that DOES use workflows behaves byte-identically to v9 on every path
unless it opts into one of this stage's three new mechanisms.** That second
half is new and is the harder claim, because unlike S3–S5 this stage changes
the behavior of an *existing* verb (`next`).

| # | Mechanism | Dormant when | The proof |
|---|---|---|---|
| D1 | **Budget enforcement** | `runs.budget = 0` (the default; "0 means unlimited") **and** `budget.default = 0` | §4.8: R7 returns `true` unconditionally when the effective cap is 0, on the *first* line, before any query. A run started without `--budget` in a repo that never set `budget.default` executes exactly the queries v9 executed |
| D2 | **Dispatch** | no dispatch has ever been opened for the run | §5.9: `next`'s refusal probe is a single indexed lookup returning no row. `dispatch` is a **new verb**; a relay that never calls it sees the v9 `next` |
| D3 | **Write-reap ack** | no write-class lease has been reaped, i.e. `reap_acks` has no unacknowledged row for the run | §6.6: the hold predicate short-circuits on an empty table |
| D4 | **`events list`** | always — it is a **new verb** | reading a table cannot change another verb's output |
| D5 | **`guard record\|spawn`** | always — **new verbs** | same |
| D6 | **Auto-registration** | **the repo has no `.docket/config/` directory** | §9.6: activation stats the directory once; absent ⇒ the scan is skipped entirely and activation is byte-identical to v9's. "A repo with no `.docket/config/` activates exactly as before" is a QA check, not an intention |

**Per-group dormancy proof** (each is a QA check in the group's own commit):

| Group | Proof |
|---|---|
| 1 | v9→v10 applied; `usage_ledger` empty; a run with no `--budget` produces a byte-identical `next` transcript to the pre-group baseline; the full byte-compat sweep passes |
| 2 | a run that never opens a dispatch produces a byte-identical `next` transcript; a run with no write-class reap likewise; `dispatches` empty |
| 3 | `events list` and both guards add no output to any existing verb; **a repo with no `.docket/config/` activates byte-identically**, asserted by re-running ZG's activation with the directory absent |

## 3.1 The v10 DDL, sliced per group

```sql
-- Group 1: the usage ledger.
CREATE TABLE IF NOT EXISTS usage_ledger (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	step_id       INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	attempt       INTEGER NOT NULL DEFAULT 0,
	unit          TEXT    NOT NULL,
	quantity      REAL    NOT NULL,
	source        TEXT    NOT NULL DEFAULT 'reported',
	created_at_ms INTEGER NOT NULL,
	UNIQUE(step_id, attempt, unit)
);

CREATE INDEX IF NOT EXISTS idx_usage_ledger_run ON usage_ledger(run_id);

-- Group 2: dispatch manifests, their rows, and the reap-acknowledgment ledger.
CREATE TABLE IF NOT EXISTS dispatches (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id        INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	status        TEXT    NOT NULL DEFAULT 'open',
	opened_seq    INTEGER NOT NULL,
	expires_ms    INTEGER NOT NULL,
	closed_at_ms  INTEGER,
	close_reason  TEXT,
	created_at_ms INTEGER NOT NULL,
	row_version   INTEGER NOT NULL DEFAULT 1
);

-- C1: EXACTLY ONE OPEN PER RUN, enforced by the database rather than by a
-- check-then-insert. §2 says "exactly one dispatch open per run (CAS/unique
-- index)"; a partial unique index is that index, and it is what makes two
-- relays racing `dispatch open` produce one manifest and one CONFLICT rather
-- than two manifests and a silent double-spawn.
CREATE UNIQUE INDEX IF NOT EXISTS idx_dispatches_one_open
	ON dispatches(run_id) WHERE status = 'open';

CREATE TABLE IF NOT EXISTS dispatch_rows (
	dispatch_id INTEGER NOT NULL REFERENCES dispatches(id) ON DELETE CASCADE,
	position    INTEGER NOT NULL,
	step_id     INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	instance    TEXT    NOT NULL,
	row_json    TEXT    NOT NULL,
	row_sha256  TEXT    NOT NULL,
	PRIMARY KEY (dispatch_id, position)
);

CREATE TABLE IF NOT EXISTS reap_acks (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id         INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	step_id        INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
	class          TEXT    NOT NULL,
	reaped_seq     INTEGER NOT NULL,
	acked_at_ms    INTEGER,
	acked_by       TEXT,
	created_at_ms  INTEGER NOT NULL,
	UNIQUE(reaped_seq)
);

CREATE INDEX IF NOT EXISTS idx_reap_acks_open
	ON reap_acks(run_id) WHERE acked_at_ms IS NULL;

-- Group 3 adds no table. The events read surface needs one index the v7 DDL
-- did not create, because `--since` without `--run` scans by seq alone:
CREATE INDEX IF NOT EXISTS idx_events_seq ON events(seq);
```

**Three shape decisions worth their sentences:**

- **`usage_ledger` is keyed `UNIQUE(step_id, attempt, unit)`, not by step
  alone.** A step that is reaped and re-claimed produces a *second* attempt
  with its own usage; summing over a step-unique key would silently overwrite
  the first attempt's numbers, and the report's attempt trail would show two
  attempts and one attempt's usage. The `attempt` column is what makes
  "retries re-accrue" (engine-core §7) true on the reported side as well as
  the floor side.
- **`dispatch_rows.row_json` and `row_sha256` are both stored.** `verify` is
  "byte-equality on rows" (§11.4), so the bytes must be stored to compare
  against; the hash is stored beside them so verification is a hash comparison
  first and a byte comparison only on mismatch — which is what lets the
  refusal *show the operator the differing row* instead of "they differ".
- **`reap_acks.reaped_seq` is `UNIQUE` and references the EVENT.** The
  acknowledgment is of an *event* (§2: "until the relay acknowledges the
  `reaped` event"), so the event's `seq` is the ack's identity. That makes the
  ack idempotent for free (C7) and makes a forged ack impossible to construct
  without naming a real reap.

## 3.2 What v10 does NOT add, deliberately

- **No `runs.floor` running total.** §4.3 argues this at length: a stored
  running total is a read-modify-write, and a read-modify-write is the one
  shape C4 has no defense against. `runs.usage_floor` (§2.3) is a *cache for
  the report*, written in the same transaction as the accrual and never read
  by the enforcement path — and §4.3's `TestFloorIsNeverReadFromCache` asserts
  that separation by seeding a wrong cache and proving enforcement ignores it.
- **No `budget_warn` column or config key.** §4.7.
- **No `dispatch_discrepancies` table.** A discrepancy is *computed*, never
  stored (§5.8). Storing one would create a second source of truth that could
  disagree with the rows it summarizes, and the disagreement would be resolved
  by whichever code path a reader happened to call.

---

# Group 1 — budgets, the floor, and `run report`

Commit group 1. Leaves the branch green and is independently stoppable: it
ships budget enforcement and a read-only report over a ledger that no other
mechanism depends on. No dispatch exists, no new guard, no events verb.

## 4. BUDGETS, exactly per engine-core §7

engine-core §7, verbatim, is the specification:

> Usage is recorded per step (executors attach what they observe; the reference
> harness back-fills from its dispatch journal, source recorded), but the cap
> does not rest on it: the engine enforces against `max(reported, floor)` where
> the **floor** is computed from facts the engine itself produced — each claimed
> step's `expected_cost` from the workflow definition (engine-spec §2), accrued
> per claim event — retries and loop entries re-accrue, never released; bounded
> loops bound the floor. Reported usage can only raise the counter, and missing
> usage rows are a dispatch discrepancy blocking the next batch (§5). Crossing
> the run cap flips the run to `waiting-human` with a budget-breach reason — a
> hard control resting on engine-owned inputs, not voluntary reporting.

Implemented clause by clause below.

## 4.1 The effective cap

| # | Clause |
|---|---|
| B1 | The cap is `runs.budget` when non-zero, else `docket config budget.default` when non-zero, else **unlimited** |
| B2 | `0` means unlimited at both levels — the flag's documented meaning since S3 (`internal/cli/run_start.go`), unchanged |
| B3 | The cap is resolved **once per invocation** and read from the run row, which was written at `run start`. It is not re-read from config mid-run: a config change must not silently re-cap a live run, for the same reason a re-registered workflow does not reach one (RA2, engine-spine §5.4) |
| B4 | A negative `--budget` is already `VALIDATION_ERROR` at S3. No change |

**B3 has a consequence worth stating**: a run started before `budget.default`
was set has `runs.budget = 0` stored, and therefore stays unlimited even after
the default is set. That is the pinning property applied to budgets, and it is
the right answer — but it is surprising enough that `run report` prints the
**effective cap and where it came from** (§4.10 R6), so an operator asking
"why didn't it stop?" gets the answer from the report rather than from the
source.

## 4.2 `expected_cost`: the engine-owned input

`expected_cost` has been parsed, validated, stored on the step, and emitted on
`next` rows since S3 (engine-spine §1: "enforces nothing at this stage").
**Nothing about its shape changes here.** What changes is that a claim now
reads it.

| # | Clause |
|---|---|
| B5 | The value accrued is the **step row's** `expected_cost`, materialized at expansion from the pinned definition — never re-read from the live `workflows` table. A run pins its definitions; its floor is computed from what it pinned |
| B6 | Default `0` (§11.1). A workflow that declares no costs has a floor of 0 forever and is therefore never budget-paused — which is D1's dormancy, arriving from the grammar's own default rather than from a special case |
| B7 | A fanned-out step's `expected_cost` is **per expanded sibling** (the fixture says so verbatim: `expected_cost = 0.60 # per expanded sibling`). Four siblings claim four times and accrue four times. No division, no proration |

## 4.3 THE FLOOR: `SUM(expected_cost)` over claim events

This is the load-bearing decision of the whole budget mechanism.

> **The floor is not a stored counter. It is a query over the event log.**
>
> ```sql
> SELECT COALESCE(SUM(s.expected_cost), 0)
>   FROM events e JOIN steps s ON s.id = e.step_id
>  WHERE e.run_id = ? AND e.kind = 'step-claimed'
> ```

| # | Clause |
|---|---|
| B8 | The floor is `SUM(expected_cost)` over the run's `step-claimed` **events**, computed inside whatever transaction needs it |
| B9 | **Retries re-accrue.** A reaped step that is claimed again writes a second `step-claimed` event, and the SUM counts both. Nothing is released on reap, on fail, or on abandon — the work was attempted and the attempt is what cost something |
| B10 | **Loop entries re-accrue.** A `fix` step instantiated at ordinal 1 is a *different step row* with its own `expected_cost`, claimed and event-logged independently. `max_fix_loops` therefore bounds the floor by construction — engine-core §7's "bounded loops bound the floor", falling out of §11.3 rather than needing its own arithmetic |
| B11 | A superseded, skipped, or never-claimed step contributes **nothing**. The accrual is per *claim event*, and a step that was never claimed produced none |

**Why events and not a counter.** Four reasons, in the order they matter:

1. **C4, the race.** A stored running total is `read → add → write`. Two claims
   committing concurrently read the same total and one increment is lost. A
   SUM over an append-only log has no read-modify-write and therefore no lost
   update — the concurrency property is structural, not defended.
2. **The engine-owned-facts argument is only true if the facts are the log.**
   §7 says the floor rests on "facts the engine itself produced". `step-claimed`
   events *are* those facts; a counter is a derived number that could drift
   from them, and a drifted counter is exactly the "voluntary reporting" the
   floor exists to be independent of.
3. **§9 item 2's audit.** Every transition must be attributable. A budget
   breach attributable to a counter is attributable to nothing; a breach
   attributable to "these seventeen claim events summing past your cap" is a
   trail an operator can read, and §8.7 makes it exactly that.
4. **Replay.** engine-core §9: the event log reconstructs a run's history
   "costing what". If the cost lives outside the log, replay cannot answer it.

**The cost.** A SUM over an indexed join, once per claim and once per report.
`idx_events_run_seq` covers `run_id`; the join is on the primary key. §12.2
records the measured numbers, and the fallback if they ever stop being
acceptable is stated there — a cache checked against the query, never a cache
replacing it.

`runs.usage_floor` is written in the same transaction as each accrual **as a
cache for the report only**. `TestFloorIsNeverReadFromCache` seeds it with a
deliberately wrong value and asserts that enforcement, refusal, and the breach
transition all behave as though it said the truth.

## 4.4 Enforcement: `max(reported, floor)`, checked in the claim's transaction

| # | Clause |
|---|---|
| B12 | The enforced quantity is `max(reported, floor)` where `reported` is the ledger's sum and `floor` is §4.3's query |
| B13 | **Reported usage can only RAISE the counter.** That is what `max` means, and it is why a claimant reporting `0` — or nothing at all — cannot lower a run below its floor. §9 item 7's whole proof is this clause |
| B14 | The check is `max(reported, floor) + this step's expected_cost <= cap`. A claim that would **cross** the cap is refused; a claim that lands exactly on it is allowed. "Crossing" is `>`, not `>=`: a cap of 12 and a floor that reaches exactly 12 has spent its budget and not exceeded it |
| B15 | The check and the accrual are **the same transaction as the claim's CAS** (C5). There is no window in which a claim is authorized and the cap is then crossed by another |

**`reported` sums across units by summing each unit separately and taking…
nothing.** This needs care and §4.5 gives it.

## 4.5 What "reported" means when usage is a bag of opaque units

engine-core §7 says usage is "opaque numbers", and §1.1 of this document says
core never decides two units are commensurable. But `max(reported, floor)`
compares `reported` to a *single* number.

**The resolution, pinned:**

| # | Clause |
|---|---|
| B16 | `reported` for the purpose of `max(reported, floor)` is the sum over the ledger rows whose `unit` matches the **budget unit**, and the budget unit is `docket config budget.unit` (default `""`) |
| B17 | When `budget.unit` is `""` — the default and the only value core ships — **`reported` is 0 and the enforcement rests entirely on the floor.** `max(0, floor) = floor` |
| B18 | Setting `budget.unit = "tokens"` makes `reported` the sum of `tokens` ledger rows. Core still does not know what a token is; it knows the operator named that unit as the one their cap counts |
| B19 | Ledger rows in every other unit are recorded, summed per-unit in the report, and **never compared to the cap** |

**Why not "sum everything".** Summing `{tokens: 4000, seconds: 12}` to 4012
would be core asserting those add up. That is a meaning, and core does not have
meanings about units — §1.1's third leak, closed. Summing nothing and resting
on the floor is the honest default and is *exactly* §9 item 7's configuration
("with reporting disabled, the run still pauses at the cap from the floor").

**Why a config key and not a workflow field.** The cap is a run-level control
(§11.3: "the per-run budget cap (`docket run start --budget N`, config
default)"), so the unit it counts is run-level too. Putting it on a step would
let two steps in one run disagree about what the run's budget means.

**`budget.unit` is added to the `docket config` table** (§4.11) with a doc line
saying exactly this: *"Which recorded usage unit the run cap counts. Empty (the
default) means the cap rests on the declared-cost floor alone."* That sentence
passes the stranger test on its face.

## 4.6 The breach: `waiting-human` with a reason

| # | Clause |
|---|---|
| B20 | When the check in B14 fails, the claim is **refused** and the run is flipped `active → waiting-human` with `breach_reason` recorded, **in one transaction** |
| B21 | The reason's shape: `budget: spend <N> of cap <M> reached at <instance>` (spend = the enforced `max(reported, floor)`) — a bare-number statement naming no unit, per §1.1's second leak |
| B22 | The transition is CAS on `(run_id, status = 'active')` (C6). A concurrent invocation that already flipped it matches zero rows and does not write a second reason or a second event |
| B23 | The event is `run-paused` with `data.reason = "budget"` — an **existing** closed-set kind (engine-spine §7.6), so the set does not widen. §8.7 explains why this is the right reuse rather than a new `budget-breached` kind |
| B24 | `run resume` clears nothing and the run pauses again on the next claim, **unless** the operator raises the cap. That is the correct behavior and it is stated because "resume immediately re-pauses" reads as a bug: the cap has not moved, so the condition has not changed |
| B25 | Raising the cap is `docket run start`-time only in v1. **Deviation filed** — see §13, DKT-29: there is no `run budget` verb in §1's surface summary, so an operator who must raise a cap mid-run resumes and re-pauses forever. The TDD recommends the amendment rather than inventing the verb |
| B26 | The reconciliation rollup's automatic resume (§4.7's `reconcileRun`, "still working, return to `active`") is NOT the same code path as `run resume` and must not fire for a budget breach. `BreachRunBudgetTx` (B20) parks only the run row, not any step, so the rollup's own "any step still parked" check reads zero for a budget-paused run — a step claimed BEFORE the breach, completing AFTER it, routes through the rollup with `parked == 0` and `unfinished > 0`. Guarded (DKT-68, generalized by DKT-305): the rollup checks `runs.pause_origin` before its default resume and stays parked while it is set, so only `run resume` (B24) or a breach-resolving `run budget --set` (DKT-80, which releases the `budget` origin with the breach record) can move the run out of the breach |
| B26a | **The same guard covers `run pause` (DKT-305).** A run-level pause parks no step either, and it writes no `breach_reason` — nor should it, that column is the budget's own record — so before v22 an operator's pause was undone by the next step to route, with an empty-data `run-resumed` naming nobody (RUN-31: paused seq 3054, resumed seq 3077, then a four-judge review, a synthesize, a reconcile, and two votes ran unattended). `MoveRun` therefore writes `pause_origin = 'operator'` with the pause and clears it with the resume or abandon that ends the park, and the rollup declines to resume ANY run-level origin. A STEP-level park is unaffected: its origin is empty, the rollup created that park and the rollup resolves it, or every retry would need an operator verb |

**B25 is the one place this stage found the spec genuinely incomplete**, and it
is filed rather than fixed, per CLAUDE.md. The workaround until the amendment
lands is documented in SKILL.md and operations.md: abandon and re-start with a
larger `--budget`, which is lossy, or edit `runs.budget` with `sqlite3`, which
is worse. Saying so plainly is better than shipping a verb the spec's surface
summary does not list.

## 4.7 Warn thresholds are INSTANCE policy — core ships the cap only

engine-spec §8, verbatim: *"budget warn 60% / pause 100%"* — listed under
**"Reference instance configuration (examples, not core defaults)"**, whose own
last line is *"Core ships with no opinions here."*

| # | Clause |
|---|---|
| B26 | Core ships **no** warn threshold, no `budget.warn` config key, and no warn event |
| B27 | `run report` publishes the numbers a warn policy needs — cap, floor, reported per unit, and burn rate — so an instance computes its own warn from a read verb |
| B28 | The reference instance's 60% warn is therefore its dispatcher reading `run report --json` and deciding. Core's contribution is the number, not the opinion |

**The argument, because it will be re-litigated:** a warn is a decision about
when a *person* should look at something, which depends entirely on what the
number counts, how long the run takes, and who is watching. Core knows none of
those. A `budget.warn = 0.6` shipping in core would be core asserting that 60%
is a meaningful moment — the exact class of statement the genericity rule
exists to keep out, arriving through a config key rather than a flag name.

## 4.8 R7 becomes the real check

engine-spine §6.3's R7 row: *"budget headroom exists — **S6**; at S3 the check
is a seam returning `true`."* `Scheduler.budgetHeadroom` in
`internal/engine/ready.go` is that seam, and it is written as a method
precisely so this stage adds the query behind it and the call site in `Ready`
never moves.

| # | Clause |
|---|---|
| B29 | `budgetHeadroom` returns `true` **on its first line** when the effective cap is 0 (D1's dormancy — no query executes) |
| B30 | Otherwise it answers B14's arithmetic over the scheduler's snapshot: would claiming this step cross the cap? |
| B31 | The floor and the reported sum are loaded **once per `LoadScheduler`**, into the snapshot, for the reason the Scheduler is a value at all: R3, R4, R5 and now R7 are questions about the same rows at the same instant |
| B32 | A step refused by R7 reports `CondBudget` — the constant that has existed since S3 and never fired. Its text, `"no budget headroom"`, is unchanged |

**R7's position in the conjunction does not move.** It stays last, per §6.3's
stated ordering rationale (cheapest and most global first, most local last) —
and budget is the most local of all, because it is the only clause whose answer
depends on the *specific step's* cost.

**The relationship between R7 and B14, stated because it looks like
duplication:** R7 makes a step *not appear in `next`*; B14 makes a claim
*refuse*. They are the same arithmetic at two moments, and both are needed. R7
alone would let a relay claim a step it already held a stale `next` row for.
B14 alone would offer steps that cannot be claimed, which is the "stalls
loudly" failure §2's whole recovery design avoids in the dispatch case and
should avoid here too. `TestBudgetR7AndClaimAgree` asserts they never disagree
over the same snapshot.

## 4.9 `--usage`: recorded, capped, opaque

`--usage` has been accepted and stored in the `step-recorded` event's `data`
since S3 (`CompleteOptions.Usage`). **The event keeps carrying it** — removing
it would break the replay property — and this stage *additionally* writes
ledger rows.

| # | Clause |
|---|---|
| B33 | `--usage '{"unit": n, …}'` is parsed as a JSON object of string → number. Any other shape is `VALIDATION_ERROR` naming the offending key |
| B34 | One ledger row per key, `source = 'reported'`, keyed `(step_id, attempt, unit)`. A second `complete` for the same attempt is impossible (the saga's stage-0 CAS), so the unique key is a belt-and-braces assertion rather than an upsert |
| B35 | **Numbers are opaque and may be any finite non-negative float.** Negative is `VALIDATION_ERROR` — "usage" that reduces a total is not usage — and NaN/Inf are refused by the same check |
| B36 | Caps, per §1.3's security note: at most **32 units** per call, unit names at most **64 bytes**, and each name must be printable ASCII without whitespace. Over any cap is `VALIDATION_ERROR` naming the limit |
| B37 | `source` is recorded because engine-core §7 says so verbatim ("source recorded"). Core writes `'reported'`; the column exists so a harness back-filling from its own journal can record `'backfill'` through a future verb without a migration. **No verb writes any other value at this stage**, and §13 does not file an issue for it because §7's sentence describes the reference harness's practice, not a core surface |

**What core never does with these numbers**: interpret them, convert them,
rate-limit on them, or route on them. They sum in the report and, when
`budget.unit` names one, they participate in `max()`. That is the whole
contract.

## 4.10 `run report`: the ledger rollup

engine-core §1.1 (*"the ledger rollup (usage, cost, wall-clock, attempts)"*) and
§3.5 (*"The run report (ledger rollup + gate summary + artifact index) is
generated deterministically: `docket run report`"*).

**Built on the `internal/cli/stats.go` pattern**, which is the repo's existing
rollup verb: a flat result struct with JSON tags, a `runXxx` function that
calls one `db.CountXxx`-shaped query per section, a `renderXxx` for human mode
and a `renderPlainXxx` for the no-color path, and `w.Success(result, message)`.
Following it means the report gains `--json=v2`'s envelope, the color
discipline, and the plain-text fallback without inventing any of them.

| # | Section | Contents | Source |
|---|---|---|---|
| R1 | **run** | id, status, reason, request, activated-at, wall-clock (activation → now or → terminal) | `runs` |
| R2 | **budget** | effective cap, its source (`--budget` \| `config` \| `unlimited`), the floor, reported-per-unit, `max(reported, floor)`, burn rate (floor ÷ wall-clock-hours), and `breach_reason` when set | §4.3, `usage_ledger` |
| R3 | **steps** | count by effective status, and per-step attempts | `steps`, effective-status computed |
| R4 | **gates** | per-gate pass/fail/unmatched counts, plus the per-step trail | `gate_results` |
| R5 | **actions** | the same rollup over `action_results`, including `builtin` | `action_results` |
| R6 | **artifacts** | the index: id, kind, producer instance, sha256, bytes — never the bodies | `artifacts` |
| R7 | **metadata** | rollup of step `metadata` keys → distinct values with counts, **verbatim and uninterpreted** | `steps.metadata` |

**R7 is the genericity line at its thinnest, so it is specified exactly.** The
rollup groups by key and by value, both as opaque strings, and reports counts.
It does not know that the reference instance puts a model tier there; it
reports `{"tier": {"a": 3, "b": 1}}` for the same reason it would report
`{"desk": {"front": 3, "back": 1}}`. **`TestMetadataRollupReadsNoKey`** asserts
the implementation contains no key-name literal.

| # | Clause |
|---|---|
| R8 | **The report WRITES NOTHING.** It computes effective status at read (engine-spec §2: "Read verbs render *effective* status … no write") and opens a read-only transaction. `TestReadVerbsWriteNothing` snapshots the DB's page-level content before and after |
| R9 | It is **deterministic** given the same rows: every section is ordered by a total key (id, then name), never by map iteration, for the same golden-stability reason `referencedSchemas` is ordered |
| R10 | It works on a run in **any** status, including `planning` (everything zero) and `abandoned` (the trail up to abandonment). A report that refused on a non-terminal run would be useless during exactly the run an operator wants to inspect |
| R11 | Human mode renders through gates-trust §5.7's escaping renderer wherever a stored string appears (gate output excerpts, metadata values, breach reasons); `--json` carries raw bytes |

## 4.11 Config keys added by this stage

| Key | Default | Doc line |
|---|---|---|
| `budget.unit` | `""` | Which recorded usage unit the run cap counts. Empty (the default) means the cap rests on the declared-cost floor alone. |
| `dispatch.ttl` | `"30m"` | How long a dispatch manifest stays open before `next` auto-abandons it. |
| `dispatch.grace` | `"15m"` | How long a claimed step may go unrecorded before it counts as a dispatch discrepancy. |

`budget.default` **already exists** (`db.KeyBudgetDefault`, since S3) and gains
enforcement, not a definition. The three above follow the existing
`internal/db/engineconfig.go` table shape — key, default, doc — so
`docket config set|get` lists them with no CLI change.

## 4.12 Test plan — group 1

**Go unit tests** (`internal/engine/budget_test.go`, `internal/cli/run_report_test.go`):

- The floor table, one case per B8–B11: a plain claim; a reaped-and-reclaimed
  step (**two** accruals); a four-way fanout (**four**); a loop entry at
  ordinal 1 (a new row, a new accrual); a superseded step (**zero**); a skipped
  step (zero); a never-claimed ready step (zero).
- `TestFloorBoundedByMaxFixLoops` — B10 at `max_fix_loops = 2`: the floor
  cannot exceed the arithmetic bound, ever, and a third loop entry is
  impossible.
- `TestFloorIsNeverReadFromCache` — §4.3's separation, with a poisoned cache.
- `max(reported, floor)` across B12/B13/B17/B18: reporting nothing; reporting
  less than the floor; reporting more than the floor; reporting in a unit that
  is not `budget.unit`; reporting in the unit that is.
- B14's boundary: exactly-at-cap allowed, one-past refused, both at float
  values that stress binary representation (0.1 × 10 vs 1.0).
- `TestBudgetR7AndClaimAgree` — §4.8's agreement property, over a generated
  matrix of (cap, floor, cost).
- The breach transition: B20's single transaction; B22's CAS idempotency under
  two concurrent flips; B24's re-pause after resume.
- D1's dormancy: with cap 0, `budgetHeadroom` executes **no query**, asserted
  by a counting `sql.DB` wrapper rather than by inspection.
- `--usage` validation, one case per B33/B35/B36 cap and shape.
- The report's determinism (R9) and its zero-write property (R8), and
  `TestMetadataRollupReadsNoKey` (R7).

**QA** — new section **`ZI`** (`scripts/qa/test_zi_budget.sh`), specified in
full at §9.2, whose headline is §9 item 7.

---

# Group 2 — dispatch manifests, `next`'s refusal, and the write-reap acknowledgment

Commit group 2. Independently stoppable: it ships the dispatch family and the
reap-ack over a `next` that refuses only when a relay opted into dispatches or
a write-class lease was actually reaped. A repo doing neither sees group 1's
`next`.

## 5. DISPATCH, per engine-spec §2's scheduling block, complete

engine-spec §2, verbatim:

> `dispatch open/close/verify` give batch dispatchers a manifest to verify
> against byte-for-byte and make "unreconciled batch" an engine-refusal state —
> with recovery designed in: exactly one dispatch open per run (CAS/unique
> index), a dispatch TTL lazily auto-abandoned by `next` (event-logged),
> explicit `dispatch abandon` for a crashed relay, and enumerated discrepancy
> resolutions (lease expiry clears claimed-but-unrecorded;
> `dispatch close --accept-missing-usage` records the acceptance).

and engine-core §5:

> `docket dispatch open` records the batch manifest; `next` **refuses** while a
> dispatch is open or while discrepancies exist (claimed-but-unrecorded past
> grace, usage rows missing) — relay drift stalls loudly instead of proceeding
> around its own mess.

## 5.1 What a dispatch IS, in one paragraph

A dispatch is a **frozen copy of one `next` answer**, recorded so that the
relay's spawns can be checked against what the engine actually offered. It is
not a lock, not a reservation, and not a claim: the steps in it are still
`pending` and any claimant may still claim them. What it buys is that the
engine can refuse to *offer a new batch* while the previous one is
unreconciled — which turns a relay that lost track of its own spawns from a
silent double-executor into a stalled run with a reason.

**Stating what it is not matters** because a reviewer's first instinct is to
ask why `dispatch open` does not claim the steps. It does not because claiming
is a capability transfer with a token and a lease, and a batch dispatcher is
not the executor — it is the thing that starts executors. Claiming on its
behalf would mint a token nobody holds.

## 5.2 The manifest = ordered `next` rows

| # | Clause |
|---|---|
| P1 | `dispatch open --run RUN-N` computes the ready set **exactly as `next` does** — the same `LoadScheduler`, the same predicate, the same `SortSteps` — and records the resulting rows in order |
| P2 | The response is §11.4's shape verbatim: `{ dispatch, run, opened_seq, rows: [<next row>…] }` |
| P3 | Each row is stored as its **canonical JSON bytes** plus their sha256. Canonical means the same marshaling the wire uses, so a stored row and a fetched row are byte-identical by construction rather than by a re-serialization that could differ in key order |
| P4 | `--limit` applies, with the same ordering-then-slicing rule as `next` (§6.3), so a relay can open a manifest for the batch size it can actually spawn |
| P5 | `dispatch open` **performs the same lazy reap `next` does** before computing. It is a scheduling verb offering a batch; offering a stale step that a reap would have freed would make the manifest wrong the moment it was written |
| P6 | Opening while a dispatch is already open is `CONFLICT` (exit 4), naming the open dispatch's id and its expiry — C1, enforced by `idx_dispatches_one_open` rather than by a check-then-insert |

**`opened_seq` is the event seq at open time**, and it is the manifest's place
in the log. §6 uses it as the boundary for "reaps this relay has not yet seen".

## 5.3 `dispatch verify`: byte-equality on rows

| # | Clause |
|---|---|
| P7 | `dispatch verify --run RUN-N` recomputes the ready set and compares it to the stored manifest **row by row, byte for byte** |
| P8 | Equal ⇒ exit 0 and a `{verified: true}` payload |
| P9 | Unequal ⇒ `CONFLICT` (exit 4) naming the **first differing position**, with the stored row and the recomputed row both rendered. Not "the manifest is stale" — the differing bytes, so an operator can see whether a lease lapsed, a priority changed, or a step completed |
| P10 | Verification compares the **hash first**; the byte rendering happens only on the mismatch path (§3.1's shape rationale) |
| P11 | `verify` is a **read verb and writes nothing** — including no reap. This is the one scheduling-shaped verb that must not reap, because reaping would change the very ready set it was asked to compare against, and a verify that mutated its own subject could never fail |

**P11 is the subtle one and is a test**: `TestVerifyDoesNotReap` opens a
dispatch, expires a lease, verifies (which must report the mismatch caused by
the *effective* status change, computed not written), and asserts the step row
still carries its stale owner afterward.

## 5.4 Exactly one open per run

Covered by `idx_dispatches_one_open` (§3.1) and P6. **C1 in full:** two relays
calling `dispatch open` in the same millisecond both compute a ready set, both
INSERT, and SQLite's unique index admits exactly one. The loser's transaction
fails, the error is mapped to `CONFLICT`, and — critically — **the loser's
computation is discarded, not merged**. A merge would produce a manifest that
neither relay saw.

## 5.5 The TTL, lazily auto-abandoned by `next`

| # | Clause |
|---|---|
| P12 | A dispatch expires at `created_at_ms + dispatch.ttl` (§4.11, default 30m) |
| P13 | **`next` auto-abandons an expired dispatch** before evaluating its own refusal — the one lazy path this stage adds, and the one §2 names explicitly |
| P14 | The abandon is `UPDATE dispatches SET status='abandoned', close_reason='ttl' WHERE id=? AND status='open'` — C3: exactly one row matches, exactly one event |
| P15 | It is **event-logged** (§2: "lazily auto-abandoned by `next` (event-logged)") with a new closed-set kind, `dispatch-abandoned`, carrying `data.reason = "ttl"` |
| P16 | `next` then proceeds normally in the **same invocation**. A relay that crashed and came back does not poll twice to get work — the same reasoning that puts the reap and the readiness pass in one transaction (§6.3) |
| P17 | `claim` does **not** auto-abandon. Reaping is confined to `next`/`claim` because both are scheduling decisions about *one step*; a dispatch is about a *batch*, and expiring one from a single-step verb would let a claim silently unwedge a run whose relay is still alive |

**P17 is a deliberate narrowing of the lazy-path rule and is stated so a
reviewer does not read it as an omission.** engine-spec §6's rule is about
*lease* reaping. Dispatch expiry is a different mechanism with a different
scope, and §2 assigns it to `next` alone.

## 5.6 `dispatch close` and `dispatch abandon`

| # | Clause |
|---|---|
| P18 | `dispatch close --run RUN-N` closes the open dispatch **only if no discrepancy exists** (§5.8). With one, it refuses `CONFLICT`, enumerating each discrepancy and its resolution |
| P19 | `close --accept-missing-usage` closes despite missing-usage discrepancies **and records the acceptance** — `close_reason = 'accepted-missing-usage'` plus the accepted step list in the event's `data`. §2 names this flag verbatim |
| P20 | `--accept-missing-usage` does **not** accept the other discrepancy class. Claimed-but-unrecorded past grace has its own resolution (lease expiry), and a flag that accepted both would let a relay close over work that is still running |
| P21 | `dispatch abandon --run RUN-N [--reason …]` closes it unconditionally — "explicit `dispatch abandon` for a crashed relay". No discrepancy blocks it: the whole point is that the relay is gone and cannot resolve anything |
| P22 | Both are CAS on `(id, status='open')` (C2). A close racing the TTL abandon loses and reports `CONFLICT` naming the abandoning event's seq, so the relay learns *why* rather than "not open" |
| P23 | Closing or abandoning writes `dispatch-closed` / `dispatch-abandoned` events |

## 5.7 `next` REFUSES while a dispatch is open or discrepancies exist

engine-core §5's sentence, implemented:

| # | Clause |
|---|---|
| P24 | After the lazy TTL abandon (P13), `next --run` probes for an open dispatch — AFTER the lease reap runs (amended per DKT-30: default grace equals the default lease TTL, so the written order would refuse on a discrepancy the same invocation was about to clear). Present ⇒ **refuse**, exit `CONFLICT` (4), with a reason naming the dispatch, its expiry, and the three ways out: `dispatch close`, `dispatch abandon`, or waiting for the TTL |
| P25 | Absent ⇒ probe for discrepancies (§5.8). Any ⇒ **refuse**, exit `CONFLICT`, enumerating each with its resolution |
| P26 | The refusal is a **`CONFLICT`, not an empty ready set**. An empty set means "nothing to do"; a refusal means "I will not answer until you reconcile". A relay cannot distinguish those from a zero-length list, and conflating them is precisely the silent proceeding §2 forbids |
| P27 | **Issue-mode `next` is unaffected.** `docket next` without `--run` never probes anything — engine-spine §6.3.1's byte-identical guarantee is preserved, and `test_x_next.sh` continues to pass untouched |
| P28 | `claim` is **not** refused by an open dispatch. A dispatch is not a lock (§5.1); an executor holding a valid `next` row from before the dispatch opened may still claim. Refusing here would break the recovery story — a relay that crashed after spawning would leave its own executors unable to work |

**P28 is the clause a reviewer should push hardest on**, so the argument is
written out. If `claim` refused while a dispatch were open, then: relay opens a
dispatch, spawns four executors, crashes. The executors each `claim` and are
refused. Nothing can complete. The TTL eventually abandons the dispatch, by
which point the four executors have failed. §2's recovery design ("a crashed
relay can never wedge a run") is violated by the very mechanism meant to
implement it. So `claim` proceeds and `next` refuses, which stalls *new*
scheduling without stranding *in-flight* work.

## 5.8 The enumerated discrepancies, and their resolutions

engine-core §5 names exactly two classes; both are **computed, never stored**
(§3.2).

| # | Discrepancy | Definition | Resolution |
|---|---|---|---|
| D1 | **Claimed but unrecorded past grace** | a step in `claimed`/`running` whose `activity_ms` is older than `dispatch.grace` (§4.11, default 15m) | **lease expiry clears it** — §2 verbatim. The step's TTL lapses, `next` reaps it, and the discrepancy dissolves. `dispatch close` names the expiry time so an operator knows how long to wait |
| D2 | **Usage rows missing** | a step that reached a terminal status **after** the run's activation, on a v10 binary, with zero `usage_ledger` rows, **in a run that has ever opened a dispatch** (a `dispatch-opened` event exists — usage completeness is a RELAY contract; a run no relay ever drove has nobody owing usage) | `dispatch close --accept-missing-usage`, which records the acceptance (P19) |

| # | Clause |
|---|---|
| D3 | **D2's scope is deliberately narrow.** Steps completed before v10 have no ledger rows and are not discrepancies; the probe therefore excludes steps whose `updated_at_ms` predates the run's `activated_at_ms`, and excludes runs activated before the migration stamped 10. Without this, upgrading the binary would instantly make every historical run's dispatch un-closable — a migration that breaks in-flight work, which is exactly what §4.9.3 of the payloads TDD refused for schemas |
| D4 | **`expected_cost = 0` steps still require usage rows under D2.** The floor and the ledger are independent mechanisms; a free step that reported nothing is still a step whose usage the relay did not record |
| D5 | **Action and human steps are exempt from D2.** No worker claims them (they are engine-run or operator-resolved), so there is nobody to have reported usage. Including them would make every fixture run permanently un-closable |
| D6 | A run with no open dispatch is still probed for discrepancies by `next` (P25). Discrepancies are a property of the *run*, not of the manifest — a relay that never opened a dispatch can still leave a claimed step unrecorded |

**D6 is the clause that makes this stage change `next`'s behavior for repos
that never touch dispatches**, and it deserves its dormancy statement: with no
dispatch ever opened, D1 fires only when a step has genuinely been claimed and
silent for fifteen minutes, and D2 fires only when a step completed with no
usage. **A repo whose relay records usage and whose executors either finish or
die never sees a refusal.** §3's D2 row and the group-2 dormancy QA check
assert exactly this over the ZG fixture, which does neither.

**Deviation candidate, examined and rejected:** D2 could be scoped to steps
*in the open manifest* only, which would make it perfectly dormant. It is not,
because engine-core §5 lists the two classes as properties of the run ("while
discrepancies exist"), and because a relay's most dangerous drift is precisely
the step it spawned outside a manifest. The cost is D6's behavior change; the
mitigation is that `usage_ledger` rows are written by any `complete --usage`,
which the ZI QA section proves is the ordinary path.

**The adopted scope is run-level dispatch history, not manifest membership**
(review fix, 2026-08-03): a relay that ever dispatched is accountable for every
step of that run, including out-of-manifest spawns — the drift D6 exists to
catch — while a run no relay drove has nobody owing usage, which is what keeps
ZK's solo rehearsal and ZH's stranger demo refusal-free (they complete
without `--usage` and open no dispatch).

## 5.9 §9 item 9 as the proof

engine-spec §9 item 9, verbatim: *"Dispatch recovery: kill the relay with a
dispatch open — TTL auto-abandon (or an explicit `dispatch abandon`) restores
`next`; nothing is lost or double-executed."*

The QA section (§9.2, `ZJ`) executes it literally, both arms:

| # | Step | Assertion |
|---|---|---|
| K1 | open a dispatch over a real ready set | manifest recorded; `verify` passes |
| K2 | `next --run` | **refused**, `CONFLICT`, reason names the dispatch |
| K3 | *(arm A)* advance the clock past `dispatch.ttl`, then `next --run` | the dispatch is `abandoned` with `reason=ttl`; a `dispatch-abandoned` event exists; **the same invocation returns the ready set** (P16) |
| K4 | *(arm B)* from K2, `dispatch abandon --reason "relay died"` | closed immediately; `next` answers |
| K5 | **nothing lost** | the ready set after recovery contains every step the manifest did, with the same instances |
| K6 | **nothing double-executed** | each step is claimed exactly once across the whole scenario, asserted by counting `step-claimed` events per instance |
| K7 | a step claimed *before* the crash | completes normally afterward (P28), and its artifact is recorded exactly once |

**"The clock advanced" is not a `sleep`.** The QA section sets
`dispatch.ttl` to a small value via `docket config` and waits deterministically,
following the repo's existing TTL-flake discipline; the Go test injects `nowMS`
directly.

---

# 6. THE WRITE-REAP ACKNOWLEDGMENT — §9 item 10's deferred half

gates-trust §1's scope table deferred exactly this:

> The **write-class reap-acknowledgment half of §9 item 10** ("a reaped
> write-class step cannot gain a successor until the reap is acknowledged") —
> S6 — it is dispatch mechanics: `guard spawn` surfaces the `reaped` event
> (§2), and `guard spawn` is stage 6's.

engine-spec §2, verbatim, is all the spec says:

> Reaping a **write-class** lease additionally holds write headroom until the
> relay acknowledges the `reaped` event (surfaced by `guard spawn`) — the DB
> fence is not a tree fence; a wedged writer must be confirmed gone before a
> successor writes.

**The spec names the surfacing and not the acknowledgment.** The mechanism is
this TDD's to pin, and §6.2 is the argument for the one chosen.

## 6.1 What the mechanism must achieve

The hazard, stated concretely: a write-class step's lease lapses. The engine
believes the executor is dead and returns the step to `ready`. But the lease is
a *database* fence — nothing about it stops a still-running process from
writing to the working tree. If a successor write-class step starts while the
supposedly-dead writer is still alive, two processes edit one tree and every
gate computed over it is meaningless.

Requirements, each of which the chosen mechanism must satisfy:

| # | Requirement | Why |
|---|---|---|
| A1 | A reaped write-class step's **successor gains no write headroom** until the reap is acknowledged | the spec's sentence |
| A2 | The ack must **not require the dead relay** | the relay may be the thing that died; an ack only a corpse can give is a permanent wedge, and §2's whole recovery posture forbids wedges |
| A3 | The ack must be **attributable** | §9 item 2: every transition traceable |
| A4 | The ack must be **idempotent and race-free** | C7 |
| A5 | The ack must **not be automatic on a timer** | an ack that happens by itself acknowledges nothing; the point is that somebody confirmed the writer is gone |
| A6 | The hold must be **narrow**: it holds *write* headroom, not the whole run | read fan-outs "parallelize freely" (engine-core §5), and a reaped writer must not stop them |

## 6.2 The mechanism, pinned: `guard spawn --ack-reap SEQ`, with `dispatch open` as the batch path

**Chosen:** acknowledgment is an explicit act naming the reap's event `seq`,
available through **two** entry points that write the same row:

| Entry point | Who uses it | Shape |
|---|---|---|
| `docket guard spawn --run RUN-N --ack-reap SEQ` (repeatable) | a live relay, at its spawn hook | acknowledges the named reaps, then answers the ordinary spawn predicate |
| `docket dispatch open --run RUN-N --ack-reap SEQ` (repeatable) | a **new** relay taking over from a crashed one | acknowledges as part of claiming the next batch |

| # | Clause |
|---|---|
| A7 | An ack sets `reap_acks.acked_at_ms` and `acked_by` for the row whose `reaped_seq` matches, CAS on `acked_at_ms IS NULL` (C7, A4) |
| A8 | `acked_by` records the acknowledging **verb and run**, never a user identity — core has no identity model. Its value is one of `guard-spawn` \| `dispatch-open` |
| A9 | Acknowledging a seq that is not a reap, or not this run's, is `VALIDATION_ERROR` naming the seq. **A4's forgery point**: an ack must name a real reap |
| A10 | An ack of an already-acknowledged reap is a **success that changes nothing** (idempotent), so a relay retrying its hook does not fail |
| A11 | `guard spawn` **without** `--ack-reap` still *reports* unacknowledged reaps in its denial reason, naming each seq and the exact flag to pass. That is §2's "surfaced by `guard spawn`", and it is what makes the mechanism discoverable rather than documented |

**Why this shape, argued against the alternatives:**

| Alternative | Rejected because |
|---|---|
| **Automatic ack after a timeout** | violates A5 outright. It converts "confirmed gone" into "probably gone by now", which is the assumption the tree-fence hazard exists because we cannot make |
| **Ack implied by the next `next` call** | violates A5 the same way, and worse: `next` is called by anything, including a human running it to look. A read-shaped verb would silently authorize a write |
| **Ack by the dead executor's token** | violates A2 fatally — the token holder is the thing presumed dead |
| **Ack by `dispatch close`** | violates A2 in the crash case: the relay that opened the dispatch is gone, and `dispatch abandon` (the crash path) must stay unconditional (P21). Tying the ack there would make a crashed relay's reap permanently unacknowledgeable |
| **A dedicated `docket reap ack` verb** | adds a verb to §1's surface summary, which is a spec deviation for no gain. `guard spawn` is where §2 already puts the surfacing, and `dispatch open` is where a *new* relay's first act naturally goes |

**A2 in full — the crashed-relay argument.** Relay opens a dispatch, spawns a
write-class executor, and both die. The lease lapses; `next` reaps it and
records an unacknowledged write reap. Now:

1. The dispatch TTL auto-abandons (P13), so `next` answers again.
2. `next` offers read-class steps freely (A6) — the run is not stopped.
3. It offers **no write-class step**, because the hold is live.
4. A **new** relay session starts, runs `guard spawn`, and is denied with a
   reason naming the seq and the flag.
5. It confirms the tree is quiet (its own business — core cannot check a
   process it did not start), then runs `dispatch open --ack-reap <seq>` or
   `guard spawn --ack-reap <seq>`.
6. Write headroom returns.

**Nothing in that sequence requires the dead relay.** The new session — which
may be a person typing — is the acknowledger. That is the honest division: core
knows the DB fence lapsed and refuses to pretend it is a tree fence; a human or
a live harness supplies the one fact core cannot observe.

## 6.3 The hold is a PREDICATE, not a flag

| # | Clause |
|---|---|
| A12 | The hold is evaluated inside the readiness transaction as: *does an unacknowledged `reap_acks` row exist for this run whose `class` equals this step's class, where that class's `[limits] max` is finite?* |
| A13 | It is **not** a stored "held" flag on the run or the class. C8: a flag written at reap time and cleared at ack time has a window between the two writes in which a concurrent reader sees the wrong answer. A predicate over the rows has no window |
| A14 | The hold applies to **the reaped step's class**, whatever it is called. Core ships no class named `write`; engine-core §5's "write-class" means "the class the instance serializes", which core knows only as *a class with a finite `[limits] max`*. §6.5 argues this |
| A15 | The hold counts toward `classHeadroom` (R5) rather than adding a new `ReadyCondition`. An unacknowledged reap **occupies a slot** in its class — which is exactly what "holds write headroom" says — EXCEPT against the reaped step itself, which re-offers per §9 item 4 (amended per DKT-31: the literal fold would stop a max=1 class dead at its first lapsed lease, contradicting W3) |

**A15's mechanism, precisely.** `Scheduler.classHeadroom` counts in-flight
steps as `claimed + running + gated`. This stage adds `+ unacknowledgedReaps`
for the class. With `[limits] write = { max = 1 }`, one unacknowledged reap
makes `inFlight = 1`, `1 < 1` is false, and no write-class step is ready. The
refusal reports `CondHeadroom` — the existing constant, whose text is "no
concurrency headroom in its class" — and `next`'s human output *additionally*
names the unacknowledged reaps, because a headroom denial with nothing running
is otherwise baffling.

**Why fold it into headroom rather than add R8.** Because it *is* headroom.
The predicate answers "how many things may write to this tree right now", and
a lapsed-but-unconfirmed writer is one of them. A separate condition would be
two mechanisms answering one question, and §6.5's `TestReapHoldIsHeadroom`
asserts the fold by proving a class with `max = 2` and one unacknowledged reap
still admits one step.

## 6.4 The reap records the ack row

| # | Clause |
|---|---|
| A16 | When `next` reaps a lease (`internal/engine/next.go`'s existing reap loop), it additionally inserts a `reap_acks` row **in the same transaction** — but **only** when the step's class has a finite `[limits] max` (A14) |
| A17 | `reaped_seq` is the `seq` of the `lease-reaped` event written by that same reap. The insert therefore happens after the event, in the same transaction, reading `last_insert_rowid()` |
| A18 | A class with no declared `max` inserts **nothing**. D3's dormancy: a repo with no `[limits]` never creates a `reap_acks` row and never holds anything |
| A19 | The row is created **unacknowledged** (`acked_at_ms IS NULL`) |

## 6.5 "Write-class" without a class named `write`

engine-core §5 says "write-class lease". engine-spec §2 says the reference
instance's config "sets its write class to 1 — serialization is *instance
policy*, not core behavior". `Scheduler.classHeadroom` already refuses to
hardcode a class named `write`, and its comment says so.

**The translation, pinned:** *write-class* means **a class whose effective
`[limits] max` is finite**. The reasoning, from engine-core §5's own argument
for why writers serialize: "gates computed on a shared mutable tree race even
across disjoint scopes", and the instance expresses that by bounding the class.
A class the author left unbounded is a class the author declared safe to
parallelize — a read fan-out — and holding headroom on it would contradict
"read-only fan-outs parallelize freely".

| # | Clause |
|---|---|
| A20 | A class with a finite `max` gets reap-ack rows and the hold |
| A21 | A class with no `max` gets neither, and `next` behaves exactly as it did at S5 |
| A22 | Core never reads the string `"write"`. `TestNoWriteClassLiteral` greps the implementation for it, in the shape of the existing genericity guards |

**This is a spec reading and it is recorded as one**, not filed as a deviation:
§2 already tells us the class name is instance policy, so a core mechanism
keyed on the name would be unimplementable. Keying on the *declared bound* is
the only reading available, and it produces exactly the reference instance's
behavior for exactly its configuration.

## 6.6 Dormancy, and the §9 item 10 proof

D3: with `reap_acks` empty, A12's predicate is one indexed lookup on a
partial index returning no row, short-circuiting before any class arithmetic.

engine-spec §9 item 10's second half — *"a reaped write-class step cannot gain
a successor until the reap is acknowledged"* — is proven by
`TestReapedWriterGainsNoSuccessor` and by QA section `ZJ`:

| # | Step | Assertion |
|---|---|---|
| W1 | a workflow with `[limits] write = { max = 1 }`, two write-class steps in sequence | baseline: the second is ready once the first is `done` |
| W2 | claim the first; let its lease lapse; `next --run` | it is reaped; a `lease-reaped` event exists; a `reap_acks` row exists, unacknowledged |
| W3 | `next --run` | **the reaped step itself is offered** (it returned to the pool) but **no other write-class step is** — the hold occupies the one slot |
| W4 | `guard spawn --run RUN-N` | **denied**, exit 2, reason naming the seq and `--ack-reap` |
| W5 | read-class steps in the same run | **still offered** (A6) |
| W6 | `guard spawn --run RUN-N --ack-reap <seq>` | allowed; the row is acknowledged; `acked_by = guard-spawn` |
| W7 | `next --run` | write-class steps flow again |
| W8 | a second ack of the same seq | success, changes nothing (A10) |
| W9 | an ack of a bogus seq | `VALIDATION_ERROR` (A9) |

**With §9 item 10's gate half proven in full at S4 (gates-trust §9.2), item 10
is COMPLETE at this stage.** The stage-review must check both halves, since
neither TDD proves item 10 alone.

## 6.7 Test plan — group 2

**Go unit tests** (`internal/engine/dispatch_test.go`, `reap_ack_test.go`):

- The manifest: P1's identity with `next` (same snapshot, same rows, asserted
  by computing both and comparing bytes); P3's canonical bytes; P4's limit.
- C1: two concurrent `dispatch open` calls; exactly one manifest, one
  `CONFLICT`.
- `verify`: equal, unequal-at-position-k, and `TestVerifyDoesNotReap` (P11).
- The TTL: P13's auto-abandon inside `next`; C3's two-invocation race; P16's
  same-invocation answer.
- `close`: P18's refusal per discrepancy class; P19's acceptance recording;
  P20's non-acceptance of D1; C2's close-vs-abandon race.
- `abandon`: P21's unconditionality with a discrepancy present.
- The refusal: P24/P25/P26's `CONFLICT` (not an empty list); P27's issue-mode
  untouched; **P28's claim-still-works**, which is the recovery argument as a
  test.
- Discrepancies: D1 and D2's definitions at their boundaries (one ms inside and
  outside grace); D3's historical-run exclusion; D5's action/human exemption.
- Reap-ack: the W1–W9 table; A12's predicate over a `max = 2` class (A15's
  fold); A18/A21's dormancy with no `[limits]`; `TestNoWriteClassLiteral`.

**QA** — new section **`ZJ`** (`scripts/qa/test_zj_dispatch.sh`), §9.2.

---

# Group 3 — the events read surface, `guard record|spawn`, auto-registration, and the rehearsal

Commit group 3. Independently stoppable: three read/predicate verbs and one
activation extension, each dormant by construction.

## 7. `guard record | spawn`, exact predicates

engine-spec §2, verbatim: *"`spawn` — proposed rows byte-match the open
dispatch and no unacknowledged write reaps; `record` — no unreconciled
dispatch."*

Both follow `internal/engine/guard.go`'s established §6.12 contract: **exit 0
allow / exit 2 deny with a reason**, independent of the error taxonomy, reason
to stderr in human mode and into the envelope's `error` under `--json`.

## 7.1 `guard record`

| # | Clause |
|---|---|
| G1 | `docket guard record --run RUN-N` allows iff **no unreconciled dispatch exists** for the run |
| G2 | "Unreconciled" = an open dispatch, **or** any discrepancy (§5.8) — the same two probes `next` uses (P24/P25), computed by the same function, so the guard and the scheduler cannot disagree |
| G3 | Denial names which of the two, and the resolution, in the same words `next`'s refusal uses |
| G4 | `--run` is **optional**. Without it the guard answers over every non-terminal run, denying if any is unreconciled — matching `guard stop`'s existing all-active-runs shape, so a hook wired once keeps working as runs come and go |

**Why `record` is the verb a harness wires before letting a worker call
`step complete`:** an unreconciled batch means the engine's picture of what is
running is already wrong. Recording an artifact into that picture is how drift
becomes durable.

## 7.2 `guard spawn`

| # | Clause |
|---|---|
| G5 | `docket guard spawn --run RUN-N [--rows FILE] [--ack-reap SEQ]…` allows iff **both**: (a) the proposed rows byte-match the open dispatch, and (b) no unacknowledged write reaps exist |
| G6 | Proposed rows arrive via `--rows FILE` (or `-` for stdin) as the JSON array a relay is about to spawn. Byte-matching is `dispatch verify`'s comparison (P7) against the *stored manifest*, position by position |
| G7 | **With no open dispatch and no `--rows`, (a) is vacuously satisfied.** A harness that does not use dispatch manifests still gets (b) — the reap check — which is the half §2 assigns to this verb by name. Requiring a manifest would make the reap-ack mechanism unavailable to any relay that batches differently |
| G8 | **With `--rows` and no open dispatch, it is a denial**, not a vacuous pass: the relay believes it is spawning a batch the engine never issued |
| G9 | Denial for (b) enumerates each unacknowledged seq and names `--ack-reap` (A11) |
| G10 | `--ack-reap` is processed **before** the predicate (§6.2), so one invocation both acknowledges and answers — which is what lets a relay's hook be a single command |

**C11's residual, stated:** between `guard spawn`'s allow and the relay's
actual spawn, the dispatch could be abandoned or a lease reaped. The guard is
an early check, not a lock, and the real enforcement remains where it has
always been — `step claim`'s CAS. This is the same honest bound `guard gate`
carries at S3 and it is recorded rather than papered over.

## 7.3 Both guards write nothing — except the ack

| # | Clause |
|---|---|
| G11 | `guard record` writes nothing, ever |
| G12 | `guard spawn` writes **only** the `reap_acks` update, and only when `--ack-reap` is passed. Without the flag it is a pure read |
| G13 | Neither guard reaps, and neither auto-abandons a dispatch. A guard that mutated scheduling state would make a hook's mere presence change a run's behavior |

## 8. `events list --since`: the §11.4 event shape as a read surface

engine-spec §1's surface line is `docket events --follow [--since SEQ]`, and
§10 splits it: `--since` here, `--follow` at S7. This stage therefore ships
**`docket events list [--since SEQ]`**, and §8.1 explains the sub-verb.

### 8.1 Why `events list` and not bare `docket events`

§1's summary writes the verb as `docket events --follow [--since SEQ]`, which
reads as a bare verb with flags. Shipping the read half as bare `docket events`
would mean S7's `--follow` changes the *default* behavior of an existing verb —
or worse, that `docket events` with no flags means "list" now and something
else later.

**`events list` is chosen**, with `docket events` alone printing help. S7 adds
`events --follow` as specified, over the same rows. This is a **nominal
deviation from §1's rendering, filed as DKT-30** (§13) — the same class as
DKT-14's `step`/`issue` subject-key note, and resolved the same way: the shape
is satisfied, the spelling is recorded.

### 8.2 The event shape, §11.4 verbatim

```
event  { seq, at_ms, kind, run?, step?, data }
```

| # | Clause |
|---|---|
| E1 | Implemented field for field. `run` is `RUN-N`, `step` is the **rendered instance identity** (`name@k#i`) — matching every other wire shape's step rendering (§11.4's note on `instance`) — and both are **omitted when NULL**, per the `?` |
| E2 | `data` is the stored JSON object, **verbatim**. Core never reshapes it; §7.6's writer already normalizes it to an object on the way in |
| E3 | `at_ms` is epoch-milliseconds, `seq` the monotonic `AUTOINCREMENT` |
| E4 | The step's **id** (`STEP-N`) also rides, as `step_id`, because `data.instance` and `step` both give the human identity and a consumer joining to `step show` needs the id. **This is an addition to §11.4 and is filed — DKT-31** (§13), with the argument that the shape is otherwise unjoinable |

### 8.3 Cursor semantics

| # | Clause |
|---|---|
| E5 | `--since SEQ` returns events with `seq > SEQ` — **strictly greater**, so a consumer stores the last seq it saw and passes it back without re-reading it |
| E6 | `--since 0` (the default) returns from the beginning |
| E7 | Ordering is `seq ASC`, always. There is no reverse mode: a cursor feed that could run backwards is a cursor feed that skips |
| E8 | `--run RUN-N` filters to a run, using `idx_events_run_seq`; without it the feed is repo-wide (trust events have no run — §3.6 of gates-trust — and are only visible in the unfiltered feed) |
| E9 | `--limit` applies **after** ordering, with the v2 truncation contract: `total` counts matching events before slicing, `truncated` is set. Default 100 |
| E10 | C9: the read is one `SELECT` in one transaction. A concurrent insert lands above the cursor and is returned next call. **No event is ever skipped and none is returned twice** — `TestCursorNeverSkipsUnderConcurrentInsert` runs inserts against a looping reader and asserts the union is exactly the inserted set |

### 8.4 The Collection envelope

| # | Clause |
|---|---|
| E11 | `events list` implements the same `Collection` interface every list verb does, so `--json=v2` yields `{items, total, truncated}` uniformly |
| E12 | v1 output is the bare array, matching every other list verb's v1 shape |
| E13 | Human mode renders one line per event: `seq  at  kind  run  issue  instance  detail`, columns aligned, `detail` a compact rendering of `data`. (Amended: `f1cef61` added the `issue` column between `run` and `instance`; the row previously read `seq  at  kind  run  instance  detail`.) |

### 8.5 Rendering untrusted bytes

Per §1.3: human mode renders `data` through gates-trust §5.7's escaping
renderer (T18), losslessly; `--json` carries raw bytes (E4 of that section).
`data` contains `--usage` blobs and gate names — attacker-influenced strings
going to a terminal.

### 8.6 `GONE` — shape here, trigger at S7

engine-spec §3, verbatim: *"`events --follow --since` below the retained
minimum returns GONE rather than silently skipping."*

| # | Clause |
|---|---|
| E14 | `GONE` is a **new error-taxonomy code**, exit **6**, meaning "the cursor is below the retained minimum; the events you asked for no longer exist" |
| E15 | It is raised when `--since SEQ` names a seq below `MIN(seq)` in `events` **and** the table is non-empty **and** a prune has occurred. §8.6.1 defines that last condition |
| E16 | **The trigger cannot fire at this stage.** Nothing prunes: `events prune` is S7's (§10 stage 7), and no other path deletes an event. So `MIN(seq)` is always 1 in a v10 repo and E15's condition is unreachable |
| E17 | The **shape is specified and implemented here** — the code, the exit, the message, the taxonomy row, the SKILL.md entry — because `--since` is the verb that must return it, and a stage that shipped the cursor without its out-of-range answer would ship a verb whose contract S7 has to retrofit |
| E18 | `TestGoneShapeIsReachableOnlyByPruning` constructs the state **by deleting rows directly in the test**, asserts `GONE` with its exit code and message, and documents that no product code path reaches it yet |

**§8.6.1 — how "a prune has occurred" is known without a pruner.** The
condition is `MIN(seq) > 1`, which in a v10 repo is only achievable by deleting
row 1. Since `seq` is `AUTOINCREMENT` and never reused (§7.6's comment says so
and explains why), `MIN(seq) > 1` is a sound and cheap proxy for "something
below your cursor is gone", and it stays sound when S7's pruner arrives. No
watermark column is needed, and adding one now would be storing a fact the
table already tells us.

### 8.7 §9 item 2 becomes a QA FACT over a completed run

engine-spec §9 item 2, verbatim: *"Zero model-made scheduling decisions in a
full run: every transition in events traceable to next/gate/threshold/human
input."*

This is the acceptance criterion the events read surface exists to make
checkable, and it stops being an argument and becomes a script.

| # | Clause |
|---|---|
| E19 | The **attribution table** maps every closed-set event kind to exactly one of four **actors**: `next` (the scheduler), `gate` (a deterministic check, incl. actions), `threshold` (computed routing), `human` (an operator verb) |
| E20 | The table lives beside the closed set in `internal/engine/event.go`, and `TestEveryEventKindHasAnActor` asserts one entry per kind — so a stage adding a kind must say who causes it or fail a test |
| E21 | `run report` publishes the per-actor counts (a fifth section, folded into R3's neighborhood) |
| E22 | **QA section `ZI`/`ZK` runs a complete fixture run to `done`, lists every event, and asserts that each kind maps to an actor and that no event's kind is outside the closed set.** That is item 2, executed |

**The attribution table**, complete for the current closed set:

| Actor | Kinds |
|---|---|
| `next` | `step-ready`, `lease-reaped`, `join-completed`, `loop-entered`, `dispatch-abandoned` (TTL), `issue-promoted` |
| `gate` | `gate-started`, `gate-recorded`, `gate-unmatched`, `gate-rerun`, `vote-opened`, `vote-tallied` |
| `threshold` | `step-routed`, `step-failed`, `step-superseded`, `step-skipped`, `step-held` |
| `human` | `run-started`, `run-activated`, `run-paused`, `run-resumed`, `run-abandoned`, `run-done`, `step-claimed`, `step-heartbeat`, `step-recorded`, `step-resolved`, `step-approved`, `step-rejected`, `issue-abandoned`, `trust-added`, `trust-removed`, `dispatch-opened`, `dispatch-closed`, `dispatch-abandoned` (explicit) |

**Two rows need their sentence:**

- **`step-claimed` is `human`, not `next`.** A claim is an *executor's* act —
  `next` offers, something else takes. In the reference instance that something
  is a spawned worker, which is precisely the case item 2 is checking: the
  claim is attributable to the harness's dispatch decision, and the *scheduling*
  decision (which steps were claimable at all) was `next`'s. Calling it `next`
  would hide exactly the boundary item 2 exists to expose.
- **`run-paused` covers the budget breach** (B23), and its actor is `human`
  even when the engine flips it, because the transition's *meaning* is "a
  person must now decide". `data.reason` distinguishes `budget` from an
  operator's `run pause`, so the trail is unambiguous without a new kind.

### 8.8 Event kinds added by this stage

The closed set gains **three**:

`dispatch-opened`, `dispatch-closed`, `dispatch-abandoned`.

Each is a transition an operator follows and each is required by §9 item 2's
attributability: a manifest appearing, being reconciled, or evaporating are
state changes with consequences for what `next` will answer. They are added to
the constants, to `eventKinds`, and to §8.7's attribution table in the same
commit, per the existing discipline.

**No `budget-breached` kind** (B23), **no `reap-acknowledged` kind.** The
acknowledgment updates a `reap_acks` row and is visible in `run report` and in
`guard spawn`'s answer; adding a kind for it would widen the set for a fact the
existing `lease-reaped` event already anchors. **This is a judgment and it is
recorded as one**, so a reviewer can push back: the argument for adding it
would be A3 (attributability), and the counter is that the ack is not a
*transition of the run* — nothing about the run's state machine moves.

## 9. AUTO-REGISTRATION — §9 item 11's machinery

engine-spec §2's instance-config lifecycle, verbatim:

> Instance files live in the repo at `.docket/config/` (`workflows/`,
> `schemas/`, `contracts/`, `fragments/`, `templates/`, `policy.toml`) —
> git-versioned like any code. … **Activation auto-registers** the config
> directory's current contents (content-hash versioning) — registration is
> never a manual step, and schemas are stable reviewed files, never generated
> per-run.

### 9.1 THE F2 CARRY-FORWARD, honored

docs/tdd/payloads-thresholds.md §4.6, verbatim and binding on this stage:

> **Auto-registration registers `.docket/config/schemas/` in full before it
> registers `.docket/config/workflows/`.** Within each directory the order is
> lexical by filename, for determinism. A workflow whose schema is in the same
> directory tree therefore always registers second.

`RegistrationOrder` in `internal/engine/schema_pins.go` **is that ordering** —
three lines and a sort, landed at S5 against this stage's signature. This stage
supplies **the directory scan that feeds it** and **activates the pending
`TestSchemasRegisterBeforeWorkflows`**.

| # | Clause |
|---|---|
| F1 | The scan collects paths, hands them to `RegistrationOrder`, and registers in the returned order. The ordering is not re-implemented |
| F2 | `TestSchemasRegisterBeforeWorkflows` **loses its `t.Skip`** and gains the behavioral half: an activation over a config directory containing a workflow that names a schema in the same tree **succeeds**, which it could not if the order were lexical-across-everything |
| F3 | The negative twin: the same two files registered manually in the wrong order **fails** with §4.6 (a)'s `VALIDATION_ERROR`. Without it, F2 could pass because the refusal does not exist rather than because the order avoids it |

**F3 is the test that makes F2 mean something**, and it is called out because
an ordering test whose subject cannot fail is a tautology.

### 9.2 What is scanned, and what is done with it

| Directory | Action | Verb reused |
|---|---|---|
| `.docket/config/schemas/*.json` | register as a payload schema | the `schema register` path |
| `.docket/config/workflows/*.toml` | register as a workflow definition | the `workflow register` path |
| `.docket/config/contracts/`, `fragments/`, `templates/`, `policy.toml` | **pin by content hash**, register nothing | the existing file-pin path (`--pin`'s) |

| # | Clause |
|---|---|
| F4 | Only the two registries are *registered*. Everything else under `.docket/config/` is **pinned**, which is exactly what §2 says core does with instance files it does not understand ("arbitrary operator-supplied file pins … which is how the reference instance pins its contracts, fragments, and policy **without core knowing what they are**") |
| F5 | The scan is **non-recursive within each registry directory** and **recursive for the pinned directories**. A schema in a subdirectory of `schemas/` would break the two-group ordering's determinism argument; a fragment three levels deep is just a file to hash |
| F6 | Files whose extension does not match (`schemas/README.md`) are **skipped silently in the registries** and **pinned** like any other config file. A refusal here would make a README a run-blocker |
| F7 | The registration reuses the **existing** register paths — same validation, same immutability contract, same `CONFLICT` on different bytes. There is no "auto" variant with looser rules |
| F8 | It happens **inside activation's fat transaction**, before binding, so a registration failure refuses the whole activation and writes nothing. Registering-then-failing would leave a repo with definitions from a run that never started |

### 9.3 THE QUESTION THE SPEC LEAVES OPEN: changed bytes at an unchanged `name@version`

The spec says activation auto-registers "the config directory's current
contents (content-hash versioning)". It does not say what happens when a file's
bytes changed but its declared `name@version` did not — which is the ordinary
consequence of editing a workflow without bumping its version.

The existing contract says `CONFLICT`: *"different bytes at the same
`name@version` → `CONFLICT` (exit 4), naming both hashes"*. The question is
whether *auto*-registration should inherit it, or auto-bump, or overwrite.

| Option | Consequence |
|---|---|
| **(a) REFUSE with a conversationally-actionable message** — **chosen** | activation fails, naming the file, both hashes, the current version, and the exact edit (`version = N+1`). The session proposes the bump; the human approves; the session edits and re-activates |
| (b) Auto-bump the version | core silently creates `name@N+1` from bytes nobody approved as a new version. Every run that pinned `name@N` is fine, but the *next* run silently binds a definition the operator never named. Version numbers stop meaning "the author decided this is a new version" |
| (c) Overwrite `name@N` in place | destroys immutability outright. A run that pinned `name@N` by hash can no longer reproduce, and engine-spec §9 item 5 (determinism at pins) fails on its face |
| (d) Ignore the change and use the registered bytes | the operator edits a file, activates, and gets the *old* definition with no message. The worst failure mode in the set: silent and confusing |

**(a) is chosen. The argument, against both tenets it is accused of violating:**

**Against the immutability contract** — (a) *is* the immutability contract.
`name@version` is frozen (SKILL.md's own table); auto-registration is a
registration; therefore it refuses. (b) and (c) are the options that break
immutability, one by minting versions nobody chose and one by mutating a frozen
row. The only question was whether auto-registration deserves an exemption, and
the answer is that an exemption is exactly how a pinned run stops reproducing.

**Against the zero-touch tenet (T9)** — this is the real tension and it
deserves the honest answer. T9 says "the human only approves in conversation"
and amendments.md says an amendment adding "manual upkeep for the developer
violates T9 on its face". Does refusing add manual upkeep?

**No, and the distinction is the mechanism, not the outcome.** T9's zero-touch
is about who *does the work*, not about the machine never asking. The
prescribed loop is: the session drafts config, the human approves in
conversation, the session runs the commands. A refusal that says

```
.docket/config/workflows/standard-dev.toml has changed since it was
registered as standard-dev@1.

  registered  sha256:3f9a…   current  sha256:c41b…

A registered name@version is frozen so that a run which pinned it can
reproduce. To adopt these changes, bump the definition's version:

  [pipeline]
  version = 2        # was 1

then activate again. Runs already pinned to standard-dev@1 are unaffected.
```

is a message a session reads, acts on, and brings to the human as "I changed
the workflow; it needs a version bump to 2 — okay?" **That is the zero-touch
loop working**, not a violation of it: the human approved a change, and the
session did the mechanical part. What T9 forbids is the human hand-editing
config or typing `docket workflow register` — neither of which this asks for.

**Core never auto-bumps** because the bump is a *decision* ("this is a new
version of the definition"), and §2's own division puts decisions with the
human and mechanics with the session. A core that bumped would be making the
decision and telling nobody.

| # | Clause |
|---|---|
| F9 | Changed bytes at an unchanged `name@version` during auto-registration is `CONFLICT` (exit 4), refusing the whole activation |
| F10 | The message names the **path**, **both hashes**, the **registered version**, and the **literal edit** (`version = N+1`), and states that existing pinned runs are unaffected |
| F11 | Identical bytes at the same `name@version` is a **success that changes nothing** — the existing contract, and the case that makes re-activation of an unedited repo free (C10) |
| F12 | Core **never** auto-bumps, never overwrites, and never silently uses the registered bytes |
| F13 | The refusal is a **hard** one, not a warning, for the same reason (d) is the worst option: a warning during a fat transaction that then proceeds is a warning nobody reads until the run behaves oddly |

**Filed as a spec note, DKT-32** (§13): §2's auto-registration sentence should
state the collision behavior, since it is the one question every implementer
will hit and the spec's silence made this TDD decide it.

### 9.4 Idempotency, and the concurrency case

| # | Clause |
|---|---|
| F14 | C10: two activations scanning the same directory both register identical bytes; both succeed (F11); the second changes nothing |
| F15 | Re-activation of an **already-active** run **inherits the original pin set** (RA2, engine-spine §5.4) and therefore **does not re-scan**. A config file edited mid-run does not reach a run already under way — F9's refusal cannot fire on re-activation, and a mid-run edit is simply invisible, exactly as a re-registered workflow is |
| F16 | The scan runs **once per activation**, not once per issue |

**F15 is important and easy to get wrong**: if re-activation re-scanned, an
operator editing a workflow while a run expanded its second phase would hit
F9's refusal on a run that was working fine, and the fix would be to revert
their edit. Inheriting the pin set makes the edit a non-event.

### 9.5 The trust posture is unchanged

Auto-registration **reads and hashes**; it executes nothing (§1.3). A workflow
found in `.docket/config/workflows/` that declares gates still requires trust
entries for those gates, and its fenced commands are still harvested,
snapshotted, and matched against the user-level allowlist (engine-spec §4). A
malicious clone that ships `.docket/config/workflows/evil.toml` gets its
definition *registered* and then gets every one of its gates reported
`unmatched` and **not executed**.

`ZH10`'s malicious-clone check (gates-trust §9.4) is **extended** to carry a
`.docket/config/` directory, so the clone's auto-registered workflow is proven
inert. Without that extension, this stage would widen the attack surface and
the existing test would not notice.

### 9.6 The dormancy proof

| # | Clause |
|---|---|
| F17 | Activation `stat`s `.docket/config/` **once**. Absent ⇒ the scan is skipped in full and activation executes exactly the statements v9's did |
| F18 | Present but empty ⇒ the scan finds nothing and registers nothing |
| F19 | The QA proof re-runs ZG's activation with the directory absent and byte-diffs the output against the pre-group baseline |

### 9.7 Activation's §7.7 report extends

gates-trust §7.7 established the activation report: every harvested fenced
command, verbatim, annotated `matched`/`unmatched`. This stage adds **what was
auto-registered**, above the fences:

| # | Clause |
|---|---|
| F20 | Human mode prints a `Registered` block: one line per file, `<name>@<version>  <path>  (new \| unchanged)` — schemas first, then workflows, in registration order, then a `Pinned` count |
| F21 | Under `--json`, a `registered` array of `{kind, name, version, path, sha256, outcome}` where `kind ∈ {schema, workflow}` and `outcome ∈ {new, unchanged}`, alongside the existing `fences` array |
| F22 | `--dry-run` prints the same report and **writes nothing**, so an operator sees what activation *would* register before committing — the same affordance §7.7 S4 gives the fences, extended to the thing this stage adds |
| F23 | An activation that registered nothing prints no `Registered` block (not an empty one), so F17's dormancy is visible in the output |

**F22 makes the zero-touch loop reviewable**: the session runs
`run activate --dry-run --json`, shows the human "this will register
standard-dev@1 and findings@1 and run these three fenced commands", the human
says yes, the session activates. That is §2's *"At plan approval the session
surfaces what activation will bind"* — now covering registration as well as
fences.

## 9.8 §9 item 11 AS QA: the zero-touch rehearsal

engine-spec §9 item 11, verbatim: *"Zero-touch: from `docket workflow init
--template …` through a completed run, the only human inputs are conversational
approvals relayed by the harness — no hand-edited config, no manual
registration."*

**QA section `ZK`** (`scripts/qa/test_zk_zerotouch.sh`) executes it literally.

| # | Step | Command | Assertion |
|---|---|---|---|
| Z1 | scaffold | `docket workflow init --template standard-dev` | writes `.docket/config/workflows/standard-dev.toml`; **nothing is registered** |
| Z2 | the work | `docket issue create` with a body containing a ```` ```checks ```` fence | the issue exists; the fence is not harvested yet |
| Z3 | **one** trust grant | `docket trust add checks --yes -- <argv>` | the single human-authorized act, matching §4's conversational posture |
| Z4 | start | `docket run start --issue DKT-N` | run in `planning` |
| Z5 | **activate** | `docket run activate RUN-N` | **auto-registers `standard-dev@1`**, reports it (F20), harvests the fence, binds, expands, promotes |
| Z6 | execute | `next` → `step claim` → `step complete` | the check step runs its fenced gate; the gate is `matched` and passes |
| Z7 | approve | `docket step approve` | the `type="human"` step resolves |
| Z8 | done | `next` until empty; `run status` | the run reaches `done` |
| Z9 | **THE ASSERTION** | — | **`docket workflow register` and `docket schema register` were never invoked**, asserted by a `set -x`-style command trace over the whole section, grepped for the two verbs. Zero hits |
| Z10 | and | — | **no config file was edited after `workflow init` wrote it**, asserted by hashing the file at Z1 and again at Z8 |
| Z11 | and | — | the only human-shaped inputs across the section are Z3's `trust add --yes`, Z7's `approve`, and the ordinary issue/run verbs a harness relays. Enumerated in the section's own comment and asserted by the same trace |

**Z9 is the criterion.** A rehearsal that merely reaches `done` proves the
engine works; a rehearsal that reaches `done` *with the register verbs proven
absent from the trace* proves item 11. The trace-grep is the mechanism because
it cannot pass by accident — if the implementation quietly needed a manual
register, the grep finds it.

**A schema variant** (`Z12`) repeats the rehearsal with a template that
declares a `payload`, dropping both a schema and a workflow into
`.docket/config/`, and asserts the F2 ordering holds end-to-end: activation
succeeds, which it could not if the workflow registered first.

## 10. Cross-cutting test obligations

### 10.1 Where tests live

Unchanged from the established layout: Go unit tests beside the code
(`internal/engine/*_test.go`, `internal/db/*_test.go`, `internal/cli/*_test.go`),
QA sections as `scripts/qa/test_z*.sh` sourced by `scripts/qa.sh`.

### 10.2 New QA sections

| Section | File | Subject |
|---|---|---|
| `ZI` | `test_zi_budget.sh` | budgets, the floor, `run report`, **§9 item 7** |
| `ZJ` | `test_zj_dispatch.sh` | dispatch, `next` refusal, the write-reap ack, **§9 items 9 and 10** |
| `ZK` | `test_zk_zerotouch.sh` | events read, guards, auto-registration, **§9 items 2 and 11** |

They append to `SECTIONS` in `scripts/qa.sh` after `ZH`, preserving the
shared-DB sequential contract, and each begins with `assert_trust_sandbox`
where it touches trust (gates-trust §9.5 SB5).

### 10.3 Standing assertions this stage adds

| Test | Asserts |
|---|---|
| `TestReadVerbsWriteNothing` | `run report`, `events list`, `dispatch verify`, `guard record`, and `guard spawn` (without `--ack-reap`) leave the database byte-identical |
| `TestEveryEventKindHasAnActor` | §8.7's attribution table has one entry per closed-set kind |
| `TestSchemaSpanIsComplete` | §2.4 |
| `TestRewindGuardProbesEveryV10Sentinel` | §2.2 |
| `TestNoWriteClassLiteral` | §6.5 A22 |
| `TestMetadataRollupReadsNoKey` | §4.10 R7 |
| `TestFloorIsNeverReadFromCache` | §4.3 |
| `TestBudgetR7AndClaimAgree` | §4.8 |
| `TestVerifyDoesNotReap` | §5.3 P11 |
| `TestCursorNeverSkipsUnderConcurrentInsert` | §8.3 E10 |
| `TestGoneShapeIsReachableOnlyByPruning` | §8.6 E18 |

### 10.4 The byte-compat sweep and the §9 item 8 fixture

Re-run per group, unchanged in shape from S3–S5: the v4 fixture migrates 4→10
in one pass, the v10 structures are asserted present, and every pre-existing
verb's output is golden-diffed. §3's per-group dormancy table names what each
group additionally proves.

## 11. Implementation phases — THREE commit groups

Per the 09 slicing. Each is green, each is independently stoppable, each
carries its own dormancy proof.

### Group 1 — budgets, the floor, `run report`

Lands: v10 stamp + `usage_ledger`; `--usage` → ledger rows; the floor query;
R7's real check; B14's claim-time enforcement; the breach transition;
`run report`; `budget.unit` config key.

**Dormancy proof:** budget default 0 = unlimited ⇒ `budgetHeadroom` executes no
query and `next` is byte-identical.

**Stoppable because:** a repo gains a read-only report and an enforcement that
does not fire. Nothing else depends on it.

### Group 2 — dispatch, `next`'s refusal, the write-reap ack

Lands: `dispatches`/`dispatch_rows`/`reap_acks`; `dispatch open|close|verify|abandon`;
the TTL auto-abandon; `next`'s refusal and the discrepancy probes; the reap-ack
row, hold predicate, and `--ack-reap`.

**Dormancy proof:** a run with no dispatch ever opened and no reaped
finite-`max` class behaves exactly as group 1's.

**Stoppable because:** the guards that surface the ack are group 3's, and the
ack's other entry point (`dispatch open --ack-reap`) ships here — so the
mechanism is complete and usable without group 3.

### Group 3 — events read, `guard record|spawn`, auto-registration, the rehearsal

Lands: `events list --since` + `GONE`'s shape + the attribution table;
`guard record|spawn`; the `.docket/config/` scan + F9's refusal + the extended
activation report; QA sections `ZI`/`ZJ`/`ZK` completed.

**Dormancy proof:** `events list` is a new verb; both guards are new verbs; a
repo with no `.docket/config/` activates exactly as before (F17/F19).

**Stoppable because:** it is the last group, and every piece in it is additive.

**Group ordering rationale:** budgets first because the floor is the only
mechanism nothing else depends on; dispatch second because the reap-ack needs
its tables; the read surface last because §9 item 2's audit needs a complete
run to audit, which requires the first two groups.

## 12. Documentation, scheduled per group

| Group | Document | Rows |
|---|---|---|
| 1 | `skills/docket/SKILL.md` | `run start --budget` loses "not yet enforced"; **new** `docket run report` section with the R1–R7 table; `docket config` gains `budget.unit`; `step complete --usage` loses "enforces nothing"; the `next row` table's `expected_cost` row loses "enforces nothing yet" |
| 1 | **`docs/spec/performance.md`** (**new**) | the floor query's cost, the report's query count, the measured numbers of §12.2, and the stated fallback |
| 1 | `docs/spec/architecture.md` | the usage ledger and the budget mechanism, as implemented |
| 2 | `skills/docket/SKILL.md` | **new** `docket dispatch` section (four verbs, flag tables, the discrepancy table, the refusal table); `docket next` gains its refusal rows; the error table gains dispatch `CONFLICT`s |
| 2 | **`docs/spec/operations.md`** (**new**) | backup (`sqlite3 .backup`), the WAL-on-synced-directory warning, retention posture, dispatch recovery runbook, and the B25 budget-raise workaround |
| 2 | `docs/spec/architecture.md` | dispatch tables and the reap-ack ledger |
| 3 | `skills/docket/SKILL.md` | **new** `docket events` section (`list --since`, the cursor contract, `GONE`); `docket guard` table gains `record` and `spawn` rows; `docket run activate` gains the auto-registration paragraph and the `registered` array; the error table gains `GONE` (exit 9 — appended; exit 6 is STALE_LEASE since S1, amended per DKT-33) |
| 3 | `docs/spec/operations.md` | the retention/prune posture note pointing at S7 |
| 3 | `docs/spec/security.md` | §1.3's three additions: `--usage` caps, event-data rendering, auto-registration reads-not-executes |

**`operations.md` and `performance.md` are this stage's named new files.**
engine-spec §3 requires "documented backup (`sqlite3 .backup`), WAL-on-synced-
directory warning in docs", and no document has carried them; this stage
creates the home. Both follow the `architecture.md`/`security.md` living-document
convention: sections appear only when the code behind them exists.

**SKILL.md's flag and verb tables are updated in the SAME commit as each
surface change** (CLAUDE.md: "a stale table is drift and blocks review"). Group
2's commit that adds `dispatch open` adds its flag table; it is not deferred to
group 3.

### 12.2 The performance rows this stage must measure

`performance.md` is created with real numbers, not placeholders:

| Measurement | Why it matters |
|---|---|
| The floor query at 10 / 100 / 1000 claim events | §4.3's cost argument; it runs once per claim |
| `run report` total query count and wall time on a 50-step run | R8's read-only rollup is the verb an operator polls |
| `events list --since` over 10k events with and without `--run` | E8's index choice |
| `next`'s added cost with dispatch and reap-ack probes, vs. group 1's | §3's dormancy claim, measured rather than asserted |

The stated fallback if the floor query ever becomes unacceptable: a cache
**checked against** the query (the query still runs, the cache is asserted to
match, a mismatch is a hard error), never a cache replacing it. Recording the
fallback now keeps a future optimization from quietly becoming C4.

## 13. Amendment issues filed by this stage

Per docs/design/amendments.md, each cites the exact line and is filed rather
than changed silently.

| # | Subject | Spec line | Proposal |
|---|---|---|---|
| **DKT-29** | No verb raises a run's budget mid-run | §1 surface summary lists `run start … pause\|resume\|abandon; status; report` — no budget mutator; §2's breach parks the run at a cap only `run start` set | Add `docket run budget RUN-N --set N` (CAS on `row_version`, event-logged), or state that a breached run must be abandoned and restarted. **The TDD implements neither** and documents the `sqlite3` workaround in operations.md |
| **DKT-30** | `events list` vs. bare `docket events` | §1: `docket events --follow [--since SEQ]` | Nominal: the read surface ships as `events list --since` so S7's `--follow` does not redefine an existing verb's default. Same class as DKT-14 |
| **DKT-31** | The `event` wire shape has no joinable step id | §11.4: `event { seq, at_ms, kind, run?, step?, data }` — `step` is the human instance identity | Add `step_id?` (`STEP-N`) so a consumer can join to `step show`. The TDD ships it (E4) |
| **DKT-32** | Auto-registration's collision behavior is unspecified | §2 Instance config lifecycle: *"Activation auto-registers the config directory's current contents (content-hash versioning)"* | State that changed bytes at an unchanged `name@version` is a `CONFLICT` refusing activation, with a version-bump message; core never auto-bumps. The TDD implements this (§9.3) |

**DKT-21's precedent applies to DKT-31 and DKT-32**: the TDD ships the behavior
and files the note, because both are cases where the spec's silence would
otherwise be resolved silently by whoever implements first.

## 14. Deliberate non-goals

- **No budget forecasting or projection.** The report publishes burn rate; it
  does not predict when a run will breach. That is a policy computation over a
  published number.
- **No automatic dispatch reconciliation.** `next` refuses; it never guesses a
  resolution. §2's enumerated resolutions are enumerated precisely so that
  reconciliation is an operator's act.
- **No cross-run budget.** The cap is per-run (§11.3). A repo-wide budget would
  need an owner and a reset policy, neither of which the spec defines.
- **No event compaction, retention enforcement, or `--follow`.** S7's, all
  three.
- **No `policy.toml` interpretation.** It is pinned as bytes (F4) and never
  parsed. engine-core §7: "The instance's `policy.toml` carries only
  dispatcher-side spawn routing — the engine never reads it."
