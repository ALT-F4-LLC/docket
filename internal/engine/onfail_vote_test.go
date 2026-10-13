package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
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
name = "review"
after = ["implement"]
executor = "y"
emits = "verdict"

[[step]]
name = "fix"
executor = "x"
emits = "findings"
loop = true
after_loop = "review"
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

	// A PASSING executor never routes to the panel, so the panel must be
	// terminalized rather than left pending: it is interposed exactly as a
	// threshold target is, and an un-skipped one blocks the issue forever.
	t.Run("a passing step skips the panel", func(t *testing.T) {
		conn := mustDB(t)
		registerVoteRule(t, conn, "majority", "0.5", "")
		src := strings.Replace(triageSrc, "APPROVED", "retry", 1)
		src = strings.Replace(src, "REJECTED", "abandon-issue", 1)
		registerSource(t, conn, []byte(src), "triage-lane.toml")

		issue := createIssue(t, conn, "passes", "body", "task", nil)
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate: %v", err)

		e := testEngine() // PassThroughRunner: every gate passes.
		claimAndComplete(t, conn, e, "implement@0", "the candidate", "")
		testsupport.Must(t, e.DriveRunLifecycles(conn, run.ID, nowMS),
			"driving after the passing record: %v", err)

		if got := stepStatus(t, conn, "triage@0"); got != db.StepSkipped {
			t.Errorf("triage@0 status = %q, want %q — a panel nobody routed to "+
				"must be terminalized, or it blocks the issue forever",
				got, db.StepSkipped)
		}
	})

	// A panel rules ONCE per ordinal: its proposal is closed after the tally, so
	// a retried step that fails again must park for an operator rather than
	// suspend for a panel that can no longer answer.
	t.Run("a second failure after the panel ruled parks", func(t *testing.T) {
		conn, _, e := triageRun(t, "retry", "abandon-issue")
		castTriage(t, conn, e, model.VerdictApprove)
		if got := stepStatus(t, conn, "implement@0"); got != db.StepPending {
			t.Fatalf("implement@0 status = %q after the retry ruling, want %q",
				got, db.StepPending)
		}

		// The retried attempt fails on the same gates, at the same ordinal.
		claimAndComplete(t, conn, e, "implement@0", "the second candidate", "")

		step := mustStep(t, conn, "implement@0")
		if step.Status != db.StepWaitingHuman {
			t.Errorf("implement@0 status = %q after failing again, want %q — "+
				"suspending for a panel that already ruled leaves the step with "+
				"nothing able to resolve it", step.Status, db.StepWaitingHuman)
		}
		if !routingIs(step.Routing, workflow.OnFailWaitingHuman) {
			t.Errorf("implement@0 routing = %q, want %q",
				step.Routing, workflow.OnFailWaitingHuman)
		}
	})

	// An EXHAUSTED attempt budget is the other failure source `on_fail` governs,
	// and it reaches statusForRouting by its own path: without the same
	// suspension it would record `done` while naming the panel, releasing the
	// step's successors as though the work had passed.
	t.Run("an exhausted step suspends for the panel", func(t *testing.T) {
		conn := mustDB(t)
		registerVoteRule(t, conn, "majority", "0.5", "")
		src := strings.Replace(triageSrc, "APPROVED", "retry", 1)
		src = strings.Replace(src, "REJECTED", "abandon-issue", 1)
		src = strings.Replace(src, `gates = ["build"]`,
			`gates = ["build"]`+"\n"+`max_attempts = 1`, 1)
		registerSource(t, conn, []byte(src), "triage-lane.toml")

		issue := createIssue(t, conn, "exhausts", "body", "task", nil)
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate: %v", err)
		e := testEngine()

		// `max_attempts = 1` on the fixture makes the FIRST failure the
		// exhausting one, which is the routing path under test.
		stepID := stepIDByInstance(t, conn, "implement@0")
		claim, err := ClaimStep(conn, stepID, ClaimOptions{
			Owner: "worker", NowMS: nowMS})
		testsupport.Must(t, err, "claiming implement@0: %v", err)
		testsupport.Must(t, e.FailStep(conn, stepID, claim.Token,
			"the work did not finish", "", nowMS),
			"failing implement@0: %v", err)

		step := mustStep(t, conn, "implement@0")
		if step.Status == db.StepDone {
			t.Fatal("implement@0 recorded `done` naming the panel: its successors " +
				"are released as though the work had passed")
		}
		if step.Status != db.StepGated || !routingIs(step.Routing, "triage") {
			t.Errorf("implement@0 = %q routing %q, want %q routing %q",
				step.Status, step.Routing, db.StepGated, "triage")
		}
	})

	// A tally that reaches NO verdict decided nothing, so the suspension must
	// not outlive it: the step returns to an operator, which is the backstop
	// the spec documents.
	t.Run("a panel that reaches no verdict parks the step", func(t *testing.T) {
		conn, _, e := triageRun(t, "retry", "abandon-issue")
		panel := mustStep(t, conn, "triage@0")
		proposalID, err := findVoteProposal(conn, panel)
		testsupport.Must(t, err, "finding triage@0's proposal: %v", err)

		// One seat of two casts: the proposal closes without reaching quorum.
		_, err = db.CastVote(conn, &model.Vote{
			ProposalID: proposalID, VoterName: "seat-a",
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(seat-a): %v", err)
		testsupport.Must(t, db.CloseProposal(conn, proposalID, "quorum not reached"),
			"closing the proposal without a tally: %v", err)
		testsupport.Must(t, e.DriveVoteProposal(conn, proposalID, nowMS),
			"driving the closed proposal: %v", err)

		step := mustStep(t, conn, "implement@0")
		if step.Status != db.StepWaitingHuman {
			t.Errorf("implement@0 status = %q, want %q — a step left suspended "+
				"behind a closed panel has nothing able to resolve it",
				step.Status, db.StepWaitingHuman)
		}
		if got := stepStatus(t, conn, "triage@0"); !db.StepTerminal(got) {
			t.Errorf("triage@0 status = %q, want a terminal status — a panel "+
				"that closed without ruling holds its own successors", got)
		}
	})

	// THE PANEL'S OWN `on_fail` IS THE BACKSTOP, and this is where it is read:
	// the panel could not rule, so its declared routing disposes of the step it
	// was asked about. `waiting-human` (the case above) is the default; a panel
	// declaring something else must actually get it.
	t.Run("the panel's own on_fail disposes of the step", func(t *testing.T) {
		conn := mustDB(t)
		registerVoteRule(t, conn, "majority", "0.5", "")
		src := strings.Replace(triageSrc, "APPROVED", "retry", 1)
		src = strings.Replace(src, "REJECTED", "abandon-issue", 1)
		src = strings.Replace(src, `on_fail = "waiting-human"`,
			`on_fail = "abandon-issue"`, 1)
		registerSource(t, conn, []byte(src), "triage-lane.toml")

		issue := createIssue(t, conn, "no-quorum", "body", "task", nil)
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate: %v", err)

		e := testEngine()
		e.Gates = failingGates{}
		claimAndComplete(t, conn, e, "implement@0", "the candidate", "")
		testsupport.Must(t, e.DriveRunLifecycles(conn, run.ID, nowMS),
			"driving after the failed record: %v", err)

		panel := mustStep(t, conn, "triage@0")
		proposalID, err := findVoteProposal(conn, panel)
		testsupport.Must(t, err, "finding triage@0's proposal: %v", err)
		testsupport.Must(t, db.CloseProposal(conn, proposalID, "no quorum"),
			"closing the proposal: %v", err)
		testsupport.Must(t, e.DriveVoteProposal(conn, proposalID, nowMS),
			"driving the closed proposal: %v", err)

		step := mustStep(t, conn, "implement@0")
		if step.Status != db.StepFailedRouted {
			t.Errorf("implement@0 status = %q, want %q — the panel declared "+
				"`on_fail = \"abandon-issue\"`, which is the backstop the spec "+
				"names", step.Status, db.StepFailedRouted)
		}
		if got := stepStatus(t, conn, "review@0"); !db.StepTerminal(got) {
			t.Errorf("review@0 status = %q, want a terminal status — the "+
				"abandonment cascade did not run", got)
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
		// parksRun is true only where the mapping's own value is
		// `waiting-human` — the panel deliberately handing the step to an
		// operator, which parks the run exactly as any other park does.
		parksRun bool
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
			name:     "a panel declining to decide parks for an operator",
			verdict:  model.VerdictReject,
			approved: "retry", rejected: "waiting-human",
			want: workflow.OnFailWaitingHuman, status: db.StepWaitingHuman,
			parksRun: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, run, e := triageRun(t, tc.approved, tc.rejected)
			castTriage(t, conn, e, tc.verdict)

			step := mustStep(t, conn, "implement@0")
			if !routingIs(step.Routing, tc.want) {
				t.Errorf("implement@0 routing = %q, want %q", step.Routing, tc.want)
			}
			if step.Status != tc.status {
				t.Errorf("implement@0 status = %q, want %q", step.Status, tc.status)
			}

			// A PANEL THAT ANSWERED IS DONE. Its own `on_fail` is the backstop
			// for a tally that reached NO verdict; firing it on a rejection the
			// mapping already consumed would park the panel, and R2b would then
			// hold the whole issue behind a question that was answered.
			panel := mustStep(t, conn, "triage@0")
			if panel.Status != db.StepDone {
				t.Errorf("triage@0 status = %q, want %q — the panel reached a "+
					"verdict and the mapping applied it", panel.Status, db.StepDone)
			}
			parked := runStatusOf(t, conn, run.ID) == string(model.RunWaitingHuman)
			if parked != tc.parksRun {
				t.Errorf("run parked = %v, want %v — a panel's verdict parks the "+
					"run only where the mapping itself says `waiting-human`",
					parked, tc.parksRun)
			}

			// THE ISSUE-LEVEL EFFECT, not just the step's row. `abandon-issue`
			// runs a cascade over the issue's remaining steps; a routing written
			// without it would read as abandoned while the issue kept
			// scheduling work.
			if tc.want == workflow.OnFailAbandonIssue {
				if got := stepStatus(t, conn, "review@0"); !db.StepTerminal(got) {
					t.Errorf("review@0 status = %q after the issue was abandoned, "+
						"want a terminal status — the cascade did not run", got)
				}
				if got := issueStatus(t, conn, step.IssueID); got == string(model.StatusDone) {
					t.Errorf("issue status = %q after the panel abandoned it — an "+
						"abandoned issue is not a completed one", got)
				}
				assertTriageAbandonedOnce(t, conn, step)
			}
		})
	}
}

// TestCommittedTriagePanelEventsMatchSkippedRow: an operator's manual commit
// is an outcome the triage mapping has no key for, so the panel closes
// `skipped` and its own `on_fail` disposes of the step it was asked about.
// Every event recorded for the panel must agree with that row: a `pass`
// routing event beside a `skipped` row tells an event consumer the panel
// passed when it did not.
func TestCommittedTriagePanelEventsMatchSkippedRow(t *testing.T) {
	conn, _, e := triageRun(t, "retry", "abandon-issue")
	panel := mustStep(t, conn, "triage@0")
	proposalID, err := findVoteProposal(conn, panel)
	testsupport.Must(t, err, "finding triage@0's proposal: %v", err)
	if proposalID == 0 {
		t.Fatal("no proposal opened for triage@0")
	}
	_, err = conn.Exec(`UPDATE proposals SET status = ? WHERE id = ?`,
		model.ProposalStatusCommitted, proposalID)
	testsupport.Must(t, err, "committing the proposal: %v", err)
	testsupport.Must(t, e.DriveVoteProposal(conn, proposalID, nowMS),
		"driving the committed proposal: %v", err)

	panel = mustStep(t, conn, "triage@0")
	if panel.Status != db.StepSkipped {
		t.Fatalf("triage@0 status = %q, want %q — a committed outcome is "+
			"outside the mapping's vocabulary, so the panel closes without ruling",
			panel.Status, db.StepSkipped)
	}

	rows, err := conn.Query(
		`SELECT kind, data FROM events WHERE step_id = ? ORDER BY seq`, panel.ID)
	testsupport.Must(t, err, "reading triage@0's events: %v", err)
	defer rows.Close()
	skipped := 0
	var seen []string
	for rows.Next() {
		var kind, data string
		testsupport.Must(t, rows.Scan(&kind, &data), "scanning an event")
		var fields struct {
			Detail string `json:"detail"`
		}
		testsupport.Must(t, json.Unmarshal([]byte(data), &fields),
			"decoding a %s event's data", kind)
		seen = append(seen, kind+" "+fields.Detail)
		switch kind {
		case EventStepRouted:
			t.Errorf("triage@0 has a %s event %q beside a %q row — the panel's "+
				"disposition is its skip, and no routing names it",
				kind, fields.Detail, panel.Status)
		case EventStepSkipped:
			skipped++
			if !strings.Contains(fields.Detail, "without reaching a verdict") {
				t.Errorf("triage@0's %s event reads %q, want the reason the "+
					"panel closed without ruling", kind, fields.Detail)
			}
		default:
			if fields.Detail == RoutingPass ||
				strings.HasPrefix(fields.Detail, RoutingPass+":") {
				t.Errorf("triage@0 has a %s event reporting routing %q beside "+
					"a %q row", kind, fields.Detail, panel.Status)
			}
		}
	}
	testsupport.Must(t, rows.Err(), "iterating triage@0's events: %v", rows.Err())
	if skipped != 1 {
		t.Errorf("triage@0 recorded %d %s events, want exactly 1; events: %q",
			skipped, EventStepSkipped, seen)
	}
}

// TestTriageUnmappedVerdict: a tally that DECIDES a verdict its
// `on_fail_routes` has no key for answered a question the mapping cannot
// apply, so the panel's own `on_fail` disposes of the triaged step exactly as
// it does for a tally that reached no verdict. Leaving the step `gated` would
// strand it: nothing else resolves a suspension once the proposal is closed.
func TestTriageUnmappedVerdict(t *testing.T) {
	for _, tc := range []struct {
		onFail string
		want   string
		status string
	}{
		{onFail: "waiting-human", want: workflow.OnFailWaitingHuman, status: db.StepWaitingHuman},
		{onFail: "abandon-issue", want: workflow.OnFailAbandonIssue, status: db.StepFailedRouted},
		{onFail: "fix-loop", want: workflow.OnFailFixLoop, status: db.StepSuperseded},
	} {
		t.Run(tc.onFail, func(t *testing.T) {
			conn := mustDB(t)
			registerVoteRule(t, conn, "majority", "0.5", "")
			src := strings.Replace(triageSrc, `approved = "APPROVED"`+"\n", "", 1)
			src = strings.Replace(src, "REJECTED", "abandon-issue", 1)
			src = strings.Replace(src, `on_fail = "waiting-human"`,
				`on_fail = "`+tc.onFail+`"`, 1)
			registerSource(t, conn, []byte(src), "triage-lane.toml")

			issue := createIssue(t, conn, "unmapped", "body", "task", nil)
			run := startRun(t, conn, issue)
			_, err := activate(conn, run.ID)
			testsupport.Must(t, err, "activate: %v", err)

			e := testEngine()
			e.Gates = failingGates{}
			claimAndComplete(t, conn, e, "implement@0", "the candidate", "")
			testsupport.Must(t, e.DriveRunLifecycles(conn, run.ID, nowMS),
				"driving after the failed record: %v", err)
			castTriage(t, conn, e, model.VerdictApprove)

			step := mustStep(t, conn, "implement@0")
			if step.Status == db.StepGated {
				t.Fatalf("implement@0 is still %q after the panel decided — "+
					"an unmapped verdict left it with nothing able to resolve it",
					step.Status)
			}
			if !routingIs(step.Routing, tc.want) {
				t.Errorf("implement@0 routing = %q, want %q", step.Routing, tc.want)
			}
			if step.Status != tc.status {
				t.Errorf("implement@0 status = %q, want %q", step.Status, tc.status)
			}
			if got := stepStatus(t, conn, "triage@0"); got != db.StepSkipped {
				t.Errorf("triage@0 status = %q, want %q — the mapping applied "+
					"nothing, so the panel closes without ruling", got, db.StepSkipped)
			}
			if tc.want == workflow.OnFailAbandonIssue {
				if got := issueStatus(t, conn, issue); got == string(model.StatusDone) {
					t.Errorf("issue status = %q after the panel's on_fail abandoned "+
						"it — an abandoned issue is not a completed one", got)
				}
				assertTriageAbandonedOnce(t, conn, step)
			}
		})
	}
}

// assertTriageAbandonedOnce: a panel's abandonment is ONE ruling about the
// issue, recorded under the step the panel triaged and carrying the panel's
// note. A second ruling under the panel's own row is the one the
// last-ruling-wins readers report, with no reason, in place of the first.
func assertTriageAbandonedOnce(t *testing.T, conn *sql.DB, triaged *db.Step) {
	t.Helper()
	rows, err := conn.Query(
		`SELECT step_id FROM events WHERE kind = ? AND issue_id = ? ORDER BY seq`,
		EventIssueAbandoned, triaged.IssueID)
	testsupport.Must(t, err, "reading issue-abandoned events: %v", err)
	defer rows.Close()
	var stepIDs []int
	for rows.Next() {
		var id int
		testsupport.Must(t, rows.Scan(&id), "scanning an issue-abandoned event")
		stepIDs = append(stepIDs, id)
	}
	testsupport.Must(t, rows.Err(), "iterating issue-abandoned events: %v", rows.Err())
	if len(stepIDs) != 1 || stepIDs[0] != triaged.ID {
		t.Errorf("issue-abandoned events by step id = %v, want exactly one, "+
			"by %s (id %d)", stepIDs, triaged.Instance, triaged.ID)
	}

	abandonedComments := 0
	for _, body := range engineCommentBodies(t, conn, triaged.IssueID) {
		if strings.Contains(body, "abandoned the issue.") {
			abandonedComments++
		}
	}
	if abandonedComments != 1 {
		t.Errorf("%d engine comments narrate the abandonment, want exactly 1",
			abandonedComments)
	}

	disposition, err := LatestIssueDisposition(conn, triaged.IssueID)
	testsupport.Must(t, err, "LatestIssueDisposition: %v", err)
	if disposition == nil {
		t.Fatal("LatestIssueDisposition = nil for an abandoned issue")
	}
	if disposition.By != triaged.Instance {
		t.Errorf("disposition by = %q, want %q — the step the panel ruled on",
			disposition.By, triaged.Instance)
	}
	if !strings.Contains(disposition.Reason, "triage@0") {
		t.Errorf("disposition reason = %q, want the panel's ruling note",
			disposition.Reason)
	}

	issue, err := db.GetIssue(conn, triaged.IssueID)
	testsupport.Must(t, err, "GetIssue: %v", err)
	if issue.Resolution != db.IssueResolutionAbandoned {
		t.Errorf("issue resolution = %q, want %q",
			issue.Resolution, db.IssueResolutionAbandoned)
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
	if err := e.DriveRunLifecycles(conn, run.ID, nowMS); err != nil {
		t.Fatalf("driving after the failed record: %v", err)
	}
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
	if err := e.DriveVoteProposal(conn, proposalID, nowMS); err != nil {
		t.Fatalf("driving the tally: %v", err)
	}
}

func mustStep(t *testing.T, conn *sql.DB, instance string) *db.Step {
	t.Helper()
	step, err := db.GetStep(conn, stepIDByInstance(t, conn, instance))
	testsupport.Must(t, err, "reading %s: %v", instance, err)
	return step
}

// loopTriageSrc puts a triage panel behind the LOOP BODY as well as the first
// executor: `fix` names the panel in its `on_fail`, and the panel is ordered
// after both. A failed `review` gate routes `fix-loop`, which is the fixture's
// way into ordinal 1.
const loopTriageSrc = `
[pipeline]
name = "loop-triage-lane"
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
after = ["implement", "fix"]
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
on_fail = "waiting-human"

[step.on_fail_routes]
approved = "fix-round"
rejected = "abandon-issue"

[[step]]
name = "review"
after = ["implement"]
executor = "y"
emits = "verdict"
gates = ["verdict"]
on_fail = "fix-loop"

[[step]]
name = "fix"
executor = "x"
emits = "findings"
gates = ["build"]
on_fail = "triage"
loop = true
after_loop = "review"
`

// TestLoopBodyFailureRoutesToItsOrdinalsPanel: a loop body whose `on_fail`
// names a triage panel needs that panel at the body's own ordinal. Without
// panel@k a failure at ordinal k >= 1 has nothing to suspend for, and the step
// strands with no panel and no release.
func TestLoopBodyFailureRoutesToItsOrdinalsPanel(t *testing.T) {
	t.Run("a failing round suspends for the ordinal's panel", func(t *testing.T) {
		conn, _, run, e := enterOrdinalOne(t, loopTriageSrc, nil)
		panelExists := stepExists(t, conn, "triage@1")
		if !panelExists {
			t.Error("no triage@1 row after the lane entered ordinal 1: fix@1 " +
				"names the panel in its on_fail, and a failure there would " +
				"have no panel to suspend for")
		}
		claimAndComplete(t, conn, e, "fix@1", "the round-1 candidate", "")
		err := e.DriveRunLifecycles(conn, run.ID, nowMS)
		testsupport.Must(t, err, "driving after fix@1's failure: %v", err)

		// Suspended FOR triage@1: a `gated` row naming the panel with no panel
		// row at its ordinal is the strand, not the suspension.
		step := mustStep(t, conn, "fix@1")
		if step.Status != db.StepGated || !routingIs(step.Routing, "triage") ||
			!panelExists {
			t.Errorf("fix@1 = %q routing %q with triage@1 present = %v, want "+
				"%q routing %q with the panel present", step.Status,
				step.Routing, panelExists, db.StepGated, "triage")
		}
		if !panelExists {
			t.FailNow()
		}

		panel := mustStep(t, conn, "triage@1")
		proposalID, err := findVoteProposal(conn, panel)
		testsupport.Must(t, err, "finding triage@1's proposal: %v", err)
		if proposalID == 0 {
			t.Fatal("no proposal opened for triage@1 — the panel was never asked")
		}
		proposal, err := db.GetProposal(conn, proposalID)
		testsupport.Must(t, err, "reading the proposal: %v", err)
		if proposal.Status != model.ProposalStatusOpen {
			t.Errorf("triage@1's proposal status = %q, want %q",
				proposal.Status, model.ProposalStatusOpen)
		}
		context := proposal.Description + "\n" + proposal.Rationale
		if !strings.Contains(context, "fix@1") {
			t.Errorf("triage@1's proposal does not name fix@1's failure; it "+
				"reads %q", context)
		}
	})

	t.Run("a passing round skips the ordinal's panel", func(t *testing.T) {
		conn, _, run, e := enterOrdinalOne(t, loopTriageSrc, nil)
		e.Gates = PassThroughRunner{}
		claimAndComplete(t, conn, e, "fix@1", "the round-1 candidate", "")
		err := e.DriveRunLifecycles(conn, run.ID, nowMS)
		testsupport.Must(t, err, "driving after fix@1 passed: %v", err)

		if got := stepStatus(t, conn, "triage@1"); got != db.StepSkipped {
			t.Errorf("triage@1 status = %q, want %q — a panel nobody routed "+
				"to must be terminalized, or it blocks the issue forever",
				got, db.StepSkipped)
		}
		if ready := readyInstances(t, conn); !slices.Contains(ready, "review@1") {
			t.Errorf("review@1 is not ready after fix@1 passed; ready = %q", ready)
		}
	})
}

// whenGatedFix gives loopTriageSrc's `fix` body a `when` that holds only for an
// issue labeled run-fix.
var whenGatedFix = strings.Replace(loopTriageSrc,
	"on_fail = \"triage\"\nloop = true",
	"on_fail = \"triage\"\nwhen = \"labels contains run-fix\"\nloop = true", 1)

// enterOrdinalOneWith is enterOrdinalOne with the engine switched to pass every
// gate on return.
func enterOrdinalOneWith(
	t *testing.T, src string, labels []string,
) (*sql.DB, int, *model.Run, *Engine) {
	t.Helper()
	conn, issue, run, e := enterOrdinalOne(t, src, labels)
	e.Gates = PassThroughRunner{}
	return conn, issue, run, e
}

// enterOrdinalOne registers the majority vote rule and src, creates an issue
// carrying labels, starts and activates its run, passes implement@0, fails
// review@0's gate into a fix round, and returns with fix@1 instantiated and
// nothing at ordinal 1 recorded yet. It is the one ordinal-one triage lane
// setup; the engine it returns fails every gate.
func enterOrdinalOne(
	t *testing.T, src string, labels []string,
) (*sql.DB, int, *model.Run, *Engine) {
	t.Helper()
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(src), "loop-triage-lane.toml")

	issue := createIssue(t, conn, "loop-triaged", "body", "task", labels)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	claimAndComplete(t, conn, e, "implement@0", "the candidate", "")
	e.Gates = failingGates{}
	claimAndComplete(t, conn, e, "review@0", "the verdict", "")
	err = e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after review@0's failure: %v", err)
	if !stepExists(t, conn, "fix@1") {
		t.Fatal("premise: review@0's fix-loop did not enter ordinal 1")
	}
	return conn, issue, run, e
}

// TestWhenFalseLoopBodyPanelDoesNotStrandAfterLoop: a loop body skipped by its
// `when` at ordinal k never runs, so nothing routes to or skips its on_fail
// panel. A pending panel@k would hold the after_loop root and the issue
// forever, so the expansion must leave it non-blocking.
func TestWhenFalseLoopBodyPanelDoesNotStrandAfterLoop(t *testing.T) {
	conn, issue, run, e := enterOrdinalOneWith(t, whenGatedFix, nil)

	if got := stepStatus(t, conn, "fix@1"); got != db.StepSkipped {
		t.Fatalf("premise: fix@1 status = %q, want %q", got, db.StepSkipped)
	}
	if stepExists(t, conn, "triage@1") {
		if got := stepStatus(t, conn, "triage@1"); got != db.StepSkipped {
			t.Errorf("triage@1 status = %q, want absent or %q: no body at "+
				"ordinal 1 can route to it", got, db.StepSkipped)
		}
	}
	if ready := readyInstances(t, conn); !slices.Contains(ready, "review@1") {
		t.Errorf("review@1 is not ready with fix@1 skipped; ready = %q", ready)
	}

	claimAndComplete(t, conn, e, "review@1", "the round-1 verdict", "")
	err := e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after review@1 passed: %v", err)
	if got := stepStatus(t, conn, "review@1"); got != db.StepDone {
		t.Errorf("review@1 status = %q, want %q", got, db.StepDone)
	}
	if got := issueStatus(t, conn, issue); got != "done" {
		t.Errorf("issue status = %q, want %q", got, "done")
	}
}

// TestWhenTrueLoopBodyPanelIsInstantiated: a body whose `when` holds can fail
// at ordinal k, so its panel must exist there.
func TestWhenTrueLoopBodyPanelIsInstantiated(t *testing.T) {
	conn, _, _, _ := enterOrdinalOneWith(t, whenGatedFix, []string{"run-fix"})

	if got := stepStatus(t, conn, "fix@1"); got != db.StepPending {
		t.Fatalf("premise: fix@1 status = %q, want %q", got, db.StepPending)
	}
	if !stepExists(t, conn, "triage@1") {
		t.Error("no triage@1 row: fix@1 runs at ordinal 1 and names the " +
			"panel in its on_fail")
	}
}

// TestWhenMixedLoopBodyPanelIsInstantiated: when two bodies name one panel and
// only one of them runs, the panel is still needed for that one.
func TestWhenMixedLoopBodyPanelIsInstantiated(t *testing.T) {
	src := strings.Replace(whenGatedFix,
		`after = ["implement", "fix"]`,
		`after = ["implement", "fix", "lint-fix"]`, 1) + `
[[step]]
name = "lint-fix"
executor = "x"
emits = "findings"
gates = ["build"]
on_fail = "triage"
when = "labels contains run-lint-fix"
loop = true
after_loop = "review"
`
	conn, _, _, _ := enterOrdinalOneWith(t, src, []string{"run-fix"})

	if got := stepStatus(t, conn, "fix@1"); got != db.StepPending {
		t.Fatalf("premise: fix@1 status = %q, want %q", got, db.StepPending)
	}
	if got := stepStatus(t, conn, "lint-fix@1"); got != db.StepSkipped {
		t.Fatalf("premise: lint-fix@1 status = %q, want %q", got, db.StepSkipped)
	}
	if !stepExists(t, conn, "triage@1") {
		t.Error("no triage@1 row: fix@1 runs at ordinal 1 and names the " +
			"panel, even though lint-fix@1 is skipped")
	}
}

// TestFirstLaneFailureOpensPanelOrderedAfterLoopBody: a panel ordered after a
// loop body is still the first lane's panel at ordinal 0, where the body has no
// instance until a fix round enters it. A failed implement@0 must reach it, or
// the suspension strands with no panel able to answer.
func TestFirstLaneFailureOpensPanelOrderedAfterLoopBody(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(loopTriageSrc), "loop-triage-lane.toml")

	issue := createIssue(t, conn, "first-lane triaged", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	e.Gates = failingGates{}
	claimAndComplete(t, conn, e, "implement@0", "the candidate", "")
	err = e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after implement@0's failure: %v", err)

	if stepExists(t, conn, "fix@0") {
		t.Fatal("premise: fix@0 exists; the loop body must be unentered at ordinal 0")
	}
	step := mustStep(t, conn, "implement@0")
	if step.Status != db.StepGated || !routingIs(step.Routing, "triage") {
		t.Errorf("implement@0 = %q routing %q, want %q routing %q",
			step.Status, step.Routing, db.StepGated, "triage")
	}

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ready, cond := sched.Ready(stepNamed(t, sched, "triage@0")); !ready {
			t.Errorf("triage@0 blocked by %q: implement@0 is suspended on it, and "+
				"the unentered loop body fix has no instance to wait for", cond)
		}
	})

	panel := mustStep(t, conn, "triage@0")
	proposalID, err := findVoteProposal(conn, panel)
	testsupport.Must(t, err, "finding triage@0's proposal: %v", err)
	if proposalID == 0 {
		t.Fatal("no proposal opened for triage@0 — implement@0 strands")
	}
	proposal, err := db.GetProposal(conn, proposalID)
	testsupport.Must(t, err, "reading the proposal: %v", err)
	if proposal.Status != model.ProposalStatusOpen {
		t.Errorf("triage@0's proposal status = %q, want %q",
			proposal.Status, model.ProposalStatusOpen)
	}
}

