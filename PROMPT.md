# Docket — Local-First CLI Issue Tracker

> **Issue tracking for ai and humans.**

Build a CLI issue tracker called **Docket** in Go. Docket is a local-first, SQLite-backed issue management tool that lives inside your repository. It provides enterprise-grade issue tracking features without requiring a server, network connection, or third-party service.

Docket serves two audiences equally: human developers who want a fast, beautiful terminal experience, and AI coding agents (like Claude Code) that need structured, machine-readable output to plan and execute work. Every command renders rich, colorful, styled output by default — and clean, parseable JSON with `--json`. Neither mode is an afterthought. Both are first-class.

## Tech Stack & Constraints

- **Language:** Go (latest stable)
- **CLI Framework:** Cobra
- **Database:** SQLite via `modernc.org/sqlite` (pure Go, no CGO, `database/sql` compatible — actively maintained and the standard CGo-free choice)
- **No external services** — everything is local and self-contained
- **Module path:** `github.com/ALT-F4-LLC/docket`

## Database Location

- Default: `$PWD/.docket/issues.db`
- Override via environment variable: `DOCKET_PATH` (points to the `.docket` directory)
- Provide an explicit `docket init` command that creates the `.docket/` directory and initializes the schema
- `docket init` should print a suggestion to add `.docket/` to `.gitignore`
- On first run of any command (except `init` and `version`), if the DB doesn't exist, prompt to run `docket init`.
- Implement the path resolution in `internal/config/config.go` — check `DOCKET_PATH` env var first, fall back to `$PWD/.docket`

## Issue ID Format

- Prefixed auto-increment: `DKT-1`, `DKT-2`, `DKT-3`, etc.
- The prefix `DKT` should be a constant that's easy to change
- IDs are unique within a single database
- Commands should accept both the full prefixed ID (`DKT-5`) and bare number (`5`)

## Schema Design

Design a normalized SQLite schema with the following entities and relationships:

### Issues
- `id` (integer primary key, auto-increment)
- `parent_id` (integer, nullable, foreign key → issues) — enables first-class sub-issue hierarchy
- `title` (text, required)
- `description` (text, markdown supported)
- `status` (text, enum — see statuses below)
- `priority` (text, enum — see priorities below)
- `type` (text, enum: `bug`, `feature`, `task`, `epic`, `chore`)
- `assignee` (text, optional)
- `created_at`, `updated_at` (timestamps)

Sub-issues inherit their parent's `type` by default but can override it. There is no depth limit — sub-issues can have their own sub-issues (recursive hierarchy). See the `delete` command for child handling behavior.

### Comments
- `id` (integer primary key)
- `issue_id` (foreign key → issues)
- `body` (text, markdown supported)
- `author` (text, defaults to git user or system username)
- `created_at` (timestamp)

### Labels
- Many-to-many relationship with issues via a join table
- Labels have a `name` and optional `color` (for terminal rendering)
- Labels are created on-the-fly when first used, or managed explicitly

### Issue Relations
- `id` (integer primary key)
- `source_issue_id` (foreign key → issues)
- `target_issue_id` (foreign key → issues)
- `relation_type` (enum: `blocks`, `depends_on`, `relates_to`, `duplicates`)
- `created_at` (timestamp)
- Note: `parent_of`/`child_of` are NOT relation types — sub-issues are first-class via `parent_id` on the issues table
- **Store only one row per relation.** Only store the "forward" types: `blocks`, `depends_on`, `relates_to`, `duplicates`. Compute the inverse at query time:
  - `blocks` A→B means B is `blocked_by` A
  - `depends_on` A→B means B is `dependency_of` A
  - `relates_to` and `duplicates` are symmetric — A→B implies B→A
- This avoids data inconsistency from maintaining two rows
- Prevent self-referential relations
- Prevent duplicate relations (including inverse duplicates — if A blocks B exists, reject B blocks A)
- Add a unique constraint on `(source_issue_id, target_issue_id, relation_type)`

### Activity Log
- Track all state changes on issues (status changes, priority changes, label additions/removals, relation changes)
- Each entry: `issue_id`, `field_changed`, `old_value`, `new_value`, `changed_by`, `timestamp`
- This powers the `docket log <id>` command

