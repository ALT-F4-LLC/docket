# Docket vNext: a generic workflow engine for issues

Status: approved — 2026-08-02 · ported verbatim (instance names genericized) from
doc 06 of the graph-engine design set — the upstream design record, which holds the
ratification trail and decisions D1–D16 (cite by number; see amendments.md). Final
boundary (third pass) set after the reference operator clarified the criterion:
features belong **in** Docket exactly when they are valuable and reusable for *anyone*
tracking multi-step work — human teams or agent fleets, any domain — and stay out when
they encode any single instance's specifics.

The result: Docket core grows a **domain-neutral workflow engine** — workflows, steps,
runs, claims, gates, typed payloads, events, usage ledger — and the public story stays
coherent with the README's existing identity ("issue tracking for ai and humans").
Everything instance-specific remains outside as configuration and harness data.

**The genericity rule (binding on every feature below):** core Docket contains zero
agent/LLM vocabulary — no node, model, prompt, brief, severity, or review concepts.
Executors are opaque string hints; execution metadata is an opaque KV bag; ordered
meaning comes from user-registered schemas; computations beyond scheduling arithmetic
are user-trusted commands. The stranger test for every verb: *a team that has never run
an LLM should find it useful as documented.*

What stays **outside** core (the reference instance — the LLM agent fleet this design
was built for): the prose (its contracts, fragments, skills), the workflow/policy
instances and payload schemas (machine-authored per upstream tenet T9, §2 "Instance
config lifecycle"), and one harness adapter plus one-line hook shims. Former instance
glue is absorbed as generic verbs: prompt formatting → `step render`, reconciliation
arithmetic → the builtin `aggregate` action, hook predicates → the `guard` family.
The earlier harness-side engine design (`engine.py`/`.graph/`) is dissolved: semantics
land here, in Go, in `.docket/issues.db`.

## 1. Surface summary

```
docket workflow register|list|show f.toml       # generic grammar (§11); versioned
docket schema   register|list|show name@v …     # payloads; ordered enums supported
                                                #   (list|show added 2026-08-03, DKT-22)
docket trust    add|list|rm …                   # user-level exec allowlist (§4)
docket config   set|get key [value]             # engine defaults: lease TTLs per class,
                                                #   attempt caps, budget default, context caps
docket guard    spawn|record|stop|gate --step NAME [--input …]
                                                # allow/deny predicates for harness
                                                #   enforcement points; exit 0/2 + reason (§2)
docket workflow init [--template NAME]          # scaffold instance config from shipped
                                                #   optional templates (zero-authoring start)

docket run start --request-file … ; activate; pause|resume|abandon; status; report
docket run budget RUN-N --set N                 # raise/lower a live cap (CAS,
                                                #   event-logged — DKT-29, stage 7)
docket next     --run RUN-N --json              # step-level ready set (engine-core §5)
docket dispatch open|close|verify|abandon --run RUN-N
                                                # batch manifest (TTL'd, one open per run,
                                                #   CAS); next refuses while open or
                                                #   discrepancies exist; abandon unsticks
docket run      note add|list RUN-N             # a standing statement recorded once
                                                #   against the run, rendered as
                                                #   `== RUN NOTE N` in EVERY packet the
                                                #   run renders from then on and carried
                                                #   by `context.notes` (DKT-1079)
docket step     claim STEP-N                    # atomic; mints a capability token and
                                                #   returns token + the step CONTEXT bundle
docket step     heartbeat|complete|fail         # token via DOCKET_TOKEN env or stdin
docket step     approve|reject [--note …]       # type=human gate steps only
docket step     resolve STEP-N --as retry|skip|abandon-issue|override-pass [--note …]
                                                # waiting-human resolutions (§2)
docket step     render STEP-N [--template F]    # context bundle → rendered work
                                                #   packet; claim --render returns it
                                                #   atomically (§2)
docket step     show|context
docket artifact show ART-N | list --run RUN-N
docket events   list [--since SEQ] ; --follow ; docket events prune --before …
                                                #   (read verb ships as `list` — DKT-32)
```

Existing verbs (`issue`, `plan`, `board`, `vote`, `doc`, `export`, …) keep exactly one
meaning each; workflow features are additive verbs and dormant tables — a repo that
never registers a workflow observes no change anywhere.

## 2. Semantics per verb group

All semantics are engine-core's (engine-core.md), restated here only where the generic
surface differs:

**Workflows.** Registered TOML in the generic grammar (§11.1): `[match]` predicates
over kind/labels; steps with `executor` (opaque hint), `after`, `inputs`, `fanout`,
`gates`, `threshold`, `on_fail` routing (closed vocabulary), loops with specified
re-expansion rules (step identity per loop entry, threshold re-application, gate
re-runs — the DSL's loop semantics are part of this spec, not implementation
discretion), `max_attempts`, `expected_cost`, `metadata` passthrough. Validation runs
at register time (the grammar itself, plus threshold fields against registered
schemas) and at activation time (bindings resolvable, pins recorded).

**Activation.** Fat transaction as engine-core §3: DAG lints; workflow binding;
version pinning — registered objects by version, **plus arbitrary operator-supplied
file pins** (path + content hash), which is how the reference instance pins its
contracts, fragments, and policy without core knowing what they are; issue-body
snapshots (incl. fenced command blocks, §4); lazy phase expansion; promotion via the
issue verbs. Re-activation lints and expands new phases only, inherits the original
pin set, and is refused while a dispatch is open.

**Steps, claims, capabilities.** As engine-core §5 and the ratified soundness set:
`claim` is CAS-atomic, mints a random capability token (delivered via env/stdin, never
argv), and returns the **context bundle** — step row, issue snapshot, declared input
artifacts, pinned-file list with hashes — in the same response; `--render` returns the
assembled packet instead (§ Rendering). `complete`/`fail`/`heartbeat` refuse
non-holders (AUTH_ERROR) and stale leases (STALE_LEASE). `complete` is the specified
saga — artifact+payload validation → gates one-by-one → routing — with panel-hardened
semantics: **the token retires when the artifact records**; from that commit the saga
is engine-owned and resumes lazily under any later engine invocation, each stage its
own transaction, no subprocess ever inside a transaction, every stage commit
refreshing the step's activity clock. Gates are **at-least-once**: a `gate-started`
event precedes each spawn; on resume, a started-but-unrecorded gate re-runs only if
its trust entry is flagged re-runnable, else the step parks `waiting-human`. Routing
is one transaction spanning step, issue mirror, run, and events. The issue mirror is
LIVE across its full `in-progress`/`review` range, not only its terminal edges:
claiming an issue's first step flips it `todo -> in-progress` (claim's own
transaction, since a claim never routes); a routing transaction that leaves ANY of the
issue's steps `waiting-human` flips it `-> review` — typically a gate or vote awaiting
a decision, but the mirror filters on the status alone, so a step parked by its own
threshold counts the same; an OPEN HOLD counts too (a tripped `hold_spread`
materializes a `type=human` step and stops its routing step at `gated`, so the issue
is blocked on a human without any step reaching `waiting-human`), and so does a
DECLARED `type=human`/`type=vote` step whose TURN HAS COME — an open gate awaiting
`approve|reject`, or an open vote proposal — for the whole window it is open, not only
once something parks it (DKT-334). "Its turn has come" is R3 and its interposition
clauses, asked of the scheduler; the other readiness clauses (run active, scope,
class headroom, budget) are questions about scheduling executor work and are not asked
of a gate nobody claims. A gate whose predecessors are still running does not count,
which is what keeps `review` meaningful in a workflow that declares a gate from
activation; and the mirror is bidirectional, computed fresh against the
steps table on every routing transaction — the issue returns `review -> in-progress`
the moment none of its steps is `waiting-human` any longer. A loop entry does not
clear a park (the supersede sweep takes `pending` instances only), so a resolution, a
retry, or an ABANDONMENT ends `review`. The `review -> in-progress` return records an event and drops NO comment: the trail
narrates what a human is being asked to decide, and "you are no longer being asked" is
the absence of a question rather than a new one. A `--as fix-round` resolution is the
exception and comments, because a new round IS a new fact. Abandonment is the one that
does not route:
both `abandon-issue` and `run abandon --issue` terminalize every remaining step
(a `waiting-human` park among them) in one cascade, so no routing transaction is left
to notice the park count reach zero — each therefore releases the issue back to `todo`
itself, the status the run found it in, and neither completes it (DKT-377). Each of
these writes, alongside `completeIssue`/`abandonIssue`'s pre-existing ones, drops a
short engine-authored comment narrating the transition. `waiting-human`
steps resolve via `step resolve` (retry = attempts reset — the retry *budget*: the
counter itself is monotonic for the usage ledger's `(step, attempt, unit)` key, and
retry moves `attempt_base` instead (DKT-86/DKT-90) · skip · abandon-issue ·
override-pass, note recorded); `approve|reject` belongs to `type=human` gate steps
only, and a human gate's reject routing may not itself be `waiting-human`
(register-time VALIDATION_ERROR).

