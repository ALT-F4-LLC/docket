package engine

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// linkedConductorPanel creates a conductor panel with three silent casts,
// linked to issueID, created at the given time, naming no run anywhere.
func linkedConductorPanel(t *testing.T, conn *sql.DB, issueID int, created time.Time) int {
	t.Helper()
	id, err := db.CreateProposal(conn, &model.Proposal{
		ProjectID: 1, Description: "should the loop extend?", Rationale: "conductor panel",
		Criticality: model.CriticalityMedium, Threshold: 0.5, RequiredVoters: 3,
		Status: model.ProposalStatusOpen, CreatedBy: "conductor",
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	linkProposal(t, conn, id, issueID)
	execSQL(t, conn, `UPDATE proposals SET created_at = ? WHERE id = ?`,
		created.UTC().Format(time.RFC3339), id)
	for i := 1; i <= 3; i++ {
		_, err := db.CastVote(conn, &model.Vote{
			ProposalID: id, VoterName: fmt.Sprintf("seat-%d", i),
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote: %v", err)
	}
	return id
}

// panelSeats counts the report's silent seats on one proposal.
func panelSeats(report *RunReport, proposalID int) int {
	n := 0
	for _, s := range report.SilentVoteSeats {
		if s.Proposal == model.FormatProposalID(proposalID) {
			n++
		}
	}
	return n
}

// TestRunReportAttributesIssueLinkedConductorPanel is DKT-3291: a conductor
// panel linked to a run-bound issue and created inside the run's
// activation-to-terminal window is the run's (criterion 1); one linked only to
// an issue outside the run (2), created after the run ended (3), or before it
// activated (4) is not.
func TestRunReportAttributesIssueLinkedConductorPanel(t *testing.T) {
	activatedAt := time.UnixMilli(nowMS)
	cases := []struct {
		name     string
		outside  bool
		created  time.Time
		end      bool
		attached bool
	}{
		{"linked inside the window", false, activatedAt.Add(time.Minute), false, true},
		{"linked to an issue outside the run", true, activatedAt.Add(time.Minute), false, false},
		{"created after the run ended", false, activatedAt.Add(2 * time.Hour), true, false},
		{"created before activation", false, activatedAt.Add(-time.Hour), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn := mustDB(t)
			run, issue := activatedRun(t, conn)
			target := issue
			if c.outside {
				target = createIssue(t, conn, "elsewhere", "a body", "task", nil)
			}
			if c.end {
				execSQL(t, conn, `INSERT INTO events (at_ms, kind, run_id, data) VALUES (?, ?, ?, '{}')`,
					activatedAt.Add(time.Hour).UnixMilli(), EventRunDone, run.ID)
			}
			id := linkedConductorPanel(t, conn, target, c.created)

			report, err := LoadRunReport(conn, run.ID, nowMS)
			testsupport.Must(t, err, "LoadRunReport: %v", err)
			cov := report.VoteUsageCoverage
			if c.attached {
				if cov.Casts != 3 || cov.Reported != 0 || panelSeats(report, id) != 3 {
					t.Errorf("coverage %+v, %d silent seats on the panel; want {3 0} and 3",
						cov, panelSeats(report, id))
				}
				return
			}
			if cov.Casts != 0 || panelSeats(report, id) != 0 {
				t.Errorf("coverage %+v, %d silent seats on the panel; want it excluded",
					cov, panelSeats(report, id))
			}
		})
	}
}
