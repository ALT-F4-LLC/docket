# Agent Instructions

This project uses **Docket**, a local-first SQLite-backed CLI issue tracker that lives inside the
repository. No server, no MCP, no network required. All commands support `--json` for structured
output and `-q`/`--quiet` to suppress non-essential output.

## Quick Reference

```
docket next --json             — Find work-ready issues (unblocked, prioritized)
docket issue show <id> --json  — View full issue detail
docket issue move <id> in-progress  — Claim work
docket issue close <id>        — Complete work (moves to done)
docket issue comment add <id> -m "summary"  — Add completion summary
```

## Session Initialization

At the start of every session, perform these steps before any work:

1. **Initialize (if needed):**
   - Run `docket init` to create the `.docket/` directory and database.
   - This is idempotent — safe to run even if already initialized.

2. **Verify configuration:**
   - Run `docket config` to confirm the current settings.

3. **Review current state:**
   - Run `docket board --json` for a Kanban overview of all issues by status.
   - Run `docket next --json` to see work-ready issues sorted by priority.
   - Run `docket stats` for a summary of issue counts and status distribution.

## Agent Workflow

### Finding Work

```bash
# Show work-ready issues (unblocked, sorted by priority)
docket next --json

# Filter by priority or type
docket next --json -p critical
docket next --json -T bug

# Full Kanban board view
docket board --json

# View execution plan with phased grouping
docket plan --json
```

### Claiming and Working Issues

```bash
# Move issue to in-progress (claim it)
docket issue move <id> in-progress

# View full issue details
docket issue show <id> --json

# Attach files you plan to edit (do this BEFORE making changes)
docket issue file add <id> path/to/file.go path/to/other_file.go

# Check issue dependency graph
docket issue graph <id>

# Add progress comments
docket issue comment add <id> -m "Started work on X"
```

**Important:** Always attach files to the issue before editing them. This creates a record of which
files were modified as part of each issue, enables collision detection if multiple issues touch the
same file, and provides traceability for code changes.

### Completing Work

```bash
# Close an issue (moves to done)
docket issue close <id>

# Add a completion summary
docket issue comment add <id> -m "Completed: <summary of what was done>"
```

### Creating Issues

```bash
# Create a new issue
docket issue create -t "Title" -d "Description" -p <priority> -T <type>

# Create with parent (subtask)
docket issue create -t "Subtask" --parent <parent_id> -p medium -T task

# Create with labels
docket issue create -t "Title" -l label1 -l label2 -T feature

# Create with blocking relationships
docket issue create -t "Title" -T task
docket issue link add <new_id> blocks <other_id>
```

### Managing Relationships

```bash
# Add a blocking relationship
docket issue link add <id> blocks <target_id>

# Add a "blocked by" relationship
docket issue link add <id> blocked-by <target_id>

# View relationships
docket issue link list <id>

# Remove a relationship
docket issue link remove <id> blocks <target_id>
```

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until all
finished issues are closed in Docket.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** — `docket issue create -t "..." -d "..." -p <priority> -T <type>`
2. **Run quality gates** (if code changed) — Tests, linters, builds
3. **Close finished work** — `docket issue close <id>` for each completed issue
4. **Add completion summaries** — `docket issue comment add <id> -m "Completed: <summary>"` for each closed issue
5. **Verify** — Run `docket board --json` to confirm all finished issues show status `done`
6. **Hand off** — Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `docket issue close` succeeds for all finished issues
- NEVER stop before closing issues — that leaves work stranded with no record of completion

## Full Command Reference

### Root-Level Commands

```
docket init                    — Initialize .docket/ directory and database
docket config                  — Show current configuration
docket version                 — Print version info
docket stats                   — Show summary statistics
docket export                  — Export issues (json, csv, markdown)
docket import <file>           — Import issues from JSON
docket plan                    — Execution plan with phased grouping (--root, -s, -l)
docket board                   — Kanban board view (-l, -p, -a, --expand)
docket next                    — Work-ready issues (-s, -p, -l, -T, --limit)
```

### Issue Commands (`docket issue` / `docket i`)

```
# CRUD
docket issue create            — Create issue (-t, -d, -s, -p, -T, -l, -f, -a, --parent)
docket issue list              — List issues (-s, -p, -l, -T, -a, --parent, --roots, --tree, --sort, --limit, --all)
docket issue show <id>         — Show full issue detail
docket issue edit <id>         — Edit issue (-t, -d, -s, -p, -T, -a, -f, --parent)
docket issue delete <id>       — Delete issue (-f, --orphan)

# Status transitions
docket issue move <id> <status>   — Change status
docket issue close <id>           — Shorthand for move <id> done
docket issue reopen <id>          — Shorthand for move <id> todo

# Activity
docket issue log <id>          — Activity history (--limit)
docket issue comment add <id>  — Add comment (-m)
docket issue comment list <id> — List comments

# Labels
docket issue label add <id> <label>...    — Add labels (--color)
docket issue label rm <id> <label>...     — Remove labels
docket issue label list                   — List all labels
docket issue label delete <label>         — Delete label (-f)

# Relationships
docket issue link add <id> <relation> <target_id>     — Create relation
docket issue link remove <id> <relation> <target_id>   — Remove relation
docket issue link list <id>                            — List relations

# File attachments
docket issue file add <id> <path>...   — Attach files
docket issue file rm <id> <path>...    — Remove file attachments
docket issue file list <id>            — List file attachments

# Visualization
docket issue graph <id>        — Dependency graph (--depth, --direction, --mermaid)
```

### Global Flags

| Flag | Description |
|---|---|
| `--json` | Structured JSON output (use for all agent queries) |
| `-q` / `--quiet` | Suppress non-essential output |

## Reference Tables

### Statuses

| Status | Meaning |
|---|---|
| `backlog` | Not yet scheduled |
| `todo` | Ready to work |
| `in-progress` | Actively being worked |
| `review` | Awaiting review |
| `done` | Completed |

### Priorities

| Priority | Flag Value |
|---|---|
| Critical | `-p critical` |
| High | `-p high` |
| Medium | `-p medium` (default) |
| Low | `-p low` |
| None | `-p none` |

### Issue Types

| Type | Flag Value | Use When |
|---|---|---|
| Bug | `-T bug` | Fixing broken behavior, errors, regressions |
| Feature | `-T feature` | Adding new functionality |
| Task | `-T task` | General work items, chores |
| Epic | `-T epic` | Large bodies of work with subtasks |
| Chore | `-T chore` | Maintenance, refactoring, documentation |

### Relation Types

| Relation | Meaning |
|---|---|
| `blocks` | This issue blocks the target |
| `blocked-by` | This issue is blocked by the target |
| `relates-to` | General relationship |
| `duplicates` | This issue duplicates the target |
