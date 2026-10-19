# Removing database/sql from internal/cli and internal/engine

Status: Proposed — 2026-10-18

At b7feee05, 103 non-test files under `internal/cli` (27) and `internal/engine`
(76) import `database/sql`. This plan removes that import from every one of
them in 41 steps of at most four files each, so the database/sql bans in the
import-boundary gates can land. It changes no code itself.

## Method

Every figure below was measured at b7feee05 with `grep` and `awk` over the
non-test `.go` files of the two packages.

- **Rows:** `grep -rlE '"database/sql"' internal/cli internal/engine --include='*.go' --exclude='*_test.go'`.
- **Handle:** the file calls `getDB(cmd)`, declares a `*sql.DB` or `*sql.Tx`
  parameter or field, opens a write transaction with `Begin()`, or opens a
  read snapshot with `db.BeginReadSnapshot`.
- **Crossing signature:** a top-level function, method or type whose
  signature names a `database/sql` type, referenced from another file (outside
  comments). "Rows" counts calling files that are themselves table rows;
  "other" counts calling files that are not. "Package" marks an engine
  signature that `internal/cli` calls.
- **Raw SQL:** lines calling `.Query(`, `.QueryRow(` or `.Exec(` (or their
  `Context` forms). The rows hold 150 such lines. Line numbers are listed in
  the store-method steps.
- **Null types:** non-comment uses of `sql.Null*` and `sql.ErrNoRows`.

The caller graph, read as "callee file, calling row", has one strongly
connected component of 26 `internal/engine` files: autoregister, claim,
context, diff_repin, dispatch_integration, dispatch, drive, held, human, loop,
next, packet, pregate_detached, pregate_scratch, pregate, ready, reap_ack,
reconcile, render, repin, saga_detached, saga_resume, saga, triage,
verify_pins and vote. The design decision below exists because of that
component.

## Design decision

DKT-4037 records the Store and transaction design: `internal/store/store.go`
(commit 4458bcf) defines `Store` for operations on a pooled connection,
`Begin` for a write `Tx`, and `BeginReadSnapshot` for a `ReadTx` that never
takes the write lock. `Tx` embeds `ReadTx`. No method names a `database/sql`
type, and the SQLite implementation delegates each method to the
`internal/db` helper of the same name. The steps rely on that design as
follows.

**Handle approach: type aliases first, retarget last.** Step 1 adds
`internal/store/legacy.go` with three aliases and two wrappers:

- `LegacyDB = *sql.DB`, `LegacyTx = *sql.Tx` and `LegacyReadTx = *sql.Tx`.
- `StoreOn(LegacyDB) Store` wraps an open pool. Its snapshot reads use the
  same connection that `db.BeginReadSnapshot(conn)` uses today.
- `TxOn(LegacyTx) Tx` wraps an open transaction without owning it. The code
  that began the transaction still commits or rolls it back.

Each step retypes its files' handle parameters, fields and results from
`*sql.DB` and `*sql.Tx` to these aliases. That includes `getDB` in
`internal/cli/root.go` and the `dbKey` and `readerDBKey` context assertions.
An alias is the same type, so no caller changes. Steps can land in any order,
and each stays within four files. A parameter that only reads takes
`LegacyReadTx`; one that writes takes `LegacyTx`. Both are the same type today.
Recording the distinction while the file is open lets the retarget point
them at `ReadTx` and `Tx`.

Three candidates were weighed:

- **Retype `*sql.Tx` to `store.Tx` directly.** Rejected. Inside the
  26-file component, retyping any one file's signatures breaks callers in the
  others, so all 26 would land in one step.
- **Callers first, with an unwrap accessor such as `store.SQLTx(tx)`.**
  Rejected. When the callee later retypes, every caller's call site changes
  again, so the callee's step edits files outside its own Files line.
- **Aliases, then one retarget.** Chosen. It is the only candidate whose
  per-file change leaves callers untouched. The retarget changes three
  declarations in one file.

**Raw-SQL approach: one Store method per query, in its own step.** Each raw
`Query`, `QueryRow` or `Exec` in a row becomes a Store method. It goes on
`ReadTx` for a read inside a transaction, on `Tx` for a write, and on `Store`
for a read on the pool. The method delegates to a new `internal/db` helper of
the same name, following the DKT-4037 convention. Where an existing Store
method already returns the needed data, the row calls that method instead. A
row reaches the method through `store.TxOn(tx)` or `store.StoreOn(conn)`.

Each store-method step adds the helpers, the interface methods, the
`sqliteStore` or `sqliteTx` delegates, and tests. Every store-method step edits
`internal/store/store.go` and `internal/store/sqlite.go`, so they run in
sequence. Each row step depends only on its own store-method step.

**Null and not-found values.** A helper returns plain Go values. A NULL column
becomes a zero value or a pointer, the way the existing helpers already fill
`model` types. A missing row returns an `internal/db` sentinel
(`db.ErrNotFound`, `db.ErrStepNotFound`, `db.ErrRunNotFound`) or a found flag
in the shape of `LookupProject`'s `(int, bool, error)`. It never returns
`sql.ErrNoRows`. Rows switch their `errors.Is` checks to the sentinel. This
removes every `sql.Null*` and `sql.ErrNoRows` use in the table.

**Helpers stay unless all their callers move together.** A row helper whose
body reduces to one Store call stays in place when another file calls it, so
its callers do not change. It is inlined only when every caller is in the same
step. The one signature a step removes is `scanTxRows` in
`internal/engine/run_lifecycle.go`, which takes `*sql.Rows`. Its two callers
share Step 13. Every other row's Callers column is therefore `none`.

## File table

