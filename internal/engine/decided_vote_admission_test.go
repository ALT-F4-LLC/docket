package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-468 — the two staleness instances at the code level.
//
// Instance 2's mechanism: phases 4/5 of the vote lifecycle (read a decided
// outcome, route on it) were gated behind the FULL admission predicate, so a
// quorum reached while any admission clause failed — budget wall, headroom
// hold, scope — sat unrouted at every driving opportunity, and `step show`
// went on reporting the children `pending` and the holding aggregate `gated`
// while `vote show` already showed the proposals decided. Instance 1's
// mechanism: `run status` rolled up the RAW status column while `step show`
// computed the effective one, so the same lapsed-lease claim read `claimed`
// on one surface and `ready` on the other at the same moment.

// statusCountOf reads one status's count out of a rollup, 0 when absent.
func statusCountOf(counts []model.StatusCount, status string) int {
	for _, sc := range counts {
		if sc.Status == status {
			return sc.Count
		}
	}
	return 0
}

// TestDecidedHeldVoteRoutesPastBudgetHold reproduces instance 2: the tally
// decides while an admission clause (here the measured usage cap) denies the
// vote steps' readiness, and the deciding cast's own hook must still route
// them — a decided tally spawns nothing and spends nothing, so the clauses
// that guard executor work may not defer its bookkeeping.
//
// Before the fix, DriveVoteProposal (and `next` itself) drove only the READY
// set, so both held votes stayed `pending`, the aggregate stayed `gated` at
// saga stage `held`, and every read surface reported the pre-resolution
// state indefinitely.
func TestDecidedHeldVoteRoutesPastBudgetHold(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	driveToReconcile(t, conn, e, clusteredPayload)
	held := heldInstances(t, conn)
	if len(held) == 0 {
		t.Fatal("nothing held")
	}
	// Phase 2 while the steps are offered: the ballots open.
	nextRun(t, conn, e)
	proposals := make(map[string]int, len(held))
	for _, instance := range held {
		proposals[instance] = heldProposalID(t, conn, e, instance)
	}

	// The admission wall: measured spend lands past the cap AFTER the ballots
	// opened — the reported shape, where the wave's own seats burn the budget
	// between phase 2 and the deciding cast.
	armUsageCap(t, conn, run.ID, 10, "output_tokens")
	_, err := e.BackfillUsage(conn, run.ID, []BackfillRow{
		{Step: stepIDByInstance(t, conn, "implement@0"),
			Unit: "output_tokens", Quantity: 5000},
	}, "", "", nowMS)
	testsupport.Must(t, err, "backfill: %v", err)

	// Premise: the wall genuinely denies the held votes' admission.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		step := stepNamed(t, sched, held[0])
		if ok, cond := sched.Ready(step); ok || cond != CondBudget {
			t.Fatalf("premise: %s ready=%v cond=%q, want a budget denial",
				held[0], ok, cond)
		}
	})

	// The tally decides: full quorum, approved.
	for _, instance := range held {
		setProposalStatus(t, conn, proposals[instance], model.ProposalStatusApproved)
	}

	// THE EVENT: the deciding cast's hook (`vote cast` calls this the moment
	// quorum is reached).
	err = e.DriveVoteProposal(conn, proposals[held[0]], nowMS)
	testsupport.Must(t, err, "DriveVoteProposal: %v", err)

	// The children routed: no lag window, no second write.
	for _, instance := range held {
		id := stepIDByInstance(t, conn, instance)

		step, err := db.GetStep(conn, id)
		testsupport.Must(t, err, "GetStep %s: %v", instance, err)
		if step.Status != db.StepDone {
			t.Errorf("%s stored status = %q after its tally decided, want %q — "+
				"the decided vote sat unrouted behind an admission clause",
				instance, step.Status, db.StepDone)
		}

		// The `step show` surface, read exactly as the verb reads it.
		view, err := LoadStepView(conn, id, nowMS)
		testsupport.Must(t, err, "LoadStepView %s: %v", instance, err)
		if view.Row.Status != db.StepDone {
			t.Errorf("step show reports %s as %q, want %q",
				instance, view.Row.Status, db.StepDone)
		}
	}

	// The `step list` surface agrees.
	rows, err := RunStepList(conn, run.ID, nowMS)
	testsupport.Must(t, err, "RunStepList: %v", err)
	for _, row := range rows {
		for _, instance := range held {
			if row.Instance == instance && row.Status != db.StepDone {
				t.Errorf("step list reports %s as %q, want %q",
					instance, row.Status, db.StepDone)
			}
		}
	}

	// And the holding aggregate resumed and finished: the parent must not
	// keep reporting `gated` after both its clusters were decided.
	parent := heldStep(t, conn, "reconcile@0")
	if parent.SagaStage == db.SagaHeld {
		t.Error("reconcile@0 is still holding after every cluster was decided")
	}
	if parent.Status != db.StepDone {
		t.Errorf("reconcile@0 status = %q after its hold was decided, want %q",
			parent.Status, db.StepDone)
	}
}

