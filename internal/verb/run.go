package verb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

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

// RunIssueAddInput attaches issues to a run.
type RunIssueAddInput struct {
	Run string
	// Issues are the references of the issues to attach.
	Issues []string
}

// RunIssueAddResult is the run and its issue set after the add.
type RunIssueAddResult struct {
	Run *model.Run
	// Added are the ids the input named, in its order.
	Added []int
	// Issues are the ids attached to the run after the add.
	Issues []int
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunIssueAdd attaches issues to a planning run, or to an active one whose
// next activation binds and snapshots them. A parked run is a person's
// decision in progress and a terminal run's issue set is history, so both
// refuse. Its project policy is RunPause's.
func RunIssueAdd(ctx context.Context, env Env, in RunIssueAddInput) (RunIssueAddResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "run issue add")
	res := RunIssueAddResult{Registered: registered}
	if err != nil {
		return res, err
	}
	run, issueIDs, err := runIssueArgs(env.Store, in.Run, in.Issues)
	if err != nil {
		return res, err
	}
	if run.Status != model.RunPlanning && run.Status != model.RunActive {
		return res, newError(
			fmt.Errorf("run %s is %s; issues can be added while a run is "+
				"planning or active", run.Ref(), run.Status),
			output.ErrConflict)
	}

	// Existence and homing are checked for the whole set before anything is
	// written, so a typo'd second id cannot leave a half-applied add behind.
	// Activation binds a run's issues against its own project's registry, so
	// an issue homed elsewhere is refused (DKT-21).
	for _, id := range issueIDs {
		issueProject, err := env.Store.IssueProjectID(id)
		if errors.Is(err, store.ErrNotFound) {
			return res, newError(
				fmt.Errorf("issue %s not found", issueDisplayID(env, projectID, id)), output.ErrNotFound)
		}
		if err != nil {
			return res, newError(fmt.Errorf("checking issue %s: %w", issueDisplayID(env, projectID, id), err),
				output.ErrGeneral)
		}
		if issueProject != run.ProjectID {
			display := issueDisplayID(env, projectID, id)
			return res, newError(fmt.Errorf(
				"issue %s belongs to %s, run %s to %s — a run's issues live in the "+
					"run's own project; migrate the issue first with `docket issue "+
					"move %s --project <target>`, or start the run from the issue's "+
					"own repository",
				display, projectLabel(env.Store, issueProject), run.Ref(),
				projectLabel(env.Store, run.ProjectID), display),
				output.ErrValidation)
		}
	}

	for _, id := range issueIDs {
		if err := env.Store.AddRunIssue(run.ID, id); err != nil {
			return res, engineError(err)
		}
	}
	if res.Issues, err = attachedIssueIDs(env.Store, run.ID); err != nil {
		return res, err
	}
	res.Run, res.Added = run, issueIDs
	return res, nil
}

// RunIssueRemoveInput detaches issues from a planning run.
type RunIssueRemoveInput struct {
	Run string
	// Issues are the references of the issues to detach.
	Issues []string
}

