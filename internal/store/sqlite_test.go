package store

import (
	"errors"
	"path/filepath"
	"testing"

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
