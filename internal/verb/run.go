package verb

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// RunStartInput is a request to create a run in planning.
type RunStartInput struct {
	Request string
	// Issues are the references of the issues to attach.
	Issues []string
	// Budget caps declared step cost and UsageBudget caps measured usage.
	// Either one nil inherits its configured default (budget.default,
	// budget.usage.default); an explicit 0 is unlimited.
	Budget      *float64
	UsageBudget *float64
	// ExecRoot, Branch, CommitSHA and Hostname record where the run was
	// started, each empty when the caller could not tell.
	ExecRoot  string
	Branch    string
	CommitSHA string
	Hostname  string
	// IdempotencyKey, when set, makes a repeated start return the original
	// run unchanged; a key reused with a different request, budget, context
	// or issue list is a conflict.
	IdempotencyKey string
}

// RunStartResult is the started run.
type RunStartResult struct {
	Run *model.Run
	// Inserted is false when IdempotencyKey replayed an earlier start.
	Inserted bool
	// Registered is the project this call registered, if it registered one,
	// for the caller to record its project-registered event. It is set even
	// when the call also returns an error: read it before checking the error.
	Registered *engine.ProjectRegistration
}

// RunStart creates a run in the caller's project with its issues attached.
// The run is created there, so it may register an anchored project.
//
// Each cap is resolved here and stored on the run row, which is the only place
// a claim reads it from, so a config change after the start cannot re-cap a
// live run.
func RunStart(ctx context.Context, env Env, in RunStartInput) (RunStartResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "run start")
	res := RunStartResult{Registered: registered}
	if err != nil {
		return res, err
	}

	if err := checkCap(in.Budget, "--budget"); err != nil {
		return res, err
	}

	// Issues are resolved before the run is created, so a typo'd id refuses
	// without leaving an empty run behind. Ids are store-wide, and activation
	// binds a run's issues against its own project's registry, so an issue
	// homed elsewhere is refused (DKT-21).
	issueIDs := make([]int, 0, len(in.Issues))
	for _, ref := range in.Issues {
		id, err := model.ParseID(ref)
		if err != nil {
			return res, newError(fmt.Errorf("invalid issue ID %q: %w", ref, err), output.ErrValidation)
		}
		issueProject, err := env.Store.IssueProjectID(id)
		if errors.Is(err, store.ErrNotFound) {
			prefix, _ := env.Store.ProjectPrefix(projectID)
			return res, newError(
				fmt.Errorf("issue %s not found", model.FormatIDWithPrefix(id, prefix)), output.ErrNotFound)
		}
		if err != nil {
			return res, newError(fmt.Errorf("checking issue %s: %w", ref, err), output.ErrGeneral)
		}
		if issueProject != projectID {
			return res, newError(fmt.Errorf(
				"issue %s belongs to %s, but this run would start in %s — a run's "+
					"issues live in the run's own project; migrate the issue first "+
					"with `docket issue move %s --project <target>`, or start the "+
					"run from the issue's own repository",
				model.FormatID(id), projectLabel(env.Store, issueProject),
				projectLabel(env.Store, projectID), model.FormatID(id)),
				output.ErrValidation)
		}
		issueIDs = append(issueIDs, id)
	}

	budget, err := capOrDefault(env, projectID, in.Budget, store.KeyBudgetDefault)
	if err != nil {
		return res, err
	}
	if err := checkCap(in.UsageBudget, "--usage-budget"); err != nil {
		return res, err
	}
	usageBudget, err := capOrDefault(env, projectID, in.UsageBudget, store.KeyUsageBudgetDefault)
	if err != nil {
		return res, err
	}

	run, inserted, err := env.Store.InsertRunWithContextIdempotent(
		projectID, in.Request, budget, model.NowMS(), store.RunContext{
			ExecRoot:    in.ExecRoot,
			Branch:      in.Branch,
			CommitSHA:   in.CommitSHA,
			Hostname:    in.Hostname,
			UsageBudget: usageBudget,
		}, in.IdempotencyKey, issueIDs...)
	if errors.Is(err, store.ErrConflict) {
		return res, newError(err, output.ErrConflict)
	}
	if err != nil {
		return res, engineError(err)
	}
	res.Run, res.Inserted = run, inserted
	return res, nil
}