| File | Handle | Crossing signature | Raw SQL | sql null types / ErrNoRows | Callers | Step |
|---|---|---|---|---|---|---|
| internal/cli/clihelpers.go | `*sql.DB` param (callers pass `getDB(cmd)`) | yes: file, 3 rows + 11 other | none | none | none | 27 |
| internal/cli/config_classes.go | `*sql.DB` param | yes: file, 1 other | none | none | none | 27 |
| internal/cli/doc_show.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 28 |
| internal/cli/events_follow.go | `getDB(cmd)`; `followOptions.conn *sql.DB` field | no | none | none | none | 28 |
| internal/cli/export_manifest.go | `*sql.DB` and `*sql.Tx` params | no | 3 | none | none | 23 |
| internal/cli/import_remap.go | `*sql.Tx` param; `importRemap` holds the tx | yes: file, 1 row | 4 | `sql.NullInt64`, `sql.ErrNoRows` | none | 23 |
| internal/cli/import.go | `getDB(cmd)`; `*sql.DB` param; `Begin` x1 | no | none | none | none | 28 |
| internal/cli/issue_claim.go | `getDB(cmd)`; `*sql.DB` param | yes: file, 1 other | none | none | none | 28 |
| internal/cli/issue_close.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 29 |
| internal/cli/issue_delete.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 29 |
| internal/cli/issue_list.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 29 |
| internal/cli/issue_scope.go | `*sql.DB` param | yes: file, 2 other | none | none | none | 29 |
| internal/cli/issue_show.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 30 |
| internal/cli/project_resolve.go | `*sql.DB` param (root.go passes its connection) | yes: file, 1 row | none | none | none | 26 |
| internal/cli/project.go | `getDB(cmd)`; `*sql.DB` param | yes: file, 2 rows + 2 other | none | none | none | 26 |
| internal/cli/registry_fanout.go | `*sql.DB` param | yes: file, 5 rows + 1 other | none | none | none | 26 |
| internal/cli/root.go | opens `db.Open` and `db.OpenReader`; stores them under the `dbKey` and `readerDBKey` context keys; `getDB` returns `*sql.DB` | yes: file, 17 rows + 66 other | none | none | none | 26 |
| internal/cli/run_issue.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 30 |
| internal/cli/schema_deprecate.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 31 |
| internal/cli/schema_register.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 31 |
| internal/cli/step.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 30 |
| internal/cli/token.go | `*sql.DB` param | yes: file, 1 row + 9 other | none | none | none | 27 |
| internal/cli/trust.go | opens its own `db.Open(cfg.DBPath)` | no | none | `sql.ErrNoRows` (in `trustCmdError`) | none | 27 |
| internal/cli/vote_list.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 30 |
| internal/cli/workflow_deprecate.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 31 |
| internal/cli/workflow_lint.go | `getDB(cmd)`; `*sql.DB` param | no | none | none | none | 31 |
| internal/cli/workflow_register.go | `getDB(cmd)`; `*sql.DB` param; `schemaResolver` and `voteRuleResolver` types | yes: file, 1 row | none | none | none | 32 |
| internal/engine/activate.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 1 row + 1 other; package | 4 | `sql.NullString`, `sql.NullInt64` | none | 19 |
| internal/engine/after_fired.go | `*sql.Tx` param | yes: file, 3 rows | none | none | none | 33 |
| internal/engine/annotate_integration.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 1 row; package | none | none | none | 33 |
| internal/engine/annotate.go | `*sql.DB` param; `Begin` x1 | yes: file, 1 row; package | none | none | none | 33 |
| internal/engine/artifacts_read.go | `*sql.DB` param | yes: file, 1 row; package | 1 | `sql.NullInt64`, `sql.NullString`, `sql.ErrNoRows` | none | 11 |
| internal/engine/autoregister.go | `*sql.DB` and `*sql.Tx` params; tx-backed resolver types | yes: file, 3 rows | none | none | none | 33 |
| internal/engine/backfill.go | `*sql.DB` param; `Begin` x2 | yes: file, 1 row + 2 other; package | 2 | `sql.ErrNoRows` | none | 21 |
| internal/engine/batch_override.go | `*sql.DB` param | yes: file, 1 row | none | none | none | 34 |
| internal/engine/budget.go | `*sql.Tx` param | yes: file, 5 rows | 1 | none | none | 9 |
| internal/engine/claim.go | `*sql.DB` and `*sql.Tx` params; `Begin` x4; `BeginReadSnapshot` x2 | yes: file, 4 rows + 1 other; package | 5 | `sql.NullString` | none | 15 |
| internal/engine/conductor.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 15 rows + 2 other; package | none | none | none | 34 |
| internal/engine/config_change.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 1 other; package | none | none | none | 34 |
| internal/engine/context.go | `*sql.DB` and `*sql.Tx` params; `rowQuerier` interface over both | yes: file, 4 rows | 3 | `sql.ErrNoRows`, `sql.NullString` | none | 15 |
| internal/engine/definitions.go | `*sql.DB` and `*sql.Tx` params | yes: file, 26 rows | 3 | none (`*sql.Rows` in `scanStepDefinitions`) | none | 19 |
| internal/engine/diff_repin.go | `*sql.DB` and `*sql.Tx` params | yes: file, 2 rows | 1 | none | none | 19 |
| internal/engine/dispatch_chain.go | `*sql.Tx` param | yes: file, 2 rows | 3 | `sql.ErrNoRows` | none | 7 |
| internal/engine/dispatch_integration.go | `*sql.DB` and `*sql.Tx` params; `BeginReadSnapshot` x1 | yes: file, 1 row | 2 | none | none | 17 |
| internal/engine/dispatch_reconcile.go | `*sql.DB` param | yes: file, 1 other; package | none | none | none | 34 |
| internal/engine/dispatch.go | `*sql.DB` and `*sql.Tx` params; `Begin` x5; `BeginReadSnapshot` x3 | yes: file, 7 rows + 1 other; package | 1 | `sql.NullInt64` | none | 17 |
| internal/engine/doctor.go | `*sql.DB` param | yes: file, 1 other; package | none | none | none | 35 |
| internal/engine/drive.go | `*sql.DB` param | yes: file, 3 rows + 1 other; package | 1 | none | none | 25 |
| internal/engine/event.go | `*sql.DB` and `*sql.Tx` params; `Begin` x2 | yes: file, 38 rows; package | 3 | `sql.ErrNoRows`, `sql.NullInt64` | none | 3 |
| internal/engine/events_prune.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 1 other; package | 5 | none | none | 3 |
| internal/engine/events_read.go | `*sql.DB` and `*sql.Tx` params; `BeginReadSnapshot` x3 | yes: file, 3 rows + 3 other; package | 5 | `sql.NullInt64`, `sql.NullString`, `sql.ErrNoRows` | none | 3 |
| internal/engine/evidence.go | `*sql.DB` param | yes: file, 1 other; package | 2 | `sql.ErrNoRows` | none | 11 |
| internal/engine/executor_ledger.go | `*sql.DB` param | yes: file, 1 other; package | 1 | none | none | 11 |
| internal/engine/fence_report.go | `*sql.DB` and `*sql.Tx` params | yes: file, 1 row | 2 | none | none | 21 |
| internal/engine/gate_preflight.go | `*sql.DB` and `*sql.Tx` params | yes: file, 1 row | none | none | none | 35 |
| internal/engine/gate_script_pins.go | `*sql.DB` param | yes: file, 1 row | none | none | none | 35 |
| internal/engine/gate_status.go | `*sql.DB` param | yes: file, 1 other; package | none | none | none | 35 |
| internal/engine/guard_dispatch.go | `*sql.DB` param; `Begin` x2; `BeginReadSnapshot` x3 | yes: file, 1 row + 1 other; package | 1 | none | none | 17 |
| internal/engine/guard.go | `*sql.DB` and `*sql.Tx` params; `BeginReadSnapshot` x1 | yes: file, 1 other; package | 3 | `sql.ErrNoRows`, `sql.NullString` | none | 17 |
| internal/engine/held.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1; `BeginReadSnapshot` x1 | yes: file, 13 rows; package | 5 | `sql.NullString`, `sql.ErrNoRows` | none | 19 |
| internal/engine/hold.go | `*sql.DB` param; `Begin` x1 | yes: file, 1 row; package | none | none | none | 36 |
| internal/engine/human.go | `*sql.DB` and `*sql.Tx` params; `Begin` x3 | yes: file, 10 rows; package | none | none | none | 36 |
| internal/engine/linked.go | `*sql.Tx` param | yes: file, 2 rows | 3 | `sql.ErrNoRows`, `sql.NullString`, `sql.NullInt64` | none | 5 |
| internal/engine/loop.go | `*sql.Tx` param | yes: file, 6 rows | 11 | `sql.NullString`, `sql.ErrNoRows` | none | 7 |
| internal/engine/next.go | `*sql.DB` param; `Begin` x1; `BeginReadSnapshot` x2 | yes: file, 5 rows + 2 other; package | 1 | none | none | 25 |
| internal/engine/note.go | `*sql.DB` param; `Begin` x1 | yes: file, 1 other; package | none | none | none | 36 |
| internal/engine/order.go | `*sql.DB` param | yes: file, 1 row | none | none | none | 36 |
| internal/engine/packet.go | `*sql.DB` param | yes: file, 2 rows + 1 other; package | 1 | `sql.NullString` | none | 15 |
| internal/engine/payload_validation.go | `*sql.DB` param | yes: file, 4 rows | none | none | none | 37 |
| internal/engine/pending_closure.go | `*sql.DB` param | yes: file, 2 rows | none | none | none | 37 |
| internal/engine/pin_staleness.go | `*sql.DB` and `*sql.Tx` params | yes: file, 1 row | 2 | none (`*sql.Rows` through `scanTxRows`) | none | 13 |
| internal/engine/policy_pin.go | `*sql.DB` param | yes: file, 1 other; package | none | none | none | 37 |
| internal/engine/pregate_detached.go | `*sql.DB` and `*sql.Tx` params; `BeginReadSnapshot` x2 | yes: file, 7 rows + 1 other; package | none | none | none | 37 |
| internal/engine/pregate_scratch.go | `*sql.DB` param | yes: file, 2 rows | none | none | none | 38 |
| internal/engine/pregate.go | `*sql.DB` and `*sql.Tx` params; `Begin` x2; `preGateMeasurement` type | yes: file, 3 rows | none | none | none | 38 |
| internal/engine/ready.go | `*sql.Tx` param | yes: file, 13 rows | 4 | `sql.NullString` | none | 5 |
| internal/engine/reap_ack.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 5 rows; package | 2 | none | none | 21 |
| internal/engine/reconcile.go | `*sql.Tx` param | yes: file, 8 rows | 14 | `sql.NullString` | none | 7 |
| internal/engine/refresh_body.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 1 other; package | 1 | `sql.NullString`, `sql.ErrNoRows` | none | 15 |
| internal/engine/refresh_files.go | `*sql.DB` param; `Begin` x1 | yes: file, 1 other; package | none | none | none | 38 |
| internal/engine/registry_audit.go | `*sql.DB` param | yes: file, 1 other; package | none | none | none | 38 |
| internal/engine/render.go | `*sql.DB` param | yes: file, 2 rows; package | none | none | none | 39 |
| internal/engine/repin.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 1 row + 3 other; package | 4 | none | none | 13 |
| internal/engine/report.go | `*sql.DB` and `*sql.Tx` params; `BeginReadSnapshot` x1 | yes: file, 3 rows + 1 other; package | 9 | `sql.NullString`, `sql.NullInt64` (`*sql.Rows` in `scanAbandonRulings`) | none | 11 |
| internal/engine/run_budget.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1; `BeginReadSnapshot` x1 | yes: file, 1 other; package | none | none | none | 39 |
| internal/engine/run_fact.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 1 other; package | 2 | `sql.ErrNoRows` | none | 3 |
| internal/engine/run_lifecycle.go | `*sql.DB` and `*sql.Tx` params; `Begin` x2; `scanTxRows` takes `*sql.Rows` | yes: file, 2 rows + 1 other; package | 4 | none | internal/engine/pin_staleness.go, internal/engine/scope_refresh.go | 13 |
| internal/engine/saga_detached.go | `*sql.DB` param | yes: file, 2 rows + 1 other; package | 1 | none | none | 9 |
| internal/engine/saga_resume.go | `*sql.DB` param; `Begin` x2 | yes: file, 1 row | 1 | none | none | 9 |
| internal/engine/saga.go | `*sql.DB` and `*sql.Tx` params; `Begin` x6; `BeginReadSnapshot` x1; `Engine.beforeCloseCAS` hook takes `*sql.Tx` | yes: file, 21 rows; package | 14 | `sql.ErrNoRows`, `sql.NullInt64`, `sql.NullString` | none | 9 |
| internal/engine/schema_pins.go | `*sql.Tx` param | yes: file, 1 row | none | none | none | 39 |
| internal/engine/schema_references.go | `*sql.DB` param | yes: file, 1 row; package | none | none | none | 39 |
| internal/engine/scope_refresh.go | `*sql.DB` and `*sql.Tx` params; `Begin` x1 | yes: file, 2 rows + 1 other; package | 1 | none (`*sql.Rows` through `scanTxRows`) | none | 13 |
| internal/engine/scope_snapshot.go | `*sql.DB` param | yes: file, 1 other; package | 3 | `sql.NullString` | none | 5 |
| internal/engine/store.go | `*sql.Tx` param | yes: file, 5 rows | 8 | `sql.NullString`, `sql.ErrNoRows` | none | 5 |
| internal/engine/strand.go | `*sql.Tx` param | yes: file, 3 rows | none | none | none | 40 |
| internal/engine/triage.go | `*sql.DB` and `*sql.Tx` params | yes: file, 3 rows | 2 | `sql.ErrNoRows` | none | 7 |
| internal/engine/verify_pins.go | `*sql.DB` param | yes: file, 2 rows + 1 other; package | none | none | none | 40 |
| internal/engine/vote_cast.go | `*sql.DB` param | yes: file, 1 other; package | none | none | none | 40 |
| internal/engine/vote_panel.go | `*sql.DB` param; `Begin` x1 | yes: file, 1 row + 1 other; package | 1 | none | none | 21 |
| internal/engine/vote_record.go | `*sql.DB` and `*sql.Tx` params | yes: file, 2 rows | none | none | none | 40 |
| internal/engine/vote.go | `*sql.DB` and `*sql.Tx` params; `Begin` x3 | yes: file, 4 rows + 2 other; package | none | none | none | 41 |
| internal/engine/waive.go | `*sql.DB` param; `Begin` x1 | yes: file, 1 other; package | none | none | none | 41 |