// TestSuccessorOfUnenteredLoopBodyWaits: the ordinal-0 exemption belongs to a
// panel a suspended router names. An ordinary step ordered after the loop body
// still waits for the body, so review-like work is not released before the
// loop is entered. implement@0 passes, which leaves the unentered `fix` as the
// only predecessor able to hold audit@0.
func TestSuccessorOfUnenteredLoopBodyWaits(t *testing.T) {
	src := loopTriageSrc + `
[[step]]
name = "audit"
after = ["implement", "fix"]
executor = "y"
emits = "verdict"
`
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(src), "loop-triage-lane.toml")

	issue := createIssue(t, conn, "loop successor", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	claimAndComplete(t, conn, e, "implement@0", "the candidate", "")
	err = e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after implement@0 passed: %v", err)
	if got := stepStatus(t, conn, "implement@0"); got != db.StepDone {
		t.Fatalf("premise: implement@0 status = %q, want %q", got, db.StepDone)
	}

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		ready, cond := sched.Ready(stepNamed(t, sched, "audit@0"))
		if ready {
			t.Fatal("audit@0 is ready at ordinal 0 although its predecessor fix " +
				"has not been entered and no suspended router names audit")
		}
		if cond != CondPredecessors {
			t.Errorf("audit@0 blocked by %q, want %q", cond, CondPredecessors)
		}
	})
}

