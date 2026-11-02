package pgstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestPgStoreReadsDoNotBlockOnWriteLock holds the global write lock from a
// second pool and checks that the store's pure reads, and a read snapshot,
// still return, while store writes, in a transaction or as a lone statement,
// queue on the lock until it is released.
func TestPgStoreReadsDoNotBlockOnWriteLock(t *testing.T) {
	st := openTestStore(t)
	issueID, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title:    "read under lock",
		Status:   model.StatusTodo,
		Priority: model.PriorityMedium,
		Kind:     model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	if _, err := st.CreateCommentIdempotent(&model.Comment{IssueID: issueID, Body: "before the lock"}, ""); err != nil {
		t.Fatalf("CreateCommentIdempotent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	holder := lockTestDB(t)
	entered := make(chan struct{})
	releaseHolder := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- WithWriteTx(ctx, holder, func(*sql.Tx) error {
			close(entered)
			<-releaseHolder
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-holderDone:
		t.Fatalf("lock holder returned before entering its transaction: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			close(releaseHolder)
		}
	}
	defer release()

	reads := []struct {
		name string
		read func() error
	}{
		{"GetIssue", func() error {
			_, err := st.GetIssue(issueID)
			return err
		}},
		{"ListIssues", func() error {
			_, _, err := st.ListIssues(db.ListOptions{})
			return err
		}},
		{"ListComments", func() error {
			_, err := st.ListComments(issueID)
			return err
		}},
		{"SchemaVersion", func() error {
			_, err := st.SchemaVersion()
			return err
		}},
		{"BeginReadSnapshot", func() error {
			rtx, err := st.BeginReadSnapshot()
			if err != nil {
				return err
			}
			defer rtx.Rollback()
			_, err = rtx.GetIssueFilesTx(issueID)
			return err
		}},
	}
	for _, r := range reads {
		t.Run(r.name+" returns while a writer holds the lock", func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- r.read() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: %v", r.name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s blocked for 5s while another session held the write lock", r.name)
			}
		})
	}

	// One write opens a transaction; the other is a single Exec outside any
	// transaction, which the connection wraps in a locked one.
	writes := []struct {
		name  string
		write func() error
	}{
		{"CreateCommentIdempotent", func() error {
			_, err := st.CreateCommentIdempotent(&model.Comment{IssueID: issueID, Body: "under the lock"}, "")
			return err
		}},
		{"SetIssueScopeGlobs", func() error {
			return st.SetIssueScopeGlobs(issueID, `["internal/**"]`)
		}},
	}
	t.Run("writes queue on the lock until the holder releases it", func(t *testing.T) {
		done := make([]chan error, len(writes))
		for i, w := range writes {
			done[i] = make(chan error, 1)
			go func() { done[i] <- w.write() }()
		}
		deadline := time.Now().Add(10 * time.Second)
		for writeLockWaiters(t, holder) < len(writes) {
			for i, w := range writes {
				select {
				case err := <-done[i]:
					t.Fatalf("%s returned while another session held the write lock: %v", w.name, err)
				default:
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("the writes neither returned nor all queued on the write lock within 10s")
			}
			time.Sleep(20 * time.Millisecond)
		}
		release()
		for i, w := range writes {
			select {
			case err := <-done[i]:
				if err != nil {
					t.Fatalf("%s after release: %v", w.name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not finish within 10s of the lock's release", w.name)
			}
		}
	})

	release()
	if err := <-holderDone; err != nil {
		t.Fatalf("lock holder: %v", err)
	}
}

// openTestStore opens a Store on DOCKET_TEST_POSTGRES_DSN in a fresh schema,
// dropped when the test ends, and migrates it to the current version.
func openTestStore(t *testing.T) store.Store {
	t.Helper()
	admin := lockTestDB(t)
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	name := "docket_store_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(`CREATE SCHEMA ` + name); err != nil {
		t.Fatalf("creating schema %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + name + ` CASCADE`); err != nil {
			t.Errorf("dropping schema %s: %v", name, err)
		}
	})

	st, err := OpenStore(context.Background(), testsupport.PostgresDSN(t), testsupport.PostgresPasswordFile(), name)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	// Registered after the schema drop, so it runs first.
	t.Cleanup(func() { st.Close() })
	if err := st.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

// sagaFixture is a run with one issue on a fresh PostgreSQL schema, plus a
// second run, for the saga queries whose PostgreSQL SQL is the store's own.
type sagaFixture struct {
	t               *testing.T
	st              store.Store
	tx              store.Tx
	workflow, issue int
	run, otherRun   int
}

func newSagaFixture(t *testing.T) *sagaFixture {
	t.Helper()
	st := openTestStore(t)
	project, err := st.EnsureProject("/repo/saga", "saga", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	wf, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: project, Name: "wf", Version: 1, SourceSHA256: "sha", Body: "body", Parsed: "{}",
	}, 1)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)
	issue, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: project, Title: "saga subject", Status: model.StatusTodo,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	f := &sagaFixture{t: t, st: st, workflow: wf.ID, issue: issue}
	for _, id := range []*int{&f.run, &f.otherRun} {
		run, _, err := st.InsertRunWithContextIdempotent(project, "saga", 0, 1, db.RunContext{}, "")
		testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
		testsupport.Must(t, st.AddRunIssue(run.ID, issue), "AddRunIssue")
		*id = run.ID
	}
	f.tx, err = st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	t.Cleanup(func() { f.tx.Rollback() })
	return f
}

