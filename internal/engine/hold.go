package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// `docket step hold` (DKT-3287): the conductor parks ONE ready step for the
// operator. Before it, a step that kept failing with no `max_attempts` was
// re-offered forever, and the only way to stop offering it was `run abandon
// --issue`, which failed every step of its issue. A held step is
// `waiting-human` with park class `held`; `step resolve` then retries, skips,
// or fails it like any other park.
//
// Ready steps only (operator ruling): a conductor holding a claimed failing
// step force-reaps it first, which returns it to the pool without spending an
// attempt, and then holds it.

// HoldOptions are `step hold`'s inputs.
type HoldOptions struct {
	// Reason is `--reason`, REQUIRED: it becomes the step's park reason.
	Reason string
	// By is who held it and from where (see Attribution).
	By Attribution
	// Token is the run's conductor capability; required on a bound run.
	Token string
	NowMS int64
}

// HoldStep parks one ready step `waiting-human` with park class `held`, and
// records one `step-held` event carrying the reason and the actor.
func HoldStep(conn *sql.DB, stepID int, opts HoldOptions) error {
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		return validationErr("--reason is required: a held step parks for the " +
			"operator, who needs to know why")
	}
	if err := opts.By.require("step hold"); err != nil {
		return err
	}
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return err
	}
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return err
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("holding %s: %w", step.Instance, err)
	}
	defer tx.Rollback()

	if err := authorizeConductorTx(tx, step.RunID, opts.Token, "step hold"); err != nil {
		return err
	}
	sched, err := LoadScheduler(tx, step.RunID, defs, opts.NowMS)
	if err != nil {
		return err
	}
	var fresh *db.Step
	for _, s := range sched.Steps() {
		if s.ID == stepID {
			fresh = s
			break
		}
	}
	if fresh == nil {
		return notFoundErr(nil, "step %s is not in its run's schedule", model.FormatStepID(stepID))
	}
	if fresh.Status != db.StepPending {
		return conflictErr(
			"step %s is %s; `step hold` parks a READY step only — a claimed step "+
				"is force-reaped first (`step reap`), and a parked one is already "+
				"waiting on the operator", fresh.Instance, fresh.Status)
	}
	if ready, cond := sched.Ready(fresh); !ready {
		return conflictErr("step %s is not ready (%s); `step hold` parks a READY step only",
			fresh.Instance, cond)
	}

	if err := db.SetStepRoutingWithParkReasonTx(tx, fresh.ID, "hold", reason,
		db.StepWaitingHuman, db.ParkClassHeld, reason, opts.NowMS); err != nil {
		return err
	}
	data, err := rulingData(opts.By, map[string]any{"reason": reason, "held_by": "step hold"})
	if err != nil {
		return err
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepHeld, RunID: fresh.RunID, Instance: fresh.Instance,
		IssueID: fresh.IssueID, Data: data, AtMS: opts.NowMS,
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("holding %s: %w", fresh.Instance, err)
	}
	return nil
}
