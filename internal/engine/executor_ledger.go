package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `report executors` — the cross-run ledger (DKT-2453).
//
// Every rollup before this one is PER RUN: `run report` answers "what happened
// in RUN-N", and a retro that wants to know how an executor hint or a voter
// name has fared over the last dozen runs reads a dozen reports and joins
// them by hand. Nothing in the store persisted that join, and the store is
// the only place that spans runs and sessions. This verb computes it at read:
// the facts the per-run rollups already carry — fix-loop routings, held
// clusters, override-pass resolutions, reaps and forced reaps, unique versus
// corroborated clusters, and casts — grouped across every run in a window by
// the two identities that recur across runs, the opaque executor hint a step
// declared and the voter name a cast carried.
//
// READ-ONLY, OPERATOR-FACING, AND NEVER AN INPUT TO ROUTING. It opens no
// transaction and writes nothing (TestExecutorLedgerWritesNothing). `next`
// does not consult it and no engine decision reads it: which hint a step
// declares is the workflow author's business, and which seats a policy
// routes is the operator's, made outside this store. It must stay that way
// for a reason beyond genericity — fed back to a seat, a track record becomes
// an incentive to agree with the majority, which is the conformity failure
// the record exists to catch. The ledger is a page an operator reads at
// retro, not a number the engine acts on.
//
// GENERICITY. The hint and the voter name are the opaque strings core already
// carries (`steps.executor`, `votes.voter_name`); every column counts a fact
// core itself recorded (a routing, a ruling, a reap, a cluster's `members`
// and `held`, a verdict), and no column interprets one. Cluster attribution
// rides on `source_field` exactly as the run report's does (DKT-2462): a
// source that resolves to no step of its run has no hint to group under and
// is left out, and a round whose aggregate step declares no `source_field`
// contributes no cluster columns at all.
//
// THE RULING COLUMNS READ THE EVENT LOG, stated rather than hidden. `reaps`,
// `forced_reaps`, and `override_passes` count `lease-reaped` and
// `step-resolved` events on hinted steps, because only the event says which
// reap a relay forced and which resolution an operator chose; the step row's
// own `reaped_claims` counter was not read beside them, since it back-fills
// nothing before v23 and would put a forced count above the total it is a
// subset of. The price is `events prune`: a pruned ruling is gone from every
// one of these columns together, never from one of them.

// ExecutorLedgerOptions bounds the window the ledger reads.
type ExecutorLedgerOptions struct {
	// ProjectID scopes the runs to one project; 0 reads every project in the
	// store, the way `events list --all-projects` does.
	ProjectID int
	// SinceRun admits runs whose id is at least this; 0 sets no floor.
	SinceRun int
	// SinceMS admits runs created at or after this instant; 0 sets no floor.
	SinceMS int64
}

// ExecutorLedger is the whole document: the runs read and one row per
// identity, each list ordered by its name (a total key, R9).
type ExecutorLedger struct {
	Runs      int                 `json:"runs"`
	Executors []ExecutorLedgerRow `json:"executors"`
	Voters    []VoterLedgerRow    `json:"voters"`
}

// ExecutorLedgerRow is one executor hint's record across the window.
type ExecutorLedgerRow struct {
	Executor string `json:"executor"`
	// Runs is how many runs in the window expanded a step carrying the hint.
	Runs  int `json:"runs"`
	Steps int `json:"steps"`
	// FixLoopRoutes counts steps carrying the hint whose recorded routing is
	// `fix-loop` — the step's own threshold or on_fail sent the issue into a
	// fix round.
	FixLoopRoutes int `json:"fix_loop_routes"`
	// OverridePasses counts `step resolve --as override-pass` rulings on steps
	// carrying the hint: the times an operator passed what the step's own
	// gates would not.
	OverridePasses int `json:"override_passes"`
	// Reaps counts the `lease-reaped` events on the hint's steps; ForcedReaps
	// is the subset a relay declared dead with `step reap`.
	Reaps       int `json:"reaps"`
	ForcedReaps int `json:"forced_reaps"`
	// UniqueClusters and CorroboratedClusters count, over every `aggregate`
	// round whose step declares `source_field`, the clusters a step carrying
	// the hint contributed a member to: unique when the cluster had one
	// member, corroborated when it had more. HeldClusters is the subset whose
	// spread tripped `hold_spread`.
	UniqueClusters       int `json:"unique_clusters"`
	CorroboratedClusters int `json:"corroborated_clusters"`
	HeldClusters         int `json:"held_clusters"`
}