// RunIssueRemoveResult is the run and its issue set after the remove.
type RunIssueRemoveResult struct {
	Run *model.Run
	// Removed are the ids the input named, in its order.
	Removed []int
	// Issues are the ids attached to the run after the remove.
	Issues []int
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunIssueRemove detaches issues from a run still in planning. Activation
// binds the set, so a run past planning refuses: removing would strand steps
// that already exist. Its project policy is RunPause's.
func RunIssueRemove(ctx context.Context, env Env, in RunIssueRemoveInput) (RunIssueRemoveResult, error) {
	projectID, registered, err := resolveRegisteringProject(env, "run issue remove")
	res := RunIssueRemoveResult{Registered: registered}
	if err != nil {
		return res, err
	}
	run, issueIDs, err := runIssueArgs(env.Store, in.Run, in.Issues)
	if err != nil {
		return res, err
	}
	if run.Status != model.RunPlanning {
		return res, newError(
			fmt.Errorf("run %s is %s; the issue set is immutable once activation "+
				"has bound it — issues can be removed only while a run is planning",
				run.Ref(), run.Status),
			output.ErrConflict)
	}

	// Membership is checked for the whole set first, so `remove A TYPO` cannot
	// detach A and then refuse.
	attached, err := attachedIssueIDs(env.Store, run.ID)
	if err != nil {
		return res, err
	}
	for _, id := range issueIDs {
		if !slices.Contains(attached, id) {
			return res, newError(
				fmt.Errorf("issue %s is not attached to %s", issueDisplayID(env, projectID, id), run.Ref()),
				output.ErrNotFound)
		}
	}

	for _, id := range issueIDs {
		if err := env.Store.RemoveRunIssue(run.ID, id); err != nil {
			return res, engineError(err)
		}
	}
	if res.Issues, err = attachedIssueIDs(env.Store, run.ID); err != nil {
		return res, err
	}
	res.Run, res.Removed = run, issueIDs
	return res, nil
}

// runIssueArgs loads the run ref names and parses the issue references, in
// that order.
func runIssueArgs(st store.Store, ref string, issues []string) (*model.Run, []int, error) {
	runID, err := model.ParseRunID(ref)
	if err != nil {
		return nil, nil, newError(err, output.ErrValidation)
	}
	run, err := st.GetRun(runID)
	if errors.Is(err, store.ErrRunNotFound) {
		return nil, nil, newError(
			fmt.Errorf("run %s not found", model.FormatRunID(runID)), output.ErrNotFound)
	}
	if err != nil {
		return nil, nil, newError(err, output.ErrGeneral)
	}

	issueIDs := make([]int, 0, len(issues))
	for _, issue := range issues {
		id, err := model.ParseID(issue)
		if err != nil {
			return nil, nil, newError(
				fmt.Errorf("invalid issue ID %q: %w", issue, err), output.ErrValidation)
		}
		issueIDs = append(issueIDs, id)
	}
	return run, issueIDs, nil
}

// attachedIssueIDs is the ids of the issues attached to runID.
func attachedIssueIDs(st store.Store, runID int) ([]int, error) {
	attached, err := st.ListRunIssues(runID)
	if err != nil {
		return nil, newError(err, output.ErrGeneral)
	}
	ids := make([]int, 0, len(attached))
	for _, ri := range attached {
		ids = append(ids, ri.IssueID)
	}
	return ids, nil
}

// RunRepinInput adopts current disk bytes for a run's drifted pins.
type RunRepinInput struct {
	Run string
	// Reason is required: it is recorded on every run-repinned event.
	Reason string
	// Drop and DropUnresolvable retire pins whose refs no longer resolve, as
	// engine.RepinOptions reads them.
	Drop             []string
	DropUnresolvable bool
	// ConductorToken is as on RunPauseInput.
	ConductorToken func() string
}

// RunRepinResult is what the repin changed.
type RunRepinResult struct {
	Outcome *engine.RepinOutcome
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunRepin moves a run's current pin agreement to what each drifted ref
// resolves to now, under the run's conductor capability. Completed steps'
// provenance is never rewritten. Its project policy is RunPause's.
func RunRepin(ctx context.Context, env Env, in RunRepinInput) (RunRepinResult, error) {
	_, registered, err := resolveRegisteringProject(env, "run repin")
	res := RunRepinResult{Registered: registered}
	if err != nil {
		return res, err
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return res, newError(err, output.ErrValidation)
	}
	if in.Reason == "" {
		return res, newError(
			errors.New("--reason is required to repin a run; the event trail must "+
				"say why the recorded agreement moved"),
			output.ErrValidation)
	}

	res.Outcome, err = engine.RepinRunWithStore(env.Store, runID, engine.RepinOptions{
		Reason: in.Reason, Drop: in.Drop, DropUnresolvable: in.DropUnresolvable,
		Token: runConductorToken(env, runID, in.ConductorToken),
	}, model.NowMS())
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}

// RunFactAddInput is one coordination fact only the conductor observes.
type RunFactAddInput struct {
	Run string
	// Kind is engine.RunFactVoteReseated or engine.RunFactStepDeferred.
	Kind   string
	Reason string
	// Proposal and Voter name the re-seated seat (vote-reseated).
	Proposal string
	Voter    string
	// Step and Cause name the deferred step (step-deferred).
	Step  string
	Cause string
	// ConductorToken is as on RunPauseInput.
	ConductorToken func() string
}

// Validate refuses a malformed run, --proposal or --step reference, the
// faults RunFactAdd reports after the project. A transport whose attribution
// fails calls it before reporting that failure, so a request invalid both
// ways reports the request's fault.
func (in RunFactAddInput) Validate() error {
	_, err := in.options()
	return err
}

// options parses the input's references into the fact's options.
func (in RunFactAddInput) options() (engine.RunFactOptions, error) {
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return engine.RunFactOptions{}, newError(err, output.ErrValidation)
	}
	opts := engine.RunFactOptions{RunID: runID, Kind: in.Kind, Reason: in.Reason, Voter: in.Voter, Cause: in.Cause}
	if in.Proposal != "" {
		if opts.ProposalID, err = model.ParseProposalID(in.Proposal); err != nil {
			return opts, newError(fmt.Errorf("invalid --proposal %q: %w", in.Proposal, err), output.ErrValidation)
		}
	}
	if in.Step != "" {
		if opts.StepID, err = model.ParseStepID(in.Step); err != nil {
			return opts, newError(fmt.Errorf("invalid --step %q: %w", in.Step, err), output.ErrValidation)
		}
	}
	return opts, nil
}

// RunFactAddResult is the fact as recorded, or the engine's own record of a
// chain deferral it already holds.
type RunFactAddResult struct {
	Fact *engine.RunFact
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunFactAdd records a conductor-observed fact against a run as an event
// attributed to env.Author from env.Cwd, under the run's conductor
// capability. Its project policy is RunPause's.
func RunFactAdd(ctx context.Context, env Env, in RunFactAddInput) (RunFactAddResult, error) {
	_, registered, err := resolveRegisteringProject(env, "run fact add")
	res := RunFactAddResult{Registered: registered}
	if err != nil {
		return res, err
	}
	opts, err := in.options()
	if err != nil {
		return res, err
	}
	opts.By, opts.NowMS = engine.Attribution{Actor: env.Author, Cwd: env.Cwd}, model.NowMS()
	opts.Token = runConductorToken(env, opts.RunID, in.ConductorToken)

	res.Fact, err = engine.RecordRunFactStore(env.Store, opts)
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}

// RunNoteAddInput is one note for every later packet of a run.
type RunNoteAddInput struct {
	Run string
	// Authority is whose ruling the note carries, as
	// engine.ParseRunNoteAuthority reads it.
	Authority string
	// Text returns the note. It is called only once the run reference and the
	// authority are accepted, so a refused note leaves a transport's stream
	// unread. A *Error it returns is returned unchanged and any other error
	// is coded GENERAL_ERROR. Nil reads as an empty note.
	Text func() (string, error)
	// ConductorToken is as on RunPauseInput.
	ConductorToken func() string
}

// Validate refuses a malformed run reference or authority, the faults
// RunNoteAdd reports after the project, as on RunFactAddInput.
func (in RunNoteAddInput) Validate() error {
	_, _, err := in.parse()
	return err
}

func (in RunNoteAddInput) parse() (int, engine.Authority, error) {
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return 0, engine.Authority{}, newError(err, output.ErrValidation)
	}
	under, err := engine.ParseRunNoteAuthority(in.Authority)
	if err != nil {
		return 0, engine.Authority{}, engineError(err)
	}
	return runID, under, nil
}

// RunNoteAddResult is the note as recorded.
type RunNoteAddResult struct {
	// Run is the run's canonical reference.
	Run  string
	Note *engine.RunNote
	// Registered is as on RunStartResult.
	Registered *engine.ProjectRegistration
}

// RunNoteAdd records a note against a run, attributed to env.Author from
// env.Cwd, under the run's conductor capability. Its project policy is
// RunPause's.
func RunNoteAdd(ctx context.Context, env Env, in RunNoteAddInput) (RunNoteAddResult, error) {
	_, registered, err := resolveRegisteringProject(env, "run note add")
	res := RunNoteAddResult{Registered: registered}
	if err != nil {
		return res, err
	}
	runID, under, err := in.parse()
	if err != nil {
		return res, err
	}
	var text string
	if in.Text != nil {
		if text, err = in.Text(); err != nil {
			var coded *Error
			if errors.As(err, &coded) {
				return res, err
			}
			return res, newError(err, output.ErrGeneral)
		}
	}

	res.Note, err = engine.AddRunNoteStore(env.Store, runID,
		runConductorToken(env, runID, in.ConductorToken),
		engine.Attribution{Actor: env.Author, Cwd: env.Cwd}, under, text, model.NowMS())
	if err != nil {
		return res, engineError(err)
	}
	res.Run = model.FormatRunID(runID)
	return res, nil
}

// RunNoteListInput names the run whose notes to list.
type RunNoteListInput struct {
	Run string
}

// RunNoteListResult is a run's notes in the order they render.
type RunNoteListResult struct {
	// Run is the run's canonical reference.
	Run   string
	Notes []engine.RunNote
}

// RunNoteList lists a run's notes. It is a read, and an identity no project
// holds is never registered.
func RunNoteList(ctx context.Context, env Env, in RunNoteListInput) (RunNoteListResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return RunNoteListResult{}, err
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return RunNoteListResult{}, newError(err, output.ErrValidation)
	}
	notes, err := engine.ListRunNotesStore(env.Store, runID)
	if err != nil {
		return RunNoteListResult{}, engineError(err)
	}
	return RunNoteListResult{Run: model.FormatRunID(runID), Notes: notes}, nil
}

