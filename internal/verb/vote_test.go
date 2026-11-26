package verb_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
)

// openProposal opens a proposal in the default project that needs voters
// casts to decide, under key when key is not empty.
func openProposal(t *testing.T, st store.Store, voters int, key string) string {
	t.Helper()
	id, err := st.CreateProposalIdempotent(&model.Proposal{
		Description:    "a ballot",
		Criticality:    model.CriticalityMedium,
		Status:         model.ProposalStatusOpen,
		RequiredVoters: voters,
		Threshold:      0.5,
	}, key)
	if err != nil {
		t.Fatalf("CreateProposalIdempotent: %v", err)
	}
	return model.FormatProposalID(id)
}

// castInput is an approving ballot from voter on proposal.
func castInput(proposal, voter string) verb.VoteCastInput {
	return verb.VoteCastInput{
		Proposal: proposal, Voter: voter, Verdict: "approve",
		Confidence: 0.9, DomainRelevance: 0.8,
	}
}

func proposalVotes(t *testing.T, st store.Store, ref string) map[string]*model.Vote {
	t.Helper()
	id, err := model.ParseProposalID(ref)
	if err != nil {
		t.Fatalf("ParseProposalID: %v", err)
	}
	votes, err := st.GetProposalVotes(id)
	if err != nil {
		t.Fatalf("GetProposalVotes: %v", err)
	}
	byVoter := map[string]*model.Vote{}
	for _, v := range votes {
		byVoter[v.VoterName] = v
	}
	return byVoter
}

func TestVoteCreateOpensTheProposalInTheCallersProjectByEnvAuthor(t *testing.T) {
	st := openStore(t)
	// The first identity registered adopts the default project, so another
	// project goes first and the caller's gets a row of its own.
	first := ensureProject(t, st, "/repos/first", "first")
	env := projectEnv(st, "/repos/anchored", true)
	env.Author = "alice"

	res, err := verb.VoteCreate(context.Background(), env, verb.VoteCreateInput{
		Description: "ship it?", Criticality: "high", Voters: 3, Threshold: 0.67,
		DomainTags: []string{"cli"}, Sealed: true,
	})
	if err != nil {
		t.Fatalf("VoteCreate: %v", err)
	}
	if res.Registered == nil || res.Registered.Identity != "/repos/anchored" {
		t.Fatalf("Registered = %+v, want the anchored identity registered on first contact", res.Registered)
	}
	p := res.Proposal
	if p.CreatedBy != "alice" || p.Status != model.ProposalStatusOpen ||
		p.RequiredVoters != 3 || !p.Sealed || p.Criticality != model.CriticalityHigh {
		t.Errorf("created proposal = %+v, want an open sealed high proposal of 3 voters by alice", p)
	}
	for project, want := range map[int]int{res.Registered.ID: 1, first: 0} {
		if _, total, err := st.ListProposals(project, "", "", "", 10); err != nil || total != want {
			t.Errorf("project %d lists %d proposal(s) (err %v), want %d", project, total, err, want)
		}
	}
}

func TestVoteCreateRefusals(t *testing.T) {
	cases := []struct {
		name string
		in   verb.VoteCreateInput
		want string
	}{
		{"no voters", verb.VoteCreateInput{Description: "d", Criticality: "low", Threshold: 0.5}, "--voters"},
		{"threshold out of range", verb.VoteCreateInput{Description: "d", Criticality: "low", Voters: 1, Threshold: 1.5}, "--threshold"},
		{"unknown criticality", verb.VoteCreateInput{Description: "d", Criticality: "dire", Voters: 1, Threshold: 0.5}, "dire"},
		{"vote-step key", verb.VoteCreateInput{Description: "d", Criticality: "low", Voters: 1, Threshold: 0.5,
			IdempotencyKey: "vote-step:1:1:review@0"}, "vote-step:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			_, err := verb.VoteCreate(context.Background(), projectEnv(st, "", false), tc.in)
			requireVerbCode(t, err, output.ErrValidation, tc.want)
			if _, err := st.GetProposal(1); err == nil {
				t.Errorf("a refused create wrote a proposal")
			}
		})
	}
}

func TestVoteCreateUnanchoredCallerIsRefusedAndRegistersNothing(t *testing.T) {
	st := openStore(t)
	_, err := verb.VoteCreate(context.Background(), projectEnv(st, "/work/scratch", false), verb.VoteCreateInput{
		Description: "d", Criticality: "low", Voters: 1, Threshold: 0.5,
	})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
	requireUnregistered(t, st, "/work/scratch")
}

func TestVoteCastRecordsTheBallotByEnvAuthorByDefault(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 2, "")
	env := projectEnv(st, "", false)
	env.Author = "bob"

	in := castInput(ref, "")
	in.FindingsJSON = `{"concerns":["slow"]}`
	in.Usage = `{"tokens":1200}`
	res, err := verb.VoteCast(context.Background(), env, in)
	if err != nil {
		t.Fatalf("VoteCast: %v", err)
	}
	if res.Cast == nil || res.Cast.VotesCast != 1 || res.Cast.VotesRequired != 2 || res.Cast.QuorumReached {
		t.Fatalf("Cast = %+v, want 1 of 2 votes cast, no quorum", res.Cast)
	}
	if got := model.FormatProposalID(res.ProposalID); got != ref {
		t.Errorf("ProposalID = %s, want %s", got, ref)
	}
	v, ok := proposalVotes(t, st, ref)["bob"]
	if !ok {
		t.Fatalf("no vote stored for env.Author bob")
	}
	if v.FindingsJSON == nil || len(v.FindingsJSON.Concerns) != 1 || v.Usage["tokens"] != 1200 {
		t.Errorf("stored vote = %+v, want its findings and usage", v)
	}
}

