package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// LOOKAHEAD-STAGED OFFERS (lookahead.go) — the staged closure, one property
// per test.

// TestOfferStagesTheDependencyClosure pins the whole shape at once on the
// standard fixture: a fresh run's FIRST offer carries its entire chain —
// implement claimable now, everything behind it staged at its dependency
// depth, in stage-major wire order — where the pre-closure offer carried
// implement alone and every later level cost a dispatch round-trip.
func TestOfferStagesTheDependencyClosure(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	want := map[string]struct {
		stage  int
		status string
	}{
		"implement@0":  {0, db.StepReady},
		"review@0#0":   {1, db.StepStaged},
		"review@0#1":   {1, db.StepStaged},
		"review@0#2":   {1, db.StepStaged},
		"review@0#3":   {1, db.StepStaged},
		"synthesize@0": {2, db.StepStaged},
		"reconcile@0":  {3, db.StepStaged},
		"verify@0":     {4, db.StepStaged},
	}
	if len(answer.Steps) != len(want) {
		t.Fatalf("offered %d rows, want the full %d-row closure: %v",
			len(answer.Steps), len(want), instancesIn(answer))
	}
	if answer.Total != len(want) {
		t.Errorf("Total = %d, want %d — the truncation contract counts the "+
			"whole offer, staged rows included", answer.Total, len(want))
	}
	prevStage := 0
	for i, row := range answer.Steps {
		w, ok := want[row.Instance]
		if !ok {
			t.Errorf("unexpected row %s", row.Instance)
			continue
		}
		if row.Stage != w.stage || row.Status != w.status {
			t.Errorf("%s = stage %d %q, want stage %d %q",
				row.Instance, row.Stage, row.Status, w.stage, w.status)
		}
		if row.Stage < prevStage {
			t.Errorf("row %d (%s, stage %d) sorts before stage %d — the wire "+
				"order must be stage-major so a top-to-bottom dispatcher "+
				"starts nothing early", i, row.Instance, row.Stage, prevStage)
		}
		prevStage = row.Stage
	}
}

// TestRowsBehindAHoldCapableStepAreConditional is DKT-26: a staged row
// downstream of an `aggregate` declaring `hold_spread` carries
// `conditional: true`, because its stage boundary is a weaker promise — the
// aggregate can finish and HOLD instead of routing, leaving the row
// unclaimable however many lower stages completed. The measured waste: a held
// reconcile's downstream verify was spawned at its stage boundary and its
// claim refused, one full boot for nothing. Rows at or above the
// hold-capable step stay unconditional.
func TestRowsBehindAHoldCapableStepAreConditional(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	// The fixture's reconcile declares hold_spread = 2; verify sits after it.
	want := map[string]bool{
		"implement@0":  false,
		"review@0#0":   false,
		"review@0#1":   false,
		"review@0#2":   false,
		"review@0#3":   false,
		"synthesize@0": false,
		"reconcile@0":  false, // hold-capable itself, not downstream of one
		"verify@0":     true,
	}
	seen := 0
	for _, row := range answer.Steps {
		w, ok := want[row.Instance]
		if !ok {
			continue
		}
		seen++
		if row.Conditional != w {
			t.Errorf("%s conditional = %v, want %v", row.Instance, row.Conditional, w)
		}
	}
	if seen != len(want) {
		t.Errorf("saw %d of the %d expected rows: %v", seen, len(want), instancesIn(answer))
	}
}

