package engine

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `step claim` — TDD §6.6.

// ClaimResult is §11.4's `claim response`, field for field:
//
//	{ step, token, lease_expires_ms, context }
//
// THE SUBJECT KEY IS `step`, exactly as §11.4 specifies, and `context` is the
// full bundle. That is what closes the deviation (§6.4.1): S2 landed claims on issues,
// where the subject key is `issue` and no context bundle is defined, and filed
// the nominal deviation. This stage implements the spec's shape verbatim at the
// level it was written for, so the deviation is CLOSED by demonstration rather
// than amended.
type ClaimResult struct {
	Step           string   `json:"step"`
	Token          string   `json:"token"`
	LeaseExpiresMS int64    `json:"lease_expires_ms"`
	Context        *Context `json:"context"`

	// Attempt and RowVersion surface under --json=v2 only, per
	// reliability-delta §6.3: a claim is a mutation and advances the version.
	Attempt    int `json:"-"`
	RowVersion int `json:"-"`
}

// ClaimOptions are `step claim`'s inputs beyond the step itself.
type ClaimOptions struct {
	Owner string
	// TTLOverride is an explicit `--ttl`. Zero means resolve from the
	// workflow's [limits] then config, per §6.4's precedence.
	TTLOverride int64
	// Metadata is `--metadata`, the opaque KV bag the DISPATCHER knows at
	// claim time, merged onto the step's own in the claim's own transaction
	// (docs/tdd/completion-metadata.md §1.7, DKT-592).
	//
	// It exists because the facts a dispatcher knows when it hands a step out
	// are exactly the facts a step that never completes would otherwise never
	// record. Deferring them to `complete` means the steps that failed or
	// crashed — the ones an operator most wants to characterize — are the only
	// steps with no bag at all, and a rollup over those keys goes blind at
	// precisely the rows that motivated it.
	//
	// Core reads no key here, as everywhere: the bag is stored and never
	// interpreted.
	Metadata string
	// CostMultiplier is `--cost-multiplier`: the DISPATCHER's declaration that
	// this claim will cost this many times the step's declared `expected_cost`
	// (DKT-867). Zero means unset — the claim accrues the declared cost,
	// byte-for-byte the pre-DKT-867 behavior.
	//
	// It exists because `expected_cost` is variant-blind: the definition
	// declares one number per step template, and the party that resolves a
	// step to an executor variant — routing an escalated retry to something
	// far pricier — is the dispatcher, at claim time. The multiplier is that
	// resolution priced in the cap's own currency, declared by the only party
	// in a position to know it. Core stores, sums, and reserves the number and
	// never interprets it (genericity.md): what varied and by how much is the
	// dispatcher's business, exactly as `--usage`'s units are the claimant's.
	//
	// The scaled cost participates in the SAME facts the declared cost always
	// has: it is checked against the cap in the claim's own transaction and
	// recorded in the claim's `step-claimed` event, so each attempt of a step
	// accrues what ITS claim declared — an escalated re-claim accrues more
	// than the cheap first attempt did (B9's re-accrual, made variant-aware).
	CostMultiplier float64
	NowMS          int64
}

// ClaimStep takes a lease on a step and returns the token AND the context
// bundle in ONE response — "one atomic mediation: an unclaimed executor has
// nothing, a claimed one has everything" (engine-core §8).
//
// IT IS THREE PHASES AS OF STAGE 4 (gates-trust §7.6.1, recorded as M-c), and
// the reason is structural: `pre = true` gates run AT CLAIM (§11.1), a pre-gate
// is a subprocess, and engine-spec §6 forbids a subprocess inside a
// transaction. So:
//
//	1  transaction A  reap, readiness (R8), CAS claim, started_ms,
//	                  status -> claimed, step-claimed event        COMMITTED
//	2  no transaction pre-gates run, one at a time, each result
//	                  committing in its own small transaction
//	3  transaction B  context assembly (now including the pre-gate
//	                  results), input materialization, row-version
//	                  read, and the lease refresh (§7.6.1.1)       COMMITTED
//
// WHAT IS PRESERVED, precisely, because "claim is atomic" is a ratified
// property (engine-core §5):
//
//   - The MUTUAL-EXCLUSION guarantee is unchanged. Exactly one claimant wins,
//     and it wins in transaction A, on the same CAS. Losers still get CONFLICT.
//   - The "token and context in one response" guarantee is unchanged. The
//     CALLER still receives both in one response, which is what engine-core §8
//     is about — what the caller observes.
//
// WHAT CHANGES: a crash between phase 1 and phase 3 leaves a claimed step whose
// caller never got a response. That is ALREADY the pre-existing behavior for a
// crash between the old single commit and the response reaching the caller, and
// it is handled by the same mechanism — the lease expires and the step is
// re-offered with attempt++ (§9 item 4).
//
// A step with NO pre-gates takes the identical path with an empty phase 2, so
// the overwhelmingly common case is byte-identical to S3's behavior.
func ClaimStep(conn *sql.DB, stepID int, opts ClaimOptions) (*ClaimResult, error) {
	return claimStepWithGates(conn, stepID, opts, nil)
}

