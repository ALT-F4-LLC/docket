package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// THE VERIFIED INTEGRATION ANNOTATION — `step annotate STEP-N --integrated-sha SHA`.
//
// A write-class step's landed content can diverge from the commit it recorded:
// a cherry-pick whose conflict the conductor resolved by hand, an operator-ruled
// patch on top of a gate failure, any post-hoc edit before integration. Two
// records then go stale at once, and until this verb neither had a sanctioned
// way back once the step was done:
//
//   - THE CLOSE SIDE. `dispatch close` verifies each write-class step's recorded
//     commit against the shared branch (dispatch_integration.go) — ancestry,
//     then patch-equivalence. A hand-resolved cherry-pick is neither by
//     construction, so the only way through was `--skip-integration-check
//     REASON`, a per-close waiver that vouches for EVERY candidate of the close,
//     not the one commit the relay actually resolved (RUN-95: closes needing
//     2-10 refusals each; one close missed 30 write commits outright).
//   - THE REVIEW SIDE. Every downstream packet renders from the step's newest
//     `issue.diff` artifact and binds its target to that record's `head`. The
//     record is written once and re-derived by nothing; `resolve --as retry`
//     refuses a done step, and `--metadata '{"integrated_sha":...}'` lands on
//     the step ROW, which neither the packet resolver nor the stale-target
//     advisory reads. Three RUN-95 issues' review rows rendered a stale diff
//     indefinitely.
//
// This verb lands both sides from ONE fact the engine verifies itself. The
// operator names the commit the shared branch now carries for this step; the
// engine establishes that it IS reachable from the shared checkout's HEAD
// (IsAncestorFn — the same probe the close and the advisory ask), and only
// then re-records the step's `issue.diff` from that commit's own patch
// (CommitPatchFn), superseding the stale record exactly as the `--worktree`
// re-pin does (diff_repin.go) — so the packets bind to the resolved tree from
// then on with no resolver change, and the close finds a recorded head that is
// an ancestor and accepts it, marking the verdict `resolved` for the audit
// trail (integrationCandidatesTx reads the record's `resolved_from`).
//
// TRUST STAYS ON THE ENGINE'S SIDE. The annotation NAMES a candidate; the
// engine establishes the integration. A sha the shared checkout does not carry
// is refused outright, never recorded as "the operator said so" — that is what
// keeps this distinct from free-form metadata, which the close's contract
// forbids trusting. `--skip-integration-check` stays for a commit with no
// resolution to point at.
//
// THE SHA NAMES ONE COMMIT WHOSE PATCH IS THE STEP'S LANDED WORK. The re-
// recorded diff is that commit's own patch against its parent (`git diff-tree`),
// scoped the way every `issue.diff` is: a cherry-pick, resolved or not, is one
// commit, and that is the shape this exists for. A merge commit shows no patch
// under `diff-tree -p` and records an empty body; name the cherry-pick instead.

// IntegrationAnnotation is what one `--integrated-sha` annotation did.
type IntegrationAnnotation struct {
	// IntegratedSHA is the verified commit, as recorded.
	IntegratedSHA string `json:"integrated_sha"`
	// ResolvedFrom is the head the step's previous `issue.diff` record named —
	// the commit the landed content diverged from.
	ResolvedFrom string `json:"resolved_from,omitempty"`
	// Repin is the `issue.diff` re-record this annotation performed, in the
	// shape the `--worktree` re-pin reports; Unchanged when the step's record
	// already named this sha at this body.
	Repin *IssueDiffRepin `json:"issue_diff_repin,omitempty"`
}

// fullCommitSHA is the one form accepted: the recorded heads are full ids, the
// close keys its prior-acceptance record on `STEP@sha`, and a prefix that
// resolves today may be ambiguous once the branch grows.
var fullCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// MetadataKeyIntegratedSHA is the step-row key the annotation also writes, so
// the convention the docket-run skill already followed by hand (`--metadata
// '{"integrated_sha": ...}'`) keeps holding for readers of the row.
const MetadataKeyIntegratedSHA = "integrated_sha"

