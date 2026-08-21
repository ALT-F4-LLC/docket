# TDD: events `--follow` + prune, and the run budget verb (stage 7)

Status: draft — 2026-08-04 · implements docs/design/engine-spec.md §3 (the
retention, prune, and GONE clauses, verbatim), §1's `events` line as amended
(DKT-32) and its `run budget` line (DKT-29), and §11.4's `event` shape as the
thing `--follow` streams. Acceptance bar: **the observability tail** — no §9
item is first proven here; §9 item 2's audit gains the retention caveat that
makes it honest. Tracker units: **DKT-7** (this stage) and **DKT-29** (the
budget verb, which closes with it). Spec of record is engine-spec.md;
deviations are DKT amendment issues per docs/design/amendments.md citing the
exact line, never silent changes.

**This is the stage where the engine learns to FORGET, and to be watched while
it works.** Both halves are small, and both are dangerous in the same way: a
subscription that skips an event and a prune that removes one an operator
needed are the same failure — the log stops being the run's complete account of
itself. Every clause below is written against that.

**It is also the stage that closes B24.** S6 recorded, in a comment in
`budget.go` and in a runbook section in operations.md, that a breached run
cannot be un-wedged without editing the database: `run resume` returns it to
`active` and the cap has not moved, so the next claim breaches again.
`run budget --set` is the missing verb, DKT-29 is the issue that reserved it,
and this stage ships it.

## 1. Scope

engine-spec.md §10 stage 7, verbatim:

> 7. **Events `--follow` + prune** (observability tail).

and §3's two sentences that define what prune must do:

> `events prune` refuses events of non-terminal runs and never crosses the
> artifact-retention boundary; `events --follow --since` below the retained
> minimum returns GONE rather than silently skipping.

and §1's budget line, as amended:

> `docket run budget RUN-N --set N`  # raise/lower a live cap (CAS,
> event-logged — DKT-29, stage 7)

Three deliverables, then: `events --follow`, `events prune`, `run budget --set`.

### 1.1 Genericity check (CLAUDE.md PR bar, docs/design/genericity.md)

The three verbs carry no agent/LLM vocabulary and none is tempting here, but
two phrasings are worth refusing in advance because they are the ones that would
arrive naturally:

