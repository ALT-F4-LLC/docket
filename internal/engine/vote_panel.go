package engine

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
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
// The emission site is the cast path, because it is the first engine call a
// panel reaches after `vote link` has made it attributable: at `vote create`
// the link does not exist yet. So vote-opened is recorded by the first cast
// that observes the panel attributed, once per run, and vote-tallied by the
// cast that reaches quorum, with the score db.CastVote computed.

// panelRun is one run that attributes a conductor panel, with the run-bound
// issue the panel is linked through.
type panelRun struct {
	RunID   int
	IssueID int
}

// recordConductorPanelEvents writes a step-less panel's vote lifecycle events
// onto every run that attributes it. A vote step's own proposal is skipped:
// its step already announces it.
func recordConductorPanelEvents(conn *sql.DB, proposalID int, result *db.CastVoteResult) error {
	stepBound, err := IsVoteStepProposal(conn, proposalID)
	if err != nil || stepBound {
		return err
	}
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

// conductorPanelRuns lists the runs that attribute a panel: each run binding
// an issue the panel is linked to, whose activation-to-terminal window holds
// the panel's creation. It is the run report's attribution rule
// (conversationalRunProposalIDs), asked from the panel's side.
func conductorPanelRuns(conn *sql.DB, proposalID int) ([]panelRun, error) {
	rows, err := conn.Query(
		`SELECT ri.run_id, MIN(ri.issue_id), p.created_at
		   FROM proposals p
		   JOIN proposal_issues pi ON pi.proposal_id = p.id
		   JOIN run_issues ri ON ri.issue_id = pi.issue_id
		  WHERE p.id = ?
		  GROUP BY ri.run_id
		  ORDER BY ri.run_id`, proposalID)
	if err != nil {
		return nil, fmt.Errorf("finding the runs of %s: %w",
			model.FormatProposalID(proposalID), err)
	}
	var candidates []panelRun
	var created string
	for rows.Next() {
		var run panelRun
		if err := rows.Scan(&run.RunID, &run.IssueID, &created); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading a run of %s: %w",
				model.FormatProposalID(proposalID), err)
		}
		candidates = append(candidates, run)
	}
	// Closed before the window reads below: the pool holds one connection.
	iterErr := rows.Err()
	rows.Close()
	if iterErr != nil {
		return nil, fmt.Errorf("reading the runs of %s: %w",
			model.FormatProposalID(proposalID), iterErr)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	createdAt, err := time.Parse(time.RFC3339, created)
	if err != nil {
		// An unparseable creation time cannot be placed in a run's window, so
		// the panel is not attributed by inference — the report's rule too.
		return nil, nil
	}
	at := createdAt.UnixMilli()

	var out []panelRun
	for _, run := range candidates {
		from, to, ok, err := runActiveWindowMS(conn, run.RunID)
		if err != nil {
			return nil, err
		}
		// created_at has second precision, so the lower bound is the
		// activation's second.
		if ok && at >= from/1000*1000 && at <= to {
			out = append(out, run)
		}
	}
	return out, nil
}

// recordPanelRunEvents records, in one transaction on one run, the panel's
// vote-opened unless the run already has it, then its vote-tallied when tally
// is non-empty. The existence check shares the transaction with the write so
// that a later cast does not open the panel a second time.
func recordPanelRunEvents(conn *sql.DB, run panelRun, proposalID int, tally string) error {
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
	return tx.Commit()
}
