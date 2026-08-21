package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// `docket run budget RUN-N --set N` — engine-spec §1's budget line
// (docs/tdd/events-follow.md §7).
//
// THIS VERB CLOSES B24. Stage 6 recorded the gap in two places and could not
// close it: a comment at the end of `enforceBudgetTx` ("nothing here clears the
// cap, so a `run resume` returns the run to `active` and the very next claim
// reaches this same refusal … Raising it is `run start`-time only in v1"), and a
// runbook section in operations.md §4 that told an operator to raise a cap with
// `sqlite3` and reminded them to bump `row_version` by hand. Both were correct
// descriptions of a missing verb. This is the verb.
//
// WHAT IT DOES NOT DO IS MOST OF THE DESIGN. It does not change the run's
// status, does not re-scan for parked steps, does not sweep anything, and does
// not repair the breach. The budget check already runs on every claim, in the
// claim's own transaction, reading the cap FRESH FROM THE ROW — so raising the
// cap un-wedges the run by arithmetic, and any mechanism added here would be a
// second thing that could disagree with the first.

// budgetChangeReasonPrefix opens the sentence a RESOLVING raise writes to
// `run.reason` below — the one and only place this package ever writes it.
// A later raise recognizes its own leftover by this prefix (DKT-47) rather
// than by guessing, which is what lets it retire a stale one without ever
// mistaking an operator's own pause reason for its own text.
const budgetChangeReasonPrefix = "budget: cap changed from "

// RunBudget is the read form's answer (B-4): the effective cap and the numbers
// it is compared against.
//
// It carries the SAME quantities `run report` shows, computed the same way, so
// an operator deciding what to raise a cap to does not have to read a report to
// find out what the run has already spent.
type RunBudget struct {
	Run      string  `json:"run"`
	Budget   float64 `json:"budget"`
	Source   string  `json:"source"`
	Floor    float64 `json:"floor"`
	Reported float64 `json:"reported"`
	Unit     string  `json:"unit,omitempty"`
	Spend    float64 `json:"spend"`
	// The MEASURED dimension (DKT-238), reported beside the declared one
	// because a run stopped on tokens and a run stopped on declared cost need
	// different answers and an operator must be able to tell which they have.
	//
	// All three are `omitempty` and absent when the dimension is dormant, so a
	// run that never armed it reads exactly as it always did.
	UsageBudget float64 `json:"usage_budget,omitempty"`
	UsageUnit   string  `json:"usage_unit,omitempty"`
	UsageSpend  float64 `json:"usage_spend,omitempty"`
	RowVersion  int     `json:"row_version"`
}

// SetRunBudget writes a run's cap, event-logged, in one transaction.
//
// `budget` is the new cap; ZERO MEANS UNLIMITED (B-1), the meaning the flag has
// carried since S3 and the one `budgetSnapshot.unlimited()` enforces. Raising
// and lowering are the same operation (B-2) — engine-spec §1 says "raise/lower a
// live cap", and a verb that refused to lower would be a verb that half exists.
func SetRunBudget(conn *sql.DB, runID int, budget float64, reason string, ifVersion *int, nowMS int64) (*RunBudget, error) {
	if budget < 0 {
		return nil, validationErr(
			"--set must be >= 0, got %g (0 means unlimited)", budget)
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("setting the budget: %w", err)
	}
	defer tx.Rollback()

	run, err := db.GetRunTx(tx, runID)
	if err != nil {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}

	// B-3: a terminal run's cap is HISTORY. Editing it would change what a
	// finished run's report says it was allowed to spend, retroactively, which
	// is a different verb from the one this is — and one nobody asked for.
	if run.Status.Terminal() {
		return nil, conflictErr(
			"run %s is %s; its budget is a record of what it was allowed to "+
				"spend, and a finished run's record does not move",
			run.Ref(), run.Status)
	}

	if err := db.SetRunBudgetTx(tx, runID, budget, ifVersion, nowMS); err != nil {
		return nil, err
	}

	// DKT-80: a cap change that RESOLVES the breach clears the stale breach
	// record. Before this, `breach_reason` (and the row's `reason`, when the
	// breach was why the run parked) still read "budget: spend N of cap M
	// reached at <instance>" after the cap moved past N — and that string
	// misled a session into treating an advancing run as budget-walled. The
	// row states what is true NOW; the history of the breach and of the raise
	// lives in the `run-paused` and `run-budget-set` events.
	facts, err := db.RunBudgetFactsTx(tx, runID)
	if err != nil {
		return nil, err
	}
	answer, err := runBudgetTx(tx, runID)
	if err != nil {
		return nil, err
	}
	breachCleared := false
	if facts.BreachReason != "" && (budget == 0 || answer.Spend <= budget) {
		newReason := ""
		if run.Reason == facts.BreachReason {
			// The rewritten reason stays true while the run remains parked:
			// the cap moved, the old breach is history, and resuming is the
			// operator's next (separate, deliberate) act.
			newReason = fmt.Sprintf(
				budgetChangeReasonPrefix+"%g to %g after breach (was: %s); "+
					"run resume to continue", run.Budget, budget, facts.BreachReason)
		}
		if err := db.ClearRunBreachTx(tx, runID, newReason, nowMS); err != nil {
			return nil, err
		}
		breachCleared = true
	} else if facts.BreachReason == "" && strings.HasPrefix(run.Reason, budgetChangeReasonPrefix) {
		// DKT-47: nothing is CURRENTLY breached, so the branch above never
		// runs — but the row still carries a PREVIOUS raise's "cap changed
		// from X to Y" sentence, naming a cap that has since moved again.
		// Left alone, THIS raise would read as though it were the one that
		// resolved a breach, which is false: no breach is standing. The
		// sentence's own history remains in the run-paused and
		// run-budget-set events; the row states only what is true now.
		// A run STILL PARKED on that sentence (waiting-human) is a different
		// case from one that already moved on: blanking it would leave a
		// parked run with no stated reason and no resume instruction, which
		// is worse than a stale number. Rewrite with the caps now in force
		// instead, keeping the same prefix and resume tail; blank only when
		// nothing is parked on it.
		newReason := ""
		if run.Status == model.RunWaitingHuman {
			newReason = fmt.Sprintf(
				budgetChangeReasonPrefix+"%g to %g; run resume to continue",
				run.Budget, budget)
		}
		if err := db.ClearStaleBudgetReasonTx(tx, runID, newReason, nowMS); err != nil {
			return nil, err
		}
	}

	// B-EV: event-logged, in the SAME transaction as the write.
	//
	// §7.3 argues the kind at length; the short form is that no existing kind
	// anchors "the cap moved". A run that breached at one number and later
	// admitted a claim at another must have something in its trail explaining
	// the difference, or the log stops being the run's complete account of
	// itself — which is precisely what operations.md §4 warned the manual edit
	// would do ("it bypasses the event log").
	//
	// `from` and `to` both ride, because the interesting question about a cap
	// change is the delta and a log carrying only the new value would make an
	// auditor reconstruct the old one from an earlier event.
	payload := map[string]any{
		"from":   run.Budget,
		"to":     budget,
		"reason": reason,
	}
	// The clear rides in the SAME event as the change that caused it (DKT-80),
	// so an auditor reading the feed sees "the cap moved AND the breach record
	// was retired" as one fact rather than reconstructing the second from the
	// row's silence.
	if breachCleared {
		payload["breach_cleared"] = facts.BreachReason
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("recording the budget change: %w", err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventRunBudgetSet, RunID: runID, Data: string(data), AtMS: nowMS,
	}); err != nil {
		return nil, err
	}

	// B-10: THE STATUS IS NOT TOUCHED.
	//
	// A breached run is `waiting-human` and stays so until `run resume`. That is
	// what `waiting-human` means — a person must decide — and a verb that
	// un-parked the run as a side effect of moving a number would take that
	// decision away from the operator who was handed it. Raising a cap and
	// restarting work are two acts, and an operator may well want the first
	// without the second.
	//
	// The breach record, by contrast, is CLEARED above once the cap change
	// resolves it (DKT-80): the row states what is true now, and the trail —
	// why the run stopped, and what was done about it — lives in the events.

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("setting the budget: %w", err)
	}
	return answer, nil
}

