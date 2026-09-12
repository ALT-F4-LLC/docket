package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The mid-run scope refresh (DKT-869), and why it exists after DKT-741 said it
// would not.
//
// DKT-741 established the freeze and it STANDS: `run_issues.issue_snapshot` is
// written once at activation stage 4, the packet's `context.issue.scope` and
// the recorded `issue.diff` scope both read it (§5.1.1, §6.6, §6.7.1 D1), and
// nothing in the engine rewrites it. What DKT-741 also decided — that no verb
// would ever refresh it, and that an authorized widen must be spent by
// abandoning the issue and re-planning it — is what RUN-52 (VPL-434) then
// charged for, twice.
//
// The RUN-52 shape, from the run's own terminal routing note: the panel
// rejected the work 3/3 on an out-of-scope migration blocker, the operator
// AGREED and widened the issue's scope, the conductor ran `issue edit --scope`
// — and the already-minted `fix@2` step still rendered the old two-path scope.
// "Scope snapshots per-step at creation and does not reach the already-created
// fix@2 step — no engine verb refreshes an existing step's scope." The
// sanctioned remedy (`run abandon --issue` plus a full re-plan) is priced for a
// run whose PREMISE changed; here the premise was intact and one declaration
// had been corrected by the operator, mid-loop, on purpose. The issue was
// abandoned instead.
//
// So the freeze keeps its default and gains an explicit, refusable, recorded
// exception. Four properties are what make it not a hole in §9 item 5:
//
//  1. IT CARRIES NO SCOPE OF ITS OWN. The refresh reads `issues.scope_globs`
//     and copies it verbatim; there is no `--scope` here and there must never
//     be one. `issue create|edit --scope` stays the SOLE writer of that column
//     (issue_scope.go), so this verb cannot make real any scope that was not
//     already declared through the one gate scope widening has always had. The
//     authorization for what lands is, exactly, the authorization for the
//     widen — and a refresh with no widen behind it has nothing to copy and is
//     refused (D4 below).
//  2. NO STEP STRADDLES IT. It refuses while any of the issue's steps is
//     `claimed`, `running`, or `gated`, and while a dispatch is open — the
//     repin quiescence rule (repin.go), applied to the other frozen premise. A
//     step that already holds a packet rendered under the old scope must not
//     record its diff under the new one.
//  3. IT REWRITES NO HISTORY. Terminal steps keep their artifacts, their
//     recorded diffs, and the scope those were computed over. What changes is
//     what the REMAINING steps will render — the same division of labour
//     `run repin` draws between an agreement and the history under it.
//  4. THE DISCONTINUITY IS IN THE LEDGER. One `issue-scope-refreshed` event
//     carries the old scope, the new scope, the steps it reaches and the
//     operator's reason, so two steps of one run rendering two different
//     scopes is a fact a reader can date and attribute rather than a drift
//     they must infer. This is what keeps "a packet is reproducible from the
//     ledger" true: the ledger now says when the input changed.
//
// Everything the freeze protects that is NOT scope is untouched — title, kind,
// labels, `linked`, and the description snapshot are re-encoded byte-for-byte
// from what activation wrote. A mid-run relabel still cannot change how a step
// routes, and a mid-run description edit still cannot reach a packet.

// RefreshedScope reports what a refresh did, as the verb answers with it.
type RefreshedScope struct {
	Run   string `json:"run"`
	Issue string `json:"issue"`
	// From and To are the frozen scope and the live one it was replaced with,
	// in DECLARED ORDER — the author's order, which the snapshot echoes back
	// verbatim (issue_scope.go) and which this must not sort.
	From []string `json:"from"`
	To   []string `json:"to"`
	// Steps names the non-terminal instances the refresh reaches, in id order:
	// exactly the steps that will render the new scope and record their diffs
	// over it.
	Steps []string `json:"steps"`
}

// refreshBlockingStepStatuses are the statuses that make a refresh a straddle.
//
// `claimed` and `running` are an executor mid-flight: its packet was rendered
// under the frozen scope and its diff will be recorded after the change, which
// is the one combination that would falsify a step's own provenance.
//
// `gated` is here for a reason the scheduler's own scope predicate does NOT
// share (stepExcludesScope deliberately omits it): a gated step's artifact has
// recorded and its token has retired, but its saga is still running, and a
// diff-shaped gate reads snapshotScope to decide what it measures (saga.go,
// DKT-63). Refreshing under one would run a gate over paths the artifact it is
// gating never covered.
//
// `pending` and `waiting-human` are the refreshable states, and both on
// purpose. `pending` is the RUN-52 case exactly. `waiting-human` is the case
// DKT-741's own advisory already treats as live (TestScopeEditWarningFiresFor
// AParkedStep): a parked step will be resolved and will render again, and a
// `retry` re-executes it and records a fresh diff — over the refreshed scope,
// which is the entire point of authorizing the widen.
var refreshBlockingStepStatuses = []string{
	db.StepClaimed, db.StepRunning, db.StepGated,
}

