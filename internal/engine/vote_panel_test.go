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
			id := conductorPanel(t, conn, c.created, target)

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

// conductorPanel creates a step-less proposal needing two ballots, stamps its
// creation time, and links it to issue: the state `vote create` plus
// `vote link --issue` leaves behind.
func conductorPanel(t *testing.T, conn *sql.DB, created time.Time, issue int) int {
	t.Helper()
	id, err := db.CreateProposal(conn, &model.Proposal{
		ProjectID: 1, Description: "should the loop extend?", Rationale: "conductor panel",
		Criticality: model.CriticalityMedium, Threshold: 0.5, RequiredVoters: 2,
		Status: model.ProposalStatusOpen, CreatedBy: "conductor",
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	execSQL(t, conn, `UPDATE proposals SET created_at = ? WHERE id = ?`,
		created.UTC().Format(time.RFC3339), id)
	linkProposal(t, conn, id, issue)
	return id
}

// TestLinkedConductorPanelOpensOnItsRunBeforeAnyCast: the link-time entry
// point records one step-less vote-opened carrying the proposal id on a run
// that attributes the panel, with no cast at all; on a panel no run
// attributes it succeeds and records nothing.
func TestLinkedConductorPanelOpensOnItsRunBeforeAnyCast(t *testing.T) {
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
			id := conductorPanel(t, conn, c.created, target)

			testsupport.Must(t, RecordConductorPanelOpened(conn, id),
				"RecordConductorPanelOpened(%s)", model.FormatProposalID(id))

			got := runVoteEventDetails(t, conn, run.ID)
			if !c.attached {
				if len(got) != 0 {
					t.Errorf("run events %v; want no vote events for an unattributed panel", got)
				}
				return
			}
			pid := model.FormatProposalID(id)
			if fmt.Sprint(got[EventVoteOpened]) != fmt.Sprint([]string{pid}) || len(got[EventVoteTallied]) != 0 {
				t.Errorf("run vote events %v; want only vote-opened [%s]", got, pid)
			}
			var stepless int
			testsupport.Must(t, conn.QueryRow(
				`SELECT COUNT(*) FROM events WHERE run_id = ? AND kind = ? AND step_id IS NULL`,
				run.ID, EventVoteOpened).Scan(&stepless), "counting step-less vote-opened")
			if stepless != 1 {
				t.Errorf("%d step-less vote-opened events on the run; want 1", stepless)
			}
		})
	}
}

// TestLinkTimeOpenIsNotRepeatedByTheQuorumCast: once the link opened the
// panel on its run, the casts that reach quorum add the vote-tallied and no
// second vote-opened.
func TestLinkTimeOpenIsNotRepeatedByTheQuorumCast(t *testing.T) {
	conn := mustDB(t)
	run, issue := activatedRun(t, conn)
	id := conductorPanel(t, conn, time.UnixMilli(nowMS).Add(time.Minute), issue)
	testsupport.Must(t, RecordConductorPanelOpened(conn, id), "RecordConductorPanelOpened")

	for i := 1; i <= 2; i++ {
		_, err := CastVote(conn, &model.Vote{
			ProposalID: id, VoterName: fmt.Sprintf("seat-%d", i),
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote: %v", err)
	}

	got := runVoteEventDetails(t, conn, run.ID)
	if len(got[EventVoteOpened]) != 1 || len(got[EventVoteTallied]) != 1 {
		t.Errorf("run vote events %v; want exactly one opened and one tallied", got)
	}
}

// TestStepBoundVoteRecordsOneEventOfEachKind guards the panel path against a
// vote step's own proposal, which is linked to a run-bound issue inside the
// window too: the step path already announces it, so neither the link-time
// entry point nor the cast path may announce it a second time.
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
	testsupport.Must(t, RecordConductorPanelOpened(conn, proposalID),
		"RecordConductorPanelOpened on a vote step's proposal")
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

// gatedShipSrc is a vote step with a step after it, so routing the vote is
// observable as the downstream step becoming claimable.
const gatedShipSrc = `
[pipeline]
name = "gated-ship"
version = 1

[match]
kind = ["task"]

[[step]]
name = "seed"
after = []
executor = "x"
emits = "findings"

[[step]]
name = "gate"
after = ["seed"]
type = "vote"
voters = ["seat-a", "seat-b", "seat-c"]
vote_rule = "majority"
on_fail = "skip"

[[step]]
name = "ship"
after = ["gate"]
executor = "x"
emits = "findings"
`

// TestStepBoundDecidingCastRoutesWhenPanelEventsWouldFail: a vote step's
// proposal is linked to a run-bound issue inside the run's window, so it looks
// attributed to the panel path. With every step-less vote event write forced to
// fail, the deciding cast still returns its committed result and no error, and
// the vote step routes so the step after it can be claimed. Step-boundness is
// settled before the commit, so no post-commit read or write of the panel path
// stands between the cast and the routing.
func TestStepBoundDecidingCastRoutesWhenPanelEventsWouldFail(t *testing.T) {
	conn := mustDB(t)
	registerVoteRule(t, conn, "majority", "0.67", "")
	registerSource(t, conn, []byte(gatedShipSrc), "gated-ship.toml")
	issue := createIssue(t, conn, "gated", "body", "task", nil)
	run := startRun(t, conn, issue)
	_, err := activate(conn, run.ID)
	testsupport.Must(t, err, "activate: %v", err)
	e := testEngine()

	proposalID := openGateProposal(t, conn, e, run.ID)
	// The fixture clock is not the wall clock: place the proposal inside the
	// run's window, where the panel attribution rule would claim it.
	execSQL(t, conn, `UPDATE proposals SET created_at = ? WHERE id = ?`,
		time.UnixMilli(nowMS).Add(time.Minute).UTC().Format(time.RFC3339), proposalID)
	failRunEventInserts(t, conn, EventVoteOpened, EventVoteTallied)

	var result *db.CastVoteResult
	for _, seat := range []string{"seat-a", "seat-b", "seat-c"} {
		result, err = CastVote(conn, &model.Vote{
			ProposalID: proposalID, VoterName: seat,
			Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		})
		testsupport.Must(t, err, "CastVote(%s): %v", seat, err)
	}
	if result == nil || !result.QuorumReached {
		t.Fatalf("deciding cast result = %+v, want the committed cast that reached quorum", result)
	}

	testsupport.Must(t, e.DriveVoteProposal(conn, proposalID, nowMS), "driving the tally")
	if got := stepStatus(t, conn, "gate@0"); got != db.StepDone {
		t.Errorf("gate@0 = %q after the deciding cast, want %q", got, db.StepDone)
	}
	_, err = ClaimStep(conn, stepIDByInstance(t, conn, "ship@0"),
		ClaimOptions{Owner: "worker", NowMS: nowMS})
	testsupport.Must(t, err, "claiming ship@0 after the vote routed: %v", err)
}
