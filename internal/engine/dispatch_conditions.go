package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// UNFILED SEAT CONDITIONS AT DISPATCH CLOSE (DKT-4263).
//
// A seat that casts approve-with-concerns names conditions the work still owes.
// The corpus asks the conductor to file each one as its own `tribunal` issue,
// linked to the vote, before the dispatch closes. Nothing enforced it: a close
// went through with a condition recorded only in the vote, and the work it named
// was never filed.
//
// So a close refuses while a vote sealed during the dispatch holds an
// approve-with-concerns cast and has no linked issue labelled `tribunal`.
// `--no-conditions REASON` waives it, and REASON is recorded as a run note.

// tribunalLabel is the label a filed seat condition carries.
const tribunalLabel = "tribunal"

// noConditionsVerb names the waiver in a conductor refusal.
const noConditionsVerb = "dispatch close --no-conditions"

// ConditionsWaiver is `dispatch close --no-conditions`. The zero value runs the
// check. A non-empty Reason waives it; Token is the run's conductor capability
// that authorizes the waiver on a bound run, and By attributes the run note.
type ConditionsWaiver struct {
	Reason string
	Token  string
	By     Attribution
}

// UnfiledCondition is one sealed vote with approve-with-concerns casts and no
// linked tribunal issue.
type UnfiledCondition struct {
	Proposal string   `json:"proposal"`
	Voters   []string `json:"voters"`
}

// unfiledConditions lists the unfiled conditions of the votes sealed since the
// run's open dispatch began, in proposal order. A vote counts when a run issue
// is linked to it, it is no longer open, and it was last updated at or after
// the dispatch opened.
func unfiledConditions(conn *sql.DB, runID int, openedSeq int64) ([]UnfiledCondition, error) {
	var openedMS int64
	if err := conn.QueryRow(`SELECT at_ms FROM events WHERE seq = ?`, openedSeq).Scan(&openedMS); err != nil {
		return nil, fmt.Errorf("reading when the dispatch opened: %w", err)
	}
	since := time.UnixMilli(openedMS).UTC().Format(time.RFC3339)

	rows, err := conn.Query(
		`SELECT p.id, GROUP_CONCAT(v.voter_name, ', ')
		   FROM proposals p
		   JOIN votes v ON v.proposal_id = p.id AND v.verdict = ?
		  WHERE p.status != ?
		    AND p.updated_at >= ?
		    AND p.id IN (SELECT pi.proposal_id FROM proposal_issues pi
		                   JOIN run_issues ri ON ri.issue_id = pi.issue_id
		                  WHERE ri.run_id = ?)
		    AND NOT EXISTS (SELECT 1 FROM proposal_issues pt
		                      JOIN issue_labels il ON il.issue_id = pt.issue_id
		                      JOIN labels l ON l.id = il.label_id
		                     WHERE pt.proposal_id = p.id AND l.name = ?)
		  GROUP BY p.id
		  ORDER BY p.id`,
		string(model.VerdictApproveWithConcerns), string(model.ProposalStatusOpen),
		since, runID, tribunalLabel)
	if err != nil {
		return nil, fmt.Errorf("reading unfiled seat conditions: %w", err)
	}
	defer rows.Close()
	var out []UnfiledCondition
	for rows.Next() {
		var (
			id     int
			voters string
		)
		if err := rows.Scan(&id, &voters); err != nil {
			return nil, fmt.Errorf("reading an unfiled seat condition: %w", err)
		}
		out = append(out, UnfiledCondition{
			Proposal: model.FormatProposalID(id), Voters: strings.Split(voters, ", "),
		})
	}
	return out, rows.Err()
}

// unfiledConditionsReason words the refusal: each vote, its seats, and the two
// ways out.
func unfiledConditionsReason(found []UnfiledCondition) string {
	parts := make([]string, len(found))
	for i, c := range found {
		parts[i] = fmt.Sprintf("%s (approve-with-concerns: %s)", c.Proposal, strings.Join(c.Voters, ", "))
	}
	return strings.Join(parts, "; ")
}

// openDispatchSeq is the open dispatch's OpenedSeq, and false when none is open.
func openDispatchSeq(conn *sql.DB, runID int) (int64, bool) {
	tx, err := conn.Begin()
	if err != nil {
		return 0, false
	}
	defer tx.Rollback()
	open, err := db.OpenDispatchTx(tx, runID)
	if err != nil {
		return 0, false
	}
	return open.OpenedSeq, true
}

// conditionsVerdict rules on the close's seat conditions before anything is
// written. It returns the conditions a waiver covers: nil when none were
// unfiled or the check does not apply (no open dispatch to bound the window).
// An unfiled condition with no waiver refuses CONFLICT; a waiver on a bound run
// needs the conductor token and a named writer, and is refused before the
// close writes anything.
func conditionsVerdict(conn *sql.DB, runID int, waiver ConditionsWaiver) ([]UnfiledCondition, error) {
	seq, ok := openDispatchSeq(conn, runID)
	if !ok {
		return nil, nil
	}
	if waiver.Reason != "" {
		if err := waiver.By.require("dispatch close --no-conditions"); err != nil {
			return nil, validationErr("%v", err)
		}
		if err := authorizeConductor(conn, runID, waiver.Token, noConditionsVerb); err != nil {
			return nil, err
		}
	}
	unfiled, err := unfiledConditions(conn, runID, seq)
	if err != nil {
		return nil, err
	}
	if len(unfiled) == 0 {
		return nil, nil
	}
	if waiver.Reason == "" {
		return nil, conflictErr(
			"%s cannot close: %d sealed vote(s) carry approve-with-concerns "+
				"conditions with no linked tribunal issue: %s. File each condition "+
				"with `docket issue create -l tribunal` and `docket vote link`, or "+
				"close with --no-conditions REASON",
			model.FormatRunID(runID), len(unfiled), unfiledConditionsReason(unfiled))
	}
	// The waiver's run note is written after the close commits, so a reason the
	// note would refuse is refused HERE, before anything closes.
	if text := conditionsWaiverText(unfiled, waiver.Reason); len(text) > RunNoteMaxBytes {
		return nil, validationErr(
			"--no-conditions reason makes a %d-byte run note, over the %d-byte cap; "+
				"shorten the reason", len(text), RunNoteMaxBytes)
	}
	return unfiled, nil
}

// conditionsWaiverText is the run note a waived close leaves.
func conditionsWaiverText(waived []UnfiledCondition, reason string) string {
	ids := make([]string, len(waived))
	for i, c := range waived {
		ids[i] = c.Proposal
	}
	return fmt.Sprintf("dispatch close waived unfiled seat conditions on %s: %s",
		strings.Join(ids, ", "), reason)
}

// recordConditionsWaiver writes the run note a waived close leaves. The close
// event already names the waived votes; the note puts the reason where the
// next worker reads it.
func recordConditionsWaiver(conn *sql.DB, runID int, waiver ConditionsWaiver, waived []UnfiledCondition, nowMS int64) error {
	text := conditionsWaiverText(waived, waiver.Reason)
	_, err := AddRunNote(conn, runID, waiver.Token, waiver.By,
		Authority{Kind: AuthorityConductor}, text, nowMS)
	return err
}
