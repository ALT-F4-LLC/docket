package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The issue.diff re-pin — `step resolve --as override-pass|rerun-gates
// --worktree C` (DKT-1034).
//
// A step's recorded `issue.diff` is the reviewed object: every downstream
// packet that consumes `issue.diff` renders the body it recorded and binds its
// target to the `head` in its round record (resolveIssueDiff, resolveTarget).
// That record is frozen at completion by design (DKT-725/DKT-741: a recorded
// reference is surfaced when it goes bad, never silently re-derived) — and no
// resolution re-records it. `rerun-gates` leaves the artifact as it found it,
// `retry` diffs a tree that already contains the change and records nothing
// (DKT-259), `override-pass` records a generic pass.
//
// RUN-67 measured the gap. STEP-3248 (implement@0) failed self-hygiene on its
// recorded commit fe5db34e; the operator ruled a conductor patch, the conductor
// committed e642afb on top in the step's worktree, integrated both, and
// override-passed the step. The review fanout then rendered from fe5db34e —
// the STALE pre-patch record — three judges re-found the E501 defect the branch
// no longer had as a hard-gate blocker, reconcile routed a full fix loop, and a
// seven-step round (~22 minutes of wall clock) ran to report "ruff is clean
// now", which it had been before the round started.
//
// The re-pin is the sanctioned way to move the record on purpose: the diff and
// its target sha are recomputed from the named checkout EXACTLY as `step record
// --worktree` computes them (computeIssueDiff — one implementation, not a
// copy), recorded as a new `issue.diff` artifact on the same step that
// SUPERSEDES the step's previous one (DKT-70's pointer, so the original stays
// readable in the chain and a consumer counting work still counts one), and
// event-logged with both shas. The resolvers already bind to a producer's
// newest emit (latestPerProducer; highest id within an ordinal), so every
// downstream packet renders the patched tree from then on with no resolver
// change at all.

// IssueDiffRepin is what one re-pin did, as the verb reports it.
type IssueDiffRepin struct {
	// Worktree is the checkout the diff and target were recomputed from.
	Worktree string `json:"worktree"`
	// FromSHA is the target the step's previous issue.diff record named — the
	// stale one — or "" when the step had recorded no diff at all.
	FromSHA string `json:"from_sha,omitempty"`
	// ToSHA is the re-pinned target: Worktree's HEAD at re-pin time.
	ToSHA string `json:"to_sha"`
	// Artifact is the `ARTIFACT-N` the re-pin recorded, and Supersedes the
	// step's previous issue.diff artifact it revises. Both empty on an
	// Unchanged re-pin, which records nothing.
	Artifact   string `json:"artifact,omitempty"`
	Supersedes string `json:"supersedes,omitempty"`
	// Unchanged reports that the step's newest issue.diff already describes
	// Worktree's tree byte for byte, at the same head, so nothing was recorded
	// and no event was written: the resolution proceeds and the caller says
	// so, rather than refusing a resolution whose only fault is redundancy.
	Unchanged bool `json:"unchanged,omitempty"`

	body, payload string
	supersededID  int
}

// prepareIssueDiffRepin computes the re-pin OUTSIDE any transaction (it is a
// git subprocess, §6) and refuses everything it can before the caller opens
// one. spec is the step's pinned definition, nil for a materialized step.
func (e *Engine) prepareIssueDiffRepin(
	conn *sql.DB, step *db.Step, spec *workflow.Step, worktree string,
) (*IssueDiffRepin, error) {
	// Only a tree-holding executor step records an issue.diff (§6.7.1 D1/D2
	// as amended by DKT-75), so only one has a record to re-pin. A
	// materialized held step (nil spec), a vote, an action, and a
	// `holds_tree = false` reader all resolve their diff from someone else's
	// record; re-pinning here would revise a record nothing downstream reads.
	if !isExecutorStep(step) || spec == nil || !stepHoldsTree(spec) {
		return nil, validationErr(
			"step %s records no issue.diff of its own — only an executor step "+
				"that holds the tree does — so --worktree has nothing to re-pin; "+
				"re-pin the step whose record the packets render from",
			step.Instance)
	}

	// A checkout that is not there diffs as nothing (GitDiff swallows the
	// failure by design, since a recording must not wedge a run), and the
	// DKT-259 guard below would then refuse for the wrong reason. Name the
	// actual mistake.
	if info, err := os.Stat(worktree); err != nil || !info.IsDir() {
		return nil, validationErr(
			"--worktree %s is not a directory; the re-pin diffs the checkout "+
				"the patched tree stands in", worktree)
	}

	// The step AS IT WOULD BE RECORDED from the named checkout: WorkRoot is
	// the one input the diff stage reads for "which tree", and the payload's
	// `worktree` field is written from it.
	pinned := *step
	pinned.WorkRoot = worktree
	body, payload, err := e.computeIssueDiff(conn, &pinned)
	if err != nil {
		return nil, err
	}

	to := handBackHead(payload)
	if to == "" {
		// A tree with no resolvable HEAD has no commit to bind the target
		// to, and a re-pin that moved the body while leaving the packets'
		// `target_sha` on the stale commit would be RUN-67 with a better diff.
		return nil, validationErr(
			"could not resolve the HEAD commit of %s, so there is no sha to "+
				"re-pin %s's target to; is it a git checkout?",
			worktree, step.Instance)
	}

	// DKT-259's rule, as a REFUSAL rather than the routing stage's silent
	// drop: the operator asked for a re-pin by name, and a re-pin that
	// recorded nothing would leave the stale target standing while reporting
	// success. The usual way to reach this is a checkout already integrated
	// VERBATIM into the shared branch (a fast-forward, a merge) — its fork
	// point has advanced onto its own HEAD, so there is nothing left to diff.
	// The conduct protocol's cherry-pick integration mints new shas and
	// leaves the fork point where it was, so the worktree keeps diffing.
	if diffRecordsNoChange(body) && issueHasRecordedChange(conn, step.RunID, step.IssueID) {
		return nil, validationErr(
			"the diff computed from %s is empty while %s already records a "+
				"change, so re-pinning would replace the recorded diff with "+
				"nothing (DKT-259). A checkout whose commits are already on the "+
				"shared branch has no fork point left to diff from — re-pin from "+
				"a checkout that still carries the patch as its own commits, or "+
				"from the run's shared checkout, whose diff is measured from the "+
				"run's pinned start commit",
			worktree, model.FormatID(step.IssueID))
	}

	prior, err := stepLatestIssueDiff(conn, step.ID)
	if err != nil {
		return nil, err
	}
	repin := &IssueDiffRepin{
		Worktree: worktree, ToSHA: to, body: body, payload: payload,
	}
	if prior != nil {
		repin.FromSHA = handBackHead(prior.Payload)
		repin.supersededID = prior.ID
		// Byte-identical at the same head is DKT-258's rule, in the re-pin's
		// own terms: nothing revised, so a new row would say something
		// happened that did not.
		repin.Unchanged = prior.Body == body && repin.FromSHA == to
	}
	return repin, nil
}

