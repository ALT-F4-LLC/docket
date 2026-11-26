# Saga work items and the engine split

Status: Proposed — 2026-10-18

The daemon owns the store and the saga state machine. Every subprocess the
saga needs (a gate, an action, a git probe of a checkout) becomes a work item
that a client claims, runs on its own host, and reports back. Stranded-saga
resumption moves out of `next` and `claim` into a daemon-side lease sweep.

This note designs the work-item protocol (claim, report, resumption), the
engine split between daemon and client, and classifies every `resolvePaths`
and `trust.Load` call site under `internal/` by the side it lands on.

Inspected revision: `4458bcf928b207d693b103ca1ae8309e0e3eb0e7`. Line
numbers below refer to that commit.

## Starting point

At that revision the engine runs gates, actions and git calls in whichever
process calls it:

- `NewEngine` (`internal/engine/saga.go:210`) builds the gate runner, the
  action runner and the git functions from the invoking process's own
  `resolvePaths()`.
- The saga already separates announcement, execution and recording.
  `runGateStage` (`saga.go:934`) commits a `gate-started` event in
  transaction A, runs the gate outside any transaction, and records rows and
  advances `saga_stage` in transaction B. `runRoutingStage` (`saga.go:1046`)
  runs its "two subprocess-shaped computations", the issue diff and the
  action, before its transaction opens.
- A step with completion gates detaches into a `step resume` child
  (`LaunchDetachedSaga`, `saga_detached.go:38`) that holds a per-step flock.
- `NextSteps` calls `ResumeStrandedSagas` before its readiness pass
  (`next.go:109`), which resumes any non-held, non-action saga idle past
  `strandedSagaGraceMS` (`saga_detached.go:131`, `saga_detached.go:140`).
  Step claim runs pre-gates inline in the claim phase `pre-gates`
  (`claim.go:794`).
- A started-but-unrecorded gate is re-run when its trust entry is
  `re_runnable` and parks the step otherwise (`resolveInterruptedGate`,
  `saga_resume.go:57`). That decision reads the trust store.
- The trust store is the operator's file under `XDG_CONFIG_HOME` or
  `$HOME/.config` (`internal/trust/store.go:227`), with no other source.

The rulings this design follows: gates and actions run on the client in
every mode; the client claims saga work items and reports results; and
stranded-saga resumption leaves `next` and `claim`. The Connect API note
(`docs/design/connect-api-v1.md`) adds a constraint already in force: the
daemon never opens a client path.

## Work items

A work item is one subprocess-shaped unit of a saga stage. The daemon creates
it when a saga reaches a stage that needs one, and the stage cannot advance
until the item is reported.

| Kind | Created by | Replaces |
|---|---|---|
| `gate` | a completion-gate stage | `runGate` inside `runGateStage` |
| `pregate` | a step claim whose step declares `pre` gates, or a target first existing for a detachable pre-gate | `runPreGates` in claim, and the detached `step pregate` child |
| `action` | the routing stage of an action-bearing step | `e.Actions.Run` in `runAction` |
| `git` | the routing stage of a tree-holding step, and the round-delta append | `e.DiffFn` and `e.HeadFn` calls in `computeIssueDiff` and `appendRoundDelta` |

The daemon stores each item in one table, `saga_work_items`, with: item id,
run id, step id, kind, the saga stage it belongs to, the spec (JSON), status
(`open`, `leased`, `reported`, `parked`), lease owner, lease token hash,
lease expiry, attempt number, and the `re_runnable` flag the claimant
reported. A step has at most one unreported item per stage, enforced by a
unique index on (step id, stage, attempt).

The spec carries everything the existing runners already take, so the client
needs no store read to run it:

- `gate` and `pregate`: the `GateSpec` (name, source, fenced commands and
  their hashes) and the `StepContext` (`saga.go:986`: instance, run, issue,
  step id, snapshotted scope, recorded work root, base sha, pin root and file
  pins).
- `action`: the `ActionSpec` (name, params, output kind, input payloads,
  context bytes) and the declared payload schema bytes, so the client
  validates against the pinned document the run agreed to.
