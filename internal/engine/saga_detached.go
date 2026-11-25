package engine

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The detached saga.
//
// `step record` used to run the completion gates inside the recording turn, so
// an executor's turn outlived the prompt cache for as long as the slowest gate.
// Stage 0 and 1 (validate, record the artifact, retire the token) are all the
// recording turn needs: from that commit the step is engine-owned. The gates
// and routing run in a detached `step resume` child, launched the way
// `step pregate` is, and the recording returns with the saga pending.
//
// THE CHILD TAKES THE OBSERVING ROLE. The record verb used to be the invocation
// that readied downstream vote and action steps (DriveRunLifecycles), because
// `next` cannot drive them while a dispatch is open. The child runs that drive
// after the saga, so a staged `judges -> gate -> reconcile -> report` chain
// still advances end to end.
//
// ONE RESUMER AT A TIME. A second resumer that finds a gate-started event with
// no result reads it as an interrupted gate and parks the step, so while the
// child runs, ResumeSaga must leave the step alone. The child holds a per-step
// flock; ResumeSaga only probes it (no create), so a store without a lock
// directory, and every test, behaves exactly as before.
//
// THE HANDOFF IS RECORDED BEFORE THE LAUNCH. A child that dies before its
// first gate writes nothing, and the recording parent has already returned, so
// only a record committed before the launch can cover it: `step record`
// commits a `saga-detach-started` event first, for every detached handoff. A
// reader of the event feed can then tell a stranded saga (a handoff with no
// `gate-started` after it, past its `stranded_after_ms`) without anyone
// calling `next`; no engine surface reads the event yet. ResumeStrandedSagas
// stays the recovery and does not consult it. A handoff that cannot be
// recorded is never launched.
//
// THE PARENT CONFIRMS THE HANDOFF BEFORE IT RETURNS. A launch that succeeds
// proves only that a process started, and a child its parent's sandbox kills
// first never reaches its first gate. Only a child holding the step's saga lock
// owns the gates; settleSagaHandoff waits for one, and runs the saga itself
// when none takes the lock in time.

// LaunchDetachedSaga starts one detached `step resume` for a step whose stage 1
// just committed. Nil means no launcher is installed and the saga runs inline;
// the CLI installs it in the binary's real entry point only, for the reason
// LaunchDetachedPreGates documents.
//
// The handoff is confirmed, not trusted. A launcher returns an error when it
// knows its child cannot outlive this process (the CLI's refuses inside a
// bubblewrap sandbox, whose pid namespace dies with the command), and the
// saga then runs inline. A nil return is a start, not a handoff: the record
// waits up to sagaHandoffWait for the child to take the step's saga lock, and
// when none does it takes the lock and runs the gates and routing inline. A
// child the sandbox kills before its first gate therefore leaves no stranded
// saga behind; one that arrives late finds the lock held and exits.
var LaunchDetachedSaga func(stepID int) error

// sagaHandoffWait bounds how long a record waits for its launched child to
// take the step's saga lock: past a healthy child's start (exec, config, store
// open), short enough that an inline fallback stays inside a recording turn.
var sagaHandoffWait = 5 * time.Second

// sagaHandoffPoll is how often the record probes the lock while it waits.
const sagaHandoffPoll = 25 * time.Millisecond

// Outcomes of RunDetachedSaga.
const (
	// DetachedSagaRan: this invocation held the lock and resumed the saga.
	DetachedSagaRan = "ran"
	// DetachedSagaRunning: another resumer holds the lock; nothing was run.
	DetachedSagaRunning = "running"
)

// DetachedSagaRun is the child's report.
type DetachedSagaRun struct {
	Step    string `json:"step"`
	Outcome string `json:"outcome"`
}

// detachedSagaLockPath names the per-step saga lockfile, or "" when no lock
// directory resolves.
func detachedSagaLockPath(stepID int) string {
	dir := detachedPreGateLockDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, fmt.Sprintf("saga-%s.lock", model.FormatStepID(stepID)))
}

// sagaHeldElsewhere reports whether a detached resumer holds the step's lock.
// A missing lockfile or lock directory is no hold.
func sagaHeldElsewhere(stepID int) bool {
	path := detachedSagaLockPath(stepID)
	return path != "" && detachedPreGateHeld(path)
}

// RunDetachedSaga is RunDetachedSagaStore on store.StoreOn(conn).
func (e *Engine) RunDetachedSaga(conn store.LegacyDB, stepID int, nowMS int64) (DetachedSagaRun, error) {
	return e.RunDetachedSagaStore(store.StoreOn(conn), stepID, nowMS)
}

