package engine

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-4263: dispatch close refuses while a vote sealed in the dispatch holds an
// approve-with-concerns cast and no linked tribunal issue.

// sealedPanel creates a one-seat panel on the run's first issue, casts verdict,
// and returns the proposal id. The cast reaches quorum, so the proposal seals.
func sealedPanel(t *testing.T, conn *sql.DB, runID int, verdict model.Verdict) int {
	t.Helper()
	var issue int
	testsupport.Must(t, conn.QueryRow(
		`SELECT issue_id FROM run_issues WHERE run_id = ? ORDER BY issue_id LIMIT 1`, runID).Scan(&issue),
		"reading the run's issue")
	id, err := db.CreateProposal(conn, &model.Proposal{
		ProjectID: 1, Description: "ship it?", Criticality: model.CriticalityMedium,
		Threshold: 0.5, RequiredVoters: 1, Status: model.ProposalStatusOpen, CreatedBy: "conductor",
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	linkProposal(t, conn, id, issue)
	_, err = db.CastVote(conn, &model.Vote{
		ProposalID: id, VoterName: "seat-a", Verdict: verdict,
		Confidence: 0.9, DomainRelevance: 0.8,
	})
	testsupport.Must(t, err, "CastVote: %v", err)
	// The suite's clock is a fixed instant after any real one, so the seal is
	// stamped inside the dispatch's window rather than at wall-clock time.
	execSQL(t, conn, `UPDATE proposals SET updated_at = ? WHERE id = ?`,
		time.UnixMilli(nowMS+1000).UTC().Format(time.RFC3339), id)
	return id
}

func closeWith(conn *sql.DB, runID int, skip IntegrationSkip) (*CloseOutcome, error) {
	return NewEngine().CloseDispatch(conn, runID, false, skip, nowMS)
}

func TestCloseRefusesAnUnfiledApproveWithConcerns(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	id := sealedPanel(t, conn, runID, model.VerdictApproveWithConcerns)

	_, err := closeWith(conn, runID, IntegrationSkip{})
	if code, ok := CodeOf(err); !ok || code != CodeConflict {
		t.Fatalf("close = %v; want a CONFLICT refusal", err)
	}
	for _, want := range []string{model.FormatProposalID(id), "seat-a", "tribunal", "--no-conditions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	if _, ok := openDispatchID(conn, runID); !ok {
		t.Error("the refused close closed the dispatch")
	}
}

func TestCloseSucceedsOnceAnIssueLabelledTribunalIsLinked(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	id := sealedPanel(t, conn, runID, model.VerdictApproveWithConcerns)

	filed := createIssue(t, conn, "condition", "run PG16 once", "task", nil)
	linkProposal(t, conn, id, filed)
	if _, err := closeWith(conn, runID, IntegrationSkip{}); err == nil {
		t.Fatal("a linked issue without the tribunal label satisfied the check")
	}

	testsupport.Must(t, db.AddLabelToIssue(conn, filed, "tribunal", "", "test"), "labelling the filed issue")
	if _, err := closeWith(conn, runID, IntegrationSkip{}); err != nil {
		t.Fatalf("close refused after the condition was filed and linked: %v", err)
	}
}

func TestCloseIgnoresVotesWithNoUnfiledCondition(t *testing.T) {
	cases := map[string]func(t *testing.T, conn *sql.DB, runID int){
		"approve": func(t *testing.T, conn *sql.DB, runID int) {
			sealedPanel(t, conn, runID, model.VerdictApprove)
		},
		"sealed before the dispatch opened": func(t *testing.T, conn *sql.DB, runID int) {
			id := sealedPanel(t, conn, runID, model.VerdictApproveWithConcerns)
			execSQL(t, conn, `UPDATE proposals SET updated_at = '2000-01-01T00:00:00Z' WHERE id = ?`, id)
		},
		"still open": func(t *testing.T, conn *sql.DB, runID int) {
			id := sealedPanel(t, conn, runID, model.VerdictApproveWithConcerns)
			execSQL(t, conn, `UPDATE proposals SET status = 'open' WHERE id = ?`, id)
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			conn := mustDB(t)
			runID := dispatchRun(t, conn)
			openDispatch(t, conn, runID, 0, nowMS)
			seed(t, conn, runID)
			if _, err := closeWith(conn, runID, IntegrationSkip{}); err != nil {
				t.Fatalf("close refused: %v", err)
			}
		})
	}
}

func TestNoConditionsWaivesAndRecordsTheReason(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	id := sealedPanel(t, conn, runID, model.VerdictApproveWithConcerns)

	waiver := ConditionsWaiver{Reason: "PG16 run tracked elsewhere", Token: testConductorToken, By: Attribution{Actor: "tester", Cwd: "/tmp"}}
	outcome, err := closeWith(conn, runID, IntegrationSkip{Conditions: waiver})
	testsupport.Must(t, err, "close with --no-conditions: %v", err)
	if len(outcome.ConditionsWaived) != 1 || outcome.ConditionsWaived[0].Proposal != model.FormatProposalID(id) {
		t.Errorf("conditions_waived = %+v; want %s", outcome.ConditionsWaived, model.FormatProposalID(id))
	}

	notes, err := db.ListRunNotes(conn, runID)
	testsupport.Must(t, err, "ListRunNotes: %v", err)
	if len(notes) != 1 || !strings.Contains(notes[0].Text, waiver.Reason) ||
		!strings.Contains(notes[0].Text, model.FormatProposalID(id)) {
		t.Errorf("run notes = %+v; want one carrying the reason and the vote", notes)
	}
	var data string
	testsupport.Must(t, conn.QueryRow(
		`SELECT data FROM events WHERE run_id = ? AND kind = ? ORDER BY seq DESC LIMIT 1`,
		runID, EventDispatchClosed).Scan(&data), "reading the close event")
	if !strings.Contains(data, "conditions_waived") || !strings.Contains(data, waiver.Reason) {
		t.Errorf("close event %s does not record the waiver", data)
	}
}

func TestNoConditionsNeedsTheConductorTokenOnABoundRun(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	sealedPanel(t, conn, runID, model.VerdictApproveWithConcerns)

	waiver := ConditionsWaiver{Reason: "r", Token: "wrong", By: Attribution{Actor: "tester", Cwd: "/tmp"}}
	if _, err := closeWith(conn, runID, IntegrationSkip{Conditions: waiver}); err == nil {
		t.Fatal("a wrong conductor token waived the check")
	}
	if _, ok := openDispatchID(conn, runID); !ok {
		t.Error("a refused waiver closed the dispatch")
	}
}

// The waiver's run note is written after the close commits, so a reason the
// note would refuse must be refused before the dispatch closes.
func TestAnOversizeWaiverReasonRefusesBeforeTheDispatchCloses(t *testing.T) {
	conn := mustDB(t)
	runID := dispatchRun(t, conn)
	openDispatch(t, conn, runID, 0, nowMS)
	sealedPanel(t, conn, runID, model.VerdictApproveWithConcerns)

	waiver := ConditionsWaiver{
		Reason: strings.Repeat("x", RunNoteMaxBytes), Token: testConductorToken,
		By: Attribution{Actor: "tester", Cwd: "/tmp"},
	}
	_, err := closeWith(conn, runID, IntegrationSkip{Conditions: waiver})
	if code, ok := CodeOf(err); !ok || code != CodeValidation {
		t.Fatalf("close = %v; want a VALIDATION refusal", err)
	}
	if _, ok := openDispatchID(conn, runID); !ok {
		t.Error("the refused close closed the dispatch")
	}
}
