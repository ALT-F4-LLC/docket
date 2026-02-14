---
name: qa
description: >
  Run quality assurance checks on the docket CLI binary. Use this skill when the
  user asks to "run QA", "test the build", "verify the binary", "smoke test",
  "run quality assurance", "check the CLI", or wants to validate that docket is
  working correctly after a build.
---

# Docket QA Skill

Run a structured quality assurance process against a built docket binary. This skill
builds docket, then executes a series of functional checks across all commands and
flags, validating output, exit codes, and error handling.

ARGUMENTS: optional path to the docket binary. If not provided, build with `go build`
and use the resulting binary.

## Workflow

### 1. Build

If no binary path was provided as an argument, build docket:

```bash
go build -o /tmp/docket-qa-bin ./cmd/docket
```

Set the binary path for all subsequent commands. If the build fails, stop and report
the failure immediately.

### 2. Setup

Create a temporary working directory for QA so tests don't interfere with the user's
project state:

```bash
export QA_DIR=$(mktemp -d)
```

All commands that interact with the database should run with `DOCKET_PATH` pointed at
a subdirectory inside `QA_DIR`. Clean up `QA_DIR` at the end regardless of outcome.

### 3. Execute Test Sections

Run each section below **in order**. For every command:
- Capture stdout, stderr, and the exit code
- Compare against the expected behavior described
- Record PASS or FAIL with details

If a command fails unexpectedly, continue to the next check rather than aborting.

---

#### Section A: No-DB Commands

These commands must succeed without any database present.

| # | Command | Expected |
|---|---------|----------|
| A1 | `docket version` | Exit 0, prints version string to stdout |
| A2 | `docket version --json` | Exit 0, valid JSON with `ok: true` and fields: `version`, `commit`, `build_date` |
| A3 | `docket --help` | Exit 0, prints usage with available commands |
| A4 | `docket config` | Exit 0, warns "No docket database found", shows `(not found)` path |
| A5 | `docket config --json` | Exit 0, valid JSON with `ok: true`, `db_size_bytes: 0`, `schema_version: 0` |

#### Section B: Init Lifecycle

| # | Command | Expected |
|---|---------|----------|
| B1 | `docket init` | Exit 0, creates `$DOCKET_PATH/issues.db`, prints initialization message |
| B2 | Verify DB file | `$DOCKET_PATH/issues.db` exists and is non-empty |
| B3 | `docket init` (second time) | Exit 0, warns "Database already exists", does not error |
| B4 | `docket init --json` | Exit 0, valid JSON with `ok: true`, `created: false` (since DB exists) |

#### Section C: Config After Init

| # | Command | Expected |
|---|---------|----------|
| C1 | `docket config` | Exit 0, shows `db_path`, `schema_version`, `issue_prefix` |
| C2 | `docket config --json` | Exit 0, valid JSON with `schema_version: 1`, `issue_prefix: "DKT"`, `db_size_bytes > 0` |

#### Section D: DOCKET_PATH Override

Use a second temporary directory to verify the env var is respected.

| # | Command | Expected |
|---|---------|----------|
| D1 | `DOCKET_PATH=/tmp/docket-qa-alt docket init` | Exit 0, creates DB at the alternate path |
| D2 | `DOCKET_PATH=/tmp/docket-qa-alt docket config --json` | Exit 0, `db_path` contains `/tmp/docket-qa-alt` |
| D3 | Clean up `/tmp/docket-qa-alt` | Directory removed |

#### Section E: Quiet Mode

| # | Command | Expected |
|---|---------|----------|
| E1 | `docket init --quiet` (fresh DOCKET_PATH) | Exit 0, stderr output is suppressed (no info lines) |
| E2 | `docket config --quiet` | Exit 0, stderr output is suppressed |

#### Section F: Create Command

Using the database from Section B.

| # | Command | Expected |
|---|---------|----------|
| F1 | `docket create --json -t "QA Test Issue"` | Exit 0, valid JSON with `ok: true`. `data` has `id` starting with `DKT-`, `title: "QA Test Issue"`, `status: "backlog"`, `priority: "none"`, `kind: "task"`, `created_at`, `updated_at` |
| F2 | `docket create --json -t "High Priority Bug" -p high -T bug -s todo` | Exit 0, JSON `data.priority: "high"`, `data.kind: "bug"`, `data.status: "todo"` |
| F3 | `docket create --json -t "With Labels" -l "frontend" -l "urgent"` | Exit 0, JSON with `ok: true`, issue created successfully |
| F4 | `docket create --json -t "With Assignee" -a "alice"` | Exit 0, JSON `data.assignee: "alice"` |
| F5 | `docket create --json` (no --title) | Exit 3 (validation error), JSON with `ok: false`, `code: "VALIDATION_ERROR"` |
| F6 | `docket create --json -t "Sub-issue" --parent DKT-1` | Exit 0, JSON `data.parent_id: "DKT-1"` |
| F7 | `docket create --json -t "Bad Status" -s invalid` | Exit 3, validation error for invalid status |
| F8 | `docket create --json -t "Bad Priority" -p invalid` | Exit 3, validation error for invalid priority |
| F9 | `docket create --json -t "Bad Type" -T invalid` | Exit 3, validation error for invalid type |
| F10 | `docket create --json -t "Bad Parent" --parent 9999` | Exit 2 (not found), parent issue not found |
| F11 | `echo "stdin desc" \| docket create --json -t "Stdin Test" -d -` | Exit 0, JSON `data.description` contains "stdin desc" |

