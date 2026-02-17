package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// Activity represents a change record for an issue field.
type Activity struct {
	ID           int
	IssueID      int
	FieldChanged string
	OldValue     string
	NewValue     string
	ChangedBy    string
	CreatedAt    time.Time
}

// activityJSON is the JSON wire format for Activity.
type activityJSON struct {
	ID           int    `json:"id"`
	IssueID      string `json:"issue_id"`
	FieldChanged string `json:"field_changed"`
	OldValue     string `json:"old_value"`
	NewValue     string `json:"new_value"`
	ChangedBy    string `json:"changed_by"`
	CreatedAt    string `json:"created_at"`
}

// MarshalJSON implements custom JSON serialization for Activity.
func (a Activity) MarshalJSON() ([]byte, error) {
	return json.Marshal(activityJSON{
		ID:           a.ID,
		IssueID:      FormatID(a.IssueID),
		FieldChanged: a.FieldChanged,
		OldValue:     a.OldValue,
		NewValue:     a.NewValue,
		ChangedBy:    a.ChangedBy,
		CreatedAt:    a.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// UnmarshalJSON implements custom JSON deserialization for Activity.
func (a *Activity) UnmarshalJSON(data []byte) error {
	var j activityJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	a.ID = j.ID

	issueID, err := ParseID(j.IssueID)
	if err != nil {
		return fmt.Errorf("parsing issue id: %w", err)
	}
	a.IssueID = issueID

	a.FieldChanged = j.FieldChanged
	a.OldValue = j.OldValue
	a.NewValue = j.NewValue
	a.ChangedBy = j.ChangedBy

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	a.CreatedAt = createdAt

	return nil
}