// RefreshIssueScopeInRun re-reads `issues.scope_globs` and overwrites the
// `scope` key of ONE run-issue's activation snapshot (DKT-869).
//
// The unit is (run, issue) rather than a single step because that is where the
// snapshot LIVES: one blob per bound issue per run, read by every step of that
// issue. A `step refresh-scope STEP-N` would name one step and silently move
// its siblings' scope too, which is a blast radius the argument no longer
// matches — and it would let two steps of one issue disagree about scope
// inside one dispatch, which is the drift D2 exists to prevent.
func RefreshIssueScopeInRun(
	conn *sql.DB, runID, issueID int, reason string, nowMS int64,
) (*RefreshedScope, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, validationErr(
			"a reason is required to refresh a snapshotted scope; the event " +
				"trail must say why a live run's packets changed what they declare")
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("refreshing the snapshotted scope: %w", err)
	}
	defer tx.Rollback()

	run, err := db.GetRunTx(tx, runID)
	if errors.Is(err, db.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}
	// The same two statuses repin accepts, for the same reason: a `planning`
	// run has frozen nothing yet (its next activation snapshots the widened
	// scope by itself, which is why the DKT-741 advisory stays silent there),
	// and a terminal run's snapshot is referenced only by completed steps'
	// history.
	if run.Status != model.RunActive && run.Status != model.RunWaitingHuman {
		return nil, conflictErr(
			"run %s is %s; a scope refresh applies to a run that is %s — a "+
				"planning run snapshots the current scope at its next "+
				"activation, and a terminal run's snapshot is history, not a "+
				"premise to move",
			run.Ref(), run.Status,
			orStatusList([]model.RunStatus{model.RunActive, model.RunWaitingHuman}))
	}

	ri, err := runIssueTx(tx, runID, issueID)
	if err != nil {
		return nil, err
	}
	if ri.IssueSnapshot == "" {
		return nil, conflictErr(
			"issue %s is attached to %s but not yet bound: activation has "+
				"frozen no snapshot for it, so the next `docket run activate` "+
				"will snapshot the scope declared then. There is nothing to "+
				"refresh",
			model.FormatID(issueID), run.Ref())
	}

	frozen, err := decodeSnapshotScope(ri.IssueSnapshot)
	if err != nil {
		return nil, err
	}

	// D1: the value comes from the column `--scope` writes, and from nowhere
	// else. This verb takes no globs.
	live, err := liveIssueScopeTx(tx, issueID)
	if err != nil {
		return nil, err
	}

	// D4, the gate: without a widen recorded through `issue edit --scope`
	// there is nothing this verb is authorized to make real. Refusing rather
	// than no-opping keeps the ledger honest — an `issue-scope-refreshed`
	// event that refreshed nothing is a ruling that ruled nothing, and a
	// reader auditing the discontinuities would have to open each one to find
	// out which were real.
	if sameScope(frozen, live) {
		return nil, conflictErr(
			"%s already renders %s in %s; a refresh copies the scope declared "+
				"on the issue into the run's snapshot, and nothing has been "+
				"declared since it was frozen. Widen it first — `docket issue "+
				"edit %s --scope GLOB --scope GLOB` — then refresh",
			model.FormatID(issueID), renderScope(frozen), run.Ref(),
			model.FormatID(issueID))
	}

	// D2: quiescence, per issue for the steps and per run for the dispatch.
	steps, err := refreshableSteps(tx, runID, issueID, run.Ref())
	if err != nil {
		return nil, err
	}
	if open, err := db.OpenDispatchTx(tx, runID); err == nil {
		return nil, conflictErr(
			"a dispatch is open for %s (%s, expiring at %d); close or abandon "+
				"it before refreshing — its manifest was offered under the "+
				"frozen scope, and a relay spawning from it would claim rows "+
				"whose packets no longer say what the manifest's reader saw",
			run.Ref(), FormatDispatchID(open.ID), open.ExpiresMS)
	} else if !errors.Is(err, db.ErrNoOpenDispatch) {
		return nil, err
	}

	refreshed, err := reScopedSnapshot(ri.IssueSnapshot, live)
	if err != nil {
		return nil, err
	}
	if err := db.SetRunIssueSnapshotTx(tx, runID, issueID, refreshed); err != nil {
		return nil, err
	}

	// D4: one event, carrying both scopes, the steps it reaches, and why.
	data, err := json.Marshal(map[string]any{
		"issue": model.FormatID(issueID), "reason": reason,
		"from": frozen, "to": live, "steps": steps,
	})
	if err != nil {
		return nil, fmt.Errorf("recording the scope refresh: %w", err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventIssueScopeRefreshed, RunID: runID, IssueID: issueID,
		Data: string(data), AtMS: nowMS,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("refreshing the snapshotted scope: %w", err)
	}
	return &RefreshedScope{
		Run: run.Ref(), Issue: model.FormatID(issueID),
		From: frozen, To: live, Steps: steps,
	}, nil
}

