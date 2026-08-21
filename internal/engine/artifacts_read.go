package engine

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// The READ surface for what a step produced.
//
// An ACTION step's verdict and an aggregate's held-cluster payload live in the
// `artifacts` table, and until this existed no verb reached them: `step show`
// rendered the row, `step context` rendered a step's INPUTS, and the run
// report's artifact index gave sizes and hashes but never a body. Reading an
// action's result meant opening .docket/issues.db with sqlite by hand — which
// is what RUN-1's conductor had to do, and which is not a surface an operator
// should need.
//
// Two shapes, because there are two questions. "What did this step produce?"
// wants a list with sizes and no bodies, so a step with a megabyte of findings
// is still readable. "What does THIS artifact say?" wants the body and payload
// in full. Serving both from one call would either truncate the answer to the
// second question or flood the answer to the first.

// StepArtifact is one artifact as a read verb reports it.
//
// The `Body`/`Payload` fields are populated only by ReadArtifact; the listing
// leaves them empty and reports sizes instead. Sharing the type keeps the
// `artifact` reference and `kind` spelled identically in both, so a caller can
// list and then fetch without re-deriving anything.
type StepArtifact struct {
	// Artifact is the ARTIFACT-N reference, matching the run report's
	// artifact index (R6) so the two surfaces name the same thing the same
	// way.
	Artifact string `json:"artifact"`
	Kind     string `json:"kind"`
	// Producer is the producing step's instance, e.g. `review@0#2`. Empty for
	// a run-scoped artifact with no producing step.
	Producer string `json:"producer,omitempty"`
	SHA256   string `json:"sha256"`
	// Bytes and PayloadBytes are the SIZES, always reported. A listing gives
	// these instead of the content so an operator can see what is there before
	// asking for a body that may be up to db.ArtifactMaxBytes.
	Bytes        int `json:"bytes"`
	PayloadBytes int `json:"payload_bytes,omitempty"`
	// Stub marks an artifact the S3/S4 stub runner produced — a computation
	// that did not actually run. `omitempty` matches the db.Artifact rule: an
	// artifact from a real run serializes with no `stub` key at all.
	Stub bool `json:"stub,omitempty"`
	// Supersedes names the artifact this one REVISES, e.g. `ARTIFACT-71`
	// (DKT-70) — see db.Artifact.Supersedes. `omitempty` keeps an original's
	// bytes exactly as they were.
	Supersedes  string `json:"supersedes,omitempty"`
	CreatedAtMS int64  `json:"created_at_ms"`

	// Body and Payload are the CONTENT, present only on a single-artifact
	// read. Both are omitempty so a listing's entries carry neither.
	Body    string `json:"body,omitempty"`
	Payload string `json:"payload,omitempty"`
}

// ListStepArtifacts reports what one step produced, WITHOUT the bodies.
//
// IT WRITES NOTHING, matching every other read verb — no reap, no lease touch.
//
// A step that produced nothing is not an error: it returns an empty list. Many
// steps legitimately produce no artifact, and turning that into a failure
// would make the verb unusable as the "what is here?" probe it exists to be.
func ListStepArtifacts(conn *sql.DB, stepID int) ([]StepArtifact, error) {
	// The step must EXIST, even though having no artifacts is fine. Without
	// this, a typo'd step reference reports "no artifacts" — an answer that
	// reads as fact and is actually a missing step.
	step, err := db.GetStep(conn, stepID)
	if errors.Is(err, db.ErrStepNotFound) {
		return nil, notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return nil, err
	}

	artifacts, err := db.ListStepArtifacts(conn, stepID)
	if err != nil {
		return nil, err
	}

	out := make([]StepArtifact, 0, len(artifacts))
	for _, a := range artifacts {
		out = append(out, StepArtifact{
			Artifact:     fmt.Sprintf("ARTIFACT-%d", a.ID),
			Kind:         a.Kind,
			Producer:     step.Instance,
			SHA256:       a.SHA256,
			Bytes:        len(a.Body),
			PayloadBytes: len(a.Payload),
			Stub:         a.Stub,
			Supersedes:   artifactRef(a.Supersedes),
			CreatedAtMS:  a.CreatedAtMS,
		})
	}
	return out, nil
}

// ReadArtifact reads ONE artifact in full, body and payload included.
//
// IT WRITES NOTHING.
//
// Reached by numeric id (the N in ARTIFACT-N), which is what the listing above
// and the run report's index both print — so the reference an operator already
// has on screen is the one this takes.
func ReadArtifact(conn *sql.DB, artifactID int) (*StepArtifact, error) {
	var (
		a          db.Artifact
		stepID     sql.NullInt64
		payload    sql.NullString
		stub       sql.NullInt64
		supersedes sql.NullInt64
	)
	err := conn.QueryRow(
		`SELECT id, run_id, step_id, kind, body, payload, sha256, stub, supersedes,
		        created_at_ms
		   FROM artifacts WHERE id = ?`, artifactID).Scan(
		&a.ID, &a.RunID, &stepID, &a.Kind, &a.Body, &payload, &a.SHA256,
		&stub, &supersedes, &a.CreatedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFoundErr(sql.ErrNoRows,
			"artifact ARTIFACT-%d not found", artifactID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading artifact ARTIFACT-%d: %w", artifactID, err)
	}

	out := &StepArtifact{
		Artifact:     fmt.Sprintf("ARTIFACT-%d", a.ID),
		Kind:         a.Kind,
		SHA256:       a.SHA256,
		Bytes:        len(a.Body),
		PayloadBytes: len(payload.String),
		Stub:         stub.Int64 != 0,
		Supersedes:   supersedesRef(supersedes),
		CreatedAtMS:  a.CreatedAtMS,
		Body:         a.Body,
		Payload:      payload.String,
	}

	// The producing step's instance, when there is one. A run-scoped artifact
	// has a NULL step_id and simply reports no producer, rather than an
	// invented one.
	if stepID.Valid {
		if producer, err := db.GetStep(conn, int(stepID.Int64)); err == nil {
			out.Producer = producer.Instance
		}
	}
	return out, nil
}

// artifactRef renders an optional artifact id as `ARTIFACT-N`, or "" when there
// is none. The two spellings below take the two shapes the reads produce — a
// decoded pointer and a raw NULL column — so neither call site has to unpack
// the absence itself.
func artifactRef(id *int) string {
	if id == nil {
		return ""
	}
	return fmt.Sprintf("ARTIFACT-%d", *id)
}

func supersedesRef(id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	return fmt.Sprintf("ARTIFACT-%d", id.Int64)
}
