package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The completion gates run in a detached child, not inside the
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
	// The handoff was recorded before the launch failed; the inline saga's
	// gate-started follows it, so the feed shows the fallback.
	var handoff, firstGate int
	testsupport.Must(t, conn.QueryRow(
		`SELECT COALESCE(MAX(CASE WHEN kind = ? THEN seq END), 0),
		        COALESCE(MIN(CASE WHEN kind = ? THEN seq END), 0) FROM events`,
		EventSagaDetachStarted, EventGateStarted,
	).Scan(&handoff, &firstGate), "reading event order: %v", nil)
	if handoff == 0 || firstGate <= handoff {
		t.Errorf("handoff seq %d, first gate-started seq %d; want the handoff, then the inline gate",
			handoff, firstGate)
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
	if n := countEvents(t, conn, EventSagaDetachStarted); n != 0 {
		t.Errorf("%d %s events with no launcher, want 0", n, EventSagaDetachStarted)
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

// A step with no completion gate has nothing slow to hand off, so it records
// inline even with a launcher installed: its routing, and the downstream vote
// and action steps it readies, are done when the record returns.
func TestGatelessStepRecordsInlineWithALauncher(t *testing.T) {
	withDetachedPreGateLockDir(t)
	conn := mustDB(t)
	_, _ = activatedRun(t, conn)

	claimAndComplete(t, conn, testEngine(), "implement@0", "summary", "")

	var launched int
	installDetachedSagaLauncher(t, func(int) error { launched++; return nil })

	stepID := stepIDByInstance(t, conn, "review@0#0")
	claim, err := ClaimStep(conn, stepID, ClaimOptions{Owner: "w", NowMS: nowMS})
	testsupport.Must(t, err, "claim: %v", err)

	var pending bool
	err = testEngine().CompleteStep(conn, stepID, CompleteOptions{
		Token: claim.Token, Artifact: []byte("summary"), Pending: &pending, NowMS: nowMS,
	})
	testsupport.Must(t, err, "record: %v", err)

	if pending || launched != 0 {
		t.Errorf("pending=%v launched=%d, want an inline record with no launch", pending, launched)
	}
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.InSaga() {
		t.Error("saga still open after a gate-less record")
	}
	if n := countEvents(t, conn, EventSagaDetachStarted); n != 0 {
		t.Errorf("%d %s events for a gate-less record, want 0", n, EventSagaDetachStarted)
	}
}

// A detached run that died leaves its step mid-saga. `next` finishes it once
// the step is past the launch grace and no run holds its lock; a fresh step, or
// one whose lock is held, is left to its child.
func TestNextResumesAStrandedSaga(t *testing.T) {
	withDetachedPreGateLockDir(t)
	conn, e, stepID, record := detachedSagaWorld(t, "dkt3977-stranded.toml")
	installDetachedSagaLauncher(t, func(int) error { return nil }) // the child never runs
	testsupport.Must(t, record(nil), "record: %v", nil)
	runID := mustRunOf(t, conn, stepID)

	age := func() {
		execSQL(t, conn, `UPDATE steps SET updated_at_ms = ? WHERE id = ?`,
			nowMS-2*strandedSagaGraceMS, stepID)
	}
	open := func() bool {
		step, err := db.GetStep(conn, stepID)
		testsupport.Must(t, err, "GetStep: %v", err)
		return step.InSaga()
	}

	execSQL(t, conn, `UPDATE steps SET updated_at_ms = ? WHERE id = ?`, nowMS, stepID)
	_, err := e.NextSteps(conn, runID, 10, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	if !open() {
		t.Fatal("next resumed a saga inside the launch grace; the child may still be starting")
	}

	age()
	path := detachedSagaLockPath(stepID)
	lock, _, err := holdDetachedPreGateLock(path)
	testsupport.Must(t, err, "taking the lock: %v", err)
	_, err = e.NextSteps(conn, runID, 10, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	if !open() {
		t.Fatal("next resumed a saga whose lock a live run holds")
	}
	releaseDetachedPreGateLock(lock, path)

	_, err = e.NextSteps(conn, runID, 10, nowMS)
	testsupport.Must(t, err, "NextSteps: %v", err)
	if open() {
		t.Error("next left a stranded saga open")
	}
}

// A detached child that dies before its first gate writes nothing, and the
// recording parent has already returned. The handoff event committed before
// the launch is the record: it exists, with no gate-started after it, and no
// NextSteps call is needed to see it.
func TestDetachedSagaChildDeathLeavesARecord(t *testing.T) {
	withDetachedPreGateLockDir(t)
	conn, e, stepID, record := detachedSagaWorld(t, "detach-childdeath.toml")

	// Written before launch: the launcher sees the event already committed.
	seenAtLaunch := -1
	installDetachedSagaLauncher(t, func(int) error {
		seenAtLaunch = countEvents(t, conn, EventSagaDetachStarted)
		return nil
	})
	var pending bool
	testsupport.Must(t, record(&pending), "record: %v", nil)
	if !pending {
		t.Fatal("Pending = false; the saga did not detach")
	}
	if seenAtLaunch != 1 {
		t.Fatalf("%d %s events when the child launched, want 1 written before launch",
			seenAtLaunch, EventSagaDetachStarted)
	}

	// The child fails before the gate stage: its entry finds no lock
	// directory and refuses before resuming the saga.
	prev := detachedPreGateLockDir
	detachedPreGateLockDir = func() string { return "" }
	_, err := e.RunDetachedSaga(conn, stepID, nowMS)
	detachedPreGateLockDir = prev
	if err == nil {
		t.Fatal("RunDetachedSaga succeeded; the child was meant to fail before its first gate")
	}

	// No NextSteps call anywhere above. The record is there; no gate ran.
	if n := countEvents(t, conn, EventGateStarted); n != 0 {
		t.Fatalf("%d gate-started events, want 0: the child died before the gate stage", n)
	}
	if n := gateRowCount(t, conn, stepID); n != 0 {
		t.Fatalf("%d gate rows, want 0", n)
	}
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if !step.InSaga() {
		t.Fatalf("saga closed (status %q); nothing should have advanced it", step.Status)
	}
	var eventStep int
	var data string
	err = conn.QueryRow(
		`SELECT COALESCE(step_id, 0), data FROM events WHERE kind = ?`, EventSagaDetachStarted,
	).Scan(&eventStep, &data)
	testsupport.Must(t, err, "reading the handoff event: %v", err)
	if eventStep != stepID {
		t.Errorf("handoff step = %d, want %d", eventStep, stepID)
	}
	if want := fmt.Sprintf(`"stranded_after_ms":%d`, nowMS+strandedSagaGraceMS); !strings.Contains(data, want) {
		t.Errorf("handoff data = %s, want it to carry %s", data, want)
	}
}

// A handoff that cannot be recorded is never launched: a child launched
// without its record could die leaving nothing. The saga runs inline instead.
func TestUnrecordedHandoffIsNotLaunched(t *testing.T) {
	withDetachedPreGateLockDir(t)
	conn, _, stepID, record := detachedSagaWorld(t, "detach-unrecorded.toml")
	execSQL(t, conn, `CREATE TRIGGER fail_handoff BEFORE INSERT ON events
		 WHEN NEW.kind = '`+EventSagaDetachStarted+`'
		 BEGIN SELECT RAISE(ABORT, 'injected handoff failure'); END`)
	launched := 0
	installDetachedSagaLauncher(t, func(int) error { launched++; return nil })

	var pending bool
	testsupport.Must(t, record(&pending), "record: %v", nil)

	if launched != 0 || pending {
		t.Errorf("launched=%d pending=%v, want no launch when the handoff cannot be recorded",
			launched, pending)
	}
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	if step.InSaga() {
		t.Error("saga still open; an unrecorded handoff must fall back to inline")
	}
}

func mustRunOf(t *testing.T, conn *sql.DB, stepID int) int {
	t.Helper()
	step, err := db.GetStep(conn, stepID)
	testsupport.Must(t, err, "GetStep: %v", err)
	return step.RunID
}
