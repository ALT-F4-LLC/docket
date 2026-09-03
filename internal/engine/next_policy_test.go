package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1282: `next --run`'s executor and vote rows resolved from the run's
// pinned policy.toml — see policy.go/policy_resolve.go for the grammar and
// walk, policy_pin.go for the pin-and-parse plumbing this file exercises
// end to end through activation and NextSteps.

// policyFixtureWorkflow is one immediately-ready executor step and one
// immediately-ready vote step — both `after = []`, so both appear in the
// SAME first `next --run` offer with no claim or vote cast needed.
const policyFixtureWorkflow = `
[pipeline]
name = "policy-fixture"
version = 1

[match]
kind = ["task"]

[[step]]
name = "work"
after = []
executor = "worker"
emits = "out"

[[step]]
name = "gate"
after = []
type = "vote"
voters = ["seat-a", "seat-b"]
vote_rule = "majority"
on_fail = "waiting-human"
`

const policyFixtureTOML = `
[policy]
version = 2

[variants]
tier-a = { model = "opus", effort = "high" }
tier-b = { model = "sonnet", effort = "medium" }

[executors]
worker = { variant = "tier-a" }
seat-a = { variant = "tier-a" }
seat-b = { variant = "tier-b" }
`

// policyFixtureRun activates policyFixtureWorkflow, writing policyToml into
// the config dir when non-empty (the dormancy test passes "").
func policyFixtureRun(t *testing.T, policyToml string) (*sql.DB, int) {
	t.Helper()
	conn, dir := configRepo(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	writeConfigFile(t, dir, "workflows/policy-fixture.toml", policyFixtureWorkflow)
	if policyToml != "" {
		writeConfigFile(t, dir, "policy.toml", policyToml)
	}
	issue := createIssue(t, conn, "policy fixture", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return conn, run.ID
}

// findRow returns the offer row whose instance is `name@...`, or nil.
func findRow(rows []model.StepRow, name string) *model.StepRow {
	for i := range rows {
		if strings.HasPrefix(rows[i].Instance, name+"@") {
			return &rows[i]
		}
	}
	return nil
}

// TestNextStepsResolvesExecutorRowFromPinnedPolicy is AC1's executor half:
// the row's model/effort/variant come from the run's pinned policy.toml.
func TestNextStepsResolvesExecutorRowFromPinnedPolicy(t *testing.T) {
	conn, runID := policyFixtureRun(t, policyFixtureTOML)
	e := testEngine()

	next, err := e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)

	row := findRow(next.Steps, "work")
	if row == nil {
		t.Fatalf("no offer row named %q; offer was %+v", "work", next.Steps)
	}
	if row.Model != "opus" || row.Effort != "high" || row.Variant != "tier-a" {
		t.Errorf("work row model/effort/variant = %q/%q/%q, want opus/high/tier-a",
			row.Model, row.Effort, row.Variant)
	}
}

// TestNextStepsResolvesVoteRowVoters is AC1's vote half: each voter resolves
// independently, in Voters order.
func TestNextStepsResolvesVoteRowVoters(t *testing.T) {
	conn, runID := policyFixtureRun(t, policyFixtureTOML)
	e := testEngine()

	next, err := e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)

	row := findRow(next.Steps, "gate")
	if row == nil {
		t.Fatalf("no offer row named %q; offer was %+v", "gate", next.Steps)
	}
	if len(row.VoterAssignments) != 2 {
		t.Fatalf("got %d voter assignments, want 2: %+v", len(row.VoterAssignments), row.VoterAssignments)
	}
	want := map[string][3]string{
		"seat-a": {"opus", "high", "tier-a"},
		"seat-b": {"sonnet", "medium", "tier-b"},
	}
	for _, va := range row.VoterAssignments {
		w, ok := want[va.Voter]
		if !ok {
			t.Errorf("unexpected voter %q in assignments", va.Voter)
			continue
		}
		if va.Model != w[0] || va.Effort != w[1] || va.Variant != w[2] {
			t.Errorf("voter %s model/effort/variant = %q/%q/%q, want %q/%q/%q",
				va.Voter, va.Model, va.Effort, va.Variant, w[0], w[1], w[2])
		}
	}
}

// TestNextStepsIsDormantWithNoPinnedPolicy is AC3's dormancy half: a run
// whose corpus ships no policy.toml carries no model/effort/variant on any
// row — byte-identical to `next --run` before this feature existed.
func TestNextStepsIsDormantWithNoPinnedPolicy(t *testing.T) {
	conn, runID := policyFixtureRun(t, "")
	e := testEngine()

	next, err := e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)

	work := findRow(next.Steps, "work")
	if work == nil {
		t.Fatalf("no offer row named %q; offer was %+v", "work", next.Steps)
	}
	if work.Model != "" || work.Effort != "" || work.Variant != "" {
		t.Errorf("work row carries policy fields with no pinned policy.toml: %+v", work)
	}

	gate := findRow(next.Steps, "gate")
	if gate == nil {
		t.Fatalf("no offer row named %q; offer was %+v", "gate", next.Steps)
	}
	if len(gate.VoterAssignments) != 0 {
		t.Errorf("gate row carries voter assignments with no pinned policy.toml: %+v", gate.VoterAssignments)
	}
}

// TestNextStepsRefusesAnUnresolvableExecutorHint: a step whose executor hint
// has no [executors] row is a policy/workflow mismatch the caller must see,
// not a row silently missing its routing fields.
func TestNextStepsRefusesAnUnresolvableExecutorHint(t *testing.T) {
	conn, dir := configRepo(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	writeConfigFile(t, dir, "workflows/policy-fixture.toml", policyFixtureWorkflow)
	// A policy.toml that never mentions "worker" at all.
	writeConfigFile(t, dir, "policy.toml", `
[policy]
version = 2

[variants]
tier-a = { model = "opus", effort = "high" }

[executors]
seat-a = { variant = "tier-a" }
seat-b = { variant = "tier-a" }
`)
	issue := createIssue(t, conn, "unresolvable", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	e := testEngine()
	if _, err := e.NextSteps(conn, run.ID, 0, nowMS); err == nil {
		t.Error("want an error when a ready executor row's hint has no [executors] row in the pinned policy")
	}
}
