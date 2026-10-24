package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// `docket events prune` — the event log's retention clause, implemented.
//
// THIS IS THE FIRST DESTRUCTIVE VERB IN THE ENGINE SURFACE, and everything
// interesting about it is what it REFUSES to delete. The retention rule names
// both refusals in one sentence:
//
//	`events prune` refuses events of non-terminal runs and never crosses the
//	artifact-retention boundary
//
// Each is implemented below with its own predicate, its own error, and its own
// test, because they fail for different reasons and an operator hitting one
// needs to know which.
//
// WHAT IT DELETES: rows in `events`. Nothing else — no artifact, no step, no
// run, no usage row, no dispatch row. It does not VACUUM: reclaiming file
// space is a decision made against a backup, and a verb that rewrote the whole
// database as a side effect of trimming a log would be a surprise with a
// different risk profile.

// PruneQuery is one prune's target and its clock.
//
// EXACTLY ONE OF Before / BeforeRun IS SET. A destructive verb with a default
// target is how a log gets deleted by a typo, so "prune everything" is not
// expressible: the caller must name a boundary or a run.
type PruneQuery struct {
	// Before deletes events with `seq < Before` — STRICTLY LESS, so
	// `--before N` and a cursor at `N-1` name the same boundary, which is the
	// convention every other seq comparison in this feed already follows.
	Before int64
	// BeforeRun deletes every event of one run, whatever its seq. It is the
	// preferred form — trim whole runs that reached a terminal status, rather
	// than the oldest N events across all of them, so the report stays honest
	// for every run still covered.
	BeforeRun int
	// RunID narrows Before to one run. Zero is every run.
	RunID int
	// DryRun computes the answer and deletes nothing.
	DryRun bool
	// NowMS is the transaction's clock, used for the retention boundary and for
	// the `events-pruned` event's timestamp. It is passed IN rather than read
	// here so the boundary and the event agree, and so a test can place the
	// window without waiting for wall-clock time to pass.
	NowMS int64
}

// PruneResult is what a prune did, or would have done.
//
// It reports the NEW RETAINED MINIMUM alongside the count so a consumer can set
// its cursor without a second call — which matters because the call that
// invalidated the cursor is exactly this one.
type PruneResult struct {
	Pruned          int   `json:"pruned"`
	RetainedMinimum int64 `json:"retained_minimum"`
	DryRun          bool  `json:"dry_run,omitempty"`
	// HeldByRetention counts rows the boundary protected. It is reported
	// rather than silently subtracted: an operator who asked to prune 900 events
	// and pruned 200 must be told a policy held back the other 700, or they will
	// conclude the verb is broken.
	HeldByRetention int `json:"held_by_retention,omitempty"`
	// LiveRuns names the non-terminal runs that blocked the prune, for the
	// refusal message.
	LiveRuns []string `json:"-"`
}

// PruneEvents is the whole verb: validate, refuse, delete, log — in ONE
// transaction.
//
// THE REFUSAL PREDICATES ARE EVALUATED INSIDE THE DELETE'S TRANSACTION, so
// the set refused and the set deleted are computed over one snapshot. A run that
// reaches `done` between a check and a delete does not widen what was deleted:
// it prunes on the next call, having been a live run for the whole of this one.
func PruneEvents(conn *sql.DB, q PruneQuery) (*PruneResult, error) {
	if err := validatePruneTarget(q); err != nil {
		return nil, err
	}

	// The retention window is read BEFORE the transaction opens, through the
	// pool, because internal/db caps the pool at one connection and a pool read
	// from inside a transaction deadlocks (TestNoPoolReadsInsideTransactions).
	// It is a config value, not a fact about the rows being pruned, so reading
	// it a moment earlier changes no decision.
	retain, err := store.StoreOn(conn).EventsRetain()
	if err != nil {
		return nil, err
	}

	tx, err := store.BeginSQLiteTx(conn)
	if err != nil {
		return nil, fmt.Errorf("pruning events: %w", err)
	}
	defer tx.Rollback()
	st := store.TxOn(tx)

	if q.BeforeRun != 0 {
		if _, err := st.GetRunTx(q.BeforeRun); err != nil {
			return nil, notFoundErr(err, "run %s not found",
				model.FormatRunID(q.BeforeRun))
		}
	}

	sel := pruneSelection(q)

	// REFUSAL 1: events of non-terminal runs are never deleted.
	if err := refuseLiveRunsTx(st, sel); err != nil {
		return nil, err
	}

	// REFUSAL 2: the retention boundary.
	//
	// It CLAMPS rather than refuses outright — a `--before` reaching past the
	// window prunes what it may and reports what it could not — because the
	// boundary is a standing policy rather than a mistake in the command. A
	// refusal would make an operator compute the boundary themselves before
	// every prune, which is the arithmetic the policy exists to do for them.
	held := 0
	if retain > 0 && q.BeforeRun == 0 {
		cutoff := q.NowMS - retain.Milliseconds()
		held, err = st.CountEventsHeldByRetentionTx(sel, cutoff)
		if err != nil {
			return nil, err
		}
		sel.BeforeAtMS = cutoff
	}

	if q.DryRun {
		pruned, err := st.CountPrunableEventsTx(sel)
		if err != nil {
			return nil, err
		}
		min, err := retainedMinimumTx(st)
		if err != nil {
			return nil, err
		}
		return &PruneResult{
			Pruned: pruned, RetainedMinimum: min, DryRun: true, HeldByRetention: held,
		}, nil
	}

	pruned, err := st.DeletePrunableEventsTx(sel)
	if err != nil {
		return nil, err
	}

	// The prune writes ONE EVENT OF ITS OWN, in the same transaction, AFTER
	// the delete. Its `seq` is therefore above everything it removed, so the
	// record of the deletion survives the deletion — and a consumer that hits
	// GONE and resumes at the new minimum reads, as its first event, the
	// explanation for the gap it was just told about.
	if pruned > 0 {
		data, err := json.Marshal(prunedEventData(q, pruned))
		if err != nil {
			return nil, fmt.Errorf("recording the prune: %w", err)
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventEventsPruned, RunID: q.BeforeRun, Data: string(data), AtMS: q.NowMS,
		}); err != nil {
			return nil, err
		}
	}

	if err := st.RaiseEventLowWatermarkTx(); err != nil {
		return nil, err
	}
	min, err := retainedMinimumTx(st)
	if err != nil {
		return nil, err
	}

	if err := commitWrite(tx); err != nil {
		return nil, fmt.Errorf("pruning events: %w", err)
	}
	return &PruneResult{
		Pruned: pruned, RetainedMinimum: min, HeldByRetention: held,
	}, nil
}

