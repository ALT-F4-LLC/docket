# Begin site audit

Status: Current — 2026-10-18

Every non-test `Begin` and `BeginTx` call site, classified as read or write.
The row set is the output of this command, one row per matched line:

```sh
git grep -nE '\.(Begin|BeginTx)\(' -- '*.go' ':!*_test.go'
```

`git grep -cE` with the same pattern and pathspecs prints per-file counts that
sum to the row count below (93).

## Why this matters

`db.Open` adds `_txlock=immediate`, so a plain `Begin` is `BEGIN IMMEDIATE`
and holds the write lock until it commits or rolls back. A pure read opened
that way makes writers in other processes wait on it, up to their
`busy_timeout`. `db.BeginReadSnapshot` opens a deferred `BEGIN` instead, which
takes no write lock. It is safe only for a transaction that issues no write.

## Classification

- **read**: the transaction is never committed. Every path rolls it back, so
  it writes nothing durable. These are candidates for `db.BeginReadSnapshot`.
- **write**: the transaction commits a write: an insert, update, delete, reap,
  lease change, or event append.

The fifteen existing callers of `db.BeginReadSnapshot` (`claim.go` two,
`dispatch.go` three, `dispatch_integration.go` one, `events_read.go` three,
`held.go` one, `next.go` two, `report.go` one, `run_budget.go` one, `saga.go`
one) already open read snapshots. They do not match the pattern and are not
rows.

`internal/engine/claim.go:322` is a comment that names `conn.Begin()`. It
matches the pattern, so it is a row; it refers to the claim's write
transaction at line 361.

## Sites

