package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The saga queries: the reads and writes the engine's completion saga makes
// against artifacts, gate results, run fences, events and steps. Each runs
// inside the caller's transaction when its name ends in Tx, and on the pool
// otherwise. Engine vocabulary (artifact kinds, event kinds, step kinds)
// arrives as arguments; what a result means stays the engine's.

// SetStepWorkRootTx records the worktree a step's work happened in, or returns
// ErrStepNotFound.
func SetStepWorkRootTx(tx *sql.Tx, stepID int, workRoot string) error {
	res, err := tx.Exec(`UPDATE steps SET work_root = ? WHERE id = ?`, workRoot, stepID)
	if err != nil {
		return fmt.Errorf("recording the worktree for %s: %w", model.FormatStepID(stepID), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("recording the worktree for %s: %w", model.FormatStepID(stepID), err)
	}
	if n == 0 {
		return ErrStepNotFound
	}
	return nil
}

// StepArtifactCount counts the artifacts of one kind a step recorded.
func StepArtifactCount(db *sql.DB, stepID int, kind string) (int, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM artifacts WHERE step_id = ? AND kind = ?`,
		stepID, kind).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting %s's %s artifacts: %w", model.FormatStepID(stepID), kind, err)
	}
	return n, nil
}

// NewestStepArtifactBody returns the body of the newest artifact of one kind a
// step recorded. found is false when it recorded none.
func NewestStepArtifactBody(db *sql.DB, stepID int, kind string) (body string, found bool, err error) {
	err = db.QueryRow(
		`SELECT body FROM artifacts WHERE step_id = ? AND kind = ?
		  ORDER BY id DESC LIMIT 1`,
		stepID, kind).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s's recorded %s: %w", model.FormatStepID(stepID), kind, err)
	}
	return body, true, nil
}

// NextGateOrdinalTx is NextGateOrdinal inside a transaction: 0 when the
// step+gate has no recorded result.
func NextGateOrdinalTx(tx *sql.Tx, stepID int, gate string) (int, error) {
	var highest sql.NullInt64
	err := tx.QueryRow(
		`SELECT MAX(ordinal) FROM gate_results WHERE step_id = ? AND gate = ?`,
		stepID, gate).Scan(&highest)
	if err != nil {
		return 0, fmt.Errorf("reading the gate ordinal: %w", err)
	}
	if !highest.Valid {
		return 0, nil
	}
	return int(highest.Int64) + 1, nil
}

// NewestStepPayload returns the payload of the newest artifact a step
// recorded, of one kind, or of any kind when kind is "". found is false when
// there is none; a recorded artifact with no payload reads "".
func NewestStepPayload(db *sql.DB, stepID int, kind string) (payload string, found bool, err error) {
	query, args := `SELECT payload FROM artifacts WHERE step_id = ?
		 ORDER BY id DESC LIMIT 1`, []any{stepID}
	if kind != "" {
		query, args = `SELECT payload FROM artifacts WHERE step_id = ? AND kind = ?
			 ORDER BY id DESC LIMIT 1`, []any{stepID, kind}
	}
	var p sql.NullString
	err = db.QueryRow(query, args...).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s's recorded payload: %w", model.FormatStepID(stepID), err)
	}
	return p.String, true, nil
}

// RunIssueSnapshot returns the issue snapshot activation froze for an issue in
// a run, "" when none was frozen, or ErrNotFound when the issue is not in the
// run.
func RunIssueSnapshot(db *sql.DB, runID, issueID int) (string, error) {
	var snapshot sql.NullString
	err := db.QueryRow(
		`SELECT issue_snapshot FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID).Scan(&snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading the snapshot of %s: %w", model.FormatID(issueID), err)
	}
	return snapshot.String, nil
}

// NewestIssueArtifactPayloadExcept returns the payload of the newest artifact
// of one kind recorded by any of an issue's steps in the run other than
// exceptStepID. found is false when there is none; a recorded artifact with no
// payload reads "".
func NewestIssueArtifactPayloadExcept(
	db *sql.DB, runID, issueID int, kind string, exceptStepID int,
) (payload string, found bool, err error) {
	var p sql.NullString
	err = db.QueryRow(
		`SELECT a.payload FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ? AND a.step_id <> ?
		  ORDER BY a.id DESC LIMIT 1`,
		runID, issueID, kind, exceptStepID).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s's newest %s: %w", model.FormatID(issueID), kind, err)
	}
	return p.String, true, nil
}

