package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Triage is DKT-1901: a step's `on_fail` may name a `type="vote"` step of the
// same workflow, and a failure then goes to that panel instead of to an
// operator.
//
// It exists for the lane's FIRST executor step, which has no review upstream of
// it: `fix-loop` has nothing to route to before a review exists, so
// `waiting-human` was the only routing left, and a parked implement step was
// the single largest source of parks in the corpus — 190 of 328 on 2026-09-07,
// 164 of them resolved `override-pass`, i.e. an operator reading the same gate
// rows a panel could have read.
//
// THE FAILED STEP IS SUSPENDED, NOT PARKED AND NOT DONE (see routeStep). It
// keeps the non-terminal `gated` status it carried out of the gate stage, which
// is the shape a held routing step already wears while a materialized gate
// answers for it: nothing downstream is released, and the issue and run stay
// live so the panel can actually be dispatched — a `waiting-human` park would
// hold every step of the issue (R2b) including the panel itself.
//
// The panel's verdict is applied by applyTriageOutcome through the SAME
// transitions `docket step resolve --as` performs, because the panel is
// answering the question an operator would otherwise have answered.

// triageRouter resolves the step a panel was asked about: the predecessor
// suspended on this panel at this ordinal, or nil when the panel is an ordinary
// vote step or the suspension has already been resolved.
//
// The routing column is the link. There is no second table: a suspended step
// records the panel's name as its routing (routeStep), which is the same fact
// the readiness latch reads, so the two cannot disagree about which failure a
// panel is deciding.
func triageRouter(conn *sql.DB, panel *db.Step, def *workflow.Definition) (*db.Step, error) {
	if def == nil {
		return nil, nil
	}
	for _, name := range workflow.RoutingPredecessors(def, panel.StepName) {
		var id int
		err := conn.QueryRow(
			`SELECT id FROM steps
			  WHERE run_id = ? AND issue_id = ? AND step_name = ? AND ordinal = ?
			    AND status = ?`,
			panel.RunID, panel.IssueID, name, panel.Ordinal, db.StepGated).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading the step %s triages: %w", panel.Instance, err)
		}
		router, err := db.GetStep(conn, id)
		if err != nil {
			return nil, err
		}
		if routingIs(router.Routing, panel.StepName) {
			return router, nil
		}
	}
	return nil, nil
}

// suspendForPanel turns a routing that names a triage panel into the SUSPENSION
// the panel needs, and is THE ONE place that decision is made (DKT-1901).
//
// Both failure paths call it — a failed completion gate (routeStep) and an
// exhausted attempt budget (FailStep) — because both reach statusForRouting,
// whose step-name default is `done`. A `done` executor naming a panel is the
// unsoundness this whole design exists to avoid: it releases the step's
// ordinary successors as though the work had passed, and leaves the panel with
// no suspended row to rule on.
//
// `gated` is the suspension: the status the step already carries out of the
// gate stage, and exactly the shape a HELD routing step wears while a
// materialized gate answers for it (§7.7.3). Non-terminal, so nothing
// downstream is released; not parked, so neither R2b nor the run rollup holds
// the lane while the panel sits.
//
// A PANEL ANSWERS ONCE PER ORDINAL. Its proposal is keyed
// `(run, issue, instance)`, so a second failure at the same ordinal — after the
// panel ruled `retry` and the retried attempt failed again — would find the
// CLOSED proposal rather than open a second one, and suspending for a panel
// that has already ruled leaves the step with nothing able to resolve it. That
// case parks for an operator instead, naming the ruling already spent.
//
// A routing that does not name the panel is returned untouched, so the ordinary
// paths are byte-identical to what they were.
func suspendForPanel(
	tx *sql.Tx, step *db.Step, spec *workflow.Step,
	routing, reason, status string, nowMS int64,
) (string, string, string, error) {
	panel := spec.OnFailTarget()
	if panel == "" || routing != panel {
		return routing, reason, status, nil
	}

	spent, err := panelSpent(tx, step, panel)
	if err != nil {
		return "", "", "", err
	}
	if !spent {
		return routing, reason, db.StepGated, nil
	}

	return workflow.OnFailWaitingHuman, fmt.Sprintf(
		"%s failed again after %s already ruled on this ordinal; the panel "+
			"answers once per ordinal, so this failure is an operator's: "+
			"`docket step resolve --as retry` re-runs it, `--as fix-round` buys "+
			"a round, `--as abandon-issue` ends it",
		step.Instance, panel), db.StepWaitingHuman, nil
}

