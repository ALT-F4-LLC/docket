package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// stepRulingWorkflow is a tree-holding executor step that parks on its first
// failure, followed by a human gate.
const stepRulingWorkflow = `
[pipeline]
name = "api-step"
version = 1
[match]
kind = ["task"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
max_attempts = 1
on_fail = "waiting-human"
[[step]]
name = "second"
after = ["first"]
type = "human"
on_fail = "skip"
`

// stepFixture is a store holding stepRulingWorkflow, a git checkout every
// run executes in, and a client of a Service over both.
type stepFixture struct {
	st       store.Store
	c        docketv1connect.DocketServiceClient
	execRoot string
}

func newStepFixture(t *testing.T) *stepFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	// Activation reads config beside the store; an empty one keeps the
	// operator's own config out of the run.
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	st := openProjectStore(t)
	def, err := workflow.Load([]byte(stepRulingWorkflow))
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
		SourceSHA256: workflow.SHA256([]byte(stepRulingWorkflow)),
		Body:         stepRulingWorkflow, Parsed: string(parsed),
	}, model.NowMS()); err != nil {
		t.Fatalf("InsertWorkflow: %v", err)
	}

	f := &stepFixture{st: st, execRoot: t.TempDir()}
	f.git(t, "init", "-q")
	f.commit(t, "base\n", "base")
	server := serve(t, NewService(st, "test"))
	f.c = client.NewClient(server.Client(), server.URL, discardLogger)
	return f
}

func (f *stepFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=api-test", "-c", "user.email=api-test@example.invalid", "-c", "commit.gpgsign=false",
	}, args...)...)
	cmd.Dir = f.execRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit writes body to the checkout's work file, commits it, and returns the
// commit id.
func (f *stepFixture) commit(t *testing.T, body, message string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.execRoot, "work.txt"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the work file: %v", err)
	}
	f.git(t, "add", "-A")
	f.git(t, "commit", "-q", "-m", message)
	return f.git(t, "rev-parse", "HEAD")
}

// boundRun activates a run of stepRulingWorkflow in the checkout and returns
// its step ids by instance and the conductor capability it is bound to.
func (f *stepFixture) boundRun(t *testing.T) (map[string]int, string) {
	t.Helper()
	issue, _, err := f.st.CreateIssueIdempotent(&model.Issue{
		Title: "step subject", Status: model.StatusTodo, Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	run, _, err := f.st.InsertRunWithContextIdempotent(
		store.DefaultProjectID, "", 0, model.NowMS(), store.RunContext{ExecRoot: f.execRoot}, "", issue)
	if err != nil {
		t.Fatalf("InsertRunWithContextIdempotent: %v", err)
	}
	activated, err := engine.ActivateStore(f.st, run.ID, engine.ActivateOptions{NowMS: model.NowMS()})
	if err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}
	if activated.ConductorToken == "" {
		t.Fatal("premise: activation minted no conductor capability")
	}
	steps, err := f.st.ListRunSteps(run.ID)
	if err != nil {
		t.Fatalf("ListRunSteps: %v", err)
	}
	ids := make(map[string]int, len(steps))
	for _, s := range steps {
		ids[s.Instance] = s.ID
	}
	return ids, activated.ConductorToken
}

func (f *stepFixture) claim(t *testing.T, id int) string {
	t.Helper()
	res, err := f.c.StepClaim(context.Background(), connect.NewRequest(&docketv1.StepClaimRequest{
		Step: model.FormatStepID(id), Owner: "worker", RecoverySecret: "secret",
	}))
	requireServed(t, "StepClaim", err)
	if res.Msg.GetToken() == "" || res.Msg.GetStep() != model.FormatStepID(id) {
		t.Fatalf("StepClaim = %v, want a token on %s", res.Msg, model.FormatStepID(id))
	}
	return res.Msg.GetToken()
}

// complete records first@0 of a run from the checkout, so the step finishes
// with an issue.diff recording the checkout's HEAD.
func (f *stepFixture) complete(t *testing.T, id int, token string) *docketv1.StepCompleteResponse {
	t.Helper()
	res, err := f.c.StepComplete(context.Background(), connect.NewRequest(&docketv1.StepCompleteRequest{
		Step: model.FormatStepID(id), Token: token, Artifact: "result", Worktree: f.execRoot,
	}))
	requireServed(t, "StepComplete", err)
	return res.Msg
}

// rowState is what a refused call must leave unchanged.
type rowState struct {
	status  string
	version int
}

func (f *stepFixture) state(t *testing.T, id int) rowState {
	t.Helper()
	step, err := f.st.GetStep(id)
	if err != nil {
		t.Fatalf("GetStep(%d): %v", id, err)
	}
	return rowState{status: step.Status, version: step.RowVersion}
}

// requireRefusal fails unless err carries both the Connect code and the
// docket code.
func requireRefusal(t *testing.T, label string, err error, code connect.Code, docket output.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted, want %v (%s)", label, code, docket)
	}
	if got := connect.CodeOf(err); got != code {
		t.Errorf("%s: connect code = %v, want %v (err: %v)", label, got, code, err)
	}
	if got := output.ErrorCodeFromConnectError(err); got != docket {
		t.Errorf("%s: docket code = %s, want %s", label, got, docket)
	}
}

