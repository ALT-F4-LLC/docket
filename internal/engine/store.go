package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The transaction-scoped reads and writes the fat transaction needs.
//
// internal/db's API is *sql.DB-based, which is right for verbs that do one
// thing. Activation is seven stages that either all happen or none do, so it
// needs every read and write on ONE *sql.Tx — a stage reading through the pool
// mid-transaction would see pre-transaction state and a stage writing through
// it would survive the rollback. These helpers exist for that, and only that:
// they duplicate no logic, they re-scope it.

// loadIssues reads a run's issues, with their labels, inside the transaction.
//
// Labels come along because binding evaluates `[match]` against them and the
// snapshot freezes them — two stages that must see the same list, which they
// will only do if it is read once.
func loadIssues(tx *sql.Tx, runIssues []*db.RunIssue) (map[int]*model.Issue, error) {
	out := make(map[int]*model.Issue, len(runIssues))

	for _, ri := range runIssues {
		issue, err := issueTx(tx, ri.IssueID)
		if err != nil {
			return nil, err
		}
		labels, err := issueLabelsTx(tx, ri.IssueID)
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
func issueTx(tx *sql.Tx, id int) (*model.Issue, error) {
	var (
		issue       model.Issue
		description sql.NullString
		createdAt   string
		updatedAt   string
	)
	err := tx.QueryRow(
		`SELECT id, title, description, status, priority, kind, created_at, updated_at, version
		   FROM issues WHERE id = ?`, id,
	).Scan(
		&issue.ID, &issue.Title, &description, &issue.Status, &issue.Priority,
		&issue.Kind, &createdAt, &updatedAt, &issue.Version,
	)
	if err == sql.ErrNoRows {
		return nil, notFoundErr(db.ErrNotFound, "issue %s not found", model.FormatID(id))
	}
	if err != nil {
		return nil, fmt.Errorf("reading issue %s: %w", model.FormatID(id), err)
	}
	issue.Description = description.String
	return &issue, nil
}

// issueLabelsTx reads an issue's label names, sorted. The order is stable
// because the snapshot is canonical JSON that gets golden-diffed (§8.3): a
// label list in join order would make the goldens flap on an unrelated insert.
func issueLabelsTx(tx *sql.Tx, issueID int) ([]string, error) {
	rows, err := tx.Query(
		`SELECT l.name FROM labels l
		   JOIN issue_labels il ON il.label_id = l.id
		  WHERE il.issue_id = ? ORDER BY l.name`, issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("reading labels for %s: %w", model.FormatID(issueID), err)
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("reading label: %w", err)
		}
		labels = append(labels, name)
	}
	return labels, rows.Err()
}

// relationsAmongTx reads the directional relations among a set of issues — the
// edges planner.BuildDAG normalizes into the work DAG.
//
// Relations naming an issue outside the run are read anyway and dropped by
// BuildDAG, which ignores edges to absent nodes. That is the right behavior:
// an issue depending on something the run does not schedule is not a cycle and
// not an error, it is simply an edge the run has no say over.
func relationsAmongTx(tx *sql.Tx, issues []*model.Issue) ([]model.Relation, error) {
	if len(issues) == 0 {
		return nil, nil
	}

	ids := make([]any, 0, len(issues))
	placeholders := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
		placeholders = append(placeholders, "?")
	}
	args := append(append([]any{}, ids...), ids...)

	query := `SELECT source_issue_id, target_issue_id, relation_type
	            FROM issue_relations
	           WHERE relation_type IN ('blocks', 'depends_on')
	             AND source_issue_id IN (` + strings.Join(placeholders, ", ") + `)
	             AND target_issue_id IN (` + strings.Join(placeholders, ", ") + `)
	           ORDER BY id`

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading work-graph relations: %w", err)
	}
	defer rows.Close()

	var relations []model.Relation
	for rows.Next() {
		var rel model.Relation
		if err := rows.Scan(&rel.SourceIssueID, &rel.TargetIssueID, &rel.RelationType); err != nil {
			return nil, fmt.Errorf("reading relation: %w", err)
		}
		relations = append(relations, rel)
	}
	return relations, rows.Err()
}

