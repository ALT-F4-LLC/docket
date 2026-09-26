package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1845: `docket guard stop` denied after the only run in the project was
// paused, even though its steps never left `pending` — `run pause` moves the
// run's status, not any step's, and the guard's own `--help` promises a
// waiting-human run is not something a stop interferes with. See guard.go's
// GuardStop doc for the exemption this pins.

// TestGuardStopAllowsAPausedRunWithPendingSteps is the repro from the report:
// an active run is dispatched (so its pending steps are eligible to block at
// all, DKT-71), then paused with pending steps still on the board. The guard
// must allow, because nothing is running for a stop to interrupt — the run is
// parked on a person.
func TestGuardStopAllowsAPausedRunWithPendingSteps(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	markDispatched(t, conn, run.ID)

	_, _, err := MoveRun(conn, run.ID, "pause", model.RunWaitingHuman,
		[]model.RunStatus{model.RunActive}, "operator stepped away", nowMS)
	testsupport.Must(t, err, "pausing the run: %v", err)

	verdict, err := GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop: %v", err)
	if !verdict.Allowed {
		t.Fatalf("guard stop denied a paused run whose steps are still "+
			"pending: %s — a waiting-human RUN must exempt its pending steps "+
			"exactly as a waiting-human STEP already does", verdict.Reason)
	}
}

// TestGuardStopStillDeniesAnActiveRunWithPendingSteps is the control: an
// otherwise-identical dispatched run that was never paused still denies. This
// is what fails if the fix is over-broad (e.g. exempting `waiting-human` runs
// entirely at the outer query) instead of narrowly exempting only the
// `pending` steps of a paused run.
func TestGuardStopStillDeniesAnActiveRunWithPendingSteps(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	markDispatched(t, conn, run.ID)

	verdict, err := GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop: %v", err)
	if verdict.Allowed {
		t.Fatal("guard stop allowed an ACTIVE run with pending steps; the " +
			"fixture never paused it, so this is unrelated regression, not " +
			"the DKT-1845 exemption")
	}
}

// TestGuardStopStillDeniesAPausedRunWithAClaimedStep locks the boundary the
// fix must not cross: `run pause` honors in-flight completes rather than
// killing them (run_lifecycle.go's runPauseCmd), so a claimed/running step is
// a live worker a stop can still interrupt. Only `pending` steps are exempted
// by a paused run — claimed/running steps must keep blocking regardless of
// run status.
func TestGuardStopStillDeniesAPausedRunWithAClaimedStep(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	markDispatched(t, conn, run.ID)
	claimInstance(t, conn, "implement@0", nowMS)
	_ = issue

	_, _, err := MoveRun(conn, run.ID, "pause", model.RunWaitingHuman,
		[]model.RunStatus{model.RunActive}, "operator stepped away", nowMS)
	testsupport.Must(t, err, "pausing the run: %v", err)

	verdict, err := GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop: %v", err)
	if verdict.Allowed {
		t.Fatal("guard stop allowed a paused run with a CLAIMED step; " +
			"`run pause` honors in-flight completes rather than killing them, " +
			"so a live worker still blocks a stop even while the run is parked")
	}
}
