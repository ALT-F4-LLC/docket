package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The event-log queries behind `events list`, `events prune`, the report's
// actor rollup, `run status`'s activation head, and the engine's event and
// run-fact recorders. Each runs inside the
// caller's transaction; the event writer and the meaning of each kind live in
// internal/engine.

// ErrNoEventLowWatermark reports that the single event_low_watermark row is
// absent. Migration v46 seeds it, so its absence is a damaged store rather
// than a missing entity.
var ErrNoEventLowWatermark = errors.New("the event low watermark row is missing")

// EventPruneSelection names the events one prune counts, checks, and deletes.
// A zero field adds no bound. A selection with neither BeforeSeq nor RunID is
// refused, so the zero value can never select the whole log.
type EventPruneSelection struct {
	// BeforeSeq selects seq < BeforeSeq.
	BeforeSeq int64
	// RunID selects one run's events.
	RunID int
	// BeforeAtMS selects at_ms < BeforeAtMS, the retention clamp.
	BeforeAtMS int64
}

// where renders the selection with each column prefixed by alias, so the
// DELETE (which cannot alias `events`) and the run join share one predicate.
func (s EventPruneSelection) where(alias string) (string, []any, error) {
	if s.BeforeSeq == 0 && s.RunID == 0 {
		return "", nil, errors.New("an event prune selection needs a seq bound or a run")
	}
	var (
		conds []string
		args  []any
	)
	if s.BeforeSeq != 0 {
		conds = append(conds, alias+`seq < ?`)
		args = append(args, s.BeforeSeq)
	}
	if s.RunID != 0 {
		conds = append(conds, alias+`run_id = ?`)
		args = append(args, s.RunID)
	}
	if s.BeforeAtMS != 0 {
		conds = append(conds, alias+`at_ms < ?`)
		args = append(args, s.BeforeAtMS)
	}
	return ` WHERE ` + strings.Join(conds, ` AND `), args, nil
}

// CountPrunableEventsTx counts the events sel selects.
func CountPrunableEventsTx(tx *sql.Tx, sel EventPruneSelection) (int, error) {
	where, args, err := sel.where("")
	if err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM events`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting the events to prune: %w", err)
	}
	return n, nil
}

// DeletePrunableEventsTx deletes the events sel selects and returns how many
// it removed.
func DeletePrunableEventsTx(tx *sql.Tx, sel EventPruneSelection) (int, error) {
	where, args, err := sel.where("")
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM events`+where, args...)
	if err != nil {
		return 0, fmt.Errorf("pruning events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pruning events: %w", err)
	}
	return int(n), nil
}

// EventRunStatus is one run that owns events in a prune selection.
type EventRunStatus struct {
	RunID  int
	Status model.RunStatus
}

// PrunableEventRunsTx lists, by run id, every run owning an event sel
// selects, with its status. Events with no run contribute nothing.
func PrunableEventRunsTx(tx *sql.Tx, sel EventPruneSelection) ([]EventRunStatus, error) {
	where, args, err := sel.where("e.")
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(
		`SELECT DISTINCT r.id, r.status
		   FROM events e JOIN runs r ON r.id = e.run_id`+where+` ORDER BY r.id`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("checking the runs a prune would touch: %w", err)
	}
	return scanRows(rows, "the runs a prune would touch",
		func(r *sql.Rows) (EventRunStatus, error) {
			var s EventRunStatus
			if err := r.Scan(&s.RunID, &s.Status); err != nil {
				return EventRunStatus{}, fmt.Errorf("reading a run's status: %w", err)
			}
			return s, nil
		})
}

