package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The write-transaction hold-time probe behind docs/design/hold-time-report.md.
//
// Each simulated client is its own *sql.DB on one SQLite file, opened the way
// db.Open opens a store (WAL, `_txlock=immediate`, one connection), so the
// clients contend for the writer lock the way separate docket processes do.
// A driver wrapper times every read-write transaction from the moment BEGIN
// IMMEDIATE returns (the lock is held) until COMMIT or ROLLBACK returns, and
// counts the statements prepared inside it; the engine prepares no statement
// explicitly, so one Prepare is one statement round trip.
//
// Setting DOCKET_HOLDTIME_REPORT to a file path runs the full sweep and writes
// the report there. Without it the test runs a two-point smoke sweep, and the
// committed report is re-checked against its own figures.

const holdTimeReportEnv = "DOCKET_HOLDTIME_REPORT"

const holdTimeReportPath = "../../docs/design/hold-time-report.md"

var (
	holdTimeClientCounts = []int{1, 2, 4, 8, 16}
	holdTimeRoundTripsMS = []float64{0.2, 1}
	holdTimeVerbs        = []string{"claim", "heartbeat", "record", "complete"}
)

const (
	// holdTimeRunIssues is the run size at every sweep point, split evenly
	// across the clients, so each point measures the same run shape.
	holdTimeRunIssues = 96
	pgUtilThreshold   = 0.5
	sqliteSaturated   = 0.9
)

type holdSample struct {
	kind       string
	hold       time.Duration
	statements int
	committed  bool
}

type holdRecorder struct {
	mu               sync.Mutex
	samples          []holdSample
	autocommitWrites int
}

func (r *holdRecorder) add(s holdSample) {
	r.mu.Lock()
	r.samples = append(r.samples, s)
	r.mu.Unlock()
}

func (r *holdRecorder) addAutocommitWrite() {
	r.mu.Lock()
	r.autocommitWrites++
	r.mu.Unlock()
}

// holdClient labels its connection's transactions with the verb in flight and
// the transaction's ordinal within that verb call ("claim tx2").
type holdClient struct {
	rec     *holdRecorder
	mu      sync.Mutex
	verb    string
	ordinal int
}

func (c *holdClient) startVerb(verb string) {
	c.mu.Lock()
	c.verb, c.ordinal = verb, 0
	c.mu.Unlock()
}

func (c *holdClient) nextKind() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ordinal++
	return fmt.Sprintf("%s tx%d", c.verb, c.ordinal)
}

type holdConnector struct {
	dsn    string
	client *holdClient
}

func (h *holdConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(h.dsn)
	if err != nil {
		return nil, err
	}
	return &holdConn{Conn: conn, client: h.client}, nil
}

func (h *holdConnector) Driver() driver.Driver { return &sqlite.Driver{} }

// holdConn embeds only driver.Conn, so database/sql routes every statement
// through Prepare, where it is counted. database/sql serializes use of one
// driver connection, so tx needs no lock.
type holdConn struct {
	driver.Conn
	client *holdClient
	tx     *holdTx
}

func (c *holdConn) Prepare(query string) (driver.Stmt, error) {
	switch {
	case c.tx != nil:
		c.tx.statements++
	case isWriteStatement(query):
		c.client.rec.addAutocommitWrite()
	}
	return c.Conn.Prepare(query)
}

func (c *holdConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	ht := &holdTx{Tx: tx, conn: c, readOnly: opts.ReadOnly, start: time.Now()}
	if !opts.ReadOnly {
		ht.kind = c.client.nextKind()
	}
	c.tx = ht
	return ht, nil
}

func (c *holdConn) Close() error { return c.Conn.Close() }

type holdTx struct {
	driver.Tx
	conn       *holdConn
	kind       string
	readOnly   bool
	start      time.Time
	statements int
}

func (t *holdTx) Commit() error   { return t.end(t.Tx.Commit(), true) }
func (t *holdTx) Rollback() error { return t.end(t.Tx.Rollback(), false) }

func (t *holdTx) end(err error, commit bool) error {
	hold := time.Since(t.start)
	t.conn.tx = nil
	if !t.readOnly {
		t.conn.client.rec.add(holdSample{
			kind: t.kind, hold: hold, statements: t.statements,
			committed: commit && err == nil,
		})
	}
	return err
}

