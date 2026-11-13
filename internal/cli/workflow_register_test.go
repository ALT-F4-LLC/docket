package cli

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestWorkflowRegisterOwnsItsProject(t *testing.T) {
	if _, ok := workflowRegisterCmd.Annotations["verbOwnsProject"]; !ok {
		t.Error("workflow register lacks the verbOwnsProject annotation")
	}
}

// The verb, not the root hook, decides which registry the definition lands
// in: the command carries only the config's identity.
func TestWorkflowRegisterWritesTheConfiguredProject(t *testing.T) {
	conn := newTestDB(t)
	cmd := workflowRegisterCmdWithDB(conn)
	caller := inCallerProject(t, conn, cmd, "/repo/caller")

	w, _ := bufWriter(true)
	testsupport.Must(t, runWorkflowRegister(cmd, []string{writeWorkflowFile(t, minimalWorkflow)}, w),
		"register")

	if _, err := db.GetWorkflow(conn, caller, "unit", 1); err != nil {
		t.Errorf("unit@1 is not in the caller's project %d: %v", caller, err)
	}
	if _, err := db.GetWorkflow(conn, db.DefaultProjectID, "unit", 1); err == nil {
		t.Error("unit@1 landed in the default project, not the configured one")
	}
}
