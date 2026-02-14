package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFormatID(t *testing.T) {
	if got := FormatID(5); got != "DKT-5" {
		t.Errorf("FormatID(5) = %q, want %q", got, "DKT-5")
	}
	if got := FormatID(42); got != "DKT-42" {
		t.Errorf("FormatID(42) = %q, want %q", got, "DKT-42")
	}
}

func TestParseID(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"DKT-5", 5, false},
		{"dkt-5", 5, false},
		{"5", 5, false},
		{"42", 42, false},
		{"", 0, true},
		{"DKT-", 0, true},
		{"abc", 0, true},
		{"DKT-0", 0, true},
		{"DKT--1", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseID(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseID(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseID(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	for _, id := range []int{1, 5, 42, 999} {
		formatted := FormatID(id)
		parsed, err := ParseID(formatted)
		if err != nil {
			t.Errorf("ParseID(FormatID(%d)) error: %v", id, err)
			continue
		}
		if parsed != id {
			t.Errorf("ParseID(FormatID(%d)) = %d", id, parsed)
		}
	}
}

func TestValidateStatus(t *testing.T) {
	valid := []Status{StatusBacklog, StatusTodo, StatusInProgress, StatusReview, StatusDone}
	for _, s := range valid {
		if err := ValidateStatus(s); err != nil {
			t.Errorf("ValidateStatus(%q) unexpected error: %v", s, err)
		}
	}
	if err := ValidateStatus("invalid"); err == nil {
		t.Error("ValidateStatus('invalid') expected error, got nil")
	}
}

func TestValidatePriority(t *testing.T) {
	valid := []Priority{PriorityCritical, PriorityHigh, PriorityMedium, PriorityLow, PriorityNone}
	for _, p := range valid {
		if err := ValidatePriority(p); err != nil {
			t.Errorf("ValidatePriority(%q) unexpected error: %v", p, err)
		}
	}
	if err := ValidatePriority("invalid"); err == nil {
		t.Error("ValidatePriority('invalid') expected error, got nil")
	}
}

func TestValidateIssueKind(t *testing.T) {
	valid := []IssueKind{IssueKindBug, IssueKindFeature, IssueKindTask, IssueKindEpic, IssueKindChore}
	for _, k := range valid {
		if err := ValidateIssueKind(k); err != nil {
			t.Errorf("ValidateIssueKind(%q) unexpected error: %v", k, err)
		}
	}
	if err := ValidateIssueKind("invalid"); err == nil {
		t.Error("ValidateIssueKind('invalid') expected error, got nil")
	}
}

func TestStatusColor(t *testing.T) {
	if c := StatusDone.Color(); c != "green" {
		t.Errorf("StatusDone.Color() = %q, want %q", c, "green")
	}
	if c := StatusInProgress.Color(); c != "yellow" {
		t.Errorf("StatusInProgress.Color() = %q, want %q", c, "yellow")
	}
}

func TestPriorityColorAndEmoji(t *testing.T) {
	if c := PriorityCritical.Color(); c != "red" {
		t.Errorf("PriorityCritical.Color() = %q, want %q", c, "red")
	}
	if e := PriorityCritical.Emoji(); e != "!!!" {
		t.Errorf("PriorityCritical.Emoji() = %q, want %q", e, "!!!")
	}
}

func TestParseRelationType(t *testing.T) {
	tests := []struct {
		input   string
		want    RelationType
		wantErr bool
	}{
		{"blocks", RelationBlocks, false},
		{"depends_on", RelationDependsOn, false},
		{"depends-on", RelationDependsOn, false},
		{"relates_to", RelationRelatesTo, false},
		{"relates-to", RelationRelatesTo, false},
		{"duplicates", RelationDuplicates, false},
		{"invalid", "", true},
	}

	for _, tt := range tests {
		got, err := ParseRelationType(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseRelationType(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseRelationType(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRelationTypeInverse(t *testing.T) {
	tests := []struct {
		rt   RelationType
		want string
	}{
		{RelationBlocks, "blocked_by"},
		{RelationDependsOn, "dependency_of"},
		{RelationRelatesTo, "relates_to"},
		{RelationDuplicates, "duplicate_of"},
	}

	for _, tt := range tests {
		if got := tt.rt.Inverse(); got != tt.want {
			t.Errorf("%q.Inverse() = %q, want %q", tt.rt, got, tt.want)
		}
	}
}

func TestIssueJSONRoundTrip(t *testing.T) {
	parentID := 1
	now := time.Date(2026, 2, 13, 12, 0, 0, 0, time.UTC)
	issue := Issue{
		ID:          5,
		ParentID:    &parentID,
		Title:       "Fix the bug",
		Description: "Something is broken",
		Status:      StatusInProgress,
		Priority:    PriorityHigh,
		Kind:        IssueKindBug,
		Assignee:    "alice",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	data, err := json.Marshal(issue)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	// Verify JSON contains prefixed ID
	var raw map[string]any
	json.Unmarshal(data, &raw)
	if raw["id"] != "DKT-5" {
		t.Errorf("JSON id = %v, want %q", raw["id"], "DKT-5")
	}
	if raw["parent_id"] != "DKT-1" {
		t.Errorf("JSON parent_id = %v, want %q", raw["parent_id"], "DKT-1")
	}
	if raw["status"] != "in-progress" {
		t.Errorf("JSON status = %v, want %q", raw["status"], "in-progress")
	}

	// Unmarshal back
	var issue2 Issue
	if err := json.Unmarshal(data, &issue2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if issue2.ID != 5 {
		t.Errorf("Unmarshaled ID = %d, want 5", issue2.ID)
	}
	if issue2.ParentID == nil || *issue2.ParentID != 1 {
		t.Errorf("Unmarshaled ParentID = %v, want 1", issue2.ParentID)
	}
	if issue2.Status != StatusInProgress {
		t.Errorf("Unmarshaled Status = %q, want %q", issue2.Status, StatusInProgress)
	}
}

func TestIssueJSONNoParent(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	issue := Issue{
		ID:        1,
		Title:     "Test",
		Status:    StatusBacklog,
		Priority:  PriorityNone,
		Kind:      IssueKindTask,
		CreatedAt: now,
		UpdatedAt: now,
	}

	data, err := json.Marshal(issue)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	json.Unmarshal(data, &raw)
	if _, exists := raw["parent_id"]; exists {
		t.Error("JSON should omit parent_id when nil")
	}
}

func TestCommentJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 2, 13, 12, 0, 0, 0, time.UTC)
	comment := Comment{
		ID:        3,
		IssueID:   5,
		Body:      "Looks good",
		Author:    "bob",
		CreatedAt: now,
	}

	data, err := json.Marshal(comment)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	json.Unmarshal(data, &raw)
	if raw["id"] != float64(3) {
		t.Errorf("JSON id = %v, want %v", raw["id"], 3)
	}
	if raw["issue_id"] != "DKT-5" {
		t.Errorf("JSON issue_id = %v, want %q", raw["issue_id"], "DKT-5")
	}

	var comment2 Comment
	if err := json.Unmarshal(data, &comment2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if comment2.ID != 3 || comment2.IssueID != 5 {
		t.Errorf("Unmarshaled comment: ID=%d IssueID=%d, want 3 and 5", comment2.ID, comment2.IssueID)
	}
}
