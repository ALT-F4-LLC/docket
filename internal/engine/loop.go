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

// Loop semantics — TDD §7.2-§7.4, engine-spec §11.3 clause by clause.
//
// §11.3 is normative and short enough to hold in one's head, which is why this
// file implements it clause by clause rather than as one procedure:
//
//	(1) on `fix-loop` routing, the ISSUE's loop counter increments; exceeding
//	    `max_fix_loops` routes `waiting-human` instead.
//	(2) not-yet-claimed instances downstream of `after_loop` become `superseded`;
//	    claimed/running instances finish, but routing from a superseded lineage
//	    is INERT.
//	(3) `loop = true` steps instantiate at ordinal k, `inputs` bound within
//	    ordinal k falling back to the highest earlier ordinal PER INPUT.
//	(4) when the loop body's gates pass, `after_loop` AND ITS DOWNSTREAM CHAIN
//	    re-instantiate at ordinal k; gates re-run; thresholds re-apply.
//
// And the two rules that are easy to lose because they are stated as asides:
// issue completion is evaluated over HIGHEST-ORDINAL instances only, and prior
// instances remain immutable and addressable — `superseded` is a status, not a
// deletion.
//
// THERE IS NO OTHER LOOP CONSTRUCT (§11.3, final sentence). A `threshold`
// routing to a step name is an interposed gate (§11.2) and is handled entirely
// by ordinary expansion — it never reaches this file and never touches
// `loop_count`. That is the distinction a reader should carry away: one of these
// re-instantiates a chain at a new ordinal, the other makes an already-expanded
// step ready. Only the first is a loop.

// LoopOutcome is what a `fix-loop` routing resolved to.
//
// It exists because the routing transaction must know whether the loop was
// ENTERED or BOUNDED before it writes the step's status: an entry routes
// `fix-loop` and the step is `done`, a bound routes `waiting-human` and the step
// parks. Returning a struct rather than mutating through a pointer keeps that
// decision inspectable in a test.
type LoopOutcome struct {
	// Entered reports whether the loop was actually entered. False means the
	// bound was hit and the routing became `waiting-human` (clause 1).
	Entered bool
	// Ordinal is the new loop ordinal when Entered, i.e. the issue's new
	// loop_count. Instances created by this entry carry it.
	Ordinal int
	// Routing is the routing that ACTUALLY applies after the bound is
	// considered: `fix-loop` on entry, `waiting-human` when bounded.
	Routing string
	// Reason explains a bound, for the operator resolving the parked step.
	Reason string
	// Superseded lists the instances the sweep terminated (clause 2).
	Superseded []string
	// Instantiated lists the instances this entry created — the loop bodies of
	// clause 3 and the re-instantiated chain of clause 4.
	Instantiated []string
}

// applyFixLoop is THE ONE bridge from a resolved `fix-loop` routing to its
// effect (DKT-168). Every path that writes `fix-loop` into a step's routing
// record must pass through here first, because EnterLoop is what makes the
// routing MEAN anything — the counter, the supersede sweep, the ordinal-k
// instantiation, and the `max_fix_loops` bound. RUN-25 shipped the defect
// this closes: a rejected security-vote wrote `fix-loop` on its step and
// nothing ever consumed it — no fix step was created, the loop counter never
// moved (so a concurrent verify's late `pass` was not even stale), and the
// issue closed `done` carrying the reproduced blocker the vote had caught.
//
// A stale lineage's routing is recorded but INERT (§7.3 (3)), so it enters no
// loop and the routing passes through unchanged. The returned outcome is nil
// when no loop entry happened; when one did, the returned routing is the
// outcome's — entered stays `fix-loop`, bounded becomes `waiting-human` — and
// the caller re-derives the step's status from it.
func applyFixLoop(
	tx *sql.Tx, step *db.Step, def *workflow.Definition, routing string, nowMS int64,
) (string, *LoopOutcome, error) {
	if routing != workflow.OnFailFixLoop {
		return routing, nil, nil
	}
	stale, err := StaleLineage(tx, step)
	if err != nil {
		return "", nil, err
	}
	if stale {
		return routing, nil, nil
	}
	outcome, err := EnterLoop(tx, step, def, nowMS)
	if err != nil {
		return "", nil, err
	}
	return outcome.Routing, outcome, nil
}

