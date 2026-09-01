package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// At-least-once gates and the resume decision (TDD docs/tdd/gates-trust.md
// §7.5, threat T12, engine-spec §9 item 10).
//
// §2, verbatim:
//
//	Gates are at-least-once: a `gate-started` event precedes each spawn; on
//	resume, a started-but-unrecorded gate re-runs only if its trust entry is
//	flagged re-runnable, else the step parks `waiting-human`.
//
// THE ENGINE NEVER INFERS IDEMPOTENCE. `re_runnable` is a per-ENTRY flag — the
// operator's declaration about their OWN command — and its default is false
// because the safe assumption about an unknown command is that it is not safe
// to run twice. A gate that committed, deployed, or charged something must not
// be re-run because the engine could not tell whether it finished.

// gateWasStartedButNotRecorded detects A1's state: a `gate-started` event
// committed for this step+gate, with no `gate_results` row to match it.
//
// The two halves are both necessary. A `gate-started` with no result is the
// crash window. A result with no start cannot happen — the event commits first,
// which is the entire reason the saga orders them that way (§7.5 A1).
func gateWasStartedButNotRecorded(conn *sql.DB, step *db.Step, gate string) (bool, error) {
	var started bool
	err := conn.QueryRow(
		`SELECT EXISTS(
		   SELECT 1 FROM events
		    WHERE run_id = ? AND step_id = ? AND kind = ? AND data LIKE ?)`,
		step.RunID, step.ID, EventGateStarted, "%"+gate+"%").Scan(&started)
	if err != nil {
		return false, fmt.Errorf("probing for an interrupted gate: %w", err)
	}
	if !started {
		return false, nil
	}

	recorded, err := db.HasGateResult(conn, step.ID, gate)
	if err != nil {
		return false, err
	}
	return !recorded, nil
}

// resolveInterruptedGate applies §7.5's A2-A6 decision to a
// started-but-unrecorded gate.
//
// It consults the LIVE trust store (A2), not a cached decision: the question is
// what the operator declares about this command NOW, and a revocation between
// the crash and the resume must take effect.
func (e *Engine) resolveInterruptedGate(
	conn *sql.DB, step *db.Step, gate workflow.Gate, stage string, nowMS int64,
) error {
	entry, reason, err := e.reRunnableEntryFor(gate, step)
	if err != nil {
		return err
	}

	// A3: `re_runnable = true` ⇒ the gate re-runs. A `gate-rerun` event
	// precedes it, so the trail shows BOTH the interrupted attempt's start and
	// the re-run — an operator reading the feed sees that the command ran
	// twice, which is what at-least-once actually means.
	if entry != nil && entry.ReRunnable {
		tx, txErr := conn.Begin()
		if txErr != nil {
			return fmt.Errorf("announcing the gate re-run: %w", txErr)
		}
		// NO AtMS (DKT-66): `nowMS` here is the RESUME's clock, taken before the
		// interrupted gate was even diagnosed. Stamping the announcement with it
		// put the re-run in the feed ahead of the events that preceded it.
		if evErr := recordEvent(tx, eventRecord{
			Kind: EventGateRerun, RunID: step.RunID, Instance: step.Instance,
			IssueID: step.IssueID, Data: gate.Name,
		}); evErr != nil {
			tx.Rollback()
			return evErr
		}
		if cErr := tx.Commit(); cErr != nil {
			return fmt.Errorf("committing the gate re-run announcement: %w", cErr)
		}
		return e.runGateStage(conn, step, gate, stage, nowMS)
	}

	// A4/A5: `re_runnable = false` (THE DEFAULT), or unmatched at resume ⇒ the
	// step PARKS. It does not re-run and it does not silently pass.
	//
	// A5 is worth its own sentence: an unmatched-at-resume gate parks too,
	// rather than "re-run because we can't tell". Fail-closed is the only
	// direction available when the question is whether something already
	// happened.
	return e.parkInterruptedGate(conn, step, gate, reason, nowMS)
}

// reRunnableEntryFor resolves the trust entry that would match this gate, so
// the resume decision reads the operator's own declaration.
//
// A named gate resolves by name and binding. A FENCE gate has no single entry —
// it is many commands, each matched independently (§7.3) — so it is treated as
// not re-runnable: re-running a partially-executed block would re-run the lines
// that already succeeded, which is precisely the double-execution T12 forbids.
func (e *Engine) reRunnableEntryFor(
	gate workflow.Gate, step *db.Step,
) (*trust.Entry, string, error) {
	runner := execRunnerBehind(e.Gates)
	if runner == nil {
		// A runner that is not the real one (the pass-through, a test fake)
		// spawns nothing, so there is nothing to double-run and nothing to
		// decide. Re-running is safe by construction.
		return &trust.Entry{ReRunnable: true}, "", nil
	}

	if gate.Source != "" {
		return nil, fmt.Sprintf(
			"gate %q was interrupted after it started; it harvests its commands from %s, "+
				"so docket cannot tell which of them ran and will not run them again",
			gate.Name, gate.Source), nil
	}

	store, err := runner.LoadStore()
	if err != nil {
		return nil, fmt.Sprintf(
			"gate %q was interrupted after it started, and the trust store could not be read: %v",
			gate.Name, err), nil
	}
	identity, err := trust.RepoIdentity(runner.RepoRoot)
	if err != nil {
		return nil, fmt.Sprintf(
			"gate %q was interrupted after it started, and the repo path could not be resolved: %v",
			gate.Name, err), nil
	}

	match := store.Lookup(identity, gate.Name, nil)
	if !match.Matched {
		// A5.
		return nil, fmt.Sprintf(
			"gate %q was interrupted after it started, and it is no longer trusted: %s",
			gate.Name, match.Reason), nil
	}
	if !match.Entry.ReRunnable {
		// A4/A6: the default. The message names the gate, the situation, and
		// BOTH operator resolutions, because a park with no stated remedy is a
		// step nobody knows how to unstick.
		return match.Entry, fmt.Sprintf(
			"gate %q started but its result was never recorded, so docket cannot tell "+
				"whether it ran. It is not marked re-runnable, so it will not be run again. "+
				"Resolve with `docket step resolve --as retry` to run the step from the top, "+
				"or `--as override-pass` to accept it. If this command is safe to run twice, "+
				"re-approve it with `docket trust add %s --re-runnable -- …`",
			gate.Name, gate.Name), nil
	}
	return match.Entry, "", nil
}

