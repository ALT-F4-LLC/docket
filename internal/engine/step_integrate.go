package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// THE ENGINE-RUN INTEGRATION — `step integrate STEP-N...`.
//
// A write-class step's commits reach the shared branch by a cherry-pick, and
// until this verb the conductor typed that sequence by hand for every writer:
// confirm the step is done, check the commits exist, `git cherry-pick -x`, read
// HEAD, `step annotate --integrated-sha`. No form of the hand sequence stopped
// on a failed pick, because the pick was piped through a pager or `tail`, so a
// conflicted pick once annotated the PREVIOUS head. This verb is that sequence
// as one operation that cannot continue past a failure.
//
// IT IS THE ENGINE'S FIRST GIT WRITE, so its limits are deliberate:
//
//   - the conductor capability gates it, as it gates the annotation it ends in,
//     and it records who ran it and from where;
//   - nothing is written until EVERY named step has passed its read-only
//     checks (done, an executor step that holds the tree, a recorded commit
//     chain the checkout holds with no merge commit, not already annotated, not
//     already carried by HEAD, no commit shared with another named chain);
//   - the shared checkout must be on a branch with no tracked change and no
//     operation in progress, or the verb refuses before touching it;
//   - a pick that does not apply is aborted and the checkout is verified to
//     stand where it started; the engine never resets, amends or discards;
//   - the annotation runs only after a pick that added exactly the chain, and
//     it is the annotation's own verification (ancestry, then patch carriage)
//     that the integration is recorded on, not the pick's exit status;
//   - repository hooks do not run from the pick (integrateGit).
//
// Steps are integrated in the order named, one at a time. The first failure
// stops the verb; the steps before it stay integrated and annotated, and the
// result lists them. A pick that succeeds followed by an annotation that is
// refused leaves the pick in place and names the sha to annotate by hand,
// because undoing a commit the checkout now carries is the discard the engine
// does not perform.

// StepIntegration is what integrating one step did.
type StepIntegration struct {
	// Step is the step, STEP-N.
	Step string `json:"step"`
	// Picked lists the commits added to the shared branch, oldest first.
	Picked []string `json:"picked"`
	// IntegratedSHA is the shared checkout's HEAD after the pick, the commit the
	// step was annotated with.
	IntegratedSHA string `json:"integrated_sha"`
	// Annotation is the verified annotation the pick ended in.
	Annotation *IntegrationAnnotation `json:"annotation"`
}

type integratePlan struct {
	id       int
	instance string
	execRoot string
	head     string
	base     string
	chain    []string
}

