package db

import (
	"reflect"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

type proposalRunPair struct {
	ProposalID, RunID, IssueID int
}

// TestProposalRunLinksFiltersByRunAndProposal seeds two proposals across two
// runs so that dropping either filter clause returns another proposal's or
// another run's pair:
//
//	P1 -> I1 (bound in R1), I2 (bound in R2)
//	P2 -> I3 (bound in R2), I4 (bound in R1)
func TestProposalRunLinksFiltersByRunAndProposal(t *testing.T) {
	conn := mustInitAndMigrate(t)

	newProposal := func(desc string) int {
		t.Helper()
		id, err := CreateProposal(conn, &model.Proposal{
			Description: desc, Criticality: model.CriticalityMedium,
			Status: model.ProposalStatusOpen, RequiredVoters: 1, Threshold: 0.67,
		})
		testsupport.Must(t, err, "CreateProposal(%q): %v", desc, err)
		return id
	}
	newRun := func(request string) int {
		t.Helper()
		run, err := InsertRun(conn, 1, request, 0, 1000)
		testsupport.Must(t, err, "InsertRun(%q): %v", request, err)
		return run.ID
	}
	link := func(proposalID, issueID int) {
		t.Helper()
		err := LinkProposalIssue(conn, proposalID, issueID)
		testsupport.Must(t, err, "LinkProposalIssue(%d, %d): %v", proposalID, issueID, err)
	}
	bind := func(runID, issueID int) {
		t.Helper()
		err := AddRunIssue(conn, runID, issueID)
		testsupport.Must(t, err, "AddRunIssue(%d, %d): %v", runID, issueID, err)
	}

	p1 := newProposal("P1")
	p2 := newProposal("P2")
	r1 := newRun("R1")
	r2 := newRun("R2")
	i1 := createTestIssueForProposal(t, conn, "I1")
	i2 := createTestIssueForProposal(t, conn, "I2")
	i3 := createTestIssueForProposal(t, conn, "I3")
	i4 := createTestIssueForProposal(t, conn, "I4")
	link(p1, i1)
	link(p1, i2)
	link(p2, i3)
	link(p2, i4)
	bind(r1, i1)
	bind(r2, i2)
	bind(r2, i3)
	bind(r1, i4)

	cases := []struct {
		name   string
		filter ProposalRunLinkFilter
		want   []proposalRunPair
	}{
		{"no filter", ProposalRunLinkFilter{}, []proposalRunPair{
			{p1, r1, i1}, {p1, r2, i2}, {p2, r1, i4}, {p2, r2, i3},
		}},
		{"run R1 only", ProposalRunLinkFilter{RunID: r1}, []proposalRunPair{
			{p1, r1, i1}, {p2, r1, i4},
		}},
		{"proposal P1 only", ProposalRunLinkFilter{ProposalID: p1}, []proposalRunPair{
			{p1, r1, i1}, {p1, r2, i2},
		}},
		{"run R1 and proposal P1", ProposalRunLinkFilter{RunID: r1, ProposalID: p1}, []proposalRunPair{
			{p1, r1, i1},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			links, err := ProposalRunLinks(conn, tc.filter)
			testsupport.Must(t, err, "ProposalRunLinks(%+v): %v", tc.filter, err)
			var got []proposalRunPair
			for _, l := range links {
				got = append(got, proposalRunPair{l.ProposalID, l.RunID, l.IssueID})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ProposalRunLinks(%+v) = %v, want %v", tc.filter, got, tc.want)
			}
		})
	}
}
