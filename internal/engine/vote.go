package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Vote-step execution (TDD docs/tdd/gates-trust.md §8), deferred to this stage
// by engine-spine §1's own scope table.
//
// engine-core §6, verbatim:
//
//	Votes. Docket's existing proposal/vote machinery is kept and demoted to a
//	gate type: a `type="vote"` step creates a proposal, fans out the configured
//	voters to cast via CLI, and the engine computes the outcome from threshold
//	config (it already does: weighted score, required voters).
//
// THE WHOLE OF THIS FILE IS WIRING. No vote semantics change. `db.CastVote`,
// its weighted-score computation, its quorum rule, and its approved/rejected
// outcome are USED UNCHANGED — not copied, not re-implemented, not
// parameterized. TestVoteTallyReachesTheExistingFunction asserts that the vote
// path reaches the same compiled function `docket vote cast` does.
//
// There is NO NEW VERB. Voters cast with the CLI docket has shipped since v2,
// which is the whole point of "kept and demoted to a gate type" — and it is the
// stranger test holding, because the vote feature predates the engine entirely.

// VoteOutcome is what a vote step's phase-4 read observed.
type VoteOutcome struct {
	// ProposalID is the proposal driving this step.
	ProposalID int
	// Status is the proposal's status, verbatim from the existing machinery.
	Status model.ProposalStatus
	// Verdict maps that status onto the gate verdict routing consumes:
	// approved => pass, rejected => fail. It is empty while the proposal is
	// still open, which is phase 3 and means "nothing to route yet".
	Verdict string
	// Score is the weighted score the existing tally computed, when there is
	// one.
	Score *float64
}

// voteStepKey identifies one run's vote step uniquely across every issue it
// binds.
//
// Instance ALONE is not enough (DKT-65). A materialized held-cluster
// instance carries no issue identity at all — heldClusterInstance
// (held.go) renders it as `<step>-held@<ordinal>#<element>`, issue-blind by
// construction. And even a DECLARED vote step's instance is identical across
// every issue's own copy of a workflow: two issues both bound to spec-doc
// each reach their own `accept@0`. Keying a run-wide map by instance alone
// merges the two into one entry.
//
// Measured live on RUN-5: two `reconcile-held@1#1` steps from different
// issues (DKT-56, DKT-50) both resolved to the SAME proposal under the old
// run-only idempotency key — the second issue's OpenVoteProposal found the
// first issue's proposal already there via CreateProposalIdempotent and
// reused it, so a panel's tally on one issue's evidence would have silently
// routed the other issue's unrelated held cluster too.
//
// Issue is the FORMATTED id (model.FormatID), not the raw int, so this key
// builds identically from a *db.Step (format its IssueID) and from a
// rendered model.StepRow (already carries Issue formatted) without either
// side round-tripping through the other's representation.
type voteStepKey struct {
	Issue    string
	Instance string
}

// voteStepKeyOf builds a voteStepKey from a step row.
func voteStepKeyOf(step *db.Step) voteStepKey {
	return voteStepKey{Issue: model.FormatID(step.IssueID), Instance: step.Instance}
}

// voteIdempotencyKey derives phase 2's key from the run, the step's issue, and
// the step instance.
//
// §8.1: "CreateProposalIdempotent with a key derived from (run, step instance)
// makes a double-invocation produce one proposal". The instance rather than the
// step name, because a loop's second ordinal is a DIFFERENT vote about
// different work and must open its own proposal. The issue is ADDED (DKT-65,
// see voteStepKey) because the instance half of that statement is not unique
// across issues, and the run-only key let two issues' vote steps collide on
// one proposal.
func voteIdempotencyKey(runID, issueID int, instance string) string {
	return voteIdempotencyPrefix(runID) + strconv.Itoa(issueID) + ":" + instance
}

// voteStepScopePrefix is the literal head of every vote-step idempotency key.
// A named constant so the writers above/below and the PARSER (voteStepRunOf)
// cannot spell it differently.
const voteStepScopePrefix = "vote-step:"

// voteIdempotencyPrefix is the key family of ONE RUN's vote steps — the part of
// the key above that does not vary per step. It stays run-only, not also
// issue-scoped, because loadVoteProposalsTx needs ONE prefix to bulk-read every
// vote-step proposal a run has opened, across every issue, in a single query.
//
// It exists so the bulk read below and the single-key lookup above cannot spell
// the key differently: a prefix reader that agreed with the writer everywhere
// except one separator would silently report no proposals at all.
func voteIdempotencyPrefix(runID int) string {
	return voteStepScopePrefix + strconv.Itoa(runID) + ":"
}