// TestVoteCastRefusalsWriteNoVote also pins Attempted: false for a ballot
// refused by the verb's own checks, true for one the engine refused.
func TestVoteCastRefusalsWriteNoVote(t *testing.T) {
	cases := []struct {
		name      string
		edit      func(*verb.VoteCastInput)
		code      output.ErrorCode
		want      string
		attempted bool
	}{
		{"malformed proposal id", func(in *verb.VoteCastInput) { in.Proposal = "DKT-Vx" }, output.ErrValidation, "invalid proposal ID", false},
		{"malformed findings JSON", func(in *verb.VoteCastInput) { in.FindingsJSON = `{"blockers":` }, output.ErrValidation, "--findings-json", false},
		{"evidence on an unbound proposal", func(in *verb.VoteCastInput) {
			in.FindingsJSON = `{"blockers":[{"text":"x","evidence":["artifact:ARTIFACT-1"]}]}`
		}, output.ErrValidation, "DKT-V1", false},
		{"confidence out of range", func(in *verb.VoteCastInput) { in.Confidence = 1.1 }, output.ErrValidation, "--confidence", false},
		{"domain relevance out of range", func(in *verb.VoteCastInput) { in.DomainRelevance = -0.1 }, output.ErrValidation, "--domain-relevance", false},
		{"unknown verdict", func(in *verb.VoteCastInput) { in.Verdict = "maybe" }, output.ErrValidation, "maybe", false},
		{"usage that is not a spend report", func(in *verb.VoteCastInput) { in.Usage = `[1]` }, output.ErrValidation, "--usage", false},
		{"concerns without a rationale", func(in *verb.VoteCastInput) { in.Verdict = "reject" }, output.ErrValidation, "rationale", true},
		{"no such proposal", func(in *verb.VoteCastInput) { in.Proposal = "DKT-V99" }, output.ErrNotFound, "proposal DKT-V99 not found", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			ref := openProposal(t, st, 2, "")
			in := castInput(ref, "seat")
			tc.edit(&in)
			res, err := verb.VoteCast(context.Background(), projectEnv(st, "", false), in)
			requireVerbCode(t, err, tc.code, tc.want)
			if res.Cast != nil {
				t.Errorf("a refused cast returned a Cast: %+v", res.Cast)
			}
			if res.Attempted != tc.attempted {
				t.Errorf("Attempted = %v, want %v", res.Attempted, tc.attempted)
			}
			if n := len(proposalVotes(t, st, ref)); n != 0 {
				t.Errorf("a refused cast stored %d vote(s)", n)
			}
		})
	}
}

func TestVoteCastSecondBallotFromOneVoterIsAConflict(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 3, "")
	env := projectEnv(st, "", false)
	if _, err := verb.VoteCast(context.Background(), env, castInput(ref, "seat")); err != nil {
		t.Fatalf("first VoteCast: %v", err)
	}
	_, err := verb.VoteCast(context.Background(), env, castInput(ref, "seat"))
	requireVerbCode(t, err, output.ErrConflict, `voter "seat" has already voted on `+ref)
}

func TestVoteCastUnanchoredCallerIsRefusedBeforeTheCast(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 2, "")
	_, err := verb.VoteCast(context.Background(), projectEnv(st, "/work/scratch", false), castInput(ref, "seat"))
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
	requireUnregistered(t, st, "/work/scratch")
	if n := len(proposalVotes(t, st, ref)); n != 0 {
		t.Errorf("a refused caller stored %d vote(s)", n)
	}
}

// encodedBag returns the JSON text of a one-key bag that encodes to exactly
// n bytes, the quantity the metadata cap measures.
func encodedBag(t *testing.T, n int) string {
	t.Helper()
	const shape = `{"blob":""}`
	raw := `{"blob":"` + strings.Repeat("x", n-len(shape)) + `"}`
	if len(raw) != n {
		t.Fatalf("fixture is %d bytes, want %d", len(raw), n)
	}
	return raw
}

// TestVoteCastMetadata pins what a ballot's metadata bag accepts and what it
// refuses, and that every refusal names the flag a caster fixes.
func TestVoteCastMetadata(t *testing.T) {
	accepts := []struct {
		name string
		raw  string
		want map[string]any
	}{
		{"an unset bag asserts nothing", "", nil},
		{"an object decodes whole", `{"a":"b","n":2,"nested":{"k":true}}`, map[string]any{
			"a": "b", "n": float64(2), "nested": map[string]any{"k": true},
		}},
		// The at-cap accept tells `>` from `>=`.
		{"exactly at the cap is accepted", encodedBag(t, store.VoteMetadataMaxBytes), map[string]any{
			"blob": strings.Repeat("x", store.VoteMetadataMaxBytes-len(`{"blob":""}`)),
		}},
		// The cap measures the encoded bag, not the caller's text: padding
		// is insignificant to JSON.
		{"whitespace padding is not part of the size", "{" + strings.Repeat(" ", store.VoteMetadataMaxBytes) + `"a":1}`, map[string]any{
			"a": float64(1),
		}},
	}
	for _, tc := range accepts {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			ref := openProposal(t, st, 2, "")
			in := castInput(ref, "seat")
			in.Metadata = tc.raw
			if _, err := verb.VoteCast(context.Background(), projectEnv(st, "", false), in); err != nil {
				t.Fatalf("VoteCast with metadata %.40q: %v", tc.raw, err)
			}
			if got := proposalVotes(t, st, ref)["seat"].Metadata; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("stored metadata = %#v, want %#v", got, tc.want)
			}
		})
	}

	refusals := []struct {
		name string
		raw  string
	}{
		{"a JSON array is not a bag", `["a","b"]`},
		{"a bare scalar is not a bag", `"a"`},
		{"null is not a bag", `null`},
		{"malformed JSON", `{"a":`},
		{"one byte over the cap", encodedBag(t, store.VoteMetadataMaxBytes+1)},
		// encoding/json writes `<` as six bytes, so a bag under the cap as
		// text is over it as stored.
		{"escaping pushes an under-cap bag over", `{"a":"` + strings.Repeat("<", store.VoteMetadataMaxBytes/2) + `"}`},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			ref := openProposal(t, st, 2, "")
			in := castInput(ref, "seat")
			in.Metadata = tc.raw
			_, err := verb.VoteCast(context.Background(), projectEnv(st, "", false), in)
			requireVerbCode(t, err, output.ErrValidation, "--metadata")
			if n := len(proposalVotes(t, st, ref)); n != 0 {
				t.Errorf("a refused bag stored %d vote(s)", n)
			}
		})
	}
}

func TestVoteCloseClosesAnOpenProposal(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 2, "")
	res, err := verb.VoteClose(context.Background(), projectEnv(st, "", false), verb.VoteCloseInput{
		Proposal: ref, Reason: "decided out of band",
	})
	if err != nil {
		t.Fatalf("VoteClose: %v", err)
	}
	p, err := st.GetProposal(res.ProposalID)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
	if p.Status != model.ProposalStatusClosed || p.FinalOutcome != "decided out of band" {
		t.Errorf("proposal after close = %+v, want closed with the reason as its outcome", p)
	}
}

