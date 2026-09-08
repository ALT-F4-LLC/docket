package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-2132 reported that `guard stop` never blocks on a step whose `step list`
// status is `ready`. The reported sequence — activate a run, never dispatch
// it, ask the guard — allows a stop because of DKT-71's exemption for a run
// nothing has ever happened to, not because `ready` is missing from
// stopBlockers's switch. `ready` is a COMPUTED status (§6.2,
// TestReadyIsNeverPersisted): the column stays `pending`, and the `pending`
// arm asks the scheduler's predicate, so a ready step blocks the moment its
// run has been dispatched. This test pins that by name, so the next reader
// who sees `ready` in `step list` and `allowed` from the guard finds the
// dispatch requirement instead of a missing case.

// TestGuardStopDeniesAReadyStepOnceDispatched proves the step under test is
// ready by the §6.3 predicate — a test that never saw readiness would pass
// against a guard that blocks on every pending step — then shows the same
// step allows a stop before dispatch and denies after it.
func TestGuardStopDeniesAReadyStepOnceDispatched(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		root := stepNamed(t, sched, "implement@0")
		if ready, cond := sched.Ready(root); !ready {
			t.Fatalf("implement@0 is not ready on a freshly activated run: %s", cond)
		}
	})

	verdict, err := GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop before dispatch: %v", err)
	if !verdict.Allowed {
		t.Fatalf("a never-dispatched run denied a stop over its ready step: %s "+
			"— DKT-71 exempts a run nothing has been handed to", verdict.Reason)
	}

	markDispatched(t, conn, run.ID)

	verdict, err = GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop after dispatch: %v", err)
	if verdict.Allowed {
		t.Fatal("guard stop allowed an ACTIVE, dispatched run whose root step " +
			"is ready; the contract names `ready` among the blocking statuses")
	}
	if !strings.Contains(verdict.Reason, "implement@0") {
		t.Fatalf("the denial does not name the ready step: %s", verdict.Reason)
	}
}
