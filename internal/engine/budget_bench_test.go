package engine

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The measurements docs/spec/performance.md publishes (TDD §12.2).
//
// They are BENCHMARKS RATHER THAN ASSERTIONS on purpose. A test that failed at
// some millisecond threshold would fail on a loaded CI runner and tell nobody
// anything about the shape of the cost; what §4.3's argument needs is the
// SHAPE — the floor query is a SUM over an indexed join, so it should grow
// linearly in claim events with a small constant, and if it ever grows
// otherwise the numbers in performance.md are where that shows up.
//
// The stated fallback, recorded before it is needed: a cache CHECKED AGAINST
// the query — the query still runs, the cache is asserted to match, a mismatch
// is a hard error — never a cache REPLACING it. Recording it now keeps a future
// optimization from quietly becoming C4, the lost update the floor's whole
// design avoids.

// benchSeedDB opens a shared in-memory store, migrates it, and seeds the one
// run / workflow / issue every benchmark here builds on — the steps stay with
// each benchmark, since the step shapes ARE the subjects. `name` keys the
// shared-cache DSN so two benchmarks' stores stay distinct; `parsed` is the
// workflow's stored parse — `{}` where the subject never reads it, the real
// fixture where it does.
func benchSeedDB(b *testing.B, name, parsed string) (conn *sql.DB, runID, issueID, wfID int) {
	b.Helper()

	conn, err := sql.Open("sqlite", "file:"+name+"?mode=memory&cache=shared")
	if err != nil {
		b.Fatalf("opening: %v", err)
	}
	conn.SetMaxOpenConns(1)
	if err := db.Initialize(conn); err != nil {
		b.Fatalf("Initialize: %v", err)
	}
	if err := db.Migrate(conn); err != nil {
		b.Fatalf("Migrate: %v", err)
	}

	res, err := conn.Exec(
		`INSERT INTO runs (request, status, budget, created_at_ms, updated_at_ms, row_version)
		 VALUES ('bench', 'active', 1000000, 1, 1, 1)`)
	if err != nil {
		b.Fatalf("seeding the run: %v", err)
	}
	runID64, _ := res.LastInsertId()

	res, err = conn.Exec(
		`INSERT INTO workflows (name, version, source_sha256, body, parsed,
		                        created_at_ms, row_version)
		 VALUES ('bench', 1, 'abc', 'body', ?, 1, 1)`, parsed)
	if err != nil {
		b.Fatalf("seeding the workflow: %v", err)
	}
	wfID64, _ := res.LastInsertId()

	res, err = conn.Exec(
		`INSERT INTO issues (title, status, priority, kind, created_at, updated_at)
		 VALUES ('bench', 'todo', 'none', 'task', '1', '1')`)
	if err != nil {
		b.Fatalf("seeding the issue: %v", err)
	}
	issueID64, _ := res.LastInsertId()
	return conn, int(runID64), int(issueID64), int(wfID64)
}

// benchFloorDB builds a run carrying n `step-claimed` events.
//
// The events are written directly rather than driven through n real claims,
// because the subject is THE QUERY's cost at a given log size, and driving a
// thousand claims would measure the claim path instead — which has its own
// transaction, its own CAS, and its own context assembly.
func benchFloorDB(b *testing.B, n int) (*sql.DB, int) {
	b.Helper()
	conn, runID, issueID, wfID := benchSeedDB(b, "bench", "{}")

	// n steps, each claimed exactly once — the shape a real run of n claims
	// leaves behind, since every claim writes one event against one step.
	for i := range n {
		res, err := conn.Exec(
			`INSERT INTO steps (run_id, issue_id, workflow_id, step_name, ordinal,
			                    instance, kind, status, attempt, expected_cost,
			                    created_at_ms, updated_at_ms, row_version)
			 VALUES (?, ?, ?, 'work', 0, ?, 'executor', 'done', 1, 1.5, 1, 1, 1)`,
			runID, issueID, wfID, fmt.Sprintf("work@0#%d", i))
		if err != nil {
			b.Fatalf("seeding step %d: %v", i, err)
		}
		stepID, _ := res.LastInsertId()

		if _, err := conn.Exec(
			`INSERT INTO events (at_ms, kind, run_id, step_id, issue_id, data)
			 VALUES (1, 'step-claimed', ?, ?, ?, '{}')`,
			runID, stepID, issueID); err != nil {
			b.Fatalf("seeding event %d: %v", i, err)
		}
	}
	return conn, runID
}

