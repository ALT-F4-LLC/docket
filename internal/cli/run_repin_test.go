package cli

import "testing"

// TestRunRepinOwnsItsProject: run repin resolves its project through
// internal/verb, so the root hook must skip its eager resolution.
func TestRunRepinOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(runRepinCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", runRepinCmd.CommandPath())
	}
}
