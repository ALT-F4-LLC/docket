package engine

import (
	"database/sql"
	"fmt"
	"path/filepath"

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

// LaunchDetachedSaga starts one detached `step resume` for a step whose stage 1
// just committed. Nil means no launcher is installed and the saga runs inline;
// the CLI installs it in the binary's real entry point only, for the reason
// LaunchDetachedPreGates documents.
var LaunchDetachedSaga func(stepID int) error

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

// RunDetachedSaga is the detached child's whole work: hold the step's saga
// lock, resume the saga, then drive the run's engine-side lifecycles. A child
// finding the lock held exits without running.
func (e *Engine) RunDetachedSaga(conn *sql.DB, stepID int, nowMS int64) (DetachedSagaRun, error) {
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

// hasCompletionGates reports whether the recorded step declares a gate that
// runs inside the saga. A step whose spec cannot be resolved reports false, so
// it runs inline.
func (e *Engine) hasCompletionGates(conn *sql.DB, stepID int) bool {
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
func (e *Engine) ResumeStrandedSagas(conn *sql.DB, runID int, nowMS int64) error {
	if LaunchDetachedSaga == nil {
		return nil
	}
	rows, err := conn.Query(
		`SELECT id FROM steps
		  WHERE run_id = ? AND saga_stage IS NOT NULL AND saga_stage NOT IN ('', ?)
		    AND kind != ? AND updated_at_ms < ?`,
		runID, store.SagaHeld, workflow.ClassAction, nowMS-strandedSagaGraceMS)
	if err != nil {
		return fmt.Errorf("listing stranded sagas: %w", err)
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("listing stranded sagas: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("listing stranded sagas: %w", err)
	}
	rows.Close()

	for _, id := range ids {
		path := detachedSagaLockPath(id)
		if path == "" {
			continue
		}
		lock, held, err := holdDetachedPreGateLock(path)
		if err != nil {
			return err
		}
		if held {
			continue
		}
		err = e.resumeSaga(conn, id, nowMS)
		releaseDetachedPreGateLock(lock, path)
		if err != nil {
			return err
		}
	}
	return nil
}
