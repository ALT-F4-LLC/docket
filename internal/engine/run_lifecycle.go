package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// The operator lifecycle verbs — `run pause`, `run resume`, `run abandon` —
// move the run row THROUGH THE LEDGER (DKT-86).
//
// Before this, the CLI wrote the status directly and no event existed for the
// transition. The gap was invisible for pause/resume — the reconciliation
// rollup emits `run-paused`/`run-resumed` of its own — but ABANDONMENT has no
// rollup twin, so a terminal transition left the feed's last lifecycle entry a
// pause: a consumer reading only events saw a paused run as the final state
// and could not learn that the run ended, when, why, or by whom (RUN-5,
// 574 events, zero `run-abandoned`). Every run-row status transition now
// writes its event in the same transaction as the status.

// MoveRunOptions are the lifecycle verbs' inputs — one options entry rather
// than a seven-positional signature, for the reason DecideOptions gives.
type MoveRunOptions struct {
	RunID int
	// Verb names the CLI verb for the refusal message: pause, resume, abandon.
	Verb string
	// To is the target status; From is the closed set of statuses the
	// transition may leave.
	To   model.RunStatus
	From []model.RunStatus
	// Reason is `--reason`, required by the CLI on an abandon.
	Reason string
	// Under is the authority a PAUSE or an ABANDON is exercised under
	// (DKT-1899), required on exactly those two. A RESUME carries none: it
	// returns a run to work rather than disposing of anything, so there is no
	// resolution for an authority to answer for.
	Under Authority
	// Token is the run's conductor capability (DKT-2465, conductor.go):
	// required on a bound run, ignored on an unbound one.
	Token string
	NowMS int64
}

// lifecycleEvents maps each operator-reachable target status to its event
// kind. `run-done` is absent deliberately: no operator verb moves a run to
// `done` — that is the reconciliation rollup's transition, and it already
// logs itself.
var lifecycleEvents = map[model.RunStatus]string{
	model.RunWaitingHuman: EventRunPaused,
	model.RunActive:       EventRunResumed,
	model.RunAbandoned:    EventRunAbandoned,
}

