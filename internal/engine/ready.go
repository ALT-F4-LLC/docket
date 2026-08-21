package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The readiness predicate — TDD §6.3, engine-core §5 and §1.3 as a conjunction.
//
// A step is ready iff ALL of R1-R7 hold. Each is a named method below rather
// than a clause in one long boolean, so a test can falsify exactly one and the
// refusal (§6.9 R8) can NAME the condition that failed — a dispatcher told only
// "not ready" cannot diagnose itself.

// ReadyCondition identifies one clause of the predicate, so a refusal names it.
type ReadyCondition string

const (
	// CondRunActive is R1: the run is `active`.
	CondRunActive ReadyCondition = "run is not active"
	// CondIssueDeps is R2: the issue's depends_on predecessors are satisfied.
	CondIssueDeps ReadyCondition = "the issue's dependencies are not satisfied"
	// CondPredecessors is R3: intra-workflow `after` predecessors are done,
	// and a fanned-out predecessor is joined.
	CondPredecessors ReadyCondition = "an `after` predecessor is not done"
	// CondUnrouted is R3's interposition clause (DKT-38): a step named as a
	// `threshold` step-name routing target is ready only once a routing
	// predecessor's RECORDED routing names it. Distinct from CondPredecessors
	// so a dispatcher staring at a pending gate whose predecessors are all
	// terminal can tell "the threshold routed elsewhere (or has not routed
	// yet)" from a join still open.
	CondUnrouted ReadyCondition = "no threshold has routed to this interposed step"
	// CondGateOpen is R3's SECOND interposition clause (DKT-168), the half
	// DKT-38 left out: a predecessor's threshold names an interposed gate,
	// and that gate has not resolved. §11.2: "on the gate's own pass execution
	// resumes at the routing step's ordinary downstream" — so until the gate
	// is terminal (routed to and tallied, or skipped because the routing chose
	// against it), the routing step's ordinary downstream waits. Without this,
	// a verify sharing a stage with a still-open security-vote was claimable
	// the moment their common predecessor recorded, and reached its own `pass`
	// while the vote was rejecting (RUN-25/VPL-129).
	CondGateOpen ReadyCondition = "an interposed gate on a predecessor has not resolved"
	// CondScope is R4: no scope conflict with a claimed/running step.
	CondScope ReadyCondition = "its scope conflicts with a claimed or running step"
	// CondHeadroom is R5: per-class concurrency headroom exists.
	CondHeadroom ReadyCondition = "no concurrency headroom in its class"
	// CondStatus is R6: the step is `pending` and `when` did not skip it.
	CondStatus ReadyCondition = "the step is not pending"
	// CondBudget is R7: budget headroom exists. S6 owns the check; at S3 the
	// seam returns true (§6.3), so this constant exists but never fires yet.
	CondBudget ReadyCondition = "no budget headroom"
)

// Scheduler answers readiness over one consistent snapshot of a run.
//
// It is a value rather than a set of free functions because R3, R4, and R5 are
// questions about the SAME set of rows at the SAME instant: a predicate that
// re-read the step table between clauses could see a step both claimed (for
// scope) and pending (for headroom), and schedule two writers into one tree.
// Loading once and answering many times is what makes the snapshot consistent.
type Scheduler struct {
	run *model.Run
	// steps are every step of THIS run only — LoadScheduler fills it from
	// db.ListRunStepsTx(runID) below, scoped to one run. blockingLoopBodyAbsent
	// (stage.go) still guards on RunID explicitly even though that guard is
	// unreachable today, protecting the invariant if this field's population
	// ever widens.
	steps []*db.Step
	// foreign holds the claimed/running steps of every OTHER active run: a
	// step holding a scope there excludes just as surely as one in this run.
	// Folded into scopeHolders locally, where a cross-run conflict is checked.
	foreign []*db.Step
	// loopHolds records every offer-time loop-body eviction this scheduler
	// performed: withheld instance -> the open body's "instance (status)".
	// Written by blockingLoopBodyAbsent when it answers true, read by
	// LoopHoldReason — the DKT-61 fact HeldReason carries for the reap case,
	// without which a dispatcher staring at an empty-for-the-judges offer
	// cannot tell this hold from any other narrowing.
	loopHolds map[string]string
	// budgetHolds records every step R7 withheld for lack of budget headroom:
	// instance -> its expected cost. Written by budgetHeadroom when it answers
	// false, read by BudgetHoldReason.
	//
	// It exists for the same reason loopHolds does. With 0.9 headroom the
	// engine offered 1 of 5 ready judges and said nothing, then `next`
	// answered {"steps":[],"total":0} against a run reporting 9 pending — a
	// round serialized around an invisible wall (DKT-242). `run budget`
	// already states the numbers honestly; the scheduling verbs did not.
	budgetHolds map[string]float64
	issues      map[int]*issueFacts
	// issueLabels are the run's issues' labels AS FROZEN AT ACTIVATION, keyed by
	// issue id. Readiness never consults them — they exist so a rendered `next`
	// row can carry what a dispatcher needs to route (model/StepRow.Labels).
	issueLabels map[int][]string
	// foreignScopes are the scope globs of issues OUTSIDE this run whose steps
	// currently hold a scope. They are loaded eagerly rather than looked up
	// lazily because a missing entry would read as "no scope" — S1's
	// never-excludes case — and silently turn a cross-run conflict into a
	// double claim on one tree. The unknown case must not be the permissive
	// one.
	foreignScopes map[int][]string
	limits        map[string]workflow.Limit
	// limitSources names, per class, the `name@version` of the workflow whose
	// `[limits]` declaration set the effective max. Carried so a headroom
	// refusal can say WHERE the cap is set — RUN-2's operator had to grep the
	// corpus to answer "what is the cap?" (DKT-23), which is the failure mode
	// refusals exist to prevent.
	limitSources map[string]string
	defs         map[int]*workflow.Definition
	nowMS        int64
	stepByID     map[int]*db.Step
	// budget is R7's half of the snapshot, loaded once for the same reason the
	// rest is (§4.8 B31): R3, R4, R5 and now R7 are questions about the same
	// rows at the same instant. With an effective cap of 0 it is loaded WITHOUT
	// executing a query at all, which is D1's dormancy (§4.8 B29).
	budget budgetSnapshot
	// reapHold is A12's predicate, materialized once per snapshot for the same
	// reason the budget is: readiness must answer every step against ONE state
	// of the world. It counts UNACKNOWLEDGED reaps per class, and it is nil on
	// the dormant path — a run with nothing reaped never allocates it (D3).
	reapHold map[string]int
	// openReaps are the rows behind reapHold, kept so the REFUSAL can name the
	// same reaps the predicate counted. A headroom denial with nothing running
	// is baffling unless the message names why (§6.3).
	openReaps []db.ReapAck
	// holdTally is the project's configured decider for MATERIALIZED held
	// steps, loaded with the rest of the snapshot so every spec this scheduler
	// synthesizes answers to one state of the config. Readiness never consults
	// it — a held step's `after` is empty whichever form it takes — but the
	// spec resolver does, and a resolver that read config per call could hand
	// two rows of one `next` two different rosters.
	holdTally holdTally
	// voteProposals maps a vote step's (issue, instance) to the proposal opened
	// for it, so a rendered row can carry the id an operator needs to find the
	// ballot. Keyed by voteStepKey rather than instance alone (DKT-65): a
	// materialized held-cluster instance carries no issue identity, and a
	// declared step's instance repeats across every issue bound to the same
	// workflow, so instance alone let two issues' vote steps collide on one
	// entry. One query for the run, and DORMANT: a run with no vote steps
	// executes it not at all, the same shape as the budget and reap-hold
	// snapshots.
	voteProposals map[voteStepKey]int
	// loopClosureCache memoizes afterLoopDownstream (loop.go) per workflow id.
	// The value is derived purely from the pinned definition and cannot change
	// within one Scheduler snapshot, but precedesInSet and blockingLoopBodyAbsent
	// (stage.go) each call it once per candidate pair/row per ClaimablePrefix
	// fixed-point pass, so recomputing the definition's own fixed point that
	// often is waste this removes. Populated lazily via loopClosure, not
	// eagerly here, because most snapshots involve no loop step at all.
	loopClosureCache map[int]map[string]bool
}

