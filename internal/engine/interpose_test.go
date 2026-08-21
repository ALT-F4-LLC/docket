package engine

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Threshold interposition (DKT-38, engine-spec §11.2): a step named as a
// `threshold` step-name routing target is a GATE the routing chooses — it
// becomes ready only when a routing predecessor's recorded routing names it,
// and a routing that resolves anywhere else terminalizes it `skipped` in the
// same transaction. Before this, the target behaved as an ordinary `after`
// successor and ran on EVERY run: the corpus's tribunal vote gates seated a
// three-judge panel per clean run, threshold or no threshold.

// interposeHumanSrc is the canonical authoring shape: the gate declares
// `after = [routing-step]` (V8-compliant, and the staged closure's anchor).
const interposeHumanSrc = `
[pipeline]
name = "interpose-human"
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
type = "human"
on_fail = "skip"

[[step]]
name = "finish"
after = ["tribunal"]
executor = "finish"
emits = "record"
`

func activateInterposed(t *testing.T, conn *sql.DB, src string) (runID, issueID int) {
	t.Helper()
	registerSource(t, conn, []byte(src), "interpose.toml")
	issue := createIssue(t, conn, "interposed", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run.ID, issue
}

func issueStatusOf(t *testing.T, conn *sql.DB, issueID int) string {
	t.Helper()
	var status string
	err := conn.QueryRow(`SELECT status FROM issues WHERE id = ?`, issueID).Scan(&status)
	testsupport.Must(t, err, "reading issue status: %v", err)
	return status
}

func runStatusOf(t *testing.T, conn *sql.DB, runID int) string {
	t.Helper()
	var status string
	err := conn.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&status)
	testsupport.Must(t, err, "reading run status: %v", err)
	return status
}

