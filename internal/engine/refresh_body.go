package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The mid-run body refresh (DKT-2291), the other half of DKT-869's exception.
//
// DKT-741's freeze STANDS on both columns: activation writes
// `run_issues.body_snapshot` once, the packet's `== REQUEST` and
// `== INPUT issue.body` both render it, and no edit to `issues.description`
// reaches a live run. What RUN-98 / HRN-830 charged for is the freeze holding
// on one column while DKT-869's exception had already been granted on the
// other.
//
// The RUN-98 shape, from the packet itself: an operator amendment landed, the
// conductor spent it with `run refresh-scope`, and step STEP-9184's packet then
// rendered the REFRESHED scope in its header beside the SUPERSEDED wording of
// the criterion in its body. A verify-ac seat judging AC-as-written from that
// packet judges the criterion the operator replaced. The partial staleness is
// what makes it worse than a wholly frozen packet: the fresh `scope:` line is
// positive evidence the ruling was applied, so nothing invites the reader to
// doubt the body beside it.
//
// So the body gets the same explicit, refusable, recorded exception, with the
// same four properties that make it not a hole in §9 item 5:
//
//  1. IT CARRIES NO BODY OF ITS OWN. The refresh reads `issues.description` and
//     copies it verbatim; there is no `--body` here and there must never be
//     one. `issue create|edit --description` stays the SOLE writer of that
//     column, so this verb cannot make real any text that was not already
//     declared through the gate description edits have always had — and a
//     refresh with no amendment behind it has nothing to copy and is refused.
//  2. NO STEP STRADDLES IT. The same quiescence rule, for the same reason: a
//     step that already holds a packet rendered under the frozen description
//     must not record its artifact under the amended one.
//  3. IT REWRITES NO HISTORY. Terminal steps keep the request their claim
//     recorded — served from this verb's own event, since the read-back
//     assembles from the live column (see recordedIssueBody in context.go).
//     What changes is what the REMAINING steps will render.
//  4. THE DISCONTINUITY IS IN THE LEDGER. One `issue-body-refreshed` event
//     carries both shas, the superseded text, the steps it reaches and the
//     operator's reason.
//
// Everything the freeze protects that is NOT the description is untouched: the
// issue snapshot blob — title, kind, labels, `linked`, and the scope DKT-869
// governs — is not read or written here.

// RefreshedBody reports what a refresh did, as the verb answers with it.
type RefreshedBody struct {
	Run   string `json:"run"`
	Issue string `json:"issue"`
	// FromSHA256 and ToSHA256 name the superseded snapshot and the refreshed
	// one. The shas rather than the bodies in THIS answer: a description runs
	// to kilobytes, and the digest is what an operator reading the verb's
	// output compares. The event is the other case — it carries the superseded
	// text itself, because that text is the only record of what the steps the
	// refresh did not reach were handed (see recordedIssueBody).
	FromSHA256 string `json:"from_sha256"`
	ToSHA256   string `json:"to_sha256"`
	// Steps names the non-terminal instances the refresh reaches, in id order:
	// exactly the steps that will render the new body.
	Steps []string `json:"steps"`
}

