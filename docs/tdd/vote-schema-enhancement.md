---
project: "docket"
maturity: "implemented"
last_updated: "2026-03-20"
updated_by: "@staff-engineer"
scope: "Enhance vote schema (v2->v3) with structured findings, richer proposal metadata, approve-with-concerns verdict, and committed lifecycle status"
owner: "@staff-engineer"
dependencies:
  - vote-subcommand.md
---

# TDD: Vote Schema Enhancement (v2 -> v3)

## 1. Problem Statement

The `/vote` skill produces richer consensus records than what docket's v2 schema can persist. Specifically, the skill generates structured review findings (blockers, concerns, suggestions), nuanced verdicts (approve-with-concerns), proposal rationale and domain tags, file change tracking, and post-approval lifecycle states (committed). These records are currently either flattened into free-text `findings` or lost entirely.

**Why now:** The v2 schema was designed for MVP vote persistence. The `/vote` skill has matured and its output format has stabilized. Enriching the schema now lets docket serve as the canonical, queryable record of consensus decisions -- not just a pass/fail tally.

**Constraints:**
- Migration must be additive -- no breaking changes to existing v2 data
- All new columns must have sensible defaults so existing records remain valid
- Existing CLI commands must continue to work identically unless a flag is explicitly added
- The `--findings` flag (string) must remain backward-compatible; the new structured format is opt-in via `--findings-json`
- JSON output format changes must be backward-compatible (new fields only, no removals or renames)

**Acceptance Criteria:**
1. Schema migration v2->v3 adds new columns to `proposals` and `votes` tables; no existing columns are modified or dropped
2. `proposals` table gains: `rationale` (TEXT), `domain_tags` (TEXT, JSON array), `files_changed` (TEXT, JSON array), `final_outcome` (TEXT), `escalation_reason` (TEXT)
3. `votes` table gains: `findings_json` (TEXT, structured JSON), `summary` (TEXT)
4. `Verdict` enum accepts `approve-with-concerns` in addition to `approve` and `reject`
5. `ProposalStatus` enum accepts `committed` in addition to `open`, `approved`, and `rejected`
6. `docket vote create` accepts `--rationale`, `--domain-tags`, and `--files-changed` flags
7. `docket vote cast` accepts `--findings-json` flag (structured JSON string) and `--summary` flag; the existing `--findings` flag continues to work as plain text
8. `docket vote show` and `docket vote result` display `effective_weight` (computed as `confidence * domain_relevance`) for each vote in JSON output
9. `docket vote commit <id>` transitions an `approved` proposal to `committed` status with an optional `--outcome` message
10. All new fields appear in `--json` output; existing fields retain their current names and types
11. Existing v2 databases migrate cleanly: `findings` values are preserved; `findings_json` defaults to `null`

## 2. Context & Prior Art

### Current v2 Schema State

The v2 schema (introduced by `vote-subcommand.md`) defines three tables:

- **`proposals`**: `id`, `description`, `criticality`, `status` (open/approved/rejected), `required_voters`, `threshold`, `weighted_score`, `created_by`, `created_at`, `updated_at`
- **`votes`**: `id`, `proposal_id`, `voter_name`, `voter_role`, `verdict` (approve/reject), `confidence`, `domain_relevance`, `findings` (TEXT), `created_at`
- **`proposal_issues`**: junction table for proposal-issue links

The current `findings` field stores free-text review notes. The `/vote` skill's output format has evolved to produce structured findings with categorized severity levels.

### Reference Consensus Model

The operator-provided reference JSON shows the target-state data model that the `/vote` skill produces:

```json
{
  "proposal": {
    "rationale": "...",
    "domain_tags": ["architecture", "developer-experience"],
    "files_changed": ["src/user.rs", ".claude/skills/..."],
    "criticality": "medium"
  },
  "review": {
    "verdict": "approve-with-concerns",
    "confidence": 0.80,
    "domain_relevance": 0.90,
    "effective_weight": 0.72,
    "findings": {
      "blockers": [],
      "concerns": ["..."],
      "suggestions": ["..."]
    },
    "summary": "Architecturally sound with three practical concerns."
  },
  "outcome": "committed",
  "final_outcome": "Consensus reached.",
  "escalation_reason": null
}
```