## Statuses (Kanban Workflow)

Implement the following statuses as an ordered workflow:

| Status        | Description                        |
|---------------|------------------------------------|
| `backlog`     | Acknowledged but not yet planned   |
| `todo`        | Planned for current work cycle     |
| `in-progress` | Actively being worked on           |
| `review`      | Work complete, pending review      |
| `done`        | Finished                           |

- Default status on creation: `backlog`
- Allow setting status on creation via flag
- `docket move <id> <status>` as a shorthand for status changes
- No enforced linear progression — allow jumping between any statuses

## Priorities

| Priority   | Display    |
|------------|------------|
| `critical` | 🔴 or red  |
| `high`     | 🟠 or orange |
| `medium`   | 🟡 or yellow |
| `low`      | 🟢 or green  |
| `none`     | ⚪ or dim    |

- Default priority: `none`

## CLI Command Structure

Design the CLI with the following command tree. Use Cobra's standard patterns with short and long descriptions, usage examples, and flag aliases.

### Global Flags (apply to all commands)

```
--json                               # Structured JSON output (for agents and scripts)
--quiet, -q                          # Suppress non-essential output (useful in scripts)
```

The `--json` flag is the switch between Docket's two modes. Without it, output is styled, colorful, and human-friendly. With it, output is a predictable JSON contract that agents can parse without guessing. Both paths must be implemented for every command — not one as a wrapper around the other.

**Critical stdout/stderr discipline:**
- **stdout** is exclusively for data output (human-readable tables OR JSON). Nothing else.
- **stderr** is for errors, warnings, progress indicators, and informational messages.
- This separation is essential — agents parse stdout and must never encounter unexpected text.
- When `--quiet` is set, suppress informational messages on stderr but still emit errors.

When `--json` is set, use a consistent envelope:

```json
{
  "ok": true,
  "data": { ... },
  "message": "Issue DKT-5 created"
}
```

On error:
```json
{
  "ok": false,
  "error": "Issue DKT-99 not found",
  "code": "NOT_FOUND"
}
```

Error `code` values map to exit codes: `GENERAL_ERROR` (exit 1), `NOT_FOUND` (exit 2), `VALIDATION_ERROR` (exit 3), `CONFLICT` (exit 4). Both the JSON `code` field and the process exit code must be set consistently.

### Exit Codes

```
0  — Success
1  — General error (invalid input, DB error, etc.)
2  — Entity not found
3  — Validation error (missing required field, invalid status, etc.)
4  — Conflict (duplicate relation, circular dependency, etc.)
```

Consistent exit codes let agents branch on failure type without parsing error messages.

### Core Issue Commands

```
docket init                          # Initialize .docket/ in current directory
docket create                        # Create issue (interactive if no flags)
  -t, --title         (required)
  -d, --description   (string, or "-" to read from stdin)
  -s, --status        (default: backlog)
  -p, --priority      (default: none)
  -T, --type          (default: task)
  -l, --label         (repeatable)
  -a, --assignee
  --parent            (parent issue ID — creates this as a sub-issue)

docket show <id>                     # Show full issue detail with sub-issues, relations, comments, activity
docket edit <id>                     # Edit issue fields
  (same flags as create, all optional — only provided flags are updated)
  --parent            (reparent an issue — set to 0 or "none" to make it a root issue)

docket list | ls                     # List issues with filtering and sorting
  -s, --status        (repeatable, filter)
  -p, --priority      (repeatable, filter)
  -l, --label         (repeatable, filter)
  -T, --type          (repeatable, filter)
  -a, --assignee      (filter)
  --parent            (filter by parent issue ID)
  --roots             (show only root issues — no parent)
  --tree              (display issues as indented hierarchy showing sub-issue nesting)
  --sort              (field:direction, e.g. "priority:desc", "created_at:asc")
  --limit             (default: 50)
  --all               (include done issues, excluded by default)

docket move <id> <status>            # Change issue status (shorthand)
docket close <id>                    # Shorthand for move <id> done
docket reopen <id>                   # Shorthand for move <id> todo
docket delete <id>                   # Delete issue (with confirmation prompt)
  -f, --force                        # Skip confirmation, cascade-deletes sub-issues
  --orphan                           # When deleting a parent, orphan sub-issues instead of cascading
  # Default behavior (interactive): if issue has sub-issues, prompt user to choose orphan or cascade
  # With --force: cascade-deletes sub-issues
  # With --orphan: removes parent reference, sub-issues become root issues
  # With --json: requires either --force or --orphan if issue has sub-issues (no interactive prompt)
```

