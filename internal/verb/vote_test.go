package verb_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

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

func TestVoteCastDecidingBallotReportsTheOutcome(t *testing.T) {
	st := openStore(t)
	ref := openProposal(t, st, 1, "")
	res, err := verb.VoteCast(context.Background(), projectEnv(st, "", false), castInput(ref, "seat"))
	if err != nil {
		t.Fatalf("VoteCast: %v", err)
	}
	if !res.Cast.QuorumReached || res.Cast.ProposalStatus != model.ProposalStatusApproved || res.Cast.WeightedScore == nil {
		t.Errorf("Cast = %+v, want quorum reached and the proposal approved with a score", res.Cast)
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
