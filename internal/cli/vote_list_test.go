package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// TestVoteListOwnsItsProject: vote list resolves its project through
// internal/verb, so the root hook must skip its eager resolution for it.
func TestVoteListOwnsItsProject(t *testing.T) {
	if !verbOwnsProject(voteListCmd) {
		t.Errorf("%s lacks the verbOwnsProject annotation", voteListCmd.CommandPath())
	}
}

// listedProposal opens a proposal in the default project with the given
// status and seat count, and returns its id.
func listedProposal(t *testing.T, conn *sql.DB, description string, status model.ProposalStatus, voters int) int {
	t.Helper()
	id, err := db.CreateProposal(conn, &model.Proposal{
		Description: description, Criticality: model.CriticalityLow,
		Status: status, RequiredVoters: voters, Threshold: 0.5,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	return id
}

// voteListJSON runs vote list under --json=v2 and returns the listed ids and
// the envelope's total.
func voteListJSON(t *testing.T, conn *sql.DB, flags ...string) ([]string, int) {
	t.Helper()
	for i := 0; i < len(flags); i += 2 {
		testsupport.Must(t, voteListCmd.Flags().Set(flags[i], flags[i+1]), "set --%s", flags[i])
	}
	out, err := runLeaseCommandOutput(t, conn, nil, voteListCmd)
	testsupport.Must(t, err, "vote list %v: %v", flags, err)

	var envelope struct {
		Data struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
			Total int `json:"total"`
		} `json:"data"`
	}
	testsupport.Must(t, json.Unmarshal([]byte(out), &envelope), "payload is not JSON: %q", out)
	ids := make([]string, 0, len(envelope.Data.Items))
	for _, item := range envelope.Data.Items {
		ids = append(ids, item.ID)
	}
	return ids, envelope.Data.Total
}

// TestVoteListCommandFiltersByStatus: open proposals only by default, every
// proposal under --all, and exactly the named status under --status.
func TestVoteListCommandFiltersByStatus(t *testing.T) {
	conn := newTestDB(t)
	open := model.FormatProposalID(listedProposal(t, conn, "open one", model.ProposalStatusOpen, 2))
	approved := model.FormatProposalID(listedProposal(t, conn, "approved one", model.ProposalStatusApproved, 2))

	cases := []struct {
		name  string
		flags []string
		want  []string
	}{
		{"default", nil, []string{open}},
		{"all", []string{"all", "true"}, []string{open, approved}},
		{"status", []string{"status", "approved"}, []string{approved}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids, total := voteListJSON(t, conn, tc.flags...)
			if total != len(tc.want) || len(ids) != len(tc.want) {
				t.Fatalf("vote list %v = %v (total %d), want %v", tc.flags, ids, total, tc.want)
			}
			for _, want := range tc.want {
				if !strings.Contains(strings.Join(ids, " "), want) {
					t.Errorf("vote list %v = %v, missing %s", tc.flags, ids, want)
				}
			}
		})
	}
}

func TestVoteListCommandRefusesAnUnknownFilter(t *testing.T) {
	conn := newTestDB(t)
	for _, flag := range []string{"status", "criticality"} {
		t.Run(flag, func(t *testing.T) {
			testsupport.Must(t, voteListCmd.Flags().Set(flag, "bogus"), "set --%s", flag)
			_, err := runLeaseCommandOutput(t, conn, nil, voteListCmd)
			if code := codeOf(t, err); code != output.ErrValidation {
				t.Errorf("vote list --%s bogus returned %s (%v), want %s", flag, code, err, output.ErrValidation)
			}
		})
	}
}

// TestVoteListCommandRendersVoteCounts: the human table shows each
// proposal's cast count against its seats.
func TestVoteListCommandRendersVoteCounts(t *testing.T) {
	conn := newTestDB(t)
	id := listedProposal(t, conn, "counted", model.ProposalStatusOpen, 3)
	_, err := db.CastVote(conn, &model.Vote{
		ProposalID: id, VoterName: "seat", Verdict: model.VerdictApprove,
		Confidence: 0.9, DomainRelevance: 0.8,
	})
	testsupport.Must(t, err, "CastVote: %v", err)

	voteListCmd.SetContext(context.WithValue(context.Background(), dbKey, conn))
	stdout := captureStdout(t)
	err = runVoteList(voteListCmd, nil, output.NewWithVersion(output.JSONNone, false))
	out := stdout()
	testsupport.Must(t, err, "vote list: %v", err)
	if !strings.Contains(out, model.FormatProposalID(id)) || !strings.Contains(out, "1/3") {
		t.Errorf("vote list table does not show %s with 1/3 votes:\n%s", model.FormatProposalID(id), out)
	}
}
