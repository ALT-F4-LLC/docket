package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-1085: `after_fired` — "run me only if the gate fired".
//
// An interposed gate ends `skipped` on every round its routing step resolved
// elsewhere, `skipped` is terminal, and R3 releases an `after` successor on
// ANY terminal predecessor — so `after = ["security-vote"]` ran the corpus's
// `drain-highs` on skipped-vote rounds too (3 of 81 measured, one spawn each
// for an empty report). `after_fired = ["security-vote"]` is the declaration
// the engine lacked: the successor is skipped in the same transaction as the
// gate, transitively, while `after` keeps its exact meaning.

// afterFiredSrc is the corpus shape: an interposed vote behind a routing
// step, a `drain-highs` that runs only if the vote fired, a `summarize`
// behind THAT (the transitive case), a plain-`after` `verify` that runs on
// every round, and a `finish` whose inputs name the conditional chain (J3).
const afterFiredSrc = `
[pipeline]
name = "after-fired"
version = 1

[match]
kind = ["task"]

[[step]]
name = "reconcile"
executor = "reconcile"
emits = "report"
threshold = { "security-vote" = "any(status == blocked)" }

[[step]]
name = "security-vote"
after = ["reconcile"]
type = "human"
on_fail = "skip"

[[step]]
name = "drain-highs"
after = ["security-vote"]
after_fired = ["security-vote"]
executor = "drain"
emits = "drain-report"

[[step]]
name = "summarize"
after = ["drain-highs"]
after_fired = ["drain-highs"]
executor = "summarize"
emits = "summary"

[[step]]
name = "verify"
after = ["security-vote"]
executor = "verify"
emits = "verification"

[[step]]
name = "finish"
after = ["verify", "summarize"]
executor = "finish"
emits = "record"
inputs = ["summarize.summary", "verify.verification"]
`

// skipEventData reads the `step-skipped` event recorded against one instance.
func skipEventData(t *testing.T, conn *sql.DB, runID int, instance string) string {
	t.Helper()
	var data string
	err := conn.QueryRow(
		`SELECT e.data FROM events e JOIN steps s ON s.id = e.step_id
		  WHERE e.run_id = ? AND e.kind = 'step-skipped' AND s.instance = ?
		  ORDER BY e.seq DESC LIMIT 1`, runID, instance).Scan(&data)
	testsupport.Must(t, err, "reading the step-skipped event for %s: %v", instance, err)
	return data
}

// TestAfterFiredSuccessorIsSkippedWithItsGate is the issue's exact scenario,
// fixed: the routing resolves `pass`, the gate is skipped, and the ONE
// transaction that skipped it also skips its `after_fired` successor and,
// transitively, the successor's own — while the plain-`after` successor is
// untouched and the issue still completes over the skipped chain.
func TestAfterFiredSuccessorIsSkippedWithItsGate(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, afterFiredSrc)
	e := testEngine()

	// Nothing is decided at activation: no `when`, so every row is pending.
	for _, inst := range []string{"security-vote@0", "drain-highs@0", "summarize@0", "verify@0"} {
		if got := stepStatus(t, conn, inst); got != db.StepPending {
			t.Fatalf("%s = %q at activation, want pending", inst, got)
		}
	}

	// No `next`, no lifecycle drive in between: the statuses below are what
	// the routing transaction itself committed.
	claimAndComplete(t, conn, e, "reconcile@0", "all clear", `[{"status":"ok"}]`)

	if got := stepStatus(t, conn, "security-vote@0"); got != db.StepSkipped {
		t.Fatalf("security-vote@0 = %q after a pass routing, want %q", got, db.StepSkipped)
	}
	if got := stepStatus(t, conn, "drain-highs@0"); got != db.StepSkipped {
		t.Errorf("drain-highs@0 = %q after its gate was skipped, want %q — an "+
			"after_fired successor must be skipped in the same transaction as "+
			"its gate, not left pending or run unconditionally", got, db.StepSkipped)
	}
	if got := stepStatus(t, conn, "summarize@0"); got != db.StepSkipped {
		t.Errorf("summarize@0 = %q, want %q — the skip must cascade through a "+
			"step whose after_fired predecessor was itself skipped by propagation",
			got, db.StepSkipped)
	}

	// `after` keeps its meaning: the plain successor is pending and, over the
	// skipped gate, ready (J1: skipped is terminal).
	if got := stepStatus(t, conn, "verify@0"); got != db.StepPending {
		t.Errorf("verify@0 = %q, want pending — an after-only successor is "+
			"unaffected by its gate's skip", got)
	}
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "verify@0")); !ok {
			t.Errorf("verify@0 not ready over the skipped gate: %q", cond)
		}
	})

	// Each cascade skip is event-logged naming the predecessor that did not
	// fire, so the ledger explains the row.
	if data := skipEventData(t, conn, runID, "drain-highs@0"); !strings.Contains(data, "security-vote@0") {
		t.Errorf("drain-highs@0 step-skipped data = %q, want it to name security-vote@0", data)
	}
	if data := skipEventData(t, conn, runID, "summarize@0"); !strings.Contains(data, "drain-highs@0") {
		t.Errorf("summarize@0 step-skipped data = %q, want it to name drain-highs@0", data)
	}

	// J3: `finish` binds no input from the skipped chain, and J1 releases it
	// over the skipped `summarize`; the issue and run complete.
	claimAndComplete(t, conn, e, "verify@0", "verified", `[]`)
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "finish@0")); !ok {
			t.Fatalf("finish@0 not ready behind a skipped after_fired chain: %q", cond)
		}
	})
	for _, in := range contextInputs(t, conn, runID, "finish@0") {
		if in.ProducerStep == "summarize@0" {
			t.Error("finish@0 bound an input from a skipped producer; J3 resolves " +
				"over recorded producers only")
		}
	}
	claimAndComplete(t, conn, e, "finish@0", "the record", `[]`)

	if got := issueStatusOf(t, conn, issue); got != "done" {
		t.Errorf("issue = %q after every live step finished, want done", got)
	}
	if got := runStatusOf(t, conn, runID); got != "done" {
		t.Errorf("run = %q, want done", got)
	}
}

