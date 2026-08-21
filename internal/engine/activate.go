package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/planner"
	"github.com/ALT-F4-LLC/docket/internal/trust"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// ActivateOptions are `docket run activate`'s inputs beyond the run itself.
type ActivateOptions struct {
	// FilePins are `--pin PATH` (repeatable): arbitrary operator-supplied
	// files pinned by path and content hash. engine-spec §2 is explicit that
	// this is "how the reference instance pins its contracts, fragments, and
	// policy without core knowing what they are" — so core reads bytes, hashes
	// them, stores the path, and never opens the content again except to serve
	// `context.pins`.
	FilePins []string
	// NowMS is the activation timestamp, injected so a test can pin it and
	// two activations of the same inputs produce comparable rows.
	NowMS int64
	// DryRun computes the whole activation and DISCARDS it (§7.7 S4), so an
	// operator sees what a run would bind and invoke before committing.
	DryRun bool
	// Reason is the operator's optional stated reason for activating, recorded
	// on the run-activated event's data when non-empty — matching `run budget
	// --set --reason`, `run abandon --reason`, and `step reap --reason`.
	Reason string
}

// ActivateResult reports what one activation did, for the verb to render.
type ActivateResult struct {
	Run *model.Run
	// IssuesBound is every issue in the run, bound and snapshotted.
	IssuesBound int
	// IssuesExpanded is the subset whose phase expanded THIS activation —
	// phase-1 issues at first activation, newly-unblocked phases at a
	// re-activation (RA1).
	IssuesExpanded int
	StepsCreated   int
	// ExpectedCostTotal is the run-wide SUM of the steps' expected_cost as
	// this activation leaves it, computed inside the SAME transaction — so a
	// --dry-run projects the steps it provisionally created (DKT-54's
	// activation half: cap-vs-cost in front of a panel before any step
	// exists for `step list` to enumerate).
	ExpectedCostTotal float64
	PinsRecorded      int
	FencesHarvested   int
	// PromotedIssues names, by display id, every issue stage 7 moved
	// `backlog -> todo` this activation (DKT-102/DKT-94: a count alone
	// answers "how many" and not "which ones" — an operator approving a
	// `--dry-run`'s roster needs the ids, the same way `issues_bound`'s
	// count-only shape was already the DKT-94 complaint elsewhere). Populated
	// identically on a dry run and a real activation, since promotion is
	// computed the same way on both — only a real activation commits it.
	PromotedIssues []string
	// BoundIssues names, by display id, every issue bound this activation, paired
	// with the exact workflow@version it bound to (DKT-94). This is the roster
	// `IssuesBound`'s count was missing — an operator approving a `--dry-run`
	// needs to know WHAT was bound, not just how many.
	BoundIssues []BoundIssue
	// Reactivation reports whether this was a re-activation of an already
	// `active` run, so the verb can say "expanded 2 new phases" rather than
	// implying a first activation.
	Reactivation bool
	// ContextWarnings names every step whose closure exceeded
	// `context.warn_bytes` (§5.5). The WARN cap does not refuse — only the
	// ERROR cap does — so these ride out on the result rather than as an
	// error, and the verb chooses the channel: stderr in human mode, a flag on
	// the row in JSON mode. Returning them rather than printing them here
	// keeps the engine free of an output dependency it has no other use for.
	ContextWarnings []ContextWarning
	// ScopeWarnings names every issue that declared no scope at all while
	// binding a workflow whose steps occupy the tree (lintUnscopedHolders).
	//
	// It travels here for ContextWarnings' reason and takes the same stance: the
	// condition is a planning omission rather than an illegal state, so it warns
	// and proceeds, and the verb picks the channel — stderr in human mode, an
	// array in JSON.
	ScopeWarnings []ScopeWarning
	// Registered is what auto-registration acted on, in registration order —
	// schemas first, then workflows (docs/tdd/runs-dispatch.md §9.7 F20/F21).
	//
	// F23: an activation that registered nothing leaves this NIL and the verb
	// prints no `Registered` block — not an empty one — so F17's dormancy is
	// VISIBLE IN THE OUTPUT rather than merely true underneath it.
	Registered []Registration
	// PinsFromConfig counts the files under `.docket/config/` that were pinned
	// rather than registered (F4). They are counted, not listed: a fragment tree
	// can hold hundreds of files and none of them is a decision an operator
	// needs to read before approving.
	PinsFromConfig int

	// Fences is the §7.7 trust report: every harvested fenced command and
	// whether a trust entry authorizes it (gates-trust §7.7, threat T16).
	//
	// engine-spec §2 requires activation to surface "what activation will bind
	// — including every harvested fenced command, verbatim". S3 harvested and
	// stored; this stage adds the TRUST STATUS, because a verbatim list an
	// operator cannot act on is only half the mechanism: they need to see which
	// commands will actually run before the run, not after.
	Fences []FenceReport
	// GatePreflight is DKT-255's static check: every gate the bound workflows
	// DECLARE, resolved against this repo's trust store before the run.
	//
	// The fence report above answers the same question for a HARVESTED command
	// and nothing asked it of a declared gate, so the gap was discovered
	// mid-run, one gate at a time, when the gate fired and found nothing to
	// run. All 34 gate-unmatched events of the epoch were missing entries, and
	// every one was knowable here.
	//
	// It WARNS, on the same footing as ScopeWarnings and the fence report: some
	// gates are legitimately absent on some machines, and activation is not the
	// place to make that a hard stop.
	GatePreflight []GatePreflight
	// HoldPolicy is who will answer a hold this run mints (DKT-266) — a panel
	// when both `vote.hold.*` keys are set, one operator otherwise.
	//
	// It is reported because a configured governance surface that does nothing
	// and a broken one look identical from outside, and telling them apart took
	// a source audit. The engine was doing exactly what it was told.
	HoldPolicy HoldPolicy `json:"hold_policy"`
	// DryRun reports that nothing was written, so a caller cannot mistake a
	// discarded activation for a real one.
	DryRun bool
	// ProjectedStatus and ProjectedActivatedAtMS are set ONLY on a dry run
	// (DKT-96/DKT-100/DKT-109): what `Run.Status`/`Run.ActivatedAtMS` would
	// become if this activation committed. `Run` itself renders the run AS IT
	// ACTUALLY IS — the still-committed row, read before activateTx's stage 7
	// mutated it in the discarded transaction — so a dry run can never be
	// mistaken, field-for-field, for a real activation. On a real activation
	// both are zero/nil: `Run` already carries the committed projected state,
	// so a second copy would be redundant.
	ProjectedStatus        model.RunStatus
	ProjectedActivatedAtMS *int64

	// preActivationStatus and preActivationActivatedAtMS are stage 7's
	// committed run state, snapshotted before setRunActiveTx mutates the
	// in-memory row — the "actually is" half of a dry run's rendering. Only
	// Activate's DryRun branch reads these; a real activation ignores them.
	preActivationStatus        model.RunStatus
	preActivationActivatedAtMS *int64
}

