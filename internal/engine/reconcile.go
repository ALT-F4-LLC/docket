package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The issue mirror and the run rollup — the second and third quarters of §6.8's
// routing transaction ("update step, issue mirror, run, events — ONE
// transaction spanning all four").
//
// They live here rather than inline in the saga because the crossing from STEP
// state back into the ISSUE vocabulary is worth being able to find. This file
// is where a routing makes that crossing; it is NOT the only file that writes
// `issues.status`, and a reader who greps here alone will miss the other two
// (DKT-379). The whole set is:
//
//   - reconcile.go (here) — reflectIssueStatus's `in-progress <-> review`
//     mirror, completeIssue's `-> done`, abandonIssue's release back to `todo`;
//     every routing transaction.
//   - store.go — promoteIssueTx's `backlog -> todo` at activation,
//     reflectIssueOnClaim's `todo -> in-progress` (claim.go calls it; a claim
//     never routes, so it cannot come through here), releaseAbandonedIssueTx,
//     and moveIssueStatusTx, the guarded UPDATE all of these share.
//   - internal/cli — the operator's own `issue move`, outside the engine
//     entirely.
//
// Keep this list current: it is the answer to "what can change an issue's
// status", and it is wrong the moment a fourth writer appears without a line
// here.

// reconcileIssueAndRun updates the issue and the run after a step routes,
// INSIDE the caller's routing transaction. `spec` is the routed step's pinned
// spec (nil when the caller has none), consulted for its threshold's
// interposed targets and nothing else.
func reconcileIssueAndRun(tx *sql.Tx, step *db.Step, spec *workflow.Step, routing string, nowMS int64) error {
	// BEFORE the completion check: an unrouted gate left `pending` is exactly
	// what issueStepsComplete must not still be counting (DKT-38).
	if err := skipUnroutedTargets(tx, step, spec, routing, nowMS); err != nil {
		return err
	}

	// Abandonment and the live-status mirror are MUTUALLY EXCLUSIVE, not
	// sequential. Abandonment decides the issue's fate for this run on its own
	// terms (abandonIssue's own rule: "the issue's own status is NOT forced to
	// a terminal value"), and reflecting `in-progress`/`review` over the top of
	// that would contradict the decision this same call just made.
	if routing == workflow.OnFailAbandonIssue {
		// AND IT RETURNS. The completion check below is not merely redundant
		// after an abandonment, it is WRONG (DKT-377): abandonIssue's cascade
		// terminalizes every remaining step of the issue, so
		// issueStepsComplete is true by construction on the very next line and
		// completeIssue — whose UPDATE guards on nothing but `status != done`
		// — forces the abandoned issue to `done`.
		//
		// That is the "✔ done for work a review reproduced as not fixing
		// anything" rendering DKT-245 was filed about. The resolution column
		// it added recorded the contradiction; it never removed it. An
		// abandoned issue is the opposite of a completed one, and the run
		// rollup below still counts its terminal steps correctly either way.
		if err := abandonIssue(tx, step, nowMS); err != nil {
			return err
		}
		return reconcileRun(tx, step.RunID, nowMS)
	}
	if err := reflectIssueStatus(tx, step, routing, nowMS); err != nil {
		return err
	}

	// The issue's own completion is evaluated over HIGHEST-ORDINAL INSTANCES
	// ONLY (§11.3), which phase 4's loops make a real distinction rather than
	// the trivial one it was at S3.
	complete, err := issueStepsComplete(tx, step.RunID, step.IssueID)
	if err != nil {
		return err
	}
	if complete {
		if err := completeIssue(tx, step, nowMS); err != nil {
			return err
		}
	}

	return reconcileRun(tx, step.RunID, nowMS)
}