## Step 1: Transitional handle aliases in internal/store

Adds the aliases and wrappers the design decision describes. No row changes.

### Draft

- Title: feat(store): add transitional handle aliases for the Store move
- Files: internal/store/legacy.go, internal/store/legacy_test.go
- Depends on: none (the Store interface is on HEAD at 4458bcf)
- AC: `LegacyDB`, `LegacyTx` and `LegacyReadTx` are aliases (`=`) of `*sql.DB`, `*sql.Tx` and `*sql.Tx`. Verify: `vorpal run go:1.26.0 test ./internal/store/ -run TestLegacy -count=1 -v` passes; `legacy_test.go` calls `Commit` and `Rollback` on `LegacyTx` values and assigns a `*sql.Tx` to a `LegacyReadTx`. Mutant: dropping the `=` from the `LegacyTx` declaration removes its methods and fails that build.
- AC: `TxOn(tx)` runs its methods inside the caller's transaction and does not end it. Verify: `TestLegacyTxOnSharesTransaction` writes through `TxOn(tx)`, rolls back `tx`, and asserts the row is absent; a second case commits and asserts it is present. Mutant: making `TxOn` begin its own transaction fails the rollback case.
- AC: `StoreOn(conn)` serves pool reads on `conn`. Verify: `TestLegacyStoreOnReadsPool` creates an issue through `db` helpers on `conn` and reads it back through `StoreOn(conn).GetIssue`. Mutant: returning a store on a fresh `db.Open` of another file fails the read.