// TestStagedRowIsNotClaimable is the safety half of the wire contract: a
// `staged` row is a PREVIEW, and the predicate — not dispatcher discipline —
// is what refuses a claim on one. A stage-skipping dispatcher gets a refusal
// naming the unmet condition, never a wrong result.
func TestStagedRowIsNotClaimable(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	answer, err := e.NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	var staged string
	for _, row := range answer.Steps {
		if row.Status == db.StepStaged {
			staged = row.Instance
			break
		}
	}
	if staged == "" {
		t.Fatalf("premise: the offer must carry a staged row, got %v",
			instancesIn(answer))
	}

	_, err = ClaimStep(conn, stepIDByInstance(t, conn, staged),
		ClaimOptions{Owner: "eager", NowMS: nowMS})
	if err == nil {
		t.Fatalf("claimed %s, a staged row whose predecessors have not "+
			"recorded — the predicate must police the stage order", staged)
	}
	if !strings.Contains(err.Error(), string(CondPredecessors)) {
		t.Errorf("the refusal %q does not name the unmet condition %q",
			err.Error(), CondPredecessors)
	}

	// And the ready row claims exactly as before.
	_, err = ClaimStep(conn, stepIDByInstance(t, conn, "implement@0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim implement@0: %v", err)
}

// TestClosureStopsAtAHumanStep: a human gate is resolvable by no wave — the
// closure neither stages the gate nor anything downstream of it.
func TestClosureStopsAtAHumanStep(t *testing.T) {
	const src = `
[pipeline]
name = "human-gated-chain"
version = 1

[match]
kind = ["task"]

[[step]]
name = "seed"
after = []
executor = "x"
emits = "findings"

[[step]]
name = "decide"
after = ["seed"]
type = "human"
on_fail = "skip"

[[step]]
name = "publish"
after = ["decide"]
executor = "x"
emits = "out"
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "human-gated-chain.toml")
	issue := createIssue(t, conn, "gated", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	answer, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	got := instancesIn(answer)
	if len(got) != 1 || got[0] != "seed@0" {
		t.Errorf("offered %v, want exactly [seed@0]: the human gate cannot "+
			"run in a wave, so neither it nor publish@0 may be staged", got)
	}
}

// TestClosureStopsAtUnsatisfiedIssueDeps: a `depends_on` edge resolves at
// ISSUE completion — rollup work no single wave owns — so the closure never
// reaches across one.
func TestClosureStopsAtUnsatisfiedIssueDeps(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue A", "body", "task", nil)
	b := createIssue(t, conn, "issue B", "body", "task", nil)
	execSQL(t, conn,
		`INSERT INTO issue_relations
		   (source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, 'depends_on', datetime('now'))`, b, a)
	run := startRun(t, conn, a, b)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	answer, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)
	for _, row := range answer.Steps {
		if stepIssueID(t, conn, row.Step) == b {
			t.Errorf("%s belongs to issue B, whose depends_on is unsatisfied — "+
				"no wave completes an ISSUE, so nothing of B's is offerable",
				row.Instance)
		}
	}
}

// voteChainSrc is the vote-mid-chain shape the closure must carry end to end:
// an executor, the vote gate over its output, and the executor behind the
// gate.
const voteChainSrc = `
[pipeline]
name = "vote-chain"
version = 1

[match]
kind = ["task"]

[[step]]
name = "seed"
after = []
executor = "x"
emits = "findings"

[[step]]
name = "poll"
after = ["seed"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
on_fail = "waiting-human"

[[step]]
name = "publish"
after = ["poll"]
executor = "x"
emits = "out"
`

// TestStagedVoteRowCarriesVotersWithoutAProposal: the staged gate row tells a
// dispatcher WHO will cast (the spec's roster, needed to plan the panel)
// while promising no ballot — the proposal does not exist until the gate is
// actually ready, and an empty Proposal field is that fact on the wire.
func TestStagedVoteRowCarriesVotersWithoutAProposal(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(voteChainSrc), "vote-chain.toml")
	issue := createIssue(t, conn, "voted", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	answer, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	var poll *model.StepRow
	for i := range answer.Steps {
		if answer.Steps[i].Instance == "poll@0" {
			poll = &answer.Steps[i]
		}
	}
	if poll == nil {
		t.Fatalf("premise: the vote step must ride the closure, got %v",
			instancesIn(answer))
	}
	if poll.Status != db.StepStaged || poll.Stage == 0 {
		t.Errorf("poll@0 = stage %d %q, want a staged row behind seed@0",
			poll.Stage, poll.Status)
	}
	if len(poll.Voters) != 2 {
		t.Errorf("poll@0 carries voters %v, want the declared roster — a "+
			"dispatcher plans the panel before the stage arrives", poll.Voters)
	}
	if poll.Proposal != "" {
		t.Errorf("poll@0 carries proposal %q before the step is ready; phase "+
			"2 has not run and the wire must not invent a ballot", poll.Proposal)
	}
	if _, ok := stageOf(answer, "publish@0"); !ok {
		t.Errorf("publish@0 is not offered — the closure must extend THROUGH "+
			"a vote step, not stop at it: %v", instancesIn(answer))
	}
}

// TestRecordDrivesAVoteThroughQuorumMidDispatch is the whole mid-wave gate
// lifecycle, with the dispatch OPEN throughout and `next` never called:
//
//  1. recording the gate's last dependency opens its proposal (phase 2 — the
//     record verb is now an observing invocation, engine/drive.go),
//  2. the quorum-reaching cast routes the gate (phases 4/5, DriveVoteProposal),
//  3. the step behind the gate is claimable in the same wave.
func TestRecordDrivesAVoteThroughQuorumMidDispatch(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(voteChainSrc), "vote-chain.toml")
	issue := createIssue(t, conn, "voted", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	openDispatch(t, conn, run.ID, 0, nowMS)

	// The wave records seed@0; the verb layer then drives (the CLI's
	// `step record` hook calls exactly this).
	claimAndComplete(t, conn, e, "seed@0", "the findings", "")
	{
		err := e.DriveRunLifecycles(conn, run.ID, nowMS)
		testsupport.Must(t, err, "driving after the record: %v", err)
	}

	pollID := stepIDByInstance(t, conn, "poll@0")
	poll, err := db.GetStep(conn, pollID)
	testsupport.Must(t, err, "reading poll@0: %v", err)
	proposalID, err := findVoteProposal(conn, poll)
	testsupport.Must(t, err, "finding poll@0's proposal: %v", err)
	if proposalID == 0 {
		t.Fatal("no proposal opened for poll@0 after its dependency recorded " +
			"— `step record` must be a phase-2 observing invocation mid-wave")
	}

	// The seats cast; the deciding cast's verb drives the routing (the CLI's
	// `vote cast` hook calls DriveVoteProposal on quorum).
	for _, seat := range []string{"seat-a", "seat-b"} {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID: proposalID, VoterName: seat,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}
	if err := e.DriveVoteProposal(conn, proposalID, nowMS); err != nil {
		t.Fatalf("driving the decided proposal: %v", err)
	}

	if got := stepStatus(t, conn, "poll@0"); got != db.StepDone {
		t.Fatalf("poll@0 = %q after an approved quorum, want %q — the "+
			"deciding cast must route the gate without a `next`", got, db.StepDone)
	}

	// The staged row behind the gate is claimable IN THIS WAVE.
	_, err = ClaimStep(conn, stepIDByInstance(t, conn, "publish@0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim publish@0 mid-dispatch: %v", err)
}

// TestRecordRunsAReadiedActionMidDispatch: the fixture's `reconcile` action
// runs the moment the recording of `synthesize` readies it — engine-side,
// mid-wave, no `next` — and the verify step behind it becomes claimable in
// the same wave.
func TestRecordRunsAReadiedActionMidDispatch(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	openDispatch(t, conn, run.ID, 0, nowMS)

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	for _, judge := range []string{"review@0#0", "review@0#1", "review@0#2", "review@0#3"} {
		claimAndComplete(t, conn, e, judge, "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", "")
	if err := e.DriveRunLifecycles(conn, run.ID, nowMS); err != nil {
		t.Fatalf("driving after synthesize recorded: %v", err)
	}

	if got := stepStatus(t, conn, "reconcile@0"); got != db.StepDone {
		t.Fatalf("reconcile@0 = %q after its dependency recorded mid-dispatch, "+
			"want %q — the record verb drives ready actions", got, db.StepDone)
	}
	_, err := ClaimStep(conn, stepIDByInstance(t, conn, "verify@0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim verify@0 mid-dispatch: %v", err)
}
