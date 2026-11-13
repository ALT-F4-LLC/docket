package verb

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
	"github.com/ALT-F4-LLC/docket/internal/store"
)

// StepClaimInput is a step claim request.
type StepClaimInput struct {
	Step  string
	Owner string
	// TTL is the lease duration; nil takes the step's configured TTL.
	TTL      *time.Duration
	Metadata string
	// CostMultiplier scales the claim's budget accrual; nil accrues the
	// step's declared cost verbatim.
	CostMultiplier *float64
	// RecoverySecret, when set, makes a re-claim's re-mint provable: only its
	// hash is stored.
	RecoverySecret string
	// Render returns the rendered work packet instead of the context bundle,
	// rendered from Template (the shipped one when empty) with Executor
	// substituted (the step's declared hint when empty).
	Render   bool
	Template string
	Executor string
}

// Validate refuses what StepClaim refuses before it reads the store: a
// malformed step reference, a missing owner, or a TTL or cost multiplier that
// is not positive. A transport that reads RecoverySecret from a file calls it
// first, so a request that is invalid both ways reports the request's fault.
func (in StepClaimInput) Validate() error {
	_, err := in.validate()
	return err
}

func (in StepClaimInput) validate() (int, error) {
	id, err := parseStepRef(in.Step)
	if err != nil {
		return 0, err
	}
	if in.Owner == "" {
		return 0, newError(
			errors.New("--owner is required: it identifies the lease holder"),
			output.ErrValidation)
	}
	if err := checkTTL(in.TTL); err != nil {
		return 0, err
	}
	if in.CostMultiplier != nil && *in.CostMultiplier <= 0 {
		return 0, newError(fmt.Errorf(
			"--cost-multiplier must be positive, got %g", *in.CostMultiplier),
			output.ErrValidation)
	}
	return id, nil
}

// StepClaimResult is a claim whose lease committed. Packet is set only for a
// rendered claim.
type StepClaimResult struct {
	Claim  *engine.ClaimResult
	Packet *engine.RenderResult
	// Registered is the project this call registered, if it registered one,
	// for the caller to record its project-registered event. It is set even
	// when StepClaim also returns an error, because the project stays
	// registered: read it before checking the error.
	Registered *engine.ProjectRegistration
}

// StepClaim takes the lease on a step and mints its capability token. A
// claim whose lease committed and whose later stage failed returns both the
// result, carrying the live token, and an error wrapping the
// *engine.IncompleteClaimError: the caller holds the lease and must be able
// to end or complete it.
//
// It takes its project from the step it is handed, so it is run-addressed.
func StepClaim(ctx context.Context, env Env, in StepClaimInput) (StepClaimResult, error) {
	id, err := in.validate()
	if err != nil {
		return StepClaimResult{}, err
	}
	var costMultiplier float64
	if in.CostMultiplier != nil {
		costMultiplier = *in.CostMultiplier
	}

	registered, err := resolveRunAddressedProject(env, "step claim")
	res := StepClaimResult{Registered: registered}
	if err != nil {
		return res, err
	}

	opts := engine.ClaimOptions{
		Owner: in.Owner, Metadata: in.Metadata,
		CostMultiplier: costMultiplier, RecoverySecret: in.RecoverySecret,
		NowMS: model.NowMS(),
	}
	if in.TTL != nil {
		opts.TTLOverride = in.TTL.Milliseconds()
	}

	var claim *engine.ClaimResult
	var packet *engine.RenderResult
	if in.Render {
		claim, packet, err = engine.NewEngine().ClaimStepRenderedStore(
			env.Store, id, opts, in.Template, in.Executor)
	} else {
		// A step's `pre = true` gates run here, at claim, with their results
		// in the returned bundle.
		claim, err = engine.NewEngine().ClaimStepWithGatesStore(env.Store, id, opts)
	}
	if err != nil {
		var incomplete *engine.IncompleteClaimError
		if errors.As(err, &incomplete) {
			res.Claim = incomplete.Result
			return res, newError(err, output.ErrGeneral)
		}
		return res, stepError(err, id)
	}
	res.Claim, res.Packet = claim, packet
	return res, nil
}

// StepHeartbeatInput is a step heartbeat request.
type StepHeartbeatInput struct {
	Step  string
	Token string
}