// add inserts one step of the fixture's issue and returns its id. A zero
// RunID takes the fixture's run; Kind defaults to executor.
func (f *sagaFixture) add(row db.StepRow) int {
	f.t.Helper()
	if row.RunID == 0 {
		row.RunID = f.run
	}
	if row.Kind == "" {
		row.Kind = "executor"
	}
	row.IssueID, row.WorkflowID = f.issue, f.workflow
	row.Instance = fmt.Sprintf("%s@%d", row.StepName, row.Ordinal)
	testsupport.Must(f.t, f.tx.InsertStepTx(row, 1), "InsertStepTx %s", row.Instance)
	steps, err := f.tx.ListIssueStepsTx(row.RunID, f.issue)
	testsupport.Must(f.t, err, "ListIssueStepsTx: %v", err)
	for _, s := range steps {
		if s.Instance == row.Instance {
			return s.ID
		}
	}
	f.t.Fatalf("step %s was not listed after its insert", row.Instance)
	return 0
}

func (f *sagaFixture) event(runID, stepID int, kind, data string) {
	f.t.Helper()
	testsupport.Must(f.t, f.tx.InsertEventTx(db.NewEvent{AtMS: 1, Kind: kind, RunID: runID, StepID: stepID, Data: data}),
		"InsertEventTx %s", kind)
}

// TestPgStoreStepEventDataContains matches a step's events of one kind on
// the text of their jsonb data, case-folded as SQLite's LIKE is. It skips
// without DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreStepEventDataContains(t *testing.T) {
	f := newSagaFixture(t)
	step := f.add(db.StepRow{StepName: "implement", Status: db.StepRunning})
	other := f.add(db.StepRow{StepName: "review", Status: db.StepRunning})
	f.event(f.run, step, "gate-started", `{"gate":"tests"}`)
	f.event(f.run, step, "gate-finished", `{"gate":"build"}`)
	f.event(f.run, other, "gate-started", `{"gate":"lint"}`)
	testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")

	ask := func(name, kind, text string, want bool) {
		t.Helper()
		got, err := f.st.StepEventDataContains(f.run, step, kind, text)
		if err != nil || got != want {
			t.Errorf("%s = (%v, %v), want (%v, nil)", name, got, err, want)
		}
	}
	ask("a started gate", "gate-started", "tests", true)
	ask("a started gate in other case", "gate-started", "TESTS", true)
	ask("a gate only another kind names", "gate-started", "build", false)
	ask("a gate only another step started", "gate-started", "lint", false)
	ask(`a backslash, which is literal`, "gate-started", `te\sts`, false)
}

// TestPgStoreRunCostFloorTx sums a run's claim costs, each the claim's own
// numeric expected_cost or else its step's declared one, plus the declared
// cost of every open vote step. It skips without DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreRunCostFloorTx(t *testing.T) {
	f := newSagaFixture(t)
	floor, err := f.tx.RunCostFloorTx(f.run, "step-claimed", "vote")
	if err != nil || floor != 0 {
		t.Errorf("a run with no claims or votes = (%v, %v), want (0, nil)", floor, err)
	}

	cheap := f.add(db.StepRow{StepName: "implement", Status: db.StepDone, ExpectedCost: 1})
	dear := f.add(db.StepRow{StepName: "review", Status: db.StepDone, ExpectedCost: 4})
	f.event(f.run, cheap, "step-claimed", `{"expected_cost":2.5}`)
	f.event(f.run, cheap, "step-claimed", `{}`)
	f.event(f.run, dear, "step-claimed", `{"expected_cost":null}`)
	f.event(f.run, dear, "step-recorded", `{"expected_cost":100}`)
	f.add(db.StepRow{StepName: "ballot", Kind: "vote", Status: db.StepPending, ExpectedCost: 3})
	f.add(db.StepRow{StepName: "ballot", Ordinal: 1, Kind: "vote", Status: db.StepSkipped, ExpectedCost: 10})
	f.add(db.StepRow{StepName: "ballot", Ordinal: 2, Kind: "vote", Status: db.StepSuperseded, ExpectedCost: 10})
	elsewhere := f.add(db.StepRow{RunID: f.otherRun, StepName: "ballot", Kind: "vote", Status: db.StepPending, ExpectedCost: 20})
	f.event(f.otherRun, elsewhere, "step-claimed", `{"expected_cost":50}`)

	floor, err = f.tx.RunCostFloorTx(f.run, "step-claimed", "vote")
	if err != nil || floor != 10.5 {
		t.Errorf("the floor = (%v, %v), want (10.5, nil): 2.5 carried + 1 and 4 declared + 3 open vote",
			floor, err)
	}
}

