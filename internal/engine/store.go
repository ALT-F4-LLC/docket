package engine

import (
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// The transaction-scoped reads and writes the fat transaction needs.
//
// Activation is seven stages that either all happen or none do, so it needs
// every read and write on ONE transaction — a stage reading through the pool
// mid-transaction would see pre-transaction state and a stage writing through
// it would survive the rollback. These helpers run the Store's Tx methods
// inside the caller's transaction and add the engine's error context.

// loadIssues reads a run's issues, with their labels, inside the transaction.
//
// Labels come along because binding evaluates `[match]` against them and the
// snapshot freezes them — two stages that must see the same list, which they
// will only do if it is read once.
func loadIssues(tx store.LegacyTx, runIssues []*store.RunIssue) (map[int]*model.Issue, error) {
	out := make(map[int]*model.Issue, len(runIssues))

	stx := store.TxOn(tx)
	for _, ri := range runIssues {
		issue, err := issueTx(stx, ri.IssueID)
		if err != nil {
			return nil, err
		}
		labels, err := stx.IssueLabelsTx(ri.IssueID)
		if err != nil {
			return nil, err
		}
		issue.Labels = labels
		out[issue.ID] = issue
	}

	return out, nil
}

// issueTx reads the issue fields binding, snapshotting, and promotion need.
// It reads a subset on purpose: activation has no business with an issue's
// assignee or parent, and a SELECT * would invite a later stage to use one.
// `size` joined `priority` and `kind` here for the same reason: binding
// evaluates `[match].sizes_any` against it (schema v34), exactly as it
// already evaluates `kind` against `[match].kind`.
func issueTx(tx store.ReadTx, id int) (*model.Issue, error) {
	issue, err := tx.ActivationIssueTx(id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFoundErr(store.ErrNotFound, "issue %s not found", model.FormatID(id))
	}
	if err != nil {
		return nil, err
	}
	return issue, nil
}

// relationsAmongTx reads the directional relations among a set of issues — the
// edges planner.BuildDAG normalizes into the work DAG.
//
// Relations naming an issue outside the run are read anyway and dropped by
// BuildDAG, which ignores edges to absent nodes. That is the right behavior:
// an issue depending on something the run does not schedule is not a cycle and
// not an error, it is simply an edge the run has no say over.
func relationsAmongTx(tx store.LegacyTx, issues []*model.Issue) ([]model.Relation, error) {
	ids := make([]int, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return store.TxOn(tx).WorkGraphRelationsTx(ids)
}

// promoteIssueTx is stage 7's per-issue half: `backlog -> todo`, via the same
// column and the same activity trail an ordinary `issue move` writes
// (promotion goes through the issue verbs).
//
// `updated_at` stays RFC3339 TEXT, because that is what every existing verb
// emits and rewriting it to epoch-ms would change their output (the
// never-mutate rule). The CAS `version` bumps, so a concurrent writer holding a
// stale version fails rather than clobbering the promotion.
//
// An issue someone moved out of `backlog` between the read and this write is
// not a failure: promotion advances an issue that has not started, and one
// that has started needs no advancing.
func promoteIssueTx(tx store.LegacyTx, issueID int, nowMS int64) error {
	if _, err := moveIssueStatusTx(tx, issueID, model.StatusBacklog, model.StatusTodo, nowMS); err != nil {
		return fmt.Errorf("promoting %s: %w", model.FormatID(issueID), err)
	}
	return nil
}

// moveIssueStatusTx moves an issue between two statuses other than `done`
// (that stays `completeIssue`'s own), guarded by `status = from` (DKT-294):
// that guard IS the concurrency check, and an issue some other write already
// moved is a no-op, not a failure.
//
// It reports whether the row actually moved, so a caller only comments or
// events on a REAL transition rather than on every call that finds the issue
// already where it was headed.
func moveIssueStatusTx(
	tx store.LegacyTx, issueID int, from, to model.Status, nowMS int64,
) (bool, error) {
	return store.TxOn(tx).MoveIssueStatusTx(issueID, from, to, nowMS)
}

// commentEngineEvent drops one line of DKT-294's activity trail — an
// engine-authored comment, in the caller's own transaction, via DKT-293's
// tx-safe writer. It never opens or commits a transaction of its own, so the
// comment lands in the SAME transaction as the step-status write it narrates,
// stamped from the SAME `nowMS` — the transition and its narration carry one
// time, not two clock reads.
func commentEngineEvent(tx store.LegacyTx, issueID int, body string, nowMS int64) error {
	if _, err := store.TxOn(tx).InsertEngineComment(issueID, body, nowMS); err != nil {
		return fmt.Errorf("recording an activity comment on %s: %w", model.FormatID(issueID), err)
	}
	return nil
}

// releaseAbandonedIssueTx returns an abandoned issue to `todo` — the status the
// run found it in — rather than leaving it stranded in a live status (DKT-377).
//
// Abandonment deliberately does not force a TERMINAL status: `abandon-issue`
// says this RUN stops working the issue, and closing it would take the
// operator's triage decision away from them. That reasoning predates DKT-294's
// live mirror, and the mirror is what turned "leave the status alone" into a
// strand: before it, nothing could write `in-progress` or `review`, so an
// abandoned issue was still sitting at `todo` and both ready-set filters
// (internal/cli/next.go, internal/planner/plan.go, each `[backlog, todo]`)
// surfaced it. Now the issue can be abandoned FROM those two statuses, and
// nothing anywhere writes an issue back to `todo` — so the issue the operator
// was invited to triage by hand is one no listing offers them.
//
// `todo` is the honest destination precisely because it is not a verdict. It
// undoes the mirror's own writes and nothing else; the fact that the run
// abandoned the issue is carried by `resolution = abandoned`, the
// `issue-abandoned` event, and the trail comment — three records that survive
// the status move.
//
// It is also what ENDS `review` on the one park-clearing path the mirror does
// not see: the abandon cascade terminalizes a `waiting-human` step without
// routing, so no `reflectIssueStatus` call is left to notice the park count
// reach zero.
//
// Reports whether the row moved. `false` means the issue was already outside
// the mirror's live range (`todo` — abandoned before any step was claimed) or
// `done`, and there is nothing to undo.
func releaseAbandonedIssueTx(tx store.LegacyTx, issueID int, nowMS int64) (bool, error) {
	for _, from := range []model.Status{model.StatusReview, model.StatusInProgress} {
		moved, err := moveIssueStatusTx(tx, issueID, from, model.StatusTodo, nowMS)
		if err != nil {
			return false, err
		}
		if moved {
			return true, nil
		}
	}
	return false, nil
}

// reflectIssueOnClaim is claim's half of DKT-294: the FIRST claim against a
// `todo` issue flips it to `in-progress`, in the SAME transaction the claim
// itself commits in (the issue mirror shares one transaction).
//
// The activity-trail comment is UNCONDITIONAL on every claim, not gated on the
// flip: a second, third, or tenth step claimed against an issue already
// `in-progress` still narrates ITS OWN claim — the trail is about the step
// claimed, not about whether the issue's status happened to move because of
// it.
func reflectIssueOnClaim(tx store.LegacyTx, step *store.Step, nowMS int64) error {
	moved, err := moveIssueStatusTx(tx, step.IssueID, model.StatusTodo, model.StatusInProgress, nowMS)
	if err != nil {
		return err
	}
	if moved {
		if err := recordEvent(tx, eventRecord{
			Kind: EventIssueInProgress, RunID: step.RunID,
			Instance: step.Instance, IssueID: step.IssueID, AtMS: nowMS,
		}); err != nil {
			return err
		}
	}
	return commentEngineEvent(tx, step.IssueID, fmt.Sprintf("%s claimed.", step.Instance), nowMS)
}

// setRunActiveTx is stage 7's run half: status -> `active` and
// `activated_at_ms` set.
//
// A RE-ACTIVATION leaves `activated_at_ms` alone. The column records when the
// run was first activated — the moment its pins and snapshots were taken — and
// overwriting it on every re-activation would erase the timestamp the pin set
// actually corresponds to.
func setRunActiveTx(tx store.LegacyTx, runID int, reactivation bool, nowMS int64) error {
	if err := store.TxOn(tx).SetRunActiveTx(runID, reactivation, nowMS); err != nil {
		return fmt.Errorf("activating run %s: %w", model.FormatRunID(runID), err)
	}
	return nil
}
