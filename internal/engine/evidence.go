package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// Evidence references on findings (DKT-2451).
//
// A cast's structured findings may cite what each entry rests on:
// `artifact:ARTIFACT-N` for an artifact the run holds, `gate:<name>` for a
// gate result the run recorded. The engine validates every reference against
// THE RUN THE PROPOSAL WAS OPENED FOR when the cast is recorded — before
// db.CastVote runs, because a cast has no amend path — and refuses an unknown
// one with a named reason. It is `step_inputs`' provenance question (DKT-1054:
// what was a step served) asked of a cast: what did this finding rely on.
//
// Core validates that a reference RESOLVES. It never opens the artifact's
// payload or the gate's output to judge whether the evidence supports the
// finding — that would be payload interpretation, which
// docs/design/genericity.md keeps out of core. A resolved reference means
// "this thing exists in this run, and a reader can go look"; the run report
// renders an entry that cites nothing as unsupported, so the two are
// distinguishable in the record without core holding an opinion about either.
//
// THE RUN IS RESOLVED POSITIVELY OR NOT AT ALL. A proposal joins a run through
// one of the two idempotency-key families the engine mints — a vote step's
// (voteIdempotencyPrefix) or a reap acknowledgment's (reapAckRunPrefix). The
// report's third attribution, a conversational ballot whose TEXT names the run
// (DKT-584), is a heuristic fit for a rollup and not for a refusal: a record
// gated on a word match would refuse or admit evidence on the strength of a
// sentence. Such a proposal's casts carry no evidence, and the refusal says so.

// Evidence reference schemes — the part before the first colon.
const (
	// EvidenceArtifact is `artifact:ARTIFACT-N` (a bare N is accepted and
	// rewritten to the canonical spelling before the cast records).
	EvidenceArtifact = "artifact"
	// EvidenceGate is `gate:<name>`, a gate name the run recorded a result
	// for, in any of its steps.
	EvidenceGate = "gate"
)

// ValidateCastEvidence checks every evidence reference in f against the run
// the proposal was opened for, and CANONICALIZES artifact references in place
// (`artifact:3` becomes `artifact:ARTIFACT-3`) so the stored row and the run
// report spell one id one way.
//
// A nil f, or one whose entries cite nothing, passes without a read — an
// ordinary cast is byte-for-byte the cast it always was, refusals included
// (db.CastVote still owns "no such proposal" for it). With evidence present,
// a missing proposal is NOT_FOUND, a proposal bound to no run and every
// unresolvable reference is VALIDATION, each naming the reference and why.
func ValidateCastEvidence(conn *sql.DB, proposalID int, f *model.Findings) error {
	refs := findingEvidence(f)
	if len(refs) == 0 {
		return nil
	}

	if _, err := db.GetProposal(conn, proposalID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return notFoundErr(err, "proposal %s not found", model.FormatProposalID(proposalID))
		}
		return fmt.Errorf("reading %s: %w", model.FormatProposalID(proposalID), err)
	}

	runID, err := proposalRunID(conn, proposalID)
	if err != nil {
		return err
	}
	if runID == 0 {
		return validationErr(
			"evidence references resolve against the run whose vote step or reap "+
				"acknowledgment opened the proposal, and %s was opened by neither; "+
				"casts on it carry no evidence",
			model.FormatProposalID(proposalID))
	}

	for _, ref := range refs {
		canonical, err := resolveEvidence(conn, runID, proposalID, *ref)
		if err != nil {
			return err
		}
		*ref = canonical
	}
	return nil
}

// findingEvidence collects pointers to every evidence reference in f, in the
// findings' own order, so a caller can rewrite each one in place.
func findingEvidence(f *model.Findings) []*string {
	if f == nil {
		return nil
	}
	var out []*string
	for _, list := range [][]model.Finding{f.Blockers, f.Concerns, f.Suggestions} {
		for i := range list {
			for j := range list[i].Evidence {
				out = append(out, &list[i].Evidence[j])
			}
		}
	}
	return out
}

// proposalRunID resolves the run a proposal was opened for through the two
// key families the engine mints, or 0 when it was opened under neither.
func proposalRunID(conn *sql.DB, proposalID int) (int, error) {
	key, found, err := db.IdempotencyKeyOf(conn, db.ScopeVoteCreate, proposalID)
	if err != nil {
		return 0, fmt.Errorf("resolving the run of %s: %w",
			model.FormatProposalID(proposalID), err)
	}
	if !found {
		return 0, nil
	}
	if runID, ok := voteStepRunOf(key); ok {
		return runID, nil
	}
	if runID, ok := reapAckRunOf(key); ok {
		return runID, nil
	}
	return 0, nil
}

// resolveEvidence checks one reference against the run and returns its
// canonical spelling.
func resolveEvidence(conn *sql.DB, runID, proposalID int, ref string) (string, error) {
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return "", validationErr(
			"evidence reference %q is not %s:%s-N or %s:<name>",
			ref, EvidenceArtifact, model.ArtifactIDPrefix, EvidenceGate)
	}
	switch scheme {
	case EvidenceArtifact:
		id, err := model.ParseArtifactID(rest)
		if err != nil {
			return "", validationErr("evidence reference %q: %v", ref, err)
		}
		var artifactRun int
		err = conn.QueryRow(`SELECT run_id FROM artifacts WHERE id = ?`, id).Scan(&artifactRun)
		if errors.Is(err, sql.ErrNoRows) {
			return "", validationErr(
				"evidence reference %q names no recorded artifact", ref)
		}
		if err != nil {
			return "", fmt.Errorf("resolving evidence reference %q: %w", ref, err)
		}
		if artifactRun != runID {
			return "", validationErr(
				"evidence reference %q names an artifact of %s, not of %s, the run %s was opened for",
				ref, model.FormatRunID(artifactRun), model.FormatRunID(runID),
				model.FormatProposalID(proposalID))
		}
		return EvidenceArtifact + ":" + model.FormatArtifactID(id), nil
	case EvidenceGate:
		var n int
		err := conn.QueryRow(
			`SELECT COUNT(*) FROM gate_results WHERE run_id = ? AND gate = ?`,
			runID, rest).Scan(&n)
		if err != nil {
			return "", fmt.Errorf("resolving evidence reference %q: %w", ref, err)
		}
		if n == 0 {
			return "", validationErr(
				"evidence reference %q names no gate result recorded in %s, the run %s was opened for",
				ref, model.FormatRunID(runID), model.FormatProposalID(proposalID))
		}
		return ref, nil
	}
	return "", validationErr(
		"evidence reference %q has unknown scheme %q; want %s:%s-N or %s:<name>",
		ref, scheme, EvidenceArtifact, model.ArtifactIDPrefix, EvidenceGate)
}