// tokenTarget is the step a token RPC acts on, the token that should
// succeed there, and the commit an integration annotation names.
type tokenTarget struct {
	id      int
	correct string
	sha     string
}

// tokenRPC is one token-carrying RPC: setup readies its step in a fresh bound
// run, and call sends the RPC with token.
type tokenRPC struct {
	name  string
	setup func(t *testing.T, f *stepFixture) tokenTarget
	call  func(f *stepFixture, target tokenTarget, token string) error
	// want is the step status the correct-token call leaves.
	want string
}

// leased readies a step holding a live lease; the correct token is the lease
// token, so any other token is not the holder's.
func leased(t *testing.T, f *stepFixture) tokenTarget {
	steps, _ := f.boundRun(t)
	id := steps["first@0"]
	return tokenTarget{id: id, correct: f.claim(t, id)}
}

// readyGate finishes first@0 so its human gate second@0 is ready.
func readyGate(t *testing.T, f *stepFixture) tokenTarget {
	steps, conductor := f.boundRun(t)
	f.complete(t, steps["first@0"], f.claim(t, steps["first@0"]))
	return tokenTarget{id: steps["second@0"], correct: conductor}
}

var tokenRPCs = []tokenRPC{
	{
		name: "StepHeartbeat", setup: leased, want: store.StepClaimed,
		call: func(f *stepFixture, target tokenTarget, token string) error {
			_, err := f.c.StepHeartbeat(context.Background(), connect.NewRequest(&docketv1.StepHeartbeatRequest{
				Step: model.FormatStepID(target.id), Token: token,
			}))
			return err
		},
	},
	{
		name: "StepComplete", setup: leased, want: store.StepDone,
		call: func(f *stepFixture, target tokenTarget, token string) error {
			_, err := f.c.StepComplete(context.Background(), connect.NewRequest(&docketv1.StepCompleteRequest{
				Step: model.FormatStepID(target.id), Token: token, Artifact: "result", Worktree: f.execRoot,
			}))
			return err
		},
	},
	{
		name: "StepFail", setup: leased, want: store.StepWaitingHuman,
		call: func(f *stepFixture, target tokenTarget, token string) error {
			_, err := f.c.StepFail(context.Background(), connect.NewRequest(&docketv1.StepFailRequest{
				Step: model.FormatStepID(target.id), Token: token, Note: "gave up",
			}))
			return err
		},
	},
	{
		name: "StepHold", want: store.StepWaitingHuman,
		setup: func(t *testing.T, f *stepFixture) tokenTarget {
			steps, conductor := f.boundRun(t)
			return tokenTarget{id: steps["first@0"], correct: conductor}
		},
		call: func(f *stepFixture, target tokenTarget, token string) error {
			_, err := f.c.StepHold(context.Background(), connect.NewRequest(&docketv1.StepHoldRequest{
				Step: model.FormatStepID(target.id), Reason: "for the operator", Token: token,
			}))
			return err
		},
	},
	{
		name: "StepReap", want: store.StepPending,
		setup: func(t *testing.T, f *stepFixture) tokenTarget {
			steps, conductor := f.boundRun(t)
			f.claim(t, steps["first@0"])
			return tokenTarget{id: steps["first@0"], correct: conductor}
		},
		call: func(f *stepFixture, target tokenTarget, token string) error {
			_, err := f.c.StepReap(context.Background(), connect.NewRequest(&docketv1.StepReapRequest{
				Step: model.FormatStepID(target.id), Reason: "the holder died", Token: token,
			}))
			return err
		},
	},
	{
		name: "StepApprove", setup: readyGate, want: store.StepDone,
		call: func(f *stepFixture, target tokenTarget, token string) error {
			_, err := f.c.StepApprove(context.Background(), connect.NewRequest(&docketv1.StepApproveRequest{
				Step: model.FormatStepID(target.id), Note: "looks right", Authority: engine.AuthorityOperator, Token: token,
			}))
			return err
		},
	},
	{
		name: "StepReject", setup: readyGate, want: store.StepSkipped,
		call: func(f *stepFixture, target tokenTarget, token string) error {
			_, err := f.c.StepReject(context.Background(), connect.NewRequest(&docketv1.StepRejectRequest{
				Step: model.FormatStepID(target.id), Note: "not yet", Authority: engine.AuthorityOperator, Token: token,
			}))
			return err
		},
	},
	{
		name: "StepResolve", want: store.StepSkipped,
		setup: func(t *testing.T, f *stepFixture) tokenTarget {
			steps, conductor := f.boundRun(t)
			id := steps["first@0"]
			_, err := f.c.StepFail(context.Background(), connect.NewRequest(&docketv1.StepFailRequest{
				Step: model.FormatStepID(id), Token: f.claim(t, id), Note: "gave up",
			}))
			requireServed(t, "StepFail", err)
			return tokenTarget{id: id, correct: conductor}
		},
		call: func(f *stepFixture, target tokenTarget, token string) error {
			_, err := f.c.StepResolve(context.Background(), connect.NewRequest(&docketv1.StepResolveRequest{
				Step: model.FormatStepID(target.id), As: engine.ResolveSkip, Authority: engine.AuthorityOperator, Token: token,
			}))
			return err
		},
	},
	{
		name: "StepAnnotate", want: store.StepDone,
		setup: func(t *testing.T, f *stepFixture) tokenTarget {
			steps, conductor := f.boundRun(t)
			id := steps["first@0"]
			token := f.claim(t, id)
			sha := f.commit(t, "the work of "+model.FormatStepID(id)+"\n", "implement")
			if got := f.complete(t, id, token).GetStep().GetStatus(); got != store.StepDone {
				t.Fatalf("premise: first@0 recorded %q, want %q", got, store.StepDone)
			}
			return tokenTarget{id: id, correct: conductor, sha: sha}
		},
		call: func(f *stepFixture, target tokenTarget, token string) error {
			_, err := f.c.StepAnnotate(context.Background(), connect.NewRequest(&docketv1.StepAnnotateRequest{
				Step: model.FormatStepID(target.id), IntegratedSha: target.sha, Token: token,
			}))
			return err
		},
	},
}