func isWriteStatement(query string) bool {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "INSERT", "UPDATE", "DELETE", "REPLACE":
		return true
	}
	return false
}

// holdPoint is one sweep point's measurement.
type holdPoint struct {
	clients          int
	wall             time.Duration
	samples          []holdSample
	autocommitWrites int
}

// measureHoldTimes builds a fresh store holding one activated run of
// holdTimeRunIssues issues, then runs `clients` concurrent clients, each
// looping claim, heartbeat, record and complete over its own share of the
// run's implement steps.
func measureHoldTimes(t *testing.T, clients, runIssues int) holdPoint {
	t.Helper()
	path := filepath.Join(t.TempDir(), "issues.db")
	shares := seedHoldTimeRun(t, path, clients, runIssues)

	rec := &holdRecorder{}
	conns := make([]*sql.DB, clients)
	holders := make([]*holdClient, clients)
	for i := range conns {
		holders[i] = &holdClient{rec: rec}
		conn := sql.OpenDB(&holdConnector{dsn: path + "?_txlock=immediate", client: holders[i]})
		conn.SetMaxOpenConns(1)
		t.Cleanup(func() { conn.Close() })
		for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
			_, err := conn.Exec(pragma)
			testsupport.Must(t, err, "setting %s: %v", pragma, err)
		}
		conns[i] = conn
	}

	start := make(chan struct{})
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- loopHoldTimeClient(conns[i], holders[i], fmt.Sprintf("client-%d", i), shares[i])
		}()
	}
	began := time.Now()
	close(start)
	wg.Wait()
	wall := time.Since(began)
	close(errs)
	for err := range errs {
		testsupport.Must(t, err, "client loop at %d clients: %v", clients, err)
	}

	return holdPoint{
		clients: clients, wall: wall,
		samples: rec.samples, autocommitWrites: rec.autocommitWrites,
	}
}

// seedHoldTimeRun creates the store and the run through the ordinary helpers,
// then returns each client's implement step ids.
func seedHoldTimeRun(t *testing.T, path string, clients, runIssues int) [][]int {
	t.Helper()
	conn, err := db.Open(path)
	testsupport.Must(t, err, "opening the probe store: %v", err)
	defer conn.Close()
	err = db.Initialize(conn)
	testsupport.Must(t, err, "Initialize: %v", err)
	err = db.Migrate(conn)
	testsupport.Must(t, err, "Migrate: %v", err)

	registerFixture(t, conn)
	issues := make([]int, runIssues)
	for i := range issues {
		issues[i] = createIssue(t, conn, fmt.Sprintf("probe issue %d", i), "a body", "task", nil)
	}
	run := startRun(t, conn, issues...)
	_, err = activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)

	shares := make([][]int, clients)
	for i, issue := range issues {
		var stepID int
		err := conn.QueryRow(
			`SELECT id FROM steps WHERE run_id = ? AND issue_id = ? AND step_name = 'implement'`,
			run.ID, issue).Scan(&stepID)
		testsupport.Must(t, err, "finding the implement step of issue %d: %v", issue, err)
		shares[i%clients] = append(shares[i%clients], stepID)
	}
	return shares
}

// loopHoldTimeClient drives each of its steps through the four verbs. record
// and complete are CompleteStep's two halves called separately, as
// CompleteStep runs them when no detached saga launcher is installed.
func loopHoldTimeClient(conn *sql.DB, client *holdClient, owner string, steps []int) error {
	e := testEngine()
	for _, stepID := range steps {
		client.startVerb("claim")
		claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: owner, NowMS: nowMS})
		if err != nil {
			return fmt.Errorf("claim step %d: %w", stepID, err)
		}

		client.startVerb("heartbeat")
		if _, err := HeartbeatStep(conn, stepID, claim.Token, nowMS); err != nil {
			return fmt.Errorf("heartbeat step %d: %w", stepID, err)
		}

		client.startVerb("record")
		step, err := db.GetStep(conn, stepID)
		if err != nil {
			return fmt.Errorf("reading step %d: %w", stepID, err)
		}
		if err := e.stageZero(conn, step, CompleteOptions{
			Token: claim.Token, Artifact: []byte("change summary"), NowMS: nowMS,
		}); err != nil {
			return fmt.Errorf("record step %d: %w", stepID, err)
		}

		client.startVerb("complete")
		if err := e.ResumeSaga(conn, stepID, nowMS); err != nil {
			return fmt.Errorf("complete step %d: %w", stepID, err)
		}
		done, err := db.GetStep(conn, stepID)
		if err != nil {
			return fmt.Errorf("reading step %d: %w", stepID, err)
		}
		if done.Status != db.StepDone {
			return fmt.Errorf("step %d ended %q, want %q", stepID, done.Status, db.StepDone)
		}
	}
	return nil
}

