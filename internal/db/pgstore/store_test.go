package pgstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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
	wantProducers := []db.StepProducer{
		{StepID: implement, Instance: "implement@0", Executor: "claude", IssueID: f.issue},
		{StepID: review, Instance: "review@0", IssueID: f.issue},
	}
	if got, err := f.st.RunStepProducers(f.run); err != nil || !reflect.DeepEqual(got, wantProducers) {
		t.Errorf("RunStepProducers = (%+v, %v), want (%+v, nil)", got, err, wantProducers)
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
