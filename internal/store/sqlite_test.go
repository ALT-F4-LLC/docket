package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

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

// TestSQLiteRunStepClasses reads a run's step classes through the Store: each
// distinct non-null class of the run's own steps once, in class order, and
// none with a nil error for a run with no classed step.
func TestSQLiteRunStepClasses(t *testing.T) {
	st := openMigratedSQLite(t)
	project, err := st.EnsureProject("/repo/classes", "classes", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	wf, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: project, Name: "wf", Version: 1, SourceSHA256: "sha", Body: "body", Parsed: "{}",
	}, 1)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)
	issue, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: project, Title: "classes subject", Status: model.StatusTodo,
		Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, "")
	testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
	newRun := func() int {
		run, _, err := st.InsertRunWithContextIdempotent(project, "classes", 0, 1, db.RunContext{}, "")
		testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
		testsupport.Must(t, st.AddRunIssue(run.ID, issue), "AddRunIssue")
		return run.ID
	}
	run, otherRun, unclassedRun := newRun(), newRun(), newRun()

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the seeding transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	addStep := func(runID int, name, class string) {
		t.Helper()
		testsupport.Must(t, tx.InsertStepTx(db.StepRow{
			RunID: runID, IssueID: issue, WorkflowID: wf.ID, StepName: name, Instance: name + "@0",
			Kind: "executor", Class: class, Status: db.StepPending,
		}, 1), "InsertStepTx %s", name)
	}
	addStep(run, "review", "worker")
	addStep(run, "implement", "worker")
	addStep(run, "judge", "judge")
	addStep(run, "gate", "")
	addStep(otherRun, "implement", "elsewhere")
	addStep(unclassedRun, "gate", "")
	testsupport.Must(t, tx.Commit(), "committing the seeded rows")

	want := []string{"judge", "worker"}
	if got, err := st.RunStepClasses(run); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("RunStepClasses = (%q, %v), want (%q, nil): each once, no NULL, no other run's", got, err, want)
	}
	if got, err := st.RunStepClasses(unclassedRun); err != nil || len(got) != 0 {
		t.Errorf("RunStepClasses of a run with only unclassed steps = (%q, %v), want (none, nil)", got, err)
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

// TestSQLiteDispatchAndGuardQueries reaches the dispatch and guard queries
// through a Tx, a read snapshot and the pool, on runs of two projects: each
// read keeps its run, project, kind, name and status filters, the list reads
// come back in seq or id order even where timestamps or later updates run the
// other way, and a missing proposal reads as not found.
func TestSQLiteDispatchAndGuardQueries(t *testing.T) {
	st := openMigratedSQLite(t)
	type scope struct{ project, workflow, issue, sibling int }
	newScope := func(identity string) scope {
		t.Helper()
		project, err := st.EnsureProject(identity, filepath.Base(identity), 1)
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
		return scope{project: project, workflow: wf.ID, issue: newIssue("dispatch subject"), sibling: newIssue("sibling")}
	}
	home, away := newScope("/repo/dispatch"), newScope("/repo/away")
	newRun := func(s scope) int {
		t.Helper()
		run, _, err := st.InsertRunWithContextIdempotent(s.project, "dispatch", 0, 1, db.RunContext{}, "")
		testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
		testsupport.Must(t, st.AddRunIssue(run.ID, s.issue), "AddRunIssue")
		testsupport.Must(t, st.AddRunIssue(run.ID, s.sibling), "AddRunIssue")
		return run.ID
	}
	dispatched, idle, gated := newRun(home), newRun(home), newRun(home)
	done, abandoned, elsewhere := newRun(home), newRun(home), newRun(away)
	scopeOf := map[int]scope{elsewhere: away}

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	add := func(runID int, name string, ordinal int, kind, status string) int {
		t.Helper()
		s, ok := scopeOf[runID]
		if !ok {
			s = home
		}
		instance := name + "@" + strconv.Itoa(ordinal)
		testsupport.Must(t, tx.InsertStepTx(db.StepRow{
			RunID: runID, IssueID: s.issue, WorkflowID: s.workflow, StepName: name, Ordinal: ordinal,
			Instance: instance, Kind: kind, Status: status,
		}, 1), "InsertStepTx %s", instance)
		id, found, err := tx.StepIDByInstanceTx(runID, s.issue, instance)
		testsupport.Must(t, err, "StepIDByInstanceTx: %v", err)
		if !found {
			t.Fatalf("step %s was not found after its insert", instance)
		}
		return id
	}
	seed := func(e NewEvent) {
		t.Helper()
		testsupport.Must(t, tx.InsertEventTx(e), "InsertEventTx %s", e.Kind)
	}

	before, err := tx.LastEventSeqTx()
	testsupport.Must(t, err, "LastEventSeqTx before the seeded events: %v", err)
	seed(NewEvent{AtMS: 900, Kind: "dispatch-closed", RunID: dispatched, Data: `{"dispatch": "DISPATCH-1"}`})
	seed(NewEvent{AtMS: 800, Kind: "dispatch-opened", RunID: dispatched, Data: `{"dispatch": "DISPATCH-2"}`})
	seed(NewEvent{AtMS: 700, Kind: "dispatch-closed", RunID: gated, Data: `{"dispatch": "DISPATCH-3"}`})
	seed(NewEvent{AtMS: 600, Kind: "dispatch-closed", RunID: dispatched, Data: `{"dispatch": "DISPATCH-2"}`})
	seed(NewEvent{AtMS: 1, Kind: "issue-abandoned", RunID: dispatched, IssueID: home.issue, Data: `{}`})
	seed(NewEvent{AtMS: 2, Kind: "issue-abandoned", RunID: dispatched, IssueID: home.issue, Data: `{}`})
	seed(NewEvent{AtMS: 3, Kind: "issue-abandoned", RunID: dispatched, Data: `{"no": "issue"}`})
	seed(NewEvent{AtMS: 4, Kind: "issue-reopened", RunID: dispatched, IssueID: home.sibling, Data: `{}`})
	seed(NewEvent{AtMS: 5, Kind: "issue-abandoned", RunID: gated, IssueID: home.sibling, Data: `{}`})
	seed(NewEvent{AtMS: 6, Kind: "run-note-added", RunID: elsewhere, Data: `{}`})
	newest, found, err := tx.MaxEventSeqTx()
	testsupport.Must(t, err, "MaxEventSeqTx: %v", err)
	if seq, err := tx.LastEventSeqTx(); err != nil || !found || seq != newest || seq <= before {
		t.Errorf("LastEventSeqTx over every run's events = (%d, %v), want (%d, nil), past %d", seq, err, newest, before)
	}

	if got, err := tx.PriorDispatchClosesTx(dispatched, "dispatch-closed"); err != nil ||
		!reflect.DeepEqual(got, []string{`{"dispatch": "DISPATCH-1"}`, `{"dispatch": "DISPATCH-2"}`}) {
		t.Errorf("PriorDispatchClosesTx = (%q, %v), want DISPATCH-1 then DISPATCH-2 in seq order", got, err)
	}
	if got, err := tx.PriorDispatchClosesTx(idle, "dispatch-closed"); err != nil || len(got) != 0 {
		t.Errorf("PriorDispatchClosesTx of a run that never closed one = (%q, %v), want (none, nil)", got, err)
	}
	if got, err := tx.RunEventIssueIDsTx(dispatched, "issue-abandoned"); err != nil ||
		!reflect.DeepEqual(got, map[int]bool{home.issue: true}) {
		t.Errorf("RunEventIssueIDsTx = (%v, %v), want ({%d: true}, nil)", got, err, home.issue)
	}
	if got, err := tx.RunEventIssueIDsTx(idle, "issue-abandoned"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("RunEventIssueIDsTx of a run that logged none = (%v, %v), want (an empty set, nil)", got, err)
	}

	now := time.Unix(1000, 0)
	for id, status := range map[int]model.ProposalStatus{7: model.ProposalStatusOpen, 8: model.ProposalStatusCommitted} {
		_, err := tx.InsertProposalWithID(&model.Proposal{
			ID: id, ProjectID: home.project, Description: "vote", Criticality: model.CriticalityLow, Status: status,
			RequiredVoters: 1, Threshold: 0.5, CreatedAt: now, UpdatedAt: now,
		})
		testsupport.Must(t, err, "InsertProposalWithID %d: %v", id, err)
	}
	if status, found, err := tx.ProposalStatusTx(8); err != nil || !found || status != model.ProposalStatusCommitted {
		t.Errorf("ProposalStatusTx = (%q, %v, %v), want (committed, true, nil)", status, found, err)
	}

	add(dispatched, "approve", 0, "human", db.StepPending)
	tallied := add(dispatched, "approve", 1, "vote", db.StepPending)
	add(dispatched, "approve", 2, "executor", db.StepDone)
	add(dispatched, "ship", 0, "human", db.StepDone)
	add(idle, "implement", 0, "executor", db.StepPending)
	add(gated, "approve", 0, "human", db.StepWaitingHuman)
	add(done, "approve", 0, "human", db.StepDone)
	add(abandoned, "implement", 0, "executor", db.StepRunning)
	add(elsewhere, "approve", 0, "human", db.StepFailedRouted)
	for _, run := range []int{dispatched, done, elsewhere} {
		_, err := tx.InsertDispatchTx(run, 0, 5000, 1000)
		testsupport.Must(t, err, "InsertDispatchTx: %v", err)
	}
	testsupport.Must(t, tx.SetRunStatusTx(done, model.RunDone, "", 2), "SetRunStatusTx done")
	testsupport.Must(t, tx.SetRunStatusTx(abandoned, model.RunAbandoned, "", 2), "SetRunStatusTx abandoned")
	testsupport.Must(t, tx.SetRunStatusTx(elsewhere, model.RunWaitingHuman, "", 2), "SetRunStatusTx waiting-human")
	// Updated last, so a row store that rewrites updated rows lists these after
	// their later siblings unless the read orders by id.
	testsupport.Must(t, tx.SetStepRoutingTx(tallied, "pass", "tallied", db.StepDone, "", 3), "SetStepRoutingTx")
	testsupport.Must(t, tx.SetRunStatusTx(dispatched, model.RunWaitingHuman, "", 3), "SetRunStatusTx dispatched")
	testsupport.Must(t, tx.Commit(), "committing the seeded rows")

	if got, err := st.ActiveStartedRunIDs(0); err != nil || !reflect.DeepEqual(got, []int{dispatched, gated, elsewhere}) {
		t.Errorf("ActiveStartedRunIDs over every project = (%v, %v), want (%v, nil)", got, err,
			[]int{dispatched, gated, elsewhere})
	}
	if got, err := st.ActiveStartedRunIDs(home.project); err != nil || !reflect.DeepEqual(got, []int{dispatched, gated}) {
		t.Errorf("ActiveStartedRunIDs of one project = (%v, %v), want (%v, nil)", got, err, []int{dispatched, gated})
	}
	if got, err := st.ActiveStartedRunIDs(away.project + 1); err != nil || len(got) != 0 {
		t.Errorf("ActiveStartedRunIDs of a project with no runs = (%v, %v), want (none, nil)", got, err)
	}

	routed, err := st.GetStep(tallied)
	testsupport.Must(t, err, "GetStep: %v", err)
	if routed.Routing == "" {
		t.Fatalf("the routed vote step stored no routing record, so the routing column went unchecked")
	}
	kinds :=[]string{"human", "vote"}
	inRun := []GateStepState{{Status: db.StepPending}, {Status: db.StepDone, Routing: routed.Routing}}
	cases := []struct {
		name string
		q    GateStepQuery
		want []GateStepState
	}{
		{"every project", GateStepQuery{StepName: "approve", Kinds: kinds},
			append(append([]GateStepState{}, inRun...), GateStepState{Status: db.StepWaitingHuman},
				GateStepState{Status: db.StepFailedRouted})},
		{"one project", GateStepQuery{StepName: "approve", Kinds: kinds, ProjectID: home.project},
			append(append([]GateStepState{}, inRun...), GateStepState{Status: db.StepWaitingHuman})},
		{"a named run outside the project", GateStepQuery{StepName: "approve", Kinds: kinds,
			RunID: elsewhere, ProjectID: home.project}, []GateStepState{{Status: db.StepFailedRouted}}},
		{"one kind", GateStepQuery{StepName: "approve", Kinds: []string{"vote"}, RunID: dispatched}, inRun[1:]},
		{"an ended run", GateStepQuery{StepName: "approve", Kinds: kinds, RunID: done}, nil},
		{"no kinds", GateStepQuery{StepName: "approve", RunID: dispatched}, nil},
	}
	for _, c := range cases {
		if got, err := st.ActiveGateSteps(c.q); err != nil || len(got) != len(c.want) || (len(got) > 0 && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("ActiveGateSteps for %s = (%+v, %v), want (%+v, nil)", c.name, got, err, c.want)
		}
	}

	snap, err := st.BeginReadSnapshot()
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

// TestSQLiteHoldAndActivationQueries reaches the hold, activation and
// definition queries through the pool, a Tx and a read snapshot on seeded
// rows: each read keeps its run, issue, project and kind filters, list reads
// come back in instance, id or name-then-version order where the rows were
// written the other way, and missing rows read as not found.
func TestSQLiteHoldAndActivationQueries(t *testing.T) {
	st := openMigratedSQLite(t)
	home, err := st.EnsureProject("/repo/hold", "hold", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	away, err := st.EnsureProject("/repo/away", "away", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	workflow := func(project int, name string, version int) int {
		t.Helper()
		wf, _, err := st.InsertWorkflow(&model.Workflow{
			ProjectID: project, Name: name, Version: version, SourceSHA256: "sha", Body: "body",
			Parsed: `{"name": "` + name + `@` + strconv.Itoa(version) + `"}`,
		}, 1)
		testsupport.Must(t, err, "InsertWorkflow %s@%d: %v", name, version, err)
		return wf.ID
	}
	beta1, alpha1, alpha2 := workflow(home, "beta", 1), workflow(home, "alpha", 1), workflow(home, "alpha", 2)
	elsewhereWF := workflow(away, "gamma", 1)
	newIssue := func(title string) int {
		t.Helper()
		id, _, err := st.CreateIssueIdempotent(&model.Issue{
			ProjectID: home, Title: title, Status: model.StatusTodo,
			Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, nil, nil, "")
		testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
		return id
	}
	issue, sibling := newIssue("held"), newIssue("sibling")
	newRun := func() int {
		t.Helper()
		run, _, err := st.InsertRunWithContextIdempotent(home, "hold", 0, 1, db.RunContext{}, "")
		testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
		testsupport.Must(t, st.AddRunIssue(run.ID, issue), "AddRunIssue")
		testsupport.Must(t, st.AddRunIssue(run.ID, sibling), "AddRunIssue")
		return run.ID
	}
	run, otherRun, emptyRun := newRun(), newRun(), newRun()

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	add := func(runID, issueID, workflowID int, instance, name string, ordinal int, cost float64) int {
		t.Helper()
		testsupport.Must(t, tx.InsertStepTx(db.StepRow{
			RunID: runID, IssueID: issueID, WorkflowID: workflowID, StepName: name, Ordinal: ordinal,
			Instance: instance, Kind: "executor", Status: db.StepPending, ExpectedCost: cost,
		}, 1), "InsertStepTx %s", instance)
		id, found, err := tx.StepIDByInstanceTx(runID, issueID, instance)
		testsupport.Must(t, err, "StepIDByInstanceTx: %v", err)
		if !found {
			t.Fatalf("step %s was not found after its insert", instance)
		}
		return id
	}
	review := add(run, issue, alpha2, "review@1", "review", 1, 0.5)
	add(run, issue, alpha2, "review@1#2", "review", 1, 0.25)
	add(run, issue, beta1, "hold#2", "hold", 2, 1)
	routedCluster := add(run, issue, beta1, "hold#1", "hold", 2, 0)
	add(run, issue, beta1, "hold", "hold", 2, 2)
	add(run, issue, beta1, "holdout", "holdout", 2, 0)
	add(run, sibling, alpha1, "hold#1", "hold", 2, 0)
	add(otherRun, issue, elsewhereWF, "hold", "hold", 2, 8)
	add(otherRun, issue, elsewhereWF, "review@1", "review", 1, 0)
	testsupport.Must(t, tx.SetStepRoutingTx(routedCluster, "pass", "held", db.StepDone, "", 2), "SetStepRoutingTx")
	testsupport.Must(t, tx.BindRunIssueTx(&db.RunIssue{RunID: run, IssueID: issue, WorkflowID: &alpha2}), "BindRunIssueTx")
	testsupport.Must(t, tx.BindRunIssueTx(&db.RunIssue{RunID: run, IssueID: sibling, WorkflowID: &beta1}), "BindRunIssueTx")
	testsupport.Must(t, tx.BindRunIssueTx(&db.RunIssue{RunID: otherRun, IssueID: issue, WorkflowID: &elsewhereWF}),
		"BindRunIssueTx")
	for _, a := range []db.Artifact{
		{RunID: run, StepID: review, Kind: "change-summary", Body: "b", Payload: `{"n": 1}`, SHA256: "s1"},
		{RunID: run, StepID: review, Kind: "change-summary", Body: "b", Payload: `{"n": 2}`, SHA256: "s2"},
		{RunID: run, StepID: review, Kind: "gate-log", Body: "b", SHA256: "s3"},
	} {
		_, err := tx.InsertArtifactTx(a, 1)
		testsupport.Must(t, err, "InsertArtifactTx %s: %v", a.Kind, err)
	}

	routed, err := tx.GetStepTx(routedCluster)
	testsupport.Must(t, err, "GetStepTx: %v", err)
	if routed.Routing == "" {
		t.Fatalf("the routed cluster row stored no routing record, so the routing column went unchecked")
	}
	cluster := []InstanceStatusRouting{
		{Instance: "hold#1", Status: db.StepDone, Routing: routed.Routing},
		{Instance: "hold#2", Status: db.StepPending},
	}
	if got, err := tx.HoldClusterInstancesTx(run, issue, "hold"); err != nil || !reflect.DeepEqual(got, cluster) {
		t.Errorf("HoldClusterInstancesTx = (%+v, %v), want (%+v, nil)", got, err, cluster)
	}

	art, found, err := tx.NewestStepArtifactExceptKindsTx(review, []string{"gate-log"})
	if err != nil || !found || art.Kind != "change-summary" || art.Payload != `{"n": 2}` {
		t.Errorf("NewestStepArtifactExceptKindsTx past gate-log = (%+v, %v, %v), want the newest change-summary",
			art, found, err)
	}
	if art, found, err := tx.NewestStepArtifactExceptKindsTx(review, nil); err != nil || !found ||
		art.Kind != "gate-log" || art.Payload != "" {
		t.Errorf("NewestStepArtifactExceptKindsTx with no exceptions = (%+v, %v, %v), want the gate-log with no payload",
			art, found, err)
	}
	if art, found, err := tx.NewestStepArtifactExceptKindsTx(routedCluster, nil); err != nil || found || art.ID != 0 {
		t.Errorf("NewestStepArtifactExceptKindsTx of a step with none = (%+v, %v, %v), want (zero, false, nil)",
			art, found, err)
	}

	if cost, err := tx.RunExpectedCostTx(run); err != nil || cost != 3.75 {
		t.Errorf("RunExpectedCostTx = (%v, %v), want (3.75, nil)", cost, err)
	}
	if cost, err := tx.RunExpectedCostTx(emptyRun); err != nil || cost != 0 {
		t.Errorf("RunExpectedCostTx of a run with no steps = (%v, %v), want (0, nil)", cost, err)
	}

	testsupport.Must(t, tx.RepinStepWorkRootTx(review, "/wt/repinned"), "RepinStepWorkRootTx")
	if step, err := tx.GetStepTx(review); err != nil || step.WorkRoot != "/wt/repinned" {
		t.Errorf("work root after RepinStepWorkRootTx = (%+v, %v), want /wt/repinned", step, err)
	}
	if err := tx.RepinStepWorkRootTx(999999, "/wt"); !errors.Is(err, ErrStepNotFound) {
		t.Errorf("RepinStepWorkRootTx of a missing step: err = %v, want ErrStepNotFound", err)
	}

	stepWorkflows := []ParsedWorkflow{
		{ID: beta1, Parsed: `{"name": "beta@1"}`}, {ID: alpha1, Parsed: `{"name": "alpha@1"}`},
		{ID: alpha2, Parsed: `{"name": "alpha@2"}`},
	}
	if got, err := tx.RunStepWorkflowsTx(run); err != nil || !reflect.DeepEqual(got, stepWorkflows) {
		t.Errorf("RunStepWorkflowsTx = (%+v, %v), want (%+v, nil)", got, err, stepWorkflows)
	}
	testsupport.Must(t, tx.Commit(), "committing the seeded rows")

	whole := append([]InstanceStatusRouting{{Instance: "hold", Status: db.StepPending}}, cluster...)
	if got, err := st.HoldInstances(run, issue, "hold"); err != nil || !reflect.DeepEqual(got, whole) {
		t.Errorf("HoldInstances = (%+v, %v), want (%+v, nil)", got, err, whole)
	}
	if got, err := st.HoldInstances(emptyRun, issue, "hold"); err != nil || len(got) != 0 {
		t.Errorf("HoldInstances of a run with no hold = (%+v, %v), want (none, nil)", got, err)
	}
	if id, found, err := st.StepIDAtOrdinal(run, issue, "review", 1); err != nil || !found || id != review {
		t.Errorf("StepIDAtOrdinal = (%d, %v, %v), want (%d, true, nil): the lowest id", id, found, err, review)
	}
	if id, found, err := st.StepIDAtOrdinal(run, sibling, "review", 1); err != nil || found || id != 0 {
		t.Errorf("StepIDAtOrdinal of an issue with none = (%d, %v, %v), want (0, false, nil)", id, found, err)
	}
	if got, err := st.RunStepWorkflows(run); err != nil || !reflect.DeepEqual(got, stepWorkflows) {
		t.Errorf("RunStepWorkflows = (%+v, %v), want (%+v, nil)", got, err, stepWorkflows)
	}
	bound := []ParsedWorkflow{{ID: beta1, Parsed: `{"name": "beta@1"}`}, {ID: alpha2, Parsed: `{"name": "alpha@2"}`}}
	if got, err := st.RunBoundWorkflows(run); err != nil || !reflect.DeepEqual(got, bound) {
		t.Errorf("RunBoundWorkflows = (%+v, %v), want (%+v, nil)", got, err, bound)
	}
	if got, err := st.RunBoundWorkflows(emptyRun); err != nil || len(got) != 0 {
		t.Errorf("RunBoundWorkflows of a run with no bound issue = (%+v, %v), want (none, nil)", got, err)
	}

	snap, err := st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	if identity, err := snap.ProjectIdentityTx(home); err != nil || identity != "/repo/hold" {
		t.Errorf("ProjectIdentityTx = (%q, %v), want (/repo/hold, nil)", identity, err)
	}
	def, err := st.GetProject(DefaultProjectID)
	testsupport.Must(t, err, "GetProject default: %v", err)
	if identity, err := snap.ProjectIdentityTx(0); err != nil || identity != def.Identity {
		t.Errorf("ProjectIdentityTx(0) = (%q, %v), want the default project's (%q, nil)", identity, err, def.Identity)
	}
	if _, err := snap.ProjectIdentityTx(away + 100); !errors.Is(err, ErrNotFound) {
		t.Errorf("ProjectIdentityTx of a missing project: err = %v, want ErrNotFound", err)
	}
	wfs, err := snap.ProjectWorkflowsTx(home)
	testsupport.Must(t, err, "ProjectWorkflowsTx: %v", err)
	var listed []string
	for _, wf := range wfs {
		listed = append(listed, wf.Name+"@"+strconv.Itoa(wf.Version))
	}
	if want := []string{"alpha@2", "alpha@1", "beta@1"}; !reflect.DeepEqual(listed, want) {
		t.Errorf("ProjectWorkflowsTx = %v, want %v: by name, newest version first, this project only", listed, want)
	}
	if got, err := snap.HoldClusterInstancesTx(run, sibling, "hold"); err != nil || len(got) != 1 || got[0].Instance != "hold#1" {
		t.Errorf("HoldClusterInstancesTx of the sibling through a snapshot = (%+v, %v), want its one cluster row", got, err)
	}
}

// TestSQLiteReapUsageAndFenceQueries reaches the reap, vote-usage and fence
// reads through a Tx and then a read snapshot on seeded rows: each keeps its
// run, step, kind, dispatch, proposal and seat filters, fences come back by
// issue, tag and ordinal where they were written the other way, a missing
// event reads as ErrNotFound, and a missing cast as not found.
func TestSQLiteReapUsageAndFenceQueries(t *testing.T) {
	st := openMigratedSQLite(t)
	project, err := st.EnsureProject("/repo/reap", "reap", 1)
	testsupport.Must(t, err, "EnsureProject: %v", err)
	wf, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: project, Name: "wf", Version: 1, SourceSHA256: "sha", Body: "body", Parsed: "{}",
	}, 1)
	testsupport.Must(t, err, "InsertWorkflow: %v", err)
	newIssue := func(title string) int {
		t.Helper()
		id, _, err := st.CreateIssueIdempotent(&model.Issue{
			ProjectID: project, Title: title, Status: model.StatusTodo,
			Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, nil, nil, "")
		testsupport.Must(t, err, "CreateIssueIdempotent: %v", err)
		return id
	}
	issue, sibling := newIssue("reaped"), newIssue("sibling")
	newRun := func() int {
		t.Helper()
		run, _, err := st.InsertRunWithContextIdempotent(project, "reap", 0, 1, db.RunContext{}, "")
		testsupport.Must(t, err, "InsertRunWithContextIdempotent: %v", err)
		testsupport.Must(t, st.AddRunIssue(run.ID, issue), "AddRunIssue")
		testsupport.Must(t, st.AddRunIssue(run.ID, sibling), "AddRunIssue")
		return run.ID
	}
	run, otherRun := newRun(), newRun()

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
	t.Cleanup(func() { tx.Rollback() })
	add := func(name string) int {
		t.Helper()
		instance := name + "@0"
		testsupport.Must(t, tx.InsertStepTx(db.StepRow{
			RunID: run, IssueID: issue, WorkflowID: wf.ID, StepName: name, Instance: instance,
			Kind: "executor", Status: db.StepClaimed,
		}, 1), "InsertStepTx %s", instance)
		id, found, err := tx.StepIDByInstanceTx(run, issue, instance)
		testsupport.Must(t, err, "StepIDByInstanceTx: %v", err)
		if !found {
			t.Fatalf("step %s was not found after its insert", instance)
		}
		return id
	}
	seed := func(e NewEvent) int64 {
		t.Helper()
		testsupport.Must(t, tx.InsertEventTx(e), "InsertEventTx %s", e.Kind)
		seq, _, err := tx.MaxEventSeqTx()
		testsupport.Must(t, err, "MaxEventSeqTx: %v", err)
		return seq
	}
	admitted, early, offManifest, unclaimed := add("implement"), add("review"), add("verify"), add("fix")
	seed(NewEvent{AtMS: 1, Kind: "step-claimed", RunID: run, StepID: early, Data: `{}`})
	opened := seed(NewEvent{AtMS: 1, Kind: "dispatch-opened", RunID: run, Data: `{}`})
	claim := seed(NewEvent{AtMS: 1, Kind: "step-claimed", RunID: run, StepID: admitted, Data: `{}`})
	seed(NewEvent{AtMS: 1, Kind: "step-claimed", RunID: run, StepID: offManifest, Data: `{}`})
	reaped := seed(NewEvent{AtMS: 1, Kind: "lease-reaped", RunID: run, StepID: admitted, Data: `{"forced": true}`})
	dispatchID, err := tx.InsertDispatchTx(run, opened, 99999, 1000)
	testsupport.Must(t, err, "InsertDispatchTx: %v", err)
	for i, stepID := range []int{admitted, early, unclaimed} {
		testsupport.Must(t, tx.InsertDispatchRowTx(dispatchID, DispatchRow{
			Position: i, StepID: stepID, Instance: "row", RowJSON: `{}`, RowSHA256: "sha",
		}), "InsertDispatchRowTx %d", i)
	}
	seed(NewEvent{AtMS: 1, Kind: "vote-opened", RunID: run, Data: `{"detail": "DKT-V7"}`})
	seed(NewEvent{AtMS: 1, Kind: "vote-tallied", RunID: run, Data: `{"detail": "DKT-V8"}`})
	seed(NewEvent{AtMS: 1, Kind: "vote-opened", RunID: run, StepID: early, Data: `{"detail": "DKT-V9"}`})
	newest := seed(NewEvent{AtMS: 1, Kind: "vote-opened", RunID: otherRun, Data: `{"detail": "DKT-V10"}`})

	now := time.Unix(1000, 0)
	for _, id := range []int{7, 8} {
		_, err := tx.InsertProposalWithID(&model.Proposal{
			ID: id, ProjectID: project, Description: "vote", Criticality: model.CriticalityLow,
			Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.5, CreatedAt: now, UpdatedAt: now,
		})
		testsupport.Must(t, err, "InsertProposalWithID %d: %v", id, err)
	}
	for _, v := range []struct {
		id, proposal int
		voter        string
	}{{70, 7, "seat-a"}, {71, 7, "seat-b"}, {80, 8, "seat-c"}} {
		_, err := tx.InsertVoteWithID(&model.Vote{
			ID: v.id, ProposalID: v.proposal, VoterName: v.voter,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.9, CreatedAt: now,
		})
		testsupport.Must(t, err, "InsertVoteWithID %d: %v", v.id, err)
	}

	for _, fence := range []RunFence{
		{RunID: run, IssueID: sibling, Tag: "ac", Ordinal: 0, Command: "make c", SHA256: "c"},
		{RunID: run, IssueID: issue, Tag: "tests", Ordinal: 1, Command: "make b2", SHA256: "b2"},
		{RunID: run, IssueID: issue, Tag: "ac", Ordinal: 0, Command: "make a", SHA256: "a"},
		{RunID: run, IssueID: issue, Tag: "tests", Ordinal: 0, Command: "make b1", SHA256: "b1"},
		{RunID: otherRun, IssueID: issue, Tag: "ac", Ordinal: 0, Command: "make z", SHA256: "z"},
	} {
		testsupport.Must(t, tx.InsertFenceTx(fence), "InsertFenceTx %s#%d", fence.Tag, fence.Ordinal)
	}
	fences := []RunFence{
		{RunID: run, IssueID: issue, Tag: "ac", Ordinal: 0, Command: "make a", SHA256: "a"},
		{RunID: run, IssueID: issue, Tag: "tests", Ordinal: 0, Command: "make b1", SHA256: "b1"},
		{RunID: run, IssueID: issue, Tag: "tests", Ordinal: 1, Command: "make b2", SHA256: "b2"},
		{RunID: run, IssueID: sibling, Tag: "ac", Ordinal: 0, Command: "make c", SHA256: "c"},
	}

	check := func(via string, r ReadTx) {
		t.Helper()
		if data, err := r.EventDataTx(reaped); err != nil || data != `{"forced": true}` {
			t.Errorf("EventDataTx of the reap through %s = (%q, %v), want its payload", via, data, err)
		}
		if data, err := r.EventDataTx(newest + 1); !errors.Is(err, ErrNotFound) || data != "" {
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

	check("a Tx", tx)
	testsupport.Must(t, tx.Commit(), "committing the seeded rows")
	snap, err := st.BeginReadSnapshot()
	testsupport.Must(t, err, "opening the read snapshot: %v", err)
	t.Cleanup(func() { snap.Rollback() })
	check("a read snapshot", snap)
}

// TestSQLiteImportExportQueries reaches the import id probes and the label
// lookup through a Tx and then a read snapshot, on one row per probed table
// in each of two projects, the higher id written first: existence and the id
// ceiling span projects, ownership follows a child row to its parent's
// project, a table outside the closed set is refused, and a label name
// resolves only in its own project. It then restores an issue's parent
// through the Tx and requires its version, updated_at and activity log to
// stay as imported.
func TestSQLiteImportExportQueries(t *testing.T) {
	st := openMigratedSQLite(t)
	owner, err := st.EnsureProject("/repo/owner", "owner", 1)
	testsupport.Must(t, err, "EnsureProject owner: %v", err)
	other, err := st.EnsureProject("/repo/other", "other", 1)
	testsupport.Must(t, err, "EnsureProject other: %v", err)

	tx, err := st.Begin()
	testsupport.Must(t, err, "opening the write transaction: %v", err)
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
		table          ImportTable
		owned, foreign int
	}{
		{ImportIssues, 10, 20},
		{ImportLabels, 131, 132},
		{ImportComments, 31, 32},
		{ImportIssueRelations, 41, 42},
		{ImportActivityLog, 51, 52},
		{ImportProposals, 60, 70},
		{ImportVotes, 81, 82},
		{ImportDocs, 90, 100},
		{ImportDocRevisions, 111, 112},
		{ImportDocComments, 121, 122},
	}

	check := func(via string, r ReadTx) {
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
		const unlisted = ImportTable("projects")
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
