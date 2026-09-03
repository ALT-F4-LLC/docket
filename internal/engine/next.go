package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `next --run` — the readiness predicate applied to a whole run, with the one
// write path a read-shaped verb is allowed (TDD §6.3).

// ReadySteps is one `next --run` answer: the offer — the ready set plus its
// staged closure (lookahead.go), in stage-major order — and the TRUE total
// before `--limit` truncated it.
type ReadySteps struct {
	Steps []model.StepRow
	// Total is the size of the OFFER (ready rows and staged rows alike)
	// BEFORE slicing — the v2 truncation contract (reliability-delta §4.2).
	// A post-limit count cannot distinguish "exactly N ready" from "N
	// returned, many more dropped", which is the silent drop the v2 envelope
	// exists to close.
	Total int
	// Reaped names the step instances whose leases this call reaped. Reaping is
	// a WRITE, and a verb that writes silently is one an operator cannot audit,
	// so the fact rides out on the result.
	Reaped []string
	// HeldReason names the unacknowledged bounded-class reaps holding headroom,
	// and the flag that clears them (§6.3's closing paragraph, A11's shape).
	// "Bounded" is writeClassOf's reading — any class with a finite max, judge
	// panels included — so the wording must not say "write": a reaped judge
	// holds its slot for the same reason a reaped writer does, and a message
	// that called it write-class sent a relay hunting a misclassification.
	//
	// It rides on the RESULT rather than being folded into the rows because a
	// headroom denial with nothing running is otherwise baffling: an operator
	// sees a class that is bounded at 1, no step claimed, and no work offered.
	// The refusal reports `CondHeadroom` — the existing constant — and this
	// carries the WHY alongside it. Empty on every run that never reaped a
	// bounded class, which is D3's dormancy visible on the wire.
	HeldReason string
	// LoopHeldReason names the rows this offer withheld behind an OPEN loop
	// body, and the body itself (DKT-61) — the eviction ClaimablePrefix and
	// the post-limit pass perform, which previously reached no field at all:
	// a dispatcher staring at an empty-for-the-judges offer could not tell a
	// waiting-human-parked fixer's indefinite hold from any other narrowing
	// without inspecting the body's own status. Same contract as HeldReason:
	// empty whenever nothing was withheld.
	LoopHeldReason string
	// BudgetHeldReason names the rows this offer withheld for lack of budget
	// headroom, with the numbers (DKT-242). Same contract as the two above:
	// empty whenever nothing was withheld, so a run with no cap carries no new
	// field on the wire.
	//
	// It rides on the RESULT for the same reason HeldReason does: a
	// dispatcher offered 1 of 5 ready judges, and then an empty `next` against
	// a run reporting 9 pending, cannot tell a budget wall from a graph that
	// has run dry — and the second reading makes it stop polling.
	BudgetHeldReason string
	// UnroutedReason names PENDING steps CondUnrouted holds back — an
	// interposed threshold target whose routing predecessor already decided,
	// terminally, against naming it (DKT-470). Unlike the three holds above,
	// this one is never resolved by anything ELSE finishing: the routing that
	// would have named this step already happened and recorded a different
	// routing, so the step is not "not yet" ready, it is never going to be
	// without an operator's own intervention (`step resolve` on it, or on the
	// routing predecessor before it records). Scoped to CondUnrouted alone —
	// not CondPredecessors or CondGateOpen, both ordinary "not yet" waits that
	// resolve themselves as the run progresses and would turn every ordinary
	// empty offer into a false alarm.
	UnroutedReason string
}