// TestAfterFiredSuccessorRunsWhenTheGateFired is the other half: when the
// threshold routes TO the gate and the gate approves, the chain runs exactly
// as an ordinary `after` chain would — nothing is skipped.
func TestAfterFiredSuccessorRunsWhenTheGateFired(t *testing.T) {
	conn := mustDB(t)
	runID, issue := activateInterposed(t, conn, afterFiredSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "reconcile@0", "blocked finding", `[{"status":"blocked"}]`)

	if got := stepRouting(t, conn, "reconcile@0"); got != "security-vote" {
		t.Fatalf("reconcile@0 routing = %q, want the interposed step name", got)
	}
	for _, inst := range []string{"security-vote@0", "drain-highs@0", "summarize@0"} {
		if got := stepStatus(t, conn, inst); got != db.StepPending {
			t.Fatalf("%s = %q while the gate is open, want pending", inst, got)
		}
	}
	// Ordered behind the gate by its `after` edge, as V39a guarantees.
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		ok, cond := sched.Ready(stepNamed(t, sched, "drain-highs@0"))
		if ok || cond != CondPredecessors {
			t.Fatalf("drain-highs@0 with its gate open: ready=%v cond=%q, want a "+
				"CondPredecessors wait", ok, cond)
		}
	})

	err := e.DecideStep(conn, stepIDByInstance(t, conn, "security-vote@0"), true, "lgtm", nowMS)
	testsupport.Must(t, err, "approving the gate: %v", err)

	if got := stepStatus(t, conn, "drain-highs@0"); got != db.StepPending {
		t.Fatalf("drain-highs@0 = %q after its gate fired, want pending", got)
	}
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "drain-highs@0")); !ok {
			t.Fatalf("drain-highs@0 not ready after its gate approved: %q", cond)
		}
	})

	claimAndComplete(t, conn, e, "drain-highs@0", "drained", `[]`)
	claimAndComplete(t, conn, e, "summarize@0", "the summary", `[]`)
	claimAndComplete(t, conn, e, "verify@0", "verified", `[]`)
	claimAndComplete(t, conn, e, "finish@0", "the record", `[]`)

	if got := issueStatusOf(t, conn, issue); got != "done" {
		t.Errorf("issue = %q, want done", got)
	}
}

// TestAfterFiredFollowsAnOnFailSkip: the gate fired but was REJECTED, and its
// `on_fail = "skip"` ends it `skipped` — "skipped for any other existing
// reason". The cascade follows that skip too, in the rejection's own
// transaction.
func TestAfterFiredFollowsAnOnFailSkip(t *testing.T) {
	conn := mustDB(t)
	runID, _ := activateInterposed(t, conn, afterFiredSrc)
	e := testEngine()

	claimAndComplete(t, conn, e, "reconcile@0", "blocked finding", `[{"status":"blocked"}]`)
	err := e.DecideStep(conn, stepIDByInstance(t, conn, "security-vote@0"), false, "no", nowMS)
	testsupport.Must(t, err, "rejecting the gate: %v", err)

	if got := stepStatus(t, conn, "security-vote@0"); got != db.StepSkipped {
		t.Fatalf("security-vote@0 = %q after a rejection routed skip, want %q", got, db.StepSkipped)
	}
	if got := stepStatus(t, conn, "drain-highs@0"); got != db.StepSkipped {
		t.Errorf("drain-highs@0 = %q after its gate ended skipped, want %q", got, db.StepSkipped)
	}
	if got := stepStatus(t, conn, "summarize@0"); got != db.StepSkipped {
		t.Errorf("summarize@0 = %q, want %q (transitive)", got, db.StepSkipped)
	}
	if got := stepStatus(t, conn, "verify@0"); got != db.StepPending {
		t.Errorf("verify@0 = %q, want pending — after-only, unaffected", got)
	}
	loadScheduler(t, conn, runID, nowMS, func(sched *Scheduler) {
		if ok, cond := sched.Ready(stepNamed(t, sched, "verify@0")); !ok {
			t.Errorf("verify@0 not ready over the skipped gate: %q", cond)
		}
	})
}

