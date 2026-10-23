package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/planner"
)

// RetryEligible is a step parked gap-only whose issue's depends_on or blocks
// prerequisites are all done, at least one of them linked or changed after the
// park (DKT-4257). The park waited on a gap another
// issue was filed to close; once those issues are done the park is a retry
// candidate, settled advisor-first, instead of sitting until an operator or a
// conductor with a long memory asks about it.
type RetryEligible struct {
	Step      string   `json:"step"`
	Instance  string   `json:"instance"`
	Issue     string   `json:"issue"`
	ParkClass string   `json:"park_class"`
	DependsOn []string `json:"depends_on"`

	issueID    int
	rowVersion int
}

// runReads is one verb's shared reads of a run's steps and of the directional
// relations. `dispatch open` and `run status` render the retry-eligible, held
// and unexpanded lists together; each read happens at most once, when a list
// first needs it, instead of once per list (the relations read scans the
// whole issue_relations table).
type runReads struct {
	conn  *sql.DB
	runID int

	steps     []*db.Step
	stepsRead bool
	relations []model.Relation
	relsRead  bool
}

// runReadsFromSteps shares a step list a caller already loaded.
func runReadsFromSteps(conn *sql.DB, runID int, steps []*db.Step) *runReads {
	return &runReads{conn: conn, runID: runID, steps: steps, stepsRead: true}
}

func (r *runReads) runSteps() ([]*db.Step, error) {
	if !r.stepsRead {
		steps, err := db.ListRunSteps(r.conn, r.runID)
		if err != nil {
			return nil, fmt.Errorf("listing run steps: %w", err)
		}
		r.steps, r.stepsRead = steps, true
	}
	return r.steps, nil
}

func (r *runReads) directionalRelations() ([]model.Relation, error) {
	if !r.relsRead {
		relations, err := db.GetAllDirectionalRelations(r.conn)
		if err != nil {
			return nil, fmt.Errorf("reading relations: %w", err)
		}
		r.relations, r.relsRead = relations, true
	}
	return r.relations, nil
}

// RunAdvisories is `run status`'s pair of advisory lists, the unexpanded
// issues and the retry-eligible parks, read over one step list and one
// relations scan.
func RunAdvisories(conn *sql.DB, runID int) ([]UnexpandedIssue, []RetryEligible, error) {
	reads := &runReads{conn: conn, runID: runID}
	unexpanded, err := reads.unexpandedIssues()
	if err != nil {
		return nil, nil, err
	}
	retryEligible, err := reads.retryEligibleParks()
	if err != nil {
		return nil, nil, err
	}
	return unexpanded, retryEligible, nil
}

// RetryEligibleParks lists the run's gap-only parks whose issue has at least
// one blocking prerequisite (depends_on, or the inverse blocks) and no
// prerequisite that is not done, where at least one prerequisite was added or
// last changed after the park, in step order. A prerequisite that was already
// met before the park cannot be what the park waits on. A pure read: `run status` renders it, and `dispatch open` renders it
// and records the event.
func RetryEligibleParks(conn *sql.DB, runID int) ([]RetryEligible, error) {
	return (&runReads{conn: conn, runID: runID}).retryEligibleParks()
}

func (r *runReads) retryEligibleParks() ([]RetryEligible, error) {
	conn := r.conn
	steps, err := r.runSteps()
	if err != nil {
		return nil, err
	}
	var parked []*db.Step
	for _, s := range steps {
		if s.Status == db.StepWaitingHuman && s.ParkClass == db.ParkClassGapOnly {
			parked = append(parked, s)
		}
	}
	if len(parked) == 0 {
		return nil, nil
	}

	relations, err := r.directionalRelations()
	if err != nil {
		return nil, err
	}
	_, blockers := planner.BuildAdjacency(relations)

	need := make(map[int]struct{})
	for _, s := range parked {
		need[s.IssueID] = struct{}{}
		for _, b := range blockers[s.IssueID] {
			need[b] = struct{}{}
		}
	}
	ids := make([]int, 0, len(need))
	for id := range need {
		ids = append(ids, id)
	}
	issues, err := db.GetIssuesByIDs(conn, ids)
	if err != nil {
		return nil, fmt.Errorf("reading prerequisite issues: %w", err)
	}
	// When each prerequisite became done. Its updated_at moves on any later
	// edit (a comment, a label), which is not the prerequisite changing; it is
	// the fallback only for an issue whose close the log never saw.
	doneAt, err := db.StatusReachedAt(conn, ids, string(model.StatusDone))
	if err != nil {
		return nil, fmt.Errorf("reading when prerequisites closed: %w", err)
	}

	// When each blocking edge was added, so a prerequisite that was already met
	// before the park does not signal it.
	type edge struct{ issue, blocker int }
	edgeAdded := make(map[edge]time.Time)
	for _, rel := range relations {
		e := edge{rel.SourceIssueID, rel.TargetIssueID}
		switch rel.RelationType {
		case model.RelationBlocks:
			e = edge{rel.TargetIssueID, rel.SourceIssueID}
		case model.RelationDependsOn:
		default:
			continue
		}
		if rel.CreatedAt.After(edgeAdded[e]) {
			edgeAdded[e] = rel.CreatedAt
		}
	}

	var out []RetryEligible
	for _, s := range parked {
		deps := blockers[s.IssueID]
		if len(deps) == 0 {
			continue
		}
		allDone, movedAfterPark := true, false
		seen := make(map[int]bool, len(deps))
		refs := make([]string, 0, len(deps))
		for _, d := range deps {
			if seen[d] {
				continue
			}
			seen[d] = true
			i, ok := issues[d]
			if !ok || i.Status != model.StatusDone {
				allDone = false
				break
			}
			// The edge was added, or the prerequisite closed, after the park.
			// Both clocks are wall clocks; the park's is the step's activity
			// stamp, which the routing write that parked it set and which no
			// later write touches while the step stays parked. The row's
			// updated_at_ms would not do: every later row write bumps it.
			closed, ok := doneAt[d]
			if !ok {
				closed = i.UpdatedAt
			}
			parkMS := stepActivityMS(s)
			if notBeforePark(edgeAdded[edge{s.IssueID, d}], parkMS) || notBeforePark(closed, parkMS) {
				movedAfterPark = true
			}
			refs = append(refs, model.FormatID(d))
		}
		if !allDone || !movedAfterPark {
			continue
		}
		sort.Strings(refs)
		out = append(out, RetryEligible{
			Step: model.FormatStepID(s.ID), Instance: s.Instance,
			Issue: model.FormatID(s.IssueID), ParkClass: string(s.ParkClass),
			DependsOn: refs, issueID: s.IssueID, rowVersion: s.RowVersion,
		})
	}
	return out, nil
}

