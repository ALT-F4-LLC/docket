package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-101: a completion that files gaps BESIDE real content records the gap
// artifacts after the declared emit, so "the step's latest artifact" was a gap
// — whose payload is empty — and the threshold evaluated over the empty set.
// Every `any()` arm was false, the step routed `pass`, and the interposed
// escalation gate was skipped over a payload that named exactly the condition
// it existed to catch (RUN-14's verify-tribunal, skipped over an
// "unverifiable" AC). The threshold must aggregate over the DECLARED emit's
// payload whatever rode along beside it.

// TestThresholdReadsDeclaredEmitPastGapArtifacts is the defect's exact shape:
// a matching payload row plus a gap must route to the interposed target.
func TestThresholdReadsDeclaredEmitPastGapArtifacts(t *testing.T) {
	conn := mustDB(t)
	activateInterposed(t, conn, interposeHumanSrc)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "verify@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("one finding is blocked"),
		Payload:  []byte(`[{"status":"blocked"}]`),
		Gaps: [][]byte{[]byte(
			"# Out-of-scope residue\n\nFiled beside the real report.")},
		NowMS: nowMS,
	})
	testsupport.Must(t, err, "complete with payload and gap: %v", err)

	if got := stepRouting(t, conn, "verify@0"); got != "tribunal" {
		t.Fatalf("verify@0 routing = %q, want the interposed step name — the "+
			"threshold read the gap artifact's empty payload instead of the "+
			"declared emit's", got)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepPending {
		t.Errorf("tribunal@0 = %q after its routing named it, want pending — "+
			"the escalation gate was skipped over a matching payload", got)
	}
}

// TestThresholdPassBesideGapsStillPasses is the other direction: a payload
// matching no arm routes `pass` with gaps beside it, exactly as without them.
func TestThresholdPassBesideGapsStillPasses(t *testing.T) {
	conn := mustDB(t)
	activateInterposed(t, conn, interposeHumanSrc)
	e := testEngine()

	stepID := stepIDByInstance(t, conn, "verify@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	err = e.CompleteStep(conn, stepID, CompleteOptions{
		Token:    claim.Token,
		Artifact: []byte("all clear"),
		Payload:  []byte(`[{"status":"ok"}]`),
		Gaps:     [][]byte{[]byte("# Residue\n\nUnrelated to the verdict.")},
		NowMS:    nowMS,
	})
	testsupport.Must(t, err, "complete: %v", err)

	if got := stepRouting(t, conn, "verify@0"); got != "pass" {
		t.Errorf("verify@0 routing = %q, want pass", got)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepSkipped {
		t.Errorf("tribunal@0 = %q after a pass routing, want %q", got, db.StepSkipped)
	}
}
