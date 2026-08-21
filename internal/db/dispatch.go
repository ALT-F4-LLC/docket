package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Dispatch manifests and the write-reap acknowledgment ledger — the storage half
// of docs/tdd/runs-dispatch.md §5 and §6.
//
// NOTHING IN HERE INTERPRETS A ROW'S CONTENT. `row_json` is stored and compared
// as BYTES, `class` is the opaque string §11.1 makes it, and `close_reason` is a
// short engine-authored word. The scheduling meaning lives in internal/engine;
// this file is the rows and their CAS.

// Dispatch statuses. A dispatch is `open` until exactly one of the two closing
// paths moves it, and the status is what the partial unique index keys on — so
// these strings are load-bearing rather than decorative.
const (
	// DispatchOpen is the state `idx_dispatches_one_open` admits one of per run.
	DispatchOpen = "open"
	// DispatchClosed is `dispatch close`: the relay reconciled its batch.
	DispatchClosed = "closed"
	// DispatchAbandoned is `dispatch abandon` or the TTL's lazy auto-abandon:
	// the batch was given up on rather than reconciled.
	DispatchAbandoned = "abandoned"
)

// Close reasons core writes. They are a small closed vocabulary because
// `run report` and `events list` render them, and a free-text reason from the
// engine's own paths would make the feed inconsistent with itself. A caller's
// `--reason` on `abandon` rides in the EVENT's data, not here (P21).
const (
	// CloseReasonTTL is P14's lazy auto-abandon: the manifest outlived
	// `dispatch.ttl` and `next` retired it.
	CloseReasonTTL = "ttl"
	// CloseReasonReconciled is the ordinary `dispatch close` (P18).
	CloseReasonReconciled = "reconciled"
	// CloseReasonAcceptedMissingUsage is P19: closed over missing-usage
	// discrepancies, with the acceptance RECORDED rather than implied.
	CloseReasonAcceptedMissingUsage = "accepted-missing-usage"
	// CloseReasonOperator is `dispatch abandon` driven by a person (P21).
	CloseReasonOperator = "abandoned"
)

// ErrNoOpenDispatch is returned when a verb that needs an open manifest finds
// none. It is a sentinel so the CLI maps it once rather than matching a message.
var ErrNoOpenDispatch = errors.New("no dispatch is open")

// Dispatch is one manifest's row.
type Dispatch struct {
	ID     int
	RunID  int
	Status string
	// OpenedSeq is the event seq at open time — the manifest's place in the log,
	// and §6's boundary for "reaps this relay has not yet seen" (P2).
	OpenedSeq   int64
	ExpiresMS   int64
	ClosedAtMS  *int64
	CloseReason string
	CreatedAtMS int64
	RowVersion  int
}

// Expired reports whether the manifest has outlived its TTL (P12).
//
// It is a method on the row rather than a query so the ONE definition of expiry
// serves the lazy abandon, the refusal's message, and any read verb that renders
// a manifest — three callers that must not be able to disagree about whether a
// dispatch is still live.
func (d *Dispatch) Expired(nowMS int64) bool { return nowMS >= d.ExpiresMS }

// DispatchRow is one stored manifest row: the canonical bytes, their hash, and
// the identity they describe.
//
// Both `RowJSON` and `RowSHA256` are stored. `verify` derives its stageless
// comparison from `RowJSON`, first asserting the bytes still hash to
// `RowSHA256` — the integrity check on the stored pair — and the spawn guard
// compares proposed rows against `RowSHA256` verbatim, stage included. The
// bytes are what a refusal SHOWS the operator: the differing row rather than
// a report that two rows differ.
type DispatchRow struct {
	Position  int
	StepID    int
	Instance  string
	RowJSON   string
	RowSHA256 string
}

