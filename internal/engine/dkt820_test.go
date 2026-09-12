package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-820 — the RETRY half of DKT-804's zombie claim. On RUN-59 four judge
// claims died on an unpinned packet file, and STEP-2679's executor saw the
// damage from the other side: its retry was refused CONFLICT "step review@2#0
// is not ready to claim: the step is not pending" — blocked by its OWN first
// attempt's lease, a lease no token was ever issued for.
//
// DKT-804's fix moved the render's validation ahead of the lease, so the first
// refusal writes nothing. This test holds the CONSEQUENCE that RUN-59's
// executors actually needed: the retry must hear the SAME diagnosis as the
// first attempt — the packet file the run never pinned — and never a conflict
// about a lease that the refusal itself left behind.

// TestRepeatedRenderRefusalKeepsRefusingForTheSameReason claims the same step
// twice against an UNCHANGED broken config: both refusals are the identical
// VALIDATION_ERROR, and the step is pending, lease-free, and attempt-free after
// each. A CONFLICT on the second claim is the regression.
func TestRepeatedRenderRefusalKeepsRefusingForTheSameReason(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml",
		autoWorkflowSrc+"packet = [\"contracts/{executor}.md\"]\n")
	// Only the DECLARED hint's contract is pinned by activation (DKT-581's
	// closure); the resolved hint's is not, and nothing fixes that between the
	// two claims below.
	writeConfigFile(t, configDir, "contracts/w.md", "the declared contract\n")

	issue := createIssue(t, conn, "the retry sees the same wall", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")

	claim := func() error {
		_, _, err := NewEngine().ClaimStepRendered(conn, stepID, ClaimOptions{
			Owner: "wave:STEP-1", NowMS: nowMS,
		}, "", "rogue")
		return err
	}

	// Attempt one and attempt two, with NOTHING changed in between — RUN-59's
	// STEP-2679, whose executor retried a claim it had never been handed a
	// token for.
	for _, attempt := range []string{"the first claim", "the retry"} {
		err := claim()
		if err == nil {
			t.Fatalf("%s: a claim whose packet references an unpinned file succeeded", attempt)
		}
		// The retry must diagnose the PACKET, not the lease. CONFLICT "the
		// step is not pending" here is the zombie claim reported from the
		// claimant's side.
		if code, _ := CodeOf(err); code != CodeValidation {
			t.Errorf("%s: code = %q, want %q — err = %q", attempt, code, CodeValidation, err.Error())
		}
		if strings.Contains(err.Error(), string(CondStatus)) {
			t.Errorf("%s: err = %q — the claim was refused by the lease its own "+
				"predecessor left behind, not by the packet", attempt, err.Error())
		}
		if !strings.Contains(err.Error(), "is not in "+run.Ref()+"'s pin set") ||
			!strings.Contains(err.Error(), "contracts/rogue.md") {
			t.Errorf("%s: err = %q, want the unpinned-file refusal naming contracts/rogue.md",
				attempt, err.Error())
		}

		// And the step is exactly as it was before the claim: no lease for a
		// token nobody holds, no attempt burned on a claim that delivered
		// nothing.
		step, err := db.GetStep(conn, stepID)
		testsupport.Must(t, err, "GetStep: %v", err)
		if step.Status != db.StepPending {
			t.Errorf("%s: status = %q, want %q", attempt, step.Status, db.StepPending)
		}
		if step.Owner != "" || step.TokenHash != "" || step.ExpiresMS != 0 {
			t.Errorf("%s: a refused claim left a lease: owner=%q token_hash set=%v expires=%d",
				attempt, step.Owner, step.TokenHash != "", step.ExpiresMS)
		}
		if step.Attempt != 0 {
			t.Errorf("%s: attempt = %d, want 0", attempt, step.Attempt)
		}
	}
}