func TestVoteCloseRefusals(t *testing.T) {
	cases := []struct {
		name string
		key  string
		in   func(ref string) verb.VoteCloseInput
		code output.ErrorCode
		want string
	}{
		{"malformed proposal id", "", func(string) verb.VoteCloseInput {
			return verb.VoteCloseInput{Proposal: "nope", Reason: "r"}
		}, output.ErrValidation, "invalid proposal ID"},
		{"no reason", "", func(ref string) verb.VoteCloseInput {
			return verb.VoteCloseInput{Proposal: ref}
		}, output.ErrValidation, "--reason is required"},
		{"a vote step's proposal", "vote-step:1:1:review@0", func(ref string) verb.VoteCloseInput {
			return verb.VoteCloseInput{Proposal: ref, Reason: "r"}
		}, output.ErrConflict, "docket step resolve"},
		{"no such proposal", "", func(string) verb.VoteCloseInput {
			return verb.VoteCloseInput{Proposal: "DKT-V99", Reason: "r"}
		}, output.ErrNotFound, "proposal DKT-V99 not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			ref := openProposal(t, st, 2, tc.key)
			_, err := verb.VoteClose(context.Background(), projectEnv(st, "", false), tc.in(ref))
			requireVerbCode(t, err, tc.code, tc.want)
			p, err := st.GetProposal(1)
			if err != nil {
				t.Fatalf("GetProposal: %v", err)
			}
			if p.Status != model.ProposalStatusOpen {
				t.Errorf("a refused close left the proposal %s", p.Status)
			}
		})
	}
}

func TestVoteCloseDecidedProposalIsAConflict(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 1, "")
	env := projectEnv(st, "", false)
	if _, err := verb.VoteCast(context.Background(), env, castInput(ref, "seat")); err != nil {
		t.Fatalf("VoteCast: %v", err)
	}
	_, err := verb.VoteClose(context.Background(), env, verb.VoteCloseInput{Proposal: ref, Reason: "r"})
	requireVerbCode(t, err, output.ErrConflict, "")
}

func TestVoteCloseUnanchoredCallerIsRefusedBeforeTheClose(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 2, "")
	_, err := verb.VoteClose(context.Background(), projectEnv(st, "/work/scratch", false), verb.VoteCloseInput{
		Proposal: ref, Reason: "r",
	})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
	requireUnregistered(t, st, "/work/scratch")
	p, err := st.GetProposal(1)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
	if p.Status != model.ProposalStatusOpen {
		t.Errorf("a refused caller left the proposal %s", p.Status)
	}
}

// TestVoteWritesRegisterAnAnchoredCallersProject: each vote write, and
// VoteProject, reports the project it registered on first contact, and a
// second call reports none.
func TestVoteWritesRegisterAnAnchoredCallersProject(t *testing.T) {
	writes := map[string]func(env verb.Env, ref string) (*model.Project, error){
		"VoteProject": func(env verb.Env, _ string) (*model.Project, error) {
			res, err := verb.VoteProject(context.Background(), env)
			return res.Registered, err
		},
		"VoteCreate": func(env verb.Env, _ string) (*model.Project, error) {
			res, err := verb.VoteCreate(context.Background(), env, verb.VoteCreateInput{
				Description: "d", Criticality: "low", Voters: 1, Threshold: 0.5,
			})
			return res.Registered, err
		},
		"VoteCast": func(env verb.Env, ref string) (*model.Project, error) {
			res, err := verb.VoteCast(context.Background(), env, castInput(ref, "seat"))
			return res.Registered, err
		},
		"VoteClose": func(env verb.Env, ref string) (*model.Project, error) {
			res, err := verb.VoteClose(context.Background(), env, verb.VoteCloseInput{Proposal: ref, Reason: "r"})
			return res.Registered, err
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			ensureProject(t, st, "/repos/first", "first")
			env := projectEnv(st, "/repos/vote", true)

			registered, err := write(env, openProposal(t, st, 2, ""))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			id, found, err := st.LookupProject("/repos/vote")
			if err != nil || !found {
				t.Fatalf("LookupProject = (%d, %v, %v), want the anchored identity registered", id, found, err)
			}
			if registered == nil || registered.ID != id || registered.Identity != "/repos/vote" {
				t.Errorf("Registered = %+v, want project %d for /repos/vote", registered, id)
			}

			again, err := write(env, openProposal(t, st, 2, ""))
			if err != nil {
				t.Fatalf("second %s: %v", name, err)
			}
			if again != nil {
				t.Errorf("a second %s reported registering %+v again", name, again)
			}
		})
	}
}

func TestVoteProjectRefusesAnUnanchoredCaller(t *testing.T) {
	st := openStore(t)
	_, err := verb.VoteProject(context.Background(), projectEnv(st, "/work/scratch", false))
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
	requireUnregistered(t, st, "/work/scratch")
}

// voteStepWorkflow puts a one-seat vote step between two executor steps, so
// the seat's ballot decides it and routing makes `last` claimable.
const voteStepWorkflow = `
[pipeline]
name = "unit-vote-step"
version = 1
[match]
kind = ["task"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
[[step]]
name = "gate"
after = ["first"]
type = "vote"
voters = ["seat"]
vote_rule = "majority"
on_fail = "skip"
[[step]]
name = "last"
after = ["gate"]
executor = "someone"
emits = "result"
`

// voteStepProposal activates voteStepWorkflow and records its first step,
// which opens the gate's proposal. It returns the proposal and the run's
// step ids by instance.
func voteStepProposal(t *testing.T, st store.Store) (string, map[string]int) {
	t.Helper()
	if err := st.SetConfig(0, store.VoteRuleThresholdKey("majority"), "0.5"); err != nil {
		t.Fatalf("registering the vote rule: %v", err)
	}
	env := projectEnv(st, "", false)
	first := activatedStepOf(t, st, voteStepWorkflow)
	claim := claimStep(t, env, first)
	if _, err := verb.StepComplete(context.Background(), env, verb.StepCompleteInput{
		Step: model.FormatStepID(first), Token: claim.Claim.Token, Artifact: []byte("result"),
	}); err != nil {
		t.Fatalf("StepComplete: %v", err)
	}

	step, err := st.GetStep(first)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	steps, err := st.ListRunSteps(step.RunID)
	if err != nil {
		t.Fatalf("ListRunSteps: %v", err)
	}
	byInstance := map[string]int{}
	for _, s := range steps {
		byInstance[s.Instance] = s.ID
	}
	proposals, _, err := st.ListProposals(store.DefaultProjectID, "", "", "", 10)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("ListProposals = (%d proposals, %v), want the gate's one", len(proposals), err)
	}
	return model.FormatProposalID(proposals[0].ID), byInstance
}

