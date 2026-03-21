package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	now := time.Now().UTC().Format(time.RFC3339)

	domainTagsJSON, err := json.Marshal(p.DomainTags)
	if err != nil {
		return 0, fmt.Errorf("marshaling domain_tags: %w", err)
	}

	filesChangedJSON, err := json.Marshal(p.FilesChanged)
	if err != nil {
		return 0, fmt.Errorf("marshaling files_changed: %w", err)
	}

	res, err := db.Exec(
		`INSERT INTO proposals (description, rationale, domain_tags, files_changed, criticality, status, final_outcome, escalation_reason, required_voters, threshold, weighted_score, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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

	return int(id64), nil
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
// proposals and the total count (before limit).
func ListProposals(db *sql.DB, status string, criticality string, domainTag string, limit int) ([]*model.Proposal, int, error) {
	var whereClauses []string
	var args []any

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
	defer rows.Close()

	var proposals []*model.Proposal
	for rows.Next() {
		p, err := scanProposalFrom(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scanning proposal row: %w", err)
		}
		proposals = append(proposals, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating proposal rows: %w", err)
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

	res, err := tx.Exec(
		`INSERT INTO votes (proposal_id, voter_name, voter_role, verdict, confidence, domain_relevance, findings, findings_json, summary, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.ProposalID,
		v.VoterName,
		v.VoterRole,
		string(v.Verdict),
		v.Confidence,
		v.DomainRelevance,
		v.Findings,
		findingsJSONStr,
		v.Summary,
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
		`SELECT id, proposal_id, voter_name, voter_role, verdict, confidence, domain_relevance, findings, findings_json, summary, created_at
		 FROM votes WHERE proposal_id = ? ORDER BY created_at ASC`, proposalID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying votes: %w", err)
	}
	defer rows.Close()

	var votes []*model.Vote
	for rows.Next() {
		v, err := scanVoteFrom(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning vote row: %w", err)
		}
		votes = append(votes, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating vote rows: %w", err)
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

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}

	return nil
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
	defer rows.Close()

	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning issue id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating proposal issue rows: %w", err)
	}

	return ids, nil
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

// scanVoteFrom scans a single vote from any scanner (*sql.Row or *sql.Rows).
func scanVoteFrom(s scanner) (*model.Vote, error) {
	var v model.Vote
	var findingsJSONRaw sql.NullString
	var createdAt string

	err := s.Scan(
		&v.ID, &v.ProposalID, &v.VoterName, &v.VoterRole,
		&v.Verdict, &v.Confidence, &v.DomainRelevance, &v.Findings,
		&findingsJSONRaw, &v.Summary, &createdAt,
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

	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parsing created_at: %w", err)
	}
	v.CreatedAt = t

	return &v, nil
}
