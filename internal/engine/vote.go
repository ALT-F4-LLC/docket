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
	// Required is the proposal's `required_voters` — the denominator a ballot
	// count is read against (DKT-895). Carried because the proposal read below
	// already has it; nothing recomputes quorum from it.
	Required int
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
		if k, ok := parseVoteStepKey(prefix, key); ok {
			out[k] = id
		}
	}
	return out, nil
}

// parseVoteStepKey recovers the (issue, instance) half of a vote-step
// idempotency key under one run's prefix — the inverse of voteIdempotencyKey,
// kept beside the bulk reader so the two readers of the family cannot parse
// it differently. The second return is false for a key of another shape.
func parseVoteStepKey(prefix, key string) (voteStepKey, bool) {
	suffix, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return voteStepKey{}, false
	}
	issuePart, instance, ok := strings.Cut(suffix, ":")
	if !ok {
		return voteStepKey{}, false
	}
	issueID, err := strconv.Atoi(issuePart)
	if err != nil {
		return voteStepKey{}, false
	}
	return voteStepKey{Issue: model.FormatID(issueID), Instance: instance}, true
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

	// A TRIAGE PANEL OPENS WITH THE FAILURE IT IS ASKED ABOUT (DKT-1901). The
	// question is "what should happen to this failed step", and a panel that
	// cannot see the gate rows and the attempt's output is being asked to decide
	// it blind — which is how the corpus's override-passes came to be rubber
	// stamps. Empty for every other vote step, so their proposals are unchanged.
	rationale := fmt.Sprintf("workflow vote step %s", step.Instance)
	if evidence, err := triageEvidence(conn, step); err != nil {
		return 0, err
	} else if evidence != "" {
		rationale += "\n\n" + evidence
	}

	proposal := &model.Proposal{
		ProjectID:   projectID,
		Description: fmt.Sprintf("%s (%s)", step.Instance, spec.Name),
		Rationale:   rationale,
		Criticality: rule.Criticality,
		Threshold:   rule.Threshold,
		Sealed:      rule.Sealed,
		// §8.2: required_voters is len(voters), NOT a config value. A rule is
		// about HOW STRICTLY TO TALLY; the step is about WHO CASTS, and §11.1
		// puts the voter list on the step.
		//
		// The voter hints themselves are OPAQUE: core never interprets one,
		// never dispatches to one, and by default never validates a cast
		// against one — it counts them. A step that declares `roster =
		// "strict"` opts into the one comparison the cast path then makes
		// (vote_cast.go): the name on the cast must be one of these hints.
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
	return readVoteProposalOutcome(conn, step)
}

// ReadStepVoteOutcome is ReadVoteOutcome asked of the STEP ROW rather than the
// pinned spec: the same single read, keyed off `step.Kind`.
//
// It exists for callers that have a step and no spec, and must not grow a
// second read of proposals to compensate (DKT-726). `step resolve` is the
// motivating one — a resolution is offered on a vote step whatever its status
// (R11), so the refusal it needs to compute has to be decidable before the
// pinned definition is even loaded, and for a MATERIALIZED step the definition
// never declares the minted name at all, so `workflow.StepByName` returns nil
// there and the spec-keyed reader could never be called.
//
// The MINTED KIND is the authority for what a step is — the same fact the
// `resolvable` test and the parked-vote refusal above it already key off. A
// step whose kind is not `vote` has no proposal by construction, and reports
// nothing.
func ReadStepVoteOutcome(conn *sql.DB, step *db.Step) (*VoteOutcome, error) {
	if step.Kind != workflow.TypeVote {
		return nil, nil
	}
	return readVoteProposalOutcome(conn, step)
}

