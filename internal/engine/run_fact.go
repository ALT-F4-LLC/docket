package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// `docket run fact add` (DKT-2759): the conductor's channel for coordination
// facts only it observes — a vote seat it re-spawned, a step it held back on
// budget or behind a chain. Each is recorded as an event so a retro reading
// the store can count it; before this the facts reached only the driving
// conversation. A hand-resolved cherry-pick has no kind here: its record is
// the `step-annotated` event `step annotate --integrated-sha` writes.
//
// A fact is an observation, not a ruling, so it takes no --authority: on a
// bound run the conductor capability is the whole authority, as for `run
// resume` and `step reap`.

// Run-fact kinds, as `--kind` spells them.
const (
	RunFactVoteReseated = EventVoteReseated
	RunFactStepDeferred = EventStepDeferred
)

// Deferral causes for RunFactStepDeferred.
const (
	DeferralBudget = "budget"
	DeferralChain  = "chain"
)

// RunFactOptions are `run fact add`'s inputs.
type RunFactOptions struct {
	RunID  int
	Kind   string
	Reason string
	// ProposalID and Voter name the re-seated seat (vote-reseated).
	ProposalID int
	Voter      string
	// StepID and Cause name the deferred step (step-deferred).
	StepID int
	Cause  string
	// Token is the run's conductor capability, from DOCKET_TOKEN or stdin.
	Token string
	By    Attribution
	NowMS int64
}

// RunFact is what `run fact add` recorded.
type RunFact struct {
	Run      string `json:"run"`
	Kind     string `json:"kind"`
	Seq      int64  `json:"seq"`
	Proposal string `json:"proposal,omitempty"`
	Voter    string `json:"voter,omitempty"`
	Step     string `json:"step,omitempty"`
	Cause    string `json:"cause,omitempty"`
	Reason   string `json:"reason"`
}

// RecordRunFact is RecordRunFactStore on store.StoreOn(conn).
func RecordRunFact(conn store.LegacyDB, opts RunFactOptions) (*RunFact, error) {
	return RecordRunFactStore(store.StoreOn(conn), opts)
}