## Step 2: Store methods for the event queries

Query sites: internal/engine/event.go:495,524,564; internal/engine/events_read.go:168,207,369,656,791; internal/engine/events_prune.go:139,155,264,307,331; internal/engine/run_fact.go:165,184.

### Draft

- Title: feat(store): add Store methods for event log queries
- Files: internal/db/events.go, internal/db/events_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 1
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/events.go`. Verify: `bash scripts/qa/build.sh` exits 0; the `_ Store` and `_ Tx` assertions in `internal/store/sqlite.go` compile only when every interface method has a delegate. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestEventQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestEventQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestEventQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified; every `sql.ErrNoRows` in the added code is translated to an `internal/db` sentinel or a found flag before return. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 3: Move event queries in four engine files

### Draft

- Title: refactor(engine): move event queries onto the Store
- Files: internal/engine/event.go, internal/engine/events_read.go, internal/engine/events_prune.go, internal/engine/run_fact.go
- Depends on: Step 2
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/event.go internal/engine/events_read.go internal/engine/events_prune.go internal/engine/run_fact.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/event.go internal/engine/events_read.go internal/engine/events_prune.go internal/engine/run_fact.go` prints nothing. Mutant: leaving internal/engine/event.go:495 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 4: Store methods for the issue and run-issue queries

Query sites: internal/engine/store.go:59,80,128,156,175,201,217,324; internal/engine/ready.go:305,404,489,522; internal/engine/linked.go:156,197,255; internal/engine/scope_snapshot.go:137,188,247.

### Draft

- Title: feat(store): add Store methods for engine issue queries
- Files: internal/db/issues.go, internal/db/issues_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 2
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/issues.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestIssueQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestIssueQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestIssueQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 5: Move issue queries in four engine files

### Draft