// NextSteps computes a run's ready steps, reaping expired leases on the way.
//
// THIS IS ONE OF THE TWO PLACES A READ-SHAPED VERB MAY WRITE (§6.3: "lazy lease
// reaping happens here and at claim, and nowhere else"). Every other read verb —
// `step show`, `step context`, `run status` — computes effective status and
// writes nothing, exactly as v6 established for issues.
//
// The reap and the readiness computation share ONE transaction, and in that
// order: a step reaped by this call must be offered by this call, or a
// dispatcher would have to poll twice to see work that is already available.
func (e *Engine) NextSteps(conn *sql.DB, runID int, limit int, nowMS int64) (*ReadySteps, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}

	ttls, err := loadTTLConfig(conn, runID)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning next: %w", err)
	}
	defer tx.Rollback()

	// ---- P13: the TTL's lazy auto-abandon, BEFORE the refusal is evaluated. -
	//
	// This is the one lazy path stage 6 adds, and the one engine-spec §2 names
	// explicitly ("a dispatch TTL lazily auto-abandoned by `next` (event-logged)").
	// It runs FIRST because P16 requires the same invocation to answer: a relay
	// that crashed and came back must not have to poll twice to get work, which
	// is the same reasoning that puts the reap and the readiness pass in one
	// transaction.
	//
	// P17 is the deliberate narrowing that lives in what this is NOT: `claim`
	// does not auto-abandon. Reaping is confined to `next`/`claim` because both
	// are scheduling decisions about ONE STEP; a dispatch is about a BATCH, and
	// expiring one from a single-step verb would let a claim silently unwedge a
	// run whose relay is still alive.
	if err := autoAbandonExpiredDispatchTx(tx, runID, nowMS); err != nil {
		return nil, err
	}

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}

	// ---- The reap. ---------------------------------------------------------
	//
	// An expired step returns to `pending` with its lease cleared. `attempt` is
	// NOT incremented here: the claim that died already incremented it, and the
	// next claim will increment again, so the trail counts one attempt per
	// claim for all time (claims-leases §5) — which is exactly what §9 item 4's
	// "the attempt trail is complete" asks for. What IS counted here is the
	// OUTCOME: the reap bumps `reaped_claims` (DKT-490), so the row itself says
	// this spent attempt was a silence, not a failure.
	//
	// Stage 6 moved the body into reapExpiredTx, SHARED WITH `dispatch open`,
	// because §5.2 P5 requires that verb to perform "the same lazy reap `next`
	// does" — and two scheduling verbs reaping differently is a bug with no
	// symptom until a manifest disagrees with the `next` that follows it. The
	// shared body also writes §6.4's `reap_acks` row.
	//
	// IT RUNS BEFORE THE REFUSAL, and that ordering is required by D1's own
	// stated resolution rather than being a convenience. §5.8 D1 says the way
	// out of a claimed-but-unrecorded discrepancy is that "the step's TTL
	// lapses, `next` reaps it, and the discrepancy dissolves". If the refusal
	// were evaluated first, a step whose lease had lapsed would be reported as a
	// discrepancy by the very invocation whose reap clears it — and since core's
	// default `lease.ttl.default` (15m) equals `dispatch.grace` (15m), that is
	// not an edge case but the ORDINARY path: every reaped lease would wedge its
	// run behind a refusal naming a resolution that had already happened.
	//
	// Reaping first makes the refusal report the state AFTER the engine has done
	// what it can, which is the only honest reading of "I will not answer until
	// you reconcile": what remains is genuinely the relay's to fix.
	reaped, err := reapExpiredTx(tx, sched, runID, nowMS)
	if err != nil {
		return nil, err
	}

	// ---- P24/P25/P26: the refusal. -----------------------------------------
	//
	// engine-core §5: `next` REFUSES while a dispatch is open or while
	// discrepancies exist — "relay drift stalls loudly instead of proceeding
	// around its own mess".
	//
	// IT IS A CONFLICT, NOT AN EMPTY READY SET (P26). An empty set means
	// "nothing to do"; a refusal means "I will not answer until you reconcile".
	// A relay cannot distinguish those from a zero-length list, and conflating
	// them is precisely the silent proceeding §2 forbids. The refusal is
	// returned before the readiness pass runs, so no partial answer escapes —
	// and because it returns an error, the reap above is rolled back with it.
	// That is correct: a refused call must not half-advance the run.
	if err := refuseIfUnreconciledTx(tx, sched, runID, nowMS); err != nil {
		return nil, err
	}

	// ---- J5's failure half: a completed join that missed its quorum. -------
	//
	// It runs BEFORE the readiness pass so a miss resolved here is reflected in
	// the same call's offer, for the same reason the reap is: a dispatcher
	// should not have to poll twice to see the consequence of a state this call
	// already discovered.
	if err := resolveQuorumMisses(tx, sched, nowMS); err != nil {
		return nil, err
	}

	// ---- The readiness pass. ----------------------------------------------
	//
	// Shared with `dispatch open` (readyRows, dispatch.go) rather than
	// reimplemented here — P1's one code path. Sharing it also means the
	// re-derived pass below, after a driver routes something, cannot drift
	// from this one's ordering, truncation, or staging rules.
	rows, total, ready, err := readyRows(sched, ttls, limit)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing next: %w", err)
	}

	// The offer now carries STAGED rows — previews of steps this wave's own
	// recordings will ready — and neither driver may touch one: phase 2's
	// trigger is "the first engine invocation that OBSERVES THE STEP READY"
	// (§8.1), and an action driven early would run against inputs that do not
	// exist yet. The ready prefix is read off the rendered status rather than
	// re-asking the predicate, so the drivers act on exactly the set this
	// response calls ready.
	ready = readyOnly(rows, ready)

	// ---- The vote lifecycle (gates-trust §8.1 phases 2, 4, and 5). ---------
	//
	// AFTER the commit, because the proposal machinery opens its own
	// transactions and internal/db caps the pool at one connection — calling it
	// from inside the transaction above would deadlock rather than fail.
	//
	// `next` is the driver because §8.1 phase 2's trigger is "the first engine
	// invocation that observes the step ready", and this is that invocation.
	// Nothing is scheduled and no daemon watches: the lifecycle advances when
	// somebody asks what is ready, which is the same laziness the saga's resume
	// uses.
	opened, voteRouted, err := e.driveVoteSteps(
		conn, defs, pendingVoteSteps(sched), ready, sched.holdTally, nowMS)
	if err != nil {
		return nil, err
	}
	// The proposal a phase-2 open just created, onto the rows this call already
	// rendered — the scheduler's snapshot predates it by construction. Keyed by
	// (issue, instance), not instance alone (DKT-65): two issues' vote steps
	// can render the identical instance string. Superseded below when a driver
	// routed something and the whole snapshot is re-derived — this patch is
	// what a plain phase-2 open (no resolution yet, so no recompute) leaves in
	// place.
	for i := range rows {
		key := voteStepKey{Issue: rows[i].Issue, Instance: rows[i].Instance}
		if id := opened[key]; id != 0 && rows[i].Proposal == "" {
			rows[i].Proposal = model.FormatProposalID(id)
		}
	}

	// ---- The action lifecycle (§6.15 as amended). ----------------
	//
	// The SAME laziness, for the same reason, in the same slot: an action step
	// is the engine's own work, and `next` is the invocation that observes it
	// ready. A dispatcher never claims one — `claim` refuses — so if `next` did
	// not drive them, every run in the fixture would stop dead at `reconcile`
	// with nothing in the system able to move it.
	actionRouted, err := e.driveActionSteps(conn, ready, nowMS)
	if err != nil {
		return nil, err
	}

	// ---- DKT-55: read-your-writes. -----------------------------------------
	//
	// The rows above are the readiness pass's OWN committed snapshot, taken
	// before either driver ran (the pool-of-one ordering the vote-lifecycle
	// comment explains). A driver that actually routed something — a tally
	// resolved, an action ran — leaves that snapshot stale: it can still say
	// `ready` for a step this same call just decided, and it cannot show a
	// step that only became ready because a routed hold un-defered its
	// aggregate. RUN-4 measured the consequence: a relay misread a decided
	// gate as undecided and a strict conductor improvised a dispatch to
	// "force" routing that had already happened.
	//
	// Gated on the drivers' own report of whether they routed anything, so the
	// ordinary call — nothing for either driver to advance — still pays
	// exactly the one scheduler pass above. The connection-pool cap of 1 rules
	// out a second pass on EVERY call, not on the routed ones, which is the
	// distinction this branch exists to make.
	loopHeld := sched.LoopHoldReason()
	budgetHeld := sched.BudgetHoldReason()
	unroutedHeld := sched.UnroutedHoldReason()
	if voteRouted || actionRouted {
		fresh, err := readySnapshot(conn, runID, defs, nowMS)
		if err != nil {
			return nil, err
		}
		rows, total, _, err = readyRows(fresh, ttls, limit)
		if err != nil {
			return nil, err
		}
		// The re-derived snapshot's own loop and budget holds ride back with
		// it — the first pass's are stale for the same reason its rows were
		// (DKT-61, DKT-242, DKT-470).
		loopHeld = fresh.LoopHoldReason()
		budgetHeld = fresh.BudgetHoldReason()
		unroutedHeld = fresh.UnroutedHoldReason()
	}

	// ---- DKT-1282: resolve executor/voter rows against the run's pinned
	// policy.toml. AFTER the commit and the possible re-derivation above, so
	// this reads the FINAL row set — a row the re-derivation added or dropped
	// must not be resolved against, or left un-resolved from, a stale pass.
	policy, err := policyForRun(conn, runID)
	if err != nil {
		return nil, err
	}
	if err := resolveRowPolicy(policy, rows); err != nil {
		return nil, err
	}

	// §6.3's closing paragraph: the refusal reports `CondHeadroom`, and `next`
	// ADDITIONALLY names the unacknowledged reaps, because a headroom denial
	// with nothing running is otherwise baffling. The rows come from the same
	// snapshot the predicate counted, so the message cannot name a different set
	// of reaps from the one that caused the denial.
	return &ReadySteps{
		Steps: rows, Total: total, Reaped: reaped,
		HeldReason:       ReapHoldReason(sched.UnacknowledgedReaps()),
		LoopHeldReason:   loopHeld,
		BudgetHeldReason: budgetHeld,
		UnroutedReason:   unroutedHeld,
	}, nil
}

