package workflow

import "strings"

// The materialized held step's identity and its synthesized spec
// (docs/tdd/payloads-thresholds.md §7.7.1 H1, H5).
//
// A held step is the one row in a run that expansion did not write. It exists
// because §2 says a tripped `hold_spread` "materializes a `type=human` step
// named `<step>-held` gating the routing step" — and the question it asks is
// about a computation the engine performed, so no author could have declared it.
//
// NOTHING UNPINNED ENTERS A RUN. The spec below is a PURE FUNCTION of the pinned
// definition plus the suffix, so §9 item 5's determinism is untouched: two runs
// at the same pins that hold the same cluster materialize the same step.

// HeldSuffix is the reserved step-name suffix (§7.7.1 H1). V27 refuses a
// declared step name ending in it, so a definition cannot collide with an
// identity the engine mints.
//
// It is a constant shared by the validator, the materializer, and the loop
// sweep rather than three string literals: a reservation enforced in one place
// and spelled differently in another is a reservation that holds until the
// first typo.
const HeldSuffix = "-held"

// GateResultsKind is the reserved `<step>.gate-results` input suffix (DKT-77):
// the named step's RECORDED gate results, engine-served from the ledger. A
// definition may not emit an artifact of this kind — the input form would
// shadow it — which V11a refuses at register.
const GateResultsKind = "gate-results"

// VoteRecordKind is the reserved `<step>.vote-record` input suffix (DKT-545):
// the named vote step's RECORDED proposal — tally outcome, casts, and
// rationales — engine-served from the existing proposal machinery.
// GateResultsKind's reasoning applied to the vote record: a definition may not
// emit an artifact of this kind (V11b), and the input form resolves only
// against `type="vote"` steps — any other step opens no proposal, so the
// input could never resolve to anything on any run (V11).
const VoteRecordKind = "vote-record"

// HeldStepName renders the materialized step's name for a routing step.
func HeldStepName(step string) string { return step + HeldSuffix }

// RoutingStepNameOf reverses HeldStepName: the routing step a materialized name
// belongs to, and whether the name is a materialized one at all.
func RoutingStepNameOf(name string) (string, bool) {
	return strings.CutSuffix(name, HeldSuffix)
}

// MaterializedHeldStep synthesizes the spec of `<step>-held` from the PINNED
// definition (§7.7.1 H5).
//
// Kind `human`, no `after`, no gates, no threshold, and `on_fail` inherited from
// the routing step. Each is decided rather than defaulted:
//
//   - `human` because §2 says so, and because approve/reject are the verbs that
//     resolve it.
//   - no `after`: R3 is vacuous, which is what makes it ready the moment it
//     exists — correct, because the question it asks is already answered by data
//     on disk (H6).
//   - no gates and no threshold: it produces no artifact and judges no result.
//     A gate on a question about a computation would be a check of the
//     operator, not of the work.
//   - `on_fail` from the routing step so V13a's "a human step declares on_fail
//     explicitly" is satisfied by derivation. The rejection's CONSEQUENCE lands
//     on the routing step (H14), and taking its routing is what makes `reject`
//     mean "route the aggregate per its own on_fail" rather than inventing a
//     second policy.
//
// It returns nil when the name is not a materialized one, or when the routing
// step it names is not in the definition — a materialized step whose routing
// step vanished is a definition edited under a live saga, and synthesizing a
// spec out of nothing would hide it.
func MaterializedHeldStep(def *Definition, name string) *Step {
	step := materializedHeldStep(def, name)
	if step == nil {
		return nil
	}
	step.Type = TypeHuman
	return step
}

// MaterializedHeldVoteStep synthesizes the same step for an instance that has
// configured a TALLY over held clusters instead of one operator's decision.
//
// Everything MaterializedHeldStep decides is decided identically here — same
// name, same absent `after`, same absent gates and threshold — and exactly two
// rows differ:
//
//   - `vote` rather than `human`, with the `voters` and `vote_rule` the
//     instance configured. They cannot come from the definition the way a
//     DECLARED vote step's do: this step is the one row no author wrote, so
//     there is no `[[step]]` table to carry them.
//   - `on_fail` is `waiting-human` rather than the routing step's, because a
//     failed tally is NOT the same answer as a rejection. A rejection is a
//     decision, and it routes the aggregate per its own `on_fail` (H14). A
//     tally that did not reach its threshold decided nothing — it declined to
//     endorse the computed value — and the question then belongs to the
//     operator, so the step PARKS and waits for one.
//
// That difference is the whole safety property: a tally may confirm the
// engine's own computation, and may not overrule it. Routing the aggregate on a
// failed tally would let a panel that could not agree produce the same effect
// as an operator who declined, which is a decision nobody made.
//
// A vote-minted step's `waiting-human` `on_fail` is legal where V13 forbids one
// on a DECLARED human gate, and the two are not in tension: V13's deadlock is a
// step parking on its own decision, and this step's park waits on an operator
// who has not been asked yet.
//
// UNPINNED. `voters` and `vote_rule` come from live engine config rather than
// from the pinned definition, which is the one place a materialized spec is not
// a pure function of the pins. It is recorded here rather than hidden: the
// alternative was a schema column carrying the roster on every held row, and
// the roster is a project-level policy, not a fact about one hold. The MINTED
// KIND is what persists — the engine reads the step row's own kind to decide
// which of these two functions to call — so a roster edited mid-run changes who
// casts, never whether the question was a tally at all.
func MaterializedHeldVoteStep(def *Definition, name, rule string, voters []string) *Step {
	step := materializedHeldStep(def, name)
	if step == nil {
		return nil
	}
	step.Type = TypeVote
	step.VoteRule = rule
	step.Voters = append([]string(nil), voters...)
	step.OnFail = OnFailWaitingHuman
	return step
}

// materializedHeldStep is everything the two forms share: the identity, the
// root `after`, and the routing step's `on_fail` as the default the caller may
// replace. Written once so the two cannot drift into two different held steps.
func materializedHeldStep(def *Definition, name string) *Step {
	if def == nil {
		return nil
	}
	routing, ok := RoutingStepNameOf(name)
	if !ok {
		return nil
	}
	parent := StepByName(def, routing)
	if parent == nil {
		return nil
	}
	return &Step{
		Name:   name,
		OnFail: parent.EffectiveOnFail(),
		// `after = []` explicitly, not omitted: an empty declared `after` is a
		// ROOT declaration (V10), which is exactly what H6 says this step is.
		After:    []string{},
		hasAfter: true,
	}
}
