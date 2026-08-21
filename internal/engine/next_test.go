package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-55/DKT-80: `next` must never answer with a snapshot that disagrees
// with a write it just made in the SAME call. driveVoteSteps and
// driveActionSteps both run AFTER the readiness pass has already committed
// (next.go's own comment on the ordering explains why: the connection pool
// caps at one, so driving from inside that transaction would deadlock), so a
// tally that resolves during THIS invocation must have its consequence
// folded back into the response — not left for the caller to discover on the
// next poll. RUN-4 measured the consequence of the old behavior: a relay
// misread a decided gate as undecided and a strict conductor improvised a
// dispatch to "force" routing that had already happened.

// rowInstances collects a `next` response's step instances, so a test can
// assert on membership without reaching back into the DB for what the row
// itself already carries.
func rowInstances(rows []model.StepRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Instance
	}
	return out
}

// TestNextRecomputesWhenATallyResolvesInTheSameCall is the regression,
// driven over the held-vote fixture (held_vote_test.go) because it is the
// one path in the tree where a tally's routing also un-defers a downstream
// step — the wider case a mere proposal-id patch cannot cover, since a
// step that only just became ready was never in the rows the first pass
// rendered at all.
//
// Before the fix, the SAME call that read a resolved tally and routed it
// still answered with the pre-routing rows: the held step rode out as
// `ready` a second time, and `verify@0` — un-defered by that same routing —
// was missing until the NEXT poll.
func TestNextRecomputesWhenATallyResolvesInTheSameCall(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "panel", "alice,bob,carol")

	driveToReconcile(t, conn, e, clusteredPayload)

	// Phase 2: the first call opens the proposal. The held step is
	// genuinely still ready — nobody has decided it yet, and its downstream
	// dependent is correctly absent.
	first := nextRun(t, conn, e)
	if !contains(rowInstances(first.Steps), "reconcile-held@0#0") {
		t.Fatalf("premise: the freshly-minted held vote is not offered: %+v",
			first.Steps)
	}
	if contains(rowInstances(first.Steps), "verify@0") {
		t.Fatalf("premise: verify@0 is ready before its gating hold is decided: %+v",
			first.Steps)
	}

	setProposalStatus(t, conn, heldProposalID(t, conn, e, "reconcile-held@0#0"),
		model.ProposalStatusApproved)

	// Phases 4 and 5 — reading the outcome, routing it, and un-deferring the
	// aggregate — all run inside THIS SAME call.
	second := nextRun(t, conn, e)

	if contains(rowInstances(second.Steps), "reconcile-held@0#0") {
		t.Errorf("the vote step this call just routed is still offered as "+
			"ready: %+v — the response disagrees with the write `next` made "+
			"in the same call", second.Steps)
	}
	if !contains(rowInstances(second.Steps), "verify@0") {
		t.Errorf("verify@0 is not offered even though the hold gating it was "+
			"decided by this same call's routing: %+v", second.Steps)
	}

	// And the DB agrees with what the row said — read-your-writes, not a
	// lucky coincidence in the rendering.
	routing := heldStep(t, conn, "reconcile@0")
	if routing.SagaStage == db.SagaHeld {
		t.Error("reconcile@0 is still holding after its hold was decided by " +
			"the same call that reported it decided")
	}
}

// TestNextDoesNotRecomputeWhenNothingRouted pins the other half of the
// constraint carried over from triage: the connection-pool cap of 1 rules
// out a second scheduler pass on EVERY call, so a call where neither driver
// routes anything must still answer correctly from the one pass it already
// computed — a plain human-step run, with no vote or action step in play,
// is exactly that call.
func TestNextDoesNotRecomputeWhenNothingRouted(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	run, _ := activatedRun(t, conn)

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}
	if len(answer.Steps) == 0 {
		t.Fatal("premise: the fixture run must offer at least one ready step")
	}
	// The one pass answers correctly: the claimable row says `ready`, and the
	// closure rows say `staged` — every status is one of the two computed
	// values, and the fresh run's single ready root is implement@0.
	var ready []string
	for _, row := range answer.Steps {
		switch row.Status {
		case db.StepReady:
			ready = append(ready, row.Instance)
		case db.StepStaged:
		default:
			t.Errorf("%s status = %q, want %q or %q",
				row.Instance, row.Status, db.StepReady, db.StepStaged)
		}
	}
	if len(ready) != 1 || ready[0] != "implement@0" {
		t.Errorf("ready rows = %v, want exactly [implement@0]", ready)
	}
}

// ---------------------------------------------------------------------------
// DKT-54: the run-scoped step inventory
// ---------------------------------------------------------------------------

// TestRunStepListEnumeratesTheRun pins `step list --run`'s contract: every
// step of the run, effective statuses, and costs — the enumeration a budget
// projection reads. Before this verb, a conductor guessed contiguous step
// ids, correct only while the store-wide sequence was quiet.
func TestRunStepListEnumeratesTheRun(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	rows, err := RunStepList(conn, run.ID, nowMS)
	testsupport.Must(t, err, "RunStepList: %v", err)
	if len(rows) == 0 {
		t.Fatal("an activated run listed no steps")
	}

	byInstance := map[string]StepListEntry{}
	for _, row := range rows {
		byInstance[row.Instance] = row
	}
	done, ok := byInstance["implement@0"]
	if !ok {
		t.Fatalf("implement@0 missing from the listing: %+v", rows)
	}
	if done.Status != db.StepDone || done.Attempt != 1 {
		t.Errorf("implement@0 = %s attempt %d, want done attempt 1",
			done.Status, done.Attempt)
	}
	if done.ExpectedCost == 0 {
		t.Error("implement@0 lists no expected_cost; the budget projection " +
			"this verb exists for cannot be built from zeros")
	}
	// A review judge is now READY — the effective status, not the stored
	// `pending`, exactly as `step show` computes it.
	judge, ok := byInstance["review@0#0"]
	if !ok {
		t.Fatalf("review@0#0 missing from the listing: %+v", rows)
	}
	if judge.Status != db.StepReady {
		t.Errorf("review@0#0 = %s, want the EFFECTIVE status ready", judge.Status)
	}

	// READ-ONLY: listing must not have reaped, promoted, or rewritten
	// anything — the stored statuses are untouched.
	var stored string
	err = conn.QueryRow(
		`SELECT status FROM steps WHERE run_id = ? AND instance = 'review@0#0'`,
		run.ID).Scan(&stored)
	testsupport.Must(t, err, "reading stored status: %v", err)
	if stored != db.StepPending {
		t.Errorf("stored status = %q after a list, want pending untouched", stored)
	}

	_, err = RunStepList(conn, 999, nowMS)
	if err == nil {
		t.Error("RunStepList(absent run) returned no error")
	}
}
