package verb

import (
	"context"
	"errors"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// DispatchOpenInput is a request to record a batch manifest of a run's ready
// steps.
type DispatchOpenInput struct {
	Run string
	// Limit caps the manifest rows; 0 means no limit.
	Limit int
	// AckReap acknowledges write-class reaps by their lease-reaped event seq.
	AckReap []int64
}

// DispatchOpenResult is the recorded manifest.
type DispatchOpenResult struct {
	Manifest *engine.Manifest
	// Registered is the project this call registered, if it registered one,
	// for the caller to record its project-registered event. It is set even
	// when the call also returns an error: read it before checking the error.
	Registered *engine.ProjectRegistration
}

// DispatchOpen records the run's ready set as its one open manifest.
//
// It takes its project from the run it is handed, so it is run-addressed.
func DispatchOpen(ctx context.Context, env Env, in DispatchOpenInput) (DispatchOpenResult, error) {
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return DispatchOpenResult{}, err
	}
	for _, seq := range in.AckReap {
		if seq <= 0 {
			return DispatchOpenResult{}, newError(fmt.Errorf(
				"--ack-reap %d is not an event seq; pass the `seq` of the "+
					"`lease-reaped` event being acknowledged", seq),
				output.ErrValidation)
		}
	}

	registered, err := resolveRunAddressedProject(env, "dispatch open")
	res := DispatchOpenResult{Registered: registered}
	if err != nil {
		return res, err
	}

	manifest, err := engine.NewEngine().OpenDispatchStore(
		env.Store, runID, in.Limit, in.AckReap, model.NowMS())
	if err != nil {
		return res, engineError(err)
	}
	res.Manifest = manifest
	return res, nil
}

// DispatchExtendInput is a request to append newly ready steps to a run's
// open manifest.
type DispatchExtendInput struct {
	Run string
}

// DispatchExtendResult is the append.
type DispatchExtendResult struct {
	Extension *engine.Extension
	// Registered is as on DispatchOpenResult.
	Registered *engine.ProjectRegistration
}

// DispatchExtend appends the steps that became ready since the open to the
// same manifest.
//
// It takes its project from the run it is handed, so it is run-addressed.
func DispatchExtend(ctx context.Context, env Env, in DispatchExtendInput) (DispatchExtendResult, error) {
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return DispatchExtendResult{}, err
	}

	registered, err := resolveRunAddressedProject(env, "dispatch extend")
	res := DispatchExtendResult{Registered: registered}
	if err != nil {
		return res, err
	}

	extension, err := engine.NewEngine().ExtendDispatchStore(env.Store, runID, model.NowMS())
	if err != nil {
		return res, engineError(err)
	}
	res.Extension = extension
	return res, nil
}

// DispatchVerifyInput is a request to compare a run's open manifest to its
// current ready set.
type DispatchVerifyInput struct {
	Run string
}

// DispatchVerifyResult is the comparison. Mismatch is set when a stored row
// no longer renders as it was stored; it is the verify's answer, not a
// failure, so each transport decides how to report it.
type DispatchVerifyResult struct {
	Verify   *engine.VerifyResult
	Mismatch *engine.RowMismatch
}

// DispatchVerify compares the open manifest to the run's current ready set,
// writing nothing.
//
// It observes, so it is read-only and never registers a project.
func DispatchVerify(ctx context.Context, env Env, in DispatchVerifyInput) (DispatchVerifyResult, error) {
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return DispatchVerifyResult{}, err
	}
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return DispatchVerifyResult{}, err
	}

	verify, mismatch, err := engine.NewEngine().VerifyDispatchStore(env.Store, runID, model.NowMS())
	if err != nil {
		return DispatchVerifyResult{}, engineError(err)
	}
	return DispatchVerifyResult{Verify: verify, Mismatch: mismatch}, nil
}

