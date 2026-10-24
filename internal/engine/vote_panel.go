package engine

import (
	"database/sql"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// A CONDUCTOR PANEL'S DELIBERATION ON THE RUN'S EVENT TRAIL (DKT-3303).
//
// A vote step announces its proposal through routeVoteStep and
// OpenVoteProposal. A panel the conductor convenes with `vote create` and
// links with `vote link --issue` has no step behind it, so until now it
// reached the run's trail only as free text. The run report already
// attributes such a panel to a run (DKT-3291, conversationalRunProposalIDs):
// linked to an issue the run binds, created inside the run's
// activation-to-terminal window. The same rule decides which runs record the
// panel's vote-opened and vote-tallied events here.
//
// A panel becomes attributable when `vote link` writes its link: at `vote
// create` the link does not exist yet. So `vote link` records vote-opened on
// each attributing run (RecordConductorPanelOpened), and the cast path records
// it for a run that came to attribute the panel only later; either way once
// per run. The cast that reaches quorum records vote-tallied, with the score
// db.CastVote computed.

// RecordConductorPanelOpened records a step-less panel's vote-opened on every
// run that attributes it and has not recorded it yet. `vote link` calls it
// once the link exists; a panel no run attributes records nothing.
func RecordConductorPanelOpened(conn *sql.DB, proposalID int) error {
	runs, err := conductorPanelRuns(conn, proposalID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if err := recordPanelRunEvents(conn, run, proposalID, ""); err != nil {
			return err
		}
	}
	return nil
}

// recordConductorPanelEvents writes a step-less panel's vote lifecycle events
// onto every run that attributes it. The caller passes only a step-less
// panel: a vote step's own proposal is announced by its step, and CastVote
// settles step-boundness before the vote commits.
func recordConductorPanelEvents(conn *sql.DB, proposalID int, result *store.CastVoteResult) error {
	runs, err := conductorPanelRuns(conn, proposalID)
	if err != nil || len(runs) == 0 {
		return err
	}

	tally := ""
	if result.QuorumReached {
		tally, err = voteTallyDetail(conn, &VoteOutcome{
			ProposalID: proposalID,
			Status:     result.ProposalStatus,
			Score:      result.WeightedScore,
			Required:   result.VotesRequired,
		})
		if err != nil {
			return err
		}
	}

	for _, run := range runs {
		if err := recordPanelRunEvents(conn, run, proposalID, tally); err != nil {
			return err
		}
	}
	return nil
}

// conductorPanelRuns lists the runs that attribute a panel, in run order. It
// is the run report's attribution rule (conversationalRunProposalIDs), asked
// from the panel's side. A vote step's own proposal has none here: its step
// already announces it.
func conductorPanelRuns(conn *sql.DB, proposalID int) ([]store.ProposalRunLink, error) {
	stepBound, err := IsVoteStepProposal(conn, proposalID)
	if err != nil || stepBound {
		return nil, err
	}
	runs, err := linkedPanelAttributions(conn, store.ProposalRunLinkFilter{ProposalID: proposalID})
	if err != nil {
		return nil, fmt.Errorf("finding the runs of %s: %w",
			model.FormatProposalID(proposalID), err)
	}
	return runs, nil
}

// recordPanelRunEvents records, in one transaction on one run, the panel's
// vote-opened unless the run already has it, then its vote-tallied when tally
// is non-empty. The existence check shares the write-locked transaction
// (internal/db opens every Begin immediate) with the write, so neither a later
// cast nor a concurrent one opens the panel a second time.
func recordPanelRunEvents(conn *sql.DB, run store.ProposalRunLink, proposalID int, tally string) error {
	pid := model.FormatProposalID(proposalID)
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("recording %s's events on %s: %w", pid, model.FormatRunID(run.RunID), err)
	}
	defer tx.Rollback()

	var opened bool
	if err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM events
		  WHERE run_id = ? AND kind = ? AND step_id IS NULL
		    AND json_extract(data, '$.detail') = ?)`,
		run.RunID, EventVoteOpened, pid).Scan(&opened); err != nil {
		return fmt.Errorf("reading whether %s opened on %s: %w", pid, model.FormatRunID(run.RunID), err)
	}
	if !opened {
		if err := recordEvent(tx, eventRecord{
			Kind: EventVoteOpened, RunID: run.RunID, IssueID: run.IssueID, Data: pid,
		}); err != nil {
			return err
		}
	}
	if tally != "" {
		if err := recordEvent(tx, eventRecord{
			Kind: EventVoteTallied, RunID: run.RunID, IssueID: run.IssueID, Data: tally,
		}); err != nil {
			return err
		}
	}
	return commitWrite(tx)
}
