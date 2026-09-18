package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-584 — vote-step usage was invisible to the budget: a declared
// `expected_cost` on a `type="vote"` step was inert (votes are unclaimable, so
// no `step-claimed` event ever carried it into the floor), engine-minted
// reconcile-held ballots carried expected_cost 0 by construction, the report's
// `vote_usage` was silently excluded from `reported`/`spend` with nothing
// saying so, and a conversational-gate proposal that named a run (an
// activation panel, a reap-ack ballot) appeared in no run section at all.

// voteCostSrc declares a vote step WITH an expected_cost — the declaration
// the issue found inert.
const voteCostSrc = `
[pipeline]
name = "dkt584-vote-cost"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
executor = "worker"
emits = "diff"
expected_cost = 1.5

[[step]]
name = "tribunal"
after = ["implement"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
expected_cost = 2.0
on_fail = "waiting-human"
`

// TestFloorAccruesDeclaredVoteStepCostAtMaterialization: the floor includes a
// vote step's declared expected_cost from the moment the row exists — no
// claim event required, because none can ever exist for it.
func TestFloorAccruesDeclaredVoteStepCostAtMaterialization(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(voteCostSrc), "dkt584.toml")
	issue := createIssue(t, conn, "vote cost", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// Before ANY claim: the floor is exactly the vote step's declaration.
	if got := runFloor(t, conn, run.ID); got != 2.0 {
		t.Fatalf("floor after activation = %g, want 2.0 (the vote step's "+
			"declared expected_cost, accrued at materialization)", got)
	}

	// A claim accrues on top, exactly as before.
	claimInstance(t, conn, "implement@0", nowMS)
	if got := runFloor(t, conn, run.ID); got != 3.5 {
		t.Errorf("floor after one claim = %g, want 3.5 (1.5 claimed + 2.0 vote)", got)
	}
}

// TestVoteStepCostIsNotReservedTwice: R7 must not add a vote step's cost on
// top of a floor that already carries it — spend()+0, not spend()+cost.
func TestVoteStepCostIsNotReservedTwice(t *testing.T) {
	conn := mustDB(t)
	e := testEngine()
	registerSource(t, conn, []byte(voteCostSrc), "dkt584.toml")
	issue := createIssue(t, conn, "vote cost", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// Cap exactly equals the run's whole declared cost: 1.5 + 2.0.
	execSQL(t, conn, `UPDATE runs SET budget = ? WHERE id = ?`, 3.5, run.ID)

	// The executor step fits: spend (2.0 vote floor) + 1.5 = 3.5 <= 3.5.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ready, cond := sched.Ready(stepNamed(t, sched, "implement@0")); !ready {
			t.Fatalf("implement@0 not ready under an exactly-fitting cap: %s "+
				"(the vote step's cost is being counted against the cap twice)",
				cond)
		}
	})

	claimAndComplete(t, conn, e, "implement@0", "a diff", "")

	// The vote step's own turn: spend is now 3.5 (1.5 claimed + 2.0 vote).
	// Its readiness must reserve 0 — the 2.0 is ALREADY in the floor — so the
	// budget clause passes at an exactly-spent cap.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ready, cond := sched.Ready(stepNamed(t, sched, "tribunal@0")); !ready {
			t.Errorf("tribunal@0 not ready at an exactly-spent cap: %s (its "+
				"declared cost is in the floor and must not be reserved again)",
				cond)
		}
	})
}

// TestHeldVoteStepMintsConfiguredCost: `vote.hold.cost` lands on the
// engine-minted held ballot's row and reaches the floor — the configurable
// default for the steps that carried 0 by construction.
func TestHeldVoteStepMintsConfiguredCost(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "dkt584-panel", "seat-a,seat-b")
	err := db.SetConfig(conn, 0, db.KeyVoteHoldCost, "2.5")
	testsupport.Must(t, err, "setting %s: %v", db.KeyVoteHoldCost, err)

	floorBefore := runFloor(t, conn, run.ID)
	driveToReconcile(t, conn, e, clusteredPayload)
	held := heldInstances(t, conn)
	if len(held) == 0 {
		t.Fatal("nothing held")
	}

	for _, instance := range held {
		step := heldStep(t, conn, instance)
		if step.Kind != workflow.TypeVote {
			t.Fatalf("%s minted %q, want %q", instance, step.Kind, workflow.TypeVote)
		}
		if step.ExpectedCost != 2.5 {
			t.Errorf("%s expected_cost = %g, want the configured 2.5",
				instance, step.ExpectedCost)
		}
	}

	// The floor rose by the drive's own claims PLUS 2.5 per minted ballot.
	var claimed float64
	err = conn.QueryRow(
		`SELECT COALESCE(SUM(s.expected_cost), 0)
		   FROM events e JOIN steps s ON s.id = e.step_id
		  WHERE e.run_id = ? AND e.kind = ?`, run.ID, EventStepClaimed,
	).Scan(&claimed)
	testsupport.Must(t, err, "summing claim events: %v", err)

	want := floorBefore + claimed + 2.5*float64(len(held))
	if got := runFloor(t, conn, run.ID); got != want {
		t.Errorf("floor = %g, want %g (claims %g + %d held ballots at 2.5)",
			got, want, claimed, len(held))
	}
}