// loopThresholdSrc gives the loop body a `threshold` that routes BY NAME to
// `drain`, a step ordered after the body and outside review's after_loop chain.
// A failed `review` gate routes `fix-loop` into ordinal 1, as in loopTriageSrc.
const loopThresholdSrc = `
[pipeline]
name = "loop-threshold-lane"
version = 1

[match]
kind = ["task"]

[[step]]
name = "implement"
after = []
executor = "x"
emits = "candidate"

[[step]]
name = "review"
after = ["implement"]
executor = "y"
emits = "verdict"
gates = ["verdict"]
on_fail = "fix-loop"

[[step]]
name = "fix"
executor = "x"
emits = "report"
loop = true
after_loop = "review"
threshold = { "drain" = "any(status == blocked)" }

[[step]]
name = "drain"
after = ["fix"]
executor = "d"
emits = "drain-report"
`

// TestLoopBodyThresholdRoutesToItsOrdinalsTarget: a body at ordinal k whose
// threshold routes by name needs the target at ordinal k. A target left at
// ordinal 0 resolves its routing predecessor at ordinal 0 or below, never sees
// fix@1's routing, and stays pending while fix@1 records `done`.
func TestLoopBodyThresholdRoutesToItsOrdinalsTarget(t *testing.T) {
	conn, _, run, e := enterOrdinalOneWith(t, loopThresholdSrc, nil)

	claimAndComplete(t, conn, e, "fix@1", "blocked", `[{"status":"blocked"}]`)
	err := e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after fix@1 routed to drain: %v", err)
	if step := mustStep(t, conn, "fix@1"); !routingIs(step.Routing, "drain") {
		t.Fatalf("premise: fix@1 routing = %q, want %q", step.Routing, "drain")
	}

	if !stepExists(t, conn, "drain@1") {
		t.Fatal("no drain@1 row: fix@1 routed to drain at ordinal 1, and a " +
			"target left at ordinal 0 cannot see that routing")
	}
	if ready := readyInstances(t, conn); !slices.Contains(ready, "drain@1") {
		t.Fatalf("drain@1 is not ready after fix@1 routed to it; ready = %q", ready)
	}
	claimAndComplete(t, conn, e, "drain@1", "drained", "")
	if got := stepStatus(t, conn, "drain@1"); got != db.StepDone {
		t.Errorf("drain@1 status = %q, want %q", got, db.StepDone)
	}
}