// driveActionSteps runs every ready action step engine-side, and reports
// whether it actually ran one — the DKT-55 signal NextSteps uses to decide
// whether the rows it already rendered need re-deriving. Every action it
// drives moves that step's status away from the `ready` its row still
// claims, so any run through the loop body makes the prior snapshot stale.
//
// A step already in the saga is left to the ordinary resume — RunActionStep is
// idempotent, but skipping the call keeps a `next` over a held step from
// re-reading a saga that H9 already decided will not advance. Skipped
// entirely, it reports no route: nothing this call did makes the caller's
// rows stale.
//
// An error here STOPS the loop and fails the call, exactly as the vote driver's
// does. It is not a step failure being swallowed: B3 already routes an action
// that cannot run per its `on_fail`, so anything reaching here is an engine
// fault — a database the next step would hit too — and reporting it is more
// useful than continuing over it.
func (e *Engine) driveActionSteps(conn *sql.DB, ready []*db.Step, nowMS int64) (bool, error) {
	routed := false
	for _, step := range ready {
		if step.Kind != workflow.ClassAction || step.InSaga() {
			continue
		}
		if err := e.RunActionStep(conn, step.ID, nowMS); err != nil {
			return routed, err
		}
		routed = true
	}
	return routed, nil
}

// driveVoteSteps advances vote steps through §8.1's lifecycle.
//
// Phase 2 opens a proposal for a step that has none; phase 4 reads the outcome
// once the proposal leaves `open`; phase 5 routes on it. A step whose proposal
// is still open is left alone — that is phase 3, waiting on voters, and nothing
// in core casts for them (§8.4: "No auto-casting").
//
// THE TWO HALVES FOLLOW DIFFERENT SETS (DKT-468). Phase 2 follows the OFFER:
// a ballot opens when the step's turn has come, so it runs only for steps in
// `ready`. Phases 4 and 5 run for EVERY pending vote step of the snapshot —
// `candidates` — whether or not the admission clauses would offer it right
// now. Routing a decided tally spawns nothing and spends nothing, so the
// clauses that guard executor work (scope, class headroom incl. reap holds,
// budget) have no business deferring it — AwaitingDecision states the same
// principle for gates. Gating phases 4/5 on full readiness let a quorum
// reached mid-wave sit unrouted for as long as any admission clause happened
// to fail at each driving opportunity: `step show` reported the children
// `pending` and the holding aggregate `gated` across a whole dispatch-close
// cycle while `vote show` already showed the proposals decided — and a run
// whose budget breached after the casts could never route them at all.
//
// It returns the proposal id now standing against each vote step's (issue,
// instance) — not instance alone (DKT-65), since a materialized held-cluster
// instance carries no issue identity and a declared step's instance repeats
// across every issue bound to the same workflow — so the call that OPENS a
// proposal can report it on the rows it already rendered. Without that the
// first `next` over a ready vote step would show a vote with no ballot to
// cast on and the second would show it — a one-poll gap in the one field a
// voter needs.
//
// It also returns whether phase 5 routed a step — the DKT-55 signal
// NextSteps uses to decide the rows it already rendered need re-deriving.
// Opening a proposal (phase 2) does not set it: the row a fresh proposal
// belongs to is still genuinely ready, and the patch above already carries
// its id. Only routing does, since it moves the step off `ready` for real
// and can un-defer an aggregate that readies a step this call never saw.
func (e *Engine) driveVoteSteps(
	conn *sql.DB, defs map[int]*workflow.Definition, candidates, ready []*db.Step,
	tally holdTally, nowMS int64,
) (map[voteStepKey]int, bool, error) {
	openable := make(map[int]bool, len(ready))
	for _, step := range ready {
		openable[step.ID] = true
	}
	opened := map[voteStepKey]int{}
	routed := false
	for _, step := range candidates {
		if step.Kind != workflow.TypeVote {
			continue
		}
		spec := materializedSpec(defs[step.WorkflowID], step, tally)
		if spec == nil || spec.Type != workflow.TypeVote {
			continue
		}
		key := voteStepKeyOf(step)

		proposalID, err := findVoteProposal(conn, step)
		if err != nil {
			return nil, false, err
		}
		if proposalID == 0 {
			// Phase 2 follows the offer: a step the admission clauses are not
			// offering has not had its turn, and opening its ballot early
			// would seat a panel on a question the scheduler has not asked.
			if !openable[step.ID] {
				continue
			}
			// Phase 2, exactly once: the idempotency key makes a concurrent
			// second invocation return the same proposal rather than open a
			// second one.
			id, err := OpenVoteProposal(conn, step, spec, nowMS)
			if err != nil {
				return nil, false, err
			}
			opened[key] = id
			continue
		}
		opened[key] = proposalID

		// Phase 4: read the outcome. Empty verdict means still open.
		outcome, err := ReadVoteOutcome(conn, step, spec)
		if err != nil {
			return nil, false, err
		}
		if outcome == nil || outcome.Verdict == "" {
			continue
		}
		if err := routeVoteStep(conn, step, defs[step.WorkflowID], spec, outcome, nowMS); err != nil {
			return nil, false, err
		}
		routed = true

		// A MATERIALIZED held step that has just been decided un-defers the
		// aggregate it gates, exactly as `approve` does (decideMaterializedStep
		// ends the same way, and for the same reason). Nothing else resumes a
		// saga parked at `held`: the stage is "NO ADVANCE until the question is
		// answered", and this call is where the answer arrived. Without it a
		// tallied hold would be decided and the run would sit gated on it
		// forever.
		//
		// A SEPARATE transaction from routeVoteStep's, deliberately, for
		// decideMaterializedStep's stated reason: the routing stage owns its own
		// four-table commit, and a crash between the two leaves a resolved
		// question and an unrouted step, which the next invocation finishes.
		if !step.Materialized {
			continue
		}
		routingStep, err := routingStepOf(conn, step)
		if err != nil {
			return nil, false, err
		}
		if err := e.ResumeSaga(conn, routingStep.ID, nowMS); err != nil {
			return nil, false, err
		}
	}
	return opened, routed, nil
}

