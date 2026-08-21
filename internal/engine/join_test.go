package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// Fanout joins J1-J5 — TDD §7.5, engine-spec §2 verbatim.
//
//	J1  a fanned-out step joins when EVERY sibling is terminal
//	    (done | skipped | superseded | failed-routed)
//	J2  a sibling in `waiting-human` PARKS the issue
//	J3  downstream `inputs` resolve over `done` siblings ONLY
//	J4  `on_fail` applies PER SIBLING
//	J5  `min_siblings` permits quorum joins: the join STILL WAITS FOR ALL
//	    siblings (no early cancel in v1); a shortfall routes per `on_fail`
//
// The fixture's `review` is a four-way fanout, which is what these exercise.

// quorumFixture is a four-way fanout with `min_siblings = 2` — the shape J5
// describes and the fixture does not have.
const quorumFixture = `
[pipeline]
name = "quorum"
version = 1

[match]
kind = ["task"]

[[step]]
name = "spread"
after = []
fanout = ["a", "b", "c", "d"]
min_siblings = 2
emits = "findings"
on_fail = "waiting-human"

[[step]]
name = "gather"
after = ["spread"]
executor = "gather"
emits = "summary"
inputs = ["spread.findings"]
`

// ---------------------------------------------------------------------------
// J1: every sibling terminal
// ---------------------------------------------------------------------------

// TestJoinWaitsForEverySibling is J1: three of four `done` does not release the
// join.
func TestJoinWaitsForEverySibling(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	// Three siblings done, one still pending.
	for i := range 3 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
	}

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		successor := stepNamed(t, sched, "synthesize@0")
		if ready, cond := sched.Ready(successor); ready {
			t.Error("synthesize@0 is ready with review@0#3 still pending; " +
				"the join must wait for every sibling (J1)")
		} else if cond != CondPredecessors {
			t.Errorf("synthesize@0 blocked by %q, want %q", cond, CondPredecessors)
		}
	})

	// The fourth releases it.
	claimAndComplete(t, conn, e, "review@0#3", "findings", "")

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		successor := stepNamed(t, sched, "synthesize@0")
		if ready, cond := sched.Ready(successor); !ready {
			t.Errorf("synthesize@0 is not ready with all four siblings done: %s", cond)
		}
	})
}

// TestJoinAcceptsEveryTerminalStatus is J1's breadth: the join releases on
// `done | skipped | superseded | failed-routed`, not on `done` alone.
//
// A join that waited for `done` would deadlock on any sibling that ended
// another way — a `when` that skipped it, a loop that superseded it, a failure
// that routed it — and the run would sit forever with nothing left to do.
func TestJoinAcceptsEveryTerminalStatus(t *testing.T) {
	for _, status := range []string{
		db.StepDone, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
	} {
		t.Run(status, func(t *testing.T) {
			conn := mustDB(t)
			run, _ := activatedRun(t, conn)
			e := testEngine()

			claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
			for i := range 3 {
				claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
			}
			// The fourth ends in the status under test, without completing.
			execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ?`,
				status, "review@0#3")

			loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
				successor := stepNamed(t, sched, "synthesize@0")
				if ready, cond := sched.Ready(successor); !ready {
					t.Errorf("synthesize@0 is not ready with a %s sibling: %s — "+
						"%s is terminal and must release the join (J1)",
						status, cond, status)
				}
			})
		})
	}
}

// ---------------------------------------------------------------------------
// J2: a `waiting-human` sibling parks
// ---------------------------------------------------------------------------

// TestWaitingHumanSiblingParksTheJoin is J2: `waiting-human` is NOT terminal, so
// it holds the join open — the issue is parked on the operator's decision.
//
// This is the counterpart of the J1 test above, and the pair is the point:
// `waiting-human` looks terminal (nobody is working on it) and is not (the work
// is not finished). Releasing the join on it would start downstream work on the
// strength of a question nobody has answered.
func TestWaitingHumanSiblingParksTheJoin(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	for i := range 3 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
	}
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ?`,
		db.StepWaitingHuman, "review@0#3")

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		successor := stepNamed(t, sched, "synthesize@0")
		if ready, _ := sched.Ready(successor); ready {
			t.Error("synthesize@0 is ready with a waiting-human sibling; " +
				"J2 parks the issue on the operator's decision")
		}
	})
}

// ---------------------------------------------------------------------------
// J3: inputs resolve over `done` siblings only
// ---------------------------------------------------------------------------