// voteStepRunOf parses the RUN id back out of a vote-step idempotency key —
// the inverse of voteIdempotencyPrefix, kept beside it for the same
// cannot-drift reason. The second return is false for any key of another
// family (an ad-hoc, operator-created proposal bound to no step).
func voteStepRunOf(key string) (int, bool) {
	suffix, ok := strings.CutPrefix(key, voteStepScopePrefix)
	if !ok {
		return 0, false
	}
	runPart, _, ok := strings.Cut(suffix, ":")
	if !ok {
		return 0, false
	}
	runID, err := strconv.Atoi(runPart)
	if err != nil {
		return 0, false
	}
	return runID, true
}

// loadVoteProposalsTx maps each of a run's vote steps to the proposal it
// opened, keyed by (issue, instance), inside the scheduler's snapshot
// transaction.
//
// It resolves through the SAME idempotency key OpenVoteProposal created the
// proposal under, rather than storing the id on the step: the link already
// exists and is already authoritative, and a second copy on the step row would
// be a column that could disagree with it.
//
// DORMANT ON A RUN WITH NO VOTE STEPS — no query at all, which is most runs.
func loadVoteProposalsTx(tx *sql.Tx, runID int, steps []*db.Step) (map[voteStepKey]int, error) {
	voting := false
	for _, step := range steps {
		if step.Kind == workflow.TypeVote {
			voting = true
			break
		}
	}
	if !voting {
		return nil, nil
	}

	prefix := voteIdempotencyPrefix(runID)
	keyed, err := db.LookupIdempotencyKeysTx(tx, db.ScopeVoteCreate, prefix)
	if err != nil {
		return nil, fmt.Errorf("reading the proposals of run %d: %w", runID, err)
	}

	out := make(map[voteStepKey]int, len(keyed))
	for key, id := range keyed {
		suffix, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		issuePart, instance, ok := strings.Cut(suffix, ":")
		if !ok {
			continue
		}
		issueID, err := strconv.Atoi(issuePart)
		if err != nil {
			continue
		}
		out[voteStepKey{Issue: model.FormatID(issueID), Instance: instance}] = id
	}
	return out, nil
}

// OpenVoteProposal is §8.1 phase 2: the first engine invocation that observes a
// ready vote step without a proposal creates one.
//
// LAZY AND IDEMPOTENT, following the saga's own discipline: the proposal is
// created by whichever invocation gets there first, and the idempotency key
// makes a double-invocation produce ONE proposal. No daemon watches for ready
// vote steps; nothing is scheduled.
func OpenVoteProposal(
	conn *sql.DB, step *db.Step, spec *workflow.Step, nowMS int64,
) (int, error) {
	if spec.Type != workflow.TypeVote {
		return 0, nil
	}

	projectID, err := db.RunProjectID(conn, step.RunID)
	if err != nil {
		return 0, err
	}
	rule, err := resolveVoteRule(conn, projectID, spec.VoteRule)
	if err != nil {
		return 0, err
	}

	proposal := &model.Proposal{
		ProjectID:   projectID,
		Description: fmt.Sprintf("%s (%s)", step.Instance, spec.Name),
		Rationale:   fmt.Sprintf("workflow vote step %s", step.Instance),
		Criticality: rule.Criticality,
		Threshold:   rule.Threshold,
		// §8.2: required_voters is len(voters), NOT a config value. A rule is
		// about HOW STRICTLY TO TALLY; the step is about WHO CASTS, and §11.1
		// puts the voter list on the step.
		//
		// The voter hints themselves are OPAQUE: core never interprets one,
		// never validates it against anything, and never dispatches to one. It
		// counts them.
		RequiredVoters: len(spec.Voters),
		Status:         model.ProposalStatusOpen,
		CreatedBy:      "docket",
	}

	proposalID, err := db.CreateProposalIdempotent(
		conn, proposal, voteIdempotencyKey(step.RunID, step.IssueID, step.Instance))
	if err != nil {
		return 0, fmt.Errorf("opening the proposal for %s: %w", step.Instance, err)
	}

	// The proposal is linked to the step's issue through the EXISTING link
	// table, which is what makes `vote show` and `vote list` find it without
	// any engine-specific reader.
	if err := db.LinkProposalIssue(conn, proposalID, step.IssueID); err != nil &&
		!errors.Is(err, db.ErrConflict) {
		return 0, fmt.Errorf("linking the proposal to %s: %w", step.Instance, err)
	}

	if err := recordVoteEvent(conn, EventVoteOpened, step,
		model.FormatProposalID(proposalID), nowMS); err != nil {
		return 0, err
	}

	return proposalID, nil
}

