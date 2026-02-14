package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RelationType represents the kind of relationship between two issues.
type RelationType string

const (
	RelationBlocks    RelationType = "blocks"
	RelationDependsOn RelationType = "depends_on"
	RelationRelatesTo RelationType = "relates_to"
	RelationDuplicates RelationType = "duplicates"
)

var validRelationTypes = []RelationType{
	RelationBlocks,
	RelationDependsOn,
	RelationRelatesTo,
	RelationDuplicates,
}

// ValidateRelationType returns an error if rt is not a recognized relation type.
func ValidateRelationType(rt RelationType) error {
	for _, v := range validRelationTypes {
		if rt == v {
			return nil
		}
	}
	return fmt.Errorf("invalid relation type %q: must be one of %v", rt, validRelationTypes)
}

// ParseRelationType accepts both hyphenated ("depends-on") and underscored ("depends_on")
// forms and returns the canonical underscored RelationType.
func ParseRelationType(input string) (RelationType, error) {
	normalized := RelationType(strings.ReplaceAll(strings.TrimSpace(input), "-", "_"))
	if err := ValidateRelationType(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

// Inverse returns the display name for the inverse direction of a relation.
// For example, "blocks" returns "blocked_by" and "depends_on" returns "dependency_of".
// Symmetric relations ("relates_to") return themselves.
func (rt RelationType) Inverse() string {
	switch rt {
	case RelationBlocks:
		return "blocked_by"
	case RelationDependsOn:
		return "dependency_of"
	case RelationRelatesTo:
		return "relates_to"
	case RelationDuplicates:
		return "duplicate_of"
	default:
		return string(rt)
	}
}

// Relation represents a relationship between two issues.
type Relation struct {
	ID            int
	SourceIssueID int
	TargetIssueID int
	RelationType  RelationType
	CreatedAt     time.Time
}

// relationJSON is the JSON wire format for Relation.
type relationJSON struct {
	ID            int    `json:"id"`
	SourceIssueID string `json:"source_issue_id"`
	TargetIssueID string `json:"target_issue_id"`
	RelationType  string `json:"relation_type"`
	CreatedAt     string `json:"created_at"`
}

// MarshalJSON implements custom JSON serialization for Relation.
func (r Relation) MarshalJSON() ([]byte, error) {
	return json.Marshal(relationJSON{
		ID:            r.ID,
		SourceIssueID: FormatID(r.SourceIssueID),
		TargetIssueID: FormatID(r.TargetIssueID),
		RelationType:  string(r.RelationType),
		CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
	})
}

// UnmarshalJSON implements custom JSON deserialization for Relation.
func (r *Relation) UnmarshalJSON(data []byte) error {
	var j relationJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}

	r.ID = j.ID

	sourceID, err := ParseID(j.SourceIssueID)
	if err != nil {
		return fmt.Errorf("parsing source issue id: %w", err)
	}
	r.SourceIssueID = sourceID

	targetID, err := ParseID(j.TargetIssueID)
	if err != nil {
		return fmt.Errorf("parsing target issue id: %w", err)
	}
	r.TargetIssueID = targetID

	rt, err := ParseRelationType(j.RelationType)
	if err != nil {
		return err
	}
	r.RelationType = rt

	createdAt, err := time.Parse(time.RFC3339, j.CreatedAt)
	if err != nil {
		return fmt.Errorf("parsing created_at: %w", err)
	}
	r.CreatedAt = createdAt

	return nil
}
