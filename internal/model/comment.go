package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// Comment represents a comment on an issue.
type Comment struct {
	ID        int
	IssueID   int
	Body      string
	Author    string
	CreatedAt time.Time
}

// AuthorOrAnonymous returns the author name, falling back to "anonymous"
// when the field is empty.
func (c Comment) AuthorOrAnonymous() string {
	if c.Author == "" {
		return "anonymous"
	}
	return c.Author
}

// commentJSON is the JSON wire format for Comment.
type commentJSON struct {
	ID        int    `json:"id"`
	IssueID   string `json:"issue_id"`
	Body      string `json:"body"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

// MarshalJSON implements custom JSON serialization for Comment.
func (c Comment) MarshalJSON() ([]byte, error) {
	return json.Marshal(commentJSON{
		ID:        c.ID,
		IssueID:   FormatID(c.IssueID),
		Body:      c.Body,
		Author:    c.AuthorOrAnonymous(),
		CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// UnmarshalJSON implements custom JSON deserialization for Comment.
func (c *Comment) UnmarshalJSON(data []byte) error {
	var j commentJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	c.ID = j.ID

	issueID, err := ParseID(j.IssueID)
	if err != nil {
		return fmt.Errorf("parsing issue id: %w", err)
	}
	c.IssueID = issueID

	c.Body = j.Body
	c.Author = j.Author

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	c.CreatedAt = createdAt

	return nil
}