// MoveRunWith applies one operator lifecycle transition and records its event
// atomically. `Verb` names the CLI verb for the refusal message; `From` is the
// closed set of statuses the transition may leave.
//
// The event's `data` carries `from`, `to`, and the operator's `reason` —
// `from` because the interesting question about a transition is what it left,
// and `reason` because "abandoned" alone does not answer the question somebody
// will ask about a run that ended without completing. The actor is `human` by
// the kind's attribution mapping, which is the truth: nothing in the engine
// pauses, resumes, or abandons a run BY THIS VERB'S ROUTE — every automatic
// transition goes through the reconciliation rollup (reconcile.go) instead: a
// budget breach writes its own `run-paused` with `data.reason = "budget"`
// (distinguishable by exactly that field), and the rollup's own resume/pause
// carry no `data` at all, which is how an event consumer tells an operator
// verb from an automatic one. DKT-68: the rollup's automatic resume
// deliberately DECLINES to fire while a run sits at `waiting-human` on an
// unresolved budget breach (`runs.breach_reason` still set) — that run can
// only return to `active` through this verb, or through a cap raise that
// itself resolves the breach (DKT-80).
// The []string it returns is the run's OUTSTANDING WORKTREES (DKT-116) —
// non-nil only on an abandon; see recordedWorktreesTx.
//
// EVERY MOVE REQUIRES THE RUN'S CONDUCTOR CAPABILITY on a bound run
// (DKT-2465, conductor.go): a pause, resume or abandon is the conductor's
// decision, and before the capability existed any executor sharing the
// checkout could park or end the run it was working in. The check is inside
// the transaction, after the run is found and before its status is inspected,
// so a caller without the capability learns only that the run exists.
func MoveRunWith(conn store.LegacyDB, opts MoveRunOptions) (*model.Run, []string, error) {
	runID, verb, to, from, reason, nowMS :=
		opts.RunID, opts.Verb, opts.To, opts.From, opts.Reason, opts.NowMS
	kind, ok := lifecycleEvents[to]
	if !ok {
		return nil, nil, fmt.Errorf("no lifecycle event kind for a move to %s", to)
	}
	// A pause and an abandon are dispositions, and DKT-1899 requires each to
	// say under what authority it was made. A resume is neither, and is
	// deliberately exempt.
	if kind == EventRunPaused || kind == EventRunAbandoned {
		if err := opts.Under.require("run " + verb); err != nil {
			return nil, nil, err
		}
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, nil, fmt.Errorf("moving run %s: %w", model.FormatRunID(runID), err)
	}
	defer tx.Rollback()
	st := store.TxOn(tx)

	run, err := st.GetRunTx(runID)
	if errors.Is(err, store.ErrRunNotFound) {
		return nil, nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, nil, err
	}
	if err := authorizeConductorTx(tx, runID, opts.Token, "run "+verb); err != nil {
		return nil, nil, err
	}

	// The transition is refused rather than silently applied, so a script that
	// pauses an already-parked run learns it did nothing. A no-op success here
	// would let a harness believe it had quiesced a run that never was active.
	if !slices.Contains(from, run.Status) {
		return nil, nil, conflictErr("run %s is %s; %s applies to a run that is %s",
			run.Ref(), run.Status, verb, orStatusList(from))
	}

	// An ABANDONED run's worktrees are named in the same transaction that ends
	// it (DKT-116): a relay's close-time sweep only covers worktrees its own
	// session created, and an abandoned run never reaches a close — so
	// abandonment was the one exit that stranded checkouts and their
	// worktree-wf_* branches with no surface ever reporting them. Docket names
	// what it recorded and removes nothing: the checkouts are the operator's
	// tree, and a recorded-but-never-integrated sha may still be worth
	// recovering from one.
	var worktrees []string
	if to == model.RunAbandoned {
		worktrees, err = recordedWorktreesTx(tx, runID, 0)
		if err != nil {
			return nil, nil, err
		}
		// ABANDONMENT CLOSES WHAT IT ORPHANS (DKT-262). A run that ends takes
		// its vote steps with it, and the ballots those steps opened have
		// nothing left to decide — DKT-V11 stood open forever because RUN-4 was
		// abandoned around it. An open proposal is not inert: `vote list` shows
		// it as outstanding work, and since DKT-236 it is what a spawn-guard
		// carve-out points at, so a stale one makes two surfaces lie.
		//
		// In THIS transaction, beside the status write, for the reason the
		// worktree collection above is: a close committed separately can be
		// lost while the abandonment stands, which puts the row back in the
		// state this exists to prevent.
		if _, err := closeRunProposalsTx(
			tx, runID, abandonedProposalReason(runID, reason)); err != nil {
			return nil, nil, err
		}
	}

	if err := st.SetRunStatusTx(runID, to, reason, nowMS); err != nil {
		return nil, nil, err
	}

	// The park's ORIGIN moves with the status (DKT-305). A pause typed by an
	// operator parks no step, so the reconciliation rollup — which resumes a
	// run once no step is parked — used to read this run as unparked and flip
	// it back to `active` at the next step to route, with an empty-data
	// `run-resumed` naming nobody. RUN-31 ran a four-judge review, a
	// synthesize, a reconcile, and two fresh votes past its operator's pause
	// that way. The origin is what the rollup now reads instead, and it is
	// cleared by the same verbs that end the park: a resume, or an abandon
	// that ends the run outright.
	origin := model.RunPauseOriginNone
	if to == model.RunWaitingHuman {
		origin = model.RunPauseOriginOperator
	}
	if err := st.SetRunPauseOriginTx(runID, origin); err != nil {
		return nil, nil, err
	}

	payload := map[string]any{
		"from":   string(run.Status),
		"to":     string(to),
		"reason": reason,
	}
	if len(worktrees) > 0 {
		payload["worktrees"] = worktrees
	}
	// DKT-1899, on the two kinds that require it. A resume's payload is
	// unchanged, so nothing reading it meets a key that means nothing there.
	if opts.Under.Kind != "" {
		opts.Under.addTo(payload)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("recording the %s transition: %w", kind, err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: kind, RunID: runID, Data: string(data), AtMS: nowMS,
	}); err != nil {
		return nil, nil, err
	}

	updated, err := st.GetRunTx(runID)
	if err != nil {
		return nil, nil, err
	}

	if err := commitWrite(tx); err != nil {
		return nil, nil, fmt.Errorf("moving run %s: %w", model.FormatRunID(runID), err)
	}
	return updated, worktrees, nil
}