func TestVoteCastDecidingBallotRoutesTheVoteStep(t *testing.T) {
	st := openStore(t)
	ref, steps := voteStepProposal(t, st)
	env := projectEnv(st, "", false)

	res, err := verb.VoteCast(context.Background(), env, castInput(ref, "seat"))
	if err != nil {
		t.Fatalf("VoteCast: %v", err)
	}
	if res.Cast == nil || !res.Cast.QuorumReached {
		t.Fatalf("Cast = %+v, want the deciding ballot", res.Cast)
	}
	gate, err := st.GetStep(steps["gate@0"])
	if err != nil {
		t.Fatalf("GetStep(gate@0): %v", err)
	}
	if gate.Status != store.StepDone {
		t.Errorf("gate@0 is %s after the deciding ballot, want %s", gate.Status, store.StepDone)
	}
	claimStep(t, env, steps["last@0"])
}

// TestVoteCastRoutingFailureReturnsNoCast: a deciding ballot that commits and
// then fails to route returns the routing error with no Cast, and the vote
// stays recorded.
func TestVoteCastRoutingFailureReturnsNoCast(t *testing.T) {
	st := openStore(t)
	ref, steps := voteStepProposal(t, st)
	conn, err := store.SQLDB(st)
	if err != nil {
		t.Fatalf("SQLDB: %v", err)
	}
	if _, err := conn.Exec(
		`CREATE TRIGGER fail_gate_routing BEFORE UPDATE ON steps
		 WHEN OLD.instance = 'gate@0'
		 BEGIN SELECT RAISE(ABORT, 'injected routing failure'); END`); err != nil {
		t.Fatalf("installing the failing trigger: %v", err)
	}

	res, err := verb.VoteCast(context.Background(), projectEnv(st, "", false), castInput(ref, "seat"))
	requireVerbCode(t, err, output.ErrGeneral,
		"the vote recorded and the proposal decided, but routing the vote step failed: ")
	if !strings.Contains(err.Error(), "injected routing failure") {
		t.Errorf("error %q does not carry the routing cause", err)
	}
	if res.Cast != nil {
		t.Errorf("a routing failure returned a Cast: %+v", res.Cast)
	}
	if _, ok := proposalVotes(t, st, ref)["seat"]; !ok {
		t.Errorf("the committed ballot is missing after the routing failure")
	}
	if gate, err := st.GetStep(steps["gate@0"]); err != nil || gate.Status == store.StepDone {
		t.Errorf("gate@0 = (%+v, %v), want it left unrouted", gate, err)
	}
}

// approvedVoteStepProposal decides a vote step's proposal with the seat's
// approving ballot and binds its run to token's conductor capability, or
// leaves the run unbound when token is empty.
func approvedVoteStepProposal(t *testing.T, st store.Store, token string) string {
	t.Helper()
	ref, steps := voteStepProposal(t, st)
	if _, err := verb.VoteCast(context.Background(), projectEnv(st, "", false), castInput(ref, "seat")); err != nil {
		t.Fatalf("VoteCast: %v", err)
	}
	gate, err := st.GetStep(steps["gate@0"])
	if err != nil {
		t.Fatalf("GetStep(gate@0): %v", err)
	}
	bindConductor(t, st, model.FormatRunID(gate.RunID), token)
	return ref
}

func requireProposalStatus(t *testing.T, st store.Store, ref string, want model.ProposalStatus) {
	t.Helper()
	id, err := model.ParseProposalID(ref)
	if err != nil {
		t.Fatalf("ParseProposalID: %v", err)
	}
	p, err := st.GetProposal(id)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
	if p.Status != want {
		t.Errorf("proposal %s is %s, want %s", ref, p.Status, want)
	}
}

func TestVoteCommitCommitsAnApprovedProposal(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 1, "")
	env := projectEnv(st, "", false)
	if _, err := verb.VoteCast(context.Background(), env, castInput(ref, "seat")); err != nil {
		t.Fatalf("VoteCast: %v", err)
	}

	res, err := verb.VoteCommit(context.Background(), env, verb.VoteCommitInput{
		Proposal: ref, Outcome: "shipped", EscalationReason: "out of hours",
	})
	if err != nil {
		t.Fatalf("VoteCommit: %v", err)
	}
	p, err := st.GetProposal(res.ProposalID)
	if err != nil {
		t.Fatalf("GetProposal: %v", err)
	}
	if model.FormatProposalID(p.ID) != ref || p.Status != model.ProposalStatusCommitted ||
		p.FinalOutcome != "shipped" || p.EscalationReason == nil || *p.EscalationReason != "out of hours" {
		t.Errorf("proposal after commit = %+v, want %s committed with the outcome and reason", p, ref)
	}
}

func TestVoteCommitRefusals(t *testing.T) {
	cases := []struct {
		name     string
		proposal string
		code     output.ErrorCode
		want     string
	}{
		{"malformed proposal id", "nope", output.ErrValidation, "invalid proposal ID"},
		{"no such proposal", "DKT-V99", output.ErrNotFound, "proposal DKT-V99 not found"},
		{"an open proposal", "DKT-V1", output.ErrConflict, "must be approved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			openProposal(t, st, 2, "")
			_, err := verb.VoteCommit(context.Background(), projectEnv(st, "", false),
				verb.VoteCommitInput{Proposal: tc.proposal, Outcome: "o"})
			requireVerbCode(t, err, tc.code, tc.want)
			requireProposalStatus(t, st, "DKT-V1", model.ProposalStatusOpen)
		})
	}
}

func TestVoteCommitUnanchoredCallerIsRefusedBeforeTheCommit(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 1, "")
	if _, err := verb.VoteCast(context.Background(), projectEnv(st, "", false), castInput(ref, "seat")); err != nil {
		t.Fatalf("VoteCast: %v", err)
	}
	_, err := verb.VoteCommit(context.Background(), projectEnv(st, "/work/scratch", false),
		verb.VoteCommitInput{Proposal: ref, Outcome: "o"})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
	requireUnregistered(t, st, "/work/scratch")
	requireProposalStatus(t, st, ref, model.ProposalStatusApproved)
}