// StepHeartbeatResult is the extended lease.
type StepHeartbeatResult struct {
	Step  int
	Lease model.Lease
	// Registered is as on StepClaimResult.
	Registered *engine.ProjectRegistration
}

// StepHeartbeat extends the lease token holds on a step. The attempt counter
// is untouched: a heartbeat is not a new claim.
func StepHeartbeat(ctx context.Context, env Env, in StepHeartbeatInput) (StepHeartbeatResult, error) {
	id, err := parseStepRef(in.Step)
	if err != nil {
		return StepHeartbeatResult{}, err
	}
	if err := checkToken(in.Token); err != nil {
		return StepHeartbeatResult{}, err
	}

	registered, err := resolveRunAddressedProject(env, "step heartbeat")
	res := StepHeartbeatResult{Step: id, Registered: registered}
	if err != nil {
		return res, err
	}

	lease, err := engine.HeartbeatStepStore(env.Store, id, in.Token, model.NowMS())
	if err != nil {
		return res, stepError(err, id)
	}
	res.Lease = *lease
	return res, nil
}

// StepCompleteInput is a step completion request. Every body arrives as
// content: the transport reads any files.
type StepCompleteInput struct {
	Step  string
	Token string
	// WorkDir is the absolute path of the checkout the work happened in,
	// where the engine computes the recorded diff and runs the gates.
	WorkDir  string
	Artifact []byte
	Payload  []byte
	Usage    string
	Metadata string
	Gaps     [][]byte
	Comments [][]byte
}

// StepCompleteResult is the recorded step as the completion left it.
type StepCompleteResult struct {
	Step model.StepRow
	// Detached reports that the gates and routing run after this call
	// returns, so Step shows the recorded state rather than the outcome and
	// FailedGates is empty.
	Detached bool
	// GapIssues names the backlog issue each gap filed, in order.
	GapIssues []string
	// FailedGates are the completion gates that did not pass when they ran
	// inline.
	FailedGates []engine.FailedGate
	// Registered is as on StepClaimResult.
	Registered *engine.ProjectRegistration
}

// StepComplete records a step's artifact and runs the completion saga with
// the token that holds its lease, then drives any engine-run steps the
// recording readied.
func StepComplete(ctx context.Context, env Env, in StepCompleteInput) (StepCompleteResult, error) {
	id, err := parseStepRef(in.Step)
	if err != nil {
		return StepCompleteResult{}, err
	}
	if err := checkToken(in.Token); err != nil {
		return StepCompleteResult{}, err
	}

	registered, err := resolveRunAddressedProject(env, "step complete")
	res := StepCompleteResult{Registered: registered}
	if err != nil {
		return res, err
	}

	e := engine.NewEngine()
	err = e.CompleteStepStore(env.Store, id, engine.CompleteOptions{
		Token: in.Token, WorkDir: in.WorkDir, Artifact: in.Artifact, Payload: in.Payload,
		Usage: in.Usage, Metadata: in.Metadata,
		Gaps: in.Gaps, GapIssues: &res.GapIssues, Comments: in.Comments, Pending: &res.Detached,
		NowMS: model.NowMS(),
	})
	if err != nil {
		return res, stepError(err, id)
	}

	if err := driveStepRun(e, env, id); err != nil {
		return res, newError(fmt.Errorf(
			"the step recorded, but driving the run's engine-side "+
				"lifecycles failed: %w", err), output.ErrGeneral)
	}

	if !res.Detached {
		if res.FailedGates, err = engine.FailedGatesStore(env.Store, id); err != nil {
			return res, stepError(err, id)
		}
	}
	res.Step, err = stepRow(env, id)
	return res, err
}

// StepFailInput is a step failure report.
type StepFailInput struct {
	Step  string
	Token string
	// Note lands on the failure event; Metadata merges onto the step's row.
	Note     string
	Metadata string
}

// StepFailResult is the failed step as the failure left it.
type StepFailResult struct {
	Step model.StepRow
	// Registered is as on StepClaimResult.
	Registered *engine.ProjectRegistration
}

