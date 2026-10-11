package engine

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/db"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/testsupport"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// The cast path's authorization (vote_cast.go), driven against a PINNED
// definition: the fixture stores workflow.Canonical of a real definition in
// `workflows.parsed`, because that is what castPolicyOf reads. A fixture that
// stored `{}` (seedVoteStep's, which serves tests that never resolve the spec)
// would find no step and pass every enforcement case vacuously.

// pinnedVoteFixture is one run whose vote step `review` was opened for one
// issue, against the definition `src` declares.
type pinnedVoteFixture struct {
	proposalID int
	spec       *workflow.Step
}

// seedPinnedVoteStep registers `src` as a pinned workflow, expands its `review`
// step for a fresh issue in a fresh run, and opens the step's proposal through
// the engine's own phase 2 — so the idempotency key, the link and the pinned
// bytes are exactly what a live run leaves.
func seedPinnedVoteStep(t *testing.T, conn *sql.DB, src string) pinnedVoteFixture {
	t.Helper()
	registerVoteRule(t, conn, "majority", "0.6", "")

	def, err := workflow.Parse([]byte(src))
	testsupport.Must(t, err, "Parse: %v", err)
	testsupport.Must(t, workflow.Validate(def), "Validate: %v", err)
	parsed, err := workflow.Canonical(def)
	testsupport.Must(t, err, "Canonical: %v", err)

	res, err := conn.Exec(
		`INSERT INTO workflows (name, version, source_sha256, body, parsed, created_at_ms)
		 VALUES ('pinned-vote', 1, 'x', ?, ?, 1)`, src, string(parsed))
	testsupport.Must(t, err, "seeding a workflow: %v", err)
	wfID, _ := res.LastInsertId()
	res, err = conn.Exec(
		`INSERT INTO runs (request, status, created_at_ms, updated_at_ms)
		 VALUES ('', 'active', 1, 1)`)
	testsupport.Must(t, err, "seeding a run: %v", err)
	runID, _ := res.LastInsertId()

	step := seedVoteStepInRun(t, conn, runID, wfID, "review@0")
	spec := workflow.StepByName(def, "review")
	if spec == nil {
		t.Fatal("fixture declares no `review` step")
	}
	id, err := OpenVoteProposal(conn, step, spec, nowMS)
	testsupport.Must(t, err, "OpenVoteProposal: %v", err)
	return pinnedVoteFixture{proposalID: id, spec: spec}
}

// voteFixture renders a two-step workflow — a producer and the `review`
// panel over it — with `tail` appended to the panel's table.
func voteFixture(producer, tail string) string {
	return fmt.Sprintf(`
[pipeline]
name = "pinned-vote"
version = 1
[[step]]
name = "implement"
%s
emits = "k"
[[step]]
name = "review"
after = ["implement"]
type = "vote"
voters = ["alice", "bob", "carol"]
vote_rule = "majority"
on_fail = "skip"
%s
`, producer, tail)
}

// castAs is one cast through the engine's cast path, at a confidence and
// relevance that never reach quorum on a three-seat panel by themselves.
func castAs(conn *sql.DB, proposalID int, voter string) (*db.CastVoteResult, error) {
	return castWeighted(conn, proposalID, voter, model.VerdictApprove, 0.9, 0.8)
}

func castWeighted(
	conn *sql.DB, proposalID int, voter string, verdict model.Verdict,
	confidence, relevance float64,
) (*db.CastVoteResult, error) {
	return CastVote(conn, &model.Vote{
		ProposalID: proposalID, VoterName: voter, Verdict: verdict,
		Confidence: confidence, DomainRelevance: relevance,
		Summary: "the seat's rationale",
	})
}

