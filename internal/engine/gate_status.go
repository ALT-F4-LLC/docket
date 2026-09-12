package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// `docket gate status` — DKT-1286.
//
// A vote row cost a wave three to five separate read-only probes — step show
// for the row and target, vote show for the seat roster and tally, and a
// re-derived outcome — each relayed by an agent and re-parsed with regex
// fallbacks. tribunal.js reads the same scatter for conversational gates.
//
// GateStatus answers all of it in ONE composed read: the step's effective
// status (LoadStepView, unchanged), and — on a vote step — the proposal it
// opened, its tally, and which declared seats have and have not cast. IT
// WRITES NOTHING: every read it composes already writes nothing on its own.

// GateOutcome is a gate's decided-or-not verdict, in the three states a
// caller ever needs to branch on. It collapses `model.ProposalStatusClosed`
// (a proposal retired without a tally) into "open" — a closed proposal
// answered no vote and a gate status probe exists to say which way a gate
// was decided, not to add a fourth state nothing above this asks for.
type GateOutcome string

const (
	GateOutcomeApproved GateOutcome = "approved"
	GateOutcomeRejected GateOutcome = "rejected"
	GateOutcomeOpen     GateOutcome = "open"
)

// GateTally is a vote gate's score against its threshold. Nil on a human gate
// and on a vote gate whose proposal has not opened yet — a gate with no
// proposal has nothing to tally.
type GateTally struct {
	WeightedScore *float64
	Threshold     float64
}

// GateSeat is one declared voter's cast state on a vote gate.
type GateSeat struct {
	Voter   string
	Cast    bool
	Verdict string
}

// GateTarget is the git ref a gate judges — the same resolved pair
// StepView.TargetSHA/TargetWorktree carries, present only when the step's
// packet named one.
type GateTarget struct {
	SHA      string
	Worktree string
}

// GateStatusResult is `gate status`'s one-call envelope.
type GateStatusResult struct {
	StepStatus string
	// Proposal is the display id of the vote this gate opened, empty on a
	// human gate and on a vote gate whose proposal has not opened yet.
	Proposal     string
	Outcome      GateOutcome
	Tally        *GateTally
	Seats        []GateSeat
	MissingSeats []string
	Target       *GateTarget
}

// GateStatus reads STEP-N's decision state: step status, outcome, and — for a
// vote step with an open proposal — the tally and every declared seat's cast
// state, all from ONE StepView plus the two proposal reads a seat roster
// needs.
//
// stepID must name a `type="human"` or `type="vote"` step — the same
// restriction `guard gate` applies, because every other step kind has no
// decision to report status for.
func GateStatus(conn *sql.DB, stepID int, nowMS int64) (*GateStatusResult, error) {
	view, err := LoadStepView(conn, stepID, nowMS)
	if err != nil {
		return nil, err
	}
	if view.Step.Kind != workflow.TypeHuman && view.Step.Kind != workflow.TypeVote {
		return nil, validationErr(
			"%s is a %q step, not a gate — `gate status` answers for "+
				"`type=\"human\"` or `type=\"vote\"` steps only",
			model.FormatStepID(stepID), view.Step.Kind)
	}

	result := &GateStatusResult{StepStatus: view.Step.Status}
	if view.TargetSHA != "" || view.TargetWorktree != "" {
		result.Target = &GateTarget{SHA: view.TargetSHA, Worktree: view.TargetWorktree}
	}

	if view.Step.Kind == workflow.TypeHuman {
		result.Outcome = humanGateOutcome(view.Step.Status, view.Routing)
		return result, nil
	}

	// The seat roster is known from the pinned definition whether or not a
	// proposal has opened — `next` hands it out the same way (StepRowFor) — so
	// a vote step nobody has opened a ballot on still reports every declared
	// seat as missing, rather than an empty roster that reads as no vote at all.
	cast := map[string]*model.Vote{}
	if view.Row.Proposal == "" {
		result.Outcome = GateOutcomeOpen
	} else {
		result.Proposal = view.Row.Proposal

		proposalID, err := model.ParseProposalID(view.Row.Proposal)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing proposal %q on %s: %w", view.Row.Proposal, model.FormatStepID(stepID), err)
		}
		proposal, err := db.GetProposal(conn, proposalID)
		if err != nil {
			return nil, fmt.Errorf("reading proposal %s: %w", view.Row.Proposal, err)
		}
		result.Outcome = voteGateOutcome(proposal.Status)
		result.Tally = &GateTally{WeightedScore: proposal.WeightedScore, Threshold: proposal.Threshold}

		votes, err := db.GetProposalVotes(conn, proposalID)
		if err != nil {
			return nil, fmt.Errorf("reading votes on %s: %w", view.Row.Proposal, err)
		}
		for _, v := range votes {
			cast[v.VoterName] = v
		}
	}

	// §11.1: voter entries are OPAQUE — this counts and matches them by name
	// and never interprets one.
	result.Seats = make([]GateSeat, 0, len(view.Row.Voters))
	for _, voter := range view.Row.Voters {
		seat := GateSeat{Voter: voter}
		if v, ok := cast[voter]; ok {
			seat.Cast = true
			seat.Verdict = string(v.Verdict)
		} else {
			result.MissingSeats = append(result.MissingSeats, voter)
		}
		result.Seats = append(result.Seats, seat)
	}
	return result, nil
}

// voteGateOutcome maps a proposal's status onto the three-state outcome, in
// the SAME reading readVoteProposalOutcome already uses for `step resolve`'s
// pass/fail routing: approved or operator-committed reads as decided-yes,
// rejected as decided-no, anything else (open, or closed without a tally) as
// undecided.
func voteGateOutcome(status model.ProposalStatus) GateOutcome {
	switch status {
	case model.ProposalStatusApproved, model.ProposalStatusCommitted:
		return GateOutcomeApproved
	case model.ProposalStatusRejected:
		return GateOutcomeRejected
	default:
		return GateOutcomeOpen
	}
}

// humanGateOutcome mirrors GuardGate's own "passed" reading: `done` with a
// `pass` routing is approved, still short of `done` is open, and `done` by
// any other routing is a decision that was not a pass — rejected.
func humanGateOutcome(status, routing string) GateOutcome {
	if status != db.StepDone {
		return GateOutcomeOpen
	}
	if routingIs(routing, RoutingPass) {
		return GateOutcomeApproved
	}
	return GateOutcomeRejected
}
