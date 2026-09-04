package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// Concern-aware vote routing and the `<step>.vote-record` input form (DKT-545).
//
// The gap this closes, measured on the corpus: every tribunal tally that ever
// ran APPROVED, and none was clean — DKT-V34 passed 2-1 over a substantive
// security dissent, DKT-V140 and DKT-V160 each passed with two
// approve-with-concerns casts — and a workflow definition had no way to act on
// any of it. Only the binary approved/rejected tally reached routing
// (ReadVoteOutcome), `on_fail` fired only on a rejection, and the concern text
// lived on the proposal record where no `inputs` form could address it. The
// concerns evaporated, or the operator carried them by hand.
//
// Two pieces, mirroring machinery that already exists rather than inventing
// any:
//
//   - a vote step's `threshold` table, evaluated over the CAST SET after an
//     APPROVED tally — the executor threshold's exact evaluator
//     (EvaluateThreshold, §6.14 T1/T4) over engine-built cast payloads, with
//     the same first-match-routes order and the same no-match ⇒ pass default.
//     A rejected tally still routes per `on_fail`, untouched; a step declaring
//     no threshold behaves exactly as before.
//   - `<step>.vote-record`, resolving to the proposal record the named vote
//     step's tally left — outcome, casts, and rationales — the gate-results
//     form's reasoning (DKT-77) applied to the vote machinery, so a revise
//     step reads WHAT the panel said without shelling out to `docket vote
//     show`.
//
// THE TALLY IS STILL db.CastVote'S. Nothing here recomputes a weighted score
// or consults a quorum; the threshold asks a question the tally does not —
// "how many casts carried concerns" — and reads the casts the existing
// machinery recorded to answer it.

// evaluateVoteThreshold applies a vote step's `threshold` table to its
// proposal's recorded casts (DKT-545).
//
// Called by routeVoteStep after an APPROVED tally only:
//
//   - a REJECTED tally routes per `on_fail`, exactly as before — the threshold
//     asks "was the approval clean", which is not a question about a rejection.
//   - a COMMITTED proposal skips it too: §8.4's manual commit is an operator
//     setting the final outcome by hand, and a threshold overriding that would
//     re-open a question a person just closed.
//
// The schema resolver is nil — casts have no registered payload schema — which
// makes equality the whole language (T1 needs no schema) and any ordered
// comparison a T3 park. V36 refuses ordered operators at register, so a park
// here means the definition arrived without passing register-time validation
// (a restored database), and the park is the honest answer.
func evaluateVoteThreshold(
	conn *sql.DB, step *db.Step, spec *workflow.Step, proposalID int,
) (ThresholdResult, error) {
	votes, err := db.GetProposalVotes(conn, proposalID)
	if err != nil {
		return ThresholdResult{}, fmt.Errorf(
			"reading the casts of %s for its threshold: %w",
			model.FormatProposalID(proposalID), err)
	}

	result, err := EvaluateThreshold(
		step.Instance, spec.Threshold, ThresholdOrder(spec.Threshold),
		voteCastPayloads(votes), nil)
	if err != nil {
		return ThresholdResult{}, err
	}

	// A matched routing carries the predicate verbatim in its reason (the
	// same courtesy T3 extends): the operator reading the routing record sees
	// WHY an approved tally did not pass, not just where it went.
	if result.Routing != RoutingPass && !result.Parked {
		result.Reason = fmt.Sprintf(
			"approved, and threshold %q matched: %s",
			result.Routing, spec.Threshold[result.Routing])
	}
	return result, nil
}

// voteCastPayloads renders recorded casts as threshold payloads — one element
// per cast, keyed by EXACTLY workflow.VoteCastFields, which is what V36
// validates predicates against. A key here that the validator does not admit,
// or vice versa, is the drift both sides importing one list prevents.
func voteCastPayloads(votes []*model.Vote) []map[string]any {
	out := make([]map[string]any, 0, len(votes))
	for _, v := range votes {
		out = append(out, map[string]any{
			// `vote` and `verdict` are aliases for the same value — see the
			// VoteCastField constants for why both spellings are admitted.
			workflow.VoteCastFieldVote:    string(v.Verdict),
			workflow.VoteCastFieldVerdict: string(v.Verdict),
			workflow.VoteCastFieldVoter:   v.VoterName,
		})
	}
	return out
}

