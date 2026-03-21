---
project: "docket"
maturity: "implemented"
last_updated: "2026-03-20"
updated_by: "@staff-engineer"
scope: "Add `docket vote` subcommand group for PBFT-inspired consensus voting persistence and quorum checking"
owner: "@staff-engineer"
dependencies: []
---

# TDD: `docket vote` Subcommand

## 1. Problem Statement

Claude Code multi-agent teams need a structured way to record consensus decisions. The `/vote` Claude Code skill handles reviewer orchestration and PBFT logic, but it needs a persistence layer to create proposals, record individual votes, and check quorum results.

**Why now:** The `/vote` skill is being built in parallel and requires docket to provide the data layer. Without it, consensus records would be ad-hoc JSON files with no querying, auditability, or integration with the existing issue tracker.

**Constraints:**
- Must use the same SQLite database as issues (`.docket/issues.db`)
- Docket owns persistence and simple quorum math only; PBFT orchestration stays in the `/vote` skill
- Must follow existing CLI patterns (cobra subcommands, `--json` flag, `output.Writer`, `model` types with custom JSON marshaling)
- Max voters equals required quorum count (no over-voting)

**Acceptance Criteria:**
1. `docket vote create` creates a proposal with description, criticality, and required voter count
2. `docket vote show <id>` displays proposal details and current vote tally
3. `docket vote list` lists all proposals with status summary
4. `docket vote cast <id>` records a vote with voter name, role, verdict, confidence, domain_relevance, and findings
5. `docket vote result <id>` shows final consensus result (approved/rejected/pending) with weighted score
6. `docket vote link <id> --issue <issue-id>` links a docket issue to a proposal
7. All commands support `--json` for agent consumption
8. Extra votes beyond required count are rejected
9. When all required votes are cast, weighted approval score is computed and final status is set automatically
10. Every vote records: voter name, role, timestamp, verdict, confidence, domain_relevance, findings

## 2. Context & Prior Art

### Existing Codebase Patterns

The codebase follows a clean layered architecture:

- **`internal/model/`** — Domain types with custom `MarshalJSON`/`UnmarshalJSON`, validation functions, and display helpers (e.g., `model.Issue`, `model.Relation`, `model.Status`)
- **`internal/db/`** — Data access layer with raw `database/sql` queries against SQLite. Uses `scanner` interface abstraction. Error sentinel values (`db.ErrNotFound`, etc.). Schema in `schema.go` with versioned migrations
- **`internal/cli/`** — Cobra commands. Each subcommand group has a parent file (e.g., `issue.go`) registering with `rootCmd`, and child files (e.g., `issue_create.go`). Uses `getWriter()`, `getDB()`, `cmdErr()` helpers. Interactive forms via `charmbracelet/huh` for non-JSON mode
- **`internal/output/`** — `Writer` with `Success(data, message)` and `Error(err, code)` methods. JSON envelope: `{"ok": true, "data": ..., "message": "..."}`. Error codes: `GENERAL_ERROR`, `NOT_FOUND`, `VALIDATION_ERROR`, `CONFLICT`
- **`internal/render/`** — Human-readable table/detail rendering with lipgloss styling

The existing ID format is `DKT-<n>` (e.g., `DKT-5`). Proposals will use a distinct prefix `DKT-V<n>` to avoid ID collisions and make it clear in agent output which entity type is being referenced.

### How PBFT-Inspired Consensus Works (Context Only)

The `/vote` skill (not docket) handles:
1. Spawning 2-4 independent reviewer agents based on criticality
2. Each reviewer reads the artifact and casts a verdict
3. Weighted approval score: `sum(confidence * domain_relevance * verdict_weight) / sum(confidence * domain_relevance)` where approve=1.0, reject=0.0
4. Threshold: weighted score >= 0.67 means approved (2/3 supermajority, matching PBFT 2f+1)

Docket's job is to store the data and compute the final score when quorum is reached. The threshold (0.67) is stored on the proposal so the skill can configure it per-proposal.

## 3. Alternatives Considered

### Alternative A: Separate SQLite Database

Store vote data in `.docket/votes.db` alongside `issues.db`.

**Strengths:** No schema migration needed for existing databases. Clean separation of concerns.
**Weaknesses:** Cannot use foreign keys for issue linking. Two database connections to manage. Breaks the single-DB pattern. More complex transactional guarantees for linking.