// roundMovedNothing reports whether the round BELOW the one about to be entered
// left the issue's scope byte-identical to the round below that (DKT-340).
//
// The measurement is `issue.diff`, which is cumulative over the issue's scope
// and carries its own sha256 — so two rounds whose newest diff hashes agree
// left the same tree behind, and core reaches that conclusion without knowing
// what a single finding, criterion or severity means. That is the whole reason
// this is the check core is entitled to make and the three remedies DKT-340
// sketched are not: each of those needs the engine to understand which payload
// element is "the same one" across rounds, which is instance vocabulary.
//
// IT FAILS TOWARD ENTERING THE ROUND. Every uncertain case — the first round,
// a missing diff on either side, a read error — answers false, because a
// wrongly-entered round costs one round and a wrongly-refused one costs the
// operator a park they have to reason about and undo. The same direction
// `issueHasRecordedChange` fails in, for the same reason: the guard must not
// act on absence of evidence.
func roundMovedNothing(tx *sql.Tx, step *db.Step, count int) (bool, error) {
	// count is the ordinal about to be entered, so `count-1` just ran and
	// `count-2` is what it built on. At count = 1 there is no prior round to
	// compare against: ordinal 0 is the original work, not a repeat of it.
	if count < 2 {
		return false, nil
	}
	before, body, err := newestIssueDiffUpTo(tx, step, count-2)
	if err != nil || before == "" {
		// No diff recorded before the round ran: either this issue produces
		// none at all (no tree-holding executor step, or no diff function),
		// or the run is too early to compare. Either way there is no evidence.
		return false, err
	}
	// THE EVIDENCE MUST BE A REAL MEASUREMENT. `diffRecordsNoChange` is true
	// for an empty diff AND for one whose only content is the `#` warning the
	// saga writes when the run's pinned base commit could not be resolved —
	// the degenerate case that produced 36 of RUN-5's 71 empty-hashed diffs.
	// Two degenerate measurements agreeing says nothing about whether the tree
	// moved; it says the tree was never measured. Parking a loop on that would
	// turn a broken diff setup into a stalled run, which is a far worse
	// failure than the wasted round this guard exists to prevent.
	if diffRecordsNoChange(body) {
		return false, nil
	}
	now, _, err := newestIssueDiffUpTo(tx, step, count-1)
	if err != nil || now == "" {
		return false, err
	}
	return now == before, nil
}

// newestIssueDiffUpTo is the fingerprint AND body of the issue's newest
// `issue.diff` recorded at or below one ordinal — "the tree as of the end of that round" —
// or "" when the issue has none that far back.
//
// UP TO, not AT, and that is the whole robustness of the check. The engine
// suppresses an `issue.diff` write whose bytes match the one already recorded
// (DKT-258) and one that is empty when a real diff already exists (DKT-259), so
// a round that changed nothing records NO ARTIFACT rather than an identical
// one. Asking "what is the newest diff as of this ordinal" gets the same answer
// under either behavior: the suppressed round's newest is still the previous
// round's row, and an unsuppressed identical row would carry the same sha
// anyway. A version that asked for the artifact AT ordinal k-1 would read the
// suppression as "no evidence" and never fire — which is precisely the case
// this guard exists for.
//
// Newest-first by artifact id, matching every other "the latest artifact wins"
// read in the engine: a re-claimed step completing a second time records again,
// and the last record is the one that describes the tree.
func newestIssueDiffUpTo(tx *sql.Tx, step *db.Step, ordinal int) (hash, body string, err error) {
	if ordinal < 0 {
		return "", "", nil
	}
	err = tx.QueryRow(
		`SELECT a.sha256, a.body FROM artifacts a JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND s.issue_id = ? AND a.kind = ? AND s.ordinal <= ?
		  ORDER BY a.id DESC LIMIT 1`,
		step.RunID, step.IssueID, ArtifactKindIssueDiff, ordinal).Scan(&hash, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("reading %s's diff up to ordinal %d: %w",
			model.FormatID(step.IssueID), ordinal, err)
	}
	return hash, body, nil
}

// EnterLoop performs a `fix-loop` routing, inside the caller's routing
// transaction.
//
// It is called ONLY when a routing resolved to `fix-loop` on a step whose
// lineage is live (see StaleLineage). The whole of §11.3 happens here, in one
// transaction with the step update that triggered it, because a partial loop
// entry — a counter raised with no bodies instantiated, or bodies instantiated
// twice — is not a state any later pass can repair without guessing.
// The routing STEP's own spec is deliberately not a parameter: nothing about a
// loop entry depends on which step routed. The counter is the issue's, the
// bound is the workflow's, the sweep set is `after_loop`'s downstream, and the
// instantiation is the definition's — so a `fix-loop` from `verify` and one
// from `reconcile` must produce identical effects. Taking the spec would invite
// a future reader to make one of those depend on it.
func EnterLoop(
	tx *sql.Tx, step *db.Step, def *workflow.Definition, nowMS int64,
) (*LoopOutcome, error) {
	return enterLoop(tx, step, def, false, nowMS)
}

