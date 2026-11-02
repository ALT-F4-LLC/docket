package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func openMigratedSQLite(t *testing.T) Store {
	t.Helper()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "issues.db"))
	testsupport.Must(t, err, "opening the store: %v", err)
	t.Cleanup(func() { st.Close() })
	testsupport.Must(t, st.Initialize(), "Initialize")
	testsupport.Must(t, st.Migrate(), "Migrate")
	return st
}

// TestSQLiteEventAndRunFactQueries reaches the engine's event and run-fact
// queries through a Tx, building their inputs from the store aliases: an
// inserted event moves the log's newest stamp and highest seq, and the step
// and deferral lookups report no match on a store with no runs.
func TestSQLiteEventAndRunFactQueries(t *testing.T) {
	st := openMigratedSQLite(t)
	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })

	if seq, found, err := tx.MaxEventSeqTx(); err != nil || found || seq != 0 {
		t.Errorf("MaxEventSeqTx on an empty log = (%d, %v, %v), want (0, false, nil)", seq, found, err)
	}
	if atMS, found, err := tx.LastEventAtMSTx(); err != nil || found || atMS != 0 {
		t.Errorf("LastEventAtMSTx on an empty log = (%d, %v, %v), want (0, false, nil)", atMS, found, err)
	}

	testsupport.Must(t, tx.InsertEventTx(NewEvent{AtMS: 70, Kind: "trust-added", Data: `{}`}),
		"inserting the first event")
	testsupport.Must(t, tx.InsertEventTx(NewEvent{AtMS: 40, Kind: "trust-added", Data: `{}`}),
		"inserting the second event")

	if seq, found, err := tx.MaxEventSeqTx(); err != nil || !found || seq != 2 {
		t.Errorf("MaxEventSeqTx after two inserts = (%d, %v, %v), want (2, true, nil)", seq, found, err)
	}
	if atMS, found, err := tx.LastEventAtMSTx(); err != nil || !found || atMS != 40 {
		t.Errorf("LastEventAtMSTx after two inserts = (%d, %v, %v), want (40, true, nil): the newest seq's stamp",
			atMS, found, err)
	}

	if id, found, err := tx.StepIDByInstanceTx(1, 0, "implement@0"); err != nil || found || id != 0 {
		t.Errorf("StepIDByInstanceTx with no runs = (%d, %v, %v), want (0, false, nil)", id, found, err)
	}
	seq, data, found, err := tx.ConductorChainDeferralTx(ChainDeferralQuery{
		RunID: 1, StepID: 1, DeferredKind: "step-deferred", Cause: "chain", DispatchClosedKind: "dispatch-closed",
	})
	if err != nil || found || seq != 0 || data != "" {
		t.Errorf("ConductorChainDeferralTx with no runs = (%d, %q, %v, %v), want (0, \"\", false, nil)",
			seq, data, found, err)
	}
}

