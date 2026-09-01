# TDD: reliability delta (stage 1)

Status: draft — 2026-08-02 · implements docs/design/engine-spec.md §5, with §11.4
(envelope/wire shapes) and §9 item 8 (compatibility) as the acceptance bar.
Tracker unit: DKT-1. Spec of record is engine-spec.md; deviations are DKT amendment
issues per docs/design/amendments.md, never silent changes.

## 1. Scope

engine-spec.md §5, verbatim, is the whole of this stage:

> CAS `--if-version` everywhere + versions in `.data`; uniform envelopes
> (`{items,total,truncated}`) behind `--json=v2`; explicit truncation flags; hard
> VALIDATION_ERROR on silent-drop cases; idempotency keys on create verbs;
> millisecond timestamps + `seq`; error taxonomy extended once for all new verbs
> (NOT_FOUND, VALIDATION_ERROR, CONFLICT, AUTH_ERROR, STALE_LEASE, TIMEOUT,
> UNTRUSTED). Generic, valuable standalone — and it deletes nine wrapper scripts in
> the reference instance's predecessor tooling.

Out of scope, explicitly: everything in §10 stages 2–7. No workflow, run, step,
artifact, gate, trust, or event surface lands here. Schema stops at v5.

**Genericity check (CLAUDE.md PR bar).** Every noun introduced by this stage —
`items`, `total`, `truncated`, `version`, `if-version`, `idempotency-key`, `seq`,
`AUTH_ERROR`, `STALE_LEASE`, `TIMEOUT`, `UNTRUSTED` — is a generic
concurrency/pagination/transport concept. No agent, model, prompt, brief, node,
severity, or review vocabulary appears in the core surface. The stranger test holds:
a team that has never run an LLM wants optimistic concurrency and honest pagination.

## 2. Schema version span

This stage ships **v5**. The engine work spans **v5–v10** overall (one version per
§10 stage 1–6; stage 7's events tail reuses stage 6's tables and adds no version).
Stage-to-version mapping, recorded here so later stages do not re-litigate it:

| Schema | Stage (§10) | Contents |
|---|---|---|
| **v5** | 1 — reliability delta | **this stage**; see §2.1 |
| v6 | 2 — claims/leases + capability tokens | leases, capability tokens |
| v7 | 3 — workflows, steps, `next`, activation/pinning | workflows, steps, runs (minimal), pins |
| v8 | 4 — gates + trust model | gate_results, trust-cache |
| v9 | 5 — payload schemas + ordered enums + action steps | schemas, payloads |
| v10 | 6 — runs, budgets/floor, report; dispatch; events read | dispatches, events, usage ledger |
| **v11** | — workflow retirement (amendment, DKT-21) | `workflows.deprecated_at_ms` |
| **v12** | — projects dimension (amendment, operator request 2026-08-09) | `projects`, `project_id` scoping |
| **v13** | — vote provenance (amendment, DKT-71) | `votes.metadata` |
| **v14** | — per-seat vote spend (amendment, DKT-95) | `vote_usage` |
| **v15** | — artifact revisions (amendment, DKT-70) | `artifacts.supersedes` |
| **v16** | — retry-budget base (amendment, DKT-86/DKT-90) | `steps.attempt_base` |
| **v17** | — vote-usage provenance (amendment, DKT-115) | `vote_usage.source` |
| **v18** | — issue resolution (amendment, DKT-245) | `issues.resolution` |
| **v19** | — measured-usage cap (amendment, DKT-238) | `runs.usage_budget` |
| **v20** | — operator loop grants (amendment, DKT-237) | `run_issues.loop_grants` |
| **v21** | — hollow-assurance marker (amendment, DKT-265) | `gate_results.stub_entry` |
| **v22** | — pause origin (amendment, DKT-305) | `runs.pause_origin` |
| **v23** | — attempt-outcome breakdown (amendment, DKT-490) | `steps.failed_attempts`, `steps.reaped_claims` |

v5's contents, exactly:

1. `version INTEGER NOT NULL DEFAULT 1` CAS columns on the four existing mutable
   entities: `issues`, `docs`, `proposals`, `labels`. Greenfield — no version column
   exists at v4 (verified against this worktree).
2. `idempotency_keys` table (**new** table): `(scope TEXT, key TEXT, entity_id
   INTEGER, created_at_ms INTEGER, seq INTEGER)`, `PRIMARY KEY (scope, key)`.
3. Millisecond timestamps + `seq` **in the new table only**. `created_at_ms INTEGER`
   and a monotonic `seq INTEGER`.

Amendments extending the span follow, oldest first, each under its own `###
AMENDMENT` heading — inserted BELOW this list, so section 2's own material
stays contiguous. (The v11 and v12 amendments originally slotted between the
prose above and this list, each following the one before it, which left the
list reading as the newest amendment's; the next version bump appends after
the last amendment rather than re-creating that drift.)

### AMENDMENT — the span extends to v11 (DKT-21, 2026-08-07)

The v5–v10 span above was ratified when the engine stages were the only source
of schema change, and §2.4's tripwire enforces it: moving past v10 fails
`TestSchemaSpanIsComplete` with this section named, which is how this amendment
came to be written rather than discovered afterwards.

**What changed.** v11 adds ONE nullable column, `workflows.deprecated_at_ms`.
NULL means the version still binds; a timestamp means it has been retired from
binding by an operator.

**Why it needed a version at all.** DKT-21 is a reproduced defect, not an
ergonomic request: a registered workflow NAME binds forever at its highest
version, deleting its TOML does not unregister it, and no verb could stop it.
Retirement has to survive restarts and be auditable, so it is persisted state,
and persisted state on an existing table is a migration. The alternative
considered and rejected was a config-file exclusion list, which would have put
routing truth in two places.

**Why it does not disturb the ratified span.** v11 adds no table and no index,
touches no existing column's format, and back-fills nothing — every pre-v11 row
reads as binding, which is what it was. The never-mutate rule of §2.1 holds
unchanged. Stage 1–6's arithmetic is untouched: v11 is not a stage, it is an
amendment carrying its own issue.

**Guard shape.** v11 is the first version whose only artifact is a COLUMN, so
the rewind guard needed a third probe kind alongside the table and index lists
(`v11ColumnSentinels`). Merging it into either existing list would rewind every
database forever, since `tableExists` and `indexExists` filter `sqlite_master`
by type and can never see a column. This mirrors the reasoning `v10IndexSentinels`
already records for the index half.

### AMENDMENT — the span extends to v12 (operator request, 2026-08-09)

