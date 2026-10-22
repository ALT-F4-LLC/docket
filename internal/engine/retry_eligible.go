package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

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

// RetryEligibleParks lists the run's gap-only parks whose issue has at least
// one blocking prerequisite (depends_on, or the inverse blocks) and no
// prerequisite that is not done, where at least one prerequisite was added or
// last changed after the park, in step order. A prerequisite that was already
// met before the park cannot be what the park waits on. A pure read: `run status` renders it, and `dispatch open` renders it
// and records the event.
func RetryEligibleParks(conn *sql.DB, runID int) ([]RetryEligible, error) {
	steps, err := db.ListRunSteps(conn, runID)
	if err != nil {
		return nil, fmt.Errorf("listing run steps: %w", err)
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

	relations, err := db.GetAllDirectionalRelations(conn)
	if err != nil {
		return nil, fmt.Errorf("reading relations: %w", err)
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

	// When each blocking edge was added, so a prerequisite that was already met
	// before the park does not signal it.
	type edge struct{ issue, blocker int }
	edgeAddedMS := make(map[edge]int64)
	for _, rel := range relations {
		e := edge{rel.SourceIssueID, rel.TargetIssueID}
		switch rel.RelationType {
		case model.RelationBlocks:
			e = edge{rel.TargetIssueID, rel.SourceIssueID}
		case model.RelationDependsOn:
		default:
			continue
		}
		if ms := rel.CreatedAt.UnixMilli(); ms > edgeAddedMS[e] {
			edgeAddedMS[e] = ms
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
			// The edge was added, or the prerequisite last changed (it closed),
			// after the park. Both clocks are wall clocks; the park's is the
			// step row's last write.
			if edgeAddedMS[edge{s.IssueID, d}] > s.UpdatedAtMS || i.UpdatedAt.UnixMilli() > s.UpdatedAtMS {
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

// recordRetryEligible writes one park-retry-eligible event per eligible park
// that has none yet, and returns the list. A park is identified by its step
// and row_version: a retry that parks the step again moves the version, so the
// new park is signalled afresh. Each event commits in its own transaction; a
// failure to record is returned without hiding the list.
func recordRetryEligible(conn *sql.DB, runID int, nowMS int64) ([]RetryEligible, error) {
	eligible, err := RetryEligibleParks(conn, runID)
	if err != nil || len(eligible) == 0 {
		return eligible, err
	}
	for _, r := range eligible {
		stepID, err := model.ParseStepID(r.Step)
		if err != nil {
			return eligible, err
		}
		if err := recordRetryEligibleOnce(conn, runID, stepID, r, nowMS); err != nil {
			return eligible, err
		}
	}
	return eligible, nil
}

func recordRetryEligibleOnce(
	conn *sql.DB, runID, stepID int, r RetryEligible, nowMS int64,
) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("recording a retry-eligible park: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? AND step_id = ?`,
		runID, EventParkRetryEligible, stepID)
	if err != nil {
		return fmt.Errorf("reading retry-eligible events: %w", err)
	}
	seen := false
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return fmt.Errorf("reading a retry-eligible event: %w", err)
		}
		var data struct {
			RowVersion int `json:"row_version"`
		}
		if json.Unmarshal([]byte(raw), &data) == nil && data.RowVersion == r.rowVersion {
			seen = true
		}
	}
	rows.Close()
	if seen {
		return nil
	}

	data, err := json.Marshal(map[string]any{
		"step": r.Step, "park_class": r.ParkClass,
		"depends_on": r.DependsOn, "row_version": r.rowVersion,
	})
	if err != nil {
		return fmt.Errorf("recording a retry-eligible park: %w", err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventParkRetryEligible, RunID: runID, Instance: r.Instance,
		IssueID: r.issueID, Data: string(data), AtMS: nowMS,
	}); err != nil {
		return err
	}
	return tx.Commit()
}
