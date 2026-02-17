# Docket

[![Go](https://img.shields.io/badge/Go-1.24.2+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

**Issue tracking for AI and humans.**

Docket is a local-first, SQLite-backed CLI issue tracker that lives inside your repository. It provides enterprise-grade issue tracking without requiring a server, network connection, or third-party service.

Docket serves two audiences equally: **human developers** who want a fast, beautiful terminal experience with rich lipgloss styling, and **AI coding agents** that need structured, machine-readable output to plan and execute work. Every command renders colorful, styled output by default and clean, parseable JSON with `--json`. Neither mode is an afterthought. Both are first-class.

## Quick Start

```bash
# Initialize a new issue database in the current directory
docket init

# Create an issue with priority and type
docket issue create -t "Build auth module" -p high -T feature

# Create a sub-issue that depends on the first
docket issue create -t "Set up database schema" -p high -T task --parent DKT-1
docket issue link add DKT-2 depends-on DKT-1

# See what's ready to work on (unblocked, sorted by priority)
docket next

# View the Kanban board
docket board
```

**AI agents:** add `--json` to any command for structured, machine-readable output:

```bash
docket next --json
docket issue list --json -s todo -s in-progress
```

## Installation

### From source

Requires Go 1.24.2+.

```bash
# Build the binary to ./bin/docket
make build
./bin/docket --help

# Install to $GOPATH/bin
make install
```

## Why Docket?

- **No servers, no network** — everything is a local SQLite file in `.docket/`. Works offline, on planes, in CI.
- **AI-native from day one** — every command supports `--json` with a consistent envelope. Agents can create, query, plan, and update issues without parsing human text.
- **Dependency-aware planning** — `docket next` and `docket plan` use a DAG to surface only unblocked, work-ready issues. No stale sprint boards.
- **Zero configuration** — `docket init` and you're done. No accounts, no tokens, no YAML.
- **Portable data** — the `.docket/` directory travels with your repo. Clone it, fork it, archive it.

## AI Agent Integration

Every command supports `--json` for structured, machine-readable output. All JSON responses use a consistent envelope:

**Success:** `{"ok": true, "data": { ... }, "message": "..."}`

**Error:** `{"ok": false, "error": "...", "code": "NOT_FOUND"}`

Error codes: `GENERAL_ERROR` (exit 1), `NOT_FOUND` (exit 2), `VALIDATION_ERROR` (exit 3), `CONFLICT` (exit 4).

### Recommended Agent Workflow

1. **Read the backlog** — `docket next --json` to get unblocked, priority-sorted issues.
2. **Pick an issue** — `docket issue show DKT-N --json` to read full details, sub-issues, and relations.
3. **Work on it** — `docket issue move DKT-N in-progress --json` to signal you've started.
4. **Complete it** — `docket issue close DKT-N --json` when done, or `docket issue move DKT-N review --json` if it needs review.
5. **Check what's next** — `docket next --json` again. Dependencies auto-resolve; newly unblocked work appears.

### Configuring Claude Code

Add to your project's `CLAUDE.md`:

```markdown
Use `docket` for issue tracking. Always pass `--json` when reading or updating issues.
Run `docket next --json` to find work. Move issues to `in-progress` before starting.
```

### Configuring Other Agents

Any agent that can run shell commands works with Docket. Point it at `docket next --json` to discover work items, and use `docket issue show <id> --json` to get full context before starting a task. The consistent JSON envelope (`ok`, `data`, `error`, `code`) makes parsing straightforward in any language.

<details>
<summary>Verbose JSON examples</summary>

#### Create an issue

```bash
docket issue create --json -t "Build auth module" -p high -T feature --parent DKT-3
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

#### Get work-ready issues

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

#### Compute execution plan

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

#### List issues with filters

```bash
docket issue list --json -s todo -s in-progress -p high
```

</details>

<details>
<summary><h2>Command Reference</h2></summary>

### Global Flags

```
--json        Structured JSON output (for agents and scripts)
--quiet, -q   Suppress non-essential output
```

### Issue Commands (`docket issue` / `docket i`)

| Command | Description |
|---------|-------------|
| `docket issue create` | Create a new issue (interactive or via flags) |
| `docket issue list` / `docket issue ls` | List issues with filtering and sorting |
| `docket issue show <id>` | Show full issue detail with sub-issues, relations, comments |
| `docket issue edit <id>` | Edit issue fields |
| `docket issue move <id> <status>` | Change issue status |
| `docket issue close <id>` | Shorthand for `move <id> done` |
| `docket issue reopen <id>` | Shorthand for `move <id> todo` |
| `docket issue delete <id>` | Delete an issue (with confirmation prompt) |
| `docket issue log <id>` | View activity history for an issue |

### Comments (`docket issue comment`)

| Command | Description |
|---------|-------------|
| `docket issue comment add <id>` | Add a comment (`-m` for inline, stdin, or `$EDITOR`) |
| `docket issue comment list <id>` | List all comments on an issue |

### Labels (`docket issue label`)

| Command | Description |
|---------|-------------|
| `docket issue label add <id> <label>...` | Add labels to an issue |
| `docket issue label rm <id> <label>...` | Remove labels from an issue |
| `docket issue label list` | List all labels in the database |
| `docket issue label delete <label>` | Delete a label entirely |

### Relations (`docket issue link`)

| Command | Description |
|---------|-------------|
| `docket issue link add <id> <relation> <target_id>` | Create a relation (blocks, depends-on, relates-to, duplicates) |
| `docket issue link remove <id> <relation> <target_id>` | Remove a relation |
| `docket issue link list <id>` | Show all relations for an issue |

### Graph (`docket issue graph`)

| Command | Description |
|---------|-------------|
| `docket issue graph <id>` | Show the dependency graph for an issue |

### Files (`docket issue file`)

| Command | Description |
|---------|-------------|
| `docket issue file add <id> <path>...` | Attach files to an issue |
| `docket issue file rm <id> <path>...` | Remove file attachments from an issue |
| `docket issue file list <id>` | List file attachments on an issue |

### Planning Commands

| Command | Description |
|---------|-------------|
| `docket next` | Show work-ready issues (unblocked, sorted by priority) |
| `docket plan` | Compute a phased execution plan from the dependency graph |
| `docket board` | Kanban board view in the terminal |

### Top-Level Commands

| Command | Description |
|---------|-------------|
| `docket init` | Initialize `.docket/` directory and database |
| `docket config` | Show current configuration (database path, schema version, etc.) |
| `docket version` | Print version, commit, and build date |
| `docket stats` | Show summary statistics for the issue database |

### Export / Import

| Command | Description |
|---------|-------------|
| `docket export` | Export issues as JSON (default), CSV, or Markdown |
| `docket import <file>` | Import issues from a JSON export file |

</details>

## Configuration

### Repository Setup

Add `.docket/` to your `.gitignore` to keep the issue database local:

```bash
echo ".docket/" >> .gitignore
```

> **Note:** `docket init` will remind you to do this if `.docket/` is not already ignored.

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

## Architecture

```
cmd/
  docket/          Entry point (main.go)
internal/
  cli/             Cobra command definitions (one file per command)
  config/          Configuration resolution (DOCKET_PATH, defaults)
  db/              SQLite queries and migrations
  filter/          Shared filtering helpers
  model/           Domain types (Issue, Status, Priority, Activity, etc.)
  output/          JSON envelope writer (ok/data/error/code)
  planner/         DAG builder, topological sort, phase planner
  render/          Lipgloss-based terminal rendering (tables, board, graphs)
scripts/
  qa.sh            End-to-end QA test suite
```

## Contributing

### Development Setup

```bash
git clone https://github.com/ALT-F4-LLC/docket.git
cd docket
make build          # Build to ./bin/docket
make test           # Run unit tests
make lint           # Run staticcheck + go vet
make clean          # Remove build artifacts
```

### Running the QA Suite

The QA suite exercises the full CLI end-to-end:

```bash
./scripts/qa.sh                       # Run all sections
./scripts/qa.sh --verbose             # Show all results, not just failures
./scripts/qa.sh ./bin/docket G        # Run a single section (with prerequisites)
```

### Guidelines

- **One file per command** in `internal/cli/` (e.g., `issue_create.go`, `board.go`).
- **Every command must support `--json`** using the shared `output.Writer` envelope.
- **Terminal styling** uses lipgloss via helpers in `internal/render/`.
- **Add QA checks** to `scripts/qa.sh` for any new command or flag.

## License

Apache 2.0. See [LICENSE](LICENSE) for the full text.
