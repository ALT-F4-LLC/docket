---
project: "docket"
maturity: "experimental"
last_updated: "2026-03-21"
updated_by: "@staff-engineer"
scope: "Code quality standards, conventions, tooling, and patterns observed in the docket codebase"
owner: "@staff-engineer"
dependencies:
  - testing.md
---

# Code Quality

## Overview

Docket is a Go CLI project (Go 1.26) structured as a single-module monorepo. The codebase follows idiomatic Go conventions with a clean separation between CLI, business logic, data access, and rendering layers. There are no third-party linter configurations checked into the repository (no `.golangci-lint.yaml`, `.editorconfig`, or similar). Static analysis relies on `go vet` and an optional `staticcheck` invocation via the Makefile.

## Static Analysis and Linting

### Current Tooling

| Tool | Invocation | Enforcement |
|------|-----------|-------------|
| `go vet` | `make vet` (also a prerequisite of `make lint`) | Local only; not enforced in CI |
| `staticcheck` | `make lint` — runs only if `staticcheck` binary is on `$PATH` | Optional; gracefully skipped if not installed |
| `go build` | `make build` — CGO_ENABLED=0 | CI builds via Vorpal on all target platforms |

### What Is Missing

- **No golangci-lint configuration.** There is no `.golangci-lint.yaml` or equivalent meta-linter setup. Linting is limited to `go vet` and optional `staticcheck`.
- **No CI lint step.** The GitHub Actions CI workflow (`ci.yaml`) only builds the project via Vorpal. It does not run `make lint`, `make vet`, or `make test`.
- **No formatter enforcement.** There is no `gofmt`/`goimports` check in CI or pre-commit hooks. Formatting is assumed to be handled by developer editors.
- **No pre-commit hooks.** No `.pre-commit-config.yaml` or git hooks are configured.
- **No `.editorconfig`.** Editor settings are not standardized across contributors.

## Naming Conventions

### Package Names

Packages use short, lowercase, single-word names following Go conventions:

- `cli` — cobra command definitions
- `db` — database access layer (SQLite)
- `model` — domain types and validation
- `config` — configuration resolution
- `filter` — issue filtering utilities
- `output` — output formatting dispatch (JSON/human)
- `render` — terminal rendering (board, table, detail views)
- `planner` — DAG construction and topological sorting

### Variable and Function Naming

- **Exported types** use PascalCase: `Issue`, `Proposal`, `Vote`, `Status`, `Priority`
- **Unexported types** use camelCase: `issueJSON`, `proposalJSON`, `contextKey`
- **Constants** use PascalCase for exported (`StatusBacklog`, `IDPrefix`) and camelCase for unexported (`dbKey`, `cfgKey`, `currentSchemaVersion`)
- **Sentinel errors** use `Err` prefix: `ErrNotFound`, `ErrConflict`, `ErrSelfRelation`, `ErrCycleDetected`, `ErrDuplicateRelation`, `ErrNotAttached`, `ErrLabelColorConflict`
- **Validation functions** follow `Validate<Type>` pattern: `ValidateStatus`, `ValidatePriority`, `ValidateIssueKind`, `ValidateCriticality`, `ValidateVerdict`
- **Test helpers** use `must` prefix for fatal helpers: `mustOpen`, `mustInit`

### File Naming

- Source files use lowercase with underscores: `issue_create.go`, `vote_cast.go`, `activity.go`
- Test files use the standard `_test.go` suffix
- CLI subcommands follow `<noun>_<verb>.go` pattern: `issue_create.go`, `issue_close.go`, `vote_cast.go`, `vote_commit.go`
- The parent command file is the bare noun: `issue.go`, `vote.go`

## Type System Patterns

### String-Typed Enums

Domain enums are implemented as named `string` types with associated constants, validation functions, and display methods:

```
type Status string → StatusBacklog, StatusTodo, StatusInProgress, StatusReview, StatusDone
type Priority string → PriorityCritical, PriorityHigh, PriorityMedium, PriorityLow, PriorityNone
type IssueKind string → IssueKindBug, IssueKindFeature, IssueKindTask, IssueKindEpic, IssueKindChore
type Criticality string → CriticalityLow, CriticalityMedium, CriticalityHigh, CriticalityCritical
type ProposalStatus string → ProposalStatusOpen, ProposalStatusApproved, ProposalStatusRejected, ProposalStatusCommitted
type Verdict string → VerdictApprove, VerdictApproveWithConcerns, VerdictReject
```

Each enum type consistently provides:
- A `validXxx` slice for iteration
- A `ValidateXxx(x) error` function
- `.Color() string` and `.Icon() string` methods for terminal rendering (on Issue-related enums)

### Custom JSON Serialization

Domain types (`Issue`, `Proposal`, `Vote`, `Comment`) implement `MarshalJSON`/`UnmarshalJSON` using a private `xxxJSON` struct as the wire format. This pattern:
- Converts internal int IDs to prefixed string IDs (`DKT-5`, `DKT-V1`)
- Normalizes timestamps to RFC3339 UTC
- Ensures nil slices serialize as `[]` instead of `null`
- Validates enum values during deserialization

### ID Formatting

Two ID systems exist:
- Issue IDs: `DKT-<n>` prefix (`FormatID` / `ParseID`)
- Proposal IDs: `DKT-V<n>` prefix (`FormatProposalID` / `ParseProposalID`)

Both parsers are case-insensitive and accept bare numeric input.

## Error Handling Patterns

### Sentinel Errors in `db` Package

The `db` package defines sentinel errors for domain-specific failure cases:
- `ErrNotFound` — entity does not exist
- `ErrConflict` — concurrent modification or constraint violation
- `ErrSelfRelation` — self-referential relation attempted
- `ErrDuplicateRelation` — duplicate relation detected
- `ErrCycleDetected` — cycle detected in dependency graph
- `ErrNotAttached` — label not attached to issue
- `ErrLabelColorConflict` — label color mismatch

### CLI Error Wrapping

The CLI layer wraps errors with `CmdError`, which pairs an `error` with a machine-readable `ErrorCode` (`GENERAL_ERROR`, `NOT_FOUND`, `VALIDATION_ERROR`, `CONFLICT`). The `cmdErr()` helper creates these. Each error code maps to a distinct exit code (0-4).

### Error Propagation Pattern

Errors are consistently wrapped with `fmt.Errorf("context: %w", err)` throughout the codebase, preserving the error chain for `errors.Is`/`errors.As` inspection. The `db` package wraps with action context (e.g., `"creating issue: %w"`), and the CLI layer wraps with user-facing context (e.g., `"reading description from stdin: %w"`).

## Module Organization

### Package Dependency Flow

```
cmd/docket/main.go
  └── internal/cli (cobra commands)
        ├── internal/config (configuration resolution)
        ├── internal/db (data access — SQLite)
        │     └── internal/model (domain types)
        ├── internal/model (domain types — shared)
        ├── internal/output (JSON/human output dispatch)
        │     └── internal/render (terminal rendering)
        ├── internal/filter (issue filtering)
        └── internal/planner (DAG / topological sort)
              └── internal/model
```

All application code lives under `internal/`, preventing external imports. There are no exported packages — docket is a standalone CLI binary.

### Separation of Concerns

- **`model`** — Pure data types, validation, and serialization. No database or I/O dependencies.
- **`db`** — All SQL lives here. Functions accept `*sql.DB` as first argument. No CLI or rendering logic.
- **`cli`** — Cobra command wiring. Reads flags, calls `db` functions, uses `output.Writer` for results.
- **`output`** — Dispatch layer: routes between JSON (`json.go`) and human (`human.go`) output.
- **`render`** — Terminal-specific rendering: board layout, tables, detail views, markdown. Uses lipgloss for styling.
- **`config`** — Resolves `DOCKET_PATH` env var or falls back to `$PWD/.docket`.
- **`filter`** — Stateless filtering utilities (label set membership).
- **`planner`** — DAG construction from issue relations, topological sorting for execution planning.

