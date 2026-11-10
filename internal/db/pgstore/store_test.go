package pgstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
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

// TestPgStoreRunStepClasses reads a run's step classes through the Store:
// each distinct non-null class of the run's own steps once, in class order,
// and none with a nil error for a run with no classed step. It skips without
// DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreRunStepClasses(t *testing.T) {
	t.Run("distinct non-null classes of the run only", func(t *testing.T) {
		f := newSagaFixture(t)
		f.add(db.StepRow{StepName: "review", Class: "worker", Status: db.StepPending})
		f.add(db.StepRow{StepName: "implement", Class: "worker", Status: db.StepPending})
		f.add(db.StepRow{StepName: "judge", Class: "judge", Status: db.StepPending})
		f.add(db.StepRow{StepName: "gate", Status: db.StepPending})
		f.add(db.StepRow{RunID: f.otherRun, StepName: "implement", Class: "elsewhere", Status: db.StepPending})
		testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")

		want := []string{"judge", "worker"}
		if got, err := f.st.RunStepClasses(f.run); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("RunStepClasses = (%q, %v), want (%q, nil): each once, no NULL, no other run's", got, err, want)
		}
	})

	t.Run("a run with no classed steps", func(t *testing.T) {
		f := newSagaFixture(t)
		f.add(db.StepRow{StepName: "gate", Status: db.StepPending})
		f.add(db.StepRow{RunID: f.otherRun, StepName: "implement", Class: "elsewhere", Status: db.StepPending})
		testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")

		if got, err := f.st.RunStepClasses(f.run); err != nil || len(got) != 0 {
			t.Errorf("RunStepClasses of a run with only unclassed steps = (%q, %v), want (none, nil)", got, err)
		}
	})
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

// TestPgStoreReportAndLedgerQueries runs the report and ledger event reads,
// whose helper SQL the PostgreSQL store shares, against a seeded run: the
// jsonb data scans back as text, a runless event reads run 0, and an
// unrouted or absent step reads routing "". Payloads are written in jsonb's
// canonical text so they read back unchanged. It skips without
// DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreReportAndLedgerQueries(t *testing.T) {
	f := newSagaFixture(t)
	implement := f.add(db.StepRow{StepName: "implement", Executor: "claude", Status: db.StepDone})
	review := f.add(db.StepRow{StepName: "review", Status: db.StepDone})
	elsewhere := f.add(db.StepRow{RunID: f.otherRun, StepName: "implement", Executor: "codex", Status: db.StepDone})
	testsupport.Must(t, f.tx.SetStepRoutingTx(review, "advance", "approved", db.StepDone, "", 1), "SetStepRoutingTx")
	for _, e := range []db.NewEvent{
		{AtMS: 10, Kind: "step-claimed", RunID: f.run, StepID: implement, Data: `{"n": 1}`},
		{AtMS: 20, Kind: "step-recorded", RunID: f.run, StepID: review, Data: `{"n": 2}`},
		{AtMS: 30, Kind: "step-claimed", RunID: f.run, Data: `{"n": 3}`},
		{AtMS: 40, Kind: "issue-done", RunID: f.run, StepID: review, IssueID: f.issue, Data: `{"n": 4}`},
		{AtMS: 50, Kind: "step-claimed", RunID: f.otherRun, StepID: elsewhere, Data: `{"n": 5}`},
		{AtMS: 60, Kind: "issue-done", IssueID: f.issue, Data: `{"n": 6}`},
	} {
		testsupport.Must(t, f.tx.InsertEventTx(e), "InsertEventTx %s", e.Kind)
	}
	testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")

	wantStepEvents := []db.StepEvent{
		{StepID: implement, Kind: "step-claimed", Data: `{"n": 1}`},
		{StepID: review, Kind: "step-recorded", Data: `{"n": 2}`},
	}
	if got, err := f.st.RunStepEvents(f.run, "step-claimed", "step-recorded"); err != nil || !reflect.DeepEqual(got, wantStepEvents) {
		t.Errorf("RunStepEvents = (%+v, %v), want (%+v, nil)", got, err, wantStepEvents)
	}
	if first, last, found, err := f.st.RunEventSpanMS(f.run, "step-claimed", "issue-done"); err != nil || !found || first != 10 || last != 40 {
		t.Errorf("RunEventSpanMS = (%d, %d, %v, %v), want (10, 40, true, nil)", first, last, found, err)
	}
	if first, last, found, err := f.st.RunEventSpanMS(f.otherRun, "issue-done"); err != nil || found || first != 0 || last != 0 {
		t.Errorf("RunEventSpanMS of a kind the run never logged = (%d, %d, %v, %v), want (0, 0, false, nil)",
			first, last, found, err)
	}
	wantLatest := db.IssueEvent{IssueID: f.issue, AtMS: 60, Data: `{"n": 6}`}
	if got, found, err := f.st.LatestIssueEvent(f.issue, "issue-done"); err != nil || !found || got != wantLatest {
		t.Errorf("LatestIssueEvent = (%+v, %v, %v), want (%+v, true, nil)", got, found, err, wantLatest)
	}
	if got, found, err := f.st.LatestIssueEvent(f.issue, "issue-abandoned"); err != nil || found || got != (db.IssueEvent{}) {
		t.Errorf("LatestIssueEvent of a kind never logged = (%+v, %v, %v), want (zero, false, nil)", got, found, err)
	}

	snap, err := f.st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	if got, err := snap.RunStepEventsTx(f.run, "step-claimed", "step-recorded"); err != nil || !reflect.DeepEqual(got, wantStepEvents) {
		t.Errorf("RunStepEventsTx = (%+v, %v), want (%+v, nil)", got, err, wantStepEvents)
	}
	wantData := []string{`{"n": 1}`, `{"n": 3}`}
	if got, err := snap.RunEventDataTx(f.run, "step-claimed"); err != nil || !reflect.DeepEqual(got, wantData) {
		t.Errorf("RunEventDataTx = (%q, %v), want (%q, nil)", got, err, wantData)
	}
	wantIssueEvents := []db.IssueEvent{
		{IssueID: f.issue, RunID: f.run, AtMS: 40, Data: `{"n": 4}`, StepRouting: "advance: approved"},
	}
	if got, err := snap.RunIssueEventsTx(f.run, "issue-done"); err != nil || !reflect.DeepEqual(got, wantIssueEvents) {
		t.Errorf("RunIssueEventsTx = (%+v, %v), want (%+v, nil)", got, err, wantIssueEvents)
	}
}

