package model

import (
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
