package api

import (
	"context"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// voteHandlerWorkflow puts a one-seat vote step after an executor step, so
// completing the executor step opens the gate's proposal and the seat's
// ballot approves it.
const voteHandlerWorkflow = `
[pipeline]
name = "vote-handler"
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
`

// voteClient serves st and returns a raw client, which sends the headers
// attributed sets; client.NewClient would replace them with this host's
// identity.
func voteClient(t *testing.T, st store.Store) docketv1connect.DocketServiceClient {
	t.Helper()
	server := serve(t, NewService(st, "dev"))
	return docketv1connect.NewDocketServiceClient(server.Client(), server.URL)
}

// voteStepProposal activates a run of voteHandlerWorkflow in the default
// project of st and completes its first step, which opens the vote step's
// proposal. It returns the proposal and the conductor capability activation
// bound the run to.
func voteStepProposal(t *testing.T, st store.Store) (proposal, capability string) {
	t.Helper()
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	if err := st.SetConfig(0, store.VoteRuleThresholdKey("majority"), "0.5"); err != nil {
		t.Fatalf("registering the vote rule: %v", err)
	}
	def, err := workflow.Load([]byte(voteHandlerWorkflow))
	if err != nil {
		t.Fatalf("workflow.Load: %v", err)
	}
	parsed, err := workflow.Canonical(def)
	if err != nil {
		t.Fatalf("workflow.Canonical: %v", err)
	}
	if _, _, err := st.InsertWorkflow(&model.Workflow{
		ProjectID: store.DefaultProjectID,
		Name:      def.Pipeline.Name, Version: def.Pipeline.Version,
		SourceSHA256: workflow.SHA256([]byte(voteHandlerWorkflow)),
		Body:         voteHandlerWorkflow, Parsed: string(parsed),
	}, model.NowMS()); err != nil {
		t.Fatalf("InsertWorkflow: %v", err)
	}
	issueID, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title: "vote subject", Status: model.StatusTodo,
		Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	run, _, err := st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{}, "", issueID)
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	activated, err := engine.ActivateStore(st, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	if err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}
	if activated.ConductorToken == "" {
		t.Fatal("activation minted no conductor capability, want the run bound")
	}

	steps, err := st.ListRunSteps(run.ID)
	if err != nil {
		t.Fatalf("ListRunSteps: %v", err)
	}
	first := 0
	for _, s := range steps {
		if s.Instance == "first@0" {
			first = s.ID
		}
	}
	env := verb.Env{Store: st, Author: "vote-handler-author", Cwd: "/srv/vote-handler"}
	ctx := context.Background()
	claim, err := verb.StepClaim(ctx, env, verb.StepClaimInput{Step: model.FormatStepID(first), Owner: "worker-1"})
	if err != nil {
		t.Fatalf("StepClaim: %v", err)
	}
	if _, err := verb.StepComplete(ctx, env, verb.StepCompleteInput{
		Step: model.FormatStepID(first), Token: claim.Claim.Token, Artifact: []byte("result"),
	}); err != nil {
		t.Fatalf("StepComplete: %v", err)
	}

	proposals, _, err := st.ListProposals(store.DefaultProjectID, "", "", "", 10)
	if err != nil || len(proposals) != 1 {
		t.Fatalf("ListProposals = (%d proposals, %v), want the gate's one", len(proposals), err)
	}
	return model.FormatProposalID(proposals[0].ID), activated.ConductorToken
}

// proposalRow is a proposal's stored CAS version and status.
type proposalRow struct {
	version int
	status  string
}

func readProposalRow(t *testing.T, st store.Store, ref string) proposalRow {
	t.Helper()
	id, err := model.ParseProposalID(ref)
	if err != nil {
		t.Fatalf("ParseProposalID: %v", err)
	}
	conn, err := store.SQLDB(st)
	if err != nil {
		t.Fatalf("SQLDB: %v", err)
	}
	var row proposalRow
	if err := conn.QueryRow(`SELECT version, status FROM proposals WHERE id = ?`, id).Scan(&row.version, &row.status); err != nil {
		t.Fatalf("reading proposal %s: %v", ref, err)
	}
	return row
}

func requireCode(t *testing.T, rpc string, err error, want output.ErrorCode) {
	t.Helper()
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("%s answered unimplemented: *Service does not serve it", rpc)
	}
	if got := output.ErrorCodeFromConnectError(err); got != want {
		t.Fatalf("%s code = %q, want %q (err %v)", rpc, got, want, err)
	}
}

