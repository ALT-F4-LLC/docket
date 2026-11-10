package db

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

func TestDispatchConditionQueries(t *testing.T) {
	db := mustOpen(t)
	testsupport.Must(t, Initialize(db), "Initialize")
	testsupport.Must(t, Migrate(db), "Migrate")

	newIssue := func(labels ...string) int {
		t.Helper()
		id, err := CreateIssue(db, &model.Issue{
			Title: "condition subject", Status: model.StatusTodo,
			Priority: model.PriorityNone, Kind: model.IssueKindTask,
		}, labels, nil)
		testsupport.Must(t, err, "CreateIssue: %v", err)
		return id
	}
	newRun := func() int {
		t.Helper()
		run, err := InsertRun(db, 1, "req", 0, 1000)
		testsupport.Must(t, err, "InsertRun: %v", err)
		return run.ID
	}
	run, otherRun := newRun(), newRun()
	work, tribunal, elsewhere := newIssue(), newIssue("tribunal"), newIssue()
	testsupport.Must(t, AddRunIssue(db, run, work), "AddRunIssue")
	testsupport.Must(t, AddRunIssue(db, run, tribunal), "AddRunIssue")
	testsupport.Must(t, AddRunIssue(db, otherRun, elsewhere), "AddRunIssue")

	since := time.Unix(5000, 0)
	tx, err := db.Begin()
	testsupport.Must(t, err, "Begin: %v", err)
	t.Cleanup(func() { _ = tx.Rollback() })

	testsupport.Must(t, InsertEventTx(tx, NewEvent{AtMS: since.UnixMilli(), Kind: "dispatch-opened", RunID: run, Data: `{}`}),
		"InsertEventTx")
	var openedSeq int64
	testsupport.Must(t, tx.QueryRow(`SELECT MAX(seq) FROM events`).Scan(&openedSeq), "reading the seq")

	voteID := 0
	propose := func(id int, status model.ProposalStatus, updated time.Time, issues []int, casts map[string]model.Verdict) {
		t.Helper()
		_, err := InsertProposalWithID(tx, &model.Proposal{
			ID: id, Description: "vote", Criticality: model.CriticalityLow,
			Status: status, RequiredVoters: 1, Threshold: 0.5,
			CreatedAt: since.Add(-time.Hour), UpdatedAt: updated,
		})
		testsupport.Must(t, err, "InsertProposalWithID %d: %v", id, err)
		for _, issue := range issues {
			_, err := InsertProposalIssueLink(tx, id, issue)
			testsupport.Must(t, err, "InsertProposalIssueLink %d: %v", id, err)
		}
		for voter, verdict := range casts {
			voteID++
			_, err := InsertVoteWithID(tx, &model.Vote{
				ID: voteID, ProposalID: id, VoterName: voter, Verdict: verdict,
				Confidence: 0.9, DomainRelevance: 0.9, CreatedAt: since,
			})
			testsupport.Must(t, err, "InsertVoteWithID %d: %v", voteID, err)
		}
	}
	concerns := model.VerdictApproveWithConcerns
	sealed := model.ProposalStatusApproved
	propose(20, sealed, since.Add(time.Minute), []int{work}, map[string]model.Verdict{"seat-c": concerns})
	propose(10, sealed, since, []int{work}, map[string]model.Verdict{
		"seat-z": concerns, "seat-a": concerns, "seat-m": model.VerdictApprove,
	})
	propose(30, model.ProposalStatusOpen, since.Add(time.Minute), []int{work},
		map[string]model.Verdict{"seat-open": concerns})
	propose(40, sealed, since.Add(-time.Second), []int{work},
		map[string]model.Verdict{"seat-early": concerns})
	propose(50, sealed, since.Add(time.Minute), []int{work, tribunal},
		map[string]model.Verdict{"seat-filed": concerns})
	propose(60, sealed, since.Add(time.Minute), []int{elsewhere},
		map[string]model.Verdict{"seat-elsewhere": concerns})
	testsupport.Must(t, tx.Commit(), "Commit")

	t.Run("the at_ms of an event by seq", func(t *testing.T) {
		got, err := EventAtMS(db, openedSeq)
		if err != nil || got != since.UnixMilli() {
			t.Errorf("the opened event's at_ms = (%d, %v), want (%d, nil)", got, err, since.UnixMilli())
		}
		got, err = EventAtMS(db, openedSeq+1)
		if !errors.Is(err, ErrNotFound) || got != 0 {
			t.Errorf("a seq the log never reached = (%d, %v), want (0, ErrNotFound)", got, err)
		}
	})

	t.Run("unfiled seat conditions of the run since the open", func(t *testing.T) {
		got, err := UnfiledSeatConditions(db, run, since.UnixMilli())
		want := []UnfiledSeatCondition{
			{ProposalID: 10, Voter: "seat-a"},
			{ProposalID: 10, Voter: "seat-z"},
			{ProposalID: 20, Voter: "seat-c"},
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("unfiled conditions = (%+v, %v), want (%+v, nil): open, pre-since, "+
				"tribunal-filed, off-run proposals and plain approvals excluded", got, err, want)
		}
	})

	t.Run("a run with no sealed votes", func(t *testing.T) {
		got, err := UnfiledSeatConditions(db, 999, since.UnixMilli())
		if err != nil || len(got) != 0 {
			t.Errorf("a run with no votes = (%+v, %v), want (none, nil)", got, err)
		}
	})
}
