package cli

import (
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

func TestIssueLinkCommandsLetTheVerbOwnTheProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{linkAddCmd, linkRemoveCmd, linkListCmd} {
		if !verbOwnsProject(cmd) {
			t.Errorf("issue link %s is not annotated verbOwnsProject", cmd.Name())
		}
	}
}

// TestIssueLinkAddListAndRemoveAnswerAsBefore drives the three commands'
// JSON payloads, the related_to typo courtesy included, through one
// relation's life.
func TestIssueLinkAddListAndRemoveAnswerAsBefore(t *testing.T) {
	conn, source := seedIssue(t)
	targetID, err := db.CreateIssue(conn, &model.Issue{
		Title: "link target", Status: model.StatusBacklog, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)
	target := model.FormatID(targetID)

	var added model.Relation
	data := runJSON(t, conn, linkAddCmd, source, "related-to", target)
	testsupport.Must(t, json.Unmarshal(data, &added), "link add data: %s", data)
	if added.ID == 0 || model.FormatID(added.SourceIssueID) != source ||
		added.TargetIssueID != targetID || added.RelationType != model.RelationRelatesTo {
		t.Errorf("link add = %s, want a stored %s relates_to %s", data, source, target)
	}

	var listed []relationDisplay
	data = runJSON(t, conn, linkListCmd, target)
	testsupport.Must(t, json.Unmarshal(data, &listed), "link list data: %s", data)
	want := relationDisplay{ID: added.ID, RelationType: model.RelationRelatesTo.Inverse(), IssueID: source, Direction: "incoming"}
	if len(listed) != 1 || listed[0] != want {
		t.Errorf("link list %s = %+v, want [%+v]", target, listed, want)
	}

	var removed unlinkResult
	data = runJSON(t, conn, linkRemoveCmd, source, "relates_to", target)
	testsupport.Must(t, json.Unmarshal(data, &removed), "link remove data: %s", data)
	if removed != (unlinkResult{SourceIssueID: source, TargetIssueID: target, RelationType: "relates_to"}) {
		t.Errorf("link remove = %+v, want %s relates_to %s", removed, source, target)
	}

	if data = runJSON(t, conn, linkListCmd, source); string(data) != "[]" {
		t.Errorf("link list after the remove = %s, want []", data)
	}
}

func TestIssueLinkRefusalsKeepTheirCodesAndText(t *testing.T) {
	conn, source := seedIssue(t)

	tests := []struct {
		cmd  *cobra.Command
		args []string
		code string
		msg  string
	}{
		{linkAddCmd, []string{source, "blocks", source}, "VALIDATION_ERROR", "cannot link an issue to itself"},
		{linkAddCmd, []string{source, "blocks", "nope"}, "VALIDATION_ERROR", `invalid target ID: invalid issue ID "nope": strconv.Atoi: parsing "nope": invalid syntax`},
		{linkRemoveCmd, []string{source, "blocks", "DKT-9999"}, "NOT_FOUND", "relation not found"},
		{linkListCmd, []string{"DKT-9999"}, "NOT_FOUND", "issue not found: DKT-9999"},
	}
	for _, tt := range tests {
		ce := runRefusedJSON(t, conn, tt.cmd, tt.args...)
		if string(ce.Code) != tt.code || ce.Error() != tt.msg {
			t.Errorf("%s %v = %s %q, want %s %q", tt.cmd.Name(), tt.args, ce.Code, ce.Error(), tt.code, tt.msg)
		}
	}
}

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