- `git`: the operation (`issue-diff`, `head`, `ancestor`, `patch-contained`,
  `tree-match`, `object-exists`, `commit-patch`), the directory (the step's
  recorded work root or the run's recorded exec root), and its revisions and
  scope.

### Claim

Inputs:

- run id, and optionally a step id to restrict the claim to one step's items;
- the kinds the client accepts;
- the client's owner string and its resolved project identity
  (`config.Config.Identity` from its own `config.Resolve`);
- for a `gate` or `pregate` item, the client's trust match for the item's
  gate: matched or not, and the entry's `re_runnable` flag. The client sends
  these with a second call, `WorkItemAck`, after the claim returns the gate
  name and before it spawns anything.

Daemon behavior, in one transaction:

1. Refuse when the run is not active, or when the client identity differs
   from the run's project identity: a gate must run against the project the
   run belongs to.
2. Select the oldest `open` item for the run (or step) whose kind the client
   accepts.
3. Write the lease: owner, a fresh token (stored hashed, as step tokens are),
   expiry, status `leased`, attempt incremented.
4. For a `gate` or `pregate` item, write the `gate-started` event that
   transaction A writes today. Claiming and announcing the spawn are the same
   commit, so a lease that later expires without a report is exactly
   today's started-but-unrecorded gate.

Outcomes:

- `claimed`: the item id, kind, spec, token and lease expiry.
- `none`: no claimable item. The answer carries the count of items leased to
  other owners, so a worker can tell "nothing to do" from "someone else is
  doing it".
- `refused`: run not active (CONFLICT) or identity mismatch (VALIDATION_ERROR),
  with the reason.

A running item's lease is extended with `WorkItemHeartbeat(item id, token)`.
Its outcomes are `extended` (new expiry) or `lost` (CONFLICT: the lease
expired and the item reopened or parked; the client stops the subprocess and
reports nothing).

### Report

Inputs:

- item id and token;
- the result in the kind's own shape: the `GateResultRow` list a gate or
  pre-gate produced (verdict, exit code, output, stub, pre); the
  `ActionResult` (artifact body, payload, outcome); or the git answer (body,
  sha, or the `known`/answer pair the git functions return today).

Daemon behavior:

1. Verify the token against the lease and that the item is `leased` and
   unexpired. Verification and recording share one transaction.
2. Record the result the way transaction B does today: `recordGateRows`,
   `recordGateEvents`, then `db.AdvanceSagaTx` with the stage the item
   belongs to as the CAS expectation. For `action` and `git` items, store the
   answer on the item. The routing stage opens its transaction once every
   item it created is reported.
3. Mark the item `reported`.
4. After commit, continue the saga in the daemon: run every following stage
   that needs no subprocess (`enterRouting`, the routing transaction, vote
   and hold handling, `DriveRunLifecycles`) until the saga completes or
   creates its next work item.

Outcomes:

- `accepted`: the step's saga stage after the report, and the next work item
  id when the saga created one.
- `replayed`: the same token reports an item already `reported`. Nothing is
  written; the original outcome is returned. A client retrying a report over
  a dropped connection is safe.
- `stale` (CONFLICT): the lease expired, or the token does not match. Nothing
  is written. The result is discarded; whichever attempt holds the current
  lease decides the stage.
- `invalid` (VALIDATION_ERROR): the result does not fit the kind, or an action
  payload fails the declared schema. Nothing is written and the lease stays,
  so the client can correct and report again before expiry.

### Stranded-saga resumption

A saga is stranded when an item's lease expires without a report: the client
died, lost the network, or was killed with its subprocess. Resumption is a
daemon-side sweep. It never runs inside `next`, step claim, or any other
client verb.

Inputs: the clock, and every `leased` item whose expiry has passed. The
daemon runs the sweep on a ticker and once at startup.

Daemon behavior, per expired item, in one transaction CAS-guarded on the
item's status and attempt:

- `gate` or `pregate` with `re_runnable = true`: write a `gate-rerun` event
  and return the item to `open`. The next claimant runs it again, which is
  the at-least-once behavior `resolveInterruptedGate` gives today.
- `gate` or `pregate` with `re_runnable = false`, or whose claimant never
  acknowledged a match: mark the item `parked` and park the step the way
  `parkInterruptedGate` does today. Fail-closed is the only direction when
  the question is whether the command already ran.
- `action` and `git`: return the item to `open`. Today a resumed routing stage
  re-runs `runRoutingStage` from its start (`advanceOne`, `saga.go:872`),
  which recomputes both; reopening keeps that behavior.

Outcomes: each expired item is `reopened` or `parked`, with an event naming
the item, the lapsed owner, and the attempt. Open items with no worker are
reported, not resumed: `next` and `run status` carry a count of open saga work
items, so a conductor sees that a worker is needed. `next` reads that count
and writes nothing for it.

Held sagas are not stranded. A tally or an approval that decides a hold still
resumes the routing step from the verb that decided it (`next.go:526`,
`human.go:1000`, `held.go:1028`). After the split that resume runs only daemon
stages and creates work items; it never waits on a subprocess.

What the sweep replaces:

- `ResumeStrandedSagas` and its call in `NextSteps` (`next.go:109`);
- the per-step saga flock (`detachedSagaLockPath`, `sagaHeldElsewhere`), whose
  single-resumer guarantee the lease gives instead;
- `LaunchDetachedSaga` as a saga driver. Step record still launches a
  detached client child, `docket step resume STEP-N`, but that child is now a
  worker: it claims the step's items, runs them, and reports until `none`.

## Engine split

| Side | Owns | Inputs | Outcomes |
|---|---|---|---|
| Daemon engine | The store; stage 0 and 1 of record; the saga stage machine and its CAS; routing, votes and holds; scheduling (`next`, step claim, dispatch); the work-item table, leases and the sweep; the store-side shared config corpus | RPC requests, including work-item claims and reports; its own store configuration from `config.Resolve` at daemon start (store directory and shared config directory only) | Committed state transitions, work items, events. It spawns no gate, action or git subprocess and opens no client path. |
| Client executor | The gate runner (`ExecRunner`), the action runner (`ExecActionRunner`), the git functions, the trust store, the invocation's `config.Resolve` (exec root, identity, tree lock, the checkout-side config root), the tree mutex and pre-gate locks | Work items from claim; the operator's trust roster; the checkout | Reports; acks carrying the trust match; client-gathered facts sent with requests (the checkout-side config scan at activation, the trust preflight) |

The seam is the one the saga is already written against. `GateRunner` and
`ActionRunner` keep their signatures and move to the client side unchanged;
the daemon's `Engine` drops the `Gates`, `Actions` and git function fields,
and each place that called them creates a work item instead. The client
worker loop is: claim, run the spec through the existing runner, report.

The daemon is reached through the same claim and report calls in every mode.
Without a separate daemon process the CLI hosts both halves in one process
and the calls use an in-process transport, so local mode and daemon mode run
one code path.

Trust stays on the client in every case. The daemon learns only what a client
reports about its own roster (matched, `re_runnable`). It never reads a trust
file, so a remote daemon cannot be pointed at one and a repository cannot
change what the daemon believes is trusted.

Instance config splits by root. The store-side shared corpus
(`SharedConfigDir`) sits beside the store and is read by the daemon. The
checkout-side root (`<worktree>/.docket/config`) is read by the client, which
sends the scanned files with the request that needs them, the way
`WorkflowRegister` already sends a definition's TOML.

## Call-site classification

Every result of `grep -rn "resolvePaths\|trust.Load" internal/` at
`4458bcf928b207d693b103ca1ae8309e0e3eb0e7`, by file and function. "Client"
means the read moves to the client executor; "daemon" means it stays with the
daemon engine.

| File:line | Function | Reads | Side | Why |
|---|---|---|---|---|
| `internal/cli/trust_probe.go:73` | `runTrustProbe` | `trust.Load` | client | Probes the operator's roster by spawning its entries. |
| `internal/cli/trust.go:409` | `runTrustList` | `trust.Load` | client | Lists the operator's roster. |
| `internal/engine/repopaths.go:30`, `:35` | `resolvePaths` (doc and definition) | `config.Resolve` | client | Resolves the invoking process's cwd. The daemon has no invocation cwd; it resolves its store once at start. |
| `internal/engine/saga.go:211` | `NewEngine` | `resolvePaths` | client | Builds the runners from exec root, identity and tree lock. Becomes the client executor's constructor; the daemon engine is built without runners. |
| `internal/engine/saga.go:2813`, `:2823` | `runExecRoot` (doc and body) | `resolvePaths().ExecRoot` | client | Feeds `sharedCheckoutHead`, a git call on the run's checkout; the fallback names the invoker's checkout. |
| `internal/engine/render.go:365`, `:369` | `renderTrustRoster` | `trust.Load`, `resolvePaths().Identity` | client | Matches the packet's `== GATES` lines against the roster. |
| `internal/engine/fence_report.go:234`, `:235` | `buildFenceReportTx` | `trust.Load`, `resolvePaths().Identity` | client | Matches harvested fence commands against the roster. The fence rows it reads come from the daemon. |
| `internal/engine/gate_script_pins.go:23` | `gatePinStore` (package var) | `trust.Load` | client | Supplies the roster to `verifyGateScripts`. |
| `internal/engine/gate_exec.go:62` | `NewExecRunner` | `trust.Load` | client | The gate runner's roster. |
| `internal/engine/action_exec.go:69` | `NewActionRunner` | `trust.Load` | client | The action runner's roster. |
| `internal/engine/autoregister.go:317` | `instanceConfigRoots` | `resolvePaths` | client | Unions in the checkout-side config root for packet composition. |
| `internal/engine/autoregister.go:327` | `runConfigRoots` | `resolvePaths` | client | Same roots with the run's recorded exec root, a checkout path. |
| `internal/engine/pregate_detached.go:105` | `detachedPreGateLockDir` (package var) | `resolvePaths` | client | The tree lock's directory; pre-gate locks serialize subprocesses on the host that runs them. The saga flock that also used it is replaced by the lease. |
| `internal/engine/orphan_registration.go:78` | `ScanWorkflowOrigins` | `resolvePaths().InstanceConfigDirs()` | client | Scans the union, which includes the checkout-side root. |
| `internal/engine/action_names.go:30` | `ActionNames` | `resolvePaths().InstanceConfigDirs()` | client | Classifies roster entries for `trust list` and `trust probe`, both client verbs. |
| `internal/engine/verify_pins.go:154` | `VerifyPins` | `resolvePaths().ExecRoot` | client | Compares gate scripts under the invoker's exec root with their pins. |
| `internal/engine/registry_audit.go:100` | `ScanCorpus` | `resolvePaths().SharedConfigDir()` | daemon | Reads only the store-side shared corpus and audits every project's rows against it. |
| `internal/engine/activate.go:364`, `:509`, `:520` | `Activate` | `resolvePaths().InstanceConfigDirs()`, `trust.Load`, `resolvePaths().Identity` | client | The checkout-side scan and both trust reports. The activation transaction itself stays in the daemon; the client sends the scan with the request and builds the reports from the response. |
| `internal/engine/gate_preflight.go:95` | `BuildGatePreflight` | `resolvePaths().ExecRoot` | client | Resolves gate scripts under the invoker's exec root. |
| `internal/engine/gate_preflight.go:280`, `:281` | `buildGatePreflightTx` | `resolvePaths`, `trust.Load` | client | The dry run's trust match. The definitions it reads in the transaction come from the daemon. |

Tests follow their subject:

- `internal/cli/trust_event_test.go` (`trustEntries`): lines 131, 132
- `internal/engine/registry_audit_test.go`: line 332
- `internal/engine/doctor_test.go`: line 234
- `internal/engine/diff_execroot_test.go`: lines 15, 49, 106
- `internal/engine/verify_pins_test.go`: lines 447, 476, 631, 752

## Decision and rejected alternatives

**Decision.** The daemon owns the saga and a lease-based work-item queue. A
client claims a work item, runs it with the existing runners on its own
host, and reports the result. The daemon verifies the lease, records the
result in the transaction that advances the saga, and continues the saga
until the next item or completion. Stranded sagas are lease expiries, which
a daemon-side sweep reopens or parks; `next` and step claim never resume a
saga. The engine splits at the `GateRunner` and `ActionRunner` seam: the
daemon keeps the store and every state transition, and the client keeps every
subprocess, every checkout read, and the trust store.

It won because it reuses the saga's existing three-part stage (announce, run
outside a transaction, record under CAS) as the protocol's claim, execution
and report. The single-resumer guarantee moves from a host-local flock to a
lease in the store, which holds when the client and daemon are on different
hosts.

