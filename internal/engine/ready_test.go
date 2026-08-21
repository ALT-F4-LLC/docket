package engine

import (
	"database/sql"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// ---------------------------------------------------------------------------
// Harness for the readiness predicate
// ---------------------------------------------------------------------------

// loadScheduler opens a transaction, loads the scheduler over a run, and hands
// both to fn. Every readiness assertion runs through it so the predicate is
// always answered over ONE consistent snapshot, exactly as `next --run` does.
func loadScheduler(t *testing.T, conn *sql.DB, runID int, at int64, fn func(*Scheduler)) {
	t.Helper()

	defs, err := StepDefinitions(conn, runID)
	testsupport.Must(t, err, "loading definitions: %v", err)

	tx, err := conn.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, runID, defs, at)
	testsupport.Must(t, err, "LoadScheduler: %v", err)
	fn(sched)
}

// stepNamed finds a loaded step by its rendered instance identity.
func stepNamed(t *testing.T, sched *Scheduler, instance string) *db.Step {
	t.Helper()
	for _, s := range sched.Steps() {
		if s.Instance == instance {
			return s
		}
	}
	t.Fatalf("no step %q among %d loaded steps", instance, len(sched.Steps()))
	return nil
}

// activatedRun is the common setup: fixture registered, one issue, run
// activated, steps expanded.
func activatedRun(t *testing.T, conn *sql.DB) (*model.Run, int) {
	t.Helper()
	registerFixture(t, conn)
	issue := createIssue(t, conn, "do the thing", "a body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	return run, issue
}

// markDispatched records that a run has been handed to the machine at least
// once, which is what GuardStop now requires before a pending step of that run
// can block a stop (DKT-71).
//
// It writes the `dispatches` row directly rather than driving `dispatch open`,
// because what the guard reads is the bare EXISTENCE of a dispatch — the row's
// contents, its status, and its manifest are all irrelevant to the predicate,
// and a fixture that opened a real one would couple every guard test to the
// dispatcher's own rules about what is offerable.
//
// Tests whose subject is something OTHER than dispatch call this so their
// fixture describes a run the machine has actually started, which is the state
// they were always meant to be describing: a held cluster and an open vote
// panel are both reachable only after a dispatch in production.
func markDispatched(t *testing.T, conn *sql.DB, runID int) {
	t.Helper()
	execSQL(t, conn,
		`INSERT INTO dispatches (run_id, status, opened_seq, expires_ms, created_at_ms)
		 VALUES (?, 'closed', 0, ?, ?)`, runID, nowMS+60_000, nowMS)
}

// execSQL runs a statement against the test database.
func execSQL(t *testing.T, conn *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := conn.Exec(query, args...)
	testsupport.Must(t, err, "exec %q: %v", query, err)
}

// ---------------------------------------------------------------------------
// R1-R7, each independently satisfied and falsified
// ---------------------------------------------------------------------------

// TestReadyBaseline is the satisfied case every falsification below is measured
// against: the fixture's root step, on a freshly activated run, is ready.
//
// It exists separately because a falsification test that never saw the
// predicate return true would pass just as well against a predicate that always
// returns false.
func TestReadyBaseline(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		root := stepNamed(t, sched, "implement@0")
		ready, cond := sched.Ready(root)
		if !ready {
			t.Fatalf("implement@0 is not ready: %s", cond)
		}

		// And its successor is NOT ready, because R3 is unmet — the baseline
		// for the R3 case below.
		for _, s := range sched.Steps() {
			if s.StepName == "review" {
				if ready, cond := sched.Ready(s); ready {
					t.Errorf("%s is ready before implement@0 is done", s.Instance)
				} else if cond != CondPredecessors {
					t.Errorf("%s blocked by %q, want %q", s.Instance, cond, CondPredecessors)
				}
			}
		}
	})
}

