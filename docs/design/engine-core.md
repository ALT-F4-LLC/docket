# Engine core semantics

Status: approved — 2026-08-02 · ported (agent vocabulary generalized to
executor-hint vocabulary) from doc 02 of the graph-engine design set — the upstream
design record. This document defines *what the engine means*; engine-spec.md defines
the surface, and its §11 is the normative grammar. **Docket core carries the whole
workflow engine, designed domain-neutrally** — runs, steps, artifacts, gates, leases,
workflows, typed payloads, events — while instance domain content (the reference
instance's contracts, fragments, prompt formatting, workflow/policy instances,
reconcile arithmetic) rides on top as hash-pinned config, trusted actions, and harness
data. "The engine" below means Docket core. The reference instance — the LLM agent
fleet this design was built for — appears throughout as the worked example; nothing it
names (reviewers, models, severities, prompts) is core surface.

## 1. Entity model

Six entities defined below (run, issue, step, artifact, gate result, event — issue
exists today, artifact generalizes today's doc), plus Docket's existing proposal/vote
entities, retained unchanged and driven as gates (§6). Workflow definitions are
registered, versioned objects (`docket workflow register`); instance files (contracts,
fragments, policy) are repo files hash-pinned per run at activation via generic file
pins — core versions them without knowing what they are.

```
run 1──* issue 1──* step *──* artifact
              │         │
              │         └──* gate-result
              └──* relation (depends_on | blocks | relates_to | duplicates | parent)
policy, pipeline-def: versioned config, referenced by id@version from runs/steps
event: append-only log of every transition above
```

### 1.1 Run

The unit of "the operator asked for something." Holds the request text, a reference to
the plan artifact, status, the policy version and pipeline versions pinned at
activation, and the ledger rollup (usage, cost, wall-clock, attempts) that the
upstream measurement plan (08) reads.

Run status: `planning → active ⇄ waiting-human → done | abandoned`. `paused` is
`waiting-human` with a recorded reason — by policy (budget breach, attempts exhausted,
gate anomaly) or by the operator (`docket run pause`).

### 1.2 Issue (exists today; two additions)

Issues remain the human-facing work items (DKT-N, kind, priority, labels, parent,
relations, comments, files). Two additions:

- **scope** — a list of path globs the issue's implementation is expected to touch.
  Declared by the planner (a judgment, so it may be wrong); *used* deterministically by
  the scheduler for mutual exclusion (§5) and *checked* deterministically by a gate
  (diff-outside-scope ⇒ gate fail ⇒ replan). A wrong guess degrades to a caught error,
  never a race.
- **pipeline binding** — resolved at activation from the issue's kind + labels via the
  pipeline definitions (§4). Recorded on the issue so the binding is inspectable.

The work DAG is issues + `depends_on` relations, exactly as Docket models it today.
`plan` (topological phases) and `next` (ready set) remain the scheduling core.

### 1.3 Step

The unit of execution — one worker invocation or one deterministic action, belonging
to an issue (or to the run itself for run-level steps like planning and final review).
Steps are what the dispatcher spawns and what leases, budgets, and telemetry attach to.

Fields: id (STEP-N), run, issue, an `executor` hint for worker steps or a step type
∈ {`action`, `human`, `vote`} for engine steps (`action` = a named deterministic
computation, e.g. the builtin `aggregate`; matching the workflow grammar's
`type=`/`action=` keys, engine-spec §11.1), sequence info from the pipeline, status,
inputs (artifact refs), outputs (artifact refs), lease {owner, expires_at},
attempt / max_attempts, usage (opaque numbers), duration, and opaque execution
`metadata` (the reference instance records requested/resolved model tier and effort
here).

Step status machine (every transition CAS-guarded and event-logged):

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

Two panel-hardened notes: once the artifact records, the executor's token retires and
the rest of the saga (`gated` onward) is engine-owned — resumed lazily by any engine
invocation, no lease required; and read verbs always render *effective* status (lease
expiry computed at read), so state never lies while nobody is scheduling.

`ready` is *computed*, never stored as intent: a step is ready iff its issue's
dependencies are satisfied, its intra-pipeline predecessors are done, its scope conflicts
with no claimed/running step (§5), the run is active, and concurrency + budget headroom
exist (§7). `docket next` returns exactly this set — the entire scheduling decision.

### 1.4 Artifact

Every worker output. Markdown body + metadata: id (ART-N), run, producer step, kind
(free-form string: `plan`, `tdd`, `change-summary`, `findings`, `gap`, …), and an
optional **payload** — a JSON document validated against a registered schema (§6).
Artifacts are immutable; a re-run of a step produces a new artifact. Workers reference
artifacts by id; context bundles (§8) inline their bodies.

Today's `doc` entity (DOC-N, revisions) remains for human-authored, long-lived
documents; artifacts are run-scoped and immutable. engine-spec §3 discusses sharing
storage.

### 1.5 Gate result

The record of one deterministic check: step, gate name, resolved command, exit code,
captured output (stored, truncated with an explicit flag), duration, and the verdict
the policy derived from it. Gate results are facts the engine produced itself, so
policies may branch on them (upstream evidence E-8).

### 1.6 Event

Append-only NDJSON feed of every state transition, claim, gate result, budget
increment, and policy trigger, with monotonic sequence numbers. This is what observers
follow (`docket events --follow`, `board --watch`, exporters), what resume replays
(§9), and what external telemetry consumes (an optional exporter — upstream D7 keeps
it out of core); the dispatch relay itself needs only `next` plus its harness's own
completion notifications. The activity log Docket has today is the seed; events
generalize it.

## 2. The two-level graph

**Level 1 — the work DAG (judged).** The planner composes it per run: issues with
`depends_on` edges, scopes, kinds, labels, and acceptance criteria in the body. This
is a judgment — what the work *is* — and therefore, in the reference instance, a
model's job (upstream AC-1).

**Level 2 — the process graph (deterministic).** For each issue, the engine expands a
pipeline — the sequence/fan-out of steps that take that issue from promoted to done
(implement → review fan-out → reconcile → verify → gates → close, in the reference
instance's standard case). Expansion is a pure function of (issue kind, labels,
pipeline definitions @ pinned version). Same issue, same pipeline version ⇒ identical
steps, every time (upstream AC-2).

This split is the load-bearing decision of the whole design. The reference instance's
history shows the process is the *stable, recurring* part (every issue flowed
claim→implement→review→verify→close, enforced by ~100KB of prose); the work is the
*novel* part. The novel part gets judgment; the recurring part gets data.

## 3. Run lifecycle

1. **Intake.** Operator talks to the planner (upstream 03 §2). Planner records the
   request and a plan artifact; creates issues + relations + scopes in `backlog`;
   `run` is `planning`.
2. **Approval (human gate).** Operator reviews `docket plan` / the plan artifact and
   activates: `docket run activate RUN-N`. Activation pins policy + pipeline versions,
   binds pipelines, expands Level-2 steps for phase-1 issues, and promotes them.
   Nothing executes before this gate.
3. **Execution.** Dispatcher loop (upstream 03 §3): `next` → spawn → record → repeat.
   All sequencing, claiming, gating, and branching are engine-computed.
4. **Human gates.** Steps of `type = "human"` (the reference instance's: plan
   approval, commit authorization, security-waiver, budget-breach review) put the run
   in `waiting-human`; the reference harness surfaces them conversationally and stops
   spawning until resolved (upstream 03 §6). Pause blocks new claims but honors
   in-flight completes; abandon revokes live leases.
5. **Completion.** All issues done ⇒ run `done`. The run report (ledger rollup + gate
   summary + artifact index) is generated deterministically: `docket run report`.
   Abandonment is explicit (`docket run abandon`, reason required).

## 4. Pipelines

A pipeline definition is a versioned TOML file in the generic workflow grammar,
registered (`docket workflow register`) and version-pinned per run at activation
(engine-spec §1–§2). It declares, per step: an executor hint (or an
action/human/vote type), inputs (by producer step name), fan-out (a list of executor
hints spawned in parallel), attached gates, `max_attempts`, on-fail routing (one of
the *enumerated* deterministic options: `fix-loop`, `waiting-human`, `skip`,
`abandon-issue`), opaque metadata (the reference instance's model/effort overrides
ride here), and conditions (predicates over issue kind/labels only). Full format:
engine-spec §11.1; the reference instance's standard workflow set lives upstream (05).

Two properties are load-bearing:

- **Closed vocabulary.** Every branch a pipeline can take is one of the enumerated
  routings. There is no expression language, no scripting, no model call. If a situation
  isn't expressible, the answer is `waiting-human`, not cleverness.
- **Version pinning.** Runs pin pipeline versions at activation; editing a pipeline never
  changes an in-flight run. Reproducibility and safe local change both fall out.

## 5. Scheduling, claims, and mutual exclusion

**Ready set.** §1.3 defines readiness. `docket next --run RUN-N --json` returns ready
steps with everything a dispatcher needs to spawn: step, executor hint, class,
attempt, expected_cost, lease TTL, and metadata (the reference instance routes
model/effort through metadata; engine-spec §11.4).

**Claims are capabilities.** `docket step claim STEP-N` is atomic (single SQLite
transaction, CAS on step version): exactly one claimant wins; losers get `CONFLICT`.
The claim **mints a random capability token** (not derivable from ids), returns it
together with the step's context bundle (§8), and every subsequent mutating verb —
heartbeat, complete, fail — refuses a caller whose token doesn't match the live lease
(AUTH_ERROR). An executor that never claimed cannot record; a stale one gets
STALE_LEASE. TTLs and a schedule-to-close `max_step_duration` come from `[limits]` per
executor class, falling back to `docket config` defaults (engine-spec §11.1). Reaping
a write-class lease holds write headroom until the relay acknowledges the reap — the
DB fence is not a tree fence (engine-spec §2).

**Leases.** A claim is a lease. Expiry without completion ⇒ the engine returns the step
to `ready` and increments `attempt` — that *is* the liveness mechanism. Executors renew
implicitly: the reference harness wires a post-tool hook in the executor session to
run `docket step heartbeat` (upstream 03 §5), so any tool activity extends the lease;
a wedged or dead executor stops heartbeating and the lease lapses. This retires the
reference instance's entire hand-built liveness doctrine (probe-once death evidence,
retire-then-replace, dead-man watch scripts).

