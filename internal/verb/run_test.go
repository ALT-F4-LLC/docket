package verb_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/engine"
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
		Branch:      "main",
		CommitSHA:   "0123456789abcdef0123456789abcdef01234567",
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
	stored, err := st.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Request != "the request" || stored.ExecRoot != "/src/checkout" || stored.Hostname != "host-1" ||
		stored.Branch != "main" || stored.CommitSHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("stored run = %+v, want the request and context it was started with", stored)
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

// The missing issue is named under the caller's project prefix, which the
// verb reads from the store rather than from process state.
func TestRunStartRefusesAMissingIssue(t *testing.T) {
	st := openStore(t)
	if err := st.SetProjectPrefix(store.DefaultProjectID, "ABC"); err != nil {
		t.Fatalf("SetProjectPrefix: %v", err)
	}
	_, err := verb.RunStart(context.Background(), projectEnv(st, "", false),
		verb.RunStartInput{Issues: []string{"999"}})
	requireVerbCode(t, err, output.ErrNotFound, "issue ABC-999 not found")
	requireNoRuns(t, st)
}

// The faults run start can report are checked in a fixed order: a negative
// --budget, then the issues, then the configured budget default. A bad issue
// is the reported fault even when budget.default is unusable.
func TestRunStartChecksTheIssuesBetweenTheBudgetAndItsDefault(t *testing.T) {
	st := openStore(t)
	// SetConfig refuses a non-number, so the unusable default is written as
	// the raw row a store holding one would carry.
	conn, err := store.SQLDB(st)
	if err != nil {
		t.Fatalf("SQLDB: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`,
		"config.p1."+store.KeyBudgetDefault, "lots"); err != nil {
		t.Fatalf("writing an unusable budget.default: %v", err)
	}
	env := projectEnv(st, "", false)
	missing := []string{"999"}

	_, err = verb.RunStart(context.Background(), env, verb.RunStartInput{Issues: missing})
	requireVerbCode(t, err, output.ErrNotFound, "issue DKT-999 not found")

	negative := -1.0
	_, err = verb.RunStart(context.Background(), env, verb.RunStartInput{Issues: missing, Budget: &negative})
	requireVerbCode(t, err, output.ErrValidation, "--budget must be >= 0")

	issue := model.FormatID(createIssue(t, st))
	_, err = verb.RunStart(context.Background(), env, verb.RunStartInput{Issues: []string{issue}})
	requireVerbCode(t, err, output.ErrValidation, `budget.default is "lots"`)
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

	res, err := verb.RunActivate(context.Background(), projectEnv(st, "", false), verb.RunActivateInput{
		Run: run, Reason: "go",
		HeadBranch: "main", HeadCommit: "0123456789abcdef0123456789abcdef01234567",
	})
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
	want := runActivated{Reason: "go", HeadBranch: "main", HeadCommit: "0123456789abcdef0123456789abcdef01234567"}
	if events := runActivatedEvents(t, st, got.Run.ID); len(events) != 1 || events[0] != want {
		t.Errorf("run-activated events = %+v, want one carrying %+v", events, want)
	}
}

// runActivated is the data a run-activated event carries.
type runActivated struct {
	Reason     string `json:"reason"`
	HeadBranch string `json:"head_branch"`
	HeadCommit string `json:"head_commit"`
}

func runActivatedEvents(t *testing.T, st store.Store, runID int) []runActivated {
	t.Helper()
	tx, err := st.BeginReadSnapshot()
	if err != nil {
		t.Fatalf("BeginReadSnapshot: %v", err)
	}
	defer tx.Rollback()
	rows, err := tx.RunEventsTx(runID, engine.EventRunActivated)
	if err != nil {
		t.Fatalf("RunEventsTx: %v", err)
	}
	out := make([]runActivated, 0, len(rows))
	for _, row := range rows {
		var e runActivated
		if row.Data != "" {
			if err := json.Unmarshal([]byte(row.Data), &e); err != nil {
				t.Fatalf("decoding event data %q: %v", row.Data, err)
			}
		}
		out = append(out, e)
	}
	return out
}

func TestRunActivateDryRunWritesNothing(t *testing.T) {
	st := openStore(t)
	run := plannedRun(t, st)
	runID, err := model.ParseRunID(run)
	if err != nil {
		t.Fatalf("ParseRunID: %v", err)
	}

	res, err := verb.RunActivate(context.Background(), projectEnv(st, "", false),
		verb.RunActivateInput{Run: run, DryRun: true})
	if err != nil {
		t.Fatalf("RunActivate: %v", err)
	}
	if !res.Activation.DryRun || res.Activation.Run.Status != model.RunPlanning {
		t.Errorf("activation = %+v, want a dry run leaving the run in planning", res.Activation)
	}
	stored, err := st.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Status != model.RunPlanning {
		t.Errorf("stored run status = %s, want planning", stored.Status)
	}
	steps, err := st.ListRunSteps(runID)
	if err != nil {
		t.Fatalf("ListRunSteps: %v", err)
	}
	pins, err := st.ListPins(runID)
	if err != nil {
		t.Fatalf("ListPins: %v", err)
	}
	if len(steps) != 0 || len(pins) != 0 {
		t.Errorf("dry run stored %d step(s) and %d pin(s), want none", len(steps), len(pins))
	}
	if events := runActivatedEvents(t, st, runID); len(events) != 0 {
		t.Errorf("dry run recorded run-activated events %+v, want none", events)
	}
}

// Activation and taking the conductor seat write into the caller's project,
// so each refuses an unanchored caller with no project rather than giving it
// one, and registers an anchored caller's project on first contact.
func TestRunActivateAndConductResolveTheCallersProject(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(verb.Env) (*engine.ProjectRegistration, error){
		"run activate": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunActivate(ctx, env, verb.RunActivateInput{Run: "RUN-42"})
			return res.Registered, err
		},
		"run conduct": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunConduct(ctx, env, verb.RunConductInput{Run: "RUN-42"})
			return res.Registered, err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			unanchored := projectEnv(st, "/tmp/scratch", false)
			unanchored.Author = "someone"
			_, err := call(unanchored)
			requireVerbCode(t, err, output.ErrValidation, "")
			requireUnregistered(t, st, "/tmp/scratch")

			anchored := projectEnv(st, "/repos/caller", true)
			anchored.Author = "someone"
			registered, err := call(anchored)
			requireCode(t, err, output.ErrNotFound)
			if registered == nil || registered.Identity != "/repos/caller" || registered.Verb != name {
				t.Errorf("Registered = %+v, want the caller's project as `%s` registered it", registered, name)
			}
		})
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
	requireVerbCode(t, err, output.ErrGeneral, "refusing to record an unattributed run conduct")
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