// readVoteProposalOutcome is the one read both entry points share: resolve the
// step's proposal through its idempotency key and observe the status the tally
// already wrote.
func readVoteProposalOutcome(conn *sql.DB, step *db.Step) (*VoteOutcome, error) {
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
		Required:   proposal.RequiredVoters,
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
	// Sealed is the rule's opt-in rendering dimension (DKT-2447): a proposal
	// opened under a sealed rule withholds its casts from the read verbs until
	// the tally closes it. Resolved here and STORED on the proposal at open,
	// so a rule edited mid-vote cannot change a live ballot's rendering.
	Sealed bool
	// A rule carries NO roster and NO weighting (DKT-2764). Both once sat here
	// as `vote.rule.<name>.roster` / `.weighting` config keys; they moved onto
	// the vote step (`roster`, `weighting`, workflow/vote_policy.go) because a
	// config key has no per-caller identity and a constrained seat could flip
	// it before casting. A legacy row under either key is ignored by design —
	// the step's declaration, pinned at activation, is the only authority.
	//
	// HoldOnDissent is the rule's opt-in routing dimension (DKT-2449): an
	// APPROVED tally carrying at least one `reject` parks its vote step for
	// the operator rather than passing, with the dissenting seat named in the
	// routing record. Unlike Sealed it is read at ROUTE time rather than
	// stored on the proposal at open, so a key edited between the last cast
	// and the routing invocation governs that routing.
	HoldOnDissent bool
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

	sealedEntry, err := db.GetConfig(conn, projectID, db.VoteRuleSealedKey(name))
	if err != nil {
		return voteRule{}, fmt.Errorf("resolving vote rule %q: %w", name, err)
	}
	sealed, err := strconv.ParseBool(sealedEntry.Value)
	if err != nil {
		return voteRule{}, fmt.Errorf(
			"vote rule %q has a malformed sealed flag %q: %w", name, sealedEntry.Value, err)
	}

	holdEntry, err := db.GetConfig(conn, projectID, db.VoteRuleHoldOnDissentKey(name))
	if err != nil {
		return voteRule{}, fmt.Errorf("resolving vote rule %q: %w", name, err)
	}
	// A malformed stored value fails loudly rather than reading as false: a
	// silent false would fail OPEN with respect to the hold, passing a
	// dissented approval the operator asked to see.
	holdOnDissent, err := strconv.ParseBool(holdEntry.Value)
	if err != nil {
		return voteRule{}, fmt.Errorf(
			"vote rule %q has a malformed hold_on_dissent flag %q: %w",
			name, holdEntry.Value, err)
	}

	return voteRule{
		Threshold:     threshold,
		Criticality:   model.Criticality(criticalityEntry.Value),
		Sealed:        sealed,
		HoldOnDissent: holdOnDissent,
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

// voteTallyDetail renders the `vote-tallied` event's detail: the proposal, the
// status, the WEIGHTED SCORE, and the BALLOT COUNT, each labelled.
//
// DKT-895: the detail used to read `DKT-V289 approved (1)`, where `1` was the
// weighted score printed bare. On RUN-62 a three-ballot unanimous
// approve-with-concerns scored 1.00 and rendered as `(1)` — indistinguishable
// from "one ballot", so a reader watching the feed saw a panel that had lost
// two of its three seats and could only disprove it with `docket vote show`.
// Two numbers with one pair of parentheses between them is the whole defect:
// the fix labels both and prints the score the way every other surface does
// (`%.2f`, matching `vote show`'s "Weighted score" line, so the two agree
// digit for digit).
//
// The ballot count is the ONE extra read, taken here at tally time rather than
// on VoteOutcome, so the per-invocation outcome read (readVoteProposalOutcome,
// which every dispatch does for every vote step) does not grow a second query
// to serve an event detail written once — DKT-726's rule about that reader.
// `RequiredVoters` is free: the proposal that read already loaded carries it.
func voteTallyDetail(conn *sql.DB, outcome *VoteOutcome) (string, error) {
	score := "none"
	if outcome.Score != nil {
		score = strconv.FormatFloat(*outcome.Score, 'f', 2, 64)
	}

	votes, err := db.GetProposalVotes(conn, outcome.ProposalID)
	if err != nil {
		return "", fmt.Errorf("reading the casts of %s for its tally event: %w",
			model.FormatProposalID(outcome.ProposalID), err)
	}

	return fmt.Sprintf("%s %s score=%s ballots=%d/%d",
		model.FormatProposalID(outcome.ProposalID), outcome.Status,
		score, len(votes), outcome.Required), nil
}

// routeVoteStep is §8.1 phase 5: the ordinary routing transaction, over the
// verdict the tally produced.
//
// It is DELIBERATELY the same shape as a human gate's decision (human.go's
// approve/reject): `pass` ⇒ the step is done and successors become ready;
// `fail` ⇒ routed per the step's EFFECTIVE on_fail — identically to a human
// gate's reject.
//
// DKT-545 adds one clause between the two: an APPROVED tally on a step that
// declares a `threshold` is evaluated over the recorded casts before it
// routes pass — see evaluateVoteThreshold. A step declaring none behaves
// exactly as the paragraph above describes, byte for byte.
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
	// The step this panel was asked about, if any (DKT-1901). Read BEFORE the
	// transaction opens, for the reason every other pooled read here is: inside
	// it the pooled connection would deadlock rather than fail.
	triaged, err := triageRouter(conn, step, def)
	if err != nil {
		return err
	}

	// A proposal retired WITHOUT a tally routes nothing on its own: no verdict
	// was reached, so the vote step keeps waiting exactly as it always has. The
	// one thing it must do is release a step suspended behind it, which is the
	// only reason this function is reached for a closed proposal at all.
	if outcome.Verdict == "" {
		if triaged == nil {
			return nil
		}
		tx, err := conn.Begin()
		if err != nil {
			return fmt.Errorf("releasing the step %s triaged: %w", step.Instance, err)
		}
		defer tx.Rollback()
		if err := releaseUntriaged(
			tx, step, triaged, spec, def, outcome, nowMS); err != nil {
			return err
		}
		return tx.Commit()
	}

	routing := RoutingPass
	concernReason := ""
	switch {
	case triaged != nil && triageDecided(outcome):
		// A TRIAGE PANEL THAT REACHED A VERDICT IS DONE, whichever way it
		// voted (DKT-1901). A rejection is not this step's own failure — it is
		// the answer it was convened to give, and `on_fail_routes` applies it
		// below. Routing the panel per its `on_fail` here would park the panel
		// on the question it just answered, and R2b would then hold every step
		// of the issue behind it: the triage lane would park exactly what it
		// exists to keep moving.
		//
		// The panel's own `on_fail` is reached only through the case below,
		// where the tally produced NO verdict and the mapping has nothing to
		// apply. That is the backstop the spec documents.
		routing = RoutingPass
	case outcome.Verdict == VerdictFail:
		routing = spec.EffectiveOnFail()
	case outcome.Status == model.ProposalStatusApproved && len(spec.Threshold) > 0:
		// DKT-545: an APPROVED tally with a declared `threshold` is asked one
		// more question — over the CAST SET, not the tally: an approval built
		// on approve-with-concerns casts can route into the same revise loop
		// a rejection does, instead of the concerns evaporating. No threshold
		// declared (every pre-existing workflow) means no evaluation and the
		// exact prior behavior; a COMMITTED proposal skips it too, because
		// §8.4's manual commit is an operator setting the final outcome by
		// hand. Evaluated OUTSIDE the transaction below, like every other
		// pooled read in this function.
		result, err := evaluateVoteThreshold(conn, step, spec, outcome.ProposalID)
		if err != nil {
			return err
		}
		if result.Routing != RoutingPass {
			routing, concernReason = result.Routing, result.Reason
		}
	}

	// A LONE REJECT UNDER A KEYED RULE PARKS THE STEP (DKT-2449). The tally is
	// a weighted mean and approve-with-concerns adds to it, so a dissenting
	// seat's findings left no trace in routing once the score cleared the
	// threshold.
	//
	// Placed AFTER the switch and guarded on `routing == RoutingPass`, which
	// is what makes the park strictly ADDITIVE: it can only ever displace a
	// pass. A triage panel (which the first arm passed) is excluded
	// explicitly, because parking it on the question it just answered is what
	// DKT-1901 forbids; a rejected tally keeps its `on_fail` because the
	// second arm already moved `routing`; a `threshold` match keeps its own
	// routing for the same reason; and a COMMITTED proposal is skipped by the
	// approved-status test, as §8.4's manual commit is the operator's answer.
	if routing == RoutingPass && outcome.Status == model.ProposalStatusApproved &&
		!(triaged != nil && triageDecided(outcome)) {
		dissentReason, err := dissentHold(conn, step, spec, outcome.ProposalID)
		if err != nil {
			return err
		}
		if dissentReason != "" {
			routing, concernReason = workflow.OnFailWaitingHuman, dissentReason
		}
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
	detail, err := voteTallyDetail(conn, outcome)
	if err != nil {
		return err
	}
	if err := recordVoteEvent(conn, EventVoteTallied, step, detail, nowMS); err != nil {
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
	// The class follows the SAME branch that chose the routing above, read from
	// the tally and the threshold result rather than from the text either wrote:
	// a failed verdict is a rejection, and an approved tally that a declared
	// threshold routed away from `pass` is a threshold park.
	class := db.ParkClassVoteRejected
	if outcome.Verdict != VerdictFail && concernReason != "" {
		class = db.ParkClassThresholdRouted
	}
	reason := string(outcome.Status)
	if concernReason != "" {
		// The concern routing's record names the matched predicate (or the T3
		// park's cause), because "approved" alone would read as a pass to
		// anyone auditing why the step did not route pass.
		reason = concernReason
	}
	var loop *LoopOutcome
	routing, loop, err = applyFixLoop(tx, step, def, routing, nowMS)
	if err != nil {
		return err
	}
	if loop != nil && loop.Reason != "" {
		reason = loop.Reason
	}
	if bound, ok := loopBoundClass(loop); ok {
		class = bound
	}
	status := statusForRouting(routing)

	if err := db.SetStepRoutingTx(tx, step.ID,
		routing, reason, status, class, nowMS); err != nil {
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
	// THE PANEL'S VERDICT REACHES THE STEP IT TRIAGED (DKT-1901), in this same
	// transaction: a verdict recorded without its consequence is the defect
	// DKT-168 fixed for `fix-loop`, and it would leave the triaged step
	// suspended with nothing left to resolve it.
	//
	// A REJECTION IS A DECISION and applies its mapped routing: the panel read
	// the failure and declined to endorse the work, which is exactly one of the
	// two verdicts the mapping is keyed on. What applies NOTHING is a tally that
	// reached no verdict at all — a quorum miss, or a proposal retired without a
	// tally — because there the panel could not agree and the step's disposition
	// is still an open question. It stays suspended, and this vote step's own
	// `on_fail`, which V13a requires it to declare, is the human backstop.
	if triaged != nil {
		if triageDecided(outcome) {
			if err := applyTriageOutcome(
				tx, step, spec, def, triaged, triageVerdict(outcome), nowMS,
			); err != nil {
				return err
			}
		} else if err := releaseUntriaged(
			tx, step, triaged, spec, def, outcome, nowMS); err != nil {
			// A verdict outside the mapping's vocabulary — an operator's manual
			// commit (§8.4). The panel reached an outcome the mapping has no key
			// for, so its own `on_fail` disposes of the step rather than this
			// guessing which of two keys was meant.
			return err
		}
	}
	if err := recordEvent(tx, eventRecord{
		Kind: EventStepRouted, RunID: step.RunID, Instance: step.Instance,
		IssueID: step.IssueID, Data: routingRecord(routing, reason), AtMS: nowMS,
	}); err != nil {
		return err
	}
	if err := reconcileIssueAndRun(tx, step, def, spec, routing, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}
