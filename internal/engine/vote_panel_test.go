package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// runVoteEventDetails lists the details of a run's vote-opened and
// vote-tallied events through the reader `docket events list --run` uses.
func runVoteEventDetails(t *testing.T, conn *sql.DB, runID int) map[string][]string {
	t.Helper()
	page, err := ListEvents(conn, EventQuery{RunID: runID})
	testsupport.Must(t, err, "ListEvents: %v", err)
	out := map[string][]string{}
	for _, ev := range page.Events {
		if ev.Kind != EventVoteOpened && ev.Kind != EventVoteTallied {
			continue
		}
		var data struct {
			Detail string `json:"detail"`
		}
		testsupport.Must(t, json.Unmarshal(ev.Data, &data), "decoding %s data %s", ev.Kind, ev.Data)
		out[ev.Kind] = append(out[ev.Kind], data.Detail)
	}
	return out
}

// TestConductorPanelAttributedToRunRecordsVoteEvents: a step-less conductor
// panel attributed to a run (linked to a run-bound issue, created inside the
// run's activation-to-terminal window) records one vote-opened and one
// vote-tallied carrying the score on that run, cast through the engine's cast
// path; a panel the run does not attribute records neither there.
func TestConductorPanelAttributedToRunRecordsVoteEvents(t *testing.T) {
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

			id, err := db.CreateProposal(conn, &model.Proposal{
				ProjectID: 1, Description: "should the loop extend?", Rationale: "conductor panel",
				Criticality: model.CriticalityMedium, Threshold: 0.5, RequiredVoters: 2,
				Status: model.ProposalStatusOpen, CreatedBy: "conductor",
			})
			testsupport.Must(t, err, "CreateProposal: %v", err)
			linkProposal(t, conn, id, target)
			execSQL(t, conn, `UPDATE proposals SET created_at = ? WHERE id = ?`,
				c.created.UTC().Format(time.RFC3339), id)

			for i := 1; i <= 2; i++ {
				_, err := CastVote(conn, &model.Vote{
					ProposalID: id, VoterName: fmt.Sprintf("seat-%d", i),
					Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
				})
				testsupport.Must(t, err, "CastVote: %v", err)
			}

			got := runVoteEventDetails(t, conn, run.ID)
			if !c.attached {
				if len(got) != 0 {
					t.Errorf("run events %v; want no vote events for an unattributed panel", got)
				}
				return
			}
			pid := model.FormatProposalID(id)
			wantOpened := []string{pid}
			wantTallied := []string{pid + " approved score=1.00 ballots=2/2"}
			if fmt.Sprint(got[EventVoteOpened]) != fmt.Sprint(wantOpened) ||
				fmt.Sprint(got[EventVoteTallied]) != fmt.Sprint(wantTallied) {
				t.Errorf("run vote events %v; want opened %v and tallied %v",
					got, wantOpened, wantTallied)
			}
		})
	}
}

// TestStepBoundVoteRecordsOneEventOfEachKind guards the panel path against a
// vote step's own proposal, which is linked to a run-bound issue inside the
// window too: the step path already announces it, so the cast path must not
// announce it a second time.
func TestStepBoundVoteRecordsOneEventOfEachKind(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.67", "")
	registerSource(t, conn, []byte(dkt895VoteSrc), "dkt895-vote.toml")
	issue := createIssue(t, conn, "voted", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	claimAndComplete(t, conn, e, "seed@0", "the findings", "")
	testsupport.Must(t, e.DriveRunLifecycles(conn, run.ID, nowMS), "driving after the record")

	gate, err := db.GetStep(conn, stepIDByInstance(t, conn, "gate@0"))
	testsupport.Must(t, err, "reading gate@0: %v", err)
	proposalID, err := findVoteProposal(conn, gate)
	testsupport.Must(t, err, "finding gate@0's proposal: %v", err)
	if proposalID == 0 {
		t.Fatal("no proposal opened for gate@0")
	}
	for _, seat := range []string{"seat-a", "seat-b", "seat-c"} {
		_, err := CastVote(conn, &model.Vote{
			ProposalID: proposalID, VoterName: seat,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}
	testsupport.Must(t, e.DriveVoteProposal(conn, proposalID, nowMS), "driving the tally")

	got := runVoteEventDetails(t, conn, run.ID)
	if len(got[EventVoteOpened]) != 1 || len(got[EventVoteTallied]) != 1 {
		t.Errorf("run vote events %v; want exactly one opened and one tallied", got)
	}
}