### Design Principle: Signal vs. Noise

Not every field from the reference JSON warrants a database column. The filter applied here is: **does persisting this field improve queryability, auditability, or agent decision-making?**

| Field | Verdict | Reasoning |
|---|---|---|
| `rationale` | **Add** | Critical for auditability -- why was this proposed? Currently only `description` captures *what*, not *why* |
| `domain_tags` | **Add** | Enables filtering proposals by domain (e.g., "show me all security-related votes"). Stored as JSON array in TEXT |
| `files_changed` | **Add** | Enables "which proposals touched this file?" queries. High value for traceability |
| `final_outcome` | **Add** | Human-readable summary of the consensus result. Distinct from `status` (enum) -- captures nuance |
| `escalation_reason` | **Add** | Records why consensus was escalated (null when not escalated). Audit value |
| `findings_json` | **Add** | Structured blockers/concerns/suggestions enables severity-based filtering and automated blocker detection |
| `summary` | **Add** | Per-vote one-liner for table rendering and agent digests |
| `effective_weight` | **Computed** | `confidence * domain_relevance` -- derive at query time, do NOT store. Adding a stored column would create a denormalization that could drift |
| `approve-with-concerns` | **Add** | Captures the middle ground between full approval and rejection. Treated as approval for quorum math (weight 1.0) but surfaces concerns prominently |
| `committed` status | **Add** | Post-approval lifecycle state: "this decision has been acted upon." Prevents re-committing and provides completion signal |
| `consensus_id` | **Skip** | The proposal ID (`DKT-V<n>`) already serves this purpose |
| `artifact_type` / `artifact_ref` | **Skip** | These are `/vote` skill concerns, not persistence-layer concerns. The skill can include them in `rationale` or `description` |
| `round` / multi-round support | **Skip** | The current single-round model covers the practical use case. Multi-round voting is orchestration complexity that belongs in the skill, not the data layer |
| `quorum` object | **Skip** | Fully derivable from existing `threshold`, `weighted_score`, `required_voters`, and vote count |

## 3. Alternatives Considered

### Alternative A: Replace `findings` TEXT with `findings` JSON (Breaking Change)

Change the existing `findings` column type from free-text to structured JSON.

**Strengths:** Single field, no ambiguity about which findings field to use.
**Weaknesses:** Breaks existing v2 data (plain text is not valid JSON). Breaks existing `--findings "some text"` CLI usage. Requires complex data migration to wrap existing text in `{"summary": "..."}` objects, which lossy-transforms the original data.

**Rejected** -- breaking existing data and CLI contracts violates the additive migration constraint.

### Alternative B: Add `findings_json` Alongside `findings` (Recommended)

Add a new `findings_json` column (TEXT, nullable) that stores structured JSON. Keep the existing `findings` column as-is for plain-text findings.

**Strengths:** Zero breaking changes. Existing CLI usage (`--findings "text"`) works identically. New structured format is opt-in. During a transition period, agents can use either. The JSON output merges both: if `findings_json` is present, it takes precedence for structured display; `findings` always available as the plain-text fallback.
**Weaknesses:** Two findings fields is conceptually redundant. Over time, `findings` may become vestigial.

**Recommendation:** Alternative B. The transition cost is near-zero, and the `/vote` skill can immediately start populating `findings_json` while older integrations continue using `findings`. A future v4 migration could deprecate the plain-text field once adoption is confirmed.

### Alternative C: Store All New Metadata as a Single JSON Blob Column

Add a single `metadata` TEXT column to both tables and dump all new fields into it.

**Strengths:** Maximum flexibility, single migration, no schema changes for future fields.
**Weaknesses:** Cannot index or query individual fields (e.g., `WHERE domain_tags LIKE '%security%'`). Loses type safety. Makes the schema opaque -- violates the principle that the schema should document the data model.

**Rejected** -- the fields being added have clear, stable semantics and benefit from being individually queryable.

