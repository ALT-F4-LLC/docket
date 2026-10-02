package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// DKT-742 — the run-scoped stale-target waiver.
//
// The stale-target advisory (DKT-193/424/451) has no memory: an operator who
// investigated a warning and ruled it acceptable saw the IDENTICAL warning
// re-fire at every subsequent `dispatch open`/`verify` of the same pair —
// RUN-52 fired one adjudicated warning four times across three dispatches,
// each firing costing an investigation and (the first time) an operator gate,
// until the operator issued a standing waiver that lived only in session
// memory, where the engine could not see it. `dispatch waive-target` records
// that ruling ONCE, as one waiver per named step instance for one target sha,
// and staleTargets drops matching rows from every later advisory.
//
// THE WARNING MACHINERY STILL RUNS. What a waiver changes is the reporting of
// a (step, target) pair the operator already ruled on — the same pair on a
// DIFFERENT sha, or a different row on the same sha, warns exactly as before,
// which is what keeps a new divergence from sailing through under an old
// ruling. Run-scoped by the row's run_id, like gate_override_grants
// (DKT-546): a new run re-warns.

// WaivedTarget names one recorded waiver, as the verb reports it.
type WaivedTarget struct {
	ID       int    `json:"id"`
	Instance string `json:"instance"`
	Target   string `json:"target_sha"`
}

// waiverSHAPattern is what `--target` must look like: a hex sha, or an
// unambiguous prefix of one. The 7-character floor matches git's own
// short-sha convention; the advisory the operator copies from renders 12.
var waiverSHAPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// WaiveStaleTargets records one waiver per named step instance, all for one
// target sha, in ONE transaction — the operator's ruling either covers every
// row they listed or none of them. Each waiver logs its own
// `stale-target-waived` event, so the feed shows exactly what standing
// precedent was minted and by which verb.
//
// The instances are the strings the advisory itself printed (`review@1#0`),
// matched verbatim by staleTargets — which is also why nothing here checks
// them against live steps: a fanout sibling minted by a later round is a NEW
// instance with a new signature, and the operator waives what warned, not
// what might.
func (e *Engine) WaiveStaleTargets(
	conn *sql.DB, runID int, instances []string, targetSHA, note string,
	nowMS int64,
) ([]WaivedTarget, error) {
	if len(instances) == 0 {
		return nil, validationErr("name at least one step instance to waive")
	}
	for _, instance := range instances {
		if instance == "" {
			return nil, validationErr("a step instance cannot be empty")
		}
	}
	if !waiverSHAPattern.MatchString(targetSHA) {
		return nil, validationErr(
			"target sha %q is not a hex commit sha (or a prefix of at least "+
				"7 characters); pass the `target_sha` the stale-target "+
				"warning named", targetSHA)
	}

	if _, err := db.GetRun(conn, runID); err != nil {
		if errors.Is(err, db.ErrRunNotFound) {
			return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
		}
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("recording the stale-target waiver(s): %w", err)
	}
	defer tx.Rollback()

	out := make([]WaivedTarget, 0, len(instances))
	for _, instance := range instances {
		id, err := db.InsertStaleTargetWaiverTx(tx, db.StaleTargetWaiver{
			RunID: runID, StepInstance: instance, TargetSHA: targetSHA,
			Note: note, CreatedAtMS: nowMS,
		})
		if err != nil {
			return nil, err
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventStaleTargetWaived, RunID: runID, Instance: instance,
			Data: fmt.Sprintf("%s#%d", targetSHA, id), AtMS: nowMS,
		}); err != nil {
			return nil, err
		}
		out = append(out, WaivedTarget{ID: id, Instance: instance, Target: targetSHA})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("recording the stale-target waiver(s): %w", err)
	}
	return out, nil
}