// EnterLoopAuthorized is EnterLoop under an EXPLICIT operator authorization —
// `step resolve --as fix-round`, which has just recorded a grant.
//
// The difference is the non-convergence refusal, and only that: an operator who
// has read the park and asked for the round anyway has answered the question
// the park asked. Making them unable to is how a park that "names the way out"
// becomes a park with no way out — DKT-237's exact failure, reproduced by the
// guard that was meant to prevent a different one.
//
// The BOUND is not waived here, because it does not need to be: a grant raises
// the effective maximum, so the same resolution already gets past it through
// LoopGrantsTx. Only the convergence check has no such counter to move.
func EnterLoopAuthorized(
	tx *sql.Tx, step *db.Step, def *workflow.Definition, nowMS int64,
) (*LoopOutcome, error) {
	return enterLoop(tx, step, def, true, nowMS)
}

func enterLoop(
	tx *sql.Tx, step *db.Step, def *workflow.Definition, authorized bool, nowMS int64,
) (*LoopOutcome, error) {
	// ---- Clause (1): the issue's counter, and the bound. -------------------
	//
	// The increment happens FIRST and its result is what decides the bound, so
	// two concurrent routings cannot both read the same pre-increment value and
	// both conclude they are loop k+1. `max_fix_loops` bounding nothing is the
	// failure mode "loops are bounded by construction" exists to exclude.
	count, err := db.IncrementLoopCountTx(tx, step.RunID, step.IssueID)
	if err != nil {
		return nil, err
	}

	// The effective bound is the workflow's declared one PLUS whatever an
	// operator has explicitly granted for this issue (DKT-237). A grant is one
	// decision about one issue, recorded on its own row, so the declared
	// policy stays what the author wrote for every other issue the workflow
	// matches.
	grants, err := db.LoopGrantsTx(tx, step.RunID, step.IssueID)
	if err != nil {
		return nil, err
	}
	max := maxFixLoops(def)
	if max > 0 {
		max += grants
	}
	if max > 0 && count > max {
		// BOUNDED. §11.3 (1): "exceeding `max_fix_loops` routes `waiting-human`
		// instead".
		//
		// Nothing is superseded and nothing is instantiated — this is not a loop
		// entry, it is a park, and the downstream work the loop would have
		// replaced is still the work that has to happen once a human decides.
		//
		// AND THE COUNTER IS PUT BACK, which is the half a refusal that merely
		// returns gets wrong. `loop_count` is not a tally of attempts; it names
		// THE ISSUE'S CURRENT ORDINAL, and the whole of §11.3 (2)'s inert half
		// reads it that way — StaleLineage calls every step below it a
		// superseded lineage "because a later loop entry replaced the work it is
		// part of". A refusal replaced nothing and created no ordinal, so a
		// counter left one above the highest instantiated ordinal declares that
		// ordinal — the LAST one the run will ever have — stale in its entirety.
		//
		// DKT-78 is what that costs. The final ordinal's check still runs once a
		// human unparks the driver, and every effect of its verdict is dropped
		// on the floor: its threshold cannot route, its issue cannot reconcile,
		// and an unmet acceptance criterion reaches the commit with the step
		// built to catch it reduced to a bystander. Every terminal step, no
		// completion, and nothing in the ledger saying why.
		//
		// The refusal stays observable through the routing this returns, which
		// names the bound and is written to the step by the caller — a durable
		// record on the row an operator is being asked to resolve, rather than a
		// phantom ordinal in a counter that four other rules read.
		if err := restoreLoopCount(tx, step, count); err != nil {
			return nil, err
		}
		// The reason NAMES THE WAY OUT (DKT-237). Before `fix-round` existed,
		// this park was terminal in practice: no verb could mint another round,
		// and the fix got built outside the engine instead — an agent, a
		// 1,128-insertion commit cherry-picked with no judge review, and ~100k
		// output tokens in no ledger. A refusal that names no next move is what
		// makes that the reasonable thing to do.
		reason := fmt.Sprintf(
			"loop %d would exceed max_fix_loops = %d on %s; "+
				"`docket step resolve --as fix-round` authorizes one more round",
			count, max, model.FormatID(step.IssueID))
		return &LoopOutcome{
			Entered: false, Ordinal: count - 1,
			Routing: workflow.OnFailWaitingHuman, Reason: reason,
		}, nil
	}

	// NON-CONVERGENCE (DKT-340). A round that changed nothing in the issue's
	// scope cannot have changed what any check measures, so the next round is
	// handed the identical tree and reaches the identical verdict. The loop is
	// not converging; it is repeating.
	//
	// This is checked HERE, beside the bound, because it is the same kind of
	// refusal and takes the same shape: nothing superseded, nothing
	// instantiated, the counter put back, and a park whose reason names the
	// way out. The two differ only in what they measure — a budget the author
	// declared, versus evidence the engine can see is unchanged.
	//
	// RUN-34 is the cost. `verify-ac`'s threshold re-triggered `fix@1` and
	// `fix@2` on a criterion three judge panels and two votes had already read
	// as correctly unmet — unmeetable by design — and each round bought a full
	// review fanout and a verify pass for no possible progress, until an ad-hoc
	// budget raise was needed to survive the churn. Core cannot know that an
	// acceptance criterion is unmeetable; it can see that a round moved no
	// bytes the run is scoped to, which is enough to stop and ask.
	stalled, err := roundMovedNothing(tx, step, count)
	if err != nil {
		return nil, err
	}
	if stalled && !authorized {
		if err := restoreLoopCount(tx, step, count); err != nil {
			return nil, err
		}
		reason := fmt.Sprintf(
			"loop %d would repeat: round %d changed nothing in %s's scope, so "+
				"another round reads the same tree and reaches the same verdict; "+
				"`docket step resolve --as fix-round` authorizes one anyway",
			count, count-1, model.FormatID(step.IssueID))
		return &LoopOutcome{
			Entered: false, Ordinal: count - 1,
			Routing: workflow.OnFailWaitingHuman, Reason: reason,
		}, nil
	}

	out := &LoopOutcome{
		Entered: true, Ordinal: count, Routing: workflow.OnFailFixLoop,
	}

	// ---- Clause (2): the supersede sweep. ----------------------------------
	superseded, err := supersedeSweep(tx, step, def, count, nowMS)
	if err != nil {
		return nil, err
	}
	out.Superseded = superseded

	// ---- Clauses (3) and (4): instantiate at the new ordinal. --------------
	//
	// The loop BODIES (clause 3) and the re-instantiated `after_loop` chain
	// (clause 4) are one expansion at ordinal `count`, not two: they are the
	// same set of rows written by the same rules, and separating them would
	// invite two writers disagreeing about which steps belong to ordinal k.
	instantiated, err := instantiateOrdinal(tx, step, def, count, nowMS)
	if err != nil {
		return nil, err
	}
	out.Instantiated = instantiated

	// ---- Clauses (2) and (4) as ONE invariant. -----------------------------
	//
	// Nothing is superseded without a successor at the ordinal that superseded
	// it. The two clauses are written above as two passes over two different
	// inputs — the sweep filters step ROWS, the instantiation walks the
	// DEFINITION — and they agree today only because `downstream` is handed to
	// both. That agreement is a property of the current code, not of §11.3, and
	// the failure it guards against is silent: a step swept with nothing to
	// succeed it is work the run will never do, and because `superseded` is
	// terminal and completion reads highest-ordinal instances only, the issue
	// COMPLETES without it. DKT-78's `verify-ac` is the step that class of gap
	// costs the most.
	repaired, err := ensureSupersededHaveSuccessors(tx, step, def, superseded, count, nowMS)
	if err != nil {
		return nil, err
	}
	out.Instantiated = append(out.Instantiated, repaired...)

	if err := recordEvent(tx, eventRecord{
		Kind: EventLoopEntered, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
		Data: fmt.Sprintf(`{"ordinal":%d}`, count), AtMS: nowMS,
	}); err != nil {
		return nil, err
	}

	return out, nil
}

