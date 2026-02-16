# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Development Commands

```bash
make build                    # Build to ./bin/docket (CGO_ENABLED=0, ldflags for version)
make test                     # Run all tests (go test ./...)
make lint                     # Run staticcheck + go vet
make install                  # Install to $GOPATH/bin
go test -v ./internal/db/     # Run tests for a single package
go test -run TestCycleDetection ./internal/db/  # Run a single test
```

## Architecture

Docket is a local-first, SQLite-backed CLI issue tracker. It runs entirely inside a repository (`$PWD/.docket/issues.db`) with no server or network. Every command supports dual-mode output: styled terminal (lipgloss) for humans, structured JSON envelopes (`--json`) for agents.

### Package Layout

- **`cmd/docket/main.go`** — Entry point, calls `cli.Execute()`
- **`internal/cli/`** — All Cobra command definitions. The root command (`root.go`) opens/closes the DB via `PersistentPreRunE`/`PersistentPostRunE` and injects it into `context.Context`. Commands annotated with `skipDB` bypass DB setup (e.g., `init`, `version`).
- **`internal/db/`** — SQLite data access layer. Single-writer with WAL mode, foreign keys, busy timeout. Schema migrations in `schema.go`. All queries use parameterized SQL.
- **`internal/model/`** — Domain types: Issue (ID format `DKT-N`), Status (5 states: backlog→todo→in-progress→review→done), Priority, IssueKind, Relation, Comment, Activity, Label. JSON marshaling uses formatted IDs.
- **`internal/output/`** — Dual-mode Writer pattern. JSON mode emits `{ok, data, message}` or `{ok, error, code}` envelopes. Error codes: `GENERAL_ERROR` (exit 1), `NOT_FOUND` (exit 2), `VALIDATION_ERROR` (exit 3), `CONFLICT` (exit 4).
- **`internal/render/`** — Terminal renderers: table (list view), board (kanban), detail (full issue), markdown (export). Uses lipgloss for styling with graceful degradation.
- **`internal/planner/`** — DAG construction from issue relations, topological sort into parallelizable phases, file collision detection that splits phases into sequential sub-phases when issues share files.
- **`internal/config/`** — Resolves `DOCKET_PATH` env var, falls back to `$PWD/.docket`. Author from `git config user.name` or OS username.
- **`internal/filter/`** — Set operations and label matching for issue filtering.

### Key Patterns

- **Context-based DI**: DB connection and config are stored in `context.Context` via the root command's `PersistentPreRunE`, retrieved with `getDB(cmd)` and `getCfg(cmd)`.
- **CmdError**: Commands return `*CmdError` wrapping an error with an `ErrorCode`. The root `Execute()` function dispatches to the output writer accordingly.
- **Relation cycle detection**: `blocks` and `depends_on` relations are checked for cycles before insertion. A SQLite trigger prevents inverse duplicates.
- **Plan generation**: Issues are topologically sorted by dependency into phases. Within each phase, file collision detection further splits into sub-phases so issues touching the same files run sequentially.

### Testing

Tests use the standard `testing` package with in-memory SQLite (`:memory:`) for isolation. Helper pattern: `mustOpen(t)` returns a connection with `t.Cleanup()` for teardown. Table-driven tests are used throughout.

## Issue Tracking

This repository uses Docket itself for issue tracking. Issues live in `.docket/issues.db` and are managed via the `docket` CLI. See **`AGENTS.md`** for the complete agent workflow, including session initialization, claiming work, file attachment rules, and session completion requirements.

**Critical rule:** Always attach files to an issue before editing them — this enables the planner's file collision detection for parallel work.