### Alternative B: JSON File Storage (`.docket/consensus/`)

Store each proposal as a JSON file, as the `/vote` skill might do on its own.

**Strengths:** Simple, human-readable, no schema changes.
**Weaknesses:** No querying (list/filter), no atomic vote recording, no referential integrity for issue links, doesn't leverage docket's existing DB infrastructure.

### Alternative C: New Tables in Existing Database (Recommended)

Add `proposals`, `votes`, and `proposal_issues` tables to the existing `issues.db` via a schema migration (version 1 -> 2).

**Strengths:** Leverages existing DB infrastructure, foreign keys for issue linking, atomic operations, consistent patterns, single connection. Query-friendly for list/filter operations.
**Weaknesses:** Requires a schema migration. Slightly more complex schema.

**Recommendation:** Alternative C. It follows the established pattern, enables referential integrity for issue links, and keeps all docket data in one place.

## 4. Architecture & System Design

### New Package Structure

```
internal/
  model/
    proposal.go        # Proposal, Vote, ProposalStatus, Verdict, Criticality types
  db/
    proposals.go       # CRUD operations for proposals and votes
    schema.go          # Updated: migration from v1 -> v2 adds proposal tables
  cli/
    vote.go            # Parent "docket vote" command
    vote_create.go     # docket vote create
    vote_show.go       # docket vote show <id>
    vote_list.go       # docket vote list
    vote_cast.go       # docket vote cast <id>
    vote_result.go     # docket vote result <id>
    vote_link.go       # docket vote link <id> --issue <issue-id>
  render/
    vote.go            # Human-readable proposal/vote rendering
```

### Component Boundaries

```
┌─────────────────────────────────────────────┐
│  /vote skill (Claude Code)                   │
│  - Spawns reviewers                          │
│  - Orchestrates PBFT flow                    │
│  - Calls docket vote CLI with --json         │
└──────────────┬──────────────────────────────┘
               │ CLI invocations
┌──────────────▼──────────────────────────────┐
│  cli/vote_*.go                               │
│  - Parses flags/args                         │
│  - Validates input                           │
│  - Delegates to db layer                     │
│  - Formats output via output.Writer          │
└──────────────┬──────────────────────────────┘
               │
┌──────────────▼──────────────────────────────┐
│  db/proposals.go                             │
│  - SQL queries for proposals/votes           │
│  - Quorum check + weighted score computation │
│  - Auto-finalization when quorum reached     │
└──────────────┬──────────────────────────────┘
               │
┌──────────────▼──────────────────────────────┐
│  SQLite (issues.db)                          │
│  - proposals table                           │
│  - votes table                               │
│  - proposal_issues junction table            │
└─────────────────────────────────────────────┘
```

## 5. Data Models & Storage

### Schema Migration (v1 -> v2)

```sql
CREATE TABLE IF NOT EXISTS proposals (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    description     TEXT NOT NULL,
    criticality     TEXT NOT NULL DEFAULT 'medium',
    status          TEXT NOT NULL DEFAULT 'open',
    required_voters INTEGER NOT NULL,
    threshold       REAL NOT NULL DEFAULT 0.67,
    weighted_score  REAL,
    created_by      TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS votes (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    proposal_id      INTEGER NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
    voter_name       TEXT NOT NULL,
    voter_role       TEXT NOT NULL DEFAULT '',
    verdict          TEXT NOT NULL,
    confidence       REAL NOT NULL,
    domain_relevance REAL NOT NULL,
    findings         TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    UNIQUE(proposal_id, voter_name)
);

CREATE TABLE IF NOT EXISTS proposal_issues (
    proposal_id INTEGER NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    PRIMARY KEY (proposal_id, issue_id)
);

CREATE INDEX IF NOT EXISTS idx_proposals_status ON proposals(status);
CREATE INDEX IF NOT EXISTS idx_proposals_created_at ON proposals(created_at);
CREATE INDEX IF NOT EXISTS idx_votes_proposal_id ON votes(proposal_id);
```

### Model Types (`internal/model/proposal.go`)

