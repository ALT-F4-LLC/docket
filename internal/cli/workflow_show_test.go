package cli

import (
	"strings"
	"testing"
)

func TestWorkflowShowOwnsItsProject(t *testing.T) {
	if _, ok := workflowShowCmd.Annotations["verbOwnsProject"]; !ok {
		t.Error("workflow show lacks the verbOwnsProject annotation")
	}
}

// The verb, not the root hook, decides which registry is read: the command
// carries only the config's identity, and shows that project's row.
func TestWorkflowShowReadsTheConfiguredProject(t *testing.T) {
	conn := newTestDB(t)
	cmd := workflowShowCmdWithDB(conn, false)
	inCallerProject(t, conn, cmd, "/repo/caller")
	w, _ := bufWriter(true)
	if err := runWorkflowRegister(fanoutCmd(conn, "caller", false),
		[]string{writeWorkflowFile(t, minimalWorkflow)}, w); err != nil {
		t.Fatalf("registering unit@1 in the caller's project: %v", err)
	}

	w, buf := bufWriter(true)
	if err := runWorkflowShow(cmd, []string{"unit@1"}, w); err != nil {
		t.Fatalf("show unit@1 in the configured project: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, `"name":"unit"`) {
		t.Errorf("show did not return the configured project's unit@1:\n%s", out)
	}
}