// pendingVoteSteps is driveVoteSteps' phase-4/5 candidate set: every vote step
// of the snapshot still `pending`, whatever the admission clauses say about it
// (DKT-468 — see driveVoteSteps' own comment for why the set is wider than the
// offer).
//
// Empty when the run is not active: pause means pause, and widening the sweep
// past the offer must not start routing decided tallies on a run an operator
// parked — the same R1 every driving opportunity respected before.
func pendingVoteSteps(sched *Scheduler) []*db.Step {
	if sched.Run() == nil || sched.Run().Status != model.RunActive {
		return nil
	}
	var out []*db.Step
	for _, step := range sched.Steps() {
		if step.Kind == workflow.TypeVote && step.Status == db.StepPending {
			out = append(out, step)
		}
	}
	return out
}

// stepRow renders one step into §11.4's `next row` shape.
//
// The step's rendered STATUS is `ready`, computed — never read from the column,
// which says `pending` (§6.2: "`ready` is computed, never stored as intent").
func stepRow(sched *Scheduler, step *db.Step, ttls ttlConfig) (model.StepRow, error) {
	metadata, err := decodeMetadata(step.Metadata)
	if err != nil {
		return model.StepRow{}, err
	}

	// The vote facts, on vote steps only (model.StepRow.Voters/Proposal). Both
	// come from the snapshot the scheduler already loaded, so rendering a set of
	// rows issues no query per row.
	var (
		voters   []string
		proposal string
	)
	if step.Kind == workflow.TypeVote {
		if spec := materializedSpec(
			sched.defs[step.WorkflowID], step, sched.holdTally); spec != nil {
			voters = spec.Voters
		}
		if id := sched.voteProposals[voteStepKeyOf(step)]; id != 0 {
			proposal = model.FormatProposalID(id)
		}
	}

	return model.StepRow{
		Step:     model.FormatStepID(step.ID),
		Instance: step.Instance,
		Issue:    model.FormatID(step.IssueID),
		Run:      model.FormatRunID(step.RunID),
		Kind:     step.Kind,
		Voters:   voters,
		Proposal: proposal,
		// Human and vote steps carry no executor and the field is omitted for
		// them, so a dispatcher that spawns on every row cannot spawn a worker
		// for a gate by reading an empty string as a hint (§6.15).
		Executor: step.Executor,
		Class:    step.Class,
		// The issue's frozen labels, so a dispatcher can apply label-keyed
		// routing policy BEFORE it spawns. Without them every such rule fell
		// through to its default silently (see model.StepRow.Labels).
		Labels:  sched.LabelsFor(step.IssueID),
		Attempt: step.Attempt,
		// The outcome breakdown rides beside the count it explains (DKT-490):
		// a dispatcher deciding whether to escalate reads how many of those
		// attempts FAILED from the row, instead of guessing it from `attempt`
		// — which also counts claims that were merely reaped.
		FailedAttempts: step.FailedAttempts,
		ReapedClaims:   step.ReapedClaims,
		// The last claim's own ending (DKT-1279), beside the tally above: a
		// router deciding how to treat THIS re-offer needs to know whether
		// the attempt it follows was reaped or failed, not how many of each
		// this step has ever had.
		PriorAttemptEnd: step.LastClaimEnd,
		ExpectedCost:    step.ExpectedCost,
		LeaseTTLS:       int(ttls.forClass(sched.Limit(step.Class), step.Class).Seconds()),
		Status:          db.StepReady,
		Metadata:        metadata,
	}, nil
}

