package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// THE PIN SECTIONS OF `run report` (DKT-594).
//
// Two questions an analyst had to answer with git and a hand-join, on a corpus
// that took 41 commits in 4.3 days:
//
//  1. HOW FAR HAS THE CORPUS MOVED SINCE THIS RUN FROZE. Every reader of
//     RUN-32's report had to go to git to learn that its `ui-change@8` was five
//     registered versions behind before they would trust a finding from it. The
//     report published the run's pins nowhere and the registry's head nowhere,
//     so the subtraction was not available from any read verb.
//
//  2. WHICH AGREEMENT DID THIS STEP ACTUALLY RUN UNDER. After a mid-run repin
//     the `pins` table says what the REMAINING steps will read, and says
//     nothing about the bytes the completed ones consumed. RUN-39's post-mortem
//     recovered that by correlating repin event seqs (5375/5376) against step
//     ids (STEP-1350/1353) by hand.
//
// NO NEW PERSISTED STATE ANSWERS EITHER. The first is a subtraction over
// `pins.ref` (a workflow pin's ref IS `name@version` — activation writes
// `bound.workflow.Ref()`) and the `workflows` table. The second is exactly the
// reconstruction repin.go's own architecture note reserves for the event log:
// "the event log is already the package's history mechanism … a parallel
// pin-history table would be a second source of the same fact", and
// EventRunRepinned's comment already states the reconstruction rule — "steps
// recorded before this event's seq worked under `old_sha256`, steps claimed
// after it work under `new_sha256`". This file performs that reconstruction
// instead of asking an operator to.

// PinnedWorkflowStaleness is criterion 1: one pinned workflow, and how far the
// registry has moved past it.
//
// It covers WORKFLOW pins only. A `schema` pin is also a registered
// `name@version` and could carry the same diff, but the issue asks about
// workflows and a schema's version moves for different reasons; a FILE pin
// (a contract, a fragment, `policy.toml`) has no version at all — its "11
// versions stale" in the retro means corpus git commits, which live outside
// this store and which core cannot count. `verify-pins` is the verb that
// answers the file question, by hash.
type PinnedWorkflowStaleness struct {
	// Ref is the pin's ref verbatim — `name@version`, what the run froze.
	Ref  string `json:"ref"`
	Name string `json:"name"`
	// PinnedVersion is the version this run is expanded from.
	PinnedVersion int `json:"pinned_version"`
	// CurrentVersion is the highest registered version of this name that STILL
	// BINDS — the version a run activated now would pin. Zero when no version
	// of the name binds any more (every one retired, or the name is gone from
	// this project's registry), which is a fact worth publishing rather than
	// rounding to the pinned version.
	CurrentVersion int `json:"current_version"`
	// Behind is THE STALENESS COUNT: how many binding versions are registered
	// STRICTLY ABOVE the pinned one.
	//
	// A count of rows rather than `current - pinned`, and never negative. The
	// subtraction would go negative the moment a higher version is retired
	// while a run holds it, and would over-count where a name's versions are
	// not contiguous — a registry with @1 and @8 and nothing between is one
	// version of advance, not seven.
	Behind int `json:"behind"`
}

// PinEpoch is criterion 2's timeline: one segment of a run's life during which
// its pin agreement did not move.
//
// Epoch 1 is ACTIVATION — the agreement the run froze. Every later epoch is one
// `run repin`, which is the only thing in the engine that rewrites a `pins` row
// (the two statements in repin.go are the whole set; activation's own writes are
// `INSERT OR IGNORE`, so re-activation ADDS refs and never moves one).
type PinEpoch struct {
	// Epoch numbers from 1, so a zero on a step means "no epoch", never
	// "the first one".
	Epoch int `json:"epoch"`
	// FromSeq is the event seq this agreement took effect at — the
	// `run-activated` event for epoch 1, the repin's first event for the rest.
	// Zero for epoch 1 on a run whose activation event has been pruned away.
	FromSeq int64 `json:"from_seq"`
	AtMS    int64 `json:"at_ms,omitempty"`
	// Origin is `activation` or `repin`.
	Origin string `json:"origin"`
	// Reason is the operator's `--reason`, verbatim, on a repin epoch.
	Reason string `json:"reason,omitempty"`
	// Changes is what MOVED entering this epoch, one entry per changed ref,
	// carrying both hashes exactly as the event recorded them. Empty on epoch 1:
	// nothing moved, the run froze.
	Changes []RepinChange `json:"changes,omitempty"`
}

// PinEpochOrigin values.
const (
	PinEpochActivation = "activation"
	PinEpochRepin      = "repin"
)