// applyIssueDiffRepin records a prepared re-pin inside the resolution's
// transaction: the step's worktree, the superseding artifact, and the event.
func applyIssueDiffRepin(
	tx *sql.Tx, step *db.Step, repin *IssueDiffRepin, as string, nowMS int64,
) error {
	// The named checkout becomes the step's RECORDED worktree, exactly as
	// `step record --worktree` persists it (stageZero): a rerun-gates resumed
	// after this measures its gates there, and the round record's `worktree`
	// already names it. Persisted even when the diff is unchanged — the
	// operator has just said where the tree is.
	if step.WorkRoot != repin.Worktree {
		if _, err := tx.Exec(
			`UPDATE steps SET work_root = ? WHERE id = ?`, repin.Worktree, step.ID,
		); err != nil {
			return fmt.Errorf("recording the re-pinned worktree for %s: %w", step.Instance, err)
		}
		step.WorkRoot = repin.Worktree
	}
	if repin.Unchanged {
		return nil
	}

	var supersedes *int
	if repin.supersededID != 0 {
		id := repin.supersededID
		supersedes = &id
	}
	// The same row shape the routing stage records (runRoutingStage), plus
	// DKT-70's pointer at the record this one revises.
	id, err := db.InsertArtifactTx(tx, db.Artifact{
		RunID: step.RunID, StepID: step.ID, Kind: ArtifactKindIssueDiff,
		Body: repin.body, Payload: repin.payload,
		SHA256:     workflow.SHA256([]byte(repin.body)),
		Supersedes: supersedes,
	}, nowMS)
	if err != nil {
		return err
	}
	repin.Artifact = fmt.Sprintf("ARTIFACT-%d", id)
	repin.Supersedes = artifactRef(supersedes)

	// Both shas, the checkout, both artifacts, and the resolution the re-pin
	// rode on — the facts a reader comparing a packet against the executor's
	// recorded commit needs to see where the two parted.
	data, err := json.Marshal(map[string]any{
		"from_sha":   repin.FromSHA,
		"to_sha":     repin.ToSHA,
		"worktree":   repin.Worktree,
		"artifact":   repin.Artifact,
		"supersedes": repin.Supersedes,
		"resolution": as,
	})
	if err != nil {
		return fmt.Errorf("recording the re-pin of %s: %w", step.Instance, err)
	}
	return recordEvent(tx, eventRecord{
		Kind: EventIssueDiffRepinned, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
		Data: string(data), AtMS: nowMS,
	})
}

// stepLatestIssueDiff is the newest `issue.diff` artifact THIS step recorded,
// or nil when it never recorded one — the record a re-pin supersedes.
//
// Scoped to the step rather than to the issue on purpose: a supersession
// pointer says "this revises that" (DKT-70), and a step revises its own
// record. Whether the issue's newest diff belongs to this step or to another
// is the resolver's question, answered by ordinal and id as always.
func stepLatestIssueDiff(conn *sql.DB, stepID int) (*db.Artifact, error) {
	artifacts, err := db.ListStepArtifacts(conn, stepID)
	if err != nil {
		return nil, err
	}
	var latest *db.Artifact
	for _, a := range artifacts {
		if a.Kind != ArtifactKindIssueDiff {
			continue
		}
		if latest == nil || a.ID > latest.ID {
			latest = a
		}
	}
	return latest, nil
}
