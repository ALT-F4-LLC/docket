---
project: "docket"
maturity: "experimental"
last_updated: "2026-03-21"
updated_by: "@staff-engineer"
scope: "Performance characteristics, database tuning, query patterns, and scaling considerations"
owner: "@staff-engineer"
dependencies:
  - architecture.md
---

# Performance

## Overview

Docket is a single-user, local-first CLI tool backed by an embedded SQLite database. Its performance profile is that of a command-line utility: sub-second response times for typical workloads (hundreds to low thousands of issues), with no network latency, no connection overhead beyond process startup, and no concurrent-user contention. There are no explicit performance SLAs, benchmarks, or profiling infrastructure in the codebase today.

## Database Configuration

### SQLite Pragmas

The database is opened in `internal/db/db.go:Open()` with three performance-relevant pragmas:

| Pragma | Value | Effect |
|--------|-------|--------|
| `journal_mode` | `WAL` | Write-Ahead Logging enables concurrent readers with a single writer, reducing lock contention vs. the default rollback journal. |
| `foreign_keys` | `ON` | Enforces referential integrity at the cost of additional constraint checks on every write. |
| `busy_timeout` | `5000` | Waits up to 5 seconds for a locked database before returning `SQLITE_BUSY`, preventing immediate failures when the WAL writer is active. |

### Connection Pool

The pool is explicitly limited to **1 connection** (`SetMaxOpenConns(1)`). This is intentional: SQLite is single-writer, so a single connection avoids Go's `database/sql` pool from opening multiple connections that would contend on SQLite's file-level lock. The tradeoff is that all operations are serialized, including reads. For a single-user CLI this is appropriate; it would become a bottleneck under concurrent access (e.g., if docket were ever used as a library or server).

## Schema Indexing

The schema (`internal/db/schema.go`) defines the following indexes:

### Issues Table Indexes
- `idx_issues_status` on `issues(status)` -- used by default list filtering (excludes `done`)
- `idx_issues_priority` on `issues(priority)` -- used by priority-based list filtering
- `idx_issues_assignee` on `issues(assignee)` -- used by assignee filter
- `idx_issues_parent_id` on `issues(parent_id)` -- used by parent/child queries, `RootsOnly` filter
- `idx_issues_created_at` on `issues(created_at)` -- used by default sort order
- `idx_issues_updated_at` on `issues(updated_at)` -- used by `updated_at` sort

### Other Indexes
- `idx_issue_files_file_path` on `issue_files(file_path)` -- supports file-path lookups
- `idx_proposals_status` on `proposals(status)` -- proposal listing by status
- `idx_proposals_created_at` on `proposals(created_at)` -- proposal ordering
- `idx_votes_proposal_id` on `votes(proposal_id)` -- vote lookups by proposal

Composite primary keys on `issue_labels(issue_id, label_id)`, `issue_files(issue_id, file_path)`, and `proposal_issues(proposal_id, issue_id)` also serve as indexes.

**Gap:** There is no composite index for the default sort order (`status` rank, `priority` rank, `created_at`). The `CASE`-based ordering expressions in `ListIssues` cannot use indexes, so the default sort requires a full table scan followed by an in-memory sort. This is acceptable at current scale but would degrade with tens of thousands of issues.

## Query Patterns

### N+1 Query Avoidance

The codebase has explicit batch-loading patterns to avoid N+1 queries:

- **`HydrateLabels()`** (`internal/db/issues.go`): Bulk-loads all labels for a set of issues using a single `WHERE issue_id IN (...)` query. Called after `ListIssues`, `ListAllIssues`, and `GetIssuesByIDs`.
- **`HydrateFiles()`** (`internal/db/files.go`): Same pattern for issue-file attachments.
- **`GetBatchSubIssueProgress()`** (`internal/db/issues.go`): Batch-computes (done, total) counts for multiple parent IDs in a single recursive CTE query, avoiding per-parent queries.
- **`GetIssuesByIDs()`** (`internal/db/issues.go`): Fetches multiple issues by ID in a single query with label and file hydration.

### Recursive CTEs

Several operations use SQLite recursive CTEs:

- **`GetSubIssueTree()`**: Fetches the full recursive descendant tree of an issue.
- **`GetSubIssueProgress()`** and **`GetBatchSubIssueProgress()`**: Compute progress aggregates over descendant trees.
- **`IsDescendant()`**: Cycle detection for parent reparenting.
- **`CascadeDeleteIssue()`**: Deletes an entire sub-tree in one operation.
- **`checkCycleTx()`** (`internal/db/relations.go`): Cycle detection for `blocks`/`depends_on` relations using recursive reachability.

These CTEs are unbounded -- they traverse as deep as the tree goes. For typical issue hierarchies (2-5 levels) this is negligible. Pathologically deep trees (hundreds of levels) would be slower, but this is an unlikely usage pattern.

### ListIssues Query Complexity

`ListIssues()` dynamically builds a query with optional `JOIN`, `WHERE`, `GROUP BY`, `HAVING`, and `ORDER BY` clauses. Key observations:

- **Label filtering** adds a JOIN to `issue_labels` and `labels`, plus `GROUP BY i.id` and `HAVING COUNT(DISTINCT l.name) = N` for AND-logic. This is the most expensive filter combination.
- A **count query** always runs before the main query to support pagination metadata, meaning every `ListIssues` call executes two queries against the database.
- The default composite sort uses `CASE` expressions that cannot leverage indexes.

### Export Path