// TestVoteCommitVoteStepProposalNeedsTheRunsCapability: a bound run's
// proposal commits only under the run's own capability, and the token source
// is never read for an unbound run.
func TestVoteCommitVoteStepProposalNeedsTheRunsCapability(t *testing.T) {
	capability, _, err := model.MintToken()
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	wrong, _, err := model.MintToken()
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	cases := []struct {
		name      string
		bound     bool
		presented string
		code      output.ErrorCode
		want      string
	}{
		{name: "no token", bound: true, code: output.ErrValidation, want: "requires it"},
		{name: "a token that is not the capability", bound: true, presented: wrong, code: output.ErrAuth, want: "is not"},
		{name: "the run's capability", bound: true, presented: capability},
		{name: "an unbound run", presented: wrong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			bound := ""
			if tc.bound {
				bound = capability
			}
			ref := approvedVoteStepProposal(t, st, bound)
			read := 0
			_, err := verb.VoteCommit(context.Background(), projectEnv(st, "", false), verb.VoteCommitInput{
				Proposal: ref, Outcome: "o",
				ConductorToken: func() string { read++; return tc.presented },
			})
			if tc.code != "" {
				requireVerbCode(t, err, tc.code, tc.want)
				if tc.presented != "" && strings.Contains(err.Error(), tc.presented) {
					t.Errorf("the refusal echoes the presented token: %v", err)
				}
				requireProposalStatus(t, st, ref, model.ProposalStatusApproved)
				return
			}
			if err != nil {
				t.Fatalf("VoteCommit: %v", err)
			}
			requireProposalStatus(t, st, ref, model.ProposalStatusCommitted)
			if !tc.bound && read != 0 {
				t.Errorf("the token source was read %d times for an unbound run", read)
			}
		})
	}
}

func TestVoteShowReportsTheProposalVotesAndLinks(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 2, "")
	env := projectEnv(st, "", false)
	if _, err := verb.VoteCast(context.Background(), env, castInput(ref, "seat")); err != nil {
		t.Fatalf("VoteCast: %v", err)
	}
	pid, err := model.ParseProposalID(ref)
	if err != nil {
		t.Fatalf("ParseProposalID: %v", err)
	}
	issue := createIssue(t, st)
	if err := st.LinkProposalIssue(pid, issue); err != nil {
		t.Fatalf("LinkProposalIssue: %v", err)
	}

	res, err := verb.VoteShow(context.Background(), env, verb.VoteShowInput{Proposal: ref})
	if err != nil {
		t.Fatalf("VoteShow: %v", err)
	}
	if res.Proposal == nil || res.Proposal.ID != pid {
		t.Errorf("Proposal = %+v, want %s", res.Proposal, ref)
	}
	if len(res.Votes) != 1 || res.Votes[0].VoterName != "seat" {
		t.Errorf("Votes = %+v, want the seat's one ballot", res.Votes)
	}
	if !reflect.DeepEqual(res.LinkedIssues, []int{issue}) {
		t.Errorf("LinkedIssues = %v, want [%d]", res.LinkedIssues, issue)
	}
	if len(res.LinkedDocs) != 0 {
		t.Errorf("LinkedDocs = %v, want none", res.LinkedDocs)
	}
}

// TestVoteReadsRefuseByTheGivenReference: both reads refuse a malformed id
// and name a missing proposal by the reference the caller gave.
func TestVoteReadsRefuseByTheGivenReference(t *testing.T) {
	reads := map[string]func(env verb.Env, ref string) error{
		"VoteResult": func(env verb.Env, ref string) error {
			_, err := verb.VoteResult(context.Background(), env, verb.VoteResultInput{Proposal: ref})
			return err
		},
		"VoteShow": func(env verb.Env, ref string) error {
			_, err := verb.VoteShow(context.Background(), env, verb.VoteShowInput{Proposal: ref})
			return err
		},
	}
	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			env := projectEnv(openStore(t), "", false)
			requireVerbCode(t, read(env, "nope"), output.ErrValidation, "invalid proposal ID")
			requireVerbCode(t, read(env, "99"), output.ErrNotFound, "proposal 99 not found")
		})
	}
}

// TestVoteReadsNeverRegisterAProject: the reads resolve read-only, so an
// anchored caller's unregistered identity is not registered and an
// unanchored one is not refused.
func TestVoteReadsNeverRegisterAProject(t *testing.T) {
	reads := map[string]func(env verb.Env, ref string) error{
		"VoteResult": func(env verb.Env, ref string) error {
			_, err := verb.VoteResult(context.Background(), env, verb.VoteResultInput{Proposal: ref})
			return err
		},
		"VoteShow": func(env verb.Env, ref string) error {
			_, err := verb.VoteShow(context.Background(), env, verb.VoteShowInput{Proposal: ref})
			return err
		},
	}
	for name, read := range reads {
		for _, anchored := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s anchored=%v", name, anchored), func(t *testing.T) {
				st := openStore(t)
				ensureProject(t, st, "/repos/first", "first")
				ref := openProposal(t, st, 2, "")
				if err := read(projectEnv(st, "/repos/vote", anchored), ref); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				requireUnregistered(t, st, "/repos/vote")
			})
		}
	}
}

// listedRefs is the proposal references a VoteList result carries.
func listedRefs(res verb.VoteListResult) []string {
	refs := make([]string, 0, len(res.Proposals))
	for _, p := range res.Proposals {
		refs = append(refs, model.FormatProposalID(p.ID))
	}
	return refs
}

