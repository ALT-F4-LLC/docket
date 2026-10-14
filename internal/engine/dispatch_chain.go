package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// FactSourceEngine marks a `step-deferred` event the engine derived itself, in
// `data.source`. A conductor-filed fact (`run fact add`) carries no source.
const FactSourceEngine = "engine"

// chainDeferredRows returns the manifest rows a closing dispatch left
// chain-deferred: still pending, behind an earlier-stage row of the same issue
// whose step did not settle (done, skipped, superseded). A wave launches an
// issue's stages in order and stops the issue's lane at the first stage that
// fails, parks, or conflicts, so every later stage of that issue goes unlaunched
// and is re-offered next dispatch. A row held back on budget has no such
// predecessor, which is what separates the two causes.
//
// Rows at stage 0 are unstaged and carry no ordering, so they never qualify.
func chainDeferredRows(
	rows []db.DispatchRow, steps []*db.Step,
) ([]*db.Step, error) {
	status := make(map[int]string, len(steps))
	byID := make(map[int]*db.Step, len(steps))
	for _, s := range steps {
		status[s.ID], byID[s.ID] = s.Status, s
	}
	type placed struct {
		step  int
		issue string
		stage int
	}
	manifest := make([]placed, 0, len(rows))
	for _, r := range rows {
		var row model.StepRow
		if err := json.Unmarshal([]byte(r.RowJSON), &row); err != nil {
			return nil, fmt.Errorf("reading manifest row %d: %w", r.Position, err)
		}
		manifest = append(manifest, placed{r.StepID, row.Issue, row.Stage})
	}

	var out []*db.Step
	for _, row := range manifest {
		if row.issue == "" || row.stage == 0 || status[row.step] != db.StepPending {
			continue
		}
		for _, prior := range manifest {
			if prior.issue != row.issue || prior.stage >= row.stage {
				continue
			}
			if st := status[prior.step]; st != db.StepDone && st != db.StepSkipped &&
				st != db.StepSuperseded {
				out = append(out, byID[row.step])
				break
			}
		}
	}
	return out, nil
}

// recordChainDeferralsTx records one engine-derived `step-deferred` fact with
// cause `chain` for each chain-deferred row of the dispatch, so the fact does
// not depend on a conductor filing it. A fact already recorded for the same
// step and dispatch is not recorded again. The cause `budget` stays
// conductor-filed: the engine does not observe a budget hold.
func recordChainDeferralsTx(
	tx *sql.Tx, runID, dispatchID int, steps []*db.Step, nowMS int64,
) error {
	rows, err := db.ListDispatchRowsTx(tx, dispatchID)
	if err != nil {
		return err
	}
	deferred, err := chainDeferredRows(rows, steps)
	if err != nil {
		return err
	}
	dispatch := FormatDispatchID(dispatchID)
	for _, step := range deferred {
		var seen int
		err := tx.QueryRow(
			`SELECT 1 FROM events
			  WHERE run_id = ? AND step_id = ? AND kind = ?
			    AND json_extract(data, '$.cause') = ?
			    AND json_extract(data, '$.dispatch') = ?`,
			runID, step.ID, EventStepDeferred, DeferralChain, dispatch).Scan(&seen)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("checking for a recorded chain deferral: %w", err)
		}
		data, err := json.Marshal(map[string]any{
			"step":     model.FormatStepID(step.ID),
			"cause":    DeferralChain,
			"reason":   "skipped-chain-dead: an earlier stage of the issue did not settle in " + dispatch,
			"dispatch": dispatch,
			"source":   FactSourceEngine,
		})
		if err != nil {
			return fmt.Errorf("encoding a chain deferral: %w", err)
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventStepDeferred, RunID: runID, Instance: step.Instance,
			IssueID: step.IssueID, Data: string(data), AtMS: nowMS,
		}); err != nil {
			return err
		}
	}
	return nil
}

// engineChainFact returns the latest engine-derived chain deferral recorded for
// the step, if any, as the fact `run fact add` would have returned.
func engineChainFact(tx *sql.Tx, runID, stepID int) (*RunFact, bool, error) {
	var (
		seq  int64
		data string
	)
	err := tx.QueryRow(
		`SELECT seq, data FROM events
		  WHERE run_id = ? AND step_id = ? AND kind = ?
		    AND json_extract(data, '$.cause') = ?
		    AND json_extract(data, '$.source') = ?
		  ORDER BY seq DESC LIMIT 1`,
		runID, stepID, EventStepDeferred, DeferralChain, FactSourceEngine).Scan(&seq, &data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading the engine's chain deferral: %w", err)
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(data), &body); err != nil {
		return nil, false, fmt.Errorf("reading the engine's chain deferral: %w", err)
	}
	return &RunFact{
		Run: model.FormatRunID(runID), Kind: EventStepDeferred, Seq: seq,
		Step: model.FormatStepID(stepID), Cause: DeferralChain, Reason: body.Reason,
	}, true, nil
}