// TestStepHandlersBindVerbLayer calls every step RPC over Connect, then runs
// the threat model's token rows against each conductor-token and lease RPC on
// a bound run.
func TestStepHandlersBindVerbLayer(t *testing.T) {
	f := newStepFixture(t)
	ctx := context.Background()
	steps, _ := f.boundRun(t)
	ref := model.FormatStepID(steps["first@0"])

	shown, err := f.c.StepShow(ctx, connect.NewRequest(&docketv1.StepShowRequest{Steps: []string{ref}}))
	requireServed(t, "StepShow", err)
	if got := shown.Msg.GetSteps(); len(got) != 1 || got[0].GetRow().GetStep() != ref || got[0].GetVersion() < 1 {
		t.Fatalf("StepShow steps = %v, want %s with its row version", got, ref)
	}

	bundle, err := f.c.StepContext(ctx, connect.NewRequest(&docketv1.StepContextRequest{Step: ref}))
	requireServed(t, "StepContext", err)
	if got := bundle.Msg.GetContext().GetStep().GetStep(); got != ref {
		t.Errorf("StepContext bundle step = %q, want %q", got, ref)
	}

	rendered, err := f.c.StepRender(ctx, connect.NewRequest(&docketv1.StepRenderRequest{Step: ref}))
	requireServed(t, "StepRender", err)
	if !strings.Contains(rendered.Msg.GetPacket(), ref) {
		t.Errorf("StepRender packet does not name %s:\n%s", ref, rendered.Msg.GetPacket())
	}

	run := shown.Msg.GetSteps()[0].GetRow().GetRun()
	listed, err := f.c.StepList(ctx, connect.NewRequest(&docketv1.StepListRequest{Run: run}))
	requireServed(t, "StepList", err)
	if got := listed.Msg.GetItems(); len(got) != 2 || listed.Msg.GetTotal() != 2 {
		t.Errorf("StepList items = %v, want the run's two steps", got)
	}

	token := f.claim(t, steps["first@0"])
	beat, err := f.c.StepHeartbeat(ctx, connect.NewRequest(&docketv1.StepHeartbeatRequest{Step: ref, Token: token}))
	requireServed(t, "StepHeartbeat", err)
	if beat.Msg.GetStep() != ref || beat.Msg.GetLeaseExpiresMs() <= model.NowMS() {
		t.Errorf("StepHeartbeat = %v, want %s's lease in the future", beat.Msg, ref)
	}
	if got := f.complete(t, steps["first@0"], token).GetStep().GetStatus(); got != store.StepDone {
		t.Errorf("StepComplete step status = %q, want %q", got, store.StepDone)
	}

	artifacts, err := f.c.StepArtifacts(ctx, connect.NewRequest(&docketv1.StepArtifactsRequest{Step: ref}))
	requireServed(t, "StepArtifacts", err)
	var result string
	for _, a := range artifacts.Msg.GetItems() {
		if a.GetKind() == "result" {
			result = a.GetId()
		}
	}
	if result == "" {
		t.Fatalf("StepArtifacts items = %v, want the result artifact", artifacts.Msg.GetItems())
	}
	artifact, err := f.c.StepArtifact(ctx, connect.NewRequest(&docketv1.StepArtifactRequest{Artifact: result}))
	requireServed(t, "StepArtifact", err)
	if got := artifact.Msg.GetArtifact(); got.GetId() != result || got.GetBody() != "result" {
		t.Errorf("StepArtifact = %v, want %s with its body", got, result)
	}

	gates, err := f.c.StepGates(ctx, connect.NewRequest(&docketv1.StepGatesRequest{Step: ref}))
	requireServed(t, "StepGates", err)
	if gates.Msg.GetTotal() != int32(len(gates.Msg.GetItems())) {
		t.Errorf("StepGates total = %d over %d items", gates.Msg.GetTotal(), len(gates.Msg.GetItems()))
	}

	for _, rpc := range tokenRPCs {
		t.Run(rpc.name, func(t *testing.T) {
			target := rpc.setup(t, f)
			before := f.state(t, target.id)

			err := rpc.call(f, target, "not-the-token")
			requireRefusal(t, rpc.name+" with a wrong token", err, connect.CodePermissionDenied, output.ErrAuth)
			if after := f.state(t, target.id); after != before {
				t.Errorf("%s with a wrong token changed the row %+v -> %+v", rpc.name, before, after)
			}

			err = rpc.call(f, target, "")
			requireRefusal(t, rpc.name+" with no token", err, connect.CodeInvalidArgument, output.ErrValidation)
			if after := f.state(t, target.id); after != before {
				t.Errorf("%s with no token changed the row %+v -> %+v", rpc.name, before, after)
			}

			requireServed(t, rpc.name+" with the correct token", rpc.call(f, target, target.correct))
			if after := f.state(t, target.id); after.status != rpc.want {
				t.Errorf("%s with the correct token left the step %s, want %s", rpc.name, after.status, rpc.want)
			}
		})
	}
}
