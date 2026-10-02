package engine

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// THE CAST PATH'S AUTHORIZATION (DKT-2541).
//
// A vote step declares three switches in its `[[step]]` table — `roster`,
// `weighting`, `recuse` — and activation pins them with `voters`
// (workflow/vote_policy.go). This file is where the cast path READS them: it
// resolves the proposal back to the pinned vote step, refuses a cast the step
// excludes, and hands db.CastVoteWeighted the weighting the step chose.
//
// The switches live on the step and not in engine config because `docket
// config set` has no per-caller identity: a seat constrained by a rule could
// rewrite the rule before casting. A pinned definition cannot be rewritten
// under a live ballot, and a caster who wants a different roster has to get a
// different workflow registered and a new run activated (operator decision,
// 2026-09-23).
//
// It is a separate file from vote.go on purpose: that file's contract is
// "wiring only, no vote semantics" and its test scans its source for the
// tally's own input names. Nothing here computes a score either — the tally
// is still db.CastVote's, reached through CastVoteWeighted with one flag.

// CastVote is the engine half of `docket vote cast`: authorize the cast
// against the vote step's pinned switches, then record it through the existing
// tally. An ad-hoc proposal (no vote step behind it) and a materialized held
// step (whose minted name the pinned definition never declares) enforce
// nothing and tally exactly as before.
//
// A refusal is a VALIDATION_ERROR, raised BEFORE the transaction opens, so a
// refused cast writes no vote row. The db sentinels (ErrNotFound, ErrConflict)
// come back unwrapped from the tally, as they always did.
func CastVote(conn *sql.DB, vote *model.Vote) (*db.CastVoteResult, error) {
	policy, err := castPolicyOf(conn, vote.ProposalID)
	if err != nil {
		return nil, err
	}
	if err := policy.authorize(vote.VoterName); err != nil {
		return nil, err
	}
	return db.CastVoteWeighted(conn, vote, policy.equalWeighting())
}

// castPolicy is the vote step behind one proposal, as pinned. The zero value —
// no declared vote step — is the permissive policy: open roster, declared
// weighting, no recusal, which is every ballot's behavior before DKT-2541.
type castPolicy struct {
	// spec is the pinned vote step, or nil for a proposal no declared step
	// opened.
	spec *workflow.Step
	// reviewed is the step spec.Reviews names, resolved from the same pinned
	// definition, or nil when the vote step declares no producer.
	reviewed *workflow.Step
}

// castPolicyOf resolves a proposal to its vote step's pinned switches.
//
// The route is the one every other reader of a vote-step proposal takes: the
// idempotency key OpenVoteProposal recorded (`vote-step:<run>:<issue>:<instance>`)
// names the run, the issue and the instance; the step row names the pinned
// workflow; the workflow's stored `parsed` form names the step. Nothing is
// re-parsed from TOML and nothing is read from engine config.
func castPolicyOf(conn *sql.DB, proposalID int) (castPolicy, error) {
	key, found, err := db.IdempotencyKeyOf(conn, db.ScopeVoteCreate, proposalID)
	if err != nil {
		return castPolicy{}, fmt.Errorf("resolving the vote step of %s: %w",
			model.FormatProposalID(proposalID), err)
	}
	if !found || !strings.HasPrefix(key, voteStepScopePrefix) {
		return castPolicy{}, nil
	}
	runID, ok := voteStepRunOf(key)
	if !ok {
		return castPolicy{}, nil
	}
	want, ok := parseVoteStepKey(voteIdempotencyPrefix(runID), key)
	if !ok {
		return castPolicy{}, nil
	}

	steps, err := db.ListRunSteps(conn, runID)
	if err != nil {
		return castPolicy{}, fmt.Errorf("reading the steps of %s for %s: %w",
			model.FormatRunID(runID), model.FormatProposalID(proposalID), err)
	}
	var step *db.Step
	for _, s := range steps {
		if voteStepKeyOf(s) == want {
			step = s
			break
		}
	}
	if step == nil {
		// The key names a step the run no longer has. Nothing to enforce
		// and nothing to fail on: the proposal is still a real ballot, and
		// the tally is what decides it.
		return castPolicy{}, nil
	}

	defs, err := StepDefinitions(conn, runID)
	if err != nil {
		return castPolicy{}, err
	}
	def := defs[step.WorkflowID]
	if def == nil {
		return castPolicy{}, nil
	}
	spec := workflow.StepByName(def, step.StepName)
	if spec == nil || spec.Type != workflow.TypeVote {
		// A materialized held step: the definition never declares its
		// minted name, so there is no `[[step]]` table to read switches from.
		return castPolicy{}, nil
	}
	policy := castPolicy{spec: spec}
	if spec.Reviews != "" {
		policy.reviewed = workflow.StepByName(def, spec.Reviews)
	}
	return policy, nil
}

// authorize refuses a cast the vote step excludes: a name off a strict roster,
// or the reviewed step's executor under `recuse = "executor"`. Every refusal
// names the step and the switch that refused, so a seat reading it on a
// command line knows which declaration it ran into.
func (p castPolicy) authorize(voter string) error {
	if p.spec == nil {
		return nil
	}

	if p.spec.EffectiveRoster() == workflow.RosterStrict &&
		!slices.Contains(p.spec.Voters, voter) {
		return validationErr(
			"vote step %q declares roster = %q, and %q is not one of its voters "+
				"(%s); a cast from off the roster is refused",
			p.spec.Name, workflow.RosterStrict, voter,
			strings.Join(p.spec.Voters, ", "))
	}

	// Recusal compares against the REVIEWED step's executor hint — its scalar
	// `executor`, or every sibling name of its `fanout` — never against the
	// vote step's own hint, and never against `class`, which defaults to the
	// executor name and would match the wrong party. With no `reviews`
	// declared there is no producer to compare against, and the switch is a
	// no-op by construction.
	if p.spec.EffectiveRecuse() == workflow.RecuseExecutor && p.reviewed != nil {
		if slices.Contains(executorHints(p.reviewed), voter) {
			return validationErr(
				"vote step %q declares recuse = %q and reviews step %q, whose "+
					"executor %q may not cast on its own work",
				p.spec.Name, workflow.RecuseExecutor, p.reviewed.Name, voter)
		}
	}
	return nil
}

// equalWeighting reports whether the tally should count every cast the same.
func (p castPolicy) equalWeighting() bool {
	return p.spec != nil && p.spec.EffectiveWeighting() == workflow.WeightingEqual
}

// executorHints is every name a step's work could have been produced under:
// the scalar `executor`, or each entry of a per-sibling `fanout`.
func executorHints(step *workflow.Step) []string {
	var out []string
	if step.Executor != "" {
		out = append(out, step.Executor)
	}
	out = append(out, step.Fanout...)
	return out
}
