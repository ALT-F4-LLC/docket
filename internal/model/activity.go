package model

import "time"

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