// StepRowFor renders one step's `next row` at its EFFECTIVE status, for the
// read verbs (`step show`) and for the claim response's `context.step`. It
// writes nothing.
func StepRowFor(sched *Scheduler, step *db.Step, ttls ttlConfig) (model.StepRow, error) {
	row, err := stepRow(sched, step, ttls)
	if err != nil {
		return model.StepRow{}, err
	}
	row.Status = EffectiveStatus(sched, step)
	row.BlockedReason = BlockedReason(sched, step)
	// The DKT-489 label, from the SAME predicate `next`/`claim` reap on — so
	// the flag marks exactly the rows whose rendered status is the reap's
	// future answer while the stored claim, which `run repin`'s quiescence
	// guard counts as mid-flight, has not been reaped yet.
	row.LeaseExpired = sched.Expired(step)
	return row, nil
}

// StepListEntry is one row of `step list --run`: the identity, effective
// status, and cost a budget projection reads — an INVENTORY row, not an
// offer. model.StepRow is the dispatch wire shape (executor hints, lease
// TTLs, packet identity) and would overpromise here.
type StepListEntry struct {
	Step string `json:"step"`
	// Run names the row's run. A run-scoped listing repeats one value, but an
	// issue-scoped one spans runs, and without it two rounds of the same
	// instance are indistinguishable (DKT-244).
	Run      string `json:"run"`
	Instance string `json:"instance"`
	Issue    string `json:"issue"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	// BlockedReason is StepRow's field of the same name (DKT-470), on the
	// inventory row too: `step list` is where an operator scanning a whole
	// run for a stall notices one `pending` row sitting among steps that have
	// long since finished.
	BlockedReason string `json:"blocked_reason,omitempty"`
	// LeaseExpired is StepRow's field of the same name (DKT-489), on the
	// inventory row too: `step list` is where a caller reconciling this
	// listing against a `run repin` CONFLICT sees WHICH `ready` rows still
	// carry an unreaped claim the repin is refusing over.
	LeaseExpired bool `json:"lease_expired,omitempty"`
	Attempt      int  `json:"attempt"`
	// FailedAttempts / ReapedClaims are StepRow's fields of the same name
	// (DKT-490), on the inventory row too: `step list` is where an operator
	// scanning a run asks "why is this step on attempt 3", and the breakdown
	// is the answer — how many of those claims failed outright vs were reaped
	// with nothing measured.
	FailedAttempts int `json:"failed_attempts,omitempty"`
	ReapedClaims   int `json:"reaped_claims,omitempty"`
	// PriorAttemptEnd is StepRow's field of the same name (DKT-1279), on the
	// inventory row too: how the step's MOST RECENT claim ended, "reaped" or
	// "failed", never a tally the breakdown above already gives.
	PriorAttemptEnd string  `json:"prior_attempt_end,omitempty"`
	ExpectedCost    float64 `json:"expected_cost"`
}

// RunStepList answers `docket step list --run RUN-N`: every step of one run,
// with its EFFECTIVE status (§6.2) and expected cost, in the (issue, id)
// order ListRunSteps defines.
//
// It exists because no read verb enumerated a run's steps (DKT-54): `run
// status` rolls statuses up to counts, `next` offers ready rows only, and
// step ids are one store-wide sequence shared across projects — so a
// conductor building the budget projection its contract demands (done spend
// plus pending expected costs against the cap) could only guess contiguous
// ids, correct exactly as long as the store was quiet. READ-ONLY: the
// scheduler loads in a transaction that is always rolled back, and no reap
// runs.
func RunStepList(conn *sql.DB, runID int, nowMS int64) ([]StepListEntry, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("listing %s's steps: %w", model.FormatRunID(runID), err)
	}
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	steps := sched.Steps()
	out := make([]StepListEntry, 0, len(steps))
	for _, step := range steps {
		out = append(out, StepListEntry{
			Step:            model.FormatStepID(step.ID),
			Run:             model.FormatRunID(runID),
			Instance:        step.Instance,
			Issue:           model.FormatID(step.IssueID),
			Kind:            step.Kind,
			Status:          EffectiveStatus(sched, step),
			BlockedReason:   BlockedReason(sched, step),
			LeaseExpired:    sched.Expired(step),
			Attempt:         step.Attempt,
			FailedAttempts:  step.FailedAttempts,
			ReapedClaims:    step.ReapedClaims,
			PriorAttemptEnd: step.LastClaimEnd,
			ExpectedCost:    step.ExpectedCost,
		})
	}
	return out, nil
}