**Note:** The global `--json` flag is the only way to get JSON envelope output. Do not add per-command `-o`/`--output` or `--format` flags for JSON. The only exception is `docket export -o` which controls the export serialization format (csv, markdown).

### Comments

```
docket comment <id>                  # Add comment (opens $EDITOR or reads stdin)
  -m, --message                      # Inline message (required when --json is set)
docket comments <id>                 # List all comments on an issue
```

When `--json` is set, `-m` is required — do not open `$EDITOR` or attempt interactive input. Also support reading from stdin via pipe: `echo "comment body" | docket comment 5`.

### Labels

```
docket label add <id> <label>...     # Add labels to an issue
docket label rm <id> <label>...      # Remove labels from an issue
docket label list                    # List all labels in the database
docket label delete <label>          # Delete a label entirely
```

### Relations

```
docket link <id> <relation> <target_id>
  # e.g., docket link 3 blocks 7
  # e.g., docket link 3 depends-on 1
  # Valid relations: blocks, depends-on, relates-to, duplicates
  # CLI accepts hyphens (depends-on); normalize to underscores (depends_on) for storage
  # Note: parent/child relationships use --parent on create/edit, not link

docket unlink <id> <relation> <target_id>
docket links <id>                    # Show all relations for an issue
```

### Activity & History

```
docket log <id>                      # Show activity log / changelog for an issue
  --limit             (default: 20)
```

### Board View

```
docket board                         # Kanban board view in terminal
  -l, --label         (filter)
  -p, --priority      (filter)
  -a, --assignee      (filter)
  --expand                           # Show sub-issues as individual cards under parents
```

Render a columnar Kanban board in the terminal using `lipgloss`. Each column is a status, showing status name and issue count. Each card shows ID, title (truncated), priority indicator, and labels. Parent issues display a sub-issue progress indicator (e.g., "▰▰▰▱▱ 3/5"). Sub-issues roll up into their parent by default — use `--expand` to show them as individual cards. Columns adapt to terminal width. Show at most 10 issues per column, with a "+N more" indicator.

### Work Planning (Agent-Oriented)

These commands exist specifically so an AI agent can query the issue graph and compute what to work on. They are the primary interface for Claude Code.

```
docket next                          # Show issues that are ready to work on (unblocked, not done)
  -s, --status        (filter, default: backlog,todo)
  -p, --priority      (filter)
  -l, --label         (filter)
  -T, --type          (filter)
  --limit             (default: 10)
```

`docket next` returns issues sorted by work-readiness: highest priority first, then oldest first. An issue is "ready" if:
- Its status is not `done` or `review`
- All its `blocked_by`/`depends_on` targets are in `done` status
- Its parent (if any) is not `done`
- It has no incomplete sub-issues (agents should work on leaf tasks, not parent groupings)

```
docket plan                          # Compute a full execution plan from the issue graph
  --root <id>                        # Scope plan to a parent issue and its sub-issue tree
  -s, --status        (filter, default: backlog,todo,in-progress)
  -l, --label         (filter)
```

`docket plan` performs a topological sort on the dependency graph (blocking + depends-on relations + sub-issue hierarchy) and outputs an execution plan with parallelization groups. Use `--json` for machine-readable output.

**Text output example:**
```
Execution Plan (scoped to DKT-3):

Phase 1 (parallel):
  DKT-7  [high]     Set up database schema
  DKT-8  [medium]   Configure auth provider

Phase 2 (parallel, after Phase 1):
  DKT-9  [high]     Build user model          (depends on DKT-7)
  DKT-10 [medium]   Build login endpoint      (depends on DKT-7, DKT-8)

Phase 3:
  DKT-11 [high]     Integration tests         (depends on DKT-9, DKT-10)

Summary: 5 issues, 3 phases, max parallelism: 2
```