// VoterLedgerRow is one voter name's record across the window. Reaps and
// cluster columns do not apply to a seat — a vote step is never claimed and
// produces no cluster — so the row carries neither rather than an honest zero
// that reads as a measurement.
type VoterLedgerRow struct {
	Voter string `json:"voter"`
	// Runs is how many runs in the window the name cast in.
	Runs  int `json:"runs"`
	Casts int `json:"casts"`
	// The casts by verdict, in the tally's own vocabulary.
	Approve             int `json:"approve"`
	ApproveWithConcerns int `json:"approve_with_concerns"`
	Reject              int `json:"reject"`
	// FixLoopRoutes, OverridePasses, and HeldClusters count the casts this
	// name made on a vote step that then routed `fix-loop`, that an operator
	// resolved `override-pass`, or that was a materialized held-cluster
	// ballot. Each is a fact about the panel the seat sat on, never about
	// how the seat voted; the verdict columns carry that.
	FixLoopRoutes  int `json:"fix_loop_routes"`
	OverridePasses int `json:"override_passes"`
	HeldClusters   int `json:"held_clusters"`
}

// executorTally and voterTally are the rows while they accumulate: the same
// counts plus the set of runs behind the `runs` column.
type executorTally struct {
	ExecutorLedgerRow
	runs map[int]bool
}

type voterTally struct {
	VoterLedgerRow
	runs map[int]bool
}

type ledgerTallies struct {
	executors map[string]*executorTally
	voters    map[string]*voterTally
}

func (l *ledgerTallies) executor(hint string, runID int) *executorTally {
	t := l.executors[hint]
	if t == nil {
		t = &executorTally{
			ExecutorLedgerRow: ExecutorLedgerRow{Executor: hint}, runs: map[int]bool{},
		}
		l.executors[hint] = t
	}
	t.runs[runID] = true
	return t
}

func (l *ledgerTallies) voter(name string, runID int) *voterTally {
	t := l.voters[name]
	if t == nil {
		t = &voterTally{VoterLedgerRow: VoterLedgerRow{Voter: name}, runs: map[int]bool{}}
		l.voters[name] = t
	}
	t.runs[runID] = true
	return t
}

// LoadExecutorLedger builds the document over every run the options admit.
// IT WRITES NOTHING.
func LoadExecutorLedger(conn *sql.DB, opts ExecutorLedgerOptions) (*ExecutorLedger, error) {
	runs, _, err := db.ListRuns(conn, db.RunListOptions{ProjectID: opts.ProjectID})
	if err != nil {
		return nil, err
	}
	// ListRuns is newest-first; the ledger walks oldest-first so the run set
	// behind every count is read in one fixed order (R9).
	sort.Slice(runs, func(i, j int) bool { return runs[i].ID < runs[j].ID })

	tallies := &ledgerTallies{
		executors: map[string]*executorTally{}, voters: map[string]*voterTally{},
	}
	ledger := &ExecutorLedger{Executors: []ExecutorLedgerRow{}, Voters: []VoterLedgerRow{}}
	for _, run := range runs {
		if opts.SinceRun > 0 && run.ID < opts.SinceRun {
			continue
		}
		if opts.SinceMS > 0 && run.CreatedAtMS < opts.SinceMS {
			continue
		}
		ledger.Runs++
		if err := ledgerRun(conn, run.ID, tallies); err != nil {
			return nil, err
		}
	}

	hints := make([]string, 0, len(tallies.executors))
	for hint := range tallies.executors {
		hints = append(hints, hint)
	}
	sort.Strings(hints)
	for _, hint := range hints {
		t := tallies.executors[hint]
		t.Runs = len(t.runs)
		ledger.Executors = append(ledger.Executors, t.ExecutorLedgerRow)
	}

	names := make([]string, 0, len(tallies.voters))
	for name := range tallies.voters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := tallies.voters[name]
		t.Runs = len(t.runs)
		ledger.Voters = append(ledger.Voters, t.VoterLedgerRow)
	}
	return ledger, nil
}

