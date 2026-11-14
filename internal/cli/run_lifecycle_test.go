package cli

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/config"
	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/spf13/cobra"
)

// TestRunLifecycleCommandsOwnTheirProject pins `run pause`, `run resume` and
// `run abandon` to the verb layer: the root hook leaves project resolution to
// internal/verb for each.
func TestRunLifecycleCommandsOwnTheirProject(t *testing.T) {
	for _, cmd := range []*cobra.Command{runPauseCmd, runResumeCmd, runAbandonCmd} {
		if !verbOwnsProject(cmd) {
			t.Errorf("`run %s` lacks the verbOwnsProject annotation", cmd.Name())
		}
	}
}

// The root hook resolves no project for these commands, so each command's own
// RunE refuses an unanchored caller, before any flag check, and moves nothing.
func TestRunLifecycleCommandsRefuseAnUnanchoredCaller(t *testing.T) {
	conn := newTestDB(t)
	run := activeRunIn(t, conn)
	unanchored := &config.Config{Identity: "/work/scratch"}

	for _, cmd := range []*cobra.Command{runPauseCmd, runResumeCmd, runAbandonCmd} {
		t.Run(cmd.Name(), func(t *testing.T) {
			_, err := runLeaseCommandOutput(t, conn, unanchored, cmd, run.Ref())
			requireCmdError(t, err, output.ErrValidation, "cannot tell which project this belongs to")
		})
	}
	stored, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if stored.Status != model.RunActive {
		t.Errorf("run status = %s after refused moves, want active", stored.Status)
	}
}