// TestPgStoreRunLifecycleQueries runs the run lifecycle and repin queries,
// whose helper SQL the PostgreSQL store shares, against a seeded run: the
// reads keep their run, issue and kind filters, the jsonb data scans back as
// text, INSERT OR IGNORE on a held pin affects no row, and each write reports
// the rows it changed. Payloads are written in jsonb's canonical text so they
// read back unchanged. It skips without DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreRunLifecycleQueries(t *testing.T) {
	f := newSagaFixture(t)
	implement := f.add(db.StepRow{StepName: "implement", Status: db.StepDone})
	review := f.add(db.StepRow{StepName: "review", Status: db.StepRunning})
	verify := f.add(db.StepRow{StepName: "verify", Status: db.StepPending})
	elsewhere := f.add(db.StepRow{RunID: f.otherRun, StepName: "implement", Status: db.StepPending})
	for id, root := range map[int]string{implement: "/wt/b", review: "/wt/a", verify: "/wt/b", elsewhere: "/wt/elsewhere"} {
		testsupport.Must(t, f.tx.SetStepWorkRootTx(id, root), "SetStepWorkRootTx %d", id)
	}
	f.event(f.run, implement, "step-claimed", `{"n": 1}`)
	f.event(f.run, 0, "run-paused", `{"n": 2}`)
	f.event(f.run, review, "step-recorded", `{"n": 3}`)
	f.event(f.otherRun, elsewhere, "step-claimed", `{"n": 4}`)
	testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")

	snap, err := f.st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	for _, issueID := range []int{0, f.issue} {
		if got, err := snap.RunWorktreesTx(f.run, issueID); err != nil || !reflect.DeepEqual(got, []string{"/wt/a", "/wt/b"}) {
			t.Errorf("RunWorktreesTx(run, %d) = (%q, %v), want ([/wt/a /wt/b], nil)", issueID, got, err)
		}
	}
	if got, err := snap.RunWorktreesTx(f.run, f.issue+1000); err != nil || len(got) != 0 {
		t.Errorf("RunWorktreesTx of an issue with no steps = (%q, %v), want (none, nil)", got, err)
	}
	if in, err := snap.RunHasIssueTx(f.run, f.issue); err != nil || !in {
		t.Errorf("RunHasIssueTx of a bound issue = (%v, %v), want (true, nil)", in, err)
	}
	if in, err := snap.RunHasIssueTx(f.run, f.issue+1000); err != nil || in {
		t.Errorf("RunHasIssueTx of an unbound issue = (%v, %v), want (false, nil)", in, err)
	}
	wantUnfinished := []db.UnfinishedStep{
		{ID: review, Instance: "review@0", Status: db.StepRunning},
		{ID: verify, Instance: "verify@0", Status: db.StepPending},
	}
	if got, err := snap.IssueUnfinishedStepsTx(f.run, f.issue); err != nil || !reflect.DeepEqual(got, wantUnfinished) {
		t.Errorf("IssueUnfinishedStepsTx = (%+v, %v), want (%+v, nil)", got, err, wantUnfinished)
	}
	if n, err := snap.RunUnfinishedStepCountTx(f.run); err != nil || n != 2 {
		t.Errorf("RunUnfinishedStepCountTx = (%d, %v), want (2, nil)", n, err)
	}
	events, err := snap.RunEventsTx(f.run, "step-claimed", "step-recorded")
	if err != nil || len(events) != 2 {
		t.Fatalf("RunEventsTx = (%+v, %v), want two events", events, err)
	}
	if events[0].Seq >= events[1].Seq {
		t.Errorf("RunEventsTx seqs %d, %d are not in ascending order", events[0].Seq, events[1].Seq)
	}
	wantEvents := []db.RunEvent{
		{Seq: events[0].Seq, AtMS: 1, Kind: "step-claimed", Data: `{"n": 1}`},
		{Seq: events[1].Seq, AtMS: 1, Kind: "step-recorded", Data: `{"n": 3}`},
	}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("RunEventsTx = %+v, want %+v", events, wantEvents)
	}
	if got, err := snap.RunEventsTx(f.run); err != nil || len(got) != 0 {
		t.Errorf("RunEventsTx naming no kind = (%+v, %v), want (none, nil)", got, err)
	}
	wantSeqs := []db.StepEventSeq{
		{StepID: implement, Kind: "step-claimed", Seq: events[0].Seq},
		{StepID: review, Kind: "step-recorded", Seq: events[1].Seq},
	}
	if got, err := snap.RunStepEventSeqsTx(f.run); err != nil || !reflect.DeepEqual(got, wantSeqs) {
		t.Errorf("RunStepEventSeqsTx = (%+v, %v), want (%+v, nil)", got, err, wantSeqs)
	}

	tx, err := f.st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	pin := db.Pin{RunID: f.run, Kind: "contract", Ref: "contracts/implement.md", SHA256: "old",
		Content: sql.NullString{String: "body", Valid: true}}
	if n, err := tx.AddPinTx(pin); err != nil || n != 1 {
		t.Errorf("AddPinTx of a new ref = (%d, %v), want (1, nil)", n, err)
	}
	if n, err := tx.AddPinTx(db.Pin{RunID: f.run, Kind: pin.Kind, Ref: pin.Ref, SHA256: "other"}); err != nil || n != 0 {
		t.Errorf("AddPinTx of a ref the run already pins = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := tx.RepinPinTx(f.run, pin.Kind, pin.Ref, "stale", "new"); err != nil || n != 0 {
		t.Errorf("RepinPinTx from a hash the run does not hold = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := tx.RepinPinTx(f.run, pin.Kind, pin.Ref, "old", "new"); err != nil || n != 1 {
		t.Errorf("RepinPinTx = (%d, %v), want (1, nil)", n, err)
	}
	wantPins := []db.Pin{{RunID: f.run, Kind: pin.Kind, Ref: pin.Ref, SHA256: "new"}}
	if got, err := tx.ListPinsTx(f.run); err != nil || !reflect.DeepEqual(got, wantPins) {
		t.Errorf("ListPinsTx after the repin = (%+v, %v), want (%+v, nil): the old bytes are cleared", got, err, wantPins)
	}
	if n, err := tx.DropPinTx(f.run, pin.Kind, pin.Ref, "old"); err != nil || n != 0 {
		t.Errorf("DropPinTx at a hash the run no longer holds = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := tx.DropPinTx(f.run, pin.Kind, pin.Ref, "new"); err != nil || n != 1 {
		t.Errorf("DropPinTx = (%d, %v), want (1, nil)", n, err)
	}
	if n, err := tx.FailRouteIssueStepsTx(f.run, f.issue, 99); err != nil || n != 2 {
		t.Errorf("FailRouteIssueStepsTx = (%d, %v), want (2, nil)", n, err)
	}
	for id, want := range map[int]string{
		implement: db.StepDone, review: db.StepFailedRouted, verify: db.StepFailedRouted, elsewhere: db.StepPending,
	} {
		step, err := tx.GetStepTx(id)
		testsupport.Must(t, err, "GetStepTx: %v", err)
		if step.Status != want {
			t.Errorf("after FailRouteIssueStepsTx, step %d status = %q, want %q", id, step.Status, want)
		}
	}
}

// TestPgStoreClaimAndContextQueries runs the claim and context queries, whose
// helper SQL the PostgreSQL store shares, against a seeded run: the EXISTS
// pair scans as a boolean and keeps its run, step and kind filters, a NULL
// column reads as not found, the jsonb snapshot and event data scan back as
// text, and a missing row reports its not-found error. Payloads are written in
// jsonb's canonical text so they read back unchanged. It skips without
// DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreClaimAndContextQueries(t *testing.T) {
	f := newSagaFixture(t)
	hashed := f.add(db.StepRow{StepName: "implement", Status: db.StepPending})
	bare := f.add(db.StepRow{StepName: "review", Status: db.StepPending})
	other := f.add(db.StepRow{StepName: "verify", Status: db.StepPending})

	before, err := f.tx.StepRowVersionTx(hashed)
	testsupport.Must(t, err, "StepRowVersionTx before the claim: %v", err)
	_, _, err = f.tx.ClaimStepWithRecoveryTx(hashed, "owner", "recovery secret", 1000, 1)
	testsupport.Must(t, err, "ClaimStepWithRecoveryTx: %v", err)
	_, _, err = f.tx.ClaimStepWithRecoveryTx(bare, "owner", "", 1000, 1)
	testsupport.Must(t, err, "ClaimStepWithRecoveryTx with no secret: %v", err)
	if after, err := f.tx.StepRowVersionTx(hashed); err != nil || after != before+1 {
		t.Errorf("StepRowVersionTx after the claim = (%d, %v), want (%d, nil)", after, err, before+1)
	}
	if _, err := f.tx.StepRowVersionTx(hashed + 1000); !errors.Is(err, db.ErrStepNotFound) {
		t.Errorf("StepRowVersionTx of a missing step = %v, want ErrStepNotFound", err)
	}
	if hash, found, err := f.tx.StepRecoveryHashTx(hashed); err != nil || !found || hash != model.HashToken("recovery secret") {
		t.Errorf("StepRecoveryHashTx of a claim with a secret = (%q, %v, %v), want (its hash, true, nil)", hash, found, err)
	}
	if hash, found, err := f.tx.StepRecoveryHashTx(bare); err != nil || found || hash != "" {
		t.Errorf("StepRecoveryHashTx of a claim with no secret = (%q, %v, %v), want (\"\", false, nil)", hash, found, err)
	}
	if _, _, err := f.tx.StepRecoveryHashTx(hashed + 1000); !errors.Is(err, db.ErrStepNotFound) {
		t.Errorf("StepRecoveryHashTx of a missing step = %v, want ErrStepNotFound", err)
	}

	testsupport.Must(t, f.tx.SetRunIssueBodyTx(f.run, f.issue, "frozen body", "sha"), "SetRunIssueBodyTx")
	testsupport.Must(t, f.tx.SetRunIssueSnapshotTx(f.run, f.issue, `{"title": "frozen"}`), "SetRunIssueSnapshotTx")
	if body, snapshot, err := f.tx.RunIssueSnapshotsTx(f.run, f.issue); err != nil || body != "frozen body" || snapshot != `{"title": "frozen"}` {
		t.Errorf("RunIssueSnapshotsTx = (%q, %q, %v), want (frozen body, {\"title\": \"frozen\"}, nil)", body, snapshot, err)
	}
	if body, snapshot, err := f.tx.RunIssueSnapshotsTx(f.otherRun, f.issue); err != nil || body != "" || snapshot != "" {
		t.Errorf("RunIssueSnapshotsTx of an unfrozen issue = (%q, %q, %v), want (\"\", \"\", nil)", body, snapshot, err)
	}
	if _, _, err := f.tx.RunIssueSnapshotsTx(f.run, f.issue+1000); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("RunIssueSnapshotsTx of an issue outside the run = %v, want ErrNotFound", err)
	}

	seed := func(e db.NewEvent) {
		t.Helper()
		testsupport.Must(t, f.tx.InsertEventTx(e), "InsertEventTx %s", e.Kind)
	}
	pruned := db.ClaimEventsPrunedQuery{
		RunID: f.run, StepID: hashed, PrunedKind: "events-pruned", ClaimKind: "step-claimed", ClaimedAtMS: 500,
	}
	askPruned := func(name string, want bool) {
		t.Helper()
		if got, err := f.tx.ClaimEventsPrunedTx(pruned); err != nil || got != want {
			t.Errorf("ClaimEventsPrunedTx with %s = (%v, %v), want (%v, nil)", name, got, err, want)
		}
	}
	seed(db.NewEvent{AtMS: 400, Kind: "step-claimed", RunID: f.run, StepID: hashed, Data: `{}`})
	seed(db.NewEvent{AtMS: 600, Kind: "step-claimed", RunID: f.run, StepID: other, Data: `{}`})
	seed(db.NewEvent{AtMS: 600, Kind: "step-heartbeat", RunID: f.run, StepID: hashed, Data: `{}`})
	askPruned("no prune logged", false)
	seed(db.NewEvent{AtMS: 700, Kind: "events-pruned", RunID: f.otherRun, Data: `{}`})
	askPruned("only another run pruned", false)
	seed(db.NewEvent{AtMS: 800, Kind: "events-pruned", RunID: f.run, Data: `{}`})
	askPruned("a prune that took the claim record, past another kind's event", true)
	seed(db.NewEvent{AtMS: 500, Kind: "step-claimed", RunID: f.run, StepID: hashed, Data: `{}`})
	askPruned("a prune that left the claim record", false)

	for _, e := range []db.NewEvent{
		{AtMS: 100, Kind: "body-refreshed", RunID: f.run, IssueID: f.issue, Data: `{"n": "at the claim"}`},
		{AtMS: 300, Kind: "body-refreshed", RunID: f.run, IssueID: f.issue, Data: `{"n": "later"}`},
		{AtMS: 200, Kind: "body-refreshed", RunID: f.run, IssueID: f.issue, Data: `{"n": "first after"}`},
		{AtMS: 200, Kind: "body-refreshed", RunID: f.run, IssueID: f.issue, Data: `{"n": "same time, logged later"}`},
		{AtMS: 150, Kind: "scope-refreshed", RunID: f.run, IssueID: f.issue, Data: `{"n": "another kind"}`},
		{AtMS: 150, Kind: "body-refreshed", RunID: f.otherRun, IssueID: f.issue, Data: `{"n": "another run"}`},
	} {
		seed(e)
	}
	refreshes := db.IssueEventAfterQuery{RunID: f.run, IssueID: f.issue, Kind: "body-refreshed", AfterMS: 100}
	if data, found, err := f.tx.FirstIssueEventDataAfterTx(refreshes); err != nil || !found || data != `{"n": "first after"}` {
		t.Errorf("FirstIssueEventDataAfterTx = (%q, %v, %v), want ({\"n\": \"first after\"}, true, nil)", data, found, err)
	}
	testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")

	if data, found, err := f.st.FirstIssueEventDataAfter(refreshes); err != nil || !found || data != `{"n": "first after"}` {
		t.Errorf("FirstIssueEventDataAfter = (%q, %v, %v), want ({\"n\": \"first after\"}, true, nil)", data, found, err)
	}
	newest := refreshes
	newest.AfterMS = 300
	if data, found, err := f.st.FirstIssueEventDataAfter(newest); err != nil || found || data != "" {
		t.Errorf("FirstIssueEventDataAfter past the newest = (%q, %v, %v), want (\"\", false, nil)", data, found, err)
	}

	testsupport.Must(t, f.st.UpdateIssueCAS(f.issue, map[string]interface{}{"description": "live description"}, "tester", nil),
		"UpdateIssueCAS")
	snap, err := f.st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	if got, err := snap.IssueDescriptionTx(f.issue); err != nil || got != "live description" {
		t.Errorf("IssueDescriptionTx = (%q, %v), want (live description, nil)", got, err)
	}
	if _, err := snap.IssueDescriptionTx(f.issue + 1000); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("IssueDescriptionTx of a missing issue = %v, want ErrNotFound", err)
	}
}

