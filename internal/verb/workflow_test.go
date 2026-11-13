package verb_test

import (
	"context"
	"errors"
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

func sourceOf(src string) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(src), nil }
}

// A register writes into the caller's project: an anchored identity is
// registered on first contact, the same bytes again change nothing, and other
// bytes at the same name@version conflict.
func TestWorkflowRegisterWritesTheCallersProject(t *testing.T) {
	st := openStore(t)
	callerProject(t, st)
	env := projectEnv(st, "/repos/newcomer", true)
	register := func(src string) (verb.WorkflowRegisterResult, error) {
		return verb.WorkflowRegister(context.Background(), env,
			verb.WorkflowRegisterInput{Source: sourceOf(src), SourcePath: "/defs/unit.toml"})
	}

	got, err := register(unitWorkflow)
	if err != nil {
		t.Fatalf("WorkflowRegister: %v", err)
	}
	if got.Registered == nil || got.Registered.Verb != "workflow register" {
		t.Fatalf("Registered = %+v, want the newcomer's registration by workflow register", got.Registered)
	}
	newcomer := got.Registered.ID
	if !got.Created || got.Targets != nil || got.Workflow.Ref() != "unit@1" ||
		got.Workflow.ProjectID != newcomer || got.Workflow.SourcePath != "/defs/unit.toml" {
		t.Errorf("first register = %+v, want unit@1 created in project %d", got, newcomer)
	}

	if got, err = register(unitWorkflow); err != nil || got.Created || got.Registered != nil {
		t.Errorf("re-register = %+v, %v; want unchanged and no registration", got, err)
	}

	_, err = register(unitWorkflow + "\n# edited\n")
	requireVerbCode(t, err, output.ErrConflict, "")

	_, err = register(strings.Replace(unitWorkflow, `name = "first"`, `name = ""`, 1))
	requireVerbCode(t, err, output.ErrValidation, "/defs/unit.toml: ")
}

// An unanchored caller is refused before the source is read, and nothing is
// registered.
func TestWorkflowRegisterRefusesAnUnanchoredCaller(t *testing.T) {
	st := openStore(t)
	_, err := verb.WorkflowRegister(context.Background(), projectEnv(st, "/work/scratch", false),
		verb.WorkflowRegisterInput{Source: func() ([]byte, error) {
			t.Fatal("the source was read for a refused caller")
			return nil, nil
		}})
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	requireUnregistered(t, st, "/work/scratch")
}

// --all-projects registers into every project and reports each outcome, a
// conflict included, without failing the call; the caller's own project is
// written only as one of the targets.
func TestWorkflowRegisterAllProjectsReportsEachOutcome(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertWorkflow(t, st, caller, unitWorkflow+"\n# other bytes\n")

	got, err := verb.WorkflowRegister(context.Background(), projectEnv(st, "/repos/caller", true),
		verb.WorkflowRegisterInput{Source: sourceOf(unitWorkflow), AllProjects: true})
	if err != nil {
		t.Fatalf("WorkflowRegister --all-projects: %v", err)
	}
	if got.Name != "unit" || got.Version != 1 || got.Workflow != nil || len(got.Targets) != 2 {
		t.Fatalf("result = %+v, want unit@1 with two targets and no single row", got)
	}
	for _, target := range got.Targets {
		switch target.Project.ID {
		case caller:
			requireVerbCode(t, target.Err, output.ErrConflict, "")
		case store.DefaultProjectID:
			if target.Err != nil || !target.Changed || target.Workflow.Ref() != "unit@1" {
				t.Errorf("default project outcome = %+v, want unit@1 registered", target)
			}
		default:
			t.Errorf("registered into unexpected project %d", target.Project.ID)
		}
	}
}

func TestWorkflowRegisterTargetRefusals(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	env := projectEnv(st, "/repos/caller", true)

	_, err := verb.WorkflowRegister(context.Background(), env, verb.WorkflowRegisterInput{
		Source: sourceOf(unitWorkflow), Project: "caller", AllProjects: true})
	requireVerbCode(t, err, output.ErrValidation, "--project and --all-projects")

	_, err = verb.WorkflowRegister(context.Background(), env, verb.WorkflowRegisterInput{
		Source: sourceOf(unitWorkflow), Project: "no-such-repo"})
	requireVerbCode(t, err, output.ErrNotFound, "")
	for _, id := range []int{store.DefaultProjectID, caller} {
		if _, err := st.GetWorkflow(id, "unit", 1); !errors.Is(err, store.ErrWorkflowNotFound) {
			t.Errorf("a refused target still wrote unit@1 to project %d (%v)", id, err)
		}
	}
}