// IntegrateSteps cherry-picks each named step's recorded writer chain onto the
// shared checkout and annotates the step with the resulting HEAD. It returns
// the steps it completed, also on a failure part way through.
func (e *Engine) IntegrateSteps(
	conn *sql.DB, stepIDs []int, by Attribution, token string, nowMS int64,
) ([]StepIntegration, error) {
	if len(stepIDs) == 0 {
		return nil, validationErr("name at least one step to integrate")
	}
	seen := make(map[int]bool, len(stepIDs))
	for _, id := range stepIDs {
		if seen[id] {
			return nil, validationErr("%s is named twice", model.FormatStepID(id))
		}
		seen[id] = true
	}
	if err := by.require("step integrate"); err != nil {
		return nil, err
	}
	if e == nil || e.CherryPickFn == nil || e.IntegrationChainFn == nil ||
		e.IsAncestorFn == nil || e.IntegrationRangeFn == nil {
		return nil, conflictErr(
			"this engine has no git integration wired, and an integration the " +
				"engine cannot verify afterwards is not performed")
	}

	// Every read-only check first, so a bad later argument stops the verb
	// before the first write.
	plans := make([]integratePlan, 0, len(stepIDs))
	for _, id := range stepIDs {
		p, err := e.planIntegration(conn, id, token)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	// Each plan was checked against HEAD before any pick, so two chains that
	// share commits (a writer that forked from another's unintegrated branch)
	// both pass, and the second pick re-applies the first one's commits after
	// they landed and fails part way through the verb. Refuse before the first
	// write instead.
	owner := map[string]string{}
	for _, p := range plans {
		for _, c := range p.chain {
			if other, ok := owner[c]; ok {
				return nil, conflictErr(
					"%s and %s share commit %.12s: a writer forked from the other's chain, and "+
						"the engine picks each chain whole, so it would land that commit twice; "+
						"integrate them by hand and annotate each step with the commit that carries it",
					other, p.instance, c)
			}
			owner[c] = p.instance
		}
	}

	done := make([]StepIntegration, 0, len(plans))
	for _, p := range plans {
		pick, err := e.CherryPickFn(p.execRoot, p.base, p.head)
		if err != nil {
			if pick.Tip != "" && pick.Tip != pick.Before {
				return done, conflictErr(
					"%s: the shared branch moved %.12s -> %.12s but the pick was not accepted: %v; "+
						"nothing was reset — inspect the branch, then run `docket step annotate %s "+
						"--integrated-sha <sha>` once it carries the work",
					p.instance, pick.Before, pick.Tip, err, model.FormatStepID(p.id))
			}
			return done, conflictErr("%s was not integrated: %v", p.instance, err)
		}
		ann, err := e.AnnotateIntegration(conn, p.id, pick.Tip, "", by, token, nowMS)
		if err != nil {
			return done, conflictErr(
				"%s: the shared branch now carries %d commit(s) from the pick (HEAD %s) but the "+
					"annotation was refused: %v; nothing was reset — fix the cause and run "+
					"`docket step annotate %s --integrated-sha %s`",
				p.instance, len(pick.Picked), pick.Tip, err, model.FormatStepID(p.id), pick.Tip)
		}
		done = append(done, StepIntegration{
			Step: model.FormatStepID(p.id), Picked: pick.Picked,
			IntegratedSHA: pick.Tip, Annotation: ann,
		})
	}
	return done, nil
}

// planIntegration is one step's read-only gate: it writes nothing and runs no
// command that can.
func (e *Engine) planIntegration(conn *sql.DB, id int, token string) (integratePlan, error) {
	step, err := db.GetStep(conn, id)
	if errors.Is(err, db.ErrStepNotFound) {
		return integratePlan{}, notFoundErr(err, "step %s not found", model.FormatStepID(id))
	}
	if err != nil {
		return integratePlan{}, err
	}
	if err := authorizeConductor(conn, step.RunID, token, "step integrate"); err != nil {
		return integratePlan{}, err
	}
	if step.Status != db.StepDone {
		return integratePlan{}, conflictErr(
			"step %s is %s; only a done step's work is integrated", step.Instance, step.Status)
	}
	defs, err := StepDefinitions(conn, step.RunID)
	if err != nil {
		return integratePlan{}, err
	}
	spec := workflow.StepByName(defs[step.WorkflowID], step.StepName)
	if !isExecutorStep(step) || spec == nil || !stepHoldsTree(spec) {
		return integratePlan{}, validationErr(
			"step %s records no issue.diff of its own — only an executor step that holds "+
				"the tree does — so it has no writer chain to integrate", step.Instance)
	}
	var meta map[string]any
	if step.Metadata != "" && json.Unmarshal([]byte(step.Metadata), &meta) == nil {
		if sha, ok := meta[MetadataKeyIntegratedSHA].(string); ok && sha != "" {
			return integratePlan{}, conflictErr(
				"step %s is already annotated as integrated at %.12s; picking again would "+
					"land its commits twice", step.Instance, sha)
		}
	}
	prior, err := stepLatestIssueDiff(conn, step.ID)
	if err != nil {
		return integratePlan{}, err
	}
	head := ""
	if prior != nil {
		head = handBackHead(prior.Payload)
	}
	if head == "" {
		return integratePlan{}, validationErr(
			"step %s recorded no commit in its issue.diff, so there is nothing to pick", step.Instance)
	}
	// A Live record means the worktree held work the head commit does not
	// carry (uncommitted changes, or no base to measure from), and a missing one
	// (a re-pin whose tree moved records none) leaves nothing to say it did not,
	// so the gates and the diff judged bytes a pick of the chain may not
	// reproduce.
	if rec, ok := recordedHeadOf(conn, step); !ok || rec.Live {
		return integratePlan{}, conflictErr(
			"step %s has no recorded head, or recorded a live worktree whose work its head commit does not carry, so the "+
				"engine will not pick the commits as the work that was measured; integrate it "+
				"by hand and annotate the result", step.Instance)
	}
	writerHead, writerBase := writerChain(prior.Payload, head)
	// An unrecorded base would make the chain the head commit alone, and the
	// annotation's own check reads it the same way, so a multi-commit writer
	// would be reported integrated with its earlier commits never picked.
	if writerBase == "" {
		return integratePlan{}, validationErr(
			"step %s recorded no base for its writer chain, so the engine cannot tell which "+
				"commits are the writer's; integrate it by hand and annotate the result",
			step.Instance)
	}
	// The chain is passed to git as revisions. It comes from a recorded payload,
	// so it is held to the shape the engine itself records (full commit ids)
	// before any argument is built from it: a value that began with "-" would
	// be an option, not a revision.
	if !fullCommitSHA.MatchString(writerHead) || !fullCommitSHA.MatchString(writerBase) {
		return integratePlan{}, validationErr(
			"step %s recorded a writer chain that is not full commit ids (head %q, base %q); "+
				"the engine will not build a git command from it", step.Instance, writerHead, writerBase)
	}
	execRoot := runExecRoot(conn, step.RunID)
	chain, err := e.IntegrationChainFn(execRoot, writerBase, writerHead)
	if err != nil {
		return integratePlan{}, conflictErr("step %s cannot be integrated: %v", step.Instance, err)
	}
	// A chain the checkout already carries (a merge, or an earlier pick whose
	// annotation never ran) is annotated, not picked a second time.
	if r := e.IntegrationRangeFn(execRoot, "HEAD", writerHead, writerBase); r.Known && r.Carried {
		return integratePlan{}, conflictErr(
			"the shared checkout's HEAD already carries step %s's %d commit(s); annotate it "+
				"instead: `docket step annotate %s --integrated-sha <the commit that carries it>`",
			step.Instance, len(chain), model.FormatStepID(id))
	}
	return integratePlan{
		id: id, instance: fmt.Sprintf("%s (%s)", model.FormatStepID(id), step.Instance),
		execRoot: execRoot, head: writerHead, base: writerBase, chain: chain,
	}, nil
}
