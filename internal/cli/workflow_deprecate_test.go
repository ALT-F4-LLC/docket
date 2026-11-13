package cli

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestWorkflowDeprecateOwnsItsProject(t *testing.T) {
	if _, ok := workflowDeprecateCmd.Annotations["verbOwnsProject"]; !ok {
		t.Error("workflow deprecate lacks the verbOwnsProject annotation")
	}
}

// The verb retires the version in the configured project and leaves the same
// name@version in another project binding.
func TestWorkflowDeprecateRetiresInTheConfiguredProject(t *testing.T) {
	conn := newTestDB(t)
	cmd := fanoutCmd(conn, "", false)
	caller := inCallerProject(t, conn, cmd, "/repo/caller")
	auditWorkflowRow(t, conn, caller, "release", 7)
	auditWorkflowRow(t, conn, db.DefaultProjectID, "release", 7)

	w, _ := bufWriter(true)
	testsupport.Must(t, runWorkflowDeprecate(cmd, []string{"release@7"}, w), "deprecate")

	retired, err := db.GetWorkflow(conn, caller, "release", 7)
	testsupport.Must(t, err, "reading the caller's row: %v", err)
	if !retired.Deprecated() {
		t.Error("release@7 is still binding in the caller's project")
	}
	untouched, err := db.GetWorkflow(conn, db.DefaultProjectID, "release", 7)
	testsupport.Must(t, err, "reading the default project's row: %v", err)
	if untouched.Deprecated() {
		t.Error("release@7 was retired in the default project, not the configured one")
	}
}