**What changed.** v12 makes tenancy a row. A `projects` table
(`identity TEXT UNIQUE` — the canonical project path `config.Resolve` derives,
`name`, `prefix`, `created_at_ms`) plus a `project_id INTEGER NOT NULL
DEFAULT 1` column on the seven root entities: `issues`, `docs`, `proposals`,
`runs` (probed ALTERs), and `labels`, `workflows`, `schemas` (rebuilds,
because their UNIQUE constraints change to `(project_id, name)` and
`(project_id, name, version)`). Ids are preserved across the rebuilds, so
every child reference (`issue_labels.label_id`, `steps.workflow_id`,
`run_issues.workflow_id`) holds without rewriting.

**Why it needed a version.** The operator's requirement is a single shared
store in `~/.docket` serving every repository, with worktrees of one
repository sharing state. A single-tenant schema in a shared store makes
every list verb machine-wide and every `name@version` registry a collision
surface between projects; partitioning is persisted state on existing tables,
which is a migration.

**What deliberately did NOT change.** Issue identity stays the global DKT-N
rowid sequence: an id names one issue machine-wide, so run records, step
inputs, and cross-project references stay unambiguous. Per-project numbering
was considered and rejected — it would put a display concern inside every
persisted reference. `projects.prefix` is reserved should that decision ever
be revisited. Child tables (comments, relations, activity, steps, events,
artifacts, …) inherit scope through their parent keys and gain no column.

**Backfill.** Every pre-v12 row lands on project 1, seeded UNCLAIMED (empty
identity). The first repository to open the store claims row 1 as itself
(`EnsureProject`), which is exactly right for a legacy store: it has only
ever served one repository, and its history should bind to that repository.

**Execution context rides in the same version.** `runs` gains `exec_root`,
`branch`, `commit_sha`, `hostname` (stamped at `run start`, best-effort) and
`steps` gains `work_root` (`--worktree` at complete/record). Several
worktrees of one project share the store, and a record that cannot say which
checkout it came from cannot be audited; `work_root` is also what the diff
stage reads, so a lazily-resumed saga diffs the tree the work touched rather
than the resumer's cwd — closing engine-spine §6.7.1 D5's recorded hazard.
Empty means "not a checkout" or "pre-v12", never an invented value.

**Guard shape.** v12 ships in one commit group and one transaction, so a
partial v12 is impossible in normal operation; the guard still probes the
`projects` table and every `project_id` column (`v12Sentinels`,
`v12ColumnSentinels`) as insurance against a later split of the group. The
rebuilds run under `PRAGMA foreign_keys=OFF` around the migration's
transaction — the pragma is a no-op inside one — with `foreign_key_check`
before enforcement returns (`migrationsNeedingFKOff`).

### AMENDMENT — the span extends to v13 (DKT-71, 2026-08-11)

**What changed.** v13 adds ONE nullable column, `votes.metadata`. It holds a
JSON object — the casting seat's own opaque KV claim about what produced the
vote — or NULL when nobody supplied one.

**Why it needed a version at all.** DKT-71 is a reproduced gap: `votes` has
no column that can carry which model and effort level cast a vote, and
`internal/cli/vote_cast.go` had no flag to set one even if the column
existed. That is persisted state on an existing table, which is a migration.

**Why `metadata`, not `model`/`effort`.** `scripts/qa/genericity.sh` bans
`model`/`prompt`/`llm`/`agent`/`brief` from core surface — flag names, JSON
keys, column names, and single-line ALTER literals alike — and a
`votes.model` column or a `--model` flag trips it exactly as the DDL
demonstrating that ban already does. `steps.metadata` (§10 stage 3) is the
same shape for the same reason: an opaque KV bag a caller fills with whatever
provenance it wants to assert, which core never reads a key out of. A vote's
`{"model_resolved":"...","effort_resolved":"..."}` rides through the same
door `step complete --metadata` already opened, rather than through a second,
banned one.

**Why it does not disturb the ratified span.** v13 adds no table and no
index, touches no existing column's format, and back-fills nothing — every
pre-v13 vote asserted nothing about what cast it, and NULL is that vote's
true, unenriched state. The never-mutate rule of §2.1 holds unchanged. Stage
1–6's arithmetic is untouched: v13 is not a stage, it is an amendment
carrying its own issue, exactly as v11 and v12 were.

**Guard shape.** v13 adds no table and no index, so it needs the same COLUMN
probe kind v11 and v12 established (`v13ColumnSentinels`) rather than a new
fourth kind: a database stamped 13 by a binary built mid-change carries every
v12 sentinel, and only a column probe can see `votes.metadata` missing. Its
guard RETURNS a failed probe rather than swallowing it, because "the column
is there" and "I could not look" must not be the same answer on the one
guard whose column a later cast depends on.

**Who reads it, and what is deliberately left open.** The namespace is PER
SEAT and reaches a reader through exactly three surfaces: `vote show --json`,
`vote result --json`, and the export manifest. Those are the verbs that emit a
whole vote. `vote list` reports per-proposal counts and emits no vote at all,
so it never carries a bag. Neither `vote show` nor `vote result` renders the
bag in HUMAN mode — those tables show voter, verdict, confidence, relevance,
weight and time — and that is a choice: a self-asserted, unverified,
free-text claim does not belong in the table an operator skims to read a
tribunal's outcome, where it would sit beside facts core does stand behind.
`--json` is where a caller that wants provenance goes. No run-level surface
aggregates it either —
`db.MetadataRollup` reads `steps.metadata` and nothing else, and the run
report has no vote path. That is a choice, not an oversight: `steps.metadata`
is the obvious aggregate home and it cannot hold this, because a tribunal's
seats share ONE step row whose bag merges last-write-wins
(`internal/engine/metadata.go`), so seat B's claim would erase seat A's.

**Unreadable is not absent.** A cell that does not decode still never fails
the read — one odd cell must not break a whole proposal's vote verbs — but it
reads back with `metadata_unreadable` set rather than as an empty bag. A
corrupted, truncated, or in-place-rewritten claim is therefore distinguishable
from a seat that asserted nothing, which is the difference an operator
investigating a store needs and the write-side cap alone cannot give.

**One cap, one quantity.** `VoteMetadataMaxBytes` bounds the ENCODED bag at
both ends. `vote cast --metadata` decodes the caller's text, re-encodes it,
and measures that — the same bytes `marshalVoteMetadata` measures at the
column — because raw text and encoded JSON diverge in both directions:
`encoding/json` escapes `<`, `>` and `&` to six bytes each, and drops
insignificant whitespace. A raw-text check under the same constant would
refuse bags the column accepts and accept bags the column refuses.

The other half — the SPEND QUANTITY for a tribunal seat — stays exactly
where it already was, and `votes.metadata` does not move it. No
`usage_ledger` row is written on the cast path, because that ledger is keyed
`UNIQUE(step_id, attempt, unit)` and a vote step's attempt is permanently 0,
so a second seat's insert would collide with the first's and park the
tribunal. `docket dispatch backfill-usage` remains the only writer of vote-
step usage, and a run report still reads vote spend as an absent zero until
an operator runs it. Closing that half needs a key shape that admits one row
per seat — its own issue, not this amendment.