// StepFail reports a step as failed with the token that holds its lease,
// consuming an attempt, then drives any engine-run steps a routed failure
// readied.
func StepFail(ctx context.Context, env Env, in StepFailInput) (StepFailResult, error) {
	id, err := parseStepRef(in.Step)
	if err != nil {
		return StepFailResult{}, err
	}
	if err := checkToken(in.Token); err != nil {
		return StepFailResult{}, err
	}

	registered, err := resolveRunAddressedProject(env, "step fail")
	res := StepFailResult{Registered: registered}
	if err != nil {
		return res, err
	}

	e := engine.NewEngine()
	if err := e.FailStepStore(env.Store, id, in.Token, in.Note, in.Metadata, model.NowMS()); err != nil {
		return res, stepError(err, id)
	}

	if err := driveStepRun(e, env, id); err != nil {
		return res, newError(fmt.Errorf(
			"the failure recorded, but driving the run's engine-side "+
				"lifecycles failed: %w", err), output.ErrGeneral)
	}

	res.Step, err = stepRow(env, id)
	return res, err
}

// parseStepRef parses a step reference ("STEP-3" or a bare number) and
// reports a malformed one as VALIDATION_ERROR.
func parseStepRef(ref string) (int, error) {
	id, err := model.ParseStepID(ref)
	if err != nil {
		return 0, newError(fmt.Errorf("invalid step ID: %w", err), output.ErrValidation)
	}
	return id, nil
}

// resolveRunAddressedProject resolves the caller's project for a verb that
// takes its project from the step it is handed. When it registers one, it
// returns the registration, attributed to verbPath, for the caller to record
// as an event.
func resolveRunAddressedProject(env Env, verbPath string) (*engine.ProjectRegistration, error) {
	projectID, created, err := env.ResolveProject(ProjectRunAddressed)
	if err != nil || !created {
		return nil, err
	}
	prefix, _ := env.Store.ProjectPrefix(projectID)
	return &engine.ProjectRegistration{
		ID:       projectID,
		Name:     filepath.Base(env.Project.Identity),
		Identity: env.Project.Identity,
		Cwd:      env.Cwd,
		Verb:     verbPath,
		Prefix:   prefix,
	}, nil
}

// driveStepRun advances the engine-run lifecycles of step id's run after a
// write to the step, since a recording or a routed failure can ready vote and
// action steps that nothing else would drive mid-wave. A step that cannot be
// read leaves nothing to drive.
func driveStepRun(e *engine.Engine, env Env, id int) error {
	step, err := env.Store.GetStep(id)
	if err != nil {
		return nil
	}
	return e.DriveRunLifecyclesStore(env.Store, step.RunID, model.NowMS())
}

// stepRow re-reads step id at its effective status after a write.
func stepRow(env Env, id int) (model.StepRow, error) {
	view, err := engine.LoadStepViewStore(env.Store, id, model.NowMS())
	if err != nil {
		return model.StepRow{}, stepError(err, id)
	}
	return view.Row, nil
}

// stepError maps an engine or lease failure on step id to its error code:
// the lease refusal matrix first, then a missing step, then the engine's own
// taxonomy.
func stepError(err error, id int) error {
	label := "step " + model.FormatStepID(id)
	if e := LeaseError(err, label); e != nil {
		return e
	}
	if errors.Is(err, store.ErrStepNotFound) {
		return newError(fmt.Errorf("%s not found", label), output.ErrNotFound)
	}
	return engineError(err)
}

// engineError maps an engine failure to its error code by the engine's own
// taxonomy, then the store's not-found sentinels. Anything else is a general
// failure carrying the cause unchanged. It names no noun, so every verb whose
// failures come from the engine maps them through this one table.
func engineError(err error) error {
	if code, ok := engine.CodeOf(err); ok {
		switch code {
		case engine.CodeValidation:
			return newError(err, output.ErrValidation)
		case engine.CodeNotFound:
			return newError(err, output.ErrNotFound)
		case engine.CodeConflict:
			return newError(err, output.ErrConflict)
		case engine.CodeGone:
			return newError(err, output.ErrGone)
		case engine.CodeAuth:
			return newError(err, output.ErrAuth)
		}
	}
	if errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrNotFound) {
		return newError(err, output.ErrNotFound)
	}
	return newError(err, output.ErrGeneral)
}