// ReadVoteOutcome is §8.1 phase 4: the first engine invocation after the
// proposal leaves `open` observes its status.
//
// It READS. The tally happened inside db.CastVote when the last voter cast, and
// this does not recompute it — recomputing would be a second implementation of
// the rule, which is exactly what "used unchanged" forbids.
func ReadVoteOutcome(conn *sql.DB, step *db.Step, spec *workflow.Step) (*VoteOutcome, error) {
	if spec.Type != workflow.TypeVote {
		return nil, nil
	}

	proposalID, err := findVoteProposal(conn, step)
	if err != nil || proposalID == 0 {
		return nil, err
	}

	proposal, err := db.GetProposal(conn, proposalID)
	if err != nil {
		return nil, fmt.Errorf("reading the proposal for %s: %w", step.Instance, err)
	}

	out := &VoteOutcome{
		ProposalID: proposalID,
		Status:     proposal.Status,
		Score:      proposal.WeightedScore,
	}
	switch proposal.Status {
	case model.ProposalStatusApproved, model.ProposalStatusCommitted:
		// §8.4: an operator committing a proposal manually sets its final
		// outcome, and this read observes the resulting status like any other.
		// No special case.
		out.Verdict = VerdictPass
	case model.ProposalStatusRejected:
		out.Verdict = VerdictFail
	default:
		// Still open: phase 3, waiting on voters. Nothing to route.
		out.Verdict = ""
	}
	return out, nil
}

// findVoteProposal resolves the proposal this step opened, through the
// idempotency key it was created under.
func findVoteProposal(conn *sql.DB, step *db.Step) (int, error) {
	id, found, err := db.LookupIdempotencyKey(
		conn, db.ScopeVoteCreate, voteIdempotencyKey(step.RunID, step.IssueID, step.Instance))
	if err != nil {
		return 0, fmt.Errorf("resolving the proposal for %s: %w", step.Instance, err)
	}
	if !found {
		return 0, nil
	}
	return id, nil
}

// IsVoteStepProposal reports whether a proposal was opened by an engine vote
// step — its create keyed under the vote-step scope prefix.
//
// `vote close` (DKT-114) is for CONVERSATIONAL proposals whose decision was
// made another way. A vote step's proposal is the step's own machinery:
// closing it underneath the step would not route the step, and the run is
// moved past an uncast vote with `docket step resolve` instead — so the close
// verb asks this first and refuses with that guidance.
func IsVoteStepProposal(conn *sql.DB, proposalID int) (bool, error) {
	key, found, err := db.IdempotencyKeyOf(conn, db.ScopeVoteCreate, proposalID)
	if err != nil {
		return false, err
	}
	return found && strings.HasPrefix(key, voteStepScopePrefix), nil
}

// voteRule is a resolved threshold configuration (§8.3).
type voteRule struct {
	Threshold   float64
	Criticality model.Criticality
}

// resolveVoteRule reads a named rule from the engine-config registry.
//
// V26 already refused, at REGISTER time, a workflow naming a rule that does not
// exist, so reaching this with an unregistered name means the rule was removed
// after the workflow registered. That is a real possibility and it fails
// loudly rather than silently tallying at some default: a threshold nobody
// chose is not a threshold.
func resolveVoteRule(conn *sql.DB, projectID int, name string) (voteRule, error) {
	thresholdEntry, err := db.GetConfig(conn, projectID, db.VoteRuleThresholdKey(name))
	if err != nil {
		return voteRule{}, fmt.Errorf("resolving vote rule %q: %w", name, err)
	}
	if thresholdEntry.Source != "set" {
		return voteRule{}, validationErr(
			"vote rule %q is not registered; set it with "+
				"`docket config set vote.rule.%s.threshold <0-1>`", name, name)
	}
	threshold, err := strconv.ParseFloat(thresholdEntry.Value, 64)
	if err != nil {
		return voteRule{}, fmt.Errorf(
			"vote rule %q has a malformed threshold %q: %w", name, thresholdEntry.Value, err)
	}

	criticalityEntry, err := db.GetConfig(conn, projectID, db.VoteRuleCriticalityKey(name))
	if err != nil {
		return voteRule{}, fmt.Errorf("resolving vote rule %q: %w", name, err)
	}

	return voteRule{
		Threshold:   threshold,
		Criticality: model.Criticality(criticalityEntry.Value),
	}, nil
}