// refreshableSteps returns the non-terminal instances of one issue in one run,
// in id order, refusing when any of them would straddle the change or when
// there are none left to reach.
func refreshableSteps(tx *sql.Tx, runID, issueID int, runRef string) ([]string, error) {
	rows, err := tx.Query(
		`SELECT instance, status FROM steps
		  WHERE run_id = ? AND issue_id = ? AND status NOT IN (?, ?, ?, ?)
		  ORDER BY id`,
		runID, issueID,
		db.StepDone, db.StepSkipped, db.StepSuperseded, db.StepFailedRouted)
	if err != nil {
		return nil, fmt.Errorf("collecting %s's remaining steps: %w", runRef, err)
	}
	type liveStep struct{ instance, status string }
	remaining, err := scanTxRows(rows, func(r *sql.Rows) (liveStep, error) {
		var s liveStep
		return s, r.Scan(&s.instance, &s.status)
	})
	if err != nil {
		return nil, err
	}

	blocking := make(map[string]bool, len(refreshBlockingStepStatuses))
	for _, s := range refreshBlockingStepStatuses {
		blocking[s] = true
	}
	var (
		instances []string
		straddled []string
	)
	for _, s := range remaining {
		if blocking[s.status] {
			straddled = append(straddled, fmt.Sprintf("%s (%s)", s.instance, s.status))
			continue
		}
		instances = append(instances, s.instance)
	}
	if len(straddled) > 0 {
		return nil, conflictErr(
			"%d step(s) of %s in %s are mid-flight (%s); a refresh under one "+
				"would change what an executor's packet means mid-execution, "+
				"or run a gate over paths the artifact it gates never covered "+
				"— wait for them to record (or for their leases to be reaped), "+
				"then retry",
			len(straddled), model.FormatID(issueID), runRef,
			strings.Join(straddled, ", "))
	}
	if len(instances) == 0 {
		return nil, conflictErr(
			"every step of %s in %s is terminal; their diffs are already "+
				"recorded over the scope they ran under and no packet will "+
				"render again, so a refresh could only rewrite that record. "+
				"The widened scope reaches the issue's NEXT run",
			model.FormatID(issueID), runRef)
	}
	return instances, nil
}

// runIssueTx loads one `run_issues` row, distinguishing "not in this run" from
// a read failure — the membership check `run abandon --issue` makes, returning
// the row because the caller needs its snapshot.
func runIssueTx(tx *sql.Tx, runID, issueID int) (*db.RunIssue, error) {
	all, err := db.ListRunIssuesTx(tx, runID)
	if err != nil {
		return nil, err
	}
	for _, ri := range all {
		if ri.IssueID == issueID {
			return ri, nil
		}
	}
	return nil, notFoundErr(db.ErrNotFound, "issue %s is not part of run %s",
		model.FormatID(issueID), model.FormatRunID(runID))
}

// liveIssueScopeTx is liveIssueScope inside the refresh's transaction, so the
// value written is the value that was read — a widen landing between a
// standalone read and the UPDATE would otherwise be reported as the `to` of an
// event that wrote something else.
func liveIssueScopeTx(tx *sql.Tx, issueID int) ([]string, error) {
	stored, err := db.IssueScopeGlobsTx(tx, issueID)
	if err != nil {
		return nil, fmt.Errorf("reading the live scope for %s: %w",
			model.FormatID(issueID), err)
	}
	return decodeScope(stored)
}

// decodeSnapshotScope pulls the `scope` key out of a snapshot blob. It is
// snapshotScope's transaction-free half, spelled here because the refresh reads
// the blob it is about to rewrite rather than the column.
func decodeSnapshotScope(snapshot string) ([]string, error) {
	var frozen struct {
		Scope []string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(snapshot), &frozen); err != nil {
		return nil, fmt.Errorf("reading the snapshotted scope: %w", err)
	}
	return frozen.Scope, nil
}

// reScopedSnapshot re-encodes a snapshot with a new `scope` and EVERY OTHER
// FIELD byte-identical.
//
// It decodes into `issueSnapshotFields` — the same type activation encodes
// with — so the canonical key order is the one type's declaration order in both
// directions, and a field added to §11.4's issue shape lands in both paths at
// once. TestRefreshedSnapshotIsByteIdenticalApartFromScope pins the round trip.
//
// An unknown key in the stored blob would be DROPPED by this round trip, which
// is why the pinning test exists rather than a tolerant merge: a snapshot
// carrying a key no Go field names means the two writers have already diverged,
// and the right time to find that out is in the test suite.
func reScopedSnapshot(snapshot string, scope []string) (string, error) {
	var fields issueSnapshotFields
	if err := json.Unmarshal([]byte(snapshot), &fields); err != nil {
		return "", fmt.Errorf("reading the issue snapshot: %w", err)
	}
	if fields.Labels == nil {
		fields.Labels = []string{}
	}
	// The undeclared case survives as `[]`, exactly as activation writes it for
	// an issue with no `--scope`: the blob's own vocabulary has no NULL, and
	// snapshotScope reads a missing or empty list as "no declared scope".
	if scope == nil {
		scope = []string{}
	}
	fields.Scope = scope

	out, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("serializing the refreshed snapshot: %w", err)
	}
	return string(out), nil
}
