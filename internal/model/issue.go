package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IDPrefix is the prefix used for issue IDs in display and JSON output.
const IDPrefix = "DKT"

// Status represents the workflow state of an issue.
type Status string

const (
	StatusBacklog    Status = "backlog"
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in-progress"
	StatusReview     Status = "review"
	StatusDone       Status = "done"
)

var validStatuses = []Status{
	StatusBacklog,
	StatusTodo,
	StatusInProgress,
	StatusReview,
	StatusDone,
}

// ValidateStatus returns an error if s is not a recognized status.
func ValidateStatus(s Status) error {
	for _, v := range validStatuses {
		if s == v {
			return nil
		}
	}
	return fmt.Errorf("invalid status %q: must be one of %v", s, validStatuses)
}

// Color returns a color name string suitable for terminal rendering.
func (s Status) Color() string {
	switch s {
	case StatusBacklog:
		return "gray"
	case StatusTodo:
		return "blue"
	case StatusInProgress:
		return "yellow"
	case StatusReview:
		return "magenta"
	case StatusDone:
		return "green"
	default:
		return "white"
	}
}

// Priority represents the urgency of an issue.
type Priority string

const (
	PriorityCritical Priority = "critical"
	PriorityHigh     Priority = "high"
	PriorityMedium   Priority = "medium"
	PriorityLow      Priority = "low"
	PriorityNone     Priority = "none"
)

var validPriorities = []Priority{
	PriorityCritical,
	PriorityHigh,
	PriorityMedium,
	PriorityLow,
	PriorityNone,
}

// ValidatePriority returns an error if p is not a recognized priority.
func ValidatePriority(p Priority) error {
	for _, v := range validPriorities {
		if p == v {
			return nil
		}
	}
	return fmt.Errorf("invalid priority %q: must be one of %v", p, validPriorities)
}

// Color returns a color name string suitable for terminal rendering.
func (p Priority) Color() string {
	switch p {
	case PriorityCritical:
		return "red"
	case PriorityHigh:
		return "yellow"
	case PriorityMedium:
		return "blue"
	case PriorityLow:
		return "gray"
	case PriorityNone:
		return "white"
	default:
		return "white"
	}
}

// Emoji returns an emoji string for the priority level.
func (p Priority) Emoji() string {
	switch p {
	case PriorityCritical:
		return "!!!"
	case PriorityHigh:
		return "!!"
	case PriorityMedium:
		return "!"
	case PriorityLow:
		return "-"
	case PriorityNone:
		return " "
	default:
		return " "
	}
}

// IssueKind represents the category of an issue.
type IssueKind string

const (
	IssueKindBug     IssueKind = "bug"
	IssueKindFeature IssueKind = "feature"
	IssueKindTask    IssueKind = "task"
	IssueKindEpic    IssueKind = "epic"
	IssueKindChore   IssueKind = "chore"
)

var validIssueKinds = []IssueKind{
	IssueKindBug,
	IssueKindFeature,
	IssueKindTask,
	IssueKindEpic,
	IssueKindChore,
}

// ValidateIssueKind returns an error if k is not a recognized issue kind.
func ValidateIssueKind(k IssueKind) error {
	for _, v := range validIssueKinds {
		if k == v {
			return nil
		}
	}
	return fmt.Errorf("invalid issue kind %q: must be one of %v", k, validIssueKinds)
}

// FormatID returns the display form of an issue ID, e.g. "DKT-5".
func FormatID(id int) string {
	return fmt.Sprintf("%s-%d", IDPrefix, id)
}

// ParseID accepts both "DKT-5" and "5" and returns the numeric ID.
// The prefix check is case-insensitive; len(prefix) is safe to use for
// slicing because IDPrefix is ASCII and ToUpper preserves its byte length.
func ParseID(input string) (int, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, fmt.Errorf("empty issue ID")
	}

	prefix := IDPrefix + "-"
	if strings.HasPrefix(strings.ToUpper(s), prefix) {
		s = s[len(prefix):]
	}

	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid issue ID %q: %w", input, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("invalid issue ID %q: must be positive", input)
	}

	return id, nil
}

// Issue represents a tracked issue.
type Issue struct {
	ID          int
	ParentID    *int
	Title       string
	Description string
	Status      Status
	Priority    Priority
	Kind        IssueKind
	Assignee    string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// issueJSON is the JSON wire format for Issue.
type issueJSON struct {
	ID          string  `json:"id"`
	ParentID    *string `json:"parent_id,omitempty"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Status      string  `json:"status"`
	Priority    string  `json:"priority"`
	Kind        string  `json:"kind"`
	Assignee    string  `json:"assignee"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// MarshalJSON implements custom JSON serialization for Issue.
func (i Issue) MarshalJSON() ([]byte, error) {
	j := issueJSON{
		ID:          FormatID(i.ID),
		Title:       i.Title,
		Description: i.Description,
		Status:      string(i.Status),
		Priority:    string(i.Priority),
		Kind:        string(i.Kind),
		Assignee:    i.Assignee,
		CreatedAt:   i.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   i.UpdatedAt.UTC().Format(time.RFC3339),
	}

	if i.ParentID != nil {
		pid := FormatID(*i.ParentID)
		j.ParentID = &pid
	}

	return json.Marshal(j)
}

// UnmarshalJSON implements custom JSON deserialization for Issue.
func (i *Issue) UnmarshalJSON(data []byte) error {
	var j issueJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	id, err := ParseID(j.ID)
	if err != nil {
		return fmt.Errorf("parsing issue id: %w", err)
	}
	i.ID = id

	if j.ParentID != nil {
		pid, err := ParseID(*j.ParentID)
		if err != nil {
			return fmt.Errorf("parsing parent id: %w", err)
		}
		i.ParentID = &pid
	}

	i.Title = j.Title
	i.Description = j.Description
	i.Status = Status(j.Status)
	if err := ValidateStatus(i.Status); err != nil {
		return err
	}

	i.Priority = Priority(j.Priority)
	if err := ValidatePriority(i.Priority); err != nil {
		return err
	}

	i.Kind = IssueKind(j.Kind)
	if err := ValidateIssueKind(i.Kind); err != nil {
		return err
	}

	i.Assignee = j.Assignee

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	i.CreatedAt = createdAt

	updatedAt, err := time.Parse(time.RFC3339, j.UpdatedAt)
	if err != nil {
		return fmt.Errorf("parsing updated_at: %w", err)
	}
	i.UpdatedAt = updatedAt

	return nil
}
