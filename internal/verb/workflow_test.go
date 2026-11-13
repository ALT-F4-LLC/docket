package verb_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

const unitWorkflow = `
[pipeline]
name = "unit"
version = 1
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
`

func insertWorkflow(t *testing.T, st store.Store, projectID int, src string) {
	t.Helper()
	def, err := workflow.Load([]byte(src))
	if err != nil {
		t.Fatalf("workflow.Load: %v", err)
	}
	_, _, err = st.InsertWorkflow(&model.Workflow{
		ProjectID:    projectID,
		Name:         def.Pipeline.Name,
		Version:      def.Pipeline.Version,
		SourceSHA256: workflow.SHA256([]byte(src)),
		Body:         src,
		Parsed:       "{}",
	}, 1)
	if err != nil {
		t.Fatalf("InsertWorkflow: %v", err)
	}
}

func workflowRefs(wfs []*model.Workflow) []string {
	refs := make([]string, 0, len(wfs))
	for _, wf := range wfs {
		refs = append(refs, wf.Ref())
	}
	return refs
}

func TestWorkflowListReadsTheCallersProject(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertWorkflow(t, st, caller, unitWorkflow)

	got, err := verb.WorkflowList(context.Background(),
		projectEnv(st, "/repos/caller", true), verb.WorkflowListInput{Limit: 50})
	if err != nil {
		t.Fatalf("WorkflowList: %v", err)
	}
	if got.Total != 1 || strings.Join(workflowRefs(got.Workflows), ",") != "unit@1" {
		t.Errorf("listed %v (total %d), want [unit@1]", workflowRefs(got.Workflows), got.Total)
	}
}

// A list is a read: a stranger sees none of the caller's rows and is not
// registered by looking.
func TestWorkflowListNeverRegistersTheCaller(t *testing.T) {
	st := openStore(t)
	insertWorkflow(t, st, callerProject(t, st), unitWorkflow)

	got, err := verb.WorkflowList(context.Background(),
		projectEnv(st, "/repos/stranger", true), verb.WorkflowListInput{Limit: 50})
	if err != nil {
		t.Fatalf("WorkflowList: %v", err)
	}
	if got.Total != 0 {
		t.Errorf("a stranger listed %v", workflowRefs(got.Workflows))
	}
	requireUnregistered(t, st, "/repos/stranger")
}

// The Project ref reads another project's registry, by any key
// ResolveProjectRef accepts, and an unknown ref is NOT_FOUND.
func TestWorkflowListProjectRefReadsAnotherRegistry(t *testing.T) {
	st := openStore(t)
	insertWorkflow(t, st, callerProject(t, st), unitWorkflow)
	env := projectEnv(st, "/repos/default", true)

	got, err := verb.WorkflowList(context.Background(), env,
		verb.WorkflowListInput{Limit: 50, Project: "caller"})
	if err != nil {
		t.Fatalf("WorkflowList --project caller: %v", err)
	}
	if got.Total != 1 {
		t.Errorf("--project caller listed %v, want [unit@1]", workflowRefs(got.Workflows))
	}

	_, err = verb.WorkflowList(context.Background(), env,
		verb.WorkflowListInput{Limit: 50, Project: "no-such-project"})
	requireVerbCode(t, err, output.ErrNotFound, "no project named")
}

