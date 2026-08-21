package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Project scoping for the store-wide-read guards (DKT-32).
//
// Under the shared per-user store, "ANY active run" silently became "any run
// on the MACHINE": a Stop hook firing in one repository was denied over
// another repository's run (observed 2026-08-11, RUN-2 — a session standing
// in a run-less project could not end its turn). The guards now answer for
// ONE project by default, 0 meaning every project — the same contract as
// RunListOptions.ProjectID, exercised here with two projects in one store.

// secondProject registers a project DISTINCT from the fixtures' default one.
//
// The first non-empty identity to touch a fresh store CLAIMS the unclaimed
// default row (EnsureProject's ladder, step 2) — so the run's own identity is
// bound to project 1 first, and only then does a second identity insert a
// genuinely new row.
func secondProject(t *testing.T, conn *sql.DB) int {
	t.Helper()
	p1, err := db.EnsureProject(conn, "/home/run.git", "run.git", nowMS)
	testsupport.Must(t, err, "claiming the default project: %v", err)
	if p1 != db.DefaultProjectID {
		t.Fatalf("the run's identity claimed project %d, want the default %d",
			p1, db.DefaultProjectID)
	}
	p2, err := db.EnsureProject(conn, "/elsewhere/other.git", "other.git", nowMS)
	testsupport.Must(t, err, "creating the second project: %v", err)
	if p2 == db.DefaultProjectID {
		t.Fatalf("the second identity resolved to the default project; the "+
			"fixture needs a distinct one (got %d)", p2)
	}
	return p2
}

func TestGuardStopScopesToTheProject(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn) // pending steps land in the default project
	// The subject here is PROJECT SCOPING, not dispatch: the run has to be one
	// the machine has started for its pending steps to block at all (DKT-71).
	markDispatched(t, conn, run.ID)

	p2 := secondProject(t, conn)

	// The run-less project's question: nothing of ITS is working — allow.
	// This is the wedge repro: before scoping, this call denied.
	verdict, err := GuardStop(conn, p2, nowMS)
	testsupport.Must(t, err, "GuardStop(other project): %v", err)
	if !verdict.Allowed {
		t.Fatalf("a project with no runs was denied over another project's "+
			"pending work: %s", verdict.Reason)
	}

	// The run's own project still denies — scoping must not weaken the guard
	// where the work actually is.
	verdict, err = GuardStop(conn, db.DefaultProjectID, nowMS)
	testsupport.Must(t, err, "GuardStop(run's project): %v", err)
	if verdict.Allowed {
		t.Fatal("the run's own project was allowed while its steps are pending")
	}

	// 0 asks the old store-wide question, for --all-projects.
	verdict, err = GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop(store-wide): %v", err)
	if verdict.Allowed {
		t.Fatal("the store-wide question was allowed while a run has pending steps")
	}
}

// TestGuardStopAllowsANeverDispatchedRun is DKT-71.
//
// `bootstrap`'s contractual terminal state is an activated run that has NOT
// been dispatched: it wires a repository up and stops, leaving the decision to
// run anything to the operator. The stop guard counted that run's pending steps
// as machine work in flight and denied the turn-end, naming a step nothing had
// ever started. Measured in all six successful bootstraps on one day, twice
// pushing the operator into starting work nobody had asked for.
//
// The guard's own header states the predicate is "whether the MACHINE is still
// working". With no dispatch, it is not.
func TestGuardStopAllowsANeverDispatchedRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	verdict, err := GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop: %v", err)
	if !verdict.Allowed {
		t.Fatalf("a never-dispatched run denied a stop: %s\nnothing has been "+
			"handed to the machine, so there is nothing for a stop to interrupt",
			verdict.Reason)
	}

	// The exemption ends at the dispatch — the SAME run, once started, blocks
	// exactly as it always did. Without this half the change would read as
	// "pending steps never block", which is not what DKT-71 asked for.
	markDispatched(t, conn, run.ID)

	verdict, err = GuardStop(conn, 0, nowMS)
	testsupport.Must(t, err, "GuardStop after dispatch: %v", err)
	if verdict.Allowed {
		t.Fatal("a dispatched run with pending steps allowed a stop; the " +
			"exemption is for runs the machine never started, not for pending " +
			"steps in general")
	}
}

func TestGuardRecordWithoutRunScopesToTheProject(t *testing.T) {
	conn := mustDB(t)
	first := dispatchRun(t, conn) // reconciled, default project
	openDispatch(t, conn, first, 0, nowMS)

	p2 := secondProject(t, conn)

	// No --run, scoped to the run-less project: nothing of its is
	// unreconciled — allow.
	verdict, err := GuardRecord(conn, 0, p2, nowMS)
	testsupport.Must(t, err, "GuardRecord(other project): %v", err)
	if !verdict.Allowed {
		t.Fatalf("a project with no runs was denied over another project's "+
			"open dispatch: %s", verdict.Reason)
	}

	// No --run, the dispatch's own project: denied.
	verdict, err = GuardRecord(conn, 0, db.DefaultProjectID, nowMS)
	testsupport.Must(t, err, "GuardRecord(run's project): %v", err)
	if verdict.Allowed {
		t.Fatal("the dispatch's own project was allowed while it is unreconciled")
	}

	// 0 keeps the old store-wide answer, for --all-projects.
	verdict, err = GuardRecord(conn, 0, 0, nowMS)
	testsupport.Must(t, err, "GuardRecord(store-wide): %v", err)
	if verdict.Allowed {
		t.Fatal("the store-wide question was allowed while a dispatch is open")
	}

	// An EXPLICIT --run is honored regardless of the caller's project:
	// naming a run is naming intent.
	verdict, err = GuardRecord(conn, first, p2, nowMS)
	testsupport.Must(t, err, "GuardRecord(explicit run, other project): %v", err)
	if verdict.Allowed {
		t.Fatal("an explicitly named unreconciled run was allowed because the " +
			"caller stood in another project")
	}
}