// TestDecidedVoteWaitsWhileTheRunIsParked pins the one admission clause the
// widened sweep deliberately keeps: R1. Routing a decided tally is engine
// bookkeeping, but a parked run is parked — nothing advances until an
// operator resumes it, and then the ordinary lazy catch-up finishes the
// routing.
func TestDecidedVoteWaitsWhileTheRunIsParked(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	driveToReconcile(t, conn, e, clusteredPayload)
	held := heldInstances(t, conn)
	if len(held) == 0 {
		t.Fatal("nothing held")
	}
	nextRun(t, conn, e)
	proposals := make(map[string]int, len(held))
	for _, instance := range held {
		proposals[instance] = heldProposalID(t, conn, e, instance)
		setProposalStatus(t, conn, proposals[instance], model.ProposalStatusApproved)
	}

	// The run parks between the quorum and the drive.
	execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
		string(model.RunWaitingHuman), run.ID)

	err := e.DriveVoteProposal(conn, proposals[held[0]], nowMS)
	testsupport.Must(t, err, "DriveVoteProposal on a parked run: %v", err)
	for _, instance := range held {
		if got := heldStep(t, conn, instance).Status; got != db.StepPending {
			t.Errorf("%s status = %q while the run is parked, want %q — a "+
				"parked run must not advance", instance, got, db.StepPending)
		}
	}

	// Resumed, the same hook finishes the routing.
	execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
		string(model.RunActive), run.ID)
	err = e.DriveVoteProposal(conn, proposals[held[0]], nowMS)
	testsupport.Must(t, err, "DriveVoteProposal after resume: %v", err)
	for _, instance := range held {
		if got := heldStep(t, conn, instance).Status; got != db.StepDone {
			t.Errorf("%s status = %q after resume, want %q", instance, got, db.StepDone)
		}
	}
}

// TestRunStatusRollupAgreesWithStepShowOnALapsedClaim is instance 1: one
// claim, read on both surfaces at the same two instants.
//
// Immediately after the claim both report `claimed` (the acceptance's
// "non-stale claimed" read). After the lease lapses un-reaped, both report
// the step effectively back in the pool — where the raw GROUP BY the rollup
// used to ship still says `claimed`, which is exactly the contradiction the
// operator hit ("repin refused and run status said claimed while step show
// said ready").
func TestRunStatusRollupAgreesWithStepShowOnALapsedClaim(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	stepID := stepIDByInstance(t, conn, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	// Fresh claim: both surfaces say `claimed`, immediately.
	view, err := LoadStepView(conn, stepID, nowMS)
	testsupport.Must(t, err, "LoadStepView: %v", err)
	if view.Row.Status != db.StepClaimed {
		t.Fatalf("step show status = %q right after the claim, want %q",
			view.Row.Status, db.StepClaimed)
	}
	counts, err := EffectiveStatusCounts(conn, run.ID, nowMS)
	testsupport.Must(t, err, "EffectiveStatusCounts: %v", err)
	if got := statusCountOf(counts, db.StepClaimed); got != 1 {
		t.Fatalf("rollup claimed = %d right after the claim, want 1 (%v)",
			got, counts)
	}

	// The lease lapses, un-reaped. Effective status computes the reap's
	// answer on BOTH surfaces; the raw column still says `claimed` on
	// neither's authority.
	late := claim.LeaseExpiresMS + 1
	view, err = LoadStepView(conn, stepID, late)
	testsupport.Must(t, err, "LoadStepView after expiry: %v", err)
	if view.Row.Status != db.StepReady {
		t.Fatalf("step show status = %q after the lease lapsed, want %q",
			view.Row.Status, db.StepReady)
	}
	counts, err = EffectiveStatusCounts(conn, run.ID, late)
	testsupport.Must(t, err, "EffectiveStatusCounts after expiry: %v", err)
	if got := statusCountOf(counts, db.StepClaimed); got != 0 {
		t.Errorf("rollup still counts %d claimed after the lease lapsed; the "+
			"rollup and step show must agree at every instant (%v)", got, counts)
	}
	if got := statusCountOf(counts, db.StepReady); got != 1 {
		t.Errorf("rollup ready = %d after the lease lapsed, want 1 (%v)", got, counts)
	}

	// The raw column really does still say `claimed` — the divergence this
	// fix removes from the verb, kept visible here so the two rollups'
	// different questions stay documented.
	raw, err := db.StepStatusCounts(conn, run.ID)
	testsupport.Must(t, err, "StepStatusCounts: %v", err)
	if got := statusCountOf(raw, db.StepClaimed); got != 1 {
		t.Errorf("raw rollup claimed = %d, want 1 — the stored status is "+
			"untouched by a read", got)
	}
}
