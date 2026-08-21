package engine

import (
	"database/sql"

	"github.com/ALT-F4-LLC/docket/internal/db"
)

// MID-WAVE LIFECYCLE DRIVING.
//
// `next` refuses while a dispatch is open — deliberately (P24) — so for as
// long as a wave runs, nothing used to advance the engine-side lifecycles:
// a vote step readied by the wave's own recordings sat with no proposal until
// the NEXT round's `next` opened one, an action step sat ready and unexecuted,
// and a quorum reached mid-wave stayed unrouted. Every engine-run step in a
// chain therefore cost a full dispatch round-trip, which defeats the staged
// closure (lookahead.go): an offer may stage `judges -> gate -> reconcile ->
// report`, but only if something actually drives the gate and the action WHEN
// the wave's recordings ready them.
//
// The drivers themselves already exist — driveVoteSteps and driveActionSteps
// (next.go), §8.1's "the first engine invocation that observes the step
// ready". These entry points make `step record` and the quorum-reaching `vote
// cast` observing invocations too: the verb layer calls DriveRunLifecycles
// after its own write commits, exactly as `next` and `dispatch open` call the
// same drivers after theirs. No daemon watches; the lifecycle still advances
// only when somebody talks to the engine.

// DriveRunLifecycles advances every engine-run lifecycle a run has ready — the
// vote phases (open, read, route) and the action steps — to quiescence.
//
// A LOOP, because routing cascades: an action's routing un-defers a step
// whose readiness exposes another action, and a routed vote can do the same.
// Each pass reloads the snapshot (the previous pass's routings made it stale —
// the DKT-55 lesson applied here by recomputation) and stops on the first pass
// that routed nothing. The pass count is bounded by the run's step count plus
// one: every routing pass moves at least one step OFF ready, so a hypothetical
// non-converging chain is an engine fault the bound turns from a hang into a
// finished call.
//
// Errors are engine faults, exactly as next.go classifies them: B3 already
// routes a step that cannot run per its `on_fail`, so anything failing here is
// a database the next verb would hit too, and reporting beats proceeding.
func (e *Engine) DriveRunLifecycles(conn *sql.DB, runID int, nowMS int64) error {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return err
	}
	passes, err := runStepCount(conn, runID)
	if err != nil {
		return err
	}
	for pass := 0; pass <= passes; pass++ {
		sched, err := readySnapshot(conn, runID, defs, nowMS)
		if err != nil {
			return err
		}
		var ready []*db.Step
		for _, step := range sched.Steps() {
			if ok, _ := sched.Ready(step); ok {
				ready = append(ready, step)
			}
		}
		_, voteRouted, err := e.driveVoteSteps(
			conn, defs, pendingVoteSteps(sched), ready, sched.holdTally, nowMS)
		if err != nil {
			return err
		}
		actionRouted, err := e.driveActionSteps(conn, ready, nowMS)
		if err != nil {
			return err
		}
		if !voteRouted && !actionRouted {
			return nil
		}
	}
	return nil
}

// DriveVoteProposal is DriveRunLifecycles addressed by PROPOSAL: the hook the
// `vote cast` verb calls when its cast reached quorum, which knows the ballot
// it filled but not the run it belongs to.
//
// The run is recovered from the proposal's own idempotency key —
// `vote-step:<run>:<issue>:<instance>`, the link OpenVoteProposal recorded at
// phase 2 — rather than from a column on the proposal, for
// loadVoteProposalsTx's reason: the link already exists and is authoritative,
// and a second copy could disagree with it. A proposal with no such key is an
// ad-hoc, operator-created ballot bound to no step; there is nothing to
// route, and the call is a no-op rather than an error.
func (e *Engine) DriveVoteProposal(conn *sql.DB, proposalID int, nowMS int64) error {
	key, found, err := db.IdempotencyKeyOf(conn, db.ScopeVoteCreate, proposalID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	runID, ok := voteStepRunOf(key)
	if !ok {
		return nil
	}
	return e.DriveRunLifecycles(conn, runID, nowMS)
}

// runStepCount bounds DriveRunLifecycles' cascade (see its doc comment).
func runStepCount(conn *sql.DB, runID int) (int, error) {
	var n int
	err := conn.QueryRow(
		`SELECT COUNT(*) FROM steps WHERE run_id = ?`, runID).Scan(&n)
	return n, err
}
