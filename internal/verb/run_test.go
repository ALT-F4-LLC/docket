package verb_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	return plannedRunOf(t, st, stepWorkflow)
}

// plannedRunOf is plannedRun over the workflow body wf.
func plannedRunOf(t *testing.T, st store.Store, wf string) string {
	t.Helper()
	t.Setenv("DOCKET_PATH", filepath.Join(t.TempDir(), ".docket"))
	def, err := workflow.Load([]byte(wf))
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
		SourceSHA256: workflow.SHA256([]byte(wf)),
		Body:         wf, Parsed: string(parsed),
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

// Activation, taking the conductor seat, the lifecycle moves, the budget
// verb, facts and notes write into the caller's project, so each refuses an unanchored caller
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
		"run fact add": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunFactAdd(ctx, env, verb.RunFactAddInput{
				Run: "RUN-42", Kind: engine.RunFactStepDeferred, Step: "STEP-1",
				Cause: engine.DeferralBudget, Reason: "r",
			})
			return res.Registered, err
		},
		"run note add": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunNoteAdd(ctx, env, verb.RunNoteAddInput{
				Run: "RUN-42", Authority: engine.AuthorityOperator, Text: textOf("a ruling"),
			})
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
	_, err = verb.RunStatus(ctx, env, verb.RunStatusInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunReport(ctx, env, verb.RunReportInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunVerifyPins(ctx, env, verb.RunVerifyPinsInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunFactAdd(ctx, env, verb.RunFactAddInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunNoteAdd(ctx, env, verb.RunNoteAddInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunNoteList(ctx, env, verb.RunNoteListInput{Run: "RUN-x"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunRefreshBody(ctx, env, verb.RunRefreshBodyInput{Run: "RUN-x", Issue: "DKT-1", Reason: "r"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunRefreshFiles(ctx, env, verb.RunRefreshFilesInput{Run: "RUN-x", Issue: "DKT-1", Reason: "r"})
	requireCode(t, err, output.ErrValidation)
	_, err = verb.RunRefreshScope(ctx, env, verb.RunRefreshScopeInput{Run: "RUN-x", Issue: "DKT-1", Reason: "r"})
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
	_, err = verb.RunStatus(ctx, env, verb.RunStatusInput{Run: "RUN-42"})
	requireCode(t, err, output.ErrNotFound)
	_, err = verb.RunReport(ctx, env, verb.RunReportInput{Run: "RUN-42"})
	requireCode(t, err, output.ErrNotFound)
	_, err = verb.RunVerifyPins(ctx, env, verb.RunVerifyPinsInput{Run: "RUN-42"})
	requireCode(t, err, output.ErrNotFound)
	_, err = verb.RunFactAdd(ctx, env, verb.RunFactAddInput{
		Run: "RUN-42", Kind: engine.RunFactStepDeferred, Step: "STEP-1", Cause: engine.DeferralBudget, Reason: "r",
	})
	requireCode(t, err, output.ErrNotFound)
	_, err = verb.RunNoteAdd(ctx, env, verb.RunNoteAddInput{
		Run: "RUN-42", Authority: engine.AuthorityOperator, Text: textOf("a ruling"),
	})
	requireCode(t, err, output.ErrNotFound)
	_, err = verb.RunNoteList(ctx, env, verb.RunNoteListInput{Run: "RUN-42"})
	requireCode(t, err, output.ErrNotFound)
}

func textOf(text string) func() (string, error) { return func() (string, error) { return text, nil } }

// A fact names the step it deferred and is recorded only under the bound
// run's conductor capability.
func TestRunFactAddRecordsAStepDeferralUnderTheCapability(t *testing.T) {
	st := openStore(t)
	run, runID, token := activeRun(t, st)
	steps, err := st.ListRunSteps(runID)
	if err != nil || len(steps) == 0 {
		t.Fatalf("ListRunSteps = %v, %v; want the run's steps", steps, err)
	}
	step := model.FormatStepID(steps[0].ID)
	env := projectEnv(st, "", false)
	env.Author = "conductor"
	ctx := context.Background()
	in := verb.RunFactAddInput{
		Run: run, Kind: engine.RunFactStepDeferred, Step: step, Cause: engine.DeferralChain,
		Reason: "behind its predecessor",
	}

	_, err = verb.RunFactAdd(ctx, env, in)
	requireVerbCode(t, err, output.ErrValidation, "run fact add requires it")
	in.ConductorToken = tokenOf("not-the-token")
	_, err = verb.RunFactAdd(ctx, env, in)
	requireCode(t, err, output.ErrAuth)

	in.ConductorToken = tokenOf(token)
	res, err := verb.RunFactAdd(ctx, env, in)
	if err != nil {
		t.Fatalf("RunFactAdd: %v", err)
	}
	want := engine.RunFact{
		Run: run, Kind: engine.RunFactStepDeferred, Seq: res.Fact.Seq, Step: step,
		Cause: engine.DeferralChain, Reason: "behind its predecessor",
	}
	if *res.Fact != want || res.Fact.Seq == 0 {
		t.Errorf("fact = %+v, want %+v with a recorded seq", *res.Fact, want)
	}
	requireFactAttribution(t, st, runID, engine.RunFactStepDeferred, "conductor", "/work/scratch")
}

// A re-seated vote seat is recorded with the proposal and the voter it names.
func TestRunFactAddRecordsAVoteReseat(t *testing.T) {
	st := openStore(t)
	run, runID, token := activeRun(t, st)
	steps, err := st.ListRunSteps(runID)
	if err != nil || len(steps) == 0 {
		t.Fatalf("ListRunSteps = %v, %v; want the run's steps", steps, err)
	}
	proposal := openProposal(t, st, 2, "")
	pid, err := model.ParseProposalID(proposal)
	if err != nil {
		t.Fatalf("ParseProposalID: %v", err)
	}
	if err := st.LinkProposalIssue(pid, steps[0].IssueID); err != nil {
		t.Fatalf("LinkProposalIssue: %v", err)
	}
	env := projectEnv(st, "", false)
	env.Author = "conductor"

	res, err := verb.RunFactAdd(context.Background(), env, verb.RunFactAddInput{
		Run: run, Kind: engine.RunFactVoteReseated, Proposal: proposal, Voter: "seat-a",
		Reason: "the seat died mid-cast", ConductorToken: tokenOf(token),
	})
	if err != nil {
		t.Fatalf("RunFactAdd: %v", err)
	}
	want := engine.RunFact{
		Run: run, Kind: engine.RunFactVoteReseated, Seq: res.Fact.Seq, Proposal: proposal,
		Voter: "seat-a", Reason: "the seat died mid-cast",
	}
	if *res.Fact != want || res.Fact.Seq == 0 {
		t.Errorf("fact = %+v, want %+v with a recorded seq", *res.Fact, want)
	}
	requireFactAttribution(t, st, runID, engine.RunFactVoteReseated, "conductor", "/work/scratch")
}

// requireFactAttribution asserts the run's newest fact event of kind names
// actor and cwd.
func requireFactAttribution(t *testing.T, st store.Store, runID int, kind, actor, cwd string) {
	t.Helper()
	page, err := engine.ListEventsStore(st, engine.EventQuery{RunID: runID, Kind: kind})
	if err != nil || len(page.Events) == 0 {
		t.Fatalf("ListEventsStore(%s) = %d events, %v; want the recorded fact", kind, len(page.Events), err)
	}
	var data struct{ Actor, Cwd string }
	if err := json.Unmarshal(page.Events[len(page.Events)-1].Data, &data); err != nil {
		t.Fatalf("decoding the %s event: %v", kind, err)
	}
	if data.Actor != actor || data.Cwd != cwd {
		t.Errorf("%s event attributed to %q from %q, want %q from %q", kind, data.Actor, data.Cwd, actor, cwd)
	}
}

func TestRunFactAddRefusesAMalformedStepOrProposal(t *testing.T) {
	st := openStore(t)
	env := projectEnv(st, "", false)
	ctx := context.Background()

	_, err := verb.RunFactAdd(ctx, env, verb.RunFactAddInput{Run: "RUN-1", Step: "STEP-x"})
	requireVerbCode(t, err, output.ErrValidation, `invalid --step "STEP-x"`)
	_, err = verb.RunFactAdd(ctx, env, verb.RunFactAddInput{Run: "RUN-1", Proposal: "PROPOSAL-x"})
	requireVerbCode(t, err, output.ErrValidation, `invalid --proposal "PROPOSAL-x"`)
}

// A note is recorded under the stated authority and the bound run's
// capability, and the list reads it back in the shape the add returned.
func TestRunNoteAddRecordsTheNoteTheListReadsBack(t *testing.T) {
	st := openStore(t)
	run, _, token := activeRun(t, st)
	env := projectEnv(st, "", false)
	env.Author = "conductor"
	ctx := context.Background()
	in := verb.RunNoteAddInput{Run: run, Authority: engine.AuthorityConductor, Text: textOf("gate fails on clean HEAD\n")}

	_, err := verb.RunNoteAdd(ctx, env, in)
	requireVerbCode(t, err, output.ErrValidation, "run note add requires it")

	in.ConductorToken = tokenOf(token)
	added, err := verb.RunNoteAdd(ctx, env, in)
	if err != nil {
		t.Fatalf("RunNoteAdd: %v", err)
	}
	if added.Run != run || added.Note.Text != "gate fails on clean HEAD" ||
		added.Note.Authority != engine.AuthorityConductor {
		t.Errorf("added = %s %+v, want %s's conductor note without its trailing newline", added.Run, *added.Note, run)
	}

	listed, err := verb.RunNoteList(ctx, env, verb.RunNoteListInput{Run: run})
	if err != nil {
		t.Fatalf("RunNoteList: %v", err)
	}
	if listed.Run != run || len(listed.Notes) != 1 || listed.Notes[0] != *added.Note {
		t.Errorf("listed = %s %+v, want %s with the one added note %+v", listed.Run, listed.Notes, run, *added.Note)
	}
}

// The text source is read only after the run reference and the authority are
// accepted, so a refused note leaves a transport's stream unread. A
// *verb.Error the source returns is the verb's unchanged; any other error is
// coded GENERAL_ERROR.
func TestRunNoteAddReadsTheTextOnlyAfterTheAuthority(t *testing.T) {
	st := openStore(t)
	run, _, token := activeRun(t, st)
	env := projectEnv(st, "", false)
	env.Author = "conductor"
	ctx := context.Background()
	unread := func() (string, error) {
		t.Error("the text source was read before the refusal")
		return "", nil
	}

	_, err := verb.RunNoteAdd(ctx, env, verb.RunNoteAddInput{Run: "RUN-x", Authority: engine.AuthorityOperator, Text: unread})
	requireCode(t, err, output.ErrValidation)
	for _, authority := range []string{"", "standing-grant", "admin"} {
		_, err = verb.RunNoteAdd(ctx, env, verb.RunNoteAddInput{Run: run, Authority: authority, Text: unread})
		requireVerbCode(t, err, output.ErrValidation, "--authority")
	}

	coded := &verb.Error{Code: output.ErrNotFound, Err: errors.New("note file n.md not found")}
	_, err = verb.RunNoteAdd(ctx, env, verb.RunNoteAddInput{
		Run: run, Authority: engine.AuthorityOperator, ConductorToken: tokenOf(token),
		Text: func() (string, error) { return "", coded },
	})
	if err != coded {
		t.Errorf("error = %v, want the text source's *verb.Error unchanged", err)
	}

	sourceErr := errors.New("reading the note failed")
	_, err = verb.RunNoteAdd(ctx, env, verb.RunNoteAddInput{
		Run: run, Authority: engine.AuthorityOperator, ConductorToken: tokenOf(token),
		Text: func() (string, error) { return "", sourceErr },
	})
	requireCode(t, err, output.ErrGeneral)
	if !errors.Is(err, sourceErr) {
		t.Errorf("error = %v, want it to wrap the text source's error", err)
	}
}

// Validate reports the faults of the input alone, as the verb does, for a
// transport whose attribution failed first.
func TestRunFactAndNoteAddValidateTheirInput(t *testing.T) {
	for name, err := range map[string]error{
		"fact run":       verb.RunFactAddInput{Run: "RUN-x"}.Validate(),
		"fact step":      verb.RunFactAddInput{Run: "RUN-1", Step: "STEP-x"}.Validate(),
		"fact proposal":  verb.RunFactAddInput{Run: "RUN-1", Proposal: "PROPOSAL-x"}.Validate(),
		"note run":       verb.RunNoteAddInput{Run: "RUN-x", Authority: engine.AuthorityOperator}.Validate(),
		"note authority": verb.RunNoteAddInput{Run: "RUN-1", Authority: "standing-grant"}.Validate(),
	} {
		var ve *verb.Error
		if !errors.As(err, &ve) || ve.Code != output.ErrValidation {
			t.Errorf("%s: Validate = %v, want a VALIDATION_ERROR *verb.Error", name, err)
		}
	}
	if err := (verb.RunFactAddInput{Run: "RUN-1", Step: "STEP-2", Proposal: model.FormatProposalID(3)}).Validate(); err != nil {
		t.Errorf("fact: Validate = %v on a well-formed input", err)
	}
	if err := (verb.RunNoteAddInput{Run: "RUN-1", Authority: engine.AuthorityConductor}).Validate(); err != nil {
		t.Errorf("note: Validate = %v on a well-formed input", err)
	}
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
	pinDriftedFile(t, st, runID, ref)

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

// pinDriftedFile pins ref on runID at bytes other than the ones now on disk
// under the store's config root.
func pinDriftedFile(t *testing.T, st store.Store, runID int, ref string) {
	t.Helper()
	path := filepath.Join(os.Getenv("DOCKET_PATH"), "config", ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("AFTER\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	insertFilePin(t, st, runID, ref)
}

// insertFilePin pins ref on runID at bytes no file under the config root
// holds.
func insertFilePin(t *testing.T, st store.Store, runID int, ref string) {
	t.Helper()
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
}

// The run reads observe: an anchored identity no project holds is answered
// as an empty project and never registered.
func TestRunReadsNeverRegisterTheCallersProject(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	run, _, _ := activeRun(t, st)
	env := projectEnv(st, "/repos/caller", true)

	list, err := verb.RunStatus(ctx, env, verb.RunStatusInput{All: true})
	if err != nil {
		t.Fatalf("RunStatus list: %v", err)
	}
	if len(list.Runs) != 0 || list.Total != 0 {
		t.Errorf("list = %+v, want no runs in an unregistered project", list)
	}
	if _, err := verb.RunStatus(ctx, env, verb.RunStatusInput{Run: run}); err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if _, err := verb.RunReport(ctx, env, verb.RunReportInput{Run: run}); err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if _, err := verb.RunVerifyPins(ctx, env, verb.RunVerifyPinsInput{Run: run}); err != nil {
		t.Fatalf("RunVerifyPins: %v", err)
	}
	if _, err := verb.RunNoteList(ctx, env, verb.RunNoteListInput{Run: run}); err != nil {
		t.Fatalf("RunNoteList: %v", err)
	}
	requireUnregistered(t, st, "/repos/caller")
}

// A bare list hides done and abandoned runs, All shows them, and Limit pages
// the list while Total still counts every match.
func TestRunStatusListsTheCallersRuns(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	live, _, _ := runAt(t, st, model.RunActive)
	ended, _, _ := runAt(t, st, model.RunAbandoned)
	env := projectEnv(st, "", false)

	refs := func(runs []*model.Run) []string {
		out := make([]string, 0, len(runs))
		for _, r := range runs {
			out = append(out, r.Ref())
		}
		return out
	}

	bare, err := verb.RunStatus(ctx, env, verb.RunStatusInput{})
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if got := refs(bare.Runs); len(got) != 1 || got[0] != live || bare.Total != 1 || bare.Detail != nil {
		t.Errorf("bare list = %v total %d detail %v, want %s alone", got, bare.Total, bare.Detail, live)
	}

	all, err := verb.RunStatus(ctx, env, verb.RunStatusInput{All: true})
	if err != nil {
		t.Fatalf("RunStatus --all: %v", err)
	}
	if got := strings.Join(refs(all.Runs), " "); all.Total != 2 ||
		!strings.Contains(got, live) || !strings.Contains(got, ended) {
		t.Errorf("--all list = %s total %d, want %s and %s", got, all.Total, live, ended)
	}

	page, err := verb.RunStatus(ctx, env, verb.RunStatusInput{All: true, Limit: 1})
	if err != nil {
		t.Fatalf("RunStatus --limit: %v", err)
	}
	if len(page.Runs) != 1 || page.Total != 2 {
		t.Errorf("limited list = %d run(s) total %d, want 1 of 2", len(page.Runs), page.Total)
	}
}

func TestRunStatusRefusesAllWithARun(t *testing.T) {
	st := openStore(t)
	_, err := verb.RunStatus(context.Background(), projectEnv(st, "", false),
		verb.RunStatusInput{Run: "RUN-1", All: true})
	requireVerbCode(t, err, output.ErrValidation, "--all widens a list")
}

// A caller that names one run by an empty ref is refused, never answered with
// the list an empty Run alone asks for.
func TestRunStatusRefusesAnEmptyNamedRun(t *testing.T) {
	st := openStore(t)
	runAt(t, st, model.RunActive)
	env := projectEnv(st, "", false)
	ctx := context.Background()

	res, err := verb.RunStatus(ctx, env, verb.RunStatusInput{OneRun: true})
	requireVerbCode(t, err, output.ErrValidation, "empty run ID")
	if res.Runs != nil || res.Detail != nil {
		t.Errorf("result = %+v, want nothing beside the refusal", res)
	}
	_, err = verb.RunStatus(ctx, env, verb.RunStatusInput{OneRun: true, All: true})
	requireVerbCode(t, err, output.ErrValidation, "--all widens a list")
}

// One run's status carries its issue count, its step rollup and its pins,
// and the drift among those pins while the run is live.
func TestRunStatusShowsOneRunsRollupAndDrift(t *testing.T) {
	st := openStore(t)
	run, runID, _ := activeRun(t, st)
	const ref = "contracts/drifted.md"
	pinDriftedFile(t, st, runID, ref)

	res, err := verb.RunStatus(context.Background(), projectEnv(st, "", false), verb.RunStatusInput{Run: run})
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	d := res.Detail
	if d == nil || d.Run.Ref() != run || d.Issues != 1 || len(d.Steps) == 0 || res.Runs != nil {
		t.Fatalf("result = %+v, want %s's rollup with one issue and its steps", res, run)
	}
	var pinned bool
	for _, p := range d.Pins {
		pinned = pinned || p.Ref == ref
	}
	if !pinned {
		t.Errorf("pins = %+v, want %s among them", d.Pins, ref)
	}
	if len(d.PinDrift) != 1 || d.PinDrift[0].Ref != ref {
		t.Errorf("PinDrift = %+v, want the one drifted pin %s", d.PinDrift, ref)
	}
}

// A parked run still reads its drift, so a resuming conductor sees it first.
func TestRunStatusStatesDriftOnAParkedRun(t *testing.T) {
	st := openStore(t)
	run, runID, _ := runAt(t, st, model.RunWaitingHuman)
	const ref = "contracts/drifted.md"
	pinDriftedFile(t, st, runID, ref)

	res, err := verb.RunStatus(context.Background(), projectEnv(st, "", false), verb.RunStatusInput{Run: run})
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if res.Detail == nil || len(res.Detail.PinDrift) != 1 || res.Detail.PinDrift[0].Ref != ref {
		t.Errorf("result = %+v, want the parked run's one drifted pin %s", res.Detail, ref)
	}
}

// A terminal run's pins are history, so its status reads no drift.
func TestRunStatusChecksNoDriftOnATerminalRun(t *testing.T) {
	st := openStore(t)
	run, runID, _ := runAt(t, st, model.RunAbandoned)
	pinDriftedFile(t, st, runID, "contracts/drifted.md")

	res, err := verb.RunStatus(context.Background(), projectEnv(st, "", false), verb.RunStatusInput{Run: run})
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if res.Detail == nil || len(res.Detail.PinDrift) != 0 {
		t.Errorf("result = %+v, want a terminal run's status with no drift", res.Detail)
	}
}

func TestRunReportReportsARunInAnyStatus(t *testing.T) {
	st := openStore(t)
	run := plannedRun(t, st)

	res, err := verb.RunReport(context.Background(), projectEnv(st, "", false), verb.RunReportInput{Run: run})
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if res.Report == nil || res.Report.Run.Ref() != run || res.Report.Run.Status != model.RunPlanning {
		t.Errorf("report = %+v, want the planning run %s", res.Report, run)
	}
}

// A sound pin set is a plain result; a drifted one is refused as CONFLICT
// naming the repin remedy, with the report still returned beside the error.
func TestRunVerifyPinsRefusesAnUnsoundPinSetWithItsReport(t *testing.T) {
	st := openStore(t)
	run, runID, _ := activeRun(t, st)
	env := projectEnv(st, "", false)
	ctx := context.Background()

	sound, err := verb.RunVerifyPins(ctx, env, verb.RunVerifyPinsInput{Run: run})
	if err != nil {
		t.Fatalf("RunVerifyPins on a sound run: %v", err)
	}
	if sound.Report == nil || !sound.Report.Sound() {
		t.Fatalf("report = %+v, want a sound pin set", sound.Report)
	}

	pinDriftedFile(t, st, runID, "contracts/drifted.md")
	drifted, err := verb.RunVerifyPins(ctx, env, verb.RunVerifyPinsInput{Run: run})
	requireVerbCode(t, err, output.ErrConflict, "docket run repin "+run)
	if drifted.Report == nil || drifted.Report.Changed != 1 {
		t.Errorf("report = %+v, want the one changed pin beside the refusal", drifted.Report)
	}
}

// A pin whose file is gone has no bytes a repin could adopt, so the refusal
// is NOT_FOUND and names restore or abandon instead.
func TestRunVerifyPinsRefusesAMissingPinAsNotFound(t *testing.T) {
	st := openStore(t)
	run, runID, _ := activeRun(t, st)
	insertFilePin(t, st, runID, "contracts/gone.md")

	res, err := verb.RunVerifyPins(context.Background(), projectEnv(st, "", false),
		verb.RunVerifyPinsInput{Run: run})
	requireVerbCode(t, err, output.ErrNotFound, "restore the file(s) or abandon the run")
	if strings.Contains(err.Error(), "docket run repin") {
		t.Errorf("error = %q names repin, which has no bytes to adopt", err)
	}
	if res.Report == nil || res.Report.Missing != 1 {
		t.Errorf("report = %+v, want the one missing pin beside the refusal", res.Report)
	}
}

// packetWorkflow's one step reads contracts/judge.md, so activation pins
// that contract and the closure its packet_includes can see.
const packetWorkflow = `
[pipeline]
name = "unit-packet"
version = 1
[match]
kind = ["task"]
[[step]]
name = "first"
after = []
executor = "someone"
emits = "result"
packet = ["contracts/judge.md"]
`

// A pin set that froze without a ref its pinned contract includes is refused
// with the code the blocked claim returns, VALIDATION_ERROR, and a remedy
// that starts a new run: nothing drifted, so repin has nothing to adopt.
func TestRunVerifyPinsRefusesAnOpenClosureAsValidation(t *testing.T) {
	st := openStore(t)
	run := plannedRunOf(t, st, packetWorkflow)
	configDir := filepath.Join(os.Getenv("DOCKET_PATH"), "config")
	write := func(rel, body string) {
		path := filepath.Join(configDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("contracts/judge.md", "---\npacket_includes:\n  - fragments/ladder.md\n---\nthe judge contract\n")
	env := projectEnv(st, "", false)
	ctx := context.Background()
	if _, err := verb.RunActivate(ctx, env, verb.RunActivateInput{Run: run}); err != nil {
		t.Fatalf("RunActivate: %v", err)
	}
	// The fragment lands after the pin set froze.
	write("fragments/ladder.md", "the laziness ladder\n")

	res, err := verb.RunVerifyPins(ctx, env, verb.RunVerifyPinsInput{Run: run})
	requireVerbCode(t, err, output.ErrValidation, "start a new run to pin them")
	for _, want := range []string{"contracts/judge.md", "fragments/ladder.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "docket run repin") {
		t.Errorf("error = %q names repin, which has no drift to adopt", err)
	}
	if res.Report == nil || res.Report.Unpinned != 1 || res.Report.Changed != 0 || res.Report.Missing != 0 {
		t.Errorf("report = %+v, want only the one unpinned reference", res.Report)
	}
}

// runIssueIDs is the ids of the issues attached to runID, in attach order.
func runIssueIDs(t *testing.T, st store.Store, runID int) []int {
	t.Helper()
	attached, err := st.ListRunIssues(runID)
	if err != nil {
		t.Fatalf("ListRunIssues: %v", err)
	}
	ids := make([]int, 0, len(attached))
	for _, ri := range attached {
		ids = append(ids, ri.IssueID)
	}
	return ids
}

func TestRunIssueAddAndRemoveChangeAPlanningRunsSet(t *testing.T) {
	st := openStore(t)
	run, runID, _ := runAt(t, st, model.RunPlanning)
	first := runIssueIDs(t, st, runID)[0]
	second := createIssue(t, st)
	env := projectEnv(st, "", false)
	ctx := context.Background()

	added, err := verb.RunIssueAdd(ctx, env, verb.RunIssueAddInput{Run: run, Issues: []string{model.FormatID(second)}})
	if err != nil {
		t.Fatalf("RunIssueAdd: %v", err)
	}
	if added.Run == nil || added.Run.ID != runID || !slices.Equal(added.Added, []int{second}) {
		t.Errorf("added = %+v, want %s gaining issue %d", added, run, second)
	}
	if want := []int{first, second}; !slices.Equal(added.Issues, want) || !slices.Equal(runIssueIDs(t, st, runID), want) {
		t.Errorf("issues = %v (stored %v), want %v", added.Issues, runIssueIDs(t, st, runID), want)
	}

	removed, err := verb.RunIssueRemove(ctx, env, verb.RunIssueRemoveInput{Run: run, Issues: []string{model.FormatID(first)}})
	if err != nil {
		t.Fatalf("RunIssueRemove: %v", err)
	}
	if !slices.Equal(removed.Removed, []int{first}) {
		t.Errorf("removed = %+v, want issue %d removed", removed, first)
	}
	if want := []int{second}; !slices.Equal(removed.Issues, want) || !slices.Equal(runIssueIDs(t, st, runID), want) {
		t.Errorf("issues = %v (stored %v), want %v", removed.Issues, runIssueIDs(t, st, runID), want)
	}
}

// An add checks every issue before writing any, so one bad id in the set
// leaves the run's issues as they were.
func TestRunIssueAddRefusesTheWholeSetBeforeWriting(t *testing.T) {
	st := openStore(t)
	run, runID, _ := runAt(t, st, model.RunPlanning)
	before := runIssueIDs(t, st, runID)
	fresh := model.FormatID(createIssue(t, st))
	// The first identity registered claims the default project's row.
	if _, _, err := st.EnsureProjectCreated("/repos/default", "default", model.NowMS()); err != nil {
		t.Fatalf("EnsureProjectCreated: %v", err)
	}
	otherProject, _, err := st.EnsureProjectCreated("/repos/other", "other", model.NowMS())
	if err != nil || otherProject == store.DefaultProjectID {
		t.Fatalf("EnsureProjectCreated = %d, %v; want a project other than the default", otherProject, err)
	}
	// The foreign issue is named in its owner's prefix, not the caller's.
	if err := st.SetProjectPrefix(otherProject, "OTH"); err != nil {
		t.Fatalf("SetProjectPrefix: %v", err)
	}
	foreign, _, err := st.CreateIssueIdempotent(&model.Issue{
		ProjectID: otherProject, Title: "elsewhere", Status: model.StatusTodo,
		Priority: model.PriorityMedium, Kind: model.IssueKindTask,
	}, nil, nil, "")
	if err != nil {
		t.Fatalf("CreateIssueIdempotent: %v", err)
	}
	env := projectEnv(st, "", false)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		in     verb.RunIssueAddInput
		code   output.ErrorCode
		reason string
	}{
		{"run ref", verb.RunIssueAddInput{Run: "RUN-x", Issues: []string{fresh}}, output.ErrValidation, `invalid run ID "RUN-x"`},
		{"unknown run", verb.RunIssueAddInput{Run: "RUN-42", Issues: []string{fresh}}, output.ErrNotFound, "run RUN-42 not found"},
		{"issue ref", verb.RunIssueAddInput{Run: run, Issues: []string{fresh, "nope"}}, output.ErrValidation, `invalid issue ID "nope"`},
		{"unknown issue", verb.RunIssueAddInput{Run: run, Issues: []string{fresh, "DKT-99"}}, output.ErrNotFound, "issue DKT-99 not found"},
		{"other project", verb.RunIssueAddInput{Run: run, Issues: []string{fresh, model.FormatID(foreign)}},
			output.ErrValidation, fmt.Sprintf(`issue OTH-%d belongs to project "other" (id %d), run %s to project "default" (id %d)`,
				foreign, otherProject, run, store.DefaultProjectID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verb.RunIssueAdd(ctx, env, tc.in)
			requireVerbCode(t, err, tc.code, tc.reason)
			if got := runIssueIDs(t, st, runID); !slices.Equal(got, before) {
				t.Fatalf("issues = %v after a refused add, want %v", got, before)
			}
		})
	}
}

func TestRunIssueAddTakesOnlyAPlanningOrActiveRun(t *testing.T) {
	ctx := context.Background()
	for _, status := range []model.RunStatus{model.RunActive, model.RunWaitingHuman, model.RunAbandoned} {
		st := openStore(t)
		run, runID, _ := runAt(t, st, status)
		issue := createIssue(t, st)
		_, err := verb.RunIssueAdd(ctx, projectEnv(st, "", false), verb.RunIssueAddInput{
			Run: run, Issues: []string{model.FormatID(issue)},
		})
		attached := runIssueIDs(t, st, runID)
		if status == model.RunActive {
			if err != nil || attached[len(attached)-1] != issue {
				t.Errorf("active: RunIssueAdd = %v, issues %v; want issue %d attached", err, attached, issue)
			}
			continue
		}
		requireVerbCode(t, err, output.ErrConflict, "issues can be added while a run is planning or active")
		if len(attached) != 1 {
			t.Errorf("%s: issues = %v, want the refused add to attach nothing", status, attached)
		}
	}
}

// A remove checks membership for the whole set first, so a typo'd second id
// detaches nothing; a run past planning refuses outright.
func TestRunIssueRemoveRefusesBeforeDetachingAny(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	run, runID, _ := runAt(t, st, model.RunPlanning)
	before := runIssueIDs(t, st, runID)

	_, err := verb.RunIssueRemove(ctx, projectEnv(st, "", false), verb.RunIssueRemoveInput{
		Run: run, Issues: []string{model.FormatID(before[0]), "DKT-99"},
	})
	requireVerbCode(t, err, output.ErrNotFound, "issue DKT-99 is not attached to "+run)
	if got := runIssueIDs(t, st, runID); !slices.Equal(got, before) {
		t.Errorf("issues = %v after a refused remove, want %v", got, before)
	}

	st = openStore(t)
	run, runID, _ = runAt(t, st, model.RunActive)
	before = runIssueIDs(t, st, runID)
	_, err = verb.RunIssueRemove(ctx, projectEnv(st, "", false), verb.RunIssueRemoveInput{
		Run: run, Issues: []string{model.FormatID(before[0])},
	})
	requireVerbCode(t, err, output.ErrConflict, "issues can be removed only while a run is planning")
	if got := runIssueIDs(t, st, runID); !slices.Equal(got, before) {
		t.Errorf("issues = %v after a refused remove, want %v", got, before)
	}
}

// A refused id no project owns is named in the caller's project's prefix, the
// voice the rest of its board reads in.
func TestRunIssueRefusalsNameAnUnownedIdInTheCallersPrefix(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	run, _, _ := runAt(t, st, model.RunPlanning)
	if err := st.SetProjectPrefix(store.DefaultProjectID, "VOR"); err != nil {
		t.Fatalf("SetProjectPrefix: %v", err)
	}
	env := projectEnv(st, "", false)

	_, err := verb.RunIssueAdd(ctx, env, verb.RunIssueAddInput{Run: run, Issues: []string{"DKT-99"}})
	requireVerbCode(t, err, output.ErrNotFound, "issue VOR-99 not found")

	_, err = verb.RunIssueRemove(ctx, env, verb.RunIssueRemoveInput{Run: run, Issues: []string{"DKT-77"}})
	requireVerbCode(t, err, output.ErrNotFound, "issue VOR-77 is not attached to "+run)
}

// The run issue verbs and repin write on the caller's behalf, so an
// unanchored caller is refused before its run reference is read, and an
// anchored one is registered.
func TestRunIssueAndRepinVerbsMayRegisterTheCallersProject(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(env verb.Env) (*engine.ProjectRegistration, error){
		"issue add": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunIssueAdd(ctx, env, verb.RunIssueAddInput{Run: "RUN-x", Issues: []string{"DKT-1"}})
			return res.Registered, err
		},
		"issue remove": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunIssueRemove(ctx, env, verb.RunIssueRemoveInput{Run: "RUN-x", Issues: []string{"DKT-1"}})
			return res.Registered, err
		},
		"repin": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunRepin(ctx, env, verb.RunRepinInput{Run: "RUN-x", Reason: "drift"})
			return res.Registered, err
		},
		"refresh-body": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunRefreshBody(ctx, env, verb.RunRefreshBodyInput{Run: "RUN-x", Issue: "DKT-1", Reason: "r"})
			return res.Registered, err
		},
		"refresh-files": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunRefreshFiles(ctx, env, verb.RunRefreshFilesInput{Run: "RUN-x", Issue: "DKT-1", Reason: "r"})
			return res.Registered, err
		},
		"refresh-scope": func(env verb.Env) (*engine.ProjectRegistration, error) {
			res, err := verb.RunRefreshScope(ctx, env, verb.RunRefreshScopeInput{Run: "RUN-x", Issue: "DKT-1", Reason: "r"})
			return res.Registered, err
		},
	}
	for name, call := range calls {
		st := openStore(t)
		_, err := call(projectEnv(st, "/tmp/scratch", false))
		requireVerbCode(t, err, output.ErrValidation, "cannot tell which project this belongs to")
		requireUnregistered(t, st, "/tmp/scratch")

		registered, err := call(projectEnv(st, "/repos/caller", true))
		requireVerbCode(t, err, output.ErrValidation, `invalid run ID "RUN-x"`)
		if registered == nil || registered.Identity != "/repos/caller" || registered.Verb != "run "+name {
			t.Errorf("%s: Registered = %+v, want the caller's project registered by run %s", name, registered, name)
		}
	}
}

// A repin of a drifted pin needs a reason and the run's conductor capability,
// and adopts the bytes on disk once it has both.
func TestRunRepinRequiresAReasonAndTheConductorToken(t *testing.T) {
	st := openStore(t)
	run := plannedRunOf(t, st, packetWorkflow)
	contract := filepath.Join(os.Getenv("DOCKET_PATH"), "config", "contracts", "judge.md")
	if err := os.MkdirAll(filepath.Dir(contract), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(contract, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	env := projectEnv(st, "", false)
	ctx := context.Background()
	activated, err := verb.RunActivate(ctx, env, verb.RunActivateInput{Run: run})
	if err != nil {
		t.Fatalf("RunActivate: %v", err)
	}
	token := activated.Activation.ConductorToken
	if err := os.WriteFile(contract, []byte("after\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	runID, err := model.ParseRunID(run)
	if err != nil {
		t.Fatalf("ParseRunID: %v", err)
	}
	pinned := pinSHA(t, st, runID, "contracts/judge.md")

	_, err = verb.RunRepin(ctx, env, verb.RunRepinInput{Run: run, ConductorToken: tokenOf(token)})
	requireVerbCode(t, err, output.ErrValidation, "--reason is required to repin a run")

	// A wrong token is refused as AUTH_ERROR; a missing one asks for it. A
	// refused repin moves no pin and records no run-repinned event.
	for _, tc := range []struct {
		name      string
		presented func() string
		code      output.ErrorCode
	}{
		{"wrong", tokenOf("wrong"), output.ErrAuth},
		{"empty", tokenOf(""), output.ErrValidation},
		{"absent", nil, output.ErrValidation},
	} {
		_, err = verb.RunRepin(ctx, env, verb.RunRepinInput{Run: run, Reason: "drift", ConductorToken: tc.presented})
		if err == nil {
			t.Fatalf("%s token: RunRepin accepted the repin", tc.name)
		}
		requireCode(t, err, tc.code)
		if got := pinSHA(t, st, runID, "contracts/judge.md"); got != pinned {
			t.Errorf("%s token: pin sha = %s after a refused repin, want %s", tc.name, got, pinned)
		}
		if n := runRepinnedEvents(t, st, runID); n != 0 {
			t.Errorf("%s token: %d run-repinned events after a refused repin, want none", tc.name, n)
		}
	}

	res, err := verb.RunRepin(ctx, env, verb.RunRepinInput{Run: run, Reason: "drift", ConductorToken: tokenOf(token)})
	if err != nil {
		t.Fatalf("RunRepin: %v", err)
	}
	if res.Outcome == nil || res.Outcome.Run != run || len(res.Outcome.Repinned) != 1 ||
		res.Outcome.Repinned[0].Ref != "contracts/judge.md" {
		t.Errorf("outcome = %+v, want %s repinning contracts/judge.md", res.Outcome, run)
	}
	if got := pinSHA(t, st, runID, "contracts/judge.md"); got == pinned {
		t.Errorf("pin sha = %s after the repin, want the bytes on disk adopted", got)
	}
	if n := runRepinnedEvents(t, st, runID); n != 1 {
		t.Errorf("%d run-repinned events after the repin, want one", n)
	}
}

// A pinned file missing from disk has no bytes to adopt, so the repin refuses
// it unless Drop names its ref or DropUnresolvable covers it; either one
// retires a pin no step reads.
func TestRunRepinDropsAnUnresolvablePinOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		in   verb.RunRepinInput
	}{
		{"drop", verb.RunRepinInput{Drop: []string{"contracts/retired.md"}}},
		{"drop-unresolvable", verb.RunRepinInput{DropUnresolvable: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			run := plannedRunOf(t, st, packetWorkflow)
			contract := filepath.Join(os.Getenv("DOCKET_PATH"), "config", "contracts", "judge.md")
			if err := os.MkdirAll(filepath.Dir(contract), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(contract, []byte("before\n"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			env := projectEnv(st, "", false)
			activated, err := verb.RunActivate(ctx, env, verb.RunActivateInput{Run: run})
			if err != nil {
				t.Fatalf("RunActivate: %v", err)
			}
			runID, err := model.ParseRunID(run)
			if err != nil {
				t.Fatalf("ParseRunID: %v", err)
			}
			// A pin no step reads, on a file that is not on disk.
			insertFilePin(t, st, runID, "contracts/retired.md")
			token := tokenOf(activated.Activation.ConductorToken)

			_, err = verb.RunRepin(ctx, env, verb.RunRepinInput{Run: run, Reason: "deleted", ConductorToken: token})
			requireVerbCode(t, err, output.ErrNotFound, "--drop")

			in := tc.in
			in.Run, in.Reason, in.ConductorToken = run, "deleted", token
			res, err := verb.RunRepin(ctx, env, in)
			if err != nil {
				t.Fatalf("RunRepin(%s): %v", tc.name, err)
			}
			if res.Outcome == nil || len(res.Outcome.Dropped) != 1 || res.Outcome.Dropped[0].Ref != "contracts/retired.md" {
				t.Errorf("outcome = %+v, want contracts/retired.md dropped", res.Outcome)
			}
			if got := pinSHA(t, st, runID, "contracts/retired.md"); got != "" {
				t.Errorf("pin sha = %s after the drop, want the pin retired", got)
			}
		})
	}
}

// pinSHA is the sha runID's pin on ref records, or "" when it has none.
func pinSHA(t *testing.T, st store.Store, runID int, ref string) string {
	t.Helper()
	pins, err := st.ListPins(runID)
	if err != nil {
		t.Fatalf("ListPins: %v", err)
	}
	for _, p := range pins {
		if p.Ref == ref {
			return p.SHA256
		}
	}
	return ""
}

// runRepinnedEvents counts runID's run-repinned events.
func runRepinnedEvents(t *testing.T, st store.Store, runID int) int {
	t.Helper()
	page, err := engine.ListEventsStore(st, engine.EventQuery{RunID: runID, Kind: engine.EventRunRepinned})
	if err != nil {
		t.Fatalf("ListEventsStore: %v", err)
	}
	return len(page.Events)
}

// snapshotRefresh is one of the three run refresh verbs, reduced to what the
// shared contract checks: the issue its outcome names and the steps it reached.
type snapshotRefresh struct {
	name string
	// amend changes what issue declares, so the refresh has something to copy.
	amend   func(t *testing.T, st store.Store, issue int)
	refresh func(env verb.Env, run, issue, reason string, token func() string) (string, []string, error)
}

var snapshotRefreshes = []snapshotRefresh{
	{
		name: "refresh-body",
		amend: func(t *testing.T, st store.Store, issue int) {
			t.Helper()
			if err := st.UpdateIssueCAS(issue, map[string]interface{}{"description": "AC1: amended"}, "operator", nil); err != nil {
				t.Fatalf("UpdateIssueCAS: %v", err)
			}
		},
		refresh: func(env verb.Env, run, issue, reason string, token func() string) (string, []string, error) {
			res, err := verb.RunRefreshBody(context.Background(), env, verb.RunRefreshBodyInput{
				Run: run, Issue: issue, Reason: reason, ConductorToken: token,
			})
			if res.Outcome == nil {
				return "", nil, err
			}
			return res.Outcome.Issue, res.Outcome.Steps, err
		},
	},
	{
		name: "refresh-files",
		amend: func(t *testing.T, st store.Store, issue int) {
			t.Helper()
			if err := st.AttachFiles(issue, []string{"docs/attached.md"}, "operator"); err != nil {
				t.Fatalf("AttachFiles: %v", err)
			}
		},
		refresh: func(env verb.Env, run, issue, reason string, token func() string) (string, []string, error) {
			res, err := verb.RunRefreshFiles(context.Background(), env, verb.RunRefreshFilesInput{
				Run: run, Issue: issue, Reason: reason, ConductorToken: token,
			})
			if res.Outcome == nil {
				return "", nil, err
			}
			return res.Outcome.Issue, res.Outcome.Steps, err
		},
	},
	{
		name: "refresh-scope",
		amend: func(t *testing.T, st store.Store, issue int) {
			t.Helper()
			if err := st.SetIssueScopeGlobs(issue, `["a/**"]`); err != nil {
				t.Fatalf("SetIssueScopeGlobs: %v", err)
			}
		},
		refresh: func(env verb.Env, run, issue, reason string, token func() string) (string, []string, error) {
			res, err := verb.RunRefreshScope(context.Background(), env, verb.RunRefreshScopeInput{
				Run: run, Issue: issue, Reason: reason, ConductorToken: token,
			})
			if res.Outcome == nil {
				return "", nil, err
			}
			return res.Outcome.Issue, res.Outcome.Steps, err
		},
	},
}

// Each refresh names one issue and a reason, refuses a wrong or missing
// conductor capability on a bound run and an unattributed caller, and with
// all of them moves that issue's snapshot for its remaining steps.
func TestRunRefreshVerbsMoveOneIssuesSnapshotUnderTheCapability(t *testing.T) {
	for _, r := range snapshotRefreshes {
		t.Run(r.name, func(t *testing.T) {
			st := openStore(t)
			run, runID, token := activeRun(t, st)
			issueID := runIssueIDs(t, st, runID)[0]
			issue := model.FormatID(issueID)
			r.amend(t, st, issueID)
			env := projectEnv(st, "", false)
			env.Author = "operator"

			_, _, err := r.refresh(env, run, "", "amended", tokenOf(token))
			requireVerbCode(t, err, output.ErrValidation, "--issue is required")
			_, _, err = r.refresh(env, run, "DKT-x", "amended", tokenOf(token))
			requireVerbCode(t, err, output.ErrValidation, "invalid issue ID")
			_, _, err = r.refresh(env, run, issue, "", tokenOf(token))
			requireVerbCode(t, err, output.ErrValidation, "--reason is required")

			for _, tc := range []struct {
				name      string
				presented func() string
				code      output.ErrorCode
			}{
				{"wrong", tokenOf("wrong"), output.ErrAuth},
				{"empty", tokenOf(""), output.ErrValidation},
				{"absent", nil, output.ErrValidation},
			} {
				_, _, err := r.refresh(env, run, issue, "amended", tc.presented)
				if err == nil {
					t.Fatalf("%s token: the refresh was accepted", tc.name)
				}
				requireCode(t, err, tc.code)
			}

			_, _, err = r.refresh(projectEnv(st, "", false), run, issue, "amended", tokenOf(token))
			requireVerbCode(t, err, output.ErrGeneral, "refusing to record an unattributed run "+r.name)

			got, steps, err := r.refresh(env, run, issue, "amended", tokenOf(token))
			if err != nil {
				t.Fatalf("%s: %v", r.name, err)
			}
			if got != issue || len(steps) == 0 {
				t.Errorf("outcome names %s reaching %v, want %s reaching its remaining steps", got, steps, issue)
			}
		})
	}
}

// Every refresh input check runs through Validate, so a transport whose
// attribution fails can still report the request's own fault first.
func TestRunRefreshInputsValidateTheirReferences(t *testing.T) {
	for name, validate := range map[string]func(run, issue, reason string) error{
		"refresh-body": func(run, issue, reason string) error {
			return verb.RunRefreshBodyInput{Run: run, Issue: issue, Reason: reason}.Validate()
		},
		"refresh-files": func(run, issue, reason string) error {
			return verb.RunRefreshFilesInput{Run: run, Issue: issue, Reason: reason}.Validate()
		},
		"refresh-scope": func(run, issue, reason string) error {
			return verb.RunRefreshScopeInput{Run: run, Issue: issue, Reason: reason}.Validate()
		},
	} {
		requireVerbCode(t, validate("RUN-x", "DKT-1", "r"), output.ErrValidation, `invalid run ID "RUN-x"`)
		requireVerbCode(t, validate("RUN-1", "", "r"), output.ErrValidation, "--issue is required")
		requireVerbCode(t, validate("RUN-1", "DKT-1", ""), output.ErrValidation, "--reason is required")
		if err := validate("RUN-1", "DKT-1", "r"); err != nil {
			t.Errorf("%s: Validate of a well-formed request = %v, want nil", name, err)
		}
	}
}