### AMENDMENT — the span extends to v14 (DKT-95, 2026-08-14)

**What changed.** v14 adds ONE table, `vote_usage` — the per-seat vote-spend
ledger the v13 amendment's closing paragraph deferred to "its own issue".
`(id, vote_id REFERENCES votes, unit, quantity, created_at_ms,
UNIQUE(vote_id, unit))`, plus `idx_vote_usage_vote`. The key shape decided:
the VOTE's own id, which is per (proposal, voter) by construction — not a
voter-qualified unit string on `usage_ledger`, which would have fragmented
every per-unit sum that ledger feeds.

**The write half** is `vote cast --usage '{"unit": n, ...}'`: the seat's own
report, validated under the step ledger's B33/B35 rules (finite,
non-negative numbers; opaque unit names) and inserted in the cast's own
transaction. **The read half** is the run report's `vote_usage` section:
`VoteUsageRollup` sums per unit over the run's vote-step proposals, selected
by the same idempotency scope and prefix the vote-metadata rollup reads by.
`docket dispatch backfill-usage` remains the writer of STEP-level vote
usage; the two records answer different questions (what an operator
attributed to the step vs. what each seat reported at cast time) and
neither replaces the other.

**Why the ratified arithmetic is untouched.** Like v11–v13, v14 is an
amendment, not a stage: additive DDL only, `IF NOT EXISTS`, no back-fill (no
pre-v14 seat reported spend, and inventing rows would make the ledger an
opinion about history), and no existing column or verb changes shape. The
rewind guard probes the table (the v5 form), erroring rather than guessing
on a failed probe, per the v13 guard's own reasoning.

### AMENDMENT — the span extends to v15 (DKT-70, 2026-08-17)

**What changed.** v15 adds ONE nullable column, `artifacts.supersedes INTEGER
REFERENCES artifacts(id)`. It names the artifact a given artifact REVISES, and
is NULL — the default, and every pre-v15 row's value — for an original.

**What it fixes.** A held cluster's resolution records its own artifact rather
than annotating the original (payloads-thresholds H13, deliberately: what the
engine computed and what the operator accepted are two records, and rewriting
the first would destroy the evidence of what was held). The two share a `kind`,
and they share a `sha256` too, because the BODY is unchanged and only the
structured payload gains the resolution markers. A run report accordingly showed
`ARTIFACT-71` and `ARTIFACT-81` both as `findings` from `reconcile@0` — same
hash, same 79 bytes — and ledger mining, which counts artifacts as evidence of
work, read one operator decision as a second unit of work.

The reported diagnosis ("dispatch close re-recording an action step's output")
is falsified: `runRoutingStage` does not re-run an action on the hold-resume
path (`resumedFromHold`), so `recordActionResult` is never called twice. There
is no duplicate write. What was missing is which of the two rows records a
computation and which records a decision.

**Why a column and not a kind.** Changing the revision's `kind` would break the
read path — the threshold resolves the step's latest artifact BY kind — and
changing its body would falsify the record. The pointer leaves both rows exactly
as they are: the original stays immutable and addressable, the revision stays
reachable through the same kind, and a consumer counting work counts rows where
`supersedes IS NULL`. It surfaces on the run report's artifact index and on
`step artifact`/`step artifacts` as `ARTIFACT-N`.

**Why the ratified arithmetic is untouched.** Like v11–v14, v15 is an amendment,
not a stage: one additive nullable column, a `hasColumn`-probed `ALTER` so the
migration is idempotent and re-runnable, and NO BACK-FILL — a migration cannot
know which historical pairs were revisions, and inferring them from matching
hashes would be the migration asserting a fact about history it does not have.
The rewind guard probes the COLUMN (the v13 form, since v15 adds no table and no
index), erroring rather than guessing on a failed probe.

### AMENDMENT — the span extends to v16 (DKT-86/DKT-90, 2026-08-17)

**What changed.** v16 adds ONE column, `steps.attempt_base INTEGER NOT NULL
DEFAULT 0` — the attempt count a step's retry budget counts FROM.

**What it fixes.** `steps.attempt` had silently been serving two masters: the
retry budget `max_attempts` bounds (engine-spine §6.10: "retry = attempts
reset") and the usage ledger's key half (`UNIQUE(step_id, attempt, unit)`, v10:
"a retried step's second attempt records beside its first"). `step resolve
--as retry` zeroed the column to reset the budget, so a step that recorded,
parked, and was retried re-claimed under an attempt number the ledger had
already recorded — and the re-execution's genuinely distinct usage was
permanently unrecordable through `dispatch backfill-usage` (observed live:
RUN-13/STEP-132 and RUN-14/STEP-142, both refused "already has usage recorded
for attempt 1" for a second execution whose measured spend then fell out of the
run's ledger entirely).

**Why a base and not a bigger key.** The alternatives were an `--attempt`
override on `backfill-usage` (an explicit rewriting-history channel the verb's
own help forswears) or widening the ledger key with an invocation sequence
(a third counter nothing else needs). Splitting the two meanings restores both
columns' stated contracts instead: `attempt` becomes what §11.4 always said it
was — "claims made against this step, ever", monotonic — and the budget resets
by moving `attempt_base = attempt`, with exhaustion comparing
`attempt - attempt_base` against `max_attempts`. The budget still reads
zero-spent after every retry, so the ratified retry semantics keep their
meaning; the re-execution's claim mints a fresh attempt and its usage records
beside the first execution's.

**Why the ratified arithmetic is untouched.** Like v11–v15, v16 is an
amendment, not a stage: one additive column with a default, a
`hasColumn`-probed `ALTER` so the migration is idempotent and re-runnable, and
NO BACK-FILL — 0 is the correct base for every existing row (no retry has
moved it), and the old reset erased the very history a back-fill would need.
The rewind guard probes the COLUMN (the v13 form, since v16 adds no table and
no index), erroring rather than guessing on a failed probe.

### AMENDMENT — the span extends to v17 (DKT-115, 2026-08-17)

**What changed.** v17 adds ONE column, `vote_usage.source TEXT NOT NULL
DEFAULT 'reported'` — who measured the row.

**What it fixes.** Governance spend had no back-fill path: tribunal seats
carry a proposal id, never a step id, so `dispatch backfill-usage` (step-keyed
by design) could not receive panel cost a relay measured from its transcripts
— measured live at up to the whole of a run's visible spend (RUN-17: 185,673
panel tokens vs 186,606 tracked for the entire run; RUN-15: 46% of true
spend). The new `docket vote backfill-usage` verb closes the path, and it
needs the same provenance discipline `usage_ledger.source` gives the step
side: without the column, a relay's reconstruction and a seat's own
`vote cast --usage` report were indistinguishable forever.