**JSON output** (`--json`) returns:
```json
{
  "ok": true,
  "data": {
    "phases": [
      {
        "phase": 1,
        "issues": [
          {
            "id": "DKT-7",
            "title": "Set up database schema",
            "priority": "high",
            "status": "todo",
            "blocked_by": [],
            "depends_on": []
          }
        ]
      }
    ],
    "total_issues": 5,
    "total_phases": 3,
    "max_parallelism": 2
  }
}
```

Implementation notes for `docket plan`:
- Build a DAG from `blocks`/`depends_on` relations AND parent→child hierarchy (children must complete before parent is considered done)
- Detect cycles and report as errors (exit code 4)
- Phase 1 = issues with no dependencies; each subsequent phase depends only on earlier phases
- Within a phase, sort by priority (critical first)
- Skip issues already in `done` status
- `--root` scopes the plan to a parent and all its descendants

```
docket graph <id>                    # Show the dependency graph for an issue
  --direction         (up, down, both — default: both)
  --depth             (max traversal depth, default: unlimited)
  --mermaid                          # Output as Mermaid flowchart syntax instead of text
```

`docket graph` traverses the full dependency and sub-issue tree for a given issue. `--direction up` shows what this issue blocks/is a child of. `--direction down` shows what depends on it and its sub-issues. `--mermaid` generates a Mermaid flowchart definition that can be pasted into markdown. Use the global `--json` flag for structured graph data.

### Export

```
docket export                        # Export all issues as JSON (default)
  -o, --output        (csv, markdown — alternative export formats)
  -f, --file          (output file path, default: stdout)
  -s, --status        (filter)
  -l, --label         (filter)
```

- **JSON (default):** Full structured export of issues with sub-issues (nested), comments, relations, labels
- **CSV:** Flat table of issue fields (no nested comments/relations)
- **Markdown:** Render a project report with issues grouped by status, including descriptions and comments

### Import

```
docket import <file>                 # Import from JSON export
  --merge                            # Merge with existing (skip duplicates by ID)
  --replace                          # Replace entire database
```

### Config / Meta

```
docket config                        # Show current configuration
docket version                       # Print version
docket stats                         # Show summary stats (total issues, root vs sub-issues, by status, by priority, label cloud)
```

`docket config` displays: database path (resolved), database size, schema version, issue prefix, and whether `DOCKET_PATH` env var is set. This helps agents and users confirm they're operating on the right database.

## Terminal Output & UX