// TestInputsResolveOverDoneSiblingsOnly is J3.
//
// A sibling that ended some other way produced no result, and a bundle that
// included its artifact would feed downstream work a partial or abandoned
// output as though it were a finding.
func TestInputsResolveOverDoneSiblingsOnly(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")
	for i := range 4 {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
	}

	// One sibling completed and is then marked failed-routed: its artifact
	// exists, but it is no longer a `done` producer.
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ?`,
		db.StepFailedRouted, "review@0#1")

	inputs := contextInputs(t, conn, run.ID, "synthesize@0")

	for _, in := range inputs {
		if in.ProducerStep == "review@0#1" {
			t.Error("synthesize@0 bound an input from a failed-routed sibling; " +
				"J3 resolves over `done` siblings only")
		}
	}
	// And all three `done` siblings DO contribute — a filter that dropped too
	// much would pass the assertion above vacuously.
	//
	// Counted by PRODUCER rather than by artifact: `synthesize` declares
	// `inputs = ["review.*"]`, and `*` matches every kind each sibling
	// produced — its own `findings` plus the engine-computed `issue.diff`
	// (§6.7.1 D1 recomputes the diff at every executor step's completion). The
	// question J3 asks is which SIBLINGS contributed, not how many rows they
	// produced between them.
	producers := make(map[string]bool)
	for _, in := range inputs {
		producers[in.ProducerStep] = true
	}
	if len(producers) != 3 {
		t.Errorf("synthesize@0 bound inputs from %d siblings, want 3 (the `done` ones): %v",
			len(producers), producers)
	}
}

// ---------------------------------------------------------------------------
// J4: `on_fail` applies per sibling
// ---------------------------------------------------------------------------

// TestOnFailAppliesPerSibling is J4: one sibling's failure routes THAT SIBLING,
// leaving the others to finish on their own terms.
//
// The alternative reading — a failure routing the whole fanned step — would
// discard three siblings' completed work because a fourth failed, which is
// precisely what a quorum join exists to avoid.
func TestOnFailAppliesPerSibling(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()

	claimAndComplete(t, conn, e, "implement@0", "the change summary", "")

	// One sibling fails; the rest complete normally.
	failStep(t, conn, e, "review@0#1")
	for _, i := range []int{0, 2, 3} {
		claimAndComplete(t, conn, e, fmt.Sprintf("review@0#%d", i), "findings", "")
	}

	// The failed sibling routed per `on_fail`; the others are `done`.
	if got := stepStatus(t, conn, "review@0#1"); got == db.StepDone {
		t.Error("review@0#1 is done after failing; on_fail must route it")
	}
	for _, i := range []int{0, 2, 3} {
		instance := fmt.Sprintf("review@0#%d", i)
		if got := stepStatus(t, conn, instance); got != db.StepDone {
			t.Errorf("%s = %q, want %q — J4 routes per sibling, not per step",
				instance, got, db.StepDone)
		}
	}
	_ = run
}

// ---------------------------------------------------------------------------
// J5: min_siblings, and NO EARLY CANCEL
// ---------------------------------------------------------------------------

// TestQuorumJoinDoesNotCancelEarly is J5's explicit case, and §7.5 names it as
// "the clause an optimizer breaks".
//
// A four-way fanout with `min_siblings = 2` and two early `done` siblings must
// NOT advance until the other two are terminal. Reaching the quorum does not
// release the join — v1 waits for every sibling and compares afterwards.
//
// The failure this excludes is not hypothetical: an implementation that
// short-circuits on reaching the count passes every happy-path test and starts
// downstream work while two siblings are still running, which is the exact
// double-write a scope lock is meant to prevent.
func TestQuorumJoinDoesNotCancelEarly(t *testing.T) {
	conn, run := quorumRun(t)
	e := testEngine()

	// The quorum is MET after two...
	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("spread@0#%d", i), "findings", "")
	}

	// ...but the join does not release: two siblings are still pending.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		successor := stepNamed(t, sched, "gather@0")
		if ready, _ := sched.Ready(successor); ready {
			t.Error("gather@0 is ready with min_siblings met but two siblings " +
				"still running; J5 has NO EARLY CANCEL in v1")
		}
	})

	// Only when every sibling is terminal does it release.
	for i := 2; i < 4; i++ {
		claimAndComplete(t, conn, e, fmt.Sprintf("spread@0#%d", i), "findings", "")
	}
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		successor := stepNamed(t, sched, "gather@0")
		if ready, cond := sched.Ready(successor); !ready {
			t.Errorf("gather@0 is not ready with every sibling terminal: %s", cond)
		}
	})
}

// TestQuorumMissRoutesPerOnFail is J5's failure half: when the `done` count is
// below `min_siblings` AT JOIN, the fanned step routes per its `on_fail`.
//
// Without this the run would wedge silently — every sibling terminal, the
// successor never ready, and nothing recorded to say why.
func TestQuorumMissRoutesPerOnFail(t *testing.T) {
	conn, run := quorumRun(t)
	e := testEngine()

	// One `done`, three failed: the join completes, the quorum (2) is missed.
	claimAndComplete(t, conn, e, "spread@0#0", "findings", "")
	for i := 1; i < 4; i++ {
		execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ?`,
			db.StepFailedRouted, fmt.Sprintf("spread@0#%d", i))
	}

	// `next` is where the miss is resolved.
	_, err := testEngine().NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)

	// The surviving sibling carries the routing, per the step's `on_fail`.
	if got := stepStatus(t, conn, "spread@0#0"); got != db.StepWaitingHuman {
		t.Errorf("spread@0#0 = %q after a missed quorum, want %q (its on_fail)",
			got, db.StepWaitingHuman)
	}
	routing := stepRoutingRaw(t, conn, "spread@0#0")
	if !strings.Contains(routing, "min_siblings") {
		t.Errorf("routing = %q, want it to name min_siblings", routing)
	}

	// And a `join-completed` event records it, so the ledger explains the stop.
	if n := countEvents(t, conn, EventJoinCompleted); n != 1 {
		t.Errorf("%d join-completed events, want 1", n)
	}

	// The resolution is IDEMPOTENT: `next` is called constantly, and a second
	// call must not re-route or re-emit.
	_, err = testEngine().NextSteps(conn, run.ID, 0, nowMS)
	testsupport.Must(t, err, "second NextSteps: %v", err)
	if n := countEvents(t, conn, EventJoinCompleted); n != 1 {
		t.Errorf("%d join-completed events after a second next, want 1", n)
	}
}