// validatePruneTarget enforces exactly one target, and it must be sane.
func validatePruneTarget(q PruneQuery) error {
	switch {
	case q.Before == 0 && q.BeforeRun == 0:
		return validationErr(
			"events prune needs a target: --before SEQ to prune below a cursor, " +
				"or --before-run RUN-N to prune one run's events")
	case q.Before != 0 && q.BeforeRun != 0:
		return validationErr(
			"--before and --before-run name different targets; pass one")
	case q.Before < 0:
		return validationErr("--before must not be negative: %d", q.Before)
	case q.RunID != 0 && q.BeforeRun != 0:
		return validationErr(
			"--run narrows --before; --before-run already names its run")
	}
	return nil
}

// pruneSelection names the events that the count, the DELETE, and the
// live-run check all run over.
//
// ONE SELECTION, THREE STATEMENTS. The count a caller is shown, the rows that
// are deleted, and the runs that are checked must be the same set — two filters
// written separately is how a verb ends up reporting one number, deleting
// another, and validating a third.
func pruneSelection(q PruneQuery) store.EventPruneSelection {
	if q.BeforeRun != 0 {
		return store.EventPruneSelection{RunID: q.BeforeRun}
	}
	return store.EventPruneSelection{BeforeSeq: q.Before, RunID: q.RunID}
}

// refuseLiveRunsTx enforces that an event belonging to a run that is not
// `done` or `abandoned` is never deleted.
//
// IT REFUSES RATHER THAN SKIPPING. A prune that quietly retained half its
// range would leave an operator believing space was reclaimed and a consumer
// believing a boundary had moved — and the second of those is the failure GONE
// exists to prevent, arriving through the back door.
//
// WHY NON-TERMINAL IS THE LINE. A live run's events are LOAD-BEARING, not merely
// interesting: the budget floor is a SUM over its `step-claimed` events, and
// the saga's resume path reads `gate-started` events to decide whether a gate
// already ran. Pruning a live run's log would not lose an
// audit trail — it would change what the engine COMPUTES, silently, in the
// direction of running a gate twice and letting a run spend past its cap.
//
// A TERMINAL run's events are not unused either: they are deletable because
// what still reads them detects the loss. The recorded body read-back
// (recordedIssueBody in context.go) reconstructs the description a handed-out
// step was given from the run's `issue-body-refreshed` rows. When a prune has
// removed a step's claim event, and with it any refresh row the read-back
// needs, that read-back reports the recorded body unavailable (GONE) rather
// than stating the live description as the body the step was handed.
//
// Events with NO RUN — trust grants — pass this check. There is no run
// whose liveness could forbid them, and refusing them would make the repo-wide
// feed unprunable forever.
func refuseLiveRunsTx(st store.ReadTx, sel store.EventPruneSelection) error {
	runs, err := st.PrunableEventRunsTx(sel)
	if err != nil {
		return err
	}

	var live []string
	for _, run := range runs {
		if !run.Status.Terminal() {
			live = append(live, fmt.Sprintf("%s (%s)", model.FormatRunID(run.RunID), run.Status))
		}
	}
	if len(live) == 0 {
		return nil
	}
	sort.Strings(live)

	return conflictErr(
		"these runs have not finished, and their events are what the engine "+
			"computes from: %s — a run's budget floor is summed from its claim "+
			"events and its saga resumes from its gate events, so pruning them "+
			"would change the run rather than only its record. Prune after the "+
			"run reaches done or abandoned",
		strings.Join(live, ", "))
}

// retainedMinimumTx reports the lowest seq no prune has deleted, the
// watermark plus 1, which is what a consumer's cursor must not fall below.
func retainedMinimumTx(st store.ReadTx) (int64, error) {
	watermark, err := st.EventLowWatermarkTx()
	if err != nil {
		return 0, err
	}
	return watermark + 1, nil
}

// prunedEventData is the `events-pruned` payload: what was asked for, and what
// went.
func prunedEventData(q PruneQuery, pruned int) map[string]any {
	data := map[string]any{"deleted": pruned}
	if q.BeforeRun != 0 {
		data["before_run"] = model.FormatRunID(q.BeforeRun)
	} else {
		data["before"] = q.Before
	}
	if q.RunID != 0 {
		data["run"] = model.FormatRunID(q.RunID)
	}
	return data
}