```go
// ProposalIDPrefix distinguishes proposal IDs from issue IDs.
const ProposalIDPrefix = "DKT-V"

type Criticality string
const (
    CriticalityLow    Criticality = "low"
    CriticalityMedium Criticality = "medium"
    CriticalityHigh   Criticality = "high"
    CriticalityCritical Criticality = "critical"
)

type ProposalStatus string
const (
    ProposalStatusOpen     ProposalStatus = "open"
    ProposalStatusApproved ProposalStatus = "approved"
    ProposalStatusRejected ProposalStatus = "rejected"
)

type Verdict string
const (
    VerdictApprove Verdict = "approve"
    VerdictReject  Verdict = "reject"
)

type Proposal struct {
    ID              int
    Description     string
    Criticality     Criticality
    Status          ProposalStatus
    RequiredVoters  int
    Threshold       float64
    WeightedScore   *float64       // nil until finalized
    CreatedBy       string
    CreatedAt       time.Time
    UpdatedAt       time.Time
}

type Vote struct {
    ID              int
    ProposalID      int
    VoterName       string
    VoterRole       string
    Verdict         Verdict
    Confidence      float64        // 0.0 - 1.0
    DomainRelevance float64        // 0.0 - 1.0
    Findings        string
    CreatedAt       time.Time
}
```

Both types will have custom `MarshalJSON`/`UnmarshalJSON` following the same pattern as `model.Issue`: a private `*JSON` struct with string IDs (`DKT-V<n>`), RFC3339 timestamps, and nil-safe slice handling.

### Quorum Logic (in `db/proposals.go`)

When a vote is cast via `CastVote()`:
1. Insert the vote row (fails on UNIQUE constraint if voter already voted)
2. Count votes for the proposal
3. If count == required_voters:
   a. Compute weighted approval score
   b. Compare against threshold
   c. Update proposal status to "approved" or "rejected" and set weighted_score
4. All within a single transaction for atomicity

Weighted score formula:
```
score = sum(confidence_i * domain_relevance_i * verdict_weight_i) / sum(confidence_i * domain_relevance_i)
```
Where `verdict_weight` is 1.0 for approve, 0.0 for reject.

Edge case: if `sum(confidence * domain_relevance) == 0` (all zeros), treat as rejected.

## 6. API Contracts (CLI)

### `docket vote create`

```
Usage: docket vote create [flags]

Flags:
  -d, --description string     Proposal description (use "-" for stdin) [required in --json mode]
  -c, --criticality string     Criticality level: low|medium|high|critical (default "medium")
  -n, --voters int             Required number of voters [required]
      --threshold float        Approval threshold 0.0-1.0 (default 0.67)
      --created-by string      Creator identity (default: git user.name)
```

**JSON output:**
```json
{
  "ok": true,
  "data": {
    "id": "DKT-V1",
    "description": "Approve TDD for vote subcommand",
    "criticality": "high",
    "status": "open",
    "required_voters": 3,
    "threshold": 0.67,
    "weighted_score": null,
    "created_by": "team-lead",
    "created_at": "2026-03-20T10:00:00Z",
    "updated_at": "2026-03-20T10:00:00Z"
  },
  "message": "Created DKT-V1: Approve TDD for vote subcommand"
}
```

**Validation:**
- `--voters` is required and must be >= 1
- `--description` is required in `--json` mode; interactive form in human mode
- `--threshold` must be > 0.0 and <= 1.0
- `--criticality` must be a valid enum value

### `docket vote show <id>`

```
Usage: docket vote show <id>
```

**JSON output:**
```json
{
  "ok": true,
  "data": {
    "id": "DKT-V1",
    "description": "Approve TDD for vote subcommand",
    "criticality": "high",
    "status": "open",
    "required_voters": 3,
    "threshold": 0.67,
    "weighted_score": null,
    "created_by": "team-lead",
    "created_at": "2026-03-20T10:00:00Z",
    "updated_at": "2026-03-20T10:00:00Z",
    "votes": [
      {
        "id": 1,
        "voter_name": "security-reviewer",
        "voter_role": "security",
        "verdict": "approve",
        "confidence": 0.9,
        "domain_relevance": 0.85,
        "findings": "No security concerns identified",
        "created_at": "2026-03-20T10:05:00Z"
      }
    ],
    "linked_issues": ["DKT-5", "DKT-12"]
  },
  "message": ""
}
```

### `docket vote list`

```
Usage: docket vote list [flags]

Flags:
  -s, --status string        Filter by status: open|approved|rejected
  -c, --criticality string   Filter by criticality
      --all                  Include resolved proposals (default: open only)
      --limit int            Maximum results (default 50)
```

