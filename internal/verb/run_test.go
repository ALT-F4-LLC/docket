package verb_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
	"github.com/ALT-F4-LLC/docket/internal/verb"
	"github.com/ALT-F4-LLC/docket/internal/workflow"
)

// plannedRun registers stepWorkflow and starts a planning run over one issue
// in the default project, returning the run's reference. DOCKET_PATH is
// pinned to an empty store, as in activatedStepOf.
func plannedRun(t *testing.T, st store.Store) string {
	t.Helper()
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	def, err := workflow.Load([]byte(stepWorkflow))
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
		SourceSHA256: workflow.SHA256([]byte(stepWorkflow)),
		Body:         stepWorkflow, Parsed: string(parsed),
	}, model.NowMS()); err != nil {
		t.Fatalf("InsertWorkflow: %v", err)
	}
	res, err := verb.RunStart(context.Background(), projectEnv(st, "", false),
		verb.RunStartInput{Issues: []string{model.FormatID(createIssue(t, st))}})
	if err != nil {
		t.Fatalf("RunStart: %v", err)
	}
	return res.Run.Ref()
}

func requireNoRuns(t *testing.T, st store.Store) {
	t.Helper()
	if _, err := st.GetRun(1); !errors.Is(err, store.ErrRunNotFound) {
		t.Errorf("GetRun(1) error = %v, want no run created", err)
	}
}