**Mutual exclusion.** The scheduler never has two claimed/running steps whose issues'
scopes overlap (glob intersection). Overlap ⇒ the later step simply isn't ready yet.
This replaces the reference instance's edit batons and singleton locks with scheduler
refusal — mutual exclusion by construction, not by cooperation. Scope-less issues
(docs-only, investigation) declare `scope = []` and never exclude; a step whose diff
escapes its scope fails the scope gate and routes per pipeline.

**Serialized writers (v1, ratified).** At most one write-class step is claimable at
a time — engine-enforced in `next` — because gates computed on a shared mutable tree
race even across disjoint scopes. Read-only fan-outs parallelize freely (the proven
win). Worktree-isolated parallel writes are an optional instance optimization
(upstream D9), requiring no semantic change here — single-writer is the
always-available baseline.

**Fairness/ordering.** Within the ready set: priority, then oldest-first. The
dispatcher spawns up to the concurrency budget (§7) and may not reorder (it receives
an ordered list). `docket dispatch open` records the batch manifest; `next` **refuses**
while a dispatch is open or while discrepancies exist (claimed-but-unrecorded past
grace, usage rows missing) — relay drift stalls loudly instead of proceeding around
its own mess. Dispatches carry a TTL (lazily auto-abandoned by `next`) and an explicit
`dispatch abandon` verb, so a crashed relay can never wedge a run (engine-spec §2).

