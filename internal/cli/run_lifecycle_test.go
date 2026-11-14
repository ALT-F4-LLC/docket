package cli

import (
	"testing"

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