func TestWorkflowShowReadsTheCallersProject(t *testing.T) {
	st := openStore(t)
	insertWorkflow(t, st, callerProject(t, st), unitWorkflow)
	env := projectEnv(st, "/repos/caller", true)

	got, err := verb.WorkflowShow(context.Background(), env, verb.WorkflowShowInput{Ref: "unit"})
	if err != nil {
		t.Fatalf("WorkflowShow: %v", err)
	}
	if got.Workflow.Ref() != "unit@1" {
		t.Errorf("showed %s, want unit@1", got.Workflow.Ref())
	}
	// No source path was recorded, so the verdict is computed and says so.
	if got.Workflow.SourceStatus == nil ||
		got.Workflow.SourceStatus.State != model.WorkflowSourceUnchecked {
		t.Errorf("source status = %+v, want unchecked", got.Workflow.SourceStatus)
	}

	// --source asks for the bytes only, so no verdict is computed.
	got, err = verb.WorkflowShow(context.Background(), env,
		verb.WorkflowShowInput{Ref: "unit@1", Source: true})
	if err != nil {
		t.Fatalf("WorkflowShow --source: %v", err)
	}
	if got.Workflow.SourceStatus != nil || got.Workflow.Body != unitWorkflow {
		t.Errorf("--source returned status %+v and body %q", got.Workflow.SourceStatus, got.Workflow.Body)
	}
}

func TestWorkflowShowRefusals(t *testing.T) {
	st := openStore(t)
	insertWorkflow(t, st, callerProject(t, st), unitWorkflow)

	_, err := verb.WorkflowShow(context.Background(),
		projectEnv(st, "/repos/stranger", true), verb.WorkflowShowInput{Ref: "unit"})
	requireVerbCode(t, err, output.ErrNotFound, "workflow not found: unit is not registered")
	requireUnregistered(t, st, "/repos/stranger")

	_, err = verb.WorkflowShow(context.Background(),
		projectEnv(st, "/repos/caller", true), verb.WorkflowShowInput{Ref: "unit@x"})
	requireVerbCode(t, err, output.ErrValidation, "")
}

func TestWorkflowLintJudgesTheCallersRegistry(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	env := projectEnv(st, "/repos/caller", true)
	lint := func(src string) (verb.WorkflowLintResult, error) {
		return verb.WorkflowLint(context.Background(), env,
			verb.WorkflowLintInput{Body: []byte(src), SourcePath: "wf.toml"})
	}

	got, err := lint(unitWorkflow)
	if err != nil {
		t.Fatalf("lint of a free slot: %v", err)
	}
	if got.Registration != verb.WorkflowLintNew || got.Name != "unit" || got.Version != 1 ||
		got.SHA256 != workflow.SHA256([]byte(unitWorkflow)) {
		t.Errorf("free-slot verdict = %+v, want unit@1 new", got)
	}

	insertWorkflow(t, st, caller, unitWorkflow)
	if got, err = lint(unitWorkflow); err != nil || got.Registration != verb.WorkflowLintUnchanged {
		t.Errorf("lint of the registered bytes = %+v, %v; want unchanged", got, err)
	}

	_, err = lint(unitWorkflow + "\n# edited\n")
	requireVerbCode(t, err, output.ErrConflict, "unit@1 is registered with different bytes")

	_, err = lint(strings.Replace(unitWorkflow, `name = "first"`, `name = ""`, 1))
	requireVerbCode(t, err, output.ErrValidation, "wf.toml: ")
}

// --all-projects judges every project on its own registry and reports each
// verdict, a refusal included, without failing the call.
func TestWorkflowLintAllProjectsReportsEachVerdict(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertWorkflow(t, st, caller, unitWorkflow+"\n# other bytes\n")

	got, err := verb.WorkflowLint(context.Background(), projectEnv(st, "/repos/caller", true),
		verb.WorkflowLintInput{Body: []byte(unitWorkflow), AllProjects: true})
	if err != nil {
		t.Fatalf("WorkflowLint --all-projects: %v", err)
	}
	if len(got.Projects) != 2 {
		t.Fatalf("judged %d projects, want 2", len(got.Projects))
	}
	for _, v := range got.Projects {
		switch v.Project.ID {
		case caller:
			requireVerbCode(t, v.Err, output.ErrConflict, "unit@1 is registered with different bytes")
		case store.DefaultProjectID:
			if v.Err != nil || v.Registration != verb.WorkflowLintNew {
				t.Errorf("default project verdict = %q, %v; want new", v.Registration, v.Err)
			}
		default:
			t.Errorf("judged unexpected project %d", v.Project.ID)
		}
	}
}