// loopClosure returns afterLoopDownstream(def), computed once per workflow id
// per Scheduler and reused — see loopClosureCache's own doc for why.
func (s *Scheduler) loopClosure(workflowID int, def *workflow.Definition) map[string]bool {
	if set, ok := s.loopClosureCache[workflowID]; ok {
		return set
	}
	set := afterLoopDownstream(def)
	if s.loopClosureCache == nil {
		s.loopClosureCache = make(map[int]map[string]bool)
	}
	s.loopClosureCache[workflowID] = set
	return set
}

// issueFacts is the per-issue state readiness needs: the LIVE scope (R4 reads
// it live by design — §5.1.1 — so an operator's mid-run correction takes
// effect), the priority for ordering, and whether the issue's own
// dependencies are satisfied.
type issueFacts struct {
	id         int
	priority   model.Priority
	scopeGlobs []string
	depsOK     bool
}

// LoadScheduler reads everything the predicate needs, once, inside tx.
func LoadScheduler(tx *sql.Tx, runID int, defs map[int]*workflow.Definition, nowMS int64) (*Scheduler, error) {
	run, err := db.GetRunTx(tx, runID)
	if err != nil {
		return nil, err
	}

	steps, err := db.ListRunStepsTx(tx, runID)
	if err != nil {
		return nil, err
	}

	foreign, err := foreignHoldingStepsTx(tx, run.ProjectID, runID)
	if err != nil {
		return nil, err
	}

	runIssues, err := db.ListRunIssuesTx(tx, runID)
	if err != nil {
		return nil, err
	}

	facts := make(map[int]*issueFacts, len(runIssues))
	// Labels come from the FROZEN snapshot activation wrote, not from a live
	// join — the same source and the same reason as the context bundle (§6.6).
	// Routing is a context question, so a mid-run relabel must not silently
	// change how an already-scheduled step routes. Contrast `scope_globs` in
	// loadIssueFacts, read live on purpose because it answers a scheduling
	// question instead. No extra query: ListRunIssuesTx already carries it.
	labels := make(map[int][]string, len(runIssues))
	for _, ri := range runIssues {
		f, err := loadIssueFacts(tx, ri.IssueID)
		if err != nil {
			return nil, err
		}
		facts[ri.IssueID] = f

		ls, err := snapshotLabels(ri.IssueSnapshot)
		if err != nil {
			return nil, fmt.Errorf("reading the issue snapshot for %s: %w",
				model.FormatID(ri.IssueID), err)
		}
		labels[ri.IssueID] = ls
	}

	// Every foreign holder's scope, eagerly: an unknown scope must not read as
	// an empty one (see the field comment).
	foreignScopes := make(map[int][]string)
	for _, holder := range foreign {
		if _, ok := facts[holder.IssueID]; ok {
			continue
		}
		if _, ok := foreignScopes[holder.IssueID]; ok {
			continue
		}
		var scope sql.NullString
		err := tx.QueryRow(
			`SELECT scope_globs FROM issues WHERE id = ?`, holder.IssueID,
		).Scan(&scope)
		if err != nil {
			return nil, fmt.Errorf("reading scope for %s: %w",
				model.FormatID(holder.IssueID), err)
		}
		globs, err := decodeScope(scope.String)
		if err != nil {
			return nil, fmt.Errorf("reading scope for %s: %w",
				model.FormatID(holder.IssueID), err)
		}
		foreignScopes[holder.IssueID] = globs
	}

	// R7's snapshot, in the same transaction as the rest of it (§4.8 B31). With
	// an effective cap of 0 this executes NO query beyond the config read that
	// establishes the cap is 0 — see loadBudget — so a run without a budget in a
	// repo without a default is byte-identical to v9's scheduling path.
	budget, err := loadBudget(tx, run)
	if err != nil {
		return nil, err
	}

	// A12's half of the snapshot, in the same transaction and for the same
	// reason (§6.3): the hold is a question about the same rows at the same
	// instant as R5, which folds it in. On a database where nothing was ever
	// reaped this is one lookup on a partial index that returns no row (D3).
	reapHold, openReaps, err := loadReapHold(tx, runID)
	if err != nil {
		return nil, err
	}

	// The project's held-step decider, and the proposals its vote steps have
	// opened — the last two rows of the same snapshot, for the same reason: a
	// spec synthesized twice in one call must be synthesized identically.
	tally, err := loadHoldTallyTx(tx, run.ProjectID)
	if err != nil {
		return nil, err
	}
	proposals, err := loadVoteProposalsTx(tx, runID, steps)
	if err != nil {
		return nil, err
	}

	limits, limitSources := mergeLimits(defs)
	s := &Scheduler{
		run: run, steps: steps, foreign: foreign, issues: facts,
		issueLabels:   labels,
		foreignScopes: foreignScopes,
		defs:          defs, nowMS: nowMS,
		stepByID:      make(map[int]*db.Step, len(steps)),
		limits:        limits,
		limitSources:  limitSources,
		budget:        budget,
		reapHold:      reapHold,
		openReaps:     openReaps,
		holdTally:     tally,
		voteProposals: proposals,
	}
	for _, step := range steps {
		s.stepByID[step.ID] = step
	}
	return s, nil
}

// loadIssueFacts reads one issue's scheduling facts. `scope_globs` is read LIVE
// and deliberately so: the scheduler asks "what does this issue touch NOW", so
// an operator's mid-run scope correction — which exists precisely to prevent a
// collision — takes effect immediately. The CONTEXT BUNDLE asks a different
// question and reads the snapshot (§6.6); the two answers legitimately differ.
func loadIssueFacts(tx *sql.Tx, issueID int) (*issueFacts, error) {
	var (
		priority string
		scope    sql.NullString
	)
	err := tx.QueryRow(
		`SELECT priority, scope_globs FROM issues WHERE id = ?`, issueID,
	).Scan(&priority, &scope)
	if err != nil {
		return nil, fmt.Errorf("reading scheduling facts for %s: %w",
			model.FormatID(issueID), err)
	}

	globs, err := decodeScope(scope.String)
	if err != nil {
		return nil, fmt.Errorf("reading scope for %s: %w", model.FormatID(issueID), err)
	}

	depsOK, err := issueDependenciesSatisfiedTx(tx, issueID)
	if err != nil {
		return nil, err
	}

	return &issueFacts{
		id: issueID, priority: model.Priority(priority),
		scopeGlobs: globs, depsOK: depsOK,
	}, nil
}

// snapshotLabels pulls the labels out of a `run_issues.issue_snapshot` blob.
//
// An issue frozen before the snapshot carried labels, or one with none, yields
// nil rather than an error: a label-less issue is ordinary, and `omitempty` on
// the wire makes it serialize exactly as it did before the field existed.
func snapshotLabels(snapshot string) ([]string, error) {
	if snapshot == "" {
		return nil, nil
	}
	var frozen struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(snapshot), &frozen); err != nil {
		return nil, err
	}
	return frozen.Labels, nil
}

