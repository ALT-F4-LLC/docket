package verb

import (
	"context"

	"github.com/ALT-F4-LLC/docket/internal/engine"
	"github.com/ALT-F4-LLC/docket/internal/model"
)

// GateStatusInput names the gate step to report on.
type GateStatusInput struct {
	Step string
}

// GateStatusResult is one gate step's decision state.
type GateStatusResult struct {
	engine.GateStatusResult
}

// GateStatus reads a human or vote gate step's status, outcome and, on a vote
// gate, its tally and seats. It writes nothing, so it is a read: the step it
// is handed carries its own project, and the caller's identity is never
// registered.
func GateStatus(ctx context.Context, env Env, in GateStatusInput) (GateStatusResult, error) {
	id, err := parseStepRef(in.Step)
	if err != nil {
		return GateStatusResult{}, err
	}
	if _, _, err := env.ResolveProject(ProjectReadOnly); err != nil {
		return GateStatusResult{}, err
	}

	status, err := engine.GateStatusStore(env.Store, id, model.NowMS())
	if err != nil {
		return GateStatusResult{}, stepError(err, id)
	}
	return GateStatusResult{GateStatusResult: *status}, nil
}
