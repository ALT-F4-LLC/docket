package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
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

// Through the real root in human mode, link add, list and remove name each
// issue under its own project's prefix, list shows both directions under a
// Relations header, and an issue left with no relations says so and how to
// add one.
func TestIssueLinkCommandsReportInHumanModeThroughTheRoot(t *testing.T) {
	clientConfigSandbox(t)
	t.Setenv("NO_COLOR", "1")
	// The root hook leaves the owner resolver on this test's store, where
	// issues read as ABC; a later test must not inherit it.
	t.Cleanup(func() {
		model.SetDisplayPrefix("DKT")
		model.SetIssueOwnerPrefixResolver(nil)
	})
	_, err := runRootVerb(t, initCmd, "init")
	testsupport.Must(t, err, "docket init: %v", err)
	_, err = runRootVerb(t, projectSetPrefixCmd, "project", "set-prefix", "abc")
	testsupport.Must(t, err, "docket project set-prefix: %v", err)
	source := createThroughRoot(t, "--title", "link source")
	target := createThroughRoot(t, "--title", "link target")
	if !strings.HasPrefix(source, "ABC-") || !strings.HasPrefix(target, "ABC-") {
		t.Fatalf("created ids = %q, %q; want them under the project's ABC prefix", source, target)
	}
	// The prefix the process renders by default must not supply the owner's.
	model.SetDisplayPrefix("DKT")

	for _, tc := range []struct {
		cmd  *cobra.Command
		args []string
		want string
	}{
		{linkAddCmd, []string{"add", source, "blocks", target}, "Linked " + source + " blocks " + target + "\n"},
		// The table ends in a newline and the writer adds its own.
		{linkListCmd, []string{"list", source}, "Relations for " + source + ":\n  blocks " + target + "\n\n"},
		{linkListCmd, []string{"list", target}, "Relations for " + target + ":\n  blocked_by " + source + "\n\n"},
		{linkRemoveCmd, []string{"remove", source, "blocks", target}, "Unlinked " + source + " blocks " + target + "\n"},
		{linkListCmd, []string{"list", source}, "No relations found for " + source + "\n" +
			"Add one with: docket issue link add " + source + " <relation> <target>\n"},
	} {
		out, err := runRootVerb(t, tc.cmd, append([]string{"issue", "link"}, tc.args...)...)
		testsupport.Must(t, err, "issue link %v: %v", tc.args, err)
		if out != tc.want {
			t.Errorf("issue link %v printed %q, want %q", tc.args, out, tc.want)
		}
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
		ce := runRefusedJSON(t, conn, nil, tt.cmd, tt.args...)
		if string(ce.Code) != tt.code || ce.Error() != tt.msg {
			t.Errorf("%s %v = %s %q, want %s %q", tt.cmd.Name(), tt.args, ce.Code, ce.Error(), tt.code, tt.msg)
		}
	}
}

// The link writes take their project from the invocation's identity: an
// unanchored caller is refused and no relation changes.
func TestIssueLinkWritesRefuseAnUnanchoredCaller(t *testing.T) {
	conn, source := seedIssue(t)
	targetID, err := db.CreateIssue(conn, &model.Issue{
		Title: "link target", Status: model.StatusBacklog, Kind: model.IssueKindTask,
	}, nil, nil)
	testsupport.Must(t, err, "CreateIssue: %v", err)
	target := model.FormatID(targetID)
	_, err = db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	runJSON(t, conn, linkAddCmd, source, "relates_to", target)
	unanchored := &config.Config{Identity: "/work/scratch"}

	for _, cmd := range []*cobra.Command{linkAddCmd, linkRemoveCmd} {
		args := []string{source, "blocks", target}
		if cmd == linkRemoveCmd {
			args = []string{source, "relates_to", target}
		}
		ce := runRefusedJSON(t, conn, unanchored, cmd, args...)
		if ce.Code != output.ErrValidation || !strings.HasPrefix(ce.Error(), "cannot tell which project this belongs to") {
			t.Errorf("link %s %v from an unanchored caller = %s %q, want the project refusal",
				cmd.Name(), args, ce.Code, ce.Error())
		}
	}
	relations, err := db.GetIssueRelations(conn, targetID)
	testsupport.Must(t, err, "GetIssueRelations: %v", err)
	if len(relations) != 1 || relations[0].RelationType != model.RelationRelatesTo {
		t.Errorf("relations after unanchored writes = %+v, want only the relates_to link", relations)
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