// LabelsFor is the frozen labels of the issue a step belongs to, for rendering
// a `next` row. It answers nothing about readiness.
func (s *Scheduler) LabelsFor(issueID int) []string {
	if s == nil {
		return nil
	}
	return s.issueLabels[issueID]
}

func decodeScope(stored string) ([]string, error) {
	if stored == "" {
		return nil, nil
	}
	var globs []string
	if err := json.Unmarshal([]byte(stored), &globs); err != nil {
		return nil, fmt.Errorf("stored scope is not a JSON array: %w", err)
	}
	return globs, nil
}

// issueDependenciesSatisfiedTx is R2: every `depends_on` predecessor of this
// issue is `done`.
//
// It asks about the ISSUE graph, not the step graph — R3 is the step half —
// and it reads the relation table directly rather than through the planner
// because the planner's DAG is built over a candidate set, while this question
// is about one issue and all of its predecessors, in or out of the run.
func issueDependenciesSatisfiedTx(tx *sql.Tx, issueID int) (bool, error) {
	rows, err := tx.Query(
		`SELECT i.status
		   FROM issue_relations r
		   JOIN issues i ON i.id = r.target_issue_id
		  WHERE r.source_issue_id = ? AND r.relation_type = 'depends_on'`,
		issueID,
	)
	if err != nil {
		return false, fmt.Errorf("reading dependencies for %s: %w",
			model.FormatID(issueID), err)
	}
	defer rows.Close()

	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			return false, fmt.Errorf("reading dependency status: %w", err)
		}
		if model.Status(status) != model.StatusDone {
			return false, nil
		}
	}
	return true, rows.Err()
}