// assertRefused checks a cast came back VALIDATION_ERROR naming each `wants`.
func assertRefused(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the cast was accepted, want a refusal")
	}
	if code, ok := CodeOf(err); !ok || code != CodeValidation {
		t.Fatalf("refusal is %v (code %q), want VALIDATION_ERROR", err, code)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

// voteCount is the votes table's row count for one proposal.
func voteCount(t *testing.T, conn *sql.DB, proposalID int) int {
	t.Helper()
	var n int
	err := conn.QueryRow(`SELECT COUNT(*) FROM votes WHERE proposal_id = ?`, proposalID).Scan(&n)
	testsupport.Must(t, err, "counting votes: %v", err)
	return n
}

// adHocProposal is a proposal no vote step opened — an operator's own ballot,
// or a reap acknowledgment's — which the cast path must leave exactly as it was.
func adHocProposal(t *testing.T, conn *sql.DB, required int) int {
	t.Helper()
	id, err := db.CreateProposal(conn, &model.Proposal{
		Description: "ad hoc", Criticality: model.CriticalityLow,
		Status: model.ProposalStatusOpen, RequiredVoters: required, Threshold: 0.6,
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	return id
}

// TestVoteCastStrictRosterRefusesOffRoster is DKT-2511: under `roster =
// "strict"` a cast whose voter is not one of the step's pinned Voters is
// refused with a VALIDATION_ERROR naming the voter and the roster, and writes
// no vote; on-roster casts land; under the default the same off-roster cast is
// accepted; and a proposal no vote step opened enforces no roster at all.
func TestVoteCastStrictRosterRefusesOffRoster(t *testing.T) {
	t.Run("off-roster voter is refused and the votes table is unchanged", func(t *testing.T) {
		conn := mustDB(t)
		fx := seedPinnedVoteStep(t, conn, voteFixture(`executor = "worker"`, `roster = "strict"`))

		_, err := castAs(conn, fx.proposalID, "mallory")
		assertRefused(t, err, `"mallory"`, `"strict"`, `"review"`, "alice, bob, carol")
		if n := voteCount(t, conn, fx.proposalID); n != 0 {
			t.Errorf("the refused cast wrote %d vote(s)", n)
		}
	})

	t.Run("on-roster voter is accepted", func(t *testing.T) {
		conn := mustDB(t)
		fx := seedPinnedVoteStep(t, conn, voteFixture(`executor = "worker"`, `roster = "strict"`))

		_, err := castAs(conn, fx.proposalID, "bob")
		testsupport.Must(t, err, "an on-roster cast was refused: %v", err)
		if n := voteCount(t, conn, fx.proposalID); n != 1 {
			t.Errorf("the accepted cast left %d vote(s), want 1", n)
		}
	})

	t.Run("off-roster voter is accepted under the default roster", func(t *testing.T) {
		conn := mustDB(t)
		fx := seedPinnedVoteStep(t, conn, voteFixture(`executor = "worker"`, ``))
		if fx.spec.EffectiveRoster() != workflow.RosterOpen {
			t.Fatalf("fixture roster = %q, want the open default", fx.spec.EffectiveRoster())
		}

		_, err := castAs(conn, fx.proposalID, "mallory")
		testsupport.Must(t, err, "an off-roster cast under the open default was refused: %v", err)
	})

	t.Run("off-roster cast on a non-vote-step proposal is recorded", func(t *testing.T) {
		conn := mustDB(t)
		id := adHocProposal(t, conn, 3)
		ok, err := IsVoteStepProposal(conn, id)
		testsupport.Must(t, err, "IsVoteStepProposal: %v", err)
		if ok {
			t.Fatal("fixture bug: an ad-hoc proposal reads as a vote step's")
		}

		_, err = castAs(conn, id, "mallory")
		testsupport.Must(t, err, "a cast on an ad-hoc proposal was refused: %v", err)
		if n := voteCount(t, conn, id); n != 1 {
			t.Errorf("the cast left %d vote(s), want 1", n)
		}
	})
}

// TestVoteEqualWeightingIgnoresDeclaredConfidence is DKT-2512 at the engine
// seam: under `weighting = "equal"` every cast weighs 1.0 in the tally while
// the vote rows keep the confidence each seat declared; under the default the
// declared values price the tally as before; and a proposal no vote step
// opened has no weighting to declare and tallies at declared weight.
func TestVoteEqualWeightingIgnoresDeclaredConfidence(t *testing.T) {
	// A two-seat panel so the second cast tallies. One approve at 0.9 and one
	// reject at 0.2, both at relevance 1.0: equal weighting scores 0.5, the
	// declared arithmetic 0.9 / 1.1.
	twoSeat := func(tail string) string {
		return strings.Replace(
			voteFixture(`executor = "worker"`, tail),
			`voters = ["alice", "bob", "carol"]`, `voters = ["alice", "bob"]`, 1)
	}
	tally := func(t *testing.T, conn *sql.DB, proposalID int) float64 {
		t.Helper()
		_, err := castWeighted(conn, proposalID, "alice", model.VerdictApprove, 0.9, 1.0)
		testsupport.Must(t, err, "first cast: %v", err)
		res, err := castWeighted(conn, proposalID, "bob", model.VerdictReject, 0.2, 1.0)
		testsupport.Must(t, err, "second cast: %v", err)
		if !res.QuorumReached || res.WeightedScore == nil {
			t.Fatalf("the second cast did not tally: %+v", res)
		}
		return *res.WeightedScore
	}
	assertDeclaredKept := func(t *testing.T, conn *sql.DB, proposalID int) {
		t.Helper()
		votes, err := db.GetProposalVotes(conn, proposalID)
		testsupport.Must(t, err, "GetProposalVotes: %v", err)
		want := map[string]float64{"alice": 0.9, "bob": 0.2}
		for _, v := range votes {
			if v.Confidence != want[v.VoterName] {
				t.Errorf("%s's row stores confidence %v, want the declared %v",
					v.VoterName, v.Confidence, want[v.VoterName])
			}
		}
	}

	t.Run("equal weighting scores every cast at 1.0", func(t *testing.T) {
		conn := mustDB(t)
		fx := seedPinnedVoteStep(t, conn, twoSeat(`weighting = "equal"`))
		if got := tally(t, conn, fx.proposalID); math.Abs(got-0.5) > 1e-9 {
			t.Errorf("equal-weighted score = %v, want 0.5", got)
		}
		assertDeclaredKept(t, conn, fx.proposalID)
	})

	t.Run("declared weighting prices the tally as before", func(t *testing.T) {
		conn := mustDB(t)
		fx := seedPinnedVoteStep(t, conn, twoSeat(``))
		if got := tally(t, conn, fx.proposalID); math.Abs(got-0.9/1.1) > 1e-9 {
			t.Errorf("declared score = %v, want %v", got, 0.9/1.1)
		}
		assertDeclaredKept(t, conn, fx.proposalID)
	})

	t.Run("a non-vote-step proposal tallies at declared weight", func(t *testing.T) {
		conn := mustDB(t)
		id := adHocProposal(t, conn, 2)
		if got := tally(t, conn, id); math.Abs(got-0.9/1.1) > 1e-9 {
			t.Errorf("ad-hoc score = %v, want the declared %v", got, 0.9/1.1)
		}
	})
}

// TestVoteCastRecusesDeclaredReviewedStep is DKT-2525: under `recuse =
// "executor"` a cast from the executor hint of the step the vote step
// `reviews` is refused with a VALIDATION_ERROR naming both steps; a sibling
// seat is accepted; the same cast lands when the step declares no recuse;
// with `reviews` unset the switch is a no-op; and a fanout producer's every
// sibling name is recused.
func TestVoteCastRecusesDeclaredReviewedStep(t *testing.T) {
	// The producer's executor hint is also a declared seat, which is the shape
	// recusal exists for: a name on the roster whose own work is on the ballot.
	withWorkerSeat := func(producer, tail string) string {
		return strings.Replace(
			voteFixture(producer, tail),
			`voters = ["alice", "bob", "carol"]`, `voters = ["alice", "bob", "worker"]`, 1)
	}

	t.Run("the reviews-named step's executor is refused", func(t *testing.T) {
		conn := mustDB(t)
		fx := seedPinnedVoteStep(t, conn,
			withWorkerSeat(`executor = "worker"`, "reviews = \"implement\"\nrecuse = \"executor\""))

		_, err := castAs(conn, fx.proposalID, "worker")
		assertRefused(t, err, `"review"`, `"implement"`, `"worker"`, `"executor"`)
		if n := voteCount(t, conn, fx.proposalID); n != 0 {
			t.Errorf("the refused cast wrote %d vote(s)", n)
		}
	})

	t.Run("a sibling seat is accepted", func(t *testing.T) {
		conn := mustDB(t)
		fx := seedPinnedVoteStep(t, conn,
			withWorkerSeat(`executor = "worker"`, "reviews = \"implement\"\nrecuse = \"executor\""))

		_, err := castAs(conn, fx.proposalID, "alice")
		testsupport.Must(t, err, "a sibling seat's cast was refused: %v", err)
	})

	t.Run("the same cast is accepted when the step declares no recuse", func(t *testing.T) {
		conn := mustDB(t)
		fx := seedPinnedVoteStep(t, conn,
			withWorkerSeat(`executor = "worker"`, `reviews = "implement"`))
		if fx.spec.EffectiveRecuse() != workflow.RecuseNone {
			t.Fatalf("fixture recuse = %q, want the none default", fx.spec.EffectiveRecuse())
		}

		_, err := castAs(conn, fx.proposalID, "worker")
		testsupport.Must(t, err, "the executor's cast was refused with no recuse declared: %v", err)
	})

	t.Run("reviews unset makes recuse a no-op", func(t *testing.T) {
		conn := mustDB(t)
		fx := seedPinnedVoteStep(t, conn,
			withWorkerSeat(`executor = "worker"`, `recuse = "executor"`))

		// Every declared voter lands, including the one whose name equals the
		// producing step's executor hint: with no `reviews` there is no
		// declared producer to compare against, and nothing falls back to the
		// vote step's own hint (it has none).
		for _, voter := range fx.spec.Voters {
			if _, err := castAs(conn, fx.proposalID, voter); err != nil {
				t.Errorf("%s's cast was refused with reviews unset: %v", voter, err)
			}
		}
	})

	t.Run("a fanout producer's every sibling name is refused", func(t *testing.T) {
		conn := mustDB(t)
		src := strings.Replace(
			voteFixture(`fanout = ["worker-a", "worker-b"]`,
				"reviews = \"implement\"\nrecuse = \"executor\""),
			`voters = ["alice", "bob", "carol"]`,
			`voters = ["alice", "worker-a", "worker-b"]`, 1)
		fx := seedPinnedVoteStep(t, conn, src)

		for _, sibling := range []string{"worker-a", "worker-b"} {
			_, err := castAs(conn, fx.proposalID, sibling)
			assertRefused(t, err, `"review"`, `"implement"`, sibling)
		}
		_, err := castAs(conn, fx.proposalID, "alice")
		testsupport.Must(t, err, "a name in neither sibling's hint was refused: %v", err)
	})
}

// TestVoteCastNeedsARationaleWithConcerns is DKT-3290: an approve-with-concerns
// or reject cast with an empty or whitespace-only summary is refused naming
// --summary and records nothing; a non-empty summary records verbatim; an
// approve still records with none.
func TestVoteCastNeedsARationaleWithConcerns(t *testing.T) {
	cases := []struct {
		name    string
		verdict model.Verdict
		summary string
		refused bool
	}{
		{"empty approve-with-concerns", model.VerdictApproveWithConcerns, "", true},
		{"approve-with-concerns with a rationale", model.VerdictApproveWithConcerns, "concern: X", false},
		{"empty reject", model.VerdictReject, "", true},
		{"whitespace approve-with-concerns", model.VerdictApproveWithConcerns, "  \n\t", true},
		{"empty approve", model.VerdictApprove, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn := mustDB(t)
			id := openProposal(t, conn, "needs a rationale")
			_, err := CastVote(conn, &model.Vote{
				ProposalID: id, VoterName: "seat-a", VoterRole: "reviewer",
				Verdict: c.verdict, Confidence: 0.9, DomainRelevance: 0.8, Summary: c.summary,
			})
			votes, readErr := db.GetProposalVotes(conn, id)
			testsupport.Must(t, readErr, "GetProposalVotes: %v", readErr)
			if c.refused {
				if !hasCode(err, CodeValidation) || !strings.Contains(err.Error(), "--summary") {
					t.Errorf("err = %v, want a validation error naming --summary", err)
				}
				if len(votes) != 0 {
					t.Errorf("%d votes recorded by a refused cast, want 0", len(votes))
				}
				return
			}
			testsupport.Must(t, err, "CastVote: %v", err)
			if len(votes) != 1 || votes[0].Summary != c.summary {
				t.Errorf("votes = %+v, want one with summary %q", votes, c.summary)
			}
		})
	}
}

// failRunEventInserts installs a trigger that aborts every step-less insert of
// the given event kinds: the writes a panel's run events make after the cast's
// own transaction has committed.
func failRunEventInserts(t *testing.T, conn *sql.DB, kinds ...string) {
	t.Helper()
	execSQL(t, conn, fmt.Sprintf(
		`CREATE TRIGGER fail_run_event_inserts BEFORE INSERT ON events
		 WHEN NEW.step_id IS NULL AND NEW.kind IN ('%s')
		 BEGIN SELECT RAISE(ABORT, 'injected run-event failure'); END`,
		strings.Join(kinds, "', '")))
}

// TestCastVoteReturnsCommittedResultWhenPanelEventsFail: when an attributed
// panel's run events cannot be written after the vote has committed, CastVote
// still hands back the committed cast's result beside an error naming the
// proposal; a cast refused before the commit still returns no result.
func TestCastVoteReturnsCommittedResultWhenPanelEventsFail(t *testing.T) {
	conn := mustDB(t)
	_, issue := activatedRun(t, conn)
	id, err := db.CreateProposal(conn, &model.Proposal{
		ProjectID: 1, Description: "should the loop extend?", Rationale: "conductor panel",
		Criticality: model.CriticalityMedium, Threshold: 0.5, RequiredVoters: 2,
		Status: model.ProposalStatusOpen, CreatedBy: "conductor",
	})
	testsupport.Must(t, err, "CreateProposal: %v", err)
	linkProposal(t, conn, id, issue)
	execSQL(t, conn, `UPDATE proposals SET created_at = ? WHERE id = ?`,
		time.UnixMilli(nowMS).Add(time.Minute).UTC().Format(time.RFC3339), id)
	failRunEventInserts(t, conn, EventVoteOpened)

	refused, err := CastVote(conn, &model.Vote{
		ProposalID: id, VoterName: "seat-0", Verdict: model.VerdictReject,
		Confidence: 0.9, DomainRelevance: 0.8,
	})
	if err == nil || refused != nil {
		t.Fatalf("refused cast = (%+v, %v), want a nil result and an error", refused, err)
	}
	if n := voteCount(t, conn, id); n != 0 {
		t.Fatalf("%d votes after a refused cast, want 0", n)
	}

	result, err := castAs(conn, id, "seat-1")
	if err == nil {
		t.Fatal("CastVote returned no error although the panel's run events failed")
	}
	if pid := model.FormatProposalID(id); !strings.Contains(err.Error(), pid) {
		t.Errorf("error %q does not name %s", err.Error(), pid)
	}
	if result == nil {
		t.Fatal("CastVote returned a nil result for a cast that committed")
	}
	if result.VotesCast != 1 || result.Vote == nil || result.Vote.VoterName != "seat-1" {
		t.Errorf("result = %+v, want seat-1's committed cast with 1 vote cast", result)
	}
	if n := voteCount(t, conn, id); n != 1 {
		t.Errorf("%d votes recorded, want the committed cast's 1", n)
	}
}

// TestCastPolicyOfStepBound pins stepBound on castPolicyOf's unenforced
// returns. A proposal created under the vote-step prefix is step-bound even
// when no pinned spec resolves for it, so CastVote skips the panel's run events
// for it; a proposal created under any other key is not step-bound.
func TestCastPolicyOfStepBound(t *testing.T) {
	conn := mustDB(t)
	fx := seedPinnedVoteStep(t, conn, voteFixture(`executor = "dev"`, ""))

	key, found, err := db.IdempotencyKeyOf(conn, db.ScopeVoteCreate, fx.proposalID)
	testsupport.Must(t, err, "IdempotencyKeyOf: %v", err)
	runID, ok := voteStepRunOf(key)
	if !found || !ok {
		t.Fatalf("fixture proposal key %q (found %v) names no run", key, found)
	}
	var wfID int64
	err = conn.QueryRow(`SELECT workflow_id FROM steps WHERE run_id = ?`, runID).Scan(&wfID)
	testsupport.Must(t, err, "reading the fixture's workflow: %v", err)

	// A materialized held step: a real step row in the run, under a minted
	// name the pinned definition never declares.
	held := seedVoteStepInRun(t, conn, int64(runID), wfID,
		workflow.HeldStepName("review")+"@0")

	keyed := func(t *testing.T, key string) int {
		t.Helper()
		id, err := db.CreateProposalIdempotent(conn, &model.Proposal{
			Description: "keyed", Criticality: model.CriticalityLow,
			Status: model.ProposalStatusOpen, RequiredVoters: 3, Threshold: 0.6,
		}, key)
		testsupport.Must(t, err, "CreateProposalIdempotent(%q): %v", key, err)
		return id
	}

	prefix := voteIdempotencyPrefix(runID)
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"run part does not parse", voteStepScopePrefix + "not-a-run:1:review@0"},
		{"issue part does not parse", prefix + "not-an-issue:review@0"},
		{"key names a step the run has no row for", prefix + "999999:review@0"},
		{"key names a materialized held step",
			voteIdempotencyKey(runID, held.IssueID, held.Instance)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := castPolicyOf(conn, keyed(t, tc.key))
			if err != nil {
				t.Fatalf("castPolicyOf(%q) error = %v, want nil", tc.key, err)
			}
			if !policy.stepBound {
				t.Errorf("castPolicyOf(%q).stepBound = false, want true", tc.key)
			}
			if policy.spec != nil {
				t.Errorf("castPolicyOf(%q).spec = %q, want nil", tc.key, policy.spec.Name)
			}
		})
	}

	t.Run("the declared vote step resolves its spec", func(t *testing.T) {
		policy, err := castPolicyOf(conn, fx.proposalID)
		testsupport.Must(t, err, "castPolicyOf: %v", err)
		if !policy.stepBound || policy.spec == nil || policy.spec.Name != "review" {
			t.Errorf("policy = %+v, want step-bound with the `review` spec", policy)
		}
	})

	t.Run("a proposal outside the vote-step prefix is not step-bound", func(t *testing.T) {
		for _, id := range []int{adHocProposal(t, conn, 3), keyed(t, "operator:ballot-1")} {
			policy, err := castPolicyOf(conn, id)
			testsupport.Must(t, err, "castPolicyOf: %v", err)
			if policy.stepBound || policy.spec != nil {
				t.Errorf("castPolicyOf(%s) = %+v, want the zero policy",
					model.FormatProposalID(id), policy)
			}
		}
	})
}
