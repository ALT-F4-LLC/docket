package engine

import (
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// parkReasonSrc parks its vote gate at waiting-human on a rejected tally, so
// the park's reason is the ENGINE's ("rejected") and not an operator's note.
const parkReasonSrc = `
[pipeline]
name = "park-reason"
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
`

// TestParkReasonSurvivesResolve is DKT-1898.
//
// The engine's park text lived only in the `routing` column, glued behind the
// routing word. `step resolve` rewrites that column with the RESOLUTION's own
// routing and note, so the reason the step parked was destroyed by the act of
// answering it — and reconstructing it afterwards meant reading gate rows,
// artifact statuses and vote tallies.
func TestParkReasonSurvivesResolve(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	registerSource(t, conn, []byte(parkReasonSrc), "park-reason.toml")
	issue := createIssue(t, conn, "parked", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, run.ID)
	for _, seat := range []string{"seat-a", "seat-b", "seat-c"} {
		castSeat(t, conn, proposalID, seat, model.VerdictReject, "no")
	}
	testsupport.Must(t, e.DriveRunLifecycles(conn, run.ID, nowMS), "driving the tally: %v", err)

	gateID := stepIDByInstance(t, conn, "gate@0")
	parked, err := LoadStepView(conn, gateID, nowMS)
	testsupport.Must(t, err, "LoadStepView(parked): %v", err)
	if parked.Step.Status != db.StepWaitingHuman {
		t.Fatalf("gate@0 is %q, want %q — the fixture must park",
			parked.Step.Status, db.StepWaitingHuman)
	}
	if parked.ParkReason == "" {
		t.Fatal("a parked step reports no park_reason; the engine's own text " +
			"is the only record of what could not be decided")
	}
	want := parked.ParkReason

	// AC2: the step-routed event for the park carries that reason too, so the
	// feed says why without a join against the step row.
	var routedData string
	err = conn.QueryRow(
		`SELECT data FROM events WHERE step_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		gateID, EventStepRouted).Scan(&routedData)
	testsupport.Must(t, err, "reading the step-routed event: %v", err)
	if !strings.Contains(routedData, want) {
		t.Errorf("step-routed data = %q, want it to carry the park reason %q",
			routedData, want)
	}

	// AC1: resolving answers the question; it does not erase it.
	const note = "the blocker was a stale fixture, not the change"
	testsupport.Must(t,
		e.ResolveStep(conn, gateID, ResolveOverridePass, note, nowMS),
		"resolve: %v", err)

	resolved, err := LoadStepView(conn, gateID, nowMS)
	testsupport.Must(t, err, "LoadStepView(resolved): %v", err)
	if resolved.ParkReason != want {
		t.Errorf("park_reason after resolve = %q, want the unchanged %q",
			resolved.ParkReason, want)
	}
	if strings.Contains(resolved.ParkReason, note) {
		t.Errorf("park_reason = %q absorbed the resolver's note; the two are "+
			"separate facts", resolved.ParkReason)
	}
	if !strings.Contains(resolved.Routing, note) {
		t.Errorf("routing = %q lost the resolution note", resolved.Routing)
	}
}