// TestReadyR1RunActive falsifies R1: a run that is not `active` offers nothing,
// whatever its steps say.
func TestReadyR1RunActive(t *testing.T) {
	for _, status := range []model.RunStatus{
		model.RunPlanning, model.RunWaitingHuman, model.RunDone, model.RunAbandoned,
	} {
		t.Run(string(status), func(t *testing.T) {
			conn := mustDB(t)
			run, _ := activatedRun(t, conn)

			execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`, string(status), run.ID)

			loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
				root := stepNamed(t, sched, "implement@0")
				ready, cond := sched.Ready(root)
				if ready {
					t.Errorf("a %s run offered a ready step", status)
				}
				if cond != CondRunActive {
					t.Errorf("blocked by %q, want %q", cond, CondRunActive)
				}
			})
		})
	}
}

// TestReadyR2IssueDependencies falsifies R2: an issue whose `depends_on`
// predecessor is not `done` offers no step.
//
// This is the ISSUE graph, distinct from R3's step graph — a distinction worth
// a test of its own because both are called "dependencies" and conflating them
// would make a two-issue run schedule its second issue's steps immediately.
func TestReadyR2IssueDependencies(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	blocker := createIssue(t, conn, "first", "body", "task", nil)
	dependent := createIssue(t, conn, "second", "body", "task", nil)

	// `second` depends_on `first`.
	execSQL(t, conn,
		`INSERT INTO issue_relations (source_issue_id, target_issue_id, relation_type, created_at)
		 VALUES (?, ?, 'depends_on', '2026-08-02T00:00:00Z')`, dependent, blocker)

	run := startRun(t, conn, blocker, dependent)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// The dependent issue's steps exist only if it expanded; force expansion so
	// the predicate is what refuses, not the absence of a row.
	execSQL(t, conn, `UPDATE issues SET status = 'done' WHERE id = ?`, blocker)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "re-activate: %v", err)

	// Now put the blocker back to not-done: the steps exist, the dependency does not hold.
	execSQL(t, conn, `UPDATE issues SET status = 'in-progress' WHERE id = ?`, blocker)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		var checked bool
		for _, s := range sched.Steps() {
			if s.IssueID != dependent || s.StepName != "implement" {
				continue
			}
			checked = true
			ready, cond := sched.Ready(s)
			if ready {
				t.Errorf("%s is ready while its issue's dependency is unmet", s.Instance)
			}
			if cond != CondIssueDeps {
				t.Errorf("blocked by %q, want %q", cond, CondIssueDeps)
			}
		}
		if !checked {
			t.Fatal("the dependent issue expanded no `implement` step to test")
		}
	})
}

// TestReadyR3Predecessors falsifies and satisfies R3, including the FANOUT JOIN:
// a successor of a fanned-out step waits for every sibling, not the first.
func TestReadyR3Predecessors(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	// `review` fans out four ways in the fixture. Complete `implement@0` so the
	// siblings become ready, then complete them one at a time and assert
	// `synthesize@0` stays blocked until the LAST one lands.
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ? AND issue_id = ?`,
		db.StepDone, "implement@0", issue)

	var siblings []string
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		for _, s := range sched.Steps() {
			if s.StepName == "review" {
				siblings = append(siblings, s.Instance)
				if ready, cond := sched.Ready(s); !ready {
					t.Errorf("%s not ready after implement@0 is done: %s", s.Instance, cond)
				}
			}
		}
	})
	if len(siblings) != 4 {
		t.Fatalf("review expanded %d siblings, want 4", len(siblings))
	}

	for i, instance := range siblings {
		execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ? AND issue_id = ?`,
			db.StepDone, instance, issue)

		last := i == len(siblings)-1
		loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
			synth := stepNamed(t, sched, "synthesize@0")
			ready, cond := sched.Ready(synth)
			switch {
			case last && !ready:
				t.Errorf("synthesize@0 not ready after all four siblings are done: %s", cond)
			case !last && ready:
				t.Errorf("synthesize@0 is ready after only %d of 4 siblings — "+
					"the fanout join is not waiting for every sibling", i+1)
			case !last && cond != CondPredecessors:
				t.Errorf("blocked by %q, want %q", cond, CondPredecessors)
			}
		})
	}
}

// TestReadyR3SkippedPredecessorSatisfies pins that a `skipped` predecessor does
// NOT strand its successors. §11.1 makes a false `when` mean the step does not
// run, not that everything after it is unschedulable — and expansion creates
// the skipped row precisely so the `after` edge stays resolvable (§5.3.1).
func TestReadyR3SkippedPredecessorSatisfies(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ? AND issue_id = ?`,
		db.StepSkipped, "implement@0", issue)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		for _, s := range sched.Steps() {
			if s.StepName != "review" {
				continue
			}
			if ready, cond := sched.Ready(s); !ready {
				t.Errorf("%s blocked by a SKIPPED predecessor (%s) — "+
					"a skipped step must not strand its successors", s.Instance, cond)
			}
		}
	})
}