Rejected alternatives:

- **The daemon runs gates and actions itself.** The engine moves whole into
  the daemon and the CLI becomes a thin client. Lost because it contradicts
  the ruling that gates and actions run on the client in every mode, and
  because a remote daemon has neither the checkout nor the operator's trust
  roster. Running them would need the daemon to open client paths.
- **The client runs the whole saga against row-level RPCs.** The daemon
  becomes a store proxy and the engine library stays in every client. Lost
  because each stage's CAS and the routing stage's multi-table commit would
  span network round trips, every client version would have to carry the
  full state machine, and single-resumer enforcement would again need a lock
  the daemon cannot see.
- **The daemon pushes work to a connected client over a server stream.** Lost
  because it needs the client to stay connected for the length of the slowest
  gate, which the recording turn is designed not to do. A dropped stream
  would strand the saga with no durable record of who held it. Unary claim
  and report fit the existing Connect service.
- **Keep stranded-saga resumption in `next` and step claim and only move
  execution to the client.** Lost because it contradicts the ruling, and
  because it ties scheduling latency to gate duration: a `next` that resumes
  a saga waits on that saga's subprocesses.
- **The client computes the issue diff at record time instead of a `git`
  work item.** Lost because the diff would be taken before the completion
  gates run, while today the routing stage takes it after them. That
  changes what review reads whenever a gate touches the tree.

## Open questions

- The packet's `== GATES` section is rendered from the roster. After the split
  the daemon renders the packet without it and the client appends it at claim.
  A consumer that hashes the rendered packet must hash the daemon's part only.
- Non-saga git calls (the dispatch integration check's `e.HeadFn`,
  `dispatch.go:1347`, and `diff_repin.go:123`) follow the same rule: the
  client gathers the facts and sends them with the request. Their RPC shape
  belongs to the issue that moves dispatch verbs.