// TestUnroutedInterposedGateIsSkippedAndTheIssueCompletes is the defect's
// exact scenario, fixed: a clean run whose threshold routes `pass` must never
// ready the gate, must terminalize it `skipped` in the routing transaction,
// and must still complete the issue — "a readiness latch alone is not a fix".
func TestUnroutedInterposedGateIsSkippedAndTheIssueCompletes(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, interposeHumanSrc)
	e := testEngine()

	// Before the routing step records, the gate is an ordinary R3 wait.
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		ok, cond := sched.Ready(stepNamed(t, sched, "tribunal@0"))
		if ok || cond != CondPredecessors {
			t.Fatalf("tribunal@0 before routing: ready=%v cond=%q, want a "+
				"CondPredecessors wait", ok, cond)
		}
	})

	claimAndComplete(t, conn, e, "verify@0", "all clear", `[{"status":"ok"}]`)

	if got := stepRouting(t, conn, "verify@0"); got != "pass" {
		t.Fatalf("verify@0 routing = %q, want pass", got)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepSkipped {
		t.Errorf("tribunal@0 = %q after a pass routing, want %q — an unrouted "+
			"interposed gate must terminalize, or the issue never completes",
			got, db.StepSkipped)
	}

	// The skip is event-logged, naming the routing that decided against it.
	var data string
	err := conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = 'step-skipped'
		  ORDER BY seq DESC LIMIT 1`, runID).Scan(&data)
	testsupport.Must(t, err, "reading the step-skipped event: %v", err)
	if !strings.Contains(data, "verify@0") || !strings.Contains(data, "pass") {
		t.Errorf("step-skipped data = %q, want it to name the routing step "+
			"and its routing", data)
	}

	// Downstream of the gate resolves over the skip (J1: skipped is terminal).
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "finish@0")); !ok {
			t.Fatalf("finish@0 not ready over a skipped gate: %q", cond)
		}
	})
	claimAndComplete(t, conn, e, "finish@0", "the record", `[]`)

	if got := issueStatusOf(t, conn, issue); got != "done" {
		t.Errorf("issue = %q after every live step finished, want done", got)
	}
	if got := runStatusOf(t, conn, runID); got != "done" {
		t.Errorf("run = %q, want done", got)
	}
}

// TestRootShapedInterposedGateIsLatched covers the engine's own historical
// fixture shape — the gate declares `after = []`, a ROOT declaration (V10) —
// which was WORSE than the corpus shape: R3 was vacuous and the gate was
// ready AT ACTIVATION. The latch must hold it unready with a condition that
// names interposition, and the pass routing must still skip it.
func TestRootShapedInterposedGateIsLatched(t *testing.T) {
	const src = `
[pipeline]
name = "interpose-root"
version = 1

[match]
kind = ["task"]

[[step]]
name = "measure"
executor = "measure"
emits = "report"
threshold = { "escalate" = "any(status == bad)" }

[[step]]
name = "escalate"
after = []
type = "human"
on_fail = "skip"

[[step]]
name = "finish"
after = ["measure"]
executor = "finish"
emits = "record"
`
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, src)
	e := testEngine()

	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		ok, cond := sched.Ready(stepNamed(t, sched, "escalate@0"))
		if ok || cond != CondUnrouted {
			t.Fatalf("escalate@0 at activation: ready=%v cond=%q, want the "+
				"CondUnrouted latch — a root-shaped gate was ready at "+
				"activation before DKT-38", ok, cond)
		}
	})

	claimAndComplete(t, conn, e, "measure@0", "fine", `[{"status":"good"}]`)

	if got := stepStatus(t, conn, "escalate@0"); got != db.StepSkipped {
		t.Errorf("escalate@0 = %q after a pass routing, want %q", got, db.StepSkipped)
	}
	claimAndComplete(t, conn, e, "finish@0", "the record", `[]`)
	if got := issueStatusOf(t, conn, issue); got != "done" {
		t.Errorf("issue = %q, want done", got)
	}
}

// TestRoutedInterposedGateBecomesReady is the other half of the contract: when
// the threshold DOES name the gate, it opens — and on its pass, execution
// resumes at the downstream chain.
func TestRoutedInterposedGateBecomesReady(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, interposeHumanSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "verify@0", "blocked finding", `[{"status":"blocked"}]`)

	if got := stepRouting(t, conn, "verify@0"); got != "tribunal" {
		t.Fatalf("verify@0 routing = %q, want the interposed step name", got)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepPending {
		t.Fatalf("tribunal@0 = %q after being routed to, want pending", got)
	}
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "tribunal@0")); !ok {
			t.Fatalf("tribunal@0 not ready after its routing named it: %q", cond)
		}
	})

	err := e.DecideStep(conn, stepIDByInstance(t, conn, "tribunal@0"), true, "lgtm", nowMS)
	testsupport.Must(t, err, "approving the gate: %v", err)

	claimAndComplete(t, conn, e, "finish@0", "the record", `[]`)
	if got := issueStatusOf(t, conn, issue); got != "done" {
		t.Errorf("issue = %q, want done", got)
	}
}

// interposeVoteSrc is the corpus's actual shape: the interposed gate is a
// `type="vote"` step — the tribunal. The gate is a LEAF, so issue completion
// itself depends on the unrouted gate terminalizing.
const interposeVoteSrc = `
[pipeline]
name = "interpose-vote"
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
voters = ["seat-a", "seat-b", "seat-c"]
vote_rule = "majority"
on_fail = "waiting-human"
`

// TestUnroutedInterposedVoteGateOpensNoProposal is the measured cost of the
// defect, asserted away: on a clean run, no panel seats — the vote lifecycle
// driver never sees the gate ready, no proposal is opened, and the run
// completes.
func TestUnroutedInterposedVoteGateOpensNoProposal(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, interposeVoteSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "verify@0", "all clear", `[{"status":"ok"}]`)
	err := e.DriveRunLifecycles(conn, runID, nowMS)
	testsupport.Must(t, err, "driving lifecycles: %v", err)

	var proposals int
	err = conn.QueryRow(`SELECT COUNT(*) FROM proposals`).Scan(&proposals)
	testsupport.Must(t, err, "counting proposals: %v", err)
	if proposals != 0 {
		t.Errorf("%d proposal(s) opened on a clean run — the panel seated "+
			"for a gate nothing routed to", proposals)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepSkipped {
		t.Errorf("tribunal@0 = %q, want %q", got, db.StepSkipped)
	}
	if got := issueStatusOf(t, conn, issue); got != "done" {
		t.Errorf("issue = %q, want done — a leaf gate that never terminalizes "+
			"blocks completion forever", got)
	}
	if got := runStatusOf(t, conn, runID); got != "done" {
		t.Errorf("run = %q, want done", got)
	}
}

// TestRoutedInterposedVoteGateSeatsItsPanel: when the threshold escalates, the
// panel is real — the driver opens the proposal for the now-ready gate.
func TestRoutedInterposedVoteGateSeatsItsPanel(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.6", "high")
	runID, _ := activateInterposed(t, conn, interposeVoteSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "verify@0", "blocked finding", `[{"status":"blocked"}]`)
	err := e.DriveRunLifecycles(conn, runID, nowMS)
	testsupport.Must(t, err, "driving lifecycles: %v", err)

	var proposals int
	err = conn.QueryRow(`SELECT COUNT(*) FROM proposals`).Scan(&proposals)
	testsupport.Must(t, err, "counting proposals: %v", err)
	if proposals != 1 {
		t.Errorf("%d proposal(s) after the threshold routed to the gate, want 1", proposals)
	}
	if got := stepStatus(t, conn, "tribunal@0"); got != db.StepPending {
		t.Errorf("tribunal@0 = %q while its vote is open, want pending", got)
	}
}

// TestFanoutRoutingDefersTheSkipUntilTheJoin: with a fanned routing step, the
// choice is open until every sibling records — one sibling routing `pass`
// must not skip a gate a later sibling goes on to route to.
func TestFanoutRoutingDefersTheSkipUntilTheJoin(t *testing.T) {
	const src = `
[pipeline]
name = "interpose-fanout"
version = 1

[match]
kind = ["task"]

[[step]]
name = "probe"
fanout = ["a", "b"]
emits = "report"
threshold = { "gate" = "any(status == blocked)" }

[[step]]
name = "gate"
after = ["probe"]
type = "human"
on_fail = "skip"
`
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, src)
	e := testEngine()

	claimAndComplete(t, conn, e, "probe@0#0", "clear", `[{"status":"ok"}]`)
	if got := stepStatus(t, conn, "gate@0"); got != db.StepPending {
		t.Fatalf("gate@0 = %q with a sibling still open — the skip must wait "+
			"for the join, or a later sibling's routing is foreclosed", got)
	}

	claimAndComplete(t, conn, e, "probe@0#1", "blocked", `[{"status":"blocked"}]`)
	if got := stepStatus(t, conn, "gate@0"); got != db.StepPending {
		t.Fatalf("gate@0 = %q after a sibling routed to it, want pending", got)
	}
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "gate@0")); !ok {
			t.Fatalf("gate@0 not ready after sibling #1 routed to it: %q", cond)
		}
	})

	err := e.DecideStep(conn, stepIDByInstance(t, conn, "gate@0"), true, "ok", nowMS)
	testsupport.Must(t, err, "approving the gate: %v", err)
	if got := issueStatusOf(t, conn, issue); got != "done" {
		t.Errorf("issue = %q, want done", got)
	}
}

// TestStagedInterposedGateIsConditional: the gate may ride the staged closure
// — that is what lets the routed path seat mid-wave without another dispatch
// round — but as a CONDITIONAL row: its routing predecessor can finish
// without naming it, exactly the finish-without-routing promise a
// hold-capable step makes. (A vote gate, because human steps never join the
// closure at all.)
func TestStagedInterposedGateIsConditional(t *testing.T) {
	conn := mustDB(t)
	runID, _ := activateInterposed(t, conn, interposeVoteSrc)

	answer, err := testEngine().NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "next: %v", err)

	want := map[string]bool{
		"verify@0":   false,
		"tribunal@0": true,
	}
	seen := 0
	for _, row := range answer.Steps {
		w, ok := want[row.Instance]
		if !ok {
			continue
		}
		seen++
		if row.Conditional != w {
			t.Errorf("%s conditional = %v, want %v", row.Instance, row.Conditional, w)
		}
	}
	if seen != len(want) {
		t.Errorf("saw %d of the %d expected rows: %v", seen, len(want), instancesIn(answer))
	}
}
