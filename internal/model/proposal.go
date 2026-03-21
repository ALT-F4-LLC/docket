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
	ProposalStatusOpen      ProposalStatus = "open"
	ProposalStatusApproved  ProposalStatus = "approved"
	ProposalStatusRejected  ProposalStatus = "rejected"
	ProposalStatusCommitted ProposalStatus = "committed"
)

var validProposalStatuses = []ProposalStatus{
	ProposalStatusOpen,
	ProposalStatusApproved,
	ProposalStatusRejected,
	ProposalStatusCommitted,
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
	VerdictApprove             Verdict = "approve"
	VerdictApproveWithConcerns Verdict = "approve-with-concerns"
	VerdictReject              Verdict = "reject"
)

var validVerdicts = []Verdict{
	VerdictApprove,
	VerdictApproveWithConcerns,
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

// Findings represents structured review findings.
type Findings struct {
	Blockers    []string `json:"blockers"`
	Concerns    []string `json:"concerns"`
	Suggestions []string `json:"suggestions"`
}

// Proposal represents a consensus proposal for PBFT-inspired voting.
type Proposal struct {
	ID               int
	Description      string
	Criticality      Criticality
	Status           ProposalStatus
	RequiredVoters   int
	Threshold        float64
	WeightedScore    *float64
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Rationale        string
	DomainTags       []string
	FilesChanged     []string
	FinalOutcome     string
	EscalationReason *string
}

// proposalJSON is the JSON wire format for Proposal.
type proposalJSON struct {
	ID               string    `json:"id"`
	Description      string    `json:"description"`
	Rationale        string    `json:"rationale"`
	DomainTags       []string  `json:"domain_tags"`
	FilesChanged     []string  `json:"files_changed"`
	Criticality      string    `json:"criticality"`
	Status           string    `json:"status"`
	FinalOutcome     string    `json:"final_outcome"`
	EscalationReason *string   `json:"escalation_reason"`
	RequiredVoters   int       `json:"required_voters"`
	Threshold        float64   `json:"threshold"`
	WeightedScore    *float64  `json:"weighted_score"`
	CreatedBy        string    `json:"created_by"`
	CreatedAt        string    `json:"created_at"`
	UpdatedAt        string    `json:"updated_at"`
}

// MarshalJSON implements custom JSON serialization for Proposal.
func (p Proposal) MarshalJSON() ([]byte, error) {
	domainTags := p.DomainTags
	if domainTags == nil {
		domainTags = []string{}
	}
	filesChanged := p.FilesChanged
	if filesChanged == nil {
		filesChanged = []string{}
	}

	j := proposalJSON{
		ID:               FormatProposalID(p.ID),
		Description:      p.Description,
		Rationale:        p.Rationale,
		DomainTags:       domainTags,
		FilesChanged:     filesChanged,
		Criticality:      string(p.Criticality),
		Status:           string(p.Status),
		FinalOutcome:     p.FinalOutcome,
		EscalationReason: p.EscalationReason,
		RequiredVoters:   p.RequiredVoters,
		Threshold:        p.Threshold,
		WeightedScore:    p.WeightedScore,
		CreatedBy:        p.CreatedBy,
		CreatedAt:        p.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:        p.UpdatedAt.UTC().Format(time.RFC3339),
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

	p.Rationale = j.Rationale
	if j.DomainTags != nil {
		p.DomainTags = j.DomainTags
	}
	if j.FilesChanged != nil {
		p.FilesChanged = j.FilesChanged
	}
	p.FinalOutcome = j.FinalOutcome
	p.EscalationReason = j.EscalationReason

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
	FindingsJSON    *Findings
	Summary         string
	CreatedAt       time.Time
}

// voteJSON is the JSON wire format for Vote.
type voteJSON struct {
	ID              int        `json:"id"`
	ProposalID      string     `json:"proposal_id,omitempty"`
	VoterName       string     `json:"voter_name"`
	VoterRole       string     `json:"voter_role"`
	Verdict         string     `json:"verdict"`
	Confidence      float64    `json:"confidence"`
	DomainRelevance float64    `json:"domain_relevance"`
	EffectiveWeight float64    `json:"effective_weight"`
	Findings        string     `json:"findings"`
	FindingsJSON    *Findings  `json:"findings_json"`
	Summary         string     `json:"summary"`
	CreatedAt       string     `json:"created_at"`
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
		EffectiveWeight: v.Confidence * v.DomainRelevance,
		Findings:        v.Findings,
		FindingsJSON:    v.FindingsJSON,
		Summary:         v.Summary,
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
	v.FindingsJSON = j.FindingsJSON
	v.Summary = j.Summary

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	v.CreatedAt = createdAt

	return nil
}
