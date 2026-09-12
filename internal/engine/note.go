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

// DKT-1079 — the run note: the dispatcher's channel into every packet.
//
// A packet draws from frozen and recorded state only (render.go's exhaustive
// list), and until this verb every writable source in it was STEP-SCOPED:
// `step resolve -m` reaches the step it rules on and the round it authorizes.
// A fact about the WHOLE RUN had no channel at all. RUN-70's conductor
// gate-probed before dispatch, found `tests` failing on clean HEAD, asked the
// operator, got "file issue, override-pass", filed DKT-1075 — and dispatched
// packets that carried none of it, because issue comments never render
// (§6.6), the issue body froze at activation, and no step had a routing
// record yet. The executor stashed, re-ran the suite, re-derived the same
// failure, and filed DKT-1076: a duplicate the conductor then spent three more
// calls closing.
//
// `run note add` records the statement ONCE against the run. Context assembly
// reads it as its sixth source, the default template renders it as
// `== RUN NOTE N` beside the request, and `step context` carries it as
// `notes` so a contract can name it. It is append-only and event-logged,
// like every other post-activation move of a packet's premises (repin, scope
// refresh): what a worker was told is part of the record.

// RunNoteMaxBytes caps one note. A note rides EVERY packet of the run, so an
// oversized one is paid for by every step; 16 KiB — MetadataMaxBytes' figure,
// for the same reason — is three orders of magnitude above the motivating use
// (a gate name, why it is pre-existing, an issue id, and a disposition: a few
// hundred bytes) and small enough that a note stays a note. Bulk detail goes
// in the issue the note cites.
const RunNoteMaxBytes = 16 << 10

// RunNote is one recorded note, in the ONE shape it has everywhere: the verb's
// answer, the `run note list` row, and the bundle's `notes` element are all
// this, so a consumer parses one shape wherever a note appears.
type RunNote struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
	// RecordedAtMS dates the note, so a reader of a packet can tell a note
	// recorded before dispatch from one added mid-run.
	RecordedAtMS int64 `json:"recorded_at_ms"`
}

// AddRunNote records one note against a run, event-logged, in one
// transaction.
//
// It refuses an empty note (a note with no content steers nothing), one over
// the cap, an unknown run, and a TERMINAL run: a run that is done or abandoned
// renders no more packets, so a note against it would be a statement nobody
// will ever be told, recorded as if somebody would. A `planning` run is fine —
// the motivating note is written BEFORE the first dispatch, and the verb must
// not force a dispatcher to activate first.
//
// One trailing newline is dropped, so a note fed from a file renders exactly
// as the same words passed inline; nothing else about the text is touched.
func AddRunNote(conn *sql.DB, runID int, text string, nowMS int64) (*RunNote, error) {
	text = strings.TrimSuffix(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil, validationErr(
			"the note is empty; a note with no content records nothing")
	}
	if len(text) > RunNoteMaxBytes {
		return nil, validationErr(
			"the note is %d bytes, over the %d-byte cap; a note rides every packet "+
				"of the run, so record the detail on the issue it cites and keep the "+
				"note to the ruling", len(text), RunNoteMaxBytes)
	}

	run, err := db.GetRun(conn, runID)
	if errors.Is(err, db.ErrRunNotFound) {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
	}
	if err != nil {
		return nil, err
	}
	if run.Status.Terminal() {
		return nil, conflictErr(
			"run %s is %s; it renders no more packets, so a note against it "+
				"would reach nobody", run.Ref(), run.Status)
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("recording the run note: %w", err)
	}
	defer tx.Rollback()

	id, err := db.InsertRunNoteTx(tx, db.RunNote{
		RunID: runID, Text: text, CreatedAtMS: nowMS,
	})
	if err != nil {
		return nil, err
	}

	// The event carries the note VERBATIM, as `step-annotated` carries its
	// annotation: the feed must be able to say what every later worker was
	// told without a join against the note table.
	data, err := json.Marshal(map[string]any{"note": id, "text": text})
	if err != nil {
		return nil, fmt.Errorf("recording the run note: %w", err)
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventRunNoteAdded, RunID: runID, Data: string(data), AtMS: nowMS,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("recording the run note: %w", err)
	}
	return &RunNote{ID: id, Text: text, RecordedAtMS: nowMS}, nil
}

// ListRunNotes returns a run's notes in the order they render.
func ListRunNotes(conn *sql.DB, runID int) ([]RunNote, error) {
	if _, err := db.GetRun(conn, runID); err != nil {
		if errors.Is(err, db.ErrRunNotFound) {
			return nil, notFoundErr(err, "run %s not found", model.FormatRunID(runID))
		}
		return nil, err
	}
	rows, err := db.ListRunNotes(conn, runID)
	if err != nil {
		return nil, err
	}
	out := make([]RunNote, 0, len(rows))
	for _, n := range rows {
		out = append(out, RunNote{ID: n.ID, Text: n.Text, RecordedAtMS: n.CreatedAtMS})
	}
	return out, nil
}