### Table Output
- Use a clean, minimal table format for `docket list` via `lipgloss/table`
- Color-code priorities and statuses using ANSI colors
- Truncate long titles with ellipsis based on available terminal width
- Show relative timestamps ("2h ago", "3d ago") via `go-humanize`
- Respect `NO_COLOR` environment variable (see https://no-color.org/) — disable all color output when set
- Respect `TERM=dumb` — fall back to unformatted output

### Issue Detail View (`docket show`)
- Render a rich detail view with sections: header, metadata, parent (if sub-issue), description (markdown rendered), sub-issues (as indented tree with status/priority indicators), relations, comments, recent activity
- If the issue has a parent, show the parent's ID and title at the top (e.g., "↑ Parent: DKT-3 — Build auth module")
- Sub-issue tree shows each child with status icon, priority color, ID, and title — recursively for nested sub-issues
- Show a progress summary line for sub-issues (e.g., "Sub-issues: 3/5 done")
- Use `glamour` for markdown rendering in descriptions and comments
- Use `lipgloss/tree` for rendering the sub-issue hierarchy

### Interactive Creation
- If `docket create` is called without `--title`, enter an interactive mode that prompts for each field
- Use `charmbracelet/huh` for interactive forms
- **When `--json` is set, NEVER enter interactive mode** — if `--title` is missing, return a validation error. Agents cannot interact with TUI prompts.

### Machine-Readable Output (`--json`)

Per-command contracts:

- `docket create --json` → returns the full created issue object including its assigned ID. The agent needs this to capture the ID for subsequent commands (adding sub-issues, relations, etc.)
- `docket list --json` → returns an array of issue objects
- `docket show <id> --json` → returns the full issue with nested sub-issues, relations, comments, and activity
- `docket next --json` → returns an ordered array of work-ready issues
- `docket plan --json` → returns the phased execution plan (see plan command spec above)
- `docket move/close/reopen --json` → returns the updated issue object
- `docket link --json` → returns the created relation with both sides

All timestamps in JSON output should be ISO 8601 in UTC. All enums should be lowercase strings. IDs should include the prefix (e.g., `"id": "DKT-5"`).

**Example: `docket create --json -t "Build auth" -p high -T feature --parent 3`**
```json
{
  "ok": true,
  "data": {
    "id": "DKT-12",
    "parent_id": "DKT-3",
    "title": "Build auth",
    "description": "",
    "status": "backlog",
    "priority": "high",
    "type": "feature",
    "assignee": "",
    "labels": [],
    "created_at": "2026-02-13T18:30:00Z",
    "updated_at": "2026-02-13T18:30:00Z"
  },
  "message": "Created DKT-12"
}
```

**Example: `docket list --json -s todo -s in-progress`**
```json
{
  "ok": true,
  "data": {
    "issues": [ ... ],
    "total": 8
  }
}
```

## Recommended Libraries

- `github.com/spf13/cobra` — CLI framework
- `modernc.org/sqlite` — Pure Go SQLite driver, CGo-free, `database/sql` compatible
- `github.com/charmbracelet/lipgloss` — Terminal styling, layout, tables, and tree rendering (use v1 stable)
- `github.com/charmbracelet/glamour` — Terminal markdown rendering
- `github.com/charmbracelet/huh` — Interactive forms and prompts (uses bubbletea internally — do not build custom bubbletea programs, just use huh for forms)
- `github.com/dustin/go-humanize` — Relative timestamps ("3 hours ago"), byte sizes

Stay within the Charmbracelet ecosystem for all TUI/rendering needs. Use stable v1 releases.

## Project Structure

```
docket/
├── main.go
├── go.mod
├── go.sum
├── Makefile
├── cmd/                    # Cobra command definitions
│   ├── root.go             # Root command, global flags (--json, --quiet), DB initialization
│   ├── init.go
│   ├── create.go
│   ├── show.go
│   ├── edit.go
│   ├── list.go
│   ├── move.go
│   ├── close.go
│   ├── reopen.go
│   ├── delete.go
│   ├── comment.go
│   ├── label.go
│   ├── link.go
│   ├── log.go
│   ├── board.go
│   ├── next.go
│   ├── plan.go
│   ├── graph.go
│   ├── export.go
│   ├── import.go
│   ├── config.go
│   ├── stats.go
│   └── version.go
├── internal/
│   ├── db/                 # Database layer
│   │   ├── db.go           # Connection management, initialization, WAL mode
│   │   ├── schema.go       # Schema definition and migrations
│   │   ├── issues.go       # Issue CRUD operations
│   │   ├── comments.go     # Comment operations
│   │   ├── labels.go       # Label operations
│   │   ├── relations.go    # Issue relation operations
│   │   └── activity.go     # Activity log operations
│   ├── model/              # Data types and enums
│   │   ├── issue.go
│   │   ├── comment.go
│   │   ├── label.go
│   │   ├── relation.go
│   │   └── activity.go
│   ├── output/             # Output abstraction (critical — every command uses this)
│   │   ├── output.go       # Interface: Render(data) switches on --json flag
│   │   ├── json.go         # JSON envelope rendering (ok/data/error)
│   │   └── human.go        # Human-readable rendering dispatch
│   ├── render/             # Terminal rendering (human mode only)
│   │   ├── table.go        # Table output for lists
│   │   ├── detail.go       # Issue detail view
│   │   ├── board.go        # Kanban board rendering
│   │   └── markdown.go     # Markdown rendering helpers
│   ├── planner/            # Dependency graph & execution planning
│   │   ├── dag.go          # DAG construction from issues + relations
│   │   ├── topo.go         # Topological sort & cycle detection
│   │   └── plan.go         # Phase grouping & parallelization logic
│   └── config/             # Configuration management
│       └── config.go       # Path resolution, env vars
└── README.md               # Include the motto: "Structured output for agents. Pretty things for humans."
```

The `internal/output` package is critical. It provides a single entry point that every command calls to render its result. Based on the `--json` flag, it either serializes to the JSON envelope or delegates to the `render` package for human output. This prevents commands from having scattered `if json { ... } else { ... }` blocks — the output mode is handled in one place.

## Implementation Notes

1. **SQLite Pragmas:** On every connection, set these pragmas before any operations:
   - `PRAGMA journal_mode=WAL;` — Write-Ahead Logging for better concurrent read performance
   - `PRAGMA foreign_keys=ON;` — SQLite does NOT enforce foreign keys by default; this must be explicitly enabled
   - `PRAGMA busy_timeout=5000;` — Wait up to 5s on lock contention instead of failing immediately

2. **Schema Migrations:** Use a simple versioned migration approach. Store a `schema_version` in a `meta` table. On startup, check the version and apply any pending migrations. This allows the schema to evolve without breaking existing databases.

3. **Transactions:** Wrap all multi-step operations (e.g., creating an issue with labels and relations) in transactions.

4. **Timestamps:** Store all timestamps as ISO 8601 strings in UTC. The `updated_at` field must be automatically updated on any mutation to an issue (status change, priority change, edit, label change, relation change, etc.) — use a trigger or handle it explicitly in the update functions.

5. **Sub-issue Hierarchy Queries:** Use SQLite recursive CTEs (`WITH RECURSIVE`) to traverse the sub-issue tree for display, cascade operations, and progress rollups. Example: computing "3/5 sub-issues done" requires walking all descendants, not just direct children.

6. **Cycle Detection:** The `docket link` command must validate that adding a relation does not create a cycle in the dependency graph. Before inserting a `blocks` or `depends_on` relation, traverse the graph from the target back to the source — if a path exists, reject the relation with exit code 4 and a clear error message explaining the cycle. Similarly, `docket edit --parent` must prevent creating cycles in the sub-issue tree (an issue cannot be reparented under one of its own descendants).

7. **Git Integration (lightweight):** Default `author`/`assignee` values by shelling out to `git config user.name` and `git config user.email`. Don't make git a hard dependency — fall back to OS username via `os/user`.

8. **Error Handling:** Use clear, actionable error messages. If the DB doesn't exist, suggest `docket init`. If an issue ID doesn't exist, say so clearly with exit code 2. Errors go to stderr (see stdout/stderr discipline in Global Flags).

9. **Testing:** Write table-driven tests for:
   - Database layer: CRUD operations, constraint enforcement, cascade behavior
   - Cycle detection: both in relations and sub-issue hierarchy
   - Planner: topological sort correctness, phase grouping, parallelization
   - Output: JSON envelope structure matches the contract
   - Use an in-memory SQLite database (`:memory:`) for all tests

10. **Build:** Include a `Makefile` with:
    - `make build` — build binary
    - `make test` — run tests
    - `make lint` — run `golangci-lint`
    - `make install` — install to `$GOPATH/bin`
    - Version injection via `-ldflags` (embed version, commit hash, build date)

## MVP Priority Order

Build in this order to maintain a usable tool at each step:

1. **Schema + init + config** — Set up the database layer first: connection management, SQLite pragmas, schema creation, migration framework, and path resolution. This is the foundation everything else depends on.
2. **create, list, show** — Core CRUD with `--parent` flag, sub-issue display, and `--json` output from day one. Build the `internal/output` package here — every subsequent command will use it.
3. **edit, move, close, reopen, delete** — State management including reparenting and sub-issue cascade delete.
4. **comment, comments** — Comments with `-m` flag and stdin support.
5. **label** subcommands — Organization and filtering.
6. **link, unlink, links** — Relations and dependencies with cycle detection.
7. **next, plan, graph** — **Agent-critical**: dependency graph queries and execution planning. Build immediately after relations since agents depend on these to determine work order.
8. **log** — Activity history.
9. **board** — Kanban board visualization.
10. **export, import** — Data portability.
11. **stats, version** — Meta commands.

**At each step:**
- The tool must compile with zero warnings: `go build ./...`
- All existing tests must pass: `go test ./...`
- Run `go vet ./...` and fix any issues
- Manually verify the new commands work end-to-end (both human and `--json` modes)
- Do not move to the next step until the current step is solid