**Scheduling.** `next` computes readiness (engine-core §5): dependencies,
predecessors, scope non-overlap, run active, concurrency headroom per executor-hint
class (a generic knob; the reference instance's config sets its write class to 1 —
serialization is *instance policy*, not core behavior), budget headroom. Ordering:
priority then age. `dispatch open/close/verify` give batch dispatchers a manifest to
verify against byte-for-byte and make "unreconciled batch" an engine-refusal state —
with recovery designed in: exactly one dispatch open per run (CAS/unique index), a
dispatch TTL lazily auto-abandoned by `next` (event-logged), explicit
`dispatch abandon` for a crashed relay, and enumerated discrepancy resolutions (lease
expiry clears claimed-but-unrecorded; `dispatch close --accept-missing-usage` records
the acceptance). Reaping a **write-class** lease additionally holds write headroom
until the relay acknowledges the `reaped` event (surfaced by `guard spawn`) — the DB
fence is not a tree fence; a wedged writer must be confirmed gone before a successor
writes.

**Fanout joins.** A fanned-out step joins when every sibling is terminal
(`done | skipped | superseded | failed-routed`); a sibling in `waiting-human` parks
the issue. Downstream `inputs` resolve over siblings that RECORDED their work — `done`, and
`superseded` (a `resolve --as fix-round` supersedes the parked instance whose findings
authorized the round, and those findings are the round's whole subject; the loop's
supersede sweep takes only never-run `pending` steps, which own no artifact) — ordered
by (declared position, sibling index, artifact id). `on_fail` applies per sibling; `min_siblings`
(§11.1) permits quorum joins: the join still waits for all siblings to reach a
terminal state (no early cancel in v1), and if `done` count < `min_siblings` at join,
the fanned step routes per its `on_fail`.

**Rendering.** `docket step render` (or `claim --render`) formats the context bundle
into a rendered *work packet* (text) through a template — a shipped generic default,
or a pinned instance template file. Packet *layout* is thereby core mechanics while
*content* stays instance data; no harness needs a formatting script. (The reference
instance's harness hands the packet to an LLM as its prompt; core neither knows nor
cares.)

