# Docket

**Issue tracking for ai and humans.**

Docket is a local-first, SQLite-backed CLI issue tracker that lives inside your repository. It provides enterprise-grade issue tracking without requiring a server, network connection, or third-party service.

Docket serves two audiences equally: **human developers** who want a fast, beautiful terminal experience with rich lipgloss styling, and **AI coding agents** that need structured, machine-readable output to plan and execute work. Every command renders colorful, styled output by default and clean, parseable JSON with `--json`. Neither mode is an afterthought. Both are first-class.

## Quick Start

```bash
# Initialize a new issue database in the current directory
docket init

# Create your first issue
docket create -t "My first issue"

# List all open issues
docket list

# Show full details for an issue
docket show DKT-1
```

## Installation

### From source

```bash
make build
./bin/docket --help
```

To install to `$GOPATH/bin`:

```bash
make install
```

## Command Reference

### Global Flags

```
--json        Structured JSON output (for agents and scripts)
--quiet, -q   Suppress non-essential output
```

### Core Commands

| Command | Description |
|---------|-------------|
| `docket init` | Initialize `.docket/` directory and database |
| `docket create` | Create a new issue (interactive or via flags) |
| `docket list` / `docket ls` | List issues with filtering and sorting |
| `docket show <id>` | Show full issue detail with sub-issues, relations, comments |
| `docket edit <id>` | Edit issue fields |
| `docket move <id> <status>` | Change issue status |
| `docket close <id>` | Shorthand for `move <id> done` |
| `docket reopen <id>` | Shorthand for `move <id> todo` |
| `docket delete <id>` | Delete an issue (with confirmation prompt) |

### Comments

| Command | Description |
|---------|-------------|
| `docket comment <id>` | Add a comment (`-m` for inline, stdin, or `$EDITOR`) |
| `docket comments <id>` | List all comments on an issue |

### Labels

| Command | Description |
|---------|-------------|
| `docket label add <id> <label>...` | Add labels to an issue |
| `docket label rm <id> <label>...` | Remove labels from an issue |
| `docket label list` | List all labels in the database |
| `docket label delete <label>` | Delete a label entirely |

### Relations

| Command | Description |
|---------|-------------|
| `docket link <id> <relation> <target_id>` | Create a relation (blocks, depends-on, relates-to, duplicates) |
| `docket unlink <id> <relation> <target_id>` | Remove a relation |
| `docket links <id>` | Show all relations for an issue |

### Planning

| Command | Description |
|---------|-------------|
| `docket next` | Show work-ready issues (unblocked, sorted by priority) |
| `docket plan` | Compute a phased execution plan from the dependency graph |
| `docket graph <id>` | Show the dependency graph for an issue |

### Board

| Command | Description |
|---------|-------------|
| `docket board` | Kanban board view in the terminal |

### Export / Import

| Command | Description |
|---------|-------------|
| `docket export` | Export issues as JSON (default), CSV, or Markdown |
| `docket import <file>` | Import issues from a JSON export file |

### Meta

| Command | Description |
|---------|-------------|
| `docket stats` | Show summary statistics for the issue database |
| `docket version` | Print version, commit, and build date |
| `docket config` | Show current configuration (database path, schema version, etc.) |

## Agent Usage

Every command supports `--json` for structured, machine-readable output. All JSON responses use a consistent envelope:

**Success:**

```json
{
  "ok": true,
  "data": { ... },
  "message": "Issue DKT-5 created"
}
```

**Error:**

```json
{
  "ok": false,
  "error": "Issue DKT-99 not found",
  "code": "NOT_FOUND"
}
```

Error codes: `GENERAL_ERROR` (exit 1), `NOT_FOUND` (exit 2), `VALIDATION_ERROR` (exit 3), `CONFLICT` (exit 4).

### Example: Create an issue

```bash
docket create --json -t "Build auth module" -p high -T feature --parent DKT-3
```

```json
{
  "ok": true,
  "data": {
    "id": "DKT-12",
    "parent_id": "DKT-3",
    "title": "Build auth module",
    "description": "",
    "status": "backlog",
    "priority": "high",
    "kind": "feature",
    "assignee": "",
    "labels": [],
    "created_at": "2026-02-13T18:30:00Z",
    "updated_at": "2026-02-13T18:30:00Z"
  },
  "message": "Created DKT-12"
}
```

### Example: Get work-ready issues

```bash
docket next --json
```

```json
{
  "ok": true,
  "data": {
    "issues": [
      {
        "id": "DKT-7",
        "title": "Set up database schema",
        "status": "todo",
        "priority": "high",
        "kind": "task",
        "assignee": "",
        "labels": [],
        "created_at": "2026-02-13T18:30:00Z",
        "updated_at": "2026-02-13T18:30:00Z"
      }
    ],
    "total": 1
  }
}
```

### Example: Compute execution plan

```bash
docket plan --json --root DKT-3
```

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
            "status": "todo"
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

### Example: List issues with filters

```bash
docket list --json -s todo -s in-progress -p high
```

## Build from Source

### Prerequisites

- Go 1.24.2+

### Commands

```bash
# Build the binary to ./bin/docket
make build

# Run tests
make test

# Run linter (staticcheck + go vet)
make lint

# Install to $GOPATH/bin
make install

# Clean build artifacts
make clean
```

## Configuration

### Database Location

Docket stores its SQLite database at `$PWD/.docket/issues.db` by default. This means each project gets its own isolated issue database.

Override the location with the `DOCKET_PATH` environment variable:

```bash
export DOCKET_PATH=/path/to/custom/.docket
```

When set, `DOCKET_PATH` points to the `.docket` directory (the database file will be `$DOCKET_PATH/issues.db`).

Use `docket config` to verify the resolved database path and whether `DOCKET_PATH` is active.

### Statuses

Issues follow a Kanban workflow with five statuses:

| Status | Description |
|--------|-------------|
| `backlog` | Acknowledged but not yet planned (default) |
| `todo` | Planned for current work cycle |
| `in-progress` | Actively being worked on |
| `review` | Work complete, pending review |
| `done` | Finished |

### Priorities

| Priority | Description |
|----------|-------------|
| `critical` | Immediate attention required |
| `high` | Important, address soon |
| `medium` | Normal priority |
| `low` | Address when convenient |
| `none` | No priority set (default) |

### Issue Types

| Type | Description |
|------|-------------|
| `bug` | Defect or broken behavior |
| `feature` | New functionality |
| `task` | General work item (default) |
| `epic` | Large body of work with sub-issues |
| `chore` | Maintenance or housekeeping |

## Issue ID Format

Issues use the `DKT-N` format (e.g., `DKT-1`, `DKT-42`). The `DKT` prefix is constant and IDs auto-increment within each database. Commands accept both the full prefixed form (`DKT-5`) and the bare number (`5`).

## License

See [LICENSE](LICENSE) for details.