func TestVoteHandlersBindVerbLayer(t *testing.T) {
	ctx := context.Background()
	st := openDocStore(t)
	c := voteClient(t, st)

	issueID, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title: "linked issue", Status: model.StatusBacklog, Priority: model.PriorityNone, Kind: model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("creating the issue: %v", err)
	}
	issueRef := model.FormatID(issueID)

	created, err := c.VoteCreate(ctx, attributed(&docketv1.VoteCreateRequest{
		Description: "ship it", Criticality: "medium", Voters: 1, DomainTags: []string{"api"},
	}))
	mustServe(t, "VoteCreate", err)
	proposal := created.Msg.GetProposal()
	if proposal.GetDescription() != "ship it" || proposal.GetStatus() != "open" || proposal.GetThreshold() != defaultVoteThreshold {
		t.Fatalf("VoteCreate proposal = %+v, want an open proposal at the default threshold", proposal)
	}
	ref := proposal.GetId()

	linked, err := c.VoteLink(ctx, attributed(&docketv1.VoteLinkRequest{Proposal: ref, Issue: issueRef}))
	mustServe(t, "VoteLink", err)
	if linked.Msg.GetProposal() != ref || linked.Msg.GetIssue() != issueRef {
		t.Errorf("VoteLink = %+v, want %s and %s", linked.Msg, ref, issueRef)
	}

	listed, err := c.VoteList(ctx, attributed(&docketv1.VoteListRequest{Limit: 10}))
	mustServe(t, "VoteList", err)
	if items := listed.Msg.GetItems(); len(items) != 1 || items[0].GetId() != ref || listed.Msg.GetTotal() != 1 {
		t.Errorf("VoteList = %+v, want the one open proposal %s", listed.Msg, ref)
	}

	cast, err := c.VoteCast(ctx, attributed(&docketv1.VoteCastRequest{
		Proposal: ref, Voter: "seat", Verdict: "approve", Confidence: 0.9, DomainRelevance: 0.8,
		FindingsJson: `{"blockers":[],"concerns":["watch the cap"],"suggestions":[]}`,
		MetadataJson: `{"model":"m"}`,
	}))
	mustServe(t, "VoteCast", err)
	vote := cast.Msg.GetVote()
	if vote.GetVoterName() != "seat" || vote.GetVerdict() != "approve" || cast.Msg.GetProposal().GetStatus() != "approved" {
		t.Fatalf("VoteCast = %+v, want seat's approving ballot deciding the proposal", cast.Msg)
	}
	if f := vote.GetFindingsJson(); len(f) != 1 || f[0].GetSeverity() != "concern" || f[0].GetTitle() != "watch the cap" {
		t.Errorf("VoteCast findings_json = %+v, want the one concern", f)
	}
	if got := vote.GetMetadataJson(); got != `{"model":"m"}` {
		t.Errorf("VoteCast metadata_json = %q, want the seat's bag", got)
	}

	result, err := c.VoteResult(ctx, attributed(&docketv1.VoteResultRequest{Proposal: ref}))
	mustServe(t, "VoteResult", err)
	if result.Msg.GetProposal() != ref || result.Msg.GetOutcome() != "approved" ||
		result.Msg.GetCast() != 1 || result.Msg.GetRequired() != 1 {
		t.Errorf("VoteResult = %+v, want %s approved by 1 of 1", result.Msg, ref)
	}

	shown, err := c.VoteShow(ctx, attributed(&docketv1.VoteShowRequest{Proposal: ref}))
	mustServe(t, "VoteShow", err)
	if got := shown.Msg.GetProposal(); got.GetId() != ref || len(got.GetVotes()) != 1 ||
		got.GetVotes()[0].GetVerdict() != "approve" || len(got.GetLinkedIssues()) != 1 || got.GetLinkedIssues()[0] != issueRef {
		t.Errorf("VoteShow proposal = %+v, want %s with its vote, linked to %s", got, ref, issueRef)
	}

	backfilled, err := c.VoteBackfillUsage(ctx, attributed(&docketv1.VoteBackfillUsageRequest{
		Proposal: ref, Rows: []*docketv1.UsageRow{{Voter: "seat", Unit: "tokens", Quantity: 1200}},
	}))
	mustServe(t, "VoteBackfillUsage", err)
	if got := backfilled.Msg.GetRecorded(); got != 1 {
		t.Errorf("VoteBackfillUsage recorded = %d, want 1", got)
	}
	_, err = c.VoteBackfillUsage(ctx, attributed(&docketv1.VoteBackfillUsageRequest{
		Proposal: ref, Rows: []*docketv1.UsageRow{{Step: "STEP-1", Voter: "seat", Unit: "tokens", Quantity: 1}},
	}))
	requireCode(t, "VoteBackfillUsage with a step", err, output.ErrValidation)

	committed, err := c.VoteCommit(ctx, attributed(&docketv1.VoteCommitRequest{Proposal: ref, Outcome: "shipped"}))
	mustServe(t, "VoteCommit", err)
	if got := committed.Msg.GetProposal().GetId(); got != ref || readProposalRow(t, st, ref).status != "committed" {
		t.Errorf("VoteCommit proposal = %q, want %s committed", got, ref)
	}

	unlinked, err := c.VoteUnlink(ctx, attributed(&docketv1.VoteUnlinkRequest{Proposal: ref, Issue: issueRef}))
	mustServe(t, "VoteUnlink", err)
	if unlinked.Msg.GetProposal() != ref || unlinked.Msg.GetIssue() != issueRef {
		t.Errorf("VoteUnlink = %+v, want %s and %s", unlinked.Msg, ref, issueRef)
	}

	other, err := c.VoteCreate(ctx, attributed(&docketv1.VoteCreateRequest{
		Description: "abandon it", Criticality: "low", Voters: 2,
	}))
	mustServe(t, "VoteCreate", err)
	closed, err := c.VoteClose(ctx, attributed(&docketv1.VoteCloseRequest{
		Proposal: other.Msg.GetProposal().GetId(), Reason: "superseded",
	}))
	mustServe(t, "VoteClose", err)
	if got := closed.Msg.GetProposal().GetId(); got != other.Msg.GetProposal().GetId() {
		t.Errorf("VoteClose proposal = %q, want %q", got, other.Msg.GetProposal().GetId())
	}

	t.Run("VoteCommit on a bound run's vote step", func(t *testing.T) {
		st := openDocStore(t)
		c := voteClient(t, st)
		ref, capability := voteStepProposal(t, st)
		if _, err := c.VoteCast(ctx, attributed(&docketv1.VoteCastRequest{
			Proposal: ref, Voter: "seat", Verdict: "approve", Confidence: 0.9, DomainRelevance: 0.8,
		})); err != nil {
			t.Fatalf("VoteCast: %v", err)
		}
		approved := readProposalRow(t, st, ref)
		if approved.status != "approved" {
			t.Fatalf("proposal %s is %s, want approved before the commit rows", ref, approved.status)
		}
		wrong, _, err := model.MintToken()
		if err != nil {
			t.Fatalf("MintToken: %v", err)
		}

		for _, row := range []struct {
			name, token string
			code        output.ErrorCode
		}{
			{"a wrong token", wrong, output.ErrAuth},
			{"an empty token", "", output.ErrValidation},
		} {
			_, err := c.VoteCommit(ctx, attributed(&docketv1.VoteCommitRequest{Proposal: ref, Outcome: "o", Token: row.token}))
			requireCode(t, "VoteCommit with "+row.name, err, row.code)
			if got := readProposalRow(t, st, ref); got != approved {
				t.Errorf("after VoteCommit with %s the proposal row is %+v, want %+v unchanged", row.name, got, approved)
			}
		}

		res, err := c.VoteCommit(ctx, attributed(&docketv1.VoteCommitRequest{Proposal: ref, Outcome: "o", Token: capability}))
		mustServe(t, "VoteCommit with the run's capability", err)
		if got := res.Msg.GetProposal().GetId(); got != ref || readProposalRow(t, st, ref).status != "committed" {
			t.Errorf("VoteCommit with the run's capability = %q, want %s committed", got, ref)
		}
	})
}