**Run notes.** `docket run note add RUN-N --text "…"` (or `--file F`, `-` for stdin)
records a standing statement against a run — once — and every packet the run
renders from then on carries it verbatim as a `== RUN NOTE N` section directly
after `== REQUEST`, for every step of every issue; `step context` carries it as
`context.notes`, so a contract can name it. It is the one steering channel that is
run-wide: `step resolve -m` reaches the step it rules on (and the round it
authorizes), which is right for a ruling about one step's work and useless for a
fact about the whole run — a required gate known to fail on clean HEAD, the issue
tracking it, and the disposition already given, which a dispatcher learns before
the first dispatch and which every worker otherwise rediscovers and re-files
(DKT-1079). Issue comments remain an audit surface and never render. Notes are
append-only (a changed ruling is a second note, rendered after the first), capped
at 16 KiB because each rides every packet, refused on a terminal run, and each is
recorded as a `run-note-added` event carrying its text, attributed human.
`docket run note list RUN-N` reads them back in render order.

**Guards.** `docket guard spawn|record|stop|gate` are deterministic allow/deny
predicates over engine state for harness enforcement points (exit 0 allow / exit 2
deny with reason): `spawn` — proposed rows byte-match the open dispatch and no
unacknowledged write reaps; `record` — no unreconciled dispatch; `stop` — no pending
work outside `waiting-human`; `gate --step NAME` — an approved `type=human` step of
that name exists for the active run (the reference instance's commit hook shims
`guard gate --step commit-gate`). Any harness's hook mechanism wires these as
one-liners; the logic lives here. (`step heartbeat` serves the heartbeat hook — an
engine verb, though not a guard predicate.)

**Payloads and thresholds.** `schema register` accepts JSON Schema plus an
`ordered_enum` annotation; thresholds in workflow defs are then generic comparisons
(`threshold = { "fix-loop" = "any(severity >= high)" }` works because *the user's
schema* declared the order — core never knows what a severity is). Aggregations beyond
comparison are **action steps**. One is builtin and generic: `action = "aggregate"`
with `params = { field, method = median|max|min, hold_spread, output, route_at }`
computes over any ordered-enum payload field — median, spread-hold, and a recorded
demotion trail work for severities, priorities, or tiers alike. Cluster membership arrives in the
payload itself: each element is one cluster, whose `field` value is either a scalar
(a one-member cluster — the identity case) or an array of the cluster's member
values; the builtin's input payload is the concatenated payloads of the step's
declared `inputs` artifacts, resolved per the input rules — action steps are
engine-run, never claimed *(amended 2026-08-03, DKT-23 / DKT-28)*. When `hold_spread` trips, the engine
materializes a `type=human` step named `<step>-held` PER HELD CLUSTER, suffixed with
that cluster's payload index (`<step>-held@k#i`), together gating the routing step
*(amended 2026-08-07, DKT-15: one step for the whole hold made approve/reject binary
over the set, so an operator who wanted two clusters escalated and two accepted could
not say so)*. The routing step waits for every one of them and routes once: per
`on_fail` if any was rejected, otherwise through the threshold. An optional
`route_at = "<value>"` names a routing floor in the field's declared order: only
clusters whose reduced value's position is at or above it are emitted to the output
payload — what the threshold evaluates and downstream `inputs` read — while the rest
are recorded, fully reduced, on the aggregate's own `action_results` row and never
enter the loop; a held cluster is never routed below the floor (the operator's
decision, not the untrusted computed value, decides it), an unknown `route_at` value
is a register-time refusal naming it, and with `route_at` absent the output is
byte-for-byte what it always was *(amended 2026-08-23, DKT-593)*. The
aggregate's output payload — per-cluster value, members, held flag, `demoted_from`,
`operator_resolved` — validates against the shipped `aggregate@1` schema, and
`operator_resolved` is set per cluster, on the approved ones only. The
reference instance's reconciliation is therefore parameters, not code. Other
computations remain user-trusted commands receiving step context on stdin.

**Budgets.** Enforced against `max(reported, floor)`; floor = Σ claimed steps'
`expected_cost` (from the workflow def) — engine-owned facts, so the cap holds with
reporting absent. Reported usage (opaque numbers on `complete`) only raises the
counter; missing usage rows are a dispatch discrepancy.

**Runs, report, events.** As engine-core §3/§1.6: ledger rollup (usage, attempts,
gate trail, artifact index, metadata rollups), NDJSON event feed with `--follow`,
prune verb. Read verbs render *effective* status (lease expiry computed at read time,
no write) — status never lies just because nobody called `next`.

**Instance config lifecycle (upstream tenet T9 — zero-touch).** Instance files live in
the repo at `.docket/config/` (`workflows/`, `schemas/`, `contracts/`, `fragments/`,
`templates/`, `policy.toml`) — git-versioned like any code. They are
**machine-authored**: a bootstrap agent drafts them at project start (mining the
build system, test commands, git history, conventions), the retro pipeline evolves
them from run evidence, and the human only approves in conversation. **Activation
auto-registers** the config directory's current contents (content-hash versioning) —
registration is never a manual step, and schemas are stable reviewed files, never
generated per-run. Strangers start from shipped optional templates
(`docket workflow init --template standard-dev`) — a working baseline with zero
authoring. At plan approval the session surfaces what activation will bind — including
every harvested fenced command, verbatim — so what the human approves is what was
actually read, not a summary. Developer responsibility, by design: provide work,
approve at gates — nothing else.

## 3. Storage

