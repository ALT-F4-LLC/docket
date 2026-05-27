package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFormatDocID(t *testing.T) {
	tests := []struct {
		id   int
		want string
	}{
		{1, "DOC-1"},
		{5, "DOC-5"},
		{42, "DOC-42"},
		{999, "DOC-999"},
	}
	for _, tt := range tests {
		if got := FormatDocID(tt.id); got != tt.want {
			t.Errorf("FormatDocID(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestParseDocID(t *testing.T) {
	tests := []struct {
		input   string
		want    int
		wantErr bool
	}{
		{"DOC-5", 5, false},
		{"doc-5", 5, false},
		{"  DOC-10  ", 10, false},
		{"5", 5, false},
		{"42", 42, false},
		{"", 0, true},
		{"DOC-", 0, true},
		{"abc", 0, true},
		{"DOC-0", 0, true},
		{"DOC--1", 0, true},
	}

	for _, tt := range tests {
		got, err := ParseDocID(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseDocID(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseDocID(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestFormatParseDocIDRoundTrip(t *testing.T) {
	for _, id := range []int{1, 5, 42, 999} {
		formatted := FormatDocID(id)
		parsed, err := ParseDocID(formatted)
		if err != nil {
			t.Errorf("ParseDocID(FormatDocID(%d)) error: %v", id, err)
			continue
		}
		if parsed != id {
			t.Errorf("ParseDocID(FormatDocID(%d)) = %d", id, parsed)
		}
	}
}

func TestDocJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 26, 16, 0, 0, 0, time.UTC)
	doc := Doc{
		ID:        42,
		Type:      "tdd",
		Status:    "draft",
		Title:     "Add docket doc CLI",
		Body:      "...current body...",
		Author:    "Erik Reinert",
		CreatedAt: now,
		UpdatedAt: now,
	}

	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal into map error: %v", err)
	}
	if raw["id"] != "DOC-42" {
		t.Errorf("JSON id = %v, want %q", raw["id"], "DOC-42")
	}
	if raw["type"] != "tdd" {
		t.Errorf("JSON type = %v, want %q", raw["type"], "tdd")
	}
	if raw["status"] != "draft" {
		t.Errorf("JSON status = %v, want %q", raw["status"], "draft")
	}
	if raw["created_at"] != "2026-05-26T16:00:00Z" {
		t.Errorf("JSON created_at = %v, want RFC3339 string", raw["created_at"])
	}

	var doc2 Doc
	if err := json.Unmarshal(data, &doc2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if doc2.ID != 42 {
		t.Errorf("Unmarshaled ID = %d, want 42", doc2.ID)
	}
	if doc2.Type != "tdd" || doc2.Status != "draft" || doc2.Title != "Add docket doc CLI" {
		t.Errorf("Unmarshaled doc = %+v", doc2)
	}
	if !doc2.CreatedAt.Equal(now) || !doc2.UpdatedAt.Equal(now) {
		t.Errorf("Unmarshaled timestamps lost precision: created=%v updated=%v", doc2.CreatedAt, doc2.UpdatedAt)
	}
}

func TestDocJSONFreeFormTypeAndStatus(t *testing.T) {
	now := time.Date(2026, 5, 26, 16, 0, 0, 0, time.UTC)
	doc := Doc{
		ID:        1,
		Type:      "custom-shape",
		Status:    "wip-in-flight",
		Title:     "Free-form fields",
		CreatedAt: now,
		UpdatedAt: now,
	}

	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var doc2 Doc
	if err := json.Unmarshal(data, &doc2); err != nil {
		t.Fatalf("Unmarshal error (free-form values should not be validated): %v", err)
	}
	if doc2.Type != "custom-shape" || doc2.Status != "wip-in-flight" {
		t.Errorf("Unmarshaled free-form fields = %+v", doc2)
	}
}

func TestDocRevisionJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 26, 16, 15, 0, 0, time.UTC)
	rev := DocRevision{
		ID:             7,
		DocID:          42,
		RevisionNumber: 3,
		Body:           "revision body",
		ChangeKind:     "status+body",
		Author:         "vote-bot",
		CreatedAt:      now,
	}

	data, err := json.Marshal(rev)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal into map error: %v", err)
	}
	if raw["doc_id"] != "DOC-42" {
		t.Errorf("JSON doc_id = %v, want %q", raw["doc_id"], "DOC-42")
	}
	if raw["revision_number"] != float64(3) {
		t.Errorf("JSON revision_number = %v, want 3", raw["revision_number"])
	}
	if raw["change_kind"] != "status+body" {
		t.Errorf("JSON change_kind = %v, want %q", raw["change_kind"], "status+body")
	}
	if raw["created_at"] != "2026-05-26T16:15:00Z" {
		t.Errorf("JSON created_at = %v, want RFC3339 string", raw["created_at"])
	}

	var rev2 DocRevision
	if err := json.Unmarshal(data, &rev2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if rev2.DocID != 42 || rev2.RevisionNumber != 3 || rev2.ChangeKind != "status+body" {
		t.Errorf("Unmarshaled DocRevision = %+v", rev2)
	}
	if !rev2.CreatedAt.Equal(now) {
		t.Errorf("Unmarshaled created_at lost precision: %v", rev2.CreatedAt)
	}
}

func TestDocCommentJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 26, 16, 30, 0, 0, time.UTC)
	comment := DocComment{
		ID:        9,
		DocID:     42,
		Body:      "Looks good",
		Author:    "alice",
		CreatedAt: now,
	}

	data, err := json.Marshal(comment)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal into map error: %v", err)
	}
	if raw["id"] != float64(9) {
		t.Errorf("JSON id = %v, want 9", raw["id"])
	}
	if raw["doc_id"] != "DOC-42" {
		t.Errorf("JSON doc_id = %v, want %q", raw["doc_id"], "DOC-42")
	}

	var c2 DocComment
	if err := json.Unmarshal(data, &c2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if c2.ID != 9 || c2.DocID != 42 || c2.Author != "alice" || c2.Body != "Looks good" {
		t.Errorf("Unmarshaled DocComment = %+v", c2)
	}
	if !c2.CreatedAt.Equal(now) {
		t.Errorf("Unmarshaled created_at lost precision: %v", c2.CreatedAt)
	}
}

func TestDocIssueLinkJSON(t *testing.T) {
	link := DocIssueLink{DocID: 42, IssueID: 12, CreatedAt: "2026-05-26T16:00:00Z"}
	data, err := json.Marshal(link)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal into map error: %v", err)
	}
	if raw["doc_id"] != float64(42) {
		t.Errorf("JSON doc_id = %v, want 42 (plain int per export shape)", raw["doc_id"])
	}
	if raw["issue_id"] != float64(12) {
		t.Errorf("JSON issue_id = %v, want 12", raw["issue_id"])
	}
	if raw["created_at"] != "2026-05-26T16:00:00Z" {
		t.Errorf("JSON created_at = %v", raw["created_at"])
	}

	var link2 DocIssueLink
	if err := json.Unmarshal(data, &link2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if link2 != link {
		t.Errorf("Unmarshaled DocIssueLink = %+v, want %+v", link2, link)
	}
}

func TestProposalDocLinkJSON(t *testing.T) {
	link := ProposalDocLink{ProposalID: 3, DocID: 42, CreatedAt: "2026-05-26T16:00:00Z"}
	data, err := json.Marshal(link)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal into map error: %v", err)
	}
	if raw["proposal_id"] != float64(3) {
		t.Errorf("JSON proposal_id = %v, want 3 (plain int per export shape)", raw["proposal_id"])
	}
	if raw["doc_id"] != float64(42) {
		t.Errorf("JSON doc_id = %v, want 42", raw["doc_id"])
	}

	var link2 ProposalDocLink
	if err := json.Unmarshal(data, &link2); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if link2 != link {
		t.Errorf("Unmarshaled ProposalDocLink = %+v, want %+v", link2, link)
	}
}