// IssueStepList answers `docket step list --issue ISSUE-N`: every step recorded
// for one issue, across every run that holds one, with the same effective
// status RunStepList computes.
//
// It exists because the issue, not the run, is what a conductor has in hand
// when it asks "where did this get to" — reaching for --issue and finding only
// --run meant paging a whole run's listing through an external filter, and
// guessing at --run/--issue combinations that did not exist (DKT-244). Each
// run is scheduled on its own, because readiness is a question about one run's
// graph; the rows are then concatenated in run order. READ-ONLY, like
// RunStepList.
func IssueStepList(conn *sql.DB, issueID int, nowMS int64) ([]StepListEntry, error) {
	runIDs, err := db.IssueStepRuns(conn, issueID)
	if err != nil {
		return nil, err
	}
	issue := model.FormatID(issueID)
	out := make([]StepListEntry, 0)
	for _, runID := range runIDs {
		rows, err := RunStepList(conn, runID, nowMS)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.Issue == issue {
				out = append(out, row)
			}
		}
	}
	return out, nil
}

// EffectiveStatusCounts rolls one run's steps up by EFFECTIVE status (§6.2),
// for `run status` — the same status LoadStepView and RunStepList report, so
// the rollup can never contradict `step show`/`step list` (DKT-468).
//
// It exists because the rollup used to GROUP BY the raw column while the
// verb's own contract said "this verb computes effective status": a claimed
// step whose lease had lapsed counted as `claimed` here and read as `ready`
// in `step show`, and an operator holding both outputs at the same moment had
// no way to tell which surface was lying. Neither was — they answered
// different questions — but two answers to "what is this step's status" is
// exactly the divergence the effective-status discipline exists to prevent.
// READ-ONLY, like every §6.2 computation: the transaction is always rolled
// back and no reap runs.
func EffectiveStatusCounts(conn *sql.DB, runID int, nowMS int64) ([]model.StatusCount, error) {
	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return nil, err
	}
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf(
			"counting %s's step statuses: %w", model.FormatRunID(runID), err)
	}
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, runID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	for _, step := range sched.Steps() {
		counts[EffectiveStatus(sched, step)]++
	}
	out := make([]model.StatusCount, 0, len(counts))
	for status, count := range counts {
		out = append(out, model.StatusCount{Status: status, Count: count})
	}
	// The same stable order db.StepStatusCounts renders, so the only change a
	// consumer sees is the statuses being effective.
	sort.Slice(out, func(i, j int) bool { return out[i].Status < out[j].Status })
	return out, nil
}

