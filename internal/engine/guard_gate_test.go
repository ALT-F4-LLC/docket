package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `docket guard gate --step NAME` over BOTH gate kinds.
//
// The predicate was `type="human"` alone, which meant converting a workflow
// gate from `human` to `vote` — the same question, asked of several voters
// instead of one — silently stopped matching. Every hook checking that gate
// started denying with "no such step" while the gate itself sat approved, and
// nothing in the denial hinted that the gate had been found and rejected as
// the wrong kind.
//
// A tallied pass IS a decision, so it counts. Nothing else loosened: an
// unfinished vote reads `pending` here and denies exactly as an unapproved
// human gate does.

// gateKind retypes the fixture's declared human gate, which is the conversion
// the regression is about — one step name, two kinds across a definition edit.
func gateKind(t *testing.T, conn *sql.DB, kind string) {
	t.Helper()
	execSQL(t, conn, `UPDATE steps SET kind = ? WHERE step_name = 'commit-gate'`, kind)
}

// passGate puts the gate in the state a decision produces: `done` with a
// `pass` routing, which is what `step approve` writes on a human gate and what
// a tallied approval writes on a vote gate.
func passGate(t *testing.T, conn *sql.DB) {
	t.Helper()
	execSQL(t, conn,
		`UPDATE steps SET status = ?, routing = ? WHERE step_name = 'commit-gate'`,
		db.StepDone, RoutingPass)
}

func TestGuardGateAcceptsBothGateKinds(t *testing.T) {
	for _, kind := range []string{workflow.TypeHuman, workflow.TypeVote} {
		t.Run(kind, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)
			gateKind(t, conn, kind)

			// Undecided: the gate is found and DENIED, naming its state.
			verdict, err := GuardGate(conn, "commit-gate", 0, db.DefaultProjectID)
			testsupport.Must(t, err, "GuardGate: %v", err)
			if verdict.Allowed {
				t.Fatalf("an undecided %s gate was allowed", kind)
			}
			if !strings.Contains(verdict.Reason, "not approved") {
				t.Errorf("denial does not say the gate is undecided: %s", verdict.Reason)
			}

			// Decided: allowed.
			passGate(t, conn)
			verdict, err = GuardGate(conn, "commit-gate", 0, db.DefaultProjectID)
			testsupport.Must(t, err, "GuardGate: %v", err)
			if !verdict.Allowed {
				t.Fatalf("a passed %s gate was denied: %s", kind, verdict.Reason)
			}
		})
	}
}

// TestGuardGateStillRefusesAnUndecidedRoute is the strictness the widened
// filter must not have cost: `done` reached by any route OTHER than a pass is
// not a decision, whichever kind the gate is.
func TestGuardGateStillRefusesAnUndecidedRoute(t *testing.T) {
	for _, kind := range []string{workflow.TypeHuman, workflow.TypeVote} {
		t.Run(kind, func(t *testing.T) {
			conn := mustDB(t)
			activatedRun(t, conn)
			gateKind(t, conn, kind)

			execSQL(t, conn,
				`UPDATE steps SET status = ?, routing = ? WHERE step_name = 'commit-gate'`,
				db.StepDone, workflow.OnFailSkip)

			verdict, err := GuardGate(conn, "commit-gate", 0, db.DefaultProjectID)
			testsupport.Must(t, err, "GuardGate: %v", err)
			if verdict.Allowed {
				t.Errorf("a %s gate that reached done by skipping was read as "+
					"approved; an override must not stand in for a decision "+
					"nobody made", kind)
			}
		})
	}
}

// TestGuardGateNamesBothKindsWhenAbsent: the miss message must describe what
// was searched for, or an operator whose gate is a vote step reads a refusal
// naming only human gates and concludes the wrong thing.
func TestGuardGateNamesBothKindsWhenAbsent(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	verdict, err := GuardGate(conn, "no-such-gate", 0, db.DefaultProjectID)
	testsupport.Must(t, err, "GuardGate: %v", err)
	if verdict.Allowed {
		t.Fatal("a gate that does not exist was allowed")
	}
	for _, kind := range []string{workflow.TypeHuman, workflow.TypeVote} {
		if !strings.Contains(verdict.Reason, kind) {
			t.Errorf("the miss message does not name %q: %s", kind, verdict.Reason)
		}
	}
}

// ---------------------------------------------------------------------------
// `--run RUN-N`: one run's gate, not the project's
// ---------------------------------------------------------------------------
//
// The unscoped guard answers over every active run in the project, so one
// run's approval opens the gate for every caller in the project — a session
// working under run B commits on run A's approval. That is the intended
// reading for a caller with no run context, and the wrong one for a caller
// that has a run. The scoped form is how such a caller asks about its own.

