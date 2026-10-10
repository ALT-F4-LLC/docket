package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// `policy resolve` — the seat lookup a CONVERSATIONAL gate needs, which has
// no step row for routing to ride on. Every assertion here is
// parity with the vote step row the same policy resolves, since the point of
// the verb is that it is the same read and not a second implementation of it.

// securityPolicyTOML exercises all three [security] mechanisms on seats a
// vote row can also carry:
//
//   - seat-a is sensitive BY NODE: its standing tier-a lies beyond the
//     ceiling, so it clamps to `guarded`.
//   - seat-b is sensitive only BY LABEL: with the label its `forbidden`
//     standing variant is an opus one, which [security].never bans and
//     [escalation.fallback] redirects to `safe`; without it, it stands.
const securityPolicyTOML = `
[policy]
version = 2

[variants]
guarded = { model = "sonnet", effort = "medium", escalate_to = "tier-a" }
tier-a = { model = "opus", effort = "high" }
forbidden = { model = "opus", effort = "low" }
safe = { model = "haiku", effort = "low" }

[executors]
worker = { variant = "guarded" }
seat-a = { variant = "tier-a" }
seat-b = { variant = "forbidden" }

[security]
ceiling = "guarded"
nodes = ["seat-a"]
labels = ["sensitive"]
never = ["opus"]

[escalation.fallback]
forbidden = "safe"
`

// TestResolveSeatsMatchesVoteRowAssignments is AC1: one call answers every
// seat, with the tuple the run's vote step row carries for the same seats.
func TestResolveSeatsMatchesVoteRowAssignments(t *testing.T) {
	conn, runID := policyFixtureRun(t, policyFixtureTOML)

	next, err := testEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	row := findRow(next.Steps, "gate")
	if row == nil {
		t.Fatalf("no offer row named %q; offer was %+v", "gate", next.Steps)
	}

	got, err := ResolveSeats(conn, runID, []string{"seat-a", "seat-b"}, nil)
	testsupport.Must(t, err, "ResolveSeats: %v", err)

	assertSeatsEqual(t, got, row.VoterAssignments)
	if got[0].Model != "opus" || got[0].Effort != "high" || got[0].Variant != "tier-a" {
		t.Errorf("seat-a = %+v, want opus/high/tier-a", got[0])
	}
	if got[1].Model != "sonnet" || got[1].Effort != "medium" || got[1].Variant != "tier-b" {
		t.Errorf("seat-b = %+v, want sonnet/medium/tier-b", got[1])
	}
}

// TestResolveSeatsOrderFollowsTheRequest: the answer is positional, so a
// caller pairing seats with its own list by index is not silently misrouted.
func TestResolveSeatsOrderFollowsTheRequest(t *testing.T) {
	conn, runID := policyFixtureRun(t, policyFixtureTOML)

	got, err := ResolveSeats(conn, runID, []string{"seat-b", "seat-a"}, nil)
	testsupport.Must(t, err, "ResolveSeats: %v", err)

	if len(got) != 2 || got[0].Voter != "seat-b" || got[1].Voter != "seat-a" {
		t.Errorf("got %+v, want seat-b then seat-a", got)
	}
}

// TestResolveSeatsAppliesSecurityByNode is AC2's node half: a seat named in
// [security].nodes is clamped to the ceiling — and to the SAME value the vote
// row resolves it to.
func TestResolveSeatsAppliesSecurityByNode(t *testing.T) {
	conn, runID := policyFixtureRun(t, securityPolicyTOML)

	next, err := testEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	row := findRow(next.Steps, "gate")
	if row == nil {
		t.Fatalf("no offer row named %q; offer was %+v", "gate", next.Steps)
	}

	got, err := ResolveSeats(conn, runID, []string{"seat-a"}, nil)
	testsupport.Must(t, err, "ResolveSeats: %v", err)

	if got[0].Variant != "guarded" || got[0].Model != "sonnet" || got[0].Effort != "medium" {
		t.Errorf("seat-a = %+v, want the ceiling sonnet/medium/guarded", got[0])
	}
	assertSeatsEqual(t, got, row.VoterAssignments[:1])
}