// TestPgStoreDispatchAndGuardQueries runs the dispatch and guard queries,
// whose helper SQL the PostgreSQL store shares, against seeded runs of two
// projects: each read keeps its run, project, kind, name and status filters,
// the jsonb close payloads scan back as text in seq order although their
// timestamps descend, and a missing proposal reads as not found. The run and
// step updated last move to the end of their tables' heaps, so a list read
// that dropped its ORDER BY would return them after their later siblings.
// Payloads are written in jsonb's canonical text so they read back unchanged.
// It skips without DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreDispatchAndGuardQueries(t *testing.T) {
	f := newSagaFixture(t)
	testsupport.Must(t, f.tx.Commit(), "releasing the fixture's transaction")
	project, err := f.st.RunProjectID(f.run)
	testsupport.Must(t, err, "RunProjectID: %v", err)
	awayProject, err := f.st.EnsureProject("/repo/away", "away", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	sibling, _, err := f.st.CreateIssueIdempotent(&model.Issue{
		ProjectID: project, Title: "sibling", Status: model.StatusTodo,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	newRun := func(projectID int) int {
		t.Helper()
		run, _, err := f.st.InsertRunWithContextIdempotent(projectID, "dispatch", 0, 1, db.RunContext{}, "")
		testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
		testsupport.Must(t, f.st.AddRunIssue(run.ID, f.issue), "AddRunIssue")
		return run.ID
	}
	dispatched, idle := f.run, f.otherRun
	gated, done, abandoned, elsewhere := newRun(project), newRun(project), newRun(project), newRun(awayProject)
	f.tx, err = f.st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	seed := func(e db.NewEvent) {
		t.Helper()
		testsupport.Must(t, f.tx.InsertEventTx(e), "InsertEventTx %s", e.Kind)
	}

	before, err := f.tx.LastEventSeqTx()
	testsupport.Must(t, err, "LastEventSeqTx before the seeded events: %v", err)
	seed(db.NewEvent{AtMS: 900, Kind: "dispatch-closed", RunID: dispatched, Data: `{"dispatch": "DISPATCH-1"}`})
	seed(db.NewEvent{AtMS: 800, Kind: "dispatch-opened", RunID: dispatched, Data: `{"dispatch": "DISPATCH-2"}`})
	seed(db.NewEvent{AtMS: 700, Kind: "dispatch-closed", RunID: gated, Data: `{"dispatch": "DISPATCH-3"}`})
	seed(db.NewEvent{AtMS: 600, Kind: "dispatch-closed", RunID: dispatched, Data: `{"dispatch": "DISPATCH-2"}`})
	seed(db.NewEvent{AtMS: 1, Kind: "issue-abandoned", RunID: dispatched, IssueID: f.issue, Data: `{}`})
	seed(db.NewEvent{AtMS: 2, Kind: "issue-abandoned", RunID: dispatched, IssueID: f.issue, Data: `{}`})
	seed(db.NewEvent{AtMS: 3, Kind: "issue-abandoned", RunID: dispatched, Data: `{"no": "issue"}`})
	seed(db.NewEvent{AtMS: 4, Kind: "issue-reopened", RunID: dispatched, IssueID: sibling, Data: `{}`})
	seed(db.NewEvent{AtMS: 5, Kind: "issue-abandoned", RunID: gated, IssueID: sibling, Data: `{}`})
	seed(db.NewEvent{AtMS: 6, Kind: "run-note-added", RunID: elsewhere, Data: `{}`})
	newest, found, err := f.tx.MaxEventSeqTx()
	testsupport.Must(t, err, "MaxEventSeqTx: %v", err)
	if seq, err := f.tx.LastEventSeqTx(); err != nil || !found || seq != newest || seq <= before {
		t.Errorf("LastEventSeqTx over every run's events = (%d, %v), want (%d, nil), past %d", seq, err, newest, before)
	}

	if got, err := f.tx.PriorDispatchClosesTx(dispatched, "dispatch-closed"); err != nil ||
		!reflect.DeepEqual(got, []string{`{"dispatch": "DISPATCH-1"}`, `{"dispatch": "DISPATCH-2"}`}) {
		t.Errorf("PriorDispatchClosesTx = (%q, %v), want DISPATCH-1 then DISPATCH-2 in seq order", got, err)
	}
	if got, err := f.tx.PriorDispatchClosesTx(idle, "dispatch-closed"); err != nil || len(got) != 0 {
		t.Errorf("PriorDispatchClosesTx of a run that never closed one = (%q, %v), want (none, nil)", got, err)
	}
	if got, err := f.tx.RunEventIssueIDsTx(dispatched, "issue-abandoned"); err != nil ||
		!reflect.DeepEqual(got, map[int]bool{f.issue: true}) {
		t.Errorf("RunEventIssueIDsTx = (%v, %v), want ({%d: true}, nil)", got, err, f.issue)
	}
	if got, err := f.tx.RunEventIssueIDsTx(idle, "issue-abandoned"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("RunEventIssueIDsTx of a run that logged none = (%v, %v), want (an empty set, nil)", got, err)
	}

	now := time.Unix(1000, 0)
	for id, status := range map[int]model.ProposalStatus{7: model.ProposalStatusOpen, 8: model.ProposalStatusCommitted} {
		_, err := f.tx.InsertProposalWithID(&model.Proposal{
			ID: id, ProjectID: project, Description: "vote", Criticality: model.CriticalityLow, Status: status,
			RequiredVoters: 1, Threshold: 0.5, CreatedAt: now, UpdatedAt: now,
		})
		testsupport.Must(t, err, "InsertProposalWithID %d: %v", id, err)
	}
	if status, found, err := f.tx.ProposalStatusTx(8); err != nil || !found || status != model.ProposalStatusCommitted {
		t.Errorf("ProposalStatusTx = (%q, %v, %v), want (committed, true, nil)", status, found, err)
	}

	f.add(db.StepRow{RunID: dispatched, StepName: "approve", Kind: "human", Status: db.StepPending})
	tallied := f.add(db.StepRow{RunID: dispatched, StepName: "approve", Ordinal: 1, Kind: "vote", Status: db.StepPending})
	f.add(db.StepRow{RunID: dispatched, StepName: "approve", Ordinal: 2, Status: db.StepDone})
	f.add(db.StepRow{RunID: dispatched, StepName: "ship", Kind: "human", Status: db.StepDone})
	f.add(db.StepRow{RunID: idle, StepName: "implement", Status: db.StepPending})
	f.add(db.StepRow{RunID: gated, StepName: "approve", Kind: "human", Status: db.StepWaitingHuman})
	f.add(db.StepRow{RunID: done, StepName: "approve", Kind: "human", Status: db.StepDone})
	f.add(db.StepRow{RunID: abandoned, StepName: "implement", Status: db.StepRunning})
	f.add(db.StepRow{RunID: elsewhere, StepName: "approve", Kind: "human", Status: db.StepFailedRouted})
	for _, run := range []int{dispatched, done, elsewhere} {
		_, err := f.tx.InsertDispatchTx(run, 0, 5000, 1000)
		testsupport.Must(t, err, "InsertDispatchTx: %v", err)
	}
	testsupport.Must(t, f.tx.SetRunStatusTx(done, model.RunDone, "", 2), "SetRunStatusTx done")
	testsupport.Must(t, f.tx.SetRunStatusTx(abandoned, model.RunAbandoned, "", 2), "SetRunStatusTx abandoned")
	testsupport.Must(t, f.tx.SetRunStatusTx(elsewhere, model.RunWaitingHuman, "", 2), "SetRunStatusTx waiting-human")
	testsupport.Must(t, f.tx.SetStepRoutingTx(tallied, "pass", "tallied", db.StepDone, "", 3), "SetStepRoutingTx")
	testsupport.Must(t, f.tx.SetRunStatusTx(dispatched, model.RunWaitingHuman, "", 3), "SetRunStatusTx dispatched")
	testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")

	if got, err := f.st.ActiveStartedRunIDs(0); err != nil || !reflect.DeepEqual(got, []int{dispatched, gated, elsewhere}) {
		t.Errorf("ActiveStartedRunIDs over every project = (%v, %v), want (%v, nil)", got, err,
			[]int{dispatched, gated, elsewhere})
	}
	if got, err := f.st.ActiveStartedRunIDs(project); err != nil || !reflect.DeepEqual(got, []int{dispatched, gated}) {
		t.Errorf("ActiveStartedRunIDs of one project = (%v, %v), want (%v, nil)", got, err, []int{dispatched, gated})
	}
	if got, err := f.st.ActiveStartedRunIDs(awayProject + 1); err != nil || len(got) != 0 {
		t.Errorf("ActiveStartedRunIDs of a project with no runs = (%v, %v), want (none, nil)", got, err)
	}

	routed, err := f.st.GetStep(tallied)
	testsupport.Must(t, err, "GetStep: %v", err)
	if routed.Routing == "" {
		t.Fatalf("the routed vote step stored no routing record, so the routing column went unchecked")
	}
	kinds := []string{"human", "vote"}
	inRun := []store.GateStepState{{Status: db.StepPending}, {Status: db.StepDone, Routing: routed.Routing}}
	cases := []struct {
		name string
		q    store.GateStepQuery
		want []store.GateStepState
	}{
		{"every project", store.GateStepQuery{StepName: "approve", Kinds: kinds},
			append(append([]store.GateStepState{}, inRun...), store.GateStepState{Status: db.StepWaitingHuman},
				store.GateStepState{Status: db.StepFailedRouted})},
		{"one project", store.GateStepQuery{StepName: "approve", Kinds: kinds, ProjectID: project},
			append(append([]store.GateStepState{}, inRun...), store.GateStepState{Status: db.StepWaitingHuman})},
		{"a named run outside the project", store.GateStepQuery{StepName: "approve", Kinds: kinds,
			RunID: elsewhere, ProjectID: project}, []store.GateStepState{{Status: db.StepFailedRouted}}},
		{"one kind", store.GateStepQuery{StepName: "approve", Kinds: []string{"vote"}, RunID: dispatched}, inRun[1:]},
		{"an ended run", store.GateStepQuery{StepName: "approve", Kinds: kinds, RunID: done}, nil},
	}
	for _, c := range cases {
		if got, err := f.st.ActiveGateSteps(c.q); err != nil || len(got) != len(c.want) || (len(got) > 0 && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("ActiveGateSteps for %s = (%+v, %v), want (%+v, nil)", c.name, got, err, c.want)
		}
	}

	snap, err := f.st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	if status, found, err := snap.ProposalStatusTx(7); err != nil || !found || status != model.ProposalStatusOpen {
		t.Errorf("ProposalStatusTx through a snapshot = (%q, %v, %v), want (open, true, nil)", status, found, err)
	}
	if status, found, err := snap.ProposalStatusTx(999); err != nil || found || status != "" {
		t.Errorf("ProposalStatusTx of a missing proposal = (%q, %v, %v), want (\"\", false, nil)", status, found, err)
	}
	if seq, err := snap.LastEventSeqTx(); err != nil || seq != newest {
		t.Errorf("LastEventSeqTx through a snapshot = (%d, %v), want (%d, nil)", seq, err, newest)
	}
}

// TestPgStoreDispatchConditionQueries reaches the dispatch-conditions reads,
// whose helper SQL the PostgreSQL store shares, through the pool: an event's
// at_ms by seq, with a seq past the log reading as ErrNotFound, and the
// approve-with-concerns casts on the run's sealed votes since a stamp that no
// tribunal issue files, in proposal then voter order. It skips without
// DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreDispatchConditionQueries(t *testing.T) {
	f := newSagaFixture(t)
	project, err := f.st.RunProjectID(f.run)
	testsupport.Must(t, err, "RunProjectID: %v", err)
	tribunal, _, err := f.st.CreateIssueIdempotent(&model.Issue{
		ProjectID: project, Title: "tribunal", Status: model.StatusTodo,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, []string{"tribunal"}, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	work := f.issue

	since := time.Unix(5000, 0)
	testsupport.Must(t, f.tx.InsertEventTx(db.NewEvent{AtMS: since.UnixMilli(), Kind: "dispatch-opened", RunID: f.run, Data: `{}`}),
		"InsertEventTx")
	opened, err := f.tx.LastEventSeqTx()
	testsupport.Must(t, err, "LastEventSeqTx: %v", err)
	voteID := 0
	propose := func(id int, status model.ProposalStatus, updated time.Time, issues []int, casts map[string]model.Verdict) {
		t.Helper()
		_, err := f.tx.InsertProposalWithID(&model.Proposal{
			ID: id, ProjectID: project, Description: "vote", Criticality: model.CriticalityLow, Status: status,
			RequiredVoters: 1, Threshold: 0.5, CreatedAt: since.Add(-time.Hour), UpdatedAt: updated,
		})
		testsupport.Must(t, err, "InsertProposalWithID %d: %v", id, err)
		for _, issue := range issues {
			_, err := f.tx.InsertProposalIssueLink(id, issue)
			testsupport.Must(t, err, "InsertProposalIssueLink %d: %v", id, err)
		}
		for voter, verdict := range casts {
			voteID++
			_, err := f.tx.InsertVoteWithID(&model.Vote{
				ID: voteID, ProposalID: id, VoterName: voter, Verdict: verdict,
				Confidence: 0.9, DomainRelevance: 0.9, CreatedAt: since,
			})
			testsupport.Must(t, err, "InsertVoteWithID %d: %v", voteID, err)
		}
	}
	concerns, sealed := model.VerdictApproveWithConcerns, model.ProposalStatusApproved
	propose(20, sealed, since.Add(time.Minute), []int{work}, map[string]model.Verdict{"seat-c": concerns})
	propose(10, sealed, since, []int{work}, map[string]model.Verdict{
		"seat-z": concerns, "seat-a": concerns, "seat-m": model.VerdictApprove,
	})
	propose(30, model.ProposalStatusOpen, since.Add(time.Minute), []int{work},
		map[string]model.Verdict{"seat-open": concerns})
	propose(40, sealed, since.Add(-time.Second), []int{work}, map[string]model.Verdict{"seat-early": concerns})
	propose(50, sealed, since.Add(time.Minute), []int{work, tribunal}, map[string]model.Verdict{"seat-filed": concerns})
	testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")

	if got, err := f.st.EventAtMS(opened); err != nil || got != since.UnixMilli() {
		t.Errorf("EventAtMS of the opened event = (%d, %v), want (%d, nil)", got, err, since.UnixMilli())
	}
	if got, err := f.st.EventAtMS(opened + 1); !errors.Is(err, db.ErrNotFound) || got != 0 {
		t.Errorf("EventAtMS past the log = (%d, %v), want (0, ErrNotFound)", got, err)
	}
	want := []db.UnfiledSeatCondition{
		{ProposalID: 10, Voter: "seat-a"},
		{ProposalID: 10, Voter: "seat-z"},
		{ProposalID: 20, Voter: "seat-c"},
	}
	if got, err := f.st.UnfiledSeatConditions(f.run, since.UnixMilli()); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("UnfiledSeatConditions = (%+v, %v), want (%+v, nil): open, pre-since, tribunal-filed "+
			"proposals and plain approvals excluded", got, err, want)
	}
	if got, err := f.st.UnfiledSeatConditions(f.otherRun+1, since.UnixMilli()); err != nil || len(got) != 0 {
		t.Errorf("UnfiledSeatConditions of a run with no issues = (%+v, %v), want (none, nil)", got, err)
	}
}

// TestPgStoreHoldAndActivationQueries runs the hold, activation and definition
// queries, whose helper SQL the PostgreSQL store shares, against seeded rows:
// each read keeps its run, issue, project and kind filters, list reads come
// back in instance, id or name-then-version order where the rows were written
// the other way, the jsonb parsed workflows scan back as text under DISTINCT,
// the expected-cost sum is a DOUBLE PRECISION total, and missing rows read as
// not found. Parsed bodies are written in jsonb's canonical text so they read
// back unchanged. It skips without DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreHoldAndActivationQueries(t *testing.T) {
	f := newSagaFixture(t)
	testsupport.Must(t, f.tx.Commit(), "releasing the fixture's transaction")
	project, err := f.st.RunProjectID(f.run)
	testsupport.Must(t, err, "RunProjectID: %v", err)
	away, err := f.st.EnsureProject("/repo/away", "away", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	workflow := func(projectID int, name string, version int) int {
		t.Helper()
		wf, _, err := f.st.InsertWorkflow(&model.Workflow{
			ProjectID: projectID, Name: name, Version: version, SourceSHA256: "sha", Body: "body",
			Parsed: fmt.Sprintf(`{"name": "%s@%d"}`, name, version),
		}, 1)
		testsupport.Must(t, err, "InsertWorkflow %s@%d: %v", name, version, err)
		return wf.ID
	}
	alpha1, alpha2, elsewhereWF := workflow(project, "alpha", 1), workflow(project, "alpha", 2), workflow(away, "gamma", 1)
	sibling, _, err := f.st.CreateIssueIdempotent(&model.Issue{
		ProjectID: project, Title: "sibling", Status: model.StatusTodo,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	testsupport.Must(t, f.st.AddRunIssue(f.run, sibling), "AddRunIssue")
	emptyRun, _, err := f.st.InsertRunWithContextIdempotent(project, "empty", 0, 1, db.RunContext{}, "")
	testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
	testsupport.Must(t, f.st.AddRunIssue(emptyRun.ID, f.issue), "AddRunIssue")

	f.tx, err = f.st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	add := func(runID, issueID, workflowID int, instance, name string, ordinal int, cost float64) int {
		t.Helper()
		testsupport.Must(t, f.tx.InsertStepTx(db.StepRow{
			RunID: runID, IssueID: issueID, WorkflowID: workflowID, StepName: name, Ordinal: ordinal,
			Instance: instance, Kind: "executor", Status: db.StepPending, ExpectedCost: cost,
		}, 1), "InsertStepTx %s", instance)
		id, found, err := f.tx.StepIDByInstanceTx(runID, issueID, instance)
		testsupport.Must(t, err, "StepIDByInstanceTx: %v", err)
		if !found {
			t.Fatalf("step %s was not found after its insert", instance)
		}
		return id
	}
	review := add(f.run, f.issue, alpha2, "review@1", "review", 1, 0.5)
	add(f.run, f.issue, alpha2, "review@1#2", "review", 1, 0.25)
	add(f.run, f.issue, f.workflow, "hold#2", "hold", 2, 1)
	routedCluster := add(f.run, f.issue, f.workflow, "hold#1", "hold", 2, 0)
	add(f.run, f.issue, f.workflow, "hold", "hold", 2, 2)
	add(f.run, f.issue, f.workflow, "holdout", "holdout", 2, 0)
	add(f.run, sibling, alpha1, "hold#1", "hold", 2, 0)
	add(f.otherRun, f.issue, elsewhereWF, "hold", "hold", 2, 8)
	testsupport.Must(t, f.tx.SetStepRoutingTx(routedCluster, "pass", "held", db.StepDone, "", 2), "SetStepRoutingTx")
	testsupport.Must(t, f.tx.BindRunIssueTx(&db.RunIssue{RunID: f.run, IssueID: f.issue, WorkflowID: &alpha2}),
		"BindRunIssueTx")
	testsupport.Must(t, f.tx.BindRunIssueTx(&db.RunIssue{RunID: f.run, IssueID: sibling, WorkflowID: &alpha1}),
		"BindRunIssueTx")
	testsupport.Must(t, f.tx.BindRunIssueTx(&db.RunIssue{RunID: f.otherRun, IssueID: f.issue, WorkflowID: &elsewhereWF}),
		"BindRunIssueTx")
	for _, a := range []db.Artifact{
		{RunID: f.run, StepID: review, Kind: "change-summary", Body: "b", Payload: `{"n": 1}`, SHA256: "s1"},
		{RunID: f.run, StepID: review, Kind: "change-summary", Body: "b", Payload: `{"n": 2}`, SHA256: "s2"},
		{RunID: f.run, StepID: review, Kind: "gate-log", Body: "b", SHA256: "s3"},
	} {
		_, err := f.tx.InsertArtifactTx(a, 1)
		testsupport.Must(t, err, "InsertArtifactTx %s: %v", a.Kind, err)
	}

	routed, err := f.tx.GetStepTx(routedCluster)
	testsupport.Must(t, err, "GetStepTx: %v", err)
	if routed.Routing == "" {
		t.Fatalf("the routed cluster row stored no routing record, so the routing column went unchecked")
	}
	cluster := []store.InstanceStatusRouting{
		{Instance: "hold#1", Status: db.StepDone, Routing: routed.Routing},
		{Instance: "hold#2", Status: db.StepPending},
	}
	if got, err := f.tx.HoldClusterInstancesTx(f.run, f.issue, "hold"); err != nil || !reflect.DeepEqual(got, cluster) {
		t.Errorf("HoldClusterInstancesTx = (%+v, %v), want (%+v, nil)", got, err, cluster)
	}
	art, found, err := f.tx.NewestStepArtifactExceptKindsTx(review, []string{"gate-log"})
	if err != nil || !found || art.Kind != "change-summary" || art.Payload != `{"n": 2}` {
		t.Errorf("NewestStepArtifactExceptKindsTx past gate-log = (%+v, %v, %v), want the newest change-summary",
			art, found, err)
	}
	if art, found, err := f.tx.NewestStepArtifactExceptKindsTx(review, nil); err != nil || !found ||
		art.Kind != "gate-log" || art.Payload != "" {
		t.Errorf("NewestStepArtifactExceptKindsTx with no exceptions = (%+v, %v, %v), want the gate-log with no payload",
			art, found, err)
	}
	if art, found, err := f.tx.NewestStepArtifactExceptKindsTx(routedCluster, nil); err != nil || found || art.ID != 0 {
		t.Errorf("NewestStepArtifactExceptKindsTx of a step with none = (%+v, %v, %v), want (zero, false, nil)",
			art, found, err)
	}
	if cost, err := f.tx.RunExpectedCostTx(f.run); err != nil || cost != 3.75 {
		t.Errorf("RunExpectedCostTx = (%v, %v), want (3.75, nil)", cost, err)
	}
	if cost, err := f.tx.RunExpectedCostTx(emptyRun.ID); err != nil || cost != 0 {
		t.Errorf("RunExpectedCostTx of a run with no steps = (%v, %v), want (0, nil)", cost, err)
	}
	testsupport.Must(t, f.tx.RepinStepWorkRootTx(review, "/wt/repinned"), "RepinStepWorkRootTx")
	if step, err := f.tx.GetStepTx(review); err != nil || step.WorkRoot != "/wt/repinned" {
		t.Errorf("work root after RepinStepWorkRootTx = (%+v, %v), want /wt/repinned", step, err)
	}
	if err := f.tx.RepinStepWorkRootTx(999999, "/wt"); !errors.Is(err, db.ErrStepNotFound) {
		t.Errorf("RepinStepWorkRootTx of a missing step: err = %v, want ErrStepNotFound", err)
	}
	stepWorkflows := []store.ParsedWorkflow{
		{ID: f.workflow, Parsed: `{}`}, {ID: alpha1, Parsed: `{"name": "alpha@1"}`},
		{ID: alpha2, Parsed: `{"name": "alpha@2"}`},
	}
	if got, err := f.tx.RunStepWorkflowsTx(f.run); err != nil || !reflect.DeepEqual(got, stepWorkflows) {
		t.Errorf("RunStepWorkflowsTx = (%+v, %v), want (%+v, nil)", got, err, stepWorkflows)
	}
	testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")

	whole := append([]store.InstanceStatusRouting{{Instance: "hold", Status: db.StepPending}}, cluster...)
	if got, err := f.st.HoldInstances(f.run, f.issue, "hold"); err != nil || !reflect.DeepEqual(got, whole) {
		t.Errorf("HoldInstances = (%+v, %v), want (%+v, nil)", got, err, whole)
	}
	if id, found, err := f.st.StepIDAtOrdinal(f.run, f.issue, "review", 1); err != nil || !found || id != review {
		t.Errorf("StepIDAtOrdinal = (%d, %v, %v), want (%d, true, nil): the lowest id", id, found, err, review)
	}
	if id, found, err := f.st.StepIDAtOrdinal(f.run, sibling, "review", 1); err != nil || found || id != 0 {
		t.Errorf("StepIDAtOrdinal of an issue with none = (%d, %v, %v), want (0, false, nil)", id, found, err)
	}
	if got, err := f.st.RunStepWorkflows(f.run); err != nil || !reflect.DeepEqual(got, stepWorkflows) {
		t.Errorf("RunStepWorkflows = (%+v, %v), want (%+v, nil)", got, err, stepWorkflows)
	}
	bound := []store.ParsedWorkflow{{ID: alpha1, Parsed: `{"name": "alpha@1"}`}, {ID: alpha2, Parsed: `{"name": "alpha@2"}`}}
	if got, err := f.st.RunBoundWorkflows(f.run); err != nil || !reflect.DeepEqual(got, bound) {
		t.Errorf("RunBoundWorkflows = (%+v, %v), want (%+v, nil)", got, err, bound)
	}
	if got, err := f.st.RunBoundWorkflows(emptyRun.ID); err != nil || len(got) != 0 {
		t.Errorf("RunBoundWorkflows of a run with no bound issue = (%+v, %v), want (none, nil)", got, err)
	}

	snap, err := f.st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	if identity, err := snap.ProjectIdentityTx(project); err != nil || identity != "/repo/saga" {
		t.Errorf("ProjectIdentityTx = (%q, %v), want (/repo/saga, nil)", identity, err)
	}
	def, err := f.st.GetProject(db.DefaultProjectID)
	testsupport.Must(t, err, "GetProject default: %v", err)
	if identity, err := snap.ProjectIdentityTx(0); err != nil || identity != def.Identity {
		t.Errorf("ProjectIdentityTx(0) = (%q, %v), want the default project's (%q, nil)", identity, err, def.Identity)
	}
	if _, err := snap.ProjectIdentityTx(away + 100); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("ProjectIdentityTx of a missing project: err = %v, want ErrNotFound", err)
	}
	wfs, err := snap.ProjectWorkflowsTx(project)
	testsupport.Must(t, err, "ProjectWorkflowsTx: %v", err)
	var listed []string
	for _, wf := range wfs {
		listed = append(listed, fmt.Sprintf("%s@%d", wf.Name, wf.Version))
	}
	if want := []string{"alpha@2", "alpha@1", "wf@1"}; !reflect.DeepEqual(listed, want) {
		t.Errorf("ProjectWorkflowsTx = %v, want %v: by name, newest version first, this project only", listed, want)
	}
}

// TestPgStoreReapUsageAndFenceQueries runs the reap, vote-usage and fence
// reads, whose helper SQL the PostgreSQL store shares, through a Tx and then
// a read snapshot on seeded rows: the EXISTS AND COALESCE admission test
// scans as a boolean, the json_extract detail match runs as ->> on jsonb,
// each read keeps its run, step, kind, dispatch, proposal and seat filters,
// fences come back by issue, tag and ordinal where they were written the
// other way, a missing event reads as ErrNotFound, and a missing cast as not
// found. Payloads are written in jsonb's canonical text so they read back
// unchanged. It skips without DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreReapUsageAndFenceQueries(t *testing.T) {
	f := newSagaFixture(t)
	testsupport.Must(t, f.tx.Commit(), "releasing the fixture's transaction")
	project, err := f.st.RunProjectID(f.run)
	testsupport.Must(t, err, "RunProjectID: %v", err)
	sibling, _, err := f.st.CreateIssueIdempotent(&model.Issue{
		ProjectID: project, Title: "sibling", Status: model.StatusTodo,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	testsupport.Must(t, f.st.AddRunIssue(f.run, sibling), "AddRunIssue")
	run, otherRun := f.run, f.otherRun
	f.tx, err = f.st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	seed := func(e db.NewEvent) int64 {
		t.Helper()
		testsupport.Must(t, f.tx.InsertEventTx(e), "InsertEventTx %s", e.Kind)
		seq, _, err := f.tx.MaxEventSeqTx()
		testsupport.Must(t, err, "MaxEventSeqTx: %v", err)
		return seq
	}

	admitted := f.add(db.StepRow{StepName: "implement", Status: db.StepClaimed})
	early := f.add(db.StepRow{StepName: "review", Status: db.StepClaimed})
	offManifest := f.add(db.StepRow{StepName: "verify", Status: db.StepClaimed})
	unclaimed := f.add(db.StepRow{StepName: "fix", Status: db.StepPending})
	seed(db.NewEvent{AtMS: 1, Kind: "step-claimed", RunID: run, StepID: early, Data: `{}`})
	opened := seed(db.NewEvent{AtMS: 1, Kind: "dispatch-opened", RunID: run, Data: `{}`})
	claim := seed(db.NewEvent{AtMS: 1, Kind: "step-claimed", RunID: run, StepID: admitted, Data: `{}`})
	seed(db.NewEvent{AtMS: 1, Kind: "step-claimed", RunID: run, StepID: offManifest, Data: `{}`})
	reaped := seed(db.NewEvent{AtMS: 1, Kind: "lease-reaped", RunID: run, StepID: admitted, Data: `{"forced": true}`})
	dispatchID, err := f.tx.InsertDispatchTx(run, opened, 99999, 1000)
	testsupport.Must(t, err, "InsertDispatchTx: %v", err)
	for i, stepID := range []int{admitted, early, unclaimed} {
		testsupport.Must(t, f.tx.InsertDispatchRowTx(dispatchID, db.DispatchRow{
			Position: i, StepID: stepID, Instance: "row", RowJSON: `{}`, RowSHA256: "sha",
		}), "InsertDispatchRowTx %d", i)
	}
	seed(db.NewEvent{AtMS: 1, Kind: "vote-opened", RunID: run, Data: `{"detail": "DKT-V7"}`})
	seed(db.NewEvent{AtMS: 1, Kind: "vote-tallied", RunID: run, Data: `{"detail": "DKT-V8"}`})
	seed(db.NewEvent{AtMS: 1, Kind: "vote-opened", RunID: run, StepID: early, Data: `{"detail": "DKT-V9"}`})
	newest := seed(db.NewEvent{AtMS: 1, Kind: "vote-opened", RunID: otherRun, Data: `{"detail": "DKT-V10"}`})

	now := time.Unix(1000, 0)
	for _, id := range []int{7, 8} {
		_, err := f.tx.InsertProposalWithID(&model.Proposal{
			ID: id, ProjectID: project, Description: "vote", Criticality: model.CriticalityLow,
			Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5, CreatedAt: now, UpdatedAt: now,
		})
		testsupport.Must(t, err, "InsertProposalWithID %d: %v", id, err)
	}
	for _, v := range []struct {
		id, proposal int
		voter        string
	}{{70, 7, "seat-a"}, {71, 7, "seat-b"}, {80, 8, "seat-c"}} {
		_, err := f.tx.InsertVoteWithID(&model.Vote{
			ID: v.id, ProposalID: v.proposal, VoterName: v.voter,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9, CreatedAt: now,
		})
		testsupport.Must(t, err, "InsertVoteWithID %d: %v", v.id, err)
	}

	for _, fence := range []db.RunFence{
		{RunID: run, IssueID: sibling, Tag: "ac", Ordinal: 0, Command: "make c", SHA256: "c"},
		{RunID: run, IssueID: f.issue, Tag: "tests", Ordinal: 1, Command: "make b2", SHA256: "b2"},
		{RunID: run, IssueID: f.issue, Tag: "ac", Ordinal: 0, Command: "make a", SHA256: "a"},
		{RunID: run, IssueID: f.issue, Tag: "tests", Ordinal: 0, Command: "make b1", SHA256: "b1"},
		{RunID: otherRun, IssueID: f.issue, Tag: "ac", Ordinal: 0, Command: "make z", SHA256: "z"},
	} {
		testsupport.Must(t, f.tx.InsertFenceTx(fence), "InsertFenceTx %s#%d", fence.Tag, fence.Ordinal)
	}
	fences := []db.RunFence{
		{RunID: run, IssueID: f.issue, Tag: "ac", Ordinal: 0, Command: "make a", SHA256: "a"},
		{RunID: run, IssueID: f.issue, Tag: "tests", Ordinal: 0, Command: "make b1", SHA256: "b1"},
		{RunID: run, IssueID: f.issue, Tag: "tests", Ordinal: 1, Command: "make b2", SHA256: "b2"},
		{RunID: run, IssueID: sibling, Tag: "ac", Ordinal: 0, Command: "make c", SHA256: "c"},
	}

	check := func(via string, r store.ReadTx) {
		t.Helper()
		if data, err := r.EventDataTx(reaped); err != nil || data != `{"forced": true}` {
			t.Errorf("EventDataTx of the reap through %s = (%q, %v), want its payload", via, data, err)
		}
		if data, err := r.EventDataTx(newest + 1); !errors.Is(err, db.ErrNotFound) || data != "" {
			t.Errorf("EventDataTx past the newest seq through %s = (%q, %v), want (\"\", ErrNotFound)", via, data, err)
		}

		admittedCases := []struct {
			name             string
			dispatch, stepID int
			reapedSeq        int64
			want             bool
		}{
			{"a manifest step claimed after the open", dispatchID, admitted, reaped, true},
			{"a reap at the claim's own seq", dispatchID, admitted, claim, false},
			{"a manifest step last claimed before the open", dispatchID, early, reaped, false},
			{"a step claimed after the open but off the manifest", dispatchID, offManifest, reaped, false},
			{"a manifest step never claimed", dispatchID, unclaimed, reaped, false},
			{"a dispatch that does not exist", dispatchID + 100, admitted, reaped, false},
		}
		for _, c := range admittedCases {
			got, err := r.ClaimAdmittedByDispatchTx(c.dispatch, opened, c.stepID, "step-claimed", c.reapedSeq)
			if err != nil || got != c.want {
				t.Errorf("ClaimAdmittedByDispatchTx for %s through %s = (%v, %v), want (%v, nil)",
					c.name, via, got, err, c.want)
			}
		}

		for id, want := range map[int]bool{7: true, 8: true, 999: false} {
			if got, err := r.ProposalExistsTx(id); err != nil || got != want {
				t.Errorf("ProposalExistsTx(%d) through %s = (%v, %v), want (%v, nil)", id, via, got, err, want)
			}
		}
		voteCases := []struct {
			name      string
			proposal  int
			voter     string
			wantID    int64
			wantFound bool
		}{
			{"seat b's cast", 7, "seat-b", 71, true},
			{"a seat that cast only on another proposal", 7, "seat-c", 0, false},
			{"a proposal that does not exist", 999, "seat-a", 0, false},
		}
		for _, c := range voteCases {
			id, found, err := r.VoteIDTx(c.proposal, c.voter)
			if err != nil || found != c.wantFound || id != c.wantID {
				t.Errorf("VoteIDTx for %s through %s = (%d, %v, %v), want (%d, %v, nil)",
					c.name, via, id, found, err, c.wantID, c.wantFound)
			}
		}

		if got, err := r.ListFencesTx(run); err != nil || !reflect.DeepEqual(got, fences) {
			t.Errorf("ListFencesTx through %s = (%+v, %v), want (%+v, nil)", via, got, err, fences)
		}
		if got, err := r.ListFencesTx(otherRun + 100); err != nil || len(got) != 0 {
			t.Errorf("ListFencesTx of a run with none through %s = (%+v, %v), want (none, nil)", via, got, err)
		}

		detailCases := []struct {
			name   string
			runID  int
			detail string
			want   bool
		}{
			{"the run's opened panel", run, "DKT-V7", true},
			{"a panel the run only tallied", run, "DKT-V8", false},
			{"a panel opened on a step", run, "DKT-V9", false},
			{"a panel opened on another run", run, "DKT-V10", false},
			{"the other run's opened panel", otherRun, "DKT-V10", true},
		}
		for _, c := range detailCases {
			got, err := r.RunEventDetailExistsTx(c.runID, "vote-opened", c.detail)
			if err != nil || got != c.want {
				t.Errorf("RunEventDetailExistsTx for %s through %s = (%v, %v), want (%v, nil)", c.name, via, got, err, c.want)
			}
		}
	}

	check("a Tx", f.tx)
	testsupport.Must(t, f.tx.Commit(), "committing the seeded rows")
	snap, err := f.st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	check("a read snapshot", snap)
}

// TestPgStoreImportExportQueries runs the import id probes, the label lookup
// and the parent restore, whose helper SQL the PostgreSQL store shares,
// through a Tx and then a read snapshot, on one row per probed table in each
// of two projects, the higher id written first: the EXISTS probes scan as
// booleans, existence and the id ceiling span projects, ownership follows a
// child row to its parent's project, a table outside the closed set is
// refused, and a label name resolves only in its own project. The restore
// must leave the issue's version, updated_at and activity log as imported.
// It skips without DOCKET_TEST_POSTGRES_DSN.
func TestPgStoreImportExportQueries(t *testing.T) {
	st := openTestStore(t)
	owner, err := st.EnsureProject("/repo/owner", "owner", 1)
	testsupport.Must(t, err, "EnsureProject owner: %v", err)
	other, err := st.EnsureProject("/repo/other", "other", 1)
	testsupport.Must(t, err, "EnsureProject other: %v", err)

	tx, err := st.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	inserted := func(what string, id int, ok bool, err error) {
		t.Helper()
		testsupport.Must(t, err, "inserting %s %d: %v", what, id, err)
		if !ok {
			t.Fatalf("inserting %s %d wrote no row", what, id)
		}
	}
	imported := time.Unix(2000, 0).UTC()
	for _, i := range []struct{ id, project int }{{20, other}, {15, owner}, {10, owner}} {
		ok, err := tx.InsertIssueWithID(&model.Issue{
			ID: i.id, ProjectID: i.project, Title: "imported", Status: model.StatusTodo,
			Priority: model.PriorityNone, Kind: model.IssueKindTask, CreatedAt: imported, UpdatedAt: imported,
		})
		inserted("issue", i.id, ok, err)
	}
	for _, l := range []model.Label{{ID: 132, ProjectID: other, Name: "bug"}, {ID: 131, ProjectID: owner, Name: "bug"},
		{ID: 130, ProjectID: owner, Name: "feature"}} {
		ok, err := tx.InsertLabelWithID(&l)
		inserted("label", l.ID, ok, err)
	}
	for _, c := range []model.Comment{{ID: 32, IssueID: 20}, {ID: 31, IssueID: 10}} {
		c.Body, c.CreatedAt = "imported", imported
		ok, err := tx.InsertCommentWithID(&c)
		inserted("comment", c.ID, ok, err)
	}
	for _, r := range []model.Relation{
		{ID: 42, SourceIssueID: 20, TargetIssueID: 10, RelationType: model.RelationBlocks},
		{ID: 41, SourceIssueID: 10, TargetIssueID: 15, RelationType: model.RelationRelatesTo},
	} {
		r.CreatedAt = imported
		ok, err := tx.InsertRelationWithID(&r)
		inserted("relation", r.ID, ok, err)
	}
	for _, a := range []model.Activity{{ID: 52, IssueID: 20}, {ID: 51, IssueID: 10}} {
		a.FieldChanged, a.CreatedAt = "status", imported
		ok, err := tx.InsertActivityWithID(&a)
		inserted("activity", a.ID, ok, err)
	}
	for _, p := range []struct{ id, project int }{{70, other}, {60, owner}} {
		ok, err := tx.InsertProposalWithID(&model.Proposal{
			ID: p.id, ProjectID: p.project, Description: "imported", Criticality: model.CriticalityLow,
			Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5, CreatedAt: imported, UpdatedAt: imported,
		})
		inserted("proposal", p.id, ok, err)
	}
	for _, v := range []model.Vote{{ID: 82, ProposalID: 70}, {ID: 81, ProposalID: 60}} {
		v.VoterName, v.Verdict, v.Confidence, v.DomainRelevance, v.CreatedAt = "seat", model.VerdictApprove, 0.9, 0.9, imported
		ok, err := tx.InsertVoteWithID(&v)
		inserted("vote", v.ID, ok, err)
	}
	for _, d := range []struct{ id, project int }{{100, other}, {90, owner}} {
		ok, err := tx.InsertDocWithID(&model.Doc{
			ID: d.id, ProjectID: d.project, Type: "adr", Status: "draft", Title: "imported",
			CreatedAt: imported, UpdatedAt: imported,
		})
		inserted("doc", d.id, ok, err)
	}
	for _, r := range []model.DocRevision{{ID: 112, DocID: 100}, {ID: 111, DocID: 90}} {
		r.RevisionNumber, r.Body, r.CreatedAt = 1, "imported", imported
		ok, err := tx.InsertDocRevisionWithID(&r)
		inserted("doc revision", r.ID, ok, err)
	}
	for _, c := range []model.DocComment{{ID: 122, DocID: 100}, {ID: 121, DocID: 90}} {
		c.Body, c.CreatedAt = "imported", imported
		ok, err := tx.InsertDocCommentWithID(&c)
		inserted("doc comment", c.ID, ok, err)
	}

	// Each probed table's row in owner and its row in other. A child row's id
	// differs from its parent's, so a probe that matched the wrong column
	// would miss.
	rows := []struct {
		table          store.ImportTable
		owned, foreign int
	}{
		{store.ImportIssues, 10, 20},
		{store.ImportLabels, 131, 132},
		{store.ImportComments, 31, 32},
		{store.ImportIssueRelations, 41, 42},
		{store.ImportActivityLog, 51, 52},
		{store.ImportProposals, 60, 70},
		{store.ImportVotes, 81, 82},
		{store.ImportDocs, 90, 100},
		{store.ImportDocRevisions, 111, 112},
		{store.ImportDocComments, 121, 122},
	}

	check := func(via string, r store.ReadTx) {
		t.Helper()
		for _, row := range rows {
			for id, want := range map[int]bool{row.owned: true, row.foreign: true, 999: false} {
				if got, err := r.ImportRowExistsTx(row.table, id); err != nil || got != want {
					t.Errorf("ImportRowExistsTx(%s, %d) through %s = (%v, %v), want (%v, nil)", row.table, id, via, got, err, want)
				}
			}
			if got, err := r.ImportMaxIDTx(row.table); err != nil || got != row.foreign {
				t.Errorf("ImportMaxIDTx(%s) through %s = (%d, %v), want (%d, nil)", row.table, via, got, err, row.foreign)
			}
			ownedCases := []struct {
				id, project int
				want        bool
			}{
				{row.owned, owner, true},
				{row.foreign, other, true},
				{row.foreign, owner, false},
				{row.owned, other, false},
				{999, owner, false},
			}
			for _, c := range ownedCases {
				if got, err := r.ImportRowOwnedTx(row.table, c.id, c.project); err != nil || got != c.want {
					t.Errorf("ImportRowOwnedTx(%s, %d, project %d) through %s = (%v, %v), want (%v, nil)",
						row.table, c.id, c.project, via, got, err, c.want)
				}
			}
		}
		const unlisted = store.ImportTable("projects")
		if _, err := r.ImportRowExistsTx(unlisted, owner); err == nil {
			t.Errorf("ImportRowExistsTx(%s) through %s succeeded, want an unknown-table error", unlisted, via)
		}
		if _, err := r.ImportMaxIDTx(unlisted); err == nil {
			t.Errorf("ImportMaxIDTx(%s) through %s succeeded, want an unknown-table error", unlisted, via)
		}
		if _, err := r.ImportRowOwnedTx(unlisted, owner, owner); err == nil {
			t.Errorf("ImportRowOwnedTx(%s) through %s succeeded, want an unknown-table error", unlisted, via)
		}

		labelCases := []struct {
			project   int
			name      string
			wantID    int
			wantFound bool
		}{
			{owner, "bug", 131, true},
			{other, "bug", 132, true},
			{owner, "feature", 130, true},
			{other, "feature", 0, false},
			{owner, "absent", 0, false},
		}
		for _, c := range labelCases {
			id, found, err := r.LabelIDByNameTx(c.project, c.name)
			if err != nil || id != c.wantID || found != c.wantFound {
				t.Errorf("LabelIDByNameTx(project %d, %q) through %s = (%d, %v, %v), want (%d, %v, nil)",
					c.project, c.name, via, id, found, err, c.wantID, c.wantFound)
			}
		}
	}

	check("a Tx", tx)
	child, err := tx.ActivationIssueTx(15)
	testsupport.Must(t, err, "ActivationIssueTx before the restore: %v", err)
	testsupport.Must(t, tx.RestoreIssueParentTx(15, 10), "RestoreIssueParentTx")
	testsupport.Must(t, tx.RestoreIssueParentTx(999, 10), "RestoreIssueParentTx of an absent issue")
	testsupport.Must(t, tx.Commit(), "committing the seeded rows")
	snap, err := st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	check("a read snapshot", snap)

	restored, err := st.GetIssue(15)
	testsupport.Must(t, err, "GetIssue after the restore: %v", err)
	if restored.ParentID == nil || *restored.ParentID != 10 {
		t.Errorf("issue 15 parent after RestoreIssueParentTx = %v, want 10", restored.ParentID)
	}
	if restored.Version != child.Version || !restored.UpdatedAt.Equal(imported) {
		t.Errorf("issue 15 after RestoreIssueParentTx has version %d, updated_at %v; want the imported %d, %v",
			restored.Version, restored.UpdatedAt, child.Version, imported)
	}
	if activity, err := st.GetActivity(15, 0); err != nil || len(activity) != 0 {
		t.Errorf("activity of issue 15 after RestoreIssueParentTx = (%+v, %v), want none", activity, err)
	}
	if parent, err := st.GetIssue(10); err != nil || parent.ParentID != nil {
		t.Errorf("issue 10 after RestoreIssueParentTx = (%+v, %v), want it still parentless", parent, err)
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
