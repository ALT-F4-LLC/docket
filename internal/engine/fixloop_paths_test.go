package engine

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-168: every path that can resolve a routing to `fix-loop` must ENTER the
// loop — counter, supersede sweep, ordinal-k instantiation, bound — not merely
// write the string into the routing column. RUN-25 shipped the gap: a rejected
// security-vote routed `fix-loop` with reconcile long done, no fix step was
// ever created, and the issue closed `done` carrying the reproduced blocker
// the vote had caught. The saga's threshold path was the only one wired to
// EnterLoop; these tests pin the other paths.

// voteLoopSrc is RUN-25's shape reduced: a declared vote gate whose rejection
// routes `fix-loop`, with a loop body to instantiate.
const voteLoopSrc = `
[pipeline]
name = "vote-loop"
version = 1

[match]
kind = ["task"]

[[step]]
name = "seed"
after = []
executor = "x"
emits = "findings"

[[step]]
name = "gate"
after = ["seed"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
on_fail = "fix-loop"

[[step]]
name = "fix"
executor = "x"
emits = "findings"
loop = true
after_loop = "gate"
`

// TestRejectedVoteTallyEntersTheLoop is DKT-168's direct regression test: a
// rejected tally whose on_fail is `fix-loop` raises the issue's loop counter
// and instantiates the fix step, exactly as a saga threshold routing does.
func TestRejectedVoteTallyEntersTheLoop(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(voteLoopSrc), "vote-loop.toml")
	issue := createIssue(t, conn, "voted", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	claimAndComplete(t, conn, e, "seed@0", "the findings", "")
	err = e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after the record: %v", err)

	gate, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
	testsupport.Must(t, err, "reading gate@0: %v", err)
	proposalID, err := findVoteProposal(conn, gate)
	testsupport.Must(t, err, "finding gate@0's proposal: %v", err)
	if proposalID == 0 {
		t.Fatal("no proposal opened for gate@0")
	}

	for _, seat := range []string{"seat-a", "seat-b"} {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID: proposalID, VoterName: seat,
			Verdict: model.VerdictReject, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the rejected proposal: %v", err)

	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Errorf("loop_count = %d after a rejected fix-loop tally, want 1 — "+
			"the routing was recorded with no loop entry", got)
	}
	stepIDByInstance(t, conn, "fix@1") // fatals if the fix step was never created
	step, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
	testsupport.Must(t, err, "re-reading gate@0: %v", err)
	if !strings.HasPrefix(step.Routing, workflow.OnFailFixLoop) {
		t.Errorf("gate@0 routing = %q, want %q", step.Routing, workflow.OnFailFixLoop)
	}
}

// TestRejectedHumanGateEntersTheLoop pins the same property on the human-gate
// path — the shipped standard-change fixture routes commit-gate rejections
// `fix-loop`, and before DKT-168 a reject recorded the string and created
// nothing.
func TestRejectedHumanGateEntersTheLoop(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, "review@0#"+strconv.Itoa(i), "findings", "")
	}
	claimAndComplete(t, conn, e, "synthesize@0", "synthesized", "")
	driveAction(t, conn, e, "reconcile@0")
	claimAndComplete(t, conn, e, "verify@0", "report", metPayload)

	err := e.DecideStep(conn, stepIDByInstance(t, conn, "commit-gate@0"),
		false, "not yet", nowMS)
	testsupport.Must(t, err, "reject: %v", err)

	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Errorf("loop_count = %d after a rejected fix-loop human gate, want 1", got)
	}
	stepIDByInstance(t, conn, "fix@1") // fatals if the fix step was never created
}

// interposeDownstreamSrc is RUN-25's topology reduced to the readiness
// question (DKT-168 defect A): `report` is the routing step's ORDINARY
// downstream — a sibling of the interposed gate, not its successor. Before
// the CondGateOpen clause, `report` became claimable the moment `verify`
// recorded, racing the still-open vote.
const interposeDownstreamSrc = `
[pipeline]
name = "interpose-downstream"
version = 1

[match]
kind = ["task"]

[[step]]
name = "verify"
executor = "verify"
emits = "report"
threshold = { "tribunal" = "any(status == blocked)" }

[[step]]
name = "tribunal"
after = ["verify"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
on_fail = "waiting-human"

[[step]]
name = "report"
after = ["verify"]
executor = "reporter"
emits = "record"
`

// TestDownstreamWaitsForAnOpenInterposedGate: while the routed-to vote gate is
// open, the routing step's ordinary downstream is NOT ready — §11.2's
// "on the gate's own pass execution resumes at the routing step's ordinary
// downstream", the half DKT-38's latch left out.
func TestDownstreamWaitsForAnOpenInterposedGate(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	runID, _ := activateInterposed(t, conn, interposeDownstreamSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "verify@0", "blocked finding", `[{"status":"blocked"}]`)
	err := e.DriveRunLifecycles(conn, runID, nowMS)
	testsupport.Must(t, err, "driving lifecycles: %v", err)

	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		ok, cond := sched.Ready(stepNamed(t, sched, "report@0"))
		if ok {
			t.Fatal("report@0 is ready while tribunal@0's vote is still open — " +
				"its verdict would finalize before the gate's tally can contradict it")
		}
		if cond != CondGateOpen {
			t.Errorf("report@0 held by %q, want CondGateOpen", cond)
		}
	})

	// The panel approves; the tally terminalizes the gate and the downstream
	// resumes.
	tribunal, err := db.GetStep(conn, stepIDByInstance(t, conn, "tribunal@0"))
	testsupport.Must(t, err, "reading tribunal@0: %v", err)
	proposalID, err := findVoteProposal(conn, tribunal)
	testsupport.Must(t, err, "finding tribunal@0's proposal: %v", err)
	for _, seat := range []string{"seat-a", "seat-b"} {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID: proposalID, VoterName: seat,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the approved proposal: %v", err)

	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "report@0")); !ok {
			t.Errorf("report@0 not ready after the gate passed: %q", cond)
		}
	})
}

// TestDownstreamFreeWhenTheThresholdRoutesElsewhere: a clean verify routes
// `pass`, the unrouted gate is skipped in the SAME transaction, and the
// downstream is ready immediately — the clause adds zero latency to the
// ordinary path.
func TestDownstreamFreeWhenTheThresholdRoutesElsewhere(t *testing.T) {
	conn := mustDB(t)
	runID, _ := activateInterposed(t, conn, interposeDownstreamSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "verify@0", "all clear", `[{"status":"ok"}]`)

	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepSkipped {
		t.Fatalf("tribunal@0 = %q after verify routed pass, want %q", got, db.StepSkipped)
	}
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "report@0")); !ok {
			t.Errorf("report@0 not ready behind a skipped gate: %q", cond)
		}
	})
}
