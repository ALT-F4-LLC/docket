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

#### Section F: JSON Contract Validation

Using the database from Section B, validate JSON structure more deeply.

| # | Command | Validate |
|---|---------|----------|
| F1 | `docket version --json` | Top-level keys: `ok`, `data`, `message`. `data` has `version`, `commit`, `build_date` |
| F2 | `docket config --json` | Top-level keys: `ok`, `data`. `data` has `db_path`, `db_size_bytes`, `schema_version`, `issue_prefix`, `docket_path_env`, `docket_path_set` |
| F3 | `docket init --json` | Top-level keys: `ok`, `data`, `message`. `data` has `path`, `db_path`, `schema_version`, `created` |

#### Section G: Exit Codes

| # | Scenario | Expected Exit Code |
|---|----------|-------------------|
| G1 | `docket version` | 0 |
| G2 | `docket config` (with DB) | 0 |
| G3 | `docket init` (with DB) | 0 |

#### Section H: Error Paths

| # | Command | Expected |
|---|---------|----------|
| H1 | `docket config` with no DB and no DOCKET_PATH | Exit 0, warns about missing database |
| H2 | `docket --help` with no DB | Exit 0, help text renders |

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