// trustBackedRunner is implemented by a runner that wraps the real one, so a
// decorator (a spawn counter, a recorder) does not silently turn the resume
// decision into "re-running is safe".
//
// It exists because the alternative — a bare type assertion to *ExecRunner —
// FAILS OPEN: any wrapper would fall through to the no-op branch and every
// interrupted gate would re-run regardless of its flag. A security decision
// that depends on a concrete type is one refactor away from being wrong.
type trustBackedRunner interface {
	TrustRunner() *ExecRunner
}

// TrustRunner reports the runner itself, so *ExecRunner satisfies the interface
// its own wrappers use.
func (r *ExecRunner) TrustRunner() *ExecRunner { return r }

// execRunnerBehind unwraps a runner to the real one, or reports nil when there
// is none.
func execRunnerBehind(g GateRunner) *ExecRunner {
	if backed, ok := g.(trustBackedRunner); ok {
		return backed.TrustRunner()
	}
	return nil
}

// parkInterruptedGate records the refusal and parks the step `waiting-human`.
//
// The park is recorded as a gate RESULT as well as a step status, so the run's
// trail carries why: an operator opening the step sees the interrupted gate and
// the reason, rather than a `waiting-human` with no explanation.
func (e *Engine) parkInterruptedGate(
	conn *sql.DB, step *db.Step, gate workflow.Gate, reason string, nowMS int64,
) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("parking an interrupted gate: %w", err)
	}
	defer tx.Rollback()

	if err := recordGateRows(tx, step, gate.Name, []GateResultRow{{
		Gate: gate.Name, Verdict: VerdictUnmatched, Reason: reason,
	}}, nowMS); err != nil {
		return err
	}
	// NO AtMS (DKT-66): the gate ROW keeps `nowMS`, because the row describes
	// the attempt; the EVENT stamps at emission, because the feed describes the
	// order things were observed in.
	//
	// The VERDICT rides along (DKT-63) so this reads as a refusal in the feed
	// rather than as a gate that merely happened. There is no exit code: an
	// unmatched gate never ran, and `exit=0` here would read as a pass.
	//
	// `gate.Pre` is read rather than hardcoded false (DKT-862). It IS false on
	// every reachable call today — this path resolves a completion gate, and
	// completionGates drops the `pre` ones — but the marker has exactly one
	// definition, the declaration itself, and a second spelling here is how the
	// two surfaces would start to disagree again.
	unmatched, err := gateEventData(gate.Name, VerdictUnmatched, nil, gate.Pre, false)
	if err != nil {
		return err
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventGateUnmatched, RunID: step.RunID, Instance: step.Instance,
		IssueID: step.IssueID, Data: unmatched,
	}); err != nil {
		return err
	}
	if err := db.SetStepRoutingTx(tx, step.ID,
		routingRecord(workflow.OnFailWaitingHuman, reason),
		db.StepWaitingHuman, nowMS); err != nil {
		return err
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepRouted, RunID: step.RunID, Instance: step.Instance,
		IssueID: step.IssueID, Data: workflow.OnFailWaitingHuman, AtMS: nowMS,
	}); err != nil {
		return err
	}
	// THE ISSUE MIRROR AND RUN ROLLUP STILL RUN (DKT-379). This path routes —
	// it writes the routing column and a `step-routed` event two statements up
	// — and §6.8 makes reconciliation part of what routing IS, in the same
	// transaction. Leaving it out meant an issue parked ONLY by an interrupted
	// gate never read `review`: the mirror fires from reconcileIssueAndRun
	// alone, and this was the one park with nothing downstream left to call it.
	// The run had the same hole, staying `active` with a parked step in it.
	//
	// `spec` is nil, which is exactly right here rather than a shortcut:
	// skipUnroutedTargets is threshold-target bookkeeping and an interrupted
	// gate decided no threshold. The completion check inside is false by
	// construction — the step this call is about is `waiting-human`, which is
	// not terminal.
	//
	// This is NOT the saga advancing; see below.
	if err := reconcileIssueAndRun(
		tx, step, nil, workflow.OnFailWaitingHuman, nowMS,
	); err != nil {
		return err
	}
	// THE SAGA LEAVES ENTIRELY — it does not advance to routing.
	//
	// Advancing to `routing` would let the resume loop run the routing stage
	// next, which recomputes the routing from the gate verdict and OVERWRITES
	// the reason recorded just above. The park is a terminal disposition for
	// this attempt: a `step resolve` moves it now, not another resume, and
	// clearing `saga_stage` is what makes InSaga() false so the loop stops.
	//
	// Reconciling above and refusing the stage here are different things: the
	// first is the routing's own consequences, the second is whether the saga
	// machine takes another turn.
	if err := db.AdvanceSagaTx(tx, step.ID, step.SagaStage, "", nowMS); err != nil {
		return err
	}
	return tx.Commit()
}