// ClaimStepWithGates is ClaimStep with a gate runner, so pre-gates execute.
//
// The runner is a PARAMETER rather than a package-level value because
// `ClaimStep` is a package-level function with no access to `e.Gates` — §7.1's
// M-c names exactly this. The nil-runner form keeps every existing caller and
// every S3 test working unchanged: with no runner, a declared pre-gate records
// `unmatched` rather than executing, which is the fail-closed direction.
func (e *Engine) ClaimStepWithGates(
	conn *sql.DB, stepID int, opts ClaimOptions,
) (*ClaimResult, error) {
	return claimStepWithGates(conn, stepID, opts, e)
}

// ClaimStepRendered is ClaimStepWithGates for `step claim --render`: the claim
// and the packet render as ONE saga (DKT-804).
//
// The render used to run strictly AFTER the claim's commit, and a render-time
// refusal — an unpinned packet file, a template that does not parse — landed on
// a step already `claimed` with no token ever delivered. RUN-56 stranded eight
// steps this way in one dispatch: the lease was held, every recovery verb
// requires the token nobody received, and the only exits were the full TTL or
// an operator-gated `step reap`.
//
// So the render is VALIDATED FIRST, pre-claim, on the same rule §1.7 places the
// metadata-bag validation before `Begin()`: a refusal must write NOTHING —
// no lease, no attempt, no event — and leave the step exactly as claimable as
// it was. The render cannot simply move inside the claim's transaction, because
// it reads through the connection while the claim's transaction would hold the
// pool's single connection (the same deadlock loadTTLConfig's placement
// avoids).
//
// The AUTHORITATIVE render then runs post-claim, so the returned packet reports
// the step as its claimant will see it — `context.step` claimed, this claim's
// `attempt`, the merged metadata bag. Every failure the preflight can catch is
// deterministic over the run's pins and the template's bytes, so a post-claim
// refusal requires the filesystem to change between two reads milliseconds
// apart; that residual race keeps today's disposition — the error surfaces and
// the lease stands — because rolling the claim back is not a rollback at all:
// the claim is COMMITTED, and the `step-claimed` event it wrote is the budget
// floor's accrual (§4.3, a SUM over those events), so un-taking the lease means
// un-writing a ledger entry the log is append-only about. What the refusal does
// instead is tell its caller how to end the lease NOW — `step reap` is the
// relay's channel for exactly this (DKT-83, DKT-820) — so the cost is one verb
// rather than the full TTL. Every deterministic failure still refuses before
// anything is written.
func (e *Engine) ClaimStepRendered(
	conn *sql.DB, stepID int, opts ClaimOptions, templatePath, executor string,
) (*ClaimResult, *RenderResult, error) {
	if _, err := RenderStepAs(conn, stepID, templatePath, executor, opts.NowMS); err != nil {
		return nil, nil, err
	}

	result, err := claimStepWithGates(conn, stepID, opts, e)
	if err != nil {
		return nil, nil, err
	}

	packet, err := RenderStepAs(conn, stepID, templatePath, executor, opts.NowMS)
	if err != nil {
		// The one refusal that can still leave a lease standing, and the
		// caller is the only party who can act on it: the token went nowhere,
		// so every token-bearing verb is closed to it, and the relay that
		// spawned this claim is exactly the party `step reap` is the channel
		// for (DKT-83). Naming the remedy here is what saves it the TTL —
		// DKT-820's RUN-59 executors had the verb and did not know it applied.
		return nil, nil, fmt.Errorf(
			"%w; the lease is held and no token was issued — run "+
				"`docket step reap %s --reason ...` to return the step to the "+
				"pool without waiting out the lease",
			err, model.FormatStepID(stepID))
	}
	return result, packet, nil
}