// RecordRunFactStore validates and records one conductor-observed fact.
func RecordRunFactStore(storage store.Store, opts RunFactOptions) (*RunFact, error) {
	conn, err := store.SQLDB(storage)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(opts.Reason)
	if reason == "" {
		return nil, validationErr("--reason is required: a fact with no reason " +
			"tells a retro that something happened but not why")
	}
	st := store.StoreOn(conn)
	if _, err := st.GetRun(opts.RunID); err != nil {
		return nil, notFoundErr(err, "run %s not found", model.FormatRunID(opts.RunID))
	}

	fact := &RunFact{Run: model.FormatRunID(opts.RunID), Kind: opts.Kind, Reason: reason}
	record := eventRecord{Kind: opts.Kind, RunID: opts.RunID, AtMS: opts.NowMS}
	fields := map[string]any{"reason": reason}

	switch opts.Kind {
	case RunFactVoteReseated:
		if opts.ProposalID == 0 || strings.TrimSpace(opts.Voter) == "" {
			return nil, validationErr("--kind %s needs --proposal PROPOSAL-N and --voter NAME",
				opts.Kind)
		}
		if _, err := st.GetProposal(opts.ProposalID); err != nil {
			return nil, notFoundErr(err, "proposal %s not found",
				model.FormatProposalID(opts.ProposalID))
		}
		served, err := decidingVoteRuns(conn, opts.ProposalID)
		if err != nil {
			return nil, err
		}
		if !served[opts.RunID] {
			return nil, validationErr(
				"proposal %s is linked to no issue with a step on %s, so no seat of it "+
					"was re-seated for this run", model.FormatProposalID(opts.ProposalID), fact.Run)
		}
		fact.Proposal, fact.Voter = model.FormatProposalID(opts.ProposalID), opts.Voter
		fields["proposal"], fields["voter"] = fact.Proposal, fact.Voter
	case RunFactStepDeferred:
		if opts.StepID == 0 {
			return nil, validationErr("--kind %s needs --step STEP-N and --cause %s|%s",
				opts.Kind, DeferralBudget, DeferralChain)
		}
		if opts.Cause != DeferralBudget && opts.Cause != DeferralChain {
			return nil, validationErr("--cause must be %s or %s, got %q",
				DeferralBudget, DeferralChain, opts.Cause)
		}
		step, err := st.GetStep(opts.StepID)
		if errors.Is(err, store.ErrStepNotFound) {
			return nil, notFoundErr(err, "step %s not found", model.FormatStepID(opts.StepID))
		}
		if err != nil {
			return nil, err
		}
		if step.RunID != opts.RunID {
			return nil, validationErr("step %s belongs to %s, not %s",
				model.FormatStepID(opts.StepID), model.FormatRunID(step.RunID), fact.Run)
		}
		fact.Step, fact.Cause = model.FormatStepID(opts.StepID), opts.Cause
		fields["step"], fields["cause"] = fact.Step, fact.Cause
		record.Instance, record.IssueID = step.Instance, step.IssueID
	default:
		return nil, validationErr("unknown fact kind %q; valid kinds: %s, %s",
			opts.Kind, RunFactVoteReseated, RunFactStepDeferred)
	}

	if err := opts.By.require("run fact"); err != nil {
		return nil, err
	}
	data, err := rulingData(opts.By, fields)
	if err != nil {
		return nil, err
	}
	record.Data = data

	tx, err := conn.Begin()
	if err != nil {
		return nil, fmt.Errorf("recording the run fact: %w", err)
	}
	defer tx.Rollback()
	if err := authorizeConductorTx(tx, opts.RunID, opts.Token, "run fact add"); err != nil {
		return nil, err
	}
	if opts.Kind == RunFactStepDeferred && opts.Cause == DeferralChain {
		// The engine records a chain deferral when the dispatch closes, and the
		// conductor may already have filed one since; filing it again would
		// double-count it.
		if prior, ok, err := engineChainFact(tx, opts.RunID, opts.StepID); err != nil {
			return nil, err
		} else if ok {
			return prior, nil
		}
		if prior, ok, err := conductorChainFact(tx, opts.RunID, opts.StepID); err != nil {
			return nil, err
		} else if ok {
			return prior, nil
		}
	}
	if err := recordEvent(tx, record); err != nil {
		return nil, err
	}
	seq, found, err := store.TxOn(tx).MaxEventSeqTx()
	if err != nil {
		return nil, fmt.Errorf("reading the fact's seq: %w", err)
	}
	if !found {
		return nil, errors.New("reading the fact's seq: the event log is empty after recording the fact")
	}
	fact.Seq = seq
	if err := commitWrite(tx); err != nil {
		return nil, fmt.Errorf("recording the run fact: %w", err)
	}
	return fact, nil
}

// conductorChainFact returns the first conductor-filed chain deferral for the
// step recorded after the run's most recently closed dispatch closed, if any.
// The window opens at that close's `dispatch-closed` event seq, so a deferral
// filed for a later dispatch is a new fact; with no dispatch closed, any prior
// filing covers the step.
func conductorChainFact(tx store.LegacyReadTx, runID, stepID int) (*RunFact, bool, error) {
	seq, data, found, err := store.TxOn(tx).ConductorChainDeferralTx(store.ChainDeferralQuery{
		RunID: runID, StepID: stepID,
		DeferredKind: EventStepDeferred, Cause: DeferralChain,
		DispatchClosedKind: EventDispatchClosed,
	})
	if err != nil || !found {
		return nil, false, err
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(data), &body); err != nil {
		return nil, false, fmt.Errorf("reading the conductor's chain deferral: %w", err)
	}
	return &RunFact{
		Run: model.FormatRunID(runID), Kind: EventStepDeferred, Seq: seq,
		Step: model.FormatStepID(stepID), Cause: DeferralChain, Reason: body.Reason,
	}, true, nil
}
