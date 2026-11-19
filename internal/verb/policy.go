package verb

import (
	"context"
	"fmt"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
	"github.com/ALT-F4-LLC/docket/internal/output"
)

// PolicyResolveInput names the run whose pinned policy.toml the seats resolve
// against. Size and Labels stand in for the frozen Size and issue labels a
// vote step row snapshots; empty resolves as an unsized, unlabelled row does.
type PolicyResolveInput struct {
	Run    string
	Seats  []string
	Size   string
	Labels []string
}

// PolicyResolveResult holds one assignment per requested seat, in request
// order.
type PolicyResolveResult struct {
	Seats []model.VoterAssignment
}

// PolicyResolve resolves each seat's model, effort and variant from the run's
// pinned policy.toml. Every seat resolves or the call fails with NOT_FOUND.
// It is a read: the run names its own project, and the caller's identity is
// never registered.
func PolicyResolve(ctx context.Context, env Env, in PolicyResolveInput) (PolicyResolveResult, error) {
	if in.Run == "" {
		return PolicyResolveResult{}, newError(fmt.Errorf(
			"--run is required: name the run whose pinned policy.toml the seats "+
				"resolve against"), output.ErrValidation)
	}
	runID, err := ParseRunRef(in.Run)
	if err != nil {
		return PolicyResolveResult{}, err
	}
	// Validated as a vote row's frozen Size is, so a typo cannot silently
	// resolve the unsized roster. The empty default stays unvalidated.
	if in.Size != "" {
		if err := model.ValidateSize(model.Size(in.Size)); err != nil {
			return PolicyResolveResult{}, newError(err, output.ErrValidation)
		}
	}

	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return PolicyResolveResult{}, err
	}

	seats, err := engine.ResolveSeatsStore(env.Store, runID, in.Seats, in.Size, in.Labels)
	if err != nil {
		return PolicyResolveResult{}, engineError(err)
	}
	return PolicyResolveResult{Seats: seats}, nil
}
