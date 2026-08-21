package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `guard stop|gate` — TDD §6.12.
//
// Both are deterministic predicates over engine state, and both answer with
// EXIT 0 ALLOW / EXIT 2 DENY WITH REASON (§2).
//
// Exit 2 collides numerically with NOT_FOUND's exit 2, and THAT IS INTENTIONAL
// AND SPECIFIED. §2 defines the guard contract as "exit 0/2 + reason",
// INDEPENDENT of the error taxonomy, because guards are hook predicates whose
// caller tests a boolean — not CLI verbs whose caller maps a code. This is
// recorded here because a reviewer will otherwise read it as a taxonomy
// violation; it is the spec's own contract.
//
// `guard record|spawn` are stage 6's and live in guard_dispatch.go, beside the
// dispatch state they are predicates over. They share GuardVerdict and the exit
// contract; what they do not share is a file, because their subject is the
// manifest rather than the step graph.

// GuardVerdict is one guard's answer.
type GuardVerdict struct {
	// Allowed is exit 0 when true, exit 2 when false.
	Allowed bool
	// Reason explains a denial. It goes to stderr in human mode and into the
	// JSON envelope's `error` under --json.
	Reason string
}

// GuardStop answers `docket guard stop`: is the machine done working?
//
// The base rule (§6.12): a step in `claimed`/`running`/`gated`/`pending` for
// an active AND DISPATCHED run blocks a stop. Four refinements narrow it to
// steps a stop would actually interfere with:
//
//   - A run NOTHING HAS EVER HAPPENED TO does not block at all (DKT-71):
//     never dispatched, and no step ever out of `pending`. Nothing was handed
//     to anything, so there is nothing in flight for a stop to interrupt. See
//     the probe in GuardStop.
//
//   - A `waiting-human` step does NOT block — a run parked on a person is
//     waiting for something a stop cannot interfere with.
//   - A VOTE step whose proposal is OPEN does not block (DKT-107): its panel
//     decides out-of-session, and yielding the turn is exactly how a session
//     waits for it — the deny was a toll paid at every turn-end for as long
//     as a panel deliberated. The exemption ends with the proposal: a decided
//     proposal leaves a dispatchable step, which blocks again until `next`
//     routes it. The same reading covers a `gated` routing step whose every
//     unresolved held cluster is such a vote.
//   - A `pending` step waiting only on its predecessors does not block on its
//     own: whatever it waits on either blocks in its place or is exempt for a
//     reason that covers the whole chain. Any other unreadiness — headroom, a
//     paused run, a budget stop, an unacknowledged reap — still blocks, since
//     those name work or acknowledgment the session owes before stopping.
//     (A held cluster awaiting ONE OPERATOR still denies: the materialized
//     human step is pending and ready, per H11's decided semantics.)
//
// projectID scopes the question to one project's runs; 0 answers over every
// project — the same contract as RunListOptions.ProjectID. Scoping exists
// because the shared per-user store made "ANY active run" mean "any run on the
// MACHINE": a Stop hook firing in one repository was denied over another
// repository's run, which the hook could not distinguish from its own work
// (observed 2026-08-11, RUN-2). A stop only interferes with the invoking
// project's runs, so that is the default question; --all-projects asks the old
// one.
func GuardStop(conn *sql.DB, projectID int, nowMS int64) (*GuardVerdict, error) {
	// A NEVER-DISPATCHED RUN IS NOT WORK IN FLIGHT (DKT-71).
	//
	// This guard's own header states the predicate is "is the MACHINE done
	// working?", and a run that has been activated but never dispatched has
	// never handed a step to anything. Its steps are pending because nobody has
	// started them — which is the state `bootstrap` deliberately ENDS IN: it
	// activates a run and stops, leaving the operator to decide whether to run
	// it. All six bootstraps measured on one day were denied a turn-end over
	// their own contractual terminal state, and twice the denial pushed the
	// operator into starting work nobody had asked for, purely on the hook's
	// momentum.
	//
	// The probe is the `dispatches` TABLE, not the `dispatch-opened` EVENT the
	// ask names. Both record the same fact — it is the same existence check D2
	// uses for "a run no relay drove has nobody owing usage" — but events are
	// prunable (`events prune`) and dispatch rows are not deleted by anything,
	// so a long-lived run whose early events had been trimmed would read as
	// never-dispatched and stop blocking exactly when it mattered most.
	//
	// THE SECOND ARM IS WHAT KEEPS THE EXEMPTION HONEST. A run can acquire
	// history without a dispatch — a step claimed and recorded directly, a
	// cluster held, a vote opened — and any of those mean something started
	// this run even though no manifest did. So the exemption requires BOTH: no
	// dispatch, and no step that has ever left `pending`. What it describes is
	// a run NOTHING HAS EVER HAPPENED TO, which is bootstrap's terminal state
	// exactly and is not a state a stop can interrupt.
	query := `SELECT id FROM runs WHERE status NOT IN ('done', 'abandoned')
		AND (EXISTS (SELECT 1 FROM dispatches WHERE dispatches.run_id = runs.id)
		     OR EXISTS (SELECT 1 FROM steps
		                 WHERE steps.run_id = runs.id AND steps.status != ?))`
	args := []any{db.StepPending}
	if projectID != 0 {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY id`

	rows, err := conn.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading active runs: %w", err)
	}
	var runIDs []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading an active run: %w", err)
		}
		runIDs = append(runIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("reading active runs: %w", err)
	}
	rows.Close()

	var blocking []string
	for _, runID := range runIDs {
		names, err := stopBlockers(conn, runID, nowMS)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			// Name at most three: a reason listing forty instances is one
			// nobody reads, and three is enough to identify which run is
			// still working.
			if len(blocking) < 3 {
				blocking = append(blocking, name)
			}
		}
	}

	if len(blocking) == 0 {
		return &GuardVerdict{Allowed: true}, nil
	}

	reason := fmt.Sprintf("work is still pending: %v", blocking)
	return &GuardVerdict{Allowed: false, Reason: reason}, nil
}

// stopBlockers lists one run's stop-blocking steps, per GuardStop's rules.
//
// It loads the run's scheduler READ-ONLY — the transaction is rolled back
// unconditionally, per LoadStepView's pattern — because two of the rules are
// readiness questions the raw status column cannot answer.
func stopBlockers(conn *sql.DB, runID int, nowMS int64) ([]string, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning guard read: %w", err)
	}
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	openVotes, err := openVoteSteps(tx, sched)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, step := range sched.steps {
		blocked := false
		switch step.Status {
		case db.StepClaimed, db.StepRunning:
			blocked = true
		case db.StepGated:
			blocked = !gatedOnOpenVotes(sched, step, openVotes)
		case db.StepPending:
			if openVotes[step.ID] {
				break
			}
			ready, cond := sched.Ready(step)
			blocked = ready ||
				(cond != CondPredecessors && cond != CondIssueDeps)
		}
		if blocked {
			out = append(out, step.Instance+" ("+step.Status+")")
		}
	}
	return out, nil
}

// openVoteSteps reports which of a run's vote steps have an OPEN proposal —
// the ones whose voters are still deliberating. A vote step with no proposal
// yet is not among them: opening the proposal is `next`'s work, which a stop
// would leave undone.
func openVoteSteps(tx *sql.Tx, sched *Scheduler) (map[int]bool, error) {
	if len(sched.voteProposals) == 0 {
		return nil, nil
	}
	out := make(map[int]bool)
	for _, step := range sched.steps {
		if step.Kind != workflow.TypeVote {
			continue
		}
		id := sched.voteProposals[voteStepKey{
			Issue: model.FormatID(step.IssueID), Instance: step.Instance,
		}]
		if id == 0 {
			continue
		}
		var status string
		err := tx.QueryRow(`SELECT status FROM proposals WHERE id = ?`, id).
			Scan(&status)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading proposal %d: %w", id, err)
		}
		if status == string(model.ProposalStatusOpen) {
			out[step.ID] = true
		}
	}
	return out, nil
}

// gatedOnOpenVotes reports whether a gated routing step is waiting on nothing
// but open vote proposals: at least one of its materialized held steps is
// unresolved, and every unresolved one is a vote with an open proposal.
//
// A gated step with NO unresolved holds is dispatchable — its saga resumes
// under the next engine invocation — so it blocks. One with an unresolved
// HUMAN hold blocks through H11's decided semantics, and the mixed case
// blocks with it.
func gatedOnOpenVotes(sched *Scheduler, step *db.Step, openVotes map[int]bool) bool {
	heldName := workflow.HeldStepName(step.StepName)
	unresolved := 0
	for _, s := range sched.steps {
		if !s.Materialized || s.IssueID != step.IssueID {
			continue
		}
		if s.StepName != heldName || s.Ordinal != step.Ordinal {
			continue
		}
		if db.StepTerminal(s.Status) {
			continue
		}
		unresolved++
		if !openVotes[s.ID] {
			return false
		}
	}
	return unresolved > 0
}

// GuardGate answers `docket guard gate --step NAME`: does a PASSED gate step of
// that name exist for the active run?
//
// "Passed" is `done` with a `pass` routing — the state `step approve` produces
// on a human gate, and the state a tallied approval produces on a vote gate. A
// step that reached `done` by any other route did not receive a decision, and a
// guard that accepted it would let an override stand in for a decision nobody
// made.
//
// BOTH GATE KINDS ANSWER. The filter was `type="human"` alone, which meant
// converting a gate from `human` to `vote` — the same question, asked of
// several voters instead of one — silently stopped matching, and every hook
// checking that gate started denying with "no such step" while the gate itself
// sat approved. That is the strictness comment's own concern inverted: the
// decision WAS made, by the machinery §8 exists to run, and the guard was the
// only thing that could not see it. A tallied pass IS a decision, so it counts;
// nothing else about the test loosened, and a vote still open reads `pending`
// here and denies exactly as an unapproved human gate does.
//
// projectID scopes the search to one project's runs; 0 answers over every
// project (see GuardStop). An approval is a decision about ONE project's gate,
// so a same-named gate in another project must not answer for it.
func GuardGate(conn *sql.DB, stepName string, projectID int) (*GuardVerdict, error) {
	if stepName == "" {
		return nil, validationErr("--step is required: name the gate to check")
	}

	query := `SELECT s.status, s.routing
	   FROM steps s JOIN runs r ON r.id = s.run_id
	  WHERE r.status NOT IN ('done', 'abandoned')
	    AND s.step_name = ? AND s.kind IN (?, ?)`
	args := []any{stepName, workflow.TypeHuman, workflow.TypeVote}
	if projectID != 0 {
		query += ` AND r.project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY s.id`

	rows, err := conn.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading gate %s: %w", stepName, err)
	}
	defer rows.Close()

	var (
		found    bool
		approved bool
		state    string
	)
	for rows.Next() {
		var (
			status  string
			routing sql.NullString
		)
		if err := rows.Scan(&status, &routing); err != nil {
			return nil, fmt.Errorf("reading gate %s: %w", stepName, err)
		}
		found = true
		state = status
		if status == db.StepDone && routingIs(routing.String, RoutingPass) {
			approved = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading gate %s: %w", stepName, err)
	}

	switch {
	case approved:
		return &GuardVerdict{Allowed: true}, nil
	case found:
		return &GuardVerdict{Allowed: false, Reason: fmt.Sprintf(
			"gate %q is %s, not approved", stepName, state)}, nil
	default:
		return &GuardVerdict{Allowed: false, Reason: fmt.Sprintf(
			"no `type=\"human\"` or `type=\"vote\"` step named %q in any active run",
			stepName)}, nil
	}
}

// routingIs reports whether a stored routing record names the given routing.
//
// The stored value may carry an appended note or park reason
// (`routingRecord`), so this compares the routing PREFIX rather than the whole
// string — otherwise an approval with a note would read as unapproved.
func routingIs(stored, want string) bool {
	if stored == want {
		return true
	}
	return len(stored) > len(want) &&
		stored[:len(want)] == want && stored[len(want)] == ':'
}