// CountEventsHeldByRetentionTx counts the events sel selects at or after
// cutoffMS: the rows the retention window keeps from the prune.
func CountEventsHeldByRetentionTx(tx *sql.Tx, sel EventPruneSelection, cutoffMS int64) (int, error) {
	where, args, err := sel.where("")
	if err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM events`+where+` AND at_ms >= ?`,
		append(args, cutoffMS)...,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting the events the retention window holds: %w", err)
	}
	return n, nil
}

// RaiseEventLowWatermarkTx moves the watermark up to just below the lowest
// surviving seq. It never lowers it, and leaves it alone when no event
// survives: the comparison is NULL over an empty log, so no row updates. The
// comparison sits in WHERE because SQLite's two-argument MAX and PostgreSQL's
// GREATEST have no common spelling.
func RaiseEventLowWatermarkTx(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`UPDATE event_low_watermark
		    SET seq = (SELECT MIN(seq) - 1 FROM events)
		  WHERE id = 1 AND (SELECT MIN(seq) - 1 FROM events) > seq`,
	); err != nil {
		return fmt.Errorf("raising the event low watermark: %w", err)
	}
	return nil
}

// EventLowWatermarkTx reads the highest pruned event seq, 0 when nothing was
// pruned. A missing row is ErrNoEventLowWatermark.
func EventLowWatermarkTx(tx *sql.Tx) (int64, error) {
	var watermark int64
	err := tx.QueryRow(`SELECT seq FROM event_low_watermark WHERE id = 1`).Scan(&watermark)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoEventLowWatermark
	}
	if err != nil {
		return 0, fmt.Errorf("reading the event low watermark: %w", err)
	}
	return watermark, nil
}

// EventFilter selects the events of one `events list` read. A zero field adds
// no bound, except Since, which always applies as seq > Since.
type EventFilter struct {
	Since  int64
	RunID  int
	Kind   string
	StepID int
	// ProjectID scopes the feed to one project. An event belongs to its run's
	// project, else its issue's, else the repository its payload names; a
	// store-level event naming no repository belongs to every project. It is
	// ignored when RunID is set, because a run already belongs to exactly one
	// project.
	ProjectID int
}

func (f EventFilter) where() (string, []any) {
	where := ` WHERE e.seq > ?`
	args := []any{f.Since}
	if f.RunID != 0 {
		where += ` AND e.run_id = ?`
		args = append(args, f.RunID)
	}
	if f.Kind != "" {
		where += ` AND e.kind = ?`
		args = append(args, f.Kind)
	}
	if f.StepID != 0 {
		where += ` AND e.step_id = ?`
		args = append(args, f.StepID)
	}
	if f.ProjectID != 0 && f.RunID == 0 {
		where += ` AND (
			e.run_id IN (SELECT id FROM runs WHERE project_id = ?)
			OR (e.run_id IS NULL AND e.issue_id IN (SELECT id FROM issues WHERE project_id = ?))
			OR (e.run_id IS NULL AND e.issue_id IS NULL AND (
				` + eventRepoBinding + ` IS NULL
				OR ` + eventRepoBinding + ` = (SELECT identity FROM projects WHERE id = ?))))`
		args = append(args, f.ProjectID, f.ProjectID, f.ProjectID)
	}
	return where, args
}

// eventRepoBinding is the repository a store-level event's payload names, or
// NULL when it names none. A global trust entry writes `"repo": ""`, which
// NULLIF turns into "names none" so the entry stays visible everywhere. The
// json_valid guards keep one malformed `data` cell from aborting the query:
// json_extract fails the statement on malformed input rather than returning
// NULL.
const eventRepoBinding = `NULLIF(COALESCE(
	CASE WHEN json_valid(e.data) THEN json_extract(e.data, '$.repo') END,
	CASE WHEN json_valid(e.data) THEN json_extract(e.data, '$.identity') END), '')`

// CountEventsTx counts the events f selects.
func CountEventsTx(tx *sql.Tx, f EventFilter) (int, error) {
	where, args := f.where()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM events e`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting events: %w", err)
	}
	return n, nil
}

