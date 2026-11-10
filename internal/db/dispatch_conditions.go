package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// tribunalLabel is the label a filed seat condition's issue carries.
const tribunalLabel = "tribunal"

// EventAtMS reads the at_ms of the event at seq. A seq with no event is
// ErrNotFound.
func EventAtMS(db *sql.DB, seq int64) (int64, error) {
	var atMS int64
	err := db.QueryRow(`SELECT at_ms FROM events WHERE seq = ?`, seq).Scan(&atMS)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("reading the event at seq %d: %w", seq, err)
	}
	return atMS, nil
}

// UnfiledSeatCondition is one approve-with-concerns cast on a sealed vote that
// no tribunal issue files.
type UnfiledSeatCondition struct {
	ProposalID int
	Voter      string
}

// UnfiledSeatConditions reads one row per approve-with-concerns cast on the
// proposals linked to an issue of the run that are no longer open, were last
// updated at or after sinceMS, and have no linked issue labelled tribunal, in
// proposal then voter-name order. proposals.updated_at is kept to the second,
// so a proposal updated earlier in sinceMS's second counts as updated since.
func UnfiledSeatConditions(db *sql.DB, runID int, sinceMS int64) ([]UnfiledSeatCondition, error) {
	since := time.UnixMilli(sinceMS).UTC().Format(time.RFC3339)
	rows, err := db.Query(
		`SELECT p.id, v.voter_name
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
		  ORDER BY p.id, v.voter_name`,
		string(model.VerdictApproveWithConcerns), string(model.ProposalStatusOpen),
		since, runID, tribunalLabel)
	if err != nil {
		return nil, fmt.Errorf("reading unfiled seat conditions: %w", err)
	}
	return scanRows(rows, "unfiled seat conditions", func(r *sql.Rows) (UnfiledSeatCondition, error) {
		var c UnfiledSeatCondition
		if err := r.Scan(&c.ProposalID, &c.Voter); err != nil {
			return c, fmt.Errorf("reading an unfiled seat condition: %w", err)
		}
		return c, nil
	})
}