// recordedWorktreesTx collects the distinct worktrees a run's steps recorded
// (`steps.work_root`, declared at `step record --worktree`), the whole run's
// or one issue's. It is what abandonment NAMES (DKT-116).
//
// It deliberately under-reports: a worktree a relay created for a step that
// never recorded is a fact docket was never told, and inventing a discovery
// pass over the operator's tree would be the engine executing filesystem
// walks it has no business running. Naming what was recorded turns "debris
// standing in five repos with no surface reporting it" into a listed set, and
// the relay's own sweep remains the authority for the rest.
func recordedWorktreesTx(tx store.LegacyTx, runID, issueID int) ([]string, error) {
	return store.TxOn(tx).RunWorktreesTx(runID, issueID)
}

// AbandonIssueOutcome reports what AbandonIssueInRun did.
type AbandonIssueOutcome struct {
	Run   string `json:"run"`
	Issue string `json:"issue"`
	// Steps lists the instances moved to `failed-routed`, in id order.
	Steps []string `json:"steps"`
	// RunStatus is the run's status AFTER the rollup — the abandoned issue may
	// have been the last unfinished work, in which case the run is now done.
	RunStatus string `json:"run_status"`
	// Worktrees is the issue's recorded worktrees (DKT-116) — what its steps
	// declared at record time and no close will ever sweep now. Excluded from
	// the frozen v1 payload; the CLI's v2 wrapper and the event carry it.
	Worktrees []string `json:"-"`
}

// AbandonIssueOptions are `run abandon --issue`'s inputs.
type AbandonIssueOptions struct {
	RunID   int
	IssueID int
	// Reason is `--reason`, required.
	Reason string
	// Token is the run's conductor capability (DKT-2465, conductor.go):
	// required on a bound run, ignored on an unbound one.
	Token string
	NowMS int64
}

