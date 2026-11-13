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

	budget, err := startCap(env, projectID, in.Budget, "--budget", store.KeyBudgetDefault)
	if err != nil {
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

	usageBudget, err := startCap(env, projectID, in.UsageBudget, "--usage-budget", store.KeyUsageBudgetDefault)
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

// startCap resolves one of a run's caps: the caller's value when set, which
// must not be negative, else the project's configured default under key. flag
// names the cap in a refusal.
func startCap(env Env, projectID int, set *float64, flag, key string) (float64, error) {
	if set != nil {
		if *set < 0 {
			return 0, newError(
				fmt.Errorf("%s must be >= 0, got %g (0 means unlimited)", flag, *set),
				output.ErrValidation)
		}
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
