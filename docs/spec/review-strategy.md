---
project: "docket"
maturity: "draft"
last_updated: "2026-03-21"
updated_by: "@staff-engineer"
scope: "Review dimensions, quality gates, and review workflow for the docket project"
owner: "@staff-engineer"
dependencies:
  - code-quality.md
  - testing.md
---

# Review Strategy

## 1. Overview

Docket is a Go CLI project with a small codebase (~70 source files) and a solo/small-team contributor model. Reviews currently happen through GitHub pull requests, with CI as the primary automated quality gate. There are no formal CODEOWNERS, PR templates, or written review checklists. This spec documents what exists and identifies where review practices should be strengthened as the project grows.

## 2. Current Review Workflow

### 2.1 Pull Request Flow

All changes merge to `main` via GitHub pull requests. Commit messages follow conventional commit format (`feat:`, `fix:`, `chore:`). PR titles mirror this convention with issue references (e.g., `feat: add docket vote subcommand for PBFT consensus voting (#5)`).

There is no branch naming convention enforced, though feature branches are used in practice (e.g., `feature/watch`).

### 2.2 CI as Automated Reviewer

The CI pipeline (`.github/workflows/ci.yaml`) runs on every PR and push to `main`. It performs:

- **Multi-platform build**: macOS (Intel + ARM) and Linux (x86_64 + ARM64) via Vorpal build system
- **Artifact generation**: Cross-platform binary archives uploaded as artifacts

**What CI does NOT do today:**

- No `go test ./...` step in CI (tests exist but are only run locally via `make test`)
- No `go vet` or `staticcheck` step in CI (available locally via `make lint`)
- No QA suite (`scripts/qa.sh`) execution in CI
- No code coverage measurement or thresholds
- No dependency vulnerability scanning

This is a significant gap: the CI pipeline validates that the binary builds across platforms but does not validate correctness or code quality.

### 2.3 Manual Quality Gates

Available locally but not enforced in CI:

| Command | What It Checks |
|---------|---------------|
| `make test` | Unit tests (`go test ./...`) |
| `make lint` | `go vet` + `staticcheck` (if installed) |
| `make vet` | `go vet ./...` only |
| `scripts/qa.sh` | Comprehensive end-to-end CLI test suite (29 sections, 150+ checks) |

### 2.4 Human Review

With 11 total commits and 3 merged PRs, the project is in early stages. PRs have been merged with squash commits. There is no evidence of formal reviewer assignment, required approvals, or branch protection rules enforcing reviews.

## 3. Review Dimensions

The following dimensions are weighted by their relevance to the docket project, given its nature as a local-first CLI tool backed by SQLite.

### 3.1 High Priority

**Data Integrity (Critical)**
- SQLite schema changes require migration paths (`internal/db/schema.go` has versioned migrations up to v3)
- Schema changes can silently corrupt or lose user data if migrations are wrong
- The `ON DELETE SET NULL` and `ON DELETE CASCADE` behaviors in foreign keys are correctness-critical
- Review must verify: migration idempotency, rollback safety, data preservation across versions

**JSON API Contract Stability (Critical)**
- Every command supports `--json` with a documented envelope (`{"ok": true, "data": ..., "message": ...}`)
- AI agents depend on stable JSON shapes — breaking changes silently break agent workflows
- Error codes (`GENERAL_ERROR`, `NOT_FOUND`, `VALIDATION_ERROR`, `CONFLICT`) are part of the contract
- The QA suite has dedicated sections (Q, R) for contract and exit code validation
- Review must verify: no field renames/removals without versioning, exit codes match documented behavior

**CLI UX Consistency (High)**
- One-file-per-command pattern in `internal/cli/` must be maintained
- All commands must support `--json` and `--quiet` flags via `getWriter()`
- Interactive forms (via `charmbracelet/huh`) must degrade gracefully in non-TTY contexts
- Review must verify: new commands follow established patterns, help text is consistent

### 3.2 Medium Priority

**Dependency Graph Correctness (Medium-High)**
- The DAG builder (`internal/planner/`) powers `docket next` and `docket plan`
- Cycle detection, topological sort, and phase planning must remain correct
- Bugs here cause agents to get stuck or execute work in wrong order
- Review must verify: edge cases (cycles, orphans, deep trees) are covered

**Error Handling (Medium)**
- `CmdError` wrapping with `output.ErrorCode` ensures machine-readable errors
- Review must verify: errors propagate correctly, appropriate error codes are used, no silent failures

**Security at System Boundaries (Medium)**
- SQL injection via user-supplied issue titles, labels, comments, file paths
- Path traversal in file attachment commands (`docket issue file add`)
- Review must verify: parameterized queries used (they are, via `database/sql`), file paths validated

### 3.3 Lower Priority (for this project)

**Performance** — Local SQLite with small datasets; not a current concern unless someone loads thousands of issues.

**Concurrency** — Single-user CLI; SQLite handles serialization. No goroutine-safety concerns in current architecture.