func claimStepWithGates(
	conn *sql.DB, stepID int, opts ClaimOptions, e *Engine,
) (*ClaimResult, error) {
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return nil, notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return nil, err
	}

	// ---- The claim bag's validation, PRE-TRANSACTION (§1.7). ---------------
	//
	// The cap and the shape check sit here, before `conn.Begin()`, for the
	// reason §6.9 places `complete`'s there: a refusal writes NOTHING and
	// leaves `row_version` where it was. On this path that property has an
	// extra edge — the transaction below performs the LAZY REAP, so a
	// validation that ran after `Begin()` would let a malformed bag consume an
	// attempt on its way to being refused.
	//
	// Same order as stage zero's, and for C5's reason: an operator debugging
	// one input must not get a different message depending on which check
	// happened to run first.
	if err := validateClaimMetadataSize(opts.Metadata); err != nil {
		return nil, err
	}
	if _, err := DecodeMetadataBag(opts.Metadata, "metadata"); err != nil {
		return nil, err
	}
	// The multiplier's validation sits with the bag's, for the same reason: a
	// refusal must write nothing, and this path's transaction performs the
	// lazy reap, so a malformed input validated any later would consume an
	// attempt on its way to being refused (DKT-867).
	if err := validateCostMultiplier(opts.CostMultiplier); err != nil {
		return nil, err
	}

	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	ttls, err := loadTTLConfig(conn, step.RunID)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning claim: %w", err)
	}
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, step.RunID, defs, opts.NowMS)
	if err != nil {
		return nil, err
	}

	// Re-read through the scheduler's snapshot so the readiness check and the
	// CAS see one consistent state.
	fresh := sched.stepByID[stepID]
	if fresh == nil {
		return nil, notFoundErr(db.ErrStepNotFound,
			"step %s not found", model.FormatStepID(stepID))
	}

	// ---- The lazy reap (§6.3: confined to next and claim). ------------------
	//
	// A step whose lease lapsed is returned to the pool HERE, so a claim
	// against an expired lease succeeds (R7) with no operator action and no
	// reaper. This is the liveness mechanism §9 item 4 rests on.
	if sched.Expired(fresh) {
		if err := db.ReapStepTx(tx, fresh.ID, opts.NowMS); err != nil {
			return nil, err
		}
		// The outcome breakdown (DKT-490): a claim reaped here spent an
		// attempt in silence, and the counter is what keeps that from reading
		// as a failure on the row this same transaction re-claims.
		if err := db.MarkStepClaimReapedTx(tx, fresh.ID, opts.NowMS); err != nil {
			return nil, err
		}
		if err := recordEvent(tx, eventRecord{
			Kind: EventLeaseReaped, RunID: fresh.RunID,
			Instance: fresh.Instance, IssueID: fresh.IssueID,
		}); err != nil {
			return nil, err
		}
		fresh.Status = db.StepPending
		fresh.Owner, fresh.TokenHash, fresh.ExpiresMS = "", "", 0
		fresh.StartedMS = nil
		fresh.ReapedClaims++
	}

	spec := materializedSpec(defs[fresh.WorkflowID], fresh, sched.holdTally)
	if spec == nil {
		return nil, validationErr(
			"step %s: %q is not a step of its pinned workflow",
			fresh.Instance, fresh.StepName)
	}

	// ---- §6.15: gates and actions are not executor work. --------------------
	//
	// A `human`, `vote`, or `action` step is OFFERED by `next --run` — a
	// dispatcher needs to know the run is waiting on a person, and needs to see
	// the engine's own steps in the feed — but is claimable by nothing. The
	// refusal NAMES the class, because a dispatcher that spawns on every `next`
	// row must be told why this one is different rather than left to infer it.
	//
	// `action` joins the branch at S5. Action steps are the engine's
	// DETERMINISTIC HALF (AC-2): the saga runs them itself, feeding the builtin
	// from the step's declared `inputs` and recording the result. A worker that
	// could claim one would be a relay copying a predecessor's payload onto it —
	// reconcile.py reborn as a claim+complete shim, which D13 exists to forbid,
	// and the refusal is what makes that unwritable rather than merely
	// discouraged.
	if fresh.Kind == workflow.TypeHuman || fresh.Kind == workflow.TypeVote ||
		fresh.Kind == workflow.ClassAction {
		return nil, conflictErr(
			"step %s is a %s step and is not claimable: it is %s",
			fresh.Instance, fresh.Kind, unclaimableReason(fresh.Kind))
	}

	// ---- The claim's EFFECTIVE cost (DKT-867). ------------------------------
	//
	// What this claim will accrue: the step's reservable cost, scaled by the
	// dispatcher's `--cost-multiplier` when one was declared. The offer (R7)
	// necessarily reserved the DECLARED cost — `next` cannot know which
	// variant a dispatcher will resolve a step to — so the claim, which is the
	// engine's resolve moment, is where the actual cost becomes knowable and
	// is therefore where it is enforced and accrued.
	claimCost := reservableCost(fresh)
	if opts.CostMultiplier > 0 {
		claimCost = fresh.ExpectedCost * opts.CostMultiplier
	}

	// ---- R8: claim enforces readiness ITSELF. -------------------------------
	//
	// It does not trust that the caller ran `next`. A dispatcher racing a scope
	// conflict would otherwise claim a step `next` would never have offered —
	// and the refusal names WHICH condition failed, so a stalled dispatcher can
	// diagnose itself instead of retrying blind.
	ready, cond := sched.Ready(fresh)
	if !ready && cond == CondBudget && sched.budgetAdmits(fresh, claimCost) {
		// The DECLARED cost would cross the cap but the cost this claim
		// actually accrues does not — a dispatcher routing to a CHEAPER
		// variant (`--cost-multiplier` under 1). Budget is the LAST clause of
		// §6.3's conjunction, so CondBudget means every other condition held,
		// and admitting on the real cost is the same arithmetic the refusal
		// below runs, answered with the honest number (DKT-867).
		ready, cond = true, ""
	}
	if !ready {
		// ---- B14/B15/B20: the budget refusal is not an ordinary one. --------
		//
		// R7 IS the claim-time check — the same arithmetic over the same
		// snapshot, which is why TestBudgetR7AndClaimAgree can assert the two
		// never disagree — but a budget refusal additionally PAUSES THE RUN, and
		// the refusal and the pause are one fact (B20).
		//
		// So this branch runs the enforcement, which writes the flip and its
		// event, and COMMITS BEFORE RETURNING. A refusal that rolled back its
		// own pause would leave a run refusing every claim while reporting
		// itself active — an operator would see a stalled run whose status says
		// nothing is wrong.
		//
		// The only other write this transaction can be holding is the lazy reap
		// above, and committing that is CORRECT rather than incidental: the
		// lease genuinely lapsed, the step genuinely returned to the pool, and
		// rolling that back because a later clause refused would make the reap
		// depend on which step happened to be claimed next.
		if cond == CondBudget {
			refusal := enforceBudgetTx(tx, sched, fresh, claimCost, opts.NowMS)
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("committing the budget pause: %w", err)
			}
			return nil, refusal
		}
		// A headroom refusal carries its arithmetic and its source, in the
		// budget refusal's shape (condition, then the numbers in parentheses).
		// DKT-23: the claimant that hears this has already paid its spawn, and
		// "no headroom" without the cap, the occupancy, and the workflow that
		// set the bound sent RUN-2's operator grepping the corpus.
		if cond == CondHeadroom {
			return nil, conflictErr(
				"step %s is not ready to claim: %s (%s)",
				fresh.Instance, cond, sched.HeadroomDetail(fresh))
		}
		return nil, conflictErr(
			"step %s is not ready to claim: %s", fresh.Instance, cond)
	}

	// ---- The ESCALATED claim's own budget check (DKT-867). ------------------
	//
	// Readiness above admitted the DECLARED cost; a dispatcher that declared a
	// multiplier above 1 is about to accrue more than that, and the cap must
	// see the real number BEFORE the accrual commits — this is precisely the
	// hop DKT-867 observed sailing past the cap: an escalated fix attempt
	// priced at the cap as though it were the cheap variant. Same arithmetic,
	// same snapshot, same pause-and-refuse contract as the CondBudget branch.
	if opts.CostMultiplier > 0 && !sched.budgetAdmits(fresh, claimCost) {
		refusal := enforceBudgetTx(tx, sched, fresh, claimCost, opts.NowMS)
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("committing the budget pause: %w", err)
		}
		return nil, refusal
	}

	// The TTL is resolved from the ALREADY-LOADED config (read before the
	// transaction opened), never by querying the pool from in here: the
	// connection pool is capped at one connection, so a pool read while this
	// transaction holds it deadlocks permanently rather than failing.
	ttlMS := opts.TTLOverride
	if ttlMS == 0 {
		ttlMS = ttls.forClass(sched.Limit(fresh.Class), fresh.Class).Milliseconds()
	}

	// ---- The CAS claim. ----------------------------------------------------
	token, lease, err := db.ClaimStepTx(tx, fresh.ID, opts.Owner, ttlMS, opts.NowMS)
	if err != nil {
		return nil, err
	}

	// `started_ms` stamps HERE, at claim, not at first heartbeat — that is what
	// makes `max_step_duration` schedule-to-close (§6.3).
	if err := db.StartStepTx(tx, fresh.ID, opts.NowMS); err != nil {
		return nil, err
	}
	if err := db.SetStepStatusTx(tx, fresh.ID, db.StepClaimed, opts.NowMS, opts.NowMS); err != nil {
		return nil, err
	}

	// ---- The dispatcher's bag lands WITH THE CLAIM (§1.7, DKT-592). --------
	//
	// In transaction A, with the CAS that awarded the claim, and before the
	// event that records the claim happened — the same ordering stage zero
	// uses for the completion bag, and for the same reason: the step's own row
	// reaches its final shape for this transition before the transition is
	// recorded as having occurred. A crash between them rolls back both.
	//
	// This is what makes the bag survive every non-completing end. The reap
	// (`ReapStepTx`) and the failure paths write status, lease and counters
	// and never touch `metadata`, so a step that crashes after this commit
	// still carries what the dispatcher knew when it handed the step out.
	//
	// It MERGES rather than assigns, on the same last-write-wins-per-key rule
	// as every other writer of this column: a definition-side bag survives, a
	// re-claim overlays the previous attempt's, and the completion bag later
	// overlays this one. `mergeMetadata` cannot fail on caller input here —
	// the shape was decoded before the transaction opened — so the only error
	// left is a serialization one.
	if opts.Metadata != "" {
		merged, err := mergeMetadata(fresh.Metadata, opts.Metadata)
		if err != nil {
			return nil, err
		}
		if err := db.SetStepMetadataTx(tx, fresh.ID, merged, opts.NowMS); err != nil {
			return nil, err
		}
		// The snapshot carries the merge forward so THIS claim's own context
		// bundle reports the bag it just recorded: a worker reading
		// `context.metadata` sees what it was dispatched with, not the state
		// before its own claim.
		fresh.Metadata = merged
	}

	// The claim event IS the accrual (§4.3), so a claim whose dispatcher
	// declared a variant scaling records the SCALED cost on the event itself
	// (DKT-867): RunFloorTx sums `data.expected_cost` when present and the
	// step row's declaration otherwise. The resulting number is recorded
	// rather than the multiplier, so the ledger entry is self-contained — what
	// this claim accrued does not change if the step row is ever different.
	// An unscaled claim writes exactly the event it always has.
	claimEvent := eventRecord{
		Kind: EventStepClaimed, RunID: fresh.RunID,
		Instance: fresh.Instance, IssueID: fresh.IssueID,
	}
	if opts.CostMultiplier > 0 {
		claimEvent.Data = fmt.Sprintf(`{"expected_cost":%g}`, claimCost)
	}
	if err := recordEvent(tx, claimEvent); err != nil {
		return nil, err
	}

	// ---- The issue mirror's other write site (DKT-294). --------------------
	//
	// `reconcileIssueAndRun` is where every ROUTING transaction crosses back
	// into issue-space; a claim never routes, so it needs its own call. Same
	// transaction as the claim itself, exactly like the event just above.
	if err := reflectIssueOnClaim(tx, fresh, opts.NowMS); err != nil {
		return nil, err
	}

	// ---- The accrual (§4.3). -----------------------------------------------
	//
	// THE EVENT ABOVE *IS* THE ACCRUAL. The floor is a SUM over `step-claimed`
	// events, so writing that event in this transaction is what makes this
	// claim's `expected_cost` count — there is no counter to increment and
	// therefore no lost update to lose (C4).
	//
	// What follows is only the REPORT'S CACHE, refreshed in the same
	// transaction so it can never be stale by more than a rollback. Nothing
	// reads it to decide anything, and TestFloorIsNeverReadFromCache proves the
	// separation by poisoning it.
	//
	// IT IS SKIPPED ENTIRELY ON AN UNBUDGETED RUN (D1). Refreshing a cache for
	// a number nothing is enforcing against would run the floor query on every
	// claim in a repo that never set a budget — which is exactly the "a
	// workflow-free repo shows no behavioral change" claim (§9 item 8) failing
	// by a query nobody asked for. The report computes the floor itself when it
	// wants it, so nothing is lost: the cache exists to save the REPORT a query
	// on a run that is already paying for one per claim.
	//
	// TestBudgetDormancyRunsNoBudgetQuery counts this, and caught it here.
	if !sched.budget.unlimited() {
		if err := cacheRunFloorTx(tx, fresh.RunID); err != nil {
			return nil, err
		}
	}

	// Reflect the claim in the snapshot so the bundle's `context.step` reports
	// the step as `claimed` — which is what it now is.
	fresh.Status = db.StepClaimed
	fresh.Attempt = lease.Attempt
	fresh.Owner, fresh.TokenHash, fresh.ExpiresMS = opts.Owner, lease.TokenHash, lease.ExpiresMS
	fresh.StartedMS = &opts.NowMS

	// ---- END OF TRANSACTION A. ---------------------------------------------
	//
	// Exclusion is decided, and decided HERE and only here. Everything after
	// this commit runs as the winning claimant.
	preGates := preClaimGates(spec)
	if len(preGates) == 0 {
		// NO PRE-GATES: the S3 path exactly. Assembly stays in the same
		// transaction, so the common case keeps the single-transaction claim
		// rather than paying for a phase split it does not need.
		bundle, err := AssembleContext(tx, sched, fresh, ttls)
		if err != nil {
			return nil, err
		}
		if err := recordStepInputs(tx, fresh.ID, bundle.Inputs); err != nil {
			return nil, err
		}
		var rowVersion int
		if err := tx.QueryRow(
			`SELECT row_version FROM steps WHERE id = ?`, fresh.ID).Scan(&rowVersion); err != nil {
			return nil, fmt.Errorf("reading step version: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("committing claim: %w", err)
		}
		return &ClaimResult{
			Step:           model.FormatStepID(fresh.ID),
			Token:          token,
			LeaseExpiresMS: lease.ExpiresMS,
			Context:        bundle,
			Attempt:        lease.Attempt,
			RowVersion:     rowVersion,
		}, nil
	}

	// DKT-9: the target-ref resolution AssembleContext will repeat in phase 3
	// runs a first time HERE, while transaction A is still open, because the
	// pre-gates spawn before any bundle exists — and they need to know which
	// tree to measure before they spawn, not after.
	//
	// BOTH halves of the target are resolved, not only the worktree (DKT-254):
	// a step with an object under review and no tree to hold it must be told
	// apart from a step with nothing under review at all, and the worktree
	// alone renders both as the empty string.
	preTargetSHA, preWorkRoot, err := preGateTarget(tx, sched, fresh, spec)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing the claim: %w", err)
	}

	// ---- PHASE 2: pre-gates, OUTSIDE any transaction. ----------------------
	//
	// Each result commits in its own small transaction, exactly as the saga's
	// gates do. §6's "no subprocess ever executes inside a transaction" is why
	// this phase exists at all.
	preResults, err := runPreGates(
		conn, e, fresh, preGates, preTargetSHA, preWorkRoot, opts.NowMS)
	if err != nil {
		return nil, err
	}

	// ---- PHASE 3: transaction B. -------------------------------------------
	txB, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning the claim's context transaction: %w", err)
	}
	defer txB.Rollback()

	// §7.6.1.1 LR1: transaction B RECOMPUTES `expires_ms` from its own commit
	// time. Phase 2 ran subprocesses — each bounded by the gate's own timeout,
	// defaulting to 5m — and ALL of that wall time would otherwise be deducted
	// from a lease the caller has not yet received. A pre-gate-heavy step would
	// hand its worker a mostly-spent lease, and with a short TTL an ALREADY
	// EXPIRED one: the worker's first `step complete` fails on a lease it never
	// had a chance to use, the step is reaped, and the pre-gates run again on
	// the re-offer — a livelock shaped like a too-short TTL but caused by
	// docket's own pre-gate phase.
	//
	// LR2: it is GUARDED BY THE CLAIM IDENTITY, not an unconditional UPDATE.
	// The claimant still holds the CAS, so this is a heartbeat by another name
	// — the mechanism the lease model already sanctions for a live claimant —
	// and not a second authorization decision. If the claim is gone (the lease
	// expired and another attempt won during phase 2), the refresh matches zero
	// rows and the claim fails in the ordinary way.
	//
	// LR4: the single-winner property is untouched. This guard can only FAIL,
	// never award a claim.
	refreshed, err := db.RefreshClaimLeaseTx(
		txB, fresh.ID, lease.TokenHash, ttlMS, opts.NowMS)
	if err != nil {
		return nil, err
	}
	if !refreshed {
		return nil, conflictErr(
			"step %s: the claim was lost while its pre-gates ran; another attempt holds it now",
			fresh.Instance)
	}
	fresh.ExpiresMS = refreshed2ExpiresMS(opts.NowMS, ttlMS)

	// LR3: NO NEW EVENT. The refresh is part of the claim, and the claim
	// already emitted `step-claimed` in transaction A. A second lifecycle event
	// on a single claim would break the event-sequence goldens for no
	// observational gain.

	bundle, err := AssembleContext(txB, sched, fresh, ttls)
	if err != nil {
		return nil, err
	}
	// §7.6.3 / amendment A5: the results ride in the bundle, present only when
	// the step declares pre-gates.
	bundle.PreGates = preResults

	if err := recordStepInputs(txB, fresh.ID, bundle.Inputs); err != nil {
		return nil, err
	}

	var rowVersion int
	if err := txB.QueryRow(
		`SELECT row_version FROM steps WHERE id = ?`, fresh.ID).Scan(&rowVersion); err != nil {
		return nil, fmt.Errorf("reading step version: %w", err)
	}

	if err := txB.Commit(); err != nil {
		return nil, fmt.Errorf("committing the claim's context: %w", err)
	}

	return &ClaimResult{
		Step:           model.FormatStepID(fresh.ID),
		Token:          token,
		LeaseExpiresMS: fresh.ExpiresMS,
		Context:        bundle,
		Attempt:        lease.Attempt,
		RowVersion:     rowVersion,
	}, nil
}

