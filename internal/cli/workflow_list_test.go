package cli

import (
	"strings"
	"testing"
)

func TestWorkflowListOwnsItsProject(t *testing.T) {
	if _, ok := workflowListCmd.Annotations["verbOwnsProject"]; !ok {
		t.Error("workflow list lacks the verbOwnsProject annotation")
	}
}

// The verb, not the root hook, decides which registry is read: the command
// carries only the config's identity, and lists that project's rows.
func TestWorkflowListReadsTheConfiguredProject(t *testing.T) {
	conn := newTestDB(t)
	cmd := workflowListCmdWithDB(conn, 50)
	inCallerProject(t, conn, cmd, "/repo/caller")
	w, _ := bufWriter(true)
	if err := runWorkflowRegister(fanoutCmd(conn, "caller", false),
		[]string{writeWorkflowFile(t, minimalWorkflow)}, w); err != nil {
		t.Fatalf("registering unit@1 in the caller's project: %v", err)
	}

	out := workflowListJSON(t, cmd)
	if !strings.Contains(out, `"name":"unit"`) || !strings.Contains(out, `"total":1`) {
		t.Errorf("the configured project's listing lacks unit@1:\n%s", out)
	}
}
