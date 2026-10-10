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
	driveDissentGate := func(
		t *testing.T, hold string, verdicts ...model.Verdict,
	) (*sql.DB, *db.Step) {
		t.Helper()
		return driveDissentGateSrc(t, holdOnDissentSrc, hold, verdicts...)
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
		// No threshold is declared, so the class must name the hold, not a
		// threshold park.
		if gate.ParkClass != db.ParkClassDissentHeld {
			t.Errorf("gate@0 park_class = %q, want %q", gate.ParkClass, db.ParkClassDissentHeld)
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

// driveDissentGateSrc registers src under rule `majority` at 0.5 with
// `.hold_on_dissent` set to hold (left unset when empty), opens the gate's
// ballot, casts the given verdicts in seat order (seat-a, seat-b, seat-c),
// drives the tally, and returns the routed gate step.
func driveDissentGateSrc(
	t *testing.T, src, hold string, verdicts ...model.Verdict,
) (*sql.DB, *db.Step) {
	t.Helper()
	conn, e, proposalID := castDissentGate(t, src, hold, verdicts...)
	err := e.DriveVoteProposal(conn, proposalID, nowMS)
	testsupport.Must(t, err, "driving the tally: %v", err)
	return conn, dissentGateStep(t, conn)
}

// castDissentGate is driveDissentGateSrc up to the last cast: the tally is
// decided but not yet routed, so a test can change the rule's config before
// it calls DriveVoteProposal itself.
func castDissentGate(
	t *testing.T, src, hold string, verdicts ...model.Verdict,
) (*sql.DB, *Engine, int) {
	t.Helper()
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.5", "")
	if hold != "" {
		err := db.SetConfig(conn, 0, db.VoteRuleHoldOnDissentKey("majority"), hold)
		testsupport.Must(t, err, "setting hold_on_dissent: %v", err)
	}
	registerSource(t, conn, []byte(src), "dissent-hold.toml")
	issue := createIssue(t, conn, "dissent", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, run.ID)
	for i, seat := range []string{"seat-a", "seat-b", "seat-c"} {
		castSeat(t, conn, proposalID, seat, verdicts[i], "")
	}
	return conn, e, proposalID
}

func dissentGateStep(t *testing.T, conn *sql.DB) *db.Step {
	t.Helper()
	gate, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
	testsupport.Must(t, err, "reading gate@0: %v", err)
	return gate
}

// plantRuleConfig writes a rule key straight into `meta` after the casts,
// bypassing SetConfig's validation — the shape a restored or hand-edited
// store has. An empty value deletes the row instead.
func plantRuleConfig(t *testing.T, conn *sql.DB, key, value string) {
	t.Helper()
	var err error
	if value == "" {
		_, err = conn.Exec(`DELETE FROM meta WHERE key = ?`, "config."+key)
	} else {
		_, err = conn.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
			"config."+key, value)
	}
	testsupport.Must(t, err, "planting %s=%q: %v", key, value, err)
}

// An UNKEYED rule's routing reads only `.hold_on_dissent`, so a fault in any
// other rule field after the casts land leaves the ordinary route untouched,
// exactly as it did before the hold existed.
func TestVoteHoldOnDissentUnkeyedRoutesWithoutThreshold(t *testing.T) {
	conn, e, proposalID := castDissentGate(t, holdOnDissentSrc, "",
		model.VerdictApprove, model.VerdictApprove, model.VerdictApprove)
	plantRuleConfig(t, conn, db.VoteRuleThresholdKey("majority"), "")

	if err := e.DriveVoteProposal(conn, proposalID, nowMS); err != nil {
		t.Fatalf("DriveVoteProposal: %v — an unkeyed step must not need the "+
			"rule's threshold to route", err)
	}
	assertDissentGatePassed(t, conn)
}

func TestVoteHoldOnDissentUnkeyedIgnoresMalformedSealed(t *testing.T) {
	conn, e, proposalID := castDissentGate(t, holdOnDissentSrc, "",
		model.VerdictApprove, model.VerdictApprove, model.VerdictApprove)
	plantRuleConfig(t, conn, db.VoteRuleSealedKey("majority"), "maybe")

	if err := e.DriveVoteProposal(conn, proposalID, nowMS); err != nil {
		t.Fatalf("DriveVoteProposal: %v — an unkeyed step must not read the "+
			"rule's sealed flag to route", err)
	}
	assertDissentGatePassed(t, conn)
}

// A malformed hold value fails CLOSED: reading it as false would pass a
// dissented approval the operator may have asked to see.
func TestVoteHoldOnDissentMalformedHoldBlocksRouting(t *testing.T) {
	conn, e, proposalID := castDissentGate(t, holdOnDissentSrc, "",
		model.VerdictApprove, model.VerdictApprove, model.VerdictReject)
	plantRuleConfig(t, conn, db.VoteRuleHoldOnDissentKey("majority"), "maybe")

	err := e.DriveVoteProposal(conn, proposalID, nowMS)
	if err == nil || !strings.Contains(err.Error(), "malformed hold_on_dissent flag") {
		t.Errorf("DriveVoteProposal error = %v, want one naming the malformed "+
			"hold_on_dissent flag", err)
	}
	gate := dissentGateStep(t, conn)
	if gate.Routing != "" {
		t.Errorf("gate@0 routing = %q, want none while the hold is unreadable", gate.Routing)
	}
	if got := stepStatus(t, conn, "gate@0"); got != db.StepPending {
		t.Errorf("gate@0 status = %q, want %q", got, db.StepPending)
	}
}

func assertDissentGatePassed(t *testing.T, conn *sql.DB) {
	t.Helper()
	gate := dissentGateStep(t, conn)
	if !strings.HasPrefix(gate.Routing, RoutingPass) {
		t.Errorf("gate@0 routing = %q, want %q", gate.Routing, RoutingPass)
	}
	if got := stepStatus(t, conn, "gate@0"); got != db.StepDone {
		t.Errorf("gate@0 status = %q, want %q", got, db.StepDone)
	}
}

// explicitPassSrc is holdOnDissentSrc with the gate's author affirmatively
// declaring the cast set fine: the `pass` predicate matches any tally with an
// approve cast, so it matches every approved tally below.
const explicitPassSrc = holdOnDissentSrc + `threshold = { "pass" = 'any(verdict == approve)' }
`

// TestVoteHoldOnDissentExplicitPassStillParks pins the precedence between the
// two routing authorities on an approved tally: the operator's
// `hold_on_dissent` outranks a workflow author's threshold predicate that
// explicitly matched `pass`.
func TestVoteHoldOnDissentExplicitPassStillParks(t *testing.T) {
	t.Run("keyed approved-with-reject parks despite the explicit pass", func(t *testing.T) {
		conn, gate := driveDissentGateSrc(t, explicitPassSrc, "true",
			model.VerdictApprove, model.VerdictApprove, model.VerdictReject)
		if !strings.HasPrefix(gate.Routing, workflow.OnFailWaitingHuman) {
			t.Fatalf("gate@0 routing = %q, want %q — an author's explicit `pass` "+
				"must not silence the operator's hold on a dissented approval",
				gate.Routing, workflow.OnFailWaitingHuman)
		}
		if !strings.Contains(gate.Routing, "seat-c") {
			t.Errorf("gate@0 routing record %q does not name the dissenting seat", gate.Routing)
		}
		if got := stepStatus(t, conn, "gate@0"); got != db.StepWaitingHuman {
			t.Errorf("gate@0 status = %q, want %q", got, db.StepWaitingHuman)
		}
	})

	// Control: with no dissent the explicit `pass` predicate routes the gate
	// to pass, so the park above is what moved it.
	t.Run("keyed clean approval takes the explicit pass", func(t *testing.T) {
		_, gate := driveDissentGateSrc(t, explicitPassSrc, "true",
			model.VerdictApprove, model.VerdictApprove, model.VerdictApprove)
		if !strings.HasPrefix(gate.Routing, RoutingPass) {
			t.Errorf("gate@0 routing = %q for a clean approval, want %q",
				gate.Routing, RoutingPass)
		}
	})
}

// concernParkSrc is holdOnDissentSrc with a threshold that parks any approved
// tally carrying an approve-with-concerns cast.
const concernParkSrc = holdOnDissentSrc + `threshold = { "waiting-human" = 'any(verdict == approve-with-concerns)' }
`

// TestVoteThresholdParkKeepsThresholdClass: an approved tally that a matched
// threshold parks, with no dissent hold in play, is a threshold park. The hold
// owns its own class; a threshold park must not read as one.
func TestVoteThresholdParkKeepsThresholdClass(t *testing.T) {
	conn, gate := driveDissentGateSrc(t, concernParkSrc, "",
		model.VerdictApproveWithConcerns, model.VerdictApprove, model.VerdictApprove)
	proposalID, err := findVoteProposal(conn, gate)
	testsupport.Must(t, err, "finding gate@0's proposal: %v", err)
	proposal, err := db.GetProposal(conn, proposalID)
	testsupport.Must(t, err, "GetProposal: %v", err)
	if proposal.Status != model.ProposalStatusApproved {
		t.Fatalf("proposal status = %q, want approved", proposal.Status)
	}

	if got := stepStatus(t, conn, "gate@0"); got != db.StepWaitingHuman {
		t.Fatalf("gate@0 status = %q, want %q — the threshold must park the step",
			got, db.StepWaitingHuman)
	}
	if gate.ParkClass != db.ParkClassThresholdRouted {
		t.Errorf("gate@0 park_class = %q, want %q", gate.ParkClass, db.ParkClassThresholdRouted)
	}
}

// TestVoteHoldOnDissentExclusions holds the park guard's two exclusions that
// routing order alone enforces: each case is an APPROVED tally carrying a
// reject under a keyed rule, which parks anywhere the exclusion is lost.
func TestVoteHoldOnDissentExclusions(t *testing.T) {
	requireApproved := func(t *testing.T, conn *sql.DB, proposalID int) {
		t.Helper()
		proposal, err := db.GetProposal(conn, proposalID)
		testsupport.Must(t, err, "GetProposal: %v", err)
		if proposal.Status != model.ProposalStatusApproved {
			t.Fatalf("proposal status = %q, want approved — the case must reach "+
				"the park guard with a dissented approval", proposal.Status)
		}
	}

	// The triage arm itself sets `pass`, so only the explicit triage conjunct
	// keeps a decided panel from being parked on the question it answered.
	t.Run("a decided triage panel is not parked", func(t *testing.T) {
		conn, _, e := triageRun(t, "retry", "abandon-issue")
		err := db.SetConfig(conn, 0, db.VoteRuleHoldOnDissentKey("majority"), "true")
		testsupport.Must(t, err, "setting hold_on_dissent: %v", err)

		proposalID, err := findVoteProposal(conn, mustStep(t, conn, "triage@0"))
		testsupport.Must(t, err, "finding triage@0's proposal: %v", err)
		if proposalID == 0 {
			t.Fatal("no proposal opened for triage@0")
		}
		// One approve against one reject at equal weight scores exactly the
		// 0.5 rule, which approves.
		castSeat(t, conn, proposalID, "seat-a", model.VerdictApprove, "")
		castSeat(t, conn, proposalID, "seat-b", model.VerdictReject, "")
		err = e.DriveVoteProposal(conn, proposalID, nowMS)
		testsupport.Must(t, err, "driving the tally: %v", err)
		requireApproved(t, conn, proposalID)

		panel := mustStep(t, conn, "triage@0")
		if strings.HasPrefix(panel.Routing, workflow.OnFailWaitingHuman) ||
			strings.Contains(panel.Routing, "hold_on_dissent") {
			t.Errorf("triage@0 routing = %q — a decided panel must not be "+
				"parked on its own dissent", panel.Routing)
		}
		if panel.Status != db.StepDone {
			t.Errorf("triage@0 status = %q, want %q", panel.Status, db.StepDone)
		}
		step := mustStep(t, conn, "implement@0")
		if !routingIs(step.Routing, ResolveRetry) {
			t.Errorf("implement@0 routing = %q, want the panel's mapped %q",
				step.Routing, ResolveRetry)
		}
	})

	// A threshold that routed `fix-loop` must keep it: the park is clearable
	// by `override-pass`, so displacing a fix round with it loosens the gate.
	t.Run("a threshold fix-loop is not displaced by the park", func(t *testing.T) {
		conn, gate := driveDissentGateSrc(t, concernLoopSrc, "true",
			model.VerdictApproveWithConcerns, model.VerdictApproveWithConcerns,
			model.VerdictReject)
		proposalID, err := findVoteProposal(conn, gate)
		testsupport.Must(t, err, "finding gate@0's proposal: %v", err)
		requireApproved(t, conn, proposalID)

		if !strings.HasPrefix(gate.Routing, workflow.OnFailFixLoop) {
			t.Errorf("gate@0 routing = %q, want the threshold's %q",
				gate.Routing, workflow.OnFailFixLoop)
		}
		if strings.HasPrefix(gate.Routing, workflow.OnFailWaitingHuman) {
			t.Errorf("gate@0 routing = %q — the dissent park replaced a fix-loop route",
				gate.Routing)
		}
		stepIDByInstance(t, conn, "fix@1") // fatals if the fix round never opened
	})
}
