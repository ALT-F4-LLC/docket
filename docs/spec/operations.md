---
project: "docket"
maturity: "experimental"
last_updated: "2026-03-21"
updated_by: "@staff-engineer"
scope: "CI/CD pipelines, build system, release process, database operations, and operational characteristics of the docket CLI"
owner: "@staff-engineer"
dependencies:
  - architecture.md
  - security.md
---

# Operations

## Overview

Docket is a local-first CLI issue tracker backed by a single SQLite database file per project. It has no server component, no network services, and no runtime infrastructure to operate. Operational concerns center on: CI/CD pipelines, build/release automation, database lifecycle management, and developer environment tooling.

## Build System

### Vorpal Build System

The project uses [Vorpal](https://github.com/ALT-F4-LLC/vorpal), an in-house Nix-inspired build system. Build configuration lives in two files:

- **`Vorpal.toml`** -- declares the project language (Go) and source includes (`cmd/vorpal`, `go.mod`, `go.sum`).
- **`cmd/vorpal/main.go`** -- Go-based build definition that configures two build artifacts:
  - `docket-shell`: a development environment with Go toolchain, `goimports`, `gopls`, `staticcheck`, `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`, `ffmpeg`, `ttyd`, and `vhs`.
  - `docket`: the production binary, built from `cmd/docket` with `CGO_ENABLED=0`.

Vorpal uses an S3-backed artifact registry (`altf4llc-vorpal-registry`) for caching build artifacts. AWS credentials are required in CI.

### Makefile

A `Makefile` provides conventional local build targets:

| Target    | Command                              | Notes                                    |
|-----------|--------------------------------------|------------------------------------------|
| `build`   | `go build` with ldflags              | Outputs to `./bin/docket`, CGO disabled  |
| `test`    | `go test ./...`                      | Runs all tests                           |
| `lint`    | `go vet` + `staticcheck`             | Staticcheck is optional (skipped if absent) |
| `vet`     | `go vet ./...`                       | Subset of lint                           |
| `install` | `go install` with ldflags            | CGO disabled                             |
| `clean`   | `rm -rf ./bin/`                      | Removes build artifacts                  |
| `demo`    | Builds then runs `vhs scripts/demo.tape` | Requires VHS installed                |

### Version Injection

Build version metadata is injected via `-ldflags` at compile time into `internal/cli`:

- `version` -- from `git describe --tags --always --dirty` (defaults to `"dev"`)
- `commit` -- from `git rev-parse --short HEAD` (defaults to `"none"`)
- `buildDate` -- UTC timestamp (defaults to `"unknown"`)

These are surfaced via `docket --version`.

## CI/CD

### Primary CI Pipeline (`.github/workflows/ci.yaml`)

Triggered on: pull requests, pushes to `main`, and tag pushes.

**Jobs:**

1. **`build-shell`** -- Builds the `docket-shell` development environment across a 4-runner matrix:
   - `macos-latest` (ARM64)
   - `macos-latest-large` (x86_64)
   - `ubuntu-latest` (x86_64)
   - `ubuntu-latest-arm64` (ARM64)
   - Uses `ALT-F4-LLC/setup-vorpal-action@main` to bootstrap Vorpal with S3 registry.
   - Uploads `Vorpal.lock` as an artifact per architecture/OS.

2. **`build`** -- Depends on `build-shell`. Same 4-runner matrix. Builds the `docket` binary, archives it as `docket-{arch}-{os}.tar.gz`, uploads as artifact.

3. **`release`** -- Runs only on tag pushes (`refs/tags/*`). Downloads all build artifacts and creates a GitHub Release via `softprops/action-gh-release@v2`. Releases are marked as **prerelease**. Build provenance is attested via `actions/attest-build-provenance@v3`.

**Permissions granted to release job:** `attestations: write`, `contents: write`, `id-token: write`, `packages: write`.

### Nightly Pipeline (`.github/workflows/ci-nightly.yaml`)

Triggered on: daily schedule (`0 8 * * *` UTC) and manual `workflow_dispatch`.

Uses concurrency groups to cancel in-progress runs. Authenticates via a GitHub App token (`ALTF4LLC_GITHUB_APP_ID` / `ALTF4LLC_GITHUB_APP_PRIVATE_KEY`).

**Behavior:** Deletes the existing `nightly` release and tag, then re-creates the `nightly` tag pointing at the current `main` HEAD SHA. This triggers the primary CI pipeline's release job to produce a fresh nightly build.

### CI Secrets and Variables

| Secret/Variable              | Purpose                         |
|------------------------------|---------------------------------|
| `AWS_ACCESS_KEY_ID`          | S3 registry access for Vorpal   |
| `AWS_SECRET_ACCESS_KEY`      | S3 registry access for Vorpal   |
| `AWS_DEFAULT_REGION`         | S3 region (stored as variable)  |
| `ALTF4LLC_GITHUB_APP_ID`    | GitHub App for nightly releases |
| `ALTF4LLC_GITHUB_APP_PRIVATE_KEY` | GitHub App private key     |

## Release Process

### Tag-Based Releases

Releases are driven entirely by Git tags:

1. Push a tag (e.g., `v0.1.0`) to trigger CI.
2. CI builds cross-platform binaries (4 targets: macOS ARM64/x86_64, Linux ARM64/x86_64).
3. Artifacts are packaged as `.tar.gz` archives.
4. A GitHub Release is created with all archives.
5. Build provenance attestation is attached.

All releases are currently marked as **prerelease**, consistent with the project's experimental maturity.

### Nightly Releases

A rolling `nightly` release is automatically maintained. Each night at 08:00 UTC, the previous nightly release and tag are deleted and recreated from `main` HEAD.

### No Versioned Release Process

There is no changelog generation, semantic versioning automation, or release branch strategy. Tags are created manually.

## Database Operations

### Initialization

`docket init` creates a `.docket/` directory (or uses `DOCKET_PATH`) containing `issues.db`. The SQLite database is initialized with the full schema DDL and stamped at schema version 1.

### Auto-Migration

On every command invocation (via `PersistentPreRunE`), the database is opened and `db.Migrate()` is called. This applies any pending migrations sequentially (currently v1->v2->v3). Migrations run within individual transactions; each migration bumps the schema version on success.

The migration system includes a repair mechanism for databases incorrectly stamped at v2 without the proposals table -- it detects the missing table and re-runs migrations from v1.

### Schema Versioning

- Current schema version: **3**
- Version is stored in a `meta` table (`key='schema_version'`).
- Migration functions are registered in a `map[int]func(tx *sql.Tx) error`.

### SQLite Configuration

The database is configured with these pragmas on every open:

| Pragma                | Value  | Purpose                                        |
|-----------------------|--------|------------------------------------------------|
| `journal_mode`        | `WAL`  | Write-Ahead Logging for concurrent read access |
| `foreign_keys`        | `ON`   | Enforce referential integrity                  |
| `busy_timeout`        | `5000` | 5-second wait before returning SQLITE_BUSY     |

Connection pool is limited to 1 (`SetMaxOpenConns(1)`) since SQLite is single-writer.

### Data Portability

- **Export:** `docket export` outputs to JSON, CSV, or Markdown. Supports `--status` and `--label` filters. Can write to file (`-f`) or stdout.
- **Import:** `docket import <file>` reads JSON export format. Three modes:
  - Default: requires empty database.
  - `--merge`: skips duplicate IDs.
  - `--replace`: destructively clears all data first (with interactive confirmation in human mode).

Import runs within a single transaction for atomicity. Export data is validated before any mutations.

### Backup

There is no built-in backup command. Since the database is a single file (`.docket/issues.db`), file-level copying suffices. WAL mode means the `-wal` and `-shm` files should be included in any backup, or a checkpoint should be forced first.

## Error Handling and Exit Codes

The CLI uses a structured error system with machine-readable error codes:

| Exit Code | Error Code         | Meaning                       |
|-----------|--------------------|-------------------------------|
| 0         | --                 | Success                       |
| 1         | `GENERAL_ERROR`    | Unexpected or internal error  |
| 2         | `NOT_FOUND`        | Requested resource not found  |
| 3         | `VALIDATION_ERROR` | Invalid input or arguments    |
| 4         | `CONFLICT`         | Operation conflicts with state|

In `--json` mode, errors are wrapped in a JSON envelope (`{"ok": false, "error": "...", "code": "..."}`). In human mode, errors are printed to stderr.

## Output Modes

The CLI supports three output modes controlled by global flags:

- **Human mode** (default): Colored, styled output via lipgloss to stdout/stderr.
- **JSON mode** (`--json`): Structured JSON envelopes to stdout. Info/warn messages suppressed.
- **Quiet mode** (`-q`): Suppresses informational messages (warnings still shown).

## Monitoring, Logging, and Observability

**There is no monitoring, logging, or observability infrastructure.** This is consistent with docket's nature as a local-first CLI tool:

- No structured logging framework (only `log.Fatalf` in the Vorpal build definition).
- No metrics collection or export.
- No tracing or OpenTelemetry integration.
- No health checks or readiness probes.
- No dashboards or alerting.

Error reporting is entirely through CLI exit codes and stderr output.

## Deployment Model

Docket is distributed as a **static binary** (CGO disabled). There is no container image, no server deployment, no infrastructure to manage. Users download a platform-specific archive from GitHub Releases or build from source.

### Supported Platforms

| Architecture | OS    | CI Runner              |
|-------------|-------|------------------------|
| aarch64     | macOS | `macos-latest`         |
| x86_64      | macOS | `macos-latest-large`   |
| x86_64      | Linux | `ubuntu-latest`        |
| aarch64     | Linux | `ubuntu-latest-arm64`  |

## Operational Gaps

The following are notable gaps relative to a production-grade CLI tool. These are documented for awareness, not as immediate action items -- several are appropriate for the project's current maturity level.

- **No automated testing in CI**: The CI pipeline builds but does not run `go test` or `staticcheck`. Tests and linting are Makefile-only.
- **No changelog or release notes automation**: Releases have no generated changelogs.
- **No installation automation**: No Homebrew formula, no `go install` published path, no package manager integration.
- **No backup/restore commands**: Users must manage SQLite file copies manually.
- **No database integrity verification**: No `PRAGMA integrity_check` command or corrupted-database recovery path.
- **No update/upgrade mechanism**: Users must manually download new releases.
- **No telemetry or crash reporting**: Expected for a local-first tool, but means no visibility into real-world usage or failures.