// foreignHoldingStepsTx reads the claimed/running steps of every OTHER active
// run IN THE SAME PROJECT. A scope is held against the whole repository, not
// against one run, so a step claimed elsewhere excludes here (§6.5 S3) — but
// only within the project (v12): scope globs are literal path prefixes with no
// repo qualifier, so without the project predicate `internal/**` in one
// repository would block `internal/**` in every other repository sharing the
// store.
func foreignHoldingStepsTx(tx *sql.Tx, projectID, runID int) ([]*db.Step, error) {
	rows, err := tx.Query(
		`SELECT s.id, s.issue_id, s.status
		   FROM steps s
		   JOIN runs r ON r.id = s.run_id
		  WHERE s.run_id != ? AND s.status IN (?, ?)
		    AND r.status NOT IN (?, ?)
		    AND r.project_id = ?`,
		runID, db.StepClaimed, db.StepRunning,
		string(model.RunDone), string(model.RunAbandoned),
		db.DefaultProjectIDOr(projectID),
	)
	if err != nil {
		return nil, fmt.Errorf("reading steps holding scope elsewhere: %w", err)
	}
	defer rows.Close()

	var out []*db.Step
	for rows.Next() {
		var s db.Step
		if err := rows.Scan(&s.ID, &s.IssueID, &s.Status); err != nil {
			return nil, fmt.Errorf("reading foreign step: %w", err)
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

// mergeLimits collects the `[limits]` tables of every bound workflow, keyed by
// class. Classes are opaque strings and two workflows naming the same class
// mean the same class — that is what makes `[limits] write = { max = 1 }`
// serialize writers ACROSS pipelines, which is the whole point of the class
// being a repo-wide accounting key rather than a per-workflow one.
//
// On a conflict the TIGHTER bound wins. A class constrained to 1 by one
// pipeline and 4 by another is a class whose stricter declaration exists for a
// reason; taking the looser one would silently discard it.
//
// The second return names, per class, the `name@version` whose declaration set
// the effective max, so a headroom refusal can cite its source (DKT-23).
// Definitions are visited in pinned-id order, which makes the recorded source
// deterministic when two declarations tie — map order would let the same
// refusal name a different workflow on each invocation.
func mergeLimits(defs map[int]*workflow.Definition) (map[string]workflow.Limit, map[string]string) {
	ids := make([]int, 0, len(defs))
	for id := range defs {
		ids = append(ids, id)
	}
	sort.Ints(ids)

	out := make(map[string]workflow.Limit)
	sources := make(map[string]string)
	for _, id := range ids {
		def := defs[id]
		for class, limit := range def.Limits {
			existing, ok := out[class]
			if !ok {
				out[class] = limit
				if limit.Max > 0 {
					sources[class] = fmt.Sprintf("%s@%d",
						def.Pipeline.Name, def.Pipeline.Version)
				}
				continue
			}
			if limit.Max > 0 && (existing.Max == 0 || limit.Max < existing.Max) {
				existing.Max = limit.Max
				sources[class] = fmt.Sprintf("%s@%d",
					def.Pipeline.Name, def.Pipeline.Version)
			}
			if existing.LeaseTTL == "" {
				existing.LeaseTTL = limit.LeaseTTL
			}
			if existing.MaxStepDuration == "" {
				existing.MaxStepDuration = limit.MaxStepDuration
			}
			out[class] = existing
		}
	}
	return out, sources
}

// Steps returns the loaded step set, for callers that render alongside the
// readiness answer.
func (s *Scheduler) Steps() []*db.Step { return s.steps }

// Run returns the loaded run.
func (s *Scheduler) Run() *model.Run { return s.run }

// Limit returns the effective `[limits]` entry for a class.
func (s *Scheduler) Limit(class string) workflow.Limit { return s.limits[class] }

// Ready reports whether a step satisfies R1-R7, and when it does not, WHICH
// condition failed.
//
// The clauses are evaluated in R1..R7 order and the first failure is reported.
// That order is not arbitrary: it goes from the cheapest and most global (is
// the run even active?) to the most local, so the reason a caller sees is the
// most useful one — "the run is paused" explains a whole stalled dispatcher,
// where "no headroom" on the same step would send someone hunting for a
// concurrency problem that is not there.
func (s *Scheduler) Ready(step *db.Step) (bool, ReadyCondition) {
	// R1: the run is active.
	if s.run.Status != model.RunActive {
		return false, CondRunActive
	}

	// R6: the step's status is `pending`. A `skipped` step (its `when` was
	// false at expansion) is excluded here, since expansion CREATES it skipped
	// rather than omitting it (§5.3.1) — which is what keeps a downstream
	// `after` resolvable.
	//
	// R6 is evaluated before R2-R5 despite its number because the other
	// clauses are questions about a step that COULD run, and a done, skipped,
	// or already-claimed step is not one. Asking whether a `done` step has
	// concurrency headroom is a category error, not a cheaper path to `false`.
	if step.Status != db.StepPending {
		return false, CondStatus
	}

	facts := s.issues[step.IssueID]

	// R2: the issue's depends_on predecessors are satisfied.
	if facts != nil && !facts.depsOK {
		return false, CondIssueDeps
	}

	// R3: intra-workflow `after` predecessors are done.
	if !s.predecessorsDone(step) {
		return false, CondPredecessors
	}

	// R3's interposition clause (DKT-38): a threshold-target step waits for a
	// routing predecessor to actually route TO it. After R3, so the ordinary
	// join question is answered first — a gate behind a still-running routing
	// step reports CondPredecessors (and stays stageable behind it), while a
	// gate whose predecessors all ended without naming it reports this.
	if !s.routedTo(step) {
		return false, CondUnrouted
	}

	// R3's second interposition clause (DKT-168): an OPEN interposed gate on a
	// predecessor holds that predecessor's ordinary downstream. Deadlock-free
	// by the routing's own bookkeeping: a routing that chose against the gate
	// terminalizes it in the same transaction (skipUnroutedTargets), one that
	// chose it leaves it pending exactly until it resolves, and a gate parked
	// `waiting-human` is the intended escalation.
	if len(s.openInterposedGates(step)) > 0 {
		return false, CondGateOpen
	}

	// R4: scope non-overlap against claimed/running steps.
	if s.scopeConflict(step) {
		return false, CondScope
	}

	// R5: per-class concurrency headroom.
	if !s.classHeadroom(step) {
		return false, CondHeadroom
	}

	// R7: budget headroom — S6's, a seam returning true at S3 (§6.3).
	if !s.budgetHeadroom(step) {
		return false, CondBudget
	}

	return true, ""
}

// AwaitingDecision reports whether this step is a DECLARED gate whose turn has
// come and which nobody has decided yet — the issue-mirror's third `review`
// shape (DKT-334).
//
// It is R3 and its two interposition clauses, R6, and R2, and deliberately NOT
// the rest of Ready(). R1 (run active), R4 (scope), R5 (class headroom) and R7
// (budget) are all questions about scheduling EXECUTOR work: nobody claims a
// gate, it holds no tree and consumes no class or budget headroom, and pausing
// the run does not answer the question the gate is asking. A gate whose turn
// has come is outstanding against a human whatever the scheduler is doing with
// the rest of the run.
//
// Materialized held steps are excluded because they are already counted by
// their own shape (a non-terminal `materialized` row), which needs no
// scheduler at all.
//
// It lives here, on the Scheduler, rather than as a query in reconcile.go so
// that R3 has exactly ONE implementation. A second copy — "are this step's
// predecessors terminal", written out longhand next to the mirror — is what
// would drift at the first fanout join or interposed gate that mattered.
func (s *Scheduler) AwaitingDecision(step *db.Step) bool {
	if step.Status != db.StepPending || step.Materialized {
		return false
	}
	if step.Kind != workflow.TypeHuman && step.Kind != workflow.TypeVote {
		return false
	}
	if facts := s.issues[step.IssueID]; facts != nil && !facts.depsOK {
		return false
	}
	return s.predecessorsDone(step) && s.routedTo(step) &&
		len(s.openInterposedGates(step)) == 0
}

// IssueAwaitingDecision reports whether ANY step of one issue is an undecided
// declared gate whose turn has come.
func (s *Scheduler) IssueAwaitingDecision(issueID int) bool {
	for _, step := range s.steps {
		if step.IssueID == issueID && s.AwaitingDecision(step) {
			return true
		}
	}
	return false
}

// predecessorsDone is R3, and with it the fanout JOIN rules J1-J5 (§7.5, §2).
//
//	J1  a fanned-out step joins when EVERY sibling is terminal
//	    (done | skipped | superseded | failed-routed)
//	J2  a sibling in `waiting-human` PARKS the issue
//	J3  downstream `inputs` resolve over RECORDED siblings only [context.go]
//	J4  `on_fail` applies per sibling                          [the saga]
//	J5  `min_siblings` permits quorum joins: the join STILL WAITS FOR ALL
//	    siblings (no early cancel in v1), and if the `done` count is below
//	    `min_siblings` at join, the fanned step routes per its `on_fail`
//
// J1 is why the terminal set is wider than `done`: a sibling that failed and
// routed, or was skipped by its `when`, or was superseded by a loop entry, has
// ENDED — and a join waiting for it to be `done` would wait forever. J2 falls
// out of the same test rather than being written separately: `waiting-human` is
// not terminal, so the join does not advance, and the issue is parked exactly
// as §2 says. Writing J2 as its own branch would be writing the same condition
// twice and inviting the two to disagree.
func (s *Scheduler) predecessorsDone(step *db.Step) bool {
	def := s.defs[step.WorkflowID]
	if def == nil {
		return false
	}
	// H5/H6: a materialized held step gets its SYNTHESIZED spec, which
	// declares no `after` — so R3 is vacuous and it is ready the moment it
	// exists. That is correct: the question it asks is already answered by data
	// on disk, so there is nothing for it to wait on.
	spec := materializedSpec(def, step, s.holdTally)
	if spec == nil {
		return false
	}

	for _, predName := range spec.After {
		siblings := s.predecessorInstances(step, predName)
		if len(siblings) == 0 {
			// A predecessor with no instance at or below this ordinal has not
			// been expanded (or is a loop body not yet entered): not done.
			return false
		}

		// J1/J2: EVERY sibling must be terminal. `waiting-human` is not, so an
		// operator's open question holds the join — which is the park.
		for _, pred := range siblings {
			if !db.StepTerminal(pred.Status) {
				return false
			}
		}

		// J5: the quorum is compared ONLY NOW — after every sibling is
		// terminal. Reaching `min_siblings` early does not release the join
		// ("no early cancel in v1"), which is the clause an optimizer breaks:
		// short-circuiting the loop above the moment the count is met would
		// pass every test that checks the happy path and silently start
		// downstream work while two siblings were still running.
		//
		// A quorum MISS does not make the successor ready — the fanned step
		// routes per its `on_fail` (the saga's J4 half), and readiness is not
		// the place to decide it.
		if !s.quorumMet(predName, def, siblings) {
			return false
		}
	}
	return true
}

// predecessorInstances returns the instances of a named predecessor that R3
// answers over: those at the step's OWN ordinal when there are any, otherwise
// those at the HIGHEST ordinal below it.
//
// This is §7.4's rule applied to `after` edges rather than to `inputs`, and it
// is required by the same fact about loops: re-instantiation covers the
// `after_loop` chain only, so a step at ordinal k routinely declares `after` a
// step that exists at ordinal 0 alone. The fixture makes it concrete —
// `review@1` declares `after = ["implement"]`, and `implement` is upstream of
// `after_loop` and never re-runs. Asking only for `implement@1` finds nothing
// and deadlocks the loop at its first step.
//
// The reason to fall back rather than to search all ordinals: `done` at some
// ordinal is not the question. A predecessor that re-ran at ordinal k must be
// judged at ordinal k — its ordinal-0 instance being `done` says nothing about
// the work ordinal k just redid — so the step's own ordinal wins whenever it
// has instances, and the fallback applies only where re-instantiation did not
// reach.
func (s *Scheduler) predecessorInstances(step *db.Step, predName string) []*db.Step {
	if at := s.instancesOf(step.IssueID, predName, step.Ordinal); len(at) > 0 {
		return at
	}

	best := -1
	for _, other := range s.steps {
		if other.IssueID != step.IssueID || other.StepName != predName {
			continue
		}
		if other.Ordinal < step.Ordinal && other.Ordinal > best {
			best = other.Ordinal
		}
	}
	if best < 0 {
		return nil
	}
	return s.instancesOf(step.IssueID, predName, best)
}

// routedTo is R3's interposition clause (DKT-38): for a step some `threshold`
// routes to by name, at least one routing predecessor instance must have
// RECORDED that routing. A step no threshold names answers true — the clause
// is invisible to ordinary steps.
//
// The recorded routing is read from `done` instances only: a step-name routing
// is the one routing that produces `done` (statusForRouting), and a superseded
// or failed-routed predecessor's late routing is the ledger's record of an
// INERT decision (§7.3 (3)) — opening a gate on it would give a stale lineage
// the downstream effect the supersede exists to remove. Instances resolve by
// predecessorInstances' own ordinal rule, so a gate at ordinal k is opened by
// ITS ordinal's routing, falling back exactly where re-instantiation did not
// reach.
//
// A materialized held step is never a target — its name is minted, not
// declared — and RoutingPredecessors answers empty for it, so it takes the
// ordinary-path answer without a special case.
func (s *Scheduler) routedTo(step *db.Step) bool {
	def := s.defs[step.WorkflowID]
	if def == nil {
		return true // predecessorsDone already refused this step.
	}
	preds := workflow.RoutingPredecessors(def, step.StepName)
	if len(preds) == 0 {
		return true
	}
	for _, predName := range preds {
		for _, pred := range s.predecessorInstances(step, predName) {
			if pred.Status == db.StepDone && routingIs(pred.Routing, step.StepName) {
				return true
			}
		}
	}
	return false
}

// openInterposedGates returns the non-terminal interposed-gate instances that
// hold this step (DKT-168): for each `after` predecessor whose threshold
// names step-name targets, the target instances that are not yet terminal.
// The step's own name is excluded — a gate is not held by itself — and its
// instances resolve by predecessorInstances' ordinal rule, the same fallback
// every other R3 read uses. A target with no instances holds nothing: there
// is no gate to wait on.
func (s *Scheduler) openInterposedGates(step *db.Step) []*db.Step {
	def := s.defs[step.WorkflowID]
	if def == nil {
		return nil
	}
	spec := materializedSpec(def, step, s.holdTally)
	if spec == nil {
		return nil
	}
	var out []*db.Step
	for _, predName := range spec.After {
		predSpec := workflow.StepByName(def, predName)
		if predSpec == nil {
			continue
		}
		for _, target := range workflow.ThresholdTargets(predSpec.Threshold) {
			if target == step.StepName {
				continue
			}
			for _, gate := range s.predecessorInstances(step, target) {
				if !db.StepTerminal(gate.Status) {
					out = append(out, gate)
				}
			}
		}
	}
	return out
}

// QuorumMisses returns the fanned-out predecessors of this run whose join has
// completed (every sibling terminal) but whose `done` count fell below
// `min_siblings` — J5's failure half.
//
// It is separate from the readiness predicate because the two answer different
// questions. Readiness asks "may this step run?", and the answer for a missed
// quorum is no, forever — the successor's predecessor produced too few results
// and no amount of waiting changes that. J5 says what happens INSTEAD: "the
// fanned step routes per its `on_fail`". Without this, a missed quorum would
// leave the run silently wedged — every sibling terminal, the successor never
// ready, nothing left to complete and nothing to explain why.
//
// The siblings are returned rather than routed here so the caller applies the
// routing in a transaction it owns, alongside the events, exactly as the saga
// does for an ordinary failure.
func (s *Scheduler) QuorumMisses() []*db.Step {
	var out []*db.Step

	for _, step := range s.steps {
		// The routing is carried by a sibling that itself ended `done` — the
		// quorum failed AROUND it, not because of it — or by one this
		// resolution already moved off `done`. The caller's reason check is
		// what makes the second case idempotent; excluding it here would make
		// that check unreachable and leave the routing to be re-derived from a
		// state that no longer describes a miss.
		if step.SiblingIndex == nil {
			continue
		}
		if step.Status != db.StepDone && step.Routing == "" {
			continue
		}
		def := s.defs[step.WorkflowID]
		if def == nil {
			continue
		}
		spec := workflow.StepByName(def, step.StepName)
		// Only a REAL quorum can be missed. The materialized default
		// (`min_siblings` = the sibling count) is J1, not a quorum — see
		// quorumMet — and treating it as one here would route every ordinary
		// fanout that had a single failed sibling.
		if spec == nil || spec.MinSiblings == nil || *spec.MinSiblings >= len(spec.Fanout) {
			continue
		}

		siblings := s.instancesOf(step.IssueID, step.StepName, step.Ordinal)
		joined := true
		for _, sib := range siblings {
			if !db.StepTerminal(sib.Status) {
				joined = false
				break
			}
		}
		// The join must be COMPLETE before the comparison — no early cancel
		// (J5). A quorum that is short with siblings still running is not a
		// miss yet; it is a join in progress.
		if !joined || s.quorumMet(step.StepName, def, siblings) {
			continue
		}
		out = append(out, step)
	}
	return out
}

// quorumMet is J5's comparison: at join time, are at least `min_siblings` of
// the fanned-out predecessor's siblings `done`?
//
// `done` and not merely terminal, deliberately. The quorum asks how many
// siblings PRODUCED A RESULT, and a skipped, superseded, or failed-routed
// sibling produced none — J3 makes the same distinction for inputs, and the two
// would be incoherent if this one counted more broadly.
//
// THE MATERIALIZED DEFAULT IS NOT A QUORUM. Registration fills `min_siblings`
// with `len(fanout)` when the author declared none (§11.1 "default = all"), so
// reading the stored value at face value would impose "every sibling must be
// `done`" on every ordinary fanout — and the fixture's `review`, with one
// `skipped` sibling, would deadlock. §11.1's default means "the ordinary join",
// which is J1's every-sibling-TERMINAL rule and is already enforced above.
//
// A quorum is therefore checked only when the declaration is a REAL one: below
// the sibling count. `min_siblings = len(fanout)` — whether written by an author
// or materialized by the default — is J1 and nothing more.
func (s *Scheduler) quorumMet(predName string, def *workflow.Definition, siblings []*db.Step) bool {
	predSpec := workflow.StepByName(def, predName)
	if predSpec == nil || predSpec.MinSiblings == nil {
		return true
	}
	if *predSpec.MinSiblings >= len(predSpec.Fanout) {
		return true
	}

	done := 0
	for _, sib := range siblings {
		if sib.Status == db.StepDone {
			done++
		}
	}
	return done >= *predSpec.MinSiblings
}

// instancesOf returns every instance of a named step for one issue at an
// ordinal — the fanout siblings when there are any, or the single instance.
func (s *Scheduler) instancesOf(issueID int, name string, ordinal int) []*db.Step {
	var out []*db.Step
	for _, step := range s.steps {
		if step.IssueID == issueID && step.StepName == name && step.Ordinal == ordinal {
			out = append(out, step)
		}
	}
	return out
}

// scopeConflict is R4: does this step's issue scope intersect the scope of any
// claimed or running step?
//
// S4: computed FRESH on every call, stored nowhere. Exclusion is symmetric, so
// the comparison is over scope lists rather than over any recorded claim.
func (s *Scheduler) scopeConflict(step *db.Step) bool {
	facts := s.issues[step.IssueID]
	if facts == nil || len(facts.scopeGlobs) == 0 {
		return false // S1: a scope-less issue is never excluded.
	}

	// A step that does not hold the tree is not excluded BY the tree. Judges
	// read the working tree without writing it, and an artifact-only step like
	// `synthesize` reads recorded payloads and never opens a file — yet both
	// inherited their issue's scope and lost claim races over files they would
	// never touch. Exclusion is symmetric, so this is also the first half of
	// letting such steps coexist across issues.
	if !s.holdsTree(step) {
		return false
	}

	holders := make([]*db.Step, 0, len(s.steps)+len(s.foreign))
	for _, other := range s.steps {
		// The second half: a non-holder excludes nobody. Without this a
		// running judge would still block another issue's writer, which is the
		// same serialization read from the other side.
		if other.ID != step.ID && stepExcludesScope(other.Status) && s.holdsTree(other) {
			holders = append(holders, other)
		}
	}
	holders = append(holders, s.foreign...)

	for _, holder := range holders {
		if holder.IssueID == step.IssueID {
			// Two steps of the SAME issue share a scope by construction.
			// Excluding on it would serialize every issue's pipeline against
			// itself, which is not what mutual exclusion is for — the fixture's
			// four-way `review` fanout would never run more than one sibling.
			continue
		}
		other := s.foreignScope(holder.IssueID)
		if ScopesIntersect(facts.scopeGlobs, other) {
			return true
		}
	}
	return false
}

// foreignScope reads a holder's scope: from this run's loaded facts when the
// issue is in the run, otherwise from the eagerly-loaded foreign set. Both maps
// are populated at load, so a lookup never silently yields the permissive empty
// answer for an issue that actually declared a scope.
func (s *Scheduler) foreignScope(issueID int) []string {
	if f, ok := s.issues[issueID]; ok {
		return f.scopeGlobs
	}
	return s.foreignScopes[issueID]
}

// classHeadroom is R5: the step's class has room under `[limits] max`.
//
// A class with no declared `max` is UNBOUNDED. Core ships no default of 1 and
// no class named `write`: engine-spec §2 is explicit that "the reference
// instance's config sets its write class to 1 — serialization is INSTANCE
// POLICY, not core behavior", and a hardcoded writer serialization here would
// be instance policy living in core, failing the genericity rule on its face
// (§6.5).
//
// A15, STAGE 6: THE WRITE-REAP HOLD IS FOLDED IN HERE rather than added as an
// R8. An unacknowledged reap OCCUPIES A SLOT in its class, which is exactly
// what engine-spec §2's "holds write headroom" says — so the arithmetic gains a
// term and the predicate gains no clause. The alternative, a separate
// ReadyCondition, would be two mechanisms answering one question ("how many
// things may write to this tree right now"), and the two would disagree the
// first time somebody changed one of them.
func (s *Scheduler) classHeadroom(step *db.Step) bool {
	limit, ok := s.limits[step.Class]
	if !ok || limit.Max <= 0 {
		// A21: an unbounded class gets neither ack rows nor the hold, and
		// scheduling behaves exactly as it did at S5. The return is BEFORE the
		// hold is consulted, so an unbounded class cannot be held even if a row
		// somehow named it.
		return true
	}
	return s.classInFlight(step) < limit.Max
}

// classInFlight is R5's occupancy sum for one step's class. It is a method
// shared by the predicate, the offer's rationing (ClaimablePrefix), and the
// refusal's arithmetic (HeadroomDetail) so the three cannot disagree about
// what occupies a slot — the same single-definition discipline that folded the
// reap hold into headroom rather than adding an R8 (A15).
//
// The sum is PER STEP, not per class: the reap-hold term excludes a hold on
// the step itself (W3 re-offers the reaped step), so two steps of one class
// can legitimately see different occupancy.
func (s *Scheduler) classInFlight(step *db.Step) int {
	inFlight := 0
	for _, other := range s.steps {
		if other.ID == step.ID || other.Class != step.Class {
			continue
		}
		// Occupancy is claimed + running + gated: a `gated` step's worker has
		// finished, but the saga is still the engine's and counting it keeps
		// the bound honest against a burst of completions.
		switch other.Status {
		case db.StepClaimed, db.StepRunning, db.StepGated:
			inFlight++
		}
	}
	// The stage-6 term: a lapsed-but-unconfirmed writer is a thing that may
	// still be writing to the tree, so it counts as in flight — against every
	// step of its class EXCEPT the one it reaped, which W3 requires to be
	// re-offered.
	inFlight += s.unacknowledgedReapsInClass(step.Class, step.ID)
	return inFlight
}

// HeadroomDetail renders the numbers behind a CondHeadroom refusal: the
// class's occupancy, the cap, and the workflow whose `[limits]` set it.
//
// DKT-23's second ask. A dispatcher that just paid a full spawn to hear "no
// concurrency headroom in its class" could answer none of the questions that
// follow — what is the cap, how full is the class, who set the bound — without
// grepping the corpus. The counts come from the same snapshot the predicate
// used, so the message cannot cite different arithmetic from the refusal it
// explains.
func (s *Scheduler) HeadroomDetail(step *db.Step) string {
	limit := s.limits[step.Class]
	held := s.unacknowledgedReapsInClass(step.Class, step.ID)
	claimed := s.classInFlight(step) - held

	detail := fmt.Sprintf("class %q has %d claimed/running", step.Class, claimed)
	if held > 0 {
		detail += fmt.Sprintf(" and %d unacknowledged reap(s) holding headroom", held)
	}
	detail += fmt.Sprintf(" against max %d", limit.Max)
	if source := s.limitSources[step.Class]; source != "" {
		detail += fmt.Sprintf(", set by [limits] in %s", source)
	}
	return detail
}

// budgetHeadroom is R7. The seam engine-spine §6.3 left returning `true` is now
// the real check, and it lives in budget.go beside the arithmetic it shares with
// the claim's enforcement — the call site in Ready never moved, which is what
// the seam was written as a method for.

// Expired reports whether a step's lease should be reaped: either the lease
// lapsed, or the step passed `max_step_duration` measured from `started_ms`.
//
// The second half is the one that matters and is easy to omit. §11.1 makes
// `max_step_duration` a SCHEDULE-TO-CLOSE bound evaluated against `started_ms`,
// "independent of heartbeats — so a runaway executor cannot renew forever". A
// step past it is reaped even with a live lease and a fresh heartbeat, which is
// the entire difference between this and a lease timeout.
//
// BOTH HALVES ARE SCOPED TO AN ACTIVE RUN. A TTL is a bet that a
// silent worker is dead and its step is better re-offered; on a run that is not
// active, R1 refuses to offer anything, so the bet has no payoff and the reap
// is pure loss — it clears a live worker's lease, returns the step to
// `pending`, and takes a write-reap hold on its class. The state is ordinary,
// not exotic: rollupRunTx pauses a run when ANY step is parked (`parked > 0`),
// leaving that step's siblings legitimately `claimed` while the run sits at
// `waiting-human`. That is the mid-wave human hold, and the clock ran against
// workers that were never in trouble.
//
// This is a SUSPENSION, not an exemption. Nothing here writes `expires_ms`, so
// a step whose lease lapsed during a pause is reaped by the first `next` after
// the run returns to `active` — which is what preserves D1 (next.go's reap
// ordering) and `max_step_duration` alike, both of which are statements about
// runs the engine is actually scheduling. Deliberately NOT implemented by
// extending leases on resume: that would need a writer on the paused path, and
// the reap is already lazy, so there is nothing to fix at resume time.
func (s *Scheduler) Expired(step *db.Step) bool {
	if s.run == nil || s.run.Status != model.RunActive {
		return false
	}

	if step.Status != db.StepClaimed && step.Status != db.StepRunning {
		return false
	}

	lease := step.Lease()
	if lease.Held() && !lease.Live(s.nowMS) {
		return true
	}

	limit, ok := s.limits[step.Class]
	if !ok || limit.MaxStepDuration == "" || step.StartedMS == nil {
		return false
	}
	max, err := time.ParseDuration(limit.MaxStepDuration)
	if err != nil || max <= 0 {
		// A malformed duration is V24's error at register time. Reaching here
		// with one means it was never validated; refusing to reap is the safe
		// reading — a step killed by a typo'd bound is worse than one that
		// runs long.
		return false
	}
	return s.nowMS-*step.StartedMS >= max.Milliseconds()
}

// SortSteps orders a ready set by PRIORITY THEN AGE (§2: "Ordering: priority
// then age").
//
// Priority is the ISSUE's, ranked by planner.PriorityRank — the same ranking
// `next` uses, reused rather than redefined so the two verbs cannot disagree
// about what "higher priority" means. Age is the STEP's `created_at_ms`, and
// `id` is the final tie-break so the order is TOTAL and reproducible: two steps
// created in the same millisecond are common (one expansion writes them all),
// and without the id they would order nondeterministically, which would make
// the topology goldens flap.
func (s *Scheduler) SortSteps(steps []*db.Step) {
	sort.SliceStable(steps, func(i, j int) bool {
		pi, pj := s.priorityOf(steps[i]), s.priorityOf(steps[j])
		if pi != pj {
			return pi < pj
		}
		if steps[i].CreatedAtMS != steps[j].CreatedAtMS {
			return steps[i].CreatedAtMS < steps[j].CreatedAtMS
		}
		return steps[i].ID < steps[j].ID
	})
}

// holdsTree reports whether a step occupies its issue's scope while it runs.
//
// Read from the PINNED definition rather than a step column: the answer is a
// property of the workflow the run pinned, so it cannot drift for a step
// already scheduled, and it needs no schema change to ask.
//
// An unresolvable spec answers TRUE. A step whose definition cannot be read is
// the case where guessing "does not hold" would let two writers race one tree,
// and the two error directions are not symmetric (see workflow.Step.HoldsTree).
// Materialized steps, which the engine mints rather than the definition
// declaring, land here too and are correctly treated as holders.
func (s *Scheduler) holdsTree(step *db.Step) bool {
	def := s.defs[step.WorkflowID]
	if def == nil {
		return true
	}
	return stepHoldsTree(workflow.StepByName(def, step.StepName))
}

// stepHoldsTree is holdsTree over an already-resolved spec, for callers
// outside the scheduler — the diff recording (DKT-75) asks the same question
// of the same declaration, and a second reading of the default is how the two
// would drift. Nil answers TRUE for holdsTree's reason: the error directions
// are not symmetric.
func stepHoldsTree(spec *workflow.Step) bool {
	if spec == nil || spec.HoldsTree == nil {
		return true
	}
	return *spec.HoldsTree
}

// ClaimablePrefix narrows a SORTED ready set to the largest prefix-greedy
// subset whose members can be claimed AS A SET: no two admitted rows exclude
// each other on scope, no class is offered more rows than it has remaining
// headroom, and the offer's summed cost never exceeds remaining budget
// headroom (DKT-47).
//
// R4 and R5 ask whether a step conflicts with something that HOLDS — a claimed
// or running scope holder, an occupied class slot. Neither has an opinion about
// two steps that are both merely READY, because neither holds anything yet — so
// `next` offered sets whose own members excluded one another, and a dispatcher
// that spawned the whole set watched every loser die on a claim refusal. RUN-3
// lost 21 of 53 spawns to the scope half (CONFLICT at claim); RUN-2 bounced 5
// write-class spawns off one slot to the class half, each dying on "no
// concurrency headroom" after paying its full worktree bootstrap (DKT-23). In
// both cases the offer was honest about each row in isolation and false about
// the set.
//
// Greedy over the caller's order, which SortSteps has already made total
// (priority, then age, then id). That is what makes this FAIR as well as
// correct: the oldest ready step in a conflicting cluster always wins, every
// time it is asked, so a repeatedly-losing step cannot starve — it becomes the
// oldest and takes its turn. An arbitrary or map-ordered choice would schedule
// just as correctly and starve unpredictably, which is the failure RUN-3's
// review fanout hit going 0-for-5.
//
// All three constraints — class headroom, scope, budget — are checked BEFORE
// any of the three is consumed: a row skipped for headroom grants no scope
// and charges no cost, a row skipped for scope occupies no slot and charges
// no cost, and a row skipped for budget occupies no slot and grants no scope.
// scopeAdmits is a pure predicate for exactly this reason — its grant
// (grantScope) is applied only once every constraint has passed, alongside
// the class and cost charges. A single pass consuming a resource mid-check
// would exclude a later row that fits, which is what a check-then-grant
// scope half did until DKT-47.
//
// This narrows the OFFER, never the readiness predicate: a step held back here
// is still ready and is offered by the very next call once a slot, scope, or
// budget headroom frees. Nothing is written, and R4/R5/R7 remain the
// authority a claim is checked against.
//
// DEPENDENT EVICTION. A row admitted here whose loop body — the same-issue,
// same-ordinal loop step whose `after_loop` chain covers it — is still open
// and unoffered is evicted from the offer as well, via blockingLoopBodyAbsent
// (stage.go), regardless of WHY the body is absent: rationed out of THIS
// offer by class headroom or scope (DKT-26), or excluded from readiness
// itself before it ever reached `sorted` (DKT-48; R7 budget, R5 class
// headroom, R4 scope). blockingLoopBodyAbsent asks the PINNED DEFINITION,
// not the offered set, which is what lets one predicate answer both cases —
// see its own doc comment for the reasoning and RUN-2's measured failures.
//
// precedesInSet stays in use for assignStages' stage LABELS (a comparison
// between two rows of one offer), but eviction no longer runs it: it only
// ever saw a loop body that made it into `sorted` and was then rejected
// here, a proper subset of what blockingLoopBodyAbsent already catches by
// scanning every status of `later`'s run, offer membership included.
//
// The evicted dependents are offered again once the predecessor stops
// blocking — usually because it records, but `db.StepTerminal` also lets a
// body sitting at `waiting-human` (a persisted NON-terminal status, alongside
// claimed/running/gated) hold the offer open-endedly, until an operator acts
// on it. That hold has no dedicated `ReadySteps` field or `ReadyCondition` of
// its own the way a scope or budget park does (next.go), so a dispatcher
// staring at an empty-for-the-judges offer cannot tell this case apart from
// any other narrowing without inspecting the loop body's own status.
//
// The pass iterates to a fixed point because eviction and admission feed each
// other in both directions: the fixer sorts AFTER its judges (younger), so a
// single scan admits the dependents before it ever rejects their predecessor —
// and an evicted dependent frees a slot that may admit a row an earlier pass
// rationed out. Each iteration either returns or grows the evicted set, so the
// bound is the set's length.
func (s *Scheduler) ClaimablePrefix(sorted []*db.Step) []*db.Step {
	if len(sorted) == 0 {
		return sorted
	}

	evicted := make(map[int]bool)
	for {
		out := s.claimablePass(sorted, evicted)

		inOffer := make(map[int]bool, len(out))
		for _, step := range out {
			inOffer[step.ID] = true
		}

		changed := false
		for _, later := range out {
			if s.blockingLoopBodyAbsent(later, inOffer) {
				evicted[later.ID] = true
				changed = true
			}
		}
		if !changed {
			return out
		}
	}
}

// LoopHoldReason renders the loop-body holds this scheduler's offer passes
// recorded, "" when they evicted nothing — ReapHoldReason's shape, for the
// DKT-61 case: rows withheld because their same-issue loop body is open and
// absent from the offer. Without it, a dispatcher staring at an
// empty-for-the-judges offer cannot tell this hold from any other narrowing
// short of inspecting the body's own status. At most three withheld
// instances are named; a reason listing forty is one nobody reads.
func (s *Scheduler) LoopHoldReason() string {
	if len(s.loopHolds) == 0 {
		return ""
	}
	withheld := make([]string, 0, len(s.loopHolds))
	for instance := range s.loopHolds {
		withheld = append(withheld, instance)
	}
	sort.Strings(withheld)
	shown := withheld
	if len(shown) > 3 {
		shown = shown[:3]
	}
	parts := make([]string, 0, len(shown))
	for _, instance := range shown {
		parts = append(parts, fmt.Sprintf("%s (behind %s)", instance, s.loopHolds[instance]))
	}
	suffix := ""
	if len(withheld) > len(shown) {
		suffix = fmt.Sprintf(" and %d more", len(withheld)-len(shown))
	}
	return fmt.Sprintf(
		"%d row(s) withheld behind an open loop body: %s%s — offered again "+
			"once the body records or is resolved",
		len(withheld), strings.Join(parts, ", "), suffix)
}

// BudgetHoldReason names the rows this offer withheld for lack of budget
// headroom, with the numbers that decided it — DKT-242's "withheld: N steps,
// reason=budget headroom X < cost Y".
//
// Same contract as LoopHoldReason and HeldReason: empty whenever nothing was
// withheld, so a run with no cap — B29's dormancy — carries no new field on
// the wire. A withheld step is not refused and not failed; it is simply not
// offered yet, and the difference between that and "there is no work" is the
// whole reason this string exists.
func (s *Scheduler) BudgetHoldReason() string {
	if len(s.budgetHolds) == 0 {
		return ""
	}
	withheld := make([]string, 0, len(s.budgetHolds))
	for instance := range s.budgetHolds {
		withheld = append(withheld, instance)
	}
	sort.Strings(withheld)
	shown := withheld
	if len(shown) > 3 {
		shown = shown[:3]
	}
	parts := make([]string, 0, len(shown))
	for _, instance := range shown {
		parts = append(parts, fmt.Sprintf("%s (cost %g)", instance, s.budgetHolds[instance]))
	}
	suffix := ""
	if len(withheld) > len(shown) {
		suffix = fmt.Sprintf(" and %d more", len(withheld)-len(shown))
	}
	// WHICH cap withheld them, because the two are different controls and
	// raising the wrong one changes nothing (DKT-238). The measured cap is a
	// run-level stop rather than a per-step reservation, so when it is the
	// blocker it blocks everything for one reason and the costs beside each
	// row are not what decided it.
	if !s.budget.usageAdmits() {
		return fmt.Sprintf(
			"withheld: %d step(s), reason=measured %s spend %g exceeds cap %g: "+
				"%s%s — offered again once the usage cap is raised",
			len(withheld), s.budget.usageUnit, s.budget.usageSpend,
			s.budget.usageCap, strings.Join(parts, ", "), suffix)
	}

	// The headroom is stated as ONE number, computed once here rather than per
	// row: it is a property of the run at this instant, and rendering it
	// beside each cost is what lets a reader do the subtraction the engine
	// just did.
	return fmt.Sprintf(
		"withheld: %d step(s), reason=budget headroom %g < cost: %s%s — "+
			"offered again once spend falls or the cap is raised",
		len(withheld), s.budget.headroom(), strings.Join(parts, ", "), suffix)
}

// claimablePass is one greedy admission scan — ClaimablePrefix's original
// body, with the fixed-point's evictions excluded up front so a row evicted
// for a missing predecessor neither occupies a slot nor grants a scope.
func (s *Scheduler) claimablePass(sorted []*db.Step, evicted map[int]bool) []*db.Step {
	out := make([]*db.Step, 0, len(sorted))
	// Scopes already granted to a winner, by issue: two steps of ONE issue
	// share a scope by construction and must still coexist (the same exemption
	// scopeConflict makes), so admission is tracked per issue rather than per
	// step.
	granted := make(map[int][]string, len(sorted))
	// Rows already admitted per bounded class. The snapshot's own occupancy
	// (claimed/running/gated plus the reap hold) is per-step arithmetic —
	// classInFlight, the same sum R5 runs — so only the admissions themselves
	// accumulate here.
	admitted := make(map[string]int)
	// admittedCost is the summed expected_cost of every row THIS OFFER has
	// already admitted; see offerBudget below for why it exists.
	var admittedCost float64

	for _, step := range sorted {
		if evicted[step.ID] {
			continue
		}
		if !s.offerHeadroom(step, admitted) {
			continue
		}
		if !s.scopeAdmits(step, granted) {
			continue
		}
		if !s.offerBudget(step, admittedCost) {
			continue
		}
		// All three constraints passed: only now does the row consume
		// anything, in the same order the doc comment above states —
		// scope's grant included, so a row this pass goes on to reject
		// (on budget, or on a later step's own headroom check) never
		// excludes a later row that fits.
		if limit, ok := s.limits[step.Class]; ok && limit.Max > 0 {
			admitted[step.Class]++
		}
		s.grantScope(step, granted)
		admittedCost += step.ExpectedCost
		out = append(out, step)
	}
	return out
}

// offerHeadroom is ClaimablePrefix's class half: would one more row of this
// step's class leave the offer claimable, counting the slots the snapshot
// already charges (classInFlight — R5's own sum) plus the rows this offer has
// already admitted?
//
// An unbounded class rations nothing, by the same reading writeClassOf makes:
// a class the author left unbounded is one declared safe to parallelize.
func (s *Scheduler) offerHeadroom(step *db.Step, admitted map[string]int) bool {
	limit, ok := s.limits[step.Class]
	if !ok || limit.Max <= 0 {
		return true
	}
	return s.classInFlight(step)+admitted[step.Class] < limit.Max
}

// scopeAdmits is ClaimablePrefix's scope half — a PURE predicate, consulted
// only. It answers whether step's scope is free of every scope already
// GRANTED, never mutating granted itself: DKT-47 found the combined
// check-and-grant shape excluding a later, budget-fitting row on a grant made
// for a row the budget check went on to reject. grantScope, below, is the
// only writer, and claimablePass calls it only once every constraint —
// including budget — has passed.
func (s *Scheduler) scopeAdmits(step *db.Step, granted map[int][]string) bool {
	scope := s.foreignScope(step.IssueID)
	if len(scope) == 0 || !s.holdsTree(step) {
		// S1: a scope-less issue never excludes and is never excluded — and
		// neither does a step that does not hold the tree, which is what lets
		// every issue's judges run in one wave.
		return true
	}
	if _, ok := granted[step.IssueID]; ok {
		// This issue already holds the floor; its siblings ride along.
		return true
	}

	for issueID, held := range granted {
		if issueID == step.IssueID {
			continue
		}
		if ScopesIntersect(scope, held) {
			return false
		}
	}
	return true
}

// grantScope records step's issue as holding the floor, once claimablePass
// has confirmed every admission constraint passed. Called at most once per
// issue (scopeAdmits' own `granted[step.IssueID]` check short-circuits every
// later sibling), so re-recording the same scope is harmless but never
// happens.
func (s *Scheduler) grantScope(step *db.Step, granted map[int][]string) {
	scope := s.foreignScope(step.IssueID)
	if len(scope) == 0 || !s.holdsTree(step) {
		return
	}
	if _, ok := granted[step.IssueID]; ok {
		return
	}
	granted[step.IssueID] = scope
}

// offerBudget is ClaimablePrefix's budget half (DKT-47): would admitting one
// more row of this cost still fit inside remaining headroom, counting every
// row THIS OFFER has already admitted?
//
// Unlimited rations nothing — `unlimited()` is checked first, so the dormant
// path (D1) never runs this arithmetic. Otherwise the check is the snapshot's
// own `spend() + cost <= cap`, widened by `admittedCost`: the same "check
// before consuming" discipline `offerHeadroom` and `scopeAdmits` already keep
// (the doc comment above `claimablePass` states it once for all three). The
// accumulation lives HERE, in the offer, and nowhere near `budgetSnapshot` or
// `admits` — those stay the one stateless answer R7 and the claim's own
// enforcement both read, per TestBudgetR7AndClaimAgree.
func (s *Scheduler) offerBudget(step *db.Step, admittedCost float64) bool {
	if s.budget.unlimited() {
		return true
	}
	return s.budget.spend()+admittedCost+step.ExpectedCost <= s.budget.cap
}

func (s *Scheduler) priorityOf(step *db.Step) int {
	if f, ok := s.issues[step.IssueID]; ok {
		return planner.PriorityRank(f.priority)
	}
	return planner.PriorityRank(model.PriorityNone)
}
