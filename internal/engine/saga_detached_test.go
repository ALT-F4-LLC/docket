package engine

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-3977 — the completion gates run in a detached child, not inside the
// recording turn. These tests inject the launcher, so no process starts.

// installDetachedSagaLauncher sets the launcher for one test and restores it.
func installDetachedSagaLauncher(t *testing.T, launch func(stepID int) error) {
	t.Helper()
	prev := LaunchDetachedSaga
	LaunchDetachedSaga = launch
	t.Cleanup(func() { LaunchDetachedSaga = prev })
}

// detachedSagaWorld activates a run whose implement@0 carries a completion gate
// and claims it, returning the step and a recording closure.
func detachedSagaWorld(t *testing.T, name string) (*sql.DB, *Engine, int, func(*bool) error) {
	t.Helper()
	conn := mustDB(t)
	runID := activatedBatchRun(t, conn, batchOverrideSrc, name)

	e := testEngine()
	e.Gates = &exitGates{fail: true, exit: 2}

	stepID := stepIDInRun(t, conn, runID, "implement@0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)
	record := func(pending *bool) error {
		return e.CompleteStep(conn, stepID, CompleteOptions{
			Token: claim.Token, Artifact: []byte("the change summary"),
			Pending: pending, NowMS: nowMS,
		})
	}
	return conn, e, stepID, record
}

func gateRowCount(t *testing.T, conn *sql.DB, stepID int) int {
	t.Helper()
	rows, err := db.GateResultsForStep(conn, stepID)
	testsupport.Must(t, err, "GateResultsForStep: %v", err)
	return len(rows)
}

// A record with a launcher returns with the artifact recorded and the saga
// pending: no gate has run. The child then runs the gates and routes.
func TestRecordHandsTheGatesToTheDetachedRun(t *testing.T) {
	withDetachedPreGateLockDir(t)
	conn, e, stepID, record := detachedSagaWorld(t, "dkt3977-detached.toml")

	var launched []int
	installDetachedSagaLauncher(t, func(id int) error {
		launched = append(launched, id)
		return nil
	})

	var pending bool
	testsupport.Must(t, record(&pending), "record: %v", nil)

	if !pending {
		t.Fatal("Pending = false; the launcher accepted the run, so the gates are the child's")
	}
	if len(launched) != 1 || launched[0] != stepID {
		t.Fatalf("launched = %v, want one launch for step %d", launched, stepID)
	}
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if !step.InSaga() {
		t.Fatalf("saga closed inside the recording turn (status %q); the gates ran inline", step.Status)
	}
	if n := gateRowCount(t, conn, stepID); n != 0 {
		t.Fatalf("%d gate rows after record, want 0 — no gate may run in the recording turn", n)
	}

	run, err := e.RunDetachedSaga(conn, stepID, nowMS)
	testsupport.Must(t, err, "RunDetachedSaga: %v", err)
	if run.Outcome != DetachedSagaRan {
		t.Errorf("outcome = %q, want %q", run.Outcome, DetachedSagaRan)
	}
	step, err = db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.InSaga() {
		t.Error("saga still open after the detached run")
	}
	failed, err := FailedGates(conn, stepID)
	testsupport.Must(t, err, "FailedGates: %v", err)
	if len(failed) != 1 || failed[0].Gate != "build" {
		t.Errorf("FailedGates = %v, want the build gate the child ran", failed)
	}
}

// A launcher that fails leaves nothing to pick the saga up inside an open
// dispatch, so the record runs it inline as it always did.
func TestRecordRunsInlineWhenTheLaunchFails(t *testing.T) {
	withDetachedPreGateLockDir(t)
	conn, _, stepID, record := detachedSagaWorld(t, "dkt3977-launchfail.toml")
	installDetachedSagaLauncher(t, func(int) error { return errors.New("no exec") })

	var pending bool
	testsupport.Must(t, record(&pending), "record: %v", nil)

	if pending {
		t.Error("Pending = true after a failed launch")
	}
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.InSaga() {
		t.Error("saga still open; a failed launch must fall back to inline")
	}
	if n := gateRowCount(t, conn, stepID); n == 0 {
		t.Error("no gate rows; the inline fallback did not run the gates")
	}
}

// With no launcher (every test binary, and any caller outside the real entry
// point) the record is synchronous, exactly as before.
func TestRecordIsInlineWithoutALauncher(t *testing.T) {
	withDetachedPreGateLockDir(t)
	conn, _, stepID, record := detachedSagaWorld(t, "dkt3977-nolauncher.toml")
	installDetachedSagaLauncher(t, nil)

	var pending bool
	testsupport.Must(t, record(&pending), "record: %v", nil)

	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if pending || step.InSaga() {
		t.Errorf("pending=%v inSaga=%v, want an inline, finished saga", pending, step.InSaga())
	}
}

// While a detached run holds the step's lock, a second detached run exits
// without running and ResumeSaga leaves the step alone — a resumer reading the
// child's started-but-unrecorded gate would park the step as interrupted.
func TestHeldSagaLockKeepsOtherResumersOut(t *testing.T) {
	withDetachedPreGateLockDir(t)
	conn, e, stepID, record := detachedSagaWorld(t, "dkt3977-lock.toml")
	installDetachedSagaLauncher(t, func(int) error { return nil })
	testsupport.Must(t, record(nil), "record: %v", nil)

	path := detachedSagaLockPath(stepID)
	lock, held, err := holdDetachedPreGateLock(path)
	testsupport.Must(t, err, "taking the lock: %v", err)
	if held {
		t.Fatal("lock reported held before anyone took it")
	}

	run, err := e.RunDetachedSaga(conn, stepID, nowMS)
	testsupport.Must(t, err, "RunDetachedSaga: %v", err)
	if run.Outcome != DetachedSagaRunning {
		t.Errorf("outcome = %q, want %q while the lock is held", run.Outcome, DetachedSagaRunning)
	}
	testsupport.Must(t, e.ResumeSaga(conn, stepID, nowMS), "ResumeSaga: %v", nil)
	if n := gateRowCount(t, conn, stepID); n != 0 {
		t.Errorf("%d gate rows while another resumer held the lock, want 0", n)
	}

	releaseDetachedPreGateLock(lock, path)
	testsupport.Must(t, e.ResumeSaga(conn, stepID, nowMS), "ResumeSaga: %v", nil)
	if n := gateRowCount(t, conn, stepID); n == 0 {
		t.Error("no gate rows once the lock was released; the saga should resume")
	}
}