// TestVoteListFiltersTheCallersProposals: open proposals only by default,
// every status under All, exactly the named status otherwise, and vote counts
// only when asked for.
func TestVoteListFiltersTheCallersProposals(t *testing.T) {
	st := openStore(t)
	open := openProposal(t, st, 2, "")
	closed := openProposal(t, st, 2, "")
	env := projectEnv(st, "", false)
	if _, err := verb.VoteClose(context.Background(), env, verb.VoteCloseInput{Proposal: closed, Reason: "r"}); err != nil {
		t.Fatalf("VoteClose: %v", err)
	}
	if _, err := verb.VoteCast(context.Background(), env, castInput(open, "seat")); err != nil {
		t.Fatalf("VoteCast: %v", err)
	}

	cases := []struct {
		name string
		in   verb.VoteListInput
		want []string
	}{
		{"default", verb.VoteListInput{}, []string{open}},
		{"all", verb.VoteListInput{All: true}, []string{open, closed}},
		{"status", verb.VoteListInput{Status: "closed"}, []string{closed}},
		{"all with status", verb.VoteListInput{All: true, Status: "closed"}, []string{closed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := verb.VoteList(context.Background(), env, tc.in)
			if err != nil {
				t.Fatalf("VoteList: %v", err)
			}
			got := listedRefs(res)
			if res.Total != len(tc.want) || len(got) != len(tc.want) {
				t.Fatalf("VoteList(%+v) = %v (total %d), want %v", tc.in, got, res.Total, tc.want)
			}
			for _, want := range tc.want {
				if !strings.Contains(strings.Join(got, " "), want) {
					t.Errorf("VoteList(%+v) = %v, missing %s", tc.in, got, want)
				}
			}
			if res.VoteCounts != nil {
				t.Errorf("VoteCounts = %v without asking, want nil", res.VoteCounts)
			}
		})
	}

	res, err := verb.VoteList(context.Background(), env, verb.VoteListInput{VoteCounts: true})
	if err != nil {
		t.Fatalf("VoteList with counts: %v", err)
	}
	pid, _ := model.ParseProposalID(open)
	if !reflect.DeepEqual(res.VoteCounts, map[int]int{pid: 1}) {
		t.Errorf("VoteCounts = %v, want {%d: 1}", res.VoteCounts, pid)
	}
}

func TestVoteListRefusesAnUnknownFilter(t *testing.T) {
	env := projectEnv(openStore(t), "", false)
	for name, in := range map[string]verb.VoteListInput{
		"status":      {Status: "bogus"},
		"criticality": {Criticality: "bogus"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := verb.VoteList(context.Background(), env, in)
			requireVerbCode(t, err, output.ErrValidation, "bogus")
		})
	}
}

// TestVoteListFiltersByCriticalityDomainTagAndLimit: each filter keeps only
// its matches, and the limit caps the listing while Total still counts
// every match.
func TestVoteListFiltersByCriticalityDomainTagAndLimit(t *testing.T) {
	st := openStore(t)
	propose := func(criticality model.Criticality, tags ...string) string {
		t.Helper()
		id, err := st.CreateProposalIdempotent(&model.Proposal{
			Description: "a ballot", Criticality: criticality, DomainTags: tags,
			Status: model.ProposalStatusOpen, RequiredVoters: 2, Threshold: 0.5,
		}, "")
		if err != nil {
			t.Fatalf("CreateProposalIdempotent: %v", err)
		}
		return model.FormatProposalID(id)
	}
	highSecurity := propose(model.CriticalityHigh, "security")
	highStorage := propose(model.CriticalityHigh, "storage")
	lowSecurity := propose(model.CriticalityLow, "security")
	env := projectEnv(st, "", false)

	cases := []struct {
		name  string
		in    verb.VoteListInput
		total int
		among []string
	}{
		{"criticality", verb.VoteListInput{Criticality: "high"}, 2, []string{highSecurity, highStorage}},
		{"domain tag", verb.VoteListInput{DomainTag: "security"}, 2, []string{highSecurity, lowSecurity}},
		{"both", verb.VoteListInput{Criticality: "high", DomainTag: "security"}, 1, []string{highSecurity}},
		{"limit", verb.VoteListInput{Limit: 1}, 3, []string{highSecurity, highStorage, lowSecurity}},
		{"limited criticality", verb.VoteListInput{Criticality: "high", Limit: 1}, 2, []string{highSecurity, highStorage}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := verb.VoteList(context.Background(), env, tc.in)
			if err != nil {
				t.Fatalf("VoteList: %v", err)
			}
			got := listedRefs(res)
			wantListed := tc.total
			if tc.in.Limit > 0 && tc.in.Limit < wantListed {
				wantListed = tc.in.Limit
			}
			if res.Total != tc.total || len(got) != wantListed {
				t.Fatalf("VoteList(%+v) = %v (total %d), want %d listed of total %d among %v",
					tc.in, got, res.Total, wantListed, tc.total, tc.among)
			}
			for _, ref := range got {
				if !slices.Contains(tc.among, ref) {
					t.Errorf("VoteList(%+v) listed %s, want only %v", tc.in, ref, tc.among)
				}
			}
		})
	}
}

// TestVoteListNeverRegistersAProject: the listing is a read, so an anchored
// unregistered identity is not registered and sees none of the default
// project's proposals, and an unanchored one is not refused.
func TestVoteListNeverRegistersAProject(t *testing.T) {
	for _, anchored := range []bool{true, false} {
		t.Run(fmt.Sprintf("anchored=%v", anchored), func(t *testing.T) {
			st := openStore(t)
			ensureProject(t, st, "/repos/first", "first")
			openProposal(t, st, 2, "")
			res, err := verb.VoteList(context.Background(), projectEnv(st, "/repos/vote", anchored), verb.VoteListInput{})
			if err != nil {
				t.Fatalf("VoteList: %v", err)
			}
			if len(res.Proposals) != 0 || res.Total != 0 {
				t.Errorf("VoteList from an unregistered identity = %v, want none", listedRefs(res))
			}
			requireUnregistered(t, st, "/repos/vote")
		})
	}
}

// TestVoteLinkAndUnlink: link writes the link and reports the pair, a second
// link is a conflict, unlink removes it, and a second unlink finds nothing.
func TestVoteLinkAndUnlink(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 2, "")
	pid, _ := model.ParseProposalID(ref)
	issue := createIssue(t, st)
	env := projectEnv(st, "", false)
	in := verb.VoteLinkInput{Proposal: ref, Issue: model.FormatID(issue)}

	res, err := verb.VoteLink(context.Background(), env, in)
	if err != nil {
		t.Fatalf("VoteLink: %v", err)
	}
	if res.ProposalID != pid || res.IssueID != issue {
		t.Errorf("VoteLink = %+v, want proposal %d and issue %d", res, pid, issue)
	}
	if linked, err := st.GetProposalIssues(pid); err != nil || !reflect.DeepEqual(linked, []int{issue}) {
		t.Errorf("GetProposalIssues = (%v, %v), want [%d]", linked, err, issue)
	}
	_, err = verb.VoteLink(context.Background(), env, in)
	requireVerbCode(t, err, output.ErrConflict, "link already exists")

	unlinked, err := verb.VoteUnlink(context.Background(), env, verb.VoteUnlinkInput(in))
	if err != nil {
		t.Fatalf("VoteUnlink: %v", err)
	}
	if unlinked.ProposalID != pid || unlinked.IssueID != issue {
		t.Errorf("VoteUnlink = %+v, want proposal %d and issue %d", unlinked, pid, issue)
	}
	if linked, err := st.GetProposalIssues(pid); err != nil || len(linked) != 0 {
		t.Errorf("GetProposalIssues after unlink = (%v, %v), want none", linked, err)
	}
	_, err = verb.VoteUnlink(context.Background(), env, verb.VoteUnlinkInput(in))
	requireVerbCode(t, err, output.ErrNotFound, "link not found")
}