// TestReadyR4ScopeConflict falsifies R4 at the predicate level: a step whose
// issue's scope intersects a CLAIMED step's is not ready, and the same step is
// ready once that holder is done.
func TestReadyR4ScopeConflict(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	a := createIssue(t, conn, "issue a", "body", "task", nil)
	b := createIssue(t, conn, "issue b", "body", "task", nil)

	// Overlapping scopes.
	err := db.SetIssueScopeGlobs(conn, a, `["internal/db/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	err = db.SetIssueScopeGlobs(conn, b, `["internal/db/leases.go"]`)
	testsupport.Must(t, err, "setting scope: %v", err)

	run := startRun(t, conn, a, b)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// Claim issue a's root step.
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE issue_id = ? AND step_name = 'implement'`,
		db.StepClaimed, a)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		var checked bool
		for _, s := range sched.Steps() {
			if s.IssueID != b || s.StepName != "implement" {
				continue
			}
			checked = true
			ready, cond := sched.Ready(s)
			if ready {
				t.Error("a step is ready while another claimed step holds an intersecting scope")
			}
			if cond != CondScope {
				t.Errorf("blocked by %q, want %q", cond, CondScope)
			}
		}
		if !checked {
			t.Fatal("issue b expanded no `implement` step to test")
		}
	})

	// S3: a `done` holder excludes nothing.
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE issue_id = ? AND step_name = 'implement'`,
		db.StepDone, a)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		for _, s := range sched.Steps() {
			if s.IssueID != b || s.StepName != "implement" {
				continue
			}
			if ready, cond := sched.Ready(s); !ready {
				t.Errorf("blocked by %q after the holder finished — "+
					"a done step excludes nothing (S3)", cond)
			}
		}
	})
}

// TestReadyR4SameIssueDoesNotSelfExclude pins the exception that makes fanout
// work at all: two steps of the SAME issue share a scope by construction, so
// excluding on it would serialize every pipeline against itself and the
// fixture's four-way `review` fanout would never run more than one sibling.
func TestReadyR4SameIssueDoesNotSelfExclude(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	issue := createIssue(t, conn, "scoped", "body", "task", nil)
	err := db.SetIssueScopeGlobs(conn, issue, `["internal/**"]`)
	testsupport.Must(t, err, "setting scope: %v", err)
	run := startRun(t, conn, issue)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	execSQL(t, conn, `UPDATE steps SET status = ? WHERE issue_id = ? AND step_name = 'implement'`,
		db.StepDone, issue)

	// Claim one review sibling; the others must stay ready.
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ? AND issue_id = ?`,
		db.StepClaimed, "review@0#0", issue)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		for _, s := range sched.Steps() {
			if s.StepName != "review" || s.Instance == "review@0#0" {
				continue
			}
			if ready, cond := sched.Ready(s); !ready {
				t.Errorf("%s blocked by %q while a SIBLING of the same issue holds "+
					"the scope — an issue must not exclude itself", s.Instance, cond)
			}
		}
	})
}

// TestReadyR5ClassHeadroom falsifies R5 with a `[limits]` max, and pins that
// core ships NO default: a class with no declared max is unbounded.
//
// The serialized-writer behavior engine-core §5 ratifies is produced HERE, by
// instance config, not by a hardcoded rule — §6.5 is explicit that a hardcoded
// writer serialization would be instance policy in core and would fail the
// genericity rule on its face.
func TestReadyR5ClassHeadroom(t *testing.T) {
	const src = `
[pipeline]
name = "limited"
version = 1

[match]
kind = ["task"]

[limits]
write = { max = 1 }

[[step]]
name = "one"
executor = "w"
class = "write"
emits = "out"
after = []

[[step]]
name = "two"
executor = "w"
class = "write"
emits = "out"
after = []
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "limited.toml")

	issue := createIssue(t, conn, "limited", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	// Both roots are ready with the class empty.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		for _, name := range []string{"one@0", "two@0"} {
			if ready, cond := sched.Ready(stepNamed(t, sched, name)); !ready {
				t.Errorf("%s not ready with an empty class: %s", name, cond)
			}
		}
	})

	// Claim one; the class is now full at max = 1.
	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ? AND issue_id = ?`,
		db.StepClaimed, "one@0", issue)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		ready, cond := sched.Ready(stepNamed(t, sched, "two@0"))
		if ready {
			t.Error("two@0 is ready with the write class full at max = 1")
		}
		if cond != CondHeadroom {
			t.Errorf("blocked by %q, want %q", cond, CondHeadroom)
		}
	})
}

