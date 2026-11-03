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