- Title: refactor(engine): move issue queries onto the Store
- Files: internal/engine/store.go, internal/engine/ready.go, internal/engine/linked.go, internal/engine/scope_snapshot.go
- Depends on: Step 4
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/store.go internal/engine/ready.go internal/engine/linked.go internal/engine/scope_snapshot.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/store.go internal/engine/ready.go internal/engine/linked.go internal/engine/scope_snapshot.go` prints nothing. Mutant: leaving internal/engine/store.go:59 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 6: Store methods for the routing and loop queries

Query sites: internal/engine/reconcile.go:176,221,281,356,382,429,466,482,528,597,622,688,710,723; internal/engine/loop.go:197,291,410,482,1035,1088,1319,1446,1475,1639,1660; internal/engine/triage.go:50,129; internal/engine/dispatch_chain.go:91,127,157.

### Draft

- Title: feat(store): add Store methods for routing and loop queries
- Files: internal/db/steps.go, internal/db/steps_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 4
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/steps.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestRoutingQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestRoutingQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestRoutingQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 7: Move routing and loop queries in four engine files

### Draft

- Title: refactor(engine): move routing and loop queries onto the Store
- Files: internal/engine/reconcile.go, internal/engine/loop.go, internal/engine/triage.go, internal/engine/dispatch_chain.go
- Depends on: Step 6
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/reconcile.go internal/engine/loop.go internal/engine/triage.go internal/engine/dispatch_chain.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/reconcile.go internal/engine/loop.go internal/engine/triage.go internal/engine/dispatch_chain.go` prints nothing. Mutant: leaving internal/engine/reconcile.go:176 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 8: Store methods for the saga queries

Query sites: internal/engine/saga.go:465,724,735,2217,2258,2562,3654,3692,3732,3778,3939,4071,4104,4134; internal/engine/saga_resume.go:32; internal/engine/saga_detached.go:144; internal/engine/budget.go:335.

### Draft

- Title: feat(store): add Store methods for saga queries
- Files: internal/db/artifacts.go, internal/db/artifacts_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 6
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/artifacts.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestSagaQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestSagaQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestSagaQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 9: Move saga queries in four engine files

### Draft

- Title: refactor(engine): move saga queries onto the Store
- Files: internal/engine/saga.go, internal/engine/saga_resume.go, internal/engine/saga_detached.go, internal/engine/budget.go
- Depends on: Step 8
- AC: No listed file imports database/sql; the `Engine.beforeCloseCAS` hook takes a `store.LegacyTx`. Verify: `grep -lE '"database/sql"' internal/engine/saga.go internal/engine/saga_resume.go internal/engine/saga_detached.go internal/engine/budget.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/saga.go internal/engine/saga_resume.go internal/engine/saga_detached.go internal/engine/budget.go` prints nothing. Mutant: leaving internal/engine/saga.go:465 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 10: Store methods for the report and ledger queries

Query sites: internal/engine/report.go:962,1091,1455,1490,1784,1833,1876,2035,2044; internal/engine/executor_ledger.go:280; internal/engine/evidence.go:147,164; internal/engine/artifacts_read.go:121.

### Draft

- Title: feat(store): add Store methods for report and ledger queries
- Files: internal/db/rollups.go, internal/db/rollups_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 8
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/rollups.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestReportQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestReportQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestReportQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 11: Move report and ledger queries in four engine files

### Draft

- Title: refactor(engine): move report and ledger queries onto the Store
- Files: internal/engine/report.go, internal/engine/executor_ledger.go, internal/engine/evidence.go, internal/engine/artifacts_read.go
- Depends on: Step 10
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/report.go internal/engine/executor_ledger.go internal/engine/evidence.go internal/engine/artifacts_read.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/report.go internal/engine/executor_ledger.go internal/engine/evidence.go internal/engine/artifacts_read.go` prints nothing. Mutant: leaving internal/engine/report.go:962 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 12: Store methods for the run-lifecycle and repin queries

Query sites: internal/engine/run_lifecycle.go:234,316,326,352; internal/engine/pin_staleness.go:205,315; internal/engine/scope_refresh.go:306; internal/engine/repin.go:419,458,502,698.

### Draft

- Title: feat(store): add Store methods for run lifecycle and repin queries
- Files: internal/db/runs.go, internal/db/runs_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 10
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/runs.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestRunLifecycleQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestRunLifecycleQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestRunLifecycleQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 13: Move run-lifecycle and repin queries in four engine files

This step removes `scanTxRows` from `internal/engine/run_lifecycle.go`. Both of
its callers are on this step's Files line.

### Draft

- Title: refactor(engine): move run lifecycle and repin queries onto the Store
- Files: internal/engine/run_lifecycle.go, internal/engine/pin_staleness.go, internal/engine/scope_refresh.go, internal/engine/repin.go
- Depends on: Step 12
- AC: No listed file imports database/sql, and `scanTxRows` is gone. Verify: `grep -lE '"database/sql"' internal/engine/run_lifecycle.go internal/engine/pin_staleness.go internal/engine/scope_refresh.go internal/engine/repin.go` prints nothing and `grep -rn 'scanTxRows' internal/engine` prints nothing. Mutant: keeping `scanTxRows` with its `*sql.Rows` parameter prints the import line's file.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/run_lifecycle.go internal/engine/pin_staleness.go internal/engine/scope_refresh.go internal/engine/repin.go` prints nothing. Mutant: leaving internal/engine/run_lifecycle.go:234 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 14: Store methods for the claim and context queries

Query sites: internal/engine/claim.go:719,767,873,927,965; internal/engine/context.go:381,469,762; internal/engine/packet.go:663; internal/engine/refresh_body.go:248.

### Draft

- Title: feat(store): add Store methods for claim and context queries
- Files: internal/db/artifacts.go, internal/db/artifacts_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 12
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/artifacts.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestContextQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestContextQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestContextQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 15: Move claim and context queries in four engine files

### Draft

- Title: refactor(engine): move claim and context queries onto the Store
- Files: internal/engine/claim.go, internal/engine/context.go, internal/engine/packet.go, internal/engine/refresh_body.go
- Depends on: Step 14
- AC: No listed file imports database/sql, and `rowQuerier` is gone from `context.go`. Verify: `grep -lE '"database/sql"' internal/engine/claim.go internal/engine/context.go internal/engine/packet.go internal/engine/refresh_body.go` prints nothing. Mutant: keeping `rowQuerier` with its `*sql.Row` result prints `internal/engine/context.go`.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/claim.go internal/engine/context.go internal/engine/packet.go internal/engine/refresh_body.go` prints nothing. Mutant: leaving internal/engine/claim.go:719 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 16: Store methods for the dispatch and guard queries

Query sites: internal/engine/dispatch.go:812; internal/engine/dispatch_integration.go:195,221; internal/engine/guard.go:123,237,375; internal/engine/guard_dispatch.go:149.

### Draft

- Title: feat(store): add Store methods for dispatch and guard queries
- Files: internal/db/dispatch.go, internal/db/dispatch_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 14
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/dispatch.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestDispatchQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestDispatchQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestDispatchQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 17: Move dispatch and guard queries in four engine files

### Draft

- Title: refactor(engine): move dispatch and guard queries onto the Store
- Files: internal/engine/dispatch.go, internal/engine/dispatch_integration.go, internal/engine/guard.go, internal/engine/guard_dispatch.go
- Depends on: Step 16
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/dispatch.go internal/engine/dispatch_integration.go internal/engine/guard.go internal/engine/guard_dispatch.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/dispatch.go internal/engine/dispatch_integration.go internal/engine/guard.go internal/engine/guard_dispatch.go` prints nothing. Mutant: leaving internal/engine/guard.go:123 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 18: Store methods for the hold and activation queries

