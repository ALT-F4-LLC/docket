package api

import (
	"context"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/api/client"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

const workflowCallerIdentity = "/repos/workflow-caller"

const unitWorkflowBody = `
[pipeline]
name = "unit"
version = 1
description = "one step"
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
`

// TestWorkflowHandlersBindVerbLayer calls every workflow RPC against a
// registered project that is not the default one, so a handler that dropped
// the Docket-Project header would read or write somewhere the test does not
// look.
func TestWorkflowHandlersBindVerbLayer(t *testing.T) {
	ctx := context.Background()
	st := openProjectStore(t)
	if _, err := st.EnsureProject("/repos/default", "default", 1); err != nil {
		t.Fatalf("claiming the default project: %v", err)
	}
	projectID, err := st.EnsureProject(workflowCallerIdentity, "workflow-caller", 1)
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	server := serve(t, NewService(st, "dev"))
	rpc := client.NewClient(server.Client(), server.URL, discardLogger)

	registered, err := rpc.WorkflowRegister(ctx, inProject(&docketv1.WorkflowRegisterRequest{
		Body: unitWorkflowBody, SourcePath: "/defs/unit.toml",
	}, workflowCallerIdentity))
	requireServed(t, "WorkflowRegister", err)
	got := registered.Msg.GetWorkflow()
	if got.GetName() != "unit" || got.GetVersion() != 1 || got.GetBody() != unitWorkflowBody ||
		got.GetSourcePath() != "/defs/unit.toml" || got.GetDescription() != "one step" {
		t.Errorf("WorkflowRegister workflow = %v, want unit@1 with the sent body, source_path and description", got)
	}
	stored, err := st.GetWorkflow(projectID, "unit", 1)
	if err != nil {
		t.Fatalf("unit@1 is not in the Docket-Project project: %v", err)
	}
	if got.GetSourceSha256() == "" || got.GetSourceSha256() != stored.SourceSHA256 {
		t.Errorf("source_sha256 = %q, want the stored %q", got.GetSourceSha256(), stored.SourceSHA256)
	}

	listed, err := rpc.WorkflowList(ctx, inProject(&docketv1.WorkflowListRequest{Name: "unit"}, workflowCallerIdentity))
	requireServed(t, "WorkflowList", err)
	if items := listed.Msg.GetItems(); len(items) != 1 || items[0].GetName() != "unit" ||
		listed.Msg.GetTotal() != 1 || listed.Msg.GetTruncated() {
		t.Errorf("WorkflowList = %v, want unit@1 alone, total 1, not truncated", listed.Msg)
	}

	shown, err := rpc.WorkflowShow(ctx, inProject(&docketv1.WorkflowShowRequest{Name: "unit", Source: true}, workflowCallerIdentity))
	requireServed(t, "WorkflowShow", err)
	if w := shown.Msg.GetWorkflow(); w.GetVersion() != 1 || w.GetBody() != unitWorkflowBody || w.GetDeprecated() {
		t.Errorf("WorkflowShow unit = %v, want version 1 in service with the registered body", w)
	}

	linted, err := rpc.WorkflowLint(ctx, inProject(&docketv1.WorkflowLintRequest{
		Body: unitWorkflowBody, SourcePath: "/defs/unit.toml",
	}, workflowCallerIdentity))
	requireServed(t, "WorkflowLint", err)
	if !linted.Msg.GetOk() || len(linted.Msg.GetFindings()) != 0 {
		t.Errorf("WorkflowLint of the registered bytes = %v, want ok with no findings", linted.Msg)
	}

	// Edited bytes at the registered version conflict in the caller's
	// project and are new in the default one: the sweep reports the one
	// refusal as a finding naming that project, and is not ok.
	swept, err := rpc.WorkflowLint(ctx, inProject(&docketv1.WorkflowLintRequest{
		Body: unitWorkflowBody + "\n# edited\n", AllProjects: true,
	}, workflowCallerIdentity))
	requireServed(t, "WorkflowLint all_projects", err)
	findings := swept.Msg.GetFindings()
	if swept.Msg.GetOk() || len(findings) != 1 || findings[0].GetLocation() != workflowCallerIdentity ||
		findings[0].GetSeverity() != "error" || findings[0].GetMessage() == "" {
		t.Errorf("WorkflowLint all_projects = %v, want not ok with one error finding located at %s", swept.Msg, workflowCallerIdentity)
	}

	deprecated, err := rpc.WorkflowDeprecate(ctx, inProject(&docketv1.WorkflowDeprecateRequest{Name: "unit", Version: 1}, workflowCallerIdentity))
	requireServed(t, "WorkflowDeprecate", err)
	if !deprecated.Msg.GetWorkflow().GetDeprecated() {
		t.Errorf("WorkflowDeprecate workflow = %v, want it retired", deprecated.Msg.GetWorkflow())
	}

	version := int32(1)
	_, err = rpc.WorkflowShow(ctx, inProject(&docketv1.WorkflowShowRequest{Name: "missing", Version: &version}, workflowCallerIdentity))
	if got := output.ErrorCodeFromConnectError(err); got != output.ErrNotFound {
		t.Errorf("WorkflowShow missing@1 code = %q, want %q: the verb's error code reaches the client (err %v)", got, output.ErrNotFound, err)
	}
}