// checkCap refuses a negative cap the caller set; flag names it in the refusal.
func checkCap(set *float64, flag string) error {
	if set != nil && *set < 0 {
		return newError(
			fmt.Errorf("%s must be >= 0, got %g (0 means unlimited)", flag, *set),
			output.ErrValidation)
	}
	return nil
}

// capOrDefault resolves one of a run's caps: the caller's value when set,
// else the project's configured default under key.
func capOrDefault(env Env, projectID int, set *float64, key string) (float64, error) {
	if set != nil {
		return *set, nil
	}
	entry, err := env.Store.GetConfig(projectID, key)
	if err != nil {
		return 0, newError(fmt.Errorf("reading %s: %w", key, err), output.ErrGeneral)
	}
	fallback, err := strconv.ParseFloat(entry.Value, 64)
	if err != nil {
		return 0, newError(
			fmt.Errorf("%s is %q, which is not a number", key, entry.Value), output.ErrValidation)
	}
	return fallback, nil
}

// projectLabel renders a project for a refusal: its name when it has one,
// always with the numeric id an operator can act on.
func projectLabel(st store.Store, id int) string {
	if p, err := st.GetProject(id); err == nil && p.Name != "" {
		return fmt.Sprintf("project %q (id %d)", p.Name, p.ID)
	}
	return fmt.Sprintf("project %d", id)
}

// RunActivateInput is a request to bind, pin, snapshot and expand a run.
type RunActivateInput struct {
	Run string
	// Pins are the paths of files to pin by content hash.
	Pins   []string
	DryRun bool
	Reason string
	// HeadBranch and HeadCommit are the activating checkout's HEAD, each
	// empty when unknown.
	HeadBranch string
	HeadCommit string
}