## Design Patterns

### Context-Based Dependency Injection

The CLI layer uses `context.Context` value injection to pass the database connection (`*sql.DB`) and configuration (`*config.Config`) through cobra's `PersistentPreRunE`/`PersistentPostRunE` hooks. Helper functions `getDB(cmd)` and `getCfg(cmd)` extract these from the command context.

### Writer Pattern for Output

`output.Writer` abstracts output formatting. Commands call `w.Success()`, `w.Error()`, `w.Info()`, and `w.Warn()` without knowing whether output is JSON or human-formatted. JSON mode produces structured envelopes (`{ok: true/false, data/error, ...}`). Human mode produces styled terminal output via lipgloss.

### Interactive Fallback Pattern

CLI commands support three modes: flags-only (for scripting/JSON), interactive forms (via charmbracelet/huh when flags are absent), and stdin piping (description from `-`). The pattern is: if `--json` and required flag missing, return validation error; if not `--json` and flag missing, launch interactive form.

### Schema Migration Pattern

The database uses a versioned migration system. `schema.go` defines `schemaDDL` for the initial schema and a `migrations` map keyed by target version. `Migrate()` applies pending migrations sequentially within transactions, updating the `meta.schema_version` row after each step. The system handles edge cases like databases stamped at a higher version than their actual state.

## Build Configuration

### Makefile Targets

| Target | Description |
|--------|-------------|
| `build` | `CGO_ENABLED=0 go build` with ldflags for version/commit/date |
| `test` | `go test ./...` |
| `lint` | `go vet` then optional `staticcheck` |
| `vet` | `go vet ./...` |
| `install` | `CGO_ENABLED=0 go install` with ldflags |
| `clean` | Remove `./bin/` |
| `demo` | Build then run VHS tape for terminal GIF |

### Build Flags

The build injects version metadata via ldflags:
- `internal/cli.version` — git describe output
- `internal/cli.commit` — short SHA
- `internal/cli.buildDate` — UTC timestamp

`CGO_ENABLED=0` is used for all builds, leveraging `modernc.org/sqlite` (a pure-Go SQLite implementation) to avoid CGO dependency.

## CI/CD Pipeline

### GitHub Actions (`ci.yaml`)

The CI pipeline builds on a 4-platform matrix (macOS x86/ARM, Linux x86/ARM) using the Vorpal build system. It does **not** run tests, linting, or vetting — only builds the binary. Tagged pushes trigger a release job that:
- Downloads build artifacts
- Creates a GitHub release with platform-specific tarballs
- Attests build provenance via `actions/attest-build-provenance`

### Nightly (`ci-nightly.yaml`)

A scheduled nightly workflow recreates a `nightly` tag pointing to `main`'s HEAD, enabling nightly release tracking. Uses a GitHub App token for authentication.

## Gaps and Recommendations

### Critical Gaps

1. **No tests in CI.** `go test ./...` is not run in any CI workflow. Test regressions can ship to release.
2. **No lint/vet in CI.** Static analysis is entirely local and optional.

### Moderate Gaps

3. **No golangci-lint.** The project would benefit from a meta-linter configuration to enforce consistent standards (unused code, error checking, import ordering, etc.).
4. **No code coverage tracking.** There is no coverage reporting or threshold enforcement.
5. **No formatter check.** `gofmt` correctness is not verified anywhere.

### Minor Gaps

6. **No `.editorconfig`.** Multi-contributor projects benefit from standardized editor settings.
7. **No pre-commit hooks.** Catching lint/format issues before commit reduces CI feedback loops.