// InsertDispatchTx opens a manifest, and C1 IS THE INSERT.
//
// `idx_dispatches_one_open` is a partial UNIQUE index on (run_id) WHERE
// status='open', so two relays racing produce one row and one constraint
// violation — never a check-then-insert's window, in which both SELECT no open
// dispatch and both then INSERT one. The loser gets ErrDispatchAlreadyOpen and
// its whole computation is DISCARDED rather than merged (§5.4).
func InsertDispatchTx(tx *sql.Tx, runID int, openedSeq, expiresMS, nowMS int64) (int, error) {
	res, err := tx.Exec(
		`INSERT INTO dispatches (run_id, status, opened_seq, expires_ms, created_at_ms)
		 VALUES (?, ?, ?, ?, ?)`,
		runID, DispatchOpen, openedSeq, expiresMS, nowMS,
	)
	if err != nil {
		if isUniqueOrPKConflict(err) {
			return 0, ErrDispatchAlreadyOpen
		}
		return 0, fmt.Errorf("opening a dispatch for %s: %w",
			model.FormatRunID(runID), err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("opening a dispatch for %s: %w",
			model.FormatRunID(runID), err)
	}
	return int(id), nil
}

// ErrDispatchAlreadyOpen is C1's loser — the CONFLICT of P6.
var ErrDispatchAlreadyOpen = errors.New("a dispatch is already open for this run")

// InsertDispatchRowTx stores one manifest row at its position.
func InsertDispatchRowTx(tx *sql.Tx, dispatchID int, row DispatchRow) error {
	_, err := tx.Exec(
		`INSERT INTO dispatch_rows
		   (dispatch_id, position, step_id, instance, row_json, row_sha256)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		dispatchID, row.Position, row.StepID, row.Instance, row.RowJSON, row.RowSHA256,
	)
	if err != nil {
		return fmt.Errorf("recording manifest row %d: %w", row.Position, err)
	}
	return nil
}

// OpenDispatchTx reads the run's open manifest, or ErrNoOpenDispatch.
//
// It is the probe P24 runs and the one D2 asks about, so it is ONE query with
// one definition of "open": the status the partial index keys on.
func OpenDispatchTx(tx *sql.Tx, runID int) (*Dispatch, error) {
	return scanDispatch(tx.QueryRow(
		`SELECT id, run_id, status, opened_seq, expires_ms, closed_at_ms,
		        close_reason, created_at_ms, row_version
		   FROM dispatches WHERE run_id = ? AND status = ?`,
		runID, DispatchOpen))
}

// GetDispatchTx reads one manifest by id, whatever its status — the read a
// closing verb makes after its CAS to report what actually happened.
func GetDispatchTx(tx *sql.Tx, id int) (*Dispatch, error) {
	return scanDispatch(tx.QueryRow(
		`SELECT id, run_id, status, opened_seq, expires_ms, closed_at_ms,
		        close_reason, created_at_ms, row_version
		   FROM dispatches WHERE id = ?`, id))
}

func scanDispatch(row *sql.Row) (*Dispatch, error) {
	var (
		d      Dispatch
		closed sql.NullInt64
		reason sql.NullString
	)
	err := row.Scan(&d.ID, &d.RunID, &d.Status, &d.OpenedSeq, &d.ExpiresMS,
		&closed, &reason, &d.CreatedAtMS, &d.RowVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoOpenDispatch
	}
	if err != nil {
		return nil, fmt.Errorf("reading a dispatch: %w", err)
	}
	if closed.Valid {
		ms := closed.Int64
		d.ClosedAtMS = &ms
	}
	d.CloseReason = reason.String
	return &d, nil
}

// ListDispatchRowsTx reads a manifest's rows IN POSITION ORDER.
//
// The order is the manifest (P1: "records the resulting rows in order"), so it
// is an ORDER BY rather than an insertion-order assumption: `verify` compares
// position by position, and a row set that came back in rowid order would make
// the comparison depend on how SQLite happened to store them.
func ListDispatchRowsTx(tx *sql.Tx, dispatchID int) ([]DispatchRow, error) {
	rows, err := tx.Query(
		`SELECT position, step_id, instance, row_json, row_sha256
		   FROM dispatch_rows WHERE dispatch_id = ? ORDER BY position`, dispatchID)
	if err != nil {
		return nil, fmt.Errorf("reading manifest rows: %w", err)
	}
	return scanRows(rows, "manifest rows", func(r *sql.Rows) (DispatchRow, error) {
		var row DispatchRow
		if err := r.Scan(&row.Position, &row.StepID, &row.Instance, &row.RowJSON, &row.RowSHA256); err != nil {
			return DispatchRow{}, fmt.Errorf("reading a manifest row: %w", err)
		}
		return row, nil
	})
}

// CloseDispatchTx is C2: close and abandon are both CAS on (id, status='open').
//
// It reports whether it MOVED the row. A close racing the TTL abandon matches
// zero rows and learns so, which is what lets the caller report `CONFLICT`
// naming what actually happened rather than "not open" (P22) — the loser needs
// to know WHY, and the row it then reads says.
func CloseDispatchTx(tx *sql.Tx, id int, status, reason string, nowMS int64) (bool, error) {
	res, err := tx.Exec(
		`UPDATE dispatches
		    SET status = ?, close_reason = ?, closed_at_ms = ?,
		        row_version = row_version + 1
		  WHERE id = ? AND status = ?`,
		status, reason, nowMS, id, DispatchOpen,
	)
	if err != nil {
		return false, fmt.Errorf("closing dispatch %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("closing dispatch %d: %w", id, err)
	}
	return n > 0, nil
}

// RunEverDispatchedTx answers §5.8 D2's SCOPE as the 2026-08-03 review fixed it:
// has this run EVER opened a dispatch?
//
// The question is deliberately about HISTORY rather than about an open manifest.
// A relay that ever dispatched is accountable for every step of that run,
// including the ones it spawned outside a manifest — which is the drift D6
// exists to catch. A run no relay ever drove has nobody owing usage, which is
// what keeps a solo rehearsal and a human-only demo refusal-free.
//
// It reads the `dispatches` table rather than the event log because the row
// outlives nothing: a dispatch that was opened and closed leaves its row, and
// the row is the cheaper and more direct record of the same fact.
func RunEverDispatchedTx(tx *sql.Tx, runID int) (bool, error) {
	var exists bool
	err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM dispatches WHERE run_id = ?)`, runID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("reading dispatch history for %s: %w",
			model.FormatRunID(runID), err)
	}
	return exists, nil
}

// DispatchTTLTx and DispatchGraceTx resolve §4.11's two durations inside the
// caller's transaction, for the reason configValueTx exists: internal/db caps
// the pool at ONE connection, so a pool read from inside an open transaction
// deadlocks rather than failing.
func DispatchTTLTx(tx *sql.Tx, projectID int) (time.Duration, error) {
	return durationConfigTx(tx, projectID, KeyDispatchTTL)
}

// DispatchGraceTx is D1's window: how long a claimed step may go unrecorded.
func DispatchGraceTx(tx *sql.Tx, projectID int) (time.Duration, error) {
	return durationConfigTx(tx, projectID, KeyDispatchGrace)
}

func durationConfigTx(tx *sql.Tx, projectID int, key string) (time.Duration, error) {
	value, err := configValueTx(tx, projectID, key)
	if err != nil {
		return 0, err
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parsing %s %q: %w", key, value, err)
	}
	return d, nil
}

// ---- The write-reap acknowledgment ledger (§6.4) ---------------------------

// ReapAck is one unacknowledged-or-acknowledged reap.
type ReapAck struct {
	ID       int
	RunID    int
	StepID   int
	Class    string
	Instance string
	// ReapedSeq is the `seq` of the `lease-reaped` event this ack is OF. §2 says
	// the relay acknowledges "the `reaped` event", so the event's seq is the
	// ack's identity — which makes the ack idempotent for free (C7) and a forged
	// ack impossible to construct without naming a real reap.
	ReapedSeq   int64
	AckedAtMS   *int64
	AckedBy     string
	CreatedAtMS int64
}

// Acknowledgers — A8's closed pair. The value records the acknowledging VERB
// and never a user identity, because core has no identity model.
const (
	// AckByGuardSpawn is `guard spawn --ack-reap`, group 3's entry point.
	AckByGuardSpawn = "guard-spawn"
	// AckByDispatchOpen is `dispatch open --ack-reap`, this group's.
	AckByDispatchOpen = "dispatch-open"
)

// InsertReapAckTx records a reap as unacknowledged, in the reap's own
// transaction (A16, A19).
//
// The caller is responsible for A16's OTHER half — calling this only for a class
// whose `[limits] max` is finite — because that decision needs the scheduler's
// merged limits, which the storage layer does not have and must not guess at.
func InsertReapAckTx(tx *sql.Tx, ack ReapAck, nowMS int64) error {
	_, err := tx.Exec(
		`INSERT INTO reap_acks (run_id, step_id, class, reaped_seq, created_at_ms)
		 VALUES (?, ?, ?, ?, ?)`,
		ack.RunID, ack.StepID, ack.Class, ack.ReapedSeq, nowMS,
	)
	if err != nil {
		if isUniqueOrPKConflict(err) {
			// UNIQUE(reaped_seq): the same reap event cannot be recorded twice.
			// Reaching here means a caller re-derived an ack row for an event
			// that already has one, which is a no-op rather than a failure — the
			// row that exists says exactly what this one would have.
			return nil
		}
		return fmt.Errorf("recording the reap of step %s for acknowledgment: %w",
			model.FormatStepID(ack.StepID), err)
	}
	return nil
}

// UnacknowledgedReapsTx lists a run's outstanding reaps, oldest first.
//
// It is the query behind A12's predicate AND behind the human-readable reason
// `next` prints, so the two cannot name different rows: a headroom denial with
// nothing running is baffling unless the same rows that caused it are the ones
// reported.
//
// The `instance` join is for the MESSAGE. The predicate needs only the class,
// but a refusal that named `STEP-7` rather than `implement@0` would make an
// operator look the step up to understand a sentence about their own run.
func UnacknowledgedReapsTx(tx *sql.Tx, runID int) ([]ReapAck, error) {
	rows, err := tx.Query(
		`SELECT a.id, a.run_id, a.step_id, a.class, a.reaped_seq, a.created_at_ms,
		        COALESCE(s.instance, '')
		   FROM reap_acks a
		   LEFT JOIN steps s ON s.id = a.step_id
		  WHERE a.run_id = ? AND a.acked_at_ms IS NULL
		  ORDER BY a.reaped_seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading unacknowledged reaps for %s: %w",
			model.FormatRunID(runID), err)
	}
	return scanRows(rows,
		fmt.Sprintf("unacknowledged reaps for %s", model.FormatRunID(runID)),
		func(r *sql.Rows) (ReapAck, error) {
			var a ReapAck
			if err := r.Scan(&a.ID, &a.RunID, &a.StepID, &a.Class, &a.ReapedSeq,
				&a.CreatedAtMS, &a.Instance); err != nil {
				return ReapAck{}, fmt.Errorf("reading an unacknowledged reap: %w", err)
			}
			return a, nil
		})
}

// AckReapResult is what an acknowledgment did, so the caller can tell A10's
// idempotent success from A9's forgery refusal without re-querying.
type AckReapResult int

const (
	// AckRecorded is the first acknowledgment of a real reap.
	AckRecorded AckReapResult = iota
	// AckAlreadyDone is A10: a second ack of the same seq succeeds and changes
	// nothing, so a relay retrying its hook does not fail.
	AckAlreadyDone
	// AckNoSuchReap is A9: the seq names no unacknowledged-or-acknowledged reap
	// of this run. The caller raises VALIDATION_ERROR — an ack must name a real
	// reap, and this is the forgery point.
	AckNoSuchReap
)

// AckReapTx is A7: CAS on `acked_at_ms IS NULL` for the row whose `reaped_seq`
// matches, SCOPED TO THE RUN.
//
// The run scope is A9's other half and it is not incidental: an ack naming
// another run's reap is a forgery in exactly the way naming a non-reap is, and a
// CAS without the scope would let a relay clear a hold on a run it is not
// driving.
//
// The three-way return is what makes A10 and A9 distinguishable. A CAS that
// matched zero rows means EITHER already-acknowledged OR no-such-reap, and those
// need opposite answers — one is a success, the other is a refusal — so the
// existence probe runs on the zero-rows path rather than being inferred.
func AckReapTx(tx *sql.Tx, runID int, seq int64, ackedBy string, nowMS int64) (AckReapResult, error) {
	res, err := tx.Exec(
		`UPDATE reap_acks SET acked_at_ms = ?, acked_by = ?
		  WHERE run_id = ? AND reaped_seq = ? AND acked_at_ms IS NULL`,
		nowMS, ackedBy, runID, seq,
	)
	if err != nil {
		return AckNoSuchReap, fmt.Errorf("acknowledging reap %d: %w", seq, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return AckNoSuchReap, fmt.Errorf("acknowledging reap %d: %w", seq, err)
	}
	if n > 0 {
		return AckRecorded, nil
	}

	var exists bool
	if err := tx.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM reap_acks WHERE run_id = ? AND reaped_seq = ?)`,
		runID, seq).Scan(&exists); err != nil {
		return AckNoSuchReap, fmt.Errorf("acknowledging reap %d: %w", seq, err)
	}
	if exists {
		return AckAlreadyDone, nil
	}
	return AckNoSuchReap, nil
}