// DispatchCloseInput is a request to reconcile and close a run's open
// manifest.
type DispatchCloseInput struct {
	Run                string
	AcceptMissingUsage bool
	// Reconcile makes the close the one-invocation wave reconcile: back-fill
	// BackfillRows, verify, then close. An empty batch is refused at the
	// back-fill stage. BackfillRows, Source and OnDuplicate apply to that
	// stage only.
	Reconcile    bool
	BackfillRows []engine.BackfillRow
	Source       string
	OnDuplicate  string
	// SkipIntegrationCheck is the reason to close without verifying that
	// every write-class commit reached the shared branch.
	SkipIntegrationCheck string
	// NoConditions is the reason to close despite sealed votes whose
	// conditions have no linked tribunal issue. It is recorded as a run note
	// attributed to env.Author and env.Cwd.
	NoConditions string
	// ConductorToken returns the run's conductor capability. It is called
	// only when an override is set and the run is bound to a capability, so a
	// transport whose token channel can block is never read for a close that
	// needs no token.
	ConductorToken func() string
}

// DispatchCloseResult is the close, or for a batch the reconcile.
type DispatchCloseResult struct {
	// Close is set by a plain close.
	Close *engine.CloseOutcome
	// Reconcile is set by a reconcile, and on a stage's refusal
	// carries the stages that ran before it.
	Reconcile *engine.ReconcileOutcome
	// Registered is as on DispatchOpenResult.
	Registered *engine.ProjectRegistration
}

// DispatchClose closes the run's open manifest when the run has no
// discrepancy. With Reconcile it first back-fills and verifies, refusing
// with an *engine.StageError naming the stage that failed.
//
// It takes its project from the run it is handed, so it is run-addressed.
func DispatchClose(ctx context.Context, env Env, in DispatchCloseInput) (DispatchCloseResult, error) {
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return DispatchCloseResult{}, err
	}

	skip := engine.IntegrationSkip{Reason: in.SkipIntegrationCheck}
	skip.Conditions.Reason = in.NoConditions
	if in.NoConditions != "" {
		if env.Cwd == "" {
			return DispatchCloseResult{}, newError(errors.New(
				"the working directory could not be resolved; a ruling must "+
					"record where it was made from, so run it again from a "+
					"readable directory"), output.ErrGeneral)
		}
		skip.Conditions.By = engine.Attribution{Actor: env.Author, Cwd: env.Cwd}
	}

	registered, err := resolveRunAddressedProject(env, "dispatch close")
	res := DispatchCloseResult{Registered: registered}
	if err != nil {
		return res, err
	}

	if skip.Reason != "" || skip.Conditions.Reason != "" {
		skip.Token = runConductorToken(env, runID, in.ConductorToken)
		skip.Conditions.Token = skip.Token
	}

	e := engine.NewEngine()
	if in.Reconcile {
		res.Reconcile, err = e.ReconcileDispatchStore(env.Store, runID, in.BackfillRows,
			in.Source, in.OnDuplicate, in.AcceptMissingUsage, skip, model.NowMS())
	} else {
		res.Close, err = e.CloseDispatchStore(env.Store, runID, in.AcceptMissingUsage, skip, model.NowMS())
	}
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}

// DispatchBackfillUsageInput is a batch of usage a relay measured for steps
// whose claimant could not report it.
type DispatchBackfillUsageInput struct {
	Run  string
	Rows []engine.BackfillRow
	// Source is who measured the usage; empty records "backfilled".
	Source string
	// OnDuplicate is "refuse" (the default when empty) or "skip".
	OnDuplicate string
}

// DispatchBackfillUsageResult is the recorded batch.
type DispatchBackfillUsageResult struct {
	Backfill *engine.BackfillOutcome
	// Registered is as on DispatchOpenResult.
	Registered *engine.ProjectRegistration
}