Schema v5 on `.docket/issues.db`: additive tables (runs, steps, artifacts,
gate_results, events, pins, dispatches, trust-cache) + `version` CAS columns on
existing entities (part of §5). Dormant unless workflows are used; existing verbs'
behavior is byte-compatible (`--json=v2` opt-in aside). Artifacts capped at 1MiB with
explicit refusal; events carry monotonic `seq`; lifecycle specified: `events prune`,
artifact GC per run-retention config, documented backup (`sqlite3 .backup`), WAL-on-
synced-directory warning in docs. `docket migrate` is idempotent, backup-first,
additive-only; v4 DBs open unchanged. `events prune` refuses events of non-terminal
runs and never crosses the artifact-retention boundary; `events --follow --since`
below the retained minimum returns GONE rather than silently skipping.

## 4. Execution trust model (gates, actions, fenced commands)

Docket executes registered commands at workflow transitions — the one place it runs
anything — under a trust model fit for an OSS tool:

- **User-level trust only, repo-scoped by default.** Executable argv templates live
  in `~/.config/docket/trust.toml` (per-user, never repo-shipped), managed by
  `docket trust`; each entry binds to the repo it was approved in unless `--global`
  is explicitly chosen — an argv trusted for one project does not execute in a
  malicious clone of another. Workflow defs and issues reference names only. A cloned
  repo can never introduce execution; unknown names require an explicit one-time
  `trust add` (TOFU, argv-hash recorded).
- **Fenced command blocks** (generic form of the reference instance's ```ac```
  convention): a workflow gate may declare `source = "fence:<tag>"`; commands are
  harvested from the issue body **at activation** (snapshotted, hashed —
  post-activation edits cannot inject) and each must match a trust entry — a full-argv
  hash satisfies the match; prefix entries are the explicit opt-in case — or it is
  *not executed* and reported as unmatched. Any team using executable acceptance
  checks gets this; no team gets drive-by execution.
- Mechanics: resolved argv, no shell interpolation, cwd repo root, env allowlist,
  timeout with process-group kill, captured output with explicit truncation,
  flaky-declared re-runs recorded individually. Read verbs never execute. Gates are
  at-least-once and **must be idempotent**; `re-runnable` is a per-entry trust flag
  (§2). Gates that touch the working tree declare `tree = true` and serialize on an
  engine-held per-repo mutex — parallel read-step completions never race a build.
- Trust entries default to **full-argv hashes**; prefix entries are explicit opt-in
  (`trust add --prefix`, with an over-authorization warning). Tokens pass via
  env/stdin, never argv; claim markers are 0600 in a per-user runtime dir.
- **Conversational trust (zero-touch posture):** in this solution the session
  proposes, the human approves in-chat, and the session runs `trust add --yes` — the
  harness's own command-permission prompt is the human-confirmation backstop. The
  residual risk (a misbehaving session self-trusting a command) is accepted and
  bounded by full-argv hashing plus the permission layer (upstream D14).

## 5. Docket core reliability delta (stage 1; unchanged from prior spec)

CAS `--if-version` everywhere + versions in `.data`; uniform envelopes
(`{items,total,truncated}`) behind `--json=v2`; explicit truncation flags; hard
VALIDATION_ERROR on silent-drop cases (fires under `--json=v2` only; v1/human output
stays byte-identical per §9.8); idempotency keys on create verbs; millisecond
timestamps + `seq`; error taxonomy extended once for all new verbs (NOT_FOUND,
VALIDATION_ERROR, CONFLICT, AUTH_ERROR, STALE_LEASE, TIMEOUT, UNTRUSTED; `GONE`
appended at stage 6 for the events read surface — DKT-33, 2026-08-05). Generic,
valuable standalone — and it deletes nine wrapper scripts in the reference instance's
predecessor tooling.

## 6. Concurrency model

As engine-core §9, in Go: WAL, busy_timeout, single-transaction mutations, CAS claims,
lazy lease reaping confined to `next`/`claim` (reads never write; read verbs compute
*effective* status from `expires_at`). No subprocess ever executes inside a
transaction — each saga stage commits separately (§2). Multiple dispatchers are safe
and pointless. The reference instance's writer serialization is its concurrency
config (§2), enforced by `next` like any other headroom rule.

## 7. Out of scope (hard boundaries)

No daemon; no network; no model calls; no prompt/agent vocabulary anywhere in core
(the genericity rule — a PR introducing `model`, `prompt`, or `llm` into core surface
fails review by definition; metadata KV exists precisely so such things ride through
opaquely); no execution outside §4; no cross-repo federation in v1 (upstream D6); no
VCS coupling beyond the declared `issue.diff` provider (git in v1, §11.1); board/UI
growth limited to opt-in run/step columns.

## 8. Reference instance configuration (examples, not core defaults)

The routing/budget table formerly in this section is unchanged in substance but is now
explicitly *instance data* living in its repo: `policy.toml` (executor-hint → metadata
{model, effort}, delivered to the instance's dispatcher as step `metadata` on `next`
rows — the dispatch script reads no files; `never`-style constraints enforced by its
workflow thresholds over recorded metadata plus the spawn-guard hook),
`expected_cost` per step template, lease TTLs (read 15m / write 45m / research 20m),
write-class concurrency 1, budget warn 60% / pause 100%, context-size warn 64KB / error
128KB, max_attempts 2 / max_fix_loops 2. Core ships with no opinions here.

## 9. Acceptance criteria (when implemented)

1. **Stranger test:** a human-only demo — a docs-review workflow with two steps, one
   fenced-command gate, no agents anywhere — is definable, runnable, and comprehensible
   from public docs alone, with zero references to AI concepts.
2. Zero model-made scheduling decisions in a full run: every transition in events
   traceable to next/gate/threshold/human input.