// holdStats summarizes a set of transactions.
type holdStats struct {
	count, commits, rollbacks int
	p50, p99                  time.Duration
	meanStatements            float64
	totalHold                 time.Duration
}

func summarizeHolds(samples []holdSample) holdStats {
	var s holdStats
	holds := make([]time.Duration, 0, len(samples))
	statements := 0
	for _, sample := range samples {
		s.count++
		if sample.committed {
			s.commits++
		} else {
			s.rollbacks++
		}
		holds = append(holds, sample.hold)
		statements += sample.statements
		s.totalHold += sample.hold
	}
	if s.count == 0 {
		return s
	}
	slices.Sort(holds)
	s.p50 = nearestRank(holds, 0.50)
	s.p99 = nearestRank(holds, 0.99)
	s.meanStatements = float64(statements) / float64(s.count)
	return s
}

func nearestRank(sorted []time.Duration, q float64) time.Duration {
	rank := int(math.Ceil(q * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

func durationMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// holdUtilRow is one row of the report's utilization table, holding the
// figures exactly as printed so a reader can recompute every cell from them.
type holdUtilRow struct {
	clients       int
	commitsPerSec float64
	p99MS         float64
	statements    float64
	pgUtil        []float64 // one per holdTimeRoundTripsMS entry
	sqliteUtil    float64
}

func roundTo(v float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(v*scale) / scale
}

// estimatedPGUtil is commits/s times the estimated PostgreSQL hold of one
// transaction: the SQLite p99 hold plus one round trip per statement.
func estimatedPGUtil(commitsPerSec, p99MS, statements, roundTripMS float64) float64 {
	return commitsPerSec * (p99MS + statements*roundTripMS) / 1000
}

func utilRow(p holdPoint) holdUtilRow {
	all := summarizeHolds(p.samples)
	row := holdUtilRow{
		clients:       p.clients,
		commitsPerSec: roundTo(float64(all.commits)/p.wall.Seconds(), 1),
		p99MS:         roundTo(durationMS(all.p99), 3),
		statements:    roundTo(all.meanStatements, 2),
		sqliteUtil:    roundTo(all.totalHold.Seconds()/p.wall.Seconds(), 3),
	}
	for _, rtt := range holdTimeRoundTripsMS {
		row.pgUtil = append(row.pgUtil,
			roundTo(estimatedPGUtil(row.commitsPerSec, row.p99MS, row.statements, rtt), 3))
	}
	return row
}

func formatRTT(rtt float64) string { return strconv.FormatFloat(rtt, 'f', -1, 64) }

func clientCount(n int) string {
	if n == 1 {
		return "1 client"
	}
	return strconv.Itoa(n) + " clients"
}

func utilTableHeader() string {
	cols := []string{"Clients", "Commits/s", "p99 hold (ms)", "Statements/tx"}
	for _, rtt := range holdTimeRoundTripsMS {
		cols = append(cols, "Est. PG util @ "+formatRTT(rtt)+" ms RTT")
	}
	cols = append(cols, "SQLite util", "SQLite saturated")
	return "| " + strings.Join(cols, " | ") + " |"
}

func renderHoldTimeReport(points []holdPoint, date string, wallSweep time.Duration) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# Write-transaction hold times on the SQLite build\n\n")
	w("Status: Measured — %s\n\n", date)
	w("This report sizes the PostgreSQL write-serialization ruling, which takes one\n")
	w("global advisory lock per read-write transaction. It measures how long each\n")
	w("read-write transaction holds the SQLite writer lock, and how many statements\n")
	w("it runs, then estimates the PostgreSQL hold as the SQLite hold plus one network\n")
	w("round trip per statement.\n\n")

	w("## Method\n\n")
	w("`internal/engine/holdtime_probe_test.go` produced every figure here. Regenerate\n")
	w("the report with:\n\n")
	w("```sh\n%s=$PWD/docs/design/hold-time-report.md go test ./internal/engine -run TestHoldTimeProbe -count=1\n```\n\n", holdTimeReportEnv)
	w("- One SQLite store per client count, holding one activated run of %d issues\n", holdTimeRunIssues)
	w("  on the engine's test workflow fixture. The issues are split evenly across the\n")
	w("  clients, so every client count does the same total work.\n")
	w("- Each client is its own `*sql.DB` on the store file, opened as `db.Open` opens a\n")
	w("  store: WAL, `_txlock=immediate`, one connection, `busy_timeout=5000`.\n")
	w("- Each client loops over its own `implement` steps: `claim` (`ClaimStep`),\n")
	w("  `heartbeat` (`HeartbeatStep`), `record` (`CompleteStep`'s stage 0 and 1) and\n")
	w("  `complete` (the saga resume that runs the step's five pass-through gates and\n")
	w("  routes it to `done`).\n")
	w("- A driver wrapper times every read-write transaction from the moment `BEGIN\n")
	w("  IMMEDIATE` returns until `COMMIT` returns, and counts the statements prepared\n")
	w("  inside it. Read-only transactions take no writer lock and are not counted.\n")
	w("  A transaction kind is the verb plus the transaction's ordinal within one call\n")
	w("  of that verb.\n")
	w("- Percentiles are nearest-rank.\n")
	w("- Estimated PostgreSQL utilization = commits/s × (p99 hold + statements/tx ×\n")
	w("  round trip) / 1000, with times in milliseconds. It is computed from the\n")
	w("  rounded figures printed in the same row, so any cell can be recomputed.\n")
	w("- SQLite utilization = total hold time of all read-write transactions / wall\n")
	w("  time. Above %.1f, SQLite's own writer lock capped demand, so commits/s at\n", sqliteSaturated)
	w("  that client count understates the offered write rate.\n\n")
	w("Host: %s/%s, %d CPUs, %s. Sweep wall time: %s.\n\n",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), wallSweep.Round(time.Second))

	rows := make([]holdUtilRow, len(points))
	autocommit := 0
	for i, p := range points {
		rows[i] = utilRow(p)
		autocommit += p.autocommitWrites
	}

	w("## Findings\n\n")
	for i, rtt := range holdTimeRoundTripsMS {
		crossing := "no measured client count"
		for _, row := range rows {
			if row.pgUtil[i] > pgUtilThreshold {
				crossing = fmt.Sprintf("%s (estimated utilization %.3f)", clientCount(row.clients), row.pgUtil[i])
				break
			}
		}
		w("- At a %s ms round trip, the smallest client count whose estimated\n", formatRTT(rtt))
		w("  PostgreSQL utilization exceeds %.1f: %s.\n", pgUtilThreshold, crossing)
	}
	// The probe's clients loop with no work between verbs, so their commit
	// rate is a capacity, not a production demand. The rate at which the
	// uncontended hold reaches the threshold is the figure to set against an
	// expected peak write rate.
	base := rows[0]
	commitsPerLoop := float64(summarizeHolds(points[0].samples).commits) / float64(holdTimeRunIssues)
	w("- These clients loop with no work between verbs, so commits/s is the rate the\n")
	w("  store sustained, not a production demand. From the %d-client row (p99 hold\n", base.clients)
	w("  %.3f ms, %.2f statements/tx) and %.0f commits per claim-to-done loop,\n", base.p99MS, base.statements, commitsPerLoop)
	w("  estimated PostgreSQL utilization reaches %.1f at:\n", pgUtilThreshold)
	for _, rtt := range holdTimeRoundTripsMS {
		rate := pgUtilThreshold * 1000 / (base.p99MS + base.statements*rtt)
		w("  - %s ms round trip: %.1f commits/s, %.2f step loops/s.\n", formatRTT(rtt), rate, rate/commitsPerLoop)
	}
	var saturated []string
	for _, row := range rows {
		if row.sqliteUtil > sqliteSaturated {
			saturated = append(saturated, strconv.Itoa(row.clients))
		}
	}
	if len(saturated) == 0 {
		w("- SQLite utilization exceeded %.1f at no measured client count.\n", sqliteSaturated)
	} else {
		w("- SQLite saturated (utilization above %.1f) at %s clients; those rows\n",
			sqliteSaturated, strings.Join(saturated, ", "))
		w("  measure SQLite's capacity, not the clients' demand.\n")
	}
	w("- Write statements run outside an explicit transaction: %d.\n\n", autocommit)

	w("## Utilization\n\n")
	w("%s\n", utilTableHeader())
	w("|%s\n", strings.Repeat("---|", 6+len(holdTimeRoundTripsMS)))
	for _, row := range rows {
		cells := []string{
			strconv.Itoa(row.clients),
			strconv.FormatFloat(row.commitsPerSec, 'f', 1, 64),
			strconv.FormatFloat(row.p99MS, 'f', 3, 64),
			strconv.FormatFloat(row.statements, 'f', 2, 64),
		}
		for _, u := range row.pgUtil {
			cells = append(cells, strconv.FormatFloat(u, 'f', 3, 64))
		}
		flag := "no"
		if row.sqliteUtil > sqliteSaturated {
			flag = "YES"
		}
		cells = append(cells, strconv.FormatFloat(row.sqliteUtil, 'f', 3, 64), flag)
		w("| %s |\n", strings.Join(cells, " | "))
	}
	w("\n")

	w("## Transactions by kind\n\n")
	for _, p := range points {
		w("### %s\n\n", clientCount(p.clients))
		w("Wall time %.3f s.\n\n", p.wall.Seconds())
		w("| Kind | Count | Rolled back | p50 hold (ms) | p99 hold (ms) | Statements/tx |")
		for _, rtt := range holdTimeRoundTripsMS {
			w(" Est. PG hold @ %s ms RTT (ms) |", formatRTT(rtt))
		}
		w("\n|%s\n", strings.Repeat("---|", 6+len(holdTimeRoundTripsMS)))
		byKind := map[string][]holdSample{}
		for _, s := range p.samples {
			byKind[s.kind] = append(byKind[s.kind], s)
		}
		kinds := make([]string, 0, len(byKind))
		for k := range byKind {
			kinds = append(kinds, k)
		}
		slices.SortFunc(kinds, compareHoldKinds)
		for _, k := range append(kinds, "all") {
			samples := byKind[k]
			if k == "all" {
				samples = p.samples
			}
			s := summarizeHolds(samples)
			w("| %s | %d | %d | %.3f | %.3f | %.2f |",
				k, s.count, s.rollbacks, durationMS(s.p50), durationMS(s.p99), s.meanStatements)
			for _, rtt := range holdTimeRoundTripsMS {
				w(" %.3f |", durationMS(s.p99)+s.meanStatements*rtt)
			}
			w("\n")
		}
		w("\n")
	}

	w("## Limits\n\n")
	w("- The estimate adds round trips only for the statements counted here. `BEGIN`,\n")
	w("  the advisory-lock call and `COMMIT` add about three more per transaction on\n")
	w("  PostgreSQL, and the formula leaves them out.\n")
	w("- SQLite statement time is not PostgreSQL statement time; the SQLite hold\n")
	w("  stands in for the server-side work.\n")
	w("- Every operation uses one fixed clock value, the workflow's gates are\n")
	w("  pass-through stubs, and the diff is a stub, so no subprocess runs inside or\n")
	w("  between transactions. Real gates run outside transactions, so they lower the\n")
	w("  offered write rate per client without changing hold times.\n")
	w("- One run size, %d issues. The probe does not measure how hold times and\n", holdTimeRunIssues)
	w("  statement counts change with run size.\n")
	w("- SQLite's busy handler sleeps between lock retries, so the lock can sit idle\n")
	w("  while writers wait. Commits/s can stop scaling before SQLite utilization\n")
	w("  reaches %.1f.\n", sqliteSaturated)
	w("- The clients are goroutines in one process, not separate processes, so\n")
	w("  they share one Go runtime and garbage collector.\n")
	w("- One host, one sweep. Figures vary between runs and machines.\n")
	return b.String()
}

// compareHoldKinds orders kinds by verb in loop order, then by ordinal.
func compareHoldKinds(a, b string) int {
	verbA, ordA, _ := strings.Cut(a, " tx")
	verbB, ordB, _ := strings.Cut(b, " tx")
	if d := slices.Index(holdTimeVerbs, verbA) - slices.Index(holdTimeVerbs, verbB); d != 0 {
		return d
	}
	nA, _ := strconv.Atoi(ordA)
	nB, _ := strconv.Atoi(ordB)
	return nA - nB
}

// checkHoldTimeReport parses a report's utilization table and verifies it has
// one row per client count, a column per round trip, and that every estimated
// utilization cell recomputes from the figures in its row.
func checkHoldTimeReport(report string) error {
	header := utilTableHeader()
	lines := strings.Split(report, "\n")
	at := slices.Index(lines, header)
	if at < 0 {
		return fmt.Errorf("no utilization table with header %q", header)
	}
	if want := "|" + strings.Repeat("---|", 6+len(holdTimeRoundTripsMS)); at+1 >= len(lines) || lines[at+1] != want {
		return fmt.Errorf("the utilization table's separator row is not %q", want)
	}
	seen := map[int]bool{}
	for _, line := range lines[at+2:] {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) != 6+len(holdTimeRoundTripsMS) {
			return fmt.Errorf("row %q has %d cells, want %d", line, len(cells), 6+len(holdTimeRoundTripsMS))
		}
		nums := make([]float64, 4+len(holdTimeRoundTripsMS)+1)
		for i := range nums {
			v, err := strconv.ParseFloat(cells[i], 64)
			if err != nil {
				return fmt.Errorf("row %q cell %d: %v", line, i, err)
			}
			nums[i] = v
		}
		clients := int(nums[0])
		seen[clients] = true
		for i, rtt := range holdTimeRoundTripsMS {
			want := estimatedPGUtil(nums[1], nums[2], nums[3], rtt)
			if got := nums[4+i]; math.Abs(got-want) > 0.0005 {
				return fmt.Errorf("%d clients @ %s ms: listed utilization %.3f, recomputed %.4f",
					clients, formatRTT(rtt), got, want)
			}
		}
		sqliteUtil := nums[4+len(holdTimeRoundTripsMS)]
		wantFlag := "no"
		if sqliteUtil > sqliteSaturated {
			wantFlag = "YES"
		}
		if flag := cells[len(cells)-1]; flag != wantFlag {
			return fmt.Errorf("%d clients: SQLite util %.3f flagged %q, want %q",
				clients, sqliteUtil, flag, wantFlag)
		}
	}
	for _, n := range holdTimeClientCounts {
		if !seen[n] {
			return fmt.Errorf("the utilization table has no row for %d clients", n)
		}
	}
	return nil
}