// recordVoteEvent writes a vote lifecycle event in its own transaction.
func recordVoteEvent(
	conn *sql.DB, kind string, step *db.Step, data string, nowMS int64,
) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("recording a %s event: %w", kind, err)
	}
	defer tx.Rollback()
	if err := recordEvent(tx, eventRecord{
		Kind: kind, RunID: step.RunID, Instance: step.Instance,
		IssueID: step.IssueID, Data: data, AtMS: nowMS,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// routeVoteStep is §8.1 phase 5: the ordinary routing transaction, over the
// verdict the tally produced.
//
// It is DELIBERATELY the same shape as a human gate's decision (human.go's
// approve/reject): `pass` ⇒ the step is done and successors become ready;
// `fail` ⇒ routed per the step's EFFECTIVE on_fail — identically to a human
// gate's reject.
//
// On a DECLARED vote step that on_fail should not be `waiting-human`, for the
// reason V13 states about human gates: parking would make the step wait on the
// resolution of the thing that just rejected it. V13 and V13a are written
// against `type="human"` ONLY, so the grammar does not enforce that here — a
// declared vote step omitting `on_fail` inherits §11.1's `waiting-human`
// default and parks. Recorded rather than assumed away, because this comment
// previously claimed a guarantee the validator does not make.
//
// A MATERIALIZED held step is the deliberate exception: `waiting-human` is what
// its synthesized spec declares (workflow.MaterializedHeldVoteStep), because a
// tally that did not reach its threshold decided nothing and the question then
// belongs to the operator. That park waits on somebody who has not been asked
// yet, which is not the deadlock V13 forbids.
func routeVoteStep(
	conn *sql.DB, step *db.Step, def *workflow.Definition, spec *workflow.Step,
	outcome *VoteOutcome, nowMS int64,
) error {
	routing := RoutingPass
	if outcome.Verdict == VerdictFail {
		routing = spec.EffectiveOnFail()
	}

	// A MATERIALIZED held step that PASSED resolves its cluster's payload in
	// the same transaction as the decision, which is §7.7.3's rule for
	// `approve` and is the same rule here: a tally that passes IS the
	// approve-computed answer (H14), so it must leave the same record. Without
	// it the cluster would route as decided and the payload would still read
	// `held` with nothing saying anyone had endorsed it — the boolean DKT-84
	// exists to make meaningful, absent entirely.
	//
	// The routing step is read BEFORE the transaction opens because the read
	// takes the pooled connection, which inside the transaction would deadlock.
	// A failed tally resolves nothing: it parks the step, and the operator's
	// own approve is what records a resolution.
	var routingStep *db.Step
	if step.Materialized && routing == RoutingPass {
		var err error
		if routingStep, err = routingStepOf(conn, step); err != nil {
			return err
		}
	}

	// The tally is announced before the routing commits, carrying the score the
	// EXISTING computation produced — this stage reads it, never recomputes it.
	score := "no score"
	if outcome.Score != nil {
		score = strconv.FormatFloat(*outcome.Score, 'f', -1, 64)
	}
	if err := recordVoteEvent(conn, EventVoteTallied, step,
		fmt.Sprintf("%s %s (%s)", model.FormatProposalID(outcome.ProposalID),
			outcome.Status, score), nowMS); err != nil {
		return err
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("routing the vote step %s: %w", step.Instance, err)
	}
	defer tx.Rollback()

	// DKT-168: a rejected tally whose on_fail is `fix-loop` ENTERS the loop —
	// counter, sweep, ordinal-k fix steps, bound — exactly as a threshold
	// routing from the saga does. Before this, the routing string was written
	// with nothing downstream to consume it: RUN-25's security-vote rejected
	// with a reproduced blocker, routed `fix-loop`, and the issue closed done
	// with no fix step ever created.
	reason := string(outcome.Status)
	var loop *LoopOutcome
	routing, loop, err = applyFixLoop(tx, step, def, routing, nowMS)
	if err != nil {
		return err
	}
	if loop != nil && loop.Reason != "" {
		reason = loop.Reason
	}
	status := statusForRouting(routing)

	if err := db.SetStepRoutingTx(tx, step.ID,
		routingRecord(routing, reason), status, nowMS); err != nil {
		return err
	}
	if routingStep != nil {
		// AFTER the routing above, so this cluster's own verdict is visible to
		// resolveHeldPayload's per-cluster read — the same ordering
		// decideMaterializedStep documents, and for the same reason: the read
		// asks each cluster step what it decided, so an unwritten verdict reads
		// as undecided and nothing is marked.
		//
		// No note and no corrected value: a passing tally endorses the computed
		// value as computed. A correction is an OPERATOR's verb, and it arrives
		// through `approve --value` on a hold a failed tally parked.
		res := heldResolution{Element: -1}
		if element, ok := heldClusterElementOf(
			step.Instance, heldStepInstance(routingStep)); ok {
			res.Element = element
		}
		if err := resolveHeldPayload(tx, routingStep, res, nowMS); err != nil {
			return err
		}
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepRouted, RunID: step.RunID, Instance: step.Instance,
		IssueID: step.IssueID, Data: routing, AtMS: nowMS,
	}); err != nil {
		return err
	}
	if err := reconcileIssueAndRun(tx, step, spec, routing, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}
