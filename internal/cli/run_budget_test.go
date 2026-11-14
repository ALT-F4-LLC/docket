package cli

import "testing"

// TestRunBudgetOwnsItsProject pins `run budget` to the verb layer: the root
// hook leaves project resolution to internal/verb.
func TestRunBudgetOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(runBudgetCmd) {
		t.Error("`run budget` lacks the verbOwnsProject annotation")
	}
}