// maxFixLoops reads the definition's `max_fix_loops` (§11.1).
//
// It is a WORKFLOW-level bound read off whichever step declares it, because
// §11.3 makes the counter the ISSUE's: one issue, one count, one bound. The
// fixture declares it on `reconcile`, but a workflow whose `verify` drives the
// loop must be bounded by the same number — a per-step reading would let the
// bound depend on which step happened to route, which is not a bound at all.
//
// Zero means unbounded, which is what a definition declaring nothing means.
func maxFixLoops(def *workflow.Definition) int {
	for _, step := range def.Steps {
		if step.MaxFixLoops != nil && *step.MaxFixLoops > 0 {
			return *step.MaxFixLoops
		}
	}
	return 0
}

// supersedeSweep is §7.3, exactly.
//
// On loop entry at ordinal k-1 -> k: every instance of the `after_loop`
// downstream set, at ordinal < k, that is NOT YET CLAIMED becomes `superseded`
// and is event-logged. Claimed, running, and gated instances are LEFT ALONE to
// finish; their eventual routing is made inert by StaleLineage.
//
// The status table (§7.7) is the specification and is worth stating as one:
//
//	pending        -> superseded   (unclaimed)
//	ready          -> superseded   (COMPUTED, never stored — a step that is
//	                                computed-ready is a `pending` row, so it
//	                                sweeps like any unclaimed instance)
//	claimed        -> left alone   (finishes; routing inert)
//	running        -> left alone   (finishes; routing inert)
//	gated          -> left alone   (the saga owns it; routing inert)
//	waiting-human  -> left alone   (an operator's open question is not the
//	                                loop's to close)
//	done           -> left alone   (terminal, immutable, addressable)
//	skipped        -> left alone   (terminal)
//	failed-routed  -> left alone   (terminal)
//	superseded     -> left alone   (terminal; already swept by an earlier entry)
//
// `waiting-human` is the row most easily got wrong. It is not terminal (§6.8's
// rollup depends on that) and it is not claimed, so a sweep written as "not
// terminal and not claimed" would supersede it — silently discarding a question
// an operator was asked. The sweep is therefore written as an explicit
// unclaimed-and-pending test rather than as a negation.
func supersedeSweep(
	tx *sql.Tx, step *db.Step, def *workflow.Definition, ordinal int, nowMS int64,
) ([]string, error) {
	downstream := afterLoopDownstream(def)
	if len(downstream) == 0 {
		return nil, nil
	}

	// Every instance of the downstream set for THIS issue, below the new
	// ordinal. Read before writing so the event log can name each one.
	rows, err := tx.Query(
		`SELECT id, instance, status FROM steps
		  WHERE run_id = ? AND issue_id = ? AND ordinal < ?
		  ORDER BY id`,
		step.RunID, step.IssueID, ordinal)
	if err != nil {
		return nil, fmt.Errorf("reading the supersede set for %s: %w", step.Instance, err)
	}

	type candidate struct {
		id       int
		instance string
	}
	var sweep []candidate
	for rows.Next() {
		var (
			id       int
			instance string
			status   string
		)
		if err := rows.Scan(&id, &instance, &status); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading a supersede candidate: %w", err)
		}
		// Only the `after_loop` downstream set (§7.3 (1)).
		name, _, _, err := workflow.ParseInstance(instance)
		if err != nil {
			rows.Close()
			return nil, err
		}
		// H17: a MATERIALIZED `<step>-held@k` is in its routing step's lineage,
		// so it sweeps exactly as any other non-terminal instance downstream of
		// `after_loop` does. Its name is not in the definition — expansion never
		// wrote it — so it is mapped back to the routing step it belongs to
		// rather than silently falling out of the set.
		//
		// An open held question from a superseded ordinal must not survive to
		// block ordinal k+1: the question was about THAT ordinal's computation,
		// and the loop has moved past it.
		if !downstream[name] && !heldStepBelongsToDownstream(name, downstream) {
			continue
		}
		// Only NOT-YET-CLAIMED instances (§7.3 (2)). `pending` is the whole
		// test: `ready` is computed and never stored, so a computed-ready step
		// is a `pending` row and sweeps here correctly.
		if status != db.StepPending {
			continue
		}
		sweep = append(sweep, candidate{id: id, instance: instance})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("reading the supersede set: %w", err)
	}
	rows.Close()

	out := make([]string, 0, len(sweep))
	for _, c := range sweep {
		if err := db.SetStepStatusTx(tx, c.id, db.StepSuperseded, nowMS, nowMS); err != nil {
			return nil, err
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventStepSuperseded, RunID: step.RunID,
			Instance: c.instance, IssueID: step.IssueID, AtMS: nowMS,
		}); err != nil {
			return nil, err
		}
		// A SWEPT STEP'S BALLOT GOES WITH IT (DKT-262). The question a vote
		// step asked was about the ordinal being superseded, and the loop has
		// moved past it — DKT-V3 stood open forever as "superseded by a later
		// proposal, never closed". Non-vote steps have no ballot and this is a
		// no-op for them, which is most of the sweep.
		if err := closeStepProposalTx(tx, step.RunID, step.IssueID, c.instance,
			fmt.Sprintf(
				"%s was superseded when the loop entered a later ordinal; the "+
					"ballot's question was about the ordinal that no longer "+
					"exists, and it was never decided", c.instance),
		); err != nil {
			return nil, err
		}
		out = append(out, c.instance)
	}
	return out, nil
}

