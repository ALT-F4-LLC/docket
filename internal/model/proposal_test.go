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
	valid := []ProposalStatus{ProposalStatusOpen, ProposalStatusApproved, ProposalStatusRejected, ProposalStatusCommitted}
	for _, s := range valid {
		if err := ValidateProposalStatus(s); err != nil {
			t.Errorf("ValidateProposalStatus(%q) unexpected error: %v", s, err)
		}
	}
	invalid := []ProposalStatus{"invalid", "done", "closed"}
	for _, s := range invalid {
		if err := ValidateProposalStatus(s); err == nil {
			t.Errorf("ValidateProposalStatus(%q) expected error, got nil", s)
		}
	}
}

func TestValidateVerdict(t *testing.T) {
	valid := []Verdict{VerdictApprove, VerdictApproveWithConcerns, VerdictReject}
	for _, v := range valid {
		if err := ValidateVerdict(v); err != nil {
			t.Errorf("ValidateVerdict(%q) unexpected error: %v", v, err)
		}
	}
	invalid := []Verdict{"invalid", "partial-approve", "abstain"}
	for _, v := range invalid {
		if err := ValidateVerdict(v); err == nil {
			t.Errorf("ValidateVerdict(%q) expected error, got nil", v)
		}
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

// --- Gap 3: Findings struct JSON marshaling/unmarshaling ---

func TestFindingsJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		findings Findings
	}{
		{
			name: "populated",
			findings: Findings{
				Blockers:    []string{"critical security issue"},
				Concerns:    []string{"performance concern", "naming convention"},
				Suggestions: []string{"add logging"},
			},
		},
		{
			name: "empty arrays",
			findings: Findings{
				Blockers:    []string{},
				Concerns:    []string{},
				Suggestions: []string{},
			},
		},
		{
			name: "nil arrays",
			findings: Findings{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.findings)
			if err != nil {
				t.Fatalf("Marshal error: %v", err)
			}

			var got Findings
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal error: %v", err)
			}

			// Re-marshal both to compare (handles nil vs empty slice).
			expected, _ := json.Marshal(tt.findings)
			actual, _ := json.Marshal(got)
			if string(expected) != string(actual) {
				t.Errorf("round-trip mismatch:\n  expected: %s\n  actual:   %s", expected, actual)
			}
		})
	}
}

func TestFindingsJSONNilPointer(t *testing.T) {
	// Verify *Findings nil serializes as null in a containing struct.
	type wrapper struct {
		F *Findings `json:"findings_json"`
	}

	data, err := json.Marshal(wrapper{F: nil})
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	json.Unmarshal(data, &raw)
	if raw["findings_json"] != nil {
		t.Errorf("expected null for nil *Findings, got %v", raw["findings_json"])
	}
}

// --- Gap 4: Proposal JSON roundtrip with new v3 fields ---