func TestRebindPlaceholders(t *testing.T) {
	cases := []struct {
		in    string
		nargs int
		want  string
	}{
		{`SELECT 1`, 0, `SELECT 1`},
		{`SELECT ?`, 0, `SELECT ?`},
		{`SELECT * FROM t WHERE a = ? AND b = ?`, 2, `SELECT * FROM t WHERE a = $1 AND b = $2`},
		{`SELECT '?' , "q?" FROM t WHERE a = ?`, 1, `SELECT '?' , "q?" FROM t WHERE a = $1`},
		{`SELECT 'it''s ?' WHERE a = ?`, 1, `SELECT 'it''s ?' WHERE a = $1`},
		{"SELECT a -- why?\nFROM t WHERE a = ?", 1, "SELECT a -- why?\nFROM t WHERE a = $1"},
		{`SELECT /* ? */ a FROM t WHERE a = ?`, 1, `SELECT /* ? */ a FROM t WHERE a = $1`},
	}
	for _, c := range cases {
		if got := rebindPlaceholders(c.in, c.nargs); got != c.want {
			t.Errorf("rebindPlaceholders(%q, %d) = %q, want %q", c.in, c.nargs, got, c.want)
		}
	}
}

// TestStoreConnRefusesOtherWriteIsolation checks that a read-write
// transaction runs at READ COMMITTED when the caller names no level or that
// one, and fails when the caller asks for another.
func TestStoreConnRefusesOtherWriteIsolation(t *testing.T) {
	pool := sql.OpenDB(newStoreConnector(testConnConfig(t)))
	t.Cleanup(func() { pool.Close() })
	ctx := context.Background()

	for _, level := range []sql.IsolationLevel{sql.LevelDefault, sql.LevelReadCommitted} {
		tx, err := pool.BeginTx(ctx, &sql.TxOptions{Isolation: level})
		if err != nil {
			t.Fatalf("BeginTx(%s): %v", level, err)
		}
		var got string
		if err := tx.QueryRow(`SHOW transaction_isolation`).Scan(&got); err != nil {
			t.Fatalf("reading transaction_isolation: %v", err)
		}
		tx.Rollback()
		if got != "read committed" {
			t.Errorf("BeginTx(%s) runs at %q, want read committed", level, got)
		}
	}
	for _, level := range []sql.IsolationLevel{sql.LevelRepeatableRead, sql.LevelSerializable} {
		tx, err := pool.BeginTx(ctx, &sql.TxOptions{Isolation: level})
		if err == nil {
			tx.Rollback()
			t.Errorf("BeginTx(%s) succeeded, want a refusal", level)
		}
	}
}

func TestJSONCallsToJSONB(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT 1`, `SELECT 1`},
		{
			`EXISTS (SELECT 1 FROM json_each(domain_tags) WHERE value = ?)`,
			`EXISTS (SELECT 1 FROM jsonb_array_elements_text(domain_tags) WHERE value = ?)`,
		},
		{
			`CASE WHEN json_valid(e.data) THEN json_extract(e.data, '$.repo') END`,
			`CASE WHEN (e.data IS JSON) THEN (e.data ->> 'repo') END`,
		},
		{`JSON_EXTRACT(COALESCE(d, '{}'), '$.k')`, `(COALESCE(d, '{}') ->> 'k')`},
		{`json_extract(d, '$.a.b')`, `json_extract(d, '$.a.b')`},
		{`SELECT 'json_valid(x)', my_json_valid(x)`, `SELECT 'json_valid(x)', my_json_valid(x)`},
	}
	for _, c := range cases {
		if got := jsonCallsToJSONB(c.in); got != c.want {
			t.Errorf("jsonCallsToJSONB(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInsertOrIgnoreToOnConflict(t *testing.T) {
	cases := []struct{ in, want string }{
		{`INSERT INTO t (a) VALUES (?)`, `INSERT INTO t (a) VALUES (?)`},
		{`INSERT OR IGNORE INTO t (a) VALUES (?)`, `INSERT INTO t (a) VALUES (?) ON CONFLICT DO NOTHING`},
		{"\n\t insert or ignore into t (a)\n VALUES (?);\n", "INSERT INTO t (a)\n VALUES (?) ON CONFLICT DO NOTHING"},
	}
	for _, c := range cases {
		if got := insertOrIgnoreToOnConflict(c.in); got != c.want {
			t.Errorf("insertOrIgnoreToOnConflict(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