// TestLoopBodyThresholdUnroutedTargetIsSkipped: when fix@1 routes elsewhere,
// its ordinal's target is terminalized `skipped` and the issue completes
// rather than waiting on a pending target.
func TestLoopBodyThresholdUnroutedTargetIsSkipped(t *testing.T) {
	conn, issue, run, e := enterOrdinalOneWith(t, loopThresholdSrc, nil)

	claimAndComplete(t, conn, e, "fix@1", "clear", `[{"status":"ok"}]`)
	err := e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after fix@1 passed: %v", err)

	if !stepExists(t, conn, "drain@1") {
		t.Fatal("no drain@1 row: fix@1 can route to drain at ordinal 1")
	}
	if got := stepStatus(t, conn, "drain@1"); got != db.StepSkipped {
		t.Errorf("drain@1 status = %q, want %q — a target nobody routed to "+
			"must be terminalized, or it blocks the issue", got, db.StepSkipped)
	}

	claimAndComplete(t, conn, e, "review@1", "the round-1 verdict", "")
	err = e.DriveRunLifecycles(conn, run.ID, nowMS)
	testsupport.Must(t, err, "driving after review@1 passed: %v", err)
	if got := issueStatus(t, conn, issue); got != "done" {
		t.Errorf("issue status = %q, want %q", got, "done")
	}
}