// secondRun activates another run over the already-registered fixture, so a
// test has two same-named gates in one project to tell apart.
func secondRun(t *testing.T, conn *sql.DB) *model.Run {
	t.Helper()
	issue := createIssue(t, conn, "second", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate the second run: %v", err)
	return run
}

// passGateOf approves ONE run's gate. passGate above matches by name alone and
// so approves every run's, which is exactly the ambiguity the scoped form
// exists to resolve.
func passGateOf(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()
	execSQL(t, conn,
		`UPDATE steps SET status = ?, routing = ?
		  WHERE step_name = 'commit-gate' AND run_id = ?`,
		db.StepDone, RoutingPass, runID)
}

// TestGuardGateScopedToARunAnswersForThatRunAlone: run A's approval opens the
// gate for the unscoped form, and so for a caller working under run B whose
// own gate is undecided. A query scoped to run B must deny, and one scoped to
// run A must still allow.
func TestGuardGateScopedToARunAnswersForThatRunAlone(t *testing.T) {
	conn := mustDB(t)
	runA, _ := activatedRun(t, conn)
	runB := secondRun(t, conn)
	passGateOf(t, conn, runA.ID)

	verdict, err := GuardGate(conn, "commit-gate", 0, db.DefaultProjectID)
	testsupport.Must(t, err, "GuardGate unscoped: %v", err)
	if !verdict.Allowed {
		t.Fatalf("the unscoped form denied with run A approved: %s", verdict.Reason)
	}

	verdict, err = GuardGate(conn, "commit-gate", runA.ID, db.DefaultProjectID)
	testsupport.Must(t, err, "GuardGate scoped to run A: %v", err)
	if !verdict.Allowed {
		t.Fatalf("run A's own approval was denied when scoped to it: %s", verdict.Reason)
	}

	verdict, err = GuardGate(conn, "commit-gate", runB.ID, db.DefaultProjectID)
	testsupport.Must(t, err, "GuardGate scoped to run B: %v", err)
	if verdict.Allowed {
		t.Fatal("run A's approval answered for run B's undecided gate")
	}
	for _, want := range []string{"not approved", model.FormatRunID(runB.ID)} {
		if !strings.Contains(verdict.Reason, want) {
			t.Errorf("the scoped denial does not say %q: %s", want, verdict.Reason)
		}
	}
}

// TestGuardGateScopedMissNamesTheRun: a gate absent from the named run is
// reported as absent from THAT RUN, not from "any active run" — the denial
// should say which question was asked.
func TestGuardGateScopedMissNamesTheRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	verdict, err := GuardGate(conn, "no-such-gate", run.ID, db.DefaultProjectID)
	testsupport.Must(t, err, "GuardGate: %v", err)
	if verdict.Allowed {
		t.Fatal("a gate that does not exist was allowed")
	}
	if !strings.Contains(verdict.Reason, model.FormatRunID(run.ID)) {
		t.Errorf("the miss does not name the run searched: %s", verdict.Reason)
	}
	if strings.Contains(verdict.Reason, "any active run") {
		t.Errorf("the scoped miss claims every run was searched: %s", verdict.Reason)
	}
}

// TestGuardGateRefusesAnUnknownRun: a named run that does not exist is a
// REFUSAL, not a verdict. Exit 0 would let a typo in a hook read as
// permission, and a plain denial would hide the typo behind a gate that reads
// as merely unapproved.
func TestGuardGateRefusesAnUnknownRun(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)

	_, err := GuardGate(conn, "commit-gate", 999, db.DefaultProjectID)
	if err == nil {
		t.Fatal("a guard on a nonexistent run answered; a typo must not read as a verdict")
	}
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("code = %v, want NOT_FOUND: %v", code, err)
	}
}

// TestGuardGateDeniesAFinishedRun: a finished run's approval authorized work
// that is over. The unscoped form never sees a terminal run, and the scoped
// form must not become the way around that.
func TestGuardGateDeniesAFinishedRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	passGateOf(t, conn, run.ID)
	execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`, model.RunDone, run.ID)

	verdict, err := GuardGate(conn, "commit-gate", run.ID, db.DefaultProjectID)
	testsupport.Must(t, err, "GuardGate: %v", err)
	if verdict.Allowed {
		t.Fatal("a finished run's approval opened the gate when scoped to it")
	}
	for _, want := range []string{model.FormatRunID(run.ID), string(model.RunDone)} {
		if !strings.Contains(verdict.Reason, want) {
			t.Errorf("the denial does not say %q: %s", want, verdict.Reason)
		}
	}
}