// RunStatusInput reads one run when Run is set, or lists the caller's runs
// when it is empty.
type RunStatusInput struct {
	Run string
	// OneRun reads Run as one run's ref even when it is empty, so an empty
	// ref is refused rather than answered as a list.
	OneRun bool
	// All widens a list to done and abandoned runs; it does not apply to one
	// run.
	All bool
	// Limit caps a list; 0 means no limit.
	Limit int
}

// RunStatusResult is a list (Runs and Total) or one run's Detail.
type RunStatusResult struct {
	Runs []*model.Run
	// Total counts every run the list matched, ignoring Limit.
	Total  int
	Detail *RunStatusDetail
}

// RunStatusDetail is one run and its rollup.
type RunStatusDetail struct {
	Run    *model.Run
	Issues int
	// Steps counts the run's steps by effective status, the status `step
	// show` reports.
	Steps []model.StatusCount
	Pins  []store.Pin
	// PinDrift is the unsound subset of Pins, checked against disk only while
	// the run is active or waiting-human; a terminal run's pins are history.
	PinDrift       []engine.PinVerdict
	ActivationHead *engine.ActivationHead
	Unexpanded     []engine.UnexpandedIssue
	RetryEligible  []engine.RetryEligible
}

// RunStatus lists the runs of the caller's project, or reads one run's status
// and step rollup. It is a read: it writes nothing, not even a lease reap, and
// an identity no project holds is never registered.
func RunStatus(ctx context.Context, env Env, in RunStatusInput) (RunStatusResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return RunStatusResult{}, err
	}
	if in.Run == "" && !in.OneRun {
		runs, total, err := env.Store.ListRuns(store.RunListOptions{
			ProjectID: projectID, ActiveOnly: !in.All, Limit: in.Limit,
		})
		if err != nil {
			return RunStatusResult{}, engineError(err)
		}
		return RunStatusResult{Runs: runs, Total: total}, nil
	}
	if in.All {
		return RunStatusResult{}, newError(
			errors.New("--all widens a list; it does not apply to a single run"), output.ErrValidation)
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return RunStatusResult{}, newError(err, output.ErrValidation)
	}
	detail, err := runStatusDetail(env.Store, runID)
	if err != nil {
		return RunStatusResult{}, engineError(err)
	}
	return RunStatusResult{Detail: detail}, nil
}