**Why the ratified arithmetic is untouched.** Like v11–v16, v17 is an
amendment, not a stage: one additive column with a default, a
`hasColumn`-probed `ALTER` so the migration is idempotent and re-runnable, and
NO BACK-FILL — 'reported' is the correct source for every existing row, since
only the cast-time writer existed before v17 and a cast-time row is a seat's
own report by definition. The rewind guard probes the COLUMN (the v13 form,
since v17 adds no table and no index), erroring rather than guessing on a
failed probe.

### AMENDMENT — the span extends to v18 (DKT-245, 2026-08-19)

**What changed.** v18 adds ONE column, `issues.resolution TEXT NOT NULL
DEFAULT ''` — how a routing left the issue, when the routing decided something
`status` cannot express.

**What it fixes.** `abandon-issue` deliberately does not force the issue's
status: the run stopping work is a statement about the RUN, and closing the
issue would take the operator's triage decision away. But an issue whose fix
step had already completed is `done`, so after the routing ran, the only
record that the machine had given up was an `issue-abandoned` event — nothing
on the row, no key in `issue show --json`. The tracker went on rendering
"VOD-127 ✔ done" for a fix the run's own review had reproduced as NOT fixing
the flake. Status and resolution are two different questions; one column
answering both is what made the tracker assert the opposite of what happened.

**Why the ratified arithmetic is untouched.** Like v11–v17, v18 is an
amendment, not a stage: one additive column with a default, a
`hasColumn`-probed `ALTER` so the migration is idempotent and re-runnable, and
NO BACK-FILL — an issue abandoned before v18 left its event and nothing on the
row, and inventing a resolution now would assert, on rows nobody re-examined, a
fact the store never recorded. The rewind guard probes the COLUMN (the v13
form, since v18 adds no table and no index), erroring rather than guessing on a
failed probe. On the wire it follows DKT-55's precedent exactly: emitted when
set and only then, so an unresolved issue marshals byte-identically to the
frozen v1 shape.

### AMENDMENT — the span extends to v19 (DKT-238, 2026-08-19)

**What changed.** v19 adds ONE column, `runs.usage_budget REAL NOT NULL
DEFAULT 0` — a cap over MEASURED usage, beside the declared-cost cap `budget`
already holds.

**What it fixes.** Budget enforcement was disconnected from real spend. A raise
tribunal deliberated 140 → 280 declared "units" while measured usage across the
same run's 21 wave directories was output 4,838,739 / cache_creation
14,437,823 / cache_read 689,187,075 — plus an off-ledger manual agent. Its own
security seat said so: "the budget cap tracks declared step costs, not actual
token spend — real usage (475M+ tokens so far) sits outside it, so 280 is a
floor-only proxy". The engine already held the numbers, back-filled per step;
nothing could enforce against them.

**Why two dimensions rather than one.** The quantities are not commensurable.
`budget` counts declared expected costs, which disciplines how much WORK a run
schedules; `usage_budget` counts what the ledger recorded, which bounds what
that work consumed. Folding measured tokens into `max(reported, floor)` — the
existing B12 rule — makes a token count swamp the declared discipline the
instant it is armed, leaving neither question answerable. They are also checked
differently, and must be: a step's declared cost is known BEFORE it runs, so
the declared cap reserves (`spend + cost <= cap`); a step's token spend is not
knowable in advance, so the measured cap can only STOP (`spend <= cap`). Saying
that plainly is better than pretending a reservation exists.

**Why the ratified arithmetic is untouched.** Like v11–v18, v19 is an
amendment, not a stage: one additive column with a default, a
`hasColumn`-probed `ALTER` so the migration is idempotent and re-runnable, and
NO BACK-FILL — no run started before v19 declared a measured cap, so unlimited
is not a guess but exactly what each of them was enforced as. The dimension
stays DORMANT until both a cap and `budget.usage.unit` are set, so B29's
no-query-when-unlimited property holds for it as it does for the original cap.
The rewind guard probes the COLUMN (the v13 form, since v19 adds no table and
no index).

### AMENDMENT — the span extends to v20 (DKT-237, 2026-08-19)

**What changed.** v20 adds ONE column, `run_issues.loop_grants INTEGER NOT NULL
DEFAULT 0` — how many ADDITIONAL fix loops an operator has authorized for one
issue in one run.

**What it fixes.** A fix loop that exhausts `max_fix_loops` parks at
`waiting-human`, correctly — but nothing could then mint another round, and no
verb could ask for one. After HRN-26's third round, verify-ac read 7/14
acceptance criteria unmet and design-qa held 2 blockers, and the workflow
scheduled no further fix round. So the fix was built OUTSIDE the engine: a
general-purpose agent, commit 1666085 ("14 files changed, 1128 insertions"),
cherry-picked with no judge review as a step, and ~100,923 output / 21.9M
cache-read tokens in no ledger. A park that names no next move is what makes
going around the engine the reasonable choice.

**Why a grant rather than a raised bound.** The workflow's `max_fix_loops` is
the author's standing policy over every issue that workflow matches; a grant is
one operator's decision about ONE issue on one occasion. Editing the bound to
unstick a single issue would quietly loosen it for every issue after, and would
leave no record of who reopened what. The effective bound becomes
`max_fix_loops + loop_grants`, evaluated exactly where the bound already was,
so there is one rule and not two.

**Why a new resolution rather than reusing `retry`.** They answer different
questions. `retry` re-runs the parked step — the check that reported the
problem — which asks the same question again; `--as fix-round` says the problem
is real and authorizes another round of WORK on it, judged like every other
round. The grant and the loop entry commit together, so a grant can never
exist without the round it authorized, and the parked step is recorded
`superseded` rather than passed: the question it asked is answered by the new
round's work, not by a verdict nobody reached.

**Why the ratified arithmetic is untouched.** Like v11–v19, v20 is an
amendment, not a stage: one additive column with a default, a
`hasColumn`-probed `ALTER` so the migration is idempotent and re-runnable, and
NO BACK-FILL — zero is correct for every existing row, since no operator
authorized an extra loop before the verb for authorizing one existed. The
rewind guard probes the COLUMN (the v13 form, since v20 adds no table and no
index).

### AMENDMENT — the span extends to v21 (DKT-265, 2026-08-19)

**What changed.** v21 adds ONE column, `gate_results.stub_entry INTEGER NOT NULL
DEFAULT 0` — whether the trust entry that authorized this gate declared itself a
PLACEHOLDER rather than the check its name implies. The declaration itself is a
new `stub` field on the trust entry, set by `docket trust add --stub`.