// resolveVoteRecords is the `<step>.vote-record` form (DKT-545): one input per
// recorded producer instance of the named vote step, carrying that instance's
// proposal record — tally outcome, score, and every cast with its rationale —
// as JSON.
//
// The instance selection mirrors resolveGateResults exactly — same issue,
// recorded producers only, ordinal-scoped with the per-input fallback, ordered
// by (sibling index, id) — because the question is the same one with a
// different ledger: "what did the producer record". The one departure the
// gate-results form makes (self-admission for pre-gates) is NOT made here: a
// vote step is never claimed, so no step ever reads its own vote-record at
// claim time.
//
// A recorded vote step WITHOUT a proposal — one an operator moved past with
// `docket step resolve` before its ballot opened — contributes no input:
// there is no record, and fabricating an empty one would report "a vote
// happened and nobody cast" about a vote that never opened.
func resolveVoteRecords(
	tx *sql.Tx, sched *Scheduler, step *db.Step, stepName string,
) ([]ContextInput, error) {
	var candidates []*db.Step
	best := -1
	for _, s := range sched.steps {
		if s.IssueID != step.IssueID || s.StepName != stepName ||
			s.Kind != workflow.TypeVote {
			continue
		}
		if s.Ordinal > step.Ordinal || !recordedProducer(s.Status) {
			continue
		}
		if s.Ordinal > best {
			best = s.Ordinal
		}
		candidates = append(candidates, s)
	}
	if best < 0 {
		return nil, nil
	}

	producers := make([]*db.Step, 0, len(candidates))
	for _, s := range candidates {
		if s.Ordinal == best {
			producers = append(producers, s)
		}
	}
	sort.SliceStable(producers, func(i, j int) bool {
		si, sj := -1, -1
		if producers[i].SiblingIndex != nil {
			si = *producers[i].SiblingIndex
		}
		if producers[j].SiblingIndex != nil {
			sj = *producers[j].SiblingIndex
		}
		if si != sj {
			return si < sj
		}
		return producers[i].ID < producers[j].ID
	})

	out := make([]ContextInput, 0, len(producers))
	for _, producer := range producers {
		proposalID, found, err := db.LookupIdempotencyKeyTx(
			tx, db.ScopeVoteCreate,
			voteIdempotencyKey(producer.RunID, producer.IssueID, producer.Instance))
		if err != nil {
			return nil, fmt.Errorf(
				"resolving the proposal of %s: %w", producer.Instance, err)
		}
		if !found {
			continue
		}
		proposal, err := db.GetProposalTx(tx, proposalID)
		if err != nil {
			return nil, fmt.Errorf(
				"reading the proposal of %s: %w", producer.Instance, err)
		}
		votes, err := db.GetProposalVotesTx(tx, proposalID)
		if err != nil {
			return nil, fmt.Errorf(
				"reading the casts of %s: %w", producer.Instance, err)
		}
		body, err := encodeVoteRecord(proposal, votes)
		if err != nil {
			return nil, fmt.Errorf(
				"encoding the vote record of %s: %w", producer.Instance, err)
		}
		out = append(out, ContextInput{
			Artifact:     workflow.VoteRecordKind,
			Kind:         workflow.VoteRecordKind,
			ProducerStep: producer.Instance,
			Body:         body,
		})
	}
	return out, nil
}

// encodeVoteRecord renders one proposal and its casts as the vote-record body.
//
// The vocabulary is the model's where the model has one (`verdict`,
// `weighted_score`) and the packet's where it does not (`rationale` is the
// cast's summary — the prose a seat wrote to explain itself, which is exactly
// what a downstream revise step consumes). Structured findings ride along when
// the seat recorded them; a cast without them omits the key rather than
// carrying an empty object.
func encodeVoteRecord(p *model.Proposal, votes []*model.Vote) (string, error) {
	type cast struct {
		Voter      string          `json:"voter"`
		Role       string          `json:"role,omitempty"`
		Verdict    string          `json:"verdict"`
		Confidence float64         `json:"confidence"`
		Rationale  string          `json:"rationale,omitempty"`
		Findings   *model.Findings `json:"findings,omitempty"`
	}
	type wire struct {
		Proposal      string   `json:"proposal"`
		Status        string   `json:"status"`
		WeightedScore *float64 `json:"weighted_score,omitempty"`
		Casts         []cast   `json:"casts"`
	}

	encoded := wire{
		Proposal:      model.FormatProposalID(p.ID),
		Status:        string(p.Status),
		WeightedScore: p.WeightedScore,
		Casts:         make([]cast, 0, len(votes)),
	}
	for _, v := range votes {
		encoded.Casts = append(encoded.Casts, cast{
			Voter: v.VoterName, Role: v.VoterRole, Verdict: string(v.Verdict),
			Confidence: v.Confidence, Rationale: v.Summary,
			Findings: v.FindingsJSON,
		})
	}

	body, err := json.Marshal(encoded)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