3. Capability proofs: an unclaimed worker cannot record (AUTH_ERROR); duplicates lose
   at claim (CONFLICT); late completes refused (STALE_LEASE); racing dispatchers cause
   no duplicate execution or lost updates.
4. Kill a worker mid-step: lease expiry alone re-readies it; the run completes; the
   attempt trail is complete.
5. Determinism: same run at same pins ⇒ identical step topology and byte-identical
   context bundles, immune to mid-run issue edits and working-tree changes.
6. Trust: a cloned malicious repo cannot cause execution without a prior local
   `trust add` (proof includes fenced-command harvesting); unmatched commands are
   reported, never run.
7. Budget: with reporting disabled, the run still pauses at the cap from the floor.
8. Compatibility: v4 DBs open; all existing verbs byte-compatible without `--json=v2`;
   a workflow-free repo shows no behavioral change on any existing verb.
9. Dispatch recovery: kill the relay with a dispatch open — TTL auto-abandon (or an
   explicit `dispatch abandon`) restores `next`; nothing is lost or double-executed.
10. Saga safety: crash at every stage boundary of `complete` — resume never
    double-runs a non-re-runnable gate (parks `waiting-human` instead), and a reaped
    write-class step cannot gain a successor until the reap is acknowledged.
11. Zero-touch: from `docket workflow init --template …` through a completed run,
    the only human inputs are conversational approvals relayed by the harness — no
    hand-edited config, no manual registration.

## 10. Staged delivery (ratified: Docket-first, staged)

Each stage ships as a normal Docket release, independently useful:

1. **Reliability delta** (§5) — immediate payoff for the reference instance.
2. **Claims/leases + capability tokens** on issues and steps-to-be.
3. **Workflows, steps, `next`, activation/pinning** — the engine's spine (largest
   stage; loop semantics land here).
4. **Gates + trust model** (§4).
5. **Payload schemas + ordered enums + action steps** (thresholds, reconcile-as-action).
6. **Runs, budgets/floor, report; `dispatch` manifests; the events *read* surface**
   (`events list --since`) — the upstream migration's AC-integrity audit needs it.
7. **Events `--follow` + prune** (observability tail).

Guard verbs land with their underlying features (`stop`/`gate` at stage 3,
`record`/`spawn` at stage 6); stage 3 carries the minimal run subset activation needs
(run row, status, pins) with report/budgets completing at stage 6.

**The v1 shadow run (upstream 07 §4) becomes runnable after stage 6**; stage 7 may
trail it. Harness pieces and the reference instance's config (upstream 03/05/08) can
be authored in parallel from stage 2 onward.

## 11. Appendix: normative grammar and wire shapes

This section is the implementable definition of what §1–§2 describe. Field names are
final unless a stage's implementation review changes them; anything not listed here is
not part of the core surface.

### 11.1 Workflow definition grammar (TOML)

`[pipeline]` — `name` (string, required), `version` (int, required), `description?`.