func TestVoteLinkRefusals(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 2, "")
	issue := model.FormatID(createIssue(t, st))
	env := projectEnv(st, "", false)

	link := func(proposal, issue string) error {
		_, err := verb.VoteLink(context.Background(), env, verb.VoteLinkInput{Proposal: proposal, Issue: issue})
		return err
	}
	unlink := func(proposal, issue string) error {
		_, err := verb.VoteUnlink(context.Background(), env, verb.VoteUnlinkInput{Proposal: proposal, Issue: issue})
		return err
	}
	for name, call := range map[string]func(string, string) error{"VoteLink": link, "VoteUnlink": unlink} {
		t.Run(name, func(t *testing.T) {
			requireVerbCode(t, call("nope", issue), output.ErrValidation, "invalid proposal ID")
			requireVerbCode(t, call(ref, "nope"), output.ErrValidation, "invalid issue ID")
		})
	}
	requireVerbCode(t, link("DKT-V999", issue), output.ErrNotFound, "proposal or issue not found")
}

// TestVoteLinkOpensThePanelOnTheRunItJoins: linking a step-less proposal to
// the issue a run binds records the panel's vote-opened on that run, once.
func TestVoteLinkOpensThePanelOnTheRunItJoins(t *testing.T) {
	st := openStore(t)
	step, err := st.GetStep(activatedStep(t, st))
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	ref := openProposal(t, st, 2, "")
	env := projectEnv(st, "", false)

	if _, err := verb.VoteLink(context.Background(), env,
		verb.VoteLinkInput{Proposal: ref, Issue: model.FormatID(step.IssueID)}); err != nil {
		t.Fatalf("VoteLink: %v", err)
	}
	page, err := engine.ListEventsStore(st, engine.EventQuery{RunID: step.RunID, Kind: engine.EventVoteOpened})
	if err != nil {
		t.Fatalf("ListEventsStore: %v", err)
	}
	if len(page.Events) != 1 || !strings.Contains(string(page.Events[0].Data), ref) {
		t.Errorf("vote-opened events on the run = %d (%v), want one naming %s", len(page.Events), page.Events, ref)
	}
}

// notSQLiteStore hides the SQLite store behind its interface, so the
// engine's panel-event write, which needs the *sql.DB, fails while every
// store.Store call still succeeds.
type notSQLiteStore struct{ store.Store }

// TestVoteLinkReportsALinkWhoseRunEventsFailed: the link is written before
// the run events, so a failed event write keeps the link and says so.
func TestVoteLinkReportsALinkWhoseRunEventsFailed(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 2, "")
	pid, _ := model.ParseProposalID(ref)
	issue := createIssue(t, st)

	_, err := verb.VoteLink(context.Background(), projectEnv(notSQLiteStore{st}, "", false),
		verb.VoteLinkInput{Proposal: ref, Issue: model.FormatID(issue)})
	requireVerbCode(t, err, output.ErrGeneral, "the link recorded, but recording "+ref+"'s run events failed")
	if linked, err := st.GetProposalIssues(pid); err != nil || !reflect.DeepEqual(linked, []int{issue}) {
		t.Errorf("GetProposalIssues = (%v, %v), want the link kept as [%d]", linked, err, issue)
	}
}

// TestVoteBackfillUsageRecordsTheBatch: the batch lands on each seat's cast,
// and the result counts its rows and distinct seats under the source the
// rows carry, "backfilled" by default.
func TestVoteBackfillUsageRecordsTheBatch(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 3, "")
	pid, _ := model.ParseProposalID(ref)
	env := projectEnv(st, "", false)
	for _, seat := range []string{"seat-a", "seat-b"} {
		if _, err := verb.VoteCast(context.Background(), env, castInput(ref, seat)); err != nil {
			t.Fatalf("VoteCast(%s): %v", seat, err)
		}
	}

	res, err := verb.VoteBackfillUsage(context.Background(), env, verb.VoteBackfillUsageInput{
		Proposal: ref,
		Rows: backfillRows(
			engine.VoteBackfillRow{Voter: "seat-a", Unit: "input_tokens", Quantity: 10},
			engine.VoteBackfillRow{Voter: "seat-a", Unit: "output_tokens", Quantity: 20},
			engine.VoteBackfillRow{Voter: "seat-b", Unit: "input_tokens", Quantity: 30},
		),
	})
	if err != nil {
		t.Fatalf("VoteBackfillUsage: %v", err)
	}
	if res.ProposalID != pid || res.Rows != 3 || res.Seats != 2 || res.Source != "backfilled" {
		t.Errorf("VoteBackfillUsage = %+v, want proposal %d, 3 rows, 2 seats, source backfilled", res, pid)
	}
	votes := proposalVotes(t, st, ref)
	if got := votes["seat-a"].Usage; !reflect.DeepEqual(got, map[string]float64{"input_tokens": 10, "output_tokens": 20}) {
		t.Errorf("seat-a usage = %v", got)
	}

	res, err = verb.VoteBackfillUsage(context.Background(), env, verb.VoteBackfillUsageInput{
		Proposal: ref, Source: "relay",
		Rows: backfillRows(engine.VoteBackfillRow{Voter: "seat-b", Unit: "output_tokens", Quantity: 40}),
	})
	if err != nil {
		t.Fatalf("VoteBackfillUsage with a source: %v", err)
	}
	if res.Rows != 1 || res.Seats != 1 || res.Source != "relay" {
		t.Errorf("VoteBackfillUsage = %+v, want 1 row, 1 seat, source relay", res)
	}
}