// promoteIssueTx is stage 7's per-issue half: `backlog -> todo`, via the same
// column and the same activity trail an ordinary `issue move` writes
// (engine-spec §2: "promotion via the issue verbs").
//
// `updated_at` stays RFC3339 TEXT, because that is what every existing verb
// emits and rewriting it to epoch-ms would change their output (§9 item 8's
// never-mutate rule). The CAS `version` bumps, so a concurrent writer holding a
// stale version fails rather than clobbering the promotion.
func promoteIssueTx(tx *sql.Tx, issueID int, nowMS int64) error {
	now := time.UnixMilli(nowMS).UTC().Format(time.RFC3339)

	res, err := tx.Exec(
		`UPDATE issues SET status = ?, updated_at = ?, version = version + 1
		  WHERE id = ? AND status = ?`,
		model.StatusTodo, now, issueID, model.StatusBacklog,
	)
	if err != nil {
		return fmt.Errorf("promoting %s: %w", model.FormatID(issueID), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("promoting %s: %w", model.FormatID(issueID), err)
	}
	if n == 0 {
		// Someone moved it out of `backlog` between the read and this write.
		// That is not a failure: promotion advances an issue that has not
		// started, and one that has started needs no advancing.
		return nil
	}

	_, err = tx.Exec(
		`INSERT INTO activity_log (issue_id, field_changed, old_value, new_value, changed_by, created_at)
		 VALUES (?, 'status', ?, ?, '', ?)`,
		issueID, model.StatusBacklog, model.StatusTodo, now,
	)
	if err != nil {
		return fmt.Errorf("recording promotion of %s: %w", model.FormatID(issueID), err)
	}
	return nil
}

// moveIssueStatusTx moves an issue between two statuses other than `backlog`
// and `done` (those stay `promoteIssueTx`'s and `completeIssue`'s own),
// guarded by the same raw-tx-with-WHERE-guard shape those two writers use
// (DKT-294): the WHERE clause's `status = from` IS the concurrency guard, and
// `n == 0` means some other write already moved it — a no-op, not a failure,
// exactly as `promoteIssueTx` treats it.
//
// It reports whether the row actually moved, so a caller only comments or
// events on a REAL transition rather than on every call that finds the issue
// already where it was headed.
func moveIssueStatusTx(
	tx *sql.Tx, issueID int, from, to model.Status, nowMS int64,
) (bool, error) {
	now := time.UnixMilli(nowMS).UTC().Format(time.RFC3339)

	res, err := tx.Exec(
		`UPDATE issues SET status = ?, updated_at = ?, version = version + 1
		  WHERE id = ? AND status = ?`,
		string(to), now, issueID, string(from),
	)
	if err != nil {
		return false, fmt.Errorf("moving %s to %s: %w", model.FormatID(issueID), to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("moving %s to %s: %w", model.FormatID(issueID), to, err)
	}
	if n == 0 {
		return false, nil
	}

	_, err = tx.Exec(
		`INSERT INTO activity_log (issue_id, field_changed, old_value, new_value, changed_by, created_at)
		 VALUES (?, 'status', ?, ?, '', ?)`,
		issueID, string(from), string(to), now,
	)
	if err != nil {
		return false, fmt.Errorf("recording the move of %s to %s: %w", model.FormatID(issueID), to, err)
	}
	return true, nil
}

// commentEngineEvent drops one line of DKT-294's activity trail — an
// engine-authored comment, in the caller's own transaction, via DKT-293's
// tx-safe writer. It never opens or commits a transaction of its own, so the
// comment lands in the SAME transaction as the step-status write it narrates,
// stamped from the SAME `nowMS` — the transition and its narration carry one
// time, not two clock reads.
func commentEngineEvent(tx *sql.Tx, issueID int, body string, nowMS int64) error {
	if _, err := db.InsertEngineComment(tx, issueID, body, nowMS); err != nil {
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
func releaseAbandonedIssueTx(tx *sql.Tx, issueID int, nowMS int64) (bool, error) {
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
// itself commits in (engine-spec: "the issue mirror ... one transaction").
//
// The activity-trail comment is UNCONDITIONAL on every claim, not gated on the
// flip: a second, third, or tenth step claimed against an issue already
// `in-progress` still narrates ITS OWN claim — the trail is about the step
// claimed, not about whether the issue's status happened to move because of
// it.
func reflectIssueOnClaim(tx *sql.Tx, step *db.Step, nowMS int64) error {
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
func setRunActiveTx(tx *sql.Tx, runID int, reactivation bool, nowMS int64) error {
	query := `UPDATE runs SET status = ?, updated_at_ms = ?, row_version = row_version + 1`
	args := []any{string(model.RunActive), nowMS}
	if !reactivation {
		query += `, activated_at_ms = ?`
		args = append(args, nowMS)
	}
	query += ` WHERE id = ?`
	args = append(args, runID)

	if _, err := tx.Exec(query, args...); err != nil {
		return fmt.Errorf("activating run %s: %w", model.FormatRunID(runID), err)
	}
	return nil
}
