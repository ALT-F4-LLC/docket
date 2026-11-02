package engine

import (
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
	tx store.LegacyTx, runID int, open *store.Dispatch, steps []*store.Step, nowMS int64,
) error {
	dispatchID := open.ID
	st := store.TxOn(tx)
	rows, err := st.ListDispatchRowsTx(dispatchID)
	if err != nil {
		return err
	}
	attempted, err := attemptedSince(st, runID, open.OpenedSeq)
	if err != nil {
		return err
	}
	deferred, err := chainDeferredRows(rows, steps, attempted)
	if err != nil {
		return err
	}
	dispatch := FormatDispatchID(dispatchID)
	for _, step := range deferred {
		seen, err := st.DeferralRecordedForDispatchTx(store.DispatchDeferralQuery{
			RunID: runID, StepID: step.ID, Kind: EventStepDeferred,
			Cause: DeferralChain, Dispatch: dispatch,
		})
		if err != nil {
			return err
		}
		if seen {
			continue
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
func attemptedSince(tx store.ReadTx, runID int, openedSeq int64) (map[int]bool, error) {
	return tx.StepsWithEventKindsSinceTx(runID, openedSeq, []string{
		EventStepClaimed, EventClaimRefused, EventStepFailed, EventLeaseReaped,
	})
}

// engineChainFact returns the engine-derived chain deferral recorded for the
// step by the run's most recently closed dispatch, if any, as the fact `run
// fact add` would have returned. A fact from an earlier dispatch does not
// cover a later one, so it is not returned.
func engineChainFact(tx store.LegacyTx, runID, stepID int) (*RunFact, bool, error) {
	seq, data, found, err := store.TxOn(tx).LastDispatchDeferralTx(store.SourcedDeferralQuery{
		RunID: runID, StepID: stepID, Kind: EventStepDeferred,
		Cause: DeferralChain, Source: FactSourceEngine,
	})
	if err != nil {
		return nil, false, fmt.Errorf("reading the engine's chain deferral: %w", err)
	}
	if !found {
		return nil, false, nil
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
