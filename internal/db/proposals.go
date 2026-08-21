package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// ErrConflict is returned when an operation violates a uniqueness or state constraint.
var ErrConflict = errors.New("conflict")

// CastVoteResult holds the outcome of a CastVote operation, including whether
// quorum was reached and the proposal's updated status.
type CastVoteResult struct {
	Vote           *model.Vote
	ProposalStatus model.ProposalStatus
	VotesCast      int
	VotesRequired  int
	QuorumReached  bool
	WeightedScore  *float64
}

// CreateProposal inserts a new proposal and returns its ID.
func CreateProposal(db *sql.DB, p *model.Proposal) (int, error) {
	return CreateProposalIdempotent(db, p, "")
}

// CreateProposalIdempotent is CreateProposal with an optional idempotency key.
// A repeat call with the same key returns the original proposal id and inserts
// nothing. Unlike the plain path this runs in a transaction, so the insert and
// the key record commit together.
func CreateProposalIdempotent(db *sql.DB, p *model.Proposal, idempotencyKey string) (int, error) {
	if idempotencyKey != "" {
		existingID, found, err := LookupIdempotencyKey(db, ScopeVoteCreate, idempotencyKey)
		if err != nil {
			return 0, err
		}
		if found {
			return existingID, nil
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	domainTagsJSON, err := json.Marshal(p.DomainTags)
	if err != nil {
		return 0, fmt.Errorf("marshaling domain_tags: %w", err)
	}

	filesChangedJSON, err := json.Marshal(p.FilesChanged)
	if err != nil {
		return 0, fmt.Errorf("marshaling files_changed: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO proposals (project_id, description, rationale, domain_tags, files_changed, criticality, status, final_outcome, escalation_reason, required_voters, threshold, weighted_score, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		projectOrDefault(p.ProjectID),
		p.Description,
		p.Rationale,
		string(domainTagsJSON),
		string(filesChangedJSON),
		string(p.Criticality),
		string(p.Status),
		p.FinalOutcome,
		p.EscalationReason,
		p.RequiredVoters,
		p.Threshold,
		p.WeightedScore,
		p.CreatedBy,
		now,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("inserting proposal: %w", err)
	}

	id64, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting last insert id: %w", err)
	}
	id := int(id64)

	if idempotencyKey != "" {
		if err := RecordIdempotencyKeyTx(tx, ScopeVoteCreate, idempotencyKey, id); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("committing transaction: %w", err)
	}

	return id, nil
}

// GetProposal returns a proposal by ID, or ErrNotFound if it does not exist.
func GetProposal(db *sql.DB, id int) (*model.Proposal, error) {
	row := db.QueryRow(
		`SELECT id, description, rationale, domain_tags, files_changed, criticality, status, final_outcome, escalation_reason, required_voters, threshold, weighted_score, created_by, created_at, updated_at
		 FROM proposals WHERE id = ?`, id,
	)
	p, err := scanProposalFrom(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("getting proposal: %w", err)
	}
	return p, nil
}

// ListProposals returns proposals with optional filters. It returns the matching
// proposals and the total count (before limit). A non-zero projectID scopes the
// list to one project (v12); zero lists every project.
func ListProposals(db *sql.DB, projectID int, status string, criticality string, domainTag string, limit int) ([]*model.Proposal, int, error) {
	var whereClauses []string
	var args []any

	if projectID != 0 {
		whereClauses = append(whereClauses, "project_id = ?")
		args = append(args, projectID)
	}
	if status != "" {
		whereClauses = append(whereClauses, "status = ?")
		args = append(args, status)
	}
	if criticality != "" {
		whereClauses = append(whereClauses, "criticality = ?")
		args = append(args, criticality)
	}
	if domainTag != "" {
		whereClauses = append(whereClauses, "EXISTS (SELECT 1 FROM json_each(domain_tags) WHERE value = ?)")
		args = append(args, domainTag)
	}

	where := ""
	if len(whereClauses) > 0 {
		where = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Get total count.
	countQuery := "SELECT COUNT(*) FROM proposals " + where
	var total int
	if err := db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting proposals: %w", err)
	}

	// Get rows.
	query := "SELECT id, description, rationale, domain_tags, files_changed, criticality, status, final_outcome, escalation_reason, required_voters, threshold, weighted_score, created_by, created_at, updated_at FROM proposals " + where + " ORDER BY created_at ASC"
	queryArgs := append([]any{}, args...)
	if limit > 0 {
		query += " LIMIT ?"
		queryArgs = append(queryArgs, limit)
	}

	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing proposals: %w", err)
	}
	proposals, err := scanRows(rows, "proposal rows", func(r *sql.Rows) (*model.Proposal, error) {
		p, err := scanProposalFrom(r)
		if err != nil {
			return nil, fmt.Errorf("scanning proposal row: %w", err)
		}
		return p, nil
	})
	if err != nil {
		return nil, 0, err
	}

	return proposals, total, nil
}

// CastVote inserts a vote and auto-finalizes the proposal when quorum is reached.
// Returns ErrNotFound if the proposal does not exist.
// Returns ErrConflict if the voter already voted or the proposal is already finalized.
func CastVote(db *sql.DB, v *model.Vote) (*CastVoteResult, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Load the proposal within the transaction.
	var p model.Proposal
	var weightedScore sql.NullFloat64
	var createdBy sql.NullString
	var domainTagsRaw, filesChangedRaw string
	var escalationReason sql.NullString
	var createdAt, updatedAt string
	err = tx.QueryRow(
		`SELECT id, description, rationale, domain_tags, files_changed, criticality, status, final_outcome, escalation_reason, required_voters, threshold, weighted_score, created_by, created_at, updated_at
		 FROM proposals WHERE id = ?`, v.ProposalID,
	).Scan(
		&p.ID, &p.Description, &p.Rationale, &domainTagsRaw, &filesChangedRaw,
		&p.Criticality, &p.Status, &p.FinalOutcome, &escalationReason,
		&p.RequiredVoters, &p.Threshold, &weightedScore, &createdBy,
		&createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("loading proposal: %w", err)
	}
	if weightedScore.Valid {
		ws := weightedScore.Float64
		p.WeightedScore = &ws
	}
	p.CreatedBy = createdBy.String
	if escalationReason.Valid {
		er := escalationReason.String
		p.EscalationReason = &er
	}

	// Reject if already finalized.
	if p.Status != model.ProposalStatusOpen {
		return nil, ErrConflict
	}

	// Insert the vote.
	now := time.Now().UTC().Format(time.RFC3339)

	var findingsJSONStr any
	if v.FindingsJSON != nil {
		b, merr := json.Marshal(v.FindingsJSON)
		if merr != nil {
			return nil, fmt.Errorf("marshaling findings_json: %w", merr)
		}
		findingsJSONStr = string(b)
	}

	metadataStr, err := marshalVoteMetadata(v.Metadata)
	if err != nil {
		return nil, err
	}

	res, err := tx.Exec(
		`INSERT INTO votes (proposal_id, voter_name, voter_role, verdict, confidence, domain_relevance, findings, findings_json, summary, metadata, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ProposalID,
		v.VoterName,
		v.VoterRole,
		string(v.Verdict),
		v.Confidence,
		v.DomainRelevance,
		v.Findings,
		findingsJSONStr,
		v.Summary,
		metadataStr,
		now,
	)
	if err != nil {
		// UNIQUE constraint violation on (proposal_id, voter_name) means duplicate voter.
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return nil, ErrConflict
		}
		return nil, fmt.Errorf("inserting vote: %w", err)
	}

	voteID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("getting vote id: %w", err)
	}
	v.ID = int(voteID)

	// The seat's spend report (DKT-95), in the cast's own transaction: the
	// vote and its usage land together or not at all. Units are written in
	// sorted order so identical casts produce identical row ids — the same
	// determinism rule the step ledger's writer follows.
	if len(v.Usage) > 0 {
		for _, unit := range SortedUnits(v.Usage) {
			if err := ValidateUnitName(unit); err != nil {
				return nil, fmt.Errorf("vote usage: %w", err)
			}
			quantity := v.Usage[unit]
			if math.IsNaN(quantity) || math.IsInf(quantity, 0) || quantity < 0 {
				return nil, fmt.Errorf(
					"vote usage: %q must be a finite non-negative number, got %g",
					unit, quantity)
			}
			// Source is written EXPLICITLY (v17, DKT-115): a cast-time row is
			// the seat's own report, and relying on the column default here
			// would leave the writer silent about the one distinction the
			// column preserves against the back-fill's reconstruction.
			if err := InsertVoteUsageTx(
				tx, voteID, unit, quantity, UsageSourceReported, model.NowMS()); err != nil {
				return nil, err
			}
		}
	}

	createdAtTime, err := time.Parse(time.RFC3339, now)
	if err != nil {
		return nil, fmt.Errorf("parsing vote created_at: %w", err)
	}
	v.CreatedAt = createdAtTime

	// Count votes cast.
	var votesCast int
	if err := tx.QueryRow("SELECT COUNT(*) FROM votes WHERE proposal_id = ?", v.ProposalID).Scan(&votesCast); err != nil {
		return nil, fmt.Errorf("counting votes: %w", err)
	}

	result := &CastVoteResult{
		Vote:           v,
		ProposalStatus: p.Status,
		VotesCast:      votesCast,
		VotesRequired:  p.RequiredVoters,
		QuorumReached:  false,
	}

	// Check if quorum is reached.
	if votesCast >= p.RequiredVoters {
		result.QuorumReached = true

		// Compute weighted score.
		rows, err := tx.Query(
			"SELECT verdict, confidence, domain_relevance FROM votes WHERE proposal_id = ?",
			v.ProposalID,
		)
		if err != nil {
			return nil, fmt.Errorf("querying votes for score: %w", err)
		}

		var weightedSum, totalWeight float64
		for rows.Next() {
			var verdict string
			var confidence, domainRelevance float64
			if err := rows.Scan(&verdict, &confidence, &domainRelevance); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scanning vote for score: %w", err)
			}
			weight := confidence * domainRelevance
			totalWeight += weight
			if model.Verdict(verdict) == model.VerdictApprove || model.Verdict(verdict) == model.VerdictApproveWithConcerns {
				weightedSum += weight
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating votes for score: %w", err)
		}

		var score float64
		newStatus := model.ProposalStatusRejected
		if totalWeight == 0 {
			// Edge case: all zero weights — treat as rejected with score 0.
			score = 0.0
		} else {
			score = weightedSum / totalWeight
			if score >= p.Threshold {
				newStatus = model.ProposalStatusApproved
			}
		}

		result.WeightedScore = &score
		result.ProposalStatus = newStatus

		// Update proposal.
		updatedNow := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.Exec(
			"UPDATE proposals SET status = ?, weighted_score = ?, updated_at = ? WHERE id = ?",
			string(newStatus), score, updatedNow, v.ProposalID,
		); err != nil {
			return nil, fmt.Errorf("finalizing proposal: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing vote transaction: %w", err)
	}

	return result, nil
}

// GetProposalVotes returns all votes for a proposal, ordered by creation time.
func GetProposalVotes(db *sql.DB, proposalID int) ([]*model.Vote, error) {
	rows, err := db.Query(
		`SELECT id, proposal_id, voter_name, voter_role, verdict, confidence, domain_relevance, findings, findings_json, summary, metadata, created_at
		 FROM votes WHERE proposal_id = ? ORDER BY created_at ASC`, proposalID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying votes: %w", err)
	}
	votes, err := scanRows(rows, "vote rows", func(r *sql.Rows) (*model.Vote, error) {
		v, err := scanVoteFrom(r)
		if err != nil {
			return nil, fmt.Errorf("scanning vote row: %w", err)
		}
		return v, nil
	})
	if err != nil {
		return nil, err
	}

	return votes, nil
}

// LinkProposalIssue links a proposal to an issue.
// Returns ErrNotFound if the proposal or issue does not exist.
// Returns ErrConflict if the link already exists.
func LinkProposalIssue(db *sql.DB, proposalID, issueID int) error {
	// Check proposal exists.
	var proposalExists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM proposals WHERE id = ?)", proposalID).Scan(&proposalExists); err != nil {
		return fmt.Errorf("checking proposal existence: %w", err)
	}
	if !proposalExists {
		return ErrNotFound
	}

	// Check issue exists.
	var issueExists bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM issues WHERE id = ?)", issueID).Scan(&issueExists); err != nil {
		return fmt.Errorf("checking issue existence: %w", err)
	}
	if !issueExists {
		return ErrNotFound
	}

	_, err := db.Exec(
		"INSERT INTO proposal_issues (proposal_id, issue_id) VALUES (?, ?)",
		proposalID, issueID,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return ErrConflict
		}
		return fmt.Errorf("linking proposal to issue: %w", err)
	}

	return nil
}

// UnlinkProposalIssue removes a link between a proposal and an issue.
// Returns ErrNotFound if the link does not exist.
func UnlinkProposalIssue(db *sql.DB, proposalID, issueID int) error {
	res, err := db.Exec(
		"DELETE FROM proposal_issues WHERE proposal_id = ? AND issue_id = ?",
		proposalID, issueID,
	)
	if err != nil {
		return fmt.Errorf("unlinking proposal from issue: %w", err)
	}
	return requireAffected(res)
}

// GetProposalIssues returns the issue IDs linked to a proposal.
func GetProposalIssues(db *sql.DB, proposalID int) ([]int, error) {
	rows, err := db.Query(
		"SELECT issue_id FROM proposal_issues WHERE proposal_id = ? ORDER BY issue_id ASC",
		proposalID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying proposal issues: %w", err)
	}
	ids, err := scanRows(rows, "proposal issue rows", func(r *sql.Rows) (int, error) {
		var id int
		if err := r.Scan(&id); err != nil {
			return 0, fmt.Errorf("scanning issue id: %w", err)
		}
		return id, nil
	})
	if err != nil {
		return nil, err
	}

	return ids, nil
}

// GetIssueProposals returns the proposals linked to an issue, ordered by
// proposal id ascending. It is the reverse edge of GetProposalIssues.
func GetIssueProposals(db *sql.DB, issueID int) ([]model.Proposal, error) {
	rows, err := db.Query(
		`SELECT p.id, p.description, p.rationale, p.domain_tags, p.files_changed, p.criticality, p.status, p.final_outcome, p.escalation_reason, p.required_voters, p.threshold, p.weighted_score, p.created_by, p.created_at, p.updated_at
		 FROM proposals p
		 JOIN proposal_issues pi ON pi.proposal_id = p.id
		 WHERE pi.issue_id = ?
		 ORDER BY p.id ASC`,
		issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying issue proposals: %w", err)
	}
	proposals, err := scanRows(rows, "issue proposal rows", func(r *sql.Rows) (model.Proposal, error) {
		p, err := scanProposalFrom(r)
		if err != nil {
			return model.Proposal{}, fmt.Errorf("scanning proposal row: %w", err)
		}
		return *p, nil
	})
	if err != nil {
		return nil, err
	}

	return proposals, nil
}

// CommitProposal transitions an approved proposal to committed status with a final outcome.
// If escalationReason is non-empty, it is stored on the proposal.
func CommitProposal(db *sql.DB, id int, outcome string, escalationReason string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRow("SELECT status FROM proposals WHERE id = ?", id).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("loading proposal: %w", err)
	}

	if model.ProposalStatus(status) == model.ProposalStatusCommitted {
		return fmt.Errorf("%w: proposal %s is already committed", ErrConflict, model.FormatProposalID(id))
	}
	if model.ProposalStatus(status) != model.ProposalStatusApproved {
		return fmt.Errorf("%w: proposal %s must be approved before it can be committed; current status: %s", ErrConflict, model.FormatProposalID(id), status)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	var escalationReasonVal any
	if escalationReason != "" {
		escalationReasonVal = escalationReason
	}

	_, err = tx.Exec(
		"UPDATE proposals SET status = ?, final_outcome = ?, escalation_reason = COALESCE(?, escalation_reason), updated_at = ? WHERE id = ?",
		string(model.ProposalStatusCommitted), outcome, escalationReasonVal, now, id,
	)
	if err != nil {
		return fmt.Errorf("committing proposal: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	return nil
}

// CloseProposal retires an OPEN proposal without a tally (DKT-114).
//
// The case it exists for: a gate's underlying decision was made another way —
// an operator authorized the guarded action directly — and the proposal the
// panel would have decided has no votes and no future. Before this verb such a
// proposal sat `open` forever, misreporting a settled question as a pending
// one.
//
// Only `open` closes. Every other status is the record of a decision, and a
// close that rewrote one would be exactly the overwrite the immutable-record
// rule forbids. The reason lands in `final_outcome`, so the row itself says
// how the question ended.
func CloseProposal(db *sql.DB, id int, reason string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRow("SELECT status FROM proposals WHERE id = ?", id).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("loading proposal: %w", err)
	}

	if model.ProposalStatus(status) != model.ProposalStatusOpen {
		return fmt.Errorf(
			"%w: proposal %s is %s; only an open proposal can be closed",
			ErrConflict, model.FormatProposalID(id), status)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.Exec(
		"UPDATE proposals SET status = ?, final_outcome = ?, updated_at = ? WHERE id = ?",
		string(model.ProposalStatusClosed), reason, now, id,
	)
	if err != nil {
		return fmt.Errorf("closing proposal: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// --- helpers ---

// scanProposalFrom scans a single proposal from any scanner (*sql.Row or *sql.Rows).
func scanProposalFrom(s scanner) (*model.Proposal, error) {
	var p model.Proposal
	var weightedScore sql.NullFloat64
	var createdBy sql.NullString
	var domainTagsRaw, filesChangedRaw string
	var escalationReason sql.NullString
	var createdAt, updatedAt string

	err := s.Scan(
		&p.ID, &p.Description, &p.Rationale, &domainTagsRaw, &filesChangedRaw,
		&p.Criticality, &p.Status, &p.FinalOutcome, &escalationReason,
		&p.RequiredVoters, &p.Threshold, &weightedScore, &createdBy,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}

	if weightedScore.Valid {
		ws := weightedScore.Float64
		p.WeightedScore = &ws
	}
	p.CreatedBy = createdBy.String
	if escalationReason.Valid {
		er := escalationReason.String
		p.EscalationReason = &er
	}

	if domainTagsRaw != "" {
		if err := json.Unmarshal([]byte(domainTagsRaw), &p.DomainTags); err != nil {
			return nil, fmt.Errorf("unmarshaling domain_tags: %w", err)
		}
	}
	if filesChangedRaw != "" {
		if err := json.Unmarshal([]byte(filesChangedRaw), &p.FilesChanged); err != nil {
			return nil, fmt.Errorf("unmarshaling files_changed: %w", err)
		}
	}

	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parsing created_at: %w", err)
	}
	p.CreatedAt = t

	t, err = time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing updated_at: %w", err)
	}
	p.UpdatedAt = t

	return &p, nil
}

// VoteMetadataMaxBytes caps the encoded size of a vote's opaque KV bag, in
// the shape ArtifactMaxBytes uses: the limit lives beside the column it
// protects, so every writer of `votes.metadata` crosses it — `vote cast`,
// and `import` through InsertVoteWithID — rather than only the one command
// that happens to parse a flag. The value matches the 16 KiB a step's own
// metadata bag gets; it is duplicated rather than imported because
// internal/engine already imports this package.
const VoteMetadataMaxBytes = 16 << 10

// marshalVoteMetadata encodes a vote's opaque KV bag for the
// `votes.metadata` TEXT column.
//
// This package NEVER READS A KEY out of the bag — it only round-trips the
// whole object, exactly as db.usage.go's `source` column and
// internal/engine's step metadata do. A nil or empty map stores NULL rather
// than `{}` or `""`, so a vote nobody enriched reads back exactly as it did
// before this column existed (the never-mutate rule the v10-v12 migrations
// already follow for their own added columns).
func marshalVoteMetadata(m map[string]any) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshaling vote metadata: %w", err)
	}
	if len(b) > VoteMetadataMaxBytes {
		return nil, fmt.Errorf(
			"vote metadata is %d bytes, over the %d-byte cap; record the detail in the findings instead",
			len(b), VoteMetadataMaxBytes)
	}
	return string(b), nil
}

// scanVoteFrom scans a single vote from any scanner (*sql.Row or *sql.Rows).
func scanVoteFrom(s scanner) (*model.Vote, error) {
	var v model.Vote
	var findingsJSONRaw sql.NullString
	var metadataRaw sql.NullString
	var createdAt string

	err := s.Scan(
		&v.ID, &v.ProposalID, &v.VoterName, &v.VoterRole,
		&v.Verdict, &v.Confidence, &v.DomainRelevance, &v.Findings,
		&findingsJSONRaw, &v.Summary, &metadataRaw, &createdAt,
	)
	if err != nil {
		return nil, err
	}

	if findingsJSONRaw.Valid {
		var f model.Findings
		if err := json.Unmarshal([]byte(findingsJSONRaw.String), &f); err != nil {
			return nil, fmt.Errorf("unmarshaling findings_json: %w", err)
		}
		v.FindingsJSON = &f
	}

	// A metadata cell that does not decode NEVER FAILS THE READ: the column is
	// opaque, rows may predate any writer that validated them, and a read verb
	// that refused because one vote held odd bytes would fail `vote show`,
	// `vote list` and `vote result` for the whole proposal. This is
	// db.MetadataRollup's tolerance (rollups.go), and marshalVoteMetadata plus
	// its cap is the write end that keeps the tolerance from becoming the norm.
	//
	// Tolerated is not the same as absent, though, so the undecodable cell sets
	// MetadataUnreadable: a seat whose claim was corrupted, truncated, or
	// rewritten in place reads differently from a seat that claimed nothing.
	//
	// Numbers inside the bag come back as float64, the whole-object round trip
	// through encoding/json this repo already gives a step's metadata: a value
	// above 2^53 is not exact on the way back. Quantities that must be exact
	// belong in a column, not in an opaque claim.
	if metadataRaw.Valid {
		var bag map[string]any
		if err := json.Unmarshal([]byte(metadataRaw.String), &bag); err == nil {
			v.Metadata = bag
		} else {
			v.MetadataUnreadable = true
		}
	}

	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parsing created_at: %w", err)
	}
	v.CreatedAt = t

	return &v, nil
}

// ListAllProposals returns every proposal row ordered by id ASC, for a full
// export.
func ListAllProposals(db *sql.DB, projectID int) ([]*model.Proposal, error) {
	where, args := projectFilter(projectID, "WHERE")
	rows, err := db.Query(
		`SELECT id, description, rationale, domain_tags, files_changed, criticality,
		        status, final_outcome, escalation_reason, required_voters, threshold,
		        weighted_score, created_by, created_at, updated_at
		 FROM proposals `+where+` ORDER BY id ASC`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all proposals: %w", err)
	}
	proposals, err := scanRows(rows, "proposal rows", func(r *sql.Rows) (*model.Proposal, error) {
		p, err := scanProposalFrom(r)
		if err != nil {
			return nil, fmt.Errorf("scanning proposal row: %w", err)
		}
		return p, nil
	})
	if err != nil {
		return nil, err
	}
	return proposals, nil
}

// ListAllVotes returns every vote row ordered by id ASC, for a full export.
func ListAllVotes(db *sql.DB, projectID int) ([]*model.Vote, error) {
	where, args := projectFilterVia(projectID, `proposal_id IN (SELECT id FROM proposals WHERE project_id = ?)`)
	rows, err := db.Query(
		`SELECT id, proposal_id, voter_name, voter_role, verdict, confidence,
		        domain_relevance, findings, findings_json, summary, metadata, created_at
		 FROM votes `+where+` ORDER BY id ASC`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all votes: %w", err)
	}
	votes, err := scanRows(rows, "vote rows", func(r *sql.Rows) (*model.Vote, error) {
		v, err := scanVoteFrom(r)
		if err != nil {
			return nil, fmt.Errorf("scanning vote row: %w", err)
		}
		return v, nil
	})
	if err != nil {
		return nil, err
	}
	return votes, nil
}

// ListAllProposalIssues returns every proposal_issues row ordered by
// (proposal_id, issue_id), for a full export.
func ListAllProposalIssues(db *sql.DB, projectID int) ([]model.ProposalIssueLink, error) {
	where, args := projectFilterVia(projectID, `proposal_id IN (SELECT id FROM proposals WHERE project_id = ?)`)
	rows, err := db.Query(
		`SELECT proposal_id, issue_id
		 FROM proposal_issues `+where+` ORDER BY proposal_id ASC, issue_id ASC`, args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying all proposal_issues: %w", err)
	}
	out, err := scanRows(rows, "proposal_issue rows", func(r *sql.Rows) (model.ProposalIssueLink, error) {
		var l model.ProposalIssueLink
		if err := r.Scan(&l.ProposalID, &l.IssueID); err != nil {
			return model.ProposalIssueLink{}, fmt.Errorf("scanning proposal_issue row: %w", err)
		}
		return l, nil
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = make([]model.ProposalIssueLink, 0)
	}
	return out, nil
}

// InsertProposalWithID inserts a proposal row with a caller-supplied ID,
// skipping if the ID already exists. Must be called within an existing
// transaction. Returns true if inserted. Mirrors InsertIssueWithID; domain_tags
// and files_changed are JSON-encoded identically to CreateProposal.
func InsertProposalWithID(tx *sql.Tx, p *model.Proposal) (bool, error) {
	domainTagsJSON, err := json.Marshal(p.DomainTags)
	if err != nil {
		return false, fmt.Errorf("marshaling domain_tags: %w", err)
	}
	filesChangedJSON, err := json.Marshal(p.FilesChanged)
	if err != nil {
		return false, fmt.Errorf("marshaling files_changed: %w", err)
	}

	var weightedScore any
	if p.WeightedScore != nil {
		weightedScore = *p.WeightedScore
	}
	var escalationReason any
	if p.EscalationReason != nil {
		escalationReason = *p.EscalationReason
	}

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO proposals
		 (id, project_id, description, rationale, domain_tags, files_changed, criticality, status,
		  final_outcome, escalation_reason, required_voters, threshold, weighted_score,
		  created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, projectOrDefault(p.ProjectID), p.Description, p.Rationale, string(domainTagsJSON), string(filesChangedJSON),
		string(p.Criticality), string(p.Status), p.FinalOutcome, escalationReason,
		p.RequiredVoters, p.Threshold, weightedScore, p.CreatedBy,
		p.CreatedAt.UTC().Format(time.RFC3339), p.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("inserting proposal with id %d: %w", p.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertVoteWithID inserts a vote row with a caller-supplied ID, skipping if the
// ID already exists. Must be called within an existing transaction. Returns true
// if inserted. findings_json and metadata are JSON-encoded (NULL when absent)
// identically to CastVote, so an export/import round trip carries a vote's
// provenance claim exactly as it carries its findings.
func InsertVoteWithID(tx *sql.Tx, v *model.Vote) (bool, error) {
	var findingsJSONStr any
	if v.FindingsJSON != nil {
		b, err := json.Marshal(v.FindingsJSON)
		if err != nil {
			return false, fmt.Errorf("marshaling findings_json: %w", err)
		}
		findingsJSONStr = string(b)
	}

	metadataStr, err := marshalVoteMetadata(v.Metadata)
	if err != nil {
		return false, err
	}

	res, err := tx.Exec(
		`INSERT OR IGNORE INTO votes
		 (id, proposal_id, voter_name, voter_role, verdict, confidence,
		  domain_relevance, findings, findings_json, summary, metadata, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ID, v.ProposalID, v.VoterName, v.VoterRole, string(v.Verdict), v.Confidence,
		v.DomainRelevance, v.Findings, findingsJSONStr, v.Summary, metadataStr,
		v.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("inserting vote with id %d: %w", v.ID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertProposalIssueLink inserts a proposal_issues row, skipping on PK
// conflict. Must be called within a transaction. Returns true if inserted.
func InsertProposalIssueLink(tx *sql.Tx, proposalID, issueID int) (bool, error) {
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO proposal_issues (proposal_id, issue_id) VALUES (?, ?)`,
		proposalID, issueID,
	)
	if err != nil {
		return false, fmt.Errorf("inserting proposal_issue (%d,%d): %w", proposalID, issueID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ProposalStatusesTx reads many proposals' statuses in ONE query, inside a
// caller's transaction.
//
// It exists for the reader that holds the single pooled connection and needs a
// handful of these at once: internal/db caps the pool at one connection, so a
// pool read from inside an open transaction deadlocks permanently rather than
// failing, and a per-proposal GetProposal loop from such a reader is the shape
// that produces it.
//
// Status ONLY. A report that wanted the whole row would be asking for a
// different function; narrowing it here keeps this from becoming a second
// GetProposal that drifts from the first.
func ProposalStatusesTx(tx *sql.Tx, ids []int) (map[int]model.ProposalStatus, error) {
	out := make(map[int]model.ProposalStatus, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	// The placeholder list is built from the COUNT of ids, never from their
	// values, and every id is a bound parameter — so the interpolation carries
	// no injection surface even though the ids reach it from a caller.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}

	rows, err := tx.Query(
		`SELECT id, status FROM proposals WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("reading proposal statuses: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id     int
			status string
		)
		if err := rows.Scan(&id, &status); err != nil {
			return nil, fmt.Errorf("reading a proposal status: %w", err)
		}
		out[id] = model.ProposalStatus(status)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading proposal statuses: %w", err)
	}
	return out, nil
}

// CloseOpenProposalsTx closes every OPEN proposal among `ids`, inside a
// caller's transaction, and reports how many it closed (DKT-262).
//
// It exists because closing a stranded proposal is not an operator act — it is
// the tail of a transition that already happened. `run abandon` ends the run
// that opened them; the ack of a reap answers the question its proposal asked.
// Those transitions are transactional, so the close has to be able to ride
// inside them: a close committed separately can be lost while the transition
// stands, which puts the row back in the state this exists to prevent.
//
// ONLY `open` ROWS MOVE, exactly as CloseProposal insists. Every other status
// is the record of a decision, and a bulk close that rewrote one would be the
// overwrite the immutable-record rule forbids — which matters more here than in
// the single-id case, because a caller passing a set has not looked at each one.
//
// `reason` lands in `final_outcome`, so the row itself says how the question
// ended. It should name the TRANSITION, not the verdict: these proposals were
// never decided, and a reason that read like a decision would be a worse lie
// than the stale `open` was.
func CloseOpenProposalsTx(tx *sql.Tx, ids []int, reason string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	// Built from the COUNT of ids, never their values; every id is bound.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := []any{
		string(model.ProposalStatusClosed), reason,
		time.Now().UTC().Format(time.RFC3339),
	}
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, string(model.ProposalStatusOpen))

	res, err := tx.Exec(
		`UPDATE proposals SET status = ?, final_outcome = ?, updated_at = ?
		  WHERE id IN (`+placeholders+`) AND status = ?`, args...)
	if err != nil {
		return 0, fmt.Errorf("closing stranded proposals: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("closing stranded proposals: %w", err)
	}
	return int(n), nil
}
