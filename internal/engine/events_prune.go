package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// `docket events prune` — engine-spec §3's retention clause, implemented
// (docs/tdd/events-follow.md §5).
//
// THIS IS THE FIRST DESTRUCTIVE VERB IN THE ENGINE SURFACE, and everything
// interesting about it is what it REFUSES to delete. §3 names both refusals in
// one sentence:
//
//	`events prune` refuses events of non-terminal runs and never crosses the
//	artifact-retention boundary
//
// Each is implemented below with its own predicate, its own error, and its own
// test, because they fail for different reasons and an operator hitting one
// needs to know which.
//
// WHAT IT DELETES: rows in `events`. Nothing else — no artifact, no step, no
// run, no usage row, no dispatch row (P16). It does not VACUUM (P17):
// reclaiming file space is a decision made against a backup, and a verb that
// rewrote the whole database as a side effect of trimming a log would be a
// surprise with a different risk profile.

// PruneQuery is one prune's target and its clock.
//
// EXACTLY ONE OF Before / BeforeRun IS SET (P1). A destructive verb with a
// default target is how a log gets deleted by a typo, so "prune everything" is
// not expressible: the caller must name a boundary or a run.
type PruneQuery struct {
	// Before deletes events with `seq < Before` — STRICTLY LESS (P2), so
	// `--before N` and a cursor at `N-1` name the same boundary, which is the
	// convention every other seq comparison in this feed already follows.
	Before int64
	// BeforeRun deletes every event of one run, whatever its seq (P3). It is the
	// form operations.md §2 recommends — trim whole runs that reached a terminal
	// status, rather than the oldest N events across all of them, so the report
	// stays honest for every run still covered.
	BeforeRun int
	// RunID narrows Before to one run (P4). Zero is every run.
	RunID int
	// DryRun computes the answer and deletes nothing (P5).
	DryRun bool
	// NowMS is the transaction's clock, used for the retention boundary and for
	// the `events-pruned` event's timestamp. It is passed IN rather than read
	// here so the boundary and the event agree, and so a test can place the
	// window without waiting for wall-clock time to pass.
	NowMS int64
}

// PruneResult is what a prune did, or would have done (P7).
//
// It reports the NEW RETAINED MINIMUM alongside the count so a consumer can set
// its cursor without a second call — which matters because the call that
// invalidated the cursor is exactly this one.
type PruneResult struct {
	Pruned          int   `json:"pruned"`
	RetainedMinimum int64 `json:"retained_minimum"`
	DryRun          bool  `json:"dry_run,omitempty"`
	// HeldByRetention counts rows the boundary protected (P14). It is reported
	// rather than silently subtracted: an operator who asked to prune 900 events
	// and pruned 200 must be told a policy held back the other 700, or they will
	// conclude the verb is broken.
	HeldByRetention int `json:"held_by_retention,omitempty"`
	// LiveRuns names the non-terminal runs that blocked the prune, for the
	// refusal message (P9).
	LiveRuns []string `json:"-"`
}