// TestReadyR5UndeclaredClassIsUnbounded is the genericity half of R5: core
// ships no class named `write` and no default of 1. The SAME workflow without
// the `[limits]` table schedules both steps concurrently.
func TestReadyR5UndeclaredClassIsUnbounded(t *testing.T) {
	const src = `
[pipeline]
name = "unlimited"
version = 1

[match]
kind = ["task"]

[[step]]
name = "one"
executor = "w"
class = "write"
emits = "out"
after = []

[[step]]
name = "two"
executor = "w"
class = "write"
emits = "out"
after = []
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "unlimited.toml")

	issue := createIssue(t, conn, "unlimited", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ? AND issue_id = ?`,
		db.StepClaimed, "one@0", issue)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if ready, cond := sched.Ready(stepNamed(t, sched, "two@0")); !ready {
			t.Errorf("two@0 blocked by %q with NO [limits] declared — "+
				"core must ship no class named `write` and no default of 1 "+
				"(§6.5, genericity.md)", cond)
		}
	})
}

// TestReadyR6Status falsifies R6 across every non-pending persisted status.
func TestReadyR6Status(t *testing.T) {
	for _, status := range []string{
		db.StepClaimed, db.StepRunning, db.StepGated, db.StepDone,
		db.StepWaitingHuman, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
	} {
		t.Run(status, func(t *testing.T) {
			conn := mustDB(t)
			run, issue := activatedRun(t, conn)

			execSQL(t, conn, `UPDATE steps SET status = ? WHERE instance = ? AND issue_id = ?`,
				status, "implement@0", issue)

			loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
				ready, cond := sched.Ready(stepNamed(t, sched, "implement@0"))
				if ready {
					t.Errorf("a %s step is ready", status)
				}
				if cond != CondStatus {
					t.Errorf("blocked by %q, want %q", cond, CondStatus)
				}
			})
		})
	}
}

// TestReadyR7BudgetSeam pins R7's S3 behavior: the check is a seam returning
// true (§6.3), so it never blocks at this stage.
//
// The test exists so S6's real check has a call site already under test, and so
// a reviewer does not read the always-true return as an oversight.
func TestReadyR7BudgetSeam(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if !sched.budgetHeadroom(stepNamed(t, sched, "implement@0")) {
			t.Error("R7 refused a step on a run with no cap; an unlimited run has " +
				"headroom by definition (§4.8 B29)")
		}
	})
}

// ---------------------------------------------------------------------------
// Ordering
// ---------------------------------------------------------------------------

// TestReadyOrderingIsPriorityThenAge pins §2's "Ordering: priority then age",
// with a TOTAL order: two steps created in the same millisecond — which is the
// common case, since one expansion writes them all — must still order
// deterministically, or the topology goldens flap.
func TestReadyOrderingIsPriorityThenAge(t *testing.T) {
	conn := mustDB(t)
	registerFixture(t, conn)

	low := createIssue(t, conn, "low", "body", "task", nil)
	high := createIssue(t, conn, "high", "body", "task", nil)
	execSQL(t, conn, `UPDATE issues SET priority = ? WHERE id = ?`,
		string(model.PriorityCritical), high)
	execSQL(t, conn, `UPDATE issues SET priority = ? WHERE id = ?`,
		string(model.PriorityLow), low)

	run := startRun(t, conn, low, high)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		var roots []*db.Step
		for _, s := range sched.Steps() {
			if s.StepName == "implement" {
				roots = append(roots, s)
			}
		}
		if len(roots) != 2 {
			t.Fatalf("found %d root steps, want 2", len(roots))
		}

		sched.SortSteps(roots)
		if roots[0].IssueID != high {
			t.Errorf("first step belongs to issue %d, want the CRITICAL issue %d",
				roots[0].IssueID, high)
		}

		// Total order: sorting the reversed input yields the same sequence.
		reversed := []*db.Step{roots[1], roots[0]}
		sched.SortSteps(reversed)
		if reversed[0].ID != roots[0].ID || reversed[1].ID != roots[1].ID {
			t.Error("the sort is not total: reversing the input changed the output, " +
				"so equal-priority equal-age steps order nondeterministically")
		}
	})
}

