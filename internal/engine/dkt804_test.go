package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-804 — `step claim --render` must be atomic with respect to the packet
// render. On RUN-56 the claim committed FIRST and the render refused SECOND
// (an unpinned packet file), which stranded eight steps `claimed` with no
// token ever issued: every recovery verb requires the token nobody received,
// and the only exits were the 1800s TTL or an operator-gated `step reap`.
//
// The fixture reproduces RUN-56's exact shape: a step whose `packet` entry
// substitutes `{executor}`, claimed with a resolved hint whose contract the
// run never pinned.

// TestClaimRenderRefusalLeavesTheStepClaimable is the regression: a
// `claim --render` whose packet render fails validation writes NOTHING — the
// step keeps its pre-claim status, holds no lease, spends no attempt — and
// the very next claim of the same step succeeds.
func TestClaimRenderRefusalLeavesTheStepClaimable(t *testing.T) {
	conn, configDir := configRepo(t)
	writeConfigFile(t, configDir, "workflows/auto-dev.toml",
		autoWorkflowSrc+"packet = [\"contracts/{executor}.md\"]\n")
	// The DECLARED hint's contract exists and is pinned by activation
	// (DKT-581's closure); no other contract is.
	writeConfigFile(t, configDir, "contracts/w.md", "the declared contract\n")

	issue := createIssue(t, conn, "atomic claim", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	stepID := stepIDByInstance(t, conn, "implement@0")

	// The claim, rendered for a RESOLVED hint whose contract is not pinned —
	// the label-resolved-executor path RUN-56 died on.
	_, _, err = NewEngine().ClaimStepRendered(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS,
	}, "", "rogue")
	if err == nil {
		t.Fatal("a claim whose packet references an unpinned file succeeded")
	}
	// The caller sees the SAME refusal it always did: VALIDATION_ERROR naming
	// the exact unpinned path — executor error handling is unchanged.
	if code, _ := CodeOf(err); code != CodeValidation {
		t.Errorf("code = %q, want %q", code, CodeValidation)
	}
	// DKT-818 rewrote this refusal's wording — it now names the pin set and
	// the run rather than "not pinned by this run", and says which of the two
	// unpinned causes applies (here: nothing wrote contracts/rogue.md).
	if !strings.Contains(err.Error(), "is not in "+run.Ref()+"'s pin set") ||
		!strings.Contains(err.Error(), "contracts/rogue.md") {
		t.Errorf("err = %q, want the unpinned-file refusal naming contracts/rogue.md",
			err.Error())
	}

	// The refusal wrote NOTHING: pre-claim status, no lease, no attempt.
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.Status != db.StepPending {
		t.Errorf("status = %q after a refused claim, want %q — this is the "+
			"zombie claim: a lease held that no token can ever end",
			step.Status, db.StepPending)
	}
	if step.Owner != "" || step.TokenHash != "" || step.ExpiresMS != 0 {
		t.Errorf("a refused claim left a lease: owner=%q token_hash set=%v expires=%d",
			step.Owner, step.TokenHash != "", step.ExpiresMS)
	}
	if step.Attempt != 0 {
		t.Errorf("attempt = %d after a refused claim, want 0 — the refusal "+
			"consumed an attempt on its way out", step.Attempt)
	}

	// And the step is STILL CLAIMABLE, immediately — no TTL wait, no reap.
	// The same claim with the declared (pinned) hint succeeds and delivers
	// both halves of the atomic response.
	result, packet, err := NewEngine().ClaimStepRendered(conn, stepID, ClaimOptions{
		Owner: "wave:STEP-1", NowMS: nowMS,
	}, "", "")
	testsupport.Must(t, err, "re-claim after the refusal: %v", err)
	if result.Token == "" {
		t.Error("the successful claim returned no token")
	}
	if result.Attempt != 1 {
		t.Errorf("attempt = %d on the first successful claim, want 1", result.Attempt)
	}
	if packet == nil || !strings.Contains(packet.Packet, "the declared contract") {
		t.Errorf("the packet does not carry the pinned contract: %+v", packet)
	}
}