// PruneEvents is the whole verb: validate, refuse, delete, log — in ONE
// transaction.
//
// THE REFUSAL PREDICATES ARE EVALUATED INSIDE THE DELETE'S TRANSACTION (F3), so
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
	retain, err := db.EventsRetain(conn)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("pruning events: %w", err)
	}
	defer tx.Rollback()

	if q.BeforeRun != 0 {
		if _, err := db.GetRunTx(tx, q.BeforeRun); err != nil {
			return nil, notFoundErr(err, "run %s not found",
				model.FormatRunID(q.BeforeRun))
		}
	}

	where, args := pruneFilter(q)

	// REFUSAL 1 (§5.2): events of non-terminal runs are never deleted.
	if err := refuseLiveRunsTx(tx, where, args); err != nil {
		return nil, err
	}

	// REFUSAL 2 (§5.3): the retention boundary.
	//
	// It CLAMPS rather than refuses outright (P14) — a `--before` reaching past
	// the window prunes what it may and reports what it could not — because the
	// boundary is a standing policy rather than a mistake in the command. A
	// refusal would make an operator compute the boundary themselves before
	// every prune, which is the arithmetic the policy exists to do for them.
	held := 0
	if retain > 0 && q.BeforeRun == 0 {
		cutoff := q.NowMS - retain.Milliseconds()
		held, err = countHeldByRetentionTx(tx, where, args, cutoff)
		if err != nil {
			return nil, err
		}
		where += ` AND at_ms < ?`
		args = append(args, cutoff)
	}

	var pruned int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM events`+where, args...,
	).Scan(&pruned); err != nil {
		return nil, fmt.Errorf("counting the events to prune: %w", err)
	}

	if q.DryRun {
		min, err := retainedMinimumTx(tx)
		if err != nil {
			return nil, err
		}
		return &PruneResult{
			Pruned: pruned, RetainedMinimum: min, DryRun: true, HeldByRetention: held,
		}, nil
	}

	if _, err := tx.Exec(`DELETE FROM events`+where, args...); err != nil {
		return nil, fmt.Errorf("pruning events: %w", err)
	}

	// P18: the prune writes ONE EVENT OF ITS OWN, in the same transaction, AFTER
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

	min, err := retainedMinimumTx(tx)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("pruning events: %w", err)
	}
	return &PruneResult{
		Pruned: pruned, RetainedMinimum: min, HeldByRetention: held,
	}, nil
}

// validatePruneTarget is P1: exactly one target, and it must be sane.
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

// pruneFilter builds the WHERE that the count, the DELETE, and the live-run
// check all run over.
//
// ONE FILTER, THREE STATEMENTS. The count a caller is shown, the rows that are
// deleted, and the runs that are checked must be the same set — two filters
// written separately is how a verb ends up reporting one number, deleting
// another, and validating a third.
//
// The columns are written UNQUALIFIED (`seq`, not `e.seq`) because `DELETE FROM
// events` cannot take an alias. The live-run check aliases its `events` as `e`
// but adds nothing of its own to this clause, so the unqualified names resolve
// against `events` there too — `runs` has no `seq` and its `id` is spelled
// differently from `run_id`, so there is nothing for them to collide with.
func pruneFilter(q PruneQuery) (string, []any) {
	if q.BeforeRun != 0 {
		return ` WHERE run_id = ?`, []any{q.BeforeRun}
	}
	where := ` WHERE seq < ?`
	args := []any{q.Before}
	if q.RunID != 0 {
		where += ` AND run_id = ?`
		args = append(args, q.RunID)
	}
	return where, args
}

// refuseLiveRunsTx is §5.2: an event belonging to a run that is not `done` or
// `abandoned` is never deleted.
//
// IT REFUSES RATHER THAN SKIPPING (P9). A prune that quietly retained half its
// range would leave an operator believing space was reclaimed and a consumer
// believing a boundary had moved — and the second of those is the failure GONE
// exists to prevent, arriving through the back door.
//
// WHY NON-TERMINAL IS THE LINE. A live run's events are LOAD-BEARING, not merely
// interesting: the budget floor is a SUM over its `step-claimed` events (§4.3 of
// runs-dispatch), and the saga's resume path reads `gate-started` events to
// decide whether a gate already ran. Pruning a live run's log would not lose an
// audit trail — it would change what the engine COMPUTES, silently, in the
// direction of running a gate twice and letting a run spend past its cap.
//
// Events with NO RUN — trust grants — pass this check (P11). There is no run
// whose liveness could forbid them, and refusing them would make the repo-wide
// feed unprunable forever.
func refuseLiveRunsTx(tx *sql.Tx, where string, args []any) error {
	rows, err := tx.Query(
		`SELECT DISTINCT r.id, r.status
		   FROM events e JOIN runs r ON r.id = e.run_id`+where+` ORDER BY r.id`,
		args...)
	if err != nil {
		return fmt.Errorf("checking the runs a prune would touch: %w", err)
	}
	defer rows.Close()

	var live []string
	for rows.Next() {
		var (
			id     int
			status model.RunStatus
		)
		if err := rows.Scan(&id, &status); err != nil {
			return fmt.Errorf("reading a run's status: %w", err)
		}
		if !status.Terminal() {
			live = append(live, fmt.Sprintf("%s (%s)", model.FormatRunID(id), status))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("checking the runs a prune would touch: %w", err)
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

// countHeldByRetentionTx counts the rows the boundary protects, so P14's answer
// can name them.
func countHeldByRetentionTx(tx *sql.Tx, where string, args []any, cutoff int64) (int, error) {
	var held int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM events`+where+` AND at_ms >= ?`,
		append(append([]any{}, args...), cutoff)...,
	).Scan(&held)
	if err != nil {
		return 0, fmt.Errorf("counting the events the retention window holds: %w", err)
	}
	return held, nil
}

// retainedMinimumTx reports the lowest seq that survives, which is what a
// consumer's cursor must not fall below.
//
// An EMPTY table reports 0, which is the same thing `--since 0` means: start
// from the beginning. A repo whose every event was pruned has nothing to skip.
func retainedMinimumTx(tx *sql.Tx) (int64, error) {
	var min sql.NullInt64
	if err := tx.QueryRow(`SELECT MIN(seq) FROM events`).Scan(&min); err != nil {
		return 0, fmt.Errorf("reading the retained minimum: %w", err)
	}
	if !min.Valid {
		return 0, nil
	}
	return min.Int64, nil
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