**JSON output:**
```json
{
  "ok": true,
  "data": {
    "proposals": [...],
    "total": 5
  },
  "message": ""
}
```

### `docket vote cast <id>`

```
Usage: docket vote cast <id> [flags]

Flags:
      --voter string              Voter name [required in --json mode]
      --role string               Voter role (default "")
  -v, --verdict string            Vote: approve|reject [required in --json mode]
      --confidence float          Confidence 0.0-1.0 [required in --json mode]
      --domain-relevance float    Domain relevance 0.0-1.0 [required in --json mode]
      --findings string           Review findings (use "-" for stdin, default "")
```

**JSON output (vote cast, quorum not yet reached):**
```json
{
  "ok": true,
  "data": {
    "vote": {
      "id": 2,
      "voter_name": "architecture-reviewer",
      "voter_role": "architecture",
      "verdict": "approve",
      "confidence": 0.95,
      "domain_relevance": 0.9,
      "findings": "Clean separation of concerns",
      "created_at": "2026-03-20T10:10:00Z"
    },
    "proposal_status": "open",
    "votes_cast": 2,
    "votes_required": 3,
    "quorum_reached": false
  },
  "message": "Vote recorded for DKT-V1 (2/3 votes cast)"
}
```

**JSON output (quorum reached, auto-finalized):**
```json
{
  "ok": true,
  "data": {
    "vote": { ... },
    "proposal_status": "approved",
    "votes_cast": 3,
    "votes_required": 3,
    "quorum_reached": true,
    "weighted_score": 0.89
  },
  "message": "Vote recorded for DKT-V1 (3/3 votes cast) - APPROVED (score: 0.89)"
}
```

**Error cases:**
- Proposal not found: `NOT_FOUND`
- Proposal already finalized: `CONFLICT` ("proposal DKT-V1 is already resolved")
- Voter already voted: `CONFLICT` ("voter 'security-reviewer' has already voted on DKT-V1")
- Quorum full (should not happen if max voters == required, but guard anyway): `CONFLICT`
- Confidence or domain_relevance outside [0.0, 1.0]: `VALIDATION_ERROR`

### `docket vote result <id>`

```
Usage: docket vote result <id>
```

**JSON output:**
```json
{
  "ok": true,
  "data": {
    "id": "DKT-V1",
    "status": "approved",
    "weighted_score": 0.89,
    "threshold": 0.67,
    "votes_cast": 3,
    "votes_required": 3,
    "quorum_reached": true,
    "votes": [ ... ]
  },
  "message": ""
}
```

If still pending (not all votes cast), `status` is `"open"`, `weighted_score` is `null`, and `quorum_reached` is `false`.

### `docket vote link <id> --issue <issue-id>`

```
Usage: docket vote link <id> --issue <issue-id>

Flags:
      --issue string   Issue ID to link (repeatable)
```

**JSON output:**
```json
{
  "ok": true,
  "data": {
    "proposal_id": "DKT-V1",
    "issue_id": "DKT-5"
  },
  "message": "Linked DKT-5 to proposal DKT-V1"
}
```

**Error cases:**
- Proposal not found: `NOT_FOUND`
- Issue not found: `NOT_FOUND`
- Already linked: `CONFLICT`

## 7. Migration & Rollout

### Schema Migration Strategy

**Version bump:** `currentSchemaVersion` changes from 1 to 2.

The migration function `migrations[2]` will:
1. Create the `proposals`, `votes`, and `proposal_issues` tables
2. Create the indexes

This follows the existing migration pattern in `schema.go` where `Migrate()` is called during `docket init` and on every database open (via `PersistentPreRunE`).

**Important:** The initial `schemaDDL` string should NOT be modified. New tables go only in the migration function. This ensures existing v1 databases migrate correctly and new databases get v1 DDL + v2 migration applied sequentially.

**Rollback:** Since this is additive (new tables only, no existing table modifications), rollback simply means ignoring the new tables. A future migration could drop them if the feature is removed.

### Backward Compatibility

- No existing commands are modified
- No existing tables are altered
- The `docket vote` subcommand group is additive
- Existing databases auto-migrate on next command invocation via `Migrate()`

## 8. Risks & Open Questions

