package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DocIDPrefix is the prefix used for doc IDs in display and JSON output.
// A single global counter is used across all doc types (see TDD §3.3, C2).
const DocIDPrefix = "DOC"

// FormatDocID returns the display form of a doc ID, e.g. "DOC-5".
func FormatDocID(id int) string {
	return fmt.Sprintf("%s-%d", DocIDPrefix, id)
}

// ParseDocID accepts both "DOC-5" and "5" and returns the numeric ID.
// The prefix check is case-insensitive.
func ParseDocID(input string) (int, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, fmt.Errorf("empty doc ID")
	}

	prefix := DocIDPrefix + "-"
	if strings.HasPrefix(strings.ToUpper(s), prefix) {
		s = s[len(prefix):]
	}

	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid doc ID %q: %w", input, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid doc ID %q: must be positive", input)
	}

	return id, nil
}

// Doc represents a tracked design document (TDD, ADR, PRD, etc.). `Type` and
// `Status` are free-form per TDD §5.4 — no enum validation at the model layer.
type Doc struct {
	ID        int
	Type      string
	Status    string
	Title     string
	Body      string
	Author    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// docJSON is the JSON wire format for Doc.
type docJSON struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// MarshalJSON implements custom JSON serialization for Doc.
func (d Doc) MarshalJSON() ([]byte, error) {
	return json.Marshal(docJSON{
		ID:        FormatDocID(d.ID),
		Type:      d.Type,
		Status:    d.Status,
		Title:     d.Title,
		Body:      d.Body,
		Author:    d.Author,
		CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: d.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

// UnmarshalJSON implements custom JSON deserialization for Doc.
func (d *Doc) UnmarshalJSON(data []byte) error {
	var j docJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	id, err := ParseDocID(j.ID)
	if err != nil {
		return fmt.Errorf("parsing doc id: %w", err)
	}
	d.ID = id

	d.Type = j.Type
	d.Status = j.Status
	d.Title = j.Title
	d.Body = j.Body
	d.Author = j.Author

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	d.CreatedAt = createdAt

	updatedAt, err := time.Parse(time.RFC3339, j.UpdatedAt)
	if err != nil {
		return fmt.Errorf("parsing updated_at: %w", err)
	}
	d.UpdatedAt = updatedAt

	return nil
}

type DocRef struct {
	ID     int
	Type   string
	Status string
	Title  string
}

type docRefJSON struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

func (r DocRef) MarshalJSON() ([]byte, error) {
	return json.Marshal(docRefJSON{
		ID:     FormatDocID(r.ID),
		Type:   r.Type,
		Title:  r.Title,
		Status: r.Status,
	})
}

// UnmarshalJSON is a deliberate no-op: DocRef is a render-only output
// projection hydrated from the link table, never parsed back from JSON.
func (r *DocRef) UnmarshalJSON([]byte) error {
	return nil
}

// DocRevision is an append-only history row for a Doc. `ChangeKind` is a
// free-form descriptor: "create", "body", "status", "title", "type", or
// comma-joined for combined edits (e.g. "status+body") per TDD §5.4 C8.
type DocRevision struct {
	ID             int
	DocID          int
	RevisionNumber int
	Body           string
	ChangeKind     string
	Author         string
	CreatedAt      time.Time
}

// docRevisionJSON is the JSON wire format for DocRevision.
type docRevisionJSON struct {
	ID             int    `json:"id"`
	DocID          string `json:"doc_id"`
	RevisionNumber int    `json:"revision_number"`
	Body           string `json:"body"`
	ChangeKind     string `json:"change_kind"`
	Author         string `json:"author"`
	CreatedAt      string `json:"created_at"`
}

// MarshalJSON implements custom JSON serialization for DocRevision.
func (r DocRevision) MarshalJSON() ([]byte, error) {
	return json.Marshal(docRevisionJSON{
		ID:             r.ID,
		DocID:          FormatDocID(r.DocID),
		RevisionNumber: r.RevisionNumber,
		Body:           r.Body,
		ChangeKind:     r.ChangeKind,
		Author:         r.Author,
		CreatedAt:      r.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// UnmarshalJSON implements custom JSON deserialization for DocRevision.
func (r *DocRevision) UnmarshalJSON(data []byte) error {
	var j docRevisionJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	r.ID = j.ID

	docID, err := ParseDocID(j.DocID)
	if err != nil {
		return fmt.Errorf("parsing doc id: %w", err)
	}
	r.DocID = docID

	r.RevisionNumber = j.RevisionNumber
	r.Body = j.Body
	r.ChangeKind = j.ChangeKind
	r.Author = j.Author

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	r.CreatedAt = createdAt

	return nil
}

// DocComment represents a comment on a Doc. Mirrors `model.Comment` (author
// stored as plain string; DB scan layer wraps with sql.NullString per S5).
type DocComment struct {
	ID        int
	DocID     int
	Body      string
	Author    string
	CreatedAt time.Time
}

// docCommentJSON is the JSON wire format for DocComment.
type docCommentJSON struct {
	ID        int    `json:"id"`
	DocID     string `json:"doc_id"`
	Body      string `json:"body"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

// MarshalJSON implements custom JSON serialization for DocComment.
func (c DocComment) MarshalJSON() ([]byte, error) {
	return json.Marshal(docCommentJSON{
		ID:        c.ID,
		DocID:     FormatDocID(c.DocID),
		Body:      c.Body,
		Author:    c.Author,
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// UnmarshalJSON implements custom JSON deserialization for DocComment.
func (c *DocComment) UnmarshalJSON(data []byte) error {
	var j docCommentJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	c.ID = j.ID

	docID, err := ParseDocID(j.DocID)
	if err != nil {
		return fmt.Errorf("parsing doc id: %w", err)
	}
	c.DocID = docID

	c.Body = j.Body
	c.Author = j.Author

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	c.CreatedAt = createdAt

	return nil
}

// DocIssueLink is an export-format row from the doc_issue_links join table.
// IDs are plain ints (mirrors IssueLabelMapping); CreatedAt is a string to
// round-trip the on-disk RFC3339 representation verbatim.
type DocIssueLink struct {
	DocID     int    `json:"doc_id"`
	IssueID   int    `json:"issue_id"`
	CreatedAt string `json:"created_at"`
}

// ProposalDocLink is an export-format row from the proposal_docs join table.
type ProposalDocLink struct {
	ProposalID int    `json:"proposal_id"`
	DocID      int    `json:"doc_id"`
	CreatedAt  string `json:"created_at"`
}