// TestSQLiteRoutingAndLoopQueries reaches the engine's routing and loop
// queries through the Store and a Tx, building their inputs from the store
// aliases: on a store with no runs or issues, the lookups report no match or
// the store's not-found sentinel, and the writes move nothing.
func TestSQLiteRoutingAndLoopQueries(t *testing.T) {
	st := openMigratedSQLite(t)

	if id, found, err := st.GatedStepIDAtOrdinal(1, 1, "implement", 0); err != nil || found || id != 0 {
		t.Errorf("GatedStepIDAtOrdinal with no runs = (%d, %v, %v), want (0, false, nil)", id, found, err)
	}

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })

	if _, err := tx.RunStatusTx(1); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("RunStatusTx with no runs: err = %v, want ErrRunNotFound", err)
	}
	if _, err := tx.IssueStatusTx(1); !errors.Is(err, ErrNotFound) {
		t.Errorf("IssueStatusTx with no issues: err = %v, want ErrNotFound", err)
	}
	if _, err := tx.StepByInstanceTx(1, 1, "implement@0"); !errors.Is(err, ErrStepNotFound) {
		t.Errorf("StepByInstanceTx with no runs: err = %v, want ErrStepNotFound", err)
	}
	if rollup, err := tx.RunStepRollupTx(1); err != nil || rollup != (RunStepRollup{}) {
		t.Errorf("RunStepRollupTx with no steps = (%+v, %v), want zeros", rollup, err)
	}

	r := StepArtifactRange{RunID: 1, IssueID: 1, StepName: "implement", Kind: "change-summary", MaxOrdinal: 3}
	if art, found, err := tx.NewestStepNameArtifactTx(r); err != nil || found || art != (StepArtifact{}) {
		t.Errorf("NewestStepNameArtifactTx with no artifacts = (%+v, %v, %v), want (zero, false, nil)", art, found, err)
	}
	if seen, err := tx.DeferralRecordedForDispatchTx(DispatchDeferralQuery{
		RunID: 1, StepID: 1, Kind: "step-deferred", Cause: "chain", Dispatch: "DISPATCH-1",
	}); err != nil || seen {
		t.Errorf("DeferralRecordedForDispatchTx with no events = (%v, %v), want (false, nil)", seen, err)
	}
	if seq, data, found, err := tx.LastDispatchDeferralTx(SourcedDeferralQuery{
		RunID: 1, StepID: 1, Kind: "step-deferred", Cause: "chain", Source: "conductor",
	}); err != nil || found || seq != 0 || data != "" {
		t.Errorf("LastDispatchDeferralTx with no dispatches = (%d, %q, %v, %v), want (0, \"\", false, nil)",
			seq, data, found, err)
	}

	if _, _, err := tx.TransitionRunStatusTx(1, model.RunWaitingHuman, 10); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("TransitionRunStatusTx with no runs: err = %v, want ErrRunNotFound", err)
	}
	if err := tx.SetRunIssueLoopCountTx(1, 1, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetRunIssueLoopCountTx with no run issues: err = %v, want ErrNotFound", err)
	}
	if moved, err := tx.CompleteIssueTx(1, model.StatusReview, "", 10); err != nil || moved {
		t.Errorf("CompleteIssueTx with no issues = (%v, %v), want (false, nil)", moved, err)
	}
	if n, err := tx.CascadeFailIssueStepsTx(1, 1, 0, `{}`, 10); err != nil || n != 0 {
		t.Errorf("CascadeFailIssueStepsTx with no steps = (%d, %v), want (0, nil)", n, err)
	}
}

// TestSQLiteSagaQueries reaches the engine's completion-saga queries through
// the Store and a Tx, building their inputs from the store aliases: on a
// store with no runs, the lookups report no match, the counts and sums are
// zero, and recording a worktree reports the store's step sentinel.
func TestSQLiteSagaQueries(t *testing.T) {
	st := openMigratedSQLite(t)

	if n, err := st.StepArtifactCount(1, "change-summary"); err != nil || n != 0 {
		t.Errorf("StepArtifactCount with no artifacts = (%d, %v), want (0, nil)", n, err)
	}
	if body, found, err := st.NewestStepArtifactBody(1, "change-summary"); err != nil || found || body != "" {
		t.Errorf("NewestStepArtifactBody with no artifacts = (%q, %v, %v), want (\"\", false, nil)", body, found, err)
	}
	if p, found, err := st.NewestStepPayload(1, ""); err != nil || found || p != "" {
		t.Errorf("NewestStepPayload with no artifacts = (%q, %v, %v), want (\"\", false, nil)", p, found, err)
	}
	if _, err := st.RunIssueSnapshot(1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("RunIssueSnapshot with no run issues: err = %v, want ErrNotFound", err)
	}
	if p, found, err := st.NewestIssueArtifactPayloadExcept(1, 1, "findings", 2); err != nil || found || p != "" {
		t.Errorf("NewestIssueArtifactPayloadExcept with no artifacts = (%q, %v, %v), want (\"\", false, nil)", p, found, err)
	}
	if id, p, found, err := st.NewestIssueArtifactConsumedByDoneStep(1, 1, "findings"); err != nil || found || id != 0 || p != "" {
		t.Errorf("NewestIssueArtifactConsumedByDoneStep with no inputs = (%d, %q, %v, %v), want (0, \"\", false, nil)",
			id, p, found, err)
	}
	if m, found, err := st.FirstRecorderAfterMetadata(FirstRecorderAfterQuery{
		RunID: 1, IssueID: 1, Kind: "findings", AfterArtifactID: 0, ExceptStepID: 2,
	}); err != nil || found || m != "" {
		t.Errorf("FirstRecorderAfterMetadata with no artifacts = (%q, %v, %v), want (\"\", false, nil)", m, found, err)
	}
	if p, found, err := st.NewestStepNamePayloadBelowOrdinal(1, 1, "review", 2, "findings"); err != nil || found || p != "" {
		t.Errorf("NewestStepNamePayloadBelowOrdinal with no artifacts = (%q, %v, %v), want (\"\", false, nil)", p, found, err)
	}
	var commands []FenceCommand
	commands, found, err := st.RunFenceCommands(1, 1, "verify")
	if err != nil || found || len(commands) != 0 {
		t.Errorf("RunFenceCommands with no fences = (%v, %v, %v), want (none, false, nil)", commands, found, err)
	}
	if body, p, found, err := st.NewestIssueArtifact(1, 1, "findings"); err != nil || found || body != "" || p != "" {
		t.Errorf("NewestIssueArtifact with no artifacts = (%q, %q, %v, %v), want (\"\", \"\", false, nil)", body, p, found, err)
	}
	if bodies, err := st.NewestIssueArtifactBodies(1, 1, "findings", 3); err != nil || len(bodies) != 0 {
		t.Errorf("NewestIssueArtifactBodies with no artifacts = (%v, %v), want (none, nil)", bodies, err)
	}
	if data, found, err := st.FirstStepEventData(1, "step-claimed"); err != nil || found || data != "" {
		t.Errorf("FirstStepEventData with no events = (%q, %v, %v), want (\"\", false, nil)", data, found, err)
	}
	if seen, err := st.StepEventDataContains(1, 1, "gate-started", "tests"); err != nil || seen {
		t.Errorf("StepEventDataContains with no events = (%v, %v), want (false, nil)", seen, err)
	}
	if ids, err := st.StrandedSagaStepIDs(1, "action", 1000); err != nil || len(ids) != 0 {
		t.Errorf("StrandedSagaStepIDs with no steps = (%v, %v), want (none, nil)", ids, err)
	}

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })

	if n, err := tx.NextGateOrdinalTx(1, "tests"); err != nil || n != 0 {
		t.Errorf("NextGateOrdinalTx with no results = (%d, %v), want (0, nil)", n, err)
	}
	if floor, err := tx.RunCostFloorTx(1, "step-claimed", "vote"); err != nil || floor != 0 {
		t.Errorf("RunCostFloorTx with no claims or votes = (%v, %v), want (0, nil)", floor, err)
	}
	if err := tx.SetStepWorkRootTx(1, "/work/tree"); !errors.Is(err, ErrStepNotFound) {
		t.Errorf("SetStepWorkRootTx with no steps: err = %v, want ErrStepNotFound", err)
	}
}

