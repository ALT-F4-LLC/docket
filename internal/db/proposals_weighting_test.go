package db

import (
	"math"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
)

// TestVoteEqualWeightingIgnoresDeclaredConfidence is DKT-2512 at the tally
// itself: CastVoteWeighted with `equal` scores every cast at 1.0 × 1.0 while
// each vote row keeps the confidence and domain relevance the seat declared;
// CastVote — the unchanged entry point — still prices the tally by them.
//
// One approve at confidence 0.9 and one reject at 0.2, both at relevance 1.0:
// 0.5 under equal, 0.9 / 1.1 under declared.
func TestVoteEqualWeightingIgnoresDeclaredConfidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		equal bool
		want  float64
	}{
		{"equal", true, 0.5},
		{"declared", false, 0.9 / 1.1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := mustOpen(t)
			if err := Initialize(conn); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if err := Migrate(conn); err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			id, err := CreateProposal(conn, &model.Proposal{
				Description: "weighting", Criticality: model.CriticalityLow,
				Status: model.ProposalStatusOpen, RequiredVoters: 2, Threshold: 0.6,
			})
			if err != nil {
				t.Fatalf("CreateProposal: %v", err)
			}

			cast := func(voter string, verdict model.Verdict, confidence float64) *CastVoteResult {
				t.Helper()
				v := &model.Vote{
					ProposalID: id, VoterName: voter, Verdict: verdict,
					Confidence: confidence, DomainRelevance: 1.0,
				}
				var res *CastVoteResult
				var err error
				if tc.equal {
					res, err = CastVoteWeighted(conn, v, true)
				} else {
					res, err = CastVote(conn, v)
				}
				if err != nil {
					t.Fatalf("cast by %s: %v", voter, err)
				}
				return res
			}
			cast("alice", model.VerdictApprove, 0.9)
			res := cast("bob", model.VerdictReject, 0.2)
			if !res.QuorumReached || res.WeightedScore == nil {
				t.Fatalf("the second cast did not tally: %+v", res)
			}
			if math.Abs(*res.WeightedScore-tc.want) > 1e-9 {
				t.Errorf("score = %v, want %v", *res.WeightedScore, tc.want)
			}

			// The row keeps what the seat declared, whichever way it was priced.
			votes, err := GetProposalVotes(conn, id)
			if err != nil {
				t.Fatalf("GetProposalVotes: %v", err)
			}
			want := map[string]float64{"alice": 0.9, "bob": 0.2}
			for _, v := range votes {
				if v.Confidence != want[v.VoterName] || v.DomainRelevance != 1.0 {
					t.Errorf("%s's row = confidence %v relevance %v, want %v / 1.0",
						v.VoterName, v.Confidence, v.DomainRelevance, want[v.VoterName])
				}
			}
		})
	}
}