// FenceReport is one harvested command and its trust status (§7.7 S1/S2).
type FenceReport struct {
	Issue string `json:"issue"`
	Gate  string `json:"gate"`
	Tag   string `json:"tag"`
	// Ordinal is the command's position in body order, which is the order an
	// operator read it in and the order it will run in.
	Ordinal int `json:"ordinal"`
	// Command is the RAW STORED BYTES (§5.7 E4): JSON escaping is
	// encoding/json's job and the consumer is a program. Human-mode rendering
	// escapes at the print boundary instead, so the stored bytes stay exactly
	// what was harvested and hashed.
	Command string `json:"command"`
	Matched bool   `json:"matched"`
	// Entry names the trust entry that authorized it, when one did.
	Entry  string `json:"entry,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ContextWarning is one step whose closure passed `context.warn_bytes` without
// reaching `context.error_bytes` — visible before spend, per engine-core §8,
// and not yet a refusal.
type ContextWarning struct {
	Instance string `json:"instance"`
	IssueID  string `json:"issue"`
	Bytes    int    `json:"context_bytes"`
	Cap      int    `json:"warn_bytes"`
}

// ScopeWarning is one issue that binds a tree-holding workflow while declaring
// no scope at all — reported before the run rather than discovered when its
// steps race something.
type ScopeWarning struct {
	IssueID  string `json:"issue"`
	Workflow string `json:"workflow"`
	Reason   string `json:"reason"`
}

// BoundIssue names one issue this activation bound, by display id, and the
// exact workflow@version it bound to (DKT-94: `issues_bound` reported a count
// with no roster, and the only place bound-issue identity appeared at all was
// `scope_warnings`, keyed by internal numeric ids — not the display ids a
// conductor reads or writes back into its own plan). Populated identically on
// a dry run and a real activation, since binding is computed the same way on
// both — only a real activation commits it.
type BoundIssue struct {
	IssueID  string `json:"issue"`
	Workflow string `json:"workflow"`
}

// Activate is the fat transaction (engine-core §3.2, engine-spec §2; TDD §5.3).
//
// ONE transaction, seven stages in order, each failing the whole activation:
//
//  1. Bind             — exactly one registered workflow's [match] per issue
//  2. Lint the work DAG — planner.BuildDAG + TopoSort over depends_on
//  3. Pin              — the bound workflows and every --pin file
//  4. Snapshot         — issue bodies, and {title, kind, labels, scope}
//  5. Harvest fences   — declared tags only, literal, hashed
//  6. Expand phase 1   — lazily, for issues whose predecessors are satisfied
//  7. Promote and flip — backlog -> todo, run -> active, events written
//
// NOTHING EXECUTES INSIDE THIS TRANSACTION (§6: "No subprocess ever executes
// inside a transaction"). Activation runs no gate, no action, no command. It
// READS files — for pins and their hashes — which is not execution.
//
// The transaction being fat is what makes the failure modes clean: a `--pin`
// path that does not exist aborts everything, leaving no run rows, no steps,
// and no pins. Pinning is never partial, because a partially-pinned run is a
// run that cannot reproduce itself and cannot say so.
func Activate(conn *sql.DB, runID int, opts ActivateOptions) (*ActivateResult, error) {
	// The pin files are read BEFORE the transaction opens. Reading inside it
	// would hold a write lock across arbitrary filesystem latency for data the
	// transaction cannot influence — and a missing path is a refusal that
	// wants to happen before anything is locked, not a rollback.
	filePins, err := readFilePins(opts.FilePins)
	if err != nil {
		return nil, err
	}

	// AUTO-REGISTRATION'S SCAN (§9.2). It reads and hashes files, so it happens
	// out here for readFilePins' reason: holding a write lock across arbitrary
	// filesystem latency buys nothing, and a malformed file is a refusal that
	// wants to happen before anything is locked.
	//
	// F17 IS THE FIRST THING IT DOES — one `lstat` PER ROOT, and a root that is
	// absent is skipped whole. That is what makes "a repo with no config
	// directory activates exactly as before" a property of the code rather than
	// an intention, and it is also what lets a repo carry no `.docket/` at all
	// while still activating against the shared corpus.
	scan, err := scanConfigDirs(resolvePaths().InstanceConfigDirs())
	if err != nil {
		return nil, err
	}

	// Registered workflows are read outside too, for the same reason: the
	// candidate set is the whole table and it does not change under us within
	// one activation. RA2's INHERITED pins are read inside, where they must be
	// consistent with what gets written.
	//
	// IT IS READ AFTER THE SCAN but the SCAN IS APPLIED INSIDE the transaction,
	// so this list cannot yet contain what the scan will register. Binding
	// therefore re-reads the definitions inside activateTx once anything was
	// registered — see the reload there, which is what makes a workflow
	// registered by this very activation bindable by it (Z5).
	// The RUN's project scopes everything downstream — which definitions can
	// bind, which config caps apply, which registry auto-registration writes
	// into (v12). This is also the first read that can discover the run does
	// not exist, so it owns the NOT_FOUND taxonomy the tx used to apply.
	projectID, err := db.RunProjectID(conn, runID)
	if errors.Is(err, db.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}

	definitions, err := loadDefinitions(conn, projectID)
	if err != nil {
		return nil, err
	}

	caps, err := contextCaps(conn, projectID)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning activation: %w", err)
	}
	defer tx.Rollback()

	result, err := activateTx(tx, runID, definitions, filePins, caps, scan, opts)
	if err != nil {
		return nil, err
	}

	// §7.7 S4: `--dry-run` prints the same report and WRITES NOTHING, so an
	// operator can see what a run would bind and invoke before committing to
	// it. This is the "at plan approval the session surfaces" half of §2, made
	// available as a verb rather than left to the harness.
	//
	// It is implemented by ROLLING BACK the very transaction a real activation
	// would commit, rather than by a parallel read-only code path. That is the
	// whole point: a second path would drift, and a dry run that reported what
	// a DIFFERENT algorithm would do is worse than none. What the operator sees
	// is what activation actually computed.
	if opts.DryRun {
		if tally, terr := loadHoldTallyTx(tx, projectID); terr == nil {
			result.HoldPolicy = holdPolicyOf(tally)
		}

		gates, gerr := buildGatePreflightTx(tx, runID)
		if gerr != nil {
			return nil, gerr
		}
		result.GatePreflight = gates

		fences, ferr := buildFenceReportTx(tx, runID)
		if ferr != nil {
			return nil, ferr
		}
		result.Fences = fences
		result.DryRun = true

		// The run is read INSIDE the transaction, before the rollback, rather
		// than through the pool afterwards. Both would work — the rollback
		// releases the connection — but the pool is capped at ONE connection,
		// so "a pool read after Begin()" is a shape that deadlocks the moment
		// someone moves it or adds an early return above it.
		// TestNoPoolReadsInsideTransactions rejects the shape rather than the
		// instance, and it is right to: the safe version costs nothing.
		//
		// Read the run as the discarded transaction left it — the mutated,
		// about-to-be-rolled-back row — so its `status`/`activated_at_ms`
		// become the PROJECTION, not the rendered value. DKT-96/DKT-100/
		// DKT-109: a dry run that rendered this row unchanged is byte-identical
		// to a committed activation, and an operator has no field to tell them
		// apart. `Run` is reset below to the row's committed state instead —
		// captured before activateTx's stage 7 touched it — so `status` and
		// `activated_at_ms` on a dry run are ALWAYS what is actually on disk,
		// and `ProjectedStatus`/`ProjectedActivatedAtMS` carry what committing
		// would change them to (identical to the committed pair when nothing
		// would change, e.g. re-activating an already-active run).
		run, rerr := db.GetRunTx(tx, runID)
		if rerr != nil {
			return nil, fmt.Errorf("reading the run: %w", rerr)
		}
		result.ProjectedStatus = run.Status
		result.ProjectedActivatedAtMS = run.ActivatedAtMS
		run.Status = result.preActivationStatus
		run.ActivatedAtMS = result.preActivationActivatedAtMS
		result.Run = run

		if err := tx.Rollback(); err != nil {
			return nil, fmt.Errorf("discarding the dry run: %w", err)
		}
		return result, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing activation: %w", err)
	}

	// Re-read outside the transaction so the caller renders committed state.
	run, err := db.GetRun(conn, runID)
	if err != nil {
		return nil, fmt.Errorf("reading activated run: %w", err)
	}
	result.Run = run

	// §7.7 S1: the trust report, built AFTER the fat transaction commits —
	// it reads the rows that transaction wrote, and it is a report rather than
	// a gate, so a failure to build it must not undo an activation that
	// succeeded.
	// DKT-255: the same question the fence report asks of harvested commands,
	// asked of the gates the bound workflows DECLARE. Every gate-unmatched
	// event of the epoch was a missing entry, and every one was knowable here.
	if defs, derr := StepDefinitions(conn, runID); derr == nil {
		gates, gerr := BuildGatePreflight(defs, trust.Load, resolvePaths().Identity)
		if gerr != nil {
			return nil, gerr
		}
		result.GatePreflight = gates
	}

	if policy, perr := LoadHoldPolicy(conn, runID); perr == nil {
		result.HoldPolicy = policy
	}

	fences, err := BuildFenceReport(conn, runID, trust.Load, resolvePaths().Identity)
	if err != nil {
		return nil, err
	}
	result.Fences = fences

	return result, nil
}

// activateTx is the transaction body. It is separate so every stage shares one
// `tx` and an error at any stage rolls back all seven.
func activateTx(
	tx *sql.Tx,
	runID int,
	definitions []*boundDefinition,
	filePins []db.Pin,
	caps contextLimits,
	scan *configScan,
	opts ActivateOptions,
) (*ActivateResult, error) {
	run, err := db.GetRunTx(tx, runID)
	if errors.Is(err, db.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}

	// RA5: re-activating a terminal run is CONFLICT. A `done` run's topology
	// is history; a re-activation would append steps to something nothing will
	// ever schedule.
	//
	// The refusal carries the run's recorded reason (DKT-85): an operator whose
	// picture of the run is stale — the incident's activation was aimed at an
	// abandoned run a session snapshot still showed as `planning` — learns from
	// this one message not just that the run ended but why, instead of having
	// to go read the row.
	if run.Status.Terminal() {
		if run.Reason != "" {
			return nil, conflictErr(
				"run %s is %s and cannot be activated; recorded reason: %s",
				run.Ref(), run.Status, run.Reason)
		}
		return nil, conflictErr(
			"run %s is %s and cannot be activated", run.Ref(), run.Status)
	}

	// A `waiting-human` run is parked on a person's decision, and `run resume`
	// is that decision's verb. Flipping it back to `active` here would take the
	// decision as a side effect — and worse, this path would treat the run as a
	// FIRST activation (re-scanning config, bumping activated_at_ms), because
	// only `active` runs count as re-activations below. Refuse and name the
	// right verb instead (DKT-85's family: activate and the run machine
	// disagreeing about state).
	if run.Status == model.RunWaitingHuman {
		return nil, conflictErr(
			"run %s is waiting-human; resume it with `docket run resume %s` "+
				"before re-activating", run.Ref(), run.Ref())
	}

	// RA4: refused while a dispatch is open. Vacuously true at this stage —
	// dispatches are S6 — but written as a call so S6 adds a query behind
	// dispatchOpen rather than a call site here.
	open, err := dispatchOpen(tx, runID)
	if err != nil {
		return nil, err
	}
	if open {
		return nil, conflictErr(
			"run %s has an open dispatch; close or abandon it before re-activating",
			run.Ref())
	}

	reactivation := run.Status == model.RunActive
	result := &ActivateResult{Reactivation: reactivation}

	// Snapshot the run's status/activated_at_ms AS COMMITTED, before stage 7
	// (setRunActiveTx) mutates the in-memory row inside this transaction. A
	// dry run reads this back after the mutation to render `Run` as it truly
	// is and the mutation as a separate `Projected*` pair (DKT-96/DKT-100/
	// DKT-109) — a real activation never consults these, since by the time the
	// caller reads `result.Run` the mutation IS committed.
	result.preActivationStatus = run.Status
	result.preActivationActivatedAtMS = run.ActivatedAtMS

	// ---- Stage 0: AUTO-REGISTER the config directory (§9). ------------------
	//
	// F8: INSIDE THE FAT TRANSACTION, BEFORE BINDING. A registration failure
	// refuses the whole activation and writes nothing — registering-then-failing
	// would leave a repo carrying definitions from a run that never started, and
	// the NEXT activation would bind against that debris.
	//
	// F15: A RE-ACTIVATION DOES NOT RE-SCAN. RA2 says the pin set is inherited,
	// and the same reasoning reaches registration: a config file edited mid-run
	// must not reach a run already under way. If re-activation re-scanned, an
	// operator editing a workflow while a run expanded its second phase would
	// hit F9's collision refusal on a run that was working fine, and the fix
	// would be to revert their edit. Inheriting makes the edit a non-event —
	// exactly as a re-registered workflow is already a non-event (RA2).
	if !reactivation && scan != nil {
		// registration.auto (default true) gates ONLY this half. An operator
		// who turns it off is declining silent version adoption, not the
		// corpus itself — the pinned half below has no version to adopt, so it
		// keeps running regardless: a project with registration off still
		// needs the corpus's contracts/fragments/policy.toml to render a
		// step's `packet`, and the only alternative would be hand-supplying
		// every one of them via `--pin`.
		autoRegister, err := db.AutoRegisterEnabledTx(tx, run.ProjectID)
		if err != nil {
			return nil, err
		}

		if autoRegister {
			registered, err := registerScanTx(tx, run.ProjectID, scan, opts.NowMS)
			if err != nil {
				return nil, err
			}
			result.Registered = registered

			// The definitions list was read before this transaction, so it cannot
			// contain what the scan just registered. Reloading here is what makes
			// the zero-touch path work at all (§9.8 Z5): `run activate` must be able
			// to BIND a workflow that this very activation registered, and a stale
			// candidate list would report "no workflow matches this issue" over a
			// definition sitting in the same transaction.
			if len(registered) > 0 {
				definitions, err = loadDefinitionsTx(tx, run.ProjectID)
				if err != nil {
					return nil, err
				}
			}
		}

		// F4: the pinned half. These join the pin set below rather than being
		// written here, so auto-pinned and `--pin` files travel one code path
		// and RA2's inheritance covers both.
		filePins = append(filePins, scan.pins...)
		result.PinsFromConfig = len(scan.pins)
	}

	runIssues, err := db.ListRunIssuesTx(tx, runID)
	if err != nil {
		return nil, err
	}
	if len(runIssues) == 0 {
		return nil, validationErr(
			"run %s has no issues; add issues before activating it", run.Ref())
	}

	issues, err := loadIssues(tx, runIssues)
	if err != nil {
		return nil, err
	}

	// ---- Stage 1: bind. -----------------------------------------------------
	//
	// RA3: issues added since activation are bound and snapshotted now. An
	// issue already bound keeps its binding — re-binding would let a workflow
	// registered mid-run capture an issue the operator already approved into a
	// different pipeline.
	bindings := make(map[int]*boundDefinition, len(runIssues))
	for _, ri := range runIssues {
		issue := issues[ri.IssueID]
		if ri.WorkflowID != nil {
			bound := definitionByID(definitions, *ri.WorkflowID)
			if bound == nil {
				return nil, validationErr(
					"issue %s is bound to workflow id %d, which is no longer registered",
					model.FormatID(ri.IssueID), *ri.WorkflowID)
			}
			bindings[ri.IssueID] = bound
			continue
		}
		bound, err := bindIssue(issue, definitions)
		if err != nil {
			return nil, err
		}
		bindings[ri.IssueID] = bound
	}

	// Binding's lint half: an issue that holds the tree while declaring nothing
	// about what it touches.
	//
	// R4's first branch is unconditional — "a scope-less issue is never
	// excluded" (ready.go's scopeConflict, and ClaimablePrefix takes the same
	// exit) — so such an issue is mutually exclusive with NOTHING, and under
	// stage-parallel spawning its writer runs beside every other step in the
	// repository. The state is legal and occasionally intended, which is why
	// this reports rather than refuses; it is far more often an omission at
	// planning time, which is why it must not stay silent until a claim race
	// makes it visible.
	//
	// It runs over EVERY bound issue, including ones a re-activation inherited
	// and ones whose phase will not expand until later. The omission belongs to
	// the plan, not to the expansion, so a phase-2 issue is worth naming at the
	// activation that binds it rather than at the one that schedules it.
	scopeWarnings, err := lintUnscopedHolders(tx, runIssues, bindings)
	if err != nil {
		return nil, err
	}
	result.ScopeWarnings = scopeWarnings

	// The routing lint (DKT-33): an issue whose declared scope resolves
	// NOTHING under the run's repository root is almost certainly planned
	// into the wrong repository, and without this it fails only after a full
	// wave — an executor booted into a worktree that cannot contain the fix,
	// a gap filed, and the whole review fanout dispatched over the empty
	// result. Reported at authoring time instead, where re-homing costs one
	// `issue move --project`.
	unresolvable, err := lintUnresolvableScopes(tx, runIssues, bindings, run.ExecRoot)
	if err != nil {
		return nil, err
	}
	result.ScopeWarnings = append(result.ScopeWarnings, unresolvable...)

	// ---- Stage 2: lint the work DAG. ---------------------------------------
	//
	// planner.BuildDAG + TopoSort over the run's issues and their depends_on
	// relations — reused directly, no adaptation needed at this level, since
	// the graph is already over issue IDs. A cycle is a VALIDATION_ERROR with
	// the existing CycleError rendering, which already formats DKT-N.
	levels, err := lintWorkDAG(tx, issues)
	if err != nil {
		return nil, err
	}

	// ---- Stage 3: pin. ------------------------------------------------------
	//
	// RA2: the pin set is INHERITED, never recomputed. A re-activation after a
	// workflow re-register or a pinned-file edit uses the original rows,
	// unchanged — if it re-pinned, an in-flight run would silently adopt an
	// edited workflow, precisely what engine-core §4 forbids.
	existingPins, err := db.ListPinsTx(tx, runID)
	if err != nil {
		return nil, err
	}
	pinned := make(map[string]struct{}, len(existingPins))
	for _, p := range existingPins {
		pinned[p.Kind+"\x00"+p.Ref] = struct{}{}
	}

	pins := make([]db.Pin, 0, len(bindings)+len(filePins))
	for _, ri := range runIssues {
		bound := bindings[ri.IssueID]
		pins = append(pins, db.Pin{
			RunID: runID, Kind: db.PinKindWorkflow,
			Ref: bound.workflow.Ref(), SHA256: bound.workflow.SourceSHA256,
		})
	}
	// P1/P5: one pin per schema referenced by any BOUND workflow's steps. A
	// schema is a registered object, so §2's "version pinning — registered
	// objects by version" reaches it, and P4 depends on this: validation at
	// `complete` reads the PINNED bytes, never the live table, which is what
	// makes a verdict a pinned fact rather than an environmental accident.
	schemaPins, err := schemaPinsFor(tx, run.ProjectID, runID, runIssues, bindings)
	if err != nil {
		return nil, err
	}
	pins = append(pins, schemaPins...)

	for _, p := range filePins {
		p.RunID = runID
		pins = append(pins, p)
	}

	// The declared-packet index (§1.5, §1.6): path -> size, over FILE
	// pins only.
	//
	// It is built here, from the pin set this activation is about to write,
	// because that set is the authority on what the run may read. A step
	// declaring a `packet` entry the run did not pin is refused at expansion;
	// an entry that IS pinned contributes its bytes to the closure so the caps
	// bind on the real figure.
	//
	// Sizes come from the scan that already hashed the file, so no pinned file
	// is opened twice. A pin inherited from an earlier activation (RA2) carries
	// no in-memory size, so it is looked up as present-with-unknown-size: the
	// entry resolves rather than being refused, which keeps re-activation from
	// rejecting a run that was legal when it started.
	// Keyed by the path DECLARED in a workflow — relative to the config
	// directory — while a pin's ref is the path the scan walked. The two are
	// mapped here, once, so the comparison below is a plain lookup and the
	// mapping rule lives in one place rather than at every call site.
	var configRoots []string
	if scan != nil {
		configRoots = scan.roots
	}
	packetPins := make(map[string]int, len(filePins)+len(existingPins))
	indexPin := func(ref string, bytes int) {
		// v12 pins are ALREADY config-relative — the same string a `packet`
		// entry declares — so they index directly. Absolute refs are legacy
		// rows (or hand-pinned paths) and keep the original mapping through
		// the scan roots, tried in precedence order, so a run inherited from
		// before the change still resolves its entries.
		if !filepath.IsAbs(ref) {
			packetPins[filepath.ToSlash(ref)] = bytes
			return
		}
		for _, root := range configRoots {
			rel, err := filepath.Rel(root, ref)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			packetPins[filepath.ToSlash(rel)] = bytes
			return
		}
		// Not under any config root; not addressable as a packet entry.
	}
	for _, p := range existingPins {
		if p.Kind == db.PinKindFile {
			// An inherited pin (RA2) carries no in-memory size. It is indexed
			// as present-with-unknown-size so re-activation does not reject a
			// run that was legal when it started.
			indexPin(p.Ref, 0)
		}
	}
	for _, p := range filePins {
		if p.Kind == db.PinKindFile {
			indexPin(p.Ref, p.Bytes)
		}
	}

	for _, p := range pins {
		if _, ok := pinned[p.Kind+"\x00"+p.Ref]; ok {
			continue // RA2: already pinned, at whatever hash the original had.
		}
		if err := db.InsertPinTx(tx, p); err != nil {
			return nil, err
		}
		pinned[p.Kind+"\x00"+p.Ref] = struct{}{}
		result.PinsRecorded++
	}

	// ---- Stages 4-6, per issue. --------------------------------------------
	expandable := expandableIssues(levels, issues)

	for _, ri := range runIssues {
		issue := issues[ri.IssueID]
		bound := bindings[ri.IssueID]

		// Stage 4: snapshot the body AND the four remaining context.issue
		// fields (§5.1.1). Together these are the SOLE issue state the context
		// bundle ever reads, which is what §9 item 5's mid-run-edit immunity
		// rests on. A body-only snapshot fails that test on the title.
		//
		// An already-snapshotted issue is left alone: re-snapshotting at
		// re-activation would defeat the immunity for every issue already
		// under way.
		if ri.BodySnapshot == "" && ri.IssueSnapshot == "" {
			snapshot, err := issueSnapshot(tx, issue)
			if err != nil {
				return nil, err
			}
			workflowID := bound.workflow.ID
			ri.WorkflowID = &workflowID
			ri.BodySnapshot = issue.Description
			ri.BodySHA256 = workflow.SHA256([]byte(issue.Description))
			ri.IssueSnapshot = snapshot
			if err := db.BindRunIssueTx(tx, ri); err != nil {
				return nil, err
			}
			result.IssuesBound++
			result.BoundIssues = append(result.BoundIssues, BoundIssue{
				IssueID:  model.FormatID(issue.ID),
				Workflow: bound.workflow.Ref(),
			})

			// Stage 5: harvest fenced blocks from the SNAPSHOT, for tags the
			// bound workflow's gates declare. Harvesting at activation is what
			// makes "post-activation edits cannot inject" true (engine-spec
			// §4); harvesting from the snapshot rather than the live body is
			// what makes it true even for an edit that lands mid-activation.
			n, err := harvestFences(tx, runID, ri, bound.definition)
			if err != nil {
				return nil, err
			}
			result.FencesHarvested += n
		}

		// Stage 6: expand, lazily. Only phase-1 issues — those whose
		// depends_on predecessors are all satisfied — expand now; later phases
		// expand when their predecessors complete (phase 3's §6.7).
		// `expanded_at_ms` records which issues have been expanded, so
		// expansion is idempotent and re-entrant.
		if ri.Expanded() || !expandable[ri.IssueID] {
			continue
		}

		created, warnings, err := expandIssue(
			tx, run, ri, issue, bound, caps, packetPins, opts.NowMS)
		if err != nil {
			return nil, err
		}
		result.StepsCreated += created
		result.IssuesExpanded++
		result.ContextWarnings = append(result.ContextWarnings, warnings...)

		if err := db.MarkExpandedTx(tx, runID, ri.IssueID, opts.NowMS); err != nil {
			return nil, err
		}

		// Stage 7, per issue: promote via the issue verbs' own vocabulary
		// (engine-spec §2: "promotion via the issue verbs"). Only `backlog`
		// moves: an issue an operator already advanced is not walked back.
		if issue.Status == model.StatusBacklog {
			if err := promoteIssueTx(tx, issue.ID, opts.NowMS); err != nil {
				return nil, err
			}
			if err := recordEvent(tx, eventRecord{
				Kind: EventIssuePromoted, RunID: runID, IssueID: issue.ID,
				AtMS: opts.NowMS,
			}); err != nil {
				return nil, err
			}
			result.PromotedIssues = append(result.PromotedIssues, model.FormatID(issue.ID))
		}
	}

	// The run-wide expected-cost sum, read after expansion and inside the
	// transaction, so a dry run's projection includes the steps it just
	// provisionally created. See ExpectedCostTotal's own doc for why.
	if err := tx.QueryRow(
		`SELECT COALESCE(SUM(expected_cost), 0) FROM steps WHERE run_id = ?`,
		runID).Scan(&result.ExpectedCostTotal); err != nil {
		return nil, fmt.Errorf("summing expected cost: %w", err)
	}

	// ---- Stage 7: flip the run. --------------------------------------------
	if err := setRunActiveTx(tx, runID, reactivation, opts.NowMS); err != nil {
		return nil, err
	}
	activatedData := ""
	if opts.Reason != "" {
		data, err := json.Marshal(map[string]any{"reason": opts.Reason})
		if err != nil {
			return nil, fmt.Errorf("recording the run-activated event: %w", err)
		}
		activatedData = string(data)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventRunActivated, RunID: runID, Data: activatedData, AtMS: opts.NowMS,
	}); err != nil {
		return nil, err
	}

	return result, nil
}

// boundDefinition pairs a registered workflow row with its parsed definition,
// so binding compares `[match]` clauses without re-parsing TOML per issue.
type boundDefinition struct {
	workflow   *model.Workflow
	definition *workflow.Definition
}

// loadDefinitions reads every registered workflow and restores its PARSED form
// from `workflows.parsed` — never by re-parsing `body`.
//
// The parsed JSON is the PINNED INTERPRETATION (TDD §4.1): re-parsing the TOML
// here would mean a parser change silently re-interpreting a definition a run
// already pinned, which is the thing version pinning exists to prevent.
func loadDefinitions(conn *sql.DB, projectID int) ([]*boundDefinition, error) {
	workflows, _, err := db.ListWorkflows(conn, db.WorkflowListOptions{ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("reading registered workflows: %w", err)
	}

	out := make([]*boundDefinition, 0, len(workflows))
	for _, wf := range workflows {
		def, err := workflow.FromCanonical([]byte(wf.Parsed))
		if err != nil {
			return nil, fmt.Errorf("reading stored definition %s: %w", wf.Ref(), err)
		}
		out = append(out, &boundDefinition{workflow: wf, definition: def})
	}

	// A stable candidate order, so the "candidate workflows" an
	// exactly-one-match error names come out the same on every run. An error
	// message that reorders itself is one nobody can test against.
	sort.Slice(out, func(i, j int) bool {
		if out[i].workflow.Name != out[j].workflow.Name {
			return out[i].workflow.Name < out[j].workflow.Name
		}
		return out[i].workflow.Version < out[j].workflow.Version
	})
	return out, nil
}

// loadDefinitionsTx is loadDefinitions inside the fat transaction, for the one
// caller that needs to see what the SAME transaction just registered (§9's Z5).
//
// It shares the restore-from-`parsed` discipline and the candidate ordering with
// its pool-reading twin, and differs only in the handle — which is the whole
// reason it exists: internal/db caps the pool at one connection, so a pool read
// from inside a transaction deadlocks rather than merely missing the uncommitted
// rows.
func loadDefinitionsTx(tx *sql.Tx, projectID int) ([]*boundDefinition, error) {
	rows, err := tx.Query(
		`SELECT id, name, version, description, source_path, source_sha256,
		        body, parsed, created_at_ms, row_version, deprecated_at_ms
		   FROM workflows WHERE project_id = ? ORDER BY name ASC, version DESC`,
		db.DefaultProjectIDOr(projectID))
	if err != nil {
		return nil, fmt.Errorf("reading registered workflows: %w", err)
	}
	defer rows.Close()

	var out []*boundDefinition
	for rows.Next() {
		var (
			wf                      model.Workflow
			description, sourcePath sql.NullString
			deprecatedAt            sql.NullInt64
		)
		if err := rows.Scan(
			&wf.ID, &wf.Name, &wf.Version, &description, &sourcePath,
			&wf.SourceSHA256, &wf.Body, &wf.Parsed, &wf.CreatedAtMS, &wf.RowVersion,
			&deprecatedAt,
		); err != nil {
			return nil, fmt.Errorf("reading a registered workflow: %w", err)
		}
		wf.Description = description.String
		wf.SourcePath = sourcePath.String
		wf.DeprecatedAtMS = deprecatedAt.Int64

		// The PARSED form, restored — never a re-parse of `body`. The parsed
		// JSON is the PINNED INTERPRETATION (§4.1): re-parsing here would mean a
		// parser change silently re-interpreting a definition a run already
		// pinned.
		def, err := workflow.FromCanonical([]byte(wf.Parsed))
		if err != nil {
			return nil, fmt.Errorf("reading stored definition %s: %w", wf.Ref(), err)
		}
		stored := wf
		out = append(out, &boundDefinition{workflow: &stored, definition: def})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading registered workflows: %w", err)
	}

	// The SAME stable candidate order loadDefinitions produces, so an
	// exactly-one-match error names its candidates identically whichever path
	// loaded them.
	sort.Slice(out, func(i, j int) bool {
		if out[i].workflow.Name != out[j].workflow.Name {
			return out[i].workflow.Name < out[j].workflow.Name
		}
		return out[i].workflow.Version < out[j].workflow.Version
	})
	return out, nil
}

func definitionByID(definitions []*boundDefinition, id int) *boundDefinition {
	for _, d := range definitions {
		if d.workflow.ID == id {
			return d
		}
	}
	return nil
}

// bindableDefinitions reduces the registered set to the HIGHEST VERSION OF EACH
// NAME — the candidate set `[match]` is evaluated over (§11.1, as amended
// 2026-08-05).
//
// Superseded versions remain registered, and remain reachable by explicit
// `@version` and by `definitionByID` for the runs that pinned them. They merely
// stop participating in BINDING. That distinction is the whole amendment: a
// version bump is how the retro loop evolves a workflow from run evidence, and
// before this, the bump wedged the very next activation — both versions of the
// bumped name matched, and exactly-one-match refused (the M2a toy run).
//
// The resolution deliberately MIRRORS `workflow show NAME` without `@version`
// (db.GetWorkflow's `ORDER BY version DESC LIMIT 1`). Binding and show
// disagreeing about what "the" workflow of a name is was the defect; one
// helper's worth of agreement is asserted directly by
// TestBindingAgreesWithWorkflowShowResolution.
//
// Exactly-one-match therefore applies across NAMES, which is what makes its
// error actionable: two different names matching one issue is an authoring
// problem the operator must fix, whereas two versions of one name is the
// evolution path working as designed.
// RETIREMENT is applied FIRST, before the highest-version reduction.
//
// The order is the whole design. Filtering retired rows out of the candidate
// pool up front means the reduction picks the highest version that STILL
// BINDS, so retiring the top version falls back to the one beneath it rather
// than removing the name from routing entirely. Filtering afterwards would
// give the opposite and much stranger behaviour: retiring @3 would leave the
// name unroutable even though @2 is registered and binding, because @3 would
// have already won the reduction and then been dropped.
//
// Retiring EVERY version of a name removes that name from binding altogether,
// which is the retirement use case — a name registered by mistake, or one whose
// TOML was deleted and which nothing can otherwise stop from binding forever.
// Such an issue then lands on the ZERO-match branch, whose message lists only
// candidates that could actually have bound (bindIssue's `candidates`), so a
// retired name is never offered as something to go and edit.
func bindableDefinitions(definitions []*boundDefinition) []*boundDefinition {
	binding := make([]*boundDefinition, 0, len(definitions))
	for _, d := range definitions {
		if !d.workflow.Deprecated() {
			binding = append(binding, d)
		}
	}

	highest := make(map[string]*boundDefinition, len(binding))
	for _, d := range binding {
		if cur, ok := highest[d.workflow.Name]; !ok || d.workflow.Version > cur.workflow.Version {
			highest[d.workflow.Name] = d
		}
	}

	out := make([]*boundDefinition, 0, len(highest))
	for _, d := range binding {
		if highest[d.workflow.Name] == d {
			out = append(out, d)
		}
	}
	return out
}

// bindIssue is stage 1 for one issue: evaluate the `[match]` of the highest
// registered version of each workflow name and require EXACTLY ONE to match.
//
// Zero or multiple is a VALIDATION_ERROR "naming the issue and the candidate
// workflows" (§11.1, verbatim). Both halves of that naming matter and both are
// asserted by tests: the issue, so an operator knows which of twenty to fix,
// and the candidates, so they know whether to narrow a `[match]` or write one.
//
// The candidates NAMED are the bindable ones, not every registered row: an
// error that listed superseded versions would send an operator to edit a
// definition that could not have bound the issue anyway.
func bindIssue(issue *model.Issue, definitions []*boundDefinition) (*boundDefinition, error) {
	subject := workflow.Subject{Kind: string(issue.Kind), Labels: issue.Labels}
	candidates := bindableDefinitions(definitions)

	var matched []*boundDefinition
	for _, d := range candidates {
		if d.definition.Match.Matches(subject) {
			matched = append(matched, d)
		}
	}

	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		if len(candidates) == 0 {
			// "None is registered" and "every registered version is retired"
			// are different repo states and send an operator to different
			// places — register a workflow, versus restore or supersede one.
			// Reporting the second as the first would send them
			// looking for a file that already exists.
			if len(definitions) > 0 {
				return nil, validationErr(
					"issue %s matches no registered workflow: all %d registered "+
						"version(s) are deprecated, so none can bind. Restore one "+
						"with `docket workflow deprecate <name>@<version> --restore`, "+
						"or register a new version",
					model.FormatID(issue.ID), len(definitions))
			}
			return nil, validationErr(
				"issue %s matches no registered workflow: none is registered",
				model.FormatID(issue.ID))
		}
		return nil, validationErr(
			"issue %s (kind %s, labels [%s]) matches no registered workflow; "+
				"candidates considered: %s",
			model.FormatID(issue.ID), issue.Kind, strings.Join(issue.Labels, " "),
			refList(candidates))
	default:
		return nil, validationErr(
			"issue %s matches %d workflows, and exactly one must match; "+
				"candidates: %s",
			model.FormatID(issue.ID), len(matched), refList(matched))
	}
}

// refList renders candidate workflows as `name@version`, in the stable order
// loadDefinitions established.
func refList(definitions []*boundDefinition) string {
	refs := make([]string, 0, len(definitions))
	for _, d := range definitions {
		refs = append(refs, d.workflow.Ref())
	}
	return strings.Join(refs, ", ")
}

// lintUnscopedHolders reports every issue that declares NO SCOPE while binding a
// workflow that occupies the tree.
//
// WHAT "DECLARES NO SCOPE" MEANS: `scope_globs` NULL, and NOT `[]`. The two are
// different facts — db.SetIssueScopeGlobs states the distinction as "no scope
// declared" versus "declared to touch nothing" — and only the first is an
// omission. An author who wrote an empty scope decided the issue touches
// nothing, and warning about a decision someone made on purpose is how a warning
// becomes noise, which costs the cases that are real.
//
// WHAT "OCCUPIES THE TREE" MEANS: `holds_tree`, because that is the ONLY step
// property scope exclusion consults. R4 reduces to two facts and no others —
// scopeConflict returns early on an empty scope list and again on
// `!s.holdsTree(step)`, and ClaimablePrefix narrows the offer on the same pair —
// and Scheduler.holdsTree reads `workflow.Step.HoldsTree` from the pinned
// definition. Nothing else about a step reaches the exclusion.
//
// It is deliberately NOT derived from `class`. Classes are opaque strings and
// core attaches no meaning to any of them (§6.5's genericity rule; ready.go's
// classHeadroom says the same of `[limits]`), so a lint keyed on a class name
// would be instance policy living in core and would say nothing whatever about a
// repo that names its classes differently. `holds_tree` is the declaration the
// instance already makes for exactly this question.
func lintUnscopedHolders(
	tx *sql.Tx, runIssues []*db.RunIssue, bindings map[int]*boundDefinition,
) ([]ScopeWarning, error) {
	var out []ScopeWarning
	for _, ri := range runIssues {
		bound := bindings[ri.IssueID]
		if bound == nil {
			continue
		}
		holder := treeHolder(bound.definition)
		if holder == nil {
			continue
		}

		// Read LIVE, like the scheduler and unlike the snapshot (§5.1.1): the
		// question is what this issue declares now, and an operator who set a
		// scope after a first activation has fixed the omission this reports.
		scope, err := db.IssueScopeGlobsTx(tx, ri.IssueID)
		if err != nil {
			return nil, fmt.Errorf("reading scope for %s: %w",
				model.FormatID(ri.IssueID), err)
		}
		if scope != "" {
			continue
		}

		out = append(out, ScopeWarning{
			IssueID:  model.FormatID(ri.IssueID),
			Workflow: bound.workflow.Ref(),
			Reason: fmt.Sprintf(
				"no scope is declared, so its %q step holds the tree without "+
					"excluding, or being excluded by, any other issue", holder.Name),
		})
	}
	return out, nil
}

// lintUnresolvableScopes reports every issue whose declared scope resolves
// NOTHING under the run's repository root (DKT-33) — the signature of an
// issue planned into the wrong repository.
//
// THE TEST IS ANCHORED EXISTENCE, NOT FILE EXISTENCE. A scope may name files
// the work will CREATE, so requiring the paths themselves would flag every
// new-file issue; instead each entry's literal prefix (up to the first glob
// metacharacter) must exist, or its parent directory must. One resolving
// entry clears the issue — the warning fires only when EVERY entry is
// anchorless, which in the measured case (`internal/engine/...` scopes bound
// into a repository with no `internal/` at all) is exactly the wrong-repo
// shape. Shallow scopes whose parent is the root itself always anchor, so
// this lint under-reports rather than crying wolf on greenfield work.
//
// A run with no recorded exec root is skipped: there is no root to resolve
// against, and guessing from the process cwd would make the answer depend on
// where the operator happened to stand.
func lintUnresolvableScopes(
	tx *sql.Tx, runIssues []*db.RunIssue, bindings map[int]*boundDefinition,
	execRoot string,
) ([]ScopeWarning, error) {
	if execRoot == "" {
		return nil, nil
	}

	var out []ScopeWarning
	for _, ri := range runIssues {
		raw, err := db.IssueScopeGlobsTx(tx, ri.IssueID)
		if err != nil {
			return nil, fmt.Errorf("reading scope for %s: %w",
				model.FormatID(ri.IssueID), err)
		}
		if raw == "" {
			continue // No scope declared; lintUnscopedHolders owns that case.
		}
		var globs []string
		if err := json.Unmarshal([]byte(raw), &globs); err != nil || len(globs) == 0 {
			continue
		}

		anchored := false
		for _, g := range globs {
			if scopeAnchorExists(execRoot, g) {
				anchored = true
				break
			}
		}
		if anchored {
			continue
		}

		workflowRef := ""
		if bound := bindings[ri.IssueID]; bound != nil {
			workflowRef = bound.workflow.Ref()
		}
		out = append(out, ScopeWarning{
			IssueID:  model.FormatID(ri.IssueID),
			Workflow: workflowRef,
			Reason: fmt.Sprintf(
				"none of its declared scope paths resolves under %s — the issue "+
					"may belong to a different repository; re-home it with "+
					"`docket issue move --project` or fix the scope", execRoot),
		})
	}
	return out, nil
}

// scopeAnchorExists reports whether one scope entry has an ANCHOR under root:
// its literal prefix — the part before the first glob metacharacter — exists,
// or that prefix's parent directory does.
func scopeAnchorExists(root, glob string) bool {
	lit := glob
	if i := strings.IndexAny(glob, "*?["); i >= 0 {
		lit = glob[:i]
	}
	lit = strings.TrimSuffix(lit, "/")
	anchor := filepath.Join(root, filepath.FromSlash(lit))
	if _, err := os.Stat(anchor); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Dir(anchor)); err == nil {
		return true
	}
	return false
}

// treeHolder returns the first step of a definition that OCCUPIES its issue's
// scope while it runs, or nil when no step does.
//
// A nil `holds_tree` reads as TRUE, matching Scheduler.holdsTree and §11.1's
// materialized default. Registration fills the field in, so nil here means a
// definition that never passed validation — and the default exists precisely
// because the missing value must not resolve to the permissive answer.
func treeHolder(def *workflow.Definition) *workflow.Step {
	for _, step := range def.Steps {
		if step.HoldsTree == nil || *step.HoldsTree {
			return step
		}
	}
	return nil
}

// lintWorkDAG is stage 2: planner.BuildDAG + TopoSort over the run's issues and
// their depends_on relations, REUSED directly. The returned levels are what
// stage 6 reads to decide which issues are phase 1.
func lintWorkDAG(tx *sql.Tx, issues map[int]*model.Issue) ([][]int, error) {
	list := make([]*model.Issue, 0, len(issues))
	for _, issue := range issues {
		list = append(list, issue)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })

	relations, err := relationsAmongTx(tx, list)
	if err != nil {
		return nil, err
	}

	levels, err := planner.TopoSort(planner.BuildDAG(list, relations))
	if err != nil {
		var cycle *planner.CycleError
		if errors.As(err, &cycle) {
			// The existing rendering already formats DKT-N, which is the right
			// vocabulary here — unlike the workflow layer, whose cycles are
			// over step names and needed re-rendering.
			return nil, validationErr("work graph has a cycle: %s", cycle.Error())
		}
		return nil, fmt.Errorf("linting the work graph: %w", err)
	}
	return levels, nil
}

// expandableIssues reports which of the run's issues are "phase 1" — those
// whose depends_on predecessors are all satisfied at activation.
//
// An issue is expandable when every predecessor either is not in the run (an
// external dependency the run does not schedule) or is already `done`. Level 0
// of the topological sort is the base case; later levels expand when their
// predecessors complete, which is phase 3's §6.7.
func expandableIssues(levels [][]int, issues map[int]*model.Issue) map[int]bool {
	out := make(map[int]bool, len(issues))
	if len(levels) == 0 {
		return out
	}
	for _, id := range levels[0] {
		out[id] = true
	}

	// A later-level issue whose predecessors are all `done` is expandable too:
	// a run activated over a graph whose first phase was already finished by
	// hand should schedule the second, not stall.
	for level := 1; level < len(levels); level++ {
		for _, id := range levels[level] {
			if issuePredecessorsSatisfied(id, levels, level, issues) {
				out[id] = true
			}
		}
	}
	return out
}

// issuePredecessorsSatisfied reports whether every earlier-level issue this one
// could depend on is `done`. It is deliberately conservative: it treats the
// whole prefix of the topological order as the predecessor set, so an issue
// expands early only when the phases before it are genuinely complete.
func issuePredecessorsSatisfied(
	_ int, levels [][]int, level int, issues map[int]*model.Issue,
) bool {
	for earlier := 0; earlier < level; earlier++ {
		for _, id := range levels[earlier] {
			if issue, ok := issues[id]; ok && issue.Status != model.StatusDone {
				return false
			}
		}
	}
	return true
}

// issueSnapshot is stage 4's §5.1.1 half: the canonical JSON of
// {title, kind, labels, scope}, captured at activation.
//
// ONE JSON column rather than four discrete ones, because these fields are
// never queried, filtered, or joined on — they are read back whole, exactly
// once, to materialize `context.issue` — so discrete columns would buy nothing
// over a blob while costing a migration every time §11.4's issue shape grows.
//
// Canonical JSON (sorted keys, labels/scope in stored order) is REQUIRED, not
// incidental: context bundles are golden-diffed byte for byte (§8.3), so a
// map-iteration-ordered serialization here would make the goldens flap.
// encoding/json sorts struct fields by declaration and map keys by name, and
// every slice below preserves its stored order, so the output is a pure
// function of the issue.
//
// `scope` is snapshotted even though the SCHEDULER reads it live. Two different
// questions legitimately have two different answers: the scheduling check asks
// "what does this issue touch NOW", so it reads issues.scope_globs live and
// picks up an operator's mid-run correction; the context bundle asks "what was
// this issue declared to touch when the run was activated", and §9 item 5
// requires that answer frozen. Reading live for the bundle would break mid-run
// edit immunity; freezing the scheduler would ignore a correction that exists
// precisely to prevent a collision.
func issueSnapshot(tx *sql.Tx, issue *model.Issue) (string, error) {
	scopeJSON, err := db.IssueScopeGlobsTx(tx, issue.ID)
	if err != nil {
		return "", fmt.Errorf("reading scope for %s: %w", model.FormatID(issue.ID), err)
	}

	scope := []string{}
	if scopeJSON != "" {
		if err := json.Unmarshal([]byte(scopeJSON), &scope); err != nil {
			return "", fmt.Errorf(
				"reading scope for %s: stored value is not a JSON array: %w",
				model.FormatID(issue.ID), err)
		}
	}

	labels := issue.Labels
	if labels == nil {
		labels = []string{}
	}

	snapshot := struct {
		Title  string   `json:"title"`
		Kind   string   `json:"kind"`
		Labels []string `json:"labels"`
		Scope  []string `json:"scope"`
	}{
		Title:  issue.Title,
		Kind:   string(issue.Kind),
		Labels: labels,
		Scope:  scope,
	}

	out, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("serializing snapshot for %s: %w", model.FormatID(issue.ID), err)
	}
	return string(out), nil
}

// harvestFences is stage 5: extract fenced blocks from the issue's SNAPSHOT
// whose tag a bound workflow's gates declare, one row per line, each hashed.
//
// Extraction is LITERAL (engine-core §6: "no prose parsing"). Nothing is
// executed here (§5.6) — but the harvest, the snapshot, and the hash are all
// real now, which is exactly what makes S4 a pure execution addition.
func harvestFences(
	tx *sql.Tx, runID int, ri *db.RunIssue, def *workflow.Definition,
) (int, error) {
	tags := def.FenceTags()
	if len(tags) == 0 {
		return 0, nil
	}

	fences := workflow.HarvestFences(ri.BodySnapshot, tags)
	for _, f := range fences {
		err := db.InsertFenceTx(tx, db.RunFence{
			RunID: runID, IssueID: ri.IssueID,
			Tag: f.Tag, Ordinal: f.Ordinal, Command: f.Command,
			SHA256: workflow.SHA256([]byte(f.Command)),
		})
		if err != nil {
			return 0, err
		}
	}
	return len(fences), nil
}

// expandIssue is stage 6 for one issue: the pure expansion of §5.3.1, written
// to `steps`.
//
// The context-size check lives here because engine-core §8 puts it here,
// verbatim: "Oversized context bundles (config caps) are an engine error AT
// EXPANSION TIME — the fix is a pipeline/contract change, visible before
// spend." Refusing at expansion is the whole point: an oversized closure
// discovered at claim time has already cost a dispatcher a round trip, and one
// discovered at completion has cost the work.
func expandIssue(
	tx *sql.Tx,
	run *model.Run,
	ri *db.RunIssue,
	issue *model.Issue,
	bound *boundDefinition,
	caps contextLimits,
	packetPins map[string]int,
	nowMS int64,
) (int, []ContextWarning, error) {
	subject := workflow.Subject{Kind: string(issue.Kind), Labels: issue.Labels}
	rows := workflow.Expand(bound.definition, subject, 0)

	var warnings []ContextWarning

	for _, inst := range rows {
		// A declared packet entry the run did not pin is refused HERE, with the
		// activation (§1.6). The pin set is what the run may read, so an
		// unpinned entry means the run holds no snapshot of that file — reading
		// the live tree instead would break the byte-identical property the
		// whole composition rests on.
		//
		// Refusing at expansion, in the fat transaction, leaves nothing behind:
		// the same stance the context cap takes, and for the same reason. A
		// half-expanded run would offer steps whose siblings were refused.
		for _, entry := range inst.Packet {
			if _, ok := packetPins[entry]; !ok {
				return 0, nil, validationErr(
					"step %s on %s declares the packet file %q, which this run did "+
						"not pin; add it under the instance-config directory before "+
						"activating, or remove the entry",
					inst.Instance, model.FormatID(issue.ID), entry)
			}
		}

		size := workflow.ContextSize(ri.BodySnapshot, ri.IssueSnapshot, inst,
			func(path string) int { return packetPins[path] })

		// The error cap refuses the whole activation. The transaction is fat,
		// so this leaves nothing behind — which is the correct outcome for a
		// pipeline that cannot run: a half-expanded run would offer steps
		// whose siblings were refused.
		if caps.errorBytes > 0 && size > caps.errorBytes {
			return 0, nil, validationErr(
				"step %s on %s would carry a %d-byte context, over the "+
					"context.error_bytes cap of %d; reduce the issue body or the "+
					"step's inputs",
				inst.Instance, model.FormatID(issue.ID), size, caps.errorBytes)
		}
		if caps.Warn(size) {
			warnings = append(warnings, ContextWarning{
				Instance: inst.Instance, IssueID: model.FormatID(issue.ID),
				Bytes: size, Cap: caps.warnBytes,
			})
		}

		metadata, err := marshalMetadata(inst.Metadata)
		if err != nil {
			return 0, nil, err
		}

		err = db.InsertStepTx(tx, db.StepRow{
			RunID: run.ID, IssueID: issue.ID, WorkflowID: bound.workflow.ID,
			StepName: inst.Name, Ordinal: inst.Ordinal, SiblingIndex: inst.SiblingIndex,
			Instance: inst.Instance, Kind: inst.Kind, Executor: inst.Executor,
			Class: inst.Class, Status: inst.Status, MaxAttempts: inst.MaxAttempts,
			ExpectedCost: inst.ExpectedCost, Metadata: metadata, ContextBytes: size,
		}, nowMS)
		if err != nil {
			return 0, nil, err
		}

		if inst.Status == workflow.StatusSkipped {
			if err := recordEvent(tx, eventRecord{
				Kind: EventStepSkipped, RunID: run.ID,
				Instance: inst.Instance, IssueID: issue.ID,
				AtMS: nowMS,
			}); err != nil {
				return 0, nil, err
			}
		}
	}

	return len(rows), warnings, nil
}

// marshalMetadata serializes a step's opaque KV bag. The engine never reads a
// key inside it (genericity.md); it is stored as JSON text and returned
// verbatim.
func marshalMetadata(metadata map[string]any) (string, error) {
	if len(metadata) == 0 {
		return "", nil
	}
	out, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("serializing step metadata: %w", err)
	}
	return string(out), nil
}

// contextLimits are the v6 `context.warn_bytes` / `context.error_bytes` config
// keys (claims-leases §3.3). This stage is their FIRST READER — they shipped at
// v6 so the defaults were pinned before the verbs that consume them.
type contextLimits struct {
	warnBytes  int
	errorBytes int
}

// Warn reports whether a closure of `size` bytes should warn. The warning
// surfaces on stderr in human mode and as a flag on the step row in JSON mode;
// only the ERROR cap refuses.
func (c contextLimits) Warn(size int) bool {
	return c.warnBytes > 0 && size > c.warnBytes
}

func contextCaps(conn *sql.DB, projectID int) (contextLimits, error) {
	warn, err := intConfig(conn, projectID, db.KeyContextWarnBytes)
	if err != nil {
		return contextLimits{}, err
	}
	errorBytes, err := intConfig(conn, projectID, db.KeyContextErrorBytes)
	if err != nil {
		return contextLimits{}, err
	}
	return contextLimits{warnBytes: warn, errorBytes: errorBytes}, nil
}

func intConfig(conn *sql.DB, projectID int, key string) (int, error) {
	entry, err := db.GetConfig(conn, projectID, key)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", key, err)
	}
	var n int
	if _, err := fmt.Sscanf(entry.Value, "%d", &n); err != nil {
		return 0, fmt.Errorf("reading %s: %q is not an integer", key, entry.Value)
	}
	return n, nil
}

// readFilePins is stage 3's file half: read each `--pin PATH`, hash it, record
// the path.
//
// A path that does not exist or is not a regular file is NOT_FOUND (exit 2),
// raised BEFORE the transaction opens so a bad pin costs nothing and refuses
// cleanly. Pinning is never partial: one bad path fails the whole set, because
// a run pinned to some of what its operator named is a run that cannot
// reproduce itself and cannot say which part is missing.
func readFilePins(paths []string) ([]db.Pin, error) {
	out := make([]db.Pin, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, notFoundErr(err, "pin file %s not found", path)
			}
			return nil, notFoundErr(err, "reading pin file %s: %v", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, notFoundErr(nil, "pin path %s is not a regular file", path)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil, notFoundErr(err, "reading pin file %s: %v", path, err)
		}
		out = append(out, db.Pin{
			Kind: db.PinKindFile, Ref: path, SHA256: workflow.SHA256(content),
		})
	}
	return out, nil
}

// dispatchOpen is RA4's seam. Dispatches are S6, so this is `false` today —
// written as a helper rather than an inline `false` so S6 adds a query behind
// it and the call site in activateTx never moves.
func dispatchOpen(_ *sql.Tx, _ int) (bool, error) {
	return false, nil
}
