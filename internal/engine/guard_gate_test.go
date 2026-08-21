package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
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
			verdict, err := GuardGate(conn, "commit-gate", db.DefaultProjectID)
			testsupport.Must(t, err, "GuardGate: %v", err)
			if verdict.Allowed {
				t.Fatalf("an undecided %s gate was allowed", kind)
			}
			if !strings.Contains(verdict.Reason, "not approved") {
				t.Errorf("denial does not say the gate is undecided: %s", verdict.Reason)
			}

			// Decided: allowed.
			passGate(t, conn)
			verdict, err = GuardGate(conn, "commit-gate", db.DefaultProjectID)
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

			verdict, err := GuardGate(conn, "commit-gate", db.DefaultProjectID)
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

	verdict, err := GuardGate(conn, "no-such-gate", db.DefaultProjectID)
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
