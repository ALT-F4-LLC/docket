package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// FactSourceEngine marks a `step-deferred` event the engine derived itself, in
// `data.source`. A conductor-filed fact (`run fact add`) carries no source.
const FactSourceEngine = "engine"

// chainDeferredRows returns the manifest rows a closing dispatch left
// chain-deferred: still pending, behind an earlier-stage row of the same issue
// whose step was attempted in this dispatch (attempted) and did not settle
// (done, skipped, superseded). A wave launches an issue's stages in order and
// stops the issue's lane at the first stage that fails, parks, or conflicts, so
// every later stage of that issue goes unlaunched and is re-offered next
// dispatch. A predecessor never attempted was held back by the budget or a run
// park, which is a different cause the conductor files.
//
// Rows at stage 0 are unstaged and carry no ordering, so they never qualify.
func chainDeferredRows(
	rows []store.DispatchRow, steps []*store.Step, attempted map[int]bool,
) ([]*store.Step, error) {
	status := make(map[int]string, len(steps))
	byID := make(map[int]*store.Step, len(steps))
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

	var out []*store.Step
	for _, row := range manifest {
		if row.issue == "" || row.stage == 0 || status[row.step] != store.StepPending {
			continue
		}
		for _, prior := range manifest {
			if prior.issue != row.issue || prior.stage >= row.stage || !attempted[prior.step] {
				continue
			}
			if st := status[prior.step]; st != store.StepDone && st != store.StepSkipped &&
				st != store.StepSuperseded {
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
	tx *sql.Tx, runID int, open *store.Dispatch, steps []*store.Step, nowMS int64,
) error {
	dispatchID := open.ID
	rows, err := store.TxOn(tx).ListDispatchRowsTx(dispatchID)
	if err != nil {
		return err
	}
	attempted, err := attemptedSince(tx, runID, open.OpenedSeq)
	if err != nil {
		return err
	}
	deferred, err := chainDeferredRows(rows, steps, attempted)
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

// attemptedSince returns the steps of the run that saw a claim, a refused
// claim, a failure, or a reap after the manifest opened: the evidence a wave
// tried the step rather than holding it back.
func attemptedSince(tx *sql.Tx, runID int, openedSeq int64) (map[int]bool, error) {
	rows, err := tx.Query(
		`SELECT DISTINCT step_id FROM events
		  WHERE run_id = ? AND seq > ? AND step_id IS NOT NULL
		    AND kind IN (?, ?, ?, ?)`,
		runID, openedSeq, EventStepClaimed, EventClaimRefused, EventStepFailed,
		EventLeaseReaped)
	if err != nil {
		return nil, fmt.Errorf("reading the steps attempted in the dispatch: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reading an attempted step: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

// engineChainFact returns the engine-derived chain deferral recorded for the
// step by the run's most recently closed dispatch, if any, as the fact `run
// fact add` would have returned. A fact from an earlier dispatch does not
// cover a later one, so it is not returned.
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
		    AND json_extract(data, '$.dispatch') = (
		        SELECT 'DISPATCH-' || id FROM dispatches
		         WHERE run_id = ? AND status = ?
		         ORDER BY closed_at_ms DESC, id DESC LIMIT 1)
		  ORDER BY seq DESC LIMIT 1`,
		runID, stepID, EventStepDeferred, DeferralChain, FactSourceEngine,
		runID, store.DispatchClosed).Scan(&seq, &data)
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