**Accessibility** — Terminal rendering via lipgloss; `--json` mode is the accessibility escape hatch for non-visual consumers.

## 4. Review Checklist

For use during code review of PRs to the docket project.

### Must-Check (Blockers)

- [ ] Schema changes include a versioned migration in `schema.go` with `currentSchemaVersion` incremented
- [ ] JSON output shapes are backward-compatible (no removed/renamed fields)
- [ ] Exit codes match documented behavior (0 success, 1 general error, 2 not found, 3 validation, 4 conflict)
- [ ] New commands follow the one-file-per-command pattern and register with parent command
- [ ] New commands support `--json` via `getWriter()` and use `output.Writer.Success()`/`Error()`
- [ ] SQL queries use parameterized statements (no string interpolation)
- [ ] QA tests added or updated in `scripts/qa/` for new commands/flags

### Should-Check (Concerns)

- [ ] Model types have correct `MarshalJSON`/`UnmarshalJSON` implementations
- [ ] Activity log entries created for state-changing operations
- [ ] Error messages are user-friendly in human mode and machine-parseable in JSON mode
- [ ] Cobra command metadata (Use, Short, Long, Example) is complete
- [ ] Interactive forms have non-interactive fallbacks for CI/agent contexts

### Nice-to-Check (Suggestions)

- [ ] Rendering changes tested in narrow terminal widths
- [ ] Import/export round-trip preserved for affected data
- [ ] `make lint` passes without new warnings

## 5. High-Risk Areas

These areas of the codebase warrant extra scrutiny during review:

| Area | Risk | Why |
|------|------|-----|
| `internal/db/schema.go` | Data loss | Migrations are irreversible in practice; wrong migration = corrupted databases |
| `internal/db/issues.go` | Data integrity | Core CRUD operations, complex filtering, parent-child relationships |
| `internal/db/relations.go` | Graph integrity | Dependency links that power the planner; wrong links = wrong execution order |
| `internal/planner/` | Correctness | DAG/topological sort — subtle bugs cause silent wrong behavior |
| `internal/output/` | API contract | Any change here affects every consumer of `--json` output |
| `internal/model/` (JSON methods) | API contract | Marshal/Unmarshal changes break agent integrations silently |

## 6. Gaps and Recommendations

### 6.1 Critical Gaps

**No automated testing in CI.** The CI pipeline only validates that binaries compile across platforms. Neither `go test`, `go vet`, `staticcheck`, nor the QA suite runs in CI. This means PRs can merge with broken tests, lint violations, or functional regressions.

**Recommendation:** Add a `test` job to `ci.yaml` that runs `make test`, `make lint`, and `scripts/qa.sh` before the build job. This is the single highest-leverage improvement to review quality.

**No branch protection rules.** There is no evidence of required reviews, required status checks, or restrictions on force-pushing to `main`.

**Recommendation:** Enable branch protection on `main` requiring at least one approval and passing CI checks.

### 6.2 Moderate Gaps

**No PR template.** Contributors (human or AI) have no guidance on what to include in a PR description.

**Recommendation:** Add `.github/PULL_REQUEST_TEMPLATE.md` covering: what changed, why, testing performed, and a checklist derived from Section 4 above.

**No CODEOWNERS file.** As the team grows, there is no mechanism to auto-assign reviewers for high-risk areas (db/, planner/, output/).

**No contribution guidelines.** The README has a "Contributing" section with development setup and coding guidelines, but it is minimal. The one-file-per-command and `--json` requirements are documented there, which is good.

### 6.3 Positive Practices Already in Place

- Conventional commit messages are consistently used
- The QA suite (`scripts/qa.sh`) is comprehensive with 29 test sections covering every command
- JSON contract validation exists as dedicated QA sections (Q: JSON contracts, R: exit codes)
- The `output.Writer` abstraction enforces consistent JSON envelopes across all commands
- Domain types use custom JSON marshaling, centralizing serialization concerns
- `CmdError` with error codes provides structured error handling throughout

## 7. Review Process for AI Agent Changes

Docket is designed for AI agent integration, and AI agents (via Claude Code) also contribute to its development. Special review considerations for agent-generated changes:

- **Verify intent matches TDD:** Agent implementations should trace back to approved TDDs in `docs/tdd/`
- **Check for over-engineering:** Agents may add unnecessary abstractions, error handling for impossible cases, or unused parameters
- **Validate QA coverage:** Ensure agents add QA test sections for new functionality, not just unit tests
- **Review commit granularity:** Agent-generated PRs may bundle too many changes or split them unnaturally

## 8. Vote-Based Consensus Review

The `docket vote` subsystem (added in commit `0c4077b`) provides a PBFT-inspired consensus mechanism for reviewing technical decisions. This is used by the `/vote` Claude Code skill to validate TDDs and architectural decisions before implementation begins. Vote proposals can be linked to docket issues via `docket vote link`, creating traceability between decisions and work items.

This mechanism supplements but does not replace human code review for implementation PRs.