func runStatusDetail(st store.Store, runID int) (*RunStatusDetail, error) {
	run, err := st.GetRun(runID)
	if err != nil {
		return nil, err
	}
	issues, err := st.ListRunIssues(runID)
	if err != nil {
		return nil, err
	}
	steps, err := engine.EffectiveStatusCountsStore(st, runID, model.NowMS())
	if err != nil {
		return nil, err
	}
	pins, err := st.ListPins(runID)
	if err != nil {
		return nil, err
	}
	head, err := engine.LatestActivationHeadStore(st, runID)
	if err != nil {
		return nil, err
	}
	unexpanded, retryEligible, err := engine.RunAdvisoriesStore(st, runID)
	if err != nil {
		return nil, err
	}
	detail := &RunStatusDetail{
		Run: run, Issues: len(issues), Steps: steps, Pins: pins, ActivationHead: head,
		Unexpanded: unexpanded, RetryEligible: retryEligible,
	}
	// The drift check reads disk on purpose (DKT-408): a parked run whose pins
	// a corpus install replaced must say so in the status a resuming conductor
	// reads first. It still writes nothing.
	if run.Status == model.RunActive || run.Status == model.RunWaitingHuman {
		if detail.PinDrift, err = engine.PinDriftStore(st, runID); err != nil {
			return nil, err
		}
	}
	return detail, nil
}