// ResolutionIntegratedSHA is the `resolution` the re-record's
// `issue-diff-repinned` event carries, distinguishing it from a re-pin that
// rode a `step resolve`.
const ResolutionIntegratedSHA = "integrated-sha"

// AnnotateIntegration records a verified integration for a finished write-class
// step: the sha's ancestry against the shared checkout's HEAD is established
// first, then the step's `issue.diff` is re-recorded from that commit's patch
// and `integrated_sha` (plus any further metadata) is merged onto the step's
// record — all of it in one transaction after the git questions are answered.
//
// It refuses: a malformed sha; a step that is not terminal (a live step's
// record lands under its holder's token); a step that records no `issue.diff`
// of its own (nothing downstream reads a re-record of it); a step whose record
// names no commit (there is no divergence to resolve); an engine with no
// ancestry seam wired; an unanswerable ancestry question; and a sha that is not
// an ancestor of the shared checkout's HEAD.
func (e *Engine) AnnotateIntegration(
	conn *sql.DB, stepID int, sha, metadata string, nowMS int64,
) (*IntegrationAnnotation, error) {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if !fullCommitSHA.MatchString(sha) {
		return nil, validationErr(
			"--integrated-sha must be a full 40-hex commit id, got %q; "+
				"`git rev-parse <ref>` in the shared checkout resolves one", sha)
	}
	annotation, err := integrationMetadata(sha, metadata)
	if err != nil {
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

	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	spec := workflow.StepByName(defs[step.WorkflowID], step.StepName)
	if !isExecutorStep(step) || spec == nil || !stepHoldsTree(spec) {
		return nil, validationErr(
			"step %s records no issue.diff of its own — only an executor step "+
				"that holds the tree does — so --integrated-sha has no record to "+
				"re-point; annotate the step whose record the packets render from",
			step.Instance)
	}
	prior, err := stepLatestIssueDiff(conn, step.ID)
	if err != nil {
		return nil, err
	}
	from := ""
	if prior != nil {
		from = handBackHead(prior.Payload)
	}
	if from == "" {
		return nil, validationErr(
			"step %s recorded no commit in its issue.diff, so there is no "+
				"recorded head for %.12s to resolve; --integrated-sha names the "+
				"commit a recorded one landed as", step.Instance, sha)
	}

	// The git questions, OUTSIDE any transaction (§6). Ancestry is the whole
	// authorization: nothing below runs on a sha the shared branch does not
	// carry.
	execRoot := runExecRoot(conn, step.RunID)
	if e == nil || e.IsAncestorFn == nil {
		return nil, conflictErr(
			"cannot verify %.12s: this engine has no ancestry probe wired, and an "+
				"integration nobody verified is not recorded", sha)
	}
	ancestor, known := e.IsAncestorFn(execRoot, sha)
	if !known {
		return nil, conflictErr(
			"could not establish whether %.12s is reachable from the shared "+
				"checkout's HEAD (%s); is it a commit there? Nothing was recorded",
			sha, execRoot)
	}
	if !ancestor {
		return nil, conflictErr(
			"%.12s is not an ancestor of the shared checkout's HEAD (%s); the "+
				"engine records only an integration it can verify — integrate the "+
				"commit first (cherry-pick, merge), then annotate the commit the "+
				"branch carries", sha, execRoot)
	}

	body := ""
	if e.CommitPatchFn != nil {
		scope, err := snapshotScope(conn, step.RunID, step.IssueID)
		if err != nil {
			return nil, err
		}
		if body, err = e.CommitPatchFn(execRoot, sha, scope); err != nil {
			return nil, fmt.Errorf("computing the patch of %.12s for %s: %w", sha, step.Instance, err)
		}
	}
	payload, err := json.Marshal(map[string]string{
		"head": sha, "resolved_from": from,
	})
	if err != nil {
		return nil, fmt.Errorf("recording the integration of %s: %w", step.Instance, err)
	}
	repin := &IssueDiffRepin{
		Worktree: execRoot, FromSHA: from, ToSHA: sha,
		body: body, payload: string(payload),
		supersededID: prior.ID,
		// DKT-258's rule in the re-pin's terms: the same head at the same body
		// records nothing new, and the annotation still lands below.
		Unchanged: from == sha && prior.Body == body,
	}

	merged, err := mergeMetadata(step.Metadata, annotation)
	if err != nil {
		return nil, err
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("annotating %s: %w", step.Instance, err)
	}
	defer tx.Rollback()

	if !repin.Unchanged {
		if err := recordIntegrationRepinTx(tx, step, repin, nowMS); err != nil {
			return nil, err
		}
	}
	if err := db.SetStepMetadataTx(tx, step.ID, merged, nowMS); err != nil {
		return nil, err
	}
	// The annotation VERBATIM, as every annotation is logged: what was added
	// must survive a later annotation overwriting the key.
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepAnnotated, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
		Data: annotation, AtMS: nowMS,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("annotating %s: %w", step.Instance, err)
	}

	out := &IntegrationAnnotation{IntegratedSHA: sha, ResolvedFrom: from, Repin: repin}
	if from == sha {
		out.ResolvedFrom = ""
	}
	return out, nil
}

