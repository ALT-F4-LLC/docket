package engine

import (
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestRolledBackReadsPassAHeldWriteLock pins that the step context read, the
// step view read, the post-resolution ready-set read, and the aggregate input
// read load their scheduler snapshot as readers: while another process holds
// the write lock, each returns without waiting out busy_timeout. A snapshot
// taken under BEGIN IMMEDIATE would queue behind that lock and fail with
// SQLITE_BUSY.
func TestRolledBackReadsPassAHeldWriteLock(t *testing.T) {
	conn := mustDB(t)
	runID := activatedVoteGateRun(t, conn)
	seedID := stepIDByInstance(t, conn, "seed@0")
	seed, err := db.GetStep(conn, seedID)
	testsupport.Must(t, err, "GetStep: %v", err)
	defs, err := StepDefinitions(conn, runID)
	testsupport.Must(t, err, "StepDefinitions: %v", err)

	wantContext, err := ReadContext(conn, seedID, nowMS)
	testsupport.Must(t, err, "ReadContext before the lock: %v", err)
	wantView, err := LoadStepView(conn, seedID, nowMS)
	testsupport.Must(t, err, "LoadStepView before the lock: %v", err)
	wantCandidates, err := readyStaleTargetCandidates(conn, runID, defs, nowMS)
	testsupport.Must(t, err, "readyStaleTargetCandidates before the lock: %v", err)
	wantPayloads, err := inputPayloads(conn, seed, nowMS)
	testsupport.Must(t, err, "inputPayloads before the lock: %v", err)

	var path string
	err = conn.QueryRow("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path)
	testsupport.Must(t, err, "reading the database path: %v", err)
	writer, err := db.Open(path)
	testsupport.Must(t, err, "opening the writer's handle: %v", err)
	t.Cleanup(func() { writer.Close() })
	lock, err := writer.Begin()
	testsupport.Must(t, err, "taking the write lock: %v", err)
	defer lock.Rollback()

	_, err = conn.Exec("PRAGMA busy_timeout=50")
	testsupport.Must(t, err, "shortening busy_timeout: %v", err)

	t.Run("claim context", func(t *testing.T) {
		got, err := ReadContext(conn, seedID, nowMS)
		if err != nil {
			t.Fatalf("ReadContext while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantContext) {
			t.Errorf("ReadContext under the lock = %s, want %s",
				mustJSON(t, got), mustJSON(t, wantContext))
		}
	})
	t.Run("claim step", func(t *testing.T) {
		got, err := LoadStepView(conn, seedID, nowMS)
		if err != nil {
			t.Fatalf("LoadStepView while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantView) {
			t.Errorf("LoadStepView under the lock = %s, want %s",
				mustJSON(t, got), mustJSON(t, wantView))
		}
	})
	t.Run("ready stale target candidates", func(t *testing.T) {
		got, err := readyStaleTargetCandidates(conn, runID, defs, nowMS)
		if err != nil {
			t.Fatalf("readyStaleTargetCandidates while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantCandidates) {
			t.Errorf("readyStaleTargetCandidates under the lock = %v, want %v",
				got, wantCandidates)
		}
	})
	t.Run("input payloads", func(t *testing.T) {
		got, err := inputPayloads(conn, seed, nowMS)
		if err != nil {
			t.Fatalf("inputPayloads while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantPayloads) {
			t.Errorf("inputPayloads under the lock = %v, want %v", got, wantPayloads)
		}
	})
}

// TestDispatchReadsPassAHeldWriteLock pins that the dispatch verbs' rolled-back
// reads — the ready snapshot, the verify recomputation, the open-dispatch id
// lookup, and the integration candidate read — open as readers: while another
// process holds the write lock, each returns what it returned before the lock
// instead of queueing behind it until busy_timeout.
func TestDispatchReadsPassAHeldWriteLock(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	manifest := openDispatch(t, conn, runID, 0, nowMS)
	defs, err := StepDefinitions(conn, runID)
	testsupport.Must(t, err, "StepDefinitions: %v", err)
	e := NewEngine()

	wantSched, err := readySnapshot(conn, runID, defs, nowMS)
	testsupport.Must(t, err, "readySnapshot before the lock: %v", err)
	wantVerify, wantMismatch, _, err := e.verifyDispatchTx(conn, runID, nowMS)
	testsupport.Must(t, err, "verifyDispatchTx before the lock: %v", err)
	wantCheck, wantUnintegrated, err := e.integrationVerdict(conn, runID, defs, "vouched", nowMS)
	testsupport.Must(t, err, "integrationVerdict before the lock: %v", err)

	var path string
	err = conn.QueryRow("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path)
	testsupport.Must(t, err, "reading the database path: %v", err)
	writer, err := db.Open(path)
	testsupport.Must(t, err, "opening the writer's handle: %v", err)
	t.Cleanup(func() { writer.Close() })
	lock, err := writer.Begin()
	testsupport.Must(t, err, "taking the write lock: %v", err)
	defer lock.Rollback()

	_, err = conn.Exec("PRAGMA busy_timeout=50")
	testsupport.Must(t, err, "shortening busy_timeout: %v", err)

	t.Run("ready snapshot", func(t *testing.T) {
		got, err := readySnapshot(conn, runID, defs, nowMS)
		if err != nil {
			t.Fatalf("readySnapshot while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got.Steps(), wantSched.Steps()) {
			t.Errorf("readySnapshot steps under the lock = %s, want %s",
				mustJSON(t, got.Steps()), mustJSON(t, wantSched.Steps()))
		}
	})
	t.Run("verify dispatch", func(t *testing.T) {
		got, mismatch, _, err := e.verifyDispatchTx(conn, runID, nowMS)
		if err != nil {
			t.Fatalf("verifyDispatchTx while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantVerify) || !reflect.DeepEqual(mismatch, wantMismatch) {
			t.Errorf("verifyDispatchTx under the lock = %s / %s, want %s / %s",
				mustJSON(t, got), mustJSON(t, mismatch),
				mustJSON(t, wantVerify), mustJSON(t, wantMismatch))
		}
	})
	t.Run("open dispatch id", func(t *testing.T) {
		got, ok := openDispatchID(conn, runID)
		if !ok || FormatDispatchID(got) != manifest.Dispatch {
			t.Errorf("openDispatchID under the lock = %d, %t, want %s, true",
				got, ok, manifest.Dispatch)
		}
	})
	t.Run("integration verdict", func(t *testing.T) {
		got, unintegrated, err := e.integrationVerdict(conn, runID, defs, "vouched", nowMS)
		if err != nil {
			t.Fatalf("integrationVerdict while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantCheck) || !reflect.DeepEqual(unintegrated, wantUnintegrated) {
			t.Errorf("integrationVerdict under the lock = %s / %v, want %s / %v",
				mustJSON(t, got), unintegrated, mustJSON(t, wantCheck), wantUnintegrated)
		}
	})
}
