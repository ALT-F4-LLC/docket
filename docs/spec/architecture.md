---
project: "docket"
maturity: "experimental"
last_updated: "2026-03-21"
updated_by: "@staff-engineer"
scope: "System architecture of the docket CLI issue tracker: components, data flow, module boundaries, and key design decisions"
owner: "@staff-engineer"
dependencies:
  - security.md
  - operations.md
  - performance.md
---

# Architecture

## 1. System Overview

Docket is a **local-first CLI issue tracker** that stores all data in a single SQLite database within the project directory. It is designed for developer-centric workflows where issues live alongside the code, with no external services, accounts, or network dependencies required.

The project tagline from the CLI is: **"Local-first CLI issue tracker"**.

### Key Design Principles

- **Local-first**: All state is in a single SQLite file (`.docket/issues.db`), co-located with the project. No server, no network, no accounts.
- **CLI-native**: Primary interface is the terminal. All commands support dual-mode output (human-readable with color/styling, or structured JSON via `--json`).
- **Single-binary**: Built as a statically-linked Go binary (`CGO_ENABLED=0`) using the pure-Go `modernc.org/sqlite` driver. No C dependencies at runtime.
- **Git-friendly**: Data directory (`.docket/`) is designed to live in a project root. Author resolution falls back to `git config user.name`.

## 2. High-Level Architecture

```
cmd/docket/main.go          -- Entry point: delegates to cli.Execute()
cmd/vorpal/main.go           -- Vorpal build system configuration (dev tooling)

internal/
  cli/                        -- Command definitions (cobra commands)
  config/                     -- Configuration resolution (DOCKET_PATH / cwd)
  db/                         -- Database access layer (SQLite)
  model/                      -- Domain types and validation
  filter/                     -- Shared filtering utilities
  planner/                    -- DAG construction, topological sort, execution planning
  render/                     -- Terminal rendering (board, table, detail, markdown, vote)
  output/                     -- Output dispatch (JSON envelope vs. human formatting)
```

The architecture follows a **layered pattern** with clear separation:

```
CLI Commands (internal/cli)
    |
    v
Output Layer (internal/output)  <-->  Render Layer (internal/render)
    |
    v
Business Logic (internal/planner, internal/filter)
    |
    v
Data Access (internal/db)
    |
    v
Domain Model (internal/model)
    |
    v
SQLite (modernc.org/sqlite) -- .docket/issues.db
```

## 3. Component Details

### 3.1 Entry Points

**`cmd/docket/main.go`**: Minimal entry point. Calls `cli.Execute()` and passes its return value to `os.Exit()`.

**`cmd/vorpal/main.go`**: Build system configuration using ALT-F4-LLC's Vorpal SDK. Defines the development environment (Go toolchain, staticcheck, protoc, ffmpeg, vhs, ttyd) and the release build artifact. Targets: `aarch64-darwin`, `aarch64-linux`, `x86_64-darwin`, `x86_64-linux`.

### 3.2 CLI Layer (`internal/cli`)

Built on **spf13/cobra**. The root command (`docket`) defines:

- Global flags: `--json` (structured output), `--quiet` (suppress non-essential output)
- `PersistentPreRunE`: Resolves config, opens SQLite DB, runs migrations. Commands annotated with `skipDB` bypass DB initialization (e.g., `init`).
- `PersistentPostRunE`: Closes the DB connection.
- Version info injected via ldflags at build time (`version`, `commit`, `buildDate`).

**Command tree:**

| Command | Subcommands | Description |
|---------|-------------|-------------|
| `init` | -- | Create `.docket/` directory and initialize schema |
| `board` | -- | Kanban board view (columns by status) |
| `plan` | -- | DAG-based execution plan with phased grouping |
| `next` | -- | Work-ready issues (unblocked leaf tasks) |
| `stats` | -- | Summary statistics (counts by status/priority/label) |
| `export` | -- | Export to JSON, CSV, or Markdown |
| `import` | -- | Import from JSON export file (default/merge/replace modes) |
| `version` | -- | Show version information |
| `config` | -- | Show resolved configuration |
| `issue` | `create`, `list`, `show`, `edit`, `close`, `reopen`, `delete`, `move`, `comment`, `label`, `link`, `graph`, `log`, `file` | Full issue lifecycle management |
| `issue comment` | `list` | Comment sub-operations |
| `vote` | `create`, `cast`, `show`, `list`, `result`, `commit`, `link` | PBFT-inspired consensus voting |

**Error handling**: Commands return `*CmdError` wrapping an error with a machine-readable `ErrorCode`. The `Execute()` function maps these to structured JSON error envelopes or styled human error output with distinct exit codes (0=success, 1=general, 2=not-found, 3=validation, 4=conflict).

**Interactive forms**: Issue creation uses `charmbracelet/huh` for interactive TUI forms when `--title` is not provided and JSON mode is off. Import with `--replace` prompts for confirmation.