## 4. Architecture & System Design

### Change Scope

This enhancement modifies existing files only -- no new files are created.

```
internal/
  model/
    proposal.go        # Add Verdict/ProposalStatus enum values, Findings struct,
                       # new Proposal/Vote fields, updated JSON marshaling
  db/
    schema.go          # Add migrateV2ToV3, bump currentSchemaVersion to 3
    proposals.go       # Update CRUD queries for new columns, add CommitProposal()
  cli/
    vote_create.go     # Add --rationale, --domain-tags, --files-changed flags
    vote_cast.go       # Add --findings-json, --summary flags; add approve-with-concerns to select
    vote_commit.go     # NEW FILE: docket vote commit <id> [--outcome]
    vote_show.go       # Add effective_weight to vote JSON output
    vote_result.go     # Add effective_weight to vote JSON output
  render/
    vote.go            # Update detail/result rendering for new fields
```

### Component Interaction (Unchanged)

The architecture from the v2 TDD remains identical. The `/vote` skill calls docket CLI commands with `--json`. This enhancement only adds new flags and output fields -- the interaction pattern is unchanged.

### Quorum Math Update

The `approve-with-concerns` verdict is treated identically to `approve` for weighted score computation (verdict_weight = 1.0). This preserves the existing quorum formula:

```
score = sum(confidence_i * domain_relevance_i * verdict_weight_i) / sum(confidence_i * domain_relevance_i)
```

Where `verdict_weight`:
- `approve` = 1.0
- `approve-with-concerns` = 1.0
- `reject` = 0.0

The rationale: approve-with-concerns signals "this should proceed, but address these concerns." It is affirmative consensus with caveats, not a partial rejection.

## 5. Data Models & Storage

### Schema Migration (v2 -> v3)

```sql
-- New columns on proposals
ALTER TABLE proposals ADD COLUMN rationale TEXT NOT NULL DEFAULT '';
ALTER TABLE proposals ADD COLUMN domain_tags TEXT NOT NULL DEFAULT '[]';
ALTER TABLE proposals ADD COLUMN files_changed TEXT NOT NULL DEFAULT '[]';
ALTER TABLE proposals ADD COLUMN final_outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE proposals ADD COLUMN escalation_reason TEXT;

-- New columns on votes
ALTER TABLE votes ADD COLUMN findings_json TEXT;
ALTER TABLE votes ADD COLUMN summary TEXT NOT NULL DEFAULT '';
```

**Notes:**
- `domain_tags` and `files_changed` default to `'[]'` (empty JSON array) so existing rows are valid JSON
- `findings_json` defaults to `NULL` (not all votes have structured findings; backward compatibility)
- `escalation_reason` defaults to `NULL` (most proposals are not escalated)
- No new indexes are needed -- the existing indexes on `status` and `created_at` cover the primary query patterns. `domain_tags` queries will use `LIKE` or `json_each()` which don't benefit from a B-tree index on a JSON TEXT column

### Updated Model Types

```go
// Verdict enum -- add approve-with-concerns
const (
    VerdictApprove             Verdict = "approve"
    VerdictApproveWithConcerns Verdict = "approve-with-concerns"
    VerdictReject              Verdict = "reject"
)

// ProposalStatus enum -- add committed
const (
    ProposalStatusOpen      ProposalStatus = "open"
    ProposalStatusApproved  ProposalStatus = "approved"
    ProposalStatusRejected  ProposalStatus = "rejected"
    ProposalStatusCommitted ProposalStatus = "committed"
)

// Findings represents structured review findings.
type Findings struct {
    Blockers    []string `json:"blockers"`
    Concerns    []string `json:"concerns"`
    Suggestions []string `json:"suggestions"`
}

// Updated Proposal struct (new fields only shown)
type Proposal struct {
    // ... existing fields ...
    Rationale        string   // why this proposal exists
    DomainTags       []string // e.g. ["architecture", "security"]
    FilesChanged     []string // e.g. ["src/user.rs"]
    FinalOutcome     string   // human-readable consensus summary
    EscalationReason *string  // nil when not escalated
}

// Updated Vote struct (new fields only shown)
type Vote struct {
    // ... existing fields ...
    FindingsJSON *Findings // structured findings (nullable)
    Summary      string    // one-line review summary
}
```

