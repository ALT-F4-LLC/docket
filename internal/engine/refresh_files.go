package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The mid-run attachment-list refresh: the scope refresh's exception
// (scope_refresh.go) applied to the snapshot's `files` key.
//
// Activation freezes the issue's attached path list into
// `run_issues.issue_snapshot`, and `issue.files` renders that list
// (frozenAttachedPaths, packet.go). Without this refresh a detach through
// `issue file remove` never reaches a live run: the frozen path keeps rendering
// with live bytes, and deleting the file makes the render refuse. The refresh
// keeps the scope refresh's four properties:
//
//  1. It carries no list of its own. It copies `issue_files` verbatim, so it can
//     only make real what `issue file add|remove` already declared, and with
//     nothing declared since the freeze it refuses.
//  2. No step straddles it: the same quiescence checks as the scope refresh.
//  3. It rewrites no history: terminal steps keep what they recorded.
//  4. One `issue-files-refreshed` event dates and attributes the change.
//
// Every other snapshot field is re-encoded unchanged.

// RefreshedFiles reports what a refresh did, as the verb answers with it.
type RefreshedFiles struct {
	Run   string `json:"run"`
	Issue string `json:"issue"`
	// From and To are the frozen list and the live one that replaced it, both
	// in path order, the order activation freezes them in.
	From []string `json:"from"`
	To   []string `json:"to"`
	// Steps names the non-terminal instances the refresh reaches, in id order.
	Steps []string `json:"steps"`
}

// RefreshIssueFilesInRun re-reads the issue's attached paths and overwrites the
// `files` key of ONE run-issue's activation snapshot.
//
// On a bound run it requires the conductor capability as `token`: `issue file
// add|remove` is ungated, so without the check an executor could change its own
// issue's attachments and then make that change reach its own packets.
func RefreshIssueFilesInRun(
	conn *sql.DB, runID, issueID int, reason string, by Attribution, token string,
	nowMS int64,
) (*RefreshedFiles, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, validationErr(
			"a reason is required to refresh a snapshotted attachment list; " +
				"the event trail must say why a live run's packets changed " +
				"which files they carry")
	}
	if err := by.require("run refresh-files"); err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("refreshing the snapshotted attachment list: %w", err)
	}
	defer tx.Rollback()

	run, err := db.GetRunTx(tx, runID)
	if errors.Is(err, db.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}
	if err := authorizeConductorTx(tx, runID, token, "run refresh-files"); err != nil {
		return nil, err
	}
	if run.Status != model.RunActive && run.Status != model.RunWaitingHuman {
		return nil, conflictErr(
			"run %s is %s; an attachment-list refresh applies to a run that is "+
				"%s — a planning run freezes the current list at its next "+
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
				"will freeze the attachment list declared then. There is "+
				"nothing to refresh",
			model.FormatID(issueID), run.Ref())
	}

	var snapshot issueSnapshotFields
	if err := json.Unmarshal([]byte(ri.IssueSnapshot), &snapshot); err != nil {
		return nil, fmt.Errorf("reading the issue snapshot: %w", err)
	}
	// A snapshot written before the list was frozen has no `files` key, and its
	// packets already render the live list.
	if snapshot.Files == nil {
		return nil, conflictErr(
			"%s's snapshot in %s predates the frozen attachment list, so its "+
				"packets already render the live list. There is nothing to "+
				"refresh",
			model.FormatID(issueID), run.Ref())
	}
	frozen := *snapshot.Files

	live, err := db.GetIssueFilesTx(tx, issueID)
	if err != nil {
		return nil, fmt.Errorf("reading the live attachment list for %s: %w",
			model.FormatID(issueID), err)
	}

	// A refresh that changed nothing would leave an event that ruled nothing.
	if slices.Equal(frozen, live) {
		return nil, conflictErr(
			"%s already renders its live attachment list %v in %s; a refresh "+
				"copies the issue's attached files into the run's snapshot, and "+
				"nothing has been attached or removed since it was frozen. "+
				"Change it first — `docket issue file add|remove %s` — then "+
				"refresh",
			model.FormatID(issueID), frozen, run.Ref(),
			model.FormatID(issueID))
	}

	steps, err := refreshableSteps(tx, runID, issueID, run.Ref())
	var straddle *refreshStraddleError
	var terminal *refreshAllTerminalError
	switch {
	case errors.As(err, &straddle):
		return nil, conflictErr(
			"%d step(s) of %s in %s are mid-flight (%s); a refresh under one "+
				"would change which files an executor's packet carries while it "+
				"is still executing, or while its gates still judge an artifact "+
				"written against the frozen list — wait for them to record (or "+
				"for their leases to be reaped), then retry",
			len(straddle.Straddled), model.FormatID(issueID), run.Ref(),
			strings.Join(straddle.Straddled, ", "))
	case errors.As(err, &terminal):
		return nil, conflictErr(
			"every step of %s in %s is terminal; their artifacts are already "+
				"recorded against the attachment list they ran under and no "+
				"packet will render again, so a refresh could only rewrite that "+
				"record. The changed list reaches the issue's NEXT run",
			model.FormatID(issueID), run.Ref())
	case err != nil:
		return nil, err
	}
	underDispatch, err := refreshUnderDispatchTx(tx, runID, issueID)
	if err != nil {
		return nil, err
	}
	snapshot.Files = &live
	refreshed, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("serializing the refreshed snapshot: %w", err)
	}
	if err := db.SetRunIssueSnapshotTx(tx, runID, issueID, string(refreshed)); err != nil {
		return nil, err
	}

	data, err := rulingData(by, withDispatchRows(map[string]any{
		"issue": model.FormatID(issueID), "reason": reason,
		"from": frozen, "to": live, "steps": steps,
	}, underDispatch))
	if err != nil {
		return nil, fmt.Errorf("recording the attachment-list refresh: %w", err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventIssueFilesRefreshed, RunID: runID, IssueID: issueID,
		Data: data, AtMS: nowMS,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("refreshing the snapshotted attachment list: %w", err)
	}
	return &RefreshedFiles{
		Run: run.Ref(), Issue: model.FormatID(issueID),
		From: frozen, To: live, Steps: steps,
	}, nil
}
