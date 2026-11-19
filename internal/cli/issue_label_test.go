package cli

import (
	"database/sql"
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

// runRefusedJSON runs sub's RunE against conn with --json=v2, under cfg's
// project identity when cfg is set, and returns the *CmdError it refused
// with.
func runRefusedJSON(t *testing.T, conn *sql.DB, cfg *config.Config, sub *cobra.Command, args ...string) *CmdError {
	t.Helper()
	_, err := runLeaseCommandOutput(t, conn, cfg, sub, args...)
	var ce *CmdError
	if !asCmdError(err, &ce) {
		t.Fatalf("%s %v: err = %v (%T), want a *CmdError", sub.Name(), args, err, err)
	}
	return ce
}

// setForce sets labelDeleteCmd's --force for one test.
func setForce(t *testing.T) {
	t.Helper()
	testsupport.Must(t, labelDeleteCmd.Flags().Set("force", "true"), "set --force")
	t.Cleanup(func() {
		_ = labelDeleteCmd.Flags().Set("force", "false")
		labelDeleteCmd.Flags().Lookup("force").Changed = false
	})
}

// TestIssueLabelRmEchoesTheRemainingLabels: label rm answers with the labels
// still attached.
func TestIssueLabelRmEchoesTheRemainingLabels(t *testing.T) {
	conn, id := seedIssue(t)
	runJSON(t, conn, labelAddCmd, id, "alpha", "beta")

	data := runJSON(t, conn, labelRmCmd, id, "alpha")

	var labels []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &labels); err != nil {
		t.Fatalf("label rm data is not a label list: %v\n%s", err, data)
	}
	if len(labels) != 1 || labels[0].Name != "beta" {
		t.Errorf("label rm echoed %+v, want only beta", labels)
	}
}

func TestIssueLabelListCountsEachLabelsIssues(t *testing.T) {
	conn, id := seedIssue(t)
	runJSON(t, conn, labelAddCmd, id, "alpha")

	data := runJSON(t, conn, labelListCmd)

	var labels []struct {
		Name       string `json:"name"`
		IssueCount int    `json:"issue_count"`
	}
	if err := json.Unmarshal(data, &labels); err != nil {
		t.Fatalf("label list data is not a label list: %v\n%s", err, data)
	}
	if len(labels) != 1 || labels[0].Name != "alpha" || labels[0].IssueCount != 1 {
		t.Errorf("label list = %s, want alpha on one issue", data)
	}
}

// A label still in use is refused in JSON mode without --force, with the
// refusal the CLI has always given there, and deleted with it.
func TestIssueLabelDeleteNeedsForceInJSONModeForALabelInUse(t *testing.T) {
	conn, id := seedIssue(t)
	runJSON(t, conn, labelAddCmd, id, "alpha")

	ce := runRefusedJSON(t, conn, nil, labelDeleteCmd, "alpha")
	want := `cannot delete label "alpha" without --force in JSON mode (attached to 1 issue(s))`
	if ce.Code != "VALIDATION_ERROR" || ce.Error() != want {
		t.Errorf("refusal = %s %q, want VALIDATION_ERROR %q", ce.Code, ce.Error(), want)
	}

	setForce(t)
	data := runJSON(t, conn, labelDeleteCmd, "alpha")
	if string(data) != `{"name":"alpha"}` {
		t.Errorf("label delete data = %s, want {\"name\":\"alpha\"}", data)
	}
	if listed := runJSON(t, conn, labelListCmd); string(listed) != "[]" {
		t.Errorf("label list after the delete = %s, want []", listed)
	}
}

func TestIssueLabelDeleteOfAMissingLabelIsNotFound(t *testing.T) {
	conn, _ := seedIssue(t)

	ce := runRefusedJSON(t, conn, nil, labelDeleteCmd, "ghost")
	if ce.Code != "NOT_FOUND" || ce.Error() != `label "ghost" not found` {
		t.Errorf("refusal = %s %q, want NOT_FOUND %q", ce.Code, ce.Error(), `label "ghost" not found`)
	}
}

// In human mode label add and rm name the issue, a refused delete without a
// terminal explains how to proceed and deletes nothing, and a forced delete
// says how many issues lost the label.
func TestIssueLabelCommandsReportInHumanMode(t *testing.T) {
	conn, ref := seedIssue(t)
	id, err := model.ParseID(ref)
	testsupport.Must(t, err, "ParseID(%s): %v", ref, err)

	for _, tc := range []struct {
		cmd  *cobra.Command
		args []string
		want string
	}{
		{labelAddCmd, []string{ref, "alpha", "beta"}, "Added label(s) to " + ref + ": attach target"},
		{labelRmCmd, []string{ref, "beta"}, "Removed label(s) from " + ref + ": attach target"},
	} {
		out, err := runVoteHuman(t, conn, tc.cmd, tc.args...)
		testsupport.Must(t, err, "label %s %v: %v", tc.cmd.Name(), tc.args, err)
		if !strings.Contains(out, tc.want) {
			t.Errorf("label %s %v printed %q, want it to contain %q", tc.cmd.Name(), tc.args, out, tc.want)
		}
	}

	withStdin(t, "")
	_, err = runVoteHuman(t, conn, labelDeleteCmd, "alpha")
	var ce *CmdError
	want := `non-interactive environment detected; label "alpha" is attached to 1 issue(s): use --force to skip confirmation`
	if !asCmdError(err, &ce) || ce.Code != output.ErrValidation || ce.Error() != want {
		t.Errorf("label delete without a terminal: error = %v, want %s %q", err, output.ErrValidation, want)
	}
	if labels, _ := db.GetIssueLabelObjects(conn, id); len(labels) != 1 {
		t.Fatalf("issue labels after the refused delete = %d, want alpha still attached", len(labels))
	}

	setForce(t)
	out, err := runVoteHuman(t, conn, labelDeleteCmd, "alpha")
	testsupport.Must(t, err, "label delete --force: %v", err)
	if want := `Deleted label "alpha" (removed from 1 issue(s))`; !strings.Contains(out, want) {
		t.Errorf("label delete --force printed %q, want it to contain %q", out, want)
	}
}

// The label writes take their project from the invocation's identity: an
// unanchored caller is refused and nothing is written.
func TestIssueLabelWritesRefuseAnUnanchoredCaller(t *testing.T) {
	conn, ref := seedIssue(t)
	id, err := model.ParseID(ref)
	testsupport.Must(t, err, "ParseID(%s): %v", ref, err)
	_, err = db.EnsureProject(conn, "/repos/first", "first", 1)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	runJSON(t, conn, labelAddCmd, ref, "alpha")
	unanchored := &config.Config{Identity: "/work/scratch"}

	for _, tc := range []struct {
		cmd  *cobra.Command
		args []string
	}{
		{labelAddCmd, []string{ref, "beta"}},
		{labelRmCmd, []string{ref, "alpha"}},
		{labelDeleteCmd, []string{"alpha"}},
	} {
		ce := runRefusedJSON(t, conn, unanchored, tc.cmd, tc.args...)
		if ce.Code != output.ErrValidation || !strings.HasPrefix(ce.Error(), "cannot tell which project this belongs to") {
			t.Errorf("label %s %v from an unanchored caller = %s %q, want the project refusal",
				tc.cmd.Name(), tc.args, ce.Code, ce.Error())
		}
	}
	labels, err := db.GetIssueLabelObjects(conn, id)
	testsupport.Must(t, err, "GetIssueLabelObjects: %v", err)
	if len(labels) != 1 || labels[0].Name != "alpha" {
		t.Errorf("issue labels after unanchored writes = %+v, want only alpha", labels)
	}
}