Query sites: internal/engine/held.go:433,509,545,695,889; internal/engine/activate.go:875,982,1120,1220; internal/engine/definitions.go:26,53,127; internal/engine/diff_repin.go:187.

### Draft

- Title: feat(store): add Store methods for hold and activation queries
- Files: internal/db/steps.go, internal/db/steps_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 16
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/steps.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestActivationQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestActivationQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestActivationQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 19: Move hold and activation queries in four engine files

### Draft

- Title: refactor(engine): move hold and activation queries onto the Store
- Files: internal/engine/held.go, internal/engine/activate.go, internal/engine/definitions.go, internal/engine/diff_repin.go
- Depends on: Step 18
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/held.go internal/engine/activate.go internal/engine/definitions.go internal/engine/diff_repin.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/held.go internal/engine/activate.go internal/engine/definitions.go internal/engine/diff_repin.go` prints nothing. Mutant: leaving internal/engine/held.go:433 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 20: Store methods for the reap, usage, fence and panel queries

Query sites: internal/engine/reap_ack.go:515,527; internal/engine/backfill.go:79,103; internal/engine/fence_report.go:39,194; internal/engine/vote_panel.go:107.

### Draft

- Title: feat(store): add Store methods for reap, usage and fence queries
- Files: internal/db/runs.go, internal/db/runs_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 18
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/runs.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestRunSideQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestRunSideQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestRunSideQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 21: Move reap, usage, fence and panel queries in four engine files

### Draft

- Title: refactor(engine): move reap, usage and fence queries onto the Store
- Files: internal/engine/reap_ack.go, internal/engine/backfill.go, internal/engine/fence_report.go, internal/engine/vote_panel.go
- Depends on: Step 20
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/reap_ack.go internal/engine/backfill.go internal/engine/fence_report.go internal/engine/vote_panel.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/reap_ack.go internal/engine/backfill.go internal/engine/fence_report.go internal/engine/vote_panel.go` prints nothing. Mutant: leaving internal/engine/reap_ack.go:515 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 22: Store methods for the export and import queries

Query sites: internal/cli/export_manifest.go:298,335,341; internal/cli/import_remap.go:218,233,241,268.

### Draft

- Title: feat(store): add Store methods for export and import queries
- Files: internal/db/issues.go, internal/db/issues_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 20
- AC: Each listed query site has a Store, ReadTx or Tx method that returns its result, delegating to a helper of the same name in `internal/db/issues.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestImportExportQueries*` test that seeds rows and asserts the returned values, including the missing-row result. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestImportExportQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestImportExportQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 23: Move export and import queries in two CLI files

### Draft

- Title: refactor(cli): move export and import queries onto the Store
- Files: internal/cli/export_manifest.go, internal/cli/import_remap.go
- Depends on: Step 22
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/cli/export_manifest.go internal/cli/import_remap.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in either file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/cli/export_manifest.go internal/cli/import_remap.go` prints nothing. Mutant: leaving internal/cli/import_remap.go:268 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the two files. Mutant: retyping one parameter to `store.Tx` forces an edit in `internal/cli/import.go`, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 24: Store methods for the drive and step-list queries

Query sites: internal/engine/drive.go:109; internal/engine/next.go:962.

### Draft

- Title: feat(store): add Store methods for drive and step list queries
- Files: internal/db/steps.go, internal/db/steps_test.go, internal/store/store.go, internal/store/sqlite.go
- Depends on: Step 22
- AC: Each listed query site has a Store method that returns its result, delegating to a helper of the same name in `internal/db/steps.go`. Verify: `bash scripts/qa/build.sh` exits 0. Mutant: deleting one added delegate fails the build at `internal/store/sqlite.go`.
- AC: Each added helper has a `TestStepListQueries*` test that seeds rows and asserts the returned values. Verify: `vorpal run go:1.26.0 test ./internal/db/ -run TestStepListQueries -count=1 -v` passes with at least one `--- PASS` per helper. Mutant: changing one helper's WHERE clause to match a different row fails a `TestStepListQueries` case.
- AC: No added helper returns `sql.ErrNoRows` or a `sql.Null*` value. Verify: read-verified as in Step 2. Mutant: returning the raw error from one helper fails its missing-row test case.

## Step 25: Move drive and step-list queries in two engine files

### Draft

- Title: refactor(engine): move drive and step list queries onto the Store
- Files: internal/engine/drive.go, internal/engine/next.go
- Depends on: Step 24
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/drive.go internal/engine/next.go` prints nothing. Mutant: restoring the import with one `*sql.DB` parameter in either file prints that path.
- AC: No raw query remains in the listed files. Verify: `grep -nE '\.(Query|QueryRow|Exec)(Context)?\(' internal/engine/drive.go internal/engine/next.go` prints nothing. Mutant: leaving internal/engine/next.go:962 unmigrated prints its line.
- AC: Handle types are retyped to the store aliases only, so no file outside the list changes. Verify: `git diff --name-only <step base>...HEAD` lists only the two files. Mutant: retyping one parameter to `store.Store` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: replacing one migrated call's result with its zero value makes `bash scripts/qa/tests.sh` fail; the change-summary names the test that failed.

## Step 26: Retype the CLI root handle and project resolution