// pinnedWorkflowStaleness answers criterion 1 for a whole run.
//
// It reads through the POOL, before the report's snapshot opens, for the reason
// LoadRunReport's own comment gives: internal/db caps the pool at one
// connection, so a pool read from inside the open transaction deadlocks
// permanently. Nothing here needs the snapshot — a pin set and a registry head
// read a moment early are the same two numbers.
func pinnedWorkflowStaleness(
	conn *sql.DB, projectID, runID int,
) ([]PinnedWorkflowStaleness, error) {
	pins, err := db.ListPins(conn, runID)
	if err != nil {
		return nil, err
	}

	type pinned struct {
		ref     string
		name    string
		version int
	}
	var wanted []pinned
	names := make([]string, 0, len(pins))
	seen := make(map[string]bool, len(pins))
	for _, p := range pins {
		if p.Kind != db.PinKindWorkflow {
			continue
		}
		name, version, err := workflow.ParsePayloadRef(p.Ref)
		if err != nil {
			// A ref that does not parse as `name@version` is not a workflow pin
			// this diff can speak about. It is REPORTED BY `verify-pins` (which
			// calls it missing) and skipped here rather than failing the whole
			// report: R10 says the report works on a run in any state, and a
			// rollup that refused would be unavailable during exactly the
			// incident that produced the odd row.
			continue
		}
		wanted = append(wanted, pinned{ref: p.Ref, name: name, version: version})
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	registered, err := db.WorkflowVersionsFor(conn, projectID, names)
	if err != nil {
		return nil, err
	}
	binding := make(map[string][]int, len(names))
	for _, v := range registered {
		if v.Binds {
			binding[v.Name] = append(binding[v.Name], v.Version)
		}
	}

	out := make([]PinnedWorkflowStaleness, 0, len(wanted))
	for _, p := range wanted {
		row := PinnedWorkflowStaleness{
			Ref: p.ref, Name: p.name, PinnedVersion: p.version,
		}
		for _, v := range binding[p.name] {
			if v > row.CurrentVersion {
				row.CurrentVersion = v
			}
			if v > p.version {
				row.Behind++
			}
		}
		out = append(out, row)
	}
	// A TOTAL order (R9), by the ref the run holds, so two reports of one
	// unchanged run are byte-identical.
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// runPinEpochsTx reconstructs the run's pin-agreement timeline from the event
// log, newest last.
//
// It returns NOTHING when the run never repinned. One epoch is not a timeline:
// every step of such a run ran under the agreement the `pins` table still
// states, the reader needs no reconciliation, and publishing an `epoch 1` on
// every step of every report would be a column of constants.
//
// GROUPING. A repin writes one event PER CHANGED REF, all in its own
// transaction, so three moved contracts are three events and ONE boundary.
// Events of a single transaction are consecutive in `seq` (SQLite serializes
// writers, so no other transaction's insert can land between them) and share
// the `nowMS` their caller passed, so a maximal contiguous same-`at_ms` run of
// this run's repin events is that transaction. The one shape this cannot split
// is two repin transactions that committed in the same MILLISECOND with nothing
// between them — reachable only with a frozen clock, and harmless where it is:
// no step can have executed inside a zero-length interval, so no step's epoch
// moves, only the count of boundaries.
func runPinEpochsTx(tx *sql.Tx, runID int) ([]PinEpoch, error) {
	rows, err := tx.Query(
		`SELECT seq, at_ms, kind, data FROM events
		  WHERE run_id = ? AND kind IN (?, ?) ORDER BY seq`,
		runID, EventRunActivated, EventRunRepinned)
	if err != nil {
		return nil, fmt.Errorf("reading the run's pin history: %w", err)
	}
	type eventRow struct {
		seq  int64
		atMS int64
		kind string
		data string
	}
	events, err := scanTxRows(rows, func(r *sql.Rows) (eventRow, error) {
		var e eventRow
		if err := r.Scan(&e.seq, &e.atMS, &e.kind, &e.data); err != nil {
			return eventRow{}, fmt.Errorf("reading a pin-history event: %w", err)
		}
		return e, nil
	})
	if err != nil {
		return nil, err
	}

	activation := PinEpoch{Epoch: 1, Origin: PinEpochActivation}
	var epochs []PinEpoch
	for _, e := range events {
		if e.kind == EventRunActivated {
			// THE FIRST activation. A re-activation writes a second one, and it
			// is not a boundary: RA2 INHERITS the pin set, and the only rows a
			// re-activation writes are `INSERT OR IGNORE` additions for refs the
			// run did not hold — no existing pin's bytes move, so no completed
			// step's provenance changes.
			if activation.FromSeq == 0 {
				activation.FromSeq, activation.AtMS = e.seq, e.atMS
			}
			continue
		}
		var payload struct {
			RepinChange
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(e.data), &payload); err != nil {
			// Core's own payload, but a malformed one costs the CHANGE LINE and
			// not the boundary: that this run repinned here is the event's
			// existence, and a report that refused over an unparseable row would
			// be unavailable during the incident that produced it.
			payload = struct {
				RepinChange
				Reason string `json:"reason"`
			}{}
		}
		last := len(epochs) - 1
		if last >= 0 && epochs[last].AtMS == e.atMS &&
			e.seq == epochs[last].FromSeq+int64(len(epochs[last].Changes)) {
			epochs[last].Changes = append(epochs[last].Changes, payload.RepinChange)
			continue
		}
		epochs = append(epochs, PinEpoch{
			Epoch:   len(epochs) + 2, // epoch 1 is activation
			FromSeq: e.seq,
			AtMS:    e.atMS,
			Origin:  PinEpochRepin,
			Reason:  payload.Reason,
			Changes: []RepinChange{payload.RepinChange},
		})
	}
	if len(epochs) == 0 {
		return nil, nil
	}
	return append([]PinEpoch{activation}, epochs...), nil
}

// stepEpochAnchorKinds are the events that say A STEP'S RECORDED WORK HAPPENED
// HERE — the seq an epoch lookup is keyed on.
//
// `step-claimed` is the load-bearing one: a claim is where the packet is
// rendered, and repin's quiescence guard refuses while ANY step is claimed, so
// a claim and its completion can never straddle a boundary. The terminal kinds
// carry the steps that never get claimed at all — a vote step (permanently
// attempt 0), a skipped one, one a cascade terminated.
//
// Deliberately EXCLUDED: `step-ready` (a step can be ready for an epoch and run
// in the next), `step-heartbeat` and `lease-reaped` (a reaped attempt recorded
// nothing, and the step's re-claim is the anchor), and `step-annotated` (it
// runs AFTER completion, potentially after a later repin, and annotating a
// record is not consuming bytes).
var stepEpochAnchorKinds = map[string]bool{
	EventStepClaimed:    true,
	EventStepRecorded:   true,
	EventGateRecorded:   true,
	EventStepRouted:     true,
	EventStepFailed:     true,
	EventStepSkipped:    true,
	EventStepSuperseded: true,
	EventStepResolved:   true,
	EventStepApproved:   true,
	EventStepRejected:   true,
	EventVoteTallied:    true,
}

// stepPinEpochsTx maps each step id onto the epoch its recorded work ran under.
//
// The LAST anchor wins. A step claimed, reaped, and re-claimed after a repin
// consumed the NEW bytes, and the attempt that was thrown away is not the one
// the report is describing.
func stepPinEpochsTx(tx *sql.Tx, runID int, epochs []PinEpoch) (map[int]int, error) {
	if len(epochs) < 2 {
		return nil, nil
	}
	rows, err := tx.Query(
		`SELECT step_id, kind, seq FROM events
		  WHERE run_id = ? AND step_id IS NOT NULL ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading the run's step events: %w", err)
	}
	defer rows.Close()

	anchors := make(map[int]int64)
	for rows.Next() {
		var (
			stepID int
			kind   string
			seq    int64
		)
		if err := rows.Scan(&stepID, &kind, &seq); err != nil {
			return nil, fmt.Errorf("reading a step event: %w", err)
		}
		if stepEpochAnchorKinds[kind] && seq > anchors[stepID] {
			anchors[stepID] = seq
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the run's step events: %w", err)
	}

	out := make(map[int]int, len(anchors))
	for stepID, seq := range anchors {
		out[stepID] = epochAt(epochs, seq)
	}
	return out, nil
}

// epochAt is the lookup EventRunRepinned's own comment describes: the last
// agreement that took effect at or before this seq.
func epochAt(epochs []PinEpoch, seq int64) int {
	epoch := epochs[0].Epoch
	for _, e := range epochs {
		if seq >= e.FromSeq {
			epoch = e.Epoch
		}
	}
	return epoch
}

// stepReportsAnEpoch decides whether a step's effective status makes "the
// agreement it ran under" a fact rather than a forecast.
//
// A `pending` or `ready` step has not consumed anything: whatever its history
// (a reaped claim leaves anchor events behind), the bytes it will read are the
// ones the `pins` table holds when it is finally claimed, and stamping it with
// the epoch of an attempt that recorded nothing would answer a question about
// the future with a fact about the past.
func stepReportsAnEpoch(status string) bool {
	switch status {
	case db.StepClaimed, db.StepDone, db.StepWaitingHuman,
		db.StepSkipped, db.StepSuperseded, db.StepFailedRouted:
		return true
	}
	return false
}

// annotatePinEpochs stamps each attempt row with the epoch its work ran under.
//
// It runs INSIDE the report's snapshot, like annotateVoteOutcomes and for the
// same two reasons: the one-connection pool makes a pool read from in here a
// permanent deadlock, and the annotation then describes the same instant as the
// statuses beside it.
func annotatePinEpochs(
	tx *sql.Tx, runID int, attempts []StepAttempt,
) ([]PinEpoch, error) {
	epochs, err := runPinEpochsTx(tx, runID)
	if err != nil {
		return nil, err
	}
	if len(epochs) < 2 {
		return nil, nil
	}
	byStep, err := stepPinEpochsTx(tx, runID, epochs)
	if err != nil {
		return nil, err
	}
	for i := range attempts {
		if !stepReportsAnEpoch(attempts[i].Status) {
			continue
		}
		id, err := model.ParseStepID(attempts[i].Step)
		if err != nil {
			continue
		}
		attempts[i].PinEpoch = byStep[id]
	}
	return epochs, nil
}
