---
project: "docket"
maturity: "experimental"
last_updated: "2026-03-21"
updated_by: "@staff-engineer"
scope: "Testing strategy, tooling, and coverage across unit and end-to-end test layers"
owner: "@staff-engineer"
dependencies:
  - code-quality.md
---

# Testing Specification

## Overview

Docket employs a two-tier testing strategy: **Go unit/integration tests** (`go test ./...`) for
internal package logic, and a **shell-based QA suite** (`scripts/qa/`) for end-to-end CLI
validation. There is no separate integration test tier (e.g., against a real on-disk database)
beyond what the unit tests achieve with in-memory SQLite. There is no CI pipeline configured in the
repository -- all tests are run locally via `make test` or by executing the QA scripts directly.

## Test Pyramid

| Layer | Count | Location | Runner |
|---|---|---|---|
| Unit / integration (Go) | ~205 test functions | `internal/**/*_test.go` (5 packages) | `go test ./...` / `make test` |
| End-to-end (shell) | 29 scripts, ~600 assertions | `scripts/qa/test_*.sh` | Bash, sourcing `scripts/qa/helpers.sh` |
| CI | None | N/A | N/A |

The pyramid is bottom-heavy in raw test count but the QA suite provides significant breadth
across every CLI command, exercising the full binary as a black-box.

## Unit / Integration Tests (Go)

### Packages With Tests

| Package | Test Functions | What Is Tested |
|---|---|---|
| `internal/db` | ~97 | Database open/config (WAL, FK, busy_timeout), schema initialization, idempotency, migrations, CRUD for issues/relations/files/proposals/votes, cascade deletes, unique constraints, cycle detection, activity log recording, export/import |
| `internal/model` | ~42 | ID formatting/parsing round-trips, enum validation (status, priority, kind, criticality, verdict), color/icon methods, JSON serialization round-trips for Issue/Comment/Proposal/Vote/Findings, backward compatibility with v2 JSON |
| `internal/output` | ~23 | JSON envelope marshaling (success/error), Writer modes (JSON/human/quiet), exit code mapping, NO_COLOR/TERM environment handling, multi-line vs single-line output formatting |
| `internal/render` | ~36 | Board rendering (plain-text and color paths), column ordering, title truncation, priority indicators, sub-issue progress bars, overflow indicators, grouped table rendering with parent-child relationships, empty states |
| `internal/planner` | ~7 | `splitByFileCollision` algorithm: empty input, no files, no conflicts, all-shared-file, mixed conflicts, multi-file collisions |

### Packages Without Tests

| Package | Notes |
|---|---|
| `cmd/docket` | CLI command wiring -- covered indirectly by QA shell suite |
| `cmd/vorpal` | Vorpal integration entry point -- no tests |
| `internal/cli` | CLI framework / command registration -- covered indirectly by QA shell suite |
| `internal/config` | Configuration loading -- partially covered by QA tests (sections A, C, D) |
| `internal/filter` | Issue filtering logic -- covered indirectly by QA list/board tests |

### Test Patterns and Conventions

- **In-memory SQLite**: All `internal/db` tests use `:memory:` databases via a `mustOpen(t)`
  helper. This avoids filesystem side effects and enables parallel execution but means WAL mode
  behavior is not fully exercised (in-memory reports `journal_mode = memory`).
- **Standard library only**: Tests use `testing.T` exclusively -- no third-party assertion
  libraries (testify, gomega, etc.) or mocking frameworks.
- **Table-driven tests**: Used extensively in `internal/model` (ParseID, validation) and
  `internal/db` (cycle detection). Subtests via `t.Run()` are used for cycle detection scenarios.
- **Test helpers**: `mustOpen`, `mustCreateIssue`, `mustCreateRelation`, `createTestIssue`,
  `makeIssue`, `makeTestIssue` provide DRY setup. Helpers call `t.Helper()` for correct line
  reporting.
- **Cleanup via `t.Cleanup`**: Database connections are closed automatically.
- **Environment manipulation**: `t.Setenv("NO_COLOR", "1")` is used in render/output tests to
  get deterministic plain-text output.
- **No mocking**: Tests exercise real production code paths. Database tests hit real (in-memory)
  SQLite. No mock interfaces or test doubles are used anywhere.
- **No test fixtures or golden files**: Expected values are inline in test functions.
- **No benchmarks**: No `Benchmark*` functions exist in the codebase.
- **No fuzz tests**: No `Fuzz*` functions exist in the codebase.

### Test Execution

```sh
make test       # runs: go test ./...
go vet ./...    # run via: make vet (also a prereq of make lint)
```

No test flags (race detector, coverage, timeout) are configured by default in the Makefile.

## End-to-End QA Suite (Shell)

### Structure

The QA suite lives in `scripts/qa/` and consists of:

- **`helpers.sh`**: Shared test infrastructure providing:
  - `setup()` / `cleanup()`: Creates/destroys temp directories, sets `DOCKET_PATH`
  - `run()` / `run_env()` / `run_stdin()`: Command runners that capture stdout, stderr, exit code
  - `assert_exit()`, `assert_json()`, `assert_json_exists()`, `assert_json_null()`,
    `assert_json_array_min()`, `assert_json_array_max()`, `assert_json_all()`: Assertion functions
  - `assert_stdout_contains()`, `assert_stderr_contains()`: Output content assertions
  - `extract_id()`: Extracts numeric ID from JSON `data.id` field
  - `check()`: Records PASS/FAIL results with counters

