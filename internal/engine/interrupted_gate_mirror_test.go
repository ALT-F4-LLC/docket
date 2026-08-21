package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestInterruptedGateParkEntersReview is DKT-379.
//
// DKT-294's live mirror fires from `reconcileIssueAndRun`, and every park
// EXCEPT this one reaches it. `parkInterruptedGate` writes `waiting-human`
// directly and leaves the saga rather than advancing to the routing stage —
// correct, for the reason its own comment gives (advancing would let the
// resume loop recompute the routing and overwrite the recorded reason) — but
// it also skipped reconciliation, which is a different thing. An issue whose
// only park was an interrupted gate therefore kept reading `in-progress`, and
// the run kept reading `active` with a parked step in it.
func TestInterruptedGateParkEntersReview(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	repoRoot := t.TempDir()

	// A gate the operator has NOT declared re-runnable: interrupted, it parks.
	runner := NewExecRunner(testRepoPaths(repoRoot))
	runner.LoadStore = sandboxTrust(t, fixtureTrustEntries(repoRoot, false)...)
	e := testEngine()
	e.Gates = runner

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	if got := issueStatus(t, conn, issue); got != string(model.StatusInProgress) {
		t.Fatalf("premise: issue = %q after the claim, want %q", got, model.StatusInProgress)
	}

	if err := e.CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), NowMS: nowMS,
	}); err != nil && !strings.Contains(err.Error(), "waiting-human") {
		t.Fatalf("complete: %v", err)
	}

	// The started-but-unrecorded window, reconstructed: `build`'s result is
	// gone, its `gate-started` event survives, and the saga is rewound to the
	// stage in which `build` is the gate about to run.
	_, err = conn.Exec(`DELETE FROM gate_results WHERE step_id = ? AND gate = ?`,
		stepID, "build")
	testsupport.Must(t, err, "reconstructing the crash window: %v", err)
	_, err = conn.Exec(
		`UPDATE steps SET saga_stage = ?, status = 'gated', routing = NULL
		  WHERE id = ?`, db.SagaRecorded, stepID)
	testsupport.Must(t, err, "rewinding the saga stage: %v", err)

	testsupport.Must(t, e.ResumeSaga(conn, stepID, nowMS), "resuming: %v", err)

	if got := stepStatus(t, conn, "implement@0"); got != db.StepWaitingHuman {
		t.Fatalf("premise: implement@0 = %q, want the interrupted-gate park", got)
	}
	if got := issueStatus(t, conn, issue); got != string(model.StatusReview) {
		t.Errorf("issue = %q with its only step parked by an interrupted gate, "+
			"want %q — the mirror reports that the issue is blocked on a human "+
			"decision, and how it got there is not one of its inputs",
			got, model.StatusReview)
	}

	// The run rollup had the same hole: a run with a parked step is waiting,
	// not active.
	updated, err := db.GetRun(conn, run.ID)
	testsupport.Must(t, err, "GetRun: %v", err)
	if updated.Status != model.RunWaitingHuman {
		t.Errorf("run = %q, want %q", updated.Status, model.RunWaitingHuman)
	}

	// And the park is narrated, like every other transition into review.
	bodies := engineCommentBodies(t, conn, issue)
	if !containsBody(bodies, "implement@0 is awaiting review") {
		t.Errorf("engine comments = %v, want one narrating the park", bodies)
	}
}