func TestProposalJSONRoundTripWithV3Fields(t *testing.T) {
	now := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	score := 0.89
	escalation := "Security concerns raised by @staff-engineer"
	p := Proposal{
		ID:               1,
		Description:      "Test proposal with v3 fields",
		Rationale:        "Schema gaps identified in v2",
		DomainTags:       []string{"architecture", "security"},
		FilesChanged:     []string{"internal/db/schema.go", "internal/model/proposal.go"},
		Criticality:      CriticalityHigh,
		Status:           ProposalStatusCommitted,
		FinalOutcome:     "Consensus reached. Changes applied.",
		EscalationReason: &escalation,
		RequiredVoters:   3,
		Threshold:        0.67,
		WeightedScore:    &score,
		CreatedBy:        "team-lead",
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	// Verify wire format contains new fields.
	var raw map[string]any
	json.Unmarshal(data, &raw)

	if raw["rationale"] != "Schema gaps identified in v2" {
		t.Errorf("JSON rationale = %v", raw["rationale"])
	}
	if raw["status"] != "committed" {
		t.Errorf("JSON status = %v, want 'committed'", raw["status"])
	}
	if raw["final_outcome"] != "Consensus reached. Changes applied." {
		t.Errorf("JSON final_outcome = %v", raw["final_outcome"])
	}
	if raw["escalation_reason"] != "Security concerns raised by @staff-engineer" {
		t.Errorf("JSON escalation_reason = %v", raw["escalation_reason"])
	}

	tags, ok := raw["domain_tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Errorf("JSON domain_tags = %v, want 2-element array", raw["domain_tags"])
	}
	files, ok := raw["files_changed"].([]any)
	if !ok || len(files) != 2 {
		t.Errorf("JSON files_changed = %v, want 2-element array", raw["files_changed"])
	}

	// Unmarshal back and verify all v3 fields survive.
	var p2 Proposal
	if err := json.Unmarshal(data, &p2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if p2.Rationale != p.Rationale {
		t.Errorf("Unmarshaled Rationale = %q, want %q", p2.Rationale, p.Rationale)
	}
	if len(p2.DomainTags) != 2 || p2.DomainTags[0] != "architecture" || p2.DomainTags[1] != "security" {
		t.Errorf("Unmarshaled DomainTags = %v", p2.DomainTags)
	}
	if len(p2.FilesChanged) != 2 || p2.FilesChanged[0] != "internal/db/schema.go" {
		t.Errorf("Unmarshaled FilesChanged = %v", p2.FilesChanged)
	}
	if p2.FinalOutcome != p.FinalOutcome {
		t.Errorf("Unmarshaled FinalOutcome = %q", p2.FinalOutcome)
	}
	if p2.EscalationReason == nil || *p2.EscalationReason != *p.EscalationReason {
		t.Errorf("Unmarshaled EscalationReason = %v", p2.EscalationReason)
	}
	if p2.Status != ProposalStatusCommitted {
		t.Errorf("Unmarshaled Status = %q, want 'committed'", p2.Status)
	}
}

func TestProposalJSONEmptyV3Fields(t *testing.T) {
	now := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	p := Proposal{
		ID:             1,
		Description:    "Minimal proposal",
		Criticality:    CriticalityMedium,
		Status:         ProposalStatusOpen,
		RequiredVoters: 2,
		Threshold:      0.67,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	json.Unmarshal(data, &raw)

	// Empty slices should serialize as [], not null.
	tags, ok := raw["domain_tags"].([]any)
	if !ok {
		t.Errorf("domain_tags should be [], got %v (type %T)", raw["domain_tags"], raw["domain_tags"])
	} else if len(tags) != 0 {
		t.Errorf("domain_tags should be empty, got %v", tags)
	}

	files, ok := raw["files_changed"].([]any)
	if !ok {
		t.Errorf("files_changed should be [], got %v (type %T)", raw["files_changed"], raw["files_changed"])
	} else if len(files) != 0 {
		t.Errorf("files_changed should be empty, got %v", files)
	}

	if raw["escalation_reason"] != nil {
		t.Errorf("escalation_reason should be null, got %v", raw["escalation_reason"])
	}
}

// --- Gap 5: Vote.EffectiveWeight computed field in JSON output ---

func TestVoteJSONEffectiveWeight(t *testing.T) {
	tests := []struct {
		name            string
		confidence      float64
		domainRelevance float64
		wantWeight      float64
	}{
		{"standard", 0.80, 0.90, 0.72},
		{"perfect", 1.0, 1.0, 1.0},
		{"zero confidence", 0.0, 0.9, 0.0},
		{"zero relevance", 0.9, 0.0, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
			v := Vote{
				ID:              1,
				ProposalID:      1,
				VoterName:       "test-voter",
				VoterRole:       "security",
				Verdict:         VerdictApprove,
				Confidence:      tt.confidence,
				DomainRelevance: tt.domainRelevance,
				CreatedAt:       now,
			}

			data, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("Marshal error: %v", err)
			}

			var raw map[string]any
			json.Unmarshal(data, &raw)

			ew, ok := raw["effective_weight"].(float64)
			if !ok {
				t.Fatalf("effective_weight missing or not a number: %v", raw["effective_weight"])
			}

			// Compare with tolerance for floating point.
			diff := ew - tt.wantWeight
			if diff < -0.001 || diff > 0.001 {
				t.Errorf("effective_weight = %f, want %f", ew, tt.wantWeight)
			}
		})
	}
}

func TestVoteJSONWithFindingsJSON(t *testing.T) {
	now := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	findings := &Findings{
		Blockers:    []string{},
		Concerns:    []string{"hardcoded paths"},
		Suggestions: []string{"add guard clause"},
	}
	v := Vote{
		ID:              1,
		ProposalID:      1,
		VoterName:       "reviewer",
		VoterRole:       "architecture",
		Verdict:         VerdictApproveWithConcerns,
		Confidence:      0.80,
		DomainRelevance: 0.90,
		Findings:        "Architecturally sound with concerns.",
		FindingsJSON:    findings,
		Summary:         "Architecturally sound with three practical concerns.",
		CreatedAt:       now,
	}

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	json.Unmarshal(data, &raw)

	if raw["verdict"] != "approve-with-concerns" {
		t.Errorf("verdict = %v, want 'approve-with-concerns'", raw["verdict"])
	}
	if raw["summary"] != "Architecturally sound with three practical concerns." {
		t.Errorf("summary = %v", raw["summary"])
	}

	fj, ok := raw["findings_json"].(map[string]any)
	if !ok {
		t.Fatalf("findings_json missing or wrong type: %v", raw["findings_json"])
	}
	concerns, ok := fj["concerns"].([]any)
	if !ok || len(concerns) != 1 {
		t.Errorf("findings_json.concerns = %v", fj["concerns"])
	}

	// Unmarshal back.
	var v2 Vote
	if err := json.Unmarshal(data, &v2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if v2.Verdict != VerdictApproveWithConcerns {
		t.Errorf("Unmarshaled Verdict = %q", v2.Verdict)
	}
	if v2.FindingsJSON == nil {
		t.Fatal("Unmarshaled FindingsJSON is nil")
	}
	if len(v2.FindingsJSON.Concerns) != 1 || v2.FindingsJSON.Concerns[0] != "hardcoded paths" {
		t.Errorf("Unmarshaled FindingsJSON.Concerns = %v", v2.FindingsJSON.Concerns)
	}
	if v2.Summary != v.Summary {
		t.Errorf("Unmarshaled Summary = %q", v2.Summary)
	}
}

func TestVoteJSONBackwardCompatV2(t *testing.T) {
	// Unmarshal v2-era JSON (no findings_json, no summary, no effective_weight).
	v2JSON := `{
		"id": 1,
		"proposal_id": "DKT-V3",
		"voter_name": "old-voter",
		"voter_role": "security",
		"verdict": "approve",
		"confidence": 0.9,
		"domain_relevance": 0.85,
		"findings": "Looks good",
		"created_at": "2026-03-20T10:05:00Z"
	}`

	var v Vote
	if err := json.Unmarshal([]byte(v2JSON), &v); err != nil {
		t.Fatalf("Unmarshal v2 JSON error: %v", err)
	}
	if v.FindingsJSON != nil {
		t.Errorf("FindingsJSON should be nil for v2 JSON, got %v", v.FindingsJSON)
	}
	if v.Summary != "" {
		t.Errorf("Summary should be empty for v2 JSON, got %q", v.Summary)
	}
	if v.VoterName != "old-voter" {
		t.Errorf("VoterName = %q", v.VoterName)
	}
}