- **29 test scripts** (`test_a_no_db.sh` through `test_zc_export_import.sh`): Alphabetically
  ordered, each testing a specific feature area. Scripts are designed to run sequentially as
  some depend on state created by earlier scripts.

### Coverage by Section

| Script | Area | Key Scenarios |
|---|---|---|
| `test_a` | No-DB commands | version, help, config without initialized DB |
| `test_b` | Init | Database initialization |
| `test_c` | Config | Configuration display |
| `test_d` | Path override | `DOCKET_PATH` env var |
| `test_e` | Quiet mode | Suppressed output |
| `test_f` | Create | Issue creation with all field combinations, validation errors |
| `test_g` | List | Issue listing, filtering |
| `test_h` | Show | Issue detail view |
| `test_i` | Move | Status transitions |
| `test_j` | Close | Closing issues |
| `test_k` | Reopen | Reopening issues |
| `test_l` | Edit | Field editing |
| `test_m` | Edit reparent | Parent-child reparenting, cycle prevention |
| `test_n` | Delete simple | Simple deletion |
| `test_o` | Delete cascade | Cascade and orphan deletion modes |
| `test_p` | Activity | Activity log entries |
| `test_q` | JSON contracts | Validates JSON response schema for every command |
| `test_r` | Exit codes | Validates exit code conventions (0/2/3/4) across commands |
| `test_s` | Error paths | Error handling for missing DB, invalid inputs |
| `test_t` | Comment | Single comment operations |
| `test_u` | Comments | Comment listing |
| `test_v` | Label | Label management |
| `test_w` | Link | Issue relations / linking |
| `test_x` | Next | Next-up issue suggestion |
| `test_y` | Plan | Execution plan generation |
| `test_z` | Graph | Dependency graph |
| `test_za` | Stats | Statistics command |
| `test_zb` | Board | Board view |
| `test_zc` | Export/Import | Data export and import |

### QA Execution Model

- Scripts are pure Bash, requiring `jq` for JSON assertions
- Each script defines a single function (e.g., `test_f_create()`) that is called by a runner
- Tests operate against a fresh temp directory per suite run
- Tests are stateful and ordered -- later scripts may depend on issues created by earlier ones
- No parallelism -- scripts run sequentially

## Gaps and Honest Assessment

### No CI Pipeline

There is no `.github/workflows/`, `.gitlab-ci.yml`, or equivalent. Tests are run manually.
This means there is no automated gate preventing regressions from being merged.

### No Coverage Tracking

No coverage tool is configured. There is no coverage report, no coverage threshold, and no
visibility into which code paths are exercised. Running `go test -cover ./...` would produce
coverage data but this is not part of the standard workflow.

### No Race Detector

`go test -race` is not configured in the Makefile or run by default. Given that docket is a
CLI tool (not a server), race conditions are unlikely but not impossible in concurrent database
access scenarios.

### Missing Unit Tests for CLI/Config/Filter Packages

Five packages (`cmd/docket`, `cmd/vorpal`, `internal/cli`, `internal/config`, `internal/filter`)
have zero Go unit tests. The `cmd/` and `internal/cli` packages are partially covered by the
QA shell suite, but `internal/filter` logic is only tested indirectly through CLI commands.

### No Benchmarks or Fuzz Tests

There are no performance benchmarks or fuzz tests. For a CLI tool this is reasonable, but the
`internal/db` layer (especially list queries with complex sorting) could benefit from benchmarks
as issue counts grow.

### QA Suite Is Stateful

The shell QA suite's sequential, stateful nature means individual test scripts cannot be run
in isolation (later scripts depend on database state from earlier ones). This makes debugging
failures harder and prevents parallelization.

### In-Memory vs On-Disk SQLite

All Go database tests use `:memory:` SQLite databases. This is fast and side-effect-free but
does not exercise WAL mode, file locking, or disk I/O behaviors that occur in production. The
QA shell suite partially fills this gap by running the real binary against temp-dir databases.

## Test Utilities Summary

| Utility | Location | Purpose |
|---|---|---|
| `mustOpen(t)` | `internal/db/db_test.go` | Opens in-memory DB, registers cleanup |
| `mustCreateIssue(t, db, title)` | `internal/db/relations_test.go` | Creates minimal issue |
| `mustCreateRelation(t, db, s, t, rt)` | `internal/db/relations_test.go` | Creates relation |
| `createTestIssue(t, db, ...)` | `internal/db/issues_test.go` | Creates issue with status/priority |
| `createTestIssueWithParent(...)` | `internal/db/issues_test.go` | Creates child issue |
| `makeIssue(id, title, ...)` | `internal/render/board_test.go` | Creates minimal model.Issue |
| `makeTestIssue(...)` | `internal/render/table_test.go` | Creates model.Issue with kind/parent |
| `intPtr(i)` | `internal/render/table_test.go` | Helper for *int values |
| `helpers.sh` | `scripts/qa/helpers.sh` | Full QA test infrastructure |
