package engine

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-545: concern-aware routing on vote steps, and the `<step>.vote-record`
// input form. Every read-gate tally in the corpus APPROVED and none was clean
// (DKT-V34 passed 2-1 over a security dissent; DKT-V140/V160 each passed with
// two approve-with-concerns casts), and a workflow had no way to act on it:
// only the binary tally reached routing, and the concern text lived where no
// input form could address it.

// concernLoopSrc: a vote gate whose APPROVED-with-concerns tally routes
// `fix-loop`, with a loop body to instantiate — the revise loop a rejection
// already entered, now reachable by a concerned approval.
const concernLoopSrc = `
[pipeline]
name = "concern-loop"
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
voters = ["seat-a", "seat-b", "seat-c"]
vote_rule = "majority"
on_fail = "waiting-human"
threshold = { "fix-loop" = "count>=2(vote == approve-with-concerns)" }

[[step]]
name = "fix"
executor = "x"
emits = "findings"
loop = true
after_loop = "gate"
inputs = ["gate.vote-record"]
`

// openGateProposal drives a run up to its vote step's open proposal: seed
// completes, the lifecycle drive opens the ballot, and the proposal id comes
// back for the seats to cast against.
func openGateProposal(t *testing.T, conn *sql.DB, e *Engine, runID int) int {
	t.Helper()
	claimAndComplete(t, conn, e, "seed@0", "the findings", "")
	err := e.DriveRunLifecycles(conn, runID, nowMS)
	testsupport.Must(t, err, "driving after the record: %v", err)

	gate, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
	testsupport.Must(t, err, "reading gate@0: %v", err)
	proposalID, err := findVoteProposal(conn, gate)
	testsupport.Must(t, err, "finding gate@0's proposal: %v", err)
	if proposalID == 0 {
		t.Fatal("no proposal opened for gate@0")
	}
	return proposalID
}

// castSeat casts one verdict with the weights every seat in these tests
// shares, so a unanimous approval (concerned or clean) tallies above the 0.5
// rule and the routing under test is genuinely the threshold's, never the
// tally's.
func castSeat(t *testing.T, conn *sql.DB, proposalID int, seat string, verdict model.Verdict, summary string) {
	t.Helper()
	_, err := db.CastVote(conn, &model.Vote{
		ProposalID: proposalID, VoterName: seat, Verdict: verdict,
		Confidence: 0.9, DomainRelevance: 0.8, Summary: summary,
	})
	testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
}