// RunDetachedSagaStore is the detached child's whole work: hold the step's saga
// lock, resume the saga, then drive the run's engine-side lifecycles. A child
// finding the lock held exits without running.
func (e *Engine) RunDetachedSagaStore(storage store.Store, stepID int, nowMS int64) (DetachedSagaRun, error) {
	conn, err := store.SQLDB(storage)
	if err != nil {
		return DetachedSagaRun{}, err
	}
	run := DetachedSagaRun{Step: model.FormatStepID(stepID)}

	lockPath := detachedSagaLockPath(stepID)
	if lockPath == "" {
		return run, validationErr("no lock directory could be resolved for this " +
			"store, and a detached saga run could not keep itself single")
	}
	lock, held, err := holdDetachedPreGateLock(lockPath)
	if err != nil {
		return run, err
	}
	if held {
		run.Outcome = DetachedSagaRunning
		return run, nil
	}
	defer releaseDetachedPreGateLock(lock, lockPath)

	if err := e.resumeSaga(conn, stepID, nowMS); err != nil {
		return run, err
	}
	step, err := store.StoreOn(conn).GetStep(stepID)
	if err != nil {
		return run, err
	}
	if err := e.DriveRunLifecycles(conn, step.RunID, model.NowMS()); err != nil {
		return run, fmt.Errorf("the saga ran, but driving the run's engine-side "+
			"lifecycles failed: %w", err)
	}
	run.Outcome = DetachedSagaRan
	return run, nil
}

// settleSagaHandoff decides who runs a launched step's saga. It reports true
// when a detached child holds the step's saga lock, so the gates are the
// child's; otherwise it takes the lock itself and runs the saga to completion
// before reporting false.
func (e *Engine) settleSagaHandoff(conn store.LegacyDB, stepID int, nowMS int64) (bool, error) {
	deadline := time.Now().Add(sagaHandoffWait)
	for !sagaHeldElsewhere(stepID) {
		if !time.Now().Before(deadline) {
			return e.runUnconfirmedSaga(conn, stepID, nowMS)
		}
		time.Sleep(sagaHandoffPoll)
	}
	return true, nil
}

// runUnconfirmedSaga runs the saga under its lock after no child confirmed the
// handoff. A child that takes the lock first still owns the gates.
func (e *Engine) runUnconfirmedSaga(conn store.LegacyDB, stepID int, nowMS int64) (bool, error) {
	path := detachedSagaLockPath(stepID)
	lock, held, err := holdDetachedPreGateLock(path)
	if err != nil {
		return false, err
	}
	if held {
		return true, nil
	}
	defer releaseDetachedPreGateLock(lock, path)
	return false, e.resumeSaga(conn, stepID, nowMS)
}

// recordSagaDetachStarted commits the handoff event for a step about to be
// launched detached, in its own transaction, outside stage 1's. Its deadline is
// stage 1's activity stamp plus the stranded grace: the instant
// ResumeStrandedSagas starts qualifying the step.
func recordSagaDetachStarted(conn store.LegacyDB, stepID int, nowMS int64) error {
	step, err := store.StoreOn(conn).GetStep(stepID)
	if err != nil {
		return err
	}
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("beginning the saga handoff: %w", err)
	}
	defer tx.Rollback()
	if err := recordEvent(tx, eventRecord{
		Kind: EventSagaDetachStarted, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
		Data: fmt.Sprintf(`{"stranded_after_ms":%d}`, nowMS+strandedSagaGraceMS),
		AtMS: nowMS,
	}); err != nil {
		return err
	}
	return commitWrite(tx)
}

// hasCompletionGates reports whether the recorded step declares a gate that
// runs inside the saga. A step whose spec cannot be resolved reports false, so
// it runs inline.
func (e *Engine) hasCompletionGates(conn store.LegacyDB, stepID int) bool {
	step, err := store.StoreOn(conn).GetStep(stepID)
	if err != nil {
		return false
	}
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return false
	}
	tally, err := loadHoldTally(conn, step.RunID)
	if err != nil {
		return false
	}
	spec := materializedSpec(defs[step.WorkflowID], step, tally)
	return spec != nil && len(completionGates(spec)) > 0
}

// strandedSagaGraceMS is how long a step may sit mid-saga with no detached run
// holding its lock before `next` takes it over: well past the launch-to-lock
// window of a child that is merely starting, so only a child that died leaves a
// step this old.
const strandedSagaGraceMS = 30_000

// ResumeStrandedSagas finishes the sagas a dead detached run left behind. A
// child killed mid-saga leaves a recorded step in a gate stage with no lock
// held and nothing else to advance it; the next scheduling call does. It runs
// only where a launcher is installed (the real binary), so a store that never
// detaches behaves exactly as before. Held and action sagas have their own
// resume paths and are left alone. Each step is resumed under its saga lock,
// so a live child and the sweep never both run a gate.
func (e *Engine) ResumeStrandedSagas(conn store.LegacyDB, runID int, nowMS int64) error {
	if LaunchDetachedSaga == nil {
		return nil
	}
	ids, err := store.StoreOn(conn).StrandedSagaStepIDs(
		runID, workflow.ClassAction, nowMS-strandedSagaGraceMS)
	if err != nil {
		return err
	}

	var failed []error
	for _, id := range ids {
		path := detachedSagaLockPath(id)
		if path == "" {
			continue
		}
		lock, held, err := holdDetachedPreGateLock(path)
		if err != nil {
			// A lock that cannot be taken is this step's failure, reported
			// with the others: the sweep goes on to the next step.
			failed = append(failed, fmt.Errorf("locking STEP-%d's saga: %w", id, err))
			continue
		}
		if held {
			continue
		}
		err = e.resumeSaga(conn, id, nowMS)
		releaseDetachedPreGateLock(lock, path)
		if err != nil {
			// One step's failure must not starve the others of their resume:
			// keep sweeping and report every failure together.
			failed = append(failed, fmt.Errorf("resuming STEP-%d's saga: %w", id, err))
		}
	}
	return errors.Join(failed...)
}