#### Section G: List Command

Using the database populated by Section F.

| # | Command | Expected |
|---|---------|----------|
| G1 | `docket list --json` | Exit 0, JSON with `ok: true`, `data.issues` is array, `data.total` is integer >= 1 |
| G2 | `docket ls --json` | Exit 0, same result as G1 (alias works) |
| G3 | `docket list --json -s todo` | Exit 0, all returned issues have `status: "todo"` |
| G4 | `docket list --json -p high` | Exit 0, all returned issues have `priority: "high"` |
| G5 | `docket list --json -T bug` | Exit 0, all returned issues have `kind: "bug"` |
| G6 | `docket list --json -a alice` | Exit 0, all returned issues have `assignee: "alice"` |
| G7 | `docket list --json --roots` | Exit 0, no returned issues have `parent_id` set |
| G8 | `docket list --json --parent DKT-1` | Exit 0, all returned issues have `parent_id: "DKT-1"` |
| G9 | `docket list --json --sort created_at:asc` | Exit 0, issues sorted by `created_at` ascending |
| G10 | `docket list --json --limit 2` | Exit 0, `data.issues` array has at most 2 elements |
| G11 | `docket list` (human mode) | Exit 0, stdout contains table with column headers (ID, Status, etc.) |
| G12 | `docket list --tree` | Exit 0, stdout contains tree-formatted output |

#### Section H: Show Command

| # | Command | Expected |
|---|---------|----------|
| H1 | `docket show 1 --json` | Exit 0, JSON with `ok: true`, `data.id: "DKT-1"`, `data.title`, `data.sub_issues` (array), `data.relations` (array), `data.comments` (array), `data.activity` (array) |
| H2 | `docket show DKT-1 --json` | Exit 0, same result as H1 (prefixed ID works) |
| H3 | `docket show 1` (human mode) | Exit 0, stdout contains the issue title and metadata |
| H4 | `docket show 9999 --json` | Exit 2 (not found), JSON with `ok: false`, `code: "NOT_FOUND"` |
| H5 | `docket show DKT-1 --json` | Exit 0, `data.activity` array is non-empty (creation activity recorded) |
| H6 | `docket show --json` (no ID) | Exit non-zero, error about missing argument |

#### Section I: JSON Contract Validation

Using the database from earlier sections, validate JSON structure more deeply.

| # | Command | Validate |
|---|---------|----------|
| I1 | `docket version --json` | Top-level keys: `ok`, `data`, `message`. `data` has `version`, `commit`, `build_date` |
| I2 | `docket config --json` | Top-level keys: `ok`, `data`. `data` has `db_path`, `db_size_bytes`, `schema_version`, `issue_prefix`, `docket_path_env`, `docket_path_set` |
| I3 | `docket init --json` | Top-level keys: `ok`, `data`, `message`. `data` has `path`, `db_path`, `schema_version`, `created` |
| I4 | `docket create --json -t "Contract Test"` | Top-level keys: `ok`, `data`, `message`. `data` has `id`, `title`, `description`, `status`, `priority`, `kind`, `assignee`, `created_at`, `updated_at`. All timestamps are ISO 8601 (RFC 3339). `id` matches `DKT-\d+` pattern |
| I5 | `docket list --json` | Top-level keys: `ok`, `data`. `data` has `issues` (array), `total` (integer). Each issue in array has `id`, `title`, `status`, `priority`, `kind`, `created_at`, `updated_at` |
| I6 | `docket show 1 --json` | Top-level keys: `ok`, `data`. `data` has `id`, `title`, `status`, `priority`, `kind`, `sub_issues`, `relations`, `comments`, `activity`. `sub_issues` is array, `activity` is array |

#### Section J: Exit Codes

| # | Scenario | Expected Exit Code |
|---|----------|-------------------|
| J1 | `docket version` | 0 |
| J2 | `docket config` (with DB) | 0 |
| J3 | `docket init` (with DB) | 0 |
| J4 | `docket create --json -t "Test"` | 0 |
| J5 | `docket create --json` (no title) | 3 (validation) |
| J6 | `docket list --json` | 0 |
| J7 | `docket show 1 --json` | 0 |
| J8 | `docket show 9999 --json` | 2 (not found) |

#### Section K: Error Paths

| # | Command | Expected |
|---|---------|----------|
| K1 | `docket config` with no DB and no DOCKET_PATH | Exit 0, warns about missing database |
| K2 | `docket --help` with no DB | Exit 0, help text renders |
| K3 | `docket create --json -t "No DB"` with no DB | Exit non-zero, error about missing database |
| K4 | `docket list --json` with no DB | Exit non-zero, error about missing database |
| K5 | `docket show 1 --json` with no DB | Exit non-zero, error about missing database |

### 4. Cleanup

Remove all temporary directories:

```bash
rm -rf "$QA_DIR" /tmp/docket-qa-alt /tmp/docket-qa-bin
```

### 5. Report

Print a summary table of all checks:

```
Section | Check | Result | Details
--------|-------|--------|--------
A       | A1    | PASS   |
A       | A2    | PASS   |
B       | B1    | FAIL   | Expected exit 0, got exit 1: <stderr>
...
```

End with a final summary line:

```
QA Result: X/Y checks passed
```

If any check failed, list the failures again at the bottom for visibility.