// recordStepInputs materializes the resolved input bindings. Engine-produced
// inputs (`issue.body`, and an `issue.diff` with no artifact) have no artifact
// row to bind and are skipped — there is nothing to record but the fact they
// were empty, which the bundle already says.
func recordStepInputs(tx *sql.Tx, stepID int, inputs []ContextInput) error {
	for position, in := range inputs {
		artifactID, ok := artifactIDOf(in.Artifact)
		if !ok {
			continue
		}
		if err := db.InsertStepInputTx(tx, stepID, position, artifactID); err != nil {
			return err
		}
	}
	return nil
}

// artifactIDOf parses an `ARTIFACT-N` reference back to its id.
func artifactIDOf(ref string) (int, bool) {
	var id int
	if _, err := fmt.Sscanf(ref, "ARTIFACT-%d", &id); err != nil || id < 1 {
		return 0, false
	}
	return id, true
}

// unclaimableReason completes the §6.15 refusal: what DOES advance this step,
// since a worker does not.
//
// The two halves read differently on purpose. A gate step names the VERB an
// operator types, because a person is what it is waiting for. An action step
// names no verb at all — nothing an operator can type advances it, and offering
// one would invite exactly the claim+complete relay the refusal exists to
// prevent.
func unclaimableReason(kind string) string {
	switch kind {
	case workflow.TypeHuman:
		return "resolved by `docket step approve|reject`, not by a worker"
	case workflow.ClassAction:
		return "resolved by the engine, not by a worker"
	}
	// A vote step is advanced by its VOTERS casting, through the CLI docket has
	// shipped since v2 — the engine opens the proposal, reads the tally, and
	// routes on it (§8.1). `step resolve` is what moves a run PAST one, which is
	// a different thing from what advances it, so both are named.
	return "decided by its voters casting on the linked proposal " +
		"(or moved past with `docket step resolve`), not by a worker"
}