// EventRow is one stored event with the names its ids resolve to. A field
// whose column is NULL holds its zero value: no event has run, step, or issue
// id 0.
type EventRow struct {
	Seq     int64
	AtMS    int64
	Kind    string
	RunID   int
	StepID  int
	IssueID int
	// Instance is the step's `name@k#i`.
	Instance string
	// IssuePrefix is the prefix of the project that owns the issue.
	IssuePrefix string
	// ProjectName names the run's project, else the issue's.
	ProjectName string
	// Data is the stored JSON payload, verbatim.
	Data string
}

// ListEventsTx returns the events f selects in ascending seq order. A
// positive tail returns the newest tail events, still ascending; otherwise a
// positive limit returns the oldest limit events.
func ListEventsTx(tx *sql.Tx, f EventFilter, limit, tail int) ([]EventRow, error) {
	where, args := f.where()
	const selectCols = `e.seq, e.at_ms, e.kind, e.run_id, e.step_id, e.issue_id, ` +
		`s.instance, ip.prefix, COALESCE(rp.name, ip.name), e.data`
	const from = ` FROM events e` +
		` LEFT JOIN steps s ON s.id = e.step_id` +
		` LEFT JOIN issues i ON i.id = e.issue_id` +
		` LEFT JOIN projects ip ON ip.id = i.project_id` +
		` LEFT JOIN runs r ON r.id = e.run_id` +
		` LEFT JOIN projects rp ON rp.id = r.project_id`

	var query string
	if tail > 0 {
		query = `SELECT * FROM (SELECT ` + selectCols + from + where +
			fmt.Sprintf(` ORDER BY e.seq DESC LIMIT %d) ORDER BY seq ASC`, tail)
	} else {
		query = `SELECT ` + selectCols + from + where + ` ORDER BY e.seq ASC`
		if limit > 0 {
			query += fmt.Sprintf(` LIMIT %d`, limit)
		}
	}

	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading events: %w", err)
	}
	return scanRows(rows, "events", func(r *sql.Rows) (EventRow, error) {
		var (
			e                     EventRow
			runID, stepID, issue  sql.NullInt64
			instance, prefix, prj sql.NullString
		)
		if err := r.Scan(&e.Seq, &e.AtMS, &e.Kind, &runID, &stepID, &issue,
			&instance, &prefix, &prj, &e.Data); err != nil {
			return EventRow{}, fmt.Errorf("reading an event: %w", err)
		}
		e.RunID = int(runID.Int64)
		e.StepID = int(stepID.Int64)
		e.IssueID = int(issue.Int64)
		e.Instance = instance.String
		e.IssuePrefix = prefix.String
		e.ProjectName = prj.String
		return e, nil
	})
}

// EventKindCountsTx counts one run's events by kind. A run with no events
// yields an empty map.
func EventKindCountsTx(tx *sql.Tx, runID int) (map[string]int, error) {
	rows, err := tx.Query(
		`SELECT kind, COUNT(*) FROM events WHERE run_id = ? GROUP BY kind`, runID)
	if err != nil {
		return nil, fmt.Errorf("counting events by kind: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var (
			kind string
			n    int
		)
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, fmt.Errorf("counting events by kind: %w", err)
		}
		counts[kind] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting events by kind: %w", err)
	}
	return counts, nil
}

// LatestEventDataTx returns the payload of a run's newest event of kind.
// found is false when the run has no such event.
func LatestEventDataTx(tx *sql.Tx, runID int, kind string) (data string, found bool, err error) {
	err = tx.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, kind).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the latest %s event of %s: %w",
			kind, model.FormatRunID(runID), err)
	}
	return data, true, nil
}

