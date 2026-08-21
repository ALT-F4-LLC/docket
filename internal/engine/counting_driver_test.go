package engine

import (
	"database/sql"
	"database/sql/driver"
	"strings"
	"sync"
	"testing"

	sqlite "modernc.org/sqlite"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// A COUNTING sql.DB WRAPPER (TDD §4.12).
//
// §4.12 asks for D1's dormancy to be "asserted by a counting `sql.DB` wrapper
// rather than by inspection", and that distinction is the whole reason this
// file exists. A test that read `loadBudget` and observed an early return would
// pass forever after someone moved the query above it; a test that COUNTS the
// statements SQLite is asked to prepare fails the moment a query appears,
// wherever it was added.
//
// It also supplies the measured query count docs/spec/performance.md publishes
// for `run report`, so that number is observed rather than inferred from
// reading call sites.

// countingDriver wraps the sqlite driver and tallies prepared statements whose
// text matches a caller-supplied predicate.
type countingDriver struct {
	mu      sync.Mutex
	count   int
	matched []string
	match   func(query string) bool
}

func (d *countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, d: d}, nil
}

func (d *countingDriver) observe(query string) {
	if d.match != nil && !d.match(query) {
		return
	}
	d.mu.Lock()
	d.count++
	d.matched = append(d.matched, query)
	d.mu.Unlock()
}

// matchedQueries returns what was counted, so a failure NAMES the query rather
// than reporting a number the reader then has to go hunting for.
func (d *countingDriver) matchedQueries() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.matched...)
}

func (d *countingDriver) reset() {
	d.mu.Lock()
	d.count, d.matched = 0, nil
	d.mu.Unlock()
}

func (d *countingDriver) total() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count
}

type countingConn struct {
	driver.Conn
	d *countingDriver
}

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	c.d.observe(query)
	return c.Conn.Prepare(query)
}

func (c *countingConn) Begin() (driver.Tx, error) { return c.Conn.Begin() }

func (c *countingConn) Close() error { return c.Conn.Close() }

// registerCounting registers a uniquely-named driver for one test and returns
// it with an opened database. Registration is global and permanent, so the name
// carries the test's own name — two tests registering "counting" would panic on
// the second.
func registerCounting(t *testing.T, match func(string) bool) (*countingDriver, *sql.DB) {
	t.Helper()

	d := &countingDriver{match: match}
	name := "counting-" + strings.ReplaceAll(t.Name(), "/", "-")
	sql.Register(name, d)

	conn, err := sql.Open(name, "file:"+name+"?mode=memory&cache=shared")
	testsupport.Must(t, err, "opening the counting database: %v", err)
	t.Cleanup(func() { conn.Close() })
	conn.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON",
	} {
		_, err := conn.Exec(pragma)
		testsupport.Must(t, err, "setting %s: %v", pragma, err)
	}
	return d, conn
}

// TestBudgetDormancyRunsNoBudgetQuery is D1, COUNTED.
//
// §4.8 B29: `budgetHeadroom` returns true on its first line when the effective
// cap is 0, and `loadBudget` declines to query at all in that case. So a run
// started without `--budget` in a repo that never set `budget.default` executes
// exactly the queries v9 executed — the group-1 dormancy claim, measured.
//
// The predicate matches the two queries the budget path owns: the floor's SUM
// over claim events, and the ledger's per-unit sum. Neither may appear.
func TestBudgetDormancyRunsNoBudgetQuery(t *testing.T) {
	isBudgetQuery := func(q string) bool {
		return strings.Contains(q, "usage_ledger") ||
			(strings.Contains(q, "expected_cost") && strings.Contains(q, "events"))
	}

	counter, conn := registerCounting(t, isBudgetQuery)
	err := initCountingSchema(t, conn)
	testsupport.Must(t, err, "preparing the counting database: %v", err)

	runID, _ := budgetRun(t, conn, 0)
	counter.reset()

	// The scheduling paths, over an unbudgeted run.
	e := testEngine()
	_, err = e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	claimInstance(t, conn, "implement@0", nowMS)

	if n := counter.total(); n != 0 {
		t.Errorf("an unbudgeted run executed %d budget queries, want 0 — D1's "+
			"dormancy is that the query does not run, not that its result is "+
			"ignored (§4.8 B29). The queries were:\n%s",
			n, strings.Join(counter.matchedQueries(), "\n---\n"))
	}

	// And the counter is not broken: a BUDGETED run does run them, so the zero
	// above is the zero of a path not taken rather than of a predicate that
	// matches nothing.
	execSQL(t, conn, `UPDATE runs SET budget = 100 WHERE id = ?`, runID)
	counter.reset()
	_, err = e.NextSteps(conn, runID, 0, nowMS)
	testsupport.Must(t, err, "NextSteps on a budgeted run: %v", err)
	if n := counter.total(); n == 0 {
		t.Error("a BUDGETED run executed no budget query either; the counter's " +
			"predicate matches nothing and the assertion above is vacuous")
	}
}

// TestRunReportQueryCount is the number docs/spec/performance.md publishes for
// R8's read-only rollup — the verb an operator polls.
//
// It counts every statement rather than a subset: a report that grew a query
// per step would still pass a threshold test on a small fixture, and the point
// of publishing the number is that a later reader can see it move.
func TestRunReportQueryCount(t *testing.T) {
	counter, conn := registerCounting(t, nil)
	err := initCountingSchema(t, conn)
	testsupport.Must(t, err, "preparing the counting database: %v", err)

	runID, _ := budgetRun(t, conn, 100)
	e := testEngine()
	completeWithUsage(t, conn, e, "implement@0", `{"pages": 4}`)

	counter.reset()
	_, err = LoadRunReport(conn, runID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)
	queries := counter.total()

	// The count is LOGGED rather than asserted against a constant. A constant
	// here would be a test that fails whenever a section is added — which is a
	// change performance.md should record, not one the suite should block.
	t.Logf("run report executes %d statements", queries)

	if queries == 0 {
		t.Error("the report executed no statement at all; the counter is not wired")
	}
	// The one bound worth holding: the report must not be PER-STEP-quadratic.
	// The fixture expands ten-odd steps, so anything approaching a hundred
	// statements means a section grew a query inside a loop over steps.
	if queries > 60 {
		t.Errorf("run report executes %d statements over a ~10-step run; a "+
			"rollup that queries per step turns a polled verb into the "+
			"expensive thing in a run", queries)
	}
}

// initCountingSchema brings a counting database up to the current schema,
// through the same Initialize/Migrate pair every other test uses — a counting
// database that got its schema some other way would be counting queries against
// a shape no operator has.
func initCountingSchema(t *testing.T, conn *sql.DB) error {
	t.Helper()
	if err := db.Initialize(conn); err != nil {
		return err
	}
	return db.Migrate(conn)
}