// TestSQLiteReportAndLedgerQueries reaches the run report's and the executor
// ledger's event reads through the Store and a read snapshot, on a seeded run:
// each returns the rows of the run, issue and kinds it was asked for, typed by
// the store aliases.
func TestSQLiteReportAndLedgerQueries(t *testing.T) {
	st := openMigratedSQLite(t)
	project, err := st.EnsureProject("/repo/report", "report", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	wf, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: project, Name: "wf", Version: 1, SourceSHA256: "sha", Body: "body", Parsed: "{}",
	}, 1)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)
	issue, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: project, Title: "report subject", Status: model.StatusTodo,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	newRun := func() int {
		run, _, err := st.InsertRunWithContextIdempotent(project, "report", 0, 1, db.RunContext{}, "")
		testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
		testsupport.Must(t, st.AddRunIssue(run.ID, issue), "AddRunIssue")
		return run.ID
	}
	run, otherRun := newRun(), newRun()

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the seeding transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	addStep := func(runID int, name, executor string) int {
		t.Helper()
		instance := name + "@0"
		testsupport.Must(t, tx.InsertStepTx(db.StepRow{
			RunID: runID, IssueID: issue, WorkflowID: wf.ID, StepName: name, Instance: instance,
			Kind: "executor", Executor: executor, Status: db.StepDone,
		}, 1), "InsertStepTx %s", instance)
		id, found, err := tx.StepIDByInstanceTx(runID, issue, instance)
		testsupport.Must(t, err, "StepIDByInstanceTx: %v", err)
		if !found {
			t.Fatalf("step %s was not found after its insert", instance)
		}
		return id
	}
	implement := addStep(run, "implement", "claude")
	review := addStep(run, "review", "")
	elsewhere := addStep(otherRun, "implement", "codex")
	testsupport.Must(t, tx.SetStepRoutingTx(review, "advance", "approved", db.StepDone, "", 1), "SetStepRoutingTx")
	for _, e := range []NewEvent{
		{AtMS: 10, Kind: "step-claimed", RunID: run, StepID: implement, Data: `{"n": 1}`},
		{AtMS: 20, Kind: "step-recorded", RunID: run, StepID: review, Data: `{"n": 2}`},
		{AtMS: 30, Kind: "step-claimed", RunID: run, Data: `{"n": 3}`},
		{AtMS: 40, Kind: "issue-done", RunID: run, StepID: review, IssueID: issue, Data: `{"n": 4}`},
		{AtMS: 50, Kind: "step-claimed", RunID: otherRun, StepID: elsewhere, Data: `{"n": 5}`},
		{AtMS: 60, Kind: "issue-done", IssueID: issue, Data: `{"n": 6}`},
	} {
		testsupport.Must(t, tx.InsertEventTx(e), "InsertEventTx %s", e.Kind)
	}
	testsupport.Must(t, tx.Commit(), "committing the seeded rows")

	wantStepEvents := []StepEvent{
		{StepID: implement, Kind: "step-claimed", Data: `{"n": 1}`},
		{StepID: review, Kind: "step-recorded", Data: `{"n": 2}`},
	}
	if got, err := st.RunStepEvents(run, "step-claimed", "step-recorded"); err != nil || !reflect.DeepEqual(got, wantStepEvents) {
		t.Errorf("RunStepEvents = (%+v, %v), want (%+v, nil)", got, err, wantStepEvents)
	}
	wantProducers := []StepProducer{
		{StepID: implement, Instance: "implement@0", Executor: "claude", IssueID: issue},
		{StepID: review, Instance: "review@0", IssueID: issue},
	}
	if got, err := st.RunStepProducers(run); err != nil || !reflect.DeepEqual(got, wantProducers) {
		t.Errorf("RunStepProducers = (%+v, %v), want (%+v, nil)", got, err, wantProducers)
	}
	if first, last, found, err := st.RunEventSpanMS(run, "step-claimed", "issue-done"); err != nil || !found || first != 10 || last != 40 {
		t.Errorf("RunEventSpanMS = (%d, %d, %v, %v), want (10, 40, true, nil)", first, last, found, err)
	}
	if first, last, found, err := st.RunEventSpanMS(otherRun, "issue-done"); err != nil || found || first != 0 || last != 0 {
		t.Errorf("RunEventSpanMS of a kind the run never logged = (%d, %d, %v, %v), want (0, 0, false, nil)",
			first, last, found, err)
	}
	wantLatest := IssueEvent{IssueID: issue, AtMS: 60, Data: `{"n": 6}`}
	if got, found, err := st.LatestIssueEvent(issue, "issue-done"); err != nil || !found || got != wantLatest {
		t.Errorf("LatestIssueEvent = (%+v, %v, %v), want (%+v, true, nil)", got, found, err, wantLatest)
	}
	if got, found, err := st.LatestIssueEvent(issue, "issue-abandoned"); err != nil || found || got != (IssueEvent{}) {
		t.Errorf("LatestIssueEvent of a kind never logged = (%+v, %v, %v), want (zero, false, nil)", got, found, err)
	}

	snap, err := st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	if got, err := snap.RunStepEventsTx(run, "step-claimed", "step-recorded"); err != nil || !reflect.DeepEqual(got, wantStepEvents) {
		t.Errorf("RunStepEventsTx = (%+v, %v), want (%+v, nil)", got, err, wantStepEvents)
	}
	wantData := []string{`{"n": 1}`, `{"n": 3}`}
	if got, err := snap.RunEventDataTx(run, "step-claimed"); err != nil || !reflect.DeepEqual(got, wantData) {
		t.Errorf("RunEventDataTx = (%q, %v), want (%q, nil)", got, err, wantData)
	}
	wantIssueEvents := []IssueEvent{
		{IssueID: issue, RunID: run, AtMS: 40, Data: `{"n": 4}`, StepRouting: "advance: approved"},
	}
	if got, err := snap.RunIssueEventsTx(run, "issue-done"); err != nil || !reflect.DeepEqual(got, wantIssueEvents) {
		t.Errorf("RunIssueEventsTx = (%+v, %v), want (%+v, nil)", got, err, wantIssueEvents)
	}
}