// DispatchBackfillUsage records the batch against each step's recorded
// attempt, in one transaction.
//
// It takes its project from the run it is handed, so it is run-addressed.
func DispatchBackfillUsage(ctx context.Context, env Env, in DispatchBackfillUsageInput) (DispatchBackfillUsageResult, error) {
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return DispatchBackfillUsageResult{}, err
	}

	registered, err := resolveRunAddressedProject(env, "dispatch backfill-usage")
	res := DispatchBackfillUsageResult{Registered: registered}
	if err != nil {
		return res, err
	}

	res.Backfill, err = engine.NewEngine().BackfillUsageStore(
		env.Store, runID, in.Rows, in.Source, in.OnDuplicate, model.NowMS())
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}

// DispatchAbandonInput is a request to retire a run's open manifest.
type DispatchAbandonInput struct {
	Run string
	// Reason is recorded on the abandon event.
	Reason string
	// ConductorToken returns the run's conductor capability. It is called
	// only when the run is bound to one, so a transport whose token channel
	// can block is never read for a run that needs no token.
	ConductorToken func() string
}

// DispatchAbandonResult is the abandoned manifest.
type DispatchAbandonResult struct {
	Abandon *engine.CloseOutcome
	// Registered is as on DispatchOpenResult.
	Registered *engine.ProjectRegistration
}

// DispatchAbandon retires the run's open manifest whatever its
// discrepancies, under the run's conductor capability.
//
// It takes its project from the run it is handed, so it is run-addressed.
func DispatchAbandon(ctx context.Context, env Env, in DispatchAbandonInput) (DispatchAbandonResult, error) {
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return DispatchAbandonResult{}, err
	}

	registered, err := resolveRunAddressedProject(env, "dispatch abandon")
	res := DispatchAbandonResult{Registered: registered}
	if err != nil {
		return res, err
	}

	res.Abandon, err = engine.NewEngine().AbandonDispatchWithStore(env.Store, runID, engine.AbandonDispatchOptions{
		Reason: in.Reason, Token: runConductorToken(env, runID, in.ConductorToken), NowMS: model.NowMS(),
	})
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}

// DispatchWaiveTargetInput is a ruling that named stale-target warnings were
// investigated and are acceptable.
type DispatchWaiveTargetInput struct {
	Run string
	// Steps are the step instances the warning printed, e.g. "review@1#0".
	Steps []string
	// Target is the warned sha, or a prefix of at least 7 characters.
	Target string
	Note   string
	// ConductorToken is as on DispatchAbandonInput.
	ConductorToken func() string
}

// DispatchWaiveTargetResult is the waivers recorded, one per step instance.
type DispatchWaiveTargetResult struct {
	Waivers []engine.WaivedTarget
	// Registered is as on DispatchOpenResult.
	Registered *engine.ProjectRegistration
}

// DispatchWaiveTarget records one waiver per step instance for the target,
// under the run's conductor capability, attributed to env.Author and env.Cwd.
//
// It takes its project from the run it is handed, so it is run-addressed.
func DispatchWaiveTarget(ctx context.Context, env Env, in DispatchWaiveTargetInput) (DispatchWaiveTargetResult, error) {
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return DispatchWaiveTargetResult{}, err
	}

	registered, err := resolveRunAddressedProject(env, "dispatch waive-target")
	res := DispatchWaiveTargetResult{Registered: registered}
	if err != nil {
		return res, err
	}

	res.Waivers, err = engine.NewEngine().WaiveStaleTargetsStore(
		env.Store, runID, in.Steps, in.Target, in.Note, rulingAttribution(env),
		runConductorToken(env, runID, in.ConductorToken), model.NowMS())
	if err != nil {
		return res, engineError(err)
	}
	return res, nil
}

// runConductorToken returns source's token only when run runID is bound to a
// conductor capability, and "" otherwise without calling source. The look is
// advisory: the engine re-checks the capability inside its own transaction.
func runConductorToken(env Env, runID int, source func() string) string {
	if source == nil {
		return ""
	}
	hash, err := env.Store.RunConductorHash(runID)
	if err != nil || hash == "" {
		return ""
	}
	return source()
}
