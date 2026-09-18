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
	RelationBlocks     RelationType = "blocks"
	RelationDependsOn  RelationType = "depends_on"
	RelationRelatesTo  RelationType = "relates_to"
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
// forms and returns the canonical underscored RelationType. The accepted set is
// exactly validRelationTypes in either spelling — this function backs the JSON
// wire format (Relation.UnmarshalJSON) and, through ParseRelationDirection, the
// workflow `issue.linked.<relation>.<kind>` vocabulary, so no near-miss spelling
// is tolerated here. The CLI's own typo courtesy for "related_to" lives at the
// command boundary instead (internal/cli/issue_link.go, DKT-1073/DKT-1077).
func ParseRelationType(input string) (RelationType, error) {
	normalized := RelationType(strings.ReplaceAll(strings.TrimSpace(input), "-", "_"))
	if err := ValidateRelationType(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

// RelationDirectionTokens lists every token ParseRelationDirection accepts, in
// its canonical underscored form — the four relation types and their inverse
// display names. It exists so a consumer refusing a token can name the whole
// vocabulary rather than restate it (DKT-547).
func RelationDirectionTokens() []string {
	out := make([]string, 0, 2*len(validRelationTypes))
	for _, rt := range validRelationTypes {
		out = append(out, string(rt))
		if inv := rt.Inverse(); inv != string(rt) {
			out = append(out, inv)
		}
	}
	return out
}

// ParseRelationDirection resolves a relation token that may name EITHER
// direction of a relation: a canonical type ("depends_on") or an inverse
// display name ("dependency_of", "blocked_by", "duplicate_of"). It returns the
// canonical type and whether the token named the inverse direction — for
// "depends_on" the subject is the relation's SOURCE, for "dependency_of" its
// TARGET (DKT-547: the `issue.linked.<relation>.<kind>` input form addresses
// linked issues by exactly these tokens).
//
// Hyphenated spellings normalize like ParseRelationType's ("depends-on",
// "blocked-by"). The symmetric "relates_to" is its own inverse and parses as
// the canonical direction; its consumers treat both directions alike.
func ParseRelationDirection(input string) (rt RelationType, inverse bool, err error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(input), "-", "_")
	if rt, err := ParseRelationType(normalized); err == nil {
		return rt, false, nil
	}
	for _, rt := range validRelationTypes {
		if rt.Inverse() == normalized {
			return rt, true, nil
		}
	}
	return "", false, fmt.Errorf(
		"invalid relation %q: must be one of %v", input, RelationDirectionTokens())
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