`[match]` — `kind = [..]`, `labels_any = [..]`, `labels_all = [..]`,
`unless_labels = [..]`. Evaluated at activation; **exactly one** workflow may match an
issue — zero or multiple matches is a VALIDATION_ERROR naming the issue and the
candidate workflows. Matching is evaluated over the **highest registered version of
each workflow name** (mirroring `workflow show`'s resolution); superseded versions
remain registered for the runs that pinned them but never participate in binding —
this is what makes the version-bump evolution path (D15, the retro skill) able to
complete *(amended 2026-08-05 — bind-to-highest, found by M2a's toy run)*.

A registered version may be **retired** from binding with
`docket workflow deprecate <name>@<version>` *(amended 2026-08-07 — DKT-21)*.
Retirement is a **binding-time filter, never a deletion**: the row stays,
`workflow show` still renders it, `--source` still emits the exact registered
bytes, and a run that already pinned it still resolves it and still completes.
Retired versions are excluded from the candidate set **before** the
highest-version reduction, so retiring the top version falls back to the next
one that still binds rather than removing the name from routing. Retiring
*every* version of a name removes that name from binding entirely — which is
the point: a name, once registered, otherwise binds forever, and deleting its
TOML does not unregister it. When no version of any name can bind, the
zero-match error says so specifically rather than claiming nothing is
registered. `--restore` reverses a retirement. There is deliberately **no
delete verb**: old versions stay registered, which is what keeps lineage
readable.

A registry is **per project** (`UNIQUE(project_id, name, version)`), and
`workflow register`, `workflow deprecate`, and `schema register` therefore
accept `--project <ref>` and `--all-projects` *(amended 2026-08-24 — DKT-615)*.
Without either flag each verb writes to the project the working directory
resolves to, unchanged, and emits the row it always emitted. With either flag it
emits a **per-project report** instead — one outcome per target
(`registered` / `unchanged` / `deprecated` / `restored` / `already-binding` /
`already-deprecated` / `conflict` / `not-registered` / `invalid`) — and each
project's own idempotency and conflict rules are decided *there*: a CONFLICT in
one project neither cancels nor hides another's registration, and the sweep
runs to the end. The process exits with the failures' shared code when they
agree and GENERAL_ERROR when they do not, having already written the report.
`workflow register`'s **environment validation runs per target**, because
`vote_rule` and `payload` references resolve against the registry of the project
being written to — the same bytes can be valid in one project and name a schema
that does not exist in the next, and storing them there anyway would defer a
guaranteed activation failure. Register schemas store-wide first.

`[limits]` — optional map of executor *class* → `{ max = N, lease_ttl = "45m" }` (bare
int = shorthand for `max`). When a run pins multiple workflows, the most restrictive
limit per class wins; unset values fall back to `docket config` defaults. Classes also
carry `max_step_duration` — a schedule-to-close bound independent of heartbeats, so a
runaway executor cannot renew forever. (The reference instance's writer serialization
is exactly `"write" = { max = 1, lease_ttl = "45m", max_step_duration = "2h" }`.)

`[[step]]` fields:

| Field | Type / default | Meaning |
|---|---|---|
| `name` | string, required, unique in workflow | step identity; instances are `name@k#i` (§11.3); the suffix `-held` is reserved for engine-materialized steps *(amended 2026-08-03, DKT-26)* |
| `executor` | string (opaque hint) | worker step; core never interprets the value |
| `action` | string (trusted command name) | deterministic computation step (§4) |
| `type` | `"human"` \| `"vote"` | operator gate / proposal-driven gate |
| — | | exactly one of `executor` / `action` / `type` / `fanout` per step |
| `fanout` | [executor hints] | expands to parallel siblings `name@k#0..n-1`, one per hint |
| `class` | string, default = executor value | concurrency-accounting key for `[limits]` |
| `emits` | artifact-kind string, required on executor steps | binds the step to its recorded artifact kind (`inputs` resolution; an instance contract may mirror it for its worker's benefit — the workflow is authoritative) |
| `payload` | `schema@ver`, optional | payload validated at `complete`; threshold fields check against it at register time; required on `action = "aggregate"` steps *(amended 2026-08-03, DKT-25)* |
| `voters`, `vote_rule` | [executor hints], proposal-config name | required on `type="vote"` steps — who casts, which existing Docket threshold config tallies |
| `after` | [step names], **required** except the first step and `loop = true` steps (whose ordering comes from loop entry, §11.3) | intra-workflow predecessors; `[]` = root (implicit topology was a footgun) |
| `inputs` | [`"<step>.<kind>"` \| `"<step>.*"` \| `"<step>.vote-record"` \| `"issue.body"` \| `"issue.diff"` \| `"issue.linked.<relation>.<kind>"`] | artifacts inlined into the context bundle, in order. `issue.diff` = the engine-computed VCS diff for the issue's scope, snapshotted and fingerprinted when its producing step completed (git in v1 — the one declared VCS coupling, §7). `<step>.vote-record` = the named `type="vote"` step's recorded proposal — tally outcome, weighted score, and every cast with its rationale — engine-served from the existing vote machinery; the named step must be a vote step, and the `vote-record` kind is reserved from `emits` *(amended 2026-08-22, DKT-545)*. `issue.linked.<relation>.<kind>` = a CROSS-ISSUE input: the latest recorded artifact of `<kind>` held by each issue this issue is linked to by `<relation>` (a relation type or its inverse form — `depends_on`, `dependency_of`, `blocks`, `blocked_by`, `relates_to`, `duplicates`, `duplicate_of`), resolved and pinned by artifact id at activation inside the fat transaction; activation fails loudly when the relation is missing or no linked issue holds the kind, so the binding is enforced rather than an issue-body citation. V11's produced-kind table deliberately does not apply — the producer is another issue's run — and the `issue.linked` name is reserved from step names as `issue.latest` is *(amended 2026-08-22, DKT-547)* |
| `gates` | [trusted gate names \| `{name, source="fence:<tag>", pre=bool}`] | `pre = true` gates run at claim with results included in the context bundle (measure-then-judge steps); the rest run in order inside `complete` (§2, §4) |
| `params` | opaque KV table | arguments to `action` steps (e.g. the builtin `aggregate`) |
| `min_siblings` | int, default = all | fanout join quorum (§2 Fanout joins); the default is the plain join — quorum semantics (the `on_fail` routing at join) apply only when declared below the sibling count *(clarified 2026-08-03)* |
| `threshold` | table: routing → predicate (11.2) | routing computed over the step's recorded payloads; on `type="vote"` steps, over the tally's cast set after an APPROVED tally (11.2) *(amended 2026-08-22, DKT-545)* |
| `pass_floor` | `{ field, at }`, optional; requires `payload`, and `at` must be a value of `field`'s declared order (V37/V37a) | exit bar on a `pass` routing: when the routing resolves to `pass` but the step's recorded payload holds an element whose `field` value sits at or above `at`'s position — and the element is neither `held` nor `operator_resolved` — the step parks `waiting-human` instead of exiting, naming `--as override-pass` and `--as fix-round` as the ways out. Both values are opaque tokens compared only by position, `route_at`'s discipline; declared nowhere, nothing changes *(amended 2026-08-26, DKT-870: RUN-58's reconcile routed `pass` with all 16 clusters open, six at the order's high position — "converged" in the ledger meaning "dispositioned")* |
| `on_fail` | `"fix-loop"` \| `"waiting-human"` \| `"skip"` \| `"abandon-issue"`; default `"waiting-human"` | routing for gate failure / attempts exhausted; `type="human"` steps must declare it explicitly and `"waiting-human"` is invalid there — reject routes per `on_fail` (§2's reject-routing rule; amended 2026-08-03) |
| `loop` | bool, default false | marks loop-body steps (11.3) |
| `after_loop` | step name | re-entry target after a loop body completes |
| `serves` | [step names], only on `loop = true` steps | scopes the body to the named steps' `fix-loop` routings — its loop CLUSTER (11.3); omitted = serves every trigger. Entries must name steps that can route `fix-loop`, and every step that can must be served by at least one body *(amended 2026-08-22, DKT-544)* |
| `max_attempts` | int, default engine config | per-instance retry budget |
| `max_fix_loops` | int, default engine config | loop-entry budget per issue — ONE counter over EVERY `fix-loop` routing source (threshold, `on_fail`, rejected vote/human gate, quorum miss), read off whichever non-cluster step declares it. Each admitted entry post-increments the counter to its own 1-indexed ordinal; an entry whose new count exceeds the bound is refused with the counter restored, so `= N` admits exactly N entries and parks the N+1th `waiting-human`. Only a `fix-round` grant (one per resolution, effective bound = declared + grants) admits more *(amended 2026-08-23, DKT-587)*. On a `serves`-scoped loop body it is instead that CLUSTER's round budget, checked independently under the issue-level ceiling — it never raises or lowers it *(amended 2026-08-22, DKT-544)* |
| `max_stalled_rounds` | int ≥ 0, default 0 (never fires); only on a step that can route `fix-loop` and records an artifact (V38) | non-convergence tolerance over THIS step's routed volume: a `fix-loop` entry after that many consecutive measured rounds in which the element count of the step's recorded payload never fell below the smallest count any earlier round recorded is refused in the non-convergence park's exact shape — counter restored, nothing instantiated, `waiting-human` naming `--as fix-round` as the way out, an authorized entry waived. "No improvement" means no new strict minimum, so volumes oscillating around a floor still park while a genuinely shrinking set never does *(amended 2026-08-26, DKT-870: RUN-51 held 8-12 clusters flat across TEN rounds and RUN-50 7-10 across six, both ended only by operator action — the plateau was the corpus's own non-convergence signal and nothing in the engine read it)* |
| `expected_cost` | number ≥ 0, default 0 | budget-floor contribution per claim (§2) |
| `when` | predicate over issue `kind`/`labels` — clauses `<kind\|labels> <==\|!=\|contains> <value>` or `labels contains-any (a, b, c)` / `labels contains_any [a, b, c]`, joined by `and` throughout or by `or` throughout | step is `skipped` when false. `or` holds when at least one clause does; a predicate MIXING `and` and `or` is a VALIDATION_ERROR (V22), because the grammar has no parentheses and therefore no reading of `a and b or c` to prefer — the mixed case is expressed as two steps, which is what the disjunction removed the need for in the common case *(amended 2026-08-22, DKT-548)*. `labels contains-any (…)` is the step-level spelling of the `labels_any` [match] clause and holds when the list intersects the issue's labels — a CLAUSE, not a connective, so "kind X and any of these labels" is one homogeneous-`and` predicate rather than a mix V22 would refuse. The list needs at least one element and its values carry no whitespace *(amended 2026-08-22, DKT-550)*. The operator is spelled `contains-any` or `contains_any` and its list is delimited by `(…)` or `[…]`; all four combinations are the same clause, and the delimiters must pair — `[a, b)` is a VALIDATION_ERROR. Both spellings were admitted rather than one because `contains-any (…)` is what registered definitions carry and `contains_any [a, b]` is how a list is written everywhere else in a workflow TOML, so refusing either would make an author's first correct guess an error *(amended 2026-09-01, DKT-1000)* |
| `metadata` | opaque KV table | recorded on the step; delivered in the context bundle |

### 11.2 Threshold predicates

`threshold` maps a routing (`"fix-loop"`, `"waiting-human"`, `"pass"`, or a step name)
to a predicate string, evaluated top-to-bottom, first match routes; no match ⇒ `"pass"`.
Routing to a **step name** interposes that declared step as a successor gate.
A step is interposed because **some threshold names it as a routing target** —
never because of its `after` shape (V8 forces every non-first step to declare
`after`, so the canonical authoring shape is `after = [routing-step]`, which
anchors the gate in the topology and gives the staged closure its stage;
DKT-38). An interposed step's readiness carries a latch: it becomes ready only
when a routing predecessor's **recorded** routing names it, and when the
routing resolves anywhere else the unrouted gate is terminalized `skipped` in
the same routing transaction — so joins and issue completion resolve, and on
the gate's own pass execution resumes at the routing step's ordinary
downstream (the reference instance's security workflow interposes its vote
gate this way — upstream 05 §2). Grammar: `agg(field op literal)`
where `agg ∈ {any, all, count>=n}` over the payload array,
`op ∈ {==, !=, >=, >, <=, <}`, and ordered comparisons are defined **only** for fields
whose registered schema declares `ordered_enum` (§2). Fields and literals are
validated against the registered schema at `workflow register` time. Example
(standard-change): `threshold = { "fix-loop" = "any(severity >= high)" }`.

**Vote-step thresholds** *(amended 2026-08-22, DKT-545)*: on a `type="vote"` step,
`threshold` is evaluated over the proposal's recorded **casts** — one element per
cast, addressable fields `vote` / `verdict` (aliases for the cast's verdict) and
`voter` — and only after an **APPROVED** tally. A rejected tally routes per
`on_fail`, exactly as before; a manually committed proposal (an operator setting
the final outcome by hand) skips the threshold. The routing vocabulary is
restricted to `"fix-loop"` / `"waiting-human"` / `"pass"` — step-name
interposition is not available on vote steps — and operators to equality, because
casts have no registered schema and ordered comparisons are defined only over
`ordered_enum` fields (all register-time rules: V36). First match routes, no
match ⇒ `"pass"`, and a step declaring no threshold behaves exactly as it always
did. Example (an investigation read-gate):
`threshold = { "fix-loop" = "count>=2(vote == approve-with-concerns)" }` sends an
approved-but-concerned tally into the same revise loop a rejection enters,
instead of the concerns evaporating; the loop body reads what the panel said
through `inputs = ["<step>.vote-record"]` (§11.1).

### 11.3 Loop semantics (normative)

Step instances are identified `name@k#i` — `k` = loop ordinal (0 at initial
expansion), `#i` = fanout index (absent when not fanned out). When a routing resolves
to `"fix-loop"`: (1) the issue's loop counter increments; exceeding `max_fix_loops`
routes `waiting-human` instead — loops are bounded by construction. A round is also
refused as NON-CONVERGENT when the round below it left the issue's scope
byte-identical to the round below that (`issue.diff` fingerprints), because the next
round would read the same tree and reach the same verdict; that refusal takes the same
shape as the bound's (nothing superseded, nothing instantiated, counter restored,
`waiting-human` naming `step resolve --as fix-round` as the way out) and is waived by
that resolution. It never fires on a diff that records no content — an empty or
unresolvable-base measurement is evidence the tree was not measured, not evidence that
it did not move. (2) Not-yet-claimed
instances downstream of `after_loop` (e.g. a pending `verify@0`) transition to the
terminal status **`superseded`** (event-logged); claimed/running instances finish, but
routing from a superseded lineage is inert. (3) Steps marked `loop = true` — excluded
from ordinary expansion — instantiate at ordinal `k`, with `inputs` bound within
ordinal `k` (falling back to the highest earlier ordinal per input), ordered by
(declared position, sibling index, artifact id) — never event order. (4) When the loop
body's gates pass, `after_loop` and its downstream chain re-instantiate at ordinal
`k`; gates re-run; thresholds re-apply. Issue completion is evaluated over
highest-ordinal instances only. Prior instances and artifacts remain immutable and
addressable; the ledger attributes every instance. There is no other loop construct.
Predecessor satisfaction and issue completion resolve per step name over instances at
the highest existing ordinal ≤ the consumer's (mirroring input binding);
re-instantiation never spans steps outside the `after_loop` chain. *(Clarified
2026-08-03, S3 stage review.)*

**Cluster scoping** *(amended 2026-08-22, DKT-544)*: a `loop = true` body may declare
`serves = [step names]`, scoping it to the named steps' `fix-loop` routings. The
TRIGGERING step — the one whose routing resolved to `fix-loop` — selects its
cluster: clauses (2)–(4) then apply to the serving bodies and to the downstream
chains of THOSE bodies' `after_loop` roots only (a body or `after_loop` declarer
without `serves` serves every trigger, so a workflow declaring no `serves` anywhere
has exactly one cluster and the original behavior, unchanged). The loop counter,
its ordinal sequence, the `max_fix_loops` ceiling read off non-cluster steps, the
non-convergence refusal, and `fix-round` grants all stay issue-level across every
cluster. A `max_fix_loops` declared on a `serves`-scoped body additionally bounds
that cluster's own rounds — counted as the distinct ordinals holding its scoped
bodies' instances (bodies serving several triggers are counted wherever they ran) —
and its refusal takes the ceiling's exact shape, waived once by the same
`fix-round` resolution. The `loop-entered` event data names the trigger alongside
the ordinal. Register-time rules: `serves` is valid only on `loop = true` steps,
every entry must name a step of the workflow that can route `fix-loop` (V35), and
every step that can route `fix-loop` must be served by at least one body (V17c).

Engine-enforced numbers live core-side, never in opaque pins: per-class lease TTLs and
concurrency (`[limits]` / `docket config`), attempt caps (step fields / config
defaults), the per-run budget cap (`docket run start --budget N`, config default), and
context-size warn/error caps (config). Opaque instance files may carry anything else.

### 11.4 Wire shapes (JSON, `--json` mode; envelope per §5)

```
next row        { step, instance, issue, run, executor, class, attempt,
                  expected_cost, lease_ttl_s, metadata }
claim response  { step, token, lease_expires_ms, context }
context         { step: <next row>, issue: {id, title, body_snapshot, kind, labels,
                  scope}, inputs: [{artifact, kind, producer_step, body, payload?}],
                  pins: [{path, sha256}], loop_entry, metadata, pre_gates?,
                  notes?: [{id, text, recorded_at_ms}] }   # notes: DKT-1079
dispatch        { dispatch, run, opened_seq, rows: [<next row>…] }   # verify = byte-equality on rows
complete args   --artifact-file F  [--payload-file F]  [--usage '{"unit":n,…}']
                [--metadata '{…}']   (token via DOCKET_TOKEN env or stdin — §4)
gate result     { step, gate, argv, exit, duration_ms, output, truncated, verdict,
                  reason? }
action result   { step, action, argv, exit, duration_ms, output, truncated,
                  verdict, builtin, reason? }   # argv/exit NULL for builtin|unmatched
                                                #   (added 2026-08-03, DKT-24)
event           { seq, at_ms, kind, run?, step?, step_id?, data }   # step_id: DKT-34
```

`instance` is the rendered `name@k#i` identity (§11.3) carried alongside the `STEP-N`
id on next rows and, via `context.step`, in the context bundle *(added 2026-08-03,
DKT-15)*. `pre_gates?` is an array of §11.4-shaped gate results for the step's
`pre = true` gates, present only when the step declares them; `reason?` carries why
a verdict is `unmatched` or timed out, null on ordinary pass/fail *(added
2026-08-03, DKT-19 / DKT-20)*.

`notes?` is the run's recorded notes (`docket run note add`, DKT-1079) in insertion
order, present only when the run has at least one, so every bundle of a run without
notes is byte-identical to before the member existed. `run note list RUN-N` and
`run note add`'s answer carry the same `{id, text, recorded_at_ms}` shape.

`docket step context STEP-N` re-emits `context` read-only (no token required; local
inspection). `--meta` on it reports per-section byte counts — the closure-size record
(engine-core §8) — `notes_bytes` among them, since a note rides every packet.