func TestVoteBackfillUsageRefusals(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 2, "")
	env := projectEnv(st, "", false)
	if _, err := verb.VoteCast(context.Background(), env, castInput(ref, "seat")); err != nil {
		t.Fatalf("VoteCast: %v", err)
	}
	row := backfillRows(engine.VoteBackfillRow{Voter: "seat", Unit: "tokens", Quantity: 1})
	backfill := func(proposal string, rows func() ([]engine.VoteBackfillRow, error)) error {
		_, err := verb.VoteBackfillUsage(context.Background(), env, verb.VoteBackfillUsageInput{Proposal: proposal, Rows: rows})
		return err
	}

	requireVerbCode(t, backfill("nope", row), output.ErrValidation, "invalid proposal ID")
	requireVerbCode(t, backfill("DKT-V999", row), output.ErrNotFound, "proposal DKT-V999 not found")
	requireVerbCode(t, backfill(ref, nil), output.ErrValidation, "no usage rows")
	requireVerbCode(t, backfill(ref, backfillRows()), output.ErrValidation, "no usage rows")
	requireVerbCode(t, backfill(ref, backfillRows(engine.VoteBackfillRow{Voter: "absent", Unit: "tokens", Quantity: 1})),
		output.ErrValidation, "absent")
}

// backfillRows is a VoteBackfillUsageInput.Rows that returns rows.
func backfillRows(rows ...engine.VoteBackfillRow) func() ([]engine.VoteBackfillRow, error) {
	return func() ([]engine.VoteBackfillRow, error) { return rows, nil }
}

// TestVoteBackfillUsageReadsTheBatchLast: the project and the proposal are
// settled before Rows is called, so an unanchored caller or a malformed id
// is refused without reading the batch, and a batch that cannot be read is
// reported unchanged after the anchored caller's project is registered.
func TestVoteBackfillUsageReadsTheBatchLast(t *testing.T) {
	st := openStore(t)
	ensureProject(t, st, "/repos/first", "first")
	ref := openProposal(t, st, 2, "")
	unreadable := errors.New("the batch cannot be read")
	reads := 0
	rows := func() ([]engine.VoteBackfillRow, error) {
		reads++
		return nil, unreadable
	}

	_, err := verb.VoteBackfillUsage(context.Background(), projectEnv(st, "/work/scratch", false),
		verb.VoteBackfillUsageInput{Proposal: ref, Rows: rows})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
	_, err = verb.VoteBackfillUsage(context.Background(), projectEnv(st, "/work/scratch", false),
		verb.VoteBackfillUsageInput{Proposal: "nope", Rows: rows})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
	_, err = verb.VoteBackfillUsage(context.Background(), projectEnv(st, "", false),
		verb.VoteBackfillUsageInput{Proposal: "nope", Rows: rows})
	requireVerbCode(t, err, output.ErrValidation, "invalid proposal ID")
	if reads != 0 {
		t.Fatalf("Rows was called %d times before the project and proposal were settled", reads)
	}

	res, err := verb.VoteBackfillUsage(context.Background(), projectEnv(st, "/repos/vote", true),
		verb.VoteBackfillUsageInput{Proposal: ref, Rows: rows})
	if !errors.Is(err, unreadable) {
		t.Errorf("VoteBackfillUsage error = %v, want the batch reader's error unchanged", err)
	}
	if reads != 1 {
		t.Errorf("Rows was called %d times, want once", reads)
	}
	if res.Registered == nil || res.Registered.Identity != "/repos/vote" {
		t.Errorf("Registered = %+v, want the anchored identity registered before the batch was read", res.Registered)
	}
}

// TestVoteLinkWritesRegisterAnAnchoredCallersProject: link, unlink and the
// usage back-fill write into the caller's project, so each registers an
// anchored identity on first contact and refuses an unanchored one before it
// writes.
func TestVoteLinkWritesRegisterAnAnchoredCallersProject(t *testing.T) {
	writes := map[string]func(env verb.Env, ref, issue string) (*model.Project, error){
		"VoteLink": func(env verb.Env, ref, issue string) (*model.Project, error) {
			res, err := verb.VoteLink(context.Background(), env, verb.VoteLinkInput{Proposal: ref, Issue: issue})
			return res.Registered, err
		},
		"VoteUnlink": func(env verb.Env, ref, issue string) (*model.Project, error) {
			if _, err := verb.VoteLink(context.Background(), projectEnv(env.Store, "", false),
				verb.VoteLinkInput{Proposal: ref, Issue: issue}); err != nil {
				return nil, err
			}
			res, err := verb.VoteUnlink(context.Background(), env, verb.VoteUnlinkInput{Proposal: ref, Issue: issue})
			return res.Registered, err
		},
		"VoteBackfillUsage": func(env verb.Env, ref, _ string) (*model.Project, error) {
			if _, err := verb.VoteCast(context.Background(), projectEnv(env.Store, "", false), castInput(ref, "seat")); err != nil {
				return nil, err
			}
			res, err := verb.VoteBackfillUsage(context.Background(), env, verb.VoteBackfillUsageInput{
				Proposal: ref, Rows: backfillRows(engine.VoteBackfillRow{Voter: "seat", Unit: "tokens", Quantity: 1}),
			})
			return res.Registered, err
		},
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			ensureProject(t, st, "/repos/first", "first")
			issue := model.FormatID(createIssue(t, st))

			_, err := write(projectEnv(st, "/work/scratch", false), openProposal(t, st, 2, ""), issue)
			requireVerbCode(t, err, output.ErrValidation, "cannot tell which project")
			requireUnregistered(t, st, "/work/scratch")

			env := projectEnv(st, "/repos/vote", true)
			registered, err := write(env, openProposal(t, st, 2, ""), issue)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			id, found, err := st.LookupProject("/repos/vote")
			if err != nil || !found {
				t.Fatalf("LookupProject = (%d, %v, %v), want the anchored identity registered", id, found, err)
			}
			if registered == nil || registered.ID != id || registered.Identity != "/repos/vote" {
				t.Errorf("Registered = %+v, want project %d for /repos/vote", registered, id)
			}

			again, err := write(env, openProposal(t, st, 2, ""), issue)
			if err != nil {
				t.Fatalf("second %s: %v", name, err)
			}
			if again != nil {
				t.Errorf("a second %s reported registering %+v again", name, again)
			}
		})
	}
}
