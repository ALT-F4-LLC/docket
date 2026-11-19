package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

const policySeatTOML = `
[policy]
version = 2

[variants]
guarded = { model = "sonnet", effort = "medium", escalate_to = "tier-a" }
tier-a = { model = "opus", effort = "high" }
quick = { model = "haiku", effort = "low" }

[executors]
someone = { variant = "guarded" }
seat-a = { variant = "tier-a" }

[sizes]
small = "quick"

[security]
ceiling = "guarded"
labels = ["sensitive"]
`

const policyWorkflow = `
[pipeline]
name = "policy-handler"
version = 1
[match]
kind = ["task"]
[[step]]
name = "only"
after = []
executor = "someone"
emits = "result"
`

// policyRun activates a one-step run in the default project of st with
// policySeatTOML in the instance config root, so activation pins it. The temp
// dirs are taken before t.Setenv, since t.TempDir reads TMPDIR.
func policyRun(t *testing.T, st store.Store) string {
	t.Helper()
	configDir := filepath.Join(t.TempDir(), ".docket", "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("creating the config directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "policy.toml"), []byte(policySeatTOML), 0o644); err != nil {
		t.Fatalf("writing policy.toml: %v", err)
	}
	t.Setenv("DOCKET_PATH", filepath.Dir(configDir))

	def, err := workflow.Load([]byte(policyWorkflow))
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
		SourceSHA256: workflow.SHA256([]byte(policyWorkflow)),
		Body:         policyWorkflow, Parsed: string(parsed),
	}, model.NowMS()); err != nil {
		t.Fatalf("InsertWorkflow: %v", err)
	}
	issueID, _, err := st.CreateIssueIdempotent(&model.Issue{
		Title: "policy subject", Status: model.StatusTodo,
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
	if _, err := engine.ActivateStore(st, run.ID, engine.ActivateOptions{NowMS: model.NowMS()}); err != nil {
		t.Fatalf("ActivateStore: %v", err)
	}
	return model.FormatRunID(run.ID)
}

func TestPolicyHandlersBindVerbLayer(t *testing.T) {
	ctx := context.Background()
	st := openProjectStore(t)
	run := policyRun(t, st)
	server := serve(t, NewService(st, "test"))
	rpc := client.NewClient(server.Client(), server.URL, discardLogger)

	resolved, err := rpc.PolicyResolve(ctx, connect.NewRequest(&docketv1.PolicyResolveRequest{
		Run: run, Seats: []string{"seat-a", "someone"},
	}))
	requireServed(t, "PolicyResolve", err)
	want := []*docketv1.VoterAssignment{
		{Voter: "seat-a", Model: "opus", Effort: "high", Variant: "tier-a"},
		{Voter: "someone", Model: "sonnet", Effort: "medium", Variant: "guarded"},
	}
	requireAssignments(t, "PolicyResolve", resolved.Msg.GetSeats(), want)

	clamped, err := rpc.PolicyResolve(ctx, connect.NewRequest(&docketv1.PolicyResolveRequest{
		Run: run, Seats: []string{"seat-a"}, Label: []string{"sensitive"},
	}))
	requireServed(t, "PolicyResolve with a sensitive label", err)
	requireAssignments(t, "PolicyResolve with a sensitive label", clamped.Msg.GetSeats(), []*docketv1.VoterAssignment{
		{Voter: "seat-a", Model: "sonnet", Effort: "medium", Variant: "guarded"},
	})

	sized, err := rpc.PolicyResolve(ctx, connect.NewRequest(&docketv1.PolicyResolveRequest{
		Run: run, Seats: []string{"someone"}, Size: "small",
	}))
	requireServed(t, "PolicyResolve with a size", err)
	requireAssignments(t, "PolicyResolve with a size", sized.Msg.GetSeats(), []*docketv1.VoterAssignment{
		{Voter: "someone", Model: "haiku", Effort: "low", Variant: "quick"},
	})

	_, err = rpc.PolicyResolve(ctx, connect.NewRequest(&docketv1.PolicyResolveRequest{
		Run: run, Seats: []string{"no-such-seat"},
	}))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrNotFound {
		t.Errorf("PolicyResolve unknown seat code = %q, want %q: the verb's error code reaches the client (err %v)", got, output.ErrNotFound, err)
	}
}

func requireAssignments(t *testing.T, rpc string, got, want []*docketv1.VoterAssignment) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s seats = %v, want %v", rpc, got, want)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("%s seat %d = %v, want %v", rpc, i, got[i], want[i])
		}
	}
}