`getDB` returns `store.LegacyDB`. The `dbKey` and `readerDBKey` assertions in
`PersistentPostRunE` assert `store.LegacyDB`. `root.go` still opens
`db.Open` and `db.OpenReader`; the retarget after the last step replaces them.

### Draft

- Title: refactor(cli): retype the root database handle to the store alias
- Files: internal/cli/root.go, internal/cli/project_resolve.go, internal/cli/project.go, internal/cli/registry_fanout.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/cli/root.go internal/cli/project_resolve.go internal/cli/project.go internal/cli/registry_fanout.go` prints nothing. Mutant: restoring `func getDB(cmd *cobra.Command) *sql.DB` prints `internal/cli/root.go`.
- AC: The retype changes no caller: the 66 other CLI files that call `getDB` are untouched. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: making `getDB` return `store.Store` forces edits in its callers, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: asserting the context value as a different type in `PersistentPostRunE` leaves the connection open, which a CLI test that reopens the store detects; the change-summary names that test.

## Step 27: Retype handles in CLI helpers, tokens and trust

`trust.go` checks `sql.ErrNoRows` in `trustCmdError`. Its source is
unverified at b7feee05: no package outside `internal/db`, `internal/store`,
`internal/cli` and `internal/engine` imports `database/sql`. The step traces
the error and switches the check to the `internal/db` sentinel that reaches
it, or deletes the case if nothing returns it.

### Draft

- Title: refactor(cli): retype handles in shared CLI helpers
- Files: internal/cli/clihelpers.go, internal/cli/token.go, internal/cli/trust.go, internal/cli/config_classes.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/cli/clihelpers.go internal/cli/token.go internal/cli/trust.go internal/cli/config_classes.go` prints nothing. Mutant: restoring the `sql.ErrNoRows` case in `trustCmdError` prints `internal/cli/trust.go`.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one helper parameter to `store.Store` forces edits in its callers, which that command then lists.
- AC: A not-found trust failure still maps to NOT_FOUND. Verify: `bash scripts/qa/tests.sh` exits 0, and the change-summary names the test that covers `trustCmdError`'s not-found case (or the evidence that no path returns it). Mutant: mapping the replacement sentinel to VALIDATION fails that test.

## Step 28: Retype handles in four CLI commands

### Draft

- Title: refactor(cli): retype handles in doc, events, import and claim commands
- Files: internal/cli/doc_show.go, internal/cli/events_follow.go, internal/cli/import.go, internal/cli/issue_claim.go
- Depends on: Step 1
- AC: No listed file imports database/sql; `followOptions.conn` is a `store.LegacyDB`. Verify: `grep -lE '"database/sql"' internal/cli/doc_show.go internal/cli/events_follow.go internal/cli/import.go internal/cli/issue_claim.go` prints nothing. Mutant: restoring `conn *sql.DB` in `followOptions` prints `internal/cli/events_follow.go`.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Store` forces edits in its callers, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: dropping the `Rollback` on the import transaction's error path fails an import test; the change-summary names it.

## Step 29: Retype handles in four issue commands

### Draft

- Title: refactor(cli): retype handles in issue close, delete, list and scope
- Files: internal/cli/issue_close.go, internal/cli/issue_delete.go, internal/cli/issue_list.go, internal/cli/issue_scope.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/cli/issue_close.go internal/cli/issue_delete.go internal/cli/issue_list.go internal/cli/issue_scope.go` prints nothing. Mutant: restoring the import with one `*sql.DB` parameter in any listed file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping `applyScope`'s parameter to `store.Store` forces edits in its two callers, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: swapping `doCascadeDelete` and `doOrphanDelete` at their call site fails an issue delete test; the change-summary names it.

## Step 30: Retype handles in four show and step commands

### Draft

- Title: refactor(cli): retype handles in show, run issue, step and vote list
- Files: internal/cli/issue_show.go, internal/cli/run_issue.go, internal/cli/step.go, internal/cli/vote_list.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/cli/issue_show.go internal/cli/run_issue.go internal/cli/step.go internal/cli/vote_list.go` prints nothing. Mutant: restoring the import with one `*sql.DB` parameter in any listed file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Store` forces edits in its callers, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: passing a different step id to `emitStepState` fails a step command test; the change-summary names it.

## Step 31: Retype handles in four registry commands

### Draft

- Title: refactor(cli): retype handles in schema and workflow registry commands
- Files: internal/cli/schema_deprecate.go, internal/cli/schema_register.go, internal/cli/workflow_deprecate.go, internal/cli/workflow_lint.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/cli/schema_deprecate.go internal/cli/schema_register.go internal/cli/workflow_deprecate.go internal/cli/workflow_lint.go` prints nothing. Mutant: restoring the import with one `*sql.DB` parameter in any listed file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Store` forces edits in its callers, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: skipping one project in `deprecateSchemaAcrossProjects` fails a fan-out test; the change-summary names it.

## Step 32: Retype handles in workflow registration

### Draft

- Title: refactor(cli): retype handles in workflow registration
- Files: internal/cli/workflow_register.go
- Depends on: Step 1
- AC: The file no longer imports database/sql; `schemaResolver` and `voteRuleResolver` hold a `store.LegacyDB`. Verify: `grep -lE '"database/sql"' internal/cli/workflow_register.go` prints nothing. Mutant: restoring a `*sql.DB` field in `schemaResolver` prints the path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the file. Mutant: retyping `validateWorkflowEnvironment`'s parameter to `store.Store` forces an edit in its caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: making `schemaResolver` resolve against the wrong project fails a workflow register test; the change-summary names it.

## Step 33: Retype handles in four engine annotation and registration files

### Draft

- Title: refactor(engine): retype handles in annotate and autoregister
- Files: internal/engine/after_fired.go, internal/engine/annotate_integration.go, internal/engine/annotate.go, internal/engine/autoregister.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/after_fired.go internal/engine/annotate_integration.go internal/engine/annotate.go internal/engine/autoregister.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: committing `AnnotateStep`'s transaction before its write fails an annotate test; the change-summary names it.

## Step 34: Retype handles in four engine conductor and config files

### Draft