func TestVoteAuthorOverride(t *testing.T) {
	ctx := context.Background()
	st := openDocStore(t)
	c := voteClient(t, st)
	const header = "doc-handler-author"

	creators := map[string]string{}
	for _, createdBy := range []string{"named-creator", ""} {
		res, err := c.VoteCreate(ctx, attributed(&docketv1.VoteCreateRequest{
			Description: "creator " + createdBy, Criticality: "medium", Voters: 2, CreatedBy: createdBy,
		}))
		mustServe(t, "VoteCreate", err)
		creators[createdBy] = res.Msg.GetProposal().GetId()
	}
	for createdBy, want := range map[string]string{"named-creator": "named-creator", "": header} {
		id, err := model.ParseProposalID(creators[createdBy])
		if err != nil {
			t.Fatalf("ParseProposalID: %v", err)
		}
		p, err := st.GetProposal(id)
		if err != nil {
			t.Fatalf("GetProposal: %v", err)
		}
		if p.CreatedBy != want {
			t.Errorf("created_by %q recorded creator %q, want %q", createdBy, p.CreatedBy, want)
		}
	}

	ref := creators[""]
	for _, voter := range []string{"named-voter", ""} {
		_, err := c.VoteCast(ctx, attributed(&docketv1.VoteCastRequest{
			Proposal: ref, Voter: voter, Verdict: "approve", Confidence: 0.9, DomainRelevance: 0.8,
		}))
		mustServe(t, "VoteCast", err)
	}
	id, err := model.ParseProposalID(ref)
	if err != nil {
		t.Fatalf("ParseProposalID: %v", err)
	}
	votes, err := st.GetProposalVotes(id)
	if err != nil {
		t.Fatalf("GetProposalVotes: %v", err)
	}
	voters := map[string]bool{}
	for _, v := range votes {
		voters[v.VoterName] = true
	}
	if len(votes) != 2 || !voters["named-voter"] || !voters[header] {
		t.Errorf("recorded voters = %v, want named-voter and the Docket-Author header %q", voters, header)
	}
}