// afterLoopDownstream computes §7.3 (1)'s set: the `after_loop` step and every
// step transitively `after` it.
//
// The traversal is over the DEFINITION, not over expanded rows, because the set
// is a property of the workflow's shape and must be the same on every entry —
// including one where a downstream step has no instance yet.
func afterLoopDownstream(def *workflow.Definition) map[string]bool {
	var roots []string
	for _, step := range def.Steps {
		if step.AfterLoop != "" {
			roots = append(roots, step.AfterLoop)
		}
	}
	if len(roots) == 0 {
		return nil
	}

	set := make(map[string]bool, len(def.Steps))
	for _, root := range roots {
		set[root] = true
	}

	// Transitive closure. The definition is a DAG (L1 lints for cycles at
	// register time), so iterating to a fixed point terminates; the bound is the
	// step count, since each pass adds at least one member or stops.
	for range def.Steps {
		grew := false
		for _, step := range def.Steps {
			if set[step.Name] {
				continue
			}
			for _, pred := range step.After {
				if set[pred] {
					set[step.Name] = true
					grew = true
					break
				}
			}
		}
		if !grew {
			break
		}
	}
	return set
}

// instantiateOrdinal writes the rows clauses (3) and (4) call for: the loop
// bodies and the re-instantiated `after_loop` chain, all at ordinal k.
//
// Steps NOT in either set are not re-instantiated. `implement` is the example
// worth holding onto: it is upstream of `after_loop`, it never re-runs, and its
// artifact stays bound at ordinal 0 — which is exactly why §7.4's fallback is
// per-input rather than per-step.
func instantiateOrdinal(
	tx *sql.Tx, step *db.Step, def *workflow.Definition, ordinal int, nowMS int64,
) ([]string, error) {
	downstream := afterLoopDownstream(def)

	// The issue's subject, for `when` predicates — read from the SNAPSHOT, so a
	// re-instantiation at ordinal k evaluates the same predicate against the
	// same facts activation froze. Reading the live issue here would make a
	// mid-run label edit change the topology of ordinal 1, breaking §9 item 5's
	// determinism for exactly the runs that loop.
	subject, err := snapshotSubject(tx, step.RunID, step.IssueID)
	if err != nil {
		return nil, err
	}

	// Clause (3) instantiates `loop = true` steps; clause (4) re-instantiates
	// the `after_loop` chain. workflow.ExpandOrdinal applies both rules and
	// nothing else, so the ordering and the fanout indices are the same pure
	// function ordinary expansion uses (§5.3.1) — a second implementation here
	// is how ordinal 1's topology would drift from ordinal 0's.
	rows := workflow.ExpandOrdinal(def, subject, ordinal, downstream)

	return writeInstances(tx, step, rows, nowMS)
}