// ReadContext re-emits a step's context bundle READ-ONLY, no token required
// (§11.4). It is `step context`'s implementation.
//
// IT WRITES NOTHING — not even a reap. §6.3 confines lazy reaping to
// `next`/`claim`, and this is neither: a read verb that reaped would make
// "I only looked at it" untrue, and would let a `--meta` query change an
// attempt counter.
func ReadContext(conn *sql.DB, stepID int, nowMS int64) (*Context, error) {
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return nil, notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return nil, err
	}

	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	ttls, err := loadTTLConfig(conn, step.RunID)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning context read: %w", err)
	}
	// Rolled back unconditionally: this path has nothing to commit, and the
	// rollback is the structural guarantee of it rather than a convention.
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, step.RunID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	fresh := sched.stepByID[stepID]
	if fresh == nil {
		return nil, notFoundErr(db.ErrStepNotFound,
			"step %s not found", model.FormatStepID(stepID))
	}

	return AssembleContext(tx, sched, fresh, ttls)
}

// StepView is one step as a READ verb sees it: the row, its effective status
// already computed, and the lease facts `step show` renders.
//
// It is a value rather than a (step, scheduler, config) triple because the
// scheduler and the TTL config are assembly machinery, and a CLI verb that held
// them could compute a DIFFERENT effective status from the one in the view.
// Handing over the answer rather than the apparatus is what keeps the read
// verbs unable to disagree with `next`.
type StepView struct {
	Step *db.Step
	Row  model.StepRow
	// Routing and SagaStage are rendered by `step show` so an operator can see
	// why an unclaimed step is not `done`: a step mid-saga is engine-owned and
	// needs no lease to advance (§6.8).
	Routing   string
	SagaStage string
	// Owner and ExpiresMS describe a LIVE lease only. A lapsed one reports
	// neither, matching the v6 `lease` object's rule that a field which is not
	// a fact does not appear — and matching the effective status, which already
	// reads the lapsed lease as gone.
	Owner     string
	ExpiresMS int64
	// Gates are the step's recorded gate results, in insertion order (DKT-63).
	//
	// `step show` printed no gate section at all, so the surface an operator
	// reaches for when a step is `waiting-human` said nothing about the gates
	// that put it there — a conductor read this verb and the event feed on
	// 2026-08-16 and reported three failed gates as passes. The results were
	// always in the table; nothing rendered them.
	Gates []db.GateResultRow
	// HeldCluster is a materialized held step's provenance: which cluster it
	// decides, out of how many, and which artifact carries it (DKT-239).
	//
	// nil on every step that is not a materialized hold, so an ordinary step's
	// view is unchanged. On a hold it is the answer `step artifacts` cannot
	// give — that verb reports what the STEP produced, and a hold produces
	// nothing; the payload sits on the routing step's artifact.
	HeldCluster *HeldClusterLink
	// TargetSHA and TargetWorktree are the SAME resolved target ref the step's
	// context bundle carries (Context.TargetSHA / TargetWorktree, DKT-24),
	// answered without assembling the bundle (DKT-1056).
	//
	// A vote panel is seated from this read: the wave asks `step show` for the
	// gate's row first and probes the (up to 1MiB) bundle only when the row
	// names a target, so a target the bundle knows and this view does not is a
	// panel reading its own HEAD instead of the judged tree.
	//
	// BOTH ARE EMPTY, NEVER APPROXIMATE, when the step's inputs resolve no
	// `issue.diff` round record. There is no fallback to the shared HEAD here
	// and there must not be one — the caller can tell "no target" from "this
	// target" only if absence is reported as absence.
	TargetSHA      string
	TargetWorktree string
}