| Tempting | Why it fails | What ships |
|---|---|---|
| "follow the agent's progress" / "tail the worker" | An `executor` is an opaque hint; core does not know a worker is a worker | `--follow` streams **events**, and the help says so |
| "prune old runs' conversations" / "compact the transcript" | A `data` payload is an opaque KV bag, not a transcript | `events prune` deletes **events**, named by seq or by run |
| "raise the token budget" | The cap is a bare number naming no unit (§4.9 B21's rule, already held) | `run budget --set N` — "cap", "spend", no denomination |

**The stranger test, applied.** A print shop bounding its press at one operator
(the fixture ZJ already uses) follows its own event feed to watch a job move,
prunes the events of jobs it finished last quarter, and raises a job's cap when
the estimate was low. Nothing in that sentence mentions a model.

### 1.2 CONCURRENCY INVENTORY

Four races, and where each is closed.

| # | Race | Closed by |
|---|---|---|
| F1 | `--follow`'s cursor advancing past an event written between the read and the next poll | Structurally: the cursor advances to the **last seq actually returned**, never to a clock or to `MAX(seq)`. An event inserted mid-poll lands above the cursor and arrives next cycle — the same C9 argument S6's `ListEvents` already rests on, and `--follow` adds no new query |
| F2 | A prune running while a `--follow` is polling | The follower's next poll returns `GONE` if its cursor fell below the new minimum, which is the correct answer and the reason `GONE` exists. `--follow` **exits** on `GONE` rather than resetting its cursor: silently resuming at the new minimum is exactly the silent skip §3 forbids |
| F3 | Two prunes racing | Each is one `DELETE` in one transaction; both are idempotent (deleting an absent row is a no-op) and SQLite serializes them. The refusal predicates are evaluated **inside the same transaction** as the delete, so a run that reaches `done` between the check and the delete does not widen what was deleted |
| F4 | `run budget --set` racing a claim that is breaching | CAS on `runs.row_version` (B-CAS below). The claim's breach flip is also a CAS on `(run_id, status='active')`, so one of the two wins and the other sees the row it did not expect. Neither can produce a run whose cap and status disagree |

### 1.3 Security note

Short, because this stage executes nothing.

- `events prune` **deletes rows**. It is the first destructive verb in the
  engine surface, and every refusal below exists because of that. It never
  deletes an artifact, a step, a run, or a usage row — only `events`.
- `--follow` renders `data` to a terminal on every cycle, so it goes through the
  same `exec.Render` escaping `events list` already applies (§8.5 of
  runs-dispatch). A follower left running for a day is a longer exposure window
  for the same attacker-influenced bytes, not a new one.
- `run budget --set` takes a number. The `--reason` it records is operator text
  that lands in an event and is rendered by the feed, so it is escaped on the
  way out like every other stored string.

## 2. Schema version span: NONE

**v10 is the end of the span. This stage adds no DDL — no table, no column, no
index.** That is a deliberate constraint the work order set and this section
argues it is met rather than merely obeyed.

Each thing the stage could have wanted a column for, and where it lives instead:

| Wanted | Rejected column | What is used |
|---|---|---|
| "how far back do we retain?" | `meta.retention_*` as DDL | `docket config events.retain` — the config machinery (`meta` KV) already stores engine numbers, and §11.3 puts enforced numbers core-side "never in opaque pins", which config satisfies |
| "what was the last prune?" | `runs.pruned_at_ms` / a `prunes` table | Nothing. The prune is **event-logged**, and the events table is the record. A separate bookkeeping row would be a second source of truth for a fact the log already holds |
| "what is the retained minimum?" | `meta.events_watermark` | `MIN(seq)` over `events` — S6 §8.6.1 already argued this is sound and cheap, and it stays sound now that a pruner exists. Storing it would be storing a fact the table tells us |
| "the budget's previous value" | `runs.budget_prev` | The `run-budget-set` event's `data` carries `from` and `to`. A column would hold one prior value; the log holds every one |

**The prune's bookkeeping argues into config and into events, not into DDL** —
which is what the work order asked for, and it is the same argument S6 made
when it declined a watermark column.

`migrate` is therefore untouched: a v10 DB opens and runs stage 7 with no
migration at all, and `TestSchemaVersionIsUnchangedAtS7` asserts
`schemaVersion == 10` so a later stage that reaches for DDL does it deliberately.

## 3. What is dormant

**A repo that never prunes and never follows sees nothing.**

| # | Clause |
|---|---|
| D1 | `events prune` is a verb nobody is obliged to run. Docket still deletes nothing on its own — there is no automatic retention sweep, no prune inside `next`, no compaction at `run done`. The retention *config key* exists and defaults to **0, meaning "retain everything"**, which is the posture operations.md §2 already documents |
| D2 | `--follow` is a flag on a read verb. Without it, `events list` is byte-identical to S6's, which `TestEventsListIsUnchangedByFollow` asserts by diffing the same call before and after |
| D3 | `run budget --set` is a new sub-verb. `run start --budget` is untouched, and a run nobody re-caps has the same `budget` column value it always had |
| D4 | The dormancy sweep runs against **engine-s6** and must show ZERO diffs on every existing verb's output — the standing check each stage has carried |

## 4. `events --follow`: a polling subscription, no daemon

engine-spec §7, verbatim and binding: **"No daemon."** engine-core §10 says the
same. `--follow` is therefore not a subscription in the pub/sub sense — nothing
is registered, nothing is notified, no process outlives the command.

### 4.1 The mechanism: RunWatch, reused

**`internal/watch.RunWatch` is ready-made and is reused rather than rebuilt.**
It already does every non-cursor thing `--follow` needs: a ticker, a fresh
`output.Writer` per cycle backed by a shared buffer, signal-aware cancellation
via the caller's context, TTY clearing, NDJSON pass-through in JSON mode, and a
consecutive-error ceiling. `docket next --watch` wires it in exactly the shape
this verb wants (`internal/cli/next.go`), and this stage copies that wiring.

| # | Clause |
|---|---|
| W1 | `--follow` runs the same `ListEvents` call `events list` runs, once per tick, through `RunWatch` |
| W2 | The poll period is `--interval` — the **existing persistent root flag** (default `2s`, floor `500ms`), not a new one. `--watch` already owns it, and a second `--interval` declared on this command would shadow the global with its own default and bypass its floor: two flags of one name disagreeing about what they mean. `events list` therefore joins `watchEligible` so the flag is visible there, and the floor check in `PersistentPreRunE` widens from `--watch` to *any polling mode* — a minimum enforced on one flag and not the other would make the same number legal or illegal depending on which one turned the loop on |
| W3 | The **cursor advances to the last seq actually returned**, in the CLI's own state between cycles. A cycle returning no events leaves the cursor where it was |
| W4 | `--follow` is **append-only output**: each cycle prints only what is new. It never clears the screen and never re-prints the page, because a feed is a log and not a dashboard. That is a departure from `next --watch`'s TTY behavior and is deliberate — `RunWatch` clears only when `IsTTY && !JSONMode`, so `--follow` passes `IsTTY: false` |
| W5 | `--limit` still applies per cycle. A follower that starts far behind catches up over several cycles rather than in one page, and `total` on each cycle tells it whether more is waiting |
| W6 | Ctrl-C (SIGINT/SIGTERM) exits **0**. Interrupting a follow is how a follow ends; it is not a failure |
| W7 | `--follow` is **read-only**, like the verb it extends. `TestFollowWritesNothing` asserts the database is byte-identical after a follow that saw several cycles |
| W8 | `GONE` mid-follow **terminates the follow with exit 9** (F2). It does not reset the cursor and does not skip |

**Why W4 matters enough to differ from `next --watch`.** `next --watch` shows a
*current state* — the ready set — so replacing the screen is right. `--follow`
shows a *sequence*, and a sequence that erased itself every two seconds could
not be piped, grepped, or read after the fact. The two verbs want opposite
things from the same helper, and the helper already parameterizes it.

### 4.2 What `--follow` does NOT do

| # | Clause |
|---|---|
| W9 | It does not exit when the run reaches a terminal status. A follower may be watching a repo-wide feed with no run at all, and inventing a stop condition would mean core deciding when an operator is done watching. Ctrl-C is the stop condition |
| W10 | It does not block, wait on a lock, or hold a transaction between cycles. Each poll is S6's one-`SELECT`-in-one-transaction read, and the process is idle between them |
| W11 | It is not offered on any other verb. `next --watch` exists; nothing else grows a follow at this stage |

### 4.3 Test plan — `--follow`

| Test | Asserts |
|---|---|
| `TestFollowSeesNewEventsAndStops` | The headline: a follow starts, a writer inserts events, the follow prints them, the context is cancelled, and the command returns 0 having printed **each new event exactly once** |
| `TestFollowCursorNeverRepeatsOrSkips` | Over N cycles against concurrent inserts, the union of what the follow printed is exactly the set inserted — the C9 property, extended across cycles |
| `TestFollowStopsOnGone` | A prune below a follower's cursor makes the next cycle exit 9 rather than resume at the new minimum |
| `TestFollowWritesNothing` | W7, by byte-comparing the DB file |
| `TestEventsListIsUnchangedByFollow` | D2 |

## 5. `events prune`: the refusals are the feature

`docket events prune` deletes events. Everything interesting about it is what it
refuses to delete.

### 5.1 The surface

```
docket events prune --before SEQ [--run RUN-N] [--dry-run] [--yes]
docket events prune --before-run RUN-N   # everything belonging to that run
```

| # | Clause |
|---|---|
| P1 | Exactly one of `--before` / `--before-run` is required. A bare `events prune` deletes nothing and is a VALIDATION_ERROR: a destructive verb with a default target is how a log gets deleted by a typo |
| P2 | `--before SEQ` deletes events with `seq < SEQ` — **strictly less**, so `--before N` and a cursor at `N-1` mean the same boundary as everywhere else in this feed |
| P3 | `--before-run RUN-N` deletes every event of that run, and is the form operations.md §2 already recommends ("trim whole runs that have reached `done`") |
| P4 | `--run RUN-N` narrows `--before` to one run's events |
| P5 | `--dry-run` reports the count it would delete and deletes nothing |
| P6 | Human mode **requires `--yes`** (or a TTY confirmation); `--json` requires `--yes` outright, since a program cannot answer a prompt |
| P7 | The answer is `{pruned, retained_minimum}` — how many rows went, and what the new floor is, so a consumer can set its cursor without a second call |

### 5.2 Refusal 1: non-terminal runs (§3, verbatim)

> `events prune` refuses events of non-terminal runs

| # | Clause |
|---|---|
| P8 | An event belonging to a run whose status is not `done` or `abandoned` is **never deleted**. `model.RunStatus.Terminal()` is the predicate — the same one `run status --active` and re-activation already use, so "terminal" has one definition |
| P9 | The refusal is a **CONFLICT (exit 4) naming the runs**, not a silent skip. A prune that quietly retained half its range would leave an operator believing space was reclaimed and a consumer believing a boundary moved |
| P10 | It is evaluated **inside the delete's transaction** (F3), so the set refused and the set deleted are computed over one snapshot |
| P11 | Events with **no run** — trust grants — are prunable by `--before`, because there is no run whose liveness could forbid it. They are the one class `--before-run` can never reach, and the help says so |

**Why non-terminal is the line.** A live run's events are load-bearing: the
budget floor is a SUM over its `step-claimed` events (§4.3 of runs-dispatch),
and the saga's resume path reads `gate-started` events to decide whether a gate
already ran. Pruning a live run's log would not merely lose an audit trail — it
would change what the engine computes. **`TestPruningALiveRunWouldMoveTheFloor`
makes that concrete**: it constructs a run mid-flight, attempts the prune,
asserts the refusal, and then asserts (by deleting directly, as S6's GONE test
does) that the floor *would* have moved had the refusal not been there. The
refusal is protecting arithmetic, not sentiment.

### 5.3 Refusal 2: the artifact-retention boundary (§3, verbatim)

> and never crosses the artifact-retention boundary

| # | Clause |
|---|---|
| P12 | The boundary is a **config key: `events.retain`** — a duration. Events younger than it are never pruned, whatever `--before` says |
| P13 | Default **`0`, meaning retain everything**, which makes prune a verb that refuses everything until an operator states a policy. That is the dormant posture D1 requires and the one operations.md §2 documents |
| P14 | A `--before` that would cross the boundary is **clamped and reported**, not silently truncated: the answer names how many rows the boundary held back |
| P15 | `--before-run` on a terminal run is **not** clamped by the boundary. A run that is done and whose artifacts an operator is discarding wholesale is the case §3's boundary is not about |

**What "the artifact-retention boundary" means when artifacts have no
retention verb.** §3 lists "artifact GC per run-retention config" as a
lifecycle item alongside prune; no artifact GC ships at this stage, so read
literally there is no boundary to cross. **Rather than treat the clause as
inapplicable, this stage implements the boundary as the retention window
itself** — the single number a future artifact GC would also read — so that
when GC arrives it inherits a boundary already enforced rather than one
retrofitted. That reading is recorded here so a reviewer can push back on it,
and it is the conservative direction: it can only refuse more.

**This is a nominal resolution of a clause the spec left partly open, and it is
FILED as an amendment rather than made silently** — see §8.

### 5.4 What prune deletes, exactly

| # | Clause |
|---|---|
| P16 | Rows in `events`. Nothing else. No artifact, no step, no usage row, no dispatch row |
| P17 | It does **not** VACUUM. Reclaiming file space is an operator's decision made against a backup, and a verb that rewrote the whole database as a side effect of trimming a log would be a surprise with a different risk profile |
| P18 | It writes **one event of its own** — `events-pruned`, with `data` = `{before, run?, deleted, retained_minimum}`. The closed set gains exactly one kind (§6). Its `seq` is above everything it deleted, so the record of the deletion survives the deletion |

### 5.5 GONE goes live

S6 shipped `GONE`'s shape and proved it reachable only by deleting rows in a
test. **This stage makes it reachable through product code**, and that is the
whole change:

| # | Clause |
|---|---|
| G1 | `checkRetainedMinimumTx` is **unchanged**. `MIN(seq) > 1` was chosen at S6 precisely so it would stay sound when a pruner arrived, and it does |
| G2 | `TestGoneShapeIsReachableOnlyByPruning` **flips from shape-only to real**: it stops deleting rows by hand and calls `docket events prune`, then asserts the same code, exit, and message it asserted before. The test name stops being aspirational |
| G3 | `events list --since` below the new minimum is `GONE` (exit 9, DKT-33) |
| G4 | `--follow --since` below it is the same, per W8 |
| G5 | End-to-end: prune, then list below the boundary, then list at the boundary — the second succeeds, proving the message's "resume from seq N" is actionable and not merely well-formed |

### 5.6 Test plan — prune

| Test | Asserts |
|---|---|
| `TestPruneRefusalMatrix` | The matrix: non-terminal run · boundary-crossing `--before` · no target · `--json` without `--yes` · a run that does not exist. Each with its code and its message |
| `TestPruneDeletesOnlyEvents` | P16, by row-counting every other table before and after |
| `TestPruningALiveRunWouldMoveTheFloor` | §5.2's argument, executed |
| `TestPruneIsEventLogged` | P18, including that the new event's seq is above the deleted range |
| `TestGoneShapeIsReachableOnlyByPruning` | G2 — the flip |
| `TestPruneDryRunWritesNothing` | P5 |

## 6. The event kind added by this stage

The closed set gains **exactly one**: `events-pruned`.

It earns its place on the same argument every other kind earns it (§9 item 2:
every transition must be attributable) with one addition specific to it: **the
prune is the one transition that destroys evidence**, so the record that it
happened is the only thing standing between a trimmed log and a log that looks
like it was never written. A prune that left no event would be indistinguishable
from a run that made fewer transitions.

Its actor is **`human`**. Nothing in the engine prunes; a person ran the verb.

The constant, the `eventKinds` membership entry, and the §8.7 attribution row
land in the same commit, per the existing discipline —
`TestEveryEventKindHasAnActor` fails otherwise.

## 7. `run budget --set`: closing B24

### 7.1 The surface

```
docket run budget RUN-N --set N [--reason "…"] [--if-version V]
docket run budget RUN-N            # read the effective cap
```

| # | Clause |
|---|---|
| B-1 | `--set N` writes `runs.budget`. `N >= 0`; **0 means unlimited**, the meaning the flag has carried since S3 |
| B-2 | It **raises or lowers**. Lowering below the current floor is allowed and takes effect at the next claim — the spec says "raise/lower a live cap" and a verb that refused to lower would be a verb that only half exists |
| B-3 | It is refused on a **terminal** run (CONFLICT). A `done` run's cap is history |
| B-4 | Bare `docket run budget RUN-N` **reads**: `{run, budget, source, floor, reported, spend}` — the same numbers `run report` shows, so an operator deciding what to raise it to does not have to read a report to find out |

### 7.2 CAS on `row_version` (B-CAS)

| # | Clause |
|---|---|
| B-5 | The write is `UPDATE runs SET budget = ?, row_version = row_version + 1 WHERE id = ? AND row_version = ?` when `--if-version` is given, and the same without the version predicate otherwise — the pattern every other CAS verb uses (`internal/db/cas.go`) |
| B-6 | `--if-version` mismatch is CONFLICT (exit 4), via `casError`, with the standard "modified by another writer" message |
| B-7 | `row_version` is bumped **whether or not `--if-version` was passed**. operations.md §4's manual-edit runbook made incrementing it a step an operator had to remember; the verb makes it structural, which is most of why the verb is better than the edit |
| B-8 | The read-back returns the **new** row, so `--json` carries the version a caller's next CAS should use |

### 7.3 Event-logged (B-EV)

The write is event-logged. **Which kind** is the only open question, and it is
decided below rather than assumed.

**The kind question, decided.** Three options were live:

1. A new kind, `run-budget-set`.
2. Reuse `run-resumed`, since the common case is raise-then-resume.
3. No event, matching S6's "no `budget-breached` kind" judgment.

**Option 1 ships.** Option 3 fails §9 item 2 outright: the cap is an
*enforcement input*, and a run that breached at 12 and later admitted a claim at
20 would have a trail with nothing explaining the difference — exactly the gap
operations.md §4 warns about when it says the manual edit "bypasses the event
log". Option 2 conflates two facts (the cap moved; the run un-parked) that an
operator will want to distinguish precisely when something has gone wrong.

The counter to option 1 — the one S6 used to refuse `budget-breached` — is that
the set should not widen for a fact an existing kind already anchors. It does
not apply: no existing kind anchors this. `run-paused` says a run stopped;
nothing says a cap moved.

So the closed set gains a **second** kind this stage: `run-budget-set`, with
`data` = `{from, to, reason?}`, actor **`human`**. §6's count is two, not one,
and this paragraph is why.

### 7.4 The breach re-check, and why nothing else is needed

**No re-scan, no un-park sweep, no repair pass.** The budget check already runs
on every claim, in the claim's own transaction (`enforceBudgetTx`), computing
`max(reported, floor)` against `runs.budget` **read fresh from the row**. Raising
the cap therefore un-wedges the run by arithmetic:

| # | Clause |
|---|---|
| B-10 | `run budget --set` does **not** change the run's status. A breached run is `waiting-human` and stays so until `run resume` — the operator decides when work restarts, which is what `waiting-human` means |
| B-11 | After `run budget --set` **and** `run resume`, the next claim reads the new cap, `admits()` returns true, and the claim commits. That is B24's loop closed |
| B-12 | Lowering below the current spend takes effect the same way: the next claim breaches |
| B-13 | Nothing re-writes the breach `reason` on the run row. `run resume` leaves it (SetRunStatus's documented behavior), so the trail shows *why it had stopped* alongside the events showing what was done about it |

`TestBudgetRaiseUnparksABreachedRun` is the end-to-end: drive a run into a
breach, assert the claim refuses, `run budget --set` to a higher cap,
`run resume`, assert the same claim now commits — and assert the events show
`run-paused(reason=budget)`, `run-budget-set(from,to)`, `run-resumed`, in that
order.

### 7.5 What it does not do

| # | Clause |
|---|---|
| B-14 | It does not touch `budget.unit`. Which unit a cap counts is a repo-level config decision, not a per-run one |
| B-15 | It does not move the floor. The floor is a query over claim events (§4.3 of runs-dispatch) and raising a cap cannot un-spend what was spent — operations.md §4's third caveat, now enforced by a verb rather than warned about in prose |
| B-16 | It is not automatic. Nothing raises a cap on breach; that is the decision `waiting-human` exists to hand to a person |

## 8. Amendment issues filed by this stage

| # | Clause | Filed as |
|---|---|---|
| A1 | `events prune`'s flags (`--before`, `--before-run`, `--run`, `--dry-run`, `--yes`) are a spelling §1 does not give — it writes `docket events prune --before …` and stops | **DKT-36** |
| A2 | The artifact-retention boundary is implemented as the `events.retain` window, since no artifact GC exists to bound (§5.3's reading) | **DKT-37** |
| A3 | The closed set gains `events-pruned` and `run-budget-set` | recorded here and in `event.go`; no separate issue — §6/§7.3 are the argument the discipline requires |

DKT-29 **closes** with the budget verb's commit. DKT-7 carries the stage.

**The numbers above are the tracker's, assigned at filing, not reserved by this
document.** This draft reserved DKT-35/36 and both were wrong: DKT-35 had already
gone to an unrelated `step claim` contention issue. That is the same collision
S6's `events_read.go` recorded when its reserved DKT-31 went elsewhere — *"the
tracker is authoritative on numbering, and the TDD's reserved numbers are not."*
The rule earned a second demonstration; file first, then write the number down.

## 9. Documentation

| Target | Change |
|---|---|
| `skills/docket/SKILL.md` | `docket events` gains `--follow` (flag table) and `events prune` (new sub-verb); `docket run` gains `run budget`; the `GONE` row's "no path reaches this yet" note is **deleted** — a path reaches it now |
| `docs/spec/operations.md` §2 | The retention section is completed: the S6 pointer ("stage 7's") is resolved into the actual policy, the prune runbook, and what a trim costs |
| `docs/spec/operations.md` §4 | The manual-edit runbook is replaced by the verb. The SQL stays as a footnote for a repo on an older binary, marked as such |
| `docs/spec/architecture.md` | Retention and the follow loop, one subsection each |

## 10. Implementation order — ONE commit group

The stage is small enough that splitting it would create commits that are green
only because the half they test is absent.

1. `events --follow` (RunWatch wiring, cursor, GONE termination) + tests
2. `events prune` (refusals, config key, the `events-pruned` kind) + tests, and
   the `TestGoneShapeIsReachableOnlyByPruning` flip
3. `run budget --set` (CAS, `run-budget-set`, the un-park proof) + tests
4. QA section `ZL`, extending the ZI/ZJ/ZK trio
5. Docs

Every commit green, on `feature/graph-engine`, per the delivery mode.