// writeInstances persists expanded rows and returns their instance identities.
//
// It is separate from the expansion that produced them because the repair pass
// below writes rows the same way from a different expansion, and a step row
// created by two writers with two ideas of which columns matter is how an
// instance ends up missing the `skipped` event or its metadata.
func writeInstances(
	tx *sql.Tx, step *db.Step, rows []workflow.StepInstance, nowMS int64,
) ([]string, error) {
	out := make([]string, 0, len(rows))
	for _, inst := range rows {
		metadata, err := marshalMetadata(inst.Metadata)
		if err != nil {
			return nil, err
		}
		err = db.InsertStepTx(tx, db.StepRow{
			RunID: step.RunID, IssueID: step.IssueID, WorkflowID: step.WorkflowID,
			StepName: inst.Name, Ordinal: inst.Ordinal, SiblingIndex: inst.SiblingIndex,
			Instance: inst.Instance, Kind: inst.Kind, Executor: inst.Executor,
			Class: inst.Class, Status: inst.Status, MaxAttempts: inst.MaxAttempts,
			ExpectedCost: inst.ExpectedCost, Metadata: metadata,
		}, nowMS)
		if err != nil {
			return nil, err
		}
		if inst.Status == workflow.StatusSkipped {
			if err := recordEvent(tx, eventRecord{
				Kind: EventStepSkipped, RunID: step.RunID,
				Instance: inst.Instance, IssueID: step.IssueID, AtMS: nowMS,
			}); err != nil {
				return nil, err
			}
		}
		out = append(out, inst.Instance)
	}
	return out, nil
}

// ensureSupersededHaveSuccessors is the invariant EnterLoop states above: every
// instance this entry superseded has an instance of the SAME STEP at the
// ordinal that superseded it.
//
// It repairs rather than refuses. A missing successor is not an operator's
// mistake and not a corrupt database — it is two agreeing computations having
// stopped agreeing — and the honest response is to write the row the entry
// should have written, in the transaction that superseded its predecessor. A
// returned error would roll back the whole routing and park a run on a defect
// the run can do nothing about, which trades starvation for a wedge.
//
// A MATERIALIZED held step maps back to its routing step before the lookup, the
// same mapping the sweep used to take it (H17). Its name is not in the
// definition, so expansion never writes one at the new ordinal and there is
// nothing to repair: its successor IS its routing step's new instance, which is
// what the mapped lookup asks about.
func ensureSupersededHaveSuccessors(
	tx *sql.Tx, step *db.Step, def *workflow.Definition,
	superseded []string, ordinal int, nowMS int64,
) ([]string, error) {
	if len(superseded) == 0 {
		return nil, nil
	}

	// Read the ordinal back from the database rather than trusting the list
	// instantiateOrdinal just returned. The guard exists because two
	// computations may disagree; asking one of them what it wrote would make
	// the guard agree with whichever one was wrong.
	present, err := stepNamesAtOrdinal(tx, step.RunID, step.IssueID, ordinal)
	if err != nil {
		return nil, err
	}

	subject, err := snapshotSubject(tx, step.RunID, step.IssueID)
	if err != nil {
		return nil, err
	}

	var out []string
	for _, instance := range superseded {
		name, _, _, err := workflow.ParseInstance(instance)
		if err != nil {
			return nil, err
		}
		if routing, ok := workflow.RoutingStepNameOf(name); ok {
			name = routing
		}
		if present[name] {
			continue
		}

		rows := workflow.ExpandStepAt(def, subject, ordinal, name)
		if len(rows) == 0 {
			// Neither a definition step nor a materialized name whose routing
			// step is one. Nothing coherent to create, and inventing a row for a
			// name the pinned workflow does not declare would put an instance in
			// the run that no spec can ever resolve.
			continue
		}
		written, err := writeInstances(tx, step, rows, nowMS)
		if err != nil {
			return nil, err
		}
		// Marked present before the next iteration, so a fanned-out step whose
		// siblings were all superseded is repaired once rather than once per
		// sibling.
		present[name] = true
		out = append(out, written...)
	}
	return out, nil
}