// afterFiredWhenSrc skips the gate at EXPANSION: its `when` is false for an
// issue without the label, so activation creates it `skipped` (§5.3.1) — and
// must create its `after_fired` successor skipped with it, in the fat
// transaction, before any step has run.
const afterFiredWhenSrc = `
[pipeline]
name = "after-fired-when"
version = 1

[match]
kind = ["task"]

[[step]]
name = "verify"
executor = "verify"
emits = "report"

[[step]]
name = "vote"
after = ["verify"]
type = "human"
on_fail = "skip"
when = "labels contains needs-vote"

[[step]]
name = "drain"
after = ["vote"]
after_fired = ["vote"]
executor = "drain"
emits = "drain-report"

[[step]]
name = "finish"
after = ["vote"]
executor = "finish"
emits = "record"
`

func TestAfterFiredFollowsAWhenSkipAtExpansion(t *testing.T) {
	conn := mustDB(t)
	registerSource(t, conn, []byte(afterFiredWhenSrc), "after-fired-when.toml")

	// Without the label the gate's `when` is false: gate and successor are
	// created skipped, the after-only successor pending.
	without := createIssue(t, conn, "without", "a body", "task", nil)
	run := startRun(t, conn, without)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	if got := stepStatus(t, conn, "vote@0"); got != db.StepSkipped {
		t.Fatalf("vote@0 = %q at activation with its when false, want %q", got, db.StepSkipped)
	}
	if got := stepStatus(t, conn, "drain@0"); got != db.StepSkipped {
		t.Errorf("drain@0 = %q at activation, want %q — a when-skipped gate's "+
			"after_fired successor is created skipped with it", got, db.StepSkipped)
	}
	if got := stepStatus(t, conn, "finish@0"); got != db.StepPending {
		t.Errorf("finish@0 = %q at activation, want pending", got)
	}
	if data := skipEventData(t, conn, run.ID, "drain@0"); !strings.Contains(data, "vote@0") {
		t.Errorf("drain@0 step-skipped data = %q, want it to name vote@0", data)
	}

	// With the label the gate is live, and so is its successor.
	conn2 := mustDB(t)
	registerSource(t, conn2, []byte(afterFiredWhenSrc), "after-fired-when.toml")
	with := createIssue(t, conn2, "with", "a body", "task", []string{"needs-vote"})
	run2 := startRun(t, conn2, with)
	_, err = activate(conn2, run2.ID)
	testsupport.Must(t, err, "activate: %v", err)

	for _, inst := range []string{"vote@0", "drain@0", "finish@0"} {
		if got := stepStatus(t, conn2, inst); got != db.StepPending {
			t.Errorf("%s = %q at activation with the label, want pending", inst, got)
		}
	}
}

// afterFiredQuorumSrc puts the gate behind a fanned routing step with a real
// quorum, so the skip arrives by the ONE routing path that terminalizes a
// step without reconciling — resolveQuorumMisses, inside `next`.
const afterFiredQuorumSrc = `
[pipeline]
name = "after-fired-quorum"
version = 1

[match]
kind = ["task"]

[[step]]
name = "spread"
after = []
fanout = ["a", "b", "c", "d"]
min_siblings = 2
emits = "findings"
on_fail = "skip"
threshold = { "gate" = "any(status == blocked)" }

[[step]]
name = "gate"
after = ["spread"]
type = "human"
on_fail = "skip"

[[step]]
name = "drain"
after = ["gate"]
after_fired = ["gate"]
executor = "drain"
emits = "drain-report"
`

func TestAfterFiredCascadesOnTheQuorumMissPath(t *testing.T) {
	conn := mustDB(t)
	runID, _ := activateInterposed(t, conn, afterFiredQuorumSrc)
	e := testEngine()

	// One `done` sibling routing `pass`, three that ended without a result:
	// the join completes below the quorum, and the skip of the gate — and the
	// cascade behind it — happens inside `next`'s resolution.
	claimAndComplete(t, conn, e, "spread@0#0", "clear", `[{"status":"ok"}]`)
	if got := stepStatus(t, conn, "gate@0"); got != db.StepPending {
		t.Fatalf("gate@0 = %q with siblings still open, want pending", got)
	}
	for i := 1; i < 4; i++ {
		execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ?`,
			db.StepFailedRouted, fmt.Sprintf("spread@0#%d", i))
	}

	answer, err := e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)

	if got := stepStatus(t, conn, "gate@0"); got != db.StepSkipped {
		t.Fatalf("gate@0 = %q after the missed quorum routed, want %q", got, db.StepSkipped)
	}
	if got := stepStatus(t, conn, "drain@0"); got != db.StepSkipped {
		t.Errorf("drain@0 = %q, want %q — the cascade must run on the quorum-miss "+
			"path, which reconciles nothing", got, db.StepSkipped)
	}
	for _, row := range answer.Steps {
		if row.Instance == "drain@0" {
			t.Errorf("drain@0 offered by the same `next` that skipped it: %v", instancesIn(answer))
		}
	}
}
