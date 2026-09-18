package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-726 — the refusal `retry` already made on a MATERIALIZED vote-minted
// hold, made on a plain workflow-declared `type="vote"` step too.
//
// The guard that refuses a parked vote was scoped to `step.Materialized`, so it
// saw the engine-minted `reconcile-held@N#M` clusters and nothing else. A
// workflow's own `security-vote@8`, carrying its own tribunal proposal, fell
// through to the generic retry path: attempt budget and lease reset, step back
// to `pending`, and the next `next` re-read the SAME proposal — the idempotency
// key is (run, issue, instance), so no second ballot opens — announced the
// identical verdict and routed to the identical place. Observed on RUN-51
// STEP-2433 with DKT-V256 rejected 3/3, at the cost of a full
// run-pause/run-resume cycle.
//
// TestRetryRefusesOnADecidedVoteStep is the fix, over BOTH terminal statuses:
// an approved tally is exactly as sticky as a rejected one, and re-reading it
// is exactly as much of a no-op.
func TestRetryRefusesOnADecidedVoteStep(t *testing.T) {
	for _, status := range []model.ProposalStatus{
		model.ProposalStatusRejected,
		model.ProposalStatusApproved,
		// §8.4's manual commit is a decision like any other.
		model.ProposalStatusCommitted,
	} {
		t.Run(string(status), func(t *testing.T) {
			conn := mustDB(t)
			registerVoteRule(t, conn, "majority", "0.6", "medium")
			e := testEngine()

			step, spec := seedVoteStep(t, conn)
			if step.Materialized {
				t.Fatal("premise: this must be a PLAIN workflow-declared vote " +
					"step — the materialized case was already guarded")
			}
			id, err := OpenVoteProposal(conn, step, spec, nowMS)
			testsupport.Must(t, err, "OpenVoteProposal: %v", err)
			_, err = conn.Exec(
				`UPDATE proposals SET status = ? WHERE id = ?`, status, id)
			testsupport.Must(t, err, "deciding the proposal: %v", err)

			err = e.ResolveStep(conn, step.ID, ResolveRetry, "", nowMS)
			if err == nil {
				t.Fatal("retry was accepted on a vote step whose proposal is " +
					"already decided; it re-tallies the same casts and routes " +
					"to the same place, which is the behaviour being removed")
			}
			if code, _ := CodeOf(err); code != CodeValidation {
				t.Errorf("error code = %q, want %q", code, CodeValidation)
			}

			// The refusal must NAME the proposal: an operator told only that a
			// vote decided this cannot check which vote without guessing.
			if want := model.FormatProposalID(id); !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name the proposal %s: %v", want, err)
			}
			if !strings.Contains(err.Error(), string(status)) {
				t.Errorf("the refusal does not name the status %q: %v", status, err)
			}

			// And it must name the verbs that DO move the step — `fix-round`
			// first, because it is the one RUN-51 actually wanted.
			for _, verb := range []string{
				ResolveFixRound, ResolveOverridePass, ResolveSkip, ResolveAbandonIssue,
			} {
				if !strings.Contains(err.Error(), verb) {
					t.Errorf("the refusal does not offer --as %s: %v", verb, err)
				}
			}

			// Nothing moved. A refusal that had already reset the budget would
			// be the same wasted cycle wearing an error message.
			after, err := db.GetStep(conn, step.ID)
			testsupport.Must(t, err, "re-reading the step: %v", err)
			if after.Status != db.StepPending {
				t.Errorf("step status = %q after the refusal, want it "+
					"untouched at %q", after.Status, db.StepPending)
			}
		})
	}
}

