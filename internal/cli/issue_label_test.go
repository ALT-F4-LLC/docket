package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// runRefusedJSON runs sub's RunE against conn with --json=v2 and returns the
// *CmdError it refused with.
func runRefusedJSON(t *testing.T, conn *sql.DB, sub *cobra.Command, args ...string) *CmdError {
	t.Helper()
	sub.SetContext(context.WithValue(context.Background(), dbKey, conn))
	testsupport.Must(t, rootCmd.PersistentFlags().Set("json", "v2"), "set --json")
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("json", "")
		rootCmd.PersistentFlags().Lookup("json").Changed = false
	})

	restore := captureStdout(t)
	err := sub.RunE(sub, args)
	restore()
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

func TestIssueLabelCommandsLetTheVerbOwnTheProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{labelAddCmd, labelRmCmd, labelListCmd, labelDeleteCmd} {
		if !verbOwnsProject(cmd) {
			t.Errorf("issue label %s is not annotated verbOwnsProject", cmd.Name())
		}
	}
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

	ce := runRefusedJSON(t, conn, labelDeleteCmd, "alpha")
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

	ce := runRefusedJSON(t, conn, labelDeleteCmd, "ghost")
	if ce.Code != "NOT_FOUND" || ce.Error() != `label "ghost" not found` {
		t.Errorf("refusal = %s %q, want NOT_FOUND %q", ce.Code, ce.Error(), `label "ghost" not found`)
	}
}