### 3.3 Configuration (`internal/config`)

Resolution order:
1. `DOCKET_PATH` environment variable (if set)
2. `$PWD/.docket` (default)

Configuration is a simple struct:
- `DocketDir`: Path to the `.docket` directory
- `DBPath`: Full path to `issues.db` within `DocketDir`
- `EnvVarSet`: Whether `DOCKET_PATH` was used

Author resolution (`DefaultAuthor()`): Tries `git config user.name` with a 2-second timeout, falls back to OS username, ultimate fallback is `"unknown"`. Result is cached via `sync.Once`.

### 3.4 Data Access Layer (`internal/db`)

**Connection management**: Opens SQLite via `modernc.org/sqlite` (pure Go, no CGO). Sets `MaxOpenConns(1)` to avoid lock contention. Configures pragmas:
- `journal_mode=WAL` (concurrent reads)
- `foreign_keys=ON` (referential integrity)
- `busy_timeout=5000` (5s retry on lock)

**Schema versioning**: Uses a `meta` table with a `schema_version` key. Schema is at version 3. Migrations run sequentially in transactions:
- v1: Base schema (issues, comments, labels, issue_labels, issue_relations, activity_log, issue_files)
- v2: Vote system (proposals, votes, proposal_issues)
- v3: Enhanced vote tracking (rationale, domain_tags, files_changed, final_outcome, escalation_reason, findings_json, summary)

Includes a repair mechanism for databases stamped as v2 but missing the proposals table (from a buggy `Initialize()`).

**Data access pattern**: Direct SQL queries using `database/sql`. No ORM. Functions accept `*sql.DB` or `*sql.Tx` and return domain model types. Sentinel error `ErrNotFound` for missing entities.

### 3.5 Domain Model (`internal/model`)

Core types with validation and custom JSON marshaling/unmarshaling:

**Issue**: The central entity. Fields: ID, ParentID (nullable, self-referential hierarchy), Title, Description, Status, Priority, Kind, Assignee, Labels, Files, CreatedAt, UpdatedAt.

- **Status** enum: `backlog` | `todo` | `in-progress` | `review` | `done`
- **Priority** enum: `critical` | `high` | `medium` | `low` | `none`
- **IssueKind** enum: `bug` | `feature` | `task` | `epic` | `chore`
- **ID format**: `DKT-{n}` (e.g., `DKT-5`). `ParseID()` accepts both `DKT-5` and `5`.

All enum types implement `Color()` and `Icon()` methods for terminal rendering.

**Relation**: Typed link between two issues. Types: `blocks`, `depends_on`, `relates_to`, `duplicates`. A database trigger prevents inverse duplicate relations. `ParseRelationType()` normalizes hyphens to underscores.

**Proposal** (vote system): ID prefix `DKT-V`. Fields: Description, Criticality (`low`|`medium`|`high`|`critical`), Status (`open`|`approved`|`rejected`|`committed`), RequiredVoters, Threshold, WeightedScore, Rationale, DomainTags, FilesChanged, FinalOutcome, EscalationReason.

**Vote**: Individual vote on a proposal. Fields: VoterName, VoterRole, Verdict (`approve`|`approve-with-concerns`|`reject`), Confidence, DomainRelevance, Findings, FindingsJSON (structured: blockers/concerns/suggestions), Summary.

**ExportData**: Full database export envelope containing issues, comments, relations, labels, and all join-table mappings.

### 3.6 Planner (`internal/planner`)

Implements dependency-aware execution planning:

1. **DAG construction** (`BuildDAG`): Takes issues and relations. Normalizes `blocks` and `depends_on` into a single edge direction (forward = blocker-to-blocked, reverse = blocked-to-blocker). `relates_to` and `duplicates` are ignored for ordering.

2. **Topological sort** (`TopoSort`): Kahn's algorithm producing level-grouped output. Returns `CycleError` listing involved issue IDs when cycles are detected.

3. **Plan generation** (`GeneratePlan`): Groups issues into parallelizable phases by topological level. Applies filters (status, label, root scoping). Splits phases by file collision detection -- issues touching the same file are placed in separate sub-phases.

4. **Ready issue detection** (`FindReady`): Finds unblocked leaf tasks (no children, all blockers done) in specified statuses. Sorted by priority (highest first), then ID (oldest first).

5. **Adjacency helpers** (`BuildAdjacency`): Builds forward/backward adjacency lists for graph traversal (used by `issue graph`).

### 3.7 Output Layer (`internal/output`)

**`Writer`** struct dispatches between JSON and human output modes:

- **JSON mode** (`--json`): Wraps all output in a standardized envelope: `{"ok": true, "data": ..., "message": "..."}` for success, `{"ok": false, "error": "...", "code": "..."}` for errors. Written to stdout.
- **Human mode**: Styled terminal output using lipgloss. Success messages get a checkmark prefix; multi-line content (tables, boards) is printed as-is. Errors go to stderr with styled prefix.
- **Quiet mode** (`--quiet`): Suppresses info messages. Warnings still emit in human mode.
- **Error codes**: `GENERAL_ERROR`, `NOT_FOUND`, `VALIDATION_ERROR`, `CONFLICT` -- mapped to exit codes 1-4.

### 3.8 Render Layer (`internal/render`)

Terminal rendering with automatic color detection and plain-text fallback:

- **Board** (`board.go`): Kanban columns by status. Color mode uses lipgloss with rounded border cards showing kind icon, ID, priority icon, title, labels, and sub-issue progress bars. Adapts to terminal width. Caps at 10 cards per column with "+N more" overflow.
- **Table** (`table.go`): Tabular issue list with aligned columns.
- **Detail** (`detail.go`): Single-issue detail view with markdown rendering.
- **Markdown** (`markdown.go`): Renders markdown content using `charmbracelet/glamour`.
- **Vote** (`vote.go`): Proposal and vote result rendering.

All renderers check `ColorsEnabled()` and fall back to plain text for non-TTY environments.

### 3.9 Filter Utilities (`internal/filter`)

Shared helpers for filtering across commands:
- `ToStringSet()`: Converts string slices to maps for O(1) membership checks.
- `HasAllLabels()`: AND-logic label matching (issue must have all required labels).

## 4. Data Flow

### Typical Command Execution

```
1. main() -> cli.Execute() -> cobra dispatches to command
2. PersistentPreRunE:
   a. config.Resolve() -> Config{DocketDir, DBPath}
   b. db.Open(DBPath)  -> *sql.DB (WAL, FK, busy_timeout)
   c. db.Migrate(conn) -> sequential schema migrations
3. Command RunE:
   a. Read flags
   b. Query db layer
   c. Apply business logic (planner, filter)
   d. Render output (JSON envelope or human-styled)
4. PersistentPostRunE: conn.Close()
5. Return exit code
```

### Export/Import Cycle

```
Export: DB -> ListAll*() queries -> ExportData struct -> JSON/CSV/Markdown
Import: JSON file -> Unmarshal + validate -> Transaction -> InsertWithID (idempotent in merge mode)
```

## 5. External Dependencies (Runtime)

| Dependency | Purpose | Notes |
|-----------|---------|-------|
| `modernc.org/sqlite` | SQLite database driver | Pure Go, no CGO required |
| `spf13/cobra` | CLI framework | Command routing, flag parsing |
| `charmbracelet/lipgloss` | Terminal styling | Colors, borders, layout |
| `charmbracelet/glamour` | Markdown rendering | Used for issue descriptions |
| `charmbracelet/huh` | Interactive forms | Issue creation, import confirmation |
| `dustin/go-humanize` | Human-friendly formatting | Time display |
| `golang.org/x/term` | Terminal dimensions | Board column width calculation |

## 6. Build and Distribution

- **Build**: `CGO_ENABLED=0 go build` with ldflags for version/commit/date injection.
- **Targets**: 4 platform targets via Vorpal: `{aarch64,x86_64}-{darwin,linux}`.
- **Dev environment**: Vorpal SDK manages Go toolchain, staticcheck, goimports, gopls, protoc, vhs, ttyd, ffmpeg.
- **Makefile targets**: `build`, `test`, `lint`, `vet`, `install`, `clean`, `demo`.
- **CI**: GitHub Actions workflows present (`.github/workflows/`).

## 7. Architectural Gaps and Observations

- **No authentication/authorization**: By design (local-first), there is no access control. The database file is readable/writable by anyone with filesystem access.
- **No server/API mode**: Purely CLI. No REST API, gRPC, or web interface exists.
- **Single-writer constraint**: `MaxOpenConns(1)` means only one CLI process should write at a time. WAL mode allows concurrent reads but the 5s busy_timeout is the only concurrency guard.
- **No automated backups**: No built-in mechanism for snapshots or backup beyond the export command.
- **Activity log is write-only**: The `activity_log` table records field changes but the `issue log` command is the only consumer. No undo/rollback capability is built on top of it.
- **Schema migration is forward-only**: No down-migrations. Rollback requires restoring from a backup or export.
- **Vote system (v2-v3) is relatively new**: Added via recent migrations with a repair mechanism, suggesting rapid iteration. The PBFT-inspired model is sophisticated for a local tool and appears designed for multi-agent AI workflow integration.
- **No test for CLI commands**: Tests exist for `db`, `model`, `planner`, `render`, and `output` packages but not for the `cli` package itself (command integration tests are absent).
- **Protobuf tooling in dev env but no .proto files**: The Vorpal build includes protoc and protoc-gen-go but no `.proto` files exist in the repository, suggesting planned but not yet implemented gRPC support.