// integrationMetadata is the annotation bag: the caller's `--metadata`, if
// any, with `integrated_sha` set to the verified sha — the verified value wins
// over a caller's own spelling of the same key, because the record must say
// what the engine checked.
func integrationMetadata(sha, metadata string) (string, error) {
	bag := map[string]any{}
	if metadata != "" {
		if err := validateMetadataSize(metadata); err != nil {
			return "", err
		}
		decoded, err := DecodeMetadataBag(metadata, "metadata")
		if err != nil {
			return "", err
		}
		bag = decoded
	}
	bag[MetadataKeyIntegratedSHA] = sha
	encoded, err := json.Marshal(bag)
	if err != nil {
		return "", fmt.Errorf("encoding the annotation: %w", err)
	}
	return string(encoded), validateMetadataSize(string(encoded))
}

// recordIntegrationRepinTx inserts the superseding `issue.diff` artifact and its
// `issue-diff-repinned` event — the row shape the routing stage and the
// `--worktree` re-pin both write (applyIssueDiffRepin), minus that re-pin's
// worktree rewrite: the resolved commit lives on the shared branch, and the
// step's declared worktree stays what its executor recorded.
func recordIntegrationRepinTx(tx *sql.Tx, step *db.Step, repin *IssueDiffRepin, nowMS int64) error {
	supersedes := repin.supersededID
	id, err := db.InsertArtifactTx(tx, db.Artifact{
		RunID: step.RunID, StepID: step.ID, Kind: ArtifactKindIssueDiff,
		Body: repin.body, Payload: repin.payload,
		SHA256:     workflow.SHA256([]byte(repin.body)),
		Supersedes: &supersedes,
	}, nowMS)
	if err != nil {
		return err
	}
	repin.Artifact = fmt.Sprintf("ARTIFACT-%d", id)
	repin.Supersedes = artifactRef(&supersedes)

	data, err := json.Marshal(map[string]any{
		"from_sha":   repin.FromSHA,
		"to_sha":     repin.ToSHA,
		"worktree":   repin.Worktree,
		"artifact":   repin.Artifact,
		"supersedes": repin.Supersedes,
		"resolution": ResolutionIntegratedSHA,
	})
	if err != nil {
		return fmt.Errorf("recording the re-record of %s: %w", step.Instance, err)
	}
	return recordEvent(tx, eventRecord{
		Kind: EventIssueDiffRepinned, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
		Data: string(data), AtMS: nowMS,
	})
}
