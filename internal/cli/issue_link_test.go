package cli

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// TestParseRelationTypeArg pins DKT-1073's typo courtesy to the command
// boundary it was asked for (DKT-1077): `issue link add/remove` accepts
// "related_to" / "related-to" for "relates_to" — the natural mis-typing of the
// canonical name — on top of everything model.ParseRelationType accepts, and
// nothing else. The model parser stays canonical so the JSON wire format and
// the workflow `issue.linked.<relation>` vocabulary are unchanged.
func TestParseRelationTypeArg(t *testing.T) {
	tests := []struct {
		input   string
		want    model.RelationType
		wantErr bool
	}{
		{"relates_to", model.RelationRelatesTo, false},
		{"relates-to", model.RelationRelatesTo, false},
		{"related_to", model.RelationRelatesTo, false},
		{"related-to", model.RelationRelatesTo, false},
		{" related_to ", model.RelationRelatesTo, false},
		{"blocks", model.RelationBlocks, false},
		{"depends-on", model.RelationDependsOn, false},
		{"duplicates", model.RelationDuplicates, false},
		{"relate_to", "", true},
		{"invalid", "", true},
	}
	for _, tt := range tests {
		got, err := parseRelationTypeArg(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseRelationTypeArg(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("parseRelationTypeArg(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
