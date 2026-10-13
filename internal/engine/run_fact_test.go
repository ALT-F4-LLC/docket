package engine

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// runFactRun activates a run bound to a conductor capability and returns the
// run, its issue, and the token.
func runFactRun(t *testing.T, conn *sql.DB) (runID, issueID int, token string) {
	t.Helper()
	registerFixture(t, conn)
	issue := createIssue(t, conn, "facts", "a body", "task", nil)
	run := startRun(t, conn, issue)
	result, err := Activate(conn, run.ID, ActivateOptions{NowMS: nowMS})
	testsupport.Must(t, err, "activate: %v", err)
	if result.ConductorToken == "" {
		t.Fatal("premise: activation minted no conductor capability")
	}
	return run.ID, issue, result.ConductorToken
}

func factEvents(t *testing.T, conn *sql.DB, runID int, kind string) []Event {
	t.Helper()
	page, err := ListEvents(conn, EventQuery{RunID: runID, Kind: kind})
	testsupport.Must(t, err, "ListEvents: %v", err)
	return page.Events
}

var factBy = Attribution{Actor: "conductor", Cwd: "/work/repo"}

// TestRunFactRecordsEachKind is DKT-2759 criterion 2: each fact the design
// reports through `run fact add` is recorded as its named kind with its named
// fields.
func TestRunFactRecordsEachKind(t *testing.T) {
	conn := mustDB(t)
	runID, issueID, token := runFactRun(t, conn)
	proposalID := openProposal(t, conn, "the panel")
	linkProposal(t, conn, proposalID, issueID)
	stepID := stepIDByInstance(t, conn, "implement@0")

	cases := []struct {
		opts RunFactOptions
		kind string
		want map[string]any
	}{
		{RunFactOptions{Kind: RunFactVoteReseated, ProposalID: proposalID, Voter: "seat-a",
			Reason: "the seat died mid-cast"}, EventVoteReseated,
			map[string]any{"proposal": model.FormatProposalID(proposalID), "voter": "seat-a",
				"reason": "the seat died mid-cast"}},
		{RunFactOptions{Kind: RunFactStepDeferred, StepID: stepID, Cause: DeferralBudget,
			Reason: "the cap would be crossed"}, EventStepDeferred,
			map[string]any{"step": model.FormatStepID(stepID), "cause": "budget",
				"reason": "the cap would be crossed"}},
		{RunFactOptions{Kind: RunFactStepDeferred, StepID: stepID, Cause: DeferralChain,
			Reason: "waits on the earlier chain"}, EventStepDeferred,
			map[string]any{"step": model.FormatStepID(stepID), "cause": "chain",
				"reason": "waits on the earlier chain"}},
	}
	for _, c := range cases {
		before := len(factEvents(t, conn, runID, c.kind))
		c.opts.RunID, c.opts.Token, c.opts.By, c.opts.NowMS = runID, token, factBy, nowMS
		_, err := RecordRunFact(conn, c.opts)
		testsupport.Must(t, err, "RecordRunFact(%s): %v", c.kind, err)

		events := factEvents(t, conn, runID, c.kind)
		if len(events)-before != 1 {
			t.Fatalf("%s: %d events written, want 1", c.kind, len(events)-before)
		}
		var data map[string]any
		testsupport.Must(t, json.Unmarshal(events[len(events)-1].Data, &data), "decoding: %v", nil)
		c.want["actor"], c.want["cwd"] = factBy.Actor, factBy.Cwd
		for k, v := range c.want {
			if data[k] != v {
				t.Errorf("%s data[%s] = %v, want %v", c.kind, k, data[k], v)
			}
		}
		if c.kind == EventStepDeferred && events[len(events)-1].StepID != model.FormatStepID(stepID) {
			t.Errorf("step-deferred event carries step id %q, want %s",
				events[len(events)-1].StepID, model.FormatStepID(stepID))
		}
	}
}

// TestRunFactRequiresTheConductorCapability is DKT-2759 criterion 3: on a
// bound run, reporting any fact with no token is VALIDATION_ERROR and with a
// wrong token AUTH_ERROR, and no event of that kind is written.
func TestRunFactRequiresTheConductorCapability(t *testing.T) {
	conn := mustDB(t)
	runID, issueID, _ := runFactRun(t, conn)
	proposalID := openProposal(t, conn, "the panel")
	linkProposal(t, conn, proposalID, issueID)
	stepID := stepIDByInstance(t, conn, "implement@0")

	for _, base := range []RunFactOptions{
		{Kind: RunFactVoteReseated, ProposalID: proposalID, Voter: "seat-a", Reason: "r"},
		{Kind: RunFactStepDeferred, StepID: stepID, Cause: DeferralBudget, Reason: "r"},
		{Kind: RunFactStepDeferred, StepID: stepID, Cause: DeferralChain, Reason: "r"},
	} {
		for _, tc := range []struct {
			token string
			want  ErrorCode
		}{{"", CodeValidation}, {"not-the-token", CodeAuth}} {
			opts := base
			opts.RunID, opts.Token, opts.By, opts.NowMS = runID, tc.token, factBy, nowMS
			_, err := RecordRunFact(conn, opts)
			if !hasCode(err, tc.want) {
				t.Errorf("%s with token %q: err = %v, want %s", base.Kind, tc.token, err, tc.want)
			}
			if n := len(factEvents(t, conn, runID, base.Kind)); n != 0 {
				t.Errorf("%s: %d events written by refused calls, want 0", base.Kind, n)
			}
		}
	}
}

// TestRunFactRefusesFactsItCannotTie refuses a proposal not serving the run,
// a step of another run, an unknown cause, and an unknown kind.
func TestRunFactRefusesFactsItCannotTie(t *testing.T) {
	conn := mustDB(t)
	runID, _, token := runFactRun(t, conn)
	stray := openProposal(t, conn, "unlinked")
	stepID := stepIDByInstance(t, conn, "implement@0")

	for name, opts := range map[string]RunFactOptions{
		"unlinked proposal": {Kind: RunFactVoteReseated, ProposalID: stray, Voter: "s", Reason: "r"},
		"missing proposal":  {Kind: RunFactVoteReseated, ProposalID: 9999, Voter: "s", Reason: "r"},
		"unknown cause":     {Kind: RunFactStepDeferred, StepID: stepID, Cause: "weather", Reason: "r"},
		"unknown kind":      {Kind: "lunch", Reason: "r"},
		"no reason":         {Kind: RunFactStepDeferred, StepID: stepID, Cause: DeferralChain},
	} {
		opts.RunID, opts.Token, opts.By, opts.NowMS = runID, token, factBy, nowMS
		if _, err := RecordRunFact(conn, opts); err == nil {
			t.Errorf("%s: recorded, want a refusal", name)
		}
	}
	for _, kind := range []string{EventVoteReseated, EventStepDeferred} {
		if n := len(factEvents(t, conn, runID, kind)); n != 0 {
			t.Errorf("%d %s events written by refused calls, want 0", n, kind)
		}
	}
}