// NewestIssueArtifactConsumedByDoneStep returns the id and payload of the
// newest artifact of one kind that a `done` step of the issue in the run took
// as an input while recording no artifact of that kind itself. found is false
// when there is none; an artifact with no payload reads "".
func NewestIssueArtifactConsumedByDoneStep(
	db *sql.DB, runID, issueID int, kind string,
) (artifactID int, payload string, found bool, err error) {
	var p sql.NullString
	err = db.QueryRow(
		`SELECT a.id, a.payload FROM step_inputs si
		   JOIN artifacts a ON a.id = si.artifact_id
		   JOIN steps s ON s.id = si.step_id
		  WHERE s.run_id = ? AND s.issue_id = ? AND s.status = ? AND a.kind = ?
		    AND NOT EXISTS (SELECT 1 FROM artifacts p
		                     WHERE p.step_id = s.id AND p.kind = a.kind)
		  ORDER BY a.id DESC LIMIT 1`,
		runID, issueID, StepDone, kind).Scan(&artifactID, &p)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("reading %s's newest consumed %s: %w",
			model.FormatID(issueID), kind, err)
	}
	return artifactID, p.String, true, nil
}

// FirstRecorderAfterQuery selects the artifacts of Kind recorded by an issue's
// steps in a run after artifact AfterArtifactID, by steps other than
// ExceptStepID that had recorded no artifact of Kind up to AfterArtifactID.
type FirstRecorderAfterQuery struct {
	RunID, IssueID  int
	Kind            string
	AfterArtifactID int
	ExceptStepID    int
}

// FirstRecorderAfterMetadata returns the metadata of the step behind the
// oldest artifact q selects. found is false when q selects none; a step with
// no metadata reads "".
func FirstRecorderAfterMetadata(db *sql.DB, q FirstRecorderAfterQuery) (metadata string, found bool, err error) {
	var m sql.NullString
	err = db.QueryRow(
		`SELECT s.metadata FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ?
		    AND a.id > ? AND a.step_id <> ?
		    AND NOT EXISTS (SELECT 1 FROM artifacts p
		                     WHERE p.step_id = a.step_id AND p.kind = a.kind
		                       AND p.id <= ?)
		  ORDER BY a.id LIMIT 1`,
		q.RunID, q.IssueID, q.Kind, q.AfterArtifactID, q.ExceptStepID, q.AfterArtifactID,
	).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s's first %s after artifact %d: %w",
			model.FormatID(q.IssueID), q.Kind, q.AfterArtifactID, err)
	}
	return m.String, true, nil
}

// NewestStepNamePayloadBelowOrdinal returns the payload of the newest artifact
// of one kind recorded by an issue's instances of one step name in the run at
// ordinals below ordinal. found is false when there is none; a recorded
// artifact with no payload reads "".
func NewestStepNamePayloadBelowOrdinal(
	db *sql.DB, runID, issueID int, stepName string, ordinal int, kind string,
) (payload string, found bool, err error) {
	var p sql.NullString
	err = db.QueryRow(
		`SELECT a.payload FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND s.step_name = ?
		    AND s.ordinal < ? AND a.kind = ?
		  ORDER BY a.id DESC LIMIT 1`,
		runID, issueID, stepName, ordinal, kind).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %q's %s below ordinal %d: %w", stepName, kind, ordinal, err)
	}
	return p.String, true, nil
}

// FenceCommand is one harvested fence command and its stored SHA-256.
type FenceCommand struct {
	Command, SHA256 string
}

// RunFenceCommands returns the commands harvested for one fence tag of an
// issue in a run, in ordinal order. found is false when none was harvested.
func RunFenceCommands(db *sql.DB, runID, issueID int, tag string) (commands []FenceCommand, found bool, err error) {
	rows, err := db.Query(
		`SELECT command, sha256 FROM run_fences
		  WHERE run_id = ? AND issue_id = ? AND tag = ? ORDER BY ordinal`,
		runID, issueID, tag)
	if err != nil {
		return nil, false, fmt.Errorf("reading the %s fence commands: %w", tag, err)
	}
	commands, err = scanRows(rows, "fence commands", func(r *sql.Rows) (FenceCommand, error) {
		var c FenceCommand
		if err := r.Scan(&c.Command, &c.SHA256); err != nil {
			return c, fmt.Errorf("reading a %s fence command: %w", tag, err)
		}
		return c, nil
	})
	if err != nil {
		return nil, false, err
	}
	return commands, len(commands) > 0, nil
}