// TestQuorumMetJoinsNormally is the satisfied case the two above are measured
// against: exactly `min_siblings` done, the rest terminal, and the successor
// becomes ready.
//
// It exists for the same reason TestReadyBaseline does — a pair of tests that
// only ever saw the join refuse would pass against an implementation that never
// releases it.
func TestQuorumMetJoinsNormally(t *testing.T) {
	conn, run := quorumRun(t)
	e := testEngine()

	for i := range 2 {
		claimAndComplete(t, conn, e, fmt.Sprintf("spread@0#%d", i), "findings", "")
	}
	for i := 2; i < 4; i++ {
		execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ?`,
			db.StepFailedRouted, fmt.Sprintf("spread@0#%d", i))
	}

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		successor := stepNamed(t, sched, "gather@0")
		if ready, cond := sched.Ready(successor); !ready {
			t.Errorf("gather@0 is not ready with the quorum met: %s", cond)
		}
	})

	// And J3 holds across the quorum: only the two `done` siblings are inputs.
	// `gather` declares `spread.findings` — one kind, so one row per sibling.
	inputs := contextInputs(t, conn, run.ID, "gather@0")
	if len(inputs) != 2 {
		t.Errorf("gather@0 bound %d inputs, want 2 (the `done` siblings)", len(inputs))
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// quorumRun activates the four-way `min_siblings = 2` fixture.
func quorumRun(t *testing.T) (*sql.DB, *model.Run) {
	t.Helper()
	conn := mustDB(t)
	registerSource(t, conn, []byte(quorumFixture), "quorum.toml")
	issue := createIssue(t, conn, "quorum", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return conn, run
}

// failStep drives a step through claim and `fail`, so its `on_fail` routing
// applies.
func failStep(t *testing.T, conn *sql.DB, e *Engine, instance string) {
	t.Helper()
	stepID := stepIDByInstance(t, conn, instance)
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claim %s: %v", instance, err)
	err = e.FailStep(conn, stepID, claim.Token, "the worker failed", "", nowMS)
	testsupport.Must(t, err, "fail %s: %v", instance, err)
}

// countEvents counts events of a kind.
func countEvents(t *testing.T, conn *sql.DB, kind string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM events WHERE kind = ?`, kind).Scan(&n)
	testsupport.Must(t, err, "counting %s events: %v", kind, err)
	return n
}
