package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// `docket step hold` (DKT-3287): the conductor parks ONE pending step for the
// operator. Before it, a step that kept failing with no `max_attempts` was
// re-offered forever, and the only way to stop offering it was `run abandon
// --issue`, which failed every step of its issue. A held step is
// `waiting-human` with park class `held`; `step resolve` then retries, skips,
// or fails it like any other park.
//
// Pending steps only: a conductor holding a claimed failing step force-reaps
// it first, which returns it to the pool without spending an attempt, and
// then holds it. A pending step need not be ready (DKT-4273): a staged row
// behind an unfinished predecessor holds like a ready one, which is what lets
// an operator's ruling on an issue live in the engine instead of in a launch
// filter. `run hold-issue` holds every pending step of one issue at once.

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

// HoldStep parks one pending step `waiting-human` with park class `held`, and
// records one `step-held` event carrying the reason and the actor.
func HoldStep(conn *sql.DB, stepID int, opts HoldOptions) error {
	reason, err := holdReason(opts)
	if err != nil {
		return err
	}
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
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
	if err := requireHoldableRunTx(tx, step.RunID); err != nil {
		return err
	}
	fresh, err := db.GetStepTx(tx, stepID)
	if err != nil {
		return err
	}
	if fresh.Status != db.StepPending {
		return conflictErr(
			"step %s is %s; `step hold` parks a PENDING step only — a claimed step "+
				"is force-reaped first (`step reap`), and a parked one is already "+
				"waiting on the operator", fresh.Instance, fresh.Status)
	}
	if err := holdPendingTx(tx, fresh, reason, "step hold", opts); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("holding %s: %w", fresh.Instance, err)
	}
	return nil
}

// HoldIssue parks every pending step of one issue in a run `waiting-human`
// with park class `held` (DKT-4273), ready and staged alike, and records one
// `step-held` event per step. It refuses, changing nothing, when a step of the
// issue is in flight (claimed, running, or gated) or when the issue has no
// pending step to hold, so a hold never reads as covering work it did not
// stop. The held steps leave the next dispatch's rows and are listed under the
// manifest's `held`; each releases through `step resolve --as retry`.
func HoldIssue(conn *sql.DB, runID, issueID int, opts HoldOptions) ([]int, error) {
	reason, err := holdReason(opts)
	if err != nil {
		return nil, err
	}
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("holding %s: %w", model.FormatID(issueID), err)
	}
	defer tx.Rollback()

	if err := authorizeConductorTx(tx, runID, opts.Token, "run hold-issue"); err != nil {
		return nil, err
	}
	if err := requireHoldableRunTx(tx, runID); err != nil {
		return nil, err
	}
	steps, err := db.ListIssueStepsTx(tx, runID, issueID)
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return nil, notFoundErr(nil, "issue %s has no steps in run %s",
			model.FormatID(issueID), model.FormatRunID(runID))
	}
	var pending []*db.Step
	var inFlight []string
	for _, s := range steps {
		switch s.Status {
		case db.StepPending:
			pending = append(pending, s)
		case db.StepClaimed, db.StepRunning, db.StepGated:
			inFlight = append(inFlight, fmt.Sprintf("%s (%s)", s.Instance, s.Status))
		}
	}
	if len(inFlight) > 0 {
		return nil, conflictErr(
			"issue %s has steps in flight: %s; force-reap a claimed step "+
				"(`step reap`) or let the others record before holding the issue",
			model.FormatID(issueID), strings.Join(inFlight, ", "))
	}
	if len(pending) == 0 {
		return nil, conflictErr("issue %s has no pending step in run %s to hold",
			model.FormatID(issueID), model.FormatRunID(runID))
	}
	held := make([]int, 0, len(pending))
	for _, s := range pending {
		if err := holdPendingTx(tx, s, reason, "run hold-issue", opts); err != nil {
			return nil, err
		}
		held = append(held, s.ID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("holding %s: %w", model.FormatID(issueID), err)
	}
	return held, nil
}

func holdReason(opts HoldOptions) (string, error) {
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		return "", validationErr("--reason is required: a held step parks for the " +
			"operator, who needs to know why")
	}
	if err := opts.By.require("step hold"); err != nil {
		return "", err
	}
	return reason, nil
}

// requireHoldableRunTx refuses a hold under a terminal run: a parked step
// there could never be resolved back into work.
func requireHoldableRunTx(tx *sql.Tx, runID int) error {
	run, err := db.GetRunTx(tx, runID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return conflictErr("run %s is %s; a hold there could never be released",
			run.Ref(), run.Status)
	}
	return nil
}

// holdPendingTx parks one pending step and records its step-held event.
func holdPendingTx(tx *sql.Tx, step *db.Step, reason, via string, opts HoldOptions) error {
	if err := db.SetStepRoutingWithParkReasonTx(tx, step.ID, "hold", reason,
		db.StepWaitingHuman, db.ParkClassHeld, reason, opts.NowMS); err != nil {
		return err
	}
	data, err := rulingData(opts.By, map[string]any{"reason": reason, "held_by": via})
	if err != nil {
		return err
	}
	return recordEvent(tx, eventRecord{
		Kind: EventStepHeld, RunID: step.RunID, Instance: step.Instance,
		IssueID: step.IssueID, Data: data, AtMS: opts.NowMS,
	})
}

// HeldStep is a step parked by a hold (DKT-4273), with the reason the holder
// gave. A manifest lists these beside the rows it withheld, so a reader sees
// that a missing row is held on purpose and why.
type HeldStep struct {
	Step     string `json:"step"`
	Instance string `json:"instance"`
	Issue    string `json:"issue"`
	Reason   string `json:"reason"`
}

// HeldSteps lists the run's held steps in step order. A pure read.
func HeldSteps(conn *sql.DB, runID int) ([]HeldStep, error) {
	steps, err := db.ListRunSteps(conn, runID)
	if err != nil {
		return nil, fmt.Errorf("listing run steps: %w", err)
	}
	var out []HeldStep
	for _, s := range steps {
		if s.Status == db.StepWaitingHuman && s.ParkClass == db.ParkClassHeld {
			out = append(out, HeldStep{
				Step: model.FormatStepID(s.ID), Instance: s.Instance,
				Issue: model.FormatID(s.IssueID), Reason: s.ParkReason,
			})
		}
	}
	return out, nil
}
