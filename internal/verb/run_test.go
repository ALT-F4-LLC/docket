package verb_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// Activation, taking the conductor seat, the lifecycle moves and the budget
// verb write into the caller's project, so each refuses an unanchored caller
// with no project rather than giving it one, and registers an anchored
// caller's project on first contact.
func TestRunWritesResolveTheCallersProject(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(verb.Env) (*engine.ProjectRegistration, error){
		"run pause": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunPause(ctx, env, verb.RunPauseInput{Run: "RUN-42", Authority: "operator"})
			return res.Registered, err
		},
		"run resume": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunResume(ctx, env, verb.RunResumeInput{Run: "RUN-42"})
			return res.Registered, err
		},
		"run abandon": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunAbandon(ctx, env, verb.RunAbandonInput{
				Run: "RUN-42", Reason: "r", Authority: "operator",
			})
			return res.Registered, err
		},
		"run abandon --issue": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunAbandon(ctx, env, verb.RunAbandonInput{
				Run: "RUN-42", Issue: "1", Reason: "r", Authority: "operator",
			})
			return res.Registered, err
		},
		"run budget": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunBudget(ctx, env, verb.RunBudgetInput{Run: "RUN-42"})
			return res.Registered, err
		},
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
			wantVerb := strings.TrimSuffix(name, " --issue")
			if registered == nil || registered.Identity != "/repos/caller" || registered.Verb != wantVerb {
				t.Errorf("Registered = %+v, want the caller's project as `%s` registered it", registered, wantVerb)
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
	_, err = verb.RunPause(ctx, env, verb.RunPauseInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunResume(ctx, env, verb.RunResumeInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunAbandon(ctx, env, verb.RunAbandonInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunBudget(ctx, env, verb.RunBudgetInput{Run: "RUN-x"})
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
	_, err = verb.RunPause(ctx, env, verb.RunPauseInput{Run: "RUN-42", Authority: "operator"})
	requireCode(t, err, output.ErrNotFound)
	_, err = verb.RunResume(ctx, env, verb.RunResumeInput{Run: "RUN-42"})
	requireCode(t, err, output.ErrNotFound)
	_, err = verb.RunAbandon(ctx, env, verb.RunAbandonInput{Run: "RUN-42", Reason: "r", Authority: "operator"})
	requireCode(t, err, output.ErrNotFound)
	_, err = verb.RunBudget(ctx, env, verb.RunBudgetInput{Run: "RUN-42"})
	requireCode(t, err, output.ErrNotFound)
}

// activeRun activates a planned run, returning its reference, its id and
// the conductor capability the activation minted.
func activeRun(t *testing.T, st store.Store) (string, int, string) {
	t.Helper()
	run := plannedRun(t, st)
	res, err := verb.RunActivate(context.Background(), projectEnv(st, "", false), verb.RunActivateInput{Run: run})
	if err != nil {
		t.Fatalf("RunActivate: %v", err)
	}
	return run, res.Activation.Run.ID, res.Activation.ConductorToken
}

func requireRunStatus(t *testing.T, st store.Store, runID int, want model.RunStatus) {
	t.Helper()
	got, err := st.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != want {
		t.Errorf("run status = %s, want %s", got.Status, want)
	}
}

func tokenOf(token string) func() string { return func() string { return token } }

func TestRunPauseResumeAbandonMoveTheRun(t *testing.T) {
	st := openStore(t)
	run, runID, token := activeRun(t, st)
	env := projectEnv(st, "", false)
	ctx := context.Background()

	paused, err := verb.RunPause(ctx, env, verb.RunPauseInput{
		Run: run, Reason: "operator review", Authority: "operator", ConductorToken: tokenOf(token),
	})
	if err != nil {
		t.Fatalf("RunPause: %v", err)
	}
	if paused.Run.Status != model.RunWaitingHuman || paused.Run.Reason != "operator review" {
		t.Errorf("paused run = %+v, want waiting-human with the reason", paused.Run)
	}

	_, err = verb.RunPause(ctx, env, verb.RunPauseInput{Run: run, Authority: "operator", ConductorToken: tokenOf(token)})
	requireCode(t, err, output.ErrConflict)

	resumed, err := verb.RunResume(ctx, env, verb.RunResumeInput{Run: run, ConductorToken: tokenOf(token)})
	if err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if resumed.Run.Status != model.RunActive || len(resumed.PinDrift) != 0 {
		t.Errorf("resumed = %+v, want an active run with no pin drift", resumed)
	}

	abandoned, err := verb.RunAbandon(ctx, env, verb.RunAbandonInput{
		Run: run, Reason: "done with it", Authority: "operator", ConductorToken: tokenOf(token),
	})
	if err != nil {
		t.Fatalf("RunAbandon: %v", err)
	}
	if abandoned.Run.Status != model.RunAbandoned || abandoned.IssueOutcome != nil {
		t.Errorf("abandoned = %+v, want the whole run abandoned", abandoned)
	}
	requireRunStatus(t, st, runID, model.RunAbandoned)
}

// The two dispositions say under what authority they were made and abandon
// says why; each refusal comes before the run moves.
func TestRunPauseAndAbandonRefuseAMissingAuthorityOrReason(t *testing.T) {
	st := openStore(t)
	run, runID, token := activeRun(t, st)
	env := projectEnv(st, "", false)
	ctx := context.Background()

	_, err := verb.RunPause(ctx, env, verb.RunPauseInput{Run: run, ConductorToken: tokenOf(token)})
	requireVerbCode(t, err, output.ErrValidation, "")
	_, err = verb.RunAbandon(ctx, env, verb.RunAbandonInput{Run: run, Reason: "r", ConductorToken: tokenOf(token)})
	requireVerbCode(t, err, output.ErrValidation, "")
	_, err = verb.RunAbandon(ctx, env, verb.RunAbandonInput{Run: run, Authority: "operator", ConductorToken: tokenOf(token)})
	requireVerbCode(t, err, output.ErrValidation, "--reason is required to abandon a run")
	_, err = verb.RunAbandon(ctx, env, verb.RunAbandonInput{
		Run: run, Issue: "1", Authority: "operator", ConductorToken: tokenOf(token),
	})
	requireVerbCode(t, err, output.ErrValidation, "--reason is required to abandon an issue's work")
	requireRunStatus(t, st, runID, model.RunActive)
}

// A bound run moves only under its conductor capability: a missing token is
// refused as VALIDATION, a wrong one as AUTH, and the run stays where it was.
func TestRunLifecycleVerbsRequireTheConductorCapability(t *testing.T) {
	st := openStore(t)
	run, runID, _ := activeRun(t, st)
	env := projectEnv(st, "", false)
	ctx := context.Background()

	_, err := verb.RunPause(ctx, env, verb.RunPauseInput{Run: run, Authority: "operator"})
	requireVerbCode(t, err, output.ErrValidation, "run pause requires it")
	_, err = verb.RunAbandon(ctx, env, verb.RunAbandonInput{
		Run: run, Reason: "r", Authority: "operator", ConductorToken: tokenOf("not-the-token"),
	})
	requireCode(t, err, output.ErrAuth)
	_, err = verb.RunResume(ctx, env, verb.RunResumeInput{Run: run, ConductorToken: tokenOf("not-the-token")})
	requireCode(t, err, output.ErrAuth)
	requireRunStatus(t, st, runID, model.RunActive)
}

// Abandoning an issue stops its steps without abandoning the run; the run's
// only issue was its last unfinished work, so the rollup completes the run.
func TestRunAbandonWithAnIssueDoesNotAbandonTheRun(t *testing.T) {
	st := openStore(t)
	run, runID, token := activeRun(t, st)
	issues, err := st.ListRunIssues(runID)
	if err != nil || len(issues) != 1 {
		t.Fatalf("ListRunIssues = %+v, %v; want one issue", issues, err)
	}

	res, err := verb.RunAbandon(context.Background(), projectEnv(st, "", false), verb.RunAbandonInput{
		Run: run, Issue: model.FormatID(issues[0].IssueID), Reason: "mis-routed",
		Authority: "operator", ConductorToken: tokenOf(token),
	})
	if err != nil {
		t.Fatalf("RunAbandon --issue: %v", err)
	}
	if res.Run != nil || res.IssueOutcome == nil || len(res.IssueOutcome.Steps) != 1 {
		t.Fatalf("result = %+v, want one issue's single step abandoned", res)
	}
	if res.IssueOutcome.RunStatus != string(model.RunDone) {
		t.Errorf("outcome run status = %s, want done", res.IssueOutcome.RunStatus)
	}
	requireRunStatus(t, st, runID, model.RunDone)
}

func TestRunBudgetReadsAndSetsTheCap(t *testing.T) {
	st := openStore(t)
	run, runID, _ := activeRun(t, st)
	env := projectEnv(st, "", false)
	ctx := context.Background()

	read, err := verb.RunBudget(ctx, env, verb.RunBudgetInput{Run: run})
	if err != nil {
		t.Fatalf("RunBudget read: %v", err)
	}
	if read.Budget.Run != run || read.Budget.Budget != 0 {
		t.Errorf("read = %+v, want %s with no cap", read.Budget, run)
	}

	stale := read.Budget.RowVersion + 1
	limit := 5.0
	_, err = verb.RunBudget(ctx, env, verb.RunBudgetInput{Run: run, Set: &limit, IfVersion: &stale})
	requireVerbCode(t, err, output.ErrConflict, "version mismatch")

	set, err := verb.RunBudget(ctx, env, verb.RunBudgetInput{
		Run: run, Set: &limit, Reason: "more room", IfVersion: &read.Budget.RowVersion,
	})
	if err != nil {
		t.Fatalf("RunBudget set: %v", err)
	}
	if set.Budget.Budget != 5 {
		t.Errorf("set = %+v, want a cap of 5", set.Budget)
	}
	stored, err := st.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if stored.Budget != 5 {
		t.Errorf("stored budget = %g, want 5", stored.Budget)
	}
	want := runBudgetSet{From: 0, To: 5, Reason: "more room"}
	if events := runBudgetSetEvents(t, st, runID); len(events) != 1 || events[0] != want {
		t.Errorf("run-budget-set events = %+v, want one carrying %+v", events, want)
	}
}

// runBudgetSet is the data a run-budget-set event carries.
type runBudgetSet struct {
	From   float64 `json:"from"`
	To     float64 `json:"to"`
	Reason string  `json:"reason"`
}

func runBudgetSetEvents(t *testing.T, st store.Store, runID int) []runBudgetSet {
	t.Helper()
	tx, err := st.BeginReadSnapshot()
	if err != nil {
		t.Fatalf("BeginReadSnapshot: %v", err)
	}
	defer tx.Rollback()
	rows, err := tx.RunEventsTx(runID, engine.EventRunBudgetSet)
	if err != nil {
		t.Fatalf("RunEventsTx: %v", err)
	}
	out := make([]runBudgetSet, 0, len(rows))
	for _, row := range rows {
		var e runBudgetSet
		if err := json.Unmarshal([]byte(row.Data), &e); err != nil {
			t.Fatalf("decoding event data %q: %v", row.Data, err)
		}
		out = append(out, e)
	}
	return out
}

// A cap change checks its faults in the order the CLI always reported them:
// the caller's project, then the run reference, then the --if-version
// precondition. An anchored caller is registered even when its precondition
// is refused.
func TestRunBudgetChecksTheIfVersionAfterTheProjectAndTheRun(t *testing.T) {
	ctx := context.Background()
	limit, zero := 5.0, 0
	setWithZero := func(run string) verb.RunBudgetInput {
		return verb.RunBudgetInput{Run: run, Set: &limit, IfVersion: &zero}
	}
	st := openStore(t)

	_, err := verb.RunBudget(ctx, projectEnv(st, "/tmp/scratch", false), setWithZero("RUN-42"))
	requireVerbCode(t, err, output.ErrValidation, "cannot tell which project this belongs to")
	requireUnregistered(t, st, "/tmp/scratch")

	_, err = verb.RunBudget(ctx, projectEnv(st, "", false), setWithZero("RUN-x"))
	requireVerbCode(t, err, output.ErrValidation, `invalid run ID "RUN-x"`)

	res, err := verb.RunBudget(ctx, projectEnv(st, "/repos/caller", true), setWithZero("RUN-42"))
	requireVerbCode(t, err, output.ErrValidation, "--if-version must be >= 1, got 0 (versions start at 1)")
	if res.Registered == nil || res.Registered.Identity != "/repos/caller" {
		t.Errorf("Registered = %+v, want the caller's project registered before the precondition", res.Registered)
	}
}

// runAt returns a run in status want, its id, and the conductor capability
// its activation minted ("" for a planning run, which is bound to none).
func runAt(t *testing.T, st store.Store, want model.RunStatus) (string, int, string) {
	t.Helper()
	if want == model.RunPlanning {
		run := plannedRun(t, st)
		runID, err := model.ParseRunID(run)
		if err != nil {
			t.Fatalf("ParseRunID: %v", err)
		}
		return run, runID, ""
	}
	run, runID, token := activeRun(t, st)
	env := projectEnv(st, "", false)
	ctx := context.Background()
	var err error
	switch want {
	case model.RunActive:
	case model.RunWaitingHuman:
		_, err = verb.RunPause(ctx, env, verb.RunPauseInput{
			Run: run, Authority: "operator", ConductorToken: tokenOf(token),
		})
	case model.RunAbandoned:
		_, err = verb.RunAbandon(ctx, env, verb.RunAbandonInput{
			Run: run, Reason: "setup", Authority: "operator", ConductorToken: tokenOf(token),
		})
	case model.RunDone:
		// The run's one issue is its last unfinished work, so abandoning that
		// issue's work completes the run.
		issues, listErr := st.ListRunIssues(runID)
		if listErr != nil || len(issues) != 1 {
			t.Fatalf("ListRunIssues = %+v, %v; want one issue", issues, listErr)
		}
		_, err = verb.RunAbandon(ctx, env, verb.RunAbandonInput{
			Run: run, Issue: model.FormatID(issues[0].IssueID), Reason: "setup", Authority: "operator", ConductorToken: tokenOf(token),
		})
	default:
		t.Fatalf("runAt: no setup for status %s", want)
	}
	if err != nil {
		t.Fatalf("moving the run to %s: %v", want, err)
	}
	requireRunStatus(t, st, runID, want)
	return run, runID, token
}

// Each lifecycle verb takes the run status machine's edges and no others:
//
//	planning -> active <-> waiting-human -> done | abandoned
//
// pause leaves active, resume leaves waiting-human, and abandon leaves any
// status that is not terminal. Every other start is CONFLICT and leaves the
// run where it was.
func TestRunLifecycleVerbsTakeOnlyTheirTransitions(t *testing.T) {
	statuses := []model.RunStatus{
		model.RunPlanning, model.RunActive, model.RunWaitingHuman, model.RunDone, model.RunAbandoned,
	}
	verbs := []struct {
		name string
		to   model.RunStatus
		from []model.RunStatus
		call func(env verb.Env, run, token string) error
	}{
		{"pause", model.RunWaitingHuman, []model.RunStatus{model.RunActive},
			func(env verb.Env, run, token string) error {
				_, err := verb.RunPause(context.Background(), env, verb.RunPauseInput{
					Run: run, Authority: "operator", ConductorToken: tokenOf(token),
				})
				return err
			}},
		{"resume", model.RunActive, []model.RunStatus{model.RunWaitingHuman},
			func(env verb.Env, run, token string) error {
				_, err := verb.RunResume(context.Background(), env, verb.RunResumeInput{
					Run: run, ConductorToken: tokenOf(token),
				})
				return err
			}},
		{"abandon", model.RunAbandoned,
			[]model.RunStatus{model.RunPlanning, model.RunActive, model.RunWaitingHuman},
			func(env verb.Env, run, token string) error {
				_, err := verb.RunAbandon(context.Background(), env, verb.RunAbandonInput{
					Run: run, Reason: "r", Authority: "operator", ConductorToken: tokenOf(token),
				})
				return err
			}},
	}
	for _, v := range verbs {
		for _, start := range statuses {
			t.Run(v.name+" from "+string(start), func(t *testing.T) {
				st := openStore(t)
				run, runID, token := runAt(t, st, start)
				err := v.call(projectEnv(st, "", false), run, token)
				for _, allowed := range v.from {
					if start == allowed {
						if err != nil {
							t.Fatalf("run %s from %s: %v, want the move", v.name, start, err)
						}
						requireRunStatus(t, st, runID, v.to)
						return
					}
				}
				requireCode(t, err, output.ErrConflict)
				requireRunStatus(t, st, runID, start)
			})
		}
	}
}

// A planning run is bound to no conductor capability, so no lifecycle verb
// reads its token source, whether the move is taken or refused.
func TestRunLifecycleVerbsReadNoTokenForAnUnboundRun(t *testing.T) {
	st := openStore(t)
	run, _, _ := runAt(t, st, model.RunPlanning)
	env := projectEnv(st, "", false)
	ctx := context.Background()
	reads := 0
	counting := func() string { reads++; return "unused" }

	_, err := verb.RunPause(ctx, env, verb.RunPauseInput{Run: run, Authority: "operator", ConductorToken: counting})
	requireCode(t, err, output.ErrConflict)
	_, err = verb.RunResume(ctx, env, verb.RunResumeInput{Run: run, ConductorToken: counting})
	requireCode(t, err, output.ErrConflict)
	if _, err := verb.RunAbandon(ctx, env, verb.RunAbandonInput{
		Run: run, Reason: "r", Authority: "operator", ConductorToken: counting,
	}); err != nil {
		t.Fatalf("RunAbandon: %v", err)
	}
	if reads != 0 {
		t.Errorf("token source read %d time(s) for an unbound run, want none", reads)
	}
}

// A resume reports the pins that drifted while the run was parked, and the
// drift does not stop the resume.
func TestRunResumeReportsPinDrift(t *testing.T) {
	st := openStore(t)
	run, runID, token := runAt(t, st, model.RunWaitingHuman)

	const ref = "contracts/drifted.md"
	path := filepath.Join(os.Getenv("DOCKET_PATH"), "config", ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("AFTER\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	tx, err := st.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.InsertPinTx(store.Pin{
		RunID: runID, Kind: store.PinKindFile, Ref: ref, SHA256: workflow.SHA256([]byte("BEFORE\n")),
	}); err != nil {
		tx.Rollback()
		t.Fatalf("InsertPinTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, err := verb.RunResume(context.Background(), projectEnv(st, "", false),
		verb.RunResumeInput{Run: run, ConductorToken: tokenOf(token)})
	if err != nil {
		t.Fatalf("RunResume: %v", err)
	}
	if len(res.PinDrift) != 1 || res.PinDrift[0].Ref != ref || res.PinDrift[0].Status == engine.PinOK {
		t.Errorf("PinDrift = %+v, want the one drifted pin %s", res.PinDrift, ref)
	}
	requireRunStatus(t, st, runID, model.RunActive)
}