// EffectiveStatus is §6.2's computed status, for any read verb.
//
// A `pending` step reads as `ready` when the §6.3 predicate holds. A claimed
// step whose lease has LAPSED reads as `pending` — the v6 discipline, computed
// at read and never written back — so "status never lies because nobody called
// next" is true for steps as it is for leases. NOTHING HERE WRITES.
func EffectiveStatus(sched *Scheduler, step *db.Step) string {
	if sched != nil && sched.Expired(step) {
		// The lease is gone as far as any reader is concerned, even though the
		// row still carries the stale owner until someone claims or `next`
		// reaps it.
		if ok, _ := sched.readyIfPending(step); ok {
			return db.StepReady
		}
		return db.StepPending
	}
	if step.Status != db.StepPending {
		return step.Status
	}
	if sched != nil {
		if ok, _ := sched.Ready(step); ok {
			return db.StepReady
		}
	}
	return db.StepPending
}

// BlockedReason names WHY a step is not `ready`, "" for any step EffectiveStatus
// does not render `pending` (DKT-470's second fix).
//
// EffectiveStatus already asks the §6.3 predicate and keeps only the bool;
// this asks the same question over the same snapshot and keeps the
// ReadyCondition instead — the two cannot disagree because they walk the
// identical branches. Without it, an operator staring at `step show` on a
// step whose interposed routing had already been decided AGAINST it (its
// threshold's routing step recorded a different routing, permanently) saw
// the same bare `pending` a step one predecessor away from ready shows, with
// no way to tell "not yet" from "not ever" short of reading the event log.
func BlockedReason(sched *Scheduler, step *db.Step) string {
	if sched == nil {
		return ""
	}
	if sched.Expired(step) {
		if ok, cond := sched.readyIfPending(step); !ok {
			return string(cond)
		}
		return ""
	}
	if step.Status != db.StepPending {
		return ""
	}
	if ok, cond := sched.Ready(step); !ok {
		return string(cond)
	}
	return ""
}