// skipUnroutedTargets terminalizes the interposed gates a routed step did NOT
// route to (DKT-38's second piece). A threshold naming step targets is a
// choice among routings: the one it resolved to (if a target) becomes ready
// through routedTo's latch, and the rest were decided AGAINST — leaving them
// `pending` would block joins and issue completion forever, which is why "a
// readiness latch alone is not a fix". They are `skipped`, the same terminal
// status a false `when` produces, and for the same reason: the topology chose
// not to run them.
//
// The decision is read from the ROWS, not from the in-memory step: every
// caller records the routing (SetStepRoutingTx) before reconciling, so the
// current sibling's own verdict is already on disk. For a fanned routing step
// the skip waits for the JOIN — while any sibling is non-terminal the choice
// is still open (a later sibling may route to the target), and that sibling's
// own reconcile re-runs this check. Only `done` siblings' routings count as
// having routed-to: an on_fail routing never produces `done` naming a target,
// and a superseded lineage's late routing is inert by §7.3 (3) — the same two
// readings routedTo makes, so the latch and the skip cannot disagree.
//
// A `retry` resolution reads as a non-terminal sibling (the row returned to
// `pending`) and correctly defers; a `waiting-human` park defers the same way
// until an operator's resolution re-enters here with a terminal routing.
func skipUnroutedTargets(
	tx *sql.Tx, step *db.Step, spec *workflow.Step, routing string, nowMS int64,
) error {
	if spec == nil {
		return nil
	}
	targets := workflow.ThresholdTargets(spec.Threshold)
	if len(targets) == 0 {
		return nil
	}

	routed, joined, err := targetRoutings(tx, step, targets)
	if err != nil {
		return err
	}
	if !joined {
		return nil // The join is still open; a later routing re-runs this.
	}

	for _, target := range targets {
		if routed[target] {
			continue
		}
		gates, err := pendingTargetInstances(tx, step, target)
		if err != nil {
			return err
		}
		for _, g := range gates {
			if err := db.SetStepStatusTx(tx, g.id, db.StepSkipped, nowMS, nowMS); err != nil {
				return err
			}
			if err := recordEvent(tx, eventRecord{
				Kind: EventStepSkipped, RunID: step.RunID,
				Instance: g.instance, IssueID: step.IssueID,
				Data: fmt.Sprintf("interposed gate not routed to: %s routed %s",
					step.Instance, routing),
				AtMS: nowMS,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// targetRoutings reads the routing step's sibling rows at its own ordinal and
// reports which targets a `done` sibling's recorded routing names. `joined` is
// false while any sibling is non-terminal — the choice is still open.
func targetRoutings(
	tx *sql.Tx, step *db.Step, targets []string,
) (routed map[string]bool, joined bool, err error) {
	rows, err := tx.Query(
		`SELECT status, routing FROM steps
		  WHERE run_id = ? AND issue_id = ? AND step_name = ? AND ordinal = ?`,
		step.RunID, step.IssueID, step.StepName, step.Ordinal,
	)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s's siblings: %w", step.Instance, err)
	}
	defer rows.Close()

	routed = make(map[string]bool, len(targets))
	for rows.Next() {
		var status string
		var recorded sql.NullString
		if err := rows.Scan(&status, &recorded); err != nil {
			return nil, false, fmt.Errorf("reading a sibling of %s: %w", step.Instance, err)
		}
		if !db.StepTerminal(status) {
			return nil, false, nil
		}
		if status != db.StepDone {
			continue
		}
		for _, target := range targets {
			if routingIs(recorded.String, target) {
				routed[target] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("reading %s's siblings: %w", step.Instance, err)
	}
	return routed, true, nil
}

type pendingGate struct {
	id       int
	instance string
}

// pendingTargetInstances lists a target's `pending` instances at or below the
// routing step's ordinal: the gate this ordinal's routing decided against,
// plus any straggler a loop sweep left behind. Higher ordinals belong to
// routings that have not happened yet.
func pendingTargetInstances(tx *sql.Tx, step *db.Step, target string) ([]pendingGate, error) {
	rows, err := tx.Query(
		`SELECT id, instance FROM steps
		  WHERE run_id = ? AND issue_id = ? AND step_name = ?
		    AND status = ? AND ordinal <= ?`,
		step.RunID, step.IssueID, target, db.StepPending, step.Ordinal,
	)
	if err != nil {
		return nil, fmt.Errorf("reading unrouted target %q: %w", target, err)
	}
	defer rows.Close()

	var gates []pendingGate
	for rows.Next() {
		var g pendingGate
		if err := rows.Scan(&g.id, &g.instance); err != nil {
			return nil, fmt.Errorf("reading unrouted target %q: %w", target, err)
		}
		gates = append(gates, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading unrouted target %q: %w", target, err)
	}
	return gates, nil
}

// reflectIssueStatus is the routing half of DKT-294's live mirror — claim.go's
// `reflectIssueOnClaim` is the other half, for the one issue transition that
// never routes.
//
// It is computed FRESH against the steps table on every call, over "is any of
// the issue's steps parked `waiting-human` right now" — never a flag carried
// forward from a previous call — which is what makes the operator's
// "bidirectional, reflect live reality" decision fall out for free: a step that
// parks moves the issue INTO `review`; the same issue's LAST parked step being
// resolved or retried moves it back OUT, on whichever call happens to notice
// the count reach zero. Neither direction is a special case of the other.
//
// A loop entry does NOT clear a park: the supersede sweep takes `pending`
// instances only (loop.go), so a `waiting-human` step survives into the next
// round and the issue rightly stays in `review` until someone resolves it.
//
// A `fix-loop` routing additionally drops its own trail comment, unconditionally
// on every entry rather than gated on a status change: a second or third round
// is exactly as notable as the first, and the issue may already be
// `in-progress` when it happens (a straight `verify -> fix-loop` that was never
// parked at all).
func reflectIssueStatus(tx *sql.Tx, step *db.Step, routing string, nowMS int64) error {
	if routing == workflow.OnFailFixLoop {
		if err := commentEngineEvent(tx, step.IssueID, fmt.Sprintf(
			"%s did not pass review; starting another round.", step.Instance), nowMS); err != nil {
			return err
		}
	}

	reviewing, err := issueAwaitingReviewTx(tx, step.RunID, step.IssueID, nowMS)
	if err != nil {
		return err
	}

	var current string
	if err := tx.QueryRow(`SELECT status FROM issues WHERE id = ?`, step.IssueID).Scan(&current); err != nil {
		return fmt.Errorf("reading %s's status: %w", model.FormatID(step.IssueID), err)
	}

	switch {
	case reviewing && model.Status(current) == model.StatusInProgress:
		moved, err := moveIssueStatusTx(tx, step.IssueID, model.StatusInProgress, model.StatusReview, nowMS)
		if err != nil {
			return err
		}
		if !moved {
			return nil
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventIssueReview, RunID: step.RunID,
			Instance: step.Instance, IssueID: step.IssueID, AtMS: nowMS,
		}); err != nil {
			return err
		}
		return commentEngineEvent(tx, step.IssueID,
			fmt.Sprintf("%s is awaiting review.", step.Instance), nowMS)

	case !reviewing && model.Status(current) == model.StatusReview:
		moved, err := moveIssueStatusTx(tx, step.IssueID, model.StatusReview, model.StatusInProgress, nowMS)
		if err != nil {
			return err
		}
		if !moved {
			return nil
		}
		return recordEvent(tx, eventRecord{
			Kind: EventIssueInProgress, RunID: step.RunID,
			Instance: step.Instance, IssueID: step.IssueID, AtMS: nowMS,
		})
	}
	return nil
}

// issueAwaitingReviewTx reports whether the issue is currently blocked on a
// human decision — the fact `review` reports. Two shapes count:
//
//  1. ANY step `waiting-human`, whatever kind of step it is and however it
//     parked. A gate or vote awaiting a resolution is the common case, but an
//     executor step its own threshold parked counts identically, and the query
//     says so: it filters on the status alone. That breadth is deliberate;
//     narrowing to gate/vote kinds would leave the other parks unmirrored.
//
//  2. Any non-terminal MATERIALIZED step — an open hold (DKT-380). A held step
//     is minted `type=human` and `pending`, never `waiting-human`, and its
//     routing step sits `gated` (H8) so nothing downstream of it can advance.
//     The issue is blocked on a human by construction: the hold exists
//     precisely because the computation refused to decide. Counting the status
//     alone left the whole fixture's hold path reading `in-progress`, which is
//     the status for work in flight and there is none.
//
//  3. A DECLARED `type=human` or `type=vote` step sitting `pending` because its
//     TURN HAS COME (DKT-334) — an open gate awaiting approve/reject, or an
//     open vote proposal. Neither writes a status when it opens: the operator's
//     decision is what moves the row, so waiting for a status meant the issue
//     read `review` only AFTER the window closed, which is the opposite of what
//     the mirror is for.
//
// Shapes (1) and (2) are a status/column count and answer the overwhelming
// majority of calls. Shape (3) is the only one that needs a scheduler, because
// "pending because its turn has come" and "pending because three predecessors
// are still running" differ by R3 and nothing else — so it is guarded behind a
// cheap count of candidate rows, and a run whose workflow declares no gates
// never loads one. R3 is asked of the Scheduler (AwaitingDecision) rather than
// rewritten here: a second copy of the join rules next to the mirror is what
// would drift at the first fanout or interposed gate that mattered.
//
// It is a live computation, not a cache, which is what makes leaving `review`
// automatic the moment the last one clears (DKT-294).
func issueAwaitingReviewTx(tx *sql.Tx, runID, issueID int, nowMS int64) (bool, error) {
	var n int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM steps
		  WHERE run_id = ? AND issue_id = ?
		    AND (status = ? OR (materialized = 1 AND status NOT IN (?, ?, ?, ?)))`,
		runID, issueID, db.StepWaitingHuman,
		db.StepDone, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking %s for parked steps: %w", model.FormatID(issueID), err)
	}
	if n > 0 {
		return true, nil
	}
	return issueAwaitingDecisionTx(tx, runID, issueID, nowMS)
}

// issueAwaitingDecisionTx is shape (3) above: is any DECLARED gate of this
// issue open and undecided?
//
// The candidate count comes first and is the whole reason this is affordable on
// a hot path. Every routing transaction of every run calls it; a workflow with
// no `type=human`/`type=vote` step answers with one indexed count and no
// scheduler, and one that has them answers the same way at every ordinal before
// the gate's turn comes.
func issueAwaitingDecisionTx(tx *sql.Tx, runID, issueID int, nowMS int64) (bool, error) {
	var candidates int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM steps
		  WHERE run_id = ? AND issue_id = ? AND status = ?
		    AND materialized = 0 AND kind IN (?, ?)`,
		runID, issueID, db.StepPending, workflow.TypeHuman, workflow.TypeVote,
	).Scan(&candidates)
	if err != nil {
		return false, fmt.Errorf(
			"checking %s for open gates: %w", model.FormatID(issueID), err)
	}
	if candidates == 0 {
		return false, nil
	}

	defs, err := StepDefinitionsTx(tx, runID)
	if err != nil {
		return false, err
	}
	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return false, err
	}
	return sched.IssueAwaitingDecision(issueID), nil
}

// issueStepsComplete reports whether every step of an issue has reached a
// terminal status — evaluated over HIGHEST-ORDINAL INSTANCES ONLY (§11.3).
//
// `waiting-human` is NOT terminal here and must not be: an issue parked on an
// operator's decision is not finished, and treating it as such would let a run
// reach `done` with an unanswered question in it.
//
// THE ORDINAL FILTER IS PER STEP NAME, and that is the part a single
// issue-wide maximum gets wrong. After one loop entry the fixture's issue has
// `implement` at ordinal 0 only, and `review`/`verify`/`commit` at 0 and 1. An
// issue-wide "highest ordinal is 1" would exclude `implement@0` from the check
// entirely and call the issue complete without ever looking at it. Grouping by
// name asks the right question of each step: how did its LATEST instance end?
//
// Read the other way, this is also what stops a superseded lineage from
// blocking completion. `commit-gate@0` left `superseded` by a loop entry is not
// the highest instance of `commit-gate` once `commit-gate@1` exists, so it is
// not consulted; and were it consulted, `superseded` is terminal anyway. Both
// halves of §11.3's completion rule — ignore lower ordinals, and prior
// instances stay immutable and addressable — fall out of the same query.
func issueStepsComplete(tx *sql.Tx, runID, issueID int) (bool, error) {
	var pending int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM steps s
		  WHERE s.run_id = ? AND s.issue_id = ?
		    AND s.ordinal = (SELECT MAX(h.ordinal) FROM steps h
		                      WHERE h.run_id = s.run_id AND h.issue_id = s.issue_id
		                        AND h.step_name = s.step_name)
		    AND s.status NOT IN (?, ?, ?, ?)`,
		runID, issueID,
		db.StepDone, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
	).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("counting unfinished steps: %w", err)
	}
	return pending == 0, nil
}

// completeIssue moves an issue to `done` through the SAME column and the same
// activity trail an ordinary `issue close` writes.
//
// `updated_at` stays RFC3339 TEXT — the never-mutate rule (reliability-delta
// §2.1). Rewriting it to epoch-ms would change what every existing verb emits
// and break §9 item 8, which is why v7's own timestamps are separate `_ms`
// columns rather than a migration of this one.
func completeIssue(tx *sql.Tx, step *db.Step, nowMS int64) error {
	now := time.UnixMilli(nowMS).UTC().Format(time.RFC3339)

	res, err := tx.Exec(
		`UPDATE issues SET status = ?, updated_at = ?, version = version + 1
		  WHERE id = ? AND status != ?`,
		model.StatusDone, now, step.IssueID, model.StatusDone,
	)
	if err != nil {
		return fmt.Errorf("completing %s: %w", model.FormatID(step.IssueID), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("completing %s: %w", model.FormatID(step.IssueID), err)
	}
	if n == 0 {
		return nil // Already done; nothing to record twice.
	}

	_, err = tx.Exec(
		`INSERT INTO activity_log (issue_id, field_changed, old_value, new_value, changed_by, created_at)
		 VALUES (?, 'status', ?, ?, '', ?)`,
		step.IssueID, model.StatusInProgress, model.StatusDone, now,
	)
	if err != nil {
		return fmt.Errorf("recording completion of %s: %w", model.FormatID(step.IssueID), err)
	}
	return commentEngineEvent(tx, step.IssueID,
		fmt.Sprintf("%s completed the issue.", step.Instance), nowMS)
}

// abandonIssue is the `abandon-issue` routing (§11.1): the issue leaves the run
// unfinished, with the reason recorded.
//
// The issue's own status is NOT forced to a terminal value. `abandon-issue`
// says this RUN stops working on it, which is a statement about the run; an
// operator may still triage the issue by hand, and closing it here would take
// that decision away from them.
func abandonIssue(tx *sql.Tx, step *db.Step, nowMS int64) error {
	// Every not-yet-terminal step of the issue stops, AND EACH ONE RECORDS WHY
	// (DKT-258).
	//
	// The status alone was a lie of omission. `failed-routed` says a
	// measurement was taken and came back bad; these steps were never measured
	// and most were never claimed — RUN-24's reconcile, verify, and
	// verify-tribunal all rendered `failed-routed` and not one of them had
	// failed at anything. The routing column existed and this UPDATE simply
	// never wrote it, so a cascade-terminated step and a genuinely failed one
	// were byte-identical to every reader.
	//
	// It is written HERE rather than left to a renderer to infer, because the
	// inference is not available later: nothing downstream can distinguish an
	// empty routing on a `failed-routed` step from an old row written before
	// this change. Recording the fact at the moment it is known is the only
	// version of this that stays true.
	//
	// The status stays `failed-routed` deliberately. It is a TERMINAL status
	// and every terminal-set in this file — reconcileRun's rollup, the
	// re-offer predicate, the guard — already lists it; a tenth status would
	// have to be added to each of them correctly or a cascade-terminated step
	// would quietly become re-offerable. The distinction the issue asks for is
	// a distinction in the RECORD, and the record is what now carries it.
	cascade := routingRecord(workflow.OnFailAbandonIssue, fmt.Sprintf(
		"cascade: %s was abandoned by %s; this step was never measured",
		model.FormatID(step.IssueID), step.Instance))
	_, err := tx.Exec(
		`UPDATE steps
		    SET status = ?, routing = ?, updated_at_ms = ?,
		        row_version = row_version + 1
		  WHERE run_id = ? AND issue_id = ? AND id != ?
		    AND status NOT IN (?, ?, ?, ?)`,
		db.StepFailedRouted, cascade, nowMS, step.RunID, step.IssueID, step.ID,
		db.StepDone, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
	)
	if err != nil {
		return fmt.Errorf("abandoning %s: %w", model.FormatID(step.IssueID), err)
	}

	// The issue's own STATUS is left alone, for the reason above; what is
	// recorded on the row is the RESOLUTION (DKT-245). Without it the only
	// trace was an event, and an issue whose fix step had completed kept
	// rendering "✔ done" for work a review reproduced as not fixing anything.
	if err := db.SetIssueResolutionTx(tx, step.IssueID, db.IssueResolutionAbandoned); err != nil {
		return err
	}

	// ...and the LIVE status the mirror wrote is undone, so the issue the
	// paragraph above invites an operator to triage is one a listing still
	// offers them (DKT-377). This is also what ends `review` here: the cascade
	// above may have terminalized a `waiting-human` step, and no routing
	// transaction is left to notice the park count reach zero.
	if _, err := releaseAbandonedIssueTx(tx, step.IssueID, nowMS); err != nil {
		return err
	}

	if err := recordEvent(tx, eventRecord{
		Kind: EventIssueAbandoned, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
	}); err != nil {
		return err
	}
	return commentEngineEvent(tx, step.IssueID,
		fmt.Sprintf("%s abandoned the issue.", step.Instance), nowMS)
}

// reconcileRun rolls the run up: `done` when every step is terminal,
// `waiting-human` when any step is parked on a decision.
//
// The ORDER of the two checks matters. A run with one parked step and every
// other step finished is NOT done — it is waiting — so the park is checked
// first. Checking `done` first would let a run whose last unfinished step is
// parked read as complete, which is the failure mode §6.12's `guard stop`
// exists to catch and should never have to.
func reconcileRun(tx *sql.Tx, runID int, nowMS int64) error {
	var (
		parked     int
		unfinished int
	)
	err := tx.QueryRow(
		`SELECT
		   SUM(CASE WHEN status = ? THEN 1 ELSE 0 END),
		   SUM(CASE WHEN status NOT IN (?, ?, ?, ?) THEN 1 ELSE 0 END)
		 FROM steps WHERE run_id = ?`,
		db.StepWaitingHuman,
		db.StepDone, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted,
		runID,
	).Scan(&parked, &unfinished)
	if err != nil {
		return fmt.Errorf("rolling up run %s: %w", model.FormatRunID(runID), err)
	}

	// A run with UNEXPANDED issues is not done however its expanded steps read:
	// later phases expand when their predecessors complete, and a rollup blind
	// to them would call a run finished with work it has not scheduled yet.
	var unexpanded int
	err = tx.QueryRow(
		`SELECT COUNT(*) FROM run_issues WHERE run_id = ? AND expanded_at_ms IS NULL`,
		runID,
	).Scan(&unexpanded)
	if err != nil {
		return fmt.Errorf("counting unexpanded issues: %w", err)
	}

	switch {
	case parked > 0:
		return setRunStatusTx(tx, runID, model.RunWaitingHuman, EventRunPaused, nowMS)
	case unfinished == 0 && unexpanded == 0:
		// A run-level park does NOT hold this branch, and the guard below is
		// deliberately the only place that reads the origin. `done` resumes
		// nothing — it records that the work finished — and a paused run held
		// short of it would be unreachable: no step remains to route, so
		// nothing would ever roll it up again and `run resume` would leave it
		// parked forever with an empty queue. What the operator's pause buys
		// is that no FURTHER work starts, which R1 and the guard below deliver.
		return setRunStatusTx(tx, runID, model.RunDone, EventRunDone, nowMS)
	default:
		// Still working. A run previously parked and now unblocked returns to
		// `active` — otherwise resolving the park would leave the run in
		// `waiting-human` forever and R1 would refuse every remaining step.
		//
		// EXCEPT when the park was decided at the RUN LEVEL rather than by a
		// step. `parked` above counts STEPS, and a run-level pause parks none:
		// `BreachRunBudgetTx` (internal/db/usage.go) and `MoveRun`'s `run
		// pause` both flip only the RUN row. So a run-level park reads as 0
		// parked here even while it sits unresolved, and the next step to route
		// through this rollup — one claimed before the pause, finishing after
		// it — lands in this branch and flips the run back to `active` with no
		// operator verb behind it: a `run-resumed` event carrying no data and
		// naming nobody.
		//
		// DKT-68 caught the budget case (the flap RUN-5 hit at seq 637/640/643,
		// where the very next over-cap claim re-paused what this branch had
		// just resumed) and guarded it by reading `breach_reason`. DKT-305 is
		// the same defect with the other origin: RUN-31's operator paused at
		// seq 3054 and this branch resumed the run at seq 3077, after which a
		// four-judge review, a synthesize, a reconcile, and two fresh votes ran
		// unattended on budget the operator had asked to stop spending. A plain
		// pause sets no `breach_reason` — nor should it, that column is the
		// budget's own record — so the guard reads `pause_origin` instead,
		// which every run-level pause writes and no step-level park does.
		//
		// Ending a run-level park is an OPERATOR decision — `run resume`, or
		// for a breach a cap raise via `run budget --set` that resolves it and
		// releases the hold with it (DKT-80, docs/tdd/runs-dispatch.md
		// B24/B25/B26) — and this automatic rollup must not make that decision
		// on the run's behalf. A run currently at `waiting-human` with an
		// origin therefore stays parked here: no status write, no event.
		//
		// A STEP-parked run is untouched by this. Its origin is empty, the
		// `parked > 0` case above wrote its pause, and resolving the park
		// returns it to `active` right here — without which every step-level
		// park would need an operator to type `run resume` for a decision the
		// engine made and the engine already unmade.
		origin, err := db.RunPauseOriginTx(tx, runID)
		if err != nil {
			return err
		}
		if origin.RunLevel() {
			var status string
			if err := tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&status); err != nil {
				return fmt.Errorf("reading run %s status: %w", model.FormatRunID(runID), err)
			}
			if model.RunStatus(status) == model.RunWaitingHuman {
				return nil
			}
		}
		return setRunStatusTx(tx, runID, model.RunActive, EventRunResumed, nowMS)
	}
}

// setRunStatusTx moves a run's status only when it CHANGES, so an unchanged
// rollup writes nothing and emits no event.
//
// The no-op guard is what keeps the event ledger meaningful: without it, every
// step completion in a long run would emit a `run-resumed` for a run that never
// paused, and §9 item 2's "every transition is attributable" would be checking
// a table of non-transitions.
func setRunStatusTx(
	tx *sql.Tx, runID int, status model.RunStatus, event string, nowMS int64,
) error {
	var current string
	if err := tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, runID).Scan(&current); err != nil {
		return fmt.Errorf("reading run status: %w", err)
	}
	if model.RunStatus(current) == status {
		return nil
	}
	// A terminal run is never revived by a rollup. Reaching here means a step
	// changed under a `done` or `abandoned` run, which is a state the verbs
	// refuse to create; the rollup declines to paper over it.
	if model.RunStatus(current).Terminal() {
		return nil
	}

	_, err := tx.Exec(
		`UPDATE runs SET status = ?, updated_at_ms = ?, row_version = row_version + 1
		  WHERE id = ?`,
		string(status), nowMS, runID,
	)
	if err != nil {
		return fmt.Errorf("updating run %s: %w", model.FormatRunID(runID), err)
	}

	// THE EVENT SAYS THE ROLLUP DID IT (DKT-304). Every one of these three
	// transitions has an operator verb that writes the same kind, and `MoveRun`
	// carries `from`, `to`, and the operator's `reason` in `data` — so a
	// rollup writing `{}` was indistinguishable in the feed from a person
	// typing the verb. RUN-30's operator paused mid-wave, saw a `run-resumed`
	// with an empty `data` one millisecond after a step routed, and had no way
	// to tell from the trail that nobody had asked for it.
	//
	// This follows the convention the actor table already states for
	// `run-paused`: the kind's ACTOR stays what the transition MEANS, and
	// `data.reason` carries which route produced it. A new event kind per
	// origin would split every consumer's switch for a distinction that is one
	// field.
	data, err := json.Marshal(map[string]any{
		"from": current, "to": string(status), "reason": runRollupReason,
	})
	if err != nil {
		return fmt.Errorf("recording the rollup of run %s: %w",
			model.FormatRunID(runID), err)
	}
	return recordEvent(tx, eventRecord{
		Kind: event, RunID: runID, Data: string(data), AtMS: nowMS,
	})
}

// runRollupReason is the `data.reason` every reconcileRun-written lifecycle
// event carries, and the one value no operator verb can produce: `MoveRun`'s
// reason is free text the operator supplies, and a person who typed this
// string would be describing what happened accurately anyway.
const runRollupReason = "rollup"
