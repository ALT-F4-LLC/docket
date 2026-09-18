package engine

import (
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// DKT-733 — vote-seat usage under-reporting. RUN-51's report said "45 of 57
// seat(s) reported spend — 12 reported NOTHING" and nothing anywhere said
// WHICH twelve or via which seating path, so the backfill verb built to close
// exactly this gap (`vote backfill-usage`, DKT-115) could not be aimed and
// budget-cap enforcement read an understated ledger. These tests pin the
// per-seat enumeration and its path labels.

// TestSilentVoteSeatsNameTheirSeatingPath: a run with one silent vote-step
// seat and one silent conversational-gate seat enumerates BOTH, each labeled
// with the path that minted its proposal, and the seat that reported spend is
// not listed.
func TestSilentVoteSeatsNameTheirSeatingPath(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)
	e := testEngine()
	configureHoldTally(t, conn, "dkt733-panel", "seat-a,seat-b")
	driveToReconcile(t, conn, e, clusteredPayload)
	// The first invocation that observes the held vote step ready opens its
	// proposal (§8.1 phase 2).
	nextRun(t, conn, e)
	held := heldInstances(t, conn)
	if len(held) == 0 {
		t.Fatal("nothing held")
	}
	voteStepID := heldProposalID(t, conn, e, held[0])

	// A vote-step seat casts and reports nothing.
	_, err := db.CastVote(conn, &model.Vote{
		ProposalID: voteStepID, VoterName: "seat-a", VoterRole: "security",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
	})
	testsupport.Must(t, err, "CastVote (vote-step): %v", err)

	// A conversational gate naming the run: one silent seat, one loud one.
	convID, err := db.CreateProposal(conn, &model.Proposal{
		ProjectID:   1,
		Description: "activation panel for " + model.FormatRunID(run.ID),
		Rationale:   "conversational gate",
		Criticality: model.CriticalityMedium,
		Threshold:   0.5, RequiredVoters: 3,
		Status: model.ProposalStatusOpen, CreatedBy: "conductor",
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	_, err = db.CastVote(conn, &model.Vote{
		ProposalID: convID, VoterName: "gate-seat-quiet",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
	})
	testsupport.Must(t, err, "CastVote (conversational, silent): %v", err)
	_, err = db.CastVote(conn, &model.Vote{
		ProposalID: convID, VoterName: "gate-seat-loud",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		Usage: map[string]float64{"tokens": 7},
	})
	testsupport.Must(t, err, "CastVote (conversational, loud): %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if c := report.VoteUsageCoverage; c.Silent() != 2 {
		t.Fatalf("coverage = %+v, want 2 silent seats", c)
	}
	if len(report.SilentVoteSeats) != 2 {
		t.Fatalf("silent_vote_seats = %+v, want exactly the two quiet casts",
			report.SilentVoteSeats)
	}

	byVoter := make(map[string]SilentVoteSeat, len(report.SilentVoteSeats))
	for _, s := range report.SilentVoteSeats {
		byVoter[s.Voter] = s
	}
	if s, ok := byVoter["seat-a"]; !ok || s.Path != SeatPathVoteStep ||
		s.Proposal != model.FormatProposalID(voteStepID) || s.Role != "security" {
		t.Errorf("vote-step silent seat = %+v, want seat-a (security) on %s "+
			"via %q", byVoter["seat-a"],
			model.FormatProposalID(voteStepID), SeatPathVoteStep)
	}
	if s, ok := byVoter["gate-seat-quiet"]; !ok ||
		s.Path != SeatPathConversationalGate ||
		s.Proposal != model.FormatProposalID(convID) {
		t.Errorf("conversational silent seat = %+v, want gate-seat-quiet on "+
			"%s via %q", byVoter["gate-seat-quiet"],
			model.FormatProposalID(convID), SeatPathConversationalGate)
	}
	if _, ok := byVoter["gate-seat-loud"]; ok {
		t.Error("the seat that reported spend is listed as silent")
	}
}

// TestSilentReapAckSeatIsAConversationalGate: a reap-ack ballot — keyed to
// the run, but outside the vote-step family — labels its silent seats
// conversational-gate, matching where that panel is actually seated.
func TestSilentReapAckSeatIsAConversationalGate(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	id, err := db.CreateProposalIdempotent(conn, &model.Proposal{
		ProjectID: 1, Description: "accept the reap at seq 42?",
		Criticality: model.CriticalityMedium,
		Threshold:   0.5, RequiredVoters: 3,
		Status: model.ProposalStatusOpen, CreatedBy: "conductor",
	}, ReapAckProposalKey(run.ID, 42))
	testsupport.Must(t, err, "CreateProposalIdempotent: %v", err)
	_, err = db.CastVote(conn, &model.Vote{
		ProposalID: id, VoterName: "seat-a",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if len(report.SilentVoteSeats) != 1 {
		t.Fatalf("silent_vote_seats = %+v, want the one quiet reap-ack cast",
			report.SilentVoteSeats)
	}
	if s := report.SilentVoteSeats[0]; s.Path != SeatPathConversationalGate ||
		s.Proposal != model.FormatProposalID(id) || s.Voter != "seat-a" {
		t.Errorf("silent seat = %+v, want seat-a on %s via %q",
			s, model.FormatProposalID(id), SeatPathConversationalGate)
	}
}

// TestSilentVoteSeatsAbsentWhenEverySeatReports: a run whose every seat
// reported carries no list at all — the coverage line already says N/N, and
// an empty list would restate it (`omitempty`).
func TestSilentVoteSeatsAbsentWhenEverySeatReports(t *testing.T) {
	conn := mustDB(t)
	run, _ := activatedRun(t, conn)

	id, err := db.CreateProposal(conn, &model.Proposal{
		ProjectID:   1,
		Description: "activation panel for " + model.FormatRunID(run.ID),
		Criticality: model.CriticalityMedium,
		Threshold:   0.5, RequiredVoters: 3,
		Status: model.ProposalStatusOpen, CreatedBy: "conductor",
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	_, err = db.CastVote(conn, &model.Vote{
		ProposalID: id, VoterName: "seat-a",
		Verdict: model.VerdictApprove, Confidence: 0.9, DomainRelevance: 0.8,
		Usage: map[string]float64{"tokens": 42},
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	report, err := LoadRunReport(conn, run.ID, nowMS)
	testsupport.Must(t, err, "LoadRunReport: %v", err)

	if c := report.VoteUsageCoverage; c.Casts != 1 || c.Reported != 1 {
		t.Fatalf("coverage = %+v, want 1/1", c)
	}
	if report.SilentVoteSeats != nil {
		t.Errorf("silent_vote_seats = %+v on a fully-reported run, want none",
			report.SilentVoteSeats)
	}
}