// TestHumanHoldIgnoresConfiguredCost: a hold minted `human` (no tally
// configured) stays at 0 even with `vote.hold.cost` set — an operator's
// decision is not a panel's spend.
func TestHumanHoldIgnoresConfiguredCost(t *testing.T) {
	conn := mustDB(t)
	activatedRun(t, conn)
	e := testEngine()
	err := db.SetConfig(conn, 0, db.KeyVoteHoldCost, "2.5")
	testsupport.Must(t, err, "setting %s: %v", db.KeyVoteHoldCost, err)

	driveToReconcile(t, conn, e, clusteredPayload)
	held := heldInstances(t, conn)
	if len(held) == 0 {
		t.Fatal("nothing held")
	}
	for _, instance := range held {
		step := heldStep(t, conn, instance)
		if step.Kind != workflow.TypeHuman {
			t.Fatalf("%s minted %q, want %q", instance, step.Kind, workflow.TypeHuman)
		}
		if step.ExpectedCost != 0 {
			t.Errorf("%s expected_cost = %g, want 0 on a human hold",
				instance, step.ExpectedCost)
		}
	}
}

// TestRunReportNotesVoteUsageExclusion: a run whose panels cast carries the
// explicit exclusion note in its budget section; a run with no panels does
// not. The silent omission — vote_usage disjoint from reported/spend with
// nothing saying so — is the reported bug.
func TestRunReportNotesVoteUsageExclusion(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "note-panel", "seat-a,seat-b")
	driveToReconcile(t, conn, e, clusteredPayload)
	nextRun(t, conn, e)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)
	if report.Budget.VoteUsageNote != "" {
		t.Errorf("a run with no casts carries a note: %q", report.Budget.VoteUsageNote)
	}

	held := heldInstances(t, conn)
	if len(held) == 0 {
		t.Fatal("nothing held")
	}
	proposalID := heldProposalID(t, conn, e, held[0])
	_, err = db.CastVote(conn, &model.Vote{
		ProposalID: proposalID, VoterName: "seat-a",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		Usage: map[string]float64{"tokens": 100},
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	report, err = LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)
	if report.Budget.VoteUsageNote != VoteUsageExcludedNote {
		t.Errorf("vote_usage_note = %q, want the exclusion note once a seat cast",
			report.Budget.VoteUsageNote)
	}
}

// TestConversationalProposalNamingRunJoinsVoteUsage: a proposal that NAMES
// the run in its description — an activation panel opened with `vote create`,
// no idempotency key, no step — is attributed to that run's vote_usage
// rollup and its coverage line. A proposal naming a DIFFERENT run whose
// rendered id merely contains this one's ("RUN-1" inside "RUN-17") is not.
func TestConversationalProposalNamingRunJoinsVoteUsage(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	cast := func(description string, usage float64) {
		t.Helper()
		id, err := db.CreateProposal(conn, &model.Proposal{
			ProjectID: 1, Description: description,
			Rationale:   "conversational gate",
			Criticality: model.CriticalityMedium,
			Threshold:   0.5, RequiredVoters: 3,
			Status: model.ProposalStatusOpen, CreatedBy: "conductor",
		})
		testsupport.Must(t, err, "CreateProposal(%q): %v", description, err)
		_, err = db.CastVote(conn, &model.Vote{
			ProposalID: id, VoterName: "seat-a",
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
			Usage: map[string]float64{"tokens": usage},
		})
		testsupport.Must(t, err, "CastVote on %q: %v", description, err)
	}

	token := model.FormatRunID(run.ID)
	cast("activation panel for "+token+": proceed with the batch?", 42)
	// The boundary case: this proposal names a run whose id CONTAINS ours.
	cast("activation panel for "+token+"7: unrelated run", 999)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	var tokens *db.UnitTotal
	for i := range report.VoteUsage {
		if report.VoteUsage[i].Unit == "tokens" {
			tokens = &report.VoteUsage[i]
		}
	}
	if tokens == nil {
		t.Fatalf("vote_usage carries no tokens rollup: %+v — the "+
			"run-naming conversational proposal was not attributed",
			report.VoteUsage)
	}
	if tokens.Quantity != 42 || tokens.Rows != 1 {
		t.Errorf("tokens rollup = %+v, want 42 across 1 seat report — 999 "+
			"belongs to the OTHER run", tokens)
	}
	if c := report.VoteUsageCoverage; c.Casts != 1 || c.Reported != 1 {
		t.Errorf("coverage = %+v, want 1 cast / 1 reported", c)
	}
}

// TestReapAckBallotJoinsVoteUsage: a reap-ack ballot — keyed to the run by
// ReapAckProposalKey but outside the vote-step family — is attributed too.
func TestReapAckBallotJoinsVoteUsage(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	id, err := db.CreateProposalIdempotent(conn, &model.Proposal{
		ProjectID: 1, Description: "accept the reap at seq 42?",
		Criticality: model.CriticalityMedium,
		Threshold:   0.5, RequiredVoters: 3,
		Status: model.ProposalStatusOpen, CreatedBy: "conductor",
	}, ReapAckProposalKey(run.ID, 42))
	testsupport.Must(t, err, "CreateProposalIdempotent: %v", err)
	_, err = db.CastVote(conn, &model.Vote{
		ProposalID: id, VoterName: "seat-a",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		Usage: map[string]float64{"tokens": 7},
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	var tokens *db.UnitTotal
	for i := range report.VoteUsage {
		if report.VoteUsage[i].Unit == "tokens" {
			tokens = &report.VoteUsage[i]
		}
	}
	if tokens == nil || tokens.Quantity != 7 || tokens.Rows != 1 {
		t.Errorf("vote_usage tokens = %+v, want 7 across 1 seat report from "+
			"the reap-ack ballot", tokens)
	}
}
