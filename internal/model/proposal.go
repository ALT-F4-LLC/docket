package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ProposalIDPrefix distinguishes proposal IDs from issue IDs.
const ProposalIDPrefix = "DKT-V"

// Criticality represents the severity level of a proposal.
type Criticality string

const (
	CriticalityLow      Criticality = "low"
	CriticalityMedium   Criticality = "medium"
	CriticalityHigh     Criticality = "high"
	CriticalityCritical Criticality = "critical"
)

var validCriticalities = []Criticality{
	CriticalityLow,
	CriticalityMedium,
	CriticalityHigh,
	CriticalityCritical,
}

// ValidateCriticality returns an error if c is not a recognized criticality.
func ValidateCriticality(c Criticality) error {
	if slices.Contains(validCriticalities, c) {
		return nil
	}
	return fmt.Errorf("invalid criticality %q: must be one of %v", c, validCriticalities)
}

// ProposalStatus represents the workflow state of a proposal.
type ProposalStatus string

const (
	ProposalStatusOpen     ProposalStatus = "open"
	ProposalStatusApproved ProposalStatus = "approved"
	ProposalStatusRejected ProposalStatus = "rejected"
)

var validProposalStatuses = []ProposalStatus{
	ProposalStatusOpen,
	ProposalStatusApproved,
	ProposalStatusRejected,
}

// ValidateProposalStatus returns an error if s is not a recognized proposal status.
func ValidateProposalStatus(s ProposalStatus) error {
	if slices.Contains(validProposalStatuses, s) {
		return nil
	}
	return fmt.Errorf("invalid proposal status %q: must be one of %v", s, validProposalStatuses)
}

// Verdict represents a voter's decision on a proposal.
type Verdict string

const (
	VerdictApprove Verdict = "approve"
	VerdictReject  Verdict = "reject"
)

var validVerdicts = []Verdict{
	VerdictApprove,
	VerdictReject,
}

// ValidateVerdict returns an error if v is not a recognized verdict.
func ValidateVerdict(v Verdict) error {
	if slices.Contains(validVerdicts, v) {
		return nil
	}
	return fmt.Errorf("invalid verdict %q: must be one of %v", v, validVerdicts)
}

// FormatProposalID returns the display form of a proposal ID, e.g. "DKT-V1".
func FormatProposalID(id int) string {
	return fmt.Sprintf("%s%d", ProposalIDPrefix, id)
}

// ParseProposalID accepts both "DKT-V5" and "5" and returns the numeric ID.
// The prefix check is case-insensitive.
func ParseProposalID(input string) (int, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, fmt.Errorf("empty proposal ID")
	}

	if strings.HasPrefix(strings.ToUpper(s), strings.ToUpper(ProposalIDPrefix)) {
		s = s[len(ProposalIDPrefix):]
	}

	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid proposal ID %q: %w", input, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid proposal ID %q: must be positive", input)
	}

	return id, nil
}

// Proposal represents a consensus proposal for PBFT-inspired voting.
type Proposal struct {
	ID             int
	Description    string
	Criticality    Criticality
	Status         ProposalStatus
	RequiredVoters int
	Threshold      float64
	WeightedScore  *float64
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// proposalJSON is the JSON wire format for Proposal.
type proposalJSON struct {
	ID             string   `json:"id"`
	Description    string   `json:"description"`
	Criticality    string   `json:"criticality"`
	Status         string   `json:"status"`
	RequiredVoters int      `json:"required_voters"`
	Threshold      float64  `json:"threshold"`
	WeightedScore  *float64 `json:"weighted_score"`
	CreatedBy      string   `json:"created_by"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
}

// MarshalJSON implements custom JSON serialization for Proposal.
func (p Proposal) MarshalJSON() ([]byte, error) {
	j := proposalJSON{
		ID:             FormatProposalID(p.ID),
		Description:    p.Description,
		Criticality:    string(p.Criticality),
		Status:         string(p.Status),
		RequiredVoters: p.RequiredVoters,
		Threshold:      p.Threshold,
		WeightedScore:  p.WeightedScore,
		CreatedBy:      p.CreatedBy,
		CreatedAt:      p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:      p.UpdatedAt.UTC().Format(time.RFC3339),
	}

	return json.Marshal(j)
}

// UnmarshalJSON implements custom JSON deserialization for Proposal.
func (p *Proposal) UnmarshalJSON(data []byte) error {
	var j proposalJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	id, err := ParseProposalID(j.ID)
	if err != nil {
		return fmt.Errorf("parsing proposal id: %w", err)
	}
	p.ID = id

	p.Description = j.Description

	p.Criticality = Criticality(j.Criticality)
	if err := ValidateCriticality(p.Criticality); err != nil {
		return err
	}

	p.Status = ProposalStatus(j.Status)
	if err := ValidateProposalStatus(p.Status); err != nil {
		return err
	}

	p.RequiredVoters = j.RequiredVoters
	p.Threshold = j.Threshold
	p.WeightedScore = j.WeightedScore
	p.CreatedBy = j.CreatedBy

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	p.CreatedAt = createdAt

	updatedAt, err := time.Parse(time.RFC3339, j.UpdatedAt)
	if err != nil {
		return fmt.Errorf("parsing updated_at: %w", err)
	}
	p.UpdatedAt = updatedAt

	return nil
}

// Vote represents an individual vote on a proposal.
type Vote struct {
	ID              int
	ProposalID      int
	VoterName       string
	VoterRole       string
	Verdict         Verdict
	Confidence      float64
	DomainRelevance float64
	Findings        string
	CreatedAt       time.Time
}

// voteJSON is the JSON wire format for Vote.
type voteJSON struct {
	ID              int     `json:"id"`
	ProposalID      string  `json:"proposal_id,omitempty"`
	VoterName       string  `json:"voter_name"`
	VoterRole       string  `json:"voter_role"`
	Verdict         string  `json:"verdict"`
	Confidence      float64 `json:"confidence"`
	DomainRelevance float64 `json:"domain_relevance"`
	Findings        string  `json:"findings"`
	CreatedAt       string  `json:"created_at"`
}

// MarshalJSON implements custom JSON serialization for Vote.
func (v Vote) MarshalJSON() ([]byte, error) {
	j := voteJSON{
		ID:              v.ID,
		ProposalID:      FormatProposalID(v.ProposalID),
		VoterName:       v.VoterName,
		VoterRole:       v.VoterRole,
		Verdict:         string(v.Verdict),
		Confidence:      v.Confidence,
		DomainRelevance: v.DomainRelevance,
		Findings:        v.Findings,
		CreatedAt:       v.CreatedAt.UTC().Format(time.RFC3339),
	}

	return json.Marshal(j)
}

// UnmarshalJSON implements custom JSON deserialization for Vote.
func (v *Vote) UnmarshalJSON(data []byte) error {
	var j voteJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	v.ID = j.ID

	if j.ProposalID != "" {
		proposalID, err := ParseProposalID(j.ProposalID)
		if err != nil {
			return fmt.Errorf("parsing proposal id: %w", err)
		}
		v.ProposalID = proposalID
	}

	v.VoterName = j.VoterName
	v.VoterRole = j.VoterRole

	v.Verdict = Verdict(j.Verdict)
	if err := ValidateVerdict(v.Verdict); err != nil {
		return err
	}

	v.Confidence = j.Confidence
	v.DomainRelevance = j.DomainRelevance
	v.Findings = j.Findings

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	v.CreatedAt = createdAt

	return nil
}