**What it fixes.** RUN-17's GIT-50 recorded `build`, `secret-scan`, and `tests`
all passing; every one of them was an `echo` stub. RUN-19's `secret-scan` and
`ac-commands` passed via `/usr/bin/true`. Nothing in `gate_results`, the run
report, or the review packet distinguished those rows from a scanner that ran
and found nothing — so a reviewer reading "secret-scan: pass" reasonably
concluded a secret scan happened, and a run whose assurance rested entirely on
stubs was legible only by opening the trust store and reading argvs.

**Why a declaration rather than detection.** Core cannot look at an argv and
tell a real check from a convincing one, and a heuristic that tried would be
wrong in both directions — it would miss `./scripts/scan.sh` containing `exit 0`
and would flag a legitimate `true` guard. This is the same footing
`re_runnable`, `tree`, `flaky`, and `network` already stand on: the operator who
writes the entry knows, and the entry is where they say so. Stubs stay
LEGITIMATE — they are how a repo with no scanner installed still exercises the
workflow's shape, and forbidding them would only push the same placeholder into
a script with a more convincing name.

**Why `stub_entry` and not `stub`.** `gate_results.stub` already exists and
means something else: the row was migrated from an S3 `gate_trail`. That is a
fact about WHICH ERA of this codebase produced the row; the new column is a fact
about WHAT RAN. One column carrying both would answer neither question — the
same erasure DKT-258 files against the run report's status column. In the
rendered FLAGS column the operator-facing word `stub` goes to the new fact,
since it is the word they typed at `--stub`; the migration marker renders as
`s3-migrated`.

**Why the ratified arithmetic is untouched.** Like v11–v20, v21 is an amendment,
not a stage: one additive column with a default, a `hasColumn`-probed `ALTER` so
the migration is idempotent and re-runnable, and NO BACK-FILL. Zero is the only
defensible value for an existing row — a pre-v21 pass is one whose assurance is
UNKNOWN, since the trust store carried no marker when it was written. Stamping
those rows 1 would invent a hollowness nobody declared; 0 says "not declared
hollow", which is exactly true. The distinction becomes readable going forward
only, which is the honest outcome for a fact that was never captured. The rewind
guard probes the COLUMN (the v13 form, since v21 adds no table and no index).

### AMENDMENT — the span extends to v22 (DKT-305, 2026-08-19)

**What changed.** v22 adds ONE column, `runs.pause_origin TEXT NOT NULL` with
the empty string as its default — WHERE a `waiting-human` run's park was
decided. Two values are written: `operator` for `docket run pause`, `budget` for
a breach. The empty default is a run that is not parked, or one parked by its
own steps.

**What it fixes.** A run reaches `waiting-human` two ways with opposite
resolutions, and the row recorded no difference between them. The reconciliation
rollup parks a run when it counts a parked STEP, and resumes it when that count
returns to zero — which is right, because otherwise every step-level park would
need an operator to type `run resume` for a decision the engine made and the
engine already unmade. A run-level park — the `run pause` verb, or a budget
breach — parks no step at all, so the same count reads zero while the park
stands, and the rollup's default branch resumed the run at the next step to
route. DKT-68 caught the budget half and guarded it by reading `breach_reason`.
The operator half was live until DKT-305: RUN-31 was paused at seq 3054 and
resumed at seq 3077 by an empty-data `run-resumed` naming nobody, after which a
four-judge review, a synthesize, a reconcile, and two fresh votes ran unattended
on budget the operator had asked to stop spending.

**Why a column rather than more `breach_reason`.** `breach_reason` is the
budget's own record — B21's sentence, rendered by `run report`, cleared by a
breach-resolving cap raise. Writing an operator's pause into it would make a run
that never breached claim a breach, and would hand the cap-raise verb the power
to release a park it had nothing to do with. The two facts are separable and now
separate: the origin says WHO parked the run, `breach_reason` says WHAT the
budget recorded, and `ClearRunBreachTx` releases the hold only when the origin
it finds is the budget's own.

**Why the ratified arithmetic is untouched.** Like v11–v21, v22 is an amendment,
not a stage: one additive column with a default, a `hasColumn`-probed `ALTER` so
the migration is idempotent and re-runnable, and a rewind guard that probes the
COLUMN (the v13 form, since v22 adds no table and no index). It is the first of
these amendments to BACK-FILL, and the back-fill reads facts already on the
rows rather than inventing any: a parked run with a live `breach_reason` was
parked by the budget, since nothing but `BreachRunBudgetTx` writes that column;
a parked run with no parked step was parked by a run-level verb, since the
rollup parks a run exclusively when it counts one. Leaving those rows at the
default would ship the defect one last time on exactly the databases that have
it — a run parked at the moment of upgrade would come up unmarked and be resumed
by the next step to route. Runs already resumed, done, or abandoned are
untouched; their park is history, and history is in the event log.

### AMENDMENT — the span extends to v23 (DKT-490, 2026-08-21)

**What changed.** v23 adds TWO counter columns on `steps`, both `INTEGER NOT
NULL DEFAULT 0`: `failed_attempts` — claims ended by an explicit `step fail` —
and `reaped_claims` — claims reaped without one (lease expiry,
`max_step_duration`, the forced `step reap`). Together they are the OUTCOME
breakdown of the claims `attempt` counts. A claim that recorded counts in
neither; `step resolve --as retry` touches neither, exactly as it leaves
`attempt` alone; and `failed_attempts + reaped_claims` never exceeds `attempt`
— the remainder is live claims, recorded completions, and pre-v23 history.

**What it fixes.** `attempt` is a claims-so-far spent-count (DKT-64,
docs/tdd/attempt-numbering.md), and that is all it is — it moves at a winning
claim's CAS commit and nowhere else. Two independent consumers in one run
read more into it: an escalation policy walked `attempt - 1` escalation hops
as though every spent claim were a failure, and so escalated a tier on a step
whose only prior claim had been REAPED — a lease expiry, nothing measured,
nothing failed; and a human surface presented the pre-claim sample as the
count while `step show` reported the post-claim one. The sampling half was
already documented; the failure-vs-reap half was not documented because it was
not DERIVABLE: no field on any row carried the distinction, and the event log
that does (`step-failed` vs `lease-reaped`) is prunable and keyed by instance
labels that repeat across a run's issues. attempt-numbering.md §"What to
check" named this exact gap — "a consumer that needs that distinction needs a
separate marker on the row" — and v23 is that marker.

**Why the ratified arithmetic is untouched.** Like v11–v22, v23 is an
amendment, not a stage: two additive columns with defaults, `hasColumn`-probed
`ALTER`s so the migration is idempotent and re-runnable, and a rewind guard
that probes the COLUMNS (the v13 form, since v23 adds no table and no index).
It BACK-FILLS NOTHING, returning to the v11–v21 default deliberately: the
event log could seed a count, but events are prunable, so a derived count is a
lower bound that decays with retention — stamping it into a column would make
the row assert more than the store can promise, where v22's back-fill read
facts STANDING on the rows. Zero on a pre-v23 claim therefore means "no
recorded breakdown", the same never-captured honesty as v21's stub marker;
the counters are authoritative only for claims that ended after v23. The wire
fields are `omitempty`, so every row with no counted outcome serializes
byte-identically to v22's rendering.