// TestReadyOrderingTieBreaksById pins the id tie-break in isolation: same
// priority, same creation millisecond.
func TestReadyOrderingTieBreaksById(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		var reviews []*db.Step
		for _, s := range sched.Steps() {
			if s.StepName == "review" {
				reviews = append(reviews, s)
			}
		}
		if len(reviews) != 4 {
			t.Fatalf("found %d review siblings, want 4", len(reviews))
		}

		// All four were written by one expansion, so they share a timestamp.
		for _, s := range reviews[1:] {
			if s.CreatedAtMS != reviews[0].CreatedAtMS {
				t.Fatalf("siblings have different timestamps; the tie-break is untested here")
			}
		}

		shuffled := []*db.Step{reviews[3], reviews[1], reviews[2], reviews[0]}
		sched.SortSteps(shuffled)
		for i := 1; i < len(shuffled); i++ {
			if shuffled[i-1].ID >= shuffled[i].ID {
				t.Errorf("ids out of order after sorting: %d before %d",
					shuffled[i-1].ID, shuffled[i].ID)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Expiry and max_step_duration
// ---------------------------------------------------------------------------

// TestExpiredLeaseIsReapable is the ordinary half: a lapsed lease makes a
// claimed step reapable.
func TestExpiredLeaseIsReapable(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	execSQL(t, conn,
		`UPDATE steps SET status = ?, owner = 'w', token_hash = 'h', expires_ms = ?
		  WHERE instance = ? AND issue_id = ?`,
		db.StepClaimed, nowMS+1000, "implement@0", issue)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if sched.Expired(stepNamed(t, sched, "implement@0")) {
			t.Error("a live lease reads as expired")
		}
	})
	loadScheduler(t, conn, run.ID, nowMS+2000, func(sched *Scheduler) {
		if !sched.Expired(stepNamed(t, sched, "implement@0")) {
			t.Error("a lapsed lease does not read as expired")
		}
	})
}

// TestMaxStepDurationReapsPastALiveHeartbeat is the clause that makes
// `max_step_duration` worth having, and the one an implementation naturally
// omits: it is a SCHEDULE-TO-CLOSE bound against `started_ms`, "independent of
// heartbeats — so a runaway executor cannot renew forever" (§11.1).
//
// The step here has a lease that is LIVE and freshly extended. It must still be
// reaped, or the bound is just a second lease timeout.
func TestMaxStepDurationReapsPastALiveHeartbeat(t *testing.T) {
	const src = `
[pipeline]
name = "bounded"
version = 1

[match]
kind = ["task"]

[limits]
slow = { max = 4, max_step_duration = "45m" }

[[step]]
name = "long"
executor = "w"
class = "slow"
emits = "out"
after = []
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "bounded.toml")

	issue := createIssue(t, conn, "bounded", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	const hour = 60 * 60 * 1000
	// Claimed an hour ago, with a lease that does not lapse for another hour —
	// a worker that has been heartbeating happily the whole time.
	execSQL(t, conn,
		`UPDATE steps SET status = ?, owner = 'w', token_hash = 'h',
		        expires_ms = ?, started_ms = ?
		  WHERE instance = ? AND issue_id = ?`,
		db.StepClaimed, nowMS+hour, nowMS-hour, "long@0", issue)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		step := stepNamed(t, sched, "long@0")

		// The lease itself is live — proving the reap below is not a lease timeout.
		if !step.Lease().Live(nowMS) {
			t.Fatal("the lease lapsed; this test is no longer about max_step_duration")
		}

		if !sched.Expired(step) {
			t.Error("a step 60m into a 45m max_step_duration was not reaped " +
				"despite a live lease — the bound is schedule-to-close and must " +
				"be independent of heartbeats (§6.3)")
		}
	})

	// And within the bound it is not reaped.
	loadScheduler(t, conn, run.ID, nowMS-hour+1000, func(sched *Scheduler) {
		if sched.Expired(stepNamed(t, sched, "long@0")) {
			t.Error("a step one second into a 45m bound was reaped")
		}
	})
}

// TestExpiryIgnoresStepsOfANonActiveRun: a lease must not lapse
// because wall time passed while the run it belongs to was PAUSED.
//
// THE SHAPE THAT MAKES THIS REACHABLE (and the reason "human gates are
// structurally immune" is not the whole story): reconcile pauses a run when
// ANY step is parked — `parked > 0` in rollupRunTx — while its SIBLINGS stay
// `claimed`/`running`. So a mid-wave human hold gives exactly this state: run
// `waiting-human`, one step parked, another still legitimately claimed by a
// live worker. That worker's lease then ticks down against a clock nobody can
// answer, because R1 refuses to offer anything until the run is active again.
//
// Reaping it is a false positive with teeth: the step returns to `pending`
// with its lease cleared, its class takes a write-reap hold (§6.4), and the
// operator who resolves the park finds work they never lost, undone.
func TestExpiryIgnoresStepsOfANonActiveRun(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	// A worker holding a lease that lapsed an hour ago — the ordinary reapable
	// shape, which TestExpiredLeaseIsReapable pins from the other side.
	const hour = 60 * 60 * 1000
	execSQL(t, conn,
		`UPDATE steps SET status = ?, owner = 'w', token_hash = 'h', expires_ms = ?
		  WHERE instance = ? AND issue_id = ?`,
		db.StepClaimed, nowMS-hour, "implement@0", issue)

	// While the run is ACTIVE it is reapable. Asserting this first keeps the
	// test honest: without it, a fix that broke reaping outright would pass.
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if !sched.Expired(stepNamed(t, sched, "implement@0")) {
			t.Fatal("a lapsed lease on an ACTIVE run was not reapable; this " +
				"test can no longer tell the two cases apart")
		}
	})

	// Every non-active status, including the terminal ones: a run nobody can
	// schedule against is a run whose leases have no clock to race.
	for _, status := range []model.RunStatus{
		model.RunWaitingHuman, model.RunPlanning,
		model.RunDone, model.RunAbandoned,
	} {
		execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
			string(status), run.ID)

		loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
			if sched.Expired(stepNamed(t, sched, "implement@0")) {
				t.Errorf("a claimed step on a %s run was reaped for a lease "+
					"that lapsed while the run was not active; R1 refuses to "+
					"offer anything in that state, so the TTL is counting "+
					"against a worker nobody can replace (DKT-33)", status)
			}
		})
	}
}

// TestPausedRunSuspendsRatherThanExemptsTheLease is the other half,
// and the one that keeps the fix from becoming an escape hatch: pausing a run
// must SUSPEND the TTL, not forgive it.
//
// A step whose lease lapsed during a pause is still reaped by the first reap
// after the run returns to `active` — nothing wrote `expires_ms`, so the debt
// is intact. Without this, "pause the run" would be a way to make a wedged
// writer permanently unreapable.
func TestPausedRunSuspendsRatherThanExemptsTheLease(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	const hour = 60 * 60 * 1000
	execSQL(t, conn,
		`UPDATE steps SET status = ?, owner = 'w', token_hash = 'h', expires_ms = ?
		  WHERE instance = ? AND issue_id = ?`,
		db.StepClaimed, nowMS-hour, "implement@0", issue)

	execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
		string(model.RunWaitingHuman), run.ID)
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if sched.Expired(stepNamed(t, sched, "implement@0")) {
			t.Fatal("the paused run reaped; the suspension half is already broken")
		}
	})

	// The park resolves and the run resumes. The lease is still lapsed, and the
	// step is reapable again with no further wall-clock time passing.
	execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
		string(model.RunActive), run.ID)
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if !sched.Expired(stepNamed(t, sched, "implement@0")) {
			t.Error("a lease that lapsed during a pause was not reaped once the " +
				"run resumed — the pause must SUSPEND the TTL, not forgive it, " +
				"or pausing becomes a way to make a wedged writer immortal")
		}
	})
}

// TestMaxStepDurationStillBoundsAnActiveRun is AC2: the suspension
// must not weaken `max_step_duration` where it still applies. A runaway
// executor on an ACTIVE run is reaped past a live heartbeat exactly as before.
//
// TestMaxStepDurationReapsPastALiveHeartbeat covers the same property, but it
// would keep passing if the run-status guard were written to skip the
// schedule-to-close half specifically; this pins both halves under one guard.
func TestMaxStepDurationStillBoundsAnActiveRun(t *testing.T) {
	const src = `
[pipeline]
name = "bounded-active"
version = 1

[match]
kind = ["task"]

[limits]
slow = { max = 4, max_step_duration = "45m" }

[[step]]
name = "long"
executor = "w"
class = "slow"
emits = "out"
after = []
`
	conn := mustDB(t)
	registerSource(t, conn, []byte(src), "bounded-active.toml")

	issue := createIssue(t, conn, "bounded-active", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	const hour = 60 * 60 * 1000
	execSQL(t, conn,
		`UPDATE steps SET status = ?, owner = 'w', token_hash = 'h',
		        expires_ms = ?, started_ms = ?
		  WHERE instance = ? AND issue_id = ?`,
		db.StepClaimed, nowMS+hour, nowMS-hour, "long@0", issue)

	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		step := stepNamed(t, sched, "long@0")
		if !step.Lease().Live(nowMS) {
			t.Fatal("the lease lapsed; this test is no longer about max_step_duration")
		}
		if !sched.Expired(step) {
			t.Error("the run-status guard swallowed max_step_duration on an " +
				"ACTIVE run; a runaway executor must still not renew forever")
		}
	})

	// And on a paused run the schedule-to-close bound is suspended too — the
	// same reasoning, stated for the half that is easy to leave unguarded.
	execSQL(t, conn, `UPDATE runs SET status = ? WHERE id = ?`,
		string(model.RunWaitingHuman), run.ID)
	loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
		if sched.Expired(stepNamed(t, sched, "long@0")) {
			t.Error("max_step_duration reaped a step on a paused run; a run " +
				"nobody can schedule against has no clock to run out")
		}
	})
}

// TestExpiryIgnoresUnclaimedAndTerminalSteps pins that reaping targets only
// steps with a holder to lose.
func TestExpiryIgnoresUnclaimedAndTerminalSteps(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)

	for _, status := range []string{
		db.StepPending, db.StepGated, db.StepDone, db.StepWaitingHuman,
		db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
	} {
		execSQL(t, conn,
			`UPDATE steps SET status = ?, owner = 'w', token_hash = 'h', expires_ms = ?
			  WHERE instance = ? AND issue_id = ?`,
			status, nowMS-1, "implement@0", issue)

		loadScheduler(t, conn, run.ID, nowMS, func(sched *Scheduler) {
			if sched.Expired(stepNamed(t, sched, "implement@0")) {
				t.Errorf("a %s step with a lapsed lease was reapable; only "+
					"claimed and running steps have a holder to lose", status)
			}
		})
	}
}

// TestMergeLimitsTakesTheTighterBound pins the cross-workflow rule: classes are
// repo-wide accounting keys, so two pipelines naming one class mean one class,
// and the stricter declaration wins. Taking the looser one would silently
// discard a bound someone set for a reason.
func TestMergeLimitsTakesTheTighterBound(t *testing.T) {
	defs := map[int]*workflow.Definition{
		1: {
			Pipeline: workflow.Pipeline{Name: "loose", Version: 3},
			Limits:   map[string]workflow.Limit{"write": {Max: 4}},
		},
		2: {
			Pipeline: workflow.Pipeline{Name: "tight", Version: 7},
			Limits:   map[string]workflow.Limit{"write": {Max: 1}},
		},
	}
	limits, sources := mergeLimits(defs)
	if got := limits["write"].Max; got != 1 {
		t.Errorf("merged max = %d, want 1 (the tighter bound)", got)
	}
	// DKT-23: the refusal names where the cap is set, so the source must be
	// the workflow whose declaration WON, not whichever was merged last.
	if got := sources["write"]; got != "tight@7" {
		t.Errorf("cap source = %q, want %q (the winning declaration)", got, "tight@7")
	}

	// And the unbounded case: max 0 means unbounded, so a real bound wins.
	defs = map[int]*workflow.Definition{
		1: {
			Pipeline: workflow.Pipeline{Name: "unbounded", Version: 1},
			Limits:   map[string]workflow.Limit{"write": {}},
		},
		2: {
			Pipeline: workflow.Pipeline{Name: "bounded", Version: 2},
			Limits:   map[string]workflow.Limit{"write": {Max: 2}},
		},
	}
	limits, sources = mergeLimits(defs)
	if got := limits["write"].Max; got != 2 {
		t.Errorf("merged max = %d, want 2", got)
	}
	if got := sources["write"]; got != "bounded@2" {
		t.Errorf("cap source = %q, want %q", got, "bounded@2")
	}
}
