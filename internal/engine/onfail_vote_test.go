package engine

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// triageSrc is DKT-1901's shape: a first-lane executor with no review upstream
// of it, whose gate failure routes to a declared triage panel instead of
// parking for an operator. The loop body lets the panel buy a fix round.
const triageSrc = `
[pipeline]
name = "triage-lane"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
after = []
executor = "x"
emits = "findings"
gates = ["build"]
on_fail = "triage"

[[step]]
name = "triage"
after = ["implement"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
on_fail = "waiting-human"

[step.on_fail_routes]
approved = "APPROVED"
rejected = "REJECTED"

[[step]]
name = "fix"
executor = "x"
emits = "findings"
loop = true
after_loop = "triage"
`

// failingGates fails every completion gate, which is the condition that sends
// an executor down the `on_fail` path.
type failingGates struct{}

func (failingGates) Run(_ context.Context, spec GateSpec, _ StepContext) (GateResult, error) {
	return GateResult{
		Gate: spec.Name, Exit: 1, Verdict: VerdictFail,
		Output: "boom: " + spec.Name + " failed",
	}, nil
}

// TestExecutorFailureRoutesToVoteStep is DKT-1901's criteria 2 and 3: a failed
// executor whose `on_fail` names a vote step is routed to that panel rather
// than parked, the panel's proposal opens carrying the failure evidence, and
// each declared outcome applies its mapped routing to the failed step.
func TestExecutorFailureRoutesToVoteStep(t *testing.T) {
	t.Run("the failure routes to the panel with its evidence", func(t *testing.T) {
		conn, _, _ := triageRun(t, "retry", "abandon-issue")

		step := mustStep(t, conn, "implement@0")
		if !routingIs(step.Routing, "triage") {
			t.Errorf("implement@0 routing = %q, want the panel %q", step.Routing, "triage")
		}
		if step.Status == db.StepWaitingHuman {
			t.Error("implement@0 parked waiting-human; the panel was declared " +
				"precisely so a first-lane failure does not wait on an operator")
		}

		panel := mustStep(t, conn, "triage@0")
		proposalID, err := findVoteProposal(conn, panel)
		testsupport.Must(t, err, "finding triage@0's proposal: %v", err)
		if proposalID == 0 {
			t.Fatal("no proposal opened for triage@0 — the panel was never asked")
		}
		proposal, err := db.GetProposal(conn, proposalID)
		testsupport.Must(t, err, "reading the proposal: %v", err)

		// AC2: the panel cannot triage a failure it cannot see.
		rows, err := db.GateResultsForStep(conn, step.ID)
		testsupport.Must(t, err, "reading implement@0's gate rows: %v", err)
		if len(rows) == 0 {
			t.Fatal("implement@0 recorded no gate rows; the fixture proves nothing")
		}
		context := proposal.Description + "\n" + proposal.Rationale
		for _, row := range rows {
			if !strings.Contains(context, row.Gate) {
				t.Errorf("the proposal context names no gate %q; it reads %q",
					row.Gate, context)
			}
		}
		if !strings.Contains(context, "implement@0") {
			t.Errorf("the proposal context does not name the failed step; it reads %q",
				context)
		}
	})

	// AC3: every mapped outcome produces its declared routing on the failed
	// step. The two verdicts are the vote's whole vocabulary, so the four
	// routings are driven across two workflows.
	for _, tc := range []struct {
		name     string
		verdict  model.Verdict
		approved string
		rejected string
		want     string
		status   string
	}{
		{
			name: "an approving panel retries the step", verdict: model.VerdictApprove,
			approved: "retry", rejected: "abandon-issue",
			want: ResolveRetry, status: db.StepPending,
		},
		{
			name: "an approving panel buys a fix round", verdict: model.VerdictApprove,
			approved: "fix-round", rejected: "abandon-issue",
			want: workflow.OnFailFixLoop, status: db.StepSuperseded,
		},
		{
			name: "a rejecting panel abandons the issue", verdict: model.VerdictReject,
			approved: "retry", rejected: "abandon-issue",
			want: workflow.OnFailAbandonIssue, status: db.StepFailedRouted,
		},
		{
			name: "a panel declining to decide parks for an operator",
			verdict:  model.VerdictReject,
			approved: "retry", rejected: "waiting-human",
			want: workflow.OnFailWaitingHuman, status: db.StepWaitingHuman,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, e := triageRun(t, tc.approved, tc.rejected)
			castTriage(t, conn, e, tc.verdict)

			step := mustStep(t, conn, "implement@0")
			if !routingIs(step.Routing, tc.want) {
				t.Errorf("implement@0 routing = %q, want %q", step.Routing, tc.want)
			}
			if step.Status != tc.status {
				t.Errorf("implement@0 status = %q, want %q", step.Status, tc.status)
			}
		})
	}
}

// triageRun activates the fixture with the mapping filled in and drives
// implement@0 to its gate failure.
func triageRun(t *testing.T, approved, rejected string) (*sql.DB, *model.Run, *Engine) {
	t.Helper()
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	src := strings.Replace(triageSrc, "APPROVED", approved, 1)
	src = strings.Replace(src, "REJECTED", rejected, 1)
	registerSource(t, conn, []byte(src), "triage-lane.toml")

	issue := createIssue(t, conn, "triaged", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.Gates = failingGates{}
	claimAndComplete(t, conn, e, "implement@0", "the candidate", "")
	testsupport.Must(t, e.DriveRunLifecycles(conn, run.ID, nowMS),
		"driving after the failed record: %v", err)
	return conn, run, e
}

// castTriage casts the panel's ballots and drives the tally.
func castTriage(t *testing.T, conn *sql.DB, e *Engine, verdict model.Verdict) {
	t.Helper()
	panel := mustStep(t, conn, "triage@0")
	proposalID, err := findVoteProposal(conn, panel)
	testsupport.Must(t, err, "finding triage@0's proposal: %v", err)
	if proposalID == 0 {
		t.Fatal("no proposal opened for triage@0")
	}
	for _, seat := range []string{"seat-a", "seat-b"} {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID: proposalID, VoterName: seat,
			Verdict: verdict, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}
	testsupport.Must(t, e.DriveVoteProposal(conn, proposalID, nowMS),
		"driving the tally: %v", err)
}

func mustStep(t *testing.T, conn *sql.DB, instance string) *db.Step {
	t.Helper()
	step, err := db.GetStep(conn, stepIDByInstance(t, conn, instance))
	testsupport.Must(t, err, "reading %s: %v", instance, err)
	return step
}