### JSON Marshaling Changes

**Proposal JSON** -- new fields added to the wire format:
```json
{
  "id": "DKT-V1",
  "description": "...",
  "rationale": "",
  "domain_tags": [],
  "files_changed": [],
  "criticality": "medium",
  "status": "committed",
  "final_outcome": "Consensus reached.",
  "escalation_reason": null,
  "required_voters": 3,
  "threshold": 0.67,
  "weighted_score": 0.89,
  "created_by": "team-lead",
  "created_at": "2026-03-20T10:00:00Z",
  "updated_at": "2026-03-20T10:00:00Z"
}
```

**Vote JSON** -- new fields added:
```json
{
  "id": 1,
  "proposal_id": "DKT-V1",
  "voter_name": "@staff-engineer",
  "voter_role": "security",
  "verdict": "approve-with-concerns",
  "confidence": 0.80,
  "domain_relevance": 0.90,
  "effective_weight": 0.72,
  "findings": "Architecturally sound with concerns.",
  "findings_json": {
    "blockers": [],
    "concerns": ["hardcoded paths will break cross-project"],
    "suggestions": ["Add guard clause for repo-scoped skills"]
  },
  "summary": "Architecturally sound with three practical concerns.",
  "created_at": "2026-03-20T10:05:00Z"
}
```

**Key decisions:**
- `effective_weight` is computed in `MarshalJSON()`, NOT stored in the database. It is `confidence * domain_relevance`, calculated fresh on every serialization.
- `findings` (plain text) and `findings_json` (structured) coexist. Both appear in JSON output. The skill sets whichever is appropriate; display logic prefers `findings_json` when present.
- `domain_tags` and `files_changed` are `[]string` in Go, marshaled as JSON arrays, stored as `TEXT` in SQLite. Empty slices serialize as `[]`, not `null`.

### DB Layer Changes

**`scanProposalFrom`** -- update to scan 5 additional columns. The `domain_tags` and `files_changed` TEXT columns are unmarshaled from JSON into `[]string`. `escalation_reason` uses `sql.NullString`.

**`scanVoteFrom`** -- update to scan 2 additional columns. `findings_json` is a nullable TEXT column; when non-null, unmarshal into `*Findings`.

**`CreateProposal`** -- update INSERT to include `rationale`, `domain_tags` (marshal `[]string` to JSON), `files_changed` (marshal to JSON), `final_outcome`, `escalation_reason`.

**`CastVote`** -- update INSERT to include `findings_json` (marshal `*Findings` to JSON or NULL), `summary`. Update quorum logic: `VerdictApproveWithConcerns` has verdict_weight 1.0.

**New function `CommitProposal(db, id, outcome)`**:
1. Load proposal within transaction
2. Verify status is `approved` (reject if open, rejected, or already committed)
3. Update status to `committed`, set `final_outcome` to the provided outcome string, update `updated_at`
4. Commit transaction

## 6. API Contracts (CLI)

### Updated: `docket vote create`

New flags (all optional):
```
--rationale string         Why this proposal exists (use "-" for stdin)
--domain-tags strings      Comma-separated domain tags (e.g. "architecture,security")
--files-changed strings    Comma-separated file paths affected
```

**JSON output** -- new fields in `data`:
```json
{
  "ok": true,
  "data": {
    "id": "DKT-V1",
    "description": "Approve TDD for vote schema enhancement",
    "rationale": "Schema gaps identified in v2 limiting consensus quality",
    "domain_tags": ["architecture", "data-model"],
    "files_changed": ["internal/db/schema.go", "internal/model/proposal.go"],
    "criticality": "medium",
    "status": "open",
    "final_outcome": "",
    "escalation_reason": null,
    "required_voters": 3,
    "threshold": 0.67,
    "weighted_score": null,
    "created_by": "team-lead",
    "created_at": "2026-03-20T10:00:00Z",
    "updated_at": "2026-03-20T10:00:00Z"
  },
  "message": "Created DKT-V1: Approve TDD for vote schema enhancement"
}
```