// StepIDByInstanceTx resolves a run's step by its `name@k#i` instance. A
// nonzero issueID narrows the lookup: steps are unique per (run, issue,
// instance), not per (run, instance). found is false when no step matches.
func StepIDByInstanceTx(tx *sql.Tx, runID, issueID int, instance string) (id int, found bool, err error) {
	query := `SELECT id FROM steps WHERE run_id = ? AND instance = ?`
	args := []any{runID, instance}
	if issueID != 0 {
		query += ` AND issue_id = ?`
		args = append(args, issueID)
	}
	err = tx.QueryRow(query, args...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolving step %s of %s: %w", instance, model.FormatRunID(runID), err)
	}
	return id, true, nil
}

// NewEvent is one event row to insert. A zero RunID, StepID, or IssueID
// stores NULL.
type NewEvent struct {
	AtMS    int64
	Kind    string
	RunID   int
	StepID  int
	IssueID int
	// Data is the JSON payload, stored verbatim.
	Data string
}

// InsertEventTx appends e to the event log; the store assigns its seq.
func InsertEventTx(tx *sql.Tx, e NewEvent) error {
	if _, err := tx.Exec(
		`INSERT INTO events (at_ms, kind, run_id, step_id, issue_id, data)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.AtMS, e.Kind, nullableInt(e.RunID), nullableInt(e.StepID), nullableInt(e.IssueID), e.Data,
	); err != nil {
		return fmt.Errorf("inserting a %s event: %w", e.Kind, err)
	}
	return nil
}

// LastEventAtMSTx returns the at_ms of the newest event by seq. found is
// false when the log is empty.
func LastEventAtMSTx(tx *sql.Tx) (atMS int64, found bool, err error) {
	err = tx.QueryRow(`SELECT at_ms FROM events ORDER BY seq DESC LIMIT 1`).Scan(&atMS)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading the last event's timestamp: %w", err)
	}
	return atMS, true, nil
}

// MaxEventSeqTx returns the highest event seq. found is false when the log is
// empty.
func MaxEventSeqTx(tx *sql.Tx) (seq int64, found bool, err error) {
	var highest sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&highest); err != nil {
		return 0, false, fmt.Errorf("reading the highest event seq: %w", err)
	}
	return highest.Int64, highest.Valid, nil
}

// ChainDeferralQuery names the conductor-filed deferral one
// ConductorChainDeferralTx looks for. The kinds and cause are the engine's.
type ChainDeferralQuery struct {
	RunID  int
	StepID int
	// DeferredKind is the deferral event kind; Cause is its `$.cause`.
	DeferredKind string
	Cause        string
	// DispatchClosedKind is the kind of the event a dispatch close writes,
	// whose `$.dispatch` names the dispatch as `DISPATCH-<id>`.
	DispatchClosedKind string
}

// ConductorChainDeferralTx returns the first q.DeferredKind event for the step
// with cause q.Cause and no `$.source` (the conductor files it; the engine's
// carries a source) recorded after the close event of the run's most recently
// closed dispatch. With no dispatch closed, the window covers the whole log.
// found is false when no event matches.
func ConductorChainDeferralTx(tx *sql.Tx, q ChainDeferralQuery) (seq int64, data string, found bool, err error) {
	err = tx.QueryRow(
		`SELECT seq, data FROM events
		  WHERE run_id = ? AND step_id = ? AND kind = ?
		    AND json_extract(data, '$.cause') = ?
		    AND json_extract(data, '$.source') IS NULL
		    AND seq > COALESCE((
		        SELECT MAX(seq) FROM events
		         WHERE run_id = ? AND kind = ?
		           AND json_extract(data, '$.dispatch') = (
		               SELECT 'DISPATCH-' || id FROM dispatches
		                WHERE run_id = ? AND status = ?
		                ORDER BY closed_at_ms DESC, id DESC LIMIT 1)), 0)
		  ORDER BY seq LIMIT 1`,
		q.RunID, q.StepID, q.DeferredKind, q.Cause,
		q.RunID, q.DispatchClosedKind, q.RunID, DispatchClosed).Scan(&seq, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("reading the conductor's chain deferral: %w", err)
	}
	return seq, data, true, nil
}