// stepNamesAtOrdinal is the set of step NAMES that have an instance for one
// issue at one ordinal.
func stepNamesAtOrdinal(tx *sql.Tx, runID, issueID, ordinal int) (map[string]bool, error) {
	rows, err := tx.Query(
		`SELECT DISTINCT step_name FROM steps
		  WHERE run_id = ? AND issue_id = ? AND ordinal = ?`,
		runID, issueID, ordinal)
	if err != nil {
		return nil, fmt.Errorf("reading ordinal %d of %s: %w",
			ordinal, model.FormatID(issueID), err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("reading a step name at ordinal %d: %w", ordinal, err)
		}
		out[name] = true
	}
	return out, rows.Err()
}

// restoreLoopCount puts the issue's counter back after a bounded entry, to the
// value it held before this transaction raised it.
//
// Written as an absolute assignment rather than `loop_count - 1` so the row
// lands on the value the caller actually read. The two are the same under the
// single-writer transaction this runs in; the absolute form stays right if that
// ever stops being true, and says plainly which ordinal the issue is left at.
func restoreLoopCount(tx *sql.Tx, step *db.Step, raised int) error {
	_, err := tx.Exec(
		`UPDATE run_issues SET loop_count = ? WHERE run_id = ? AND issue_id = ?`,
		raised-1, step.RunID, step.IssueID)
	if err != nil {
		return fmt.Errorf("restoring the loop counter for %s: %w",
			model.FormatID(step.IssueID), err)
	}
	return nil
}

// snapshotSubject reads the `when`-predicate subject from the issue snapshot
// activation froze (§5.1.1).
func snapshotSubject(tx *sql.Tx, runID, issueID int) (workflow.Subject, error) {
	ri, err := db.GetRunIssueTx(tx, runID, issueID)
	if err != nil {
		return workflow.Subject{}, err
	}
	return subjectFromSnapshot(ri.IssueSnapshot)
}

// resolveQuorumMisses applies J5's failure routing: a fanned-out step whose
// join completed with fewer than `min_siblings` results routes per its
// `on_fail`.
//
// It writes the routing on the sibling that carries it and a `join-completed`
// event naming the shortfall, so an operator reading the ledger sees WHY the
// chain stopped rather than finding a run with every sibling terminal and no
// successor ever offered.
//
// The re-route is idempotent, keyed on the MISS REASON already being recorded.
// The sibling carrying the routing is a `done` one, so its routing column is
// already `pass` from its own completion — "is the column empty?" would be
// false on the first pass and the miss would never resolve. `next` is called
// constantly by dispatchers, so the check has to distinguish "already resolved
// as a miss" from "resolved as anything at all".
func resolveQuorumMisses(tx *sql.Tx, sched *Scheduler, nowMS int64) error {
	for _, step := range sched.QuorumMisses() {
		def := sched.defs[step.WorkflowID]
		spec := workflow.StepByName(def, step.StepName)
		if spec == nil {
			continue
		}

		routing := spec.EffectiveOnFail()
		reason := fmt.Sprintf("join completed below min_siblings = %d", *spec.MinSiblings)

		if strings.Contains(step.Routing, reason) {
			continue // Already resolved by an earlier `next`.
		}

		// DKT-168: a miss whose on_fail is `fix-loop` enters the loop. The
		// loop outcome's reason is APPENDED, never substituted — the miss
		// reason is this path's idempotency key, and a recorded routing that
		// dropped it would re-resolve on every later `next`.
		routing, loop, err := applyFixLoop(tx, step, def, routing, nowMS)
		if err != nil {
			return err
		}
		if loop != nil && loop.Reason != "" {
			reason += "; " + loop.Reason
		}

		if err := db.SetStepRoutingTx(
			tx, step.ID, routingRecord(routing, reason), statusForRouting(routing), nowMS,
		); err != nil {
			return err
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventJoinCompleted, RunID: step.RunID,
			Instance: step.Instance, IssueID: step.IssueID,
			Data: routing, AtMS: nowMS,
		}); err != nil {
			return err
		}

		// This path terminalizes a threshold-bearing step without a reconcile,
		// so the interposed-target sweep must run here too (DKT-38) — a target
		// left `pending` by a quorum-missed router would never satisfy the
		// readiness latch and never terminalize anywhere else.
		if err := skipUnroutedTargets(tx, step, spec, routing, nowMS); err != nil {
			return err
		}

		// Reflect it in the loaded snapshot, so this call's readiness pass sees
		// the routed step rather than the row it read a moment ago — the same
		// discipline the reap above it follows.
		step.Status = statusForRouting(routing)
		step.Routing = routingRecord(routing, reason)
	}
	return nil
}

