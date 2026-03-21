---
project: "docket"
maturity: "experimental"
last_updated: "2026-03-21"
updated_by: "@staff-engineer"
scope: "Security posture, trust boundaries, input validation, and data protection for the docket CLI"
owner: "@staff-engineer"
dependencies:
  - architecture.md
---

# Security Specification

## 1. Overview

Docket is a local-first CLI issue tracker that stores all data in a local SQLite database (`.docket/issues.db`). It has no network-facing components, no authentication/authorization system, no remote API, and no secret management. The security posture is shaped by its single-user, local-only design.

## 2. Threat Model

### 2.1 Trust Boundaries

Docket operates within a single trust boundary: the local user's filesystem and terminal session. There are no network boundaries, remote services, or multi-user access controls.

| Boundary | Description |
|---|---|
| **Filesystem** | The `.docket/` directory and its `issues.db` file are readable/writable by the local user. No access controls beyond standard POSIX file permissions. |
| **CLI Input** | All user input arrives via command-line flags, arguments, stdin pipes, or interactive terminal forms (via charmbracelet/huh). |
| **Environment Variables** | `DOCKET_PATH` (database location), `EDITOR` (external editor for comments), `TERM` (terminal capability detection). |
| **External Processes** | The `$EDITOR` program is invoked for interactive comment editing. `git config user.name` is invoked to resolve the default author name. |

### 2.2 Attack Surface

The attack surface is minimal due to the local-only architecture:

- **No network listeners**: Docket does not bind sockets, serve HTTP, or accept remote connections.
- **No authentication**: There is no user authentication or session management. The tool trusts the local user entirely.
- **No secrets**: Docket does not handle API keys, tokens, passwords, or any form of credentials.
- **No encryption**: The SQLite database is stored in plaintext. There is no at-rest or in-transit encryption.

### 2.3 Residual Risks

| Risk | Severity | Description |
|---|---|---|
| Malicious import file | Low | A crafted JSON import file could inject unexpected data into the database. Mitigated by validation (see Section 4). |
| EDITOR command injection | Low | `$EDITOR` is passed directly to `exec.Command()`, which does not invoke a shell. This is safe against shell injection but the user-specified editor binary itself could be malicious. This is standard CLI behavior (git, etc. do the same). |
| Symlink attacks on temp files | Low | `os.CreateTemp()` is used for editor temp files. Go's implementation uses `O_EXCL` which prevents symlink attacks. |
| Database path traversal via DOCKET_PATH | Low | `DOCKET_PATH` is used as-is without sanitization. A malicious env variable could point to an arbitrary filesystem location. This is by design (trusted local environment). |

## 3. SQL Injection Protection

Docket constructs SQL queries using a combination of parameterized queries and allowlist-validated dynamic SQL. The approach is well-implemented overall.

### 3.1 Parameterized Queries

The vast majority of queries use Go's `database/sql` parameter placeholders (`?`). All user-supplied values (titles, descriptions, status values, assignees, etc.) are passed as parameters, never interpolated into SQL strings. Examples:

- `internal/db/issues.go`: `CreateIssue`, `GetIssue`, `UpdateIssue` -- all use `?` placeholders for values.
- `internal/db/proposals.go`: `CreateProposal` -- uses `?` placeholders for all 14 columns.
- `internal/db/comments.go`, `internal/db/labels.go`, etc. -- consistent parameterized query usage.

### 3.2 Dynamic SQL Construction (Allowlisted)

Three cases construct SQL with `fmt.Sprintf`, all with appropriate safeguards:

1. **Sort field in `ListIssues`** (`internal/db/issues.go:308-320`): The sort field is validated against both `validSortFields` (a string allowlist) and `safeIdentifier` (a regex `^[a-z_]+$`) before interpolation. The sort direction is forced to either `"ASC"` or `"DESC"`. This is defense-in-depth.

2. **`IN (?)` placeholders** (`makePlaceholders` at `internal/db/issues.go:837`): Generates `?, ?, ...` strings for parameterized `IN` clauses. The placeholder count is derived from `len(slice)`, not user input. Values are passed as parameters.

3. **`countByColumn`** (`internal/db/issues.go:922-940`): Column names are interpolated via `fmt.Sprintf` but this function is only called with hardcoded string literals (`"status"`, `"priority"`) from `CountByStatus` and `CountByPriority`. There is no path for user input to reach the column parameter.

4. **`ClearAllData`** (`internal/db/issues.go:955-978`): Table names are from a hardcoded string slice. No user input flows into the `DELETE FROM` statements.

### 3.3 Update Field Validation

`UpdateIssue` (`internal/db/issues.go:399-468`) accepts a `map[string]interface{}` of field names to values. Field names are validated against `validUpdateFields` before being interpolated into the `SET` clause. Values are passed as `?` parameters. This is correctly implemented.

## 4. Input Validation

### 4.1 Enum Validation

All enum-typed fields are validated before database insertion:

- **Status**: Validated by `model.ValidateStatus()` against `validStatuses` (`backlog`, `todo`, `in-progress`, `review`, `done`).
- **Priority**: Validated by `model.ValidatePriority()` against `validPriorities` (`critical`, `high`, `medium`, `low`, `none`).
- **Issue Kind**: Validated by `model.ValidateIssueKind()` against `validIssueKinds` (`bug`, `feature`, `task`, `epic`, `chore`).
- **Criticality**: Validated by `model.ValidateCriticality()` for proposals.
- **Verdict**: Validated by `model.ValidateVerdict()` for votes.
- **Relation Type**: Validated by `model.ValidateRelationType()` for issue relations.

Validation occurs at both the CLI layer (before calling db functions) and during JSON deserialization (via `UnmarshalJSON`).