// panelSpent reports whether the triage panel at this step's ordinal has
// already reached a terminal state — i.e. it has ruled once and its proposal is
// closed, so it cannot answer a second failure at the same ordinal.
//
// A panel with no row yet is NOT spent: expansion may not have reached it, and
// the ordinary suspension is correct.
func panelSpent(tx *sql.Tx, step *db.Step, panel string) (bool, error) {
	rows, err := tx.Query(
		`SELECT status FROM steps
		  WHERE run_id = ? AND issue_id = ? AND step_name = ? AND ordinal = ?`,
		step.RunID, step.IssueID, panel, step.Ordinal)
	if err != nil {
		return false, fmt.Errorf("reading the panel %s for %s: %w",
			panel, step.Instance, err)
	}
	defer rows.Close()

	spent := false
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			return false, fmt.Errorf("reading the panel %s for %s: %w",
				panel, step.Instance, err)
		}
		if db.StepTerminal(status) {
			spent = true
		}
	}
	return spent, rows.Err()
}

// triageEvidence renders the failure a panel is being asked about: which step
// failed, which gates failed and with what exit, and what they printed.
//
// Empty for a vote step that triages nothing, which leaves every pre-existing
// proposal byte-identical.
//
// The gate OUTPUT is included rather than summarized. The panel's whole job is
// to tell an environmental failure from a real one, and that distinction lives
// in the output text — a row saying `build: exit 1` supports no verdict the
// operator's own override-pass did not already make blindly.
func triageEvidence(conn *sql.DB, panel *db.Step) (string, error) {
	defs, err := StepDefinitions(conn, panel.RunID)
	if err != nil {
		return "", err
	}
	router, err := triageRouter(conn, panel, defs[panel.WorkflowID])
	if err != nil || router == nil {
		return "", err
	}

	rows, err := db.GateResultsForStep(conn, router.ID)
	if err != nil {
		return "", fmt.Errorf("reading %s's gate results: %w", router.Instance, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s failed and routed here for triage.\n", router.Instance)

	// A step that exhausted its attempt budget records no gate rows at all, so
	// the heading is written only when there is something under it — an empty
	// "Gate results:" reads as "the gates passed", which is the opposite of why
	// this panel was convened.
	if len(rows) == 0 {
		b.WriteString("\nNo gate results were recorded: the step exhausted its " +
			"attempt budget before its completion gates ran.\n")
		return b.String(), nil
	}

	b.WriteString("\nGate results:\n")
	for _, row := range rows {
		exit := "none"
		if row.Exit != nil {
			exit = fmt.Sprint(*row.Exit)
		}
		fmt.Fprintf(&b, "- %s (%s, exit %s)\n", row.Gate, row.Verdict, exit)
		if out := strings.TrimSpace(row.Output); out != "" {
			fmt.Fprintf(&b, "%s\n", indent(out))
		}
	}
	return b.String(), nil
}

// indent offsets a gate's captured output so it reads as a quotation rather
// than as more of the surrounding prose.
func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

// applyTriageOutcome applies a panel's verdict to the step it triaged, in the
// transaction that records the panel's own routing (DKT-1901).
//
// The mapping is the panel's `on_fail_routes`, keyed by the tally's binary
// verdict. Each value performs the SAME transition `docket step resolve --as`
// performs for an operator disposing of the same failure, because that is what
// the panel is standing in for.
//
// A verdict the mapping does not name, and a tally that FAILED, both leave the
// step suspended: a panel that could not agree decided nothing, and the vote
// step's own `on_fail` — which V13a requires it to declare — is then the human
// backstop. That asymmetry is deliberate and matches the held path's: a tally
// may answer the question it was asked, and may not decline to answer it and
// still have effect.
func applyTriageOutcome(
	tx *sql.Tx, panel *db.Step, spec *workflow.Step, def *workflow.Definition,
	router *db.Step, verdict string, nowMS int64,
) error {
	routing, ok := spec.OnFailRoutes[verdict]
	if !ok {
		return nil
	}

	note := fmt.Sprintf("%s ruled %s by %s", panel.Instance, routing, verdict)
	return applyTriageRouting(tx, def, router, routing, note, nowMS)
}

// applyTriageRouting performs one triage routing on the step that was triaged,
// and then RECONCILES THE ISSUE AND RUN FOR THAT STEP.
//
// The reconcile is not optional bookkeeping. `docket step resolve --as` ends
// with the same call for the step it resolves (human.go), and the issue-level
// effects live there, not in the routing write: `abandon-issue` runs
// abandonIssue's cascade over the issue's remaining steps, every routing sweeps
// the triaged step's own interposed targets and the `after_fired` successors
// behind them, and the run rollup runs. Writing the routing row alone would
// record `abandon-issue` on one step while the issue kept scheduling work —
// the panel's decision visible in the ledger and absent from the run.
//
// The panel's own reconcile does not cover this: it runs for the PANEL's row
// with the panel's routing, and the two steps have different successors and
// different issue-level consequences.
func applyTriageRouting(
	tx *sql.Tx, def *workflow.Definition, router *db.Step,
	routing, note string, nowMS int64,
) error {
	applied := routing
	switch routing {
	case workflow.TriageWaitingHuman:
		// The panel declined to decide, or could not. The step becomes an
		// ORDINARY park, which is where it would have gone without a panel at
		// all — the operator now has the panel's rationale beside the gate rows.
		applied = workflow.OnFailWaitingHuman
		if err := db.SetStepRoutingTx(tx, router.ID,
			applied, note, db.StepWaitingHuman, db.ParkClassTriageUndecided, nowMS); err != nil {
			return err
		}
	case workflow.TriageAbandonIssue:
		applied = workflow.OnFailAbandonIssue
		if err := db.SetStepRoutingTx(tx, router.ID,
			applied, note, db.StepFailedRouted, "", nowMS); err != nil {
			return err
		}
	case workflow.TriageRetry:
		// A retry returns the step to `pending`: the issue is not settled and
		// there is nothing to sweep, so the reconcile below only refreshes the
		// mirror and the rollup.
		applied = ResolveRetry
		if err := applyTriageRetry(tx, router, note, nowMS); err != nil {
			return err
		}
	case workflow.TriageFixRound, workflow.OnFailFixLoop:
		// `fix-round` (a panel's mapping) and `fix-loop` (a panel's own
		// `on_fail`) are the same act here: the failed step's work is redone in
		// a new round. Both enter an AUTHORIZED round, because in both cases a
		// panel was convened over the failure and the round is its answer.
		applied = workflow.OnFailFixLoop
		if err := applyTriageFixRound(tx, router, def, note, nowMS); err != nil {
			return err
		}
	default:
		return nil
	}

	return reconcileIssueAndRun(
		tx, router, def, workflow.StepByName(def, router.StepName), applied, nowMS)
}

// applyTriageRetry is `--as retry` for a panel: the budget resets, the lease is
// released, and the step returns to `pending` to be claimed again.
//
// It performs the same writes ResolveRetry does, for the reasons documented
// there: a retry that left the lease held would let the original holder record
// without re-claiming, and one that left the saga's resume point would resume a
// half-finished saga under the new attempt.
func applyTriageRetry(tx *sql.Tx, router *db.Step, note string, nowMS int64) error {
	if err := db.ResetStepRetryBudgetTx(tx, router.ID, nowMS); err != nil {
		return err
	}
	if err := db.ReleaseStepLeaseTx(tx, router.ID, nowMS); err != nil {
		return err
	}
	if err := db.ClearStepStartTx(tx, router.ID); err != nil {
		return err
	}
	if err := db.AdvanceSagaTx(tx, router.ID, router.SagaStage, "", nowMS); err != nil &&
		!errors.Is(err, db.ErrSagaStageMoved) {
		return err
	}
	return db.SetStepRoutingTx(tx, router.ID, ResolveRetry, note, db.StepPending, "", nowMS)
}

// applyTriageFixRound is `--as fix-round` for a panel: an AUTHORIZED loop entry
// — the panel has read the failure and asked for the round, which is the same
// authority an operator's grant carries — and the triaged step is superseded by
// the round that replaces its work.
//
// V40b refuses the mapping at register time unless a loop body serves the
// triaged step, so the refusal an unauthorized entry would produce here is not
// reachable from a validated definition.
func applyTriageFixRound(
	tx *sql.Tx, router *db.Step, def *workflow.Definition, note string, nowMS int64,
) error {
	if _, err := db.GrantLoopTx(tx, router.RunID, router.IssueID); err != nil {
		return err
	}
	outcome, err := EnterLoopAuthorized(tx, router, def, note, nowMS)
	if err != nil {
		return err
	}
	if !outcome.Entered {
		// The counter moved under the tally. Reporting it beats recording a
		// round that was not minted: the step stays suspended and the panel's
		// own on_fail backstop is the way out.
		return db.SetStepRoutingTx(tx, router.ID,
			workflow.OnFailWaitingHuman, note+": "+outcome.Reason,
			db.StepWaitingHuman, db.ParkClassLoopBound, nowMS)
	}
	return db.SetStepRoutingTx(tx, router.ID,
		workflow.OnFailFixLoop, note, db.StepSuperseded, "", nowMS)
}

// releaseUntriaged disposes of a suspended step whose panel closed WITHOUT
// reaching a verdict the mapping is keyed on (DKT-1901) — a quorum miss, a
// proposal retired without a tally, or an operator's manual commit.
//
// THE PANEL'S OWN `on_fail` GOVERNS HERE, and this is the only place it is
// read for a triaging panel. That is what makes it the backstop: V13a already
// requires every vote step to declare it, and on a panel it answers "what
// happens to the step I was asked about when I cannot answer". The default is
// `waiting-human`, which parks the step for an operator — where it would have
// gone without a panel at all.
//
// The suspension may not simply persist. Once the proposal is closed a step
// left `gated` has nothing able to resolve it: the operator's resolution verbs
// act on a park, and no later tally will arrive. So the panel's closure always
// disposes of the step one way or another.
//
// The panel's own row is terminalized `skipped`: it was convened, it closed
// without ruling, and leaving it non-terminal would hold its own successors.
func releaseUntriaged(
	tx *sql.Tx, panel, router *db.Step, spec *workflow.Step,
	def *workflow.Definition, outcome *VoteOutcome, nowMS int64,
) error {
	routing := spec.EffectiveOnFail()
	note := fmt.Sprintf(
		"%s closed %s without reaching a verdict, so its own `on_fail` (%s) "+
			"disposes of %s",
		panel.Instance, outcome.Status, routing, router.Instance)

	if err := applyTriageRouting(tx, def, router, routing, note, nowMS); err != nil {
		return err
	}
	if err := db.SetStepRoutingTx(tx, panel.ID, workflow.OnFailSkip, note,
		db.StepSkipped, "", nowMS); err != nil {
		return err
	}
	return recordEvent(tx, eventRecord{
		Kind: EventStepSkipped, RunID: panel.RunID, Instance: panel.Instance,
		IssueID: panel.IssueID, Data: note, AtMS: nowMS,
	})
}

// triageVerdict maps a tally's outcome onto the mapping's key vocabulary.
func triageVerdict(outcome *VoteOutcome) string {
	if outcome.Status == model.ProposalStatusApproved {
		return workflow.VoteOutcomeApproved
	}
	return workflow.VoteOutcomeRejected
}

// triageDecided reports whether a tally reached a verdict the mapping can be
// keyed on: APPROVED or REJECTED, the vote's whole verdict vocabulary.
//
// A proposal still `open`, one `closed` without a tally, and one `committed` by
// an operator's manual outcome are all excluded. The first two reached no
// verdict. The third reached one outside this vocabulary — §8.4's commit sets a
// FinalOutcome string the mapping has no key for — and guessing which of two
// keys it meant would apply a routing nobody chose.
func triageDecided(outcome *VoteOutcome) bool {
	return outcome.Status == model.ProposalStatusApproved ||
		outcome.Status == model.ProposalStatusRejected
}