- Title: refactor(engine): retype handles in conductor and config change
- Files: internal/engine/batch_override.go, internal/engine/conductor.go, internal/engine/config_change.go, internal/engine/dispatch_reconcile.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/batch_override.go internal/engine/conductor.go internal/engine/config_change.go internal/engine/dispatch_reconcile.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: The retype changes no caller. `conductor.go` has 15 calling rows, so this AC carries the ordering. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping `authorizeConductorTx` to take `store.Tx` forces edits in its callers, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: skipping the hash check in `authorizeConductorTx` fails a conductor authorization test; the change-summary names it.

## Step 35: Retype handles in four engine doctor and gate files

### Draft

- Title: refactor(engine): retype handles in doctor and gate status
- Files: internal/engine/doctor.go, internal/engine/gate_preflight.go, internal/engine/gate_script_pins.go, internal/engine/gate_status.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/doctor.go internal/engine/gate_preflight.go internal/engine/gate_script_pins.go internal/engine/gate_status.go` prints nothing. Mutant: restoring the import with one `*sql.DB` parameter in any listed file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Store` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: returning no findings from `checkDoctorPins` fails a doctor test; the change-summary names it.

## Step 36: Retype handles in four engine human-decision files

### Draft

- Title: refactor(engine): retype handles in hold, human, note and order
- Files: internal/engine/hold.go, internal/engine/human.go, internal/engine/note.go, internal/engine/order.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/hold.go internal/engine/human.go internal/engine/note.go internal/engine/order.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: The retype changes no caller. `human.go` has 10 calling rows. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping `commit` to take `store.Tx` forces edits in its callers, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: dropping the rollback on `FailStep`'s error path fails a human decision test; the change-summary names it.

## Step 37: Retype handles in four engine validation and pre-gate files

### Draft

- Title: refactor(engine): retype handles in payload validation and pre-gates
- Files: internal/engine/payload_validation.go, internal/engine/pending_closure.go, internal/engine/policy_pin.go, internal/engine/pregate_detached.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/payload_validation.go internal/engine/pending_closure.go internal/engine/policy_pin.go internal/engine/pregate_detached.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: making `validateDeclaredPayload` accept every payload fails a payload validation test; the change-summary names it.

## Step 38: Retype handles in four engine pre-gate and audit files

### Draft

- Title: refactor(engine): retype handles in pre-gate and registry audit
- Files: internal/engine/pregate_scratch.go, internal/engine/pregate.go, internal/engine/refresh_files.go, internal/engine/registry_audit.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/pregate_scratch.go internal/engine/pregate.go internal/engine/refresh_files.go internal/engine/registry_audit.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: skipping `recordPreGateRows` in `runPreGates` fails a pre-gate test; the change-summary names it.

## Step 39: Retype handles in four engine render and pin files

### Draft

- Title: refactor(engine): retype handles in render, budget and schema pins
- Files: internal/engine/render.go, internal/engine/run_budget.go, internal/engine/schema_pins.go, internal/engine/schema_references.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/render.go internal/engine/run_budget.go internal/engine/schema_pins.go internal/engine/schema_references.go` prints nothing. Mutant: restoring the import with one `*sql.DB` parameter in any listed file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Store` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: ignoring the version precondition in `SetRunBudget` fails a run budget test; the change-summary names it.

## Step 40: Retype handles in four engine vote and pin files

### Draft

- Title: refactor(engine): retype handles in strand, pins and vote records
- Files: internal/engine/strand.go, internal/engine/verify_pins.go, internal/engine/vote_cast.go, internal/engine/vote_record.go
- Depends on: Step 1
- AC: No listed file imports database/sql. Verify: `grep -lE '"database/sql"' internal/engine/strand.go internal/engine/verify_pins.go internal/engine/vote_cast.go internal/engine/vote_record.go` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in any listed file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the four files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: reporting every pin as verified in `verifyPinsIn` fails a pin verification test; the change-summary names it.

## Step 41: Retype handles in the last two engine vote files

### Draft

- Title: refactor(engine): retype handles in vote and waive
- Files: internal/engine/vote.go, internal/engine/waive.go
- Depends on: Step 1
- AC: No listed file imports database/sql, and after this step and Steps 3 to 40 no non-test file in either package does. Verify: `grep -lE '"database/sql"' internal/engine/vote.go internal/engine/waive.go` prints nothing; once every step has landed, `grep -rlE '"database/sql"' internal/cli internal/engine --include='*.go' --exclude='*_test.go'` prints nothing. Mutant: restoring the import with one `*sql.Tx` parameter in either file prints that path.
- AC: The retype changes no caller. Verify: `git diff --name-only <step base>...HEAD` lists only the two files. Mutant: retyping one parameter to `store.Tx` forces an edit in a caller, which that command then lists.
- AC: Behavior is unchanged. Verify: `bash scripts/qa/build.sh` and `bash scripts/qa/tests.sh` exit 0. Mutant: routing a failed vote as passed in `routeVoteStep` fails a vote routing test; the change-summary names it.

## After the last step

These items follow the 41 steps. None of them is needed for the import bans,
so none should become a dependency of the import-boundary gates.

- **Retarget.** Point `LegacyDB`, `LegacyTx` and `LegacyReadTx` at `Store`,
  `Tx` and `ReadTx`. Make `root.go` open `store.OpenSQLite`, and replace
  `StoreOn(x)` and `TxOn(x)` with `x` before deleting them. This needs two
  things first. The per-file `internal/db` removal work must have landed,
  because a row that still passes its handle to an `internal/db` helper stops
  compiling once the handle is a `Store`. And 199 test files in the two
  packages import `database/sql` at b7feee05 and hand `*sql.DB` values to
  engine and CLI code. They need their own plan before the retarget.
- **Gate coverage until the retarget.** While the aliases name `*sql.DB` and
  `*sql.Tx`, code can still call `Query`, `QueryRow` or `Exec` on a handle
  without importing `database/sql`. The import ban alone does not catch that.
  Each step's raw-SQL criterion guards its files when they land. The
  import-boundary gates would hold the line afterwards only if they also
  rejected those calls in the two packages' non-test files.
- **Overlap with the per-file `internal/db` work.** Those steps edit many of
  the same files. Both kinds compile against the aliases, so whichever lands
  second rebases without a type conflict.