// TestResolveSeatsAppliesSecurityByLabel is AC2's label half: --label decides
// sensitivity for a gate that has no row to snapshot labels from, and the
// merged never-list then redirects the seat through [escalation.fallback].
func TestResolveSeatsAppliesSecurityByLabel(t *testing.T) {
	conn, runID := policyFixtureRun(t, securityPolicyTOML)

	unlabelled, err := ResolveSeats(conn, runID, []string{"seat-b"}, nil)
	testsupport.Must(t, err, "ResolveSeats: %v", err)
	if unlabelled[0].Variant != "forbidden" || unlabelled[0].Model != "opus" {
		t.Errorf("seat-b without labels = %+v, want its standing opus/low/forbidden",
			unlabelled[0])
	}

	labelled, err := ResolveSeats(conn, runID, []string{"seat-b"}, []string{"sensitive"})
	testsupport.Must(t, err, "ResolveSeats: %v", err)
	if labelled[0].Variant != "safe" || labelled[0].Model != "haiku" {
		t.Errorf("seat-b with the sensitive label = %+v, want the fallback haiku/low/safe",
			labelled[0])
	}
}

// TestResolveSeatsLabelParityWithALabelledVoteRow: the label a caller passes
// buys exactly what the issue's frozen label buys a vote row.
func TestResolveSeatsLabelParityWithALabelledVoteRow(t *testing.T) {
	conn, dir := configRepo(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	writeConfigFile(t, dir, "workflows/policy-fixture.toml", policyFixtureWorkflow)
	writeConfigFile(t, dir, "policy.toml", securityPolicyTOML)
	issue := createIssue(t, conn, "sensitive fixture", "a body", "task", []string{"sensitive"})
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	next, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	row := findRow(next.Steps, "gate")
	if row == nil {
		t.Fatalf("no offer row named %q; offer was %+v", "gate", next.Steps)
	}

	got, err := ResolveSeats(
		conn, run.ID, []string{"seat-a", "seat-b"}, []string{"sensitive"})
	testsupport.Must(t, err, "ResolveSeats: %v", err)
	assertSeatsEqual(t, got, row.VoterAssignments)
}

// TestResolveSeatsRefusesAnUnknownSeat: one bad seat fails the whole call,
// naming it, rather than returning a partial roster.
func TestResolveSeatsRefusesAnUnknownSeat(t *testing.T) {
	conn, runID := policyFixtureRun(t, policyFixtureTOML)

	_, err := ResolveSeats(conn, runID, []string{"seat-a", "no-such-seat"}, nil)
	if err == nil {
		t.Fatal("want a refusal for a seat with no [executors] row")
	}
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("code = %q, want %q", code, CodeNotFound)
	}
	if !strings.Contains(err.Error(), "no-such-seat") {
		t.Errorf("error %q does not name the seat that failed", err)
	}
}

// TestResolveSeatsRefusesARunWithNoPinnedPolicy: dormancy is an error here,
// unlike on the row path — there is nothing for the caller to fall back to.
func TestResolveSeatsRefusesARunWithNoPinnedPolicy(t *testing.T) {
	conn, runID := policyFixtureRun(t, "")

	_, err := ResolveSeats(conn, runID, []string{"seat-a"}, nil)
	if err == nil {
		t.Fatal("want a refusal for a run with no pinned policy.toml")
	}
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("code = %q, want %q", code, CodeNotFound)
	}
}

// TestResolveSeatsRefusesAnUnknownRun: a run that does not exist is NOT_FOUND
// about the run, not an empty answer about its seats.
func TestResolveSeatsRefusesAnUnknownRun(t *testing.T) {
	conn, _ := policyFixtureRun(t, policyFixtureTOML)

	_, err := ResolveSeats(conn, 9999, []string{"seat-a"}, nil)
	if err == nil {
		t.Fatal("want a refusal for a run that does not exist")
	}
	if code, _ := CodeOf(err); code != CodeNotFound {
		t.Errorf("code = %q, want %q", code, CodeNotFound)
	}
}

func assertSeatsEqual(t *testing.T, got, want []model.VoterAssignment) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d seats, want %d: %+v vs %+v", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("seat %d = %+v, want %+v (the vote row's own assignment)",
				i, got[i], want[i])
		}
	}
}
