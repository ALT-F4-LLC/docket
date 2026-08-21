package engine

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Post-completion annotation (DKT-35).
//
// A step's record freezes when the saga routes it, but some facts about the
// work become true only LATER — the canonical case being integration: a
// conductor that rebases or cherry-picks a writer's commit mints a NEW sha,
// and every run record citing the writer's own becomes gc-eligible prose the
// moment the worktree is swept. The records' whole job is to let a later
// reader re-check a verdict, and their anchor dissolved.
//
// AnnotateStep is the durable channel for such facts: an opaque KV merge onto
// a FINISHED step's metadata, event-logged, no token — the lease retired with
// the artifact, and this is an operator-side act about the record, not a
// completion. The bag is opaque to core exactly as `--metadata` at complete
// is (genericity.md): the engine stores what it is handed and interprets no
// key inside it.

// AnnotateStep merges opaque metadata onto a finished step's record.
//
// It refuses a step that has not reached a terminal status: a live step's
// metadata lands with its record (`step complete --metadata`), under its
// holder's token, and a side channel into an in-flight record would bypass
// exactly that authorization.
func AnnotateStep(conn *sql.DB, stepID int, metadata string, nowMS int64) (*db.Step, error) {
	if metadata == "" {
		return nil, validationErr("--metadata is required; an annotation with no content records nothing")
	}
	if err := validateMetadataSize(metadata); err != nil {
		return nil, err
	}
	if _, err := DecodeMetadataBag(metadata, "metadata"); err != nil {
		return nil, err
	}

	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return nil, notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return nil, err
	}
	if !db.StepTerminal(step.Status) {
		return nil, conflictErr(
			"step %s is %s; annotate applies to a finished step — a live step's "+
				"metadata lands with its record", step.Instance, step.Status)
	}

	merged, err := mergeMetadata(step.Metadata, metadata)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("annotating %s: %w", step.Instance, err)
	}
	defer tx.Rollback()

	if err := db.SetStepMetadataTx(tx, step.ID, merged, nowMS); err != nil {
		return nil, err
	}
	// The event carries the annotation VERBATIM: the record of what was added
	// must survive a later annotation overwriting the same key.
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepAnnotated, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
		Data: metadata, AtMS: nowMS,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("annotating %s: %w", step.Instance, err)
	}

	updated, err := db.GetStep(conn, stepID)
	if err != nil {
		return nil, err
	}
	return updated, nil
}
