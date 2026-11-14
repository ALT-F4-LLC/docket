package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestIssueReadAndDeleteCommandsOwnTheirProject pins `issue delete`, `issue
// show` and `issue list` to the verb layer: the root hook leaves project
// resolution to internal/verb for each.
func TestIssueReadAndDeleteCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{deleteCmd, showCmd, listCmd} {
		if _, ok := cmd.Annotations["verbOwnsProject"]; !ok {
			t.Errorf("`issue %s` lacks the verbOwnsProject annotation", cmd.Name())
		}
	}
}

// createThroughRoot runs `docket issue create` with args and returns the new
// issue's id.
func createThroughRoot(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runRootVerb(t, createCmd, append([]string{"issue", "create", "--json"}, args...)...)
	testsupport.Must(t, err, "docket issue create %v: %v", args, err)
	var env struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Data.ID == "" {
		t.Fatalf("issue create printed no id (%v):\n%s", err, out)
	}
	return env.Data.ID
}

// TestIssueDeleteThroughTheRoot keeps the command's refusals and reports
// unchanged on the verb path: a parent with sub-issues is refused under --json
// until the caller chooses, and each delete names what it removed.
func TestIssueDeleteThroughTheRoot(t *testing.T) {
	clientConfigSandbox(t)
	_, err := runRootVerb(t, initCmd, "init")
	testsupport.Must(t, err, "docket init: %v", err)

	parent := createThroughRoot(t, "--title", "parent")
	child := createThroughRoot(t, "--title", "child", "--parent", parent)

	_, err = runRootVerb(t, deleteCmd, "issue", "delete", parent, "--json")
	var ce *CmdError
	if !errors.As(err, &ce) || ce.Code != output.ErrValidation {
		t.Fatalf("issue delete %s --json: error = %v, want a %s CmdError", parent, err, output.ErrValidation)
	}
	want := "issue " + parent + " has 1 sub-issue(s): use --force to cascade-delete or --orphan to make them root issues"
	if err.Error() != want {
		t.Errorf("refusal = %q, want %q", err.Error(), want)
	}

	_, err = runRootVerb(t, deleteCmd, "issue", "delete", parent, "--force", "--orphan")
	if err == nil || err.Error() != "--force and --orphan are mutually exclusive" {
		t.Errorf("--force --orphan: error = %v, want the mutual-exclusion refusal", err)
	}

	out, err := runRootVerb(t, deleteCmd, "issue", "delete", parent, "--orphan")
	testsupport.Must(t, err, "issue delete %s --orphan: %v", parent, err)
	if want := "Deleted " + parent + ": parent (orphaned 1 sub-issue(s))"; !strings.Contains(out, want) {
		t.Errorf("orphan delete printed %q, want it to contain %q", out, want)
	}

	out, err = runRootVerb(t, deleteCmd, "issue", "delete", child, "--json")
	testsupport.Must(t, err, "issue delete %s --json: %v", child, err)
	var env struct {
		Data deleteResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if env.Data.ID != child {
		t.Errorf("deleted id = %q, want %q", env.Data.ID, child)
	}

	_, err = runRootVerb(t, deleteCmd, "issue", "delete", child)
	if !errors.As(err, &ce) || ce.Code != output.ErrNotFound || err.Error() != "issue "+child+" not found" {
		t.Errorf("deleting %s twice: error = %v, want NOT_FOUND naming it", child, err)
	}
}