// notBeforePark reports whether a stored instant is not before the park. Issue
// and relation times are kept to the second while the park is in
// milliseconds, so an instant in the park's own second counts as after it: a
// close a few hundred milliseconds after the park must not read as before it.
func notBeforePark(at time.Time, parkMS int64) bool {
	return !at.IsZero() && at.Unix() >= parkMS/1000
}

// recordRetryEligible writes one park-retry-eligible event per eligible park
// that has none yet, and returns the list. A park is identified by its step
// and row_version: a retry that parks the step again moves the version, so the
// new park is signalled afresh. The parks already recorded are read in one
// query over the run, and the missing events commit in one transaction; a
// failure to record is returned without hiding the list.
func recordRetryEligible(conn *sql.DB, reads *runReads, nowMS int64) ([]RetryEligible, error) {
	eligible, err := reads.retryEligibleParks()
	if err != nil || len(eligible) == 0 {
		return eligible, err
	}
	runID := reads.runID
	seen, err := recordedRetryEligible(conn, runID)
	if err != nil {
		return eligible, err
	}
	var missing []RetryEligible
	for _, r := range eligible {
		stepID, err := model.ParseStepID(r.Step)
		if err != nil {
			return eligible, err
		}
		if !seen[retryEligiblePark{stepID, r.rowVersion}] {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		return eligible, nil
	}

	tx, err := conn.Begin()
	if err != nil {
		return eligible, fmt.Errorf("recording a retry-eligible park: %w", err)
	}
	defer tx.Rollback()
	for _, r := range missing {
		data, err := json.Marshal(map[string]any{
			"step": r.Step, "park_class": r.ParkClass,
			"depends_on": r.DependsOn, "row_version": r.rowVersion,
		})
		if err != nil {
			return eligible, fmt.Errorf("recording a retry-eligible park: %w", err)
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventParkRetryEligible, RunID: runID, Instance: r.Instance,
			IssueID: r.issueID, Data: string(data), AtMS: nowMS,
		}); err != nil {
			return eligible, err
		}
	}
	if err := tx.Commit(); err != nil {
		return eligible, fmt.Errorf("recording a retry-eligible park: %w", err)
	}
	return eligible, nil
}

// retryEligiblePark identifies one park: the step and the row version it was
// parked at.
type retryEligiblePark struct{ stepID, rowVersion int }

// recordedRetryEligible is the set of parks the run's park-retry-eligible
// events already name.
func recordedRetryEligible(conn *sql.DB, runID int) (map[retryEligiblePark]bool, error) {
	rows, err := conn.Query(
		`SELECT step_id, data FROM events WHERE run_id = ? AND kind = ?`,
		runID, EventParkRetryEligible)
	if err != nil {
		return nil, fmt.Errorf("reading retry-eligible events: %w", err)
	}
	defer rows.Close()
	seen := make(map[retryEligiblePark]bool)
	for rows.Next() {
		var (
			stepID sql.NullInt64
			raw    string
		)
		if err := rows.Scan(&stepID, &raw); err != nil {
			return nil, fmt.Errorf("reading a retry-eligible event: %w", err)
		}
		var data struct {
			RowVersion int `json:"row_version"`
		}
		if stepID.Valid && json.Unmarshal([]byte(raw), &data) == nil {
			seen[retryEligiblePark{int(stepID.Int64), data.RowVersion}] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading retry-eligible events: %w", err)
	}
	return seen, nil
}
