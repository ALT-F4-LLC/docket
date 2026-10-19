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

// TestEventAndBudgetReadsPassAHeldWriteLock pins that the event feed, the
// per-actor rollup, the activation-head read, and the run-budget read open as
// readers: while another process holds the write lock, each returns what it
// returned before the lock instead of queueing behind it until busy_timeout.
func TestEventAndBudgetReadsPassAHeldWriteLock(t *testing.T) {
	conn := mustDB(t)
	runID := activatedVoteGateRun(t, conn)

	wantEvents, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents before the lock: %v", err)
	if len(wantEvents.Events) == 0 {
		t.Fatal("ListEvents before the lock returned no events; the scenario needs the activation trail")
	}
	wantActors, err := ActorCounts(conn, runID)
	testsupport.Must(t, err, "ActorCounts before the lock: %v", err)
	wantHead, err := LatestActivationHead(conn, runID)
	testsupport.Must(t, err, "LatestActivationHead before the lock: %v", err)
	wantBudget, err := GetRunBudget(conn, runID)
	testsupport.Must(t, err, "GetRunBudget before the lock: %v", err)

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

	t.Run("list events", func(t *testing.T) {
		got, err := ListEvents(conn, EventQuery{RunID: runID})
		if err != nil {
			t.Fatalf("ListEvents while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantEvents) {
			t.Errorf("ListEvents under the lock = %s, want %s",
				mustJSON(t, got), mustJSON(t, wantEvents))
		}
	})
	t.Run("actor counts", func(t *testing.T) {
		got, err := ActorCounts(conn, runID)
		if err != nil {
			t.Fatalf("ActorCounts while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantActors) {
			t.Errorf("ActorCounts under the lock = %v, want %v", got, wantActors)
		}
	})
	t.Run("latest activation head", func(t *testing.T) {
		got, err := LatestActivationHead(conn, runID)
		if err != nil {
			t.Fatalf("LatestActivationHead while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantHead) {
			t.Errorf("LatestActivationHead under the lock = %s, want %s",
				mustJSON(t, got), mustJSON(t, wantHead))
		}
	})
	t.Run("run budget", func(t *testing.T) {
		got, err := GetRunBudget(conn, runID)
		if err != nil {
			t.Fatalf("GetRunBudget while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantBudget) {
			t.Errorf("GetRunBudget under the lock = %s, want %s",
				mustJSON(t, got), mustJSON(t, wantBudget))
		}
	})
}

// TestGuardAndPreGateReadsPassAHeldWriteLock pins that the guard's rolled-back
// reads — the stop blockers, the reconciliation probe, the stored manifest,
// and the open reap hold — and the detached pre-gate's candidate and
// still-wanted reads open as readers: while another process holds the write
// lock, each returns what it returned before the lock instead of queueing
// behind it until busy_timeout.
func TestGuardAndPreGateReadsPassAHeldWriteLock(t *testing.T) {
	conn, e, run, repoRoot := preGatedRun(t, []string{"/usr/bin/true"})
	stepID, head := advanceToVerifyWithCommit(t, conn, e, repoRoot)
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	defs, err := StepDefinitions(conn, run.ID)
	testsupport.Must(t, err, "StepDefinitions: %v", err)

	wantBlockers, err := stopBlockers(conn, run.ID, nowMS)
	testsupport.Must(t, err, "stopBlockers before the lock: %v", err)
	wantReason, err := unreconciledReason(conn, run.ID, nowMS)
	testsupport.Must(t, err, "unreconciledReason before the lock: %v", err)
	wantRows, err := storedManifestRows(conn, run.ID)
	testsupport.Must(t, err, "storedManifestRows before the lock: %v", err)
	wantHold, err := openReapHold(conn, run.ID)
	testsupport.Must(t, err, "openReapHold before the lock: %v", err)
	wantCandidates, err := detachedPreGateCandidates(conn, run.ID, nowMS)
	testsupport.Must(t, err, "detachedPreGateCandidates before the lock: %v", err)
	if len(wantCandidates) == 0 {
		t.Fatal("detachedPreGateCandidates before the lock found none; the scenario needs verify@0 pending at a target")
	}
	wantDeclined, err := stepStillWantsDetachedPreGate(conn, step, defs, head, nowMS)
	testsupport.Must(t, err, "stepStillWantsDetachedPreGate before the lock: %v", err)

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

	t.Run("stop blockers", func(t *testing.T) {
		got, err := stopBlockers(conn, run.ID, nowMS)
		if err != nil {
			t.Fatalf("stopBlockers while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantBlockers) {
			t.Errorf("stopBlockers under the lock = %v, want %v", got, wantBlockers)
		}
	})
	t.Run("unreconciled reason", func(t *testing.T) {
		got, err := unreconciledReason(conn, run.ID, nowMS)
		if err != nil {
			t.Fatalf("unreconciledReason while another process holds the write lock: %v", err)
		}
		if got != wantReason {
			t.Errorf("unreconciledReason under the lock = %q, want %q", got, wantReason)
		}
	})
	t.Run("stored manifest rows", func(t *testing.T) {
		got, err := storedManifestRows(conn, run.ID)
		if err != nil {
			t.Fatalf("storedManifestRows while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantRows) {
			t.Errorf("storedManifestRows under the lock = %s, want %s",
				mustJSON(t, got), mustJSON(t, wantRows))
		}
	})
	t.Run("open reap hold", func(t *testing.T) {
		got, err := openReapHold(conn, run.ID)
		if err != nil {
			t.Fatalf("openReapHold while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantHold) {
			t.Errorf("openReapHold under the lock = %s, want %s",
				mustJSON(t, got), mustJSON(t, wantHold))
		}
	})
	t.Run("detached pre-gate candidates", func(t *testing.T) {
		got, err := detachedPreGateCandidates(conn, run.ID, nowMS)
		if err != nil {
			t.Fatalf("detachedPreGateCandidates while another process holds the write lock: %v", err)
		}
		if !reflect.DeepEqual(got, wantCandidates) {
			t.Errorf("detachedPreGateCandidates under the lock = %+v, want %+v",
				got, wantCandidates)
		}
	})
	t.Run("step still wants detached pre-gate", func(t *testing.T) {
		got, err := stepStillWantsDetachedPreGate(conn, step, defs, head, nowMS)
		if err != nil {
			t.Fatalf("stepStillWantsDetachedPreGate while another process holds the write lock: %v", err)
		}
		if got != wantDeclined {
			t.Errorf("stepStillWantsDetachedPreGate under the lock = %q, want %q",
				got, wantDeclined)
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
