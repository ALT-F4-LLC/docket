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
	fmt.Fprintf(&b, "%s failed its completion gates and routed here for triage.\n",
		router.Instance)
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
	switch routing {
	case workflow.TriageWaitingHuman:
		// The panel declined to decide. The step becomes an ORDINARY park, which
		// is where it would have gone without a panel at all — the operator now
		// has the panel's rationale beside the gate rows.
		return db.SetStepRoutingTx(tx, router.ID,
			workflow.OnFailWaitingHuman, note, db.StepWaitingHuman, nowMS)
	case workflow.TriageAbandonIssue:
		return db.SetStepRoutingTx(tx, router.ID,
			workflow.OnFailAbandonIssue, note, db.StepFailedRouted, nowMS)
	case workflow.TriageRetry:
		return applyTriageRetry(tx, router, note, nowMS)
	case workflow.TriageFixRound:
		return applyTriageFixRound(tx, router, def, note, nowMS)
	}
	return nil
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
	return db.SetStepRoutingTx(tx, router.ID, ResolveRetry, note, db.StepPending, nowMS)
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
			db.StepWaitingHuman, nowMS)
	}
	return db.SetStepRoutingTx(tx, router.ID,
		workflow.OnFailFixLoop, note, db.StepSuperseded, nowMS)
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