### 4.2 ID Parsing

Issue IDs are parsed by `model.ParseID()` which accepts both `"DKT-5"` and `"5"` formats, validates the result is a positive integer, and rejects all other input.

### 4.3 Import Validation

The `import` command (`internal/cli/import.go`) validates imported JSON data before any database mutations:

- Version field must equal `1`.
- All issue status, priority, and kind fields are re-validated.
- All relation types are validated.
- The import is performed within a single transaction -- if any step fails, all changes are rolled back.
- Destructive `--replace` mode requires interactive confirmation (unless in `--json` mode).

### 4.4 Stdin Input Limits

Both `issue create` (description from stdin) and `comment add` (body from stdin) enforce a 1 MiB size limit via `io.LimitReader`. This prevents unbounded memory allocation from malicious pipe input.

### 4.5 Gaps

- **Free-text fields**: Title, description, comment body, assignee, label names, and file paths accept arbitrary strings with no length limits beyond the stdin cap. There is no maximum length enforced at the database layer.
- **File path values**: The `issue file add` command stores file paths as opaque strings in the database. It does not verify the path exists, is within a project boundary, or is a safe path. This is by design (metadata tracking, not file access).

## 5. External Process Execution

### 5.1 Editor Invocation

`internal/cli/issue_comment.go:74-96` invokes `$EDITOR` (or `vi` as fallback) for interactive comment editing:

```
editorCmd := exec.Command(editor, tmpPath)
```

This is safe against shell injection because `exec.Command` does not invoke a shell -- it executes the binary directly with the temp file path as a single argument. The temp file uses `os.CreateTemp` with a `docket-comment-*.md` pattern and is cleaned up via `defer os.Remove`.

### 5.2 Git Config Resolution

`internal/config/config.go:80-88` resolves the default author via:

```
exec.CommandContext(ctx, "git", "config", "user.name")
```

This is safe: the command and all arguments are hardcoded string literals. A 2-second timeout prevents hanging if git is misconfigured. Falls back to `user.Current().Username`, then `"unknown"`.

## 6. Database Security

### 6.1 SQLite Configuration

The database connection (`internal/db/db.go:12-36`) is configured with:

- **WAL mode**: `PRAGMA journal_mode=WAL` -- provides crash recovery.
- **Foreign keys**: `PRAGMA foreign_keys=ON` -- enforces referential integrity.
- **Busy timeout**: `PRAGMA busy_timeout=5000` -- prevents immediate lock failures.
- **Single connection**: `db.SetMaxOpenConns(1)` -- prevents concurrent write contention.

### 6.2 Schema Migration

Migrations (`internal/db/schema.go`) run within transactions. Each migration atomically updates both the schema and the version number. There is self-healing logic for databases that were incorrectly stamped as v2 without the proposals table (lines 223-231).

### 6.3 No Encryption

The SQLite database file is stored as plaintext. There is no SQLCipher or equivalent encryption. Data protection relies entirely on filesystem permissions. The `.docket/` directory is created with mode `0755` (`internal/cli/init.go:64`).

## 7. Build and Distribution Security

### 7.1 Build Configuration

- **CGO_ENABLED=0**: The binary is compiled as a static Go binary with no C dependencies (via Makefile and Vorpal build config). This eliminates a class of memory safety issues.
- **Version embedding**: Build version, commit hash, and date are injected via `-ldflags` at build time.
- **Static analysis**: `staticcheck` and `go vet` are configured as lint targets.

### 7.2 Install Script

`scripts/install.sh` downloads pre-built binaries from GitHub releases over HTTPS. Notable security characteristics:

- Uses `set -eu` for fail-fast behavior.
- Downloads are over HTTPS from `github.com` releases.
- Uses a temporary directory cleaned up via `trap`.
- **Gap**: No checksum verification of downloaded archives. No GPG signature verification. The integrity of the downloaded binary depends entirely on HTTPS transport security and GitHub's release infrastructure.

### 7.3 Dependencies

The project has 39 direct and indirect Go module dependencies (per `go.sum`). Key security-relevant dependencies:

| Dependency | Purpose | Notes |
|---|---|---|
| `modernc.org/sqlite` | Pure-Go SQLite | No CGO required; avoids C memory safety issues |
| `github.com/microcosm-cc/bluemonday` | HTML sanitization | Used by glamour for Markdown rendering output |
| `github.com/spf13/cobra` | CLI framework | Well-maintained, widely used |
| `google.golang.org/grpc` | gRPC (indirect, via Vorpal SDK) | Not used at runtime by docket itself |

## 8. Data Privacy

- All data is stored locally. No telemetry, analytics, or remote data collection exists.
- The `export` command can write all database content to JSON, CSV, or Markdown files. The user controls where these files are written.
- The default author is resolved from `git config user.name` or the OS username. This is the only PII-adjacent data collected, and it stays local.

## 9. Security Gaps and Recommendations

| Gap | Severity | Recommendation |
|---|---|---|
| No checksum verification in install script | Medium | Add SHA-256 checksum verification for downloaded release archives. |
| `.docket/` directory created with 0755 | Low | Consider 0700 for the directory to prevent other users on multi-user systems from reading the database. |
| No database file permission enforcement | Low | Verify or set file permissions on `issues.db` after creation (e.g., 0600). |
| No input length limits on free-text fields | Low | Consider adding reasonable length limits for title, description, assignee, and label fields at the CLI or database layer to prevent accidental abuse. |
| Markdown export does not sanitize for all contexts | Low | The `escapeMarkdown` function handles common Markdown special characters but may not cover all edge cases for downstream consumption. |
| No dependency vulnerability scanning in CI | Medium | Add `govulncheck` or equivalent to the build/lint pipeline. |