// StaleLineage is the INERT half of §11.3 (2), and §7.3 (3) exactly.
//
// A step whose ordinal is below its issue's current `loop_count` belongs to a
// superseded lineage: a later loop entry replaced the work it is part of. It
// still finishes — it was claimed before the sweep and killing a running worker
// is not what "superseded" means — and its routing is still RECORDED on the
// step for the ledger. But the routing applies NO downstream effect: no
// supersede, no re-expansion, no issue status change, no loop increment.
//
// THIS IS WHERE A NAIVE IMPLEMENTATION RACES. A slow `verify@0` completing after
// `fix@1` has started must not re-route the issue on stale ordinal-0 findings —
// it would enter a second loop for a question ordinal 1 has already moved past,
// and with `max_fix_loops = 2` it would burn the budget for a loop nobody asked
// for. The ordinal comparison is the necessary half of the guard, and
// TestStaleLineageRoutingIsInert is its test.
//
// BUT THE COUNTER ALONE OVER-REACHES (DKT-540). A loop entry replaces only the
// `after_loop` downstream set and the loop bodies; a step OUTSIDE that set —
// a branch parallel to the loop, or `implement` upstream of it — keeps its
// existing instance as the issue's CURRENT one, at an ordinal the counter has
// moved past. Reading `ordinal < loop_count` as "superseded" declared exactly
// those steps stale: RUN-41's `verify@0` routed `pass` after its issue's
// review chain had looped once, the routing was recorded and then applied
// nothing — no `step-skipped` for its interposed `verify-tribunal@0`, no issue
// completion — and the gate sat `pending` until an operator resolved it by
// hand. So staleness additionally requires what "a later loop entry replaced
// the work it is part of" literally means: a later instance of the SAME step
// exists. The sweep's own invariant (ensureSupersededHaveSuccessors) guarantees
// one for every step a loop entry actually superseded, so the two readings
// agree wherever the sweep reached — and disagree only where it never did,
// which is precisely where the routing must stay live.
func StaleLineage(tx *sql.Tx, step *db.Step) (bool, error) {
	ri, err := db.GetRunIssueTx(tx, step.RunID, step.IssueID)
	if err != nil {
		return false, err
	}
	if step.Ordinal >= ri.LoopCount {
		return false, nil
	}

	// A materialized held step maps back to its routing step before the
	// lookup, the same mapping the sweep uses (H17): its name is not in the
	// definition, so expansion never writes one at a later ordinal — its
	// successor IS its routing step's later instance.
	name := step.StepName
	if routing, ok := workflow.RoutingStepNameOf(name); ok {
		name = routing
	}

	var successors int
	err = tx.QueryRow(
		`SELECT COUNT(*) FROM steps
		  WHERE run_id = ? AND issue_id = ? AND step_name = ? AND ordinal > ?`,
		step.RunID, step.IssueID, name, step.Ordinal,
	).Scan(&successors)
	if err != nil {
		return false, fmt.Errorf("reading later instances of %s: %w", step.Instance, err)
	}
	return successors > 0, nil
}

// HighestOrdinals returns, per step NAME, the highest ordinal that has an
// instance for this issue.
//
// It is the basis of §11.3's "issue completion is evaluated over highest-ordinal
// instances only". Per NAME rather than one number for the issue, because the
// two differ exactly where it matters: after a loop entry, `implement` has
// instances only at ordinal 0 while `review` has them at 0 and 1, and a single
// issue-wide maximum would rule `implement@0` out of the completion check and
// call the issue complete without it.
func HighestOrdinals(tx *sql.Tx, runID, issueID int) (map[string]int, error) {
	rows, err := tx.Query(
		`SELECT step_name, MAX(ordinal) FROM steps
		  WHERE run_id = ? AND issue_id = ? GROUP BY step_name`,
		runID, issueID)
	if err != nil {
		return nil, fmt.Errorf("reading the highest ordinals for %s: %w",
			model.FormatID(issueID), err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var (
			name    string
			ordinal int
		)
		if err := rows.Scan(&name, &ordinal); err != nil {
			return nil, fmt.Errorf("reading a highest ordinal: %w", err)
		}
		out[name] = ordinal
	}
	return out, rows.Err()
}
