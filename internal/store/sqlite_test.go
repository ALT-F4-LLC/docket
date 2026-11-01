package store

import (
	"path/filepath"
	"testing"

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
