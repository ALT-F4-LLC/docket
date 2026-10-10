package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
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

// TestGuardStopTriageSuspended pins the guard's answer for a step whose
// failure suspended it on its triage panel: the step sits `gated` while the
// panel deliberates, which is a wait on voters rather than work a stop
// interrupts, and it blocks again once the proposal is decided and the
// verdict still has to be applied.
func TestGuardStopTriageSuspended(t *testing.T) {
	t.Run("an open panel proposal does not block", func(t *testing.T) {
		conn, run, _ := triageRun(t, "retry", "abandon-issue")
		requireSuspendedOnPanel(t, conn)
		requireProposalStatus(t, conn, model.ProposalStatusOpen)

		blockers, err := stopBlockers(conn, run.ID, nowMS)
		testsupport.Must(t, err, "stopBlockers: %v", err)
		for _, name := range blockers {
			if strings.HasPrefix(name, "implement@0 ") {
				t.Fatalf("stopBlockers lists %q while its triage panel is still "+
					"deliberating: %v", name, blockers)
			}
		}
	})

	t.Run("a decided panel proposal blocks the still-gated step", func(t *testing.T) {
		conn, _, _ := triageRun(t, "retry", "abandon-issue")
		panel := mustStep(t, conn, "triage@0")
		proposalID, err := findVoteProposal(conn, panel)
		testsupport.Must(t, err, "finding triage@0's proposal: %v", err)
		// Both seats cast, so the proposal finalizes; the tally is not driven,
		// which leaves the verdict unapplied and implement@0 still gated.
		for _, seat := range []string{"seat-a", "seat-b"} {
			_, err := db.CastVote(conn, &model.Vote{
				ProposalID: proposalID, VoterName: seat,
				Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
			})
			testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
		}
		requireSuspendedOnPanel(t, conn)
		requireProposalStatus(t, conn, model.ProposalStatusApproved)

		verdict, err := GuardStop(conn, 0, nowMS)
		testsupport.Must(t, err, "GuardStop: %v", err)
		if verdict.Allowed {
			t.Fatal("guard stop allowed a stop while implement@0 waits on a " +
				"decided panel verdict nobody has applied")
		}
		if !strings.Contains(verdict.Reason, "implement@0") {
			t.Fatalf("the denial does not name the suspended step: %s", verdict.Reason)
		}
	})
}

func requireSuspendedOnPanel(t *testing.T, conn *sql.DB) {
	t.Helper()
	step := mustStep(t, conn, "implement@0")
	if !suspendedOnPanel(step, "triage") {
		t.Fatalf("implement@0 = %q routing %q; the fixture did not suspend it "+
			"on its triage panel", step.Status, step.Routing)
	}
}

func requireProposalStatus(t *testing.T, conn *sql.DB, want model.ProposalStatus) {
	t.Helper()
	proposalID, err := findVoteProposal(conn, mustStep(t, conn, "triage@0"))
	testsupport.Must(t, err, "finding triage@0's proposal: %v", err)
	proposal, err := db.GetProposal(conn, proposalID)
	testsupport.Must(t, err, "reading the proposal: %v", err)
	if proposal.Status != want {
		t.Fatalf("triage@0's proposal status = %q, want %q", proposal.Status, want)
	}
}