// TestSQLiteRunLifecycleQueries reaches the run lifecycle and repin queries
// through a Tx and a read snapshot, on a seeded run of two issues: the reads
// return only the run, issue and kinds asked for, typed by the store aliases,
// and the pin and fail-route writes report how many rows they changed.
func TestSQLiteRunLifecycleQueries(t *testing.T) {
	st := openMigratedSQLite(t)
	project, err := st.EnsureProject("/repo/lifecycle", "lifecycle", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	wf, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: project, Name: "wf", Version: 1, SourceSHA256: "sha", Body: "body", Parsed: "{}",
	}, 1)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)
	newIssue := func(title string) int {
		id, _, err := st.CreateIssueIdempotent(&model.Issue{
			ProjectID: project, Title: title, Status: model.StatusTodo,
			Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, nil, nil, "")
		testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
		return id
	}
	issue, sibling, outsider := newIssue("lifecycle subject"), newIssue("lifecycle sibling"), newIssue("not in the run")
	newRun := func() int {
		run, _, err := st.InsertRunWithContextIdempotent(project, "lifecycle", 0, 1, db.RunContext{}, "")
		testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
		testsupport.Must(t, st.AddRunIssue(run.ID, issue), "AddRunIssue")
		testsupport.Must(t, st.AddRunIssue(run.ID, sibling), "AddRunIssue")
		return run.ID
	}
	run, otherRun := newRun(), newRun()

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the seeding transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	addStep := func(runID, issueID int, name, status, workRoot string) int {
		t.Helper()
		instance := name + "@0"
		testsupport.Must(t, tx.InsertStepTx(db.StepRow{
			RunID: runID, IssueID: issueID, WorkflowID: wf.ID, StepName: name, Instance: instance,
			Kind: "executor", Status: status,
		}, 1), "InsertStepTx %s", instance)
		id, found, err := tx.StepIDByInstanceTx(runID, issueID, instance)
		testsupport.Must(t, err, "StepIDByInstanceTx: %v", err)
		if !found {
			t.Fatalf("step %s was not found after its insert", instance)
		}
		if workRoot != "" {
			testsupport.Must(t, tx.SetStepWorkRootTx(id, workRoot), "SetStepWorkRootTx %s", instance)
		}
		return id
	}
	implement := addStep(run, issue, "implement", db.StepDone, "/wt/b")
	review := addStep(run, issue, "review", db.StepRunning, "/wt/a")
	verify := addStep(run, issue, "verify", db.StepPending, "/wt/b")
	siblingStep := addStep(run, sibling, "implement", db.StepPending, "/wt/c")
	addStep(otherRun, issue, "implement", db.StepPending, "/wt/elsewhere")
	for _, e := range []NewEvent{
		{AtMS: 10, Kind: "step-claimed", RunID: run, StepID: implement, Data: `{"n": 1}`},
		{AtMS: 20, Kind: "run-paused", RunID: run, Data: `{"n": 2}`},
		{AtMS: 30, Kind: "step-recorded", RunID: run, StepID: review, Data: `{"n": 3}`},
		{AtMS: 40, Kind: "step-claimed", RunID: otherRun, Data: `{"n": 4}`},
	} {
		testsupport.Must(t, tx.InsertEventTx(e), "InsertEventTx %s", e.Kind)
	}
	testsupport.Must(t, tx.Commit(), "committing the seeded rows")

	snap, err := st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	if got, err := snap.RunWorktreesTx(run, 0); err != nil || !reflect.DeepEqual(got, []string{"/wt/a", "/wt/b", "/wt/c"}) {
		t.Errorf("RunWorktreesTx(run, 0) = (%q, %v), want ([/wt/a /wt/b /wt/c], nil)", got, err)
	}
	if got, err := snap.RunWorktreesTx(run, issue); err != nil || !reflect.DeepEqual(got, []string{"/wt/a", "/wt/b"}) {
		t.Errorf("RunWorktreesTx(run, issue) = (%q, %v), want ([/wt/a /wt/b], nil)", got, err)
	}
	if in, err := snap.RunHasIssueTx(run, issue); err != nil || !in {
		t.Errorf("RunHasIssueTx of a bound issue = (%v, %v), want (true, nil)", in, err)
	}
	if in, err := snap.RunHasIssueTx(run, outsider); err != nil || in {
		t.Errorf("RunHasIssueTx of an unbound issue = (%v, %v), want (false, nil)", in, err)
	}
	wantUnfinished := []UnfinishedStep{
		{ID: review, Instance: "review@0", Status: db.StepRunning},
		{ID: verify, Instance: "verify@0", Status: db.StepPending},
	}
	if got, err := snap.IssueUnfinishedStepsTx(run, issue); err != nil || !reflect.DeepEqual(got, wantUnfinished) {
		t.Errorf("IssueUnfinishedStepsTx = (%+v, %v), want (%+v, nil)", got, err, wantUnfinished)
	}
	if n, err := snap.RunUnfinishedStepCountTx(run); err != nil || n != 3 {
		t.Errorf("RunUnfinishedStepCountTx = (%d, %v), want (3, nil)", n, err)
	}
	wantEvents := []RunEvent{
		{Seq: 1, AtMS: 10, Kind: "step-claimed", Data: `{"n": 1}`},
		{Seq: 3, AtMS: 30, Kind: "step-recorded", Data: `{"n": 3}`},
	}
	if got, err := snap.RunEventsTx(run, "step-claimed", "step-recorded"); err != nil || !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("RunEventsTx = (%+v, %v), want (%+v, nil)", got, err, wantEvents)
	}
	if got, err := snap.RunEventsTx(run); err != nil || len(got) != 0 {
		t.Errorf("RunEventsTx naming no kind = (%+v, %v), want (none, nil)", got, err)
	}
	wantSeqs := []StepEventSeq{
		{StepID: implement, Kind: "step-claimed", Seq: 1},
		{StepID: review, Kind: "step-recorded", Seq: 3},
	}
	if got, err := snap.RunStepEventSeqsTx(run); err != nil || !reflect.DeepEqual(got, wantSeqs) {
		t.Errorf("RunStepEventSeqsTx = (%+v, %v), want (%+v, nil)", got, err, wantSeqs)
	}
	testsupport.Must(t, snap.Rollback(), "closing the read snapshot")

	tx, err = st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	pin := db.Pin{RunID: run, Kind: "contract", Ref: "contracts/implement.md", SHA256: "old",
		Content: sql.NullString{String: "body", Valid: true}}
	if n, err := tx.AddPinTx(pin); err != nil || n != 1 {
		t.Errorf("AddPinTx of a new ref = (%d, %v), want (1, nil)", n, err)
	}
	if n, err := tx.AddPinTx(db.Pin{RunID: run, Kind: pin.Kind, Ref: pin.Ref, SHA256: "other"}); err != nil || n != 0 {
		t.Errorf("AddPinTx of a ref the run already pins = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := tx.RepinPinTx(run, pin.Kind, pin.Ref, "stale", "new"); err != nil || n != 0 {
		t.Errorf("RepinPinTx from a hash the run does not hold = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := tx.RepinPinTx(run, pin.Kind, pin.Ref, "old", "new"); err != nil || n != 1 {
		t.Errorf("RepinPinTx = (%d, %v), want (1, nil)", n, err)
	}
	wantPins := []db.Pin{{RunID: run, Kind: pin.Kind, Ref: pin.Ref, SHA256: "new"}}
	if got, err := tx.ListPinsTx(run); err != nil || !reflect.DeepEqual(got, wantPins) {
		t.Errorf("ListPinsTx after the repin = (%+v, %v), want (%+v, nil): the old bytes are cleared", got, err, wantPins)
	}
	if n, err := tx.DropPinTx(run, pin.Kind, pin.Ref, "old"); err != nil || n != 0 {
		t.Errorf("DropPinTx at a hash the run no longer holds = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := tx.DropPinTx(run, pin.Kind, pin.Ref, "new"); err != nil || n != 1 {
		t.Errorf("DropPinTx = (%d, %v), want (1, nil)", n, err)
	}
	if got, err := tx.ListPinsTx(run); err != nil || len(got) != 0 {
		t.Errorf("ListPinsTx after the drop = (%+v, %v), want (none, nil)", got, err)
	}

	if n, err := tx.FailRouteIssueStepsTx(run, issue, 99); err != nil || n != 2 {
		t.Errorf("FailRouteIssueStepsTx = (%d, %v), want (2, nil)", n, err)
	}
	for id, want := range map[int]string{
		implement: db.StepDone, review: db.StepFailedRouted, verify: db.StepFailedRouted, siblingStep: db.StepPending,
	} {
		step, err := tx.GetStepTx(id)
		testsupport.Must(t, err, "GetStepTx: %v", err)
		if step.Status != want {
			t.Errorf("after FailRouteIssueStepsTx, step %d status = %q, want %q", id, step.Status, want)
		}
	}
	if n, err := tx.RunUnfinishedStepCountTx(run); err != nil || n != 1 {
		t.Errorf("RunUnfinishedStepCountTx after the fail-route = (%d, %v), want (1, nil)", n, err)
	}
}

// TestSQLiteClaimAndContextQueries reaches the claim and context queries
// through a Tx, a read snapshot and the pool, on a seeded run of two issues:
// each read returns only the step, run, issue and kind asked for, and a
// missing step or issue reports its not-found error.
func TestSQLiteClaimAndContextQueries(t *testing.T) {
	st := openMigratedSQLite(t)
	project, err := st.EnsureProject("/repo/context", "context", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	wf, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: project, Name: "wf", Version: 1, SourceSHA256: "sha", Body: "body", Parsed: "{}",
	}, 1)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)
	newIssue := func(title, description string) int {
		id, _, err := st.CreateIssueIdempotent(&model.Issue{
			ProjectID: project, Title: title, Description: description, Status: model.StatusTodo,
			Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, nil, nil, "")
		testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
		return id
	}
	issue, sibling, outsider := newIssue("context subject", "live description"),
		newIssue("context sibling", "sibling description"), newIssue("not in the run", "")
	newRun := func() int {
		run, _, err := st.InsertRunWithContextIdempotent(project, "context", 0, 1, db.RunContext{}, "")
		testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
		testsupport.Must(t, st.AddRunIssue(run.ID, issue), "AddRunIssue")
		testsupport.Must(t, st.AddRunIssue(run.ID, sibling), "AddRunIssue")
		return run.ID
	}
	run, otherRun := newRun(), newRun()

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	addStep := func(name string) int {
		t.Helper()
		instance := name + "@0"
		testsupport.Must(t, tx.InsertStepTx(db.StepRow{
			RunID: run, IssueID: issue, WorkflowID: wf.ID, StepName: name, Instance: instance,
			Kind: "executor", Status: db.StepPending,
		}, 1), "InsertStepTx %s", instance)
		id, found, err := tx.StepIDByInstanceTx(run, issue, instance)
		testsupport.Must(t, err, "StepIDByInstanceTx: %v", err)
		if !found {
			t.Fatalf("step %s was not found after its insert", instance)
		}
		return id
	}
	hashed, bare, other := addStep("implement"), addStep("review"), addStep("verify")

	before, err := tx.StepRowVersionTx(hashed)
	testsupport.Must(t, err, "StepRowVersionTx before the claim: %v", err)
	_, _, err = tx.ClaimStepWithRecoveryTx(hashed, "owner", "recovery secret", 1000, 1)
	testsupport.Must(t, err, "ClaimStepWithRecoveryTx: %v", err)
	_, _, err = tx.ClaimStepWithRecoveryTx(bare, "owner", "", 1000, 1)
	testsupport.Must(t, err, "ClaimStepWithRecoveryTx with no secret: %v", err)
	after, err := tx.StepRowVersionTx(hashed)
	step, stepErr := tx.GetStepTx(hashed)
	testsupport.Must(t, stepErr, "GetStepTx: %v", stepErr)
	if err != nil || after != step.RowVersion || after != before+1 {
		t.Errorf("StepRowVersionTx after the claim = (%d, %v), want (%d, nil): the stored version, one past %d",
			after, err, step.RowVersion, before)
	}
	if _, err := tx.StepRowVersionTx(999); !errors.Is(err, db.ErrStepNotFound) {
		t.Errorf("StepRowVersionTx of a missing step = %v, want ErrStepNotFound", err)
	}
	if hash, found, err := tx.StepRecoveryHashTx(hashed); err != nil || !found || hash != model.HashToken("recovery secret") {
		t.Errorf("StepRecoveryHashTx of a claim with a secret = (%q, %v, %v), want (its hash, true, nil)", hash, found, err)
	}
	if hash, found, err := tx.StepRecoveryHashTx(bare); err != nil || found || hash != "" {
		t.Errorf("StepRecoveryHashTx of a claim with no secret = (%q, %v, %v), want (\"\", false, nil)", hash, found, err)
	}
	if _, _, err := tx.StepRecoveryHashTx(999); !errors.Is(err, db.ErrStepNotFound) {
		t.Errorf("StepRecoveryHashTx of a missing step = %v, want ErrStepNotFound", err)
	}

	testsupport.Must(t, tx.SetRunIssueBodyTx(run, issue, "frozen body", "sha"), "SetRunIssueBodyTx")
	testsupport.Must(t, tx.SetRunIssueSnapshotTx(run, issue, `{"title":"frozen"}`), "SetRunIssueSnapshotTx")
	testsupport.Must(t, tx.SetRunIssueBodyTx(run, sibling, "sibling body", "sha"), "SetRunIssueBodyTx")
	testsupport.Must(t, tx.SetRunIssueSnapshotTx(run, sibling, `{"title":"sibling"}`), "SetRunIssueSnapshotTx")
	if body, snapshot, err := tx.RunIssueSnapshotsTx(run, issue); err != nil || body != "frozen body" || snapshot != `{"title":"frozen"}` {
		t.Errorf("RunIssueSnapshotsTx = (%q, %q, %v), want (frozen body, {\"title\":\"frozen\"}, nil)", body, snapshot, err)
	}
	if body, snapshot, err := tx.RunIssueSnapshotsTx(otherRun, issue); err != nil || body != "" || snapshot != "" {
		t.Errorf("RunIssueSnapshotsTx of an unfrozen issue = (%q, %q, %v), want (\"\", \"\", nil)", body, snapshot, err)
	}
	if _, _, err := tx.RunIssueSnapshotsTx(run, outsider); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("RunIssueSnapshotsTx of an issue outside the run = %v, want ErrNotFound", err)
	}
	if got, err := tx.IssueDescriptionTx(issue); err != nil || got != "live description" {
		t.Errorf("IssueDescriptionTx = (%q, %v), want (live description, nil)", got, err)
	}
	if _, err := tx.IssueDescriptionTx(999); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("IssueDescriptionTx of a missing issue = %v, want ErrNotFound", err)
	}

	seed := func(e NewEvent) {
		t.Helper()
		testsupport.Must(t, tx.InsertEventTx(e), "InsertEventTx %s", e.Kind)
	}
	pruned := ClaimEventsPrunedQuery{
		RunID: run, StepID: hashed, PrunedKind: "events-pruned", ClaimKind: "step-claimed", ClaimedAtMS: 500,
	}
	askPruned := func(name string, want bool) {
		t.Helper()
		if got, err := tx.ClaimEventsPrunedTx(pruned); err != nil || got != want {
			t.Errorf("ClaimEventsPrunedTx with %s = (%v, %v), want (%v, nil)", name, got, err, want)
		}
	}
	seed(NewEvent{AtMS: 400, Kind: "step-claimed", RunID: run, StepID: hashed, Data: `{}`})
	seed(NewEvent{AtMS: 600, Kind: "step-claimed", RunID: run, StepID: other, Data: `{}`})
	seed(NewEvent{AtMS: 600, Kind: "step-heartbeat", RunID: run, StepID: hashed, Data: `{}`})
	askPruned("no prune logged", false)
	seed(NewEvent{AtMS: 700, Kind: "events-pruned", RunID: otherRun, Data: `{}`})
	askPruned("only another run pruned", false)
	seed(NewEvent{AtMS: 800, Kind: "events-pruned", RunID: run, Data: `{}`})
	askPruned("a prune that took the claim record, past another kind's event", true)
	seed(NewEvent{AtMS: 500, Kind: "step-claimed", RunID: run, StepID: hashed, Data: `{}`})
	askPruned("a prune that left the claim record", false)

	for _, e := range []NewEvent{
		{AtMS: 100, Kind: "body-refreshed", RunID: run, IssueID: issue, Data: `"at the claim"`},
		{AtMS: 300, Kind: "body-refreshed", RunID: run, IssueID: issue, Data: `"later"`},
		{AtMS: 200, Kind: "body-refreshed", RunID: run, IssueID: issue, Data: `"first after"`},
		{AtMS: 200, Kind: "body-refreshed", RunID: run, IssueID: issue, Data: `"same time, logged later"`},
		{AtMS: 150, Kind: "scope-refreshed", RunID: run, IssueID: issue, Data: `"another kind"`},
		{AtMS: 150, Kind: "body-refreshed", RunID: otherRun, IssueID: issue, Data: `"another run"`},
		{AtMS: 150, Kind: "body-refreshed", RunID: run, IssueID: sibling, Data: `"another issue"`},
	} {
		seed(e)
	}
	refreshes := IssueEventAfterQuery{RunID: run, IssueID: issue, Kind: "body-refreshed", AfterMS: 100}
	if data, found, err := tx.FirstIssueEventDataAfterTx(refreshes); err != nil || !found || data != `"first after"` {
		t.Errorf("FirstIssueEventDataAfterTx = (%q, %v, %v), want (\"first after\", true, nil)", data, found, err)
	}
	testsupport.Must(t, tx.Commit(), "committing the seeded rows")

	if data, found, err := st.FirstIssueEventDataAfter(refreshes); err != nil || !found || data != `"first after"` {
		t.Errorf("FirstIssueEventDataAfter = (%q, %v, %v), want (\"first after\", true, nil)", data, found, err)
	}
	newest := refreshes
	newest.AfterMS = 300
	if data, found, err := st.FirstIssueEventDataAfter(newest); err != nil || found || data != "" {
		t.Errorf("FirstIssueEventDataAfter past the newest = (%q, %v, %v), want (\"\", false, nil)", data, found, err)
	}
	snap, err := st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	if data, found, err := snap.FirstIssueEventDataAfterTx(newest); err != nil || found || data != "" {
		t.Errorf("FirstIssueEventDataAfterTx past the newest = (%q, %v, %v), want (\"\", false, nil)", data, found, err)
	}
}