### Updated: `docket vote cast <id>`

New flags:
```
--findings-json string     Structured findings as JSON (e.g. '{"blockers":[],"concerns":["..."],"suggestions":[]}')
--summary string           One-line review summary
```

Updated `--verdict` options: `approve|approve-with-concerns|reject`

**Validation:**
- `--findings` and `--findings-json` are mutually exclusive in intent but both can be provided (plain text goes to `findings`, structured JSON goes to `findings_json`)
- `--findings-json` must be valid JSON matching the `Findings` schema; return `VALIDATION_ERROR` on malformed input
- Interactive form adds `approve-with-concerns` to the verdict select options and adds a `Summary` text input

**JSON output** -- vote object gains `effective_weight`, `findings_json`, `summary`:
```json
{
  "ok": true,
  "data": {
    "vote": {
      "id": 2,
      "voter_name": "@staff-engineer",
      "voter_role": "architecture",
      "verdict": "approve-with-concerns",
      "confidence": 0.80,
      "domain_relevance": 0.90,
      "effective_weight": 0.72,
      "findings": "",
      "findings_json": {
        "blockers": [],
        "concerns": ["hardcoded paths"],
        "suggestions": ["Add guard clause"]
      },
      "summary": "Architecturally sound with concerns.",
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

### New: `docket vote commit <id>`

```
Usage: docket vote commit <id> [flags]

Flags:
  --outcome string    Final outcome description (default: "Committed")