// RefreshIssueBodyInRun re-reads `issues.description` and overwrites ONE
// run-issue's `body_snapshot` and `body_sha256` (DKT-2291).
//
// The unit is (run, issue) for the reason it is there: that is where the
// snapshot LIVES, one row per bound issue per run, read by every step of that
// issue. A per-step verb would let two steps of one issue state two different
// requests inside one dispatch.
func RefreshIssueBodyInRun(
	conn *sql.DB, runID, issueID int, reason string, nowMS int64,
) (*RefreshedBody, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, validationErr(
			"a reason is required to refresh a snapshotted body; the event " +
				"trail must say why a live run's packets changed what they ask for")
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("refreshing the snapshotted body: %w", err)
	}
	defer tx.Rollback()

	run, err := db.GetRunTx(tx, runID)
	if errors.Is(err, db.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}
	// The same two statuses the scope refresh accepts, for the same reason: a
	// `planning` run has frozen nothing yet, and a terminal run's snapshot is
	// referenced only by completed steps' history.
	if run.Status != model.RunActive && run.Status != model.RunWaitingHuman {
		return nil, conflictErr(
			"run %s is %s; a body refresh applies to a run that is %s — a "+
				"planning run snapshots the current description at its next "+
				"activation, and a terminal run's snapshot is history, not a "+
				"premise to move",
			run.Ref(), run.Status,
			orStatusList([]model.RunStatus{model.RunActive, model.RunWaitingHuman}))
	}

	ri, err := runIssueTx(tx, runID, issueID)
	if err != nil {
		return nil, err
	}
	// The BINDING is what "frozen" means, and the issue snapshot is its marker.
	// An issue whose description is legitimately empty is still bound, so the
	// body column cannot answer this question.
	if ri.IssueSnapshot == "" {
		return nil, conflictErr(
			"issue %s is attached to %s but not yet bound: activation has "+
				"frozen no snapshot for it, so the next `docket run activate` "+
				"will snapshot the description declared then. There is nothing "+
				"to refresh",
			model.FormatID(issueID), run.Ref())
	}

	// The value comes from the column `--description` writes, and nowhere else.
	live, err := liveIssueBodyTx(tx, issueID)
	if err != nil {
		return nil, err
	}

	// The gate: without an amendment recorded through `issue edit` there is
	// nothing this verb is authorized to make real. Refusing rather than
	// no-opping keeps the ledger honest — an `issue-body-refreshed` event that
	// refreshed nothing is a ruling that ruled nothing.
	//
	// VALIDATION_ERROR rather than the scope refresh's CONFLICT: the operator
	// named a run and an issue that are both in a refreshable state, and the
	// argument that is wrong is the request itself — nothing about the run's
	// state has to change for the same command to become correct, only the
	// description.
	if live == ri.BodySnapshot {
		return nil, validationErr(
			"%s already renders its live description in %s; a refresh copies "+
				"the description declared on the issue into the run's snapshot, "+
				"and nothing has been declared since it was frozen. Amend it "+
				"first — `docket issue edit %s --description F` — then refresh",
			model.FormatID(issueID), run.Ref(), model.FormatID(issueID))
	}

	// Quiescence, per issue for the steps and per run for the dispatch — the
	// same two checks, spelled here with the body's wording rather than
	// parameterized: the refusals are what an operator reads, and a shared one
	// would have to say "premise" where it can say what actually moved.
	steps, err := refreshableSteps(tx, runID, issueID, run.Ref())
	if err != nil {
		return nil, err
	}
	if open, err := db.OpenDispatchTx(tx, runID); err == nil {
		return nil, conflictErr(
			"a dispatch is open for %s (%s, expiring at %d); close or abandon "+
				"it before refreshing — its manifest was offered under the "+
				"frozen description, and a relay spawning from it would claim "+
				"rows whose packets no longer ask what the manifest's reader saw",
			run.Ref(), FormatDispatchID(open.ID), open.ExpiresMS)
	} else if !errors.Is(err, db.ErrNoOpenDispatch) {
		return nil, err
	}

	from, to := ri.BodySHA256, workflow.SHA256([]byte(live))
	if err := db.SetRunIssueBodyTx(tx, runID, issueID, live, to); err != nil {
		return nil, err
	}

	// One event, carrying both shas, the steps it reaches, and why — plus the
	// superseded TEXT, which this kind carries and its scope twin does not.
	// That field is load-bearing rather than descriptive: a step already handed
	// out reads its context back from the live column, so the body it was given
	// survives here or nowhere (recordedIssueBody, context.go).
	data, err := json.Marshal(map[string]any{
		"issue": model.FormatID(issueID), "reason": reason,
		"from_sha256": from, "to_sha256": to,
		"from_body": ri.BodySnapshot, "steps": steps,
	})
	if err != nil {
		return nil, fmt.Errorf("recording the body refresh: %w", err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventIssueBodyRefreshed, RunID: runID, IssueID: issueID,
		Data: string(data), AtMS: nowMS,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("refreshing the snapshotted body: %w", err)
	}
	return &RefreshedBody{
		Run: run.Ref(), Issue: model.FormatID(issueID),
		FromSHA256: from, ToSHA256: to, Steps: steps,
	}, nil
}

// liveIssueBodyTx reads the description inside the refresh's transaction, so
// the value written is the value that was read — an amendment landing between a
// standalone read and the UPDATE would otherwise be reported as the `to` of an
// event that wrote something else.
func liveIssueBodyTx(tx *sql.Tx, issueID int) (string, error) {
	var description sql.NullString
	err := tx.QueryRow(
		`SELECT description FROM issues WHERE id = ?`, issueID).Scan(&description)
	if err == sql.ErrNoRows {
		return "", notFoundErr(db.ErrNotFound, "issue %s not found",
			model.FormatID(issueID))
	}
	if err != nil {
		return "", fmt.Errorf("reading the live description for %s: %w",
			model.FormatID(issueID), err)
	}
	return description.String, nil
}