The `export` command (`internal/cli/export.go`) loads **all** data into memory in 6 separate queries (issues, comments, relations, labels, label-mappings, file-mappings), then applies in-memory filtering. This is fine for the expected scale (thousands of issues) but does not support streaming for very large datasets.

### Import Path

The `import` command (`internal/cli/import.go`) performs all inserts within a **single transaction**, which is good for both atomicity and performance (avoids per-statement fsync). The two-pass strategy for parent-child relationships (insert with NULL parent, then UPDATE parent_id) adds N extra UPDATE statements but avoids insertion-order dependency issues.

## In-Memory Algorithms

### DAG and Topological Sort (Planner)

The planner (`internal/planner/`) operates entirely in-memory after loading all issues and relations:

- **`BuildDAG()`**: O(N + E) where N = issues, E = relations. Constructs an adjacency representation using hash maps.
- **`TopoSort()`**: Kahn's algorithm, O(N + E). Groups issues by topological level for phase planning.
- **`GeneratePlan()`**: After topological sort, applies filtering (O(N)), sorting per phase (O(k log k) per phase), and file-collision splitting (O(k * f) where f = files per issue).
- **`FindReady()`**: Linear scan over all DAG nodes, O(N).
- **`scopeToDescendants()`**: BFS over parent-child tree, O(N).

All planner operations build the full DAG in memory from all issues and all directional relations. There is no incremental or partial loading.

### BFS Graph Traversal

The `issue graph` command (`internal/cli/issue_graph.go`) performs BFS over precomputed adjacency lists. It has an optional `--depth` limit to bound traversal. Without a depth limit, it visits all reachable nodes, which scales with the graph's connected component size.

## Caching

There is minimal caching in the codebase:

- **`DefaultAuthor()`** (`internal/config/config.go`): Uses `sync.Once` to cache the result of `git config user.name` for the process lifetime. This avoids repeated subprocess spawning (2-second timeout per invocation).

There is no query result caching, prepared statement caching, or memoization layer. Each CLI invocation is a fresh process that opens the database, executes queries, and exits. Process-level caching would provide no benefit in this execution model.

## Concurrency

Docket is a **single-process, single-goroutine** CLI tool. There are:

- No goroutines beyond the main goroutine
- No `sync.Mutex` or `sync.RWMutex` usage (except `sync.Once` for author caching)
- No `sync.Pool` usage
- No channel-based concurrency
- No parallel query execution

The single-connection SQLite pool enforces serialization at the database level. All transaction boundaries use `db.Begin()` / `tx.Commit()` with `defer tx.Rollback()` for cleanup, which is correct but not optimized for throughput.

## Pagination

`ListIssues` supports `LIMIT` and `OFFSET` pagination via the `ListOptions` struct. The total count is computed separately for pagination metadata. There is no cursor-based pagination. OFFSET-based pagination degrades for large offsets (SQLite must scan and discard rows), but this is unlikely to be problematic at the expected scale.

## Known Performance Gaps

1. **No benchmarks**: The test suite (`make test`) contains no `Benchmark*` functions. There is no baseline for regression detection.

2. **No prepared statements**: All queries are built as raw strings and executed directly. The `database/sql` driver may cache prepared statements internally, but the application does not explicitly prepare and reuse statements.

3. **Full table loads for planner and export**: `ListAllIssues()` and `GetAllDirectionalRelations()` load entire tables into memory. This is acceptable at current scale but would not scale to hundreds of thousands of issues.

4. **Double query for pagination**: Every `ListIssues` call runs a count query and a data query. These could potentially be combined using `COUNT(*) OVER()` window functions if SQLite's window function support is sufficient.

5. **Default sort is index-unfriendly**: The `CASE`-based status/priority ranking in the default `ORDER BY` clause cannot use indexes, forcing a full sort on every list operation.

6. **No WAL checkpoint management**: WAL mode is enabled but there is no explicit `PRAGMA wal_checkpoint` call. SQLite auto-checkpoints at the default threshold (1000 pages), which is fine for typical use but could cause occasional write stalls if the WAL grows large during bulk operations.

7. **File-collision splitting is quadratic in worst case**: `splitByFileCollision()` in the planner iterates over remaining issues in each pass. If every issue collides with every other issue on file paths, this degrades to O(N^2). In practice, file collisions should be sparse.

## Scaling Characteristics

| Dimension | Current Approach | Expected Ceiling |
|-----------|-----------------|-----------------|
| Issue count | Full table loads for planner/export | Low thousands comfortable; tens of thousands would slow planner and export |
| Relation count | Full relation loads for DAG construction | Proportional to issue count; same ceiling |
| Tree depth | Unbounded recursive CTEs | Hundreds of levels would be slow; unlikely in practice |
| Concurrent users | Single connection, single process | Not designed for concurrent access |
| Database size | Single SQLite file, no compaction | Hundreds of MB before file I/O becomes noticeable |
| CLI startup | Opens DB + runs pragmas + migrates on every invocation | Negligible for SQLite; would matter for network databases |

## Recommendations for Future Work

These are observations, not requirements. The current performance characteristics are appropriate for the tool's scope and maturity.

1. **Add benchmark tests** for `ListIssues` (with various filter combinations), `BuildDAG` + `TopoSort`, and `HydrateLabels` at representative scales (100, 1000, 10000 issues). This establishes a regression baseline.
2. **Consider `LIMIT` on recursive CTEs** for safety, especially `checkCycleTx()` which could theoretically traverse large graphs.
3. **Profile before optimizing**: The tool is fast enough today. Any optimization work should be driven by measured bottlenecks, not speculation.
