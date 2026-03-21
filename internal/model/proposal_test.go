package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestValidateCriticality(t *testing.T) {
	valid := []Criticality{CriticalityLow, CriticalityMedium, CriticalityHigh, CriticalityCritical}
	for _, c := range valid {
		if err := ValidateCriticality(c); err != nil {
			t.Errorf("ValidateCriticality(%q) unexpected error: %v", c, err)
		}
	}
	if err := ValidateCriticality("invalid"); err == nil {
		t.Error("ValidateCriticality('invalid') expected error, got nil")
	}
}

func TestValidateProposalStatus(t *testing.T) {
	valid := []ProposalStatus{ProposalStatusOpen, ProposalStatusApproved, ProposalStatusRejected}
	for _, s := range valid {
		if err := ValidateProposalStatus(s); err != nil {
			t.Errorf("ValidateProposalStatus(%q) unexpected error: %v", s, err)
		}
	}
	if err := ValidateProposalStatus("invalid"); err == nil {
		t.Error("ValidateProposalStatus('invalid') expected error, got nil")
	}
}

func TestValidateVerdict(t *testing.T) {
	valid := []Verdict{VerdictApprove, VerdictReject}
	for _, v := range valid {
		if err := ValidateVerdict(v); err != nil {
			t.Errorf("ValidateVerdict(%q) unexpected error: %v", v, err)
		}
	}
	if err := ValidateVerdict("invalid"); err == nil {
		t.Error("ValidateVerdict('invalid') expected error, got nil")
	}
}

func TestFormatProposalID(t *testing.T) {
	tests := []struct {
		id   int
		want string
	}{
		{1, "DKT-V1"},
		{42, "DKT-V42"},
		{999, "DKT-V999"},
	}
	for _, tt := range tests {
		if got := FormatProposalID(tt.id); got != tt.want {
			t.Errorf("FormatProposalID(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestParseProposalID(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"DKT-V5", 5, false},
		{"dkt-v5", 5, false},
		{"5", 5, false},
		{"42", 42, false},
		{"  DKT-V10  ", 10, false},
		{"", 0, true},
		{"DKT-V", 0, true},
		{"abc", 0, true},
		{"DKT-V0", 0, true},
		{"DKT-V-1", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseProposalID(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseProposalID(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseProposalID(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestFormatParseProposalIDRoundTrip(t *testing.T) {
	for _, id := range []int{1, 5, 42, 999} {
		formatted := FormatProposalID(id)
		parsed, err := ParseProposalID(formatted)
		if err != nil {
			t.Errorf("ParseProposalID(FormatProposalID(%d)) error: %v", id, err)
			continue
		}
		if parsed != id {
			t.Errorf("ParseProposalID(FormatProposalID(%d)) = %d", id, parsed)
		}
	}
}

func TestProposalJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	score := 0.89
	p := Proposal{
		ID:             1,
		Description:    "Test proposal",
		Criticality:    CriticalityHigh,
		Status:         ProposalStatusApproved,
		RequiredVoters: 3,
		Threshold:      0.67,
		WeightedScore:  &score,
		CreatedBy:      "team-lead",
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	// Verify JSON wire format.
	var raw map[string]any
	json.Unmarshal(data, &raw)
	if raw["id"] != "DKT-V1" {
		t.Errorf("JSON id = %v, want %q", raw["id"], "DKT-V1")
	}
	if raw["criticality"] != "high" {
		t.Errorf("JSON criticality = %v, want %q", raw["criticality"], "high")
	}
	if raw["status"] != "approved" {
		t.Errorf("JSON status = %v, want %q", raw["status"], "approved")
	}
	if raw["required_voters"] != float64(3) {
		t.Errorf("JSON required_voters = %v, want 3", raw["required_voters"])
	}
	if raw["weighted_score"] != 0.89 {
		t.Errorf("JSON weighted_score = %v, want 0.89", raw["weighted_score"])
	}
	if raw["created_by"] != "team-lead" {
		t.Errorf("JSON created_by = %v, want %q", raw["created_by"], "team-lead")
	}

	// Unmarshal back.
	var p2 Proposal
	if err := json.Unmarshal(data, &p2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if p2.ID != 1 {
		t.Errorf("Unmarshaled ID = %d, want 1", p2.ID)
	}
	if p2.Description != "Test proposal" {
		t.Errorf("Unmarshaled Description = %q", p2.Description)
	}
	if p2.Criticality != CriticalityHigh {
		t.Errorf("Unmarshaled Criticality = %q", p2.Criticality)
	}
	if p2.Status != ProposalStatusApproved {
		t.Errorf("Unmarshaled Status = %q", p2.Status)
	}
	if p2.WeightedScore == nil || *p2.WeightedScore != 0.89 {
		t.Errorf("Unmarshaled WeightedScore = %v", p2.WeightedScore)
	}
	if !p2.CreatedAt.Equal(now) {
		t.Errorf("Unmarshaled CreatedAt = %v, want %v", p2.CreatedAt, now)
	}
}

func TestProposalJSONNilWeightedScore(t *testing.T) {
	now := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	p := Proposal{
		ID:             1,
		Description:    "Open proposal",
		Criticality:    CriticalityMedium,
		Status:         ProposalStatusOpen,
		RequiredVoters: 2,
		Threshold:      0.67,
		WeightedScore:  nil,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	json.Unmarshal(data, &raw)
	if raw["weighted_score"] != nil {
		t.Errorf("JSON weighted_score = %v, want null", raw["weighted_score"])
	}

	var p2 Proposal
	if err := json.Unmarshal(data, &p2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if p2.WeightedScore != nil {
		t.Errorf("Unmarshaled WeightedScore = %v, want nil", p2.WeightedScore)
	}
}

func TestVoteJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 3, 20, 10, 5, 0, 0, time.UTC)
	v := Vote{
		ID:              1,
		ProposalID:      3,
		VoterName:       "security-reviewer",
		VoterRole:       "security",
		Verdict:         VerdictApprove,
		Confidence:      0.9,
		DomainRelevance: 0.85,
		Findings:        "No security concerns",
		CreatedAt:       now,
	}

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	json.Unmarshal(data, &raw)
	if raw["proposal_id"] != "DKT-V3" {
		t.Errorf("JSON proposal_id = %v, want %q", raw["proposal_id"], "DKT-V3")
	}
	if raw["voter_name"] != "security-reviewer" {
		t.Errorf("JSON voter_name = %v", raw["voter_name"])
	}
	if raw["verdict"] != "approve" {
		t.Errorf("JSON verdict = %v, want %q", raw["verdict"], "approve")
	}

	var v2 Vote
	if err := json.Unmarshal(data, &v2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if v2.ID != 1 {
		t.Errorf("Unmarshaled ID = %d, want 1", v2.ID)
	}
	if v2.ProposalID != 3 {
		t.Errorf("Unmarshaled ProposalID = %d, want 3", v2.ProposalID)
	}
	if v2.VoterName != "security-reviewer" {
		t.Errorf("Unmarshaled VoterName = %q", v2.VoterName)
	}
	if v2.Verdict != VerdictApprove {
		t.Errorf("Unmarshaled Verdict = %q", v2.Verdict)
	}
	if v2.Confidence != 0.9 {
		t.Errorf("Unmarshaled Confidence = %f", v2.Confidence)
	}
	if v2.DomainRelevance != 0.85 {
		t.Errorf("Unmarshaled DomainRelevance = %f", v2.DomainRelevance)
	}
	if !v2.CreatedAt.Equal(now) {
		t.Errorf("Unmarshaled CreatedAt = %v, want %v", v2.CreatedAt, now)
	}
}