## 6. Deterministic computations over worker output

Worker output is prose (an upstream principle; evidence E-2) with exactly one
exception: where the engine runs arithmetic or threshold logic, the relevant slice of
output is a typed payload.

**Findings.** The reference instance's review-family workers attach a `findings`
payload: a JSON array of `{id, title, severity, file, line?, evidence}` with
`severity ∈ {info, low, medium, high, blocker}` — the archetypal sanctioned typed enum
(per-AC status is the other), because code computes over it. It is a registered
*instance* schema with `ordered_enum` — core knows the order, never the meaning.
Everything else a reviewer wants to say lives in the markdown body.

**Clustering (judged), reconciliation (computed).** After a review fan-out, a
synthesizing worker clusters duplicate findings across reviewers — that is judgment
(upstream evidence E-7's measured run-to-run variance lives here and is tolerated).
Then the builtin `aggregate` action (engine-spec §2 — the reference instance's
reconciliation is its parameters: field=severity, method=median, hold_spread=2)
computes, deterministically: per-cluster severity = median; clusters with spread ≥ 2
levels are `held` — excluded from automatic routing, and their presence materializes a
held-findings `type=human` step the operator resolves (`docket step approve|reject` —
the note records the disposition, e.g. an accepted severity; the reconciled payload is
annotated `operator_resolved` and thresholds re-apply over the resolved set); every
demotion records `demoted_from`. Zero-variance arithmetic (upstream evidence E-3).

**Verdicts are policy, not prose.** The review step's outcome is computed from the
reconciled set by the pipeline's threshold policy — e.g. the reference instance's
standard-change: any `blocker` or ≥1 `high` in-scope ⇒ route `fix-loop`; only
`medium`s and below ⇒ pass with the findings attached to the issue. Reviewers never
say APPROVE/BLOCK; nothing reconciles "verdicts"; nobody overrides a reviewer — there
is nothing to override.

**AC verification.** Same pattern: the verifying worker emits per-AC judgments
(`met | unmet | unverifiable`, with evidence) as a payload; command-verifiable ACs are
written as ```` ```ac ````-fenced blocks in issue bodies (an activation lint requires
the convention), **snapshotted and hashed at activation** — what the operator approved
is what runs; post-activation edits cannot inject — and executed only when each command
matches the operator's user-level trust allowlist (engine-spec §4), else reported
`unverifiable`, never executed. Extraction is literal (no prose parsing); the
routing decision over {gate exits × per-AC statuses} is policy.

**Votes.** Docket's existing proposal/vote machinery is kept and demoted to a gate
type: a `type="vote"` step creates a proposal, fans out the configured voters
(engine-spec §11.1) to cast via CLI, and the engine computes the outcome from
threshold config (it already does: weighted score, required voters). Used where the
reference instance votes (TDD acceptance, security waivers, destructive-change
approval — upstream 05).

## 7. Budgets and cost governance

Budget and control inputs are core-owned (engine-spec §11): per-step `expected_cost`
in workflow definitions, the per-run cap (`docket run start --budget`,
`docket config` default), attempt caps (step fields, config defaults), and
concurrency + lease TTLs per executor class (`[limits]`). The instance's `policy.toml`
carries only dispatcher-side spawn routing — the engine never reads it.

Usage is recorded per step (executors attach what they observe; the reference harness
back-fills from its dispatch journal, source recorded), but the cap does not rest on
it: the engine enforces against `max(reported, floor)` where the **floor** is computed
from facts the engine itself produced — each claimed step's `expected_cost` from the
workflow definition (engine-spec §2), accrued per claim event — retries and loop
entries re-accrue, never released; bounded loops bound the floor. Reported usage can
only raise the counter, and missing usage rows are a dispatch discrepancy blocking the
next batch (§5). Crossing the run cap flips the run to `waiting-human` with a
budget-breach reason — a hard control resting on engine-owned inputs, not voluntary
reporting. Burn-rate is visible at any time (`docket run report`). Per upstream
evidence E-5, the governed quantities are the ones that dominate spend: number of
invocations (fan-out widths, attempt caps) and closure sizes (recorded per context
bundle, §8) — not contract byte counts.

Model routing is a reference-instance policy table, opaque to core: executor hint →
{model, effort}, with per-run overrides at activation. The step records requested vs
resolved model in its metadata (the harness may substitute), which makes tier drift
measurable (exactly the census the reference instance previously maintained by hand).
Security-sensitive executor hints carry a `never: [model…]` list honored at spawn by
the instance's dispatcher (a constraint pinning security-sensitive work off a given
model tier is a policy row with a reason, not folklore).

## 8. Context assembly (bundles and rendered packets)

Context delivery is split at the genericity line: **`docket step claim` returns the
context bundle** — step row, activation-time issue snapshot, input artifact bodies,
pinned-file list with hashes — in the claim response itself (one atomic mediation: an
unclaimed executor has nothing, a claimed one has everything, closing upstream
evidence E-6's unread-pointer hole). **Formatting that bundle into a rendered work
packet** (the reference instance's spawn prompt) is `docket step render` (or
`claim --render` in one atomic call): a shipped generic template, or the instance's
pinned template file — packet *layout* is core mechanics, packet *content* stays
instance data (engine-spec §2). The reference instance's assembled packet contains:

1. Framing header (run/issue/step ids, the issue body's AC section, scope);
2. The step's contract file at its pinned version (an opaque pin to core; upstream 04);
3. The fragment files the contract's frontmatter declares, inlined (opaque pins);
4. Input artifacts (bodies, in pipeline-declared order), each delimited and labeled;
5. The output instruction (artifact kind to emit; payload schema name if any).

Properties: assembly is pure and **snapshot-pinned** — packets read only hash-pinned
contract/fragment files, the activation-time issue-body snapshot, and recorded
artifacts (the diff artifact fingerprinted when its producing step completed), never
the live tree or a live issue body: same step ⇒ same packet, byte-identical, even
mid-run (engine-spec §2). Closure size is recorded on the step (the honest cost
figure; upstream evidence E-5); nothing in it is optional for the worker (upstream
E-6 — no pointers, no "read if needed"). Oversized context bundles (config caps,
engine-spec §11.1) are an engine error at expansion time — the fix is a
pipeline/contract change, visible before spend, not a silent 107KB spawn.

## 9. Resume and crash-safety

All durable state is in Docket (engine-spec §3 — workflow tables are additive and
dormant for non-workflow users); every mutation is CAS-guarded and event-logged. The
consequences, mechanically:

- **Executor dies** ⇒ heartbeats stop ⇒ lease lapses ⇒ step back to `ready`,
  attempt++. Its partial artifact was never recorded (recording is atomic at completion),
  so no torn state exists.
- **Dispatcher session dies** ⇒ nothing happens. Leases lapse; the run sits `active`
  with a ready set. Any new dispatcher session picks up exactly where things stood —
  the reference harness injects `docket run status --active` at session start so
  resumption is automatic (upstream 03 §7). There is no handoff narrative, no
  crash-recovery doctrine.
- **Docket contention** ⇒ SQLite WAL + busy timeout + single-transaction claims
  (engine-spec §6); losers retry or receive CONFLICT — never a partial write.
- **Replay** ⇒ within the retention window (engine-spec §3), the event log + immutable
  artifacts + pinned versions reconstruct a run's history exactly (what ran, with
  which context, producing what, gated how, costing what). This is the audit story,
  the debugging story, and the telemetry story — one mechanism.
