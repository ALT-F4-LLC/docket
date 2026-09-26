package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// DKT-2449: a lone reject routed nowhere once the weighted score passed. The
// tally is a weighted mean and approve-with-concerns adds to it, so a
// dissenting seat's findings left no trace in routing. `.hold_on_dissent` is
// the rule's opt-in answer: an approved tally carrying a `reject` parks for
// the operator instead of passing silently.

// holdOnDissentSrc: a vote gate with NO threshold table, so the routing under
// test is the dissent hold's alone and nothing else can move the step off
// pass.
const holdOnDissentSrc = `
[pipeline]
name = "dissent-hold"
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
on_fail = "abandon-issue"
`

// TestVoteRuleHoldOnDissentKey is DKT-2449's resolve half: the flag rides the
// resolved rule beside `.sealed`, defaults false, and a stored value that
// bypassed set-time validation fails LOUDLY rather than defaulting to false —
// a silent false would fail open with respect to the hold.
func TestVoteRuleHoldOnDissentKey(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")

	t.Run("unset resolves false", func(t *testing.T) {
		rule, err := resolveVoteRule(conn, 1, "majority")
		testsupport.Must(t, err, "resolveVoteRule: %v", err)
		if rule.HoldOnDissent {
			t.Error("a rule with no `.hold_on_dissent` set resolved held; the default is false")
		}
	})

	t.Run("a set true is read back", func(t *testing.T) {
		err := db.SetConfig(conn, 0, db.VoteRuleHoldOnDissentKey("majority"), "true")
		testsupport.Must(t, err, "setting the rule's hold_on_dissent flag: %v", err)
		rule, err := resolveVoteRule(conn, 1, "majority")
		testsupport.Must(t, err, "resolveVoteRule: %v", err)
		if !rule.HoldOnDissent {
			t.Error("`.hold_on_dissent = true` did not resolve held")
		}
	})

	t.Run("a malformed value is refused", func(t *testing.T) {
		// Written straight into `meta`, bypassing SetConfig's KindBool check —
		// the shape a restored or hand-edited store has. Resolve must error.
		_, err := conn.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
			"config."+db.VoteRuleHoldOnDissentKey("majority"), "maybe")
		testsupport.Must(t, err, "planting a malformed flag: %v", err)
		if _, err := resolveVoteRule(conn, 1, "majority"); err == nil {
			t.Error("a malformed `.hold_on_dissent` resolved without error; " +
				"it must fail loudly rather than default to false")
		}
	})
}

// TestVoteHoldOnDissentParksApprovedWithReject is DKT-2449's routing half.
//
// The park is strictly ADDITIVE: it only ever converts a `pass` into
// `waiting-human`, never the reverse, so a rejected tally keeps its `on_fail`
// route and a triage panel is never parked on the question it just answered.
func TestVoteHoldOnDissentParksApprovedWithReject(t *testing.T) {
	// driveDissentGate opens the ballot, casts the given verdicts in seat
	// order, drives the tally, and returns the routed gate step.
	driveDissentGate := func(
		t *testing.T, hold string, verdicts ...model.Verdict,
	) (*sql.DB, *db.Step) {
		t.Helper()
		conn := mustDB(t)
		registerVoteRule(t, conn, "majority", "0.5", "")
		if hold != "" {
			err := db.SetConfig(conn, 0, db.VoteRuleHoldOnDissentKey("majority"), hold)
			testsupport.Must(t, err, "setting hold_on_dissent: %v", err)
		}
		registerSource(t, conn, []byte(holdOnDissentSrc), "dissent-hold.toml")
		issue := createIssue(t, conn, "dissent", "body", "task", nil)
		run := startRun(t, conn, issue)
		_, err := activate(conn, run.ID)
		testsupport.Must(t, err, "activate: %v", err)
		e := testEngine()

		proposalID := openGateProposal(t, conn, e, run.ID)
		for i, seat := range []string{"seat-a", "seat-b", "seat-c"} {
			castSeat(t, conn, proposalID, seat, verdicts[i], "")
		}
		err = e.DriveVoteProposal(conn, proposalID, nowMS)
		testsupport.Must(t, err, "driving the tally: %v", err)

		gate, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
		testsupport.Must(t, err, "reading gate@0: %v", err)
		return conn, gate
	}

	// Two approvals against one reject: the weighted mean clears 0.5, so the
	// tally APPROVES and the routing under test is the hold's alone.
	approvedWithReject := []model.Verdict{
		model.VerdictApprove, model.VerdictApprove, model.VerdictReject,
	}

	t.Run("keyed approved-with-reject parks", func(t *testing.T) {
		conn, gate := driveDissentGate(t, "true", approvedWithReject...)
		if !strings.HasPrefix(gate.Routing, workflow.OnFailWaitingHuman) {
			t.Fatalf("gate@0 routing = %q, want %q — an approved tally carrying "+
				"a reject must park under a keyed rule",
				gate.Routing, workflow.OnFailWaitingHuman)
		}
		if !strings.Contains(gate.Routing, "seat-c") {
			t.Errorf("gate@0 routing record %q does not name the dissenting seat", gate.Routing)
		}
		// The routing is only half the criterion: the step must actually be
		// parked, which is what puts the question in front of the operator.
		if got := stepStatus(t, conn, "gate@0"); got != db.StepWaitingHuman {
			t.Errorf("gate@0 status = %q, want %q", got, db.StepWaitingHuman)
		}
	})

	t.Run("keyed false takes the ordinary route", func(t *testing.T) {
		_, gate := driveDissentGate(t, "false", approvedWithReject...)
		if !strings.HasPrefix(gate.Routing, RoutingPass) {
			t.Errorf("gate@0 routing = %q with the key false, want %q",
				gate.Routing, RoutingPass)
		}
	})

	t.Run("unset takes the ordinary route", func(t *testing.T) {
		_, gate := driveDissentGate(t, "", approvedWithReject...)
		if !strings.HasPrefix(gate.Routing, RoutingPass) {
			t.Errorf("gate@0 routing = %q with the key unset, want %q",
				gate.Routing, RoutingPass)
		}
	})

	t.Run("keyed true with no reject takes the ordinary route", func(t *testing.T) {
		_, gate := driveDissentGate(t, "true",
			model.VerdictApprove, model.VerdictApproveWithConcerns, model.VerdictApprove)
		if !strings.HasPrefix(gate.Routing, RoutingPass) {
			t.Errorf("gate@0 routing = %q for a clean approval under a keyed rule, want %q",
				gate.Routing, RoutingPass)
		}
	})

	// AC3: a REJECTED proposal keeps its existing `on_fail` route. The park
	// never replaces a fail route — it only ever displaces a pass.
	t.Run("a rejected proposal takes its on_fail route", func(t *testing.T) {
		_, gate := driveDissentGate(t, "true",
			model.VerdictReject, model.VerdictReject, model.VerdictReject)
		if !strings.HasPrefix(gate.Routing, workflow.OnFailAbandonIssue) {
			t.Errorf("gate@0 routing = %q for a rejected tally, want the declared %q",
				gate.Routing, workflow.OnFailAbandonIssue)
		}
	})
}