// stepCount reports how many step rows hold an instance — the non-fatal
// sibling of stepIDByInstance, for asserting a step was NOT created.
func stepCount(t *testing.T, conn *sql.DB, instance string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE instance = ?`, instance).Scan(&n)
	testsupport.Must(t, err, "counting %s: %v", instance, err)
	return n
}

// TestConcernedApprovalRoutesFixLoop is the issue's central scenario: an
// APPROVED tally carried by two approve-with-concerns casts enters the same
// revise loop a rejection does — counter, loop body, routing record naming
// the matched predicate — instead of the concerns evaporating.
func TestConcernedApprovalRoutesFixLoop(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(concernLoopSrc), "concern-loop.toml")
	issue := createIssue(t, conn, "concerned", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, run.ID)
	castSeat(t, conn, proposalID, "seat-a", model.VerdictApproveWithConcerns, "auth check is thin")
	castSeat(t, conn, proposalID, "seat-b", model.VerdictApproveWithConcerns, "no rollback path")
	castSeat(t, conn, proposalID, "seat-c", model.VerdictApprove, "")
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the concerned approval: %v", err)

	// The tally itself APPROVED — the routing under test is the threshold's.
	proposal, err := db.GetProposal(conn, proposalID)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if proposal.Status != model.ProposalStatusApproved {
		t.Fatalf("proposal status = %q, want approved — the fixture must "+
			"exercise the threshold, not the tally", proposal.Status)
	}

	if got := loopCount(t, conn, run.ID, issue); got != 1 {
		t.Errorf("loop_count = %d after a concerned approval matched the "+
			"fix-loop threshold, want 1", got)
	}
	stepIDByInstance(t, conn, "fix@1") // fatals if the fix step was never created

	gate, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
	testsupport.Must(t, err, "re-reading gate@0: %v", err)
	if !strings.HasPrefix(gate.Routing, workflow.OnFailFixLoop) {
		t.Errorf("gate@0 routing = %q, want a %q routing", gate.Routing, workflow.OnFailFixLoop)
	}
	if !strings.Contains(gate.Routing, "approve-with-concerns") {
		t.Errorf("gate@0 routing record %q does not name the matched predicate", gate.Routing)
	}
}

// TestCleanApprovalPassesConcernThreshold: three clean approvals do not match
// the concern predicate, so the step routes pass exactly as an un-thresholded
// vote step does — no loop entry, no fix step.
func TestCleanApprovalPassesConcernThreshold(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(concernLoopSrc), "concern-loop.toml")
	issue := createIssue(t, conn, "clean", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, run.ID)
	for _, seat := range []string{"seat-a", "seat-b", "seat-c"} {
		castSeat(t, conn, proposalID, seat, model.VerdictApprove, "")
	}
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the clean approval: %v", err)

	if got := stepStatus(t, conn, "gate@0"); got != db.StepDone {
		t.Errorf("gate@0 = %q after a clean approval, want %q", got, db.StepDone)
	}
	gate, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
	testsupport.Must(t, err, "reading gate@0: %v", err)
	if !strings.HasPrefix(gate.Routing, RoutingPass) {
		t.Errorf("gate@0 routing = %q, want a %q routing", gate.Routing, RoutingPass)
	}
	if got := loopCount(t, conn, run.ID, issue); got != 0 {
		t.Errorf("loop_count = %d after a clean approval, want 0", got)
	}
	if n := stepCount(t, conn, "fix@1"); n != 0 {
		t.Errorf("%d fix@1 step(s) exist after a clean approval, want none", n)
	}
}

// TestRejectedTallyIgnoresConcernThreshold: a REJECTED tally still routes per
// `on_fail`, threshold or no threshold — the threshold asks "was the approval
// clean", which is not a question about a rejection.
func TestRejectedTallyIgnoresConcernThreshold(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(concernLoopSrc), "concern-loop.toml")
	issue := createIssue(t, conn, "rejected", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, run.ID)
	for _, seat := range []string{"seat-a", "seat-b", "seat-c"} {
		castSeat(t, conn, proposalID, seat, model.VerdictReject, "no")
	}
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the rejection: %v", err)

	// The declared on_fail is waiting-human; the fix-loop THRESHOLD must not
	// capture a rejection.
	if got := stepStatus(t, conn, "gate@0"); got != db.StepWaitingHuman {
		t.Errorf("gate@0 = %q after a rejected tally, want %q — a rejection "+
			"routes per on_fail, never through the threshold", got, db.StepWaitingHuman)
	}
	if got := loopCount(t, conn, run.ID, issue); got != 0 {
		t.Errorf("loop_count = %d after a rejected tally with on_fail=waiting-human, want 0", got)
	}
}

// TestConcernedApprovalWithoutThresholdPasses is backward compatibility: the
// exact cast set that routes fix-loop under a threshold routes PASS on a step
// declaring none — every pre-existing vote step behaves as it always did.
func TestConcernedApprovalWithoutThresholdPasses(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	src := strings.Replace(concernLoopSrc,
		"threshold = { \"fix-loop\" = \"count>=2(vote == approve-with-concerns)\" }\n", "", 1)
	if src == concernLoopSrc {
		t.Fatal("fixture bug: the threshold line was not removed")
	}
	registerSource(t, conn, []byte(src), "no-threshold.toml")
	issue := createIssue(t, conn, "unthresholded", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, run.ID)
	castSeat(t, conn, proposalID, "seat-a", model.VerdictApproveWithConcerns, "worry one")
	castSeat(t, conn, proposalID, "seat-b", model.VerdictApproveWithConcerns, "worry two")
	castSeat(t, conn, proposalID, "seat-c", model.VerdictApprove, "")
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the concerned approval: %v", err)

	if got := stepStatus(t, conn, "gate@0"); got != db.StepDone {
		t.Errorf("gate@0 = %q on a step with no threshold, want %q", got, db.StepDone)
	}
	if got := loopCount(t, conn, run.ID, issue); got != 0 {
		t.Errorf("loop_count = %d on a step with no threshold, want 0", got)
	}
}

// TestConcernThresholdWaitingHumanLeg: the waiting-human routing parks the
// step for an operator, with the matched predicate in the routing record.
func TestConcernThresholdWaitingHumanLeg(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	src := strings.Replace(concernLoopSrc,
		`threshold = { "fix-loop" = "count>=2(vote == approve-with-concerns)" }`,
		`threshold = { "waiting-human" = "any(verdict == approve-with-concerns)" }`, 1)
	if src == concernLoopSrc {
		t.Fatal("fixture bug: the threshold line was not replaced")
	}
	registerSource(t, conn, []byte(src), "concern-park.toml")
	issue := createIssue(t, conn, "parked", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, run.ID)
	castSeat(t, conn, proposalID, "seat-a", model.VerdictApproveWithConcerns, "one worry")
	castSeat(t, conn, proposalID, "seat-b", model.VerdictApprove, "")
	castSeat(t, conn, proposalID, "seat-c", model.VerdictApprove, "")
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the concerned approval: %v", err)

	if got := stepStatus(t, conn, "gate@0"); got != db.StepWaitingHuman {
		t.Errorf("gate@0 = %q, want %q", got, db.StepWaitingHuman)
	}
	gate, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
	testsupport.Must(t, err, "reading gate@0: %v", err)
	if !strings.HasPrefix(gate.Routing, workflow.OnFailWaitingHuman) {
		t.Errorf("gate@0 routing = %q, want a %q routing", gate.Routing, workflow.OnFailWaitingHuman)
	}
	if !strings.Contains(gate.Routing, "approve-with-concerns") {
		t.Errorf("gate@0 routing record %q does not name the matched predicate", gate.Routing)
	}
	if got := loopCount(t, conn, run.ID, issue); got != 0 {
		t.Errorf("loop_count = %d for a waiting-human routing, want 0", got)
	}
}

// voteRecordSrc: a downstream executor reading the vote step's record through
// declared inputs — the piece that lets a consumer see WHAT the panel said
// without shelling out to `docket vote show`.
const voteRecordSrc = `
[pipeline]
name = "vote-record-wf"
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
on_fail = "skip"

[[step]]
name = "report"
after = ["gate"]
executor = "reporter"
emits = "record"
inputs = ["gate.vote-record"]
`

// TestVoteRecordResolvesAsAnInput: the record arrives in the consumer's
// context bundle — tally outcome, score, and every cast with its rationale.
func TestVoteRecordResolvesAsAnInput(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(voteRecordSrc), "vote-record-wf.toml")
	issue := createIssue(t, conn, "read the record", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, run.ID)
	castSeat(t, conn, proposalID, "seat-a", model.VerdictApproveWithConcerns,
		"tighten the auth check")
	castSeat(t, conn, proposalID, "seat-b", model.VerdictApprove, "ship it")
	err = e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the approval: %v", err)

	claim, err := ClaimStep(conn, stepIDByInstance(t, conn, "report@0"),
		ClaimOptions{Owner: "reporter", NowMS: nowMS})
	testsupport.Must(t, err, "claim report@0: %v", err)

	var body string
	for _, input := range claim.Context.Inputs {
		if input.Kind == workflow.VoteRecordKind {
			body = input.Body
			if input.ProducerStep != "gate@0" {
				t.Errorf("producer = %q, want gate@0", input.ProducerStep)
			}
		}
	}
	if body == "" {
		t.Fatal("report's context carries no vote-record input")
	}

	var record struct {
		Proposal      string   `json:"proposal"`
		Status        string   `json:"status"`
		WeightedScore *float64 `json:"weighted_score"`
		Casts         []struct {
			Voter     string `json:"voter"`
			Verdict   string `json:"verdict"`
			Rationale string `json:"rationale"`
		} `json:"casts"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(body), &record), "parsing the record: %v", err)

	if record.Proposal != model.FormatProposalID(proposalID) {
		t.Errorf("record.proposal = %q, want %q", record.Proposal, model.FormatProposalID(proposalID))
	}
	if record.Status != string(model.ProposalStatusApproved) {
		t.Errorf("record.status = %q, want approved", record.Status)
	}
	if record.WeightedScore == nil {
		t.Error("record carries no weighted_score for a tallied proposal")
	}
	if len(record.Casts) != 2 {
		t.Fatalf("record carries %d casts, want 2: %s", len(record.Casts), body)
	}
	byVoter := map[string]struct{ verdict, rationale string }{}
	for _, c := range record.Casts {
		byVoter[c.Voter] = struct{ verdict, rationale string }{c.Verdict, c.Rationale}
	}
	if got := byVoter["seat-a"]; got.verdict != string(model.VerdictApproveWithConcerns) ||
		got.rationale != "tighten the auth check" {
		t.Errorf("seat-a's cast = %+v, want its concerned verdict and rationale", got)
	}
	if got := byVoter["seat-b"]; got.verdict != string(model.VerdictApprove) ||
		got.rationale != "ship it" {
		t.Errorf("seat-b's cast = %+v, want its approval and rationale", got)
	}
}

// TestVoteCastPayloadKeysMatchTheValidator pins the drift V36 and the payload
// builder must not develop: the keys the engine builds are EXACTLY the fields
// the validator admits, both read from workflow.VoteCastFields.
func TestVoteCastPayloadKeysMatchTheValidator(t *testing.T) {
	payloads := voteCastPayloads([]*model.Vote{{
		VoterName: "seat-a", Verdict: model.VerdictApproveWithConcerns,
	}})
	if len(payloads) != 1 {
		t.Fatalf("%d payloads for one cast", len(payloads))
	}
	if len(payloads[0]) != len(workflow.VoteCastFields) {
		t.Errorf("cast payload has %d keys, validator admits %d fields",
			len(payloads[0]), len(workflow.VoteCastFields))
	}
	for _, field := range workflow.VoteCastFields {
		if _, ok := payloads[0][field]; !ok {
			t.Errorf("cast payload is missing validated field %q", field)
		}
	}
}