// GetRunBudget is the read form (B-4).
func GetRunBudget(conn *sql.DB, runID int) (*RunBudget, error) {
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("reading the budget: %w", err)
	}
	// Never committed: a read verb's zero-write property is this rollback
	// rather than the absence of an INSERT, the discipline every other read in
	// this package follows.
	defer tx.Rollback()

	if _, err := db.GetRunTx(tx, runID); err != nil {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	return runBudgetTx(tx, runID)
}

// runBudgetTx assembles the answer inside a caller's transaction.
//
// It recomputes the floor and the reported usage through the SAME helpers
// enforcement and the report use (RunFloorTx, db.ReportedUsageTx), rather than
// reading `runs.usage_floor`. That column is a cache for the report and is never
// read by anything that decides (§4.3) — and this verb's whole purpose is to
// inform a decision about the number enforcement will compare, so it must show
// the number enforcement will compare.
func runBudgetTx(tx *sql.Tx, runID int) (*RunBudget, error) {
	run, err := db.GetRunTx(tx, runID)
	if err != nil {
		return nil, err
	}

	floor, err := RunFloorTx(tx, runID)
	if err != nil {
		return nil, err
	}

	unit, err := db.BudgetUnitTx(tx, run.ProjectID)
	if err != nil {
		return nil, err
	}
	var reported float64
	if unit != "" {
		reported, err = db.ReportedUsageTx(tx, runID, unit)
		if err != nil {
			return nil, err
		}
	}

	configDefault, err := db.BudgetDefaultTx(tx, run.ProjectID)
	if err != nil {
		return nil, err
	}

	// The measured dimension, through the same helper enforcement reads
	// (DKT-238). Loaded only when the run declares a cap: dormancy here is the
	// same property it is at the predicate.
	var usageUnit string
	var usageSpend float64
	if run.UsageBudget > 0 {
		usageUnit, err = db.UsageBudgetUnitTx(tx, run.ProjectID)
		if err != nil {
			return nil, err
		}
		if usageUnit != "" {
			usageSpend, err = db.ReportedUsageTx(tx, runID, usageUnit)
			if err != nil {
				return nil, err
			}
		}
	}

	return &RunBudget{
		Run:         run.Ref(),
		Budget:      run.Budget,
		Source:      string(BudgetSourceOf(run.Budget, configDefault)),
		Floor:       floor,
		Reported:    reported,
		Unit:        unit,
		Spend:       max(floor, reported),
		UsageBudget: run.UsageBudget,
		UsageUnit:   usageUnit,
		UsageSpend:  usageSpend,
		RowVersion:  run.RowVersion,
	}, nil
}