// RunActivateResult is what the activation did.
type RunActivateResult struct {
	Activation *engine.ActivateResult
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunActivate activates a run, or re-activates an active one to expand its
// newly unblocked phases. Activation is the CLI's write of record for a run,
// so it may register the caller's anchored project.
func RunActivate(ctx context.Context, env Env, in RunActivateInput) (RunActivateResult, error) {
	_, registered, err := resolveRegisteringProject(env, "run activate")
	res := RunActivateResult{Registered: registered}
	if err != nil {
		return res, err
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return res, newError(err, output.ErrValidation)
	}

	res.Activation, err = engine.ActivateStore(env.Store, runID, engine.ActivateOptions{
		FilePins:   in.Pins,
		NowMS:      model.NowMS(),
		DryRun:     in.DryRun,
		Reason:     in.Reason,
		HeadBranch: in.HeadBranch,
		HeadCommit: in.HeadCommit,
	})
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}

// RunConductInput names the run whose conductor seat to take.
type RunConductInput struct {
	Run string
}

// RunConductResult carries the freshly minted capability, which exists
// outside its stored hash only here.
type RunConductResult struct {
	Conduct *engine.ConductResult
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunConduct takes a run's conductor seat as env.Author from env.Cwd, minting
// a capability and retiring any standing one. It may register the caller's
// anchored project.
func RunConduct(ctx context.Context, env Env, in RunConductInput) (RunConductResult, error) {
	_, registered, err := resolveRegisteringProject(env, "run conduct")
	res := RunConductResult{Registered: registered}
	if err != nil {
		return res, err
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return res, newError(err, output.ErrValidation)
	}

	res.Conduct, err = engine.ConductRunStore(env.Store, runID, engine.ConductOptions{
		By:    engine.Attribution{Actor: env.Author, Cwd: env.Cwd},
		NowMS: model.NowMS(),
	})
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}

// RunPauseInput parks an active run at waiting-human.
type RunPauseInput struct {
	Run    string
	Reason string
	// Authority and AuthorityRef name what entitled the pause, as
	// engine.ParseAuthority reads them.
	Authority    string
	AuthorityRef string
	// ConductorToken returns the run's conductor capability. It is called
	// only when the run is bound to one, so a transport whose token channel
	// can block is never read for a run that needs no token.
	ConductorToken func() string
}

// RunPauseResult is the run as the pause left it.
type RunPauseResult struct {
	Run *model.Run
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunPause moves an active run to waiting-human under the run's conductor
// capability. The run is addressed by reference, but the verb writes on the
// caller's behalf as `run activate` does, so it may register the caller's
// anchored project.
func RunPause(ctx context.Context, env Env, in RunPauseInput) (RunPauseResult, error) {
	moved, err := moveRun(env, runPause, in.Run, in.Reason, in.Authority, in.AuthorityRef, in.ConductorToken)
	return RunPauseResult{Run: moved.run, Registered: moved.registered}, err
}

// RunResumeInput returns a parked run to active. A resume disposes of
// nothing, so it names no authority.
type RunResumeInput struct {
	Run    string
	Reason string
	// ConductorToken is as on RunPauseInput.
	ConductorToken func() string
}

// RunResumeResult is the resumed run and the pins that drifted while it was
// parked.
type RunResumeResult struct {
	Run *model.Run
	// PinDrift is the run's unsound pins, read after the resume committed. It
	// is advisory: a failed drift check leaves it empty rather than failing a
	// resume that already happened.
	PinDrift []engine.PinVerdict
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunResume moves a waiting-human run back to active under the run's
// conductor capability. Its project policy is RunPause's.
func RunResume(ctx context.Context, env Env, in RunResumeInput) (RunResumeResult, error) {
	moved, err := moveRun(env, runResume, in.Run, in.Reason, "", "", in.ConductorToken)
	res := RunResumeResult{Run: moved.run, Registered: moved.registered}
	if err != nil {
		return res, err
	}
	if drift, driftErr := engine.PinDriftStore(env.Store, moved.run.ID); driftErr == nil {
		res.PinDrift = drift
	}
	return res, nil
}

// RunAbandonInput ends a run without completing it, or, with Issue, stops one
// issue's remaining steps in it.
type RunAbandonInput struct {
	Run string
	// Issue, when set, narrows the abandon to that issue's remaining steps;
	// the run and its other issues continue.
	Issue string
	// Reason is required on both forms.
	Reason string
	// Authority and AuthorityRef are as on RunPauseInput, and required on
	// both forms.
	Authority    string
	AuthorityRef string
	// ConductorToken is as on RunPauseInput.
	ConductorToken func() string
}

// RunAbandonResult is what the abandon did: Run and Worktrees for the whole
// run, or IssueOutcome alone when the input named an issue.
type RunAbandonResult struct {
	Run *model.Run
	// Worktrees are the work roots the run's steps recorded, which no close
	// will sweep now.
	Worktrees    []string
	IssueOutcome *engine.AbandonIssueOutcome
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunAbandon abandons a run, or one issue's work in it, under the run's
// conductor capability. Its project policy is RunPause's.
func RunAbandon(ctx context.Context, env Env, in RunAbandonInput) (RunAbandonResult, error) {
	if in.Issue != "" {
		return abandonIssueInRun(env, in)
	}
	moved, err := moveRun(env, runAbandon, in.Run, in.Reason, in.Authority, in.AuthorityRef, in.ConductorToken)
	return RunAbandonResult{Run: moved.run, Worktrees: moved.worktrees, Registered: moved.registered}, err
}

// abandonIssueInRun is RunAbandon narrowed to one issue.
func abandonIssueInRun(env Env, in RunAbandonInput) (RunAbandonResult, error) {
	_, registered, err := resolveRegisteringProject(env, "run abandon")
	res := RunAbandonResult{Registered: registered}
	if err != nil {
		return res, err
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return res, newError(err, output.ErrValidation)
	}
	issueID, err := ParseIssueRef(in.Issue)
	if err != nil {
		return res, err
	}
	if in.Reason == "" {
		return res, newError(
			errors.New("--reason is required to abandon an issue's work"), output.ErrValidation)
	}
	// The authority is required on both forms (DKT-1899), so narrowing the
	// disposition to one issue cannot evade it. This form emits no
	// run-abandoned event, so the authority is validated and recorded nowhere.
	if _, err := parseAuthority(in.Authority, in.AuthorityRef); err != nil {
		return res, err
	}

	res.IssueOutcome, err = engine.AbandonIssueInRunWithStore(env.Store, engine.AbandonIssueOptions{
		RunID: runID, IssueID: issueID, Reason: in.Reason,
		Token: runConductorToken(env, runID, in.ConductorToken), NowMS: model.NowMS(),
	})
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}

// runTransition is one edge of the run status machine an operator verb
// takes:
//
//	planning -> active <-> waiting-human -> done | abandoned
type runTransition struct {
	verb string
	to   model.RunStatus
	from []model.RunStatus
	// disposes marks a pause or an abandon, which must name the authority it
	// was made under; a resume disposes of nothing.
	disposes       bool
	requiresReason bool
}

var (
	runPause = runTransition{
		verb: "pause", to: model.RunWaitingHuman,
		from: []model.RunStatus{model.RunActive}, disposes: true,
	}
	runResume = runTransition{
		verb: "resume", to: model.RunActive,
		from: []model.RunStatus{model.RunWaitingHuman},
	}
	runAbandon = runTransition{
		verb: "abandon", to: model.RunAbandoned,
		from:     []model.RunStatus{model.RunPlanning, model.RunActive, model.RunWaitingHuman},
		disposes: true, requiresReason: true,
	}
)

// movedRun is what a lifecycle move returns to its verb.
type movedRun struct {
	run        *model.Run
	worktrees  []string
	registered *engine.ProjectRegistration
}

// moveRun takes transition t on the run ref names. The engine writes the
// status and its run-paused, run-resumed or run-abandoned event in one
// transaction, and checks the conductor capability inside it.
func moveRun(
	env Env, t runTransition, ref, reason, authority, authorityRef string, conductorToken func() string,
) (movedRun, error) {
	_, registered, err := resolveRegisteringProject(env, "run "+t.verb)
	moved := movedRun{registered: registered}
	if err != nil {
		return moved, err
	}
	runID, err := model.ParseRunID(ref)
	if err != nil {
		return moved, newError(err, output.ErrValidation)
	}
	if t.requiresReason && reason == "" {
		return moved, newError(fmt.Errorf("--reason is required to %s a run", t.verb), output.ErrValidation)
	}
	var under engine.Authority
	if t.disposes {
		if under, err = parseAuthority(authority, authorityRef); err != nil {
			return moved, err
		}
	}

	moved.run, moved.worktrees, err = engine.MoveRunWithStore(env.Store, engine.MoveRunOptions{
		RunID: runID, Verb: t.verb, To: t.to, From: t.from, Reason: reason, Under: under,
		Token: runConductorToken(env, runID, conductorToken), NowMS: model.NowMS(),
	})
	if err != nil {
		return moved, engineError(err)
	}
	return moved, nil
}

// RunBudgetInput reads a run's cap, or changes it when Set is non-nil.
type RunBudgetInput struct {
	Run string
	// Set is the new cap; 0 means unlimited.
	Set *float64
	// Reason is recorded on the cap change's event, and IfVersion makes the
	// change conditional on the run's row version; a version below 1 is
	// refused after the project and the run reference. Both apply only with
	// Set.
	Reason    string
	IfVersion *int
}

// RunBudgetResult is the run's effective cap and the spend it is compared
// against, after any change.
type RunBudgetResult struct {
	Budget *engine.RunBudget
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunBudget reads or sets a run's declared-cost cap. Raising the cap of a
// run parked on a breach does not resume it. Its project policy is
// RunPause's.
func RunBudget(ctx context.Context, env Env, in RunBudgetInput) (RunBudgetResult, error) {
	_, registered, err := resolveRegisteringProject(env, "run budget")
	res := RunBudgetResult{Registered: registered}
	if err != nil {
		return res, err
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return res, newError(err, output.ErrValidation)
	}

	if in.Set == nil {
		res.Budget, err = engine.GetRunBudgetStore(env.Store, runID)
		if err != nil {
			return res, engineError(err)
		}
		return res, nil
	}
	if err := checkIfVersion(in.IfVersion); err != nil {
		return res, err
	}

	res.Budget, err = engine.SetRunBudgetStore(env.Store, runID, *in.Set, in.Reason, in.IfVersion, model.NowMS())
	if err != nil {
		if mapped := CASError(err, model.FormatRunID(runID)); mapped != nil {
			return res, mapped
		}
		return res, engineError(err)
	}
	return res, nil
}