// Deprecation is version-scoped and refuses a second retirement; restore
// returns the version to binding.
func TestWorkflowDeprecateRetiresAndRestoresInTheCallersProject(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	insertWorkflow(t, st, caller, unitWorkflow)
	env := projectEnv(st, "/repos/caller", true)
	deprecate := func(in verb.WorkflowDeprecateInput) (verb.WorkflowDeprecateResult, error) {
		return verb.WorkflowDeprecate(context.Background(), env, in)
	}

	_, err := deprecate(verb.WorkflowDeprecateInput{Ref: "unit"})
	requireVerbCode(t, err, output.ErrValidation, "deprecate needs an explicit version")

	got, err := deprecate(verb.WorkflowDeprecateInput{Ref: "unit@1"})
	if err != nil || got.Workflow == nil || !got.Workflow.Deprecated() || got.Targets != nil {
		t.Fatalf("deprecate = %+v, %v; want unit@1 retired", got, err)
	}

	_, err = deprecate(verb.WorkflowDeprecateInput{Ref: "unit@1"})
	requireVerbCode(t, err, output.ErrConflict, "unit@1 is already deprecated")

	got, err = deprecate(verb.WorkflowDeprecateInput{Ref: "unit@1", Restore: true})
	if err != nil || got.Workflow.Deprecated() {
		t.Fatalf("restore = %+v, %v; want unit@1 binding", got, err)
	}

	_, err = deprecate(verb.WorkflowDeprecateInput{Ref: "unit@2"})
	requireVerbCode(t, err, output.ErrNotFound, "unit@2 is not registered")
}

// Restore across projects is idempotent where deprecate is not: a binding
// version reports unchanged, a retired one changed, a missing one an error.
func TestWorkflowDeprecateAllProjectsKeepsTheRestoreAsymmetry(t *testing.T) {
	st := openStore(t)
	caller := callerProject(t, st)
	other := ensureProject(t, st, "/repos/other", "other")
	insertWorkflow(t, st, caller, unitWorkflow)
	insertWorkflow(t, st, other, unitWorkflow)
	if _, err := st.DeprecateWorkflow(other, "unit", 1, 2); err != nil {
		t.Fatalf("DeprecateWorkflow: %v", err)
	}

	got, err := verb.WorkflowDeprecate(context.Background(), projectEnv(st, "/repos/caller", true),
		verb.WorkflowDeprecateInput{Ref: "unit@1", Restore: true, AllProjects: true})
	if err != nil {
		t.Fatalf("WorkflowDeprecate --restore --all-projects: %v", err)
	}
	if len(got.Targets) != 3 {
		t.Fatalf("restored in %d projects, want 3", len(got.Targets))
	}
	for _, target := range got.Targets {
		switch target.Project.ID {
		case caller:
			if target.Err != nil || target.Changed {
				t.Errorf("binding project outcome = %+v, want unchanged", target)
			}
		case other:
			if target.Err != nil || !target.Changed || target.Workflow.Deprecated() {
				t.Errorf("retired project outcome = %+v, want restored", target)
			}
		case store.DefaultProjectID:
			requireVerbCode(t, target.Err, output.ErrNotFound, "unit@1 is not registered")
		}
	}

	got, err = verb.WorkflowDeprecate(context.Background(), projectEnv(st, "/repos/caller", true),
		verb.WorkflowDeprecateInput{Ref: "unit@1", Project: "other"})
	if err != nil || len(got.Targets) != 1 || got.Targets[0].Err != nil || !got.Targets[0].Changed {
		t.Fatalf("deprecate --project other = %+v, %v; want one retirement", got, err)
	}
	got, err = verb.WorkflowDeprecate(context.Background(), projectEnv(st, "/repos/caller", true),
		verb.WorkflowDeprecateInput{Ref: "unit@1", Project: "other"})
	if err != nil || len(got.Targets) != 1 {
		t.Fatalf("second deprecate --project other = %+v, %v", got, err)
	}
	requireVerbCode(t, got.Targets[0].Err, output.ErrConflict, "unit@1 is already deprecated")
	if !errors.Is(got.Targets[0].Err, store.ErrWorkflowAlreadyDeprecated) {
		t.Errorf("the fanned-out refusal %v does not wrap the already-deprecated sentinel", got.Targets[0].Err)
	}
}