// TestRetryStillWorksOnAnUndecidedVoteStep is the other half, and the one the
// narrow condition exists to protect.
//
// R11 offers a resolution on a vote step WHATEVER its status precisely so a run
// is not hostage to a quorum that never arrives. A refusal keyed to "this is a
// vote step" rather than "its proposal is decided" would take the least
// destructive verb away from exactly that case. Both shapes of undecided are
// covered: a ballot open with nobody having cast, and a step whose ballot was
// never opened at all (an interposed vote skipped before phase 2 — DKT-470's
// recovery path).
func TestRetryStillWorksOnAnUndecidedVoteStep(t *testing.T) {
	t.Run("open proposal", func(t *testing.T) {
		conn := mustDB(t)
		registerVoteRule(t, conn, "majority", "0.6", "medium")
		e := testEngine()

		step, spec := seedVoteStep(t, conn)
		id, err := OpenVoteProposal(conn, step, spec, nowMS)
		testsupport.Must(t, err, "OpenVoteProposal: %v", err)

		proposal, err := db.GetProposal(conn, id)
		testsupport.Must(t, err, "GetProposal: %v", err)
		if proposal.Status != model.ProposalStatusOpen {
			t.Fatalf("premise: proposal is %q, want %q",
				proposal.Status, model.ProposalStatusOpen)
		}

		testsupport.Must(t, e.ResolveStep(conn, step.ID, ResolveRetry, "", nowMS),
			"retry was refused on a vote whose proposal is still OPEN, where "+
				"there is no decided tally to re-read: %v", nil)
	})

	t.Run("no proposal opened", func(t *testing.T) {
		conn := mustDB(t)
		registerVoteRule(t, conn, "majority", "0.6", "medium")
		e := testEngine()

		step, _ := seedVoteStep(t, conn)
		proposalID, err := findVoteProposal(conn, step)
		testsupport.Must(t, err, "findVoteProposal: %v", err)
		if proposalID != 0 {
			t.Fatalf("premise: a proposal (%d) already exists", proposalID)
		}

		testsupport.Must(t, e.ResolveStep(conn, step.ID, ResolveRetry, "", nowMS),
			"retry was refused on a vote step that never opened a ballot: %v", nil)
	})
}

// TestReadStepVoteOutcomeMatchesTheSpecKeyedReader pins the refactor DKT-726
// needed: the step-keyed entry point is the SAME read as ReadVoteOutcome, not a
// second one that could drift. `step resolve` reaches it without a pinned spec
// — R11 offers a resolution before the definition is loaded, and a materialized
// step's minted name is never declared — so the two must agree everywhere they
// both apply.
func TestReadStepVoteOutcomeMatchesTheSpecKeyedReader(t *testing.T) {
	for _, status := range []model.ProposalStatus{
		model.ProposalStatusOpen,
		model.ProposalStatusApproved,
		model.ProposalStatusRejected,
		model.ProposalStatusCommitted,
	} {
		t.Run(string(status), func(t *testing.T) {
			conn := mustDB(t)
			registerVoteRule(t, conn, "majority", "0.6", "medium")

			step, spec := seedVoteStep(t, conn)
			id, err := OpenVoteProposal(conn, step, spec, nowMS)
			testsupport.Must(t, err, "OpenVoteProposal: %v", err)
			_, err = conn.Exec(
				`UPDATE proposals SET status = ? WHERE id = ?`, status, id)
			testsupport.Must(t, err, "setting the proposal status: %v", err)

			viaSpec, err := ReadVoteOutcome(conn, step, spec)
			testsupport.Must(t, err, "ReadVoteOutcome: %v", err)
			viaStep, err := ReadStepVoteOutcome(conn, step)
			testsupport.Must(t, err, "ReadStepVoteOutcome: %v", err)

			if viaSpec == nil || viaStep == nil {
				t.Fatalf("one reader reported nothing: spec=%v step=%v", viaSpec, viaStep)
			}
			// Field-by-field: Score is a pointer, and two reads of the same
			// row hand back two pointers to equal values.
			if viaSpec.ProposalID != viaStep.ProposalID ||
				viaSpec.Status != viaStep.Status ||
				viaSpec.Verdict != viaStep.Verdict {
				t.Errorf("the two readers disagree: spec-keyed %+v, "+
					"step-keyed %+v", *viaSpec, *viaStep)
			}
		})
	}
}