// LoadStepView reads one step at its effective status. IT WRITES NOTHING — not
// even a reap (§6.3 confines reaping to next/claim).
func LoadStepView(conn *sql.DB, stepID int, nowMS int64) (*StepView, error) {
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return nil, notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return nil, err
	}

	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	ttls, err := loadTTLConfig(conn, step.RunID)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning step read: %w", err)
	}
	// Rolled back unconditionally: this path has nothing to commit, and the
	// rollback is the structural guarantee of that rather than a convention.
	defer tx.Rollback()

	sched, err := LoadScheduler(tx, step.RunID, defs, nowMS)
	if err != nil {
		return nil, err
	}
	fresh := sched.stepByID[stepID]
	if fresh == nil {
		return nil, notFoundErr(db.ErrStepNotFound,
			"step %s not found", model.FormatStepID(stepID))
	}

	row, err := StepRowFor(sched, fresh, ttls)
	if err != nil {
		return nil, err
	}

	// Read inside the SAME read-only transaction as everything above it: the
	// pool is capped at one connection, so a pool read from here would deadlock
	// against the transaction still open — the discipline actorCountsTx
	// documents for the report.
	gates, err := db.GateResultsForStepTx(tx, stepID)
	if err != nil {
		return nil, err
	}

	// Resolved in the SAME read-only transaction, for the reason the gates
	// read above is: the pool is capped at one connection.
	heldCluster, err := heldClusterLink(tx, sched, fresh)
	if err != nil {
		return nil, err
	}

	// Resolved in the SAME transaction, for the same one-connection reason —
	// and from the same snapshot the two reads above used, so the target this
	// view reports is the target a bundle assembled at this instant would
	// carry (DKT-1056).
	targetSHA, targetWorktree, err := stepTargetRef(tx, sched, fresh)
	if err != nil {
		return nil, err
	}

	view := &StepView{
		Step: fresh, Row: row,
		Routing: fresh.Routing, SagaStage: fresh.SagaStage,
		Gates:          gates,
		HeldCluster:    heldCluster,
		TargetSHA:      targetSHA,
		TargetWorktree: targetWorktree,
	}
	if lease := fresh.Lease(); lease.Live(nowMS) {
		view.Owner, view.ExpiresMS = lease.Owner, lease.ExpiresMS
	}
	return view, nil
}