// ledgerRun folds one run's facts into the tallies.
func ledgerRun(conn *sql.DB, runID int, tallies *ledgerTallies) error {
	steps, err := db.ListRunSteps(conn, runID)
	if err != nil {
		return err
	}
	byID := make(map[int]*db.Step, len(steps))
	byKey := make(map[voteStepKey]*db.Step, len(steps))
	for _, s := range steps {
		byID[s.ID] = s
		byKey[voteStepKeyOf(s)] = s
		if s.Executor == "" {
			continue
		}
		t := tallies.executor(s.Executor, runID)
		t.Steps++
		if routedFixLoop(s.Routing) {
			t.FixLoopRoutes++
		}
	}

	overridden, err := ledgerRulings(conn, runID, byID, tallies)
	if err != nil {
		return err
	}
	if err := ledgerClusters(conn, runID, steps, tallies); err != nil {
		return err
	}
	return ledgerCasts(conn, runID, byKey, overridden, tallies)
}

// routedFixLoop reports whether a step's recorded routing is `fix-loop`,
// with or without the reason routingRecord appends.
func routedFixLoop(routing string) bool {
	return routing == workflow.OnFailFixLoop ||
		strings.HasPrefix(routing, workflow.OnFailFixLoop+":")
}

// ledgerRulings reads the run's override-pass resolutions and reaps from the
// event log, credits the ones on hinted steps, and returns the
// override-passed step ids for the cast half.
func ledgerRulings(
	conn *sql.DB, runID int, byID map[int]*db.Step, tallies *ledgerTallies,
) (map[int]bool, error) {
	rows, err := conn.Query(
		`SELECT step_id, kind, data FROM events
		  WHERE run_id = ? AND step_id IS NOT NULL AND kind IN (?, ?)
		  ORDER BY seq`,
		runID, EventStepResolved, EventLeaseReaped)
	if err != nil {
		return nil, fmt.Errorf("reading the rulings of %s: %w", model.FormatRunID(runID), err)
	}
	defer rows.Close()

	overridden := make(map[int]bool)
	for rows.Next() {
		var (
			stepID int
			kind   string
			data   string
		)
		if err := rows.Scan(&stepID, &kind, &data); err != nil {
			return nil, fmt.Errorf("reading a ruling of %s: %w", model.FormatRunID(runID), err)
		}
		step := byID[stepID]
		if step == nil {
			continue
		}
		var payload struct {
			Detail string `json:"detail"`
			Forced bool   `json:"forced"`
		}
		// Core's own payload; a malformed row costs one count, not the read
		// (R10), the tolerance annotateStepRulings extends to the same rows.
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}
		switch kind {
		case EventStepResolved:
			if payload.Detail != ResolveOverridePass {
				continue
			}
			overridden[stepID] = true
			if step.Executor != "" {
				tallies.executor(step.Executor, runID).OverridePasses++
			}
		case EventLeaseReaped:
			if step.Executor == "" {
				continue
			}
			t := tallies.executor(step.Executor, runID)
			t.Reaps++
			if payload.Forced {
				t.ForcedReaps++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the rulings of %s: %w", model.FormatRunID(runID), err)
	}
	return overridden, nil
}

// ledgerClusters credits each hinted source of every `aggregate` round that
// declares `source_field` with the clusters it contributed to, over the same
// emitted-plus-recorded union the run report reads (DKT-2452, DKT-2462).
func ledgerClusters(conn *sql.DB, runID int, steps []*db.Step, tallies *ledgerTallies) error {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return err
	}
	var executors map[string]string // lazily built: most runs declare no source_field
	for _, s := range steps {
		def := defs[s.WorkflowID]
		if def == nil {
			continue
		}
		spec := workflow.StepByName(def, s.StepName)
		if spec == nil || spec.Action != workflow.ActionAggregate {
			continue
		}
		params, err := ParseAggregateParams(spec.Params)
		if err != nil || params.SourceField == "" {
			continue // the run report's restored-database allowance, and the absent param
		}
		emitted, ok, err := latestAggregatePayload(conn, s.ID, params.Output)
		if err != nil {
			return err
		}
		if !ok || emitted == nil {
			continue
		}
		recorded, err := recordedBelowFloor(conn, s.ID)
		if err != nil {
			return err
		}
		if executors == nil {
			if executors, err = runStepExecutorsByInstance(conn, runID); err != nil {
				return err
			}
		}
		for _, set := range [][]map[string]any{emitted, recorded} {
			for _, cluster := range set {
				members, ok := cluster[KeyMembers].([]any)
				if !ok {
					continue
				}
				held, _ := cluster[KeyHeld].(bool)
				sources, ok := cluster[params.SourceField].([]any)
				if !ok {
					continue
				}
				seen := make(map[string]bool, len(sources))
				for _, v := range sources {
					source, ok := v.(string)
					if !ok || source == "" || seen[source] {
						continue
					}
					seen[source] = true
					hint := executors[source]
					if hint == "" {
						continue
					}
					t := tallies.executor(hint, runID)
					switch {
					case len(members) == 1:
						t.UniqueClusters++
					case len(members) > 1:
						t.CorroboratedClusters++
					}
					if held {
						t.HeldClusters++
					}
				}
			}
		}
	}
	return nil
}