```

Transitions an `approved` proposal to `committed` status.

**JSON output:**
```json
{
  "ok": true,
  "data": {
    "id": "DKT-V1",
    "status": "committed",
    "final_outcome": "Consensus reached. Changes applied to main branch.",
    "updated_at": "2026-03-20T11:00:00Z"
  },
  "message": "DKT-V1 committed: Consensus reached. Changes applied to main branch."
}
```

**Error cases:**
- Proposal not found: `NOT_FOUND`
- Proposal not approved (open/rejected): `CONFLICT` ("proposal DKT-V1 must be approved before it can be committed; current status: open")
- Proposal already committed: `CONFLICT` ("proposal DKT-V1 is already committed")

### Updated: `docket vote show <id>` and `docket vote result <id>`

No new flags. Output gains:
- Proposal: `rationale`, `domain_tags`, `files_changed`, `final_outcome`, `escalation_reason`
- Each vote: `effective_weight`, `findings_json`, `summary`

The human-readable rendering updates:
- `vote show` detail view: new "Rationale" section (if non-empty), domain tags rendered as inline badges, files_changed as a list
- `vote show` vote list: shows `approve-with-concerns` in yellow (distinct from green approve), appends summary below findings
- `vote result` breakdown table: adds "Weight" column showing `effective_weight` (computed, formatted as `%.2f`)
- `vote result` for committed proposals: status banner shows "COMMITTED" in purple/magenta

### Updated: `docket vote list`

New filter flag:
```
--domain-tag string    Filter proposals by domain tag (substring match on JSON array)
```

Table adds no new columns (existing 5-column layout is already dense). The `status` column now renders `committed` with distinct styling.

## 7. Migration & Rollout

### Schema Migration Strategy

**Version bump:** `currentSchemaVersion` changes from 2 to 3.

The migration function `migrations[3]` (`migrateV2ToV3`) executes:
1. `ALTER TABLE proposals ADD COLUMN ...` for 5 new columns
2. `ALTER TABLE votes ADD COLUMN ...` for 2 new columns

SQLite's `ALTER TABLE ... ADD COLUMN` appends columns with defaults. This is atomic and safe for existing data. No data transformation is needed.

**Important:** SQLite does not support `ALTER TABLE ... ADD COLUMN ... NOT NULL` without a default value. All new NOT NULL columns have explicit defaults. Nullable columns (`escalation_reason`, `findings_json`) omit NOT NULL.

### Backward Compatibility

- All existing v2 CLI commands work identically -- new flags are optional with sensible defaults
- Existing `--findings "text"` usage is unchanged; `findings_json` is a separate column
- JSON output adds new fields but does not remove or rename existing ones
- Existing scripts parsing `--json` output will not break (additive changes only)
- The `approve-with-concerns` verdict is a new option, not a replacement
- The `committed` status is an additional lifecycle state, not a replacement for `approved`

### Migration from v1

Databases still at v1 will apply v1->v2 then v2->v3 sequentially. The existing `Migrate()` loop handles this correctly.

### Rollback

Since all changes are additive columns, rollback means ignoring the new columns. SQLite does not natively support `DROP COLUMN` in older versions, but the columns are harmless if unused. A future migration could drop them if the feature is removed (SQLite 3.35.0+ supports `ALTER TABLE ... DROP COLUMN`).

## 8. Risks & Open Questions

### Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `ALTER TABLE ADD COLUMN` fails on large databases | Very Low | High | SQLite ADD COLUMN is O(1) -- it modifies table metadata only, does not rewrite rows. No data-size risk |
| JSON parsing overhead for `findings_json` on every vote scan | Low | Low | Only parsed when scanning votes, not during list queries. JSON payloads are small (< 1KB) |
| `approve-with-concerns` weight of 1.0 skews quorum math | Low | Medium | This is intentional -- concerns are advisory, not dissent. If the skill needs partial weighting, it can adjust `confidence` instead |
| `domain_tags` substring matching (`LIKE '%security%'`) produces false positives | Medium | Low | Tags like "api-security" would match "security" query. Acceptable for MVP. Future: use `json_each()` for exact matching |
| Migration ordering: v1 databases need v1->v2->v3 chain | Low | High | Existing `Migrate()` loop applies migrations sequentially. Test with v1, v2, and v3 databases |

### Open Questions

1. **Should `docket vote commit` accept `--escalation-reason`?** Yes — `--escalation-reason` added to both `vote commit` and `vote create`. When provided, it sets the `escalation_reason` field on the proposal. When empty (default), the existing value is preserved. ✅ Resolved.

2. **Should `domain_tags` filtering use exact match or substring?** Exact match via `json_each()`. Docket requires a recent SQLite via go-sqlite3 which includes JSON1. ✅ Resolved.

3. **Should `effective_weight` appear in the human-readable table, or only in `--json` output?** Both — appears in the vote breakdown table as a "Weight" column and inline in detail views. Computed at render time, not stored. ✅ Resolved.

## 9. Testing Strategy

### Unit Tests (`internal/model/`)

- `ValidateVerdict`: accept `approve-with-concerns`, reject `partial-approve`
- `ValidateProposalStatus`: accept `committed`, reject `done`
- `Findings` JSON round-trip: marshal/unmarshal with empty arrays, populated arrays, nil
- `Proposal` JSON round-trip with new fields: `rationale`, `domain_tags`, `files_changed`, `final_outcome`, `escalation_reason`
- `Vote` JSON round-trip with new fields: `findings_json`, `summary`, computed `effective_weight`
- `Vote` JSON backward compatibility: unmarshal v2-era JSON (no `findings_json`, no `summary`) without error

### Integration Tests (`internal/db/`)

- **Migration v2->v3:** Apply to a v2 database with existing proposals and votes; verify new columns exist with correct defaults; verify existing data is intact
- **Migration v1->v2->v3 chain:** Apply full chain to a v1 database
- **CreateProposal with new fields:** Round-trip rationale, domain_tags (JSON), files_changed (JSON), final_outcome, escalation_reason
- **CastVote with findings_json:** Round-trip structured findings; verify NULL when not provided
- **CastVote with approve-with-concerns:** Verify quorum math treats it as approval (weight 1.0)
- **CommitProposal:** Happy path (approved -> committed), reject on open, reject on rejected, reject on already committed
- **ListProposals with domain_tag filter:** Filter by tag, verify results

### CLI Tests

- `vote create` with `--rationale`, `--domain-tags`, `--files-changed`: verify JSON output
- `vote cast` with `--findings-json`: verify structured findings in JSON output
- `vote cast` with `--verdict approve-with-concerns`: verify acceptance
- `vote commit`: happy path, error cases
- `vote show`: verify new fields in JSON output, verify `effective_weight` is present
- `vote result`: verify `effective_weight` in vote breakdown

## 10. Observability & Operational Readiness

Same as v2 TDD -- this is a local CLI tool. Key additions:

- **Migration diagnostics:** If v2->v3 migration fails, error message includes current version and specific ALTER TABLE that failed
- **Structured findings validation:** `--findings-json` parse errors return `VALIDATION_ERROR` with the specific JSON parse error, enabling the `/vote` skill to diagnose malformed input
- **Committed status audit:** The `committed` status + `final_outcome` field create a complete audit trail from proposal through approval to action

## 11. Implementation Phases

### Phase 1: Schema & Model Layer (Size: S)

**Files:** `internal/model/proposal.go`, `internal/db/schema.go`

- Add `VerdictApproveWithConcerns` to `Verdict` enum and `validVerdicts` slice
- Add `ProposalStatusCommitted` to `ProposalStatus` enum and `validProposalStatuses` slice
- Add `Findings` struct with JSON tags
- Add new fields to `Proposal` struct: `Rationale`, `DomainTags`, `FilesChanged`, `FinalOutcome`, `EscalationReason`
- Add new fields to `Vote` struct: `FindingsJSON`, `Summary`
- Update `proposalJSON` and `voteJSON` wire formats
- Update `MarshalJSON` / `UnmarshalJSON` for both types (Vote gains computed `effective_weight`)
- Add `migrateV2ToV3` to `migrations` map in `schema.go`
- Bump `currentSchemaVersion` to 3
- Unit tests for all new enum values, Findings round-trip, updated JSON marshaling

**Depends on:** Nothing
**Blocks:** Phase 2, Phase 3

### Phase 2: DB Layer Updates (Size: S)

**Files:** `internal/db/proposals.go`

- Update `scanProposalFrom` to scan 5 new columns (with JSON unmarshaling for `domain_tags` and `files_changed`)
- Update `scanVoteFrom` to scan 2 new columns (with JSON unmarshaling for `findings_json`)
- Update `CreateProposal` INSERT to include new columns
- Update `CastVote` INSERT to include new columns; update quorum logic for `VerdictApproveWithConcerns`
- Update `ListProposals` to support `domainTag` filter parameter
- Add `CommitProposal(db, id, outcome)` function
- Integration tests for migration, new CRUD operations, CommitProposal, domain_tag filtering

**Depends on:** Phase 1
**Blocks:** Phase 3

### Phase 3: CLI & Rendering (Size: M)

**Files:** `internal/cli/vote_create.go`, `vote_cast.go`, `vote_commit.go` (new), `vote_show.go`, `vote_result.go`, `vote_list.go`, `internal/render/vote.go`

- `vote_create.go`: Add `--rationale`, `--domain-tags`, `--files-changed` flags; update interactive form
- `vote_cast.go`: Add `--findings-json`, `--summary` flags; add `approve-with-concerns` to interactive select; JSON parse validation for `--findings-json`
- `vote_commit.go` (new file): Implement `docket vote commit <id> [--outcome]` command
- `vote_show.go`: Update `voteShowResultJSON` with new proposal and vote fields; add `effective_weight`
- `vote_result.go`: Update `voteResultData` with new fields; add `effective_weight`
- `vote_list.go`: Add `--domain-tag` filter flag; update status styling for `committed`
- `render/vote.go`: Update detail rendering for rationale/domain_tags/files_changed; add `effective_weight` to vote breakdown table; add `committed` status color; update interactive form for new verdict option; render structured findings with severity labels

**Depends on:** Phase 2
**Blocks:** Nothing

Phases 1 and 2 are small and should be done sequentially. Phase 3 is medium but has no external dependencies once Phase 2 is complete.
