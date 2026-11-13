package verb

import (
	"context"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// GuardDecision is a guard's verdict. A denial is a decision, not a failure:
// it comes back with a nil error, and each transport renders it as its own
// deny form.
type GuardDecision struct {
	Allowed bool
	// Reason explains a denial.
	Reason string
}

// The four guards answer the same yes/no question shape.
type (
	GuardStopResult   = GuardDecision
	GuardGateResult   = GuardDecision
	GuardRecordResult = GuardDecision
	GuardSpawnResult  = GuardDecision
)

// Every guard is a read by contract, and guards fire on hook paths that run
// from arbitrary directories, so each one resolves its project read-only: an
// identity no project holds answers over no runs and registers nothing.

// GuardStopInput asks whether any work is still pending.
type GuardStopInput struct {
	// AllProjects answers over every project's runs, not the caller's alone.
	AllProjects bool
}

// GuardStop allows when no active run of the caller's project has a step
// still working outside waiting-human.
func GuardStop(ctx context.Context, env Env, in GuardStopInput) (GuardStopResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return GuardStopResult{}, err
	}
	if in.AllProjects {
		projectID = 0
	}
	return guardDecision(engine.GuardStopStore(env.Store, projectID, model.NowMS()))
}

// GuardGateInput names the gate step to check.
type GuardGateInput struct {
	Step string
	// Run limits the answer to one run's gate; empty asks every active run
	// of the project.
	Run         string
	AllProjects bool
}

// GuardGate allows when a human or vote gate named in.Step passed by a
// decision.
func GuardGate(ctx context.Context, env Env, in GuardGateInput) (GuardGateResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return GuardGateResult{}, err
	}
	if in.AllProjects {
		projectID = 0
	}
	runID, err := optionalRunRef(in.Run)
	if err != nil {
		return GuardGateResult{}, err
	}
	return guardDecision(engine.GuardGateStore(env.Store, in.Step, runID, projectID))
}

// GuardRecordInput names the run to check; empty asks every non-terminal
// run of the project.
type GuardRecordInput struct {
	Run         string
	AllProjects bool
}

// GuardRecord allows when no open dispatch and no discrepancy exists.
func GuardRecord(ctx context.Context, env Env, in GuardRecordInput) (GuardRecordResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return GuardRecordResult{}, err
	}
	if in.AllProjects {
		projectID = 0
	}
	runID, err := optionalRunRef(in.Run)
	if err != nil {
		return GuardRecordResult{}, err
	}
	return guardDecision(engine.GuardRecordStore(env.Store, runID, projectID, model.NowMS()))
}

// GuardSpawnInput is a relay's question before it spawns a batch.
type GuardSpawnInput struct {
	// Run is the run whose batch is being spawned; required unless Active.
	Run string
	// Active answers the reap half over every active run of the project.
	Active bool
	// Rows is the JSON array of rows about to be spawned. Nil means none was
	// proposed, which differs from an empty array. Active refuses any
	// non-nil Rows, so a transport need not read the batch to send it then.
	Rows []byte
	// AckReap acknowledges write-class reaps by their lease-reaped event seq.
	AckReap []int64
	// DecidingVote names the OPEN proposal (PROPOSAL-N) this batch exists to
	// decide; empty claims no carve-out.
	DecidingVote string
	// UsageJoin admits a launch that claims no step past a reap hold.
	UsageJoin   bool
	AllProjects bool
}

// GuardSpawn allows when the proposed rows match the open dispatch and no
// write-class reap is unacknowledged. It writes the acknowledgments it is
// given and, when a carve-out admits the batch, that audit event.
func GuardSpawn(ctx context.Context, env Env, in GuardSpawnInput) (GuardSpawnResult, error) {
	projectID, _, err := env.ResolveProject(ProjectReadOnly)
	if err != nil {
		return GuardSpawnResult{}, err
	}
	if in.AllProjects {
		projectID = 0
	}
	if in.Active {
		return guardSpawnActive(env, in, projectID)
	}

	if in.Run == "" {
		return GuardSpawnResult{}, newError(
			fmt.Errorf("--run is required: name the run whose batch is being spawned"),
			output.ErrValidation)
	}
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return GuardSpawnResult{}, err
	}
	for _, seq := range in.AckReap {
		if seq <= 0 {
			return GuardSpawnResult{}, newError(fmt.Errorf(
				"--ack-reap %d is not an event seq; pass the `seq` of the "+
					"`lease-reaped` event being acknowledged", seq),
				output.ErrValidation)
		}
	}
	decidingVote, err := decidingVoteRef(in.DecidingVote)
	if err != nil {
		return GuardSpawnResult{}, err
	}

	return guardDecision(engine.NewEngine().GuardSpawnStore(
		env.Store, runID, engine.SpawnOptions{
			Rows: in.Rows, AckSeqs: in.AckReap, DecidingVote: decidingVote,
			UsageJoin: in.UsageJoin,
			NowMS:     model.NowMS(),
		}))
}

// guardSpawnActive is GuardSpawn under Active. Run, Rows and AckReap are each
// an act about ONE run's manifest or ledger, and Active answers every active
// run's reap half at once, so it refuses to guess which run they meant
// instead of silently narrowing back to one. DecidingVote needs no guess: the
// proposal resolves its own run.
func guardSpawnActive(env Env, in GuardSpawnInput, projectID int) (GuardSpawnResult, error) {
	if in.Run != "" {
		return GuardSpawnResult{}, newError(fmt.Errorf(
			"--active and --run are mutually exclusive: --active already means "+
				"every active run"), output.ErrValidation)
	}
	if in.Rows != nil {
		return GuardSpawnResult{}, newError(fmt.Errorf(
			"--active does not take --rows: a proposed batch is one run's "+
				"manifest, so name that run with --run instead"), output.ErrValidation)
	}
	if len(in.AckReap) > 0 {
		return GuardSpawnResult{}, newError(fmt.Errorf(
			"--active does not take --ack-reap: an acknowledgment is one run's "+
				"ledger entry, so name that run with --run instead"), output.ErrValidation)
	}
	decidingVote, err := decidingVoteRef(in.DecidingVote)
	if err != nil {
		return GuardSpawnResult{}, err
	}

	return guardDecision(engine.GuardSpawnActiveStore(
		env.Store, projectID, decidingVote, in.UsageJoin, model.NowMS()))
}

// optionalRunRef parses an optional run reference: empty is run 0, meaning
// every non-terminal run, and a malformed one is a VALIDATION_ERROR rather
// than a silent widening to every run.
func optionalRunRef(ref string) (int, error) {
	if ref == "" {
		return 0, nil
	}
	return ParseRunRef(ref)
}

// decidingVoteRef parses a deciding-vote proposal reference. Empty is 0,
// meaning no carve-out is claimed; a malformed reference is a
// VALIDATION_ERROR rather than a silent 0, because a hook that typed the id
// wrong would otherwise be denied with a message that never mentions its own
// typo.
func decidingVoteRef(ref string) (int, error) {
	if ref == "" {
		return 0, nil
	}
	id, err := model.ParseProposalID(ref)
	if err != nil {
		return 0, newError(
			fmt.Errorf("invalid --deciding-vote %q: %w", ref, err),
			output.ErrValidation)
	}
	return id, nil
}

func guardDecision(verdict *engine.GuardVerdict, err error) (GuardDecision, error) {
	if err != nil {
		return GuardDecision{}, engineError(err)
	}
	return GuardDecision{Allowed: verdict.Allowed, Reason: verdict.Reason}, nil
}