// ledgerCasts credits each voter name with its casts on the run's proposals —
// the vote-step family and the conversational gates the run report already
// attributes — and, where the proposal is a vote step's, with what became of
// that step. A SealedOpen proposal is withheld, as everywhere.
func ledgerCasts(
	conn *sql.DB, runID int, byKey map[voteStepKey]*db.Step,
	overridden map[int]bool, tallies *ledgerTallies,
) error {
	prefix := voteIdempotencyPrefix(runID)
	keyed, err := db.LookupIdempotencyKeys(conn, db.ScopeVoteCreate, prefix)
	if err != nil {
		return fmt.Errorf("reading the proposals of %s: %w", model.FormatRunID(runID), err)
	}
	stepOf := make(map[int]*db.Step, len(keyed))
	ids := make([]int, 0, len(keyed))
	for key, id := range keyed {
		if k, ok := parseVoteStepKey(prefix, key); ok {
			stepOf[id] = byKey[k]
		}
		ids = append(ids, id)
	}
	extra, err := conversationalRunProposalIDs(conn, runID)
	if err != nil {
		return err
	}
	for _, id := range extra {
		if _, seen := stepOf[id]; !seen {
			stepOf[id] = nil
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)

	for _, id := range ids {
		proposal, err := db.GetProposal(conn, id)
		if err != nil {
			return fmt.Errorf("reading %s: %w", model.FormatProposalID(id), err)
		}
		if proposal.SealedOpen() {
			continue
		}
		votes, err := db.GetProposalVotes(conn, id)
		if err != nil {
			return fmt.Errorf("reading the casts of %s: %w", model.FormatProposalID(id), err)
		}
		step := stepOf[id]
		for _, v := range votes {
			t := tallies.voter(v.VoterName, runID)
			t.Casts++
			switch v.Verdict {
			case model.VerdictApprove:
				t.Approve++
			case model.VerdictApproveWithConcerns:
				t.ApproveWithConcerns++
			case model.VerdictReject:
				t.Reject++
			}
			if step == nil {
				continue
			}
			if routedFixLoop(step.Routing) {
				t.FixLoopRoutes++
			}
			if overridden[step.ID] {
				t.OverridePasses++
			}
			if step.Materialized && step.Kind == workflow.TypeVote {
				t.HeldClusters++
			}
		}
	}
	return nil
}
