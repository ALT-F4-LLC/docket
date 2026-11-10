package engine

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
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
	// Worktree is the checkout the new body was computed in: the named
	// checkout for the `--worktree` re-pin, the run's shared checkout for a
	// range re-pin (an annotation, a done step's own range), which reads no
	// worktree.
	Worktree string `json:"worktree"`
	// FromSHA is the target the step's previous issue.diff record named — the
	// stale one — or "" when the step had recorded no diff at all.
	FromSHA string `json:"from_sha,omitempty"`
	// ToSHA is the target the new record names: Worktree's HEAD for the
	// `--worktree` re-pin, the integrated sha for an annotation, and the
	// unchanged head for a done step's own-range re-pin, where it equals
	// FromSHA.
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
	// rangeFrom is the commit a range re-pin's patch starts after: an
	// annotation's integrated range (DKT-4269) or a done step's own range;
	// "" for a re-pin of one commit.
	rangeFrom string
	// recorded is the commit the re-pinned checkout holds, captured like a
	// record's (DKT-4250); haveRecorded is false when the checkout holds work a
	// checkout of its head would not reproduce.
	recorded     recordedHead
	haveRecorded bool
}

// prepareIssueDiffRepin computes the re-pin OUTSIDE any transaction (it is a
// git subprocess) and refuses everything it can before the caller opens
// one. spec is the step's pinned definition, nil for a materialized step.
func (e *Engine) prepareIssueDiffRepin(
	conn store.LegacyDB, step *store.Step, spec *workflow.Step, worktree string,
) (*IssueDiffRepin, error) {
	// Only a tree-holding executor step records an issue.diff (as amended by
	// DKT-75), so only one has a record to re-pin. A
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
	// The head is captured BEFORE the diff reads the tree, and kept only if the
	// diff named the same commit: a tree that moved between the two reads
	// records no head rather than a head the diff did not describe.
	recorded, haveRecorded := e.captureRecordedHead(conn, step.RunID, worktree)
	body, payload, err := e.computeIssueDiff(conn, &pinned, false)
	if err != nil {
		return nil, err
	}

	to := handBackHead(payload)
	if to == "" {
		// The record names no head for two distinct reasons, and the refusal
		// has to name the right one (the standard stated above). A checkout
		// whose HEAD resolves but still stands at its base has COMMITTED
		// NOTHING — the round record drops that head on purpose (DKT-1374), so
		// no target points at the pre-patch tree — and the fix is a commit
		// there, not a repository diagnosis. Only a HEAD that cannot be
		// resolved at all is the "is it a git checkout?" case.
		//
		// Either way the re-pin refuses: a re-pin that moved the body while
		// leaving the packets' `target_sha` on the stale commit would be
		// RUN-67 with a better diff.
		if e.HeadFn != nil {
			if head := e.HeadFn(worktree); head != "" {
				return nil, validationErr(
					"%s has committed nothing: its HEAD %.12s is still the commit "+
						"it was forked from, so there is no commit to bind %s's "+
						"target to; commit the patch there, or re-pin from a "+
						"checkout that carries it as its own commits",
					worktree, head, step.Instance)
			}
		}
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
	repin.recorded, repin.haveRecorded = recorded, haveRecorded && recorded.Head == to
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
	tx store.LegacyTx, step *store.Step, repin *IssueDiffRepin, as string, nowMS int64,
) error {
	// The named checkout becomes the step's RECORDED worktree, exactly as
	// `step record --worktree` persists it (stageZero): a rerun-gates resumed
	// after this measures its gates there, and the round record's `worktree`
	// already names it. Persisted even when the diff is unchanged — the
	// operator has just said where the tree is.
	if step.WorkRoot != repin.Worktree {
		if err := store.TxOn(tx).RepinStepWorkRootTx(step.ID, repin.Worktree); err != nil {
			return fmt.Errorf("recording the re-pinned worktree for %s: %w", step.Instance, err)
		}
		step.WorkRoot = repin.Worktree
	}
	// The named checkout is also what the gates measure from now on, so the
	// recorded commit is replaced with the one it holds, or cleared when none
	// reproduces it (DKT-4250). An older record must not outlive the re-pin: a
	// rerun-gates would otherwise measure the commit the operator just replaced.
	if err := insertRecordedHeadTx(tx, step, repin.recorded, repin.haveRecorded, repin.Worktree, nowMS); err != nil {
		return err
	}
	if repin.Unchanged {
		return nil
	}
	return recordIssueDiffRepinTx(tx, step, repin, map[string]any{"resolution": as}, nowMS)
}

// recordIssueDiffRepinTx inserts the re-pin's superseding issue.diff artifact
// and its `issue-diff-repinned` event; event carries the caller's own fields.
func recordIssueDiffRepinTx(
	tx store.LegacyTx, step *store.Step, repin *IssueDiffRepin, event map[string]any, nowMS int64,
) error {
	var supersedes *int
	if repin.supersededID != 0 {
		id := repin.supersededID
		supersedes = &id
	}
	// The same row shape the routing stage records (runRoutingStage), plus
	// DKT-70's pointer at the record this one revises.
	id, err := store.TxOn(tx).InsertArtifactTx(store.Artifact{
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
	event["from_sha"] = repin.FromSHA
	event["to_sha"] = repin.ToSHA
	event["worktree"] = repin.Worktree
	event["artifact"] = repin.Artifact
	event["supersedes"] = repin.Supersedes
	if repin.rangeFrom != "" {
		event["range_from"] = repin.rangeFrom
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("recording the re-pin of %s: %w", step.Instance, err)
	}
	return recordEvent(tx, eventRecord{
		Kind: EventIssueDiffRepinned, RunID: step.RunID,
		Instance: step.Instance, IssueID: step.IssueID,
		Data: string(data), AtMS: nowMS,
	})
}

// ResolutionOwnRange is the `resolution` an `issue-diff-repinned` event
// carries when a done step's diff was re-recorded over its own range.
const ResolutionOwnRange = "own-range"

// RepinIssueDiffRange re-records a DONE step's `issue.diff` over the step's own
// range, scoped like every issue.diff, as a new artifact superseding the
// step's newest record. The range ends at the newest record's `head` and
// starts at the bound the step's own record of that head names (see
// stepOwnRangeRecord).
//
// It is the re-pin for a step `step resolve --worktree` cannot reach: resolve
// applies only to a parked step. A rerun-gates on a hand-integrated step walks
// the routing stage again and re-cuts the body for the same head from whatever
// base resolves then, the whole-branch one once the worktree is swept, and
// records that base beside it; this re-derives the body from the commits the
// step recorded, reading no worktree, so the step's recorded worktree and
// recorded head stay as they were. An identical body records nothing and
// reports Unchanged.
func (e *Engine) RepinIssueDiffRange(
	conn store.LegacyDB, stepID int, by Attribution, token string, nowMS int64,
) (*IssueDiffRepin, error) {
	const verb = "issue.diff range re-pin"
	if err := by.require(verb); err != nil {
		return nil, err
	}
	step, err := store.StoreOn(conn).GetStep(stepID)
	if errors.Is(err, store.ErrStepNotFound) {
		return nil, notFoundErr(err, "step %s not found", model.FormatStepID(stepID))
	}
	if err != nil {
		return nil, err
	}
	if err := authorizeConductor(conn, step.RunID, token, verb); err != nil {
		return nil, err
	}
	if step.Status != store.StepDone {
		return nil, conflictErr(
			"step %s is %s, not done; a parked step's issue.diff is re-pinned "+
				"with `step resolve --worktree`, and a live step's lands with its record",
			step.Instance, step.Status)
	}
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return nil, err
	}
	spec := workflow.StepByName(defs[step.WorkflowID], step.StepName)
	if !isExecutorStep(step) || spec == nil || !stepHoldsTree(spec) {
		return nil, validationErr(
			"step %s records no issue.diff of its own — only an executor step "+
				"that holds the tree does — so it has no range to re-record",
			step.Instance)
	}
	prior, own, err := stepOwnRangeRecord(conn, step)
	if err != nil {
		return nil, err
	}
	// The new record restores the fields of the step's own record of the head
	// (head, bases, worktree, an annotation's writer chain), not those of a
	// re-cut that named another base, and adds the range it was cut from.
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(own.Payload), &fields); err != nil {
		return nil, validationErr(
			"step %s's issue.diff record ARTIFACT-%d has no readable payload: %v",
			step.Instance, own.ID, err)
	}
	field := func(key string) string { s, _ := fields[key].(string); return s }
	head, from := field("head"), field("range_from")
	if from == "" {
		from = field("round_base")
	}
	if from == "" {
		from = field("base")
	}
	if from == "" {
		return nil, validationErr(
			"step %s's first record of head %.12s, ARTIFACT-%d, names no range_from, "+
				"round_base, or base, so there is no recorded range to re-record; a "+
				"later record's base is not used, since a re-cut names whatever base "+
				"resolved when it ran", step.Instance, head, own.ID)
	}
	// Both are handed to git as revisions, so they are held to the shape of a
	// commit id first, as the integration annotation holds its writer chain.
	if !hexCommitID.MatchString(head) || !hexCommitID.MatchString(from) {
		return nil, validationErr(
			"step %s's issue.diff record ARTIFACT-%d does not name its range as "+
				"commit ids (head %q, range start %q), so there is no range "+
				"to re-record", step.Instance, own.ID, head, from)
	}
	if e.RangePatchFn == nil {
		return nil, conflictErr(
			"cannot re-record %s: this engine has no range patch wired", step.Instance)
	}
	scope, err := snapshotScope(conn, step.RunID, step.IssueID)
	if err != nil {
		return nil, err
	}
	execRoot := runExecRoot(conn, step.RunID)
	body, err := e.RangePatchFn(execRoot, from, head, scope)
	if err != nil {
		return nil, fmt.Errorf("computing the patch of %.12s..%.12s for %s: %w",
			from, head, step.Instance, err)
	}
	fields["range_from"] = from
	payload, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("recording the re-pin of %s: %w", step.Instance, err)
	}
	repin := &IssueDiffRepin{
		Worktree: execRoot, FromSHA: head, ToSHA: head,
		body: body, payload: string(payload),
		supersededID: prior.ID, rangeFrom: from,
		Unchanged: prior.Body == body,
	}
	if repin.Unchanged {
		return repin, nil
	}

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("re-pinning %s: %w", step.Instance, err)
	}
	defer tx.Rollback()
	event := map[string]any{"resolution": ResolutionOwnRange, "actor": by.Actor, "cwd": by.Cwd}
	if err := recordIssueDiffRepinTx(tx, step, repin, event, nowMS); err != nil {
		return nil, err
	}
	if err := commitWrite(tx); err != nil {
		return nil, fmt.Errorf("re-pinning %s: %w", step.Instance, err)
	}
	return repin, nil
}