### Risks

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Schema migration fails on existing databases | Low | High | Migration is additive (CREATE TABLE IF NOT EXISTS). Test with existing populated databases |
| `/vote` skill API expectations drift from this design | Medium | Medium | JSON contract examples in this TDD serve as the interface spec. Coordinate with skill development |
| Weighted score edge cases (all-zero weights) | Low | Low | Explicit guard: treat as rejected with score 0.0 |
| Concurrent vote casting (two agents casting simultaneously) | Low | Medium | SQLite single-writer + UNIQUE constraint prevents double-voting. Busy timeout handles contention |

### Open Questions

1. **Should `docket vote unlink` be included?** Yes — included, following the `issue link remove` pattern. ✅ Resolved.

2. **Should proposals support deletion?** No — omitted. Proposals are audit records. ✅ Resolved.

3. **Should the human-readable output for `vote list` use the same grouped-table pattern as `issue list`?** No — uses a simple flat table (ID, description, status, votes cast/required, criticality). ✅ Resolved.

## 9. Testing Strategy

### Unit Tests (`internal/model/`)
- Validation functions: `ValidateCriticality`, `ValidateProposalStatus`, `ValidateVerdict`
- `FormatProposalID` / `ParseProposalID` round-trip
- Custom JSON marshaling/unmarshaling for `Proposal` and `Vote`

### Integration Tests (`internal/db/`)
- `CreateProposal` / `GetProposal` / `ListProposals` CRUD
- `CastVote` — happy path, duplicate voter rejection, quorum-full rejection
- Auto-finalization: cast required number of votes, verify status changes
- Weighted score computation: known inputs -> expected output
- `LinkProposalIssue` — happy path, missing proposal, missing issue, duplicate link
- Schema migration from v1 to v2

### CLI Tests
- Each subcommand with `--json` flag, verifying envelope structure
- Error cases: missing required flags, invalid IDs, not-found entities
- Interactive mode smoke test for `vote create` (if feasible)

## 10. Observability & Operational Readiness

This is a local CLI tool, so traditional observability (dashboards, alerts) does not apply. Key diagnostic considerations:

- **Schema version mismatch:** Clear error message if migration fails, including current and target versions
- **`--json` error envelopes:** All error cases return structured `{"ok": false, "error": "...", "code": "..."}` so the `/vote` skill can programmatically handle failures
- **Audit trail:** Every vote records voter_name, voter_role, timestamp, and findings. The proposal records created_by and timestamps. This is the primary observability mechanism for agent teams

## 11. Implementation Phases

### Phase 1: Data Layer (Size: M)

**Files:** `internal/model/proposal.go`, `internal/db/proposals.go`, `internal/db/schema.go`

- Define `Proposal`, `Vote`, `Criticality`, `ProposalStatus`, `Verdict` types with validation
- Define `FormatProposalID` / `ParseProposalID` (prefix `DKT-V`)
- Custom JSON marshaling for both types
- Add migration v1->v2 in `schema.go`
- Implement DB functions: `CreateProposal`, `GetProposal`, `ListProposals`, `CastVote` (with auto-finalization), `GetProposalVotes`, `LinkProposalIssue`, `GetProposalIssues`
- Unit and integration tests

**Depends on:** Nothing
**Blocks:** Phase 2, Phase 3

### Phase 2: CLI Commands (Size: M)

**Files:** `internal/cli/vote.go`, `vote_create.go`, `vote_show.go`, `vote_list.go`, `vote_cast.go`, `vote_result.go`, `vote_link.go`

- Register `voteCmd` parent command with `rootCmd`
- Implement all 6 subcommands following existing patterns
- Interactive forms for `vote create` and `vote cast` in non-JSON mode
- `--json` support on all commands via `getWriter()`

**Depends on:** Phase 1
**Blocks:** Phase 3

### Phase 3: Rendering (Size: S)

**Files:** `internal/render/vote.go`

- Table rendering for `vote list` (proposal table with status, votes, criticality)
- Detail rendering for `vote show` (proposal details + vote list)
- Result rendering for `vote result` (status banner + score + vote breakdown)

**Depends on:** Phase 1 (model types)
**Blocks:** Nothing (Phase 2 can stub human messages initially)

All three phases can be parallelized with Phase 1 starting first, then Phase 2 and Phase 3 in parallel once Phase 1 is complete. Phase 2 can use simple string messages initially and integrate Phase 3 renderers once available.