### AMENDMENT — the span extends to v24 (DKT-546, 2026-08-22)

**What changed.** v24 adds ONE table, `gate_override_grants`: one operator
ruling that a gate's failure signature — gate name + exit code + reason
classification — is environmental for the remainder of ONE run. A grant is
minted by `step resolve --as override-pass --batch` (one row per failed
completion gate of the parked step, in the resolution's own transaction), and
spent by the routing stage: a later step of the same run whose EVERY failing
gate matches a grant routes the same generic `pass` the operator's own
override-pass records, instead of parking. `covered_steps` counts the spends,
bumped in the routing transaction; both edges are event-logged
(`gate-override-granted` / `step-batch-overridden`, attributed human /
threshold), so the feed walks from every auto-pass back to the person. The
grant dies with its run — the `run_id` FK is the whole scope rule, and a new
run re-asks. A cover blocked by an interposed threshold target (DKT-470's
shape) parks as before, with the block named.

**What it fixes.** Refit mining of 25 runs / 34 issues found the dominant
operator toil is environmental gate parks — build failed 30/46 recorded
verdicts, tests 26/45, self-hygiene 18/34 — virtually every one
operator-overridden as a sandbox artifact, not a code defect, and each park
resolved individually: in RUN-42 the operator's own resolution was
"override-pass each as it parks", the same ruling re-made per step. No
mechanism let one ruling cover subsequent identical failures in the same run.

**Why the ratified arithmetic is untouched.** Like v11–v23, v24 is an
amendment, not a stage: one additive table, `CREATE TABLE IF NOT EXISTS`
throughout so the migration is idempotent and re-runnable, and a rewind guard
that probes the TABLE (the v7/v8 form, since v24 adds no column). It
BACK-FILLS NOTHING — no operator granted a batch override before the verb for
granting one existed — and it is dormant: a run that never records a grant
reads byte-identically to v23 on every verb.

### AMENDMENT — the span extends to v25 (DKT-742, 2026-08-25)

**What changed.** v25 adds ONE table, `stale_target_waivers`: one operator
ruling that a specific stale-target warning — one (step instance, target sha)
pair — has been adjudicated for the remainder of ONE run. A waiver is minted
by `dispatch waive-target` (one row per named step instance, all for one
target sha, in one transaction, each row event-logged as
`stale-target-waived`, attributed human), and consulted READ-ONLY by the
DKT-193/424/451 advisory judge at `dispatch open`/`verify` and at held
resolutions: a would-be warning whose (instance, target) pair matches a
waiver — the sha compared as a case-insensitive prefix of at least 7 hex
characters, because the advisory renders it at 12 — is dropped from the
answer. There is deliberately no "spent" counter and no per-application event:
the advisory is recomputed by `dispatch verify`, which writes nothing by
contract, and a suppressed warning changes no step's state. The waiver dies
with its run — the `run_id` FK is the whole scope rule, and a new run
re-warns.

**What it fixes.** The stale-target advisory had no memory: RUN-52 fired the
IDENTICAL adjudicated warning four times across DISPATCH-295/297/301 (the
shared HEAD moves at every integration, so the pair recurs under a different
rendered reason each time), each firing costing an investigation and the
first an operator gate, until the operator issued a standing waiver that
lived only in session memory — where the engine could not see it. A different
target sha on the same row, or the same sha on an unnamed row, is a different
question and still warns, which is what keeps a new divergence from riding an
old ruling.

**Why the ratified arithmetic is untouched.** Like v11–v24, v25 is an
amendment, not a stage: one additive table, `CREATE TABLE IF NOT EXISTS`
throughout so the migration is idempotent and re-runnable, and a rewind guard
that probes the TABLE (the v24 form, since v25 adds no column). It BACK-FILLS
NOTHING — no operator waived a stale-target warning before the verb for
waiving one existed — and it is dormant: a run that never records a waiver
reads byte-identically to v24 on every verb.

### 2.1 The never-mutate rule

engine-spec.md §3 requires v4 DBs open unchanged and existing verbs stay
byte-compatible. Therefore:

- **Existing column formats are never mutated.** `issues.created_at` /
  `updated_at` stay RFC3339 TEXT forever. Millisecond timestamps and `seq` appear
  **only** in tables created at v5 or later. This is a hard constraint, not a
  preference — rewriting `created_at` to epoch-ms would change every existing verb's
  JSON output and fail §9.8 on its face.
- All v5 DDL is additive and `IF NOT EXISTS` / `ADD COLUMN ... DEFAULT`, so the
  migration is idempotent and backup-safe (§3 "additive-only").
- `ADD COLUMN version INTEGER NOT NULL DEFAULT 1` backfills every existing row to
  version 1 in one statement, with no table rewrite.

## 3. `--json=v2`: the flag surgery

**This is the most compat-sensitive edit in the program.** `--json` is a Bool
persistent flag today (`internal/cli/root.go`). v2 requires a String flag. Verified
current behavior on this worktree, which is the baseline the change must preserve:

| Invocation | Today | After |
|---|---|---|
| `--json` (bare) | v1 envelope | **byte-identical v1 envelope** |
| `--json=true` | v1 envelope | v1 envelope |
| `--json=false` | human output | human output |
| `--json=v2` | `strconv.ParseBool` error | v2 envelope |
| (absent) | human output | human output |

### 3.1 Mechanism

Bool → String with Cobra's `NoOptDefVal`:

```go
rootCmd.PersistentFlags().String("json", "", "Output in JSON format (--json or --json=v2)")
rootCmd.PersistentFlags().Lookup("json").NoOptDefVal = "v1"
```

`NoOptDefVal` is precisely the mechanism that makes a String flag behave like a Bool
when written bare: `--json` with no `=value` takes the value `v1`. This is why bare
`--json` stays byte-identical rather than erroring on a missing argument.

Accepted values, parsed in one helper (`internal/output.ParseJSONMode`):

| Value | Mode | Rationale |
|---|---|---|
| `""` (flag absent) | human | unchanged |
| `v1`, `true`, `1` | JSON v1 | `true`/`1` retain Bool-era spellings |
| `v2` | JSON v2 | new |
| `false`, `0` | human | retain Bool-era spellings |
| anything else | `VALIDATION_ERROR` | e.g. `--json=v3`, `--json=yes` |

`true`/`false`/`1`/`0` are retained deliberately: they parsed successfully under the
Bool flag, so dropping them would be a silent compatibility break for any script
using the explicit spelling. Dormancy is defined against *observable output*, and
those spellings are observable today.

### 3.2 Call-site impact

**There are 24 `GetBool("json")` call sites**, not two — verified by
`grep -rn 'GetBool("json")' --include='*.go' .` against this worktree. Two are in
`root.go` (`getWriter`, `Execute`); the other 22 are the watch-mode preambles of
watch-eligible commands, which re-read the flag to build `watch.Options`.

This is the crux of the flag surgery. `pflag`'s `GetBool` on a String flag returns
`(false, error)`, and every one of these sites discards the error with `_`. So a
missed site does not fail loudly — it **silently returns `false`** and that command
drops out of JSON mode entirely under `--watch`. All 24 must be converted; a
mechanical sweep, but an exhaustive one.

Conversion: each site becomes `GetString("json")` + `output.ParseJSONMode`. To keep
this from being 24 hand-edits with 24 chances to differ, the parse lands in one
helper used by all of them:

```go
// jsonModeOf reports the JSON mode flags for a command. Single reader of the
// --json flag value, so every call site parses it identically.
func jsonModeOf(cmd *cobra.Command) (jsonMode bool, version output.JSONVersion)
```

A guard test asserts zero `GetBool("json")` occurrences remain in the tree, so a
later PR cannot silently reintroduce one.

Every command reads JSON-ness through `w.JSONMode` (an existing `*output.Writer`
field). `Writer` gains a `JSONVersion` field; `JSONMode` stays true for both v1 and
v2, preserving every existing `if !w.JSONMode` branch untouched.

## 4. The v2 envelope

engine-spec.md §5 mandates `{items,total,truncated}`; §11.4 heads its wire-shape
block "envelope per §5". v2 lands **once**, centrally, in `internal/output/` —
per the binding repo facts and because per-command envelope construction is exactly
how envelopes drift.

v1 (unchanged, byte-for-byte):
```json
{"ok":true,"data":{"issues":[...],"total":12},"message":"..."}
```

v2 success:
```json
{"ok":true,"data":{"items":[...],"total":12,"truncated":false},"message":"..."}
```

v2 error: identical to v1's error envelope plus the extended code set (§5).
`{"ok":false,"error":"...","code":"VALIDATION_ERROR"}`.

### 4.1 How a v1 payload becomes a v2 envelope

Commands today return heterogeneous result structs — `listResult{Issues,Total}`,
`nextResult{Issues,Total}`, `docListResult{Docs,Total}`, `voteListResult{Proposals,Total}`,
`logResult{IssueID,Entries,Total}`. v2 unifies these without rewriting each command:

`internal/output` gains an interface a result struct may implement:

```go
// Collection is implemented by results that are a list of items, so the v2
// envelope can render them uniformly as {items,total,truncated}.
type Collection interface {
    Items() any
    Total() int
    Truncated() bool
}
```

- A result implementing `Collection` renders as `{items,total,truncated}` under v2.
- A result not implementing it (single-entity verbs like `issue show`) renders under
  v2 as its own object, unchanged from v1's `data` — §5 specifies the collection
  envelope; scalar payloads have no items/total/truncated to report.
- Under v1, `Collection` is ignored entirely and the struct marshals exactly as
  today. **This is the dormancy guarantee at the type level**: the v1 path does not
  consult the new interface at all.

Each list command gains a 3-method impl (~6 lines). This is the one place per-command
edits are unavoidable, because only the command knows whether its total is pre- or
post-limit.

### 4.2 Explicit truncation flags

`truncated` is computed, not guessed: `truncated = (limit > 0 && total > len(items))`.

| Verb | v4 `total` semantics | `truncated` computable? |
|---|---|---|
| `issue list` | pre-LIMIT count from a `COUNT(*)` query | **yes** — honest today |
| `doc list` | pre-LIMIT count | **yes** |
| `vote list` | pre-LIMIT count | **yes** |
| `next` | `len(ready)` — **post-limit** | **no** — see §5 |
| `issue log` | `len(entries)` — **post-limit** | **no** — see §5 |

## 5. Hard VALIDATION_ERROR on today's silent-drop cases

§5 requires "hard VALIDATION_ERROR on today's silent-drop cases". The silent-drop
cases in this codebase, identified by audit of every `--limit`-bearing verb:

**Case 1 — `docket next --limit`** (`internal/cli/next.go`). The ready set is
truncated by `ready = ready[:limit]`, then `Total: len(ready)` reports the
**post-truncation** count. A consumer receiving `{"total":10}` with `--limit 10`
cannot distinguish "exactly 10 ready" from "500 ready, 490 dropped". The drop is
both silent and unrecoverable from the response.

**Case 2 — `docket issue log --limit`** (`internal/cli/issue_log.go`). `GetActivity`
applies `LIMIT ?`; `Total: len(entries)` again reports the post-limit count. Same
defect.

### 5.1 Resolution

Under **v2 only**, both verbs compute the true pre-limit total and report
`truncated` honestly. This is the correct fix and it is what §5's "explicit
truncation flags" asks for; `next` counts the ready set before slicing, and
`issue log` gains a count query alongside `GetActivity`.

The **hard VALIDATION_ERROR** applies to input that *requests* a silent drop:

| Input | v1 | v2 |
|---|---|---|
| `--limit 0` on `next` | unlimited (falsy) | unlimited — explicit, not a drop |
| `--limit -1` on `next` | unlimited (`limit > 0` false) | **VALIDATION_ERROR** |
| `--limit -1` on `issue log` | clamped to 1 by `max(limit, 1)` — **silently returns 1 row for a request of -1** | **VALIDATION_ERROR** |
| `--limit -5` on `issue list` | passed to `ListOptions.Limit`, `limit > 0` false → unlimited | **VALIDATION_ERROR** |

A negative limit is meaningless input that today is silently reinterpreted — three
different ways in three verbs (unlimited, clamp-to-1, unlimited). Under v2 it is a
hard `VALIDATION_ERROR` naming the flag and the received value. Under v1 every one of
those three behaviors is preserved bit-for-bit.

**Deviation note (nominal, pre-authorized).** §5 says "hard VALIDATION_ERROR on
today's silent-drop cases" without enumerating them or stating the v1/v2 boundary.
Making the error v2-only is forced by §9 item 8 — a v1 hard error on `--limit -1`
would be a behavioral change for a workflow-free repo and would fail the
compatibility AC. Recorded here; filed as a DKT amendment issue citing engine-spec.md
§5 line "hard VALIDATION_ERROR on today's silent-drop cases" and §9 item 8. Semantics
are unchanged (the drop is still eliminated), so the slice proceeds per the
field-name/nominal-deviation allowance.

## 6. CAS: `--if-version` and versions in `.data`

### 6.1 Storage

`version INTEGER NOT NULL DEFAULT 1` on `issues`, `docs`, `proposals`, `labels`.
Every mutating statement in `internal/db` increments it in the same UPDATE that
changes the row — never a separate statement, never a read-then-write.

### 6.2 Surface

`--if-version N` on mutating verbs. When supplied, the UPDATE carries
`AND version = ?`; zero rows affected with the row still present ⇒ `CONFLICT`
(exit 4, the existing code — **not renumbered**).

Mutating verbs receiving `--if-version`: `issue edit`, `issue move`, `issue close`,
`issue reopen`, `issue delete`, `doc edit`, `doc delete`, `vote commit`,
`issue label add|rm`. Read verbs never take it.

The distinction that makes CONFLICT correct rather than NOT_FOUND: a zero-rows UPDATE
is re-probed for row existence. Absent row ⇒ `NOT_FOUND` (exit 2); present row with a
different version ⇒ `CONFLICT` (exit 4).

### 6.3 Versions in `.data`

Under **v2 only**, entity payloads carry `"version": N`. Under v1 the field is
absent — `omitempty` is not sufficient (version 0 vs 1), so the v1 path marshals
through a shape that has no version field at all. Concretely, the version is
attached during v2 envelope construction rather than carried on the shared model
struct, which keeps `model.Issue`'s v1 marshaling byte-identical.

## 7. Idempotency keys on create verbs

`--idempotency-key KEY` on `issue create`, `doc create`, `vote create`,
`issue comment add`, `doc comment add`.

Semantics: within a `(scope, key)` pair — scope being the verb — the first call
creates and records `(scope, key) → entity_id`. A repeat call with the same key
**returns the original entity with the original ID and exit 0**, performing no second
insert. This is idempotency, not conflict-detection: a retried create after a dropped
response must succeed, or the key is useless to the caller it exists for.

The insert and the key record commit in **one transaction**, so a crash between them
cannot orphan either. The `PRIMARY KEY (scope, key)` makes the race a database
constraint rather than an application check.

Absent the flag, behavior is exactly today's. The table is dormant.

## 8. Error taxonomy — extended once, never renumbered

§5 names seven codes. Four exist. The four existing codes and their exit numbers are
**not renumbered** — that is an absolute constraint, since exit codes are the most
widely depended-upon part of a CLI contract.

| Code | Exit | Status |
|---|---|---|
| `GENERAL_ERROR` | 1 | existing — unchanged |
| `NOT_FOUND` | 2 | existing — unchanged |
| `VALIDATION_ERROR` | 3 | existing — unchanged |
| `CONFLICT` | 4 | existing — unchanged |
| `AUTH_ERROR` | 5 | **new** |
| `STALE_LEASE` | 6 | **new** |
| `TIMEOUT` | 7 | **new** |
| `UNTRUSTED` | 8 | **new** |

Extended **once, now, for all future verbs** — §5 says "extended once for all new
verbs", so stages 2–7 add no further codes and renumber nothing. The four new codes
have no emitter in this stage; they are declared constants with mapped exit codes and
a test pinning the mapping. Declaring them now is the entire point: the taxonomy is
frozen before the verbs that raise them exist.

## 9. Dormancy proof (§9 item 8)

Every PR in this stage carries a dormancy proof. The claim under proof:

> v4 DBs open; all existing verbs byte-compatible without `--json=v2`; a
> workflow-free repo shows no behavioral change on any existing verb.

Proof obligations, each mechanically checked:

1. **v4 fixture opens unchanged.** A v4 DB committed at
   `scripts/qa/fixtures/v4-baseline.db`, opened by the built binary, migrated to v5,
   and golden-diffed. Re-runs at every future stage (engine-spec §9.8). The fixture
   is committed as a fixture precisely so later stages inherit the obligation.
2. **Byte-compatibility without `--json=v2`.** Every existing verb run under
   `--json` against the fixture, before and after, diffed byte-for-byte. Non-empty
   output and a zero diff are both asserted — a diff of two empty strings passes
   vacuously and would prove nothing.
3. **Workflow-free repo unchanged.** Human-mode and v1-JSON output identical; exit
   codes identical; the v5 tables present but empty.

## 10. Test plan

**Go unit tests** (`internal/output/output_test.go`, extended):
- `ParseJSONMode` over the full table in §3.1, including the error cases.
- Bare `--json` produces byte-identical v1 output (the compat-sensitive edit —
  **tested first**, per the binding repo facts).
- Guard test: zero `GetBool("json")` occurrences remain in the tree (§3.2) — the
  24-site sweep cannot silently regress.
- v1 envelope unchanged: existing tests must pass untouched, which is itself the
  regression check.
- v2 envelope shape `{items,total,truncated}`; `truncated` true/false computation.
- `Collection` ignored under v1.
- Exit-code mapping for all eight codes, pinning the four existing numbers.
- Version absent from v1 payloads, present under v2.

**Go unit tests** (`internal/db`): v4→v5 migration idempotency; CAS increment;
CAS conflict returns a distinguishable conflict vs not-found; idempotency-key replay
returns the original ID and inserts nothing.

**QA section** (`scripts/qa/test_zd_jsonv2.sh`, `SECTIONS` entry `ZD:test_zd_jsonv2`):
flag-surgery compat matrix; v2 envelope shape; truncation flags on all five list
verbs; negative-limit VALIDATION_ERROR under v2 and preserved v1 behavior; CAS
conflict exit 4; idempotent replay; the §9.8 fixture golden-diff.

**CI** (first commit of PR 1): `go test ./...` and `scripts/qa.sh` jobs added to
`.github/workflows/ci.yaml`. CI runs no tests today and nightly re-cuts `main` daily
with `install.sh` defaulting to nightly — so an untested `main` ships to users within
24 hours. The test jobs land before any behavior change in the same PR.

## 11. Delivery

**Branching deviation (operator decision, 2026-08-02).** The work order specified
PR 1 branching off `origin/main` with PR 2 stacked on it. Branching was declined in
this environment; the operator directed that both PRs be committed directly on
`feature/graph-engine` as stacked commits. PR 1 and PR 2 are therefore commit ranges
on one branch rather than two separable branches. This is a delivery-vehicle change
only — no scope, content, or ordering changes, and the per-PR dormancy proofs are
unaffected. Recorded here rather than as a DKT amendment: it changes no line of
engine-spec.md.

**PR 1** — CI test jobs (first commit); error taxonomy;
the flag surgery; v2 envelopes with truncation flags; negative-limit
VALIDATION_ERROR. Tests: extended `output_test.go`, new QA section.

**PR 2** — stacked on PR 1's commits: v5 migration (CAS columns, idempotency keys,
ms timestamps + seq in new tables only); `--if-version` on mutating verbs; versions
in `.data` under v2; idempotency keys on create verbs; the v4 fixture and its
golden-diff proof; per-file CAS-conflict tests.

`skills/docket/SKILL.md` flag/verb tables update in whichever PR changes the surface
(both, here) — a stale table blocks review per CLAUDE.md.