// stepLatestIssueDiff is the newest `issue.diff` artifact THIS step recorded,
// or nil when it never recorded one — the record a re-pin supersedes.
//
// Scoped to the step rather than to the issue on purpose: a supersession
// pointer says "this revises that" (DKT-70), and a step revises its own
// record. Whether the issue's newest diff belongs to this step or to another
// is the resolver's question, answered by ordinal and id as always.
func stepLatestIssueDiff(conn store.LegacyDB, stepID int) (*store.Artifact, error) {
	artifacts, err := store.StoreOn(conn).ListStepArtifacts(stepID)
	if err != nil {
		return nil, err
	}
	var latest *store.Artifact
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

// stepOwnRangeRecord returns the step's newest issue.diff record, the one a
// range re-pin supersedes, and the record that names the step's own range for
// that record's head: the OLDEST of the step's records naming the same head.
//
// Every engine writer records the base it cut its body from, so a later record
// at the same head is a re-cut of the same commit whose base is whatever
// resolved when it ran (a rerun-gates after the worktree is swept falls back to
// the run's pinned commit); its base describes that cut, not the step's
// change. The first record of a head was cut when that head was handed back,
// and a re-pin that moves the head (the `--worktree` re-pin, an integration
// annotation) starts a new first record, so a sanctioned move keeps its range.
func stepOwnRangeRecord(conn store.LegacyDB, step *store.Step) (newest, own *store.Artifact, err error) {
	artifacts, err := store.StoreOn(conn).ListStepArtifacts(step.ID)
	if err != nil {
		return nil, nil, err
	}
	var records []*store.Artifact
	for _, a := range artifacts {
		if a.Kind == ArtifactKindIssueDiff {
			records = append(records, a)
		}
	}
	if len(records) == 0 {
		return nil, nil, validationErr("step %s recorded no issue.diff to re-record", step.Instance)
	}
	slices.SortFunc(records, func(a, b *store.Artifact) int { return cmp.Compare(a.ID, b.ID) })
	newest = records[len(records)-1]
	head := handBackHead(newest.Payload)
	if head == "" {
		return nil, nil, validationErr(
			"step %s's newest issue.diff record ARTIFACT-%d names no head commit, "+
				"so there is no range to re-record", step.Instance, newest.ID)
	}
	for _, a := range records[:len(records)-1] {
		if handBackHead(a.Payload) == head {
			return newest, a, nil
		}
	}
	return newest, newest, nil
}