// TestHoldTimeProbe measures, or smoke-tests, the sweep. The smoke run pins
// that every verb's transactions are observed and that the rendered report
// passes its own check.
func TestHoldTimeProbe(t *testing.T) {
	out := os.Getenv(holdTimeReportEnv)
	clientCounts, runIssues := []int{1, 2}, 4
	if out != "" {
		clientCounts, runIssues = holdTimeClientCounts, holdTimeRunIssues
	}

	began := time.Now()
	points := make([]holdPoint, 0, len(clientCounts))
	for _, n := range clientCounts {
		p := measureHoldTimes(t, n, runIssues)
		for _, verb := range holdTimeVerbs {
			if !slices.ContainsFunc(p.samples, func(s holdSample) bool {
				return strings.HasPrefix(s.kind, verb+" ") && s.committed && s.statements > 0
			}) {
				t.Errorf("%d clients: no committed read-write transaction observed for %s", n, verb)
			}
		}
		points = append(points, p)
	}
	report := renderHoldTimeReport(points, time.Now().UTC().Format("2006-01-02"), time.Since(began))

	if out == "" {
		if err := checkHoldTimeReport(report); err == nil || !strings.Contains(err.Error(), "no row for 4 clients") {
			t.Errorf("the smoke report (1 and 2 clients) passed the check or failed it for "+
				"the wrong reason: %v", err)
		}
		return
	}
	err := checkHoldTimeReport(report)
	testsupport.Must(t, err, "the generated report fails its own check: %v", err)
	err = os.WriteFile(out, []byte(report), 0o644)
	testsupport.Must(t, err, "writing %s: %v", out, err)
	t.Logf("wrote %s", out)
}

// TestHoldTimeReportRecomputes checks the committed report: a row per client
// count, a column per round trip, and every utilization cell recomputable.
func TestHoldTimeReportRecomputes(t *testing.T) {
	body, err := os.ReadFile(holdTimeReportPath)
	testsupport.Must(t, err, "reading the committed report: %v", err)
	if err := checkHoldTimeReport(string(body)); err != nil {
		t.Errorf("%s: %v", holdTimeReportPath, err)
	}
}