// NewestIssueArtifact returns the body and payload of the newest artifact of
// one kind recorded by any of an issue's steps in the run. found is false when
// there is none; a recorded artifact with no payload reads "".
func NewestIssueArtifact(db *sql.DB, runID, issueID int, kind string) (body, payload string, found bool, err error) {
	err = db.QueryRow(
		`SELECT a.body, COALESCE(a.payload, '') FROM artifacts a
		   JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ?
		  ORDER BY a.id DESC LIMIT 1`,
		runID, issueID, kind).Scan(&body, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("reading %s's newest %s: %w", model.FormatID(issueID), kind, err)
	}
	return body, payload, true, nil
}

// NewestIssueArtifactBodies returns the bodies of the newest artifacts of one
// kind recorded by an issue's steps in the run, newest first, at most limit of
// them.
func NewestIssueArtifactBodies(db *sql.DB, runID, issueID int, kind string, limit int) ([]string, error) {
	rows, err := db.Query(
		`SELECT a.body FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ?
		  ORDER BY a.id DESC LIMIT ?`,
		runID, issueID, kind, limit)
	if err != nil {
		return nil, fmt.Errorf("reading %s's %s bodies: %w", model.FormatID(issueID), kind, err)
	}
	return scanRows(rows, "artifact bodies", func(r *sql.Rows) (string, error) {
		var body string
		if err := r.Scan(&body); err != nil {
			return "", fmt.Errorf("reading a %s body: %w", kind, err)
		}
		return body, nil
	})
}

// FirstStepEventData returns the data of a step's oldest event of one kind.
// found is false when the step has none.
func FirstStepEventData(db *sql.DB, stepID int, kind string) (data string, found bool, err error) {
	err = db.QueryRow(
		`SELECT data FROM events WHERE kind = ? AND step_id = ? ORDER BY seq ASC LIMIT 1`,
		kind, stepID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s's first %s event: %w", model.FormatStepID(stepID), kind, err)
	}
	return data, true, nil
}

// StepEventDataContains reports whether a step has an event of one kind in the
// run whose data contains text, matched as a LIKE pattern `%text%`.
func StepEventDataContains(db *sql.DB, runID, stepID int, kind, text string) (bool, error) {
	var found bool
	err := db.QueryRow(
		`SELECT EXISTS(
		   SELECT 1 FROM events
		    WHERE run_id = ? AND step_id = ? AND kind = ? AND data LIKE ?)`,
		runID, stepID, kind, "%"+text+"%").Scan(&found)
	if err != nil {
		return false, fmt.Errorf("probing %s's %s events: %w", model.FormatStepID(stepID), kind, err)
	}
	return found, nil
}

// StrandedSagaStepIDs returns the ids of a run's steps whose saga stands at a
// stage other than none or `held`, whose kind is not excludedKind, and that
// were last updated before updatedBeforeMS, in no particular order.
func StrandedSagaStepIDs(db *sql.DB, runID int, excludedKind string, updatedBeforeMS int64) ([]int, error) {
	rows, err := db.Query(
		`SELECT id FROM steps
		  WHERE run_id = ? AND saga_stage IS NOT NULL AND saga_stage NOT IN ('', ?)
		    AND kind != ? AND updated_at_ms < ?`,
		runID, SagaHeld, excludedKind, updatedBeforeMS)
	if err != nil {
		return nil, fmt.Errorf("listing stranded sagas: %w", err)
	}
	return scanRows(rows, "stranded sagas", func(r *sql.Rows) (int, error) {
		var id int
		if err := r.Scan(&id); err != nil {
			return 0, fmt.Errorf("listing stranded sagas: %w", err)
		}
		return id, nil
	})
}

// RunCostFloorTx computes a run's committed cost floor: the `expected_cost`
// each claim event of claimEventKind carried, or its step's declared
// `expected_cost` when the event carries none or its data is not valid JSON,
// plus the declared `expected_cost` of every step of voteStepKind not skipped
// or superseded. A run with neither sums to 0.
func RunCostFloorTx(tx *sql.Tx, runID int, claimEventKind, voteStepKind string) (float64, error) {
	var floor float64
	err := tx.QueryRow(
		`SELECT COALESCE((SELECT SUM(COALESCE(
		           CASE WHEN json_valid(e.data)
		                THEN json_extract(e.data, '$.expected_cost') END,
		           s.expected_cost))
		           FROM events e JOIN steps s ON s.id = e.step_id
		          WHERE e.run_id = ? AND e.kind = ?), 0)
		      + COALESCE((SELECT SUM(expected_cost)
		           FROM steps WHERE run_id = ? AND kind = ?
		            AND status NOT IN (?, ?)), 0)`,
		runID, claimEventKind, runID, voteStepKind, StepSkipped, StepSuperseded,
	).Scan(&floor)
	if err != nil {
		return 0, fmt.Errorf("computing the floor for %s: %w", model.FormatRunID(runID), err)
	}
	return floor, nil
}