// readyIfPending answers the predicate as though the step were already reaped —
// the question a READ verb must ask about an expired claim, since it may not
// perform the reap that would make the answer true for real.
func (s *Scheduler) readyIfPending(step *db.Step) (bool, ReadyCondition) {
	probe := *step
	probe.Status = db.StepPending
	probe.Owner, probe.TokenHash, probe.ExpiresMS = "", "", 0
	return s.Ready(&probe)
}

func decodeMetadata(stored string) (map[string]any, error) {
	if stored == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stored), &out); err != nil {
		return nil, fmt.Errorf("reading step metadata: %w", err)
	}
	return out, nil
}

// ttlConfig is the `docket config` half of §6.4's TTL resolution: the
// per-class keys and the default, read once per verb rather than per row.
type ttlConfig struct {
	perClass map[string]time.Duration
	fallback time.Duration
}

// forClass resolves the lease TTL for a step, in §6.4's stated precedence:
//
//  1. the workflow's `[limits] <class>.lease_ttl`
//  2. `docket config lease.ttl.<class>`
//  3. `docket config lease.ttl.default`
//
// The workflow wins because it is the PINNED artifact: a run reproduces the TTL
// its definition declared, not whatever the repo's config says today.
func (t ttlConfig) forClass(limit workflow.Limit, class string) time.Duration {
	if limit.LeaseTTL != "" {
		if d, err := time.ParseDuration(limit.LeaseTTL); err == nil && d > 0 {
			return d
		}
	}
	if d, ok := t.perClass[class]; ok && d > 0 {
		return d
	}
	return t.fallback
}

// loadTTLConfig reads the configured TTLs for every class a run's steps carry.
//
// It resolves through db.LeaseTTL, which already implements the
// `lease.ttl.<class>` -> `lease.ttl.default` fallback v6 established — so the
// config half of §6.4's precedence cannot drift from what `issue claim` does.
//
// The per-class values are read ONCE per verb rather than per row: a run with
// four `review` siblings would otherwise issue four identical config lookups,
// and the values cannot change mid-call anyway.
func loadTTLConfig(conn *sql.DB, runID int) (ttlConfig, error) {
	// The RUN's project scopes the TTL policy (v12): the same class can carry
	// different TTLs in different projects, and a verb operating on another
	// project's run must apply that project's numbers.
	projectID, err := db.RunProjectID(conn, runID)
	if err != nil {
		return ttlConfig{}, err
	}
	fallback, err := db.LeaseTTL(conn, projectID, "")
	if err != nil {
		return ttlConfig{}, fmt.Errorf("reading the default lease TTL: %w", err)
	}

	rows, err := conn.Query(
		`SELECT DISTINCT class FROM steps WHERE run_id = ? AND class IS NOT NULL`, runID)
	if err != nil {
		return ttlConfig{}, fmt.Errorf("reading step classes: %w", err)
	}
	defer rows.Close()

	perClass := make(map[string]time.Duration)
	var classes []string
	for rows.Next() {
		var class string
		if err := rows.Scan(&class); err != nil {
			return ttlConfig{}, fmt.Errorf("reading step class: %w", err)
		}
		if class != "" {
			classes = append(classes, class)
		}
	}
	if err := rows.Err(); err != nil {
		return ttlConfig{}, fmt.Errorf("reading step classes: %w", err)
	}

	for _, class := range classes {
		ttl, err := db.LeaseTTL(conn, projectID, class)
		if err != nil {
			return ttlConfig{}, fmt.Errorf("reading the lease TTL for class %q: %w", class, err)
		}
		perClass[class] = ttl
	}

	return ttlConfig{perClass: perClass, fallback: fallback}, nil
}