// RunReportInput names the run to report on.
type RunReportInput struct {
	Run string
}

// RunReportResult is the run's ledger rollup.
type RunReportResult struct {
	Report *engine.RunReport
}

// RunReport rolls up a run's budget, steps, gates, actions, artifacts and
// metadata, in any status. It is a read: it writes nothing, not even a lease
// reap, and an identity no project holds is never registered.
func RunReport(ctx context.Context, env Env, in RunReportInput) (RunReportResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return RunReportResult{}, err
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return RunReportResult{}, newError(err, output.ErrValidation)
	}
	report, err := engine.LoadRunReportStore(env.Store, runID, model.NowMS())
	if err != nil {
		return RunReportResult{}, engineError(err)
	}
	return RunReportResult{Report: report}, nil
}

// RunVerifyPinsInput names the run whose pin set to check.
type RunVerifyPinsInput struct {
	Run string
}

// RunVerifyPinsResult is the whole pin set's verdicts.
type RunVerifyPinsResult struct {
	Report *engine.PinReport
}

// RunVerifyPins checks every pin a run holds against what its ref resolves to
// now, and the pin set for closure. It is a read and re-pins nothing. An
// unsound report is refused with the code the blocked verbs themselves
// return, and the report is returned beside the refusal.
func RunVerifyPins(ctx context.Context, env Env, in RunVerifyPinsInput) (RunVerifyPinsResult, error) {
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return RunVerifyPinsResult{}, err
	}
	runID, err := model.ParseRunID(in.Run)
	if err != nil {
		return RunVerifyPinsResult{}, newError(err, output.ErrValidation)
	}
	report, err := engine.VerifyPinsStore(env.Store, runID)
	if err != nil {
		return RunVerifyPinsResult{}, engineError(err)
	}
	res := RunVerifyPinsResult{Report: report}
	if !report.Sound() {
		return res, unsoundPinsError(report)
	}
	return res, nil
}

// unsoundPinsError is the refusal for an unsound pin report. Its code and
// remedy follow the first fault present, in the order changed, missing,
// executed copy changed, then an open closure.
func unsoundPinsError(report *engine.PinReport) error {
	code := output.ErrConflict
	remedy := fmt.Sprintf("; restore the file(s), or `docket run repin %s "+
		"--reason ...` adopts current disk bytes for the remaining steps "+
		"(DKT-408)", report.Run)
	switch {
	case report.Changed > 0:
		// Drift wins the disposition: it is the half a repin can fix, and
		// its remedy is the one to try first.
	case report.Missing > 0:
		code = output.ErrNotFound
		// A missing ref has no current bytes to adopt; repin would refuse.
		remedy = "; restore the file(s) or abandon the run"
	case report.ExecutedCopyChanged > 0:
		// A gate run from the invoking checkout would execute other bytes
		// than the run pinned. The run's own pins are intact, so repin has
		// nothing to adopt and a new run would pin the run's checkout
		// again; the fix is the invoking copy or where the gate runs.
		remedy = "; restore the gate script in the invoking checkout, " +
			"or run the gate from the run's checkout"
	default:
		// Only the closure is open. Nothing drifted, so there is nothing on
		// disk to restore and repin has no changed bytes to adopt. The pin
		// set froze without the ref, and only a new activation pins one. The
		// code is the one the blocked claims themselves return.
		code = output.ErrValidation
		remedy = "; the pin set froze without the ref(s), so the step(s) " +
			"reading them refuse at claim (VALIDATION_ERROR) — start a new " +
			"run to pin them, or take the reference back out of the " +
			"referencing file"
	}
	// Unpinned gates are a separate report, but an unsound run must not hide
	// them: name them beside the drift.
	var names []string
	for _, g := range report.Gates {
		if g.Status == engine.GateUnpinned {
			names = append(names, fmt.Sprintf("%q", g.Gate))
		}
	}
	if len(names) > 0 {
		remedy += "; gates with no pinned script: " + strings.Join(names, ", ")
	}
	return newError(fmt.Errorf("%s: %s%s", report.Run, engine.PinReportReason(report), remedy), code)
}