// StepRowVersionTx returns a step's row version, or ErrStepNotFound.
func StepRowVersionTx(tx *sql.Tx, stepID int) (int, error) {
	var version int
	err := tx.QueryRow(`SELECT row_version FROM steps WHERE id = ?`, stepID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrStepNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("reading %s's row version: %w", model.FormatStepID(stepID), err)
	}
	return version, nil
}

// StepRecoveryHashTx returns the recovery hash a step's claim stored. found is
// false when the claim stored none; a missing step is ErrStepNotFound.
func StepRecoveryHashTx(tx *sql.Tx, stepID int) (hash string, found bool, err error) {
	var h sql.NullString
	err = tx.QueryRow(`SELECT recovery_hash FROM steps WHERE id = ?`, stepID).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrStepNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s's recovery hash: %w", model.FormatStepID(stepID), err)
	}
	return h.String, h.Valid, nil
}

// ClaimEventsPrunedQuery names a step's claim at ClaimedAtMS and the event
// kinds that record a prune and a claim.
type ClaimEventsPrunedQuery struct {
	RunID, StepID         int
	PrunedKind, ClaimKind string
	ClaimedAtMS           int64
}

// ClaimEventsPrunedTx reports whether a prune covering the run, or one with no
// run, has been logged while no ClaimKind event of the step at or after
// ClaimedAtMS survives.
func ClaimEventsPrunedTx(tx *sql.Tx, q ClaimEventsPrunedQuery) (bool, error) {
	var pruned bool
	err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM events
		                 WHERE kind = ? AND (run_id = ? OR run_id IS NULL))
		    AND NOT EXISTS (SELECT 1 FROM events
		                     WHERE kind = ? AND step_id = ? AND at_ms >= ?)`,
		q.PrunedKind, q.RunID, q.ClaimKind, q.StepID, q.ClaimedAtMS).Scan(&pruned)
	if err != nil {
		return false, fmt.Errorf("reading %s's claim record: %w", model.FormatStepID(q.StepID), err)
	}
	return pruned, nil
}

// RunIssueSnapshotsTx returns the body and issue snapshots activation froze for
// an issue in a run, each "" when none was frozen, or ErrNotFound when the
// issue is not in the run.
func RunIssueSnapshotsTx(tx *sql.Tx, runID, issueID int) (body, snapshot string, err error) {
	var b, s sql.NullString
	err = tx.QueryRow(
		`SELECT body_snapshot, issue_snapshot FROM run_issues WHERE run_id = ? AND issue_id = ?`,
		runID, issueID).Scan(&b, &s)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("reading the snapshots of %s: %w", model.FormatID(issueID), err)
	}
	return b.String, s.String, nil
}

// IssueDescriptionTx returns an issue's live description, or ErrNotFound.
func IssueDescriptionTx(tx *sql.Tx, issueID int) (string, error) {
	var description sql.NullString
	err := tx.QueryRow(`SELECT description FROM issues WHERE id = ?`, issueID).Scan(&description)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading the live description for %s: %w", model.FormatID(issueID), err)
	}
	return description.String, nil
}

// IssueEventAfterQuery selects the events of Kind logged for an issue in a run
// after AfterMS.
type IssueEventAfterQuery struct {
	RunID, IssueID int
	Kind           string
	AfterMS        int64
}

// FirstIssueEventDataAfterTx returns the data of the earliest event q selects,
// by time and then log order. found is false when q selects none.
func FirstIssueEventDataAfterTx(tx *sql.Tx, q IssueEventAfterQuery) (data string, found bool, err error) {
	return firstIssueEventDataAfter(tx, q)
}

// FirstIssueEventDataAfter is FirstIssueEventDataAfterTx on the pool.
func FirstIssueEventDataAfter(db *sql.DB, q IssueEventAfterQuery) (data string, found bool, err error) {
	return firstIssueEventDataAfter(db, q)
}

func firstIssueEventDataAfter(rq rowQuerier, q IssueEventAfterQuery) (data string, found bool, err error) {
	err = rq.QueryRow(
		`SELECT data FROM events
		  WHERE kind = ? AND run_id = ? AND issue_id = ? AND at_ms > ?
		  ORDER BY at_ms, seq LIMIT 1`,
		q.Kind, q.RunID, q.IssueID, q.AfterMS).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s's %s events: %w", model.FormatID(q.IssueID), q.Kind, err)
	}
	return data, true, nil
}