| Site | Function | Class | Basis |
|---|---|---|---|
| internal/cli/import.go:175 | doImport | write | imports exported rows |
| internal/db/db.go:79 | BeginReadSnapshot | read | the snapshot helper itself, formerly `beginReadSnapshot` in `internal/engine/report.go`; `ReadOnly` deferred BEGIN |
| internal/db/docs.go:307 | UpdateDoc | write | updates a doc and its revision |
| internal/db/engineconfig.go:955 | SetConfig | write | writes a config key |
| internal/db/files.go:22 | AttachFiles | write | inserts issue files |
| internal/db/files.go:63 | DetachFiles | write | deletes issue files |
| internal/db/files.go:122 | SetIssueFiles | write | replaces issue files |
| internal/db/idempotency.go:187 | beginIdempotentCreate | write | opens the transaction its CreateXIdempotent caller inserts into and commits |
| internal/db/issues.go:126 | CreateIssueIdempotent | write | inserts an issue |
| internal/db/issues.go:610 | updateIssueCASLease | write | version bump or note insert on a no-field edit |
| internal/db/issues.go:633 | updateIssueCASLease | write | updates issue fields |
| internal/db/issues.go:894 | OrphanSubIssues | write | clears parent links |
| internal/db/issues.go:941 | CascadeDeleteIssue | write | deletes an issue tree |
| internal/db/issues.go:1256 | ClearAllData | write | deletes all rows |
| internal/db/labels.go:171 | DeleteLabel | write | deletes a label |
| internal/db/labels.go:228 | AddLabelsToIssue | write | inserts label links |
| internal/db/labels.go:323 | RemoveLabelsFromIssue | write | deletes label links |
| internal/db/leases.go:95 | leaseTarget.claim | write | takes a lease |
| internal/db/leases.go:249 | leaseTarget.heartbeat | write | extends a lease |
| internal/db/leases.go:280 | leaseTarget.release | write | releases a lease |
| internal/db/projects.go:461 | MoveIssueProject | write | moves an issue between projects |
| internal/db/proposals.go:61 | CreateProposalIdempotent | write | inserts a proposal |
| internal/db/proposals.go:231 | CastVoteWeighted | write | inserts a vote |
| internal/db/proposals.go:654 | CommitProposal | write | records an outcome |
| internal/db/proposals.go:711 | CloseProposal | write | closes a proposal |
| internal/db/relations.go:45 | CreateRelation | write | inserts a relation |
| internal/db/relations.go:120 | DeleteRelation | write | deletes a relation |
| internal/db/schema.go:133 | Initialize | write | creates the schema |
| internal/db/schema.go:4189 | applyMigration | write | applies one migration |
| internal/db/schemas.go:45 | InsertSchema | write | inserts a schema |
| internal/db/schemas.go:321 | DeprecateSchema | write | marks a schema deprecated |
| internal/db/schemas.go:350 | RestoreSchema | write | restores a schema |
| internal/db/workflows.go:42 | InsertWorkflow | write | inserts a workflow |
| internal/db/workflows.go:338 | DeprecateWorkflow | write | marks a workflow deprecated |
| internal/db/workflows.go:370 | RestoreWorkflow | write | restores a workflow |
| internal/engine/activate.go:416 | Activate | write | activates a run |
| internal/engine/annotate.go:63 | AnnotateStep | write | writes step metadata |
| internal/engine/annotate_integration.go:230 | Engine.AnnotateIntegration | write | records an integration annotation |
| internal/engine/backfill.go:72 | Engine.BackfillVoteUsage | write | backfills vote usage |
| internal/engine/backfill.go:325 | Engine.BackfillUsage | write | backfills step usage |
| internal/engine/claim.go:295 | recordClaimRefused | write | appends a refusal event |
| internal/engine/claim.go:322 | claimStepWithGates | write | comment naming the claim transaction at line 361 |
| internal/engine/claim.go:361 | claimStepWithGates | write | lazy reap and claim |
| internal/engine/claim.go:804 | claimStepWithGates | write | second claim transaction after pre-gates |
| internal/engine/conductor.go:142 | ConductRun | write | mints a conductor token |
| internal/engine/config_change.go:39 | SetConfig | write | writes config and its event |
| internal/engine/dispatch.go:223 | Engine.OpenDispatch | write | inserts a dispatch and its rows |
| internal/engine/dispatch.go:446 | Engine.ExtendDispatch | write | extends a dispatch |
| internal/engine/dispatch.go:2000 | recordCloseRefused | write | appends a refusal event |
| internal/engine/dispatch.go:2042 | Engine.closeDispatch | write | closes a dispatch |
| internal/engine/dispatch.go:2254 | Engine.AbandonDispatchWith | write | abandons a dispatch |
| internal/engine/event.go:874 | RecordTrustEvent | write | appends a trust event |
| internal/engine/event.go:1095 | RecordProjectRegisteredEvent | write | appends a registration event |
| internal/engine/events_prune.go:100 | PruneEvents | write | deletes events |
| internal/engine/guard.go:177 | stopBlockers | read | loads the scheduler, rolled back |
| internal/engine/guard_dispatch.go:86 | unreconciledReason | read | reads dispatch reconciliation, rolled back |
| internal/engine/guard_dispatch.go:360 | applyAcks | write | acknowledges reaps |
| internal/engine/guard_dispatch.go:494 | storedManifestRows | read | reads dispatch rows, rolled back |
| internal/engine/guard_dispatch.go:683 | openReapHold | read | reads open reap acks, rolled back |
| internal/engine/guard_dispatch.go:711 | recordGuardCarveOut | write | appends a carve-out event |
| internal/engine/held.go:958 | Engine.decideMaterializedStep | write | resolves a held step |
| internal/engine/hold.go:58 | HoldStep | write | holds a step |
| internal/engine/human.go:282 | Engine.DecideStepWith | write | records a decision |
| internal/engine/human.go:773 | Engine.resolveStep | write | completes a step |
| internal/engine/human.go:1108 | Engine.FailStep | write | fails a step |
| internal/engine/next.go:113 | Engine.NextSteps | write | lazy reap, then commits |
| internal/engine/note.go:118 | AddRunNote | write | inserts a run note |
| internal/engine/pregate.go:553 | announcePreGate | write | appends a pre-gate event |
| internal/engine/pregate.go:587 | recordPreGateRowsIf | write | inserts pre-gate results |
| internal/engine/pregate_detached.go:224 | detachedPreGateCandidates | read | loads the scheduler, rolled back |
| internal/engine/pregate_detached.go:539 | stepStillWantsDetachedPreGate | read | rereads one step, rolled back |
| internal/engine/reap_ack.go:260 | ForceReapStepWith | write | force-reaps a step |
| internal/engine/refresh_body.go:98 | RefreshIssueBodyInRun | write | refreshes a pinned issue body |
| internal/engine/refresh_files.go:66 | RefreshIssueFilesInRun | write | refreshes pinned issue files |
| internal/engine/repin.go:391 | repinRunOptsIn | write | repins a run |
| internal/engine/run_budget.go:75 | SetRunBudget | write | sets a run budget |
| internal/engine/run_fact.go:139 | RecordRunFact | write | inserts a run fact |
| internal/engine/run_lifecycle.go:103 | MoveRunWith | write | moves a run's status |
| internal/engine/run_lifecycle.go:292 | AbandonIssueInRunWith | write | abandons an issue in a run |
| internal/engine/saga.go:317 | Engine.enterActionSaga | write | enters the action saga |
| internal/engine/saga.go:430 | Engine.stageZero | write | records stage zero |
| internal/engine/saga.go:944 | Engine.runGateStage | write | appends the gate-started event |
| internal/engine/saga.go:1004 | Engine.runGateStage | write | records the gate result |
| internal/engine/saga.go:1028 | Engine.enterRouting | write | enters routing |
| internal/engine/saga.go:1419 | Engine.runRoutingStage | write | applies routing |
| internal/engine/saga_resume.go:70 | Engine.resolveInterruptedGate | write | appends the gate re-run event |
| internal/engine/saga_resume.go:200 | Engine.parkInterruptedGate | write | parks an interrupted gate |
| internal/engine/scope_refresh.go:137 | RefreshIssueScopeInRun | write | refreshes a pinned scope |
| internal/engine/vote.go:474 | recordVoteEvent | write | appends a vote event |
| internal/engine/vote.go:569 | routeVoteStep | write | releases a step triaged behind an untallied proposal |
| internal/engine/vote.go:691 | routeVoteStep | write | routes a vote step on its tally |
| internal/engine/vote_panel.go:100 | recordPanelRunEvents | write | appends panel events |
| internal/engine/waive.go:91 | Engine.WaiveStaleTargets | write | waives stale targets |

Totals: 7 read, 86 write, 93 rows.
