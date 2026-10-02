package db

import (
	"database/sql"
	"fmt"
)

// RunNote is one standing statement recorded against a run by whoever drives
// it (DKT-1079): a fact the run's workers need that no other packet source
// can carry — a gate known to fail on clean HEAD, the issue tracking it, and
// the disposition the operator already gave. Every packet the run renders
// after the note lands carries it, for every step of every issue in the run.
//
// The note is RUN-SCOPED BY CONSTRUCTION — the run_id foreign key is the
// whole scope rule, exactly gate_override_grants' and stale_target_waivers'
// shape. It is also APPEND-ONLY: no edit, no delete. A packet is the record
// of what a worker was told, and a note that could be rewritten after the
// fact would make two renders of one step disagree about that with nothing
// in the ledger explaining the difference. A ruling that changes is recorded
// as a second note, which renders after the first.
type RunNote struct {
	ID          int
	RunID       int
	Text        string
	CreatedAtMS int64
}

// InsertRunNoteTx records one note and returns its id.
//
// It takes a transaction because the verb records the note and its
// `run-note-added` event together: a note with no event would be a packet
// change no feed reader can attribute, and an event with no note would name a
// ruling that renders nowhere.
func InsertRunNoteTx(tx *sql.Tx, n RunNote) (int, error) {
	res, err := tx.Exec(
		`INSERT INTO run_notes (run_id, text, created_at_ms) VALUES (?, ?, ?)`,
		n.RunID, n.Text, n.CreatedAtMS)
	if err != nil {
		return 0, fmt.Errorf("recording the run note: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading the run note id: %w", err)
	}
	return int(id), nil
}

// ListRunNotesTx returns every note recorded for one run, in insertion order
// — the order they render in, since a later note may qualify an earlier one.
//
// It takes a transaction because its main consumer is context assembly, which
// reads every source inside the claim's own transaction so a claimant sees
// one consistent run state (engine-core §8's "one atomic mediation").
func ListRunNotesTx(tx *sql.Tx, runID int) ([]RunNote, error) {
	rows, err := tx.Query(
		`SELECT id, run_id, text, created_at_ms
		   FROM run_notes WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading run notes: %w", err)
	}
	return scanRunNotes(rows)
}

// ListRunNotes is ListRunNotesTx on the pooled connection, for the read verb.
func ListRunNotes(conn *sql.DB, runID int) ([]RunNote, error) {
	rows, err := conn.Query(
		`SELECT id, run_id, text, created_at_ms
		   FROM run_notes WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading run notes: %w", err)
	}
	return scanRunNotes(rows)
}

func scanRunNotes(rows *sql.Rows) ([]RunNote, error) {
	return scanRows(rows, "run notes", func(r *sql.Rows) (RunNote, error) {
		var n RunNote
		if err := r.Scan(&n.ID, &n.RunID, &n.Text, &n.CreatedAtMS); err != nil {
			return RunNote{}, fmt.Errorf("reading a run note: %w", err)
		}
		return n, nil
	})
}