// AbandonIssueInRunWith is the per-issue disposition (DKT-28): every remaining
// step of ONE issue stops, with a reason, and the run and its other issues
// continue.
//
// Before this verb every path out of a mis-routed issue was blocked or
// terminal: `step resolve` applies to parked steps and the mis-routed issue's
// steps were `pending`; a fix step with no `max_attempts` re-offers forever
// and never parks; `run pause` parks the RUN while the steps stay pending;
// and the only guard-satisfying end was `run abandon` — terminal for the
// WHOLE run, taken under protest with other issues cleanly delivered.
//
// The steps take the SAME `failed-routed` terminus the `abandon-issue`
// routing produces (reconcile.go), and the issue's own status is deliberately
// NOT forced terminal for the routing's reason exactly: this is a statement
// about the RUN's work on the issue, and triage stays the operator's.
//
// It requires the run's conductor capability exactly as the whole-run abandon
// does (DKT-2465): the per-issue disposition ends a lane's work the same way,
// and an executor must not be able to end a sibling's.
func AbandonIssueInRunWith(conn store.LegacyDB, opts AbandonIssueOptions) (*AbandonIssueOutcome, error) {
	runID, issueID, reason, nowMS := opts.RunID, opts.IssueID, opts.Reason, opts.NowMS
	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("abandoning an issue: %w", err)
	}
	defer tx.Rollback()
	st := store.TxOn(tx)

	run, err := st.GetRunTx(runID)
	if errors.Is(err, store.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}
	if err := authorizeConductorTx(tx, runID, opts.Token, "run abandon --issue"); err != nil {
		return nil, err
	}
	if run.Status != model.RunActive && run.Status != model.RunWaitingHuman {
		return nil, conflictErr(
			"run %s is %s; abandoning one issue applies to a run that is %s",
			run.Ref(), run.Status,
			orStatusList([]model.RunStatus{model.RunActive, model.RunWaitingHuman}))
	}

	inRun, err := st.RunHasIssueTx(runID, issueID)
	if err != nil {
		return nil, err
	}
	if !inRun {
		return nil, notFoundErr(store.ErrNotFound, "issue %s is not part of run %s",
			model.FormatID(issueID), model.FormatRunID(runID))
	}

	remaining, err := st.IssueUnfinishedStepsTx(runID, issueID)
	if err != nil {
		return nil, err
	}
	if len(remaining) == 0 {
		return nil, conflictErr(
			"every step of %s in %s is already terminal; there is nothing to abandon",
			model.FormatID(issueID), model.FormatRunID(runID))
	}

	if _, err := st.FailRouteIssueStepsTx(runID, issueID, nowMS); err != nil {
		return nil, err
	}

	instances := make([]string, 0, len(remaining))
	for _, s := range remaining {
		instances = append(instances, s.Instance)
	}
	// The issue's recorded worktrees, named at the same stop (DKT-116) — the
	// stopped work's checkouts have no later close to sweep them.
	worktrees, err := recordedWorktreesTx(tx, runID, issueID)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"issue": model.FormatID(issueID), "reason": reason, "steps": instances,
	}
	if len(worktrees) > 0 {
		payload["worktrees"] = worktrees
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("recording the abandonment: %w", err)
	}
	// The same resolution the routing records, for the same reason: the two
	// paths are one fact about the issue, so they must not leave the tracker
	// saying different things about it (DKT-245).
	if err := st.SetIssueResolutionTx(issueID, store.IssueResolutionAbandoned); err != nil {
		return nil, err
	}

	// The SAME release the routing path does (DKT-377). The cascade above just
	// terminalized every remaining step — a `waiting-human` park among them —
	// without routing, so nothing else will notice the issue is no longer
	// under review, and the live status would strand out of every ready set.
	if _, err := releaseAbandonedIssueTx(tx, issueID, nowMS); err != nil {
		return nil, err
	}

	// The SAME kind the `abandon-issue` routing writes, already attributed
	// human — the two paths are one fact ("this run stops working the issue")
	// with two actors, and the reason distinguishes them in the data.
	if err := recordEvent(tx, eventRecord{
		Kind: EventIssueAbandoned, RunID: runID, IssueID: issueID,
		Data: string(data), AtMS: nowMS,
	}); err != nil {
		return nil, err
	}

	// The SAME trail comment the routing path drops (DKT-377). Both paths
	// record one fact about the issue, and a reader of `docket issue show`
	// must not be able to tell which verb produced it by whether the trail
	// mentions it at all. The RUN is named rather than a step instance
	// because no step decided this one — an operator did, from outside the
	// graph — and the reason they gave rides along.
	abandonNote := fmt.Sprintf("%s abandoned the issue.", model.FormatRunID(runID))
	if reason != "" {
		abandonNote = fmt.Sprintf("%s abandoned the issue: %s",
			model.FormatRunID(runID), reason)
	}
	if err := commentEngineEvent(tx, issueID, abandonNote, nowMS); err != nil {
		return nil, err
	}

	// The rollup, exactly as a routed step's transaction runs it: the
	// abandoned issue may have been the run's last unfinished work, or the
	// park that held it at waiting-human.
	if err := reconcileRun(tx, runID, nowMS); err != nil {
		return nil, err
	}

	updated, err := st.GetRunTx(runID)
	if err != nil {
		return nil, err
	}
	if err := commitWrite(tx); err != nil {
		return nil, fmt.Errorf("abandoning an issue: %w", err)
	}
	return &AbandonIssueOutcome{
		Run: model.FormatRunID(runID), Issue: model.FormatID(issueID),
		Steps: instances, RunStatus: string(updated.Status),
		Worktrees: worktrees,
	}, nil
}

// orStatusList renders the legal source statuses for a refusal message.
func orStatusList(statuses []model.RunStatus) string {
	switch len(statuses) {
	case 0:
		return "in no status"
	case 1:
		return string(statuses[0])
	}
	out := ""
	for i, s := range statuses {
		switch {
		case i == 0:
			out = string(s)
		case i == len(statuses)-1:
			out += " or " + string(s)
		default:
			out += ", " + string(s)
		}
	}
	return out
}