func TestRunStartInheritsOmittedBudgetsAndKeepsExplicitZero(t *testing.T) {
	st := openStore(t)
	setConfig(t, st, store.DefaultProjectID, store.KeyBudgetDefault, "7")
	setConfig(t, st, store.DefaultProjectID, store.KeyUsageBudgetDefault, "9")
	issue := createIssue(t, st)
	unlimited := 0.0

	res, err := verb.RunStart(context.Background(), projectEnv(st, "", false), verb.RunStartInput{
		Request:     "the request",
		Issues:      []string{model.FormatID(issue)},
		UsageBudget: &unlimited,
		ExecRoot:    "/src/checkout",
		Hostname:    "host-1",
	})
	if err != nil {
		t.Fatalf("RunStart: %v", err)
	}
	run := res.Run
	if !res.Inserted || run.Status != model.RunPlanning || run.ProjectID != store.DefaultProjectID {
		t.Fatalf("run = %+v inserted %v, want a new planning run in the default project", run, res.Inserted)
	}
	if run.Budget != 7 || run.UsageBudget != 0 {
		t.Errorf("budget = %g, usage budget = %g; want the omitted cap's default 7 and the explicit 0",
			run.Budget, run.UsageBudget)
	}
	if run.Request != "the request" || run.ExecRoot != "/src/checkout" || run.Hostname != "host-1" {
		t.Errorf("run = %+v, want the request and context it was started with", run)
	}
	issues, err := st.ListRunIssues(run.ID)
	if err != nil {
		t.Fatalf("ListRunIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].IssueID != issue {
		t.Errorf("run issues = %+v, want issue %d alone", issues, issue)
	}
}

func TestRunStartRefusesNegativeBudgets(t *testing.T) {
	st := openStore(t)
	negative := -1.0
	for name, in := range map[string]verb.RunStartInput{
		"budget":       {Budget: &negative},
		"usage budget": {UsageBudget: &negative},
	} {
		_, err := verb.RunStart(context.Background(), projectEnv(st, "", false), in)
		if err == nil {
			t.Fatalf("%s: RunStart accepted a negative cap", name)
		}
		requireCode(t, err, output.ErrValidation)
	}
	requireNoRuns(t, st)
}

// A run's issues live in the run's own project, and the run starts in the
// caller's, so another project's issue is refused before any run exists.
func TestRunStartRefusesAnIssueHomedInAnotherProject(t *testing.T) {
	st := openStore(t)
	// The default row is claimed first, so the caller's registration adds a
	// project rather than claiming the issue's.
	if _, err := st.EnsureProject("/repos/first", "first", 1); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	issue := createIssue(t, st)

	res, err := verb.RunStart(context.Background(), projectEnv(st, "/repos/caller", true),
		verb.RunStartInput{Issues: []string{model.FormatID(issue)}})
	if err == nil {
		t.Fatal("RunStart attached an issue homed in another project")
	}
	requireCode(t, err, output.ErrValidation)
	requireNoRuns(t, st)
	if res.Registered == nil || res.Registered.Identity != "/repos/caller" || res.Registered.Verb != "run start" {
		t.Errorf("Registered = %+v, want the caller's project as `run start` registered it", res.Registered)
	}
}

func TestRunStartRefusesAMissingIssue(t *testing.T) {
	st := openStore(t)
	_, err := verb.RunStart(context.Background(), projectEnv(st, "", false),
		verb.RunStartInput{Issues: []string{"DKT-999"}})
	requireCode(t, err, output.ErrNotFound)
	requireNoRuns(t, st)
}

// run start writes into the caller's project, so an unanchored caller with no
// project is refused rather than given one.
func TestRunStartRefusesAnUnanchoredCaller(t *testing.T) {
	st := openStore(t)
	_, err := verb.RunStart(context.Background(), projectEnv(st, "/tmp/scratch", false), verb.RunStartInput{})
	requireCode(t, err, output.ErrValidation)
	requireUnregistered(t, st, "/tmp/scratch")
	requireNoRuns(t, st)
}

func TestRunActivateActivatesAPlanningRun(t *testing.T) {
	st := openStore(t)
	run := plannedRun(t, st)

	res, err := verb.RunActivate(context.Background(), projectEnv(st, "", false),
		verb.RunActivateInput{Run: run, Reason: "go"})
	if err != nil {
		t.Fatalf("RunActivate: %v", err)
	}
	got := res.Activation
	if got.Run.Status != model.RunActive || got.StepsCreated != 1 || got.Reactivation {
		t.Errorf("activation = %+v, want a first activation creating one step", got)
	}
	if got.ConductorToken == "" {
		t.Error("a first activation returned no conductor token")
	}
}

func TestRunActivateDryRunWritesNothing(t *testing.T) {
	st := openStore(t)
	run := plannedRun(t, st)

	res, err := verb.RunActivate(context.Background(), projectEnv(st, "", false),
		verb.RunActivateInput{Run: run, DryRun: true})
	if err != nil {
		t.Fatalf("RunActivate: %v", err)
	}
	if !res.Activation.DryRun || res.Activation.Run.Status != model.RunPlanning {
		t.Errorf("activation = %+v, want a dry run leaving the run in planning", res.Activation)
	}
}

func TestRunConductRotatesTheActivationCapability(t *testing.T) {
	st := openStore(t)
	run := plannedRun(t, st)
	env := projectEnv(st, "", false)
	activated, err := verb.RunActivate(context.Background(), env, verb.RunActivateInput{Run: run})
	if err != nil {
		t.Fatalf("RunActivate: %v", err)
	}

	env.Author = "conductor-2"
	res, err := verb.RunConduct(context.Background(), env, verb.RunConductInput{Run: run})
	if err != nil {
		t.Fatalf("RunConduct: %v", err)
	}
	if res.Conduct.Token == "" || res.Conduct.Token == activated.Activation.ConductorToken {
		t.Errorf("token = %q, want a fresh capability", res.Conduct.Token)
	}
	if !res.Conduct.Rotated {
		t.Error("Rotated = false, want the activation's capability retired")
	}
}

// Taking the seat is the audit trail of a token-free verb, so a caller with
// no author is refused before anything is minted.
func TestRunConductRefusesAnUnattributedCaller(t *testing.T) {
	st := openStore(t)
	run := plannedRun(t, st)

	_, err := verb.RunConduct(context.Background(), projectEnv(st, "", false), verb.RunConductInput{Run: run})
	if err == nil {
		t.Fatal("RunConduct minted a capability for a caller with no author")
	}
}

func TestRunVerbsRefuseAMalformedRunRef(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	env.Author = "someone"
	ctx := context.Background()

	_, err := verb.RunActivate(ctx, env, verb.RunActivateInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunConduct(ctx, env, verb.RunConductInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
}

func TestRunVerbsReportAMissingRunAsNotFound(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	env.Author = "someone"
	ctx := context.Background()

	_, err := verb.RunActivate(ctx, env, verb.RunActivateInput{Run: "RUN-42"})
	requireCode(t, err, output.ErrNotFound)
	_, err = verb.RunConduct(ctx, env, verb.RunConductInput{Run: "RUN-42"})
	requireCode(t, err, output.ErrNotFound)
}