func benchFloor(b *testing.B, n int) {
	conn, runID := benchFloorDB(b, n)
	defer conn.Close()

	b.ResetTimer()
	for b.Loop() {
		tx, err := conn.Begin()
		if err != nil {
			b.Fatalf("Begin: %v", err)
		}
		if _, err := RunFloorTx(tx, runID); err != nil {
			b.Fatalf("RunFloorTx: %v", err)
		}
		tx.Rollback()
	}
}

// BenchmarkFloorQuery10/100/1000 are §12.2's first row: the floor query at
// three log sizes. It runs ONCE PER CLAIM, so its growth is the number §4.3's
// cost argument rests on.
func BenchmarkFloorQuery10(b *testing.B)   { benchFloor(b, 10) }
func BenchmarkFloorQuery100(b *testing.B)  { benchFloor(b, 100) }
func BenchmarkFloorQuery1000(b *testing.B) { benchFloor(b, 1000) }

// benchPrefix builds a run of n pending re-review rows — every one inside
// the committed fixture's loop closure, which is what makes ClaimablePrefix's
// eviction pass scan the full step set for each offered row: the
// `len(out) × len(steps)` term DKT-62 names. Seeded directly, for
// benchFloorDB's reason — the subject is THE PASS's cost at a given set
// size, not the activation path that would produce it.
func benchPrefix(b *testing.B, n int) {
	// The REAL definition, through the real parse: the pass consults the
	// pinned loop closure, and a stub `{}` would benchmark the def == nil
	// early-out instead of the scan.
	src, err := os.ReadFile(fixturePath)
	if err != nil {
		b.Fatalf("reading fixture: %v", err)
	}
	def, err := workflow.Parse(src)
	if err != nil {
		b.Fatalf("parsing fixture: %v", err)
	}
	if err := workflow.Validate(def); err != nil {
		b.Fatalf("validating fixture: %v", err)
	}
	parsed, err := workflow.Canonical(def)
	if err != nil {
		b.Fatalf("serializing fixture: %v", err)
	}

	conn, runID, issueID, wfID := benchSeedDB(b, "benchprefix", string(parsed))
	defer conn.Close()

	for i := range n {
		if _, err := conn.Exec(
			`INSERT INTO steps (run_id, issue_id, workflow_id, step_name, ordinal,
			                    instance, kind, status, attempt, expected_cost,
			                    created_at_ms, updated_at_ms, row_version)
			 VALUES (?, ?, ?, 'review', 0, ?, 'executor', 'pending', 0, 0.6, 1, 1, 1)`,
			runID, issueID, wfID, fmt.Sprintf("review@0#%d", i)); err != nil {
			b.Fatalf("seeding step %d: %v", i, err)
		}
	}

	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		b.Fatalf("StepDefinitions: %v", err)
	}
	tx, err := conn.Begin()
	if err != nil {
		b.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()
	sched, err := LoadScheduler(tx, runID, defs, 2_000_000_000_000)
	if err != nil {
		b.Fatalf("LoadScheduler: %v", err)
	}
	if got := len(sched.ClaimablePrefix(sched.steps)); got != n {
		b.Fatalf("premise: ClaimablePrefix admitted %d of %d rows; the "+
			"benchmark would measure eviction churn, not the scan", got, n)
	}

	b.ResetTimer()
	for b.Loop() {
		sched.ClaimablePrefix(sched.steps)
	}
}

// BenchmarkClaimablePrefix10/100/1000 mirror the floor-query rows for the
// offer-composition pass, which runs once per `next` and `dispatch open`.
// Its eviction scan is quadratic in the offer by construction; these numbers
// are where that shape shows up if loopClosureCache's premise — that the
// per-candidate cost is the scan, not a definition-closure recomputation —
// ever stops holding (DKT-62).
func BenchmarkClaimablePrefix10(b *testing.B)   { benchPrefix(b, 10) }
func BenchmarkClaimablePrefix100(b *testing.B)  { benchPrefix(b, 100) }
func BenchmarkClaimablePrefix1000(b *testing.B) { benchPrefix(b, 1000) }

// BenchmarkRunReport50Steps is §12.2's second row: the rollup an operator
// POLLS. Its cost matters differently from the floor's — a report is called by
// a person or a dashboard rather than once per claim — but a verb somebody
// refreshes should not be the expensive thing in a run.
func BenchmarkRunReport50Steps(b *testing.B) {
	conn, runID := benchFloorDB(b, 50)
	defer conn.Close()

	b.ResetTimer()
	for b.Loop() {
		if _, err := LoadRunReport(conn, runID, 2_000_000_000_000); err != nil {
			b.Fatalf("LoadRunReport: %v", err)
		}
	}
}
